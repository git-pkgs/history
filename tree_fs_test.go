package history_test

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/git-pkgs/history"
	"github.com/go-git/go-git/v6/plumbing"
)

const (
	snapshotReadme     = "README.md"
	snapshotNested     = "src/nested/data.txt"
	snapshotSubmodule  = "vendor"
	snapshotRun        = "run"
	snapshotAbsent     = "absent"
	snapshotSource     = "src/main.go"
	snapshotDir        = "src"
	snapshotLoop       = "loop"
	snapshotSourceData = "package main\n"
)

var snapshotFiles = []struct {
	name, data string
	mode       fs.FileMode
}{
	{snapshotReadme, "older snapshot\n", 0o644},
	{snapshotSource, snapshotSourceData, 0o644},
	{snapshotNested, "nested\n", 0o644},
	{"src.txt", "sibling\n", 0o644},
	{snapshotRun, "#!/bin/sh\n", 0o755},
	{"empty", "", 0o644},
	{"space and ü.txt", "unicode\n", 0o644},
	{"link", snapshotReadme, fs.ModeSymlink | 0o777},
	{"dir-link", snapshotDir, fs.ModeSymlink | 0o777},
	{"outside", "../../outside", fs.ModeSymlink | 0o777},
	{snapshotLoop, snapshotLoop, fs.ModeSymlink | 0o777},
	{"broken", snapshotAbsent, fs.ModeSymlink | 0o777},
}

func snapshotGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func snapshotWrite(t *testing.T, dir, name, content string, mode fs.FileMode) {
	t.Helper()
	filename := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

type snapshotFixture struct {
	dir          string
	commit, tree plumbing.Hash
	blob, tag    plumbing.Hash
}

func newSnapshotFixture(t *testing.T, format string) snapshotFixture {
	t.Helper()
	dir := t.TempDir()
	snapshotGit(t, dir, "init", "-b", "main", "--object-format="+format)
	snapshotGit(t, dir, "config", "user.name", "Test")
	snapshotGit(t, dir, "config", "user.email", "test@example.org")
	snapshotGit(t, dir, "config", "commit.gpgsign", "false")
	for _, file := range snapshotFiles {
		if file.mode&fs.ModeSymlink != 0 {
			if err := os.Symlink(file.data, filepath.Join(dir, file.name)); err != nil {
				t.Fatal(err)
			}
		} else {
			snapshotWrite(t, dir, file.name, file.data, file.mode)
		}
	}
	snapshotGit(t, dir, "add", ".")
	snapshotGit(t, dir, "update-index", "--chmod=+x", snapshotRun)
	// Gitlinks may reference commits absent from the superproject's store.
	missing := strings.Repeat("1", len(snapshotGit(t, dir, "hash-object", snapshotReadme)))
	snapshotGit(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+missing+",vendor")
	snapshotGit(t, dir, "commit", "-m", "original snapshot")
	fixture := snapshotFixture{
		dir:    dir,
		commit: plumbing.NewHash(snapshotGit(t, dir, "rev-parse", "HEAD")),
		tree:   plumbing.NewHash(snapshotGit(t, dir, "rev-parse", "HEAD^{tree}")),
		blob:   plumbing.NewHash(snapshotGit(t, dir, "rev-parse", "HEAD:README.md")),
	}
	snapshotGit(t, dir, "-c", "tag.gpgsign=false", "tag", "-a", "old", "-m", "old snapshot")
	fixture.tag = plumbing.NewHash(snapshotGit(t, dir, "rev-parse", "old"))
	snapshotWrite(t, dir, snapshotReadme, "new snapshot\n", 0o644)
	snapshotGit(t, dir, "add", snapshotReadme)
	snapshotGit(t, dir, "commit", "-m", "change readme")
	return fixture
}

func openSnapshotRepo(t *testing.T, dir string) *history.Repo {
	t.Helper()
	repo, err := history.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Error(err)
		}
	})
	return repo
}

func TestTreeFS(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			fixture := newSnapshotFixture(t, format)
			for _, layout := range []string{"loose", "packed", "bare"} {
				t.Run(layout, func(t *testing.T) {
					dir := fixture.dir
					if layout == "packed" {
						snapshotGit(t, dir, "gc", "--quiet")
					}
					if layout == "bare" {
						dir = filepath.Join(t.TempDir(), "bare.git")
						snapshotGit(t, fixture.dir, "clone", "--bare", "--no-local", fixture.dir, dir)
					}
					repo := openSnapshotRepo(t, dir)
					for _, hash := range []plumbing.Hash{fixture.commit, fixture.tree} {
						snapshot, err := repo.TreeFS(hash)
						if err != nil {
							t.Fatal(err)
						}
						checkSnapshot(t, snapshot)
					}
				})
			}
		})
	}
}

