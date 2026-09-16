package history

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func benchmarkRepositories(b *testing.B) []string {
	b.Helper()
	value := os.Getenv("HISTORY_BENCH_REPOS")
	if value == "" {
		b.Skip("set HISTORY_BENCH_REPOS to a path-list of Git repositories")
	}
	var repos []string
	for _, repo := range filepath.SplitList(value) {
		if repo = strings.TrimSpace(repo); repo != "" {
			repos = append(repos, repo)
		}
	}
	if len(repos) == 0 {
		b.Skip("HISTORY_BENCH_REPOS contains no repository paths")
	}
	return repos
}

func BenchmarkWalkBlobs(b *testing.B) {
	for _, path := range benchmarkRepositories(b) {
		b.Run(filepath.Base(path), func(b *testing.B) {
			var total int
			var visited, bytes atomic.Int64
			for b.Loop() {
				repo, err := Open(path, DefaultTuning())
				if err != nil {
					b.Fatal(err)
				}
				total, err = repo.WalkBlobs(Options{
					Workers: runtime.GOMAXPROCS(0),
					Limit:   func(string) int64 { return 1 << 20 },
				}, func(blob Blob) error {
					visited.Add(1)
					bytes.Add(int64(len(blob.Data)))
					return nil
				})
				closeErr := repo.Close()
				if err != nil {
					b.Fatal(err)
				}
				if closeErr != nil {
					b.Fatal(closeErr)
				}
			}
			b.ReportMetric(float64(total), "blobs/op")
			b.ReportMetric(float64(visited.Load())/float64(b.N), "visited/op")
			b.ReportMetric(float64(bytes.Load())/float64(b.N), "bytes/op")
		})
	}
}

func BenchmarkWalkChanges(b *testing.B) {
	for _, path := range benchmarkRepositories(b) {
		b.Run(filepath.Base(path), func(b *testing.B) {
			var changes int64
			for b.Loop() {
				repo, err := Open(path, DefaultTuning())
				if err != nil {
					b.Fatal(err)
				}
				err = repo.WalkChanges(Options{Workers: runtime.GOMAXPROCS(0), Merges: true}, func(Change) {
					changes++
				})
				closeErr := repo.Close()
				if err != nil {
					b.Fatal(err)
				}
				if closeErr != nil {
					b.Fatal(closeErr)
				}
			}
			b.ReportMetric(float64(changes)/float64(b.N), "changes/op")
		})
	}
}

func BenchmarkWalkChangesGit(b *testing.B) {
	for _, path := range benchmarkRepositories(b) {
		b.Run(filepath.Base(path), func(b *testing.B) {
			var changes int64
			for b.Loop() {
				if err := WalkChangesGit(path, true, func(Change) {
					changes++
				}); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(changes)/float64(b.N), "changes/op")
		})
	}
}
