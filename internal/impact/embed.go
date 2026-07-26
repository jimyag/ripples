package impact

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	gopackages "golang.org/x/tools/go/packages"
)

func addEmbedDependencies(
	root string,
	loaded []*gopackages.Package,
	packages map[string]Package,
	objectIDs map[types.Object]string,
	symbols map[string]Symbol,
) error {
	for _, pkg := range loaded {
		if _, local := packages[pkg.PkgPath]; !local || len(pkg.EmbedFiles) == 0 {
			continue
		}
		for fileIndex, file := range pkg.Syntax {
			packageDir := filepath.Dir(pkg.CompiledGoFiles[fileIndex])
			for _, rawDeclaration := range file.Decls {
				declaration, ok := rawDeclaration.(*ast.GenDecl)
				if !ok || declaration.Tok != token.VAR {
					continue
				}
				for _, rawSpec := range declaration.Specs {
					spec := rawSpec.(*ast.ValueSpec)
					patterns := goEmbedPatterns(spec.Doc)
					if len(patterns) == 0 {
						patterns = goEmbedPatterns(declaration.Doc)
					}
					if len(patterns) == 0 {
						continue
					}
					dependencies, err := matchingEmbedInputs(root, packageDir, pkg, patterns, symbols)
					if err != nil {
						return err
					}
					for _, name := range spec.Names {
						id, exists := objectIDs[pkg.TypesInfo.Defs[name]]
						if !exists {
							continue
						}
						symbol := symbols[id]
						symbol.Dependencies = mergeDependencies(symbol.Dependencies, dependencies)
						symbols[id] = symbol
					}
				}
			}
		}
	}
	return nil
}

func matchingEmbedInputs(
	root string,
	packageDir string,
	pkg *gopackages.Package,
	patterns []string,
	symbols map[string]Symbol,
) ([]string, error) {
	var dependencies []string
	for _, filename := range pkg.EmbedFiles {
		relative, err := filepath.Rel(packageDir, filename)
		if err != nil {
			continue
		}
		relative = filepath.ToSlash(relative)
		if !matchesAnyEmbedPattern(patterns, relative) {
			continue
		}
		hash, err := contentHash(filename)
		if err != nil {
			return nil, fmt.Errorf("hash embedded input %s: %w", filename, err)
		}
		repositoryRelative, err := filepath.Rel(root, filename)
		if err != nil {
			repositoryRelative = filename
		}
		id := packageObjectID(pkg.PkgPath, "embed-file", filepath.ToSlash(repositoryRelative))
		symbols[id] = Symbol{
			ID:          id,
			PackagePath: pkg.PkgPath,
			Hash:        hash,
		}
		dependencies = append(dependencies, id)
	}
	sort.Strings(dependencies)
	return dependencies, nil
}

func goEmbedPatterns(group *ast.CommentGroup) []string {
	if group == nil {
		return nil
	}
	var patterns []string
	for _, comment := range group.List {
		text := strings.TrimSpace(strings.TrimPrefix(comment.Text, "//"))
		if !strings.HasPrefix(text, "go:embed") {
			continue
		}
		patterns = append(patterns, splitEmbedPatterns(strings.TrimSpace(strings.TrimPrefix(text, "go:embed")))...)
	}
	return patterns
}

func splitEmbedPatterns(value string) []string {
	var patterns []string
	for value = strings.TrimSpace(value); value != ""; value = strings.TrimSpace(value) {
		if value[0] != '"' && value[0] != '`' {
			end := strings.IndexAny(value, " \t")
			if end < 0 {
				return append(patterns, value)
			}
			patterns = append(patterns, value[:end])
			value = value[end:]
			continue
		}
		quote := value[0]
		end := 1
		for end < len(value) {
			if value[end] == quote && (quote == '`' || value[end-1] != '\\') {
				end++
				break
			}
			end++
		}
		if unquoted, err := strconv.Unquote(value[:end]); err == nil {
			patterns = append(patterns, unquoted)
		}
		value = value[end:]
	}
	return patterns
}

func matchesAnyEmbedPattern(patterns []string, filename string) bool {
	for _, pattern := range patterns {
		pattern = strings.TrimPrefix(pattern, "all:")
		for candidate := filename; candidate != "."; candidate = path.Dir(candidate) {
			if matched, _ := path.Match(pattern, candidate); matched {
				return true
			}
		}
	}
	return false
}

func mergeDependencies(existing, added []string) []string {
	merged := make(map[string]struct{}, len(existing)+len(added))
	for _, dependency := range existing {
		merged[dependency] = struct{}{}
	}
	for _, dependency := range added {
		merged[dependency] = struct{}{}
	}
	return sortedSet(merged)
}
