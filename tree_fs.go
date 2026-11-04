package history

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
)

// TreeFS is a read-only filesystem rooted at a Git tree. Paths use slash
// separators and must satisfy fs.ValidPath. Reads may run concurrently;
// individual open file handles must not be shared between goroutines.
// The Repo must remain open while the filesystem or its files are in use.
//
// Symlinks are not followed: opening one reads its target text, and Stat
// reports fs.ModeSymlink. Submodules are empty directories; their commits
// need not be present. Modes reflect Git permissions, modification times
// are zero, directory sizes are zero, and FileInfo.Sys returns nil.
type TreeFS struct {
	repo        *Repo
	root        plumbing.Hash
	treeEntries sync.Map
}

// TreeFS returns a filesystem for a commit or tree hash without a worktree.
// Other object types, including annotated tags, are rejected.
func (r *Repo) TreeFS(hash plumbing.Hash) (*TreeFS, error) {
	encoded, err := r.r.Storer.EncodedObject(plumbing.AnyObject, hash)
	if err != nil {
		return nil, treePathError("treefs", hash.String(), err)
	}
	root := hash
	switch encoded.Type() {
	case plumbing.CommitObject:
		root, err = readCommitTreeAndParents(r.r, hash, false, nil)
		if err == nil {
			_, err = r.r.Storer.EncodedObject(plumbing.TreeObject, root)
		}
	case plumbing.TreeObject:
	default:
		err = fmt.Errorf("%w: object %s is %s, want commit or tree", fs.ErrInvalid, hash, encoded.Type())
	}
	if err != nil {
		return nil, treePathError("treefs", hash.String(), err)
	}
	return &TreeFS{repo: r, root: root}, nil
}

// Open opens a file or directory relative to the snapshot root.
func (s *TreeFS) Open(name string) (fs.File, error) { //nolint:ireturn // fs.FS contract
	entry, err := s.lookup(name)
	if err != nil {
		return nil, treePathError("open", name, err)
	}
	info, err := entry.Info()
	if err != nil {
		return nil, treePathError("open", name, err)
	}
	file := &treeFile{entry: entry, info: info}
	if !entry.IsDir() {
		encoded, err := s.repo.r.Storer.EncodedObject(plumbing.BlobObject, entry.hash)
		if err != nil {
			return nil, treePathError("open", name, err)
		}
		file.reader, err = encoded.Reader()
		if err != nil {
			return nil, treePathError("open", name, err)
		}
	}
	return file, nil
}

// Stat returns metadata without opening the blob's contents.
func (s *TreeFS) Stat(name string) (fs.FileInfo, error) { //nolint:ireturn // fs.StatFS contract
	entry, err := s.lookup(name)
	if err != nil {
		return nil, treePathError("stat", name, err)
	}
	info, err := entry.Info()
	if err != nil {
		return nil, treePathError("stat", name, err)
	}
	return info, nil
}

func (s *TreeFS) lookup(name string) (treeDirEntry, error) {
	entry := treeDirEntry{snapshot: s, name: ".", hash: s.root, mode: filemode.Dir}
	if !fs.ValidPath(name) {
		return entry, fs.ErrInvalid
	}
	if name == "." {
		return entry, nil
	}
	for _, part := range strings.Split(name, "/") {
		if !entry.IsDir() {
			return entry, syscall.ENOTDIR
		}
		if entry.mode == filemode.Submodule {
			return entry, fs.ErrNotExist
		}
		children, err := s.entries(entry.hash)
		if err != nil {
			return entry, err
		}
		var found bool
		var next treeDirEntry
		for _, child := range children {
			if child.name == part {
				next = treeDirEntry{snapshot: s, name: path.Join(entry.name, part), hash: child.hash, mode: child.mode}
				found = true
				break
			}
		}
		if !found {
			return entry, fs.ErrNotExist
		}
		entry = next
	}
	return entry, nil
}

type cachedTreeEntry struct {
	name string
	hash plumbing.Hash
	mode filemode.FileMode
}

func (s *TreeFS) entries(hash plumbing.Hash) ([]cachedTreeEntry, error) {
	if cached, ok := s.treeEntries.Load(hash); ok {
		return cached.([]cachedTreeEntry), nil
	}
	entries := make([]cachedTreeEntry, 0)
	err := s.repo.WalkTreeEntries(hash, func(child TreeEntry) error {
		entries = append(entries, cachedTreeEntry{name: string(child.Name), hash: child.Hash, mode: child.Mode})
		return nil
	})
	if err != nil {
		return nil, err
	}
	actual, _ := s.treeEntries.LoadOrStore(hash, entries)
	return actual.([]cachedTreeEntry), nil
}

