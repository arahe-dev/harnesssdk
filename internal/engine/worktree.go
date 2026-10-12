package engine

import (
	"context"
	"fmt"
	"io"
	"os"
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
// agent's work, and only the user decides to drop it. Work is never thrown
// away: a worktree with uncommitted changes is committed before it is
// removed, and kept if that fails.

const gitTimeout = 2 * time.Minute

// git runs git with args, bounded by ctx and gitTimeout, as harness runs
// agents (its process tree is contained and killed with it), without
// terminal prompts. It returns the output with line endings trimmed only,
// since a path may begin or end with spaces.
func git(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	stdout, err := os.CreateTemp("", "harness-git-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(stdout.Name())
	defer stdout.Close()
	stderr, err := os.CreateTemp("", "harness-git-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(stderr.Name())
	defer stderr.Close()
	code, _, err := runProcess(ctx, procSpec{Argv: append([]string{"git"}, args...), Stdout: stdout, Stderr: stderr,
		Env: []string{"GIT_TERMINAL_PROMPT=0"}})
	out, _ := readAll(stdout)
	msg, _ := readAll(stderr)
	switch {
	case err != nil:
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	case ctx.Err() != nil:
		return "", fmt.Errorf("git %s did not finish within %s", strings.Join(args, " "), gitTimeout)
	case code != 0:
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(or(msg, fmt.Sprintf("exit status %d", code))))
	}
	return strings.TrimRight(out, "\r\n"), nil
}

func readAll(f *os.File) (string, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	b, err := io.ReadAll(f)
	return string(b), err
}

func or(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

// addWorktree makes run's worktree from the repository holding run.Cwd.
// What it is about to make (worktree, branch) is recorded in the staged run
// first, so whoever finds the staged run, its own rollback or gc after a
// crash, removes whatever git made, even if git failed half way.
func (h *Home) addWorktree(ctx context.Context, run *Run) error {
	prefix, err := git(ctx, "-C", run.Cwd, "rev-parse", "--show-prefix")
	if err != nil {
		return fmt.Errorf("--worktree needs a folder in a git repository: %w", err)
	}
	base, err := git(ctx, "-C", run.Cwd, "rev-parse", "HEAD")
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
	run.Worktree, run.Branch, run.Base = filepath.Join(h.Dir, "worktrees", run.ID), "harness/"+run.ID, base
	run.Workdir = filepath.Join(run.Worktree, filepath.FromSlash(prefix))
	if err := run.Save(); err != nil {
		return err
	}
	if _, err := git(ctx, "-C", run.Cwd, "worktree", "add", "-q", "-b", run.Branch, run.Worktree, base); err != nil {
		return err
	}
	// A folder git does not track (an empty or ignored one) is not in the
	// worktree: make it, so the agent has its folder.
	return os.MkdirAll(run.Workdir, 0o755)
}

// repo is the git option that addresses run's repository from outside its
// worktree: the folder the run was started from, or else the repository's
// own git folder (which also covers --separate-git-dir and bare repositories).
func repo(ctx context.Context, run *Run) ([]string, error) {
	if fi, err := os.Stat(run.Cwd); err == nil && fi.IsDir() {
		return []string{"-C", run.Cwd}, nil
	}
	common, err := git(ctx, "-C", run.Worktree, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return nil, fmt.Errorf("cannot find the repository of worktree %s: %w", run.Worktree, err)
	}
	return []string{"--git-dir", common}, nil
}

// removeWorktree removes run's worktree. With drop, the run never ran:
// whatever git made for it goes, branch included, and what remains of a
// half-made worktree is deleted. Without drop, the worktree's work is
// committed to its branch first, and nothing is removed if that, or git's
// removal, fails.
func removeWorktree(ctx context.Context, run *Run, drop bool) error {
	if run.Worktree == "" {
		return nil
	}
	_, statErr := os.Stat(run.Worktree)
	exists := statErr == nil
	if !drop && exists {
		if _, err := commitWork(ctx, run, fmt.Sprintf("harness: work left in run %s, kept by gc", run.ID)); err != nil {
			return fmt.Errorf("worktree %s has work that could not be committed, so it is kept: %w", run.Worktree, err)
		}
	}
	at, err := repo(ctx, run)
	if err != nil {
		if drop && exists {
			return os.RemoveAll(run.Worktree)
		}
		return err
	}
	if exists {
		if _, err := git(ctx, append(at, "worktree", "remove", "--force", run.Worktree)...); err != nil {
			if !drop {
				return err
			}
			os.RemoveAll(run.Worktree) // a half-made worktree of a run that never ran
		}
	}
	git(ctx, append(at, "worktree", "prune")...)
	if drop {
		if _, err := git(ctx, append(at, "rev-parse", "--verify", "-q", "refs/heads/"+run.Branch)...); err == nil {
			if _, err := git(ctx, append(at, "branch", "-q", "-D", run.Branch)...); err != nil {
				return err
			}
		}
	}
	return nil
}

// commitWork commits what is uncommitted in run's worktree, if anything,
// without hooks or signing (harness commits for the agent, unattended), as
// the repository's user or else as "harness". It reports whether there was
// anything to commit.
func commitWork(ctx context.Context, run *Run, message string) (bool, error) {
	status, err := git(ctx, "-C", run.Worktree, "status", "--porcelain")
	if err != nil || status == "" {
		return false, err
	}
	if _, err := git(ctx, "-C", run.Worktree, "add", "-A"); err != nil {
		return false, err
	}
	args := []string{"-C", run.Worktree, "-c", "commit.gpgsign=false"}
	if email, _ := git(ctx, "-C", run.Worktree, "config", "user.email"); email == "" {
		args = append(args, "-c", "user.name=harness", "-c", "user.email=harness@localhost")
	}
	if _, err := git(ctx, append(args, "commit", "-q", "--no-verify", "-m", message)...); err != nil {
		return false, err
	}
	return true, nil
}

// commitTurn commits what turn n left uncommitted in run's worktree and
// returns a summary of the branch's changes since it was made.
func commitTurn(ctx context.Context, run *Run, n int) (string, error) {
	if _, err := commitWork(ctx, run, fmt.Sprintf("harness: run %s, turn %d", run.ID, n)); err != nil {
		return "", err
	}
	stat, err := git(ctx, "-C", run.Worktree, "diff", "--shortstat", run.Base, "HEAD")
	if err != nil {
		return "", err
	}
	if stat = strings.TrimSpace(stat); stat == "" {
		stat = "no changes"
	}
	return stat, nil
}
