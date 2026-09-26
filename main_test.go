package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"
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
	code := run(t.Context(), nil, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run() code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "error: -old and -new are required") {
		t.Fatalf("run() stderr = %q", stderr.String())
	}
	assertNoChinese(t, stderr.String())
}

func TestRunHelpIsEnglishAndIncludesExamples(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"-h"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run(-h) code = %d, want 0", code)
	}
	want := []string{
		"Usage: ripples -old <ref> -new <ref> [options]",
		"ripples -repo . -old HEAD~1 -new HEAD",
		"ripples -repo . -old origin/main -new HEAD -output dot > impact.dot",
	}
	for _, text := range want {
		if !strings.Contains(stderr.String(), text) {
			t.Errorf("run(-h) stderr does not contain %q:\n%s", text, stderr.String())
		}
	}
	assertNoChinese(t, stderr.String())
}

func assertNoChinese(t *testing.T, text string) {
	t.Helper()
	for _, r := range text {
		if unicode.Is(unicode.Han, r) {
			t.Fatalf("output contains Chinese character %q: %q", r, text)
		}
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
	code := run(t.Context(), []string{
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

func TestRunAnalyzesNestedModuleWithRepositoryLocalReplace(t *testing.T) {
	repo := initCLIRepository(t)
	moduleDir := filepath.Join(repo, "src", "app")
	writeCLIFile(t, moduleDir, "go.mod", `module example.com/app

go 1.25

require example.com/libs v0.0.0

replace example.com/libs => ../../libs
`)
	writeCLIFile(t, repo, "libs/go.mod", "module example.com/libs\n\ngo 1.25\n")
	writeCLIFile(t, repo, "libs/value.go", `package libs

func Value() string { return "value" }
`)
	writeCLIFile(t, moduleDir, "service/service.go", `package service

import "example.com/libs"

func Value() string { return libs.Value() }
`)
	writeCLIFile(t, moduleDir, "cmd/server/main.go", `package main

import "example.com/app/service"

func main() { _ = service.Value() }
`)
	oldCommit := commitCLIRepository(t, repo, "old")
	writeCLIFile(t, moduleDir, "service/service.go", `package service

import "example.com/libs"

func Value() string { return libs.Value() + "!" }
`)
	newCommit := commitCLIRepository(t, repo, "new")

	t.Setenv("RIPPLES_CACHE", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{
		"-repo", moduleDir,
		"-old", oldCommit,
		"-new", newCommit,
		"-output", "simple",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() code = %d, want 0; stderr=%s", code, stderr.String())
	}
	want := "cmd/server.main\nservice.service\n"
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
	code := run(t.Context(), []string{
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
	n1[label="cmd/server.main",shape="box"];
	n2[color="#cf222e",label="lib.lib",penwidth="2",shape="box"];
	n2->n1;` + "\n\t\n}\n"
	if stdout.String() != want {
		t.Fatalf("run() stdout = %q, want %q", stdout.String(), want)
	}
}

func TestRunRejectsUnknownOutputBeforeAnalysis(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{
		"-repo", filepath.Join(t.TempDir(), "missing"),
		"-old", "HEAD~1",
		"-new", "HEAD",
		"-output", "jsno",
	}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), `unsupported output format "jsno"`) {
		t.Fatalf("run() code = %d, stderr = %q; want output format error", code, stderr.String())
	}
}

func TestRunPreparesExportedSourceBeforeAnalysis(t *testing.T) {
	repo := initCLIRepository(t)
	writeCLIFile(t, repo, "go.mod", "module example.com/app\n\ngo 1.25\n")
	writeCLIFile(t, repo, ".gitignore", "api/*.gen.go\n")
	writeCLIFile(t, repo, "api/doc.go", "package api\n")
	writeCLIFile(t, repo, "server/server.go", `package server

import "example.com/app/api"

func Handle(request api.Request) int { return request.ID }
`)
	writeCLIFile(t, repo, "lib/lib.go", "package lib\n\nfunc Value() int { return 1 }\n")
	oldCommit := commitCLIRepository(t, repo, "old")
	writeCLIFile(t, repo, "lib/lib.go", "package lib\n\nfunc Value() int { return 2 }\n")
	newCommit := commitCLIRepository(t, repo, "new")
	t.Setenv("RIPPLES_CACHE", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{
		"-repo", repo,
		"-old", oldCommit,
		"-new", newCommit,
		"-prepare", `printf 'package api\n\ntype Request struct{ ID int }\n' > api/request.gen.go`,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if want := "lib.lib\n"; stdout.String() != want {
		t.Fatalf("run() stdout = %q, want %q", stdout.String(), want)
	}
}

func TestRunAnalyzesTestsOnlyWhenRequested(t *testing.T) {
	repo := initCLIRepository(t)
	writeCLIFile(t, repo, "go.mod", "module example.com/app\n\ngo 1.25\n")
	writeCLIFile(t, repo, "order/order.go", "package order\n\nfunc Total() int { return 1 }\n")
	writeCLIFile(t, repo, "order/order_test.go", "package order\n\nfunc helper() int { return Total() + 1 }\n")
	oldCommit := commitCLIRepository(t, repo, "old")
	writeCLIFile(t, repo, "order/order_test.go", "package order\n\nfunc helper() int { return Total() + 2 }\n")
	newCommit := commitCLIRepository(t, repo, "new")
	t.Setenv("RIPPLES_CACHE", t.TempDir())

	for _, test := range []struct {
		args []string
		want string
	}{
		{want: ""},
		{args: []string{"-tests"}, want: "order.order\n"},
	} {
		var stdout, stderr bytes.Buffer
		args := append([]string{"-repo", repo, "-old", oldCommit, "-new", newCommit}, test.args...)
		if code := run(t.Context(), args, &stdout, &stderr); code != 0 {
			t.Fatalf("run(%v) code = %d; stderr=%s", test.args, code, stderr.String())
		}
		if stdout.String() != test.want {
			t.Fatalf("run(%v) stdout = %q, want %q", test.args, stdout.String(), test.want)
		}
	}
}

func TestRunPrunesStaleCacheEntries(t *testing.T) {
	repo := initCLIRepository(t)
	writeCLIFile(t, repo, "go.mod", "module example.com/app\n\ngo 1.25\n")
	writeCLIFile(t, repo, "lib/lib.go", "package lib\n\nfunc Value() int { return 1 }\n")
	oldCommit := commitCLIRepository(t, repo, "old")
	writeCLIFile(t, repo, "lib/lib.go", "package lib\n\nfunc Value() int { return 2 }\n")
	newCommit := commitCLIRepository(t, repo, "new")
	cacheDir := t.TempDir()
	t.Setenv("RIPPLES_CACHE", cacheDir)
	stale := filepath.Join(cacheDir, "package-snapshots", "stale.json")
	writeCLIFile(t, cacheDir, "package-snapshots/stale.json", "{}")
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"-repo", repo, "-old", oldCommit, "-new", newCommit}, &stdout, &stderr); code != 0 {
		t.Fatalf("run() code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale cache entry still exists: %v", err)
	}
}

func TestRunStopsWhenContextIsCanceled(t *testing.T) {
	repo := initCLIRepository(t)
	writeCLIFile(t, repo, "go.mod", "module example.com/app\n\ngo 1.25\n")
	writeCLIFile(t, repo, "lib/lib.go", "package lib\n\nfunc Value() string { return \"old\" }\n")
	oldCommit := commitCLIRepository(t, repo, "old")
	writeCLIFile(t, repo, "lib/lib.go", "package lib\n\nfunc Value() string { return \"new\" }\n")
	newCommit := commitCLIRepository(t, repo, "new")
	t.Setenv("RIPPLES_CACHE", t.TempDir())

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var stdout, stderr bytes.Buffer
	code := run(ctx, []string{"-repo", repo, "-old", oldCommit, "-new", newCommit}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), context.Canceled.Error()) {
		t.Fatalf("run() code = %d, stderr = %q; want canceled analysis", code, stderr.String())
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
