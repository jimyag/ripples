package snapshot

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenExtractsRequestedCommitWithoutChangingWorktree(t *testing.T) {
	repo := initRepository(t)
	writeFile(t, filepath.Join(repo, "value.txt"), "old")
	oldCommit := commitAll(t, repo, "old")
	writeFile(t, filepath.Join(repo, "value.txt"), "new")
	newCommit := commitAll(t, repo, "new")

	oldSource, err := Open(context.Background(), repo, oldCommit)
	if err != nil {
		t.Fatalf("Open(old) error = %v", err)
	}
	defer oldSource.Close()
	newSource, err := Open(context.Background(), repo, newCommit)
	if err != nil {
		t.Fatalf("Open(new) error = %v", err)
	}
	defer newSource.Close()

	assertFileContent(t, filepath.Join(oldSource.Dir, "value.txt"), "old")
	assertFileContent(t, filepath.Join(newSource.Dir, "value.txt"), "new")
	assertFileContent(t, filepath.Join(repo, "value.txt"), "new")

	if got := gitCommand(t, repo, "status", "--short"); got != "" {
		t.Fatalf("repository changed while opening snapshots: %s", got)
	}
}

func TestOpenPreservesRepositoryLayoutForNestedModule(t *testing.T) {
	repo := initRepository(t)
	moduleDir := filepath.Join(repo, "src", "app")
	writeFile(t, filepath.Join(moduleDir, "go.mod"), `module example.com/app

go 1.25

require example.com/libs v0.0.0

replace example.com/libs => ../../libs
`)
	writeFile(t, filepath.Join(repo, "libs", "go.mod"), "module example.com/libs\n\ngo 1.25\n")
	writeFile(t, filepath.Join(repo, "libs", "value.go"), "package libs\n\nconst Value = 1\n")
	commit := commitAll(t, repo, "initial")

	source, err := Open(context.Background(), moduleDir, commit)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	worktreeDir := source.worktreeDir

	assertFileContent(t, filepath.Join(source.Dir, "go.mod"), `module example.com/app

go 1.25

require example.com/libs v0.0.0

replace example.com/libs => ../../libs
`)
	assertFileContent(t, filepath.Join(source.Dir, "..", "..", "libs", "value.go"), "package libs\n\nconst Value = 1\n")

	if err := source.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := os.Stat(worktreeDir); !os.IsNotExist(err) {
		t.Fatalf("worktree directory still exists after Close(): %v", err)
	}
	if got := gitCommand(t, repo, "worktree", "list", "--porcelain"); strings.Contains(got, worktreeDir) {
		t.Fatalf("worktree still registered after Close():\n%s", got)
	}
}

func TestOpenRejectsUnknownRevision(t *testing.T) {
	repo := initRepository(t)
	writeFile(t, filepath.Join(repo, "value.txt"), "value")
	commitAll(t, repo, "initial")

	if _, err := Open(context.Background(), repo, "does-not-exist"); err == nil {
		t.Fatal("Open() error = nil, want revision error")
	}
}

func TestResolveDoesNotExtractFiles(t *testing.T) {
	repo := initRepository(t)
	writeFile(t, filepath.Join(repo, "value.txt"), "value")
	commit := commitAll(t, repo, "initial")

	revision, err := Resolve(context.Background(), repo, "HEAD")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if revision.Commit != commit {
		t.Fatalf("Resolve().Commit = %q, want %q", revision.Commit, commit)
	}
	if revision.Tree == "" {
		t.Fatal("Resolve().Tree is empty")
	}
	wantRoot, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	if revision.GitRoot != wantRoot {
		t.Fatalf("Resolve().GitRoot = %q, want %q", revision.GitRoot, wantRoot)
	}
	if revision.Subdir != "." {
		t.Fatalf("Resolve().Subdir = %q, want .", revision.Subdir)
	}
}

func initRepository(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitCommand(t, dir, "init", "-q")
	gitCommand(t, dir, "config", "user.name", "Ripples Test")
	gitCommand(t, dir, "config", "user.email", "ripples@example.com")
	return dir
}

func commitAll(t *testing.T, repo, message string) string {
	t.Helper()
	gitCommand(t, repo, "add", ".")
	gitCommand(t, repo, "commit", "-q", "-m", message)
	return gitCommand(t, repo, "rev-parse", "HEAD")
}

func gitCommand(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func writeFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertFileContent(t *testing.T, name, want string) {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != want {
		t.Fatalf("%s = %q, want %q", name, got, want)
	}
}
