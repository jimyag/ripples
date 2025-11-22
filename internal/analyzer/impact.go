package analyzer

// AffectedBinary represents a binary/service affected by code changes
type AffectedBinary struct {
	Name      string   // Binary name (e.g., "cmd/service1")
	PkgPath   string   // Package path
	TracePath []string // Call trace path from main to changed function
}
