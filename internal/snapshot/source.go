package snapshot

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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
	treeDir string
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
		treeDir:  root,
	}

	// A private index keeps the repository index, worktree list, hooks and
	// sparse-checkout state untouched. Sparse checkout is disabled so the
	// export always contains the whole tree, and LFS objects stay pointers
	// because Go analysis never needs their content.
	env := append(os.Environ(),
		"GIT_INDEX_FILE="+filepath.Join(tempDir, "index"),
		"GIT_LFS_SKIP_SMUDGE=1",
	)
	if _, err := gitOutput(ctx, revision.GitRoot, env, "-c", "core.sparseCheckout=false", "read-tree", revision.Tree); err != nil {
		_ = source.Close()
		return nil, fmt.Errorf("export tree %s: %w", revision.Tree, err)
	}
	if err := checkoutTree(ctx, revision.GitRoot, env, root); err != nil {
		_ = source.Close()
		return nil, fmt.Errorf("export tree %s: %w", revision.Tree, err)
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
	// Removing a large tree is dominated by file-system calls, so the
	// top-level entries are removed concurrently. A missing tree is left to
	// the final RemoveAll.
	entries, _ := os.ReadDir(s.treeDir)
	errs := make([]error, len(entries)+1)
	limit := make(chan struct{}, runtime.GOMAXPROCS(0))
	var wait sync.WaitGroup
	for index, entry := range entries {
		wait.Go(func() {
			limit <- struct{}{}
			errs[index] = os.RemoveAll(filepath.Join(s.treeDir, entry.Name()))
			<-limit
		})
	}
	wait.Wait()
	errs[len(entries)] = os.RemoveAll(s.tempDir)
	return errors.Join(errs...)
}

// checkoutTree writes the files of the private index below root. Writing
// thousands of files is bound by file-system calls, so once every directory
// exists the files are split across several git processes. Files whose
// directories differ only in case share a process, so case-insensitive file
// systems still resolve such collisions in index order.
func checkoutTree(ctx context.Context, gitRoot string, env []string, root string) error {
	list := exec.CommandContext(ctx, "git", "ls-files", "--stage", "-z")
	list.Dir = gitRoot
	list.Env = env
	var stderr strings.Builder
	list.Stderr = &stderr
	output, err := list.Output()
	if err != nil {
		return fmt.Errorf("git ls-files: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	entries := strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00")
	workers := max(1, min(runtime.GOMAXPROCS(0), 8, len(entries)/128))
	groups := make([][]string, workers)
	dirs := map[string]bool{".": true}
	for _, entry := range entries {
		// Each entry is "<mode> <object> <stage>\t<path>".
		meta, path, ok := strings.Cut(entry, "\t")
		if !ok {
			continue
		}
		if strings.HasPrefix(meta, "160000 ") {
			// Like checkout-index --all, a submodule becomes an empty directory.
			dirs[path] = true
			continue
		}
		dir := pathpkg.Dir(path)
		dirs[dir] = true
		group := fnv.New32a()
		_, _ = group.Write([]byte(strings.ToLower(dir)))
		index := group.Sum32() % uint32(workers)
		groups[index] = append(groups[index], path)
	}
	for dir := range dirs {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(dir)), 0o750); err != nil {
			return fmt.Errorf("create export directory: %w", err)
		}
	}

	errs := make([]error, workers)
	var wait sync.WaitGroup
	for index, paths := range groups {
		if len(paths) == 0 {
			continue
		}
		wait.Go(func() {
			cmd := exec.CommandContext(ctx, "git", "-c", "core.sparseCheckout=false",
				"checkout-index", "-z", "--stdin", "--prefix="+root+string(filepath.Separator))
			cmd.Dir = gitRoot
			cmd.Env = env
			cmd.Stdin = strings.NewReader(strings.Join(paths, "\x00") + "\x00")
			if output, err := cmd.CombinedOutput(); err != nil {
				errs[index] = fmt.Errorf("git checkout-index: %w: %s", err, strings.TrimSpace(string(output)))
			}
		})
	}
	wait.Wait()
	return errors.Join(errs...)
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