func checkSnapshot(t *testing.T, snapshot fs.FS) {
	t.Helper()
	files := make([]string, 0, len(snapshotFiles)+1)
	for _, file := range snapshotFiles {
		files = append(files, file.name)
		data, err := fs.ReadFile(snapshot, file.name)
		if err != nil || string(data) != file.data {
			t.Fatalf("ReadFile(%q) = %q, %v; want %q", file.name, data, err, file.data)
		}
		info, err := fs.Stat(snapshot, file.name)
		if err != nil {
			t.Fatal(err)
		}
		if info.Name() != filepath.Base(file.name) || info.Size() != int64(len(file.data)) || info.Mode() != file.mode || !info.ModTime().IsZero() || info.Sys() != nil {
			t.Errorf("Stat(%q) = %+v", file.name, info)
		}
	}
	files = append(files, snapshotSubmodule)
	if err := fstest.TestFS(snapshot, files...); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]fs.FileMode{
		snapshotDir: fs.ModeDir | 0o777, snapshotSubmodule: fs.ModeDir | 0o777,
	} {
		info, err := fs.Stat(snapshot, name)
		if err != nil || info.Mode() != want {
			t.Fatalf("Stat(%q) = %v, %v; want mode %v", name, info, err, want)
		}
		if info.Size() != 0 || !info.ModTime().IsZero() || info.Sys() != nil {
			t.Errorf("directory Stat(%q) = %+v", name, info)
		}
	}
	entries, err := fs.ReadDir(snapshot, snapshotSubmodule)
	if err != nil || len(entries) != 0 {
		t.Fatalf("ReadDir(vendor) = %v, %v", entries, err)
	}
	matches, err := fs.Glob(snapshot, "src/*/*.txt")
	if err != nil || !slices.Equal(matches, []string{snapshotNested}) {
		t.Fatalf("Glob = %v, %v", matches, err)
	}
	var walked []string
	err = fs.WalkDir(snapshot, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		walked = append(walked, name)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() != info.IsDir() || entry.Type() != info.Mode().Type() {
			return fmt.Errorf("entry and info differ for %q", name)
		}
		return nil
	})
	wantWalk := []string{
		".", snapshotReadme, "broken", "dir-link", "empty", "link", snapshotLoop, "outside", snapshotRun, "space and ü.txt",
		snapshotDir, snapshotSource, "src/nested", snapshotNested, "src.txt", snapshotSubmodule,
	}
	if err != nil || !slices.Equal(walked, wantWalk) {
		t.Fatalf("WalkDir = %v, %v; want %v", walked, err, wantWalk)
	}
}

func TestTreeFSErrors(t *testing.T) {
	fixture := newSnapshotFixture(t, "sha1")
	repo := openSnapshotRepo(t, fixture.dir)
	for _, hash := range []plumbing.Hash{fixture.blob, fixture.tag} {
		_, err := repo.TreeFS(hash)
		checkSnapshotPathError(t, err, "treefs", hash.String(), fs.ErrInvalid)
	}
	missing := plumbing.NewHash(strings.Repeat("1", len(fixture.commit.String())))
	_, err := repo.TreeFS(missing)
	checkSnapshotPathError(t, err, "treefs", missing.String(), fs.ErrNotExist)
	snapshot, err := repo.TreeFS(fixture.commit)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", "/", "/README.md", "..", "../README.md", "src/../README.md", "src//main.go", "src/", "./README.md"} {
		_, err := snapshot.Open(name)
		checkSnapshotPathError(t, err, "open", name, fs.ErrInvalid)
		_, err = fs.Stat(snapshot, name)
		checkSnapshotPathError(t, err, "stat", name, fs.ErrInvalid)
	}
	for _, name := range []string{snapshotAbsent, "src/absent", "vendor/absent", "a\x00b"} {
		_, err := snapshot.Open(name)
		checkSnapshotPathError(t, err, "open", name, fs.ErrNotExist)
		_, err = fs.Stat(snapshot, name)
		checkSnapshotPathError(t, err, "stat", name, fs.ErrNotExist)
	}
	for _, name := range []string{"README.md/child", "dir-link/main.go", "outside/child"} {
		_, err := snapshot.Open(name)
		if err == nil {
			t.Fatalf("Open(%q) succeeded", name)
		}
		checkSnapshotPathError(t, err, "open", name, nil)
	}
}

func checkSnapshotPathError(t *testing.T, err error, op, name string, want error) {
	t.Helper()
	var pathErr *fs.PathError
	if !errors.As(err, &pathErr) || pathErr.Op != op || pathErr.Path != name || (want != nil && !errors.Is(err, want)) {
		t.Fatalf("error = %v; want PathError(%s, %q, %v)", err, op, name, want)
	}
}

