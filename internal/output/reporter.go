package output

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/jimyag/ripples/internal/impact"
)

// Reporter formats affected packages for CLI consumers.
type Reporter struct {
	writer   io.Writer
	results  []impact.Package
	analysis *impact.Analysis
}

// NewReporter creates a reporter that writes to writer.
func NewReporter(writer io.Writer, results []impact.Package) *Reporter {
	return &Reporter{writer: writer, results: results}
}

// NewAnalysisReporter creates a reporter that can also render the reverse
// package relationships from a detailed analysis.
func NewAnalysisReporter(writer io.Writer, analysis impact.Analysis) *Reporter {
	return &Reporter{
		writer:   writer,
		results:  analysis.Packages,
		analysis: &analysis,
	}
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
	case "dot":
		return r.printDOT()
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

func (r *Reporter) printDOT() error {
	if r.analysis == nil {
		return fmt.Errorf("DOT 输出需要完整的影响分析结果")
	}

	packages := append([]impact.Package(nil), r.analysis.Packages...)
	sort.Slice(packages, func(i, j int) bool {
		return packages[i].Path < packages[j].Path
	})
	changed := make(map[string]struct{}, len(r.analysis.ChangedPackages))
	for _, packagePath := range r.analysis.ChangedPackages {
		changed[packagePath] = struct{}{}
	}

	if _, err := fmt.Fprintln(r.writer, "digraph ripples {"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(r.writer, `  rankdir="LR";`); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(r.writer, `  node [shape="box"];`); err != nil {
		return err
	}
	for _, pkg := range packages {
		attributes := []string{"label=" + dotQuote(displayName(pkg))}
		if _, ok := changed[pkg.Path]; ok {
			attributes = append(attributes, `color="#cf222e"`, `penwidth="2"`)
		}
		if _, err := fmt.Fprintf(
			r.writer,
			"  %s [%s];\n",
			dotQuote(pkg.Path),
			strings.Join(attributes, ", "),
		); err != nil {
			return err
		}
	}
	edges := append([]impact.PackageEdge(nil), r.analysis.Edges...)
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		return edges[i].To < edges[j].To
	})
	for _, edge := range edges {
		if _, err := fmt.Fprintf(
			r.writer,
			"  %s -> %s;\n",
			dotQuote(edge.From),
			dotQuote(edge.To),
		); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(r.writer, "}")
	return err
}

func dotQuote(value string) string {
	replacer := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		"\n", `\n`,
		"\r", `\r`,
	)
	return `"` + replacer.Replace(value) + `"`
}

func displayName(pkg impact.Package) string {
	return pkg.RelativePath + "." + pkg.Name
}
