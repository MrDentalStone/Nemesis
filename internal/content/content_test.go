package content_test

import (
	"reflect"
	"testing"
	"testing/fstest"

	"github.com/MrDentalStone/Nemesis"
	"github.com/MrDentalStone/Nemesis/internal/content"
)

func TestFrontMatter(t *testing.T) {
	fm, body := content.FrontMatter("---\r\nname: \"Slow Grow\"\r\nmodes: [a, b]\r\n---\r\nRules here.\r\n")
	if fm["name"] != "Slow Grow" || fm["modes"] != "[a, b]" || body != "Rules here.\n" {
		t.Fatalf("got %q %q", fm, body)
	}
	if fm, body := content.FrontMatter("no front matter"); len(fm) != 0 || body != "no front matter" {
		t.Fatalf("got %q %q", fm, body)
	}
}

func TestCommanders(t *testing.T) {
	list := "Commander\n1 Kynaios and Tiro of Meletis\n\nDeck\n1 Sol Ring\n"
	if got := content.Commanders(list); !reflect.DeepEqual(got, []string{"Kynaios and Tiro of Meletis"}) {
		t.Fatalf("got %q", got)
	}
	mox := "1 Sol Ring\n1 Tymna the Weaver (C16) 48 *CMDR*\n1 Thrasios, Triton Hero *CMDR*\n"
	if got := content.Commanders(mox); !reflect.DeepEqual(got, []string{"Tymna the Weaver", "Thrasios, Triton Hero"}) {
		t.Fatalf("got %q", got)
	}
}

func TestCardLine(t *testing.T) {
	for in, want := range map[string]struct {
		n    int
		name string
	}{
		"30 Forest":                {30, "Forest"},
		"1x Sol Ring":              {1, "Sol Ring"},
		"Llanowar Elves (M19) 314": {1, "Llanowar Elves"},
		"1 Fire // Ice":            {1, "Fire // Ice"},
	} {
		n, name, ok := content.CardLine(in)
		if !ok || n != want.n || name != want.name {
			t.Errorf("%q: got %d %q %v", in, n, name, ok)
		}
	}
}

func TestLayersOverride(t *testing.T) {
	base := fstest.MapFS{
		"modes/a.md": {Data: []byte("---\nname: A\norder: 2\n---\nold")},
		"decks/x.md": {Data: []byte("---\nname: X\ncommander: C\nmodes: [a]\n---\n```\nCommander\n1 C\n```\n")},
	}
	over := fstest.MapFS{
		"modes/a.md": {Data: []byte("---\nname: A2\norder: 1\n---\nnew")},
		"modes/b.md": {Data: []byte("---\nname: B\norder: 3\ndecks: [x]\n---\nb")},
	}
	l := content.Load(base, over)
	if len(l.Errors) > 0 {
		t.Fatal(l.Errors)
	}
	if l.Modes["a"].Name != "A2" || l.Modes["a"].Body != "new" {
		t.Fatalf("override failed: %+v", l.Modes["a"])
	}
	if got := l.DecksFor("b"); len(got) != 1 || got[0].ID != "x" {
		t.Fatalf("mode decks: %v", got)
	}
	if got := l.ModesOf(l.Decks["x"]); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("deck modes: %v", got)
	}
}

// The shipped modes and decks load cleanly and point at each other.
func TestShippedContent(t *testing.T) {
	l := content.Load(nemesis.Files)
	if len(l.Errors) > 0 {
		t.Fatal(l.Errors)
	}
	if len(l.Modes) == 0 || len(l.Decks) == 0 {
		t.Fatalf("%d modes, %d decks", len(l.Modes), len(l.Decks))
	}
	defaults := 0
	for _, m := range l.Modes {
		if m.Default {
			defaults++
		}
		if m.Blurb == "" {
			t.Errorf("mode %s has no blurb", m.ID)
		}
		for _, id := range m.Decks {
			if l.Decks[id] == nil {
				t.Errorf("mode %s lists missing deck %s", m.ID, id)
			}
		}
	}
	if defaults != 1 {
		t.Errorf("%d default modes, want 1", defaults)
	}
	for _, d := range l.Decks {
		for _, m := range d.Modes {
			if l.Modes[m] == nil {
				t.Errorf("deck %s names unknown mode %s", d.ID, m)
			}
		}
		if len(content.Commanders(d.List)) == 0 {
			t.Errorf("deck %s has no Commander section", d.ID)
		}
	}
}

// go:embed drops files starting with "_" unless the dir is embedded with all:.
func TestSharedEmbedded(t *testing.T) {
	if l := content.Load(nemesis.Files); l.Shared == "" {
		t.Fatal("modes/_shared.md is not in the binary")
	}
}
