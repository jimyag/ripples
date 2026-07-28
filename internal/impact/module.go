package impact

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"
	gopackages "golang.org/x/tools/go/packages"

	"github.com/jimyag/ripples/internal/snapshot"
)

type moduleSnapshot struct {
	GlobalHash string                    `json:"global_hash"`
	Packages   map[string]packageModules `json:"packages"`
	Sums       map[string]string         `json:"sums,omitempty"`
	Cached     bool                      `json:"-"`
}

type packageModules struct {
	Modules []string `json:"modules,omitempty"`
	SumKeys []string `json:"sum_keys,omitempty"`
}

func (a *Analyzer) loadModuleSnapshot(
	ctx context.Context,
	repoPath, ref string,
) (_ *moduleSnapshot, returnErr error) {
	revision, err := snapshot.Resolve(ctx, repoPath, ref)
	if err != nil {
		return nil, err
	}
	key := analysisCacheKey("module-graph", revision)
	var result moduleSnapshot
	if a.cache != nil {
		hit, err := a.cache.Load("module-snapshots", key, &result)
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

	result, err = buildModuleSnapshot(ctx, source.Dir)
	if err != nil {
		return nil, err
	}
	if a.cache != nil {
		if err := a.cache.Store("module-snapshots", key, result); err != nil {
			return nil, err
		}
	}
	return &result, nil
}

func buildModuleSnapshot(ctx context.Context, root string) (moduleSnapshot, error) {
	loaded, err := gopackages.Load(&gopackages.Config{
		Context: ctx,
		Dir:     root,
		Mode: gopackages.NeedName |
			gopackages.NeedImports |
			gopackages.NeedDeps |
			gopackages.NeedModule,
	}, "./...")
	if err != nil {
		return moduleSnapshot{}, fmt.Errorf("load module dependency graph: %w", err)
	}
	if err := packageLoadError(loaded); err != nil {
		return moduleSnapshot{}, err
	}

	globalHash, err := effectiveModuleConfigHash(root)
	if err != nil {
		return moduleSnapshot{}, err
	}
	sums, err := moduleSums(root)
	if err != nil {
		return moduleSnapshot{}, err
	}
	result := moduleSnapshot{
		GlobalHash: globalHash,
		Packages:   make(map[string]packageModules, len(loaded)),
		Sums:       sums,
	}
	memo := make(map[string]packageModules)
	for _, pkg := range loaded {
		result.Packages[pkg.PkgPath] = collectPackageModules(pkg, memo)
	}
	return result, nil
}

func packageLoadError(packages []*gopackages.Package) error {
	var messages []string
	for _, pkg := range packages {
		for _, packageErr := range pkg.Errors {
			messages = append(messages, packageErr.Error())
		}
	}
	if len(messages) == 0 {
		return nil
	}
	sort.Strings(messages)
	return fmt.Errorf("load module dependency graph: %s", strings.Join(messages, "; "))
}

func collectPackageModules(pkg *gopackages.Package, memo map[string]packageModules) packageModules {
	if cached, ok := memo[pkg.PkgPath]; ok {
		return cached
	}
	modules := make(map[string]struct{})
	sumKeys := make(map[string]struct{})
	for _, imported := range pkg.Imports {
		collectImportedModules(imported, modules, sumKeys, make(map[string]struct{}))
	}
	result := packageModules{
		Modules: sortedSet(modules),
		SumKeys: sortedSet(sumKeys),
	}
	memo[pkg.PkgPath] = result
	return result
}

func collectImportedModules(
	pkg *gopackages.Package,
	modules map[string]struct{},
	sumKeys map[string]struct{},
	visited map[string]struct{},
) {
	if pkg == nil {
		return
	}
	if _, seen := visited[pkg.PkgPath]; seen {
		return
	}
	visited[pkg.PkgPath] = struct{}{}
	if pkg.Module != nil && !pkg.Module.Main {
		modules[moduleIdentity(pkg.Module)] = struct{}{}
		addModuleSumKeys(sumKeys, pkg.Module)
	}
	for _, imported := range pkg.Imports {
		collectImportedModules(imported, modules, sumKeys, visited)
	}
}

func moduleIdentity(module *gopackages.Module) string {
	var value strings.Builder
	value.WriteString(module.Path)
	value.WriteByte('@')
	value.WriteString(module.Version)
	if module.Replace != nil {
		value.WriteString("=>")
		value.WriteString(module.Replace.Path)
		value.WriteByte('@')
		value.WriteString(module.Replace.Version)
	}
	return value.String()
}

func addModuleSumKeys(keys map[string]struct{}, module *gopackages.Module) {
	addModuleSumKey(keys, module.Path, module.Version)
	if module.Replace != nil {
		addModuleSumKey(keys, module.Replace.Path, module.Replace.Version)
	}
}

func addModuleSumKey(keys map[string]struct{}, path, version string) {
	if path == "" || version == "" {
		return
	}
	key := path + "@" + version
	keys[key] = struct{}{}
	keys[key+"/go.mod"] = struct{}{}
}

func effectiveModuleConfigHash(root string) (string, error) {
	moduleFile, err := parseModuleFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return "", err
	}
	workFile, err := parseWorkFile(filepath.Join(root, "go.work"))
	if err != nil {
		return "", err
	}

	modulePath := ""
	goVersion := ""
	toolchain := ""
	var godebug []string
	if moduleFile != nil {
		if moduleFile.Module != nil {
			modulePath = moduleFile.Module.Mod.Path
		}
		if moduleFile.Go != nil {
			goVersion = moduleFile.Go.Version
		}
		if moduleFile.Toolchain != nil {
			toolchain = moduleFile.Toolchain.Name
		}
		godebug = godebugValues(moduleFile.Godebug)
	}
	if workFile != nil {
		if workFile.Go != nil {
			goVersion = workFile.Go.Version
		}
		if workFile.Toolchain != nil {
			toolchain = workFile.Toolchain.Name
		}
		if len(workFile.Godebug) > 0 {
			godebug = godebugValues(workFile.Godebug)
		}
	}

	hash := sha256.New()
	writeHashValue(hash, "module", modulePath)
	writeHashValue(hash, "go", goVersion)
	writeHashValue(hash, "toolchain", toolchain)
	for _, value := range godebug {
		writeHashValue(hash, "godebug", value)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func parseModuleFile(filename string) (*modfile.File, error) {
	data, err := os.ReadFile(filename)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(filename), err)
	}
	file, err := modfile.Parse(filename, data, nil)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", filepath.Base(filename), err)
	}
	return file, nil
}

