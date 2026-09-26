package output

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/emicklei/dot"
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

// CheckFormat reports whether format is a supported output format, so the CLI
// can reject a typo before running the analysis.
func CheckFormat(format string) error {
	switch format {
	case "simple", "json", "text", "summary", "dot":
		return nil
	default:
		return fmt.Errorf("unsupported output format %q", format)
	}
}

// Print writes the requested output format.
func (r *Reporter) Print(format string) error {
	if err := CheckFormat(format); err != nil {
		return err
	}
	switch format {
	case "simple":
		return r.printSimple()
	case "json":
		return r.printJSON()
	case "dot":
		return r.printDOT()
	default:
		return r.printSummary()
	}
}

// existingPackages drops packages deleted by the change: they are reported in
// JSON and DOT but cannot be built or tested.
func (r *Reporter) existingPackages() []impact.Package {
	var packages []impact.Package
	for _, pkg := range r.results {
		if !pkg.Deleted {
			packages = append(packages, pkg)
		}
	}
	return packages
}

func (r *Reporter) printSimple() error {
	for _, pkg := range r.existingPackages() {
		if _, err := fmt.Fprintln(r.writer, displayName(pkg)); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reporter) printJSON() error {
	type packageResult struct {
		Path       string `json:"path"`
		Name       string `json:"name"`
		ImportPath string `json:"import_path"`
		Deleted    bool   `json:"deleted,omitempty"`
	}
	results := make([]packageResult, 0, len(r.results))
	for _, pkg := range r.results {
		results = append(results, packageResult{
			Path:       pkg.RelativePath,
			Name:       pkg.Name,
			ImportPath: pkg.Path,
			Deleted:    pkg.Deleted,
		})
	}
	encoder := json.NewEncoder(r.writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(results)
}

func (r *Reporter) printSummary() error {
	packages := r.existingPackages()
	if _, err := fmt.Fprintf(r.writer, "Affected packages: %d\n", len(packages)); err != nil {
		return err
	}
	for _, pkg := range packages {
		if _, err := fmt.Fprintf(r.writer, "- %s\n", displayName(pkg)); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reporter) printDOT() error {
	if r.analysis == nil {
		return fmt.Errorf("DOT output requires detailed analysis results")
	}

	packages := append([]impact.Package(nil), r.analysis.Packages...)
	sort.Slice(packages, func(i, j int) bool {
		return packages[i].Path < packages[j].Path
	})
	changed := make(map[string]struct{}, len(r.analysis.ChangedPackages))
	for _, packagePath := range r.analysis.ChangedPackages {
		changed[packagePath] = struct{}{}
	}

	graph := dot.NewGraph(dot.Directed).ID("ripples")
	graph.Attr("rankdir", "LR")
	nodes := make(map[string]dot.Node, len(packages))
	for _, pkg := range packages {
		node := graph.Node(pkg.Path).Label(displayName(pkg)).Box()
		if _, ok := changed[pkg.Path]; ok {
			node.Attr("color", "#cf222e")
			node.Attr("penwidth", 2)
		}
		if pkg.Deleted {
			node.Attr("style", "dashed")
		}
		nodes[pkg.Path] = node
	}
	edges := append([]impact.PackageEdge(nil), r.analysis.Edges...)
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].From != edges[j].From {
			return edges[i].From < edges[j].From
		}
		return edges[i].To < edges[j].To
	})
	for _, edge := range edges {
		from, fromOK := nodes[edge.From]
		to, toOK := nodes[edge.To]
		if !fromOK || !toOK {
			return fmt.Errorf(
				"impact graph edge references an unknown package: %s -> %s",
				edge.From,
				edge.To,
			)
		}
		graph.Edge(from, to)
	}
	_, err := io.WriteString(r.writer, graph.String())
	return err
}

func displayName(pkg impact.Package) string {
	return pkg.RelativePath + "." + pkg.Name
}
