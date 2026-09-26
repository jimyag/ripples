package impact

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"sync"

	"github.com/jimyag/ripples/internal/snapshot"
)

const analysisVersion = "symbol-impact-v29"

// Analyzer computes declaration-level impact between two Git revisions.
type Analyzer struct {
	cache *snapshot.Cache
	// Prepare is a shell command run in each exported tree before loading,
	// for example to generate code that is not committed.
	Prepare string
	// Tests also analyzes _test.go files so test-only changes and
	// declarations used by tests reach their packages.
	Tests bool
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
func (a *Analyzer) AnalyzeDetailed(ctx context.Context, repoPath, oldRef, newRef string) (_ Analysis, returnErr error) {
	var exports cleanups
	defer func() {
		returnErr = errors.Join(returnErr, exports.finish())
	}()
	config, err := buildConfiguration(ctx)
	if err != nil {
		return Analysis{}, err
	}
	oldSnapshot, newSnapshot, err := loadSnapshotPair(
		ctx,
		repoPath,
		oldRef,
		newRef,
		snapshot.Resolve,
		func(ctx context.Context, revision *snapshot.Revision) (*PackageSnapshot, error) {
			return a.loadResolvedSnapshot(ctx, revision, config, &exports)
		},
	)
	if err != nil {
		return Analysis{}, err
	}

	moduleChanges := changedModulePackages(&oldSnapshot.Modules, &newSnapshot.Modules)
	changed := changedSymbols(oldSnapshot, newSnapshot, moduleChanges)
	reverse := reverseDependencies(oldSnapshot, newSnapshot)
	packageOf := func(id string) string {
		if symbol, ok := newSnapshot.Symbols[id]; ok {
			return symbol.PackagePath
		}
		return oldSnapshot.Symbols[id].PackagePath
	}
	affectedSymbols, explained := transitiveDependents(
		changed,
		reverse,
		dynamicDependencies(oldSnapshot, newSnapshot),
		packageOf,
	)
	// Declarations reached through a dynamic dependency hang off the method
	// in the package graph, which keeps it connected.
	for method, users := range explained {
		if reverse[method] == nil {
			reverse[method] = make(map[string]struct{})
		}
		maps.Copy(reverse[method], users)
	}

	affectedPackages := make(map[string]struct{})
	for id := range affectedSymbols {
		affectedPackages[packageOf(id)] = struct{}{}
	}

	results := make([]Package, 0, len(affectedPackages))
	for path := range affectedPackages {
		pkg, ok := newSnapshot.Packages[path]
		if !ok {
			pkg = oldSnapshot.Packages[path]
			pkg.Deleted = true
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

	// Building a snapshot holds the module's syntax, types and SSA in memory
	// and already uses every CPU, so building both revisions at once doubles
	// peak memory for little gain; cache hits return quickly either way.
	oldSnapshot, err := load(ctx, revisions[0])
	if err != nil {
		return nil, nil, fmt.Errorf("load old snapshot: %w", err)
	}
	newSnapshot, err := load(ctx, revisions[1])
	if err != nil {
		return nil, nil, fmt.Errorf("load new snapshot: %w", err)
	}
	return oldSnapshot, newSnapshot, nil
}

// LoadSnapshot loads a package summary from cache or builds it from an
// immutable export of the Git tree.
func (a *Analyzer) LoadSnapshot(ctx context.Context, repoPath, ref string) (_ *PackageSnapshot, returnErr error) {
	var exports cleanups
	defer func() {
		returnErr = errors.Join(returnErr, exports.finish())
	}()
	revision, err := snapshot.Resolve(ctx, repoPath, ref)
	if err != nil {
		return nil, err
	}
	config, err := buildConfiguration(ctx)
	if err != nil {
		return nil, err
	}
	return a.loadResolvedSnapshot(ctx, revision, config, &exports)
}

// cleanups removes exports in the background while the analysis continues;
// finish waits for every removal and returns their errors.
type cleanups struct {
	wait sync.WaitGroup
	mu   sync.Mutex
	errs []error
}

func (c *cleanups) run(remove func() error) {
	c.wait.Go(func() {
		if err := remove(); err != nil {
			c.mu.Lock()
			c.errs = append(c.errs, err)
			c.mu.Unlock()
		}
	})
}

func (c *cleanups) finish() error {
	c.wait.Wait()
	return errors.Join(c.errs...)
}

func (a *Analyzer) loadResolvedSnapshot(
	ctx context.Context,
	revision *snapshot.Revision,
	config string,
	exports *cleanups,
) (*PackageSnapshot, error) {
	key := analysisCacheKey("package-graph", revision, config, a.Prepare, strconv.FormatBool(a.Tests))
	if a.cache != nil {
		if cached, ok := loadCachedSnapshot(a.cache, key); ok {
			return cached, nil
		}
	}

	source, err := snapshot.OpenRevision(ctx, revision)
	if err != nil {
		return nil, err
	}
	// Removing a large export takes a while; it runs in the background while
	// the snapshot is stored and the analysis continues.
	defer exports.run(source.Close)

	result, err := buildPackageSnapshot(ctx, source, a.Prepare, a.Tests)
	if err != nil {
		return nil, err
	}
	if a.cache != nil {
		if err := storeSnapshot(a.cache, key, &result); err != nil {
			return nil, err
		}
	}
	return &result, nil
}

// analysisCacheKey identifies a snapshot by the analysis version, the Git tree
// and module directory, the go/types version and every setting that changes
// how the tree is built or prepared.
func analysisCacheKey(kind string, revision *snapshot.Revision, settings ...string) string {
	return snapshot.Key(append([]string{
		analysisVersion,
		kind,
		revision.Tree,
		filepath.ToSlash(revision.Subdir),
		runtime.Version(),
	}, settings...)...)
}

// buildConfiguration returns the effective go env settings that select files,
// build tags and the toolchain, including values from the go env file and
// defaults such as CGO_ENABLED. It runs outside any module; the tree's own
// go.mod and go.work are covered by the tree hash.
func buildConfiguration(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "go", "env", "-json",
		"GOOS", "GOARCH", "CGO_ENABLED", "GOFLAGS", "GOEXPERIMENT", "GOVERSION",
		"GOTOOLCHAIN", "GOWORK", "GO386", "GOAMD64", "GOARM", "GOARM64", "GOMIPS",
		"GOMIPS64", "GOPPC64", "GORISCV64", "GOWASM",
	)
	cmd.Dir = os.TempDir()
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("read go env: %w", err)
	}
	return string(output), nil
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

// dynamicEdge makes a dispatch contract depend on a method only for the
// declarations that also reach one of the callers.
type dynamicEdge struct {
	dispatch string
	callers  []string
}

func dynamicDependencies(snapshots ...*PackageSnapshot) map[string][]dynamicEdge {
	result := make(map[string][]dynamicEdge)
	for _, packageSnapshot := range snapshots {
		for _, symbol := range packageSnapshot.Symbols {
			for _, dependency := range symbol.Dynamic {
				result[dependency.ID] = append(result[dependency.ID], dynamicEdge{
					dispatch: symbol.ID,
					callers:  dependency.Callers,
				})
			}
		}
	}
	return result
}

// transitiveDependents returns the changed declarations and every declaration
// that depends on them.
//
// A method behind a dynamic edge only runs in a binary that both converts its
// type to an interface and makes one of the calls. Package initialization and
// main are separate declarations of one binary, so the two sides meet per
// package: an affected method marks the declarations reaching either side in
// packages that reach both, and explained records them by method. They are
// not propagated further, since their users reach both sides only if their
// own packages do.
func transitiveDependents(
	changed map[string]struct{},
	reverse map[string]map[string]struct{},
	dynamic map[string][]dynamicEdge,
	packageOf func(string) string,
) (affected map[string]struct{}, explained map[string]map[string]struct{}) {
	// users over-approximates the declarations reaching id by following
	// dynamic edges unconditionally.
	usersByID := make(map[string]map[string]struct{})
	users := func(id string) map[string]struct{} {
		if result, ok := usersByID[id]; ok {
			return result
		}
		result := make(map[string]struct{})
		stack := []string{id}
		push := func(user string) {
			if _, seen := result[user]; !seen {
				result[user] = struct{}{}
				stack = append(stack, user)
			}
		}
		for len(stack) > 0 {
			current := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for user := range reverse[current] {
				push(user)
			}
			for _, edge := range dynamic[current] {
				push(edge.dispatch)
			}
		}
		usersByID[id] = result
		return result
	}

	affected = make(map[string]struct{}, len(changed))
	explained = make(map[string]map[string]struct{})
	propagated := make(map[string]bool, len(changed))
	// queue holds declarations whose users are affected; pending holds newly
	// affected declarations whose dynamic edges are still to be followed.
	var queue, pending []string
	affect := func(id string, propagate bool) {
		if _, seen := affected[id]; !seen {
			affected[id] = struct{}{}
			pending = append(pending, id)
		}
		if propagate && !propagated[id] {
			propagated[id] = true
			queue = append(queue, id)
		}
	}
	for id := range changed {
		affect(id, true)
	}
	for len(queue) > 0 || len(pending) > 0 {
		if len(queue) > 0 {
			current := queue[len(queue)-1]
			queue = queue[:len(queue)-1]
			for user := range reverse[current] {
				affect(user, true)
			}
			continue
		}
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		for _, edge := range dynamic[current] {
			converters := users(edge.dispatch)
			calls := make(map[string]struct{})
			for _, caller := range edge.callers {
				calls[caller] = struct{}{}
				maps.Copy(calls, users(caller))
			}
			converting := make(map[string]bool)
			for id := range converters {
				converting[packageOf(id)] = true
			}
			calling := make(map[string]bool)
			for id := range calls {
				calling[packageOf(id)] = true
			}
			for _, side := range []map[string]struct{}{converters, calls} {
				for id := range side {
					if pkg := packageOf(id); !converting[pkg] || !calling[pkg] {
						continue
					}
					if explained[current] == nil {
						explained[current] = make(map[string]struct{})
					}
					explained[current][id] = struct{}{}
					affect(id, false)
				}
			}
		}
	}
	return affected, explained
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
