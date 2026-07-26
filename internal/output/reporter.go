package output

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/jimyag/ripples/internal/impact"
)

// Reporter formats affected packages for CLI consumers.
type Reporter struct {
	writer  io.Writer
	results []impact.Package
}

// NewReporter creates a reporter that writes to writer.
func NewReporter(writer io.Writer, results []impact.Package) *Reporter {
	return &Reporter{writer: writer, results: results}
}

// Print writes the requested output format.
func (r *Reporter) Print(format string) error {
	switch format {
	case "simple":
		return r.printSimple()
	case "json":
		return r.printJSON()
	case "text", "summary":
		return r.printSummary()
	default:
		return fmt.Errorf("不支持的输出格式 %q", format)
	}
}

func (r *Reporter) printSimple() error {
	for _, pkg := range r.results {
		if _, err := fmt.Fprintln(r.writer, displayName(pkg)); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reporter) printJSON() error {
	type packageResult struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	results := make([]packageResult, 0, len(r.results))
	for _, pkg := range r.results {
		results = append(results, packageResult{
			Path: pkg.RelativePath,
			Name: pkg.Name,
		})
	}
	encoder := json.NewEncoder(r.writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(results)
}

func (r *Reporter) printSummary() error {
	if _, err := fmt.Fprintf(r.writer, "受影响的包: %d 个\n", len(r.results)); err != nil {
		return err
	}
	for _, pkg := range r.results {
		if _, err := fmt.Fprintf(r.writer, "- %s\n", displayName(pkg)); err != nil {
			return err
		}
	}
	return nil
}

func displayName(pkg impact.Package) string {
	return pkg.RelativePath + "." + pkg.Name
}
