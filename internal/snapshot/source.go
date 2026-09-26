package snapshot

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Source is an immutable export of a Git tree in a temporary directory. Dir
// points to the same repository-relative directory passed to Resolve. Close
// removes the export.
type Source struct {
	RepoPath string
	GitRoot  string
	Subdir   string
	Commit   string
	Tree     string
	Dir      string

	tempDir string
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

	gitRoot, err := gitOutput(ctx, repoPath, nil, "rev-parse", "--show-toplevel")
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

	commit, err := gitOutput(ctx, repoPath, nil, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return nil, err
	}
	tree, err := gitOutput(ctx, repoPath, nil, "rev-parse", "--verify", commit+"^{tree}")
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

// Open resolves ref and exports it without changing the repository.
func Open(ctx context.Context, repoPath, ref string) (*Source, error) {
	revision, err := Resolve(ctx, repoPath, ref)
	if err != nil {
		return nil, err
	}
	return OpenRevision(ctx, revision)
}

// OpenRevision exports the complete tree of a resolved revision into a
// temporary directory, preserving the repository layout.
func OpenRevision(ctx context.Context, revision *Revision) (*Source, error) {
	tempDir, err := os.MkdirTemp("", "ripples-source-*")
	if err != nil {
		return nil, fmt.Errorf("create source directory: %w", err)
	}
	root := filepath.Join(tempDir, "tree")
	source := &Source{
		RepoPath: revision.RepoPath,
		GitRoot:  revision.GitRoot,
		Subdir:   revision.Subdir,
		Commit:   revision.Commit,
		Tree:     revision.Tree,
		Dir:      filepath.Join(root, revision.Subdir),
		tempDir:  tempDir,
	}

	// A private index keeps the repository index, worktree list, hooks and
	// sparse-checkout state untouched. Sparse checkout is disabled so the
	// export always contains the whole tree, and LFS objects stay pointers
	// because Go analysis never needs their content.
	env := append(os.Environ(),
		"GIT_INDEX_FILE="+filepath.Join(tempDir, "index"),
		"GIT_LFS_SKIP_SMUDGE=1",
	)
	for _, args := range [][]string{
		{"-c", "core.sparseCheckout=false", "read-tree", revision.Tree},
		{"-c", "core.sparseCheckout=false", "checkout-index", "--all", "--prefix=" + root + string(filepath.Separator)},
	} {
		if _, err := gitOutput(ctx, revision.GitRoot, env, args...); err != nil {
			_ = source.Close()
			return nil, fmt.Errorf("export tree %s: %w", revision.Tree, err)
		}
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

// Close removes the exported tree.
func (s *Source) Close() error {
	if s == nil || s.tempDir == "" {
		return nil
	}
	return os.RemoveAll(s.tempDir)
}

func gitOutput(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}
