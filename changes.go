package history

import (
	"context"
	"errors"
	"io"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/object/commitgraph"
)

type changesResult struct {
	changes []Change
	err     error
}

type historyRootNode struct {
	nodes  []commitgraph.CommitNode
	hashes []plumbing.Hash
}

func (n *historyRootNode) ID() plumbing.Hash { return plumbing.ZeroHash }
func (n *historyRootNode) Tree() (*object.Tree, error) {
	return nil, errors.New("history root has no tree")
}
func (n *historyRootNode) CommitTime() time.Time { return time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC) }
func (n *historyRootNode) NumParents() int       { return len(n.nodes) }
func (n *historyRootNode) ParentNodes() commitgraph.CommitNodeIter { //nolint:ireturn // commitgraph.CommitNode
	return &historyRootIter{nodes: n.nodes}
}
func (n *historyRootNode) ParentHashes() []plumbing.Hash { return n.hashes }
func (n *historyRootNode) Generation() uint64            { return math.MaxUint64 }
func (n *historyRootNode) GenerationV2() uint64          { return math.MaxUint64 }
func (n *historyRootNode) Commit() (*object.Commit, error) {
	return nil, errors.New("history root has no commit")
}

func (n *historyRootNode) ParentNode(i int) (commitgraph.CommitNode, error) { //nolint:ireturn // commitgraph.CommitNode
	if i < 0 || i >= len(n.nodes) {
		return nil, object.ErrParentNotFound
	}
	return n.nodes[i], nil
}

type historyRootIter struct {
	nodes []commitgraph.CommitNode
	next  int
}

func (i *historyRootIter) Next() (commitgraph.CommitNode, error) { //nolint:ireturn // commitgraph.CommitNodeIter
	if i.next == len(i.nodes) {
		return nil, io.EOF
	}
	n := i.nodes[i.next]
	i.next++
	return n, nil
}

func (i *historyRootIter) ForEach(visit func(commitgraph.CommitNode) error) error {
	for {
		n, err := i.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := visit(n); err != nil {
			return err
		}
	}
}

func (i *historyRootIter) Close() {}

type shallowCommitNodeIndex struct {
	commitgraph.CommitNodeIndex
	boundary map[plumbing.Hash]bool
}

func (i *shallowCommitNodeIndex) Get(hash plumbing.Hash) (commitgraph.CommitNode, error) { //nolint:ireturn // commitgraph.CommitNodeIndex
	n, err := i.CommitNodeIndex.Get(hash)
	if err != nil || !i.boundary[hash] {
		return n, err
	}
	return &shallowCommitNode{CommitNode: n}, nil
}

type shallowCommitNode struct {
	commitgraph.CommitNode
}

func (n *shallowCommitNode) NumParents() int { return 0 }

func (n *shallowCommitNode) ParentNodes() commitgraph.CommitNodeIter { //nolint:ireturn // commitgraph.CommitNode
	return &historyRootIter{}
}

func (n *shallowCommitNode) ParentNode(int) (commitgraph.CommitNode, error) { //nolint:ireturn // commitgraph.CommitNode
	return nil, object.ErrParentNotFound
}
func (n *shallowCommitNode) ParentHashes() []plumbing.Hash { return nil }

type treeLoad struct {
	ready chan struct{}
	tree  *object.Tree
	err   error
}

type treeCache struct {
	mu    sync.Mutex
	trees map[plumbing.Hash]*treeLoad
}

func newTreeCache() *treeCache {
	return &treeCache{trees: make(map[plumbing.Hash]*treeLoad)}
}

func (c *treeCache) load(hash plumbing.Hash, load func() (*object.Tree, error)) (*object.Tree, error) {
	c.mu.Lock()
	entry := c.trees[hash]
	if entry == nil {
		entry = &treeLoad{ready: make(chan struct{})}
		c.trees[hash] = entry
		c.mu.Unlock()
		entry.tree, entry.err = load()
		close(entry.ready)
		return entry.tree, entry.err
	}
	c.mu.Unlock()
	<-entry.ready
	return entry.tree, entry.err
}

func (c *treeCache) release(hash plumbing.Hash) {
	c.mu.Lock()
	delete(c.trees, hash)
	c.mu.Unlock()
}

