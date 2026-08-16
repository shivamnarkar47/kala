// Package memory ports harness/memory.py: persistent project memory under
// .agent-memory/.
package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/kaal/kaal/internal/context"
)

// Section is one memory file: key -> display name. Order is the digest order.
var sections = []Section{
	{Key: "project-state", Display: "Project State"},
	{Key: "decisions", Display: "Decisions"},
	{Key: "patterns", Display: "Patterns"},
	{Key: "lessons-learned", Display: "Lessons Learned"},
}

type Section struct {
	Key     string
	Display string
}

const (
	digestTokenCap  = 4000
	digestMaxLines  = 60
	maxLinesPerFile = 200
)

// Memory is the persistent project memory under a root directory
// (`.agent-memory/`): four markdown files, each created on demand with a
// `# <Name>` heading.
type Memory struct {
	Root string
	mu   sync.Mutex // serializes append sequences within one process
}

func NewMemory(root string) *Memory {
	m := &Memory{Root: root}
	_ = os.MkdirAll(root, 0o755)
	for _, s := range sections {
		path := filepath.Join(root, s.Key+".md")
		if _, err := os.Stat(path); err != nil {
			_ = os.WriteFile(path, []byte("# "+s.Display+"\n\n"), 0o644)
		}
	}
	return m
}

// FilePath returns the absolute path of a section file.
func (m *Memory) FilePath(section string) (string, error) {
	if err := checkSection(section); err != nil {
		return "", err
	}
	abs, _ := filepath.Abs(filepath.Join(m.Root, section+".md"))
	return abs, nil
}

// LoadDigest returns the bulleted digest of all four files, capped at 4000
// estimated tokens. Each section contributes a `### <Name>` heading, the
// file's absolute path, and the first 60 lines of content; content lines are
// dropped from the end until the cap holds, with a final `…[truncated]`
// marker noting the cut. File paths are always kept.
func (m *Memory) LoadDigest() string {
	var lines []string
	var contentIndexes []int
	for _, s := range sections {
		path := filepath.Join(m.Root, s.Key+".md")
		abs, _ := filepath.Abs(path)
		lines = append(lines, "### "+s.Display)
		lines = append(lines, "path: "+abs)
		raw, err := os.ReadFile(path)
		if err != nil {
			raw = []byte{}
		}
		content := strings.Split(string(raw), "\n")
		if len(content) > digestMaxLines {
			content = content[:digestMaxLines]
		}
		for len(content) > 0 && strings.TrimSpace(content[len(content)-1]) == "" {
			content = content[:len(content)-1] // trim trailing whitespace-only lines
		}
		for _, line := range content {
			contentIndexes = append(contentIndexes, len(lines))
			lines = append(lines, line)
		}
	}

	cut := false
	for len(contentIndexes) > 0 && context.EstimateTokens(strings.Join(lines, "\n")) > digestTokenCap {
		lines = lines[:contentIndexes[len(contentIndexes)-1]]
		contentIndexes = contentIndexes[:len(contentIndexes)-1]
		cut = true
	}
	// Make room for the marker so the cap holds after appending it.
	for cut && len(contentIndexes) > 0 &&
		context.EstimateTokens(strings.Join(lines, "\n")+"\n…[truncated]") > digestTokenCap {
		lines = lines[:contentIndexes[len(contentIndexes)-1]]
		contentIndexes = contentIndexes[:len(contentIndexes)-1]
	}
	if cut {
		lines = append(lines, "…[truncated]")
	}
	return strings.Join(lines, "\n")
}

// Append records text under a timestamped `##` heading; dedupes verbatim.
// Returns "already recorded" when text already appears in the file, otherwise
// the absolute file path after enforcing the 200-line cap (the oldest `##`
// section is dropped until the file fits).
func (m *Memory) Append(section, text string) (string, error) {
	if err := checkSection(section); err != nil {
		return "", err
	}
	path, err := m.FilePath(section)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	content, err := os.ReadFile(path)
	if err != nil {
		content = []byte{}
	}
	if strings.Contains(string(content), text) {
		return "already recorded", nil
	}
	timestamp := time.Now().Format("2006-01-02 15:04")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString("\n## " + timestamp + "\n" + text + "\n"); err != nil {
		f.Close()
		return "", err
	}
	f.Close()
	prune(path)
	return path, nil
}

// RecordSessionSummary records a one-line session summary in project-state.md.
func (m *Memory) RecordSessionSummary(task, outcome string) {
	_, _ = m.Append("project-state", "session: "+task+" → "+outcome)
}

// prune drops the oldest `##` section repeatedly until the file is ≤ 200
// lines.
func prune(path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := strings.SplitAfter(string(raw), "\n")
	for len(lines) > maxLinesPerFile {
		start := -1
		for i, ln := range lines {
			if strings.HasPrefix(ln, "## ") {
				start = i
				break
			}
		}
		if start == -1 {
			break // nothing to prune (e.g. only the header, somehow huge)
		}
		end := len(lines)
		for i := start + 1; i < len(lines); i++ {
			if strings.HasPrefix(lines[i], "## ") {
				end = i
				break
			}
		}
		lines = append(lines[:start], lines[end:]...)
	}
	_ = os.WriteFile(path, []byte(strings.Join(lines, "")), 0o644)
}

func checkSection(section string) error {
	for _, s := range sections {
		if s.Key == section {
			return nil
		}
	}
	return fmt.Errorf("invalid section: %q; allowed: decisions, lessons-learned, patterns, project-state", section)
}

// RuneLen is a tiny helper kept out of the way; Go's utf8 counts runes.
func RuneLen(s string) int { return utf8.RuneCountInString(s) }
