package snapshot

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"
)

const cacheVersion = "v2"

// Cache stores content-addressed, gzip-compressed JSON analysis artifacts.
type Cache struct {
	Dir string
	// MaxBytes bounds the total size Prune keeps; zero means no bound.
	MaxBytes int64
}

// defaultMaxMB bounds the cache to about a few hundred snapshots of a large
// repository, small enough to save and restore as a CI cache.
const defaultMaxMB = 1024

// DefaultCache returns the persistent ripples cache. RIPPLES_CACHE overrides
// its directory and RIPPLES_CACHE_MAX_MB its total size.
func DefaultCache() (*Cache, error) {
	maxMB := int64(defaultMaxMB)
	if value := os.Getenv("RIPPLES_CACHE_MAX_MB"); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed <= 0 {
			return nil, fmt.Errorf("RIPPLES_CACHE_MAX_MB must be a positive number of megabytes, got %q", value)
		}
		maxMB = parsed
	}
	if dir := os.Getenv("RIPPLES_CACHE"); dir != "" {
		if !filepath.IsAbs(dir) {
			return nil, fmt.Errorf("RIPPLES_CACHE must be an absolute path")
		}
		return &Cache{Dir: dir, MaxBytes: maxMB << 20}, nil
	}

	dir, err := os.UserCacheDir()
	if err != nil {
		return nil, fmt.Errorf("resolve user cache directory: %w", err)
	}
	return &Cache{Dir: filepath.Join(dir, "ripples"), MaxBytes: maxMB << 20}, nil
}

// Key returns a stable cache key for the supplied inputs.
func Key(parts ...string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(cacheVersion))
	for _, part := range parts {
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(part))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// Load decodes a cached value. A missing entry is not an error.
func (c *Cache) Load(namespace, key string, value any) (_ bool, returnErr error) {
	file, err := os.Open(c.filename(namespace, key))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read cache entry: %w", err)
	}
	defer func() {
		returnErr = errors.Join(returnErr, file.Close())
	}()
	reader, err := gzip.NewReader(file)
	if err != nil {
		return false, fmt.Errorf("decode cache entry: %w", err)
	}
	if err := json.NewDecoder(reader).Decode(value); err != nil {
		return false, fmt.Errorf("decode cache entry: %w", err)
	}
	// The modification time records the last use for Prune. A read-only
	// cache still serves hits; it just never refreshes them.
	now := time.Now()
	_ = os.Chtimes(c.filename(namespace, key), now, now)
	return true, nil
}

// Touch refreshes an entry's last use, like Load, and reports whether the
// entry exists.
func (c *Cache) Touch(namespace, key string) bool {
	now := time.Now()
	return os.Chtimes(c.filename(namespace, key), now, now) == nil
}

// Prune removes entries that no analysis has read or written for maxAge, then
// the least recently used entries until the cache fits in MaxBytes.
func (c *Cache) Prune(maxAge time.Duration) error {
	cutoff := time.Now().Add(-maxAge)
	namespaces, err := os.ReadDir(c.Dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read cache directory: %w", err)
	}
	type entry struct {
		path string
		size int64
		used time.Time
	}
	var (
		errs  []error
		kept  []entry
		total int64
	)
	remove := func(path string) {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("remove cache entry: %w", err))
		}
	}
	for _, namespace := range namespaces {
		if !namespace.IsDir() {
			continue
		}
		dir := filepath.Join(c.Dir, namespace.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			errs = append(errs, fmt.Errorf("read cache namespace: %w", err))
			continue
		}
		for _, file := range files {
			info, err := file.Info()
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			path := filepath.Join(dir, file.Name())
			if info.ModTime().Before(cutoff) {
				remove(path)
				continue
			}
			kept = append(kept, entry{path: path, size: info.Size(), used: info.ModTime()})
			total += info.Size()
		}
	}
	// Load refreshes the modification time, so the oldest entries are the
	// least recently used ones.
	if c.MaxBytes > 0 && total > c.MaxBytes {
		slices.SortFunc(kept, func(a, b entry) int { return a.used.Compare(b.used) })
		for _, current := range kept {
			if total <= c.MaxBytes {
				break
			}
			remove(current.path)
			total -= current.size
		}
	}
	return errors.Join(errs...)
}

// Store atomically writes a gzip-compressed cached value. Snapshots repeat
// long declaration IDs, so compression shrinks them several times.
func (c *Cache) Store(namespace, key string, value any) error {
	dir := filepath.Join(c.Dir, namespace)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create cache directory: %w", err)
	}

	file, err := os.CreateTemp(dir, "entry-*")
	if err != nil {
		return fmt.Errorf("create cache entry: %w", err)
	}
	tempName := file.Name()
	defer func() {
		_ = os.Remove(tempName)
	}()

	compressed := gzip.NewWriter(file)
	if err := json.NewEncoder(compressed).Encode(value); err != nil {
		_ = file.Close()
		return fmt.Errorf("encode cache entry: %w", err)
	}
	if err := compressed.Close(); err != nil {
		_ = file.Close()
		return fmt.Errorf("write cache entry: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close cache entry: %w", err)
	}
	if err := os.Rename(tempName, c.filename(namespace, key)); err != nil {
		return fmt.Errorf("commit cache entry: %w", err)
	}
	return nil
}

func (c *Cache) filename(namespace, key string) string {
	return filepath.Join(c.Dir, namespace, key+".json.gz")
}
