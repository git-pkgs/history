package history

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/storage/filesystem"
)

type blobTask struct {
	info filesystem.ObjectInfo
	oid  string
}

// WalkBlobs decodes every blob object in the repository exactly once and
// invokes visit from opts.Workers goroutines concurrently. visit must be
// safe for concurrent use. The returned int is the total blob count
// including those excluded by opts.Limit.
func (r *Repo) WalkBlobs(opts Options, visit func(Blob) error) (int, error) {
	if r.t.ObjectInfos {
		return r.walkBlobsObjectInfos(opts, visit)
	}
	return r.walkBlobsObjects(opts, visit)
}

func (r *Repo) walkBlobsObjectInfos(opts Options, visit func(Blob) error) (int, error) {
	storage, ok := r.r.Storer.(*filesystem.Storage)
	if !ok {
		return 0, fmt.Errorf("history: filesystem storage required for ObjectInfos")
	}
	iter, err := storage.IterObjectInfos(plumbing.BlobObject)
	if err != nil {
		return 0, err
	}
	defer iter.Close()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	batchSize := max(1, r.t.ObjectBatch)
	buffer := max(1, r.t.ObjectBuffer)
	tasks := make(chan []blobTask, (buffer+batchSize-1)/batchSize)
	var wg sync.WaitGroup
	for range opts.workers() {
		wg.Go(func() { readObjectInfoBatches(ctx, cancel, storage, tasks, visit) })
	}
	total, err := enumerateObjectInfos(ctx, iter, opts, batchSize, tasks)
	close(tasks)
	wg.Wait()
	if cause := context.Cause(ctx); cause != nil && cause != context.Canceled {
		return total, cause
	}
	return total, err
}

func readObjectInfoBatches(ctx context.Context, fail context.CancelCauseFunc, storage *filesystem.Storage, tasks <-chan []blobTask, visit func(Blob) error) {
	reader := storage.NewObjectInfoReader()
	defer func() {
		if err := reader.Close(); err != nil {
			fail(err)
		}
	}()
	for batch := range tasks {
		for _, task := range batch {
			if ctx.Err() != nil {
				return
			}
			if err := visitObjectInfo(reader, task, visit); err != nil {
				fail(err)
				return
			}
		}
	}
}

func visitObjectInfo(reader *filesystem.ObjectInfoReader, task blobTask, visit func(Blob) error) error {
	obj, err := reader.EncodedObject(task.info)
	if err != nil {
		return err
	}
	if obj.Size() != task.info.Size {
		return fmt.Errorf("object %s size changed during enumeration: %d != %d", task.info.Hash, obj.Size(), task.info.Size)
	}
	data, err := readBlob(obj)
	if err != nil {
		return err
	}
	return visit(Blob{OID: task.oid, Size: task.info.Size, Data: data})
}

func enumerateObjectInfos(ctx context.Context, iter filesystem.ObjectInfoIter, opts Options, batchSize int, tasks chan<- []blobTask) (int, error) {
	total := 0
	batch := make([]blobTask, 0, batchSize)
	flush := func() bool {
		if len(batch) == 0 {
			return true
		}
		select {
		case tasks <- batch:
			batch = make([]blobTask, 0, batchSize)
			return true
		case <-ctx.Done():
			return false
		}
	}
	for {
		info, err := iter.Next()
		if err == io.EOF {
			flush()
			return total, nil
		}
		if err != nil {
			return total, err
		}
		total++
		oid := info.Hash.String()
		if info.Size > opts.limit(oid) {
			opts.skip(oid)
			continue
		}
		batch = append(batch, blobTask{info: info, oid: oid})
		if len(batch) == batchSize && !flush() {
			return total, nil
		}
	}
}

func (r *Repo) walkBlobsObjects(opts Options, visit func(Blob) error) (int, error) {
	iter, err := r.r.Storer.IterEncodedObjects(plumbing.BlobObject)
	if err != nil {
		return 0, err
	}
	defer iter.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	objects := make(chan plumbing.EncodedObject)
	var wg sync.WaitGroup
	var once sync.Once
	var readErr error
	for range opts.workers() {
		wg.Go(func() {
			for obj := range objects {
				data, err := readBlob(obj)
				if err != nil {
					once.Do(func() { readErr = err; cancel() })
					return
				}
				if err := visit(Blob{OID: obj.Hash().String(), Size: obj.Size(), Data: data}); err != nil {
					once.Do(func() { readErr = err; cancel() })
					return
				}
			}
		})
	}
	total := 0
enumerate:
	for {
		var obj plumbing.EncodedObject
		obj, err = iter.Next()
		if err == io.EOF {
			err = nil
			break
		}
		if err != nil {
			break
		}
		total++
		oid := obj.Hash().String()
		if obj.Size() > opts.limit(oid) {
			opts.skip(oid)
			continue
		}
		select {
		case objects <- obj:
		case <-ctx.Done():
			break enumerate
		}
	}
	close(objects)
	wg.Wait()
	if readErr != nil {
		return total, readErr
	}
	return total, err
}

func readBlob(obj plumbing.EncodedObject) ([]byte, error) {
	if memory, ok := obj.(interface{ Bytes() []byte }); ok {
		data := memory.Bytes()
		if int64(len(data)) != obj.Size() {
			return nil, fmt.Errorf("memory object size mismatch: %d != %d", len(data), obj.Size())
		}
		return data, nil
	}
	reader, err := obj.Reader()
	if err != nil {
		return nil, err
	}
	data := make([]byte, obj.Size())
	_, err = io.ReadFull(reader, data)
	closeErr := reader.Close()
	if err != nil {
		return nil, err
	}
	return data, closeErr
}
