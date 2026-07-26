package impact

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jimyag/ripples/internal/snapshot"
)

func TestAnalyzeReturnsChangedDeclarationAndTransitiveCallers(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "payment/payment.go", `package payment

func Pay() string { return "old" }
`)
	writeModuleFile(t, repo, "internal/order/order.go", `package order

import "example.com/app/payment"

func Create() string { return payment.Pay() }
`)
	writeModuleFile(t, repo, "cmd/server/main.go", `package main

import "example.com/app/internal/order"

func main() { _ = order.Create() }
`)
	writeModuleFile(t, repo, "cmd/other/main.go", `package main

func main() {}
`)
	oldCommit := commitModule(t, repo, "old")

	writeModuleFile(t, repo, "payment/payment.go", `package payment

func Pay() string { return "new" }
`)
	newCommit := commitModule(t, repo, "new")

	analyzer := NewAnalyzer(&snapshot.Cache{Dir: t.TempDir()})
	got, err := analyzer.Analyze(context.Background(), repo, oldCommit, newCommit)
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	assertPackages(t, got, []string{
		"cmd/server.main",
		"internal/order.order",
		"payment.payment",
	})
}

func TestAnalyzeDoesNotPropagateThroughUnrelatedDeclaration(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "shared/shared.go", `package shared

func Used() string { return "used" }
func Unrelated() string { return "old" }
`)
	writeModuleFile(t, repo, "consumer/consumer.go", `package consumer

import "example.com/app/shared"

func Value() string { return shared.Used() }
`)
	writeModuleFile(t, repo, "cmd/server/main.go", `package main

import "example.com/app/consumer"

func main() { _ = consumer.Value() }
`)
	oldCommit := commitModule(t, repo, "old")

	writeModuleFile(t, repo, "shared/shared.go", `package shared

func Used() string { return "used" }
func Unrelated() string { return "new" }
`)
	newCommit := commitModule(t, repo, "new")

	analyzer := NewAnalyzer(&snapshot.Cache{Dir: t.TempDir()})
	got, err := analyzer.Analyze(context.Background(), repo, oldCommit, newCommit)
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	assertPackages(t, got, []string{"shared.shared"})
}

func TestAnalyzeDoesNotPropagateAddedUnusedInterfaceMethod(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "client/client.go", `package client

type API interface {
	Used() string
}

type Client struct{}

func (Client) Used() string { return "used" }
`)
	writeModuleFile(t, repo, "consumer/consumer.go", `package consumer

import "example.com/app/client"

func Value(api client.API) string { return api.Used() }
`)
	oldCommit := commitModule(t, repo, "old")

	writeModuleFile(t, repo, "client/client.go", `package client

type API interface {
	Used() string
	Unrelated() string
}

type Client struct{}

func (Client) Used() string { return "used" }
func (Client) Unrelated() string { return "new" }
`)
	newCommit := commitModule(t, repo, "new")

	analyzer := NewAnalyzer(&snapshot.Cache{Dir: t.TempDir()})
	got, err := analyzer.Analyze(context.Background(), repo, oldCommit, newCommit)
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	assertPackages(t, got, []string{"client.client"})
}

func TestAnalyzeDoesNotPropagateAddedUnusedDeclaration(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "shared/shared.go", `package shared

func Used() string { return "used" }
`)
	writeModuleFile(t, repo, "consumer/consumer.go", `package consumer

import "example.com/app/shared"

func Value() string { return shared.Used() }
`)
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "shared/shared.go", `package shared

func Used() string { return "used" }
func Added() string { return "added" }
`)
	newCommit := commitModule(t, repo, "new")

	analyzer := NewAnalyzer(&snapshot.Cache{Dir: t.TempDir()})
	got, err := analyzer.Analyze(context.Background(), repo, oldCommit, newCommit)
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	assertPackages(t, got, []string{"shared.shared"})
}

func TestAnalyzeDoesNotPropagateDeletedUnusedDeclaration(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "shared/shared.go", `package shared

func Used() string { return "used" }
func Removed() string { return "removed" }
`)
	writeModuleFile(t, repo, "consumer/consumer.go", `package consumer

import "example.com/app/shared"

func Value() string { return shared.Used() }
`)
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "shared/shared.go", `package shared

func Used() string { return "used" }
`)
	newCommit := commitModule(t, repo, "new")

	analyzer := NewAnalyzer(&snapshot.Cache{Dir: t.TempDir()})
	got, err := analyzer.Analyze(context.Background(), repo, oldCommit, newCommit)
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	assertPackages(t, got, []string{"shared.shared"})
}

