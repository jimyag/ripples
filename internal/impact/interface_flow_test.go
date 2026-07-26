package impact

import (
	"context"
	"testing"

	"github.com/jimyag/ripples/internal/snapshot"
)

func TestAnalyzeDoesNotPropagateUnusedConcreteInterfaceMethod(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "runner/runner.go", `package runner

type Service interface {
	Used()
	Unused()
}

func Run(service Service) {
	service.Used()
}
`)
	writeModuleFile(t, repo, "service/service.go", `package service

type Service struct{}

func (Service) Used() {}
func (Service) Unused() { println("old") }
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

func (Service) Used() {}
func (Service) Unused() { println("new") }
`)
	newCommit := commitModule(t, repo, "new")

	assertAnalyzedPackages(t, repo, oldCommit, newCommit, []string{
		"service.service",
	})
}

func TestAnalyzePropagatesInterfaceVariableInitialization(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "runner/runner.go", `package runner

type Service interface {
	Run()
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

var current runner.Service = service.Service{}

func main() {
	current.Run()
}
`)
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "service/service.go", `package service

type Service struct{}

func (Service) Run() { println("new") }
`)
	newCommit := commitModule(t, repo, "new")

	assertAnalyzedPackages(t, repo, oldCommit, newCommit, []string{
		"cmd/server.main",
		"service.service",
	})
}

func TestAnalyzePropagatesInterfaceVariableAssignment(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "runner/runner.go", `package runner

type Service interface {
	Run()
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
	var current runner.Service
	current = service.Service{}
	current.Run()
}
`)
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "service/service.go", `package service

type Service struct{}

func (Service) Run() { println("new") }
`)
	newCommit := commitModule(t, repo, "new")

	assertAnalyzedPackages(t, repo, oldCommit, newCommit, []string{
		"cmd/server.main",
		"service.service",
	})
}

func TestAnalyzeDoesNotPropagateUnusedMethodStoredInInterfaceField(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "service/service.go", `package service

type Service struct{}

func (*Service) Used() {}
func (*Service) Unused() { println("old") }
`)
	writeModuleFile(t, repo, "handler/handler.go", `package handler

import "example.com/app/service"

type Service interface {
	Used()
	Unused()
}

type Handler struct {
	service Service
}

func New(service *service.Service) *Handler {
	return &Handler{service: service}
}

func (h *Handler) Used() {
	h.service.Used()
}
`)
	writeModuleFile(t, repo, "cmd/server/main.go", `package main

import (
	"example.com/app/handler"
	"example.com/app/service"
)

func main() {
	handler.New(&service.Service{}).Used()
}
`)
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "service/service.go", `package service

type Service struct{}

func (*Service) Used() {}
func (*Service) Unused() { println("new") }
`)
	newCommit := commitModule(t, repo, "new")

	assertAnalyzedPackages(t, repo, oldCommit, newCommit, []string{
		"service.service",
	})
}

func assertAnalyzedPackages(t *testing.T, repo, oldCommit, newCommit string, want []string) {
	t.Helper()
	analyzer := NewAnalyzer(&snapshot.Cache{Dir: t.TempDir()})
	got, err := analyzer.Analyze(context.Background(), repo, oldCommit, newCommit)
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	assertPackages(t, got, want)
}
