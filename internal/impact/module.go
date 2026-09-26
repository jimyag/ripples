package impact

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"
	gopackages "golang.org/x/tools/go/packages"
)

type moduleSnapshot struct {
	GlobalHash string                    `json:"global_hash"`
	Packages   map[string]packageModules `json:"packages"`
	Sums       map[string]string         `json:"sums,omitempty"`
}

type packageModules struct {
	Modules []string `json:"modules,omitempty"`
	SumKeys []string `json:"sum_keys,omitempty"`
}

// buildModuleSnapshot maps every local package to the modules it depends on.
// loaded holds the packages matching ./... with their dependency metadata;
// test variants are skipped so the result does not depend on -tests.
func buildModuleSnapshot(ctx context.Context, root string, loaded []*gopackages.Package) (moduleSnapshot, error) {
	workFilename, err := goWorkFile(ctx, root)
	if err != nil {
		return moduleSnapshot{}, err
	}
	workFile, err := parseWorkFile(workFilename)
	if err != nil {
		return moduleSnapshot{}, err
	}
	globalHash, err := effectiveModuleConfigHash(root, workFile)
	if err != nil {
		return moduleSnapshot{}, err
	}
	sums, err := moduleSums(moduleSumFiles(root, workFilename, workFile))
	if err != nil {
		return moduleSnapshot{}, err
	}
	result := moduleSnapshot{
		GlobalHash: globalHash,
		Packages:   make(map[string]packageModules, len(loaded)),
		Sums:       sums,
	}
	memo := make(map[*gopackages.Package]*moduleUse)
	for _, pkg := range loaded {
		if pkg.ForTest != "" {
			continue
		}
		modules := make(map[string]struct{})
		sumKeys := make(map[string]struct{})
		for _, imported := range pkg.Imports {
			use := importedModules(imported, memo)
			maps.Copy(modules, use.modules)
			maps.Copy(sumKeys, use.sumKeys)
		}
		result.Packages[pkg.PkgPath] = packageModules{
			Modules: sortedSet(modules),
			SumKeys: sortedSet(sumKeys),
		}
	}
	return result, nil
}

// moduleUse holds the modules, and their checksum keys, of a package and
// everything it imports.
type moduleUse struct {
	modules map[string]struct{}
	sumKeys map[string]struct{}
}

// importedModules computes moduleUse once per package of the import graph,
// so the cost grows with the graph instead of with every path through it.
func importedModules(pkg *gopackages.Package, memo map[*gopackages.Package]*moduleUse) *moduleUse {
	if use, ok := memo[pkg]; ok {
		return use
	}
	use := &moduleUse{modules: make(map[string]struct{}), sumKeys: make(map[string]struct{})}
	if pkg.Module != nil && !pkg.Module.Main {
		use.modules[moduleIdentity(pkg.Module)] = struct{}{}
		addModuleSumKeys(use.sumKeys, pkg.Module)
	}
	for _, imported := range pkg.Imports {
		child := importedModules(imported, memo)
		maps.Copy(use.modules, child.modules)
		maps.Copy(use.sumKeys, child.sumKeys)
	}
	memo[pkg] = use
	return use
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

// goWorkFile returns the go.work file the go command uses in dir. It may live
// in a parent directory or come from GOWORK; "" means module mode.
func goWorkFile(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "go", "env", "GOWORK")
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("go env GOWORK: %w", err)
	}
	if filename := strings.TrimSpace(string(output)); filename != "off" {
		return filename, nil
	}
	return "", nil
}

// moduleSumFiles lists the checksum files the go command consults: the
// module's go.sum and, in workspace mode, go.work.sum and each used module's
// go.sum.
func moduleSumFiles(root, workFilename string, workFile *modfile.WorkFile) []string {
	filenames := []string{filepath.Join(root, "go.sum")}
	if workFile == nil {
		return filenames
	}
	workDir := filepath.Dir(workFilename)
	filenames = append(filenames, filepath.Join(workDir, "go.work.sum"))
	for _, use := range workFile.Use {
		dir := filepath.FromSlash(use.Path)
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(workDir, dir)
		}
		filenames = append(filenames, filepath.Join(dir, "go.sum"))
	}
	return filenames
}

func effectiveModuleConfigHash(root string, workFile *modfile.WorkFile) (string, error) {
	moduleFile, err := parseModuleFile(filepath.Join(root, "go.mod"))
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

func moduleSums(filenames []string) (map[string]string, error) {
	result := make(map[string]string)
	for _, filename := range filenames {
		name := filepath.Base(filename)
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
