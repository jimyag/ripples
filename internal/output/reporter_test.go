package output

import (
	"bytes"
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

func TestReporterRejectsUnknownFormat(t *testing.T) {
	err := NewReporter(&bytes.Buffer{}, reporterPackages).Print("xml")
	if err == nil {
		t.Fatal("Print(xml) error = nil, want unsupported format error")
	}
}
