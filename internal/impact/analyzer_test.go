package impact

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jimyag/ripples/internal/snapshot"
)

func TestLoadSnapshotPairRunsConcurrently(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseLoads := func() {
		releaseOnce.Do(func() {
			close(release)
		})
	}
	defer releaseLoads()
	load := func(_ context.Context, _, ref string) (*PackageSnapshot, error) {
		started <- ref
		<-release
		return &PackageSnapshot{ModuleHash: ref}, nil
	}

	type result struct {
		old *PackageSnapshot
		new *PackageSnapshot
		err error
	}
	done := make(chan result, 1)
	go func() {
		oldSnapshot, newSnapshot, err := loadSnapshotPair(
			context.Background(),
			"repo",
			"old",
			"new",
			load,
		)
		done <- result{old: oldSnapshot, new: newSnapshot, err: err}
	}()

	refs := make(map[string]struct{}, 2)
	for range 2 {
		select {
		case ref := <-started:
			refs[ref] = struct{}{}
		case <-time.After(time.Second):
			t.Fatal("snapshots did not start concurrently")
		}
	}
	releaseLoads()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("loadSnapshotPair() error = %v", got.err)
		}
		if got.old.ModuleHash != "old" || got.new.ModuleHash != "new" {
			t.Fatalf(
				"loadSnapshotPair() hashes = (%q, %q), want (old, new)",
				got.old.ModuleHash,
				got.new.ModuleHash,
			)
		}
	case <-time.After(time.Second):
		t.Fatal("loadSnapshotPair() did not finish")
	}
	if _, ok := refs["old"]; !ok {
		t.Fatal("old snapshot did not start")
	}
	if _, ok := refs["new"]; !ok {
		t.Fatal("new snapshot did not start")
	}
}

func TestTransitiveDependentsDeduplicatesConvergingChanges(t *testing.T) {
	changed := map[string]struct{}{
		"change-a": {},
		"change-b": {},
	}
	reverse := map[string]map[string]struct{}{
		"change-a": {"shared-c": {}},
		"change-b": {"shared-c": {}},
		"shared-c": {"consumer-d": {}},
	}

	got := transitiveDependents(changed, reverse)
	want := []string{"change-a", "change-b", "consumer-d", "shared-c"}
	if len(got) != len(want) {
		t.Fatalf("transitiveDependents() = %v, want %v", got, want)
	}
	for _, id := range want {
		if _, ok := got[id]; !ok {
			t.Fatalf("transitiveDependents() missing %q: %v", id, got)
		}
	}
}

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

func TestAnalyzeDoesNotMixCallbacksPassedToSharedFunction(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "runner/runner.go", `package runner

func Run(callback func()) { callback() }
`)
	writeModuleFile(t, repo, "first/first.go", `package first

import "example.com/app/runner"

func callback() { println("old") }
func Run() { runner.Run(callback) }
`)
	writeModuleFile(t, repo, "second/second.go", `package second

import "example.com/app/runner"

func callback() {}
func Run() { runner.Run(callback) }
`)
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "first/first.go", `package first

import "example.com/app/runner"

func callback() { println("new") }
func Run() { runner.Run(callback) }
`)
	newCommit := commitModule(t, repo, "new")

	analyzer := NewAnalyzer(&snapshot.Cache{Dir: t.TempDir()})
	got, err := analyzer.Analyze(context.Background(), repo, oldCommit, newCommit)
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	assertPackages(t, got, []string{
		"first.first",
	})
}

func TestAnalyzePropagatesConcreteMethodThroughInterfaceArgument(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "runner/runner.go", `package runner

type Service interface {
	Run()
}

func Run(service Service) {
	service.Run()
}
`)
	writeModuleFile(t, repo, "service/service.go", `package service

type Service struct{}

func (Service) Run() { println("old") }
`)
	writeModuleFile(t, repo, "cmd/server/main.go", `package main

import (
	"example.com/app/runner"
	"example.com/app/service"
)

func main() {
	runner.Run(service.Service{})
}
`)
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "service/service.go", `package service

type Service struct{}

func (Service) Run() { println("new") }
`)
	newCommit := commitModule(t, repo, "new")

	analyzer := NewAnalyzer(&snapshot.Cache{Dir: t.TempDir()})
	got, err := analyzer.Analyze(context.Background(), repo, oldCommit, newCommit)
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	assertPackages(t, got, []string{
		"cmd/server.main",
		"runner.runner",
		"service.service",
	})
}

func TestAnalyzeDoesNotMixConcreteTypesPassedToSameInterface(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "runner/runner.go", `package runner

type Service interface {
	Run()
}

func Run(service Service) {
	service.Run()
}
`)
	writeModuleFile(t, repo, "first/first.go", `package first

import "example.com/app/runner"

type Service struct{}

func (Service) Run() { println("old") }
func Start() { runner.Run(Service{}) }
`)
	writeModuleFile(t, repo, "second/second.go", `package second

import "example.com/app/runner"

type Service struct{}

func (Service) Run() {}
func Start() { runner.Run(Service{}) }
`)
	writeModuleFile(t, repo, "cmd/first/main.go", `package main

import "example.com/app/first"

func main() { first.Start() }
`)
	writeModuleFile(t, repo, "cmd/second/main.go", `package main

import "example.com/app/second"

func main() { second.Start() }
`)
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "first/first.go", `package first

import "example.com/app/runner"

type Service struct{}

func (Service) Run() { println("new") }
func Start() { runner.Run(Service{}) }
`)
	newCommit := commitModule(t, repo, "new")

	analyzer := NewAnalyzer(&snapshot.Cache{Dir: t.TempDir()})
	got, err := analyzer.Analyze(context.Background(), repo, oldCommit, newCommit)
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	assertPackages(t, got, []string{
		"cmd/first.main",
		"first.first",
		"runner.runner",
	})
}

