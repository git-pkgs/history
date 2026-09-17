package history

import (
	"container/list"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/go-git/go-billy/v6"
	"github.com/go-git/go-billy/v6/osfs"
	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/cache"
	formatcfg "github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/storage/filesystem"
	"github.com/go-git/go-git/v6/storage/filesystem/dotgit"
)

// Repo wraps an open go-git repository configured for history walking.
type Repo struct {
	r            *gogit.Repository
	t            Tuning
	gitDir       string
	commonDir    string
	objectCache  cache.Object
	alternatesFS billy.Filesystem
	objectFormat formatcfg.ObjectFormat
}

// OpenOptions controls repository discovery and storage.
type OpenOptions struct {
	Tuning       Tuning
	DetectDotGit bool
	AlternatesFS billy.Filesystem
}

// Open opens the repository at path with the default tuning.
func Open(path string) (*Repo, error) {
	return OpenWithOptions(path, OpenOptions{Tuning: DefaultTuning()})
}

// OpenWithOptions opens the repository at path with repository discovery and
// alternate-object storage options.
func OpenWithOptions(path string, opts OpenOptions) (*Repo, error) { //nolint:gocognit // repository compatibility checks are sequential
	for _, name := range []string{"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES"} {
		if os.Getenv(name) != "" {
			return nil, fmt.Errorf("history: go-git does not support %s", name)
		}
	}
	if opts.Tuning.KlauspostZlib {
		if err := useKlauspostZlib(); err != nil {
			return nil, err
		}
	}
	alternatesFS := opts.AlternatesFS
	if alternatesFS == nil {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		root := filepath.VolumeName(abs) + string(filepath.Separator)
		if opts.Tuning.Mmap {
			alternatesFS = osfs.New(root, osfs.WithBoundOS(), osfs.WithMmap())
		} else {
			alternatesFS = osfs.New(root)
		}
	}
	r, err := gogit.PlainOpenWithOptions(path, &gogit.PlainOpenOptions{
		DetectDotGit: opts.DetectDotGit,
	})
	if err != nil {
		return nil, err
	}
	if err := rejectReplacementRefs(r); err != nil {
		_ = r.Close()
		return nil, err
	}
	fs := r.Storer.(*filesystem.Storage).Filesystem()
	for _, name := range []string{"info/grafts"} {
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
	gitDir, commonDir, err := repositoryDirs(r)
	if err != nil {
		_ = r.Close()
		return nil, err
	}
	cfg, err := r.Config()
	if err != nil {
		_ = r.Close()
		return nil, err
	}
	objectCache, err := configure(r, opts.Tuning, alternatesFS, cfg.Extensions.ObjectFormat)
	if err != nil {
		_ = r.Close()
		return nil, err
	}
	return &Repo{
		r: r, t: opts.Tuning, gitDir: gitDir, commonDir: commonDir,
		objectCache: objectCache, alternatesFS: alternatesFS,
		objectFormat: cfg.Extensions.ObjectFormat,
	}, nil
}

// Close releases the underlying go-git repository.
func (r *Repo) Close() error { return r.r.Close() }

// Repository returns the underlying go-git handle for callers that need
// direct storer access.
func (r *Repo) Repository() *gogit.Repository { return r.r }

// GitDir returns the worktree-specific Git directory.
func (r *Repo) GitDir() string { return r.gitDir }

// CommonDir returns the Git directory shared by linked worktrees.
func (r *Repo) CommonDir() string { return r.commonDir }

// Shallow reports whether the repository is a shallow clone.
func (r *Repo) Shallow() (bool, error) {
	shallow, err := r.r.Storer.Shallow()
	return len(shallow) != 0, err
}

// Roots returns every commit reachable from a ref, deduplicated.
func (r *Repo) Roots() ([]plumbing.Hash, error) {
	return historyRoots(r.r)
}

func repositoryDirs(r *gogit.Repository) (string, string, error) {
	storage, ok := r.Storer.(*filesystem.Storage)
	if !ok {
		return "", "", fmt.Errorf("history: unsupported storage %T", r.Storer)
	}
	fs := storage.Filesystem()
	objects, err := fs.Chroot("objects")
	if err != nil {
		return "", "", err
	}
	return filepath.Clean(fs.Root()), filepath.Clean(filepath.Dir(objects.Root())), nil
}

func rejectReplacementRefs(r *gogit.Repository) error {
	refs, err := r.References()
	if err != nil {
		return err
	}
	defer refs.Close()
	for {
		ref, err := refs.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if strings.HasPrefix(ref.Name().String(), "refs/replace/") {
			return fmt.Errorf("history: go-git does not support replacement refs")
		}
	}
}

func configure(
	r *gogit.Repository,
	t Tuning,
	alternatesFS billy.Filesystem,
	objectFormat formatcfg.ObjectFormat,
) (*objectCache, error) {
	if t.CacheBytes == 0 {
		t.CacheBytes = uint64(cache.DefaultMaxSize)
	}
	fs := r.Storer.(*filesystem.Storage).Filesystem()
	if t.Mmap {
		objects, err := fs.Chroot("objects")
		if err != nil {
			return nil, err
		}
		fs = dotgit.NewRepositoryFilesystem(
			osfs.New(fs.Root(), osfs.WithBoundOS(), osfs.WithMmap()),
			osfs.New(filepath.Dir(objects.Root()), osfs.WithBoundOS(), osfs.WithMmap()),
		)
	}
	if err := r.Close(); err != nil {
		return nil, err
	}
	objectCache := newObjectCache(cache.FileSize(t.CacheBytes), t.CacheShards)
	r.Storer = filesystem.NewStorageWithOptions(fs, objectCache, filesystem.Options{
		AlternatesFS:   alternatesFS,
		ObjectFormat:   objectFormat,
		UseInMemoryIdx: t.MemoryIndex,
	})
	return objectCache, nil
}

type objectCache struct {
	shards []*objectCacheShard
}

func newObjectCache(maxSize cache.FileSize, count int) *objectCache {
	if count < 1 {
		count = 1
	}
	result := &objectCache{shards: make([]*objectCacheShard, count)}
	perShard := maxSize / cache.FileSize(count)
	remainder := maxSize % cache.FileSize(count)
	for i := range result.shards {
		size := perShard
		if cache.FileSize(i) < remainder {
			size++
		}
		result.shards[i] = &objectCacheShard{
			maxSize: size,
			entries: make(map[plumbing.Hash]*list.Element),
		}
	}
	return result
}

func (c *objectCache) shard(hash plumbing.Hash) *objectCacheShard {
	return c.shards[int(hash.Bytes()[0])%len(c.shards)]
}

func (c *objectCache) Put(encoded plumbing.EncodedObject) {
	hash := encoded.Hash()
	c.shard(hash).put(hash, encoded)
}

func (c *objectCache) Get(hash plumbing.Hash) (plumbing.EncodedObject, bool) { //nolint:ireturn // cache.Object contract
	return c.shard(hash).get(hash)
}

func (c *objectCache) Clear() {
	for _, shard := range c.shards {
		shard.clear()
	}
}

type objectCacheShard struct {
	maxSize cache.FileSize
	size    cache.FileSize
	entries map[plumbing.Hash]*list.Element
	list    list.List
	mu      sync.Mutex
}

type objectCacheEntry struct {
	key    plumbing.Hash
	object plumbing.EncodedObject
}

func (c *objectCacheShard) put(key plumbing.Hash, object plumbing.EncodedObject) {
	size := cache.FileSize(object.Size())
	if size > c.maxSize {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if element := c.entries[key]; element != nil {
		entry := element.Value.(*objectCacheEntry)
		c.size += size - cache.FileSize(entry.object.Size())
		entry.object = object
		c.list.MoveToFront(element)
	} else {
		entry := &objectCacheEntry{key: key, object: object}
		c.entries[key] = c.list.PushFront(entry)
		c.size += size
	}
	for c.size > c.maxSize {
		element := c.list.Back()
		if element == nil {
			c.size = 0
			break
		}
		entry := element.Value.(*objectCacheEntry)
		c.size -= cache.FileSize(entry.object.Size())
		delete(c.entries, entry.key)
		c.list.Remove(element)
	}
}

func (c *objectCacheShard) get(key plumbing.Hash) (plumbing.EncodedObject, bool) { //nolint:ireturn // entries contain EncodedObject values
	c.mu.Lock()
	defer c.mu.Unlock()
	element := c.entries[key]
	if element == nil {
		return nil, false
	}
	c.list.MoveToFront(element)
	return element.Value.(*objectCacheEntry).object, true
}

func (c *objectCacheShard) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.size = 0
	c.entries = make(map[plumbing.Hash]*list.Element)
	c.list.Init()
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
