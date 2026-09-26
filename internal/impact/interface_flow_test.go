package impact

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jimyag/ripples/internal/snapshot"
)

func TestAnalyzePropagatesCommonInterfaceValueFlows(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{
			name: "factory return",
			files: map[string]string{
				"factory/factory.go": `package factory

import (
	"example.com/app/runner"
	"example.com/app/service"
)

func New() runner.Service {
	return service.Service{}
}
`,
				"cmd/server/main.go": `package main

import "example.com/app/factory"

func main() {
	factory.New().Run()
}
`,
			},
			want: []string{"cmd/server.main", "factory.factory", "service.service"},
		},
		{
			name: "factory returns package variable",
			files: map[string]string{
				"factory/factory.go": `package factory

import (
	"example.com/app/runner"
	"example.com/app/service"
)

var current runner.Service = service.Service{}

func New() runner.Service {
	return current
}
`,
				"cmd/server/main.go": `package main

import "example.com/app/factory"

func main() {
	factory.New().Run()
}
`,
			},
			want: []string{"cmd/server.main", "factory.factory", "service.service"},
		},
		{
			name: "factory returns concrete local",
			files: map[string]string{
				"factory/factory.go": `package factory

import (
	"example.com/app/runner"
	"example.com/app/service"
)

func New() runner.Service {
	current := service.Service{}
	return current
}
`,
				"cmd/server/main.go": `package main

import "example.com/app/factory"

func main() {
	factory.New().Run()
}
`,
			},
			want: []string{"cmd/server.main", "factory.factory", "service.service"},
		},
		{
			name: "forwarded return",
			files: map[string]string{
				"factory/factory.go": `package factory

import "example.com/app/runner"

func Forward(service runner.Service) runner.Service {
	return service
}
`,
				"cmd/server/main.go": `package main

import (
	"example.com/app/factory"
	"example.com/app/service"
)

func main() {
	factory.Forward(service.Service{}).Run()
}
`,
			},
			want: []string{"cmd/server.main", "service.service"},
		},
		{
			name: "closure capture",
			files: map[string]string{
				"cmd/server/main.go": `package main

import (
	"example.com/app/runner"
	"example.com/app/service"
)

func main() {
	var current runner.Service = service.Service{}
	run := func() {
		current.Run()
	}
	run()
}
`,
			},
			want: []string{"cmd/server.main", "service.service"},
		},
		{
			name: "interface assertion",
			files: map[string]string{
				"cmd/server/main.go": `package main

import (
	"example.com/app/runner"
	"example.com/app/service"
)

func main() {
	var current any = service.Service{}
	current.(runner.Service).Run()
}
`,
			},
			want: []string{"cmd/server.main", "service.service"},
		},
		{
			name: "slice range",
			files: map[string]string{
				"cmd/server/main.go": `package main

import (
	"example.com/app/runner"
	"example.com/app/service"
)

func main() {
	services := []runner.Service{service.Service{}}
	for _, current := range services {
		current.Run()
	}
}
`,
			},
			want: []string{"cmd/server.main", "service.service"},
		},
		{
			name: "map index",
			files: map[string]string{
				"cmd/server/main.go": `package main

import (
	"example.com/app/runner"
	"example.com/app/service"
)

func main() {
	services := map[string]runner.Service{"primary": service.Service{}}
	services["primary"].Run()
}
`,
			},
			want: []string{"cmd/server.main", "service.service"},
		},
		{
			name: "conditional assignment",
			files: map[string]string{
				"cmd/server/main.go": `package main

import (
	"example.com/app/runner"
	"example.com/app/service"
)

func main() {
	var current runner.Service
	if len("x") > 0 {
		current = service.Service{}
	}
	current.Run()
}
`,
			},
			want: []string{"cmd/server.main", "service.service"},
		},
		{
			name: "multiple return values",
			files: map[string]string{
				"factory/factory.go": `package factory

import (
	"example.com/app/runner"
	"example.com/app/service"
)

func New() (runner.Service, error) {
	return service.Service{}, nil
}
`,
				"cmd/server/main.go": `package main

import "example.com/app/factory"

func main() {
	current, _ := factory.New()
	current.Run()
}
`,
			},
			want: []string{"cmd/server.main", "factory.factory", "service.service"},
		},
		{
			name: "named return value",
			files: map[string]string{
				"factory/factory.go": `package factory

import (
	"example.com/app/runner"
	"example.com/app/service"
)

func New() (current runner.Service) {
	current = service.Service{}
	return
}
`,
				"cmd/server/main.go": `package main

import "example.com/app/factory"

func main() {
	factory.New().Run()
}
`,
			},
			want: []string{"cmd/server.main", "factory.factory", "service.service"},
		},
		{
			name: "generic forwarding",
			files: map[string]string{
				"factory/factory.go": `package factory

import "example.com/app/runner"

func Forward[T runner.Service](service T) runner.Service {
	return service
}
`,
				"cmd/server/main.go": `package main

import (
	"example.com/app/factory"
	"example.com/app/service"
)

func main() {
	factory.Forward(service.Service{}).Run()
}
`,
			},
			want: []string{"cmd/server.main", "service.service"},
		},
		{
			name: "method value",
			files: map[string]string{
				"cmd/server/main.go": `package main

import (
	"example.com/app/runner"
	"example.com/app/service"
)

func main() {
	var current runner.Service = service.Service{}
	run := current.Run
	run()
}
`,
			},
			want: []string{"cmd/server.main", "service.service"},
		},
		{
			name: "method expression",
			files: map[string]string{
				"cmd/server/main.go": `package main

import (
	"example.com/app/runner"
	"example.com/app/service"
)

func main() {
	runner.Service.Run(service.Service{})
}
`,
			},
			want: []string{"cmd/server.main", "service.service"},
		},
		{
			name: "append and range",
			files: map[string]string{
				"cmd/server/main.go": `package main

import (
	"example.com/app/runner"
	"example.com/app/service"
)

func main() {
	var services []runner.Service
	services = append(services, service.Service{})
	for _, current := range services {
		current.Run()
	}
}
`,
			},
			want: []string{"cmd/server.main", "service.service"},
		},
		{
			name: "map assignment",
			files: map[string]string{
				"cmd/server/main.go": `package main

import (
	"example.com/app/runner"
	"example.com/app/service"
)

func main() {
	services := make(map[string]runner.Service)
	services["primary"] = service.Service{}
	services["primary"].Run()
}
`,
			},
			want: []string{"cmd/server.main", "service.service"},
		},
		{
			name: "channel send and receive",
			files: map[string]string{
				"cmd/server/main.go": `package main

import (
	"example.com/app/runner"
	"example.com/app/service"
)

func main() {
	services := make(chan runner.Service, 1)
	services <- service.Service{}
	current := <-services
	current.Run()
}
`,
			},
			want: []string{"cmd/server.main", "service.service"},
		},
		{
			name: "function literal return",
			files: map[string]string{
				"cmd/server/main.go": `package main

import (
	"example.com/app/runner"
	"example.com/app/service"
)

func main() {
	newService := func() runner.Service {
		return service.Service{}
	}
	newService().Run()
}
`,
			},
			want: []string{"cmd/server.main", "service.service"},
		},
		{
			name: "function callback parameter",
			files: map[string]string{
				"runner/runner.go": `package runner

type Service interface {
	Run()
}

func Use(factory func() Service) {
	factory().Run()
}
`,
				"factory/factory.go": `package factory

import (
	"example.com/app/runner"
	"example.com/app/service"
)

func New() runner.Service {
	return service.Service{}
}
`,
				"cmd/server/main.go": `package main

import (
	"example.com/app/factory"
	"example.com/app/runner"
)

func main() {
	runner.Use(factory.New)
}
`,
			},
			want: []string{"cmd/server.main", "factory.factory", "service.service"},
		},
		{
			name: "function returned from factory",
			files: map[string]string{
				"factory/factory.go": `package factory

import (
	"example.com/app/runner"
	"example.com/app/service"
)

func New() runner.Service {
	return service.Service{}
}

func Select() func() runner.Service {
	return New
}
`,
				"cmd/server/main.go": `package main

import "example.com/app/factory"

func main() {
	factory.Select()().Run()
}
`,
			},
			want: []string{"cmd/server.main", "factory.factory", "service.service"},
		},
		{
			name: "function stored in struct field",
			files: map[string]string{
				"factory/factory.go": `package factory

import (
	"example.com/app/runner"
	"example.com/app/service"
)

func New() runner.Service {
	return service.Service{}
}
`,
				"holder/holder.go": `package holder

import "example.com/app/runner"

type Holder struct {
	Factory func() runner.Service
}
`,
				"cmd/server/main.go": `package main

import (
	"example.com/app/factory"
	"example.com/app/holder"
)

func main() {
	var current holder.Holder
	current.Factory = factory.New
	current.Factory().Run()
}
`,
			},
			want: []string{"cmd/server.main", "factory.factory", "service.service"},
		},
		{
			name: "function stored in slice",
			files: map[string]string{
				"factory/factory.go": `package factory

import (
	"example.com/app/runner"
	"example.com/app/service"
)

func New() runner.Service { return service.Service{} }
`,
				"cmd/server/main.go": `package main

import (
	"example.com/app/factory"
	"example.com/app/runner"
)

func main() {
	factories := []func() runner.Service{factory.New}
	factories[0]().Run()
}
`,
			},
			want: []string{"cmd/server.main", "factory.factory", "service.service"},
		},
		{
			name: "function stored in map",
			files: map[string]string{
				"factory/factory.go": `package factory

import (
	"example.com/app/runner"
	"example.com/app/service"
)

func New() runner.Service { return service.Service{} }
`,
				"cmd/server/main.go": `package main

import (
	"example.com/app/factory"
	"example.com/app/runner"
)

func main() {
	factories := make(map[string]func() runner.Service)
	factories["primary"] = factory.New
	factories["primary"]().Run()
}
`,
			},
			want: []string{"cmd/server.main", "factory.factory", "service.service"},
		},
		{
			name: "function sent through channel",
			files: map[string]string{
				"factory/factory.go": `package factory

import (
	"example.com/app/runner"
	"example.com/app/service"
)

func New() runner.Service { return service.Service{} }
`,
				"cmd/server/main.go": `package main

import (
	"example.com/app/factory"
	"example.com/app/runner"
)

func main() {
	factories := make(chan func() runner.Service, 1)
	factories <- factory.New
	(<-factories)().Run()
}
`,
			},
			want: []string{"cmd/server.main", "factory.factory", "service.service"},
		},
		{
			name: "function appended and ranged",
			files: map[string]string{
				"factory/factory.go": `package factory

import (
	"example.com/app/runner"
	"example.com/app/service"
)

func New() runner.Service { return service.Service{} }
`,
				"cmd/server/main.go": `package main

import (
	"example.com/app/factory"
	"example.com/app/runner"
)

func main() {
	var factories []func() runner.Service
	factories = append(factories, factory.New)
	for _, current := range factories {
		current().Run()
	}
}
`,
			},
			want: []string{"cmd/server.main", "factory.factory", "service.service"},
		},
		{
			name: "function appended to struct field",
			files: map[string]string{
				"factory/factory.go": `package factory

import (
	"example.com/app/runner"
	"example.com/app/service"
)

func New() runner.Service { return service.Service{} }
`,
				"holder/holder.go": `package holder

import "example.com/app/runner"

type Holder struct {
	Factories []func() runner.Service
}
`,
				"cmd/server/main.go": `package main

import (
	"example.com/app/factory"
	"example.com/app/holder"
)

func main() {
	var current holder.Holder
	current.Factories = append(current.Factories, factory.New)
	current.Factories[0]().Run()
}
`,
			},
			want: []string{"cmd/server.main", "factory.factory", "service.service"},
		},
		{
			name: "function in multiple return values",
			files: map[string]string{
				"factory/factory.go": `package factory

import (
	"example.com/app/runner"
	"example.com/app/service"
)

func New() runner.Service { return service.Service{} }

func Select() (string, func() runner.Service) {
	return "primary", New
}
`,
				"cmd/server/main.go": `package main

import "example.com/app/factory"

func main() {
	_, current := factory.Select()
	current().Run()
}
`,
			},
			want: []string{"cmd/server.main", "factory.factory", "service.service"},
		},
		{
			name: "function assigned through pointer",
			files: map[string]string{
				"factory/factory.go": `package factory

import (
	"example.com/app/runner"
	"example.com/app/service"
)

func New() runner.Service { return service.Service{} }
`,
				"cmd/server/main.go": `package main

import (
	"example.com/app/factory"
	"example.com/app/runner"
)

func main() {
	var current func() runner.Service
	target := &current
	*target = factory.New
	(*target)().Run()
}
`,
			},
			want: []string{"cmd/server.main", "factory.factory", "service.service"},
		},
		{
			name: "closure captures function parameter",
			files: map[string]string{
				"factory/factory.go": `package factory

import (
	"example.com/app/runner"
	"example.com/app/service"
)

func New() runner.Service { return service.Service{} }

func Wrap(factory func() runner.Service) func() runner.Service {
	return func() runner.Service {
		return factory()
	}
}
`,
				"cmd/server/main.go": `package main

import "example.com/app/factory"

func main() {
	factory.Wrap(factory.New)().Run()
}
`,
			},
			want: []string{"cmd/server.main", "factory.factory", "service.service"},
		},
		{
			name: "constructor stores function field",
			files: map[string]string{
				"factory/factory.go": `package factory

import (
	"example.com/app/runner"
	"example.com/app/service"
)

func New() runner.Service { return service.Service{} }
`,
				"holder/holder.go": `package holder

import "example.com/app/runner"

type Holder struct {
	Factory func() runner.Service
}

func New(factory func() runner.Service) Holder {
	return Holder{Factory: factory}
}
`,
				"cmd/server/main.go": `package main

import (
	"example.com/app/factory"
	"example.com/app/holder"
)

func main() {
	holder.New(factory.New).Factory().Run()
}
`,
			},
			want: []string{"cmd/server.main", "factory.factory", "service.service"},
		},
		{
			name: "function returned inside slice",
			files: map[string]string{
				"factory/factory.go": `package factory

import (
	"example.com/app/runner"
	"example.com/app/service"
)

func New() runner.Service { return service.Service{} }

func All() []func() runner.Service {
	return []func() runner.Service{New}
}
`,
				"cmd/server/main.go": `package main

import "example.com/app/factory"

func main() {
	factory.All()[0]().Run()
}
`,
			},
			want: []string{"cmd/server.main", "factory.factory", "service.service"},
		},
		{
			name: "function returned inside map",
			files: map[string]string{
				"factory/factory.go": `package factory

import (
	"example.com/app/runner"
	"example.com/app/service"
)

func New() runner.Service { return service.Service{} }

func All() map[string]func() runner.Service {
	return map[string]func() runner.Service{"primary": New}
}
`,
				"cmd/server/main.go": `package main

import "example.com/app/factory"

func main() {
	factory.All()["primary"]().Run()
}
`,
			},
			want: []string{"cmd/server.main", "factory.factory", "service.service"},
		},
		{
			name: "function returned through channel",
			files: map[string]string{
				"factory/factory.go": `package factory

import (
	"example.com/app/runner"
	"example.com/app/service"
)

func New() runner.Service { return service.Service{} }

func All() <-chan func() runner.Service {
	result := make(chan func() runner.Service, 1)
	result <- New
	return result
}
`,
				"cmd/server/main.go": `package main

import "example.com/app/factory"

func main() {
	(<-factory.All())().Run()
}
`,
			},
			want: []string{"cmd/server.main", "factory.factory", "service.service"},
		},
		{
			name: "function type conversion",
			files: map[string]string{
				"factory/factory.go": `package factory

import (
	"example.com/app/runner"
	"example.com/app/service"
)

type Factory func() runner.Service

func New() runner.Service { return service.Service{} }
`,
				"cmd/server/main.go": `package main

import "example.com/app/factory"

func main() {
	current := factory.Factory(factory.New)
	current().Run()
}
`,
			},
			want: []string{"cmd/server.main", "factory.factory", "service.service"},
		},
		{
			name: "bound receiver method value",
			files: map[string]string{
				"factory/factory.go": `package factory

import (
	"example.com/app/runner"
	"example.com/app/service"
)

type Factory struct{}

func (Factory) New() runner.Service { return service.Service{} }
`,
				"cmd/server/main.go": `package main

import "example.com/app/factory"

func main() {
	current := factory.Factory{}.New
	current().Run()
}
`,
			},
			want: []string{"cmd/server.main", "factory.factory", "service.service"},
		},
		{
			name: "stored method expression",
			files: map[string]string{
				"factory/factory.go": `package factory

import "example.com/app/runner"

type Factory struct{}

func (Factory) Forward(current runner.Service) runner.Service {
	return current
}
`,
				"cmd/server/main.go": `package main

import (
	"example.com/app/factory"
	"example.com/app/service"
)

func main() {
	current := factory.Factory.Forward
	current(factory.Factory{}, service.Service{}).Run()
}
`,
			},
			want: []string{"cmd/server.main", "service.service"},
		},
		{
			name: "function called with go",
			files: map[string]string{
				"factory/factory.go": `package factory

import (
	"example.com/app/runner"
	"example.com/app/service"
)

func New() runner.Service { return service.Service{} }
`,
				"cmd/server/main.go": `package main

import "example.com/app/factory"

func main() {
	go factory.New().Run()
}
`,
			},
			want: []string{"cmd/server.main", "factory.factory", "service.service"},
		},
		{
			name: "function called with defer",
			files: map[string]string{
				"factory/factory.go": `package factory

import (
	"example.com/app/runner"
	"example.com/app/service"
)

func New() runner.Service { return service.Service{} }
`,
				"cmd/server/main.go": `package main

import "example.com/app/factory"

func main() {
	defer factory.New().Run()
}
`,
			},
			want: []string{"cmd/server.main", "factory.factory", "service.service"},
		},
		{
			name: "function received in select",
			files: map[string]string{
				"factory/factory.go": `package factory

import (
	"example.com/app/runner"
	"example.com/app/service"
)

func New() runner.Service { return service.Service{} }
`,
				"cmd/server/main.go": `package main

import (
	"example.com/app/factory"
	"example.com/app/runner"
)

func main() {
	factories := make(chan func() runner.Service, 1)
	factories <- factory.New
	select {
	case current := <-factories:
		current().Run()
	default:
	}
}
`,
			},
			want: []string{"cmd/server.main", "factory.factory", "service.service"},
		},
		{
			name: "variadic function callbacks",
			files: map[string]string{
				"runner/runner.go": `package runner

type Service interface {
	Run()
}

func Use(factories ...func() Service) {
	for _, current := range factories {
		current().Run()
	}
}
`,
				"factory/factory.go": `package factory

import (
	"example.com/app/runner"
	"example.com/app/service"
)

func New() runner.Service { return service.Service{} }
`,
				"cmd/server/main.go": `package main

import (
	"example.com/app/factory"
	"example.com/app/runner"
)

func main() {
	runner.Use(factory.New)
}
`,
			},
			want: []string{"cmd/server.main", "factory.factory", "service.service"},
		},
		{
			name: "generic function callback",
			files: map[string]string{
				"factory/factory.go": `package factory

import (
	"example.com/app/runner"
	"example.com/app/service"
)

func New() runner.Service { return service.Service{} }

func Identity[T any](current T) T {
	return current
}
`,
				"cmd/server/main.go": `package main

import (
	"example.com/app/factory"
	"example.com/app/runner"
)

func main() {
	current := factory.Identity[func() runner.Service](factory.New)
	current().Run()
}
`,
			},
			want: []string{"cmd/server.main", "factory.factory", "service.service"},
		},
		{
			name: "type switch interface case",
			files: map[string]string{
				"cmd/server/main.go": `package main

import (
	"example.com/app/runner"
	"example.com/app/service"
)

func main() {
	var value any = service.Service{}
	switch current := value.(type) {
	case runner.Service:
		current.Run()
	}
}
`,
			},
			want: []string{"cmd/server.main", "service.service"},
		},
		{
			name: "interface extracted from struct field",
			files: map[string]string{
				"holder/holder.go": `package holder

import "example.com/app/runner"

type Holder struct {
	Current runner.Service
}

func New(current runner.Service) Holder {
	return Holder{Current: current}
}
`,
				"cmd/server/main.go": `package main

import (
	"example.com/app/holder"
	"example.com/app/service"
)

func main() {
	current := holder.New(service.Service{}).Current
	current.Run()
}
`,
			},
			want: []string{"cmd/server.main", "service.service"},
		},
		{
			name: "setter injection",
			files: map[string]string{
				"server/server.go": `package server

import "example.com/app/runner"

type Server struct{ current runner.Service }

func (s *Server) Set(current runner.Service) { s.current = current }

func (s *Server) Run() { s.current.Run() }
`,
				"cmd/server/main.go": `package main

import (
	"example.com/app/server"
	"example.com/app/service"
)

func main() {
	current := &server.Server{}
	current.Set(service.Service{})
	current.Run()
}
`,
			},
			want: []string{"cmd/server.main", "service.service"},
		},
		{
			name: "functional options",
			files: map[string]string{
				"server/server.go": `package server

import "example.com/app/runner"

type Server struct{ current runner.Service }

type Option func(*Server)

func WithService(current runner.Service) Option {
	return func(s *Server) { s.current = current }
}

func New(options ...Option) *Server {
	s := &Server{}
	for _, option := range options {
		option(s)
	}
	return s
}

func (s *Server) Run() { s.current.Run() }
`,
				"cmd/server/main.go": `package main

import (
	"example.com/app/server"
	"example.com/app/service"
)

func main() { server.New(server.WithService(service.Service{})).Run() }
`,
			},
			want: []string{"cmd/server.main", "service.service"},
		},
		{
			name: "embedded interface field",
			files: map[string]string{
				"holder/holder.go": `package holder

import "example.com/app/runner"

type Holder struct{ runner.Service }

func New(current runner.Service) *Holder { return &Holder{Service: current} }
`,
				"cmd/server/main.go": `package main

import (
	"example.com/app/holder"
	"example.com/app/service"
)

func main() { holder.New(service.Service{}).Run() }
`,
			},
			want: []string{"cmd/server.main", "service.service"},
		},
		{
			name: "local registry filled by blank import",
			files: map[string]string{
				"registry/registry.go": `package registry

import "example.com/app/runner"

var services = map[string]runner.Service{}

func Register(name string, current runner.Service) { services[name] = current }

func Get(name string) runner.Service { return services[name] }
`,
				"plugin/plugin.go": `package plugin

import (
	"example.com/app/registry"
	"example.com/app/service"
)

func init() { registry.Register("default", service.Service{}) }
`,
				"cmd/server/main.go": `package main

import (
	_ "example.com/app/plugin"
	"example.com/app/registry"
)

func main() { registry.Get("default").Run() }
`,
			},
			want: []string{"cmd/server.main", "plugin.plugin", "service.service"},
		},
		{
			name: "local registry filled by package initializer",
			files: map[string]string{
				"registry/registry.go": `package registry

import "example.com/app/runner"

var services = map[string]runner.Service{}

func Register(name string, current runner.Service) bool {
	services[name] = current
	return true
}

func Get(name string) runner.Service { return services[name] }
`,
				"plugin/plugin.go": `package plugin

import (
	"example.com/app/registry"
	"example.com/app/service"
)

var _ = registry.Register("default", service.Service{})
`,
				"cmd/server/main.go": `package main

import (
	_ "example.com/app/plugin"
	"example.com/app/registry"
)

func main() { registry.Get("default").Run() }
`,
			},
			want: []string{"cmd/server.main", "plugin.plugin", "service.service"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
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
			for name, content := range test.files {
				writeModuleFile(t, repo, name, content)
			}
			oldCommit := commitModule(t, repo, "old")
			writeModuleFile(t, repo, "service/service.go", `package service

type Service struct{}

func (Service) Run() { println("new") }
`)
			newCommit := commitModule(t, repo, "new")

			assertAnalyzedPackages(t, repo, oldCommit, newCommit, test.want)
		})
	}
}

