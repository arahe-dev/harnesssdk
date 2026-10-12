package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
// Each name is claimed by an operation that fails when the name is taken
// (making the folder, then git creating the branch), and recorded in the
// staged run only once claimed, so whoever finds the staged run, its own
// rollback or gc after a crash, removes only what this creation made. A
// crash between a claim and its record leaves the name behind; nothing
// someone else made is ever removed.
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
	lk, err := lockWaitCtx(ctx, filepath.Join(h.Dir, "worktrees.lock"), time.Minute)
	if err != nil {
		return err
	}
	defer lk.Close()
	root, branch := filepath.Join(h.Dir, "worktrees", run.ID), "harness/"+run.ID
	if err := os.Mkdir(root, 0o755); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("worktree folder %s is in use", root)
		}
		return err
	}
	run.Worktree, run.Base = root, base
	run.Workdir = filepath.Join(root, filepath.FromSlash(prefix))
	if err := run.Save(); err != nil {
		return err
	}
	if _, err := git(ctx, "-C", run.Cwd, "branch", "--no-track", branch, base); err != nil {
		return fmt.Errorf("cannot make branch %s: %w", branch, err) // one of that name is not this run's
	}
	run.Branch = branch
	if err := run.Save(); err != nil {
		return err
	}
	if _, err := git(ctx, "-C", run.Cwd, "worktree", "add", "-q", root, branch); err != nil {
		return err
	}
	// A folder git does not track (an empty or ignored one) is not in the
	// worktree: make it, so the agent has its folder.
	return os.MkdirAll(run.Workdir, 0o755)
}