// WalkChanges visits every file-level change in date order across all refs.
// visit is called from a single goroutine in commit order regardless of
// opts.Workers.
func (r *Repo) WalkChanges(opts Options, visit func(Change)) error {
	rootHashes, err := historyRoots(r.r)
	if err != nil {
		return err
	}
	if len(rootHashes) == 0 {
		return nil
	}
	shallow, err := r.r.Storer.Shallow()
	if err != nil {
		return err
	}
	boundary := make(map[plumbing.Hash]bool, len(shallow))
	for _, hash := range shallow {
		boundary[hash] = true
	}
	index := &shallowCommitNodeIndex{
		CommitNodeIndex: commitgraph.NewObjectCommitNodeIndex(r.r.Storer),
		boundary:        boundary,
	}
	root := &historyRootNode{}
	seen := make(map[plumbing.Hash]bool, len(rootHashes))
	for _, hash := range rootHashes {
		if seen[hash] {
			continue
		}
		seen[hash] = true
		node, err := index.Get(hash)
		if err != nil {
			return err
		}
		root.nodes = append(root.nodes, node)
		root.hashes = append(root.hashes, hash)
	}
	iter := commitgraph.NewCommitNodeIterDateOrder(root, nil, nil)
	defer iter.Close()
	if _, err := iter.Next(); err != nil {
		return err
	}
	cache := newTreeCache()
	if opts.workers() > 1 {
		return visitChangesParallel(iter, cache, opts.Merges, visit, opts.workers())
	}
	for {
		node, err := iter.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if opts.Merges || node.NumParents() < 2 {
			if err := visitCommitNodeChanges(context.Background(), node, cache, visit); err != nil {
				return err
			}
		}
		cache.release(node.ID())
	}
}

func visitCommitNodeChanges(ctx context.Context, node commitgraph.CommitNode, cache *treeCache, visit func(Change)) error {
	commit, err := node.Commit()
	if err != nil {
		return err
	}
	to, err := cache.load(node.ID(), node.Tree)
	if err != nil {
		return err
	}
	var from *object.Tree
	parents := node.ParentHashes()
	if len(parents) > 0 {
		parent, err := node.ParentNode(0)
		if err != nil {
			return err
		}
		from, err = cache.load(parents[0], parent.Tree)
		if err != nil {
			return err
		}
	}
	changes, err := object.DiffTreeContext(ctx, from, to)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		return nil
	}
	hash := node.ID().String()
	zero := strings.Repeat("0", len(hash))
	date := commit.Author.When.Format(time.RFC3339)
	subject := CommitSubject(commit.Message)
	merge := len(parents) > 1
	for _, entry := range changes {
		path := entry.To.Name
		if path == "" {
			path = entry.From.Name
		}
		oldOID := formatOID(entry.From.TreeEntry, zero)
		newOID := formatOID(entry.To.TreeEntry, zero)
		visit(Change{
			Commit: hash, Date: date, Subject: subject,
			Path: path, OldOID: oldOID, NewOID: newOID,
			OldMode: formatMode(entry.From.TreeEntry.Mode), NewMode: formatMode(entry.To.TreeEntry.Mode),
			Merge: merge,
		})
	}
	return nil
}

func formatOID(entry object.TreeEntry, zero string) string {
	if entry.Mode == 0 {
		return zero
	}
	return entry.Hash.String()
}

func formatMode(mode filemode.FileMode) string {
	const octalDigitMask = 7

	var encoded [6]byte
	for i := len(encoded) - 1; i >= 0; i-- {
		encoded[i] = '0' + byte(mode&octalDigitMask)
		mode >>= 3
	}
	return string(encoded[:])
}

type streamTask struct {
	node   commitgraph.CommitNode
	skip   bool
	result chan changesResult
}

func visitChangesParallel(iter commitgraph.CommitNodeIter, cache *treeCache, merges bool, visit func(Change), workers int) error {
	ctx, cancel := context.WithCancel(context.Background())
	tasks := make(chan streamTask, workers)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		close(tasks)
		wg.Wait()
	}()
	for range workers {
		wg.Go(func() {
			for task := range tasks {
				var result changesResult
				if !task.skip {
					result.err = visitCommitNodeChanges(ctx, task.node, cache, func(c Change) {
						result.changes = append(result.changes, c)
					})
				}
				task.result <- result
			}
		})
	}
	window := historyLookahead * workers
	pending := make([]streamTask, 0, window)
	done := false
	fill := func() error {
		for len(pending) < window && !done {
			node, err := iter.Next()
			if err == io.EOF {
				done = true
				break
			}
			if err != nil {
				return err
			}
			task := streamTask{
				node:   node,
				skip:   !merges && node.NumParents() >= 2,
				result: make(chan changesResult, 1),
			}
			pending = append(pending, task)
			tasks <- task
		}
		return nil
	}
	if err := fill(); err != nil {
		return err
	}
	for len(pending) > 0 {
		task := pending[0]
		result := <-task.result
		if result.err != nil {
			return result.err
		}
		for _, c := range result.changes {
			visit(c)
		}
		cache.release(task.node.ID())
		pending = pending[1:]
		if err := fill(); err != nil {
			return err
		}
	}
	return nil
}
