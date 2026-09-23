# history

A Go library for walking commits, file changes, trees, and blobs in Git repositories without invoking the Git binary. It reads loose and packed objects through go-git. Shallow clones, linked worktrees, alternate object stores, and SHA-1 or SHA-256 repositories are supported.

## Installation

```bash
go get github.com/git-pkgs/history
```

## Usage

### Walk a revision

`WalkCommits` visits commits oldest-first. Each result contains the commit and its retained first-parent file changes, and commits with no retained changes are still visited.

```go
package main

import (
	"fmt"
	"log"

	"github.com/git-pkgs/history"
)

func main() {
	repo, err := history.Open(".")
	if err != nil {
		log.Fatal(err)
	}
	defer repo.Close()

	err = repo.WalkCommits(history.CommitOptions{
		Ref:     "HEAD",
		Workers: 4,
	}, func(commit history.Commit) error {
		fmt.Printf("%s %s\n", commit.Object.Hash, history.CommitSubject(commit.Object.Message))
		for _, change := range commit.Changes {
			fmt.Printf("  %s %s\n", change.NewMode, change.Path)
		}
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}
}
```

Set `Since` to exclude a revision and its ancestors, or provide `Hashes` to walk an exact sequence. `Merges` retains first-parent changes for merge commits, while `PathFilter` discards unrelated paths before changes are retained. The callback always runs serially in commit order, including when workers decode commits in parallel.

### Walk changes across every ref

`WalkChanges` visits each reachable commit once in date order across all refs. The callback runs serially. Set `Merges` to include first-parent changes for merge commits.

```go
err := repo.WalkChanges(history.ChangeOptions{
	Merges:  true,
	Workers: 4,
}, func(change history.Change) error {
	fmt.Printf("%s %s %s\n", change.Commit, change.NewOID, change.Path)
	return nil
})
```

### Walk stored blobs

`WalkBlobs` visits each stored blob once, including objects supplied by Git alternates. Blob callbacks can run concurrently and must be safe for concurrent use.

```go
count, err := repo.WalkBlobs(history.BlobOptions{
	Workers: 4,
	Limit: func(string) int64 {
		return 1024 * 1024
	},
}, func(blob history.Blob) error {
	fmt.Printf("%s %d\n", blob.OID, blob.Size)
	return nil
})
```

The returned count includes blobs excluded by `Limit`. Use `Skip` when the caller needs their object IDs.

### Open options

`Open` uses the package's benchmarked storage defaults. `OpenWithOptions` exposes repository discovery, storage tuning, and an alternate-object filesystem for callers that need different behaviour.

## License

[MIT](LICENSE).
