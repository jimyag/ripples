package snapshot

import (
	"archive/tar"
	"bytes"
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

func TestOpenRejectsUnknownRevision(t *testing.T) {
	repo := initRepository(t)
	writeFile(t, filepath.Join(repo, "value.txt"), "value")
	commitAll(t, repo, "initial")

	if _, err := Open(context.Background(), repo, "does-not-exist"); err == nil {
		t.Fatal("Open() error = nil, want revision error")
	}
}

func TestExtractTarIgnoresPAXMetadata(t *testing.T) {
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{
		Name:     "pax_global_header",
		Typeflag: tar.TypeXGlobalHeader,
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteHeader(&tar.Header{
		Name:     "value.txt",
		Mode:     0o644,
		Size:     int64(len("value")),
		Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("value")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if err := extractTar(dir, &archive); err != nil {
		t.Fatalf("extractTar() error = %v", err)
	}
	assertFileContent(t, filepath.Join(dir, "value.txt"), "value")
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
