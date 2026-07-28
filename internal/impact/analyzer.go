package impact

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/jimyag/ripples/internal/snapshot"
)

const analysisVersion = "symbol-impact-v19"

// Analyzer computes declaration-level impact between two Git revisions.
type Analyzer struct {
	cache *snapshot.Cache
}

// Analysis contains affected packages and the reverse package relationships
// that explain how an impact propagates from changed packages to their users.
type Analysis struct {
	Packages        []Package
	ChangedPackages []string
	Edges           []PackageEdge
}

// PackageEdge points from a dependency package to a package that uses it.
type PackageEdge struct {
	From string
	To   string
}

// NewAnalyzer creates an analyzer backed by the persistent cache.
func NewAnalyzer(cache *snapshot.Cache) *Analyzer {
	return &Analyzer{cache: cache}
}

// Analyze returns changed packages and all packages that directly or
// transitively import them in either snapshot.
func (a *Analyzer) Analyze(ctx context.Context, repoPath, oldRef, newRef string) ([]Package, error) {
	analysis, err := a.AnalyzeDetailed(ctx, repoPath, oldRef, newRef)
	if err != nil {
		return nil, err
	}
	return analysis.Packages, nil
}

// AnalyzeDetailed returns affected packages together with the package-level
// reverse dependency subgraph used to derive them.
func (a *Analyzer) AnalyzeDetailed(ctx context.Context, repoPath, oldRef, newRef string) (Analysis, error) {
	oldSnapshot, newSnapshot, err := loadSnapshotPair(
		ctx,
		repoPath,
		oldRef,
		newRef,
		snapshot.Resolve,
		a.loadResolvedSnapshot,
	)
	if err != nil {
		return Analysis{}, err
	}

	moduleChanges := make(map[string]struct{})
	if oldSnapshot.ModuleHash != newSnapshot.ModuleHash {
		oldModules, newModules, err := a.loadModuleSnapshotPair(ctx, repoPath, oldRef, newRef)
		if err != nil {
			return Analysis{}, err
		}
		moduleChanges = changedModulePackages(oldModules, newModules)
	}
	changed := changedSymbols(oldSnapshot, newSnapshot, moduleChanges)
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
	changedPackages, edges := packageImpactGraph(
		changed,
		affectedSymbols,
		reverse,
		oldSnapshot,
		newSnapshot,
	)
	return Analysis{
		Packages:        results,
		ChangedPackages: changedPackages,
		Edges:           edges,
	}, nil
}

type (
	revisionResolver func(context.Context, string, string) (*snapshot.Revision, error)
	snapshotLoader   func(context.Context, *snapshot.Revision) (*PackageSnapshot, error)
)

func loadSnapshotPair(
	ctx context.Context,
	repoPath, oldRef, newRef string,
	resolve revisionResolver,
	load snapshotLoader,
) (*PackageSnapshot, *PackageSnapshot, error) {
	revisions := make([]*snapshot.Revision, 2)
	refs := []string{oldRef, newRef}
	labels := []string{"old", "new"}
	if err := parallelFor(2, func(index int) error {
		revision, err := resolve(ctx, repoPath, refs[index])
		if err != nil {
			return fmt.Errorf("load %s snapshot: %w", labels[index], err)
		}
		revisions[index] = revision
		return nil
	}); err != nil {
		return nil, nil, err
	}

	if revisions[0].Tree == revisions[1].Tree {
		packageSnapshot, err := load(ctx, revisions[0])
		if err != nil {
			return nil, nil, fmt.Errorf("load old snapshot: %w", err)
		}
		return packageSnapshot, packageSnapshot, nil
	}

	var (
		oldSnapshot *PackageSnapshot
		newSnapshot *PackageSnapshot
		oldErr      error
		newErr      error
		wait        sync.WaitGroup
	)
	wait.Add(2)
	go func() {
		defer wait.Done()
		oldSnapshot, oldErr = load(ctx, revisions[0])
	}()
	go func() {
		defer wait.Done()
		newSnapshot, newErr = load(ctx, revisions[1])
	}()
	wait.Wait()

	if oldErr != nil {
		return nil, nil, fmt.Errorf("load old snapshot: %w", oldErr)
	}
	if newErr != nil {
		return nil, nil, fmt.Errorf("load new snapshot: %w", newErr)
	}
	return oldSnapshot, newSnapshot, nil
}

