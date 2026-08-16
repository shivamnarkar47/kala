// Package structure ports harness/structure.py: project-structure awareness
// via a regenerable `.kaal/STRUCTURE.md` cache, signature-keyed so refreshes
// only happen when the tree changes.
package structure

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// NoiseDirs are skipped at any depth (also keeps the cache from invalidating
// itself: `.kaal` holds STRUCTURE.md, whose own mtime must never enter the sig).
var NoiseDirs = map[string]bool{
	".git": true, "__pycache__": true, ".venv": true, "node_modules": true,
	".kaal": true, "dist": true, "build": true, ".omp": true,
	".mypy_cache": true, ".pytest_cache": true, ".ruff_cache": true,
}

const (
	maxDepth    = 6
	docMaxLines = 500

	sigPrefix = "<!-- sig: "
)

// maxEntries is a var (not const) so tests can lower the cap.
var maxEntries = 20_000

// Entry is one walked entry: relpath (slash form), is-dir, size, mtime_ns.
type Entry struct {
	Rel     string
	IsDir   bool
	Size    int64
	MtimeNS int64
}

// StructureManager scans + caches a project's tree under
// `.kaal/STRUCTURE.md`.
type StructureManager struct {
	Root          string
	LastSignature string
}

func NewStructureManager(root string) *StructureManager {
	abs, _ := filepath.Abs(root)
	return &StructureManager{Root: abs}
}

// CachePath is the cache document path.
func (s *StructureManager) CachePath() string {
	return filepath.Join(s.Root, ".kaal", "STRUCTURE.md")
}

// Ensure returns the cached doc; scans + writes on first use (no rescan).
func (s *StructureManager) Ensure() string {
	path := s.CachePath()
	if raw, err := os.ReadFile(path); err == nil {
		// Warm start: recover the signature from the cached doc so the tool
		// cache can serve hits from the very first batch.
		s.LastSignature = s.storedSignature()
		return string(raw)
	}
	return s.Refresh()
}

// Refresh runs the cheap signature scan; regenerates only when the tree
// changed.
func (s *StructureManager) Refresh() string {
	sig := s.Signature()
	s.LastSignature = sig
	if sig == s.storedSignature() {
		if raw, err := os.ReadFile(s.CachePath()); err == nil {
			return string(raw)
		}
	}
	doc := s.Scan()
	s.write(doc)
	return doc
}

// Digest returns a head-capped excerpt for the system prompt (includes the
// path); a cut is marked with a trailing notice.
func (s *StructureManager) Digest(maxChars int) string {
	text := s.Ensure()
	if utf8.RuneCountInString(text) <= maxChars {
		return text
	}
	return truncateRunes(text, maxChars) + "\n… (truncated)"
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	out := make([]rune, 0, n)
	for i, r := range s {
		if i >= n {
			break
		}
		out = append(out, r)
	}
	return string(out)
}

// Scan builds the markdown document (tree + counts + signature comment).
func (s *StructureManager) Scan() string {
	entries := ordered(walk(s.Root))
	files, dirs := 0, 0
	for _, e := range entries {
		if e.IsDir {
			dirs++
		} else {
			files++
		}
	}
	lines := []string{
		"# Project Structure",
		"Generated: " + time.Now().Format("2006-01-02T15:04:05"),
		"Root: " + s.Root,
		fmt.Sprintf("Files: %d · Dirs: %d", files, dirs),
		"## Tree",
	}
	lines = append(lines, treeLines(entries)...)
	if len(entries) >= maxEntries {
		lines = append(lines, "… (structure truncated: entry cap reached)")
	}
	if len(lines) > docMaxLines {
		lines = lines[:docMaxLines]
		lines = append(lines, "… (structure truncated)")
	}
	lines = append(lines, sigPrefix+signatureFrom(entries)+"-->")
	return strings.Join(lines, "\n")
}

