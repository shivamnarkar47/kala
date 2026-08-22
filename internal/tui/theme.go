// Package tui — theme.go: the Kurukshetra skin. Palette, per-Pandava
// identities (glyph + epithet + color), the chariot hero, the Gita verse,
// and the Sudarshana spinner. Cosmetic only: every functional string the
// tests assert lives untouched in tui.go.
package tui

import "github.com/charmbracelet/lipgloss"

// The Mahabharata palette: saffron and gold for dharma, peacock blue for
// Krishna, ember and blood for the field of war, ivory for parchment.
var (
	colorSaffron = lipgloss.Color("214")
	colorGold    = lipgloss.Color("220")
	colorPeacock = lipgloss.Color("69")
	colorEmber   = lipgloss.Color("203")
	colorIvory   = lipgloss.Color("223")
	colorBlood   = lipgloss.Color("196")
	colorLeaf    = lipgloss.Color("79")
	colorViolet  = lipgloss.Color("141")
	colorDim     = lipgloss.Color("245")
)

// pandavaIdentity dresses one agent: a weapon/sigil glyph, the epic byname,
// and the banner color. Custom agents fall back to the saffron default.
type pandavaIdentity struct {
	glyph   string
	epithet string
	color   lipgloss.Color
}

// pandavaIdentities maps the five Pandavas to their regalia.
var pandavaIdentities = map[string]pandavaIdentity{
	"Yudhishthira": {glyph: "☸", epithet: "dharmaraja", color: colorGold},
	"Bhima":        {glyph: "⚒", epithet: "vrikodara", color: colorBlood},
	"Arjuna":       {glyph: "➶", epithet: "savyasachi", color: colorPeacock},
	"Nakula":       {glyph: "⚔", epithet: "ashvakovida", color: colorLeaf},
	"Sahadeva":     {glyph: "✦", epithet: "daivajna", color: colorViolet},
}

const (
	defaultGlyph = "✶"
	defaultEpi   = "kshatriya"
)

// identityFor resolves an agent's regalia; unknown names (Karna, Ekalavya,
// whatever ctrl+g invents) march under plain saffron.
func identityFor(name string) pandavaIdentity {
	if id, ok := pandavaIdentities[name]; ok {
		return id
	}
	return pandavaIdentity{glyph: defaultGlyph, epithet: defaultEpi, color: colorSaffron}
}

// The Gandiva's answer to the spinner: Sudarshana turning.
var chakraFrames = []string{"◐", "◓", "◑", "◒"}

// The verse kaal works by — BG 2.47, transliterated for the terminal.
const (
	gitaVerse = `karmany evadhikaras te ma phalesu kadacana`
	gitaGloss = `your grip is on the act alone, never on its fruit · Bhagavad Gita 2.47`
)

// Arjuna's chariot: Krishna at the reins, Gandiva answering over the rail,
// the pennant of dharma on the pole. Box-drawn like the old chakra; every
// rune single-width so the wheels land where they must.
var chariotArt = `
            ➶➶➶
           ╱
    ✦      ╱
    │     ╱
  ╔═╧═════╧═════╗
  ║             ║
  ╚═╦═════════╦═╝
   ╔╩╗       ╔╩╗
   ╚═╝       ╚═╝`
