package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const cacheVersion = "v1"

// Cache stores content-addressed JSON analysis artifacts.
type Cache struct {
	Dir string
}

// DefaultCache returns the persistent ripples cache.
func DefaultCache() (*Cache, error) {
	if dir := os.Getenv("RIPPLES_CACHE"); dir != "" {
		if !filepath.IsAbs(dir) {
			return nil, fmt.Errorf("RIPPLES_CACHE must be an absolute path")
		}
		return &Cache{Dir: dir}, nil
	}

	dir, err := os.UserCacheDir()
	if err != nil {
		return nil, fmt.Errorf("resolve user cache directory: %w", err)
	}
	return &Cache{Dir: filepath.Join(dir, "ripples")}, nil
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
func (c *Cache) Load(namespace, key string, value any) (bool, error) {
	data, err := os.ReadFile(c.filename(namespace, key))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read cache entry: %w", err)
	}
	if err := json.Unmarshal(data, value); err != nil {
		return false, fmt.Errorf("decode cache entry: %w", err)
	}
	return true, nil
}

// Store atomically writes a cached value.
func (c *Cache) Store(namespace, key string, value any) error {
	dir := filepath.Join(c.Dir, namespace)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create cache directory: %w", err)
	}

	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode cache entry: %w", err)
	}

	file, err := os.CreateTemp(dir, "entry-*")
	if err != nil {
		return fmt.Errorf("create cache entry: %w", err)
	}
	tempName := file.Name()
	defer os.Remove(tempName)

	if _, err := file.Write(data); err != nil {
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
	return filepath.Join(c.Dir, namespace, key+".json")
}
