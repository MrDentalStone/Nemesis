// Package cards turns a TTS card object's Nickname/Description into
// structured card text. Two formats are in the wild on MTG tables:
//
//	klrmngr/rikrassen:  Nickname "Name\nType line 3CMC"
//	                    Description "<oracle>\n[b]P/T[/b]"
//	Plain name:         Nickname "Name"
//	                    Description "{1}{U}\nType line\n\n<oracle>"
package cards

import (
	"regexp"
	"strings"
)

// Card is what the model needs to reason about a card.
type Card struct {
	Name     string `json:"name"`
	TypeLine string `json:"type,omitempty"`
	ManaCost string `json:"mana,omitempty"`
	CMC      int    `json:"cmc,omitempty"`
	Oracle   string `json:"oracle,omitempty"`
	PT       string `json:"pt,omitempty"` // "3/2", loyalty for planeswalkers is left in oracle
}

var (
	reCMC    = regexp.MustCompile(`\s*(\d+)\s*CMC\s*$`)
	reBBCode = regexp.MustCompile(`\[/?[a-zA-Z0-9#]+\]`)
	rePT     = regexp.MustCompile(`\[b\]\s*([0-9X*+-]+/[0-9X*+-]+)\s*\[/b\]\s*$`)
	reMana   = regexp.MustCompile(`^(\{[^}]+\})+(\s*//\s*(\{[^}]+\})+)?$`)
)

// Parse accepts either format and degrades gracefully to name+raw text.
func Parse(nick, desc string) Card {
	nick = strings.ReplaceAll(strings.TrimSpace(nick), "\r", "")
	desc = strings.ReplaceAll(strings.TrimSpace(desc), "\r", "")
	var c Card

	if i := strings.IndexByte(nick, '\n'); i >= 0 {
		// klrmngr: second line is "Type line NCMC"
		c.Name = strings.TrimSpace(nick[:i])
		rest := strings.TrimSpace(nick[i+1:])
		if m := reCMC.FindStringSubmatch(rest); m != nil {
			c.CMC = atoi(m[1])
			rest = strings.TrimSpace(rest[:len(rest)-len(m[0])])
		}
		c.TypeLine = fixDash(rest)
		if m := rePT.FindStringSubmatch(desc); m != nil {
			c.PT = m[1]
			desc = strings.TrimSpace(desc[:len(desc)-len(m[0])])
		}
		c.Oracle = clean(desc)
		return c
	}

	c.Name = nick
	lines := strings.Split(desc, "\n")
	if len(lines) >= 2 && reMana.MatchString(strings.TrimSpace(lines[0])) {
		// plain name with mana cost
		c.ManaCost = strings.TrimSpace(lines[0])
		c.TypeLine = fixDash(strings.TrimSpace(lines[1]))
		c.Oracle = clean(strings.Join(lines[2:], "\n"))
		return c
	}
	if len(lines) >= 1 && looksLikeTypeLine(lines[0]) && (len(lines) == 1 || strings.TrimSpace(lines[1]) == "") {
		// plain name without mana cost (lands)
		c.TypeLine = fixDash(strings.TrimSpace(lines[0]))
		if len(lines) > 2 {
			c.Oracle = clean(strings.Join(lines[2:], "\n"))
		}
		return c
	}
	if m := rePT.FindStringSubmatch(desc); m != nil {
		c.PT = m[1]
		desc = strings.TrimSpace(desc[:len(desc)-len(m[0])])
	}
	c.Oracle = clean(desc)
	return c
}

var typeWords = []string{"Creature", "Land", "Instant", "Sorcery", "Artifact", "Enchantment", "Planeswalker", "Battle", "Token", "Legendary", "Basic", "Kindred", "Tribal"}

func looksLikeTypeLine(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || strings.Contains(s, ".") {
		return false
	}
	for _, w := range typeWords {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

// fixDash repairs the "?" that replaced an em dash when the type line was
// written through a cp1252-ish path ("Creature ? Cat").
func fixDash(s string) string {
	return strings.ReplaceAll(s, " ? ", " — ")
}

func clean(s string) string {
	s = reBBCode.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}
