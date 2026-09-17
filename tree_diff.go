package history

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/storer"
)

const (
	treeDiffMaxDepth       = 1024
	treeModeTypeMask       = 0o170000
	treeModeExecutableMask = 0o111
)

type treeDiffEntry struct {
	Hash plumbing.Hash
	Mode filemode.FileMode
}

type treeDiffChange struct {
	From treeDiffEntry
	To   treeDiffEntry
	Path string
}

type encodedTreeEntry struct {
	name []byte
	hash plumbing.Hash
	mode filemode.FileMode
}

type encodedTree struct {
	data   []byte
	offset int
	idSize int
	entry  encodedTreeEntry
	valid  bool
}

func walkTreeDiffContext(
	ctx context.Context,
	store storer.EncodedObjectStorer,
	from, to plumbing.Hash,
	visit func(treeDiffChange) error,
) error {
	return walkEncodedTreeDiff(ctx, store, from, to, "", 0, visit)
}

func readEncodedTree(store storer.EncodedObjectStorer, hash plumbing.Hash) (_ *encodedTree, resultErr error) {
	encoded, err := store.EncodedObject(plumbing.TreeObject, hash)
	if err != nil {
		return nil, err
	}
	if encoded.Size() < 0 || encoded.Size() > int64(int(^uint(0)>>1)) {
		return nil, fmt.Errorf("tree size %d cannot be represented in memory", encoded.Size())
	}
	reader, err := encoded.Reader()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := reader.Close(); resultErr == nil {
			resultErr = err
		}
	}()
	tree := &encodedTree{
		data:   make([]byte, int(encoded.Size())),
		idSize: hash.Size(),
	}
	if _, err := io.ReadFull(reader, tree.data); err != nil {
		return nil, err
	}
	if err := tree.next(); err != nil {
		return nil, err
	}
	return tree, nil
}

func (t *encodedTree) next() error {
	if t.offset == len(t.data) {
		t.valid = false
		return nil
	}
	modeEnd := bytes.IndexByte(t.data[t.offset:], ' ')
	if modeEnd < 0 {
		return fmt.Errorf("%w: missing mode terminator", object.ErrMalformedTree)
	}
	modeEnd += t.offset
	mode, err := parseTreeMode(t.data[t.offset:modeEnd])
	if err != nil {
		return fmt.Errorf("%w: malformed mode", object.ErrMalformedTree)
	}
	nameStart := modeEnd + 1
	nameEnd := bytes.IndexByte(t.data[nameStart:], 0)
	if nameEnd < 0 {
		return fmt.Errorf("%w: missing filename terminator", object.ErrMalformedTree)
	}
	nameEnd += nameStart
	if nameEnd == nameStart {
		return fmt.Errorf("%w: empty filename", object.ErrMalformedTree)
	}
	hashStart := nameEnd + 1
	hashEnd := hashStart + t.idSize
	if hashEnd > len(t.data) {
		return fmt.Errorf("%w: truncated object id", object.ErrMalformedTree)
	}
	hash, ok := plumbing.FromBytes(t.data[hashStart:hashEnd])
	if !ok {
		return fmt.Errorf("%w: invalid object id size", object.ErrMalformedTree)
	}
	t.entry = encodedTreeEntry{
		name: t.data[nameStart:nameEnd],
		hash: hash,
		mode: canonicalTreeMode(mode),
	}
	t.offset = hashEnd
	t.valid = true
	return nil
}

func walkEncodedTreeDiff(
	ctx context.Context,
	store storer.EncodedObjectStorer,
	fromHash, toHash plumbing.Hash,
	prefix string,
	depth int,
	visit func(treeDiffChange) error,
) error {
	if depth > treeDiffMaxDepth {
		return object.ErrMaxTreeDepth
	}
	if !fromHash.IsZero() && !toHash.IsZero() && equalObjectIDs(fromHash, toHash) {
		return nil
	}
	var from, to *encodedTree
	var err error
	if !fromHash.IsZero() {
		from, err = readEncodedTree(store, fromHash)
		if err != nil {
			return err
		}
	}
	if !toHash.IsZero() {
		to, err = readEncodedTree(store, toHash)
		if err != nil {
			return err
		}
	}
	for encodedTreeValid(from) || encodedTreeValid(to) {
		select {
		case <-ctx.Done():
			return object.ErrCanceled
		default:
		}
		if err := walkEncodedTreeDiffStep(ctx, store, from, to, prefix, depth, visit); err != nil {
			return err
		}
	}
	return nil
}