func parseWorkFile(filename string) (*modfile.WorkFile, error) {
	data, err := os.ReadFile(filename)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(filename), err)
	}
	file, err := modfile.ParseWork(filename, data, nil)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", filepath.Base(filename), err)
	}
	return file, nil
}

func godebugValues(values []*modfile.Godebug) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value.Key+"="+value.Value)
	}
	sort.Strings(result)
	return result
}

func writeHashValue(writer io.Writer, key, value string) {
	_, _ = io.WriteString(writer, key)
	_, _ = io.WriteString(writer, "\x00")
	_, _ = io.WriteString(writer, value)
	_, _ = io.WriteString(writer, "\x00")
}

func moduleSums(root string) (map[string]string, error) {
	result := make(map[string]string)
	for _, name := range []string{"go.sum", "go.work.sum"} {
		filename := filepath.Join(root, name)
		file, err := os.Open(filename)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) == 3 {
				result[fields[0]+"@"+fields[1]] = fields[2]
			}
		}
		scanErr := scanner.Err()
		closeErr := file.Close()
		if scanErr != nil {
			return nil, fmt.Errorf("read %s: %w", name, scanErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close %s: %w", name, closeErr)
		}
	}
	return result, nil
}

func changedModulePackages(oldSnapshot, newSnapshot *moduleSnapshot) map[string]struct{} {
	changed := make(map[string]struct{})
	allPackages := make(map[string]struct{}, len(oldSnapshot.Packages)+len(newSnapshot.Packages))
	for path := range oldSnapshot.Packages {
		allPackages[path] = struct{}{}
	}
	for path := range newSnapshot.Packages {
		allPackages[path] = struct{}{}
	}
	for path := range allPackages {
		oldPackage, oldOK := oldSnapshot.Packages[path]
		newPackage, newOK := newSnapshot.Packages[path]
		if !oldOK || !newOK {
			continue
		}
		if oldSnapshot.GlobalHash != newSnapshot.GlobalHash ||
			!equalStrings(oldPackage.Modules, newPackage.Modules) ||
			packageChecksumChanged(oldPackage, newPackage, oldSnapshot.Sums, newSnapshot.Sums) {
			changed[path] = struct{}{}
		}
	}
	return changed
}

func packageChecksumChanged(
	oldPackage packageModules,
	newPackage packageModules,
	oldSums map[string]string,
	newSums map[string]string,
) bool {
	keys := make(map[string]struct{}, len(oldPackage.SumKeys))
	for _, key := range oldPackage.SumKeys {
		keys[key] = struct{}{}
	}
	for _, key := range newPackage.SumKeys {
		if _, usedBefore := keys[key]; !usedBefore {
			continue
		}
		oldValue, oldOK := oldSums[key]
		newValue, newOK := newSums[key]
		if oldOK && newOK && oldValue != newValue {
			return true
		}
	}
	return false
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
