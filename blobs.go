package history

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/storage/filesystem"
	"github.com/go-git/go-git/v6/storage/filesystem/dotgit"
)

type blobTask struct {
	info blobObjectInfo
	oid  string
}

// WalkBlobs decodes every blob object in the repository exactly once and
// invokes visit from opts.Workers goroutines concurrently. visit must be
// safe for concurrent use. The returned int is the total blob count
// including those excluded by opts.Limit.
func (r *Repo) WalkBlobs(opts BlobOptions, visit func(Blob) error) (total int, resultErr error) {
	stores, err := r.objectStorages()
	if err != nil {
		return 0, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, closeObjectStorages(stores[1:]))
	}()

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	tasks := make(chan []blobTask, max(1, defaultObjectBuffer/defaultObjectBatch))
	var wg sync.WaitGroup
	for range opts.workers() {
		wg.Go(func() { r.readBlobTasks(ctx, cancel, tasks, visit) })
	}
	total, err = r.enumerateBlobTasks(ctx, stores, opts, tasks)
	close(tasks)
	wg.Wait()
	if cause := context.Cause(ctx); cause != nil {
		return total, cause
	}
	return total, err
}

func (r *Repo) objectStorages() ([]*filesystem.Storage, error) {
	primary, ok := r.r.Storer.(*filesystem.Storage)
	if !ok {
		return nil, fmt.Errorf("history: unsupported storage %T", r.r.Storer)
	}
	stores := []*filesystem.Storage{primary}
	options := dotgit.Options{
		AlternatesFS: r.alternatesFS,
		ObjectFormat: r.objectFormat,
	}
	queue := []*dotgit.DotGit{dotgit.NewWithOptions(primary.Filesystem(), options)}
	seen := map[string]struct{}{filepath.Clean(primary.Filesystem().Root()): {}}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		alternates, err := current.Alternates()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, errors.Join(err, closeObjectStorages(stores[1:]))
		}
		for _, alternate := range alternates {
			fs := alternate.Fs()
			root := filepath.Clean(fs.Root())
			if _, ok := seen[root]; ok {
				continue
			}
			seen[root] = struct{}{}
			stores = append(stores, filesystem.NewStorageWithOptions(fs, r.objectCache, filesystem.Options{
				AlternatesFS:   r.alternatesFS,
				ObjectFormat:   r.objectFormat,
				UseInMemoryIdx: r.t.MemoryIndex,
			}))
			queue = append(queue, dotgit.NewWithOptions(fs, options))
		}
	}
	return stores, nil
}

func closeObjectStorages(stores []*filesystem.Storage) error {
	var result error
	for _, store := range stores {
		result = errors.Join(result, store.Close())
	}
	return result
}

func (r *Repo) readBlobTasks(
	ctx context.Context,
	fail context.CancelCauseFunc,
	tasks <-chan []blobTask,
	visit func(Blob) error,
) {
	reader := newBlobObjectReader()
	defer func() {
		if err := reader.close(); err != nil {
			fail(err)
		}
	}()
	for batch := range tasks {
		reader.clear()
		for _, task := range batch {
			if ctx.Err() != nil {
				return
			}
			data, err := reader.read(task.info)
			if err == nil {
				err = visit(Blob{OID: task.oid, Size: task.info.size, Data: data})
			}
			if err != nil {
				fail(err)
				return
			}
		}
	}
}

func sendBlobBatch(ctx context.Context, tasks chan<- []blobTask, batch []blobTask) bool {
	if len(batch) == 0 {
		return true
	}
	select {
	case tasks <- batch:
		return true
	case <-ctx.Done():
		return false
	}
}

func appendBlobTask(
	ctx context.Context,
	tasks chan<- []blobTask,
	batch []blobTask,
	task blobTask,
) ([]blobTask, bool) {
	batch = append(batch, task)
	if len(batch) < defaultObjectBatch {
		return batch, true
	}
	if !sendBlobBatch(ctx, tasks, batch) {
		return nil, false
	}
	return make([]blobTask, 0, defaultObjectBatch), true

}

func (r *Repo) enumerateBlobTasks(
	ctx context.Context,
	stores []*filesystem.Storage,
	opts BlobOptions,
	tasks chan<- []blobTask,
) (int, error) {
	infos, err := blobObjectInfos(stores, r.objectFormat)
	if err != nil {
		return 0, err
	}
	batch := make([]blobTask, 0, defaultObjectBatch)
	for index, info := range infos {
		oid := info.hash.String()
		if info.size > opts.limit(oid) {
			opts.skip(oid)
			continue
		}
		var ok bool
		batch, ok = appendBlobTask(ctx, tasks, batch, blobTask{info: info, oid: oid})
		if !ok {
			return index + 1, nil
		}
	}
	sendBlobBatch(ctx, tasks, batch)
	return len(infos), nil
}

func readBlob(obj plumbing.EncodedObject) ([]byte, error) {
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
