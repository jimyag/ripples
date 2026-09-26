package impact

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jimyag/ripples/internal/snapshot"
)

// Building a snapshot holds a whole module in memory, so the two revisions
// must not be built at the same time.
func TestLoadSnapshotPairLoadsOneSnapshotAtATime(t *testing.T) {
	resolve := func(_ context.Context, repoPath, ref string) (*snapshot.Revision, error) {
		return &snapshot.Revision{RepoPath: repoPath, Commit: ref, Tree: ref}, nil
	}
	newStarted := make(chan struct{})
	overlapped := false
	load := func(_ context.Context, revision *snapshot.Revision) (*PackageSnapshot, error) {
		switch revision.Commit {
		case "new":
			close(newStarted)
		case "old":
			select {
			case <-newStarted:
				overlapped = true
			case <-time.After(100 * time.Millisecond):
			}
		}
		return &PackageSnapshot{Tree: revision.Commit}, nil
	}

	oldSnapshot, newSnapshot, err := loadSnapshotPair(context.Background(), "repo", "old", "new", resolve, load)
	if err != nil {
		t.Fatalf("loadSnapshotPair() error = %v", err)
	}
	if overlapped {
		t.Fatal("new snapshot started while the old one was loading")
	}
	if oldSnapshot.Tree != "old" || newSnapshot.Tree != "new" {
		t.Fatalf("loadSnapshotPair() trees = (%q, %q), want (old, new)", oldSnapshot.Tree, newSnapshot.Tree)
	}
}

func TestLoadSnapshotPairReusesSameTree(t *testing.T) {
	resolve := func(_ context.Context, repoPath, ref string) (*snapshot.Revision, error) {
		return &snapshot.Revision{
			RepoPath: repoPath,
			Commit:   ref,
			Tree:     "shared-tree",
		}, nil
	}
	loads := 0
	load := func(_ context.Context, revision *snapshot.Revision) (*PackageSnapshot, error) {
		loads++
		return &PackageSnapshot{Tree: revision.Tree}, nil
	}

	oldSnapshot, newSnapshot, err := loadSnapshotPair(
		context.Background(),
		"repo",
		"old-alias",
		"new-alias",
		resolve,
		load,
	)
	if err != nil {
		t.Fatalf("loadSnapshotPair() error = %v", err)
	}
	if loads != 1 {
		t.Fatalf("snapshot loads = %d, want 1", loads)
	}
	if oldSnapshot != newSnapshot {
		t.Fatal("same tree returned different snapshots")
	}
}

func TestAnalysisCacheKeyIncludesRepositorySubdirectory(t *testing.T) {
	first := analysisCacheKey("package-graph", &snapshot.Revision{
		Tree:   "shared-tree",
		Subdir: filepath.Join("src", "first"),
	}, "config")
	second := analysisCacheKey("package-graph", &snapshot.Revision{
		Tree:   "shared-tree",
		Subdir: filepath.Join("src", "second"),
	}, "config")

	if first == second {
		t.Fatal("analysisCacheKey() reused a cache key for different repository subdirectories")
	}
}

func TestBuildConfigurationReadsGoEnvFile(t *testing.T) {
	// An empty variable lets the go command fall back to the GOENV file.
	t.Setenv("GOFLAGS", "")
	t.Setenv("GOENV", filepath.Join(t.TempDir(), "missing.env"))
	defaultConfig, err := buildConfiguration(t.Context())
	if err != nil {
		t.Fatalf("buildConfiguration() error = %v", err)
	}

	goEnv := filepath.Join(t.TempDir(), "go.env")
	if err := os.WriteFile(goEnv, []byte("GOFLAGS=-tags=integration\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOENV", goEnv)
	taggedConfig, err := buildConfiguration(t.Context())
	if err != nil {
		t.Fatalf("buildConfiguration() error = %v", err)
	}

	if defaultConfig == taggedConfig {
		t.Fatalf("buildConfiguration() ignored GOFLAGS from the go env file: %s", taggedConfig)
	}
}

func TestAnalyzeHandlesRecursiveFunctionValue(t *testing.T) {
	const helperEnv = "RIPPLES_RECURSIVE_FUNCTION_VALUE_HELPER"
	if os.Getenv(helperEnv) != "1" {
		command := exec.Command(os.Args[0], "-test.run=^TestAnalyzeHandlesRecursiveFunctionValue$")
		command.Env = append(os.Environ(), helperEnv+"=1", "GOTRACEBACK=none")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("recursive function value analysis failed: %v\n%s", err, output)
		}
		return
	}

	repo := initModule(t)
	writeModuleFile(t, repo, "go.mod", "module example.com/app\n\ngo 1.25\n")
	writeModuleFile(t, repo, "runner/runner.go", `package runner

type Service interface {
	Run()
}
`)
	writeModuleFile(t, repo, "service/service.go", `package service

type Service struct{}

func (Service) Run() {}
`)
	writeModuleFile(t, repo, "factory/factory.go", `package factory

import (
	"example.com/app/runner"
	"example.com/app/service"
)

var wrap func(runner.Service, bool) runner.Service

func init() {
	wrap = func(current runner.Service, again bool) runner.Service {
		if again {
			return wrap(current, false)
		}
		return current
	}
}

func New() runner.Service {
	return Select(func() runner.Service {
		return wrap(service.Service{}, true)
	}, true)()
}

func Select(factory func() runner.Service, again bool) func() runner.Service {
	if again {
		return Select(func() runner.Service {
			return factory()
		}, false)
	}
	return factory
}
`)
	writeModuleFile(t, repo, "cmd/server/main.go", `package main

import "example.com/app/factory"

func main() {
	factory.New().Run()
}
`)
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "service/service.go", `package service

type Service struct{}

func (Service) Run() { println("changed") }
`)
	newCommit := commitModule(t, repo, "new")

	analyzer := NewAnalyzer(&snapshot.Cache{Dir: t.TempDir()})
	got, err := analyzer.Analyze(context.Background(), repo, oldCommit, newCommit)
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	assertPackages(t, got, []string{"cmd/server.main", "service.service"})
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

	got, _ := transitiveDependents(changed, reverse, nil, nil)
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

