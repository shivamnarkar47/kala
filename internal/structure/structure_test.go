// Ported from tests/test_structure.py (111 lines): cache creation, signature
// refresh, caps (temp dirs). White-box package so the entry cap can be
// lowered.
package structure

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func populate(root string) {
	_ = os.Mkdir(filepath.Join(root, "src"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "src", "main.py"), []byte(strings.Repeat("print('hi')\n", 30)), 0o644)
	_ = os.WriteFile(filepath.Join(root, "src", "util.py"), []byte("x = 1\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "README.md"), []byte("# readme\n"), 0o644)
	for _, noise := range []string{"node_modules", ".venv", "__pycache__", ".git"} {
		_ = os.Mkdir(filepath.Join(root, noise), 0o755)
		_ = os.WriteFile(filepath.Join(root, noise, "junk.txt"), []byte("junk\n"), 0o644)
	}
}

func TestFirstScanCreatesCache(t *testing.T) {
	root := t.TempDir()
	populate(root)
	mgr := NewStructureManager(root)
	doc := mgr.Ensure()
	raw, err := os.ReadFile(mgr.CachePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != doc {
		t.Fatal("cache file must equal the returned doc")
	}
	for _, want := range []string{"# Project Structure", "Root: ", "Files: 3 · Dirs: 1", "<!-- sig: "} {
		if !strings.Contains(doc, want) {
			t.Fatalf("missing %q in doc", want)
		}
	}
	for _, noise := range []string{"node_modules", ".venv", "__pycache__", ".git", "junk"} {
		if strings.Contains(doc, noise) {
			t.Fatalf("noise %q leaked into doc", noise)
		}
	}
	if !strings.Contains(doc, "main.py") || !strings.Contains(doc, "README.md") {
		t.Fatalf("entries missing from doc")
	}
}

func TestEnsureNoRescanUntilChange(t *testing.T) {
	root := t.TempDir()
	populate(root)
	mgr := NewStructureManager(root)
	doc1 := mgr.Ensure()
	// Mutate a file's content AND mtime: shape is unchanged.
	p := filepath.Join(root, "src", "main.py")
	_ = os.WriteFile(p, []byte("changed content that is longer\n"), 0o644)
	st, _ := os.Stat(p)
	_ = os.Chtimes(p, st.ModTime(), st.ModTime().Add(time.Second))
	// ensure() reads the cache — no rescan.
	if got := mgr.Ensure(); got != doc1 {
		t.Fatal("ensure() must not rescan")
	}
	// refresh() sees the new signature and regenerates (new size shown).
	doc2 := mgr.Refresh()
	if doc2 == doc1 {
		t.Fatal("refresh() must regenerate on change")
	}
	if !strings.Contains(doc2, "main.py (31 B)") {
		t.Fatalf("new size missing from doc2")
	}
}

func TestTreeChangeDetection(t *testing.T) {
	root := t.TempDir()
	populate(root)
	mgr := NewStructureManager(root)
	mgr.Ensure()
	_ = os.WriteFile(filepath.Join(root, "newfile.txt"), []byte("n\n"), 0o644)
	doc := mgr.Refresh()
	if !strings.Contains(doc, "newfile.txt") {
		t.Fatalf("newfile missing")
	}
	_ = os.RemoveAll(filepath.Join(root, "src"))
	doc = mgr.Refresh()
	if strings.Contains(doc, "src") {
		t.Fatalf("src still present")
	}
	if !strings.Contains(doc, "Files: 2 · Dirs: 0") { // README.md + newfile.txt
		t.Fatalf("counts wrong")
	}
}

func TestDigestCappedAndContainsRoot(t *testing.T) {
	root := t.TempDir()
	populate(root)
	mgr := NewStructureManager(root)
	mgr.Ensure()
	dig := mgr.Digest(4000)
	if len(dig) > 4000+len("\n… (truncated)") {
		t.Fatalf("digest too long: %d", len(dig))
	}
	if !strings.Contains(dig, root) {
		t.Fatal("digest must contain the root path")
	}
	small := mgr.Digest(60)
	if len(small) > 60+len("\n… (truncated)") {
		t.Fatalf("small digest too long: %d", len(small))
	}
}

func TestDepthCapNotesEllipsis(t *testing.T) {
	root := t.TempDir()
	d := root
	for i := 0; i < 8; i++ {
		d = filepath.Join(d, "level"+itoa(i))
		_ = os.Mkdir(d, 0o755)
	}
	_ = os.WriteFile(filepath.Join(d, "deep.txt"), []byte("x\n"), 0o644)
	doc := NewStructureManager(root).Ensure()
	if !strings.Contains(doc, "level5/") { // depth cap: deepest dir shown
		t.Fatalf("deepest dir missing")
	}
	if strings.Contains(doc, "deep.txt") { // its children cut
		t.Fatalf("child beyond cap present")
	}
	if !strings.Contains(doc, "…") {
		t.Fatal("ellipsis marker missing")
	}
}

func TestEntryCapNotesTruncation(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 20; i++ {
		_ = os.WriteFile(filepath.Join(root, "f"+itoa(i)+".txt"), []byte("x\n"), 0o644)
	}
	old := maxEntries
	maxEntries = 10
	defer func() { maxEntries = old }()
	doc := NewStructureManager(root).Ensure()
	if !strings.Contains(doc, "structure truncated") {
		t.Fatal("truncation notice missing")
	}
	if n := strings.Count(doc, "f"); n >= 20 {
		t.Fatalf("too many entries rendered: %d", n)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
