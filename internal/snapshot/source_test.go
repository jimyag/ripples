package snapshot

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
	t.Cleanup(func() {
		if err := oldSource.Close(); err != nil {
			t.Errorf("Close(old) error = %v", err)
		}
	})
	newSource, err := Open(context.Background(), repo, newCommit)
	if err != nil {
		t.Fatalf("Open(new) error = %v", err)
	}
	t.Cleanup(func() {
		if err := newSource.Close(); err != nil {
			t.Errorf("Close(new) error = %v", err)
		}
	})

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
	exportDir := source.tempDir

	assertFileContent(t, filepath.Join(source.Dir, "go.mod"), `module example.com/app

go 1.25

require example.com/libs v0.0.0

replace example.com/libs => ../../libs
`)
	assertFileContent(t, filepath.Join(source.Dir, "..", "..", "libs", "value.go"), "package libs\n\nconst Value = 1\n")

	if err := source.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := os.Stat(exportDir); !os.IsNotExist(err) {
		t.Fatalf("export directory still exists after Close(): %v", err)
	}
}

func TestOpenRevisionSupportsConcurrentExports(t *testing.T) {
	const worktreeCount = 16

	repo := initRepository(t)
	writeFile(t, filepath.Join(repo, "value.txt"), "old")
	oldCommit := commitAll(t, repo, "old")
	writeFile(t, filepath.Join(repo, "value.txt"), "new")
	newCommit := commitAll(t, repo, "new")

	revisions := make([]*Revision, worktreeCount)
	for index := range revisions {
		commit := oldCommit
		if index%2 == 1 {
			commit = newCommit
		}
		revision, err := Resolve(context.Background(), repo, commit)
		if err != nil {
			t.Fatalf("Resolve(%s) error = %v", commit, err)
		}
		revisions[index] = revision
	}

	sources := make([]*Source, len(revisions))
	errors := make([]error, len(revisions))
	var wait sync.WaitGroup
	for index := range revisions {
		wait.Go(func() {
			sources[index], errors[index] = OpenRevision(context.Background(), revisions[index])
		})
	}
	wait.Wait()
	for index, err := range errors {
		if err != nil {
			t.Fatalf("OpenRevision(%d) error = %v", index, err)
		}
	}
	for index, source := range sources {
		want := "old"
		if index%2 == 1 {
			want = "new"
		}
		assertFileContent(t, filepath.Join(source.Dir, "value.txt"), want)
	}

	for index := range sources {
		wait.Go(func() {
			errors[index] = sources[index].Close()
		})
	}
	wait.Wait()
	for index, err := range errors {
		if err != nil {
			t.Fatalf("Close(%d) error = %v", index, err)
		}
	}

	worktrees := gitCommand(t, repo, "worktree", "list", "--porcelain")
	if strings.Count(worktrees, "\nworktree ") != 0 {
		t.Fatalf("temporary worktrees remain registered:\n%s", worktrees)
	}
}

// Large trees are written by several git processes at once.
func TestOpenRevisionExportsLargeTreesCompletely(t *testing.T) {
	repo := initRepository(t)
	for dir := range 40 {
		for file := range 20 {
			writeFile(t, filepath.Join(repo, fmt.Sprintf("pkg%02d/nested/file%02d.go", dir, file)), fmt.Sprintf("package p%d_%d\n", dir, file))
		}
	}
	if err := os.Symlink("pkg00/nested/file00.go", filepath.Join(repo, "link.go")); err != nil {
		t.Fatal(err)
	}
	commit := commitAll(t, repo, "large")

	source, err := Open(context.Background(), repo, commit)
	if err != nil {
		t.Fatal(err)
	}
	for dir := range 40 {
		for file := range 20 {
			assertFileContent(t, filepath.Join(source.Dir, fmt.Sprintf("pkg%02d/nested/file%02d.go", dir, file)), fmt.Sprintf("package p%d_%d\n", dir, file))
		}
	}
	if target, err := os.Readlink(filepath.Join(source.Dir, "link.go")); err != nil || target != "pkg00/nested/file00.go" {
		t.Fatalf("link.go -> %q, %v; want pkg00/nested/file00.go", target, err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(source.tempDir); !os.IsNotExist(err) {
		t.Fatalf("export directory still exists: %v", err)
	}
}

func TestOpenRevisionCleansUpWhenSubdirectoryIsMissing(t *testing.T) {
	repo := initRepository(t)
	writeFile(t, filepath.Join(repo, "value.txt"), "value")
	commit := commitAll(t, repo, "initial")
	revision, err := Resolve(context.Background(), repo, commit)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	revision.Subdir = filepath.Join("missing", "module")
	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)

	if _, err := OpenRevision(context.Background(), revision); err == nil {
		t.Fatal("OpenRevision() error = nil, want missing subdirectory error")
	}
	if entries, err := os.ReadDir(tempDir); err != nil || len(entries) != 0 {
		t.Fatalf("failed snapshot left temporary files: %v %v", entries, err)
	}
}

func TestOpenRevisionLeavesRepositoryMetadataAndHooksUntouched(t *testing.T) {
	repo := initRepository(t)
	writeFile(t, filepath.Join(repo, "value.txt"), "value")
	commit := commitAll(t, repo, "initial")
	marker := filepath.Join(t.TempDir(), "hook-ran")
	hook := filepath.Join(repo, ".git", "hooks", "post-checkout")
	writeFile(t, hook, "#!/bin/sh\ntouch '"+marker+"'\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}

	source, err := Open(context.Background(), repo, commit)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if got := gitCommand(t, repo, "worktree", "list", "--porcelain"); strings.Count(got, "worktree ") != 1 {
		t.Fatalf("snapshot registered a worktree:\n%s", got)
	}
	if err := source.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("post-checkout hook ran while opening a snapshot: %v", err)
	}
}

func TestOpenRevisionExtractsFilesOutsideSparseCheckout(t *testing.T) {
	repo := initRepository(t)
	writeFile(t, filepath.Join(repo, "api", "api.txt"), "api")
	writeFile(t, filepath.Join(repo, "billing", "billing.txt"), "billing")
	commit := commitAll(t, repo, "initial")
	gitCommand(t, repo, "sparse-checkout", "set", "api")

	source, err := Open(context.Background(), repo, commit)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := source.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	assertFileContent(t, filepath.Join(source.Dir, "billing", "billing.txt"), "billing")
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