// LoadSnapshot loads a package summary from cache or builds it from an
// immutable detached Git worktree.
func (a *Analyzer) LoadSnapshot(ctx context.Context, repoPath, ref string) (*PackageSnapshot, error) {
	revision, err := snapshot.Resolve(ctx, repoPath, ref)
	if err != nil {
		return nil, err
	}
	return a.loadResolvedSnapshot(ctx, revision)
}

func (a *Analyzer) loadResolvedSnapshot(
	ctx context.Context,
	revision *snapshot.Revision,
) (_ *PackageSnapshot, returnErr error) {
	key := analysisCacheKey("package-graph", revision)
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
	defer func() {
		returnErr = errors.Join(returnErr, source.Close())
	}()

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

func analysisCacheKey(kind string, revision *snapshot.Revision) string {
	return snapshot.Key(
		analysisVersion,
		kind,
		revision.Tree,
		filepath.ToSlash(revision.Subdir),
		runtime.Version(),
		os.Getenv("GOOS"),
		os.Getenv("GOARCH"),
		os.Getenv("CGO_ENABLED"),
		os.Getenv("GOFLAGS"),
		os.Getenv("GOEXPERIMENT"),
	)
}

func (a *Analyzer) loadModuleSnapshotPair(
	ctx context.Context,
	repoPath, oldRef, newRef string,
) (*moduleSnapshot, *moduleSnapshot, error) {
	var oldSnapshot, newSnapshot *moduleSnapshot
	if err := parallelFor(2, func(index int) error {
		ref := oldRef
		if index == 1 {
			ref = newRef
		}
		loaded, err := a.loadModuleSnapshot(ctx, repoPath, ref)
		if err != nil {
			return err
		}
		if index == 0 {
			oldSnapshot = loaded
		} else {
			newSnapshot = loaded
		}
		return nil
	}); err != nil {
		return nil, nil, fmt.Errorf("load module snapshots: %w", err)
	}
	return oldSnapshot, newSnapshot, nil
}

func changedSymbols(
	oldSnapshot, newSnapshot *PackageSnapshot,
	moduleChanges map[string]struct{},
) map[string]struct{} {
	changed := make(map[string]struct{})
	all := make(map[string]struct{}, len(oldSnapshot.Symbols)+len(newSnapshot.Symbols))
	for id := range oldSnapshot.Symbols {
		all[id] = struct{}{}
	}
	for id := range newSnapshot.Symbols {
		all[id] = struct{}{}
	}

	for id := range all {
		if isDispatchSymbol(id) {
			continue
		}
		oldSymbol, oldOK := oldSnapshot.Symbols[id]
		newSymbol, newOK := newSnapshot.Symbols[id]
		if !oldOK || !newOK || oldSymbol.Hash != newSymbol.Hash {
			changed[id] = struct{}{}
		}
	}
	for packagePath := range moduleChanges {
		changed[packageInitID(packagePath)] = struct{}{}
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

func packageImpactGraph(
	changed, affected map[string]struct{},
	reverse map[string]map[string]struct{},
	snapshots ...*PackageSnapshot,
) ([]string, []PackageEdge) {
	symbols := make(map[string]Symbol)
	for _, packageSnapshot := range snapshots {
		maps.Copy(symbols, packageSnapshot.Symbols)
	}

	changedPackages := make(map[string]struct{})
	for id := range changed {
		if symbol, ok := symbols[id]; ok {
			changedPackages[symbol.PackagePath] = struct{}{}
		}
	}

	edges := make(map[PackageEdge]struct{})
	for dependencyID := range affected {
		dependency, ok := symbols[dependencyID]
		if !ok {
			continue
		}
		for dependentID := range reverse[dependencyID] {
			if _, included := affected[dependentID]; !included {
				continue
			}
			dependent, ok := symbols[dependentID]
			if !ok || dependency.PackagePath == dependent.PackagePath {
				continue
			}
			edges[PackageEdge{
				From: dependency.PackagePath,
				To:   dependent.PackagePath,
			}] = struct{}{}
		}
	}

	changedPaths := sortedSet(changedPackages)
	resultEdges := make([]PackageEdge, 0, len(edges))
	for edge := range edges {
		resultEdges = append(resultEdges, edge)
	}
	sort.Slice(resultEdges, func(i, j int) bool {
		if resultEdges[i].From != resultEdges[j].From {
			return resultEdges[i].From < resultEdges[j].From
		}
		return resultEdges[i].To < resultEdges[j].To
	})
	return changedPaths, resultEdges
}
