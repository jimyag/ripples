package snapshot

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Source is an immutable checkout of a Git commit in a temporary detached
// worktree. Dir points to the same repository-relative directory passed to
// Resolve. Close removes the worktree.
type Source struct {
	RepoPath string
	GitRoot  string
	Subdir   string
	Commit   string
	Tree     string
	Dir      string

	tempDir     string
	worktreeDir string
	closeOnce   sync.Once
	closeErr    error
}

// Revision identifies an immutable Git tree.
type Revision struct {
	RepoPath string
	GitRoot  string
	Subdir   string
	Commit   string
	Tree     string
}

// Resolve resolves a Git ref without changing the repository worktree.
func Resolve(ctx context.Context, repoPath, ref string) (*Revision, error) {
	repoPath, err := filepath.Abs(repoPath)
	if err != nil {
		return nil, fmt.Errorf("resolve repository path: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(repoPath); resolveErr == nil {
		repoPath = resolved
	}

	gitRoot, err := gitOutput(ctx, repoPath, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	if resolved, resolveErr := filepath.EvalSymlinks(gitRoot); resolveErr == nil {
		gitRoot = resolved
	}
	subdir, err := filepath.Rel(gitRoot, repoPath)
	if err != nil {
		return nil, fmt.Errorf("resolve repository-relative path: %w", err)
	}
	if subdir == ".." || strings.HasPrefix(subdir, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("repository path %s is outside Git root %s", repoPath, gitRoot)
	}

	commit, err := gitOutput(ctx, repoPath, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return nil, err
	}
	tree, err := gitOutput(ctx, repoPath, "rev-parse", "--verify", commit+"^{tree}")
	if err != nil {
		return nil, err
	}

	return &Revision{
		RepoPath: repoPath,
		GitRoot:  gitRoot,
		Subdir:   subdir,
		Commit:   commit,
		Tree:     tree,
	}, nil
}

// Open resolves ref and checks it out without changing the repository worktree.
func Open(ctx context.Context, repoPath, ref string) (*Source, error) {
	revision, err := Resolve(ctx, repoPath, ref)
	if err != nil {
		return nil, err
	}
	return OpenRevision(ctx, revision)
}

// OpenRevision checks out a resolved revision in a detached worktree.
func OpenRevision(ctx context.Context, revision *Revision) (*Source, error) {
	tempDir, err := os.MkdirTemp("", "ripples-worktree-*")
	if err != nil {
		return nil, fmt.Errorf("create worktree parent: %w", err)
	}
	worktreeDir := filepath.Join(tempDir, "checkout")

	source := &Source{
		RepoPath:    revision.RepoPath,
		GitRoot:     revision.GitRoot,
		Subdir:      revision.Subdir,
		Commit:      revision.Commit,
		Tree:        revision.Tree,
		Dir:         filepath.Join(worktreeDir, revision.Subdir),
		tempDir:     tempDir,
		worktreeDir: worktreeDir,
	}
	if _, err := gitOutput(ctx, revision.GitRoot, "worktree", "add", "--detach", worktreeDir, revision.Commit); err != nil {
		_ = os.RemoveAll(tempDir)
		return nil, fmt.Errorf("create detached worktree: %w", err)
	}
	if info, err := os.Stat(source.Dir); err != nil {
		_ = source.Close()
		return nil, fmt.Errorf("open repository subdirectory %s: %w", revision.Subdir, err)
	} else if !info.IsDir() {
		_ = source.Close()
		return nil, fmt.Errorf("repository subdirectory %s is not a directory", revision.Subdir)
	}
	return source, nil
}

// Close removes the detached worktree and its temporary parent.
func (s *Source) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		if s.worktreeDir != "" {
			if _, err := gitOutput(context.Background(), s.GitRoot, "worktree", "remove", "--force", s.worktreeDir); err != nil {
				s.closeErr = err
			}
		}
		if err := os.RemoveAll(s.tempDir); err != nil && s.closeErr == nil {
			s.closeErr = err
		}
	})
	return s.closeErr
}

func gitOutput(ctx context.Context, repoPath string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repoPath
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}
