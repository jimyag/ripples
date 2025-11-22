package parser

import "go/token"

// Symbol 表示一个符号
type Symbol struct {
	Parent   *Symbol   // 父符号
	Children []*Symbol // 子符号

	Name     string         // 符号的名称
	Kind     SymbolKind     // 符号的种类
	Position token.Position // 符号的位置
	StartPos token.Pos      // 开始位置
	EndPos   token.Pos      // 结束位置

	Extra any // 额外信息,比如导入路径

	// 用于依赖分析
	PackagePath string // 所属包的导入路径
}

type SymbolKind string

const (
	SymbolKindPackage     SymbolKind = "Package"     // 包
	SymbolKindFile        SymbolKind = "File"        // 文件
	SymbolKindImport      SymbolKind = "Import"      // 导入
	SymbolKindConstant    SymbolKind = "Constant"    // 常量
	SymbolKindVariable    SymbolKind = "Variable"    // 变量
	SymbolKindType        SymbolKind = "Type"        // 类型
	SymbolKindTypeAlias   SymbolKind = "TypeAlias"   // 类型别名
	SymbolKindStruct      SymbolKind = "Struct"      // 结构体
	SymbolKindStructField SymbolKind = "StructField" // 字段
	SymbolKindInterface   SymbolKind = "Interface"   // 接口
	SymbolKindFunction    SymbolKind = "Function"    // 函数、方法
	SymbolKindInit        SymbolKind = "Init"        // init 函数
)

// ImportExtra 导入符号的额外信息
type ImportExtra struct {
	Alias string // 导入的别名
	Path  string // 导入的路径
}

// IsBlankImport 判断是否是空白导入 (_ import)
func (i ImportExtra) IsBlankImport() bool {
	return i.Alias == "_"
}

// FunctionExtra 函数符号的额外信息
type FunctionExtra struct {
	ReceiverType string // 接收者类型(如果是方法)
	IsMethod     bool   // 是否是方法
}

// TypeExtra 类型符号的额外信息
type TypeExtra struct {
	UnderlyingType string      // 底层类型
	IsStruct       bool        // 是否是结构体
	IsInterface    bool        // 是否是接口
	Fields         []*Symbol   // 字段(如果是结构体)
	Methods        []*Symbol   // 方法
}

// ContainsLine 判断符号是否包含指定行
func (s *Symbol) ContainsLine(fset *token.FileSet, line int) bool {
	startLine := fset.Position(s.StartPos).Line
	endLine := fset.Position(s.EndPos).Line
	return line >= startLine && line <= endLine
}

// IsTopLevel 判断是否是顶层符号(影响整个包)
func (s *Symbol) IsTopLevel() bool {
	// 1. 空白导入 (_ import)
	if s.Kind == SymbolKindImport {
		if extra, ok := s.Extra.(ImportExtra); ok && extra.IsBlankImport() {
			return true
		}
	}
	// 2. init 函数
	if s.Kind == SymbolKindInit {
		return true
	}
	return false
}
