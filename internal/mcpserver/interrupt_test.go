package mcpserver

import (
	"context"
	"strings"
	"testing"
)

func TestHoldFor(t *testing.T) {
	ctx := context.Background()
	d := Deps{} // no Scryfall: nothing is a Game Changer here
	counter := Window{Mana: 3, Options: []string{"Counterspell (instant, mv 2)"}, OnSpell: []bool{true}}
	removal := Window{Mana: 3, Options: []string{"Swords to Plowshares (instant, mv 1)"}, OnSpell: []bool{false}}
	harmonize := Event{"ev": "play", "from": "hand", "to": "graveyard", "name": "Harmonize\nSorcery 4CMC", "desc": "Draw three cards."}
	preordain := Event{"ev": "play", "from": "hand", "to": "graveyard", "name": "Preordain\nSorcery 1CMC", "desc": "Scry 2, then draw a card."}
	reanimated := Event{"ev": "play", "from": "graveyard", "to": "battlefield", "name": "Griselbrand\nLegendary Creature 8CMC"}
	cases := []struct {
		name string
		e    Event
		w    Window
		want bool
	}{
		{"a spell that matters, with a counter up", harmonize, counter, true},
		{"a spell that matters, removal only", harmonize, removal, false},
		{"a quick spell", preordain, counter, false},
		{"back from the graveyard is no cast", reanimated, counter, false},
	}
	for _, c := range cases {
		if got := holdFor(ctx, d, c.e, c.w); got != c.want {
			t.Errorf("%s: holdFor = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCanTarget(t *testing.T) {
	cases := []struct {
		target, typeLine string
		want             bool
	}{
		{"spell", "Legendary Creature — Angel", true},
		{"noncreature spell", "Legendary Creature — Angel", false},
		{"noncreature spell", "Sorcery", true},
		{"creature spell", "Artifact Creature — Golem", true},
		{"creature spell", "Instant", false},
		{"instant or sorcery spell", "Sorcery", true},
		{"instant or sorcery spell", "Enchantment — Aura", false},
		{"multicolored spell", "Creature — Elf", true},
	}
	for _, c := range cases {
		if got := canTarget(c.target, c.typeLine); got != c.want {
			t.Errorf("canTarget(%q, %q) = %v, want %v", c.target, c.typeLine, got, c.want)
		}
	}
	negate := Window{Mana: 3, Options: []string{"Negate (instant, mv 2)"}, OnSpell: []bool{true}, Targets: []string{"noncreature spell"}}
	shalai := Event{"ev": "play", "from": "hand", "to": "battlefield", "name": "Shalai, Voice of Plenty\nLegendary Creature — Angel 4CMC"}
	if holdFor(context.Background(), Deps{}, shalai, negate) {
		t.Error("Negate should not hold the table for a creature")
	}
}

func TestRenderInterrupts(t *testing.T) {
	held := Render(Event{"ev": "play", "color": "Blue", "from": "hand", "to": "graveyard", "guid": "abc123",
		"name": "Harmonize\nSorcery 4CMC", "desc": "Draw three cards.", "canRespond": true, "held": true,
		"window": "Counterspell (instant, mv 2); 3 mana up"})
	for _, want := range []string{"HELD FOR YOU", "respond_to=abc123", "pass", "Counterspell"} {
		if !strings.Contains(held, want) {
			t.Errorf("%q lacks %q", held, want)
		}
	}
	for ev, want := range map[string]string{
		"accepted":   "went to their graveyard",
		"refused":    "Not countered",
		"unanswered": "remind them",
	} {
		got := Render(Event{"ev": "countered", "result": ev, "color": "Blue", "name": "Harmonize"})
		if !strings.Contains(got, want) {
			t.Errorf("countered/%s: %q lacks %q", ev, got, want)
		}
	}
	if got := Render(Event{"ev": "hold", "result": "expired", "name": "Harmonize", "secs": 15.0}); !strings.Contains(got, "ran out") {
		t.Errorf("hold expiry: %q", got)
	}
}

func TestMinorEventsWaitForNews(t *testing.T) {
	forest := Event{"ev": "play", "color": "Blue", "to": "battlefield", "landRow": true, "name": "Forest\nBasic Land — Forest 0CMC"}
	theirTurn := Event{"ev": "turn", "color": "Red"}
	landCount := Event{"ev": "log", "kind": "print", "text": "[1E87FF][b][Land][/b] [1E87FF]Island | 1"}
	if !minorEvent(landCount) || summarise([]Event{landCount}) != "Meanwhile: log [Land] Island | 1" {
		t.Errorf("land counter line: minor=%v %q", minorEvent(landCount), summarise([]Event{landCount}))
	}
	for _, e := range []Event{
		{"ev": "log", "kind": "print", "text": "Blue gained 5 life |49|"},
		{"ev": "log", "kind": "error", "text": "chunk_3:(12,4-9): attempt to index a nil value"},
	} {
		if minorEvent(e) {
			t.Errorf("%v should wake you", e)
		}
	}
	for _, e := range []Event{forest, theirTurn} {
		if !minorEvent(e) {
			t.Errorf("%v should wait for news", e)
		}
	}
	for _, e := range []Event{
		{"ev": "turn", "color": "Teal", "claude": true},
		{"ev": "chat", "color": "Blue", "text": "hi"},
		{"ev": "play", "color": "Blue", "to": "your_board", "landRow": true, "name": "Forest"},
		{"ev": "play", "color": "Blue", "to": "battlefield", "name": "Grizzly Bears\nCreature — Bear 2CMC"},
	} {
		if minorEvent(e) {
			t.Errorf("%v should wake you", e)
		}
	}
	if got := summarise([]Event{forest, theirTurn}); got != "Meanwhile: Blue played Forest; Red's turn" {
		t.Errorf("summarise = %q", got)
	}
}
