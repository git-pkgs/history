package history

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
)

func TestWalkTreeEntriesAcrossBufferBoundaries(t *testing.T) {
	for _, format := range []string{testSHA1, testSHA256} {
		t.Run(format, func(t *testing.T) {
			repo := repository(t, "--object-format="+format)
			const files = 1100
			for i := range files {
				name := fmt.Sprintf("file-%04d.go", i)
				if err := os.WriteFile(filepath.Join(repo, name), []byte(name), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			git(t, repo, "add", ".")
			git(t, repo, "commit", "-m", "add source files")
			hash := plumbing.NewHash(gitOutput(t, repo, "rev-parse", "HEAD^{tree}"))
			want := strings.Split(gitOutput(t, repo, "ls-tree", "HEAD"), "\n")
			if len(want) != files {
				t.Fatalf("Git tree has %d entries, want %d", len(want), files)
			}
			for _, storage := range []string{"loose", "packed"} {
				t.Run(storage, func(t *testing.T) {
					if storage == "packed" {
						git(t, repo, "repack", "-adq")
					}
					checkTreeEntries(t, repo, hash, want)
				})
			}
		})
	}
}

func checkTreeEntries(t *testing.T, repo string, hash plumbing.Hash, want []string) {
	t.Helper()
	r := open(t, repo)
	count := 0
	err := r.WalkTreeEntries(hash, func(entry TreeEntry) error {
		got := fmt.Sprintf("%06o blob %s\t%s", entry.Mode, entry.Hash, entry.Name)
		if count >= len(want) {
			t.Fatalf("unexpected entry %q", got)
		}
		if got != want[count] {
			t.Fatalf("entry %d = %q, want %q", count, got, want[count])
		}
		count++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != len(want) {
		t.Fatalf("visited %d entries, want %d", count, len(want))
	}
}
