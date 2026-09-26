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

func TestCacheTouchRefreshesExistingEntries(t *testing.T) {
	cache := &Cache{Dir: t.TempDir()}
	if cache.Touch("chunks", "missing") {
		t.Fatal("Touch(missing) = true, want false")
	}
	if err := cache.Store("chunks", "key", "value"); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(cache.filename("chunks", "key"), old, old); err != nil {
		t.Fatal(err)
	}
	if !cache.Touch("chunks", "key") {
		t.Fatal("Touch(existing) = false, want true")
	}
	info, err := os.Stat(cache.filename("chunks", "key"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().After(old.Add(time.Minute)) {
		t.Fatalf("Touch() left modification time at %v", info.ModTime())
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

	if err := cache.Prune(7*24*time.Hour, "snapshots"); err != nil {
		t.Fatalf("Prune() error = %v", err)
	}
	for key, want := range map[string]bool{"stale": false, "fresh": true, "read": true} {
		if hit, err := cache.Load("snapshots", key, &value); err != nil || hit != want {
			t.Errorf("Load(%s) after Prune() = %v, %v; want hit %v", key, hit, err, want)
		}
	}
}

func TestCachePruneEvictsLeastRecentlyUsedEntriesOverMaxSize(t *testing.T) {
	cache := &Cache{Dir: t.TempDir()}
	keys := []string{"a", "b", "c", "d", "e"}
	now := time.Now()
	var entrySize int64
	for index, key := range keys {
		if err := cache.Store("snapshots", key, key); err != nil {
			t.Fatal(err)
		}
		used := now.Add(time.Duration(index-len(keys)) * time.Minute)
		if err := os.Chtimes(cache.filename("snapshots", key), used, used); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(cache.filename("snapshots", key))
		if err != nil {
			t.Fatal(err)
		}
		entrySize = max(entrySize, info.Size())
	}
	cache.MaxBytes = 2 * entrySize

	if err := cache.Prune(7*24*time.Hour, "snapshots"); err != nil {
		t.Fatalf("Prune() error = %v", err)
	}
	var value string
	for index, key := range keys {
		want := index >= len(keys)-2
		if hit, err := cache.Load("snapshots", key, &value); err != nil || hit != want {
			t.Errorf("Load(%s) = %v, %v; want hit %v", key, hit, err, want)
		}
	}
}

// RIPPLES_CACHE may point to a directory shared with other tools, so pruning
// must only remove ripples entries.
func TestCachePruneLeavesUnrelatedFiles(t *testing.T) {
	cache := &Cache{Dir: t.TempDir(), MaxBytes: 1}
	for _, key := range []string{"stale", "fresh"} {
		if err := cache.Store("snapshots", key, key); err != nil {
			t.Fatal(err)
		}
	}
	unrelated := []string{
		filepath.Join(cache.Dir, "other-tool", "data.json"),
		filepath.Join(cache.Dir, "snapshots", "notes.txt"),
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	for _, name := range unrelated {
		if err := os.MkdirAll(filepath.Dir(name), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(name, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(cache.filename("snapshots", "stale"), old, old); err != nil {
		t.Fatal(err)
	}

	if err := cache.Prune(7*24*time.Hour, "snapshots"); err != nil {
		t.Fatalf("Prune() error = %v", err)
	}
	for _, key := range []string{"stale", "fresh"} {
		if _, err := os.Stat(cache.filename("snapshots", key)); !os.IsNotExist(err) {
			t.Errorf("entry %s survived pruning over the size limit: %v", key, err)
		}
	}
	for _, name := range unrelated {
		if _, err := os.Stat(name); err != nil {
			t.Errorf("Prune() removed unrelated file %s: %v", name, err)
		}
	}
}

func TestDefaultCacheReadsMaxSize(t *testing.T) {
	t.Setenv("RIPPLES_CACHE", t.TempDir())
	cache, err := DefaultCache()
	if err != nil || cache.MaxBytes != 1024<<20 {
		t.Fatalf("DefaultCache().MaxBytes = %d, %v; want 1 GiB", cache.MaxBytes, err)
	}
	t.Setenv("RIPPLES_CACHE_MAX_MB", "64")
	if cache, err = DefaultCache(); err != nil || cache.MaxBytes != 64<<20 {
		t.Fatalf("DefaultCache().MaxBytes = %d, %v; want 64 MiB", cache.MaxBytes, err)
	}
	t.Setenv("RIPPLES_CACHE_MAX_MB", "lots")
	if _, err = DefaultCache(); err == nil {
		t.Fatal("DefaultCache() accepted an invalid RIPPLES_CACHE_MAX_MB")
	}
}

func TestDefaultCacheRequiresAbsoluteOverride(t *testing.T) {
	t.Setenv("RIPPLES_CACHE", filepath.Join("relative", "cache"))
	if _, err := DefaultCache(); err == nil {
		t.Fatal("DefaultCache() error = nil, want relative path error")
	}
}