// A converted value may reach any method through type assertions or
// reflection, so methods the interface does not declare still count.
func TestAnalyzePropagatesUnusedMethodOfConvertedType(t *testing.T) {
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
		"cmd/server.main",
		"service.service",
	})
}

func TestAnalyzeDoesNotPropagateNewInterfaceCallToDependencies(t *testing.T) {
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

func (Service) Run() {}
`)
	writeModuleFile(t, repo, "feature/feature.go", `package feature

func Start() {}
`)
	writeModuleFile(t, repo, "cmd/server/main.go", `package main

import "example.com/app/feature"

func main() {
	feature.Start()
}
`)
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "feature/feature.go", `package feature

import (
	"example.com/app/runner"
	"example.com/app/service"
)

func Start() {
	runner.Run(service.Service{})
}
`)
	newCommit := commitModule(t, repo, "new")

	assertAnalyzedPackages(t, repo, oldCommit, newCommit, []string{
		"cmd/server.main",
		"feature.feature",
	})
}

func TestAnalyzeDoesNotJoinReplacedInterfaceCallChains(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "runnera/runner.go", `package runnera

type Service interface {
	Run()
}

func Run(service Service) {
	service.Run()
}
`)
	writeModuleFile(t, repo, "runnerb/runner.go", `package runnerb

type Service interface {
	Run()
}

func Run(service Service) {
	service.Run()
}
`)
	writeModuleFile(t, repo, "servicea/service.go", `package servicea

type Service struct{}

func (Service) Run() { println("old") }
`)
	writeModuleFile(t, repo, "serviceb/service.go", `package serviceb

type Service struct{}

func (Service) Run() {}
`)
	writeModuleFile(t, repo, "feature/feature.go", `package feature

import (
	"example.com/app/runnera"
	"example.com/app/servicea"
)

func Start() {
	runnera.Run(servicea.Service{})
}
`)
	writeModuleFile(t, repo, "cmd/server/main.go", `package main

import "example.com/app/feature"

func main() {
	feature.Start()
}
`)
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "servicea/service.go", `package servicea

type Service struct{}

func (Service) Run() { println("new") }
`)
	writeModuleFile(t, repo, "feature/feature.go", `package feature

import (
	"example.com/app/runnerb"
	"example.com/app/serviceb"
)

func Start() {
	runnerb.Run(serviceb.Service{})
}
`)
	newCommit := commitModule(t, repo, "new")

	assertAnalyzedPackages(t, repo, oldCommit, newCommit, []string{
		"cmd/server.main",
		"feature.feature",
		"servicea.servicea",
	})
}

func TestAnalyzePropagatesConcreteMethodThroughDependencyInterface(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "service/service.go", `package service

import "net/http"

type Handler struct{}

func (Handler) ServeHTTP(http.ResponseWriter, *http.Request) {
	println("old")
}
`)
	writeModuleFile(t, repo, "cmd/server/main.go", `package main

import (
	"net/http"

	"example.com/app/service"
)

func main() {
	_ = http.ListenAndServe(":0", service.Handler{})
}
`)
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "service/service.go", `package service

import "net/http"

type Handler struct{}

func (Handler) ServeHTTP(http.ResponseWriter, *http.Request) {
	println("new")
}
`)
	newCommit := commitModule(t, repo, "new")

	assertAnalyzedPackages(t, repo, oldCommit, newCommit, []string{
		"cmd/server.main",
		"service.service",
	})
}

func TestAnalyzeTreatsDependencyInterfaceAsBlackBoxContract(t *testing.T) {
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
	"example.com/app/service"
	"example.com/dependency"
)

func main() {
	dependency.Run(service.Service{})
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
		"cmd/server.main",
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

func TestAnalyzePropagatesAssignedInterfaceThroughFunction(t *testing.T) {
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
	var current runner.Service
	current = service.Service{}
	runner.Run(current)
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

func TestAnalyzeTracksNamedFunctionAssignedToFunctionVariable(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "runner/runner.go", `package runner

type Service interface {
	Run()
}
`)
	writeModuleFile(t, repo, "service/service.go", `package service

type Default struct{}

func (Default) Run() {}

type Enterprise struct{}

func (Enterprise) Run() { println("old") }
`)
	writeModuleFile(t, repo, "factory/factory.go", `package factory

import (
	"example.com/app/runner"
	"example.com/app/service"
)

var New func() runner.Service

func NewDefault() runner.Service {
	return service.Default{}
}

func NewEnterprise() runner.Service {
	return service.Enterprise{}
}
`)
	writeModuleFile(t, repo, "factory/init_default.go", `//go:build !enterprise

package factory

func init() {
	New = NewDefault
}
`)
	writeModuleFile(t, repo, "factory/init_enterprise.go", `//go:build enterprise

package factory

func init() {
	New = NewEnterprise
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

type Default struct{}

func (Default) Run() {}

type Enterprise struct{}

func (Enterprise) Run() { println("new") }
`)
	newCommit := commitModule(t, repo, "new")

	// factory.NewEnterprise converts Enterprise in every build; main only
	// reaches it when the enterprise init assigns it.
	t.Setenv("GOFLAGS", "")
	assertAnalyzedPackages(t, repo, oldCommit, newCommit, []string{
		"factory.factory",
		"service.service",
	})

	t.Setenv("GOFLAGS", "-tags=enterprise")
	assertAnalyzedPackages(t, repo, oldCommit, newCommit, []string{
		"cmd/server.main",
		"factory.factory",
		"service.service",
	})
}

// Referencing a function is a dependency whether or not the function value is
// called later; ripples does not track function values through storage.
func TestAnalyzeTreatsReferencedFunctionValuesAsDependencies(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "runner/runner.go", `package runner

type Service interface {
	Run()
}
`)
	writeModuleFile(t, repo, "service/service.go", `package service

import "example.com/app/runner"

type First struct{}

func (First) Run() {}

type Second struct{}

func (Second) Run() { println("old") }

func NewFirst() runner.Service { return First{} }
func NewSecond() runner.Service { return Second{} }
`)
	writeModuleFile(t, repo, "holder/holder.go", `package holder

import "example.com/app/runner"

type Holder struct {
	First  func() runner.Service
	Second func() runner.Service
}
`)
	writeModuleFile(t, repo, "cmd/server/main.go", `package main

import (
	"example.com/app/holder"
	"example.com/app/service"
)

func main() {
	current := holder.Holder{
		First:  service.NewFirst,
		Second: service.NewSecond,
	}
	current.First().Run()
}
`)
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "service/service.go", `package service

import "example.com/app/runner"

type First struct{}

func (First) Run() {}

type Second struct{}

func (Second) Run() { println("new") }

func NewFirst() runner.Service { return First{} }
func NewSecond() runner.Service { return Second{} }
`)
	newCommit := commitModule(t, repo, "new")

	assertAnalyzedPackages(t, repo, oldCommit, newCommit, []string{
		"cmd/server.main",
		"service.service",
	})
}

func TestAnalyzePropagatesGenericMethodThroughInstance(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "cache/cache.go", `package cache

type Cache[K comparable, V any] struct{ items map[K]V }

func New[K comparable, V any]() *Cache[K, V] { return &Cache[K, V]{items: map[K]V{}} }

func (c *Cache[K, V]) Get(key K) (V, bool) {
	value, ok := c.items[key]
	return value, ok
}
`)
	writeModuleFile(t, repo, "user/user.go", `package user

import "example.com/app/cache"

var users = cache.New[string, int]()

func Lookup(name string) (int, bool) { return users.Get(name) }
`)
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "cache/cache.go", `package cache

type Cache[K comparable, V any] struct{ items map[K]V }

func New[K comparable, V any]() *Cache[K, V] { return &Cache[K, V]{items: map[K]V{}} }

func (c *Cache[K, V]) Get(key K) (V, bool) {
	var zero V
	return zero, false
}
`)
	newCommit := commitModule(t, repo, "new")

	assertAnalyzedPackages(t, repo, oldCommit, newCommit, []string{"cache.cache", "user.user"})
}

func TestAnalyzePropagatesTypeParameterField(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "box/box.go", "package box\n\ntype Box[T any] struct{ Value T }\n")
	writeModuleFile(t, repo, "app/app.go", `package app

import "example.com/app/box"

func Read(current box.Box[int]) int { return current.Value }
`)
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "box/box.go", "package box\n\ntype Box[T any] struct {\n\tValue T `json:\"value\"`\n}\n")
	newCommit := commitModule(t, repo, "new")

	assertAnalyzedPackages(t, repo, oldCommit, newCommit, []string{"app.app", "box.box"})
}