// walk yields (relpath, is_dir, size, mtime_ns) for non-noise entries,
// honoring the caps. Dirs first (sorted, case-insensitive) then files, in
// DFS order; a global entry cap stops the walk; dirs at the depth cap are
// yielded but not descended into.
func walk(root string) []Entry {
	var entries []Entry
	count := 0
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if path == root {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		depth := len(strings.Split(rel, "/"))
		if d.IsDir() {
			if NoiseDirs[d.Name()] {
				return filepath.SkipDir
			}
			if count >= maxEntries {
				return filepath.SkipAll
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			count++
			entries = append(entries, Entry{Rel: rel, IsDir: true, Size: info.Size(), MtimeNS: info.ModTime().UnixNano()})
			if depth >= maxDepth {
				return filepath.SkipDir // too deep: don't descend
			}
			return nil
		}
		if count >= maxEntries {
			return filepath.SkipAll
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		count++
		entries = append(entries, Entry{Rel: rel, IsDir: false, Size: info.Size(), MtimeNS: info.ModTime().UnixNano()})
		return nil
	})
	// Files and dirs must each be sorted case-insensitively within their
	// levels. WalkDir yields lexical order already, but the Python walk
	// sorts dirnames and filenames separately, case-insensitively; a plain
	// case-sensitive lexical sort can interleave differently. The preorder
	// sort below (dirs before files at each level) plus a case-insensitive
	// name sort reproduces the Python ordering.
	sort.SliceStable(entries, func(i, j int) bool {
		return strings.ToLower(entries[i].Rel) < strings.ToLower(entries[j].Rel)
	})
	return entries
}

// ordered applies the preorder sort: dirs before files at each level,
// DFS-expanded.
func ordered(entries []Entry) []Entry {
	key := func(e Entry) string {
		parts := strings.Split(e.Rel, "/")
		var sb strings.Builder
		for _, p := range parts[:len(parts)-1] {
			sb.WriteString("d/")
			sb.WriteString(strings.ToLower(p))
			sb.WriteString("/")
		}
		if e.IsDir {
			sb.WriteString("d/")
		} else {
			sb.WriteString("f/")
		}
		sb.WriteString(strings.ToLower(parts[len(parts)-1]))
		return sb.String()
	}
	sorted := make([]Entry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool { return key(sorted[i]) < key(sorted[j]) })
	return sorted
}

// Signature returns the stable hash over (relpath, size, mtime_ns) of
// non-noise entries.
func (s *StructureManager) Signature() string {
	return signatureFrom(ordered(walk(s.Root)))
}

// signatureFrom hashes (relpath, size, mtime_ns); relpath sorted
// case-insensitively.
func signatureFrom(entries []Entry) string {
	sorted := make([]Entry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		return strings.ToLower(sorted[i].Rel) < strings.ToLower(sorted[j].Rel)
	})
	h := sha256.New()
	for _, e := range sorted {
		fmt.Fprintf(h, "%s\000%d\000%d\n", e.Rel, e.Size, e.MtimeNS)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// storedSignature recovers the signature comment from the cached doc.
func (s *StructureManager) storedSignature() string {
	raw, err := os.ReadFile(s.CachePath())
	if err != nil {
		return ""
	}
	text := string(raw)
	idx := strings.LastIndex(text, sigPrefix)
	if idx < 0 {
		return ""
	}
	end := strings.Index(text[idx:], "-->")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(text[idx+len(sigPrefix) : idx+end])
}

// treeLines renders the box-drawing tree: ├──/└──/│, dirs first.
func treeLines(entries []Entry) []string {
	// lastFlags[prefix] = whether the entry at that prefix path is the last
	// among its siblings.
	lastFlags := map[string]bool{}
	for i, e := range entries {
		parts := strings.Split(e.Rel, "/")
		nxt := ""
		if i+1 < len(entries) {
			nxt = entries[i+1].Rel
		}
		parent := strings.Join(parts[:len(parts)-1], "/")
		isLast := nxt == "" || !strings.HasPrefix(nxt, parent+"/")
		lastFlags[e.Rel] = isLast
	}
	var lines []string
	for _, e := range entries {
		parts := strings.Split(e.Rel, "/")
		var prefix strings.Builder
		for k := 1; k < len(parts); k++ {
			if lastFlags[strings.Join(parts[:k], "/")] {
				prefix.WriteString("    ")
			} else {
				prefix.WriteString("│   ")
			}
		}
		branch := "├── "
		if lastFlags[e.Rel] {
			branch = "└── "
		}
		name := parts[len(parts)-1]
		if e.IsDir {
			lines = append(lines, prefix.String()+branch+name+"/")
			if len(parts) >= maxDepth {
				below := "    "
				if !lastFlags[e.Rel] {
					below = "│   "
				}
				lines = append(lines, prefix.String()+below+"…")
			}
		} else {
			lines = append(lines, prefix.String()+branch+name+" ("+humanSize(e.Size)+")")
		}
	}
	return lines
}

// humanSize renders '74 B', '1.2 KB', '3.4 MB' (Python's %.1f).
func humanSize(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	f := float64(n)
	for _, unit := range []string{"KB", "MB", "GB"} {
		f /= 1024
		if f < 1024 {
			return fmt.Sprintf("%.1f %s", f, unit)
		}
	}
	return fmt.Sprintf("%.1f TB", f)
}

// write persists the doc atomically (temp + rename).
func (s *StructureManager) write(doc string) {
	path := s.CachePath()
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	tmp := path + ".tmp"
	_ = os.WriteFile(tmp, []byte(doc), 0o644)
	_ = os.Rename(tmp, path)
}
