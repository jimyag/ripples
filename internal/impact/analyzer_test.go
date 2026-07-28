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
	resolve := func(_ context.Context, repoPath, ref string) (*snapshot.Revision, error) {
		return &snapshot.Revision{RepoPath: repoPath, Commit: ref, Tree: ref}, nil
	}
	load := func(_ context.Context, revision *snapshot.Revision) (*PackageSnapshot, error) {
		started <- revision.Commit
		<-release
		return &PackageSnapshot{ModuleHash: revision.Commit}, nil
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
			resolve,
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
	})
	second := analysisCacheKey("package-graph", &snapshot.Revision{
		Tree:   "shared-tree",
		Subdir: filepath.Join("src", "second"),
	})

	if first == second {
		t.Fatal("analysisCacheKey() reused a cache key for different repository subdirectories")
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
	affected := transitiveDependents(changed, reverse)

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
	affected := transitiveDependents(changed, reverse)

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

func TestAnalyzePropagatesPackageInitializationForms(t *testing.T) {
	tests := []struct {
		name string
		old  string
		new  string
	}{
		{
			name: "direct init body",
			old: `package startup

func init() {}
`,
			new: `package startup

func init() { println("changed") }
`,
		},
		{
			name: "added init",
			old:  "package startup\n",
			new: `package startup

func init() { println("added") }
`,
		},
		{
			name: "effectful package variable",
			old: `package startup

func setup() string { return "old" }

var State = setup()
`,
			new: `package startup

func setup() string { return "new" }

var State = setup()
`,
		},
		{
			name: "immediately invoked function literal",
			old: `package startup

var State = func() string { return "old" }()
`,
			new: `package startup

var State = func() string { return "new" }()
`,
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
			assertPackages(t, got, []string{
				"cmd/server.main",
				"startup.startup",
			})
		})
	}
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
