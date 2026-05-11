package analyzer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestChangeDetector_Placeholder(t *testing.T) {
	// Placeholder to avoid lint errors and unused file issues
}

func TestPackagePathForDeletedFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/app\n"), 0644); err != nil {
		t.Fatalf("WriteFile(go.mod) error = %v", err)
	}

	cd := NewChangeDetector(nil, dir)
	got := cd.packagePathForFile("internal/service/old.go")
	if got != "example.com/app/internal/service" {
		t.Fatalf("packagePathForFile() = %q, want %q", got, "example.com/app/internal/service")
	}
}

func TestIsRuntimeGoFile(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"internal/service/order.go", true},
		{"internal/service/order_test.go", false},
		{"internal/web/app.tsx", false},
	}

	for _, tt := range tests {
		if got := isRuntimeGoFile(tt.name); got != tt.want {
			t.Fatalf("isRuntimeGoFile(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}
