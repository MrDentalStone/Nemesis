// Package content loads play modes and decklists from Markdown files.
//
// A mode is modes/<id>.md, a deck is decks/<id>.md. Both start with a small
// front-matter block between "---" lines (flat "key: value" pairs, lists as
// [a, b, c]); the rest is Markdown the AI reads. A deck's list is the first
// fenced code block in its body. Files whose name starts with "_" are shared
// text: modes/_shared.md is appended to every mode's rules.
//
// Content comes from layers: the files built into the binary first, then each
// directory given at run time, so a table can add or override modes and decks
// by dropping .md files into a folder. A later layer replaces an earlier file
// with the same id.
package content

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
)

type Mode struct {
	ID      string
	Name    string
	Blurb   string   // one line, shown on the pad when the mode changes
	Order   int      // position on the pad's mode picker
	Default bool     // the mode a fresh pad starts in
	Decks   []string // extra deck ids for this mode, on top of decks that list it
	Body    string   // the rules, Markdown
	Source  string
}

type Deck struct {
	ID        string
	Name      string
	Commander string
	Colors    string // WUBRG letters, "C" for colourless
	Bracket   string
	Modes     []string
	Summary   string
	Body      string // pilot notes and the list, Markdown
	List      string // the decklist from the first fenced block
	Source    string
}

type Library struct {
	Modes  map[string]*Mode
	Decks  map[string]*Deck
	Shared string // modes/_shared.md
	Errors []string
}

// Load reads modes/ and decks/ from each filesystem in order.
func Load(layers ...fs.FS) *Library {
	lib := &Library{Modes: map[string]*Mode{}, Decks: map[string]*Deck{}}
	for _, fsys := range layers {
		if fsys == nil {
			continue
		}
		lib.loadDir(fsys, "modes", func(id, src string, fm map[string]string, body string) error {
			if id == "_shared" {
				lib.Shared = strings.TrimSpace(body)
				return nil
			}
			m := &Mode{ID: id, Name: fm["name"], Blurb: fm["blurb"], Body: strings.TrimSpace(body), Source: src}
			if v := fm["id"]; v != "" {
				m.ID = v
			}
			if m.Name == "" {
				return errors.New("front matter needs a name")
			}
			m.Order, _ = strconv.Atoi(fm["order"])
			m.Default = fm["default"] == "true"
			m.Decks = list(fm["decks"])
			lib.Modes[m.ID] = m
			return nil
		})
		lib.loadDir(fsys, "decks", func(id, src string, fm map[string]string, body string) error {
			if strings.HasPrefix(id, "_") {
				return nil
			}
			d := &Deck{ID: id, Name: fm["name"], Commander: fm["commander"], Colors: fm["colors"],
				Bracket: fm["bracket"], Modes: list(fm["modes"]), Summary: fm["summary"],
				Body: strings.TrimSpace(body), Source: src}
			d.List = firstFence(body)
			if d.Name == "" || d.Commander == "" {
				return errors.New("front matter needs name and commander")
			}
			if d.List == "" {
				return errors.New("no decklist (put it in a ``` fenced block)")
			}
			lib.Decks[d.ID] = d
			return nil
		})
	}
	return lib
}

// Dir is a content layer read from disk; nil when the directory has neither
// modes/ nor decks/, so callers can pass optional folders straight to Load.
func Dir(dir string) fs.FS {
	if dir == "" {
		return nil
	}
	for _, sub := range []string{"modes", "decks"} {
		if st, err := os.Stat(path.Join(dir, sub)); err == nil && st.IsDir() {
			return os.DirFS(dir)
		}
	}
	return nil
}

func (l *Library) loadDir(fsys fs.FS, dir string, add func(id, src string, fm map[string]string, body string) error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") || strings.EqualFold(e.Name(), "README.md") {
			continue
		}
		p := path.Join(dir, e.Name())
		raw, err := fs.ReadFile(fsys, p)
		if err != nil {
			l.Errors = append(l.Errors, fmt.Sprintf("%s: %v", p, err))
			continue
		}
		fm, body := FrontMatter(string(raw))
		if err := add(strings.TrimSuffix(e.Name(), ".md"), p, fm, body); err != nil {
			l.Errors = append(l.Errors, fmt.Sprintf("%s: %v", p, err))
		}
	}
}

