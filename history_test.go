package history

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
)

func TestDefaultTuningUsesFastReaders(t *testing.T) {
	tuning := DefaultTuning()
	if !tuning.Mmap {
		t.Fatal("default tuning must enable mmap")
	}
	if !tuning.KlauspostZlib {
		t.Fatal("default tuning must enable klauspost zlib")
	}
}

func git(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func repository(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init", "-b", "main")
	git(t, repo, "config", "user.name", "Test")
	git(t, repo, "config", "user.email", "test@example.org")
	git(t, repo, "config", "commit.gpgsign", "false")
	return repo
}

func commitFile(t *testing.T, repo, name, content, subject string) {
	t.Helper()
	path := filepath.Join(repo, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "--", name)
	git(t, repo, "commit", "-m", subject)
}

func open(t *testing.T, path string) *Repo {
	t.Helper()
	r, err := Open(path, DefaultTuning())
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestWalkChangesMatchesGitBinary(t *testing.T) {
	repo := repository(t)
	commitFile(t, repo, "a.txt", "one\n", "add a")
	commitFile(t, repo, "dir/b.txt", "two\n", "add b")
	commitFile(t, repo, "a.txt", "three\n", "modify a")
	git(t, repo, "rm", "dir/b.txt")
	git(t, repo, "commit", "-m", "remove b")
	git(t, repo, "gc")

	key := func(c Change) string {
		return strings.Join([]string{c.Commit, c.Path, c.OldOID, c.NewOID, c.OldMode, c.NewMode}, "|")
	}
	collect := func(walk func(func(Change)) error) []string {
		var out []string
		if err := walk(func(c Change) { out = append(out, key(c)) }); err != nil {
			t.Fatal(err)
		}
		sort.Strings(out)
		return out
	}
	fromGit := collect(func(v func(Change)) error { return WalkChangesGit(repo, false, v) })

	for _, workers := range []int{1, 4} {
		r := open(t, repo)
		got := collect(func(v func(Change)) error { return r.WalkChanges(Options{Workers: workers}, v) })
		if !slices.Equal(fromGit, got) {
			t.Fatalf("workers=%d mismatch\ngit:\n  %s\ngo-git:\n  %s",
				workers, strings.Join(fromGit, "\n  "), strings.Join(got, "\n  "))
		}
	}
}

func TestWalkBlobs(t *testing.T) {
	repo := repository(t)
	commitFile(t, repo, "a.txt", "alpha\n", "one")
	commitFile(t, repo, "b.txt", "beta\n", "two")
	commitFile(t, repo, "a.txt", "gamma\n", "three")
	git(t, repo, "gc")

	for _, infos := range []bool{true, false} {
		tune := DefaultTuning()
		tune.ObjectInfos = infos
		r, err := Open(repo, tune)
		if err != nil {
			t.Fatal(err)
		}
		var mu sync.Mutex
		got := map[string]string{}
		total, err := r.WalkBlobs(Options{Workers: 3}, func(b Blob) error {
			mu.Lock()
			got[b.OID] = string(b.Data)
			mu.Unlock()
			return nil
		})
		_ = r.Close()
		if err != nil {
			t.Fatalf("infos=%v: %v", infos, err)
		}
		if total != 3 || len(got) != 3 {
			t.Fatalf("infos=%v: total=%d unique=%d, want 3", infos, total, len(got))
		}
		for _, want := range []string{"alpha\n", "beta\n", "gamma\n"} {
			found := false
			for _, v := range got {
				if v == want {
					found = true
				}
			}
			if !found {
				t.Fatalf("infos=%v: missing blob content %q in %v", infos, want, got)
			}
		}
	}
}

func TestWalkBlobsLimit(t *testing.T) {
	repo := repository(t)
	commitFile(t, repo, "small.txt", "x", "small")
	commitFile(t, repo, "large.txt", strings.Repeat("y", 100), "large")

	r := open(t, repo)
	var visited, skipped []string
	var mu sync.Mutex
	total, err := r.WalkBlobs(Options{
		Workers: 2,
		Limit:   func(string) int64 { return 10 },
		Skip: func(oid string) {
			mu.Lock()
			skipped = append(skipped, oid)
			mu.Unlock()
		},
	}, func(b Blob) error {
		mu.Lock()
		visited = append(visited, b.OID)
		mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(visited) != 1 || len(skipped) != 1 {
		t.Fatalf("total=%d visited=%d skipped=%d", total, len(visited), len(skipped))
	}
}

func TestDefaultTuningBlobLayouts(t *testing.T) {
	repo := repository(t)
	commitFile(t, repo, "a.txt", "one\n", "one")
	commitFile(t, repo, "b.txt", "two\n", "two")
	git(t, repo, "gc", "--quiet")

	bare := filepath.Join(t.TempDir(), "bare.git")
	git(t, repo, "clone", "--bare", "--no-local", repo, bare)
	worktree := filepath.Join(t.TempDir(), "linked")
	git(t, repo, "worktree", "add", "--detach", worktree, "HEAD")
	shallow := filepath.Join(t.TempDir(), "shallow")
	git(t, repo, "clone", "--depth=1", "file://"+repo, shallow)

	for name, path := range map[string]string{
		"standard": repo,
		"bare":     bare,
		"worktree": worktree,
		"shallow":  shallow,
	} {
		t.Run(name, func(t *testing.T) {
			r, err := Open(path, DefaultTuning())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = r.Close() })
			var blobs atomic.Int64
			total, err := r.WalkBlobs(Options{Workers: 4}, func(Blob) error {
				blobs.Add(1)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if total == 0 || blobs.Load() != int64(total) {
				t.Fatalf("total=%d blobs=%d", total, blobs.Load())
			}
		})
	}
}

func TestSpoolRoundTrip(t *testing.T) {
	s, err := NewSpool("history-test")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	in := []Change{
		{Commit: "c1", Date: "2026-01-01", Subject: "one", OldOID: "0", NewOID: "a", Path: "x", OldMode: "000000", NewMode: "100644"},
		{Commit: "c2", Date: "2026-01-02", Subject: "two", OldOID: "a", NewOID: "b", Path: "y/z", OldMode: "100644", NewMode: "100644"},
	}
	for _, c := range in {
		s.Write(c)
	}
	if err := s.Ready(); err != nil {
		t.Fatal(err)
	}
	var out []Change
	if err := s.Replay(func(c Change) { out = append(out, c) }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(in, out) {
		t.Fatalf("round trip mismatch\nin:  %+v\nout: %+v", in, out)
	}
}

func TestOpenEmptyRepository(t *testing.T) {
	repo := repository(t)
	r := open(t, repo)
	if err := r.WalkChanges(Options{}, func(Change) { t.Fatal("visited change in empty repo") }); err != nil {
		t.Fatal(err)
	}
	roots, err := r.Roots()
	if err != nil || len(roots) != 0 {
		t.Fatalf("roots=%v err=%v", roots, err)
	}
}

func TestOpenRejectsAlternates(t *testing.T) {
	repo := repository(t)
	commitFile(t, repo, "a.txt", "x", "one")
	shared := filepath.Join(t.TempDir(), "shared")
	git(t, repo, "clone", "--shared", repo, shared)
	if _, err := Open(shared, DefaultTuning()); err == nil || !strings.Contains(err.Error(), "alternates") {
		t.Fatalf("want alternates error, got %v", err)
	}
}

func TestShallow(t *testing.T) {
	repo := repository(t)
	commitFile(t, repo, "a.txt", "1", "one")
	commitFile(t, repo, "a.txt", "2", "two")
	shallow := filepath.Join(t.TempDir(), "shallow")
	git(t, repo, "clone", "--depth", "1", "file://"+repo, shallow)

	r := open(t, shallow)
	ok, err := r.Shallow()
	if err != nil || !ok {
		t.Fatalf("shallow=%v err=%v", ok, err)
	}
	full := open(t, repo)
	ok, err = full.Shallow()
	if err != nil || ok {
		t.Fatalf("full shallow=%v err=%v", ok, err)
	}
}

func TestVisitCommitTrees(t *testing.T) {
	repo := repository(t)
	commitFile(t, repo, "a.txt", "1", "one")
	commitFile(t, repo, "b.txt", "2", "two")
	git(t, repo, "gc")

	r := open(t, repo)
	trees := 0
	if err := r.VisitCommitTrees(func(plumbing.Hash) error { trees++; return nil }); err != nil {
		t.Fatal(err)
	}
	if trees != 2 {
		t.Fatalf("trees=%d, want 2", trees)
	}
}

func TestCommitSubject(t *testing.T) {
	if got := CommitSubject("line one\nline two\n\nbody"); got != "line one line two" {
		t.Fatalf("got %q", got)
	}
	if got := CommitSubject("  single  \n"); got != "single" {
		t.Fatalf("got %q", got)
	}
}

func TestFormatMode(t *testing.T) {
	for mode, want := range map[filemode.FileMode]string{
		0:        "000000",
		1:        "000001",
		0o100644: "100644",
		0o100755: "100755",
		0o160000: "160000",
	} {
		if got := formatMode(mode); got != want {
			t.Errorf("formatMode(%o) = %q, want %q", mode, got, want)
		}
	}
}
