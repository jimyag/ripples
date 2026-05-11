package analyzer

// AffectedBinary represents a binary/service affected by code changes
type AffectedBinary struct {
	Name      string   `json:"name"`       // Binary name (e.g., "cmd/service1")
	PkgPath   string   `json:"package"`    // Package path
	TracePath []string `json:"trace_path"` // Call trace path from main to changed function
}
