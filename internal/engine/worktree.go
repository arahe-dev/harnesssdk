package engine

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// A run started with --worktree works in a git worktree of its own, in
// <home>/worktrees/<run-id>, on branch harness/<run-id> made from the
// commit its folder is on. Agents that edit files can then run side by
// side, each on its own copy, and compare can set them against each other.
// After each turn harness commits what the agent left uncommitted, so the
// branch holds the work, and records how far it is from where it started.
// Removing a run removes its worktree but keeps the branch: it is the
// agent's work, and only the user decides to drop it.

// git runs git in dir and returns its trimmed output.
func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// addWorktree makes run's worktree from the repository holding run.Cwd,
// and sets its Worktree, Branch, Base and Workdir.
func (h *Home) addWorktree(run *Run) error {
	prefix, err := git(run.Cwd, "rev-parse", "--show-prefix")
	if err != nil {
		return fmt.Errorf("--worktree needs a folder in a git repository: %w", err)
	}
	base, err := git(run.Cwd, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("--worktree needs a repository with a commit: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(h.Dir, "worktrees"), 0o700); err != nil {
		return err
	}
	// Two worktrees added to one repository at once can trip on git's own
	// locks: harness adds one at a time.
	unlock, err := lockFileWait(filepath.Join(h.Dir, "worktrees.lock"), time.Minute)
	if err != nil {
		return err
	}
	defer unlock()
	root := filepath.Join(h.Dir, "worktrees", run.ID)
	branch := "harness/" + run.ID
	if _, err := git(run.Cwd, "worktree", "add", "-q", "-b", branch, root, base); err != nil {
		return err
	}
	run.Worktree, run.Branch, run.Base = root, branch, base
	run.Workdir = filepath.Join(root, filepath.FromSlash(prefix))
	return nil
}

// removeWorktree removes run's worktree, and its branch too when drop is
// set (a run that never ran). It runs git from the repository, never from
// inside the worktree it removes.
func removeWorktree(run *Run, drop bool) {
	if run.Worktree == "" {
		return
	}
	main := run.Cwd
	if common, err := git(run.Worktree, "rev-parse", "--path-format=absolute", "--git-common-dir"); err == nil {
		main = filepath.Dir(filepath.FromSlash(common))
	}
	if _, err := git(main, "worktree", "remove", "--force", run.Worktree); err != nil {
		os.RemoveAll(run.Worktree)
		git(main, "worktree", "prune")
	}
	if drop {
		git(main, "branch", "-q", "-D", run.Branch)
	}
}

// commitTurn commits what turn n left uncommitted in run's worktree and
// returns a summary of the branch's changes since it was made.
func commitTurn(run *Run, n int) (string, error) {
	status, err := git(run.Worktree, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	if status != "" {
		if _, err := git(run.Worktree, "add", "-A"); err != nil {
			return "", err
		}
		args := []string{"commit", "-q", "--no-verify", "-m", fmt.Sprintf("harness: run %s, turn %d", run.ID, n)}
		if email, _ := git(run.Worktree, "config", "user.email"); email == "" {
			args = append([]string{"-c", "user.name=harness", "-c", "user.email=harness@localhost"}, args...)
		}
		if _, err := git(run.Worktree, args...); err != nil {
			return "", err
		}
	}
	stat, err := git(run.Worktree, "diff", "--shortstat", run.Base, "HEAD")
	if stat == "" && err == nil {
		stat = "no changes"
	}
	return stat, err
}
