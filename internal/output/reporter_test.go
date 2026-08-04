package output

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/jimyag/ripples/internal/impact"
)

var reporterPackages = []impact.Package{
	{RelativePath: "cmd/server", Name: "main"},
	{RelativePath: "payment", Name: "payment"},
}

func TestReporterSimple(t *testing.T) {
	var output bytes.Buffer
	err := NewReporter(&output, reporterPackages).Print("simple")
	if err != nil {
		t.Fatalf("Print(simple) error = %v", err)
	}
	want := "cmd/server.main\npayment.payment\n"
	if output.String() != want {
		t.Fatalf("Print(simple) = %q, want %q", output.String(), want)
	}
}

func TestReporterJSONOnlyExposesPathAndName(t *testing.T) {
	var output bytes.Buffer
	err := NewReporter(&output, reporterPackages).Print("json")
	if err != nil {
		t.Fatalf("Print(json) error = %v", err)
	}
	want := `[
  {
    "path": "cmd/server",
    "name": "main"
  },
  {
    "path": "payment",
    "name": "payment"
  }
]
`
	if output.String() != want {
		t.Fatalf("Print(json) = %q, want %q", output.String(), want)
	}
}

func TestReporterSummary(t *testing.T) {
	var output bytes.Buffer
	err := NewReporter(&output, reporterPackages).Print("summary")
	if err != nil {
		t.Fatalf("Print(summary) error = %v", err)
	}
	want := "Affected packages: 2\n- cmd/server.main\n- payment.payment\n"
	if output.String() != want {
		t.Fatalf("Print(summary) = %q, want %q", output.String(), want)
	}
}

func TestReporterDOTPrintsReversePackageRelationships(t *testing.T) {
	analysis := impact.Analysis{
		Packages: []impact.Package{
			{
				Path:         "example.com/app/cmd/server",
				RelativePath: "cmd/server",
				Name:         "main",
			},
			{
				Path:         "example.com/app/payment",
				RelativePath: "payment",
				Name:         "payment",
			},
			{
				Path:         "example.com/app/internal/order",
				RelativePath: "internal/order",
				Name:         "order",
			},
		},
		ChangedPackages: []string{"example.com/app/payment"},
		Edges: []impact.PackageEdge{
			{
				From: "example.com/app/internal/order",
				To:   "example.com/app/cmd/server",
			},
			{
				From: "example.com/app/payment",
				To:   "example.com/app/internal/order",
			},
		},
	}

	var output bytes.Buffer
	err := NewAnalysisReporter(&output, analysis).Print("dot")
	if err != nil {
		t.Fatalf("Print(dot) error = %v", err)
	}
	want, err := os.ReadFile("../../docs/impact-example.dot")
	if err != nil {
		t.Fatalf("ReadFile(impact-example.dot) error = %v", err)
	}
	got := strings.ReplaceAll(output.String(), "\n\t\n", "\n\n")
	if got != string(want) {
		t.Fatalf("Print(dot) = %q, want %q", output.String(), want)
	}
}

func TestReporterDOTRequiresDetailedAnalysis(t *testing.T) {
	err := NewReporter(&bytes.Buffer{}, reporterPackages).Print("dot")
	if err == nil || err.Error() != "DOT output requires detailed analysis results" {
		t.Fatalf("Print(dot) error = %v, want detailed analysis error", err)
	}
}

func TestReporterRejectsUnknownFormat(t *testing.T) {
	err := NewReporter(&bytes.Buffer{}, reporterPackages).Print("xml")
	if err == nil || err.Error() != `unsupported output format "xml"` {
		t.Fatalf("Print(xml) error = %v, want unsupported format error", err)
	}
}
