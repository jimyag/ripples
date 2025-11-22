package lsp

// CallNode represents a node in the call chain
type CallNode struct {
	FunctionName string
	PackagePath  string
}

// CallPath represents a call path from a changed symbol to a main function
type CallPath struct {
	BinaryName string
	MainURI    string
	Path       []CallNode
}
