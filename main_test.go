package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBinaryPrintsVersion(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "ripples")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, output)
	}

	command := exec.Command(binary, "--version")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("ripples --version: %v\n%s", err, output)
	}
	var info struct {
		GitTag    string `json:"gitTag"`
		BuildTime string `json:"buildTime"`
		GoVersion string `json:"goVersion"`
	}
	if err := json.Unmarshal(output, &info); err != nil {
		t.Fatalf("ripples --version output = %q: %v", output, err)
	}
	if info.GitTag == "" || info.BuildTime == "" || info.GoVersion == "" {
		t.Fatalf("ripples --version output has empty fields: %+v", info)
	}
}

func TestRunRequiresRevisions(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run() code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "必须指定 -old 和 -new") {
		t.Fatalf("run() stderr = %q", stderr.String())
	}
}

func TestRunPrintsAffectedPackages(t *testing.T) {
	repo := initCLIRepository(t)
	writeCLIFile(t, repo, "go.mod", "module example.com/app\n\ngo 1.25\n")
	writeCLIFile(t, repo, "lib/lib.go", `package lib

func Value() string { return "old" }
`)
	writeCLIFile(t, repo, "cmd/server/main.go", `package main

import "example.com/app/lib"

func main() { _ = lib.Value() }
`)
	oldCommit := commitCLIRepository(t, repo, "old")
	writeCLIFile(t, repo, "lib/lib.go", `package lib

func Value() string { return "new" }
`)
	newCommit := commitCLIRepository(t, repo, "new")

	t.Setenv("RIPPLES_CACHE", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"-repo", repo,
		"-old", oldCommit,
		"-new", newCommit,
		"-output", "simple",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() code = %d, want 0; stderr=%s", code, stderr.String())
	}
	want := "cmd/server.main\nlib.lib\n"
	if stdout.String() != want {
		t.Fatalf("run() stdout = %q, want %q", stdout.String(), want)
	}
}

func TestRunPrintsDOTImpactGraph(t *testing.T) {
	repo := initCLIRepository(t)
	writeCLIFile(t, repo, "go.mod", "module example.com/app\n\ngo 1.25\n")
	writeCLIFile(t, repo, "lib/lib.go", `package lib

func Value() string { return "old" }
`)
	writeCLIFile(t, repo, "cmd/server/main.go", `package main

import "example.com/app/lib"

func main() { _ = lib.Value() }
`)
	oldCommit := commitCLIRepository(t, repo, "old")
	writeCLIFile(t, repo, "lib/lib.go", `package lib

func Value() string { return "new" }
`)
	newCommit := commitCLIRepository(t, repo, "new")

	t.Setenv("RIPPLES_CACHE", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"-repo", repo,
		"-old", oldCommit,
		"-new", newCommit,
		"-output", "dot",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() code = %d, want 0; stderr=%s", code, stderr.String())
	}
	want := `digraph ripples {
  rankdir="LR";
  node [shape="box"];
  "example.com/app/cmd/server" [label="cmd/server.main"];
  "example.com/app/lib" [label="lib.lib", color="#cf222e", penwidth="2"];
  "example.com/app/lib" -> "example.com/app/cmd/server";
}
`
	if stdout.String() != want {
		t.Fatalf("run() stdout = %q, want %q", stdout.String(), want)
	}
}

func initCLIRepository(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitCLI(t, dir, "init", "-q")
	gitCLI(t, dir, "config", "user.name", "Ripples Test")
	gitCLI(t, dir, "config", "user.email", "ripples@example.com")
	return dir
}

func writeCLIFile(t *testing.T, repo, name, content string) {
	t.Helper()
	filename := filepath.Join(repo, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commitCLIRepository(t *testing.T, repo, message string) string {
	t.Helper()
	gitCLI(t, repo, "add", "-A")
	gitCLI(t, repo, "commit", "-q", "-m", message)
	return gitCLI(t, repo, "rev-parse", "HEAD")
}

func gitCLI(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}
