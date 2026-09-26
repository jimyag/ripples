package output

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/jimyag/ripples/internal/impact"
)

var reporterPackages = []impact.Package{
	{Path: "example.com/app/cmd/server", RelativePath: "cmd/server", Name: "main"},
	{Path: "example.com/app/legacy", RelativePath: "legacy", Name: "legacy", Deleted: true},
	{Path: "example.com/app/payment", RelativePath: "payment", Name: "payment"},
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

func TestReporterJSONMarksDeletedPackages(t *testing.T) {
	var output bytes.Buffer
	err := NewReporter(&output, reporterPackages).Print("json")
	if err != nil {
		t.Fatalf("Print(json) error = %v", err)
	}
	want := `[
  {
    "path": "cmd/server",
    "name": "main",
    "import_path": "example.com/app/cmd/server"
  },
  {
    "path": "legacy",
    "name": "legacy",
    "import_path": "example.com/app/legacy",
    "deleted": true
  },
  {
    "path": "payment",
    "name": "payment",
    "import_path": "example.com/app/payment"
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

func TestReporterDOTDashesDeletedPackages(t *testing.T) {
	analysis := impact.Analysis{Packages: reporterPackages}
	var output bytes.Buffer
	if err := NewAnalysisReporter(&output, analysis).Print("dot"); err != nil {
		t.Fatalf("Print(dot) error = %v", err)
	}
	if !strings.Contains(output.String(), `label="legacy.legacy",shape="box",style="dashed"`) {
		t.Fatalf("Print(dot) does not dash the deleted package:\n%s", output.String())
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