func TestAnalyzePropagatesConcreteMethodStoredInInterfaceField(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "service/service.go", `package service

type Service struct{}

func (*Service) Run() { println("old") }
`)
	writeModuleFile(t, repo, "handler/handler.go", `package handler

import "example.com/app/service"

type Service interface {
	Run()
}

type Handler struct {
	service Service
}

func New(service *service.Service) *Handler {
	return &Handler{service: service}
}

func (h *Handler) Run() {
	h.service.Run()
}
`)
	writeModuleFile(t, repo, "cmd/server/main.go", `package main

import (
	"example.com/app/handler"
	"example.com/app/service"
)

func main() {
	handler.New(&service.Service{}).Run()
}
`)
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "service/service.go", `package service

type Service struct{}

func (*Service) Run() { println("new") }
`)
	newCommit := commitModule(t, repo, "new")

	analyzer := NewAnalyzer(&snapshot.Cache{Dir: t.TempDir()})
	got, err := analyzer.Analyze(context.Background(), repo, oldCommit, newCommit)
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	assertPackages(t, got, []string{
		"cmd/server.main",
		"handler.handler",
		"service.service",
	})
}

func TestAnalyzePropagatesConcreteMethodThroughForwardedInterfaces(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "worker/worker.go", `package worker

type Job interface {
	Run()
}

type Worker interface {
	Execute(Job)
}

type DefaultWorker struct{}

func (DefaultWorker) Execute(job Job) {
	job.Run()
}
`)
	writeModuleFile(t, repo, "service/service.go", `package service

type Job struct{}

func (Job) Run() { println("old") }
`)
	writeModuleFile(t, repo, "orchestrator/orchestrator.go", `package orchestrator

import "example.com/app/worker"

func Start(job worker.Job, executor worker.Worker) {
	executor.Execute(job)
}
`)
	writeModuleFile(t, repo, "cmd/server/main.go", `package main

import (
	"example.com/app/orchestrator"
	"example.com/app/service"
	"example.com/app/worker"
)

func main() {
	orchestrator.Start(service.Job{}, worker.DefaultWorker{})
}
`)
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "service/service.go", `package service

type Job struct{}

func (Job) Run() { println("new") }
`)
	newCommit := commitModule(t, repo, "new")

	analyzer := NewAnalyzer(&snapshot.Cache{Dir: t.TempDir()})
	got, err := analyzer.Analyze(context.Background(), repo, oldCommit, newCommit)
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	assertPackages(t, got, []string{
		"cmd/server.main",
		"orchestrator.orchestrator",
		"service.service",
		"worker.worker",
	})
}

func TestAnalyzePropagatesPackageInitializationChange(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "startup/startup.go", `package startup

func setup() {}

func init() { setup() }
`)
	writeModuleFile(t, repo, "cmd/server/main.go", `package main

import _ "example.com/app/startup"

func main() {}
`)
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "startup/startup.go", `package startup

func setup() { println("changed") }

func init() { setup() }
`)
	newCommit := commitModule(t, repo, "new")

	analyzer := NewAnalyzer(&snapshot.Cache{Dir: t.TempDir()})
	got, err := analyzer.Analyze(context.Background(), repo, oldCommit, newCommit)
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	assertPackages(t, got, []string{
		"cmd/server.main",
		"startup.startup",
	})
}

func TestAnalyzePropagatesEmbeddedFileChange(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "resource/data.txt", "old")
	writeModuleFile(t, repo, "resource/resource.go", `package resource

import _ "embed"

//go:embed data.txt
var data string

func Value() string { return data }
`)
	writeModuleFile(t, repo, "cmd/server/main.go", `package main

import "example.com/app/resource"

func main() { println(resource.Value()) }
`)
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "resource/data.txt", "new")
	newCommit := commitModule(t, repo, "new")

	analyzer := NewAnalyzer(&snapshot.Cache{Dir: t.TempDir()})
	got, err := analyzer.Analyze(context.Background(), repo, oldCommit, newCommit)
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	assertPackages(t, got, []string{
		"cmd/server.main",
		"resource.resource",
	})
}

func TestAnalyzeDoesNotMixIndependentEmbeddedFiles(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "resource/a.txt", "old")
	writeModuleFile(t, repo, "resource/b.txt", "stable")
	writeModuleFile(t, repo, "resource/resource.go", `package resource

import _ "embed"

//go:embed a.txt
var a string

//go:embed b.txt
var b string

func A() string { return a }
func B() string { return b }
`)
	writeModuleFile(t, repo, "cmd/a/main.go", `package main

import "example.com/app/resource"

func main() { println(resource.A()) }
`)
	writeModuleFile(t, repo, "cmd/b/main.go", `package main

import "example.com/app/resource"

func main() { println(resource.B()) }
`)
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "resource/a.txt", "new")
	newCommit := commitModule(t, repo, "new")

	analyzer := NewAnalyzer(&snapshot.Cache{Dir: t.TempDir()})
	got, err := analyzer.Analyze(context.Background(), repo, oldCommit, newCommit)
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	assertPackages(t, got, []string{
		"cmd/a.main",
		"resource.resource",
	})
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
	if !reflect.DeepEqual(first.Symbols, second.Symbols) {
		t.Fatalf("cached symbols differ:\nfirst=%v\nsecond=%v", first.Symbols, second.Symbols)
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
