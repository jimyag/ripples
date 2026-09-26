package snapshot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestCacheCompressesEntries(t *testing.T) {
	cache := &Cache{Dir: t.TempDir()}
	value := make([]string, 10000)
	for index := range value {
		value[index] = "example.com/app/internal/service::method::Service.Handle"
	}
	if err := cache.Store("snapshots", "key", value); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(cache.filename("snapshots", "key"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size()*10 > int64(len(encoded)) {
		t.Fatalf("stored entry is %d bytes for %d bytes of JSON, want compression", info.Size(), len(encoded))
	}
	var got []string
	if hit, err := cache.Load("snapshots", "key", &got); err != nil || !hit || len(got) != len(value) {
		t.Fatalf("Load() = %d values, %v, %v", len(got), hit, err)
	}
}

func TestCachePrunesEntriesUnusedForMaxAge(t *testing.T) {
	cache := &Cache{Dir: t.TempDir()}
	for _, key := range []string{"stale", "fresh", "read"} {
		if err := cache.Store("snapshots", key, key); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-8 * 24 * time.Hour)
	for _, key := range []string{"stale", "read"} {
		if err := os.Chtimes(cache.filename("snapshots", key), old, old); err != nil {
			t.Fatal(err)
		}
	}
	var value string
	if hit, err := cache.Load("snapshots", "read", &value); err != nil || !hit {
		t.Fatalf("Load(read) = %v, %v", hit, err)
	}

	if err := cache.Prune(7 * 24 * time.Hour); err != nil {
		t.Fatalf("Prune() error = %v", err)
	}
	for key, want := range map[string]bool{"stale": false, "fresh": true, "read": true} {
		if hit, err := cache.Load("snapshots", key, &value); err != nil || hit != want {
			t.Errorf("Load(%s) after Prune() = %v, %v; want hit %v", key, hit, err, want)
		}
	}
}

func TestDefaultCacheRequiresAbsoluteOverride(t *testing.T) {
	t.Setenv("RIPPLES_CACHE", filepath.Join("relative", "cache"))
	if _, err := DefaultCache(); err == nil {
		t.Fatal("DefaultCache() error = nil, want relative path error")
	}
}