// branchTip is the commit branch points at in the repository at, or ""
// if there is no such branch. A failure to tell is an error, never "absent".
func branchTip(ctx context.Context, at []string, branch string) (string, error) {
	return git(ctx, append(at, "for-each-ref", "--format=%(objectname)", "refs/heads/"+branch)...)
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

// removeWorktree removes run's worktree.
//
// With drop, the run never ran: what was made for it goes (addWorktree
// recorded only names it claimed), the remains of a half-made worktree
// included, and its branch too while it still points where it was made: a
// branch that moved holds someone's work, and is left alone.
//
// Without drop, the work is kept: what is uncommitted is committed to the
// run's branch, and the worktree is removed only if its HEAD is still on
// that branch and git finds it clean, so nothing it holds is lost. Any
// failure keeps the worktree and is returned.
func removeWorktree(ctx context.Context, run *Run, drop bool) error {
	if run.Worktree == "" {
		return nil
	}
	// made: git made a worktree in the folder (it has its .git). A folder
	// without one is only what addWorktree made itself. Whatever cannot be
	// inspected is kept.
	_, err := os.Stat(run.Worktree)
	exists, made := err == nil, false
	if exists {
		_, err = os.Lstat(filepath.Join(run.Worktree, ".git"))
		made = err == nil
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("worktree %s cannot be inspected, so it is kept: %w", run.Worktree, err)
	}
	if drop && made {
		// The run never ran, so its worktree should hold nothing of
		// anyone's: it is removed only once git finds it clean, every
		// untracked, ignored and submodule change shown whatever the
		// configuration, and it holds no repository of its own (a
		// submodule's changes are its own repository's to report).
		status, err := git(ctx, "-C", run.Worktree, "status", "--porcelain", "--ignored", "--untracked-files=all", "--ignore-submodules=none")
		if err != nil {
			return fmt.Errorf("cannot tell whether worktree %s holds changes, so it is kept: %w", run.Worktree, err)
		}
		if status != "" {
			return fmt.Errorf("worktree %s has changes, so it is kept", run.Worktree)
		}
		nested, err := holdsRepository(run.Worktree)
		if err != nil {
			return fmt.Errorf("worktree %s cannot be inspected, so it is kept: %w", run.Worktree, err)
		}
		if nested != "" {
			return fmt.Errorf("worktree %s holds a submodule or another repository (%s), so it is kept", run.Worktree, nested)
		}
	}
	if drop && exists && !made {
		// Not a worktree: the folder addWorktree made goes if it is empty,
		// and anything in it is kept.
		entries, err := os.ReadDir(run.Worktree)
		switch {
		case err != nil:
			return fmt.Errorf("worktree %s cannot be inspected, so it is kept: %w", run.Worktree, err)
		case len(entries) > 0:
			return fmt.Errorf("worktree folder %s holds files but no worktree, so it is kept", run.Worktree)
		}
		if err := os.Remove(run.Worktree); err != nil {
			return err
		}
		exists = false
	}
	if !drop && exists {
		if head, err := git(ctx, "-C", run.Worktree, "symbolic-ref", "-q", "HEAD"); err != nil || head != "refs/heads/"+run.Branch {
			return fmt.Errorf("worktree %s is not on its branch %s (HEAD %s); it is kept so that no work is lost", run.Worktree, run.Branch, or(head, "detached"))
		}
		if _, err := commitWork(ctx, run, fmt.Sprintf("harness: work left in run %s, kept by gc", run.ID)); err != nil {
			return fmt.Errorf("worktree %s has work that could not be committed, so it is kept: %w", run.Worktree, err)
		}
	}
	at, err := repo(ctx, run)
	if err != nil {
		return err
	}
	if exists {
		args := append(at, "worktree", "remove")
		if drop {
			args = append(args, "--force")
		}
		if _, err := git(ctx, append(args, run.Worktree)...); err != nil {
			if !drop {
				return fmt.Errorf("worktree %s is kept: %w", run.Worktree, err)
			}
			// Found clean (above): what is left of it goes.
			if err := os.RemoveAll(run.Worktree); err != nil {
				return err
			}
		}
	}
	if _, err := git(ctx, append(at, "worktree", "prune")...); err != nil {
		return err
	}
	if drop && run.Branch != "" {
		tip, err := branchTip(ctx, at, run.Branch)
		if err != nil {
			return err
		}
		if tip == run.Base {
			// Not while a worktree has it checked out, as git branch -D
			// refuses too: that worktree would be left on no branch.
			list, err := git(ctx, append(at, "worktree", "list", "--porcelain")...)
			if err != nil {
				return err
			}
			for line := range strings.SplitSeq(list, "\n") {
				if strings.TrimSpace(line) == "branch refs/heads/"+run.Branch {
					return fmt.Errorf("branch %s is checked out in another worktree, so it is kept until it is not", run.Branch)
				}
			}
			// Deleted only if it still points there, as one change.
			if _, err := git(ctx, append(at, "update-ref", "-d", "refs/heads/"+run.Branch, run.Base)...); err != nil {
				return err
			}
		}
	}
	return nil
}

// holdsRepository returns a repository (a .git entry) found anywhere below
// root other than root's own, or "".
func holdsRepository(root string) (string, error) {
	found := ""
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.Name() != ".git":
			return nil
		case path != filepath.Join(root, ".git"):
			found = path
			return fs.SkipAll
		case d.IsDir():
			return fs.SkipDir
		}
		return nil
	})
	return found, err
}

// commitWork commits what is uncommitted in run's worktree, if anything,
// without any hook or signing (harness commits for the agent, unattended),
// as the repository's user or else as "harness". It reports whether there
// was anything to commit.
func commitWork(ctx context.Context, run *Run, message string) (bool, error) {
	status, err := git(ctx, "-C", run.Worktree, "status", "--porcelain")
	if err != nil || status == "" {
		return false, err
	}
	if _, err := git(ctx, "-C", run.Worktree, "add", "-A"); err != nil {
		return false, err
	}
	// git is given a hooks folder of its own, new and private, so that no
	// hook runs, whatever the repository or anyone else put anywhere.
	hooks, err := os.MkdirTemp("", "harness-no-hooks-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(hooks)
	args := []string{"-C", run.Worktree, "-c", "commit.gpgsign=false", "-c", "core.hooksPath=" + hooks}
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
	if head, err := git(ctx, "-C", run.Worktree, "symbolic-ref", "-q", "HEAD"); err != nil || head != "refs/heads/"+run.Branch {
		stat += fmt.Sprintf("; the agent moved HEAD off %s, so its work is not all on the branch (the worktree is kept)", run.Branch)
	}
	return stat, nil
}