func TestAnalyzePropagatesEmbeddedTypeBehindPromotedMethod(t *testing.T) {
	repo := initModule(t)
	shapes := func(embedded string) string {
		return `package shapes

type Base struct{}

func (Base) Name() string { return "base" }

type Fancy struct{}

func (Fancy) Name() string { return "fancy" }

type Widget struct{ ` + embedded + ` }
`
	}
	writeModuleFile(t, repo, "shapes/shapes.go", shapes("Base"))
	writeModuleFile(t, repo, "app/app.go", `package app

import "example.com/app/shapes"

func Label(current shapes.Widget) string { return current.Name() }
`)
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "shapes/shapes.go", shapes("Fancy"))
	newCommit := commitModule(t, repo, "new")

	assertAnalyzedPackages(t, repo, oldCommit, newCommit, []string{"app.app", "shapes.shapes"})
}

func TestAnalyzePropagatesFieldOrderToUnkeyedLiteral(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "geo/geo.go", "package geo\n\ntype Range struct {\n\tMin int\n\tMax int\n}\n")
	writeModuleFile(t, repo, "quota/quota.go", `package quota

import "example.com/app/geo"

func Limits() geo.Range { return geo.Range{0, 100} }
`)
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "geo/geo.go", "package geo\n\ntype Range struct {\n\tMax int\n\tMin int\n}\n")
	newCommit := commitModule(t, repo, "new")

	assertAnalyzedPackages(t, repo, oldCommit, newCommit, []string{"geo.geo", "quota.quota"})
}

