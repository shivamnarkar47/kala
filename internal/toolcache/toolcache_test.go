// Ported from tests/test_toolcache.py (114 lines): keying, persistence,
// atomic writes, corruption handling.
package toolcache_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kaal/kaal/internal/toolcache"
)

func cachePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), ".kaal", "tool-cache.json")
}

func TestPutGetRoundTrip(t *testing.T) {
	path := cachePath(t)
	cache := toolcache.NewToolCache(path)
	cache.Put("read", `{"path": "a.txt"}`, "sig1", "contents of a")
	got, ok := cache.Get("read", `{"path": "a.txt"}`, "sig1")
	if !ok || got != "contents of a" {
		t.Fatalf("got %q ok=%v", got, ok)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("cache file missing")
	}
}

func TestDifferentSignatureMisses(t *testing.T) {
	cache := toolcache.NewToolCache(cachePath(t))
	cache.Put("read", `{"path": "a.txt"}`, "sig1", "stale")
	if _, ok := cache.Get("read", `{"path": "a.txt"}`, "sig2"); ok {
		t.Fatal("changed tree must miss")
	}
}

func TestDifferentArgsMisses(t *testing.T) {
	cache := toolcache.NewToolCache(cachePath(t))
	cache.Put("read", `{"path": "a.txt"}`, "sig1", "stale")
	if _, ok := cache.Get("read", `{"path": "b.txt"}`, "sig1"); ok {
		t.Fatal("different args must miss")
	}
}

func TestDifferentToolMisses(t *testing.T) {
	cache := toolcache.NewToolCache(cachePath(t))
	cache.Put("read", `{"path": "a.txt"}`, "sig1", "stale")
	if _, ok := cache.Get("grep", `{"path": "a.txt"}`, "sig1"); ok {
		t.Fatal("different tool must miss")
	}
}

func TestMissingFileReturnsNone(t *testing.T) {
	cache := toolcache.NewToolCache(cachePath(t))
	if _, ok := cache.Get("read", `{"path": "a.txt"}`, "sig1"); ok {
		t.Fatal("missing file must miss")
	}
}

func TestDropClears(t *testing.T) {
	path := cachePath(t)
	cache := toolcache.NewToolCache(path)
	cache.Put("read", `{"path": "a.txt"}`, "sig1", "x")
	if _, err := os.Stat(path); err != nil {
		t.Fatal("file missing")
	}
	cache.Drop()
	if _, err := os.Stat(path); err == nil {
		t.Fatal("file must be gone after drop")
	}
	// The in-memory copy is forgotten too: a later get misses.
	if _, ok := cache.Get("read", `{"path": "a.txt"}`, "sig1"); ok {
		t.Fatal("drop must forget the in-memory copy")
	}
}

func TestOversizedPutDoesNotGrowFile(t *testing.T) {
	path := cachePath(t)
	cache := toolcache.NewToolCacheWithLimit(path, 100)
	cache.Put("read", `{"path": "a.txt"}`, "sig1", "small")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	sizeBefore := info.Size()
	cache.Put("read", `{"path": "b.txt"}`, "sig1", strings.Repeat("x", 500))
	info, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != sizeBefore {
		t.Fatalf("file grew: %d -> %d", sizeBefore, info.Size())
	}
}

func TestCorruptFileTolerated(t *testing.T) {
	path := cachePath(t)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, []byte("{not valid json"), 0o644)
	cache := toolcache.NewToolCache(path)
	if _, ok := cache.Get("read", `{"path": "a.txt"}`, "sig1"); ok {
		t.Fatal("corrupt file must miss")
	}
	// A put after corruption rewrites a clean, loadable file.
	cache.Put("read", `{"path": "a.txt"}`, "sig1", "fresh")
	got, ok := cache.Get("read", `{"path": "a.txt"}`, "sig1")
	if !ok || got != "fresh" {
		t.Fatalf("post-corruption put: %q ok=%v", got, ok)
	}
}

func TestNonDictPayloadTolerated(t *testing.T) {
	path := cachePath(t)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, []byte("[1, 2, 3]"), 0o644)
	cache := toolcache.NewToolCache(path)
	if _, ok := cache.Get("read", `{"path": "a.txt"}`, "sig1"); ok {
		t.Fatal("non-dict payload must miss")
	}
}

func TestAtomicWriteLeavesNoTmp(t *testing.T) {
	path := cachePath(t)
	cache := toolcache.NewToolCache(path)
	cache.Put("read", `{"path": "a.txt"}`, "sig1", "x")
	cache.Put("read", `{"path": "b.txt"}`, "sig1", "y")
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.Contains(e.Name(), "tmp") {
			t.Fatalf("leftover tmp file: %s", e.Name())
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("cache file missing")
	}
}

func TestPersistsAcrossInstances(t *testing.T) {
	path := cachePath(t)
	toolcache.NewToolCache(path).Put("read", `{"path": "a.txt"}`, "sig1", "persisted")
	again := toolcache.NewToolCache(path)
	got, ok := again.Get("read", `{"path": "a.txt"}`, "sig1")
	if !ok || got != "persisted" {
		t.Fatalf("got %q ok=%v", got, ok)
	}
}

func TestStoredJSONShape(t *testing.T) {
	path := cachePath(t)
	cache := toolcache.NewToolCache(path)
	cache.Put("read", `{"path": "a.txt"}`, "sig1", "x")
	raw, _ := os.ReadFile(path)
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed) != 1 {
		t.Fatalf("entries: %d", len(parsed))
	}
	var key string
	for k := range parsed {
		key = k
	}
	// tool|sha256(args_json)|signature — the args digest keeps the key short
	// while the signature in the key makes changed trees miss.
	parts := strings.Split(key, "|")
	if len(parts) != 3 {
		t.Fatalf("key: %q", key)
	}
	if parts[0] != "read" {
		t.Fatalf("tool: %q", parts[0])
	}
	if len(parts[1]) != 64 {
		t.Fatalf("digest len: %d", len(parts[1]))
	}
	if parts[2] != "sig1" {
		t.Fatalf("sig: %q", parts[2])
	}
}
