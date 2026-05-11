package analyzer

import (
	"fmt"
	"go/token"
	"os"
	"path/filepath"
	"strings"

	"github.com/jimyag/ripples/internal/git"
	"github.com/jimyag/ripples/internal/parser"
)

// ChangeDetector 变更检测器
type ChangeDetector struct {
	parser      *parser.Parser
	projectPath string
}

// NewChangeDetector 创建变更检测器
func NewChangeDetector(p *parser.Parser, projectPath string) *ChangeDetector {
	return &ChangeDetector{
		parser:      p,
		projectPath: projectPath,
	}
}

// ChangedSymbol 变更的符号
type ChangedSymbol struct {
	Symbol      *parser.Symbol
	ChangeType  ChangeType
	PackagePath string
}

// ChangeType 变更类型
type ChangeType string

const (
	ChangeTypeAdd    ChangeType = "ADD"
	ChangeTypeModify ChangeType = "MODIFY"
	ChangeTypeDelete ChangeType = "DELETE" // 目前主要关注修改和新增
)

// DetectChanges 检测变更的符号
func (cd *ChangeDetector) DetectChanges(oldCommit, newCommit string) ([]ChangedSymbol, error) {
	// 1. 获取 git diff
	diffContent, err := git.GetGitDiff(cd.projectPath, oldCommit, newCommit)
	if err != nil {
		return nil, fmt.Errorf("获取 git diff 失败: %w", err)
	}

	fileDiffs, err := git.ParseDiff(diffContent)
	if err != nil {
		return nil, fmt.Errorf("解析 diff 失败: %w", err)
	}

	var changedSymbols []ChangedSymbol
	var parseErrors []error

	// 2. 分析每个变更的文件
	for _, fileDiff := range fileDiffs {
		// 只分析 Go 文件
		if !isRuntimeGoFile(fileDiff.Filename) {
			continue
		}

		if fileDiff.IsDeletedFile {
			packagePath := cd.packagePathForFile(fileDiff.Filename)
			if packagePath == "" {
				parseErrors = append(parseErrors, fmt.Errorf("无法推断已删除文件的包路径: %s", fileDiff.Filename))
				continue
			}
			changedSymbols = append(changedSymbols, ChangedSymbol{
				Symbol: &parser.Symbol{
					Name:        filepath.Base(fileDiff.Filename),
					Kind:        parser.SymbolKindInit,
					PackagePath: packagePath,
				},
				ChangeType:  ChangeTypeDelete,
				PackagePath: packagePath,
			})
			continue
		}

		// 解析文件
		absFilename := filepath.Join(cd.projectPath, fileDiff.Filename)
		symbols, err := cd.parser.ParseFile(absFilename)
		if err != nil {
			parseErrors = append(parseErrors, fmt.Errorf("%s: %w", fileDiff.Filename, err))
			continue
		}

		// 3. 映射变更行到符号
		fileChangedSymbols := cd.mapLinesToSymbols(symbols, fileDiff.ChangedLines, fileDiff.Filename)
		changedSymbols = append(changedSymbols, fileChangedSymbols...)
	}

	if len(parseErrors) > 0 {
		return changedSymbols, fmt.Errorf("解析变更文件失败: %v", parseErrors)
	}

	return changedSymbols, nil
}

func isRuntimeGoFile(filename string) bool {
	return strings.HasSuffix(filename, ".go") && !strings.HasSuffix(filename, "_test.go")
}

// mapLinesToSymbols 将变更行映射到符号
func (cd *ChangeDetector) mapLinesToSymbols(symbols []*parser.Symbol, changedLines []int, filename string) []ChangedSymbol {
	var res []ChangedSymbol
	seen := make(map[*parser.Symbol]bool)

	fset := cd.parser.GetFileSet()

	for _, line := range changedLines {
		// 直接找到包含该行的顶层符号
		symbol := cd.findTopLevelSymbolContainingLine(symbols, fset, line)
		if symbol != nil && !seen[symbol] {
			res = append(res, ChangedSymbol{
				Symbol:      symbol,
				ChangeType:  ChangeTypeModify,
				PackagePath: symbol.PackagePath,
			})
			seen[symbol] = true
		}
	}

	return res
}

// findTopLevelSymbolContainingLine 找到包含指定行的最小符号
func (cd *ChangeDetector) findTopLevelSymbolContainingLine(symbols []*parser.Symbol, fset *token.FileSet, line int) *parser.Symbol {
	var best *parser.Symbol
	bestWidth := int(^uint(0) >> 1)

	for _, s := range symbols {
		if !s.ContainsLine(fset, line) {
			continue
		}

		startLine := fset.Position(s.StartPos).Line
		endLine := fset.Position(s.EndPos).Line
		width := endLine - startLine
		if best == nil || width < bestWidth {
			best = s
			bestWidth = width
		}
	}
	return best
}

func (cd *ChangeDetector) packagePathForFile(filename string) string {
	modulePath := getModulePath(cd.projectPath)
	if modulePath == "" {
		return ""
	}

	dir := filepath.Dir(filepath.ToSlash(filename))
	if dir == "." {
		return modulePath
	}
	return modulePath + "/" + strings.TrimPrefix(dir, "./")
}

func getModulePath(projectPath string) string {
	content, err := os.ReadFile(filepath.Join(projectPath, "go.mod"))
	if err != nil {
		return ""
	}

	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module "))
		}
	}
	return ""
}