func TestAnalyzePropagatesMethodsReachableThroughEmptyInterface(t *testing.T) {
	model := func(methods string) string {
		return `package model

type User struct{ ID int }

func New(id int) User { return User{ID: id} }
` + methods
	}
	tests := []struct {
		name    string
		old     string
		new     string
		useSite string
	}{
		{
			name:    "stringer",
			old:     `func (u User) String() string { return "old" }`,
			new:     `func (u User) String() string { return "new" }`,
			useSite: `func Label() string { return fmt.Sprint(model.New(1)) }`,
		},
		{
			name:    "added marshaler",
			new:     "func (u User) MarshalJSON() ([]byte, error) { return []byte(`{}`), nil }",
			useSite: `func Label() string { data, _ := json.Marshal(model.New(1)); return fmt.Sprint(data) }`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo := initModule(t)
			writeModuleFile(t, repo, "model/model.go", model(test.old))
			writeModuleFile(t, repo, "api/api.go", `package api

import (
	"encoding/json"
	"fmt"

	"example.com/app/model"
)

var _ = json.Marshal

`+test.useSite+"\n")
			oldCommit := commitModule(t, repo, "old")
			writeModuleFile(t, repo, "model/model.go", model(test.new))
			newCommit := commitModule(t, repo, "new")

			assertAnalyzedPackages(t, repo, oldCommit, newCommit, []string{"api.api", "model.model"})
		})
	}
}

