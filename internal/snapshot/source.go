package snapshot

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Source is an immutable checkout of a Git commit extracted into a temporary
// directory. Close removes the temporary directory.
type Source struct {
	RepoPath string
	Commit   string
	Tree     string
	Dir      string
}

// Revision identifies an immutable Git tree.
type Revision struct {
	RepoPath string
	Commit   string
	Tree     string
}

// Resolve resolves a Git ref without changing the repository worktree.
func Resolve(ctx context.Context, repoPath, ref string) (*Revision, error) {
	repoPath, err := filepath.Abs(repoPath)
	if err != nil {
		return nil, fmt.Errorf("resolve repository path: %w", err)
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
		Commit:   commit,
		Tree:     tree,
	}, nil
}

// Open resolves ref and extracts it without changing the repository worktree.
func Open(ctx context.Context, repoPath, ref string) (*Source, error) {
	revision, err := Resolve(ctx, repoPath, ref)
	if err != nil {
		return nil, err
	}
	return OpenRevision(ctx, revision)
}

// OpenRevision extracts a resolved revision.
func OpenRevision(ctx context.Context, revision *Revision) (*Source, error) {
	dir, err := os.MkdirTemp("", "ripples-snapshot-*")
	if err != nil {
		return nil, fmt.Errorf("create snapshot directory: %w", err)
	}

	source := &Source{
		RepoPath: revision.RepoPath,
		Commit:   revision.Commit,
		Tree:     revision.Tree,
		Dir:      dir,
	}
	if err := extractArchive(ctx, source); err != nil {
		_ = source.Close()
		return nil, err
	}
	return source, nil
}

// Close removes the extracted snapshot.
func (s *Source) Close() error {
	if s == nil || s.Dir == "" {
		return nil
	}
	return os.RemoveAll(s.Dir)
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

func extractArchive(ctx context.Context, source *Source) error {
	cmd := exec.CommandContext(ctx, "git", "archive", "--format=tar", source.Commit)
	cmd.Dir = source.RepoPath

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open git archive output: %w", err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start git archive: %w", err)
	}

	extractErr := extractTar(source.Dir, stdout)
	waitErr := cmd.Wait()
	if extractErr != nil {
		return extractErr
	}
	if waitErr != nil {
		return fmt.Errorf("git archive: %w: %s", waitErr, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func extractTar(root string, reader io.Reader) error {
	tr := tar.NewReader(reader)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read git archive: %w", err)
		}

		target, err := archiveTarget(root, header.Name)
		if err != nil {
			return err
		}

		switch header.Typeflag {
		case tar.TypeXHeader, tar.TypeXGlobalHeader:
			continue
		case tar.TypeDir:
			if err := os.MkdirAll(target, header.FileInfo().Mode().Perm()); err != nil {
				return fmt.Errorf("create snapshot directory %s: %w", header.Name, err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("create snapshot parent %s: %w", header.Name, err)
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, header.FileInfo().Mode().Perm())
			if err != nil {
				return fmt.Errorf("create snapshot file %s: %w", header.Name, err)
			}
			_, copyErr := io.Copy(file, tr)
			closeErr := file.Close()
			if copyErr != nil {
				return fmt.Errorf("extract snapshot file %s: %w", header.Name, copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("close snapshot file %s: %w", header.Name, closeErr)
			}
		case tar.TypeSymlink:
			if err := extractSymlink(root, target, header); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported archive entry %s with type %d", header.Name, header.Typeflag)
		}
	}
}

func archiveTarget(root, name string) (string, error) {
	name = filepath.Clean(filepath.FromSlash(name))
	if name == "." || filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe archive path %q", name)
	}
	target := filepath.Join(root, name)
	if !isWithin(root, target) {
		return "", fmt.Errorf("archive path escapes snapshot root: %q", name)
	}
	return target, nil
}

func extractSymlink(root, target string, header *tar.Header) error {
	if filepath.IsAbs(header.Linkname) {
		return fmt.Errorf("unsafe absolute symlink %s -> %s", header.Name, header.Linkname)
	}
	resolved := filepath.Clean(filepath.Join(filepath.Dir(target), filepath.FromSlash(header.Linkname)))
	if !isWithin(root, resolved) {
		return fmt.Errorf("symlink escapes snapshot root: %s -> %s", header.Name, header.Linkname)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create symlink parent %s: %w", header.Name, err)
	}
	if err := os.Symlink(filepath.FromSlash(header.Linkname), target); err != nil {
		return fmt.Errorf("create snapshot symlink %s: %w", header.Name, err)
	}
	return nil
}

func isWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
