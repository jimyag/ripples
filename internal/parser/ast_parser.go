package parser

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/packages"
)

// Parser 符号解析器
type Parser struct {
	fset     *token.FileSet
	packages []*packages.Package
}

// NewParser 创建新的符号解析器
func NewParser() *Parser {
	return &Parser{
		fset: token.NewFileSet(),
	}
}

// LoadProject 加载整个项目
func (p *Parser) LoadProject(projectPath string) error {
	cfg := &packages.Config{
		Mode: packages.LoadAllSyntax,
		Fset: p.fset,
		Dir:  projectPath,
	}

	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return fmt.Errorf("加载项目失败: %w", err)
	}

	// 检查是否有错误
	var hasErrors bool
	for _, pkg := range pkgs {
		if len(pkg.Errors) > 0 {
			hasErrors = true
			for _, err := range pkg.Errors {
				fmt.Printf("包 %s 错误: %v\n", pkg.PkgPath, err)
			}
		}
	}

	if hasErrors {
		return fmt.Errorf("部分包加载失败")
	}

	p.packages = pkgs
	return nil
}

// ParseFile 解析单个文件的符号
func (p *Parser) ParseFile(filename string) ([]*Symbol, error) {
	absFilename, err := filepath.Abs(filename)
	if err != nil {
		return nil, fmt.Errorf("获取绝对路径失败: %w", err)
	}

	var targetPkg *packages.Package
	var targetFile *ast.File

	// 查找目标文件所在的包
	for _, pkg := range p.packages {
		for i, file := range pkg.GoFiles {
			absFile, _ := filepath.Abs(file)
			if absFile == absFilename && i < len(pkg.Syntax) {
				targetPkg = pkg
				targetFile = pkg.Syntax[i]
				break
			}
		}
		if targetFile != nil {
			break
		}
	}

	if targetFile == nil || targetPkg == nil {
		return nil, fmt.Errorf("未找到文件: %s", absFilename)
	}

	return p.extractSymbolsFromFile(targetFile, targetPkg, absFilename)
}

// extractSymbolsFromFile 从文件中提取符号
func (p *Parser) extractSymbolsFromFile(file *ast.File, pkg *packages.Package, filename string) ([]*Symbol, error) {
	var symbols []*Symbol

	// 1. 提取 import 语句
	for _, imp := range file.Imports {
		importPath := strings.Trim(imp.Path.Value, `"`)
		importExtra := ImportExtra{
			Path: importPath,
		}
		if imp.Name != nil {
			importExtra.Alias = imp.Name.Name
		}

		symbol := &Symbol{
			Name:        importPath,
			Kind:        SymbolKindImport,
			Position:    p.fset.Position(imp.Pos()),
			StartPos:    imp.Pos(),
			EndPos:      imp.End(),
			Extra:       importExtra,
			PackagePath: pkg.PkgPath,
		}
		symbols = append(symbols, symbol)
	}

	// 2. 提取顶层声明
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			// 函数和方法
			funcSymbols := p.extractFunction(d, pkg, filename)
			symbols = append(symbols, funcSymbols...)

		case *ast.GenDecl:
			// 常量、变量、类型声明
			genSymbols := p.extractGenDecl(d, pkg, filename)
			symbols = append(symbols, genSymbols...)
		}
	}

	return symbols, nil
}

// extractFunction 提取函数/方法声明
func (p *Parser) extractFunction(funcDecl *ast.FuncDecl, pkg *packages.Package, filename string) []*Symbol {
	var symbols []*Symbol

	if funcDecl.Name == nil {
		return symbols
	}

	funcExtra := FunctionExtra{}
	kind := SymbolKindFunction

	// 检查是否是 init 函数
	if funcDecl.Name.Name == "init" {
		kind = SymbolKindInit
	}

	// 检查是否是方法
	if funcDecl.Recv != nil && len(funcDecl.Recv.List) > 0 {
		funcExtra.IsMethod = true
		funcExtra.ReceiverType = p.getTypeString(funcDecl.Recv.List[0].Type)
	}

	symbol := &Symbol{
		Name:        funcDecl.Name.Name,
		Kind:        kind,
		Position:    p.fset.Position(funcDecl.Pos()),
		StartPos:    funcDecl.Pos(),
		EndPos:      funcDecl.End(),
		Extra:       funcExtra,
		PackagePath: pkg.PkgPath,
	}

	symbols = append(symbols, symbol)
	return symbols
}

// extractGenDecl 提取通用声明
func (p *Parser) extractGenDecl(genDecl *ast.GenDecl, pkg *packages.Package, filename string) []*Symbol {
	var symbols []*Symbol

	for _, spec := range genDecl.Specs {
		switch s := spec.(type) {
		case *ast.ValueSpec:
			// 常量或变量
			kind := SymbolKindVariable
			if genDecl.Tok == token.CONST {
				kind = SymbolKindConstant
			}

			for _, name := range s.Names {
				symbol := &Symbol{
					Name:        name.Name,
					Kind:        kind,
					Position:    p.fset.Position(name.Pos()),
					StartPos:    s.Pos(),
					EndPos:      s.End(),
					PackagePath: pkg.PkgPath,
				}
				symbols = append(symbols, symbol)
			}

		case *ast.TypeSpec:
			// 类型声明
			typeSymbols := p.extractTypeSpec(s, pkg, filename)
			symbols = append(symbols, typeSymbols...)
		}
	}

	return symbols
}

