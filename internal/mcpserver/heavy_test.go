package mcpserver

import (
	"strings"
	"testing"

	nemesis "github.com/MrDentalStone/Nemesis"
	"github.com/MrDentalStone/Nemesis/internal/cards"
)

func TestParseHeavy(t *testing.T) {
	got := parseHeavy("# Heavy\n\n## Board wipe\n- Blasphemous Act\n- Cyclonic Rift -- overloaded\n\n## Huge threat\n- Godsire\nnot a card\n")
	want := map[string]string{"blasphemous act": "board wipe", "cyclonic rift": "board wipe", "godsire": "huge threat"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %q, want %q", k, got[k], v)
		}
	}
}

func TestHeavyHitter(t *testing.T) {
	for _, tc := range []struct {
		nick, desc string
		heavy      bool
	}{
		{"Blasphemous Act\nSorcery 9CMC", "This spell costs {1} less to cast for each creature on the battlefield.\nBlasphemous Act deals 13 damage to each creature.", true},
		{"Cyclonic Rift\nInstant 2CMC", "Return target nonland permanent you don't control to its owner's hand.\nOverload {6}{U}", true},
		{"Wrath of the Ages\nSorcery 4CMC", "Destroy all creatures. They can't be regenerated.", true},
		{"Pestilent Wave\nSorcery 4CMC", "All creatures get -3/-3 until end of turn.", true},
		{"Big Boy\nCreature — Giant 6CMC", "Trample\n[b]9/9[/b]", true},
		{"Godsire\nCreature — Beast 8CMC", "Vigilance\n[b]5/5[/b]", true},
		{"Grizzly Bears\nCreature — Bear 2CMC", "[b]2/2[/b]", false},
		{"Murder\nInstant 3CMC", "Destroy target creature.", false},
		{"Sol Ring\nArtifact 1CMC", "{T}: Add {C}{C}.", false},
		{"Boltwave\nSorcery 1CMC", "Boltwave deals 3 damage to each opponent.", false},
	} {
		c := cards.Parse(tc.nick, tc.desc)
		if got, why := heavyHitter(nemesis.Files, c); got != tc.heavy {
			t.Errorf("%s: heavy=%v (%s), want %v", c.Name, got, why, tc.heavy)
		}
	}
}

func TestRenderAttackPad(t *testing.T) {
	cardsJSON := `[{"name":"Grizzly Bears\nCreature — Bear 2CMC","desc":"[b]2/2[/b]"}]`
	at := Event{"ev": "attack", "color": "White", "player": "Dee", "cards": cardsJSON, "onMe": true, "pressed": true}
	if got := Render(at); !strings.Contains(got, "ATTACKING YOU (attack pad)") || !strings.Contains(got, "Grizzly Bears (2/2)") {
		t.Errorf("pressed: %q", got)
	}
	if minorEvent(at) {
		t.Error("an attack on you must wake you")
	}
	elsewhere := Event{"ev": "attack", "color": "White", "cards": cardsJSON, "onMe": false, "pressed": false}
	if !minorEvent(elsewhere) {
		t.Error("an attack with the pad dark should not wake you")
	}
	if got := summarise([]Event{elsewhere}); !strings.Contains(got, "White attacked someone else with 1") {
		t.Errorf("summary: %q", got)
	}
	vig := Event{"ev": "attack", "color": "White", "cards": "[]", "onMe": true, "noTaps": true}
	if got := Render(vig); !strings.Contains(got, "no creature of theirs tapped") {
		t.Errorf("no taps: %q", got)
	}
	old := Event{"ev": "attack", "color": "White", "cards": cardsJSON}
	if minorEvent(old) || !strings.Contains(Render(old), "tapped on their turn") {
		t.Errorf("an older pad's attack should read as before: %q", Render(old))
	}
}