// FrontMatter splits "---\nkey: value\n---\nbody". Values keep their text
// as written (quotes stripped); lists stay as "[a, b]" for list().
func FrontMatter(raw string) (map[string]string, string) {
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	fm := map[string]string{}
	if !strings.HasPrefix(raw, "---\n") {
		return fm, raw
	}
	head, body, ok := strings.Cut(raw[4:], "\n---")
	if !ok {
		return fm, raw
	}
	body = strings.TrimPrefix(strings.TrimPrefix(body, "-"), "\n")
	for _, line := range strings.Split(head, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		fm[strings.ToLower(strings.TrimSpace(k))] = v
	}
	return fm, body
}

func list(v string) []string {
	v = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(v), "["), "]"))
	if v == "" {
		return nil
	}
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.Trim(strings.TrimSpace(s), `"'`); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func firstFence(body string) string {
	_, rest, ok := strings.Cut(body, "```")
	if !ok {
		return ""
	}
	_, rest, _ = strings.Cut(rest, "\n") // drop the info string
	block, _, ok := strings.Cut(rest, "```")
	if !ok {
		return ""
	}
	return strings.TrimSpace(block) + "\n"
}

// SortedModes in pad order (order, then name).
func (l *Library) SortedModes() []*Mode {
	out := make([]*Mode, 0, len(l.Modes))
	for _, m := range l.Modes {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// DecksFor lists the decks suited to a mode ("" = every deck), by name.
func (l *Library) DecksFor(mode string) []*Deck {
	extra := map[string]bool{}
	if m := l.Modes[mode]; m != nil {
		for _, id := range m.Decks {
			extra[id] = true
		}
	}
	var out []*Deck
	for _, d := range l.Decks {
		ok := mode == "" || extra[d.ID]
		for _, dm := range d.Modes {
			ok = ok || dm == mode
		}
		if ok {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ModesOf a deck: the ones it names plus any mode that lists it.
func (l *Library) ModesOf(d *Deck) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range d.Modes {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	for _, m := range l.SortedModes() {
		for _, id := range m.Decks {
			if id == d.ID && !seen[m.ID] {
				seen[m.ID] = true
				out = append(out, m.ID)
			}
		}
	}
	return out
}

// Commanders named in a decklist: the lines under a "Commander" header, or
// lines marked *CMDR* (Moxfield's export).
func Commanders(decklist string) []string {
	var out []string
	in := false
	for _, line := range strings.Split(strings.ReplaceAll(decklist, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		low := strings.ToLower(strings.TrimSuffix(t, ":"))
		switch {
		case t == "":
			continue
		case low == "commander" || low == "commanders":
			in = true
			continue
		case low == "deck" || low == "mainboard" || low == "main" || low == "sideboard" || low == "maybeboard" || low == "companion":
			in = false
			continue
		}
		cmdr := strings.Contains(t, "*CMDR*")
		if !in && !cmdr {
			continue
		}
		if _, name, ok := CardLine(t); ok {
			out = append(out, name)
		}
	}
	return out
}

// CardLine reads "1 Name", "1x Name", "Name (SET) 12" and strips set pins and
// Moxfield markers. ok is false for a line that is not a card.
func CardLine(t string) (n int, name string, ok bool) {
	t = strings.TrimSpace(strings.ReplaceAll(t, "*CMDR*", ""))
	n = 1
	if num, rest, cut := strings.Cut(t, " "); cut {
		if v, err := strconv.Atoi(strings.TrimSuffix(strings.ToLower(num), "x")); err == nil {
			n, t = v, strings.TrimSpace(rest)
		}
	}
	if i := strings.Index(t, " ("); i > 0 {
		t = t[:i]
	}
	if i := strings.Index(t, " *"); i > 0 {
		t = t[:i]
	}
	t = strings.TrimSpace(t)
	return n, t, t != "" && n > 0
}