func TestPackageImpactGraphCollapsesDeclarationAndDispatchEdges(t *testing.T) {
	const (
		paymentPath = "example.com/app/payment"
		servicePath = "example.com/app/service"
		serverPath  = "example.com/app/cmd/server"
	)
	paymentMethod := paymentPath + "::func::Service.Run"
	dispatch := paymentPath + "::interface-trace::0"
	serviceFunction := servicePath + "::func::Execute"
	serverMain := serverPath + "::func::main"
	packageSnapshot := &PackageSnapshot{
		Symbols: map[string]Symbol{
			paymentMethod: {
				ID:          paymentMethod,
				PackagePath: paymentPath,
			},
			dispatch: {
				ID:           dispatch,
				PackagePath:  paymentPath,
				Dependencies: []string{paymentMethod},
			},
			serviceFunction: {
				ID:           serviceFunction,
				PackagePath:  servicePath,
				Dependencies: []string{dispatch},
			},
			serverMain: {
				ID:           serverMain,
				PackagePath:  serverPath,
				Dependencies: []string{serviceFunction},
			},
		},
	}
	changed := map[string]struct{}{paymentMethod: {}}
	reverse := reverseDependencies(packageSnapshot)
	affected, _ := transitiveDependents(changed, reverse, nil, nil)

	changedPackages, edges := packageImpactGraph(
		changed,
		affected,
		reverse,
		packageSnapshot,
	)
	if want := []string{paymentPath}; !reflect.DeepEqual(changedPackages, want) {
		t.Fatalf("changed packages = %v, want %v", changedPackages, want)
	}
	wantEdges := []PackageEdge{
		{From: paymentPath, To: servicePath},
		{From: servicePath, To: serverPath},
	}
	if !reflect.DeepEqual(edges, wantEdges) {
		t.Fatalf("edges = %v, want %v", edges, wantEdges)
	}
}

func TestPackageImpactGraphIncludesDeletedDependencyEdges(t *testing.T) {
	const (
		paymentPath = "example.com/app/payment"
		servicePath = "example.com/app/service"
	)
	paymentFunction := paymentPath + "::func::Removed"
	serviceFunction := servicePath + "::func::Execute"
	oldSnapshot := &PackageSnapshot{
		Symbols: map[string]Symbol{
			paymentFunction: {
				ID:          paymentFunction,
				PackagePath: paymentPath,
			},
			serviceFunction: {
				ID:           serviceFunction,
				PackagePath:  servicePath,
				Dependencies: []string{paymentFunction},
			},
		},
	}
	newSnapshot := &PackageSnapshot{
		Symbols: map[string]Symbol{
			serviceFunction: {
				ID:          serviceFunction,
				PackagePath: servicePath,
			},
		},
	}
	changed := map[string]struct{}{paymentFunction: {}}
	reverse := reverseDependencies(oldSnapshot, newSnapshot)
	affected, _ := transitiveDependents(changed, reverse, nil, nil)

	changedPackages, edges := packageImpactGraph(
		changed,
		affected,
		reverse,
		oldSnapshot,
		newSnapshot,
	)
	if want := []string{paymentPath}; !reflect.DeepEqual(changedPackages, want) {
		t.Fatalf("changed packages = %v, want %v", changedPackages, want)
	}
	wantEdges := []PackageEdge{{From: paymentPath, To: servicePath}}
	if !reflect.DeepEqual(edges, wantEdges) {
		t.Fatalf("edges = %v, want %v", edges, wantEdges)
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

func TestAnalyzeGo126ModuleUsesChangedService(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "go.mod", "module example.com/app\n\ngo 1.26\n")
	writeModuleFile(t, repo, "service/service.go", `package service

import "cmp"

func Name() string { return cmp.Or("", "old") }
`)
	writeModuleFile(t, repo, "cmd/server/main.go", `package main

import "example.com/app/service"

func main() { _ = service.Name() }
`)
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "service/service.go", `package service

import "cmp"

func Name() string { return cmp.Or("", "new") }
`)
	newCommit := commitModule(t, repo, "new")

	analyzer := NewAnalyzer(&snapshot.Cache{Dir: t.TempDir()})
	got, err := analyzer.Analyze(t.Context(), repo, oldCommit, newCommit)
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	assertPackages(t, got, []string{"cmd/server.main", "service.service"})
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
	// runner only calls through the interface; main performs the conversion.
	assertPackages(t, got, []string{
		"cmd/server.main",
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
	})
}

