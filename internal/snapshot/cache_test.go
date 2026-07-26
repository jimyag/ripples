package snapshot

import (
	"path/filepath"
	"testing"
)

func TestCacheRoundTrip(t *testing.T) {
	cache := &Cache{Dir: t.TempDir()}
	key := Key("tree", "darwin", "arm64")
	want := struct {
		Name  string
		Count int
	}{
		Name:  "snapshot",
		Count: 2,
	}

	var missing any
	hit, err := cache.Load("snapshots", key, &missing)
	if err != nil {
		t.Fatalf("Load(missing) error = %v", err)
	}
	if hit {
		t.Fatal("Load(missing) hit = true, want false")
	}

	if err := cache.Store("snapshots", key, want); err != nil {
		t.Fatalf("Store() error = %v", err)
	}

	var got struct {
		Name  string
		Count int
	}
	hit, err = cache.Load("snapshots", key, &got)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !hit {
		t.Fatal("Load() hit = false, want true")
	}
	if got != want {
		t.Fatalf("Load() = %#v, want %#v", got, want)
	}
}

func TestDefaultCacheRequiresAbsoluteOverride(t *testing.T) {
	t.Setenv("RIPPLES_CACHE", filepath.Join("relative", "cache"))
	if _, err := DefaultCache(); err == nil {
		t.Fatal("DefaultCache() error = nil, want relative path error")
	}
}
