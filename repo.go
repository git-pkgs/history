package history

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-billy/v6/osfs"
	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/cache"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/storage/filesystem"
	"github.com/go-git/go-git/v6/storage/filesystem/dotgit"
)

// Repo wraps an open go-git repository configured for history walking.
type Repo struct {
	r *gogit.Repository
	t Tuning
}

// Open opens the repository at path, refusing configurations go-git cannot
// walk correctly (alternate object stores, grafts, replace refs).
func Open(path string, t Tuning) (*Repo, error) {
	for _, name := range []string{"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES"} {
		if os.Getenv(name) != "" {
			return nil, fmt.Errorf("history: go-git does not support %s", name)
		}
	}
	if t.KlauspostZlib {
		if err := useKlauspostZlib(); err != nil {
			return nil, err
		}
	}
	r, err := gogit.PlainOpen(path)
	if err != nil {
		return nil, err
	}
	fs := r.Storer.(*filesystem.Storage).Filesystem()
	for _, name := range []string{"objects/info/alternates", "info/grafts"} {
		f, err := fs.Open(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err == nil {
			var data []byte
			data, err = io.ReadAll(f)
			closeErr := f.Close()
			if err == nil {
				err = closeErr
			}
			if err == nil && len(strings.TrimSpace(string(data))) > 0 {
				err = fmt.Errorf("history: go-git does not support %s", name)
			}
		}
		if err != nil {
			_ = r.Close()
			return nil, err
		}
	}
	if err := configure(r, t); err != nil {
		_ = r.Close()
		return nil, err
	}
	return &Repo{r: r, t: t}, nil
}

// Close releases the underlying go-git repository.
func (r *Repo) Close() error { return r.r.Close() }

// Repository returns the underlying go-git handle for callers that need
// direct storer access.
func (r *Repo) Repository() *gogit.Repository { return r.r }

// Shallow reports whether the repository is a shallow clone.
func (r *Repo) Shallow() (bool, error) {
	shallow, err := r.r.Storer.Shallow()
	return len(shallow) != 0, err
}

// Roots returns every commit reachable from a ref, deduplicated.
func (r *Repo) Roots() ([]plumbing.Hash, error) {
	return historyRoots(r.r)
}

func configure(r *gogit.Repository, t Tuning) error {
	if t.CacheBytes == 0 {
		t.CacheBytes = uint64(cache.DefaultMaxSize)
	}
	if !t.MemoryIndex && !t.Mmap && t.CacheBytes == uint64(cache.DefaultMaxSize) && t.CacheShards <= 1 {
		return nil
	}
	fs := r.Storer.(*filesystem.Storage).Filesystem()
	if t.Mmap {
		objects, err := fs.Chroot("objects")
		if err != nil {
			return err
		}
		fs = dotgit.NewRepositoryFilesystem(
			osfs.New(fs.Root(), osfs.WithBoundOS(), osfs.WithMmap()),
			osfs.New(filepath.Dir(objects.Root()), osfs.WithBoundOS(), osfs.WithMmap()),
		)
	}
	if err := r.Close(); err != nil {
		return err
	}
	objectCache := cache.Object(cache.NewObjectLRU(cache.FileSize(t.CacheBytes)))
	if t.CacheShards > 1 {
		objectCache = cache.NewShardedObjectLRU(cache.FileSize(t.CacheBytes), t.CacheShards)
	}
	r.Storer = filesystem.NewStorageWithOptions(fs, objectCache, filesystem.Options{
		UseInMemoryIdx: t.MemoryIndex,
	})
	return nil
}

func historyRoots(r *gogit.Repository) ([]plumbing.Hash, error) {
	refs, err := r.References()
	if err != nil {
		return nil, err
	}
	defer refs.Close()
	var roots []plumbing.Hash
	head, err := r.Head()
	if err != nil && !errors.Is(err, plumbing.ErrReferenceNotFound) {
		return nil, err
	}
	if err == nil {
		h, ok, err := commitAtReference(r, head.Hash())
		if err != nil {
			return nil, err
		}
		if ok {
			roots = append(roots, h)
		}
	}
	for {
		ref, err := refs.Next()
		if err == io.EOF {
			return roots, nil
		}
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(ref.Name().String(), "refs/") {
			continue
		}
		if strings.HasPrefix(ref.Name().String(), "refs/replace/") {
			return nil, fmt.Errorf("history: go-git does not support replacement refs")
		}
		resolved, err := r.Reference(ref.Name(), true)
		if err != nil {
			return nil, err
		}
		h, ok, err := commitAtReference(r, resolved.Hash())
		if err != nil {
			return nil, fmt.Errorf("reference %s: %w", ref.Name(), err)
		}
		if ok {
			roots = append(roots, h)
		}
	}
}

func commitAtReference(r *gogit.Repository, h plumbing.Hash) (plumbing.Hash, bool, error) {
	seen := make(map[plumbing.Hash]bool)
	for {
		if seen[h] {
			return plumbing.ZeroHash, false, fmt.Errorf("cyclic tag at %s", h)
		}
		seen[h] = true
		o, err := r.Object(plumbing.AnyObject, h)
		if err != nil {
			return plumbing.ZeroHash, false, err
		}
		switch v := o.(type) {
		case *object.Commit:
			return v.Hash, true, nil
		case *object.Tag:
			h = v.Target
		default:
			return plumbing.ZeroHash, false, nil
		}
	}
}

// CommitSubject returns the first paragraph of a commit message as a single
// line, matching git log --format=%s.
func CommitSubject(message string) string {
	var lines []string
	for line := range strings.SplitSeq(strings.TrimSpace(message), "\n") {
		if strings.TrimSpace(line) == "" {
			break
		}
		lines = append(lines, strings.TrimSpace(line))
	}
	return strings.Join(lines, " ")
}