func TestTreeFSFileHandles(t *testing.T) {
	fixture := newSnapshotFixture(t, "sha1")
	snapshot, err := openSnapshotRepo(t, fixture.dir).TreeFS(fixture.tree)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".", snapshotDir, snapshotSubmodule, snapshotReadme} {
		t.Run(name, func(t *testing.T) {
			file, err := snapshot.Open(name)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = file.Close() })
			info, err := file.Stat()
			if err != nil {
				t.Fatal(err)
			}
			dir := file.(fs.ReadDirFile)
			if info.IsDir() {
				checkSnapshotDirectory(t, snapshot, name, dir)
			} else {
				if _, err := dir.ReadDir(1); err == nil {
					t.Fatal("ReadDir(file) succeeded")
				}
				data, err := io.ReadAll(file)
				if err != nil || string(data) != "older snapshot\n" {
					t.Fatalf("Read = %q, %v", data, err)
				}
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			_, err = file.Stat()
			checkSnapshotPathError(t, err, "stat", name, fs.ErrClosed)
			_, err = file.Read(nil)
			checkSnapshotPathError(t, err, "read", name, fs.ErrClosed)
			_, err = dir.ReadDir(1)
			checkSnapshotPathError(t, err, "readdir", name, fs.ErrClosed)
			checkSnapshotPathError(t, file.Close(), "close", name, fs.ErrClosed)
		})
	}
}

func TestTreeFSSubtree(t *testing.T) {
	fixture := newSnapshotFixture(t, "sha256")
	hash := plumbing.NewHash(snapshotGit(t, fixture.dir, "rev-parse", fixture.commit.String()+":src"))
	snapshot, err := openSnapshotRepo(t, fixture.dir).TreeFS(hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := fstest.TestFS(snapshot, "main.go", "nested/data.txt"); err != nil {
		t.Fatal(err)
	}
	data, err := fs.ReadFile(snapshot, "main.go")
	if err != nil || string(data) != snapshotSourceData {
		t.Fatalf("subtree ReadFile = %q, %v", data, err)
	}
	_, err = fs.Stat(snapshot, snapshotReadme)
	checkSnapshotPathError(t, err, "stat", snapshotReadme, fs.ErrNotExist)
}

func checkSnapshotDirectory(t *testing.T, snapshot fs.FS, name string, dir fs.ReadDirFile) {
	t.Helper()
	if _, err := dir.Read(make([]byte, 1)); err == nil {
		t.Fatal("Read(directory) succeeded")
	}
	var names []string
	for {
		batch, err := dir.ReadDir(1)
		if err == io.EOF {
			if len(batch) != 0 {
				t.Fatal("ReadDir returned entries with EOF")
			}
			break
		}
		if err != nil || len(batch) != 1 {
			t.Fatalf("ReadDir(1) = %v, %v", batch, err)
		}
		names = append(names, batch[0].Name())
	}
	want, err := fs.ReadDir(snapshot, name)
	if err != nil || len(names) != len(want) || !slices.IsSorted(names) {
		t.Fatalf("paged ReadDir = %v, want %v; err %v", names, want, err)
	}
	for i, entry := range want {
		if names[i] != entry.Name() {
			t.Fatalf("paged entry %d = %q, want %q", i, names[i], entry.Name())
		}
	}
	for _, n := range []int{0, -1} {
		if entries, err := dir.ReadDir(n); err != nil || len(entries) != 0 {
			t.Fatalf("exhausted ReadDir(%d) = %v, %v", n, entries, err)
		}
	}
}

func TestTreeFSConcurrent(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			fixture := newSnapshotFixture(t, format)
			for _, packed := range []bool{false, true} {
				t.Run(fmt.Sprintf("packed=%v", packed), func(t *testing.T) {
					if packed {
						snapshotGit(t, fixture.dir, "gc", "--quiet")
					}
					for _, tuning := range []history.Tuning{{}, {MemoryIndex: true, Mmap: true, CacheBytes: 1024, CacheShards: 4}} {
						checkSnapshotTuning(t, fixture, tuning)
					}
				})
			}
		})
	}
}

func TestTreeFSRepeatedLookup(t *testing.T) {
	fixture := newSnapshotFixture(t, "sha1")
	repo, err := history.OpenWithOptions(fixture.dir, history.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Error(err)
		}
	})
	snapshot, err := repo.TreeFS(fixture.commit)
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		info, err := fs.Stat(snapshot, snapshotNested)
		if err != nil {
			t.Fatal(err)
		}
		if info.IsDir() || info.Size() == 0 {
			t.Fatalf("nested info: %+v", info)
		}
	}
}

func checkSnapshotTuning(t *testing.T, fixture snapshotFixture, tuning history.Tuning) {
	t.Helper()
	repo, err := history.OpenWithOptions(fixture.dir, history.OpenOptions{Tuning: tuning})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Error(err)
		}
	})
	snapshot, err := repo.TreeFS(fixture.commit)
	if err != nil {
		t.Fatal(err)
	}
	checkConcurrentSnapshots(t, repo, snapshot, fixture.tree)
}

func checkConcurrentSnapshots(t *testing.T, repo *history.Repo, snapshot fs.FS, tree plumbing.Hash) {
	t.Helper()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 5 {
				checkSnapshot(t, snapshot)
				other, err := repo.TreeFS(tree)
				if err != nil {
					t.Error(err)
					return
				}
				got, err := fs.ReadFile(other, snapshotSource)
				if err != nil || string(got) != snapshotSourceData {
					t.Errorf("concurrent ReadFile = %q, %v", got, err)
				}
			}
		})
	}
	wg.Wait()
}
