package history

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// Commit is one commit and its first-parent file changes.
type Commit struct {
	Object  *object.Commit
	Changes []Change
}

// CommitOptions controls an ordered commit walk.
type CommitOptions struct {
	// Hashes is an exact commit sequence. Ref and Since are ignored when set.
	Hashes []plumbing.Hash
	// Ref is the revision to walk. An empty value uses HEAD.
	Ref string
	// Since excludes this revision and its ancestors.
	Since string
	// Merges includes first-parent changes for merge commits.
	Merges bool
	// Workers is the number of concurrent commit readers and diff workers.
	Workers int
	// PathFilter retains changes whose path returns true.
	PathFilter func(string) bool
}

func (o CommitOptions) workers() int {
	if o.Workers < 1 {
		return 1
	}
	return o.Workers
}

// WalkCommits visits commits oldest-first. The callback runs serially in the
// requested order, including for commits with no retained changes.
func (r *Repo) WalkCommits(opts CommitOptions, visit func(Commit) error) error {
	hashes, boundaries, err := r.commitHashes(opts)
	if err != nil {
		return err
	}
	if opts.workers() == 1 {
		for _, hash := range hashes {
			commit, err := r.readCommit(context.Background(), hash, boundaries, opts)
			if err != nil {
				return err
			}
			if err := visit(commit); err != nil {
				return err
			}
		}
		return nil
	}
	return r.walkCommitsParallel(hashes, boundaries, opts, visit)
}

// CommitHashes returns commits oldest-first without reading their trees.
func (r *Repo) CommitHashes(opts CommitOptions) ([]plumbing.Hash, error) {
	hashes, _, err := r.commitHashes(opts)
	return hashes, err
}

func (r *Repo) commitHashes(opts CommitOptions) ([]plumbing.Hash, map[plumbing.Hash]bool, error) {
	boundaries, boundaryParents, err := r.shallowBoundaries()
	if err != nil {
		return nil, nil, err
	}
	if len(opts.Hashes) > 0 {
		return slices.Clone(opts.Hashes), boundaries, nil
	}

	from, err := r.resolveRevision(opts.Ref)
	if opts.Ref == "" && errors.Is(err, plumbing.ErrReferenceNotFound) {
		return nil, boundaries, nil
	}
	if err != nil {
		return nil, nil, err
	}
	start, err := r.r.CommitObject(from)
	if err != nil {
		return nil, nil, err
	}

	excluded := make(map[plumbing.Hash]bool)
	if opts.Since != "" {
		since, err := r.resolveRevision(opts.Since)
		if err != nil {
			return nil, nil, err
		}
		commit, err := r.r.CommitObject(since)
		if err != nil {
			return nil, nil, err
		}
		iter := object.NewCommitPreorderIter(commit, nil, boundaryParents)
		err = iter.ForEach(func(commit *object.Commit) error {
			excluded[commit.Hash] = true
			return nil
		})
		iter.Close()
		if err != nil {
			return nil, nil, err
		}
	}

	iter := object.NewCommitIterCTime(start, excluded, boundaryParents)
	defer iter.Close()
	var hashes []plumbing.Hash
	if err := iter.ForEach(func(commit *object.Commit) error {
		hashes = append(hashes, commit.Hash)
		return nil
	}); err != nil {
		return nil, nil, err
	}
	slices.Reverse(hashes)
	return hashes, boundaries, nil
}

func (r *Repo) resolveRevision(revision string) (plumbing.Hash, error) {
	if revision == "" {
		head, err := r.r.Head()
		if err != nil {
			return plumbing.ZeroHash, err
		}
		return head.Hash(), nil
	}
	hash, err := r.r.ResolveRevision(plumbing.Revision(revision))
	if err != nil {
		return plumbing.ZeroHash, err
	}
	return *hash, nil
}

func (r *Repo) shallowBoundaries() (map[plumbing.Hash]bool, []plumbing.Hash, error) {
	shallow, err := r.r.Storer.Shallow()
	if err != nil {
		return nil, nil, err
	}
	boundaries := make(map[plumbing.Hash]bool, len(shallow))
	var parents []plumbing.Hash
	for _, hash := range shallow {
		boundaries[hash] = true
		commit, err := r.r.CommitObject(hash)
		if err != nil {
			return nil, nil, err
		}
		parents = append(parents, commit.ParentHashes...)
	}
	return boundaries, parents, nil
}

func (r *Repo) readCommit(
	ctx context.Context,
	hash plumbing.Hash,
	boundaries map[plumbing.Hash]bool,
	opts CommitOptions,
) (Commit, error) {
	commit, err := r.r.CommitObject(hash)
	if err != nil {
		return Commit{}, err
	}
	result := Commit{
		Object: commit,
	}
	if len(commit.ParentHashes) > 1 && !opts.Merges {
		return result, nil
	}

	toHash := commit.TreeHash
	fromHash := plumbing.ZeroHash
	if len(commit.ParentHashes) > 0 && !boundaries[hash] {
		parent, err := commit.Parent(0)
		if err != nil {
			return Commit{}, err
		}
		fromHash = parent.TreeHash
	}
	var hashString, zero string
	date := commit.Author.When.Format(time.RFC3339)
	subject := CommitSubject(commit.Message)
	merge := len(commit.ParentHashes) > 1
	err = object.WalkTreeDiffContext(ctx, r.r.Storer, fromHash, toHash, func(entry object.TreeDiffChange) error {
		if opts.PathFilter != nil && !opts.PathFilter(entry.Path) {
			return nil
		}
		if hashString == "" {
			hashString = hash.String()
			zero = strings.Repeat("0", len(hashString))
		}
		oldOID, newOID := zero, zero
		if !entry.From.Hash.IsZero() {
			oldOID = entry.From.Hash.String()
		}
		if !entry.To.Hash.IsZero() {
			newOID = entry.To.Hash.String()
		}
		result.Changes = append(result.Changes, Change{
			Commit: hashString, Date: date, Subject: subject,
			Path: entry.Path, OldOID: oldOID, NewOID: newOID,
			OldMode: formatMode(entry.From.Mode), NewMode: formatMode(entry.To.Mode),
			Merge: merge,
		})
		return nil
	})
	if err != nil {
		return Commit{}, err
	}
	return result, nil
}

type commitTask struct {
	hash   plumbing.Hash
	result chan commitResult
}

type commitResult struct {
	commit Commit
	err    error
}

func (r *Repo) walkCommitsParallel(
	hashes []plumbing.Hash,
	boundaries map[plumbing.Hash]bool,
	opts CommitOptions,
	visit func(Commit) error,
) error {
	ctx, cancel := context.WithCancel(context.Background())
	tasks := make(chan commitTask, opts.workers())
	var wg sync.WaitGroup
	defer func() {
		cancel()
		close(tasks)
		wg.Wait()
	}()
	for range opts.workers() {
		wg.Go(func() {
			for task := range tasks {
				commit, err := r.readCommit(ctx, task.hash, boundaries, opts)
				task.result <- commitResult{commit: commit, err: err}
			}
		})
	}

	window := historyLookahead * opts.workers()
	pending := make([]commitTask, 0, window)
	next := 0
	fill := func() {
		for len(pending) < window && next < len(hashes) {
			task := commitTask{hash: hashes[next], result: make(chan commitResult, 1)}
			pending = append(pending, task)
			tasks <- task
			next++
		}
	}
	fill()
	for len(pending) > 0 {
		result := <-pending[0].result
		if result.err != nil {
			return result.err
		}
		if err := visit(result.commit); err != nil {
			return err
		}
		pending = pending[1:]
		fill()
	}
	return nil
}
