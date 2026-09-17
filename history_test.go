package history

import (
	"errors"
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
	"github.com/go-git/go-git/v6/plumbing/object"
)

const testMainBranch = "main"

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

func gitOutput(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func repository(t *testing.T, options ...string) string {
	t.Helper()
	repo := t.TempDir()
	args := append([]string{"init", "-b", testMainBranch}, options...)
	git(t, repo, args...)
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
	r, err := Open(path)
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
	commitFile(t, repo, "swap", "file\n", "add file")
	git(t, repo, "rm", "swap")
	commitFile(t, repo, "swap/child.txt", "child\n", "replace file with directory")
	git(t, repo, "rm", "-r", "swap")
	commitFile(t, repo, "swap", "file again\n", "replace directory with file")
	commitFile(t, repo, "script.sh", "#!/bin/sh\n", "add script")
	if err := os.Chmod(filepath.Join(repo, "script.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "script.sh")
	git(t, repo, "commit", "-m", "make script executable")
	git(t, repo, "gc")

	key := func(c Change) string {
		return strings.Join([]string{c.Commit, c.Path, c.OldOID, c.NewOID, c.OldMode, c.NewMode}, "|")
	}
	collect := func(walk func(func(Change) error) error) []string {
		var out []string
		if err := walk(func(c Change) error {
			out = append(out, key(c))
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		sort.Strings(out)
		return out
	}
	fromGit := collect(func(v func(Change) error) error { return WalkChangesGit(repo, false, v) })

	for _, workers := range []int{1, 4} {
		r := open(t, repo)
		got := collect(func(v func(Change) error) error {
			return r.WalkChanges(ChangeOptions{Workers: workers}, v)
		})
		if !slices.Equal(fromGit, got) {
			t.Fatalf("workers=%d mismatch\ngit:\n  %s\ngo-git:\n  %s",
				workers, strings.Join(fromGit, "\n  "), strings.Join(got, "\n  "))
		}
	}
}

func TestWalkChangesReturnsVisitorError(t *testing.T) {
	repo := repository(t)
	commitFile(t, repo, "a.txt", "one\n", "add a")
	commitFile(t, repo, "b.txt", "two\n", "add b")
	want := errors.New("stop")

	for _, workers := range []int{1, 3} {
		r := open(t, repo)
		visits := 0
		err := r.WalkChanges(ChangeOptions{Workers: workers}, func(Change) error {
			visits++
			return want
		})
		if !errors.Is(err, want) || visits != 1 {
			t.Fatalf("workers=%d: visits=%d err=%v", workers, visits, err)
		}
	}
}

func TestWalkCommitsExactOrderAndFilteredChanges(t *testing.T) {
	repo := repository(t)
	commitFile(t, repo, "README.md", "one\n", "root")
	root := gitOutput(t, repo, "rev-parse", "HEAD")
	commitFile(t, repo, "Gemfile", "gem \"rails\"\n", "manifest")
	manifest := gitOutput(t, repo, "rev-parse", "HEAD")
	commitFile(t, repo, "README.md", "two\n", "docs")
	docs := gitOutput(t, repo, "rev-parse", "HEAD")

	r := open(t, repo)
	var got []Commit
	err := r.WalkCommits(CommitOptions{
		Hashes: []plumbing.Hash{
			plumbing.NewHash(root),
			plumbing.NewHash(manifest),
			plumbing.NewHash(docs),
		},
		Workers:    2,
		PathFilter: func(path string) bool { return path == "Gemfile" },
	}, func(commit Commit) error {
		got = append(got, commit)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("commits=%d, want 3", len(got))
	}
	for i, want := range []string{root, manifest, docs} {
		if got[i].Object.Hash.String() != want {
			t.Fatalf("commit[%d]=%s, want %s", i, got[i].Object.Hash, want)
		}
	}
	if len(got[0].Changes) != 0 || len(got[1].Changes) != 1 || len(got[2].Changes) != 0 {
		t.Fatalf("filtered change counts=%d,%d,%d", len(got[0].Changes), len(got[1].Changes), len(got[2].Changes))
	}
	if got[1].Object == nil || got[1].Object.Author.Email != "test@example.org" {
		t.Fatalf("commit metadata missing: %+v", got[1])
	}
}

func TestWalkCommitsRefAndSinceMatchGit(t *testing.T) {
	repo := repository(t)
	commitFile(t, repo, "root.txt", "root\n", "root")
	root := gitOutput(t, repo, "rev-parse", "HEAD")
	commitFile(t, repo, "main.txt", "main\n", "main")
	git(t, repo, "switch", "-c", "side", root)
	commitFile(t, repo, "side.txt", "side\n", "side")
	git(t, repo, "switch", testMainBranch)
	git(t, repo, "merge", "--no-ff", "-m", "merge", "side")

	r := open(t, repo)
	for name, opts := range map[string]CommitOptions{
		"all":   {Ref: testMainBranch, Workers: 3},
		"since": {Ref: testMainBranch, Since: root, Workers: 3},
	} {
		t.Run(name, func(t *testing.T) {
			args := []string{"rev-list", "--reverse"}
			if opts.Since == "" {
				args = append(args, opts.Ref)
			} else {
				args = append(args, opts.Since+".."+opts.Ref)
			}
			want := strings.Fields(gitOutput(t, repo, args...))
			var got []string
			if err := r.WalkCommits(opts, func(commit Commit) error {
				got = append(got, commit.Object.Hash.String())
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("commits=%v, want %v", got, want)
			}
		})
	}
}

func TestCommitHashesMatchesGit(t *testing.T) {
	repo := repository(t)
	commitFile(t, repo, "one.txt", "one\n", "one")
	first := gitOutput(t, repo, "rev-parse", "HEAD")
	commitFile(t, repo, "two.txt", "two\n", "two")
	commitFile(t, repo, "three.txt", "three\n", "three")

	r := open(t, repo)
	hashes, err := r.CommitHashes(CommitOptions{Ref: testMainBranch, Since: first})
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Fields(gitOutput(t, repo, "rev-list", "--reverse", first+"..main"))
	got := make([]string, len(hashes))
	for i, hash := range hashes {
		got[i] = hash.String()
	}
	if !slices.Equal(got, want) {
		t.Fatalf("commits=%v, want %v", got, want)
	}
}

func TestRepositoryDirectories(t *testing.T) {
	repo := repository(t)
	commitFile(t, repo, "README.md", "hello\n", "root")

	r := open(t, repo)
	if want := filepath.Join(repo, ".git"); r.GitDir() != want || r.CommonDir() != want {
		t.Fatalf("GitDir=%q CommonDir=%q, want %q", r.GitDir(), r.CommonDir(), want)
	}

	worktree := filepath.Join(t.TempDir(), "linked")
	git(t, repo, "worktree", "add", "-b", "linked", worktree)
	linked := open(t, worktree)
	wantCommonDir, err := filepath.EvalSymlinks(filepath.Join(repo, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if linked.CommonDir() != wantCommonDir {
		t.Fatalf("CommonDir=%q, want %q", linked.CommonDir(), wantCommonDir)
	}
	if linked.GitDir() == linked.CommonDir() {
		t.Fatalf("linked GitDir=%q, want worktree-specific directory", linked.GitDir())
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
		r, err := OpenWithOptions(repo, OpenOptions{Tuning: tune})
		if err != nil {
			t.Fatal(err)
		}
		var mu sync.Mutex
		got := map[string]string{}
		total, err := r.WalkBlobs(BlobOptions{Workers: 3}, func(b Blob) error {
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
	total, err := r.WalkBlobs(BlobOptions{
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
			r, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = r.Close() })
			var blobs atomic.Int64
			total, err := r.WalkBlobs(BlobOptions{Workers: 4}, func(Blob) error {
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
	if err := r.WalkChanges(ChangeOptions{}, func(Change) error {
		t.Fatal("visited change in empty repo")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.WalkCommits(CommitOptions{}, func(Commit) error {
		t.Fatal("visited commit in empty repo")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	roots, err := r.Roots()
	if err != nil || len(roots) != 0 {
		t.Fatalf("roots=%v err=%v", roots, err)
	}
}

func TestOpenRejectsReplacementRefs(t *testing.T) {
	repo := repository(t)
	commitFile(t, repo, "a.txt", "one\n", "one")
	first := gitOutput(t, repo, "rev-parse", "HEAD")
	commitFile(t, repo, "a.txt", "two\n", "two")
	second := gitOutput(t, repo, "rev-parse", "HEAD")
	git(t, repo, "replace", first, second)

	r, err := Open(repo)
	if r != nil {
		_ = r.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "replacement refs") {
		t.Fatalf("Open error=%v, want replacement refs error", err)
	}
}

func TestOpenAlternates(t *testing.T) {
	repo := repository(t)
	commitFile(t, repo, "a.txt", "x", "one")
	commitFile(t, repo, "a.txt", "y", "two")
	git(t, repo, "gc", "--quiet")
	shared := filepath.Join(t.TempDir(), "shared")
	git(t, repo, "clone", "--shared", repo, shared)
	r := open(t, shared)
	changes := 0
	if err := r.WalkChanges(ChangeOptions{}, func(Change) error {
		changes++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if changes != 2 {
		t.Fatalf("changes=%d, want 2", changes)
	}

	for _, infos := range []bool{true, false} {
		tuning := DefaultTuning()
		tuning.ObjectInfos = infos
		blobRepo, err := OpenWithOptions(shared, OpenOptions{Tuning: tuning})
		if err != nil {
			t.Fatal(err)
		}
		var mu sync.Mutex
		contents := make(map[string]struct{})
		total, err := blobRepo.WalkBlobs(BlobOptions{Workers: 2}, func(blob Blob) error {
			mu.Lock()
			contents[string(blob.Data)] = struct{}{}
			mu.Unlock()
			return nil
		})
		closeErr := blobRepo.Close()
		if err != nil {
			t.Fatalf("infos=%v: %v", infos, err)
		}
		if closeErr != nil {
			t.Fatalf("infos=%v close: %v", infos, closeErr)
		}
		if total != 2 || len(contents) != 2 {
			t.Fatalf("infos=%v: total=%d contents=%v, want 2", infos, total, contents)
		}
		for _, want := range []string{"x", "y"} {
			if _, ok := contents[want]; !ok {
				t.Fatalf("infos=%v: missing %q in %v", infos, want, contents)
			}
		}
	}
}

func TestOpenWithOptionsDetectsDotGit(t *testing.T) {
	repo := repository(t)
	commitFile(t, repo, "nested/a.txt", "x", "one")
	r, err := OpenWithOptions(filepath.Join(repo, "nested"), OpenOptions{
		Tuning:       DefaultTuning(),
		DetectDotGit: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	changes := 0
	if err := r.WalkChanges(ChangeOptions{}, func(Change) error {
		changes++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if changes != 1 {
		t.Fatalf("changes=%d, want 1", changes)
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

func TestRawCommitLinksMatchDecodedCommits(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			repo := repository(t, "--object-format="+format)
			commitFile(t, repo, "base.txt", "base\n", "base")
			git(t, repo, "switch", "-c", "side")
			commitFile(t, repo, "side.txt", "side\n", "side")
			git(t, repo, "switch", testMainBranch)
			commitFile(t, repo, "main.txt", "main\n", "main")
			git(t, repo, "merge", "--no-ff", "-m", "merge", "side")
			git(t, repo, "gc", "--quiet")

			r := open(t, repo)
			iter, err := r.r.CommitObjects()
			if err != nil {
				t.Fatal(err)
			}
			defer iter.Close()
			err = iter.ForEach(func(commit *object.Commit) error {
				var parents []plumbing.Hash
				tree, err := readCommitTreeAndParents(r.r, commit.Hash, true, &parents)
				if err != nil {
					return err
				}
				if tree != commit.TreeHash || !slices.Equal(parents, commit.ParentHashes) {
					t.Fatalf("commit %s links differ", commit.Hash)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWalkTreeEntriesMatchesDecodedTree(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			repo := repository(t, "--object-format="+format)
			commitFile(t, repo, "a.txt", "a\n", "add a")
			commitFile(t, repo, "dir/b.txt", "b\n", "add b")
			git(t, repo, "gc", "--quiet")

			r := open(t, repo)
			head, err := r.r.Head()
			if err != nil {
				t.Fatal(err)
			}
			commit, err := r.r.CommitObject(head.Hash())
			if err != nil {
				t.Fatal(err)
			}
			tree, err := commit.Tree()
			if err != nil {
				t.Fatal(err)
			}
			type entryValue struct {
				hash plumbing.Hash
				mode filemode.FileMode
			}
			want := make(map[string]entryValue, len(tree.Entries))
			for _, entry := range tree.Entries {
				want[entry.Name] = entryValue{hash: entry.Hash, mode: entry.Mode}
			}
			got := make(map[string]entryValue, len(tree.Entries))
			if err := r.WalkTreeEntries(tree.Hash, func(entry TreeEntry) error {
				got[string(entry.Name)] = entryValue{hash: entry.Hash, mode: entry.Mode}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if len(got) != len(want) {
				t.Fatalf("entries=%v, want %v", got, want)
			}
			for name, entry := range want {
				if got[name] != entry {
					t.Fatalf("entry %q=%v, want %v", name, got[name], entry)
				}
			}
		})
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
