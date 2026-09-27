package impact

import (
	"context"
	"testing"
)

// Type sizes follow the architecture the go command builds for, not the one
// ripples runs on.
func TestLoadPackagesUsesTargetArchitectureSizes(t *testing.T) {
	dir := t.TempDir()
	writeModuleFile(t, dir, "go.mod", "module example.com/app\n\ngo 1.25\n")
	writeModuleFile(t, dir, "word/word.go", `package word

import "unsafe"

// The length is negative unless pointers are four bytes wide.
var _ [4 - unsafe.Sizeof(uintptr(0))]byte
`)
	t.Setenv("GOOS", "linux")
	t.Setenv("GOARCH", "amd64")
	if _, err := loadPackages(context.Background(), dir, false); err == nil {
		t.Fatal("loadPackages() for amd64 accepted an array length that is negative there")
	}
	t.Setenv("GOARCH", "386")
	if _, err := loadPackages(context.Background(), dir, false); err != nil {
		t.Fatalf("loadPackages() for 386 error = %v", err)
	}
}

// Dependencies read from export data share the objects of the packages they
// refer to: *strings.Reader implements io.WriterTo only if its WriteTo method
// takes the io.Writer that package io declares.
func TestLoadPackagesSharesDependencyTypes(t *testing.T) {
	dir := t.TempDir()
	writeModuleFile(t, dir, "go.mod", "module example.com/app\n\ngo 1.25\n")
	writeModuleFile(t, dir, "copier/copier.go", `package copier

import (
	"io"
	"strings"
)

var _ io.WriterTo = strings.NewReader("")
`)
	if _, err := loadPackages(context.Background(), dir, false); err != nil {
		t.Fatalf("loadPackages() error = %v", err)
	}
}
