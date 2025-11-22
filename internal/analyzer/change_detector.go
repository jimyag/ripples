package analyzer

import (
	"fmt"
	"go/token"
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

	// 2. 分析每个变更的文件
	for _, fileDiff := range fileDiffs {
		if fileDiff.IsDeletedFile {
			continue
		}

		// 只分析 Go 文件
		if !strings.HasSuffix(fileDiff.Filename, ".go") {
			continue
		}

		// 解析文件
		absFilename := filepath.Join(cd.projectPath, fileDiff.Filename)
		symbols, err := cd.parser.ParseFile(absFilename)
		if err != nil {
			// 如果是新文件，可能还未被 parser 加载（如果 parser 是预加载的）
			// 这里假设 parser 已经加载了最新的代码
			// 如果解析失败，可能是语法错误，跳过
			continue
		}

		// 3. 映射变更行到符号
		fileChangedSymbols := cd.mapLinesToSymbols(symbols, fileDiff.ChangedLines, fileDiff.Filename)
		changedSymbols = append(changedSymbols, fileChangedSymbols...)
	}

	return changedSymbols, nil
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

// findTopLevelSymbolContainingLine 找到包含指定行的顶层符号
func (cd *ChangeDetector) findTopLevelSymbolContainingLine(symbols []*parser.Symbol, fset *token.FileSet, line int) *parser.Symbol {
	for _, s := range symbols {
		if s.ContainsLine(fset, line) {
			return s
		}
	}
	return nil
}
