package mcpserver

import (
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/MrDentalStone/Nemesis/internal/cards"
)

// Heavy hitters: your spells big enough that the players they hit get a
// decision pad on their mat (Yes / No / Wait with a countdown) instead of
// the quiet cast window. HEAVY_HITTERS.md names cards by section; the
// patterns below catch the rest of the board wipes and the huge creatures.

var (
	// board wipes: everything (or every creature) destroyed, exiled,
	// shrunk, burned or bounced at once
	reWipe = regexp.MustCompile(`(?i)\b(destroy|exile)s? (all|each) (other )?(nonland |nontoken |non-\w+ |attacking |tapped |untapped )*(creatures?|permanents?|artifacts?|enchantments?|planeswalkers?|lands?)\b` +
		`|deals? (\d+|x|that much) damage to each (other )?creature` +
		`|\b(all|each) (other )?creatures? gets? -` +
		`|return (all|each) [^.]*to (its|their) owners?'? hands?` +
		`|\bsacrifices? all\b|gain control of all\b|take an extra turn`)
	rePower = regexp.MustCompile(`^(\d+)/`)
)

// heavyCreatureMV and heavyCreaturePower: a creature this big is a heavy
// hitter by itself (Godsire is 8 mana, Apex Devastator 10).
const (
	heavyCreatureMV    = 8
	heavyCreaturePower = 8
)

var heavyList = struct {
	sync.Once
	names map[string]string // lower-case name -> its section
}{}

// loadHeavy reads HEAVY_HITTERS.md: "## Section" headings, then one card
// per "- Name" line (anything after " -- " or " — " is a comment).
func loadHeavy(docs fs.FS) map[string]string {
	heavyList.Do(func() {
		heavyList.names = parseHeavy(readOr(docs, "HEAVY_HITTERS.md"))
	})
	return heavyList.names
}

func readOr(docs fs.FS, name string) string {
	if docs == nil {
		return ""
	}
	raw, err := fs.ReadFile(docs, name)
	if err != nil {
		return ""
	}
	return string(raw)
}

func parseHeavy(text string) map[string]string {
	out := map[string]string{}
	section := "heavy hitter"
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if h, ok := strings.CutPrefix(line, "## "); ok {
			section = strings.ToLower(strings.TrimSpace(h))
			continue
		}
		name, ok := strings.CutPrefix(line, "- ")
		if !ok {
			continue
		}
		for _, sep := range []string{" -- ", " — "} {
			name, _, _ = strings.Cut(name, sep)
		}
		if name = strings.TrimSpace(name); name != "" {
			out[draftKey(name)] = section
		}
	}
	return out
}

// heavyHitter says whether a spell of yours deserves decision pads, and why.
func heavyHitter(docs fs.FS, c cards.Card) (bool, string) {
	if s, ok := loadHeavy(docs)[draftKey(c.Name)]; ok {
		return true, "on the heavy-hitter list (" + s + ")"
	}
	if reWipe.MatchString(reNoInteraction.ReplaceAllString(c.Oracle, "")) {
		return true, "it hits the whole board"
	}
	if strings.Contains(c.TypeLine, "Creature") {
		mv := c.CMC
		if c.ManaCost != "" {
			mv = manaIn(c.ManaCost)
		}
		if mv >= heavyCreatureMV {
			return true, "a huge creature (mana value " + strconv.Itoa(mv) + ")"
		}
		if m := rePower.FindStringSubmatch(c.PT); m != nil {
			if p, _ := strconv.Atoi(m[1]); p >= heavyCreaturePower {
				return true, "a huge creature (power " + m[1] + ")"
			}
		}
	}
	return false, ""
}
