package impact

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	gopackages "golang.org/x/tools/go/packages"
)

func TestPackageBuildMetadataHashIncludesCgoPreamble(t *testing.T) {
	oldPackage := parseBuildMetadataPackage(t, `package bridge

/*
static int value() { return 1; }
*/
import "C"
`)
	newPackage := parseBuildMetadataPackage(t, `package bridge

/*
static int value() { return 2; }
*/
import "C"
`)

	oldHash := packageBuildMetadataHash(oldPackage)
	newHash := packageBuildMetadataHash(newPackage)
	if oldHash == "" || newHash == "" {
		t.Fatalf("packageBuildMetadataHash() returned an empty hash: old=%q new=%q", oldHash, newHash)
	}
	if oldHash == newHash {
		t.Fatalf("packageBuildMetadataHash() = %q for different CGo preambles", oldHash)
	}
}

func TestPackageBuildMetadataHashIgnoresDocumentation(t *testing.T) {
	oldPackage := parseBuildMetadataPackage(t, `package service

// Value returns a value.
func Value() {}
`)
	newPackage := parseBuildMetadataPackage(t, `package service

// Value returns the current value.
func Value() {}
`)

	oldHash := packageBuildMetadataHash(oldPackage)
	newHash := packageBuildMetadataHash(newPackage)
	if oldHash != newHash {
		t.Fatalf("packageBuildMetadataHash() changed for documentation: old=%q new=%q", oldHash, newHash)
	}
}

func parseBuildMetadataPackage(t *testing.T, source string) *gopackages.Package {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "source.go", source, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	return &gopackages.Package{Syntax: []*ast.File{file}}
}
