package impact

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"

	"github.com/jimyag/ripples/internal/snapshot"
)

const analysisVersion = "symbol-impact-v12"

// Analyzer computes declaration-level impact between two Git revisions.
type Analyzer struct {
	cache *snapshot.Cache
}

// NewAnalyzer creates an analyzer backed by the persistent cache.
func NewAnalyzer(cache *snapshot.Cache) *Analyzer {
	return &Analyzer{cache: cache}
}

// Analyze returns changed packages and all packages that directly or
// transitively import them in either snapshot.
func (a *Analyzer) Analyze(ctx context.Context, repoPath, oldRef, newRef string) ([]Package, error) {
	oldSnapshot, err := a.LoadSnapshot(ctx, repoPath, oldRef)
	if err != nil {
		return nil, fmt.Errorf("load old snapshot: %w", err)
	}
	newSnapshot, err := a.LoadSnapshot(ctx, repoPath, newRef)
	if err != nil {
		return nil, fmt.Errorf("load new snapshot: %w", err)
	}

	changed := changedSymbols(oldSnapshot, newSnapshot)
	reverse := reverseDependencies(oldSnapshot, newSnapshot)
	affectedSymbols := transitiveDependents(changed, reverse)

	affectedPackages := make(map[string]struct{})
	for id := range affectedSymbols {
		symbol, ok := newSnapshot.Symbols[id]
		if !ok {
			symbol = oldSnapshot.Symbols[id]
		}
		affectedPackages[symbol.PackagePath] = struct{}{}
	}

	results := make([]Package, 0, len(affectedPackages))
	for path := range affectedPackages {
		pkg, ok := newSnapshot.Packages[path]
		if !ok {
			pkg = oldSnapshot.Packages[path]
		}
		results = append(results, pkg)
	}
	sort.Slice(results, func(i, j int) bool {
		if results[i].RelativePath != results[j].RelativePath {
			return results[i].RelativePath < results[j].RelativePath
		}
		if results[i].Name != results[j].Name {
			return results[i].Name < results[j].Name
		}
		return results[i].Path < results[j].Path
	})
	return results, nil
}

// LoadSnapshot loads a package summary from cache or builds it from an
// immutable Git archive.
func (a *Analyzer) LoadSnapshot(ctx context.Context, repoPath, ref string) (*PackageSnapshot, error) {
	revision, err := snapshot.Resolve(ctx, repoPath, ref)
	if err != nil {
		return nil, err
	}

	key := snapshot.Key(
		analysisVersion,
		revision.Tree,
		runtime.Version(),
		os.Getenv("GOOS"),
		os.Getenv("GOARCH"),
		os.Getenv("CGO_ENABLED"),
		os.Getenv("GOFLAGS"),
		os.Getenv("GOEXPERIMENT"),
	)
	var result PackageSnapshot
	if a.cache != nil {
		hit, err := a.cache.Load("package-snapshots", key, &result)
		if err == nil && hit {
			result.Cached = true
			return &result, nil
		}
	}

	source, err := snapshot.OpenRevision(ctx, revision)
	if err != nil {
		return nil, err
	}
	defer source.Close()

	result, err = buildPackageSnapshot(ctx, source)
	if err != nil {
		return nil, err
	}
	if a.cache != nil {
		if err := a.cache.Store("package-snapshots", key, result); err != nil {
			return nil, err
		}
	}
	return &result, nil
}

func changedSymbols(oldSnapshot, newSnapshot *PackageSnapshot) map[string]struct{} {
	changed := make(map[string]struct{})
	all := make(map[string]struct{}, len(oldSnapshot.Symbols)+len(newSnapshot.Symbols))
	for id := range oldSnapshot.Symbols {
		all[id] = struct{}{}
	}
	for id := range newSnapshot.Symbols {
		all[id] = struct{}{}
	}

	moduleChanged := oldSnapshot.ModuleHash != newSnapshot.ModuleHash
	for id := range all {
		if isDispatchSymbol(id) {
			continue
		}
		oldSymbol, oldOK := oldSnapshot.Symbols[id]
		newSymbol, newOK := newSnapshot.Symbols[id]
		if moduleChanged || !oldOK || !newOK || oldSymbol.Hash != newSymbol.Hash {
			changed[id] = struct{}{}
		}
	}
	return changed
}

func isDispatchSymbol(id string) bool {
	return strings.Contains(id, "::interface-trace::") ||
		strings.Contains(id, "::interface-selection::") ||
		strings.Contains(id, "::interface-field::") ||
		strings.Contains(id, "::dependency-interface::")
}

func reverseDependencies(snapshots ...*PackageSnapshot) map[string]map[string]struct{} {
	reverse := make(map[string]map[string]struct{})
	for _, packageSnapshot := range snapshots {
		for _, symbol := range packageSnapshot.Symbols {
			for _, dependency := range symbol.Dependencies {
				if _, local := packageSnapshot.Symbols[dependency]; !local {
					continue
				}
				if reverse[dependency] == nil {
					reverse[dependency] = make(map[string]struct{})
				}
				reverse[dependency][symbol.ID] = struct{}{}
			}
		}
	}
	return reverse
}

func transitiveDependents(changed map[string]struct{}, reverse map[string]map[string]struct{}) map[string]struct{} {
	affected := make(map[string]struct{}, len(changed))
	queue := make([]string, 0, len(changed))
	for path := range changed {
		affected[path] = struct{}{}
		queue = append(queue, path)
	}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for importer := range reverse[current] {
			if _, seen := affected[importer]; seen {
				continue
			}
			affected[importer] = struct{}{}
			queue = append(queue, importer)
		}
	}
	return affected
}
