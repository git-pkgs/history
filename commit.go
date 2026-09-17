package history

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sync"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

type objectID [sha256.Size]byte

const rawObjectBufferSize = 8 << 10

var rawObjectReaderPool = sync.Pool{
	New: func() any { return bufio.NewReaderSize(nil, rawObjectBufferSize) },
}

// VisitCommitTrees walks every commit reachable from the repository's refs
// and invokes visit with each commit's root tree hash. Commits beyond a
// shallow boundary are treated as having no parents.
func (r *Repo) VisitCommitTrees(visit func(plumbing.Hash) error) error {
	roots, err := historyRoots(r.r)
	if err != nil {
		return err
	}
	shallow, err := r.r.Storer.Shallow()
	if err != nil {
		return err
	}
	boundary := make(map[plumbing.Hash]bool, len(shallow))
	for _, hash := range shallow {
		boundary[hash] = true
	}
	return visitCommitTrees(r.r, roots, boundary, visit)
}

func visitCommitTrees(r *gogit.Repository, roots []plumbing.Hash, boundary map[plumbing.Hash]bool, visit func(plumbing.Hash) error) error {
	seen := make(map[plumbing.Hash]struct{})
	stack := append([]plumbing.Hash(nil), roots...)
	for len(stack) > 0 {
		last := len(stack) - 1
		hash := stack[last]
		stack = stack[:last]
		if _, ok := seen[hash]; ok {
			continue
		}
		tree, err := readCommitTreeAndParents(r, hash, !boundary[hash], &stack)
		if err != nil {
			return err
		}
		seen[hash] = struct{}{}
		if err := visit(tree); err != nil {
			return err
		}
	}
	return nil
}

func readCommitTreeAndParents(r *gogit.Repository, hash plumbing.Hash, includeParents bool, parents *[]plumbing.Hash) (tree plumbing.Hash, resultErr error) {
	encoded, err := r.Storer.EncodedObject(plumbing.CommitObject, hash)
	if err != nil {
		return tree, err
	}
	if encoded.Type() != plumbing.CommitObject {
		return tree, fmt.Errorf("object %s is %s, want commit", hash, encoded.Type())
	}
	if memory, ok := encoded.(*plumbing.MemoryObject); ok {
		writer := commitLinksWriter{hash: hash, includeParents: includeParents, parents: parents}
		_, err := memory.WriteTo(&writer)
		return writer.tree, err
	}
	reader, err := encoded.Reader()
	if err != nil {
		return tree, err
	}
	defer func() {
		if err := reader.Close(); resultErr == nil {
			resultErr = err
		}
	}()
	buffer := rawObjectReaderPool.Get().(*bufio.Reader)
	buffer.Reset(reader)
	defer func() {
		buffer.Reset(nil)
		rawObjectReaderPool.Put(buffer)
	}()

	line, readErr := buffer.ReadSlice('\n')
	if readErr != nil && (readErr != io.EOF || len(line) == 0) {
		return tree, fmt.Errorf("commit %s tree: %w", hash, readErr)
	}
	line = bytes.TrimSuffix(line, []byte{'\n'})
	value, ok := bytes.CutPrefix(line, []byte("tree "))
	if !ok {
		return tree, fmt.Errorf("commit %s has no leading tree header", hash)
	}
	tree, err = parseCommitObjectID(value, hash.Size())
	if err != nil {
		return tree, fmt.Errorf("commit %s tree: %w", hash, err)
	}
	if readErr == io.EOF {
		return tree, nil
	}

	for {
		line, readErr = buffer.ReadSlice('\n')
		if readErr != nil && (readErr != io.EOF || len(line) == 0) {
			return tree, fmt.Errorf("commit %s parent: %w", hash, readErr)
		}
		line = bytes.TrimSuffix(line, []byte{'\n'})
		value, ok = bytes.CutPrefix(line, []byte("parent "))
		if !ok {
			return tree, nil
		}
		parent, err := parseCommitObjectID(value, hash.Size())
		if err != nil {
			return tree, fmt.Errorf("commit %s parent: %w", hash, err)
		}
		if includeParents {
			*parents = append(*parents, parent)
		}
		if readErr == io.EOF {
			return tree, nil
		}
	}
}

type commitLinksWriter struct {
	hash           plumbing.Hash
	tree           plumbing.Hash
	includeParents bool
	parents        *[]plumbing.Hash
}

func (w *commitLinksWriter) Write(data []byte) (int, error) {
	line, rest, _ := bytes.Cut(data, []byte{'\n'})
	value, ok := bytes.CutPrefix(line, []byte("tree "))
	if !ok {
		return 0, fmt.Errorf("commit %s has no leading tree header", w.hash)
	}
	tree, err := parseCommitObjectID(value, w.hash.Size())
	if err != nil {
		return 0, fmt.Errorf("commit %s tree: %w", w.hash, err)
	}
	w.tree = tree
	for len(rest) > 0 {
		line, next, _ := bytes.Cut(rest, []byte{'\n'})
		value, ok = bytes.CutPrefix(line, []byte("parent "))
		if !ok {
			break
		}
		parent, err := parseCommitObjectID(value, w.hash.Size())
		if err != nil {
			return 0, fmt.Errorf("commit %s parent: %w", w.hash, err)
		}
		if w.includeParents {
			*w.parents = append(*w.parents, parent)
		}
		rest = next
	}
	return len(data), nil
}

func parseCommitObjectID(value []byte, size int) (plumbing.Hash, error) {
	if len(value) != hex.EncodedLen(size) {
		return plumbing.ZeroHash, fmt.Errorf("invalid object ID %q", value)
	}
	var decoded objectID
	if _, err := hex.Decode(decoded[:size], value); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("invalid object ID %q: %w", value, err)
	}
	hash, ok := plumbing.FromBytes(decoded[:size])
	if !ok {
		return plumbing.ZeroHash, fmt.Errorf("invalid object ID %q", value)
	}
	return hash, nil
}
