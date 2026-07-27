package impact

import (
	"crypto/sha256"
	"encoding/hex"
	"go/ast"
	"go/token"
	"sort"
	"strconv"
	"strings"

	gopackages "golang.org/x/tools/go/packages"
)

func packageBuildMetadataHash(pkg *gopackages.Package) string {
	var metadata []string
	for _, file := range pkg.Syntax {
		for _, group := range file.Comments {
			for _, comment := range group.List {
				text := normalizedCommentText(comment.Text)
				if isPackageWideDirective(text) {
					metadata = append(metadata, "directive:"+text)
				}
			}
		}
		for _, rawDeclaration := range file.Decls {
			declaration, ok := rawDeclaration.(*ast.GenDecl)
			if !ok || declaration.Tok != token.IMPORT {
				continue
			}
			for _, rawSpec := range declaration.Specs {
				spec := rawSpec.(*ast.ImportSpec)
				importPath, err := strconv.Unquote(spec.Path.Value)
				if err != nil || importPath != "C" {
					continue
				}
				metadata = appendCommentGroup(metadata, "cgo-preamble:", declaration.Doc)
				metadata = appendCommentGroup(metadata, "cgo-preamble:", spec.Doc)
				metadata = appendCommentGroup(metadata, "cgo-comment:", spec.Comment)
			}
		}
	}
	if len(metadata) == 0 {
		return ""
	}
	sort.Strings(metadata)
	hash := sha256.New()
	for _, item := range metadata {
		_, _ = hash.Write([]byte(item))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func declarationBuildMetadataHash(groups ...*ast.CommentGroup) string {
	var metadata []string
	for _, group := range groups {
		if group == nil {
			continue
		}
		for _, comment := range group.List {
			text := normalizedCommentText(comment.Text)
			if isBuildAffectingDirective(text) {
				metadata = append(metadata, text)
			}
		}
	}
	if len(metadata) == 0 {
		return ""
	}
	sort.Strings(metadata)
	hash := sha256.New()
	for _, item := range metadata {
		_, _ = hash.Write([]byte(item))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func appendCommentGroup(values []string, prefix string, group *ast.CommentGroup) []string {
	if group == nil {
		return values
	}
	for _, comment := range group.List {
		values = append(values, prefix+normalizedCommentText(comment.Text))
	}
	return values
}

func normalizedCommentText(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "//")
	value = strings.TrimPrefix(value, "/*")
	value = strings.TrimSuffix(value, "*/")
	return strings.TrimSpace(value)
}

func isBuildAffectingDirective(value string) bool {
	if strings.HasPrefix(value, "go:") {
		return !strings.HasPrefix(value, "go:generate") &&
			!strings.HasPrefix(value, "go:embed") &&
			!strings.HasPrefix(value, "go:build")
	}
	return strings.HasPrefix(value, "export ")
}

func isPackageWideDirective(value string) bool {
	return strings.HasPrefix(value, "go:linkname") || strings.HasPrefix(value, "export ")
}
