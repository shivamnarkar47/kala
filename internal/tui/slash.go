// Package tui — slash.go: the slash-command surface. One metadata table
// feeds everything: the suggestion popup, tab completion, and /help.
package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// slashCommand is one entry: name, argument hint, and a human description.
type slashCommand struct {
	name string // "/help"
	args string // "<id>" — shown after the name, never auto-inserted
	desc string
}

// slashCommands drives the popup order (most-used first).
var slashCommands = []slashCommand{
	{"/help", "", "commands and keys"},
	{"/new", "", "fresh session, clear the field"},
	{"/resume", "<id>", "return to a past session"},
	{"/sessions", "", "browse recorded sessions"},
	{"/models", "", "pick a model — the astras"},
	{"/connect", "[key]", "provider picker / save a key"},
	{"/agents", "", "switch persona — the sabha"},
	{"/memory", "", "show the memory digest"},
	{"/model", "", "show the active model"},
	{"/verbose", "", "toggle reasoning display"},
	{"/diagrams", "", "toggle mermaid auto-render"},
	{"/diagram", "<file.mmd>", "render a mermaid file"},
	{"/sidebar", "", "toggle the workspace rail"},
	{"/topbar", "", "toggle the top bar"},
	{"/structure", "", "dump the structure cache"},
	{"/quit", "", "exit kaal"},
}

// slashMatches returns the commands whose name continues the typed input,
// falling back to description matches once typing is specific enough
// (typing "/sess" narrows by name; "/persona" finds /agents by description;
// single characters stay name-only so the list doesn't flood).
func slashMatches(input string) []*slashCommand {
	q := strings.ToLower(strings.TrimPrefix(input, "/"))
	var byName, byDesc []*slashCommand
	for i := range slashCommands {
		c := &slashCommands[i]
		name := strings.TrimPrefix(c.name, "/")
		switch {
		case strings.HasPrefix(name, q):
			byName = append(byName, c)
		case len(q) >= 3 && c.desc != "" &&
			strings.Contains(strings.ToLower(c.desc+" "+c.args), q):
			byDesc = append(byDesc, c)
		}
	}
	return append(byName, byDesc...)
}

// slashFind resolves an exact command by name ("/resume"), nil otherwise.
func slashFind(name string) *slashCommand {
	for i := range slashCommands {
		if slashCommands[i].name == name {
			return &slashCommands[i]
		}
	}
	return nil
}

// slashNearest suggests the closest known command for a typo: longest common
// prefix wins, ties broken by shared characters; "" when nothing resembles.
func slashNearest(typed string) string {
	typed = strings.ToLower(strings.TrimPrefix(typed, "/"))
	best, bestScore := "", 0
	for i := range slashCommands {
		name := strings.TrimPrefix(slashCommands[i].name, "/")
		score := 0
		for i := 0; i < len(typed) && i < len(name); i++ {
			if typed[i] == name[i] {
				score++
			} else {
				break
			}
		}
		// Shared-rune bonus keeps "/sessa" pointing at /sessions.
		for _, r := range typed {
			if strings.ContainsRune(name, r) {
				score++
			}
		}
		if score > bestScore {
			best, bestScore = slashCommands[i].name, score
		}
	}
	return best
}

// suggestionWindow clamps a scrolling window of maxRows around the cursor.
func suggestionWindow(n, cursor, maxRows int) (start, end int) {
	if n <= maxRows {
		return 0, n
	}
	start = cursor - maxRows/2
	if start < 0 {
		start = 0
	}
	if start+maxRows > n {
		start = n - maxRows
	}
	return start, start + maxRows
}

// renderSuggestionPopup builds the floating command palette: sigil-marked
// rows with name, argument hint, and description; the cursor row inverted.
func (m *Model) renderSuggestionPopup(maxWidth int) string {
	const maxRows = 8
	n := len(m.suggestions)
	start, end := suggestionWindow(n, m.suggestIndex, maxRows)

	nameW, argsW := 0, 0
	for _, c := range m.suggestions {
		if len(c.name) > nameW {
			nameW = len(c.name)
		}
		if len(c.args) > argsW {
			argsW = len(c.args)
		}
	}

	rowStyle := func(selected bool) lipgloss.Style {
		st := lipgloss.NewStyle()
		if selected {
			st = st.Background(colorPeacock).Foreground(lipgloss.Color("16"))
		}
		return st
	}

	var rows []string
	for i := start; i < end; i++ {
		c := m.suggestions[i]
		marker, mark := "  ", ""
		if i == m.suggestIndex {
			marker, mark = "▸ ", "✶ "
		}
		name := c.name + strings.Repeat(" ", nameW-len(c.name))
		args := c.args + strings.Repeat(" ", argsW-len(c.args))
		line := mark + name + " " + args + "  " + c.desc
		width := maxWidth
		if width < 20 {
			width = 20
		}
		rendered := rowStyle(i == m.suggestIndex).
			MaxWidth(width - lipgloss.Width(marker)).
			Render(line)
		rows = append(rows, rowStyle(i == m.suggestIndex).Render(marker)+rendered)
	}
	if end < n {
		rows = append(rows, m.dimStyle.Render(
			fmt.Sprintf("  … +%d more (type to narrow)", n-end)))
	}
	if n == 0 && !m.suggestDismissed {
		rows = append(rows, m.dimStyle.Render("  no matching command"))
	}

	title := "commands"
	body := strings.Join(rows, "\n")
	if body == "" {
		return ""
	}
	return lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(colorSaffron).
		Padding(0, 1).
		MaxWidth(maxWidth).
		Render(title + "\n" + body)
}

// overlayBottom floats a block over the lower rows of a rendered pane —
// the popup must never shove the layout around.
func overlayBottom(base, block string) string {
	if block == "" {
		return base
	}
	baseLines := strings.Split(base, "\n")
	blockLines := strings.Split(block, "\n")
	start := len(baseLines) - len(blockLines)
	if start < 0 {
		start = 0
	}
	copy(baseLines[start:], blockLines)
	return strings.Join(baseLines, "\n")
}

// renderHelpPanel is the /help output: an aligned command table plus the key
// bindings, themed.
func (m *Model) renderHelpPanel() string {
	nameW, argsW := 0, 0
	for _, c := range slashCommands {
		if len(c.name) > nameW {
			nameW = len(c.name)
		}
		if len(c.args) > argsW {
			argsW = len(c.args)
		}
	}
	nameSt := lipgloss.NewStyle().Foreground(colorSaffron).Bold(true)
	argSt := lipgloss.NewStyle().Foreground(colorGold)
	descSt := lipgloss.NewStyle().Foreground(colorIvory)

	var b strings.Builder
	b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(colorGold).Render("Commands"))
	b.WriteString("\n")
	for _, c := range slashCommands {
		b.WriteString("  " +
			nameSt.Render(c.name+strings.Repeat(" ", nameW-len(c.name))) + " " +
			argSt.Render(c.args+strings.Repeat(" ", argsW-len(c.args))) + "  " +
			descSt.Render(c.desc))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(colorGold).Render("Keys"))
	b.WriteString("\n")
	b.WriteString(m.dimStyle.Render(
		"  enter send · shift+enter newline · ctrl+p/n prompt history\n" +
			"  wheel/pgup·pgdn scroll chat (pins at bottom, releases on scroll-up)\n" +
			"  ↑↓/tab pick & complete a command · esc dismiss the popup\n" +
			"  ctrl+s sidebar · ctrl+t topbar · ctrl+d diagrams · ctrl+l bottom\n" +
			"  ctrl+g invent an agent · ctrl+c cancel turn · ctrl+q quit"))
	return b.String()
}
