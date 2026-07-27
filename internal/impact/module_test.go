package impact

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jimyag/ripples/internal/snapshot"
)

func TestChangedModulePackages(t *testing.T) {
	tests := []struct {
		name string
		old  moduleSnapshot
		new  moduleSnapshot
		want bool
	}{
		{
			name: "unchanged",
			old: moduleSnapshot{
				GlobalHash: "global",
				Packages: map[string]packageModules{
					"example.com/app/service": {Modules: []string{"example.com/dependency@v1.0.0"}},
				},
			},
			new: moduleSnapshot{
				GlobalHash: "global",
				Packages: map[string]packageModules{
					"example.com/app/service": {Modules: []string{"example.com/dependency@v1.0.0"}},
				},
			},
		},
		{
			name: "module version changed",
			old: moduleSnapshot{
				GlobalHash: "global",
				Packages: map[string]packageModules{
					"example.com/app/service": {Modules: []string{"example.com/dependency@v1.0.0"}},
				},
			},
			new: moduleSnapshot{
				GlobalHash: "global",
				Packages: map[string]packageModules{
					"example.com/app/service": {Modules: []string{"example.com/dependency@v1.1.0"}},
				},
			},
			want: true,
		},
		{
			name: "same version checksum changed",
			old: moduleSnapshot{
				GlobalHash: "global",
				Packages: map[string]packageModules{
					"example.com/app/service": {SumKeys: []string{"example.com/dependency@v1.0.0"}},
				},
				Sums: map[string]string{"example.com/dependency@v1.0.0": "old"},
			},
			new: moduleSnapshot{
				GlobalHash: "global",
				Packages: map[string]packageModules{
					"example.com/app/service": {SumKeys: []string{"example.com/dependency@v1.0.0"}},
				},
				Sums: map[string]string{"example.com/dependency@v1.0.0": "new"},
			},
			want: true,
		},
		{
			name: "new checksum entry only",
			old: moduleSnapshot{
				GlobalHash: "global",
				Packages: map[string]packageModules{
					"example.com/app/service": {SumKeys: []string{"example.com/dependency@v1.0.0"}},
				},
			},
			new: moduleSnapshot{
				GlobalHash: "global",
				Packages: map[string]packageModules{
					"example.com/app/service": {SumKeys: []string{"example.com/dependency@v1.0.0"}},
				},
				Sums: map[string]string{"example.com/dependency@v1.0.0": "new"},
			},
		},
		{
			name: "effective build configuration changed",
			old: moduleSnapshot{
				GlobalHash: "old",
				Packages: map[string]packageModules{
					"example.com/app/service": {},
				},
			},
			new: moduleSnapshot{
				GlobalHash: "new",
				Packages: map[string]packageModules{
					"example.com/app/service": {},
				},
			},
			want: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, got := changedModulePackages(&test.old, &test.new)["example.com/app/service"]
			if got != test.want {
				t.Fatalf("changedModulePackages() contains service = %v, want %v", got, test.want)
			}
		})
	}
}

func TestModuleSums(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.sum"), []byte(
		"example.com/dependency v1.0.0 h1:content\n"+
			"example.com/dependency v1.0.0/go.mod h1:module\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.work.sum"), []byte(
		"example.com/workspace v2.0.0 h1:workspace\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := moduleSums(root)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"example.com/dependency@v1.0.0":        "h1:content",
		"example.com/dependency@v1.0.0/go.mod": "h1:module",
		"example.com/workspace@v2.0.0":         "h1:workspace",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("moduleSums() = %v, want %v", got, want)
	}
}

func TestLoadModuleSnapshotUsesPersistentCache(t *testing.T) {
	repo := initModule(t)
	writeModuleFile(t, repo, "service/service.go", "package service\n")
	commitModule(t, repo, "initial")

	analyzer := NewAnalyzer(&snapshot.Cache{Dir: t.TempDir()})
	first, err := analyzer.loadModuleSnapshot(context.Background(), repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if first.Cached {
		t.Fatal("loadModuleSnapshot(first).Cached = true, want false")
	}
	second, err := analyzer.loadModuleSnapshot(context.Background(), repo, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if !second.Cached {
		t.Fatal("loadModuleSnapshot(second).Cached = false, want true")
	}
}