func TestAnalyzeCompletesDeepInterfaceCallChains(t *testing.T) {
	const depth = 30
	source := func(body string) string {
		var text strings.Builder
		text.WriteString("package chain\n\ntype Runner interface{ Run() }\n\ntype Impl struct{}\n\n")
		fmt.Fprintf(&text, "func (Impl) Run() { %s }\n\nfunc Step0() Runner { return Impl{} }\n", body)
		for index := 1; index <= depth; index++ {
			fmt.Fprintf(&text, "\nfunc Step%d() Runner {\n\tfirst := Step%d()\n\tsecond := Step%d()\n", index, index-1, index-1)
			text.WriteString("\tif first != nil {\n\t\treturn first\n\t}\n\treturn second\n}\n")
		}
		return text.String()
	}
	repo := initModule(t)
	writeModuleFile(t, repo, "chain/chain.go", source(""))
	oldCommit := commitModule(t, repo, "old")
	writeModuleFile(t, repo, "chain/chain.go", source("println()"))
	newCommit := commitModule(t, repo, "new")

	done := make(chan struct{})
	go func() {
		defer close(done)
		assertAnalyzedPackages(t, repo, oldCommit, newCommit, []string{"chain.chain"})
	}()
	select {
	case <-done:
	case <-time.After(time.Minute):
		t.Fatal("analysis of a deep interface call chain did not finish within a minute")
	}
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
