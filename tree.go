package history

import (
	"bufio"
	"fmt"
	"io"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
)

const treeModeBase = 8

// TreeEntry is one immediate child of a Git tree. Name is valid only until
// the visitor returns and must be copied if retained.
type TreeEntry struct {
	Name []byte
	Hash plumbing.Hash
	Mode filemode.FileMode
}

// WalkTreeEntries visits the immediate children of a tree without
// materializing an object.Tree.
func (r *Repo) WalkTreeEntries(hash plumbing.Hash, visit func(TreeEntry) error) (resultErr error) {
	encoded, err := r.r.Storer.EncodedObject(plumbing.TreeObject, hash)
	if err != nil {
		return err
	}
	if encoded.Type() != plumbing.TreeObject {
		return fmt.Errorf("object %s is %s, want tree", hash, encoded.Type())
	}
	reader, err := encoded.Reader()
	if err != nil {
		return err
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

	for {
		modeBytes, err := buffer.ReadSlice(' ')
		if err == io.EOF && len(modeBytes) == 0 {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tree %s mode: %w", hash, err)
		}
		mode, err := parseTreeMode(modeBytes[:len(modeBytes)-1])
		if err != nil {
			return fmt.Errorf("tree %s mode: %w", hash, err)
		}
		name, err := buffer.ReadSlice(0)
		if err != nil {
			return fmt.Errorf("tree %s name: %w", hash, err)
		}
		name = name[:len(name)-1]
		if len(name) == 0 {
			return fmt.Errorf("tree %s has an empty filename", hash)
		}
		oidBytes, err := buffer.Peek(hash.Size())
		if err != nil {
			return fmt.Errorf("tree %s object ID: %w", hash, err)
		}
		oid, ok := plumbing.FromBytes(oidBytes)
		if !ok {
			return fmt.Errorf("tree %s has an invalid object ID", hash)
		}
		if err := visit(TreeEntry{Name: name, Hash: oid, Mode: mode}); err != nil {
			return err
		}
		if _, err := buffer.Discard(hash.Size()); err != nil {
			return fmt.Errorf("tree %s object ID: %w", hash, err)
		}
	}
}

func parseTreeMode(data []byte) (filemode.FileMode, error) {
	if len(data) == 0 || len(data) > 7 {
		return filemode.Empty, fmt.Errorf("invalid mode %q", data)
	}
	var mode uint32
	for _, digit := range data {
		if digit < '0' || digit > '7' {
			return filemode.Empty, fmt.Errorf("invalid mode %q", data)
		}
		mode = mode*treeModeBase + uint32(digit-'0')
	}
	return filemode.FileMode(mode), nil
}
