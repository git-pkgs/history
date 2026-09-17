package history

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// WalkChangesGit produces the same Change stream as Repo.WalkChanges by
// shelling out to the git binary. It is also a benchmark baseline for the
// in-process walker.
func WalkChangesGit(repo string, merges bool, visit func(Change) error) error {
	mergeMode := "off"
	if merges {
		mergeMode = "first-parent"
	}
	cmd := exec.Command("git", "-C", repo, "log", "--all", "--date-order",
		"--root", "--no-abbrev", "--raw", "--no-renames", "-z",
		"--diff-merges="+mergeMode, "--format=%x01%H%x00%aI%x00%P%x00%s")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	err = readGitChanges(stdout, visit)
	if err != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if err != nil {
		return fmt.Errorf("read history: %w", err)
	}
	return waitErr
}

func readGitChanges(input io.Reader, visit func(Change) error) error {
	r := bufio.NewReaderSize(input, readerBufferSize)
	field := func() (string, error) {
		s, err := r.ReadString(0)
		return strings.TrimSuffix(s, "\x00"), err
	}
	var c Change
	for {
		token, err := field()
		if err == io.EOF && strings.TrimSpace(token) == "" {
			return nil
		}
		if err != nil {
			return err
		}
		token = strings.TrimLeft(token, "\n")
		if strings.HasPrefix(token, "\x01") {
			c.Commit = token[1:]
			if c.Date, err = field(); err != nil {
				return err
			}
			parents, err := field()
			if err != nil {
				return err
			}
			c.Merge = strings.Contains(parents, " ")
			if c.Subject, err = field(); err != nil {
				return err
			}
			continue
		}
		if !strings.HasPrefix(token, ":") {
			continue
		}
		fields := strings.Fields(token[1:])
		if len(fields) != 5 || c.Commit == "" {
			return fmt.Errorf("invalid raw change %q", token)
		}
		c.OldMode, c.NewMode = fields[0], fields[1]
		c.OldOID, c.NewOID = fields[2], fields[3]
		if c.Path, err = field(); err != nil {
			return err
		}
		if err := visit(c); err != nil {
			return err
		}
	}
}

// ShallowGit reports whether the repository is a shallow clone by shelling
// out to git rev-parse.
func ShallowGit(repo string) (bool, error) {
	out, err := exec.Command("git", "-C", repo, "rev-parse", "--is-shallow-repository").Output()
	return bytes.HasPrefix(out, []byte("true")), err
}
