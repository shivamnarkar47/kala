// Package toolcache ports harness/toolcache.py: the read-only tool-result
// cache — a small JSON file keyed by (tool, args, sig). Keyed by
// tool|sha256(args_json)|structure_signature, so a changed tree auto-misses.
// Missing or corrupt files degrade to an empty cache, never a crash.
package toolcache

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// ToolCache is the persistent tool-result cache under
// `.kaal/tool-cache.json`.
type ToolCache struct {
	path     string
	maxBytes int64
	mu       sync.Mutex
	data     map[string]string // lazy: loaded on first access
}

func NewToolCache(path string) *ToolCache {
	return &ToolCache{path: path, maxBytes: 4_000_000}
}

// NewToolCacheWithLimit builds a cache with an explicit size cap (tests).
func NewToolCacheWithLimit(path string, maxBytes int64) *ToolCache {
	return &ToolCache{path: path, maxBytes: maxBytes}
}

func (c *ToolCache) key(tool, argsJSON, signature string) string {
	digest := sha256.Sum256([]byte(argsJSON))
	return fmt.Sprintf("%s|%x|%s", tool, digest, signature)
}

// Get returns the cached result string, or "" on a miss (an empty result is
// still a valid cache payload — callers distinguish via ok).
func (c *ToolCache) Get(tool, argsJSON, signature string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.load()[c.key(tool, argsJSON, signature)]
	return v, ok
}

// Put stores a result string; skips the disk write when the file would
// exceed maxBytes (the in-memory copy still serves this process). Disk
// writes are best-effort: any OSError is swallowed so a cache failure never
// breaks a tool call.
func (c *ToolCache) Put(tool, argsJSON, signature, result string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	data := c.load()
	data[c.key(tool, argsJSON, signature)] = result
	_ = c.write(data)
}

// Drop deletes the cache file (and forgets the in-memory copy).
func (c *ToolCache) Drop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data = nil
	_ = os.Remove(c.path)
}

// load lazily loads the JSON file; missing/corrupt -> empty cache.
func (c *ToolCache) load() map[string]string {
	if c.data != nil {
		return c.data
	}
	data := map[string]string{}
	if raw, err := os.ReadFile(c.path); err == nil {
		var parsed map[string]any
		if err := json.Unmarshal(raw, &parsed); err == nil {
			for k, v := range parsed {
				if s, ok := v.(string); ok {
					data[k] = s
				}
			}
		}
	}
	c.data = data
	return data
}

// write persists the payload atomically with a unique temp name (concurrent
// --batch workers share this cache file and must never collide on the same
// .tmp path).
func (c *ToolCache) write(data map[string]string) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if int64(len(payload)) > c.maxBytes {
		return nil // cap hit: never grow the file unbounded
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp%d", c.path, os.Getpid())
	if err := os.WriteFile(tmp, payload, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}