func TestAnalyzeDoesNotMixFactoriesPassedToSameCallback(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "runner/runner.go", `package runner

type Service interface {
	Run()
}

func Use(factory func() Service) {
	factory().Run()
}
`)
	writeModuleFile(t, repo, "first/first.go", `package first

import (
	"example.com/app/runner"
)

type Service struct{}

func (Service) Run() { println("old") }
func New() runner.Service { return Service{} }
func Start() { runner.Use(New) }
`)
	writeModuleFile(t, repo, "second/second.go", `package second

import (
	"example.com/app/runner"
)

type Service struct{}

func (Service) Run() {}
func New() runner.Service { return Service{} }
func Start() { runner.Use(New) }
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

import (
	"example.com/app/runner"
)

type Service struct{}

func (Service) Run() { println("new") }
func New() runner.Service { return Service{} }
func Start() { runner.Use(New) }
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
	// orchestrator and worker only forward and call the interface value.
	assertPackages(t, got, []string{
		"cmd/server.main",
		"service.service",
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

func TestAnalyzeKeepsPackageValueDeclarationsIndependent(t *testing.T) {
	tests := []struct {
		name string
		old  string
		new  string
	}{
		{
			name: "variables",
			old:  "var A, B = 1, 2\n",
			new:  "var A, B = 1, 20\n",
		},
		{
			name: "constants",
			old:  "const A, B = 1, 2\n",
			new:  "const A, B = 1, 20\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo := initModule(t)
			writeModuleFile(t, repo, "shared/shared.go", "package shared\n\n"+test.old)
			writeModuleFile(t, repo, "cmd/a/main.go", `package main

import "example.com/app/shared"

func main() { println(shared.A) }
`)
			writeModuleFile(t, repo, "cmd/b/main.go", `package main

import "example.com/app/shared"

func main() { println(shared.B) }
`)
			oldCommit := commitModule(t, repo, "old")
			writeModuleFile(t, repo, "shared/shared.go", "package shared\n\n"+test.new)
			newCommit := commitModule(t, repo, "new")

			analyzer := NewAnalyzer(&snapshot.Cache{Dir: t.TempDir()})
			got, err := analyzer.Analyze(context.Background(), repo, oldCommit, newCommit)
			if err != nil {
				t.Fatalf("Analyze() error = %v", err)
			}
			assertPackages(t, got, []string{
				"cmd/b.main",
				"shared.shared",
			})
		})
	}
}

func TestAnalyzeDoesNotPropagateCallsInsideStoredFunctionLiteralInitializer(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "shared/shared.go", `package shared

func work() {}

func Unchanged() {}
`)
	writeModuleFile(t, repo, "consumer/consumer.go", `package consumer

import "example.com/app/shared"

func Use() { shared.Unchanged() }
`)
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "shared/shared.go", `package shared

var handlers = []func(){
	func() { work() },
}

func work() {}

func Unchanged() {}
`)
	newCommit := commitModule(t, repo, "new")

	analyzer := NewAnalyzer(&snapshot.Cache{Dir: t.TempDir()})
	got, err := analyzer.Analyze(context.Background(), repo, oldCommit, newCommit)
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	assertPackages(t, got, []string{
		"shared.shared",
	})
}

func TestAnalyzeTracksEffectfulBlankInitializers(t *testing.T) {
	tests := []struct {
		name  string
		suite string
		want  []string
	}{
		{
			name: "external registry callback",
			suite: `package suite

import (
	"example.com/app/service"
	"example.com/dependency"
)

var _ = dependency.Describe("suite", func() {
	dependency.It("case", func() {
		service.Changed()
	})
})
`,
			want: []string{
				"service.service",
				"suite.suite",
			},
		},
		{
			name: "immediately invoked function literal",
			suite: `package suite

import "example.com/app/service"

var _ = func() bool {
	service.Changed()
	return true
}()
`,
			want: []string{
				"service.service",
				"suite.suite",
			},
		},
		{
			name: "stored function literal",
			suite: `package suite

import "example.com/app/service"

var _ = []func(){
	func() { service.Changed() },
}
`,
			want: []string{
				"service.service",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo := initModule(t)
			writeModuleFile(t, repo, "go.mod", `module example.com/app

go 1.25

require example.com/dependency v0.0.0

replace example.com/dependency => ./dependency
`)
			writeModuleFile(t, repo, "dependency/go.mod", `module example.com/dependency

go 1.25
`)
			writeModuleFile(t, repo, "dependency/dependency.go", `package dependency

func Describe(_ string, _ func()) bool { return true }
func It(_ string, _ func()) bool       { return true }
`)
			writeModuleFile(t, repo, "service/service.go", `package service

func Changed() { println("old") }
`)
			writeModuleFile(t, repo, "suite/suite.go", test.suite)
			oldCommit := commitModule(t, repo, "old")
			writeModuleFile(t, repo, "service/service.go", `package service

func Changed() { println("new") }
`)
			newCommit := commitModule(t, repo, "new")

			analyzer := NewAnalyzer(&snapshot.Cache{Dir: t.TempDir()})
			got, err := analyzer.Analyze(context.Background(), repo, oldCommit, newCommit)
			if err != nil {
				t.Fatalf("Analyze() error = %v", err)
			}
			assertPackages(t, got, test.want)
		})
	}
}

func TestAnalyzePropagatesPackageInitializationForms(t *testing.T) {
	tests := []struct {
		name string
		old  string
		new  string
		want []string
	}{
		{
			name: "direct init body",
			old: `package startup

func init() {}
`,
			new: `package startup

func init() { println("changed") }
`,
			want: []string{"cmd/server.main", "startup.startup"},
		},
		{
			name: "added init",
			old:  "package startup\n",
			new: `package startup

func init() { println("added") }
`,
			want: []string{"cmd/server.main", "startup.startup"},
		},
		{
			name: "runtime-effectful named package variable",
			old: `package startup

func setup() string { return "old" }

var State = setup()
`,
			new: `package startup

func setup() string { panic("changed initializer") }

var State = setup()
`,
			want: []string{"cmd/server.main", "startup.startup"},
		},
		{
			name: "unused pure package variable",
			old: `package startup

var State = "old"
`,
			new: `package startup

var State = "new"
`,
			want: []string{"startup.startup"},
		},
		{
			name: "immediately invoked function literal",
			old: `package startup

var State = func() string { return "old" }()
`,
			new: `package startup

var State = func() string { return "new" }()
`,
			want: []string{"cmd/server.main", "startup.startup"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo := initModule(t)
			writeModuleFile(t, repo, "startup/startup.go", test.old)
			writeModuleFile(t, repo, "cmd/server/main.go", `package main

import _ "example.com/app/startup"

func main() {}
`)
			oldCommit := commitModule(t, repo, "old")
			writeModuleFile(t, repo, "startup/startup.go", test.new)
			newCommit := commitModule(t, repo, "new")

			analyzer := NewAnalyzer(&snapshot.Cache{Dir: t.TempDir()})
			got, err := analyzer.Analyze(context.Background(), repo, oldCommit, newCommit)
			if err != nil {
				t.Fatalf("Analyze() error = %v", err)
			}
			assertPackages(t, got, test.want)
		})
	}
}

func TestAnalyzeIgnoresCompileTimeBlankInitializer(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "startup/startup.go", `package startup

type API interface {
	Run()
}

type Implementation struct{}

func (Implementation) Run() {}
`)
	writeModuleFile(t, repo, "cmd/server/main.go", `package main

import _ "example.com/app/startup"

func main() {}
`)
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "startup/startup.go", `package startup

type API interface {
	Run()
}

type Implementation struct{}

func (Implementation) Run() {}

var _ API = (*Implementation)(nil)
`)
	newCommit := commitModule(t, repo, "new")

	analyzer := NewAnalyzer(&snapshot.Cache{Dir: t.TempDir()})
	got, err := analyzer.Analyze(context.Background(), repo, oldCommit, newCommit)
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	assertPackages(t, got, []string{"startup.startup"})
}

func TestAnalyzePropagatesPotentiallyPanickingConversions(t *testing.T) {
	tests := []struct {
		name       string
		oldConvert string
		newConvert string
	}{
		{
			name:       "slice to array",
			oldConvert: "[1]byte(Data)",
			newConvert: "[2]byte(Data)",
		},
		{
			name:       "slice to array pointer",
			oldConvert: "(*[1]byte)(Data)",
			newConvert: "(*[2]byte)(Data)",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo := initModule(t)
			writeModuleFile(t, repo, "startup/startup.go", "package startup\n\nvar Data = []byte{1}\nvar _ = "+test.oldConvert+"\n")
			writeModuleFile(t, repo, "cmd/server/main.go", `package main

import _ "example.com/app/startup"

func main() {}
`)
			oldCommit := commitModule(t, repo, "old")
			writeModuleFile(t, repo, "startup/startup.go", "package startup\n\nvar Data = []byte{1}\nvar _ = "+test.newConvert+"\n")
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
		})
	}
}

func TestAnalyzePropagatesCallInsideConversion(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "startup/startup.go", `package startup

type Value int

func build() int { return 1 }

var _ = Value(build())
`)
	writeModuleFile(t, repo, "cmd/server/main.go", `package main

import _ "example.com/app/startup"

func main() {}
`)
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "startup/startup.go", `package startup

type Value int

func build() int { panic("changed initializer") }

var _ = Value(build())
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

func TestAnalyzePropagatesReferencedPackageVariable(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "startup/startup.go", `package startup

func setup() string { return "old" }

var State = setup()
`)
	writeModuleFile(t, repo, "cmd/server/main.go", `package main

import "example.com/app/startup"

func main() { println(startup.State) }
`)
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "startup/startup.go", `package startup

func setup() string { return "new" }

var State = setup()
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
	if got[0].Deleted || !got[1].Deleted {
		t.Fatalf("Deleted flags = (%v, %v), want (false, true)", got[0].Deleted, got[1].Deleted)
	}
}

func TestAnalyzeReportsModuleRootPackageAsDot(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "app.go", "package app\n\nfunc Version() int { return 1 }\n")
	writeModuleFile(t, repo, "app/app.go", "package app\n\nfunc Name() string { return \"app\" }\n")
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "app.go", "package app\n\nfunc Version() int { return 2 }\n")
	writeModuleFile(t, repo, "app/app.go", "package app\n\nfunc Name() string { return \"new\" }\n")
	newCommit := commitModule(t, repo, "new")

	assertAnalyzedPackages(t, repo, oldCommit, newCommit, []string{"..app", "app.app"})
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

func TestAnalyzePropagatesCompilerDirectiveChange(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "payment/payment.go", `package payment

func Pay() string { return "value" }
`)
	writeModuleFile(t, repo, "cmd/server/main.go", `package main

import "example.com/app/payment"

func main() { _ = payment.Pay() }
`)
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "payment/payment.go", `package payment

//go:noinline
func Pay() string { return "value" }
`)
	newCommit := commitModule(t, repo, "new")

	analyzer := NewAnalyzer(&snapshot.Cache{Dir: t.TempDir()})
	got, err := analyzer.Analyze(context.Background(), repo, oldCommit, newCommit)
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	assertPackages(t, got, []string{
		"cmd/server.main",
		"payment.payment",
	})
}

func TestAnalyzeDoesNotPropagateUnusedCompilerDirectiveChange(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "payment/payment.go", `package payment

func Used() string { return "used" }
func Unused() string { return "unused" }
`)
	writeModuleFile(t, repo, "cmd/server/main.go", `package main

import "example.com/app/payment"

func main() { _ = payment.Used() }
`)
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "payment/payment.go", `package payment

func Used() string { return "used" }

//go:noinline
func Unused() string { return "unused" }
`)
	newCommit := commitModule(t, repo, "new")

	analyzer := NewAnalyzer(&snapshot.Cache{Dir: t.TempDir()})
	got, err := analyzer.Analyze(context.Background(), repo, oldCommit, newCommit)
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	assertPackages(t, got, []string{"payment.payment"})
}

func TestAnalyzePropagatesCgoPreambleChange(t *testing.T) {
	goEnv := exec.Command("go", "env", "CGO_ENABLED")
	output, err := goEnv.Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(output)) != "1" {
		t.Skip("cgo is disabled")
	}

	repo := initModule(t)
	writeModuleFile(t, repo, "bridge/bridge.go", `package bridge

/*
static int value() { return 1; }
*/
import "C"

func Value() int { return int(C.value()) }
`)
	writeModuleFile(t, repo, "cmd/server/main.go", `package main

import "example.com/app/bridge"

func main() { _ = bridge.Value() }
`)
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "bridge/bridge.go", `package bridge

/*
static int value() { return 2; }
*/
import "C"

func Value() int { return int(C.value()) }
`)
	newCommit := commitModule(t, repo, "new")

	analyzer := NewAnalyzer(&snapshot.Cache{Dir: t.TempDir()})
	got, err := analyzer.Analyze(context.Background(), repo, oldCommit, newCommit)
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	assertPackages(t, got, []string{
		"bridge.bridge",
		"cmd/server.main",
	})
}

func TestAnalyzePropagatesAssemblyImplementationChange(t *testing.T) {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip("assembly fixture covers amd64 and arm64")
	}
	assembly := map[string]func(op string) string{
		"mathx/add_amd64.s": func(op string) string {
			return "#include \"textflag.h\"\n\nTEXT ·Add(SB), NOSPLIT, $0-24\n" +
				"\tMOVQ a+0(FP), AX\n\tMOVQ b+8(FP), BX\n\t" + op + "Q BX, AX\n\tMOVQ AX, ret+16(FP)\n\tRET\n"
		},
		"mathx/add_arm64.s": func(op string) string {
			return "#include \"textflag.h\"\n\nTEXT ·Add(SB), NOSPLIT, $0-24\n" +
				"\tMOVD a+0(FP), R0\n\tMOVD b+8(FP), R1\n\t" + op + " R1, R0, R0\n\tMOVD R0, ret+16(FP)\n\tRET\n"
		},
	}
	repo := initModule(t)
	writeModuleFile(t, repo, "mathx/add.go", "package mathx\n\n// Add is implemented in assembly.\nfunc Add(a, b int64) int64\n")
	writeModuleFile(t, repo, "billing/billing.go", `package billing

import "example.com/app/mathx"

func Total(a, b int64) int64 { return mathx.Add(a, b) }
`)
	for name, source := range assembly {
		writeModuleFile(t, repo, name, source("ADD"))
	}
	oldCommit := commitModule(t, repo, "old")
	for name, source := range assembly {
		writeModuleFile(t, repo, name, source("SUB"))
	}
	newCommit := commitModule(t, repo, "new")

	assertAnalyzedPackages(t, repo, oldCommit, newCommit, []string{"billing.billing", "mathx.mathx"})
}

func TestAnalyzePropagatesCgoSourceChange(t *testing.T) {
	output, err := exec.Command("go", "env", "CGO_ENABLED").Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(output)) != "1" {
		t.Skip("cgo is disabled")
	}

	repo := initModule(t)
	writeModuleFile(t, repo, "cmath/cmath.go", `package cmath

// #include "twice.h"
import "C"

func Double(v int) int { return int(C.twice(C.int(v))) }
`)
	writeModuleFile(t, repo, "cmath/twice.h", "int twice(int v);\n")
	writeModuleFile(t, repo, "cmath/twice.c", "#include \"twice.h\"\nint twice(int v) { return v * 2; }\n")
	writeModuleFile(t, repo, "billing/billing.go", `package billing

import "example.com/app/cmath"

func Fee(v int) int { return cmath.Double(v) }
`)
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "cmath/twice.c", "#include \"twice.h\"\nint twice(int v) { return v * 3; }\n")
	newCommit := commitModule(t, repo, "new")

	assertAnalyzedPackages(t, repo, oldCommit, newCommit, []string{"billing.billing", "cmath.cmath"})
}

func TestAnalyzeIgnoresNonSemanticGoModChange(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "payment/payment.go", `package payment

func Pay() string { return "value" }
`)
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "go.mod", `module example.com/app

// This comment does not affect the build.
go 1.25
`)
	newCommit := commitModule(t, repo, "new")

	analyzer := NewAnalyzer(&snapshot.Cache{Dir: t.TempDir()})
	got, err := analyzer.Analyze(context.Background(), repo, oldCommit, newCommit)
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	assertPackages(t, got, []string{})
}

func TestAnalyzePropagatesUsedModuleReplacementChange(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "dependency-one/go.mod", "module example.com/dependency\n\ngo 1.25\n")
	writeModuleFile(t, repo, "dependency-one/dependency.go", `package dependency

func Value() string { return "one" }
`)
	writeModuleFile(t, repo, "dependency-two/go.mod", "module example.com/dependency\n\ngo 1.25\n")
	writeModuleFile(t, repo, "dependency-two/dependency.go", `package dependency

func Value() string { return "two" }
`)
	writeModuleFile(t, repo, "go.mod", `module example.com/app

go 1.25

require example.com/dependency v0.0.0

replace example.com/dependency => ./dependency-one
`)
	writeModuleFile(t, repo, "service/service.go", `package service

import "example.com/dependency"

func Value() string { return dependency.Value() }
`)
	writeModuleFile(t, repo, "cmd/server/main.go", `package main

import "example.com/app/service"

func main() { _ = service.Value() }
`)
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "go.mod", `module example.com/app

go 1.25

require example.com/dependency v0.0.0

replace example.com/dependency => ./dependency-two
`)
	newCommit := commitModule(t, repo, "new")

	analyzer := NewAnalyzer(&snapshot.Cache{Dir: t.TempDir()})
	got, err := analyzer.Analyze(context.Background(), repo, oldCommit, newCommit)
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	assertPackages(t, got, []string{
		"cmd/server.main",
		"service.service",
	})
}

func TestAnalyzeIgnoresUnusedModuleReplacementChange(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "dependency-one/go.mod", "module example.com/dependency\n\ngo 1.25\n")
	writeModuleFile(t, repo, "dependency-one/dependency.go", "package dependency\n")
	writeModuleFile(t, repo, "dependency-two/go.mod", "module example.com/dependency\n\ngo 1.25\n")
	writeModuleFile(t, repo, "dependency-two/dependency.go", "package dependency\n")
	writeModuleFile(t, repo, "go.mod", `module example.com/app

go 1.25

require example.com/dependency v0.0.0

replace example.com/dependency => ./dependency-one
`)
	writeModuleFile(t, repo, "service/service.go", "package service\n")
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "go.mod", `module example.com/app

go 1.25

require example.com/dependency v0.0.0

replace example.com/dependency => ./dependency-two
`)
	newCommit := commitModule(t, repo, "new")

	analyzer := NewAnalyzer(&snapshot.Cache{Dir: t.TempDir()})
	got, err := analyzer.Analyze(context.Background(), repo, oldCommit, newCommit)
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	assertPackages(t, got, []string{})
}

func TestAnalyzePropagatesGoWorkBuildConfigurationChange(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "go.mod", "module example.com/app\n\ngo 1.24\n")
	writeModuleFile(t, repo, "go.work", "go 1.24\n\nuse .\n")
	writeModuleFile(t, repo, "service/service.go", "package service\n")
	writeModuleFile(t, repo, "cmd/server/main.go", `package main

import _ "example.com/app/service"

func main() {}
`)
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "go.work", "go 1.25\n\nuse .\n")
	newCommit := commitModule(t, repo, "new")

	analyzer := NewAnalyzer(&snapshot.Cache{Dir: t.TempDir()})
	got, err := analyzer.Analyze(context.Background(), repo, oldCommit, newCommit)
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	assertPackages(t, got, []string{
		"cmd/server.main",
		"service.service",
	})
}

func TestAnalyzeIncludesTestFiles(t *testing.T) {
	baseFiles := map[string]string{
		"order/order.go":     "package order\n\nfunc Total(a, b int) int { return a + b }\n",
		"pricing/pricing.go": "package pricing\n\nfunc Discount(v int) int { return v }\n",
		"order/order_test.go": `package order

import (
	"testing"

	"example.com/app/pricing"
)

func TestTotal(t *testing.T) {
	if Total(1, 2) != pricing.Discount(3) {
		t.Fatal("bad")
	}
}
`,
		"order/external_test.go": `package order_test

import (
	"testing"

	"example.com/app/order"
)

func TestExternal(t *testing.T) { _ = order.Total(1, 1) }
`,
		"e2e/e2e_test.go": `package e2e

import (
	"testing"

	"example.com/app/order"
)

func TestFlow(t *testing.T) { _ = order.Total(2, 2) }
`,
		"lib/lib.go":      "package lib\n\nfunc Value() int { return 1 }\n",
		"lib/lib_test.go": "package lib\n\nfunc init() { println(\"old\") }\n",
		"cmd/server/main.go": `package main

import "example.com/app/lib"

func main() { _ = lib.Value() }
`,
	}
	tests := []struct {
		name    string
		changes map[string]string
		want    []string
	}{
		{
			name:    "test-only change",
			changes: map[string]string{"order/order_test.go": strings.Replace(baseFiles["order/order_test.go"], "Discount(3)", "Discount(4)", 1)},
			want:    []string{"order.order"},
		},
		{
			name:    "declaration used only by tests",
			changes: map[string]string{"pricing/pricing.go": "package pricing\n\nfunc Discount(v int) int { return v * 2 }\n"},
			want:    []string{"order.order", "pricing.pricing"},
		},
		{
			name:    "external test and test-only package",
			changes: map[string]string{"order/order.go": "package order\n\nfunc Total(a, b int) int { return a - b }\n"},
			want:    []string{"e2e.e2e", "order.order"},
		},
		{
			name:    "test init does not affect importers",
			changes: map[string]string{"lib/lib_test.go": "package lib\n\nfunc init() { println(\"new\") }\n"},
			want:    []string{"lib.lib"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo := initModule(t)
			for name, content := range baseFiles {
				writeModuleFile(t, repo, name, content)
			}
			oldCommit := commitModule(t, repo, "old")
			for name, content := range test.changes {
				writeModuleFile(t, repo, name, content)
			}
			newCommit := commitModule(t, repo, "new")

			analyzer := NewAnalyzer(&snapshot.Cache{Dir: t.TempDir()})
			analyzer.Tests = true
			got, err := analyzer.Analyze(t.Context(), repo, oldCommit, newCommit)
			if err != nil {
				t.Fatalf("Analyze() error = %v", err)
			}
			assertPackages(t, got, test.want)
		})
	}
}

func TestAnalyzeIgnoresTestFilesByDefault(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "order/order.go", "package order\n\nfunc Total(a, b int) int { return a + b }\n")
	writeModuleFile(t, repo, "order/order_test.go", "package order\n\nfunc helper() int { return Total(1, 2) }\n")
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "order/order_test.go", "package order\n\nfunc helper() int { return Total(2, 3) }\n")
	newCommit := commitModule(t, repo, "new")

	assertAnalyzedPackages(t, repo, oldCommit, newCommit, []string{})
}

func TestAnalyzeKeepsTestAndNonTestSnapshotsApart(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "order/order.go", "package order\n")
	commitModule(t, repo, "initial")

	cache := &snapshot.Cache{Dir: t.TempDir()}
	withoutTests := NewAnalyzer(cache)
	if _, err := withoutTests.LoadSnapshot(t.Context(), repo, "HEAD"); err != nil {
		t.Fatal(err)
	}
	withTests := NewAnalyzer(cache)
	withTests.Tests = true
	loaded, err := withTests.LoadSnapshot(t.Context(), repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Cached {
		t.Fatal("snapshot analyzed with tests reused the snapshot built without tests")
	}
}

func TestAnalyzePropagatesParentGoWorkReplacement(t *testing.T) {
	repo := initModule(t)
	if err := os.Remove(filepath.Join(repo, "go.mod")); err != nil {
		t.Fatal(err)
	}
	writeModuleFile(t, repo, "go.work", "go 1.25\n\nuse ./svc\n")
	writeModuleFile(t, repo, "svc/go.mod", `module example.com/svc

go 1.25

require example.com/dep v0.0.0

replace example.com/dep => ../dep1
`)
	for _, version := range []string{"1", "2"} {
		writeModuleFile(t, repo, "dep"+version+"/go.mod", "module example.com/dep\n\ngo 1.25\n")
		writeModuleFile(t, repo, "dep"+version+"/dep.go", "package dep\n\nfunc V() int { return "+version+" }\n")
	}
	writeModuleFile(t, repo, "svc/app/app.go", `package app

import "example.com/dep"

func Run() int { return dep.V() }
`)
	writeModuleFile(t, repo, "svc/other/other.go", "package other\n\nfunc X() int { return 1 }\n")
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "go.work", "go 1.25\n\nuse ./svc\n\nreplace example.com/dep => ./dep2\n")
	newCommit := commitModule(t, repo, "new")

	assertAnalyzedPackages(t, filepath.Join(repo, "svc"), oldCommit, newCommit, []string{"app.app"})
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
	// Empty slices and maps come back as nil, so compare the stored form.
	firstModules, _ := json.Marshal(first.Modules)
	secondModules, _ := json.Marshal(second.Modules)
	if string(firstModules) != string(secondModules) {
		t.Fatalf("cached modules differ:\nfirst=%s\nsecond=%s", firstModules, secondModules)
	}
}

func TestSnapshotCacheSharesUnchangedPackagesAcrossTrees(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "a/a.go", "package a\n\nfunc A() int { return 1 }\n")
	writeModuleFile(t, repo, "b/b.go", "package b\n\nfunc B() int { return 1 }\n")
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "b/b.go", "package b\n\nfunc B() int { return 2 }\n")
	newCommit := commitModule(t, repo, "new")

	cache := &snapshot.Cache{Dir: t.TempDir()}
	analyzer := NewAnalyzer(cache)
	chunks := func() []os.DirEntry {
		entries, err := os.ReadDir(filepath.Join(cache.Dir, "snapshot-chunks"))
		if err != nil {
			t.Fatal(err)
		}
		return entries
	}
	if _, err := analyzer.LoadSnapshot(context.Background(), repo, oldCommit); err != nil {
		t.Fatal(err)
	}
	stored := len(chunks())
	if _, err := analyzer.LoadSnapshot(context.Background(), repo, newCommit); err != nil {
		t.Fatal(err)
	}
	if got := len(chunks()); got != stored+1 {
		t.Fatalf("chunks after second tree = %d, want %d: only package b changed", got, stored+1)
	}

	// A pruned chunk turns the tree's entry into a miss instead of a partial
	// snapshot.
	for _, entry := range chunks() {
		if err := os.Remove(filepath.Join(cache.Dir, "snapshot-chunks", entry.Name())); err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := analyzer.LoadSnapshot(context.Background(), repo, newCommit)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Cached || len(loaded.Packages) != 2 {
		t.Fatalf("LoadSnapshot() after pruning chunks = cached %v with %d packages, want a rebuilt snapshot", loaded.Cached, len(loaded.Packages))
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