func TestAnalyzeUsesOldGraphForDeletedPackage(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "legacy/legacy.go", `package legacy

func Value() string { return "legacy" }
`)
	writeModuleFile(t, repo, "consumer/consumer.go", `package consumer

import "example.com/app/legacy"

func Value() string { return legacy.Value() }
`)
	oldCommit := commitModule(t, repo, "old")

	if err := os.RemoveAll(filepath.Join(repo, "legacy")); err != nil {
		t.Fatal(err)
	}
	writeModuleFile(t, repo, "consumer/consumer.go", `package consumer

func Value() string { return "replacement" }
`)
	newCommit := commitModule(t, repo, "new")

	analyzer := NewAnalyzer(&snapshot.Cache{Dir: t.TempDir()})
	got, err := analyzer.Analyze(context.Background(), repo, oldCommit, newCommit)
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	assertPackages(t, got, []string{
		"consumer.consumer",
		"legacy.legacy",
	})
}

func TestAnalyzeReturnsAddedPackage(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "existing/existing.go", "package existing\n")
	oldCommit := commitModule(t, repo, "old")

	writeModuleFile(t, repo, "added/added.go", "package added\n")
	newCommit := commitModule(t, repo, "new")

	analyzer := NewAnalyzer(&snapshot.Cache{Dir: t.TempDir()})
	got, err := analyzer.Analyze(context.Background(), repo, oldCommit, newCommit)
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	assertPackages(t, got, []string{"added.added"})
}

func TestAnalyzeIgnoresCommentOnlyChanges(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "payment/payment.go", `package payment

// Pay returns a value.
func Pay() string { return "value" }
`)
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "payment/payment.go", `package payment

// Pay returns the current value.
func Pay() string { return "value" }
`)
	newCommit := commitModule(t, repo, "new")

	analyzer := NewAnalyzer(&snapshot.Cache{Dir: t.TempDir()})
	got, err := analyzer.Analyze(context.Background(), repo, oldCommit, newCommit)
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Analyze() = %v, want no affected packages", got)
	}
}

func TestLoadSnapshotUsesPersistentCache(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "payment/payment.go", "package payment\n")
	commitModule(t, repo, "initial")

	analyzer := NewAnalyzer(&snapshot.Cache{Dir: t.TempDir()})
	first, err := analyzer.LoadSnapshot(context.Background(), repo, "HEAD")
	if err != nil {
		t.Fatalf("LoadSnapshot(first) error = %v", err)
	}
	if first.Cached {
		t.Fatal("LoadSnapshot(first).Cached = true, want false")
	}
	second, err := analyzer.LoadSnapshot(context.Background(), repo, "HEAD")
	if err != nil {
		t.Fatalf("LoadSnapshot(second) error = %v", err)
	}
	if !second.Cached {
		t.Fatal("LoadSnapshot(second).Cached = false, want true")
	}
	if !reflect.DeepEqual(first.Packages, second.Packages) {
		t.Fatalf("cached packages differ:\nfirst=%v\nsecond=%v", first.Packages, second.Packages)
	}
}

func assertPackages(t *testing.T, packages []Package, want []string) {
	t.Helper()
	got := make([]string, 0, len(packages))
	for _, pkg := range packages {
		got = append(got, pkg.RelativePath+"."+pkg.Name)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("packages = %v, want %v", got, want)
	}
}

func initModule(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitInModule(t, dir, "init", "-q")
	gitInModule(t, dir, "config", "user.name", "Ripples Test")
	gitInModule(t, dir, "config", "user.email", "ripples@example.com")
	writeModuleFile(t, dir, "go.mod", "module example.com/app\n\ngo 1.25\n")
	return dir
}

func writeModuleFile(t *testing.T, repo, name, content string) {
	t.Helper()
	filename := filepath.Join(repo, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commitModule(t *testing.T, repo, message string) string {
	t.Helper()
	gitInModule(t, repo, "add", "-A")
	gitInModule(t, repo, "commit", "-q", "-m", message)
	return gitInModule(t, repo, "rev-parse", "HEAD")
}

func gitInModule(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}