type treeDirEntry struct {
	snapshot *TreeFS
	name     string
	hash     plumbing.Hash
	mode     filemode.FileMode
}

func (e treeDirEntry) Name() string { return path.Base(e.name) }
func (e treeDirEntry) IsDir() bool  { return e.mode == filemode.Dir || e.mode == filemode.Submodule }
func (e treeDirEntry) Type() fs.FileMode {
	mode, _ := e.mode.ToOSFileMode()
	return mode.Type()
}

func (e treeDirEntry) Info() (fs.FileInfo, error) { //nolint:ireturn // fs.DirEntry contract
	mode, err := e.mode.ToOSFileMode()
	if err != nil {
		return nil, treePathError("stat", e.name, err)
	}
	info := treeFileInfo{name: e.Name(), mode: mode}
	if !e.IsDir() {
		info.size, err = e.snapshot.repo.r.Storer.EncodedObjectSize(e.hash)
		if err != nil {
			return nil, treePathError("stat", e.name, err)
		}
	}
	return info, nil
}

type treeFileInfo struct {
	name string
	size int64
	mode fs.FileMode
}

func (i treeFileInfo) Name() string       { return i.name }
func (i treeFileInfo) Size() int64        { return i.size }
func (i treeFileInfo) Mode() fs.FileMode  { return i.mode }
func (i treeFileInfo) ModTime() time.Time { return time.Time{} }
func (i treeFileInfo) IsDir() bool        { return i.mode.IsDir() }
func (i treeFileInfo) Sys() any           { return nil }

type treeFile struct {
	entry   treeDirEntry
	info    fs.FileInfo
	reader  io.ReadCloser
	entries []fs.DirEntry
	offset  int
	loaded  bool
	closed  bool
}

func (f *treeFile) Stat() (fs.FileInfo, error) { //nolint:ireturn // fs.File contract
	if f.closed {
		return nil, treePathError("stat", f.entry.name, fs.ErrClosed)
	}
	return f.info, nil
}

func (f *treeFile) Read(data []byte) (int, error) {
	if f.closed {
		return 0, treePathError("read", f.entry.name, fs.ErrClosed)
	}
	if f.entry.IsDir() {
		return 0, treePathError("read", f.entry.name, syscall.EISDIR)
	}
	n, err := f.reader.Read(data)
	if err != nil && err != io.EOF {
		err = treePathError("read", f.entry.name, err)
	}
	return n, err
}

func (f *treeFile) Close() error {
	if f.closed {
		return treePathError("close", f.entry.name, fs.ErrClosed)
	}
	f.closed = true
	if f.reader != nil {
		if err := f.reader.Close(); err != nil {
			return treePathError("close", f.entry.name, err)
		}
	}
	return nil
}

func (f *treeFile) ReadDir(n int) ([]fs.DirEntry, error) {
	if f.closed {
		return nil, treePathError("readdir", f.entry.name, fs.ErrClosed)
	}
	if !f.entry.IsDir() {
		return nil, treePathError("readdir", f.entry.name, syscall.ENOTDIR)
	}
	if !f.loaded {
		if err := f.loadEntries(); err != nil {
			return nil, treePathError("readdir", f.entry.name, err)
		}
	}
	if n > 0 && f.offset == len(f.entries) {
		return nil, io.EOF
	}
	end := len(f.entries)
	if n > 0 {
		end = f.offset + min(n, end-f.offset)
	}
	entries := slices.Clone(f.entries[f.offset:end])
	f.offset = end
	return entries, nil
}

func (f *treeFile) loadEntries() error {
	entries := make([]fs.DirEntry, 0)
	if f.entry.mode != filemode.Submodule {
		children, err := f.entry.snapshot.entries(f.entry.hash)
		if err != nil {
			return err
		}
		for _, child := range children {
			if !fs.ValidPath(child.name) || child.name == "." || strings.Contains(child.name, "/") || child.mode.IsMalformed() {
				return fmt.Errorf("%w: invalid tree entry %q (mode %s)", fs.ErrInvalid, child.name, child.mode)
			}
			entries = append(entries, treeDirEntry{
				snapshot: f.entry.snapshot, name: path.Join(f.entry.name, child.name), hash: child.hash, mode: child.mode,
			})
		}
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	f.entries = entries
	f.loaded = true
	return nil
}

func treePathError(op, name string, err error) *fs.PathError {
	if errors.Is(err, plumbing.ErrObjectNotFound) {
		err = errors.Join(fs.ErrNotExist, err)
	}
	return &fs.PathError{Op: op, Path: name, Err: err}
}
