package history

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
)

func TestTreeFSEntriesCachesConcurrentLoad(t *testing.T) {
	const readers = 32
	hash := plumbing.NewHash("1111111111111111111111111111111111111111")
	childHash := plumbing.NewHash("2222222222222222222222222222222222222222")
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	snapshot := &TreeFS{
		walkTreeEntries: func(_ plumbing.Hash, visit func(TreeEntry) error) error {
			if calls.Add(1) == 1 {
				close(started)
			}
			<-release
			return visit(TreeEntry{Name: []byte("file"), Hash: childHash, Mode: filemode.Regular})
		},
	}

	begin := make(chan struct{})
	entered := make(chan struct{})
	results := make(chan error, readers)
	var wait sync.WaitGroup
	for range readers {
		wait.Go(func() {
			<-begin
			entered <- struct{}{}
			entries, err := snapshot.entries(hash)
			if err == nil && (len(entries) != 1 || entries[0].name != "file") {
				err = errors.New("unexpected cached entries")
			}
			results <- err
		})
	}
	close(begin)
	for range readers {
		<-entered
	}
	<-started
	close(release)
	wait.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("tree walks = %d, want 1", got)
	}
}

func TestTreeFSEntriesDoesNotCacheFailure(t *testing.T) {
	hash := plumbing.NewHash("1111111111111111111111111111111111111111")
	wantErr := errors.New("read tree")
	var calls int
	snapshot := &TreeFS{
		walkTreeEntries: func(_ plumbing.Hash, _ func(TreeEntry) error) error {
			calls++
			if calls == 1 {
				return wantErr
			}
			return nil
		},
	}

	if _, err := snapshot.entries(hash); !errors.Is(err, wantErr) {
		t.Fatalf("first load error = %v, want %v", err, wantErr)
	}
	if _, err := snapshot.entries(hash); err != nil {
		t.Fatalf("second load: %v", err)
	}
	if _, err := snapshot.entries(hash); err != nil {
		t.Fatalf("cached load: %v", err)
	}
	if calls != 2 {
		t.Fatalf("tree walks = %d, want 2", calls)
	}
}
