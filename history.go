// Package history walks the full commit history and blob store of a git
// repository using go-git, without invoking the git binary. It is the shared
// object-graph layer under git-spdx and secrets.
package history

import "github.com/go-git/go-git/v6/plumbing/cache"

const (
	readerBufferSize = 1 << 16
	historyLookahead = 2

	defaultObjectBuffer = 64
	defaultObjectBatch  = 64
)

// Change is one file-level modification in a single commit, matching a raw
// entry from git log --raw. OIDs and modes are hex/octal strings so callers
// can round-trip them through the git-binary backend unchanged.
type Change struct {
	Commit, Date, Subject string
	OldOID, NewOID, Path  string
	OldMode, NewMode      string
	Merge                 bool
}

// Blob is one decoded object from the repository's blob store.
type Blob struct {
	OID  string
	Size int64
	Data []byte
}

// Tuning controls go-git storage behaviour. The zero value matches go-git
// defaults; only ObjectInfos defaults to true when applied.
type Tuning struct {
	MemoryIndex   bool
	Mmap          bool
	CacheBytes    uint64
	CacheShards   int
	ObjectInfos   bool
	ObjectBuffer  int
	ObjectBatch   int
	KlauspostZlib bool
}

// DefaultTuning returns the settings git-spdx converged on after benchmarking.
func DefaultTuning() Tuning {
	return Tuning{
		Mmap:          true,
		CacheBytes:    uint64(cache.DefaultMaxSize),
		CacheShards:   1,
		ObjectInfos:   true,
		ObjectBuffer:  defaultObjectBuffer,
		ObjectBatch:   defaultObjectBatch,
		KlauspostZlib: true,
	}
}

// Options controls a single walk.
type Options struct {
	// Merges includes merge commits in WalkChanges output.
	Merges bool
	// Workers is the number of concurrent reader/differ goroutines. Zero means one.
	Workers int
	// Limit returns the maximum size to read for a blob; larger blobs are
	// reported to Skip and not visited. Nil means no limit.
	Limit func(oid string) int64
	// Skip is called for blobs excluded by Limit. Nil is a no-op.
	Skip func(oid string)
}

func (o Options) workers() int {
	if o.Workers < 1 {
		return 1
	}
	return o.Workers
}

func (o Options) limit(oid string) int64 {
	if o.Limit == nil {
		return 1<<63 - 1
	}
	return o.Limit(oid)
}

func (o Options) skip(oid string) {
	if o.Skip != nil {
		o.Skip(oid)
	}
}