// extractTypeSpec 提取类型声明
func (p *Parser) extractTypeSpec(typeSpec *ast.TypeSpec, pkg *packages.Package, filename string) []*Symbol {
	var symbols []*Symbol

	if typeSpec.Name == nil {
		return symbols
	}

	kind := SymbolKindType
	typeExtra := TypeExtra{}

	switch t := typeSpec.Type.(type) {
	case *ast.StructType:
		kind = SymbolKindStruct
		typeExtra.IsStruct = true
		typeExtra.UnderlyingType = "struct"

		// 提取结构体字段
		if t.Fields != nil {
			for _, field := range t.Fields.List {
				fieldSymbols := p.extractStructField(field, pkg, filename)
				typeExtra.Fields = append(typeExtra.Fields, fieldSymbols...)
			}
		}

	case *ast.InterfaceType:
		kind = SymbolKindInterface
		typeExtra.IsInterface = true
		typeExtra.UnderlyingType = "interface"

		// 提取接口方法
		if t.Methods != nil {
			for _, method := range t.Methods.List {
				methodSymbols := p.extractInterfaceMethod(method, pkg, filename)
				typeExtra.Methods = append(typeExtra.Methods, methodSymbols...)
			}
		}

	default:
		kind = SymbolKindTypeAlias
		typeExtra.UnderlyingType = p.getTypeString(typeSpec.Type)
	}

	symbol := &Symbol{
		Name:        typeSpec.Name.Name,
		Kind:        kind,
		Position:    p.fset.Position(typeSpec.Pos()),
		StartPos:    typeSpec.Pos(),
		EndPos:      typeSpec.End(),
		Extra:       typeExtra,
		PackagePath: pkg.PkgPath,
	}

	symbols = append(symbols, symbol)
	return symbols
}

// extractStructField 提取结构体字段
func (p *Parser) extractStructField(field *ast.Field, pkg *packages.Package, filename string) []*Symbol {
	var symbols []*Symbol

	if len(field.Names) == 0 {
		// 嵌入字段
		symbol := &Symbol{
			Name:        p.getTypeString(field.Type),
			Kind:        SymbolKindStructField,
			Position:    p.fset.Position(field.Pos()),
			StartPos:    field.Pos(),
			EndPos:      field.End(),
			PackagePath: pkg.PkgPath,
		}
		symbols = append(symbols, symbol)
	} else {
		// 普通字段
		for _, name := range field.Names {
			symbol := &Symbol{
				Name:        name.Name,
				Kind:        SymbolKindStructField,
				Position:    p.fset.Position(name.Pos()),
				StartPos:    field.Pos(),
				EndPos:      field.End(),
				PackagePath: pkg.PkgPath,
			}
			symbols = append(symbols, symbol)
		}
	}

	return symbols
}

// extractInterfaceMethod 提取接口方法
func (p *Parser) extractInterfaceMethod(method *ast.Field, pkg *packages.Package, filename string) []*Symbol {
	var symbols []*Symbol

	if len(method.Names) == 0 {
		// 嵌入的接口
		symbol := &Symbol{
			Name:        p.getTypeString(method.Type),
			Kind:        SymbolKindInterface,
			Position:    p.fset.Position(method.Pos()),
			StartPos:    method.Pos(),
			EndPos:      method.End(),
			PackagePath: pkg.PkgPath,
		}
		symbols = append(symbols, symbol)
	} else {
		// 接口方法
		for _, name := range method.Names {
			symbol := &Symbol{
				Name:        name.Name,
				Kind:        SymbolKindFunction,
				Position:    p.fset.Position(name.Pos()),
				StartPos:    method.Pos(),
				EndPos:      method.End(),
				PackagePath: pkg.PkgPath,
			}
			symbols = append(symbols, symbol)
		}
	}

	return symbols
}

// getTypeString 获取类型字符串
func (p *Parser) getTypeString(expr ast.Expr) string {
	if expr == nil {
		return "unknown"
	}

	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + p.getTypeString(t.X)
	case *ast.ArrayType:
		if t.Len == nil {
			return "[]" + p.getTypeString(t.Elt)
		}
		return "[...]" + p.getTypeString(t.Elt)
	case *ast.MapType:
		return "map[" + p.getTypeString(t.Key) + "]" + p.getTypeString(t.Value)
	case *ast.SelectorExpr:
		return p.getTypeString(t.X) + "." + t.Sel.Name
	case *ast.InterfaceType:
		return "interface{}"
	case *ast.StructType:
		return "struct{}"
	default:
		return fmt.Sprintf("%T", expr)
	}
}

// GetTypeInfo 获取类型信息(用于依赖分析)
func (p *Parser) GetTypeInfo(pkgPath string) (*types.Package, *types.Info, error) {
	for _, pkg := range p.packages {
		if pkg.PkgPath == pkgPath {
			return pkg.Types, pkg.TypesInfo, nil
		}
	}
	return nil, nil, fmt.Errorf("未找到包: %s", pkgPath)
}

// GetPackages 返回所有加载的包
func (p *Parser) GetPackages() []*packages.Package {
	return p.packages
}

// GetFileSet 返回文件集
func (p *Parser) GetFileSet() *token.FileSet {
	return p.fset
}
