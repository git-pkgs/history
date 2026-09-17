package history

import (
	"bufio"
	"bytes"
	"fmt"
	"io"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
)

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
	if memory, ok := encoded.(*plumbing.MemoryObject); ok {
		return walkTreeEntryBytes(hash, memory.Bytes(), visit)
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
		mode, err := filemode.FromBytes(modeBytes[:len(modeBytes)-1])
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

func walkTreeEntryBytes(hash plumbing.Hash, data []byte, visit func(TreeEntry) error) error {
	for len(data) > 0 {
		modeEnd := bytes.IndexByte(data, ' ')
		if modeEnd < 0 {
			return fmt.Errorf("tree %s mode: %w", hash, io.ErrUnexpectedEOF)
		}
		mode, err := filemode.FromBytes(data[:modeEnd])
		if err != nil {
			return fmt.Errorf("tree %s mode: %w", hash, err)
		}
		data = data[modeEnd+1:]
		nameEnd := bytes.IndexByte(data, 0)
		if nameEnd < 0 {
			return fmt.Errorf("tree %s name: %w", hash, io.ErrUnexpectedEOF)
		}
		name := data[:nameEnd]
		if len(name) == 0 {
			return fmt.Errorf("tree %s has an empty filename", hash)
		}
		data = data[nameEnd+1:]
		if len(data) < hash.Size() {
			return fmt.Errorf("tree %s object ID: %w", hash, io.ErrUnexpectedEOF)
		}
		oid, ok := plumbing.FromBytes(data[:hash.Size()])
		if !ok {
			return fmt.Errorf("tree %s has an invalid object ID", hash)
		}
		if err := visit(TreeEntry{Name: name, Hash: oid, Mode: mode}); err != nil {
			return err
		}
		data = data[hash.Size():]
	}
	return nil
}