func walkEncodedTreeDiffStep(
	ctx context.Context,
	store storer.EncodedObjectStorer,
	from, to *encodedTree,
	prefix string,
	depth int,
	visit func(treeDiffChange) error,
) error {
	if !encodedTreeValid(from) {
		if err := visitAddedTreeEntry(ctx, store, to.entry, prefix, depth, visit); err != nil {
			return err
		}
		return to.next()
	}
	if !encodedTreeValid(to) {
		if err := visitDeletedTreeEntry(ctx, store, from.entry, prefix, depth, visit); err != nil {
			return err
		}
		return from.next()
	}
	switch comparison := compareEncodedTreeEntries(from.entry, to.entry); {
	case comparison < 0:
		if err := visitDeletedTreeEntry(ctx, store, from.entry, prefix, depth, visit); err != nil {
			return err
		}
		return from.next()
	case comparison > 0:
		if err := visitAddedTreeEntry(ctx, store, to.entry, prefix, depth, visit); err != nil {
			return err
		}
		return to.next()
	default:
		if err := visitChangedTreeEntry(ctx, store, from.entry, to.entry, prefix, depth, visit); err != nil {
			return err
		}
		if err := from.next(); err != nil {
			return err
		}
		return to.next()
	}
}

func encodedTreeValid(tree *encodedTree) bool {
	return tree != nil && tree.valid
}

func compareEncodedTreeEntries(from, to encodedTreeEntry) int {
	if bytes.Equal(from.name, to.name) {
		return 0
	}
	return compareTreeNames(from.name, from.mode == filemode.Dir, to.name, to.mode == filemode.Dir)
}

func compareTreeNames(a []byte, aDir bool, b []byte, bDir bool) int {
	common := min(len(a), len(b))
	if comparison := bytes.Compare(a[:common], b[:common]); comparison != 0 {
		return comparison
	}
	return int(treeNameTerminator(a, aDir, common)) - int(treeNameTerminator(b, bDir, common))
}

func treeNameTerminator(name []byte, dir bool, index int) byte {
	if index < len(name) {
		return name[index]
	}
	if dir {
		return '/'
	}
	return 0
}

func visitChangedTreeEntry(
	ctx context.Context,
	store storer.EncodedObjectStorer,
	from, to encodedTreeEntry,
	prefix string,
	depth int,
	visit func(treeDiffChange) error,
) error {
	if equalObjectIDs(from.hash, to.hash) && from.mode == to.mode {
		return nil
	}
	path := joinEncodedTreePath(prefix, from.name)
	fromDir := from.mode == filemode.Dir
	toDir := to.mode == filemode.Dir
	switch {
	case fromDir && toDir:
		return walkEncodedTreeDiff(ctx, store, from.hash, to.hash, path, depth+1, visit)
	case fromDir:
		if err := walkEncodedTreeDiff(ctx, store, from.hash, plumbing.ZeroHash, path, depth+1, visit); err != nil {
			return err
		}
		return visit(treeDiffChange{Path: path, To: newTreeDiffEntry(to)})
	case toDir:
		if err := visit(treeDiffChange{Path: path, From: newTreeDiffEntry(from)}); err != nil {
			return err
		}
		return walkEncodedTreeDiff(ctx, store, plumbing.ZeroHash, to.hash, path, depth+1, visit)
	default:
		return visit(treeDiffChange{Path: path, From: newTreeDiffEntry(from), To: newTreeDiffEntry(to)})
	}
}

func visitAddedTreeEntry(
	ctx context.Context,
	store storer.EncodedObjectStorer,
	entry encodedTreeEntry,
	prefix string,
	depth int,
	visit func(treeDiffChange) error,
) error {
	path := joinEncodedTreePath(prefix, entry.name)
	if entry.mode == filemode.Dir {
		return walkEncodedTreeDiff(ctx, store, plumbing.ZeroHash, entry.hash, path, depth+1, visit)
	}
	return visit(treeDiffChange{Path: path, To: newTreeDiffEntry(entry)})
}

func visitDeletedTreeEntry(
	ctx context.Context,
	store storer.EncodedObjectStorer,
	entry encodedTreeEntry,
	prefix string,
	depth int,
	visit func(treeDiffChange) error,
) error {
	path := joinEncodedTreePath(prefix, entry.name)
	if entry.mode == filemode.Dir {
		return walkEncodedTreeDiff(ctx, store, entry.hash, plumbing.ZeroHash, path, depth+1, visit)
	}
	return visit(treeDiffChange{Path: path, From: newTreeDiffEntry(entry)})
}

func newTreeDiffEntry(entry encodedTreeEntry) treeDiffEntry {
	return treeDiffEntry{Hash: entry.hash, Mode: entry.mode}
}

func equalObjectIDs(a, b plumbing.Hash) bool {
	return a.Size() == b.Size() && a.Equal(b)
}

func canonicalTreeMode(mode filemode.FileMode) filemode.FileMode {
	switch mode & treeModeTypeMask {
	case filemode.Dir:
		return filemode.Dir
	case filemode.Regular & treeModeTypeMask:
		if mode&treeModeExecutableMask != 0 {
			return filemode.Executable
		}
		return filemode.Regular
	case filemode.Symlink:
		return filemode.Symlink
	default:
		return filemode.Submodule
	}
}

func joinEncodedTreePath(prefix string, name []byte) string {
	if prefix == "" {
		return string(name)
	}
	var path strings.Builder
	path.Grow(len(prefix) + 1 + len(name))
	path.WriteString(prefix)
	path.WriteByte('/')
	_, _ = path.Write(name)
	return path.String()
}
