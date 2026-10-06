package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/MrDentalStone/Nemesis/internal/cards"
	"github.com/MrDentalStone/Nemesis/internal/pad"
)

func TestRenderPlay(t *testing.T) {
	bolt := Event{"ev": "play", "color": "Blue", "player": "Sam", "from": "hand", "to": "graveyard",
		"name": "Lightning Bolt\nInstant 1CMC", "desc": "Lightning Bolt deals 3 damage to any target.",
		"canRespond": true, "window": "Counterspell (instant, mv 2); 3 mana up"}
	got := Render(bolt)
	for _, want := range []string{"[play] Blue (Sam): Lightning Bolt (Instant, mv 1)", "(hand -> graveyard)", "deals 3 damage", "you could respond: Counterspell"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q lacks %q", got, want)
		}
	}
	forest := Event{"ev": "play", "color": "Blue", "from": "hand", "to": "battlefield", "landRow": true,
		"name": "Forest\nBasic Land — Forest 0CMC", "canRespond": false}
	if got := Render(forest); strings.Contains(got, "respond") {
		t.Errorf("a land should not talk about responses: %q", got)
	}
}

func TestRenderIsShorterThanJSON(t *testing.T) {
	for _, e := range []Event{
		{"ev": "chat", "color": "Blue", "name": "Sam", "text": "any responses?"},
		{"ev": "round", "id": 3.0, "kind": "responses", "result": "all_no", "answers": "Blue=no", "text": "Casting X, any responses?", "timedOut": false},
		{"ev": "turn", "color": "Red", "claude": true, "prev": "Blue"},
	} {
		if strings.HasPrefix(Render(e), "{") {
			t.Errorf("no plain rendering for %v", e)
		}
	}
}

func TestRenderZoneMoveAndEndStep(t *testing.T) {
	move := Render(Event{"ev": "zone", "dir": "move", "from": "playmat", "zone": "exileZone", "name": "Talrand"})
	if move != "[zone] your Talrand moved from playmat to exile" {
		t.Errorf("zone move: %q", move)
	}
	turn := Render(Event{"ev": "turn", "color": "Red", "claude": true, "prev": "Blue", "endStep": "Fact or Fiction (instant, mv 4); 5 mana up"})
	if !strings.Contains(turn, "Blue's end step") || !strings.Contains(turn, "Fact or Fiction") {
		t.Errorf("end step: %q", turn)
	}
	if plain := Render(Event{"ev": "turn", "color": "Red", "claude": true}); plain != "[turn] YOUR turn (Red)" {
		t.Errorf("turn without a window: %q", plain)
	}
}

func TestManaIn(t *testing.T) {
	for cost, want := range map[string]int{"{2}{U}{U}": 4, "{T}": 0, "{1}{G}, {T}": 2, "{X}{R}": 1, "{W/U}{W/U}": 2, "": 0} {
		if got := manaIn(cost); got != want {
			t.Errorf("manaIn(%q) = %d, want %d", cost, got, want)
		}
	}
}

func TestLandSummary(t *testing.T) {
	forest := func(tapped bool) pad.Permanent {
		return pad.Permanent{GUID: "f", Type: "Card", Nick: "Forest\nBasic Land — Forest 0CMC", Row: "land", Tapped: tapped}
	}
	perms := []pad.Permanent{forest(false), forest(true), forest(false),
		{GUID: "t", Type: "Card", Nick: "Command Tower\nLand 0CMC", Row: "land"},
		{GUID: "e", Type: "Card", Nick: "Llanowar Elves\nCreature — Elf Druid 1CMC", Desc: "[b]1/1[/b]"},
		{GUID: "d", Type: "Card", Nick: "Dryad Arbor\nLand Creature — Forest Dryad 0CMC", Row: "land", Tapped: true,
			Enc: &pad.EncInfo{Note: "animated"}},
	}
	line, rest := landSummary(perms)
	if line != "lands 4 (1 tapped): Forest x3 (1 tapped), Command Tower" {
		t.Errorf("got %q", line)
	}
	if len(rest) != 2 || rest[0].GUID != "e" || rest[1].GUID != "d" {
		t.Errorf("rest %+v", rest)
	}
}

func TestOlderThan(t *testing.T) {
	cases := []struct {
		have, want string
		old        bool
	}{{"v0.6.0", "v0.8.0", true}, {"v0.8.0", "v0.8.0", false}, {"v0.10.0", "v0.8.0", false}, {"", "v0.8.0", true}, {"v0.8", "v0.8.1", true}}
	for _, c := range cases {
		if got := olderThan(c.have, c.want); got != c.old {
			t.Errorf("olderThan(%q, %q) = %v", c.have, c.want, got)
		}
	}
	if PadVersion() == "" {
		t.Error("no NEMESIS_VERSION in the embedded pad")
	}
}

func TestAskTileCarriesThePad(t *testing.T) {
	lua, err := askTile("Kestrel")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"nemesisSetupTile", "spawnObjectJSON", `local NAME = "Kestrel"`, "nemesisPad = true", "%s*(%a+)"} {
		if !strings.Contains(lua, want) {
			t.Errorf("tile Lua lacks %q", want)
		}
	}
	if _, err := updateLua("abc123"); err != nil {
		t.Fatal(err)
	}
}

func TestNeedsWindow(t *testing.T) {
	ctx := context.Background()
	d := Deps{} // no Scryfall: nothing is a Game Changer here
	cases := []struct {
		c    cards.Card
		from string
		want bool
	}{
		{cards.Card{Name: "Arcane Signet", TypeLine: "Artifact", CMC: 2, Oracle: "{T}: Add one mana of any color in your commander's color identity."}, "hand", false},
		{cards.Card{Name: "Preordain", TypeLine: "Sorcery", CMC: 1, Oracle: "Scry 2, then draw a card."}, "hand", false},
		{cards.Card{Name: "Cultivate", TypeLine: "Sorcery", CMC: 3, Oracle: "Search your library for up to two basic land cards, reveal those cards, put one onto the battlefield tapped and the other into your hand, then shuffle."}, "hand", false},
		{cards.Card{Name: "Swords to Plowshares", TypeLine: "Instant", CMC: 1, Oracle: "Exile target creature. Its controller gains life equal to its power."}, "hand", true},
		{cards.Card{Name: "Harmonize", TypeLine: "Sorcery", CMC: 4, Oracle: "Draw three cards."}, "hand", true},
		{cards.Card{Name: "Talrand, Sky Summoner", TypeLine: "Legendary Creature — Merfolk Wizard", CMC: 4}, "command", true},
		{cards.Card{Name: "Llanowar Elves", TypeLine: "Creature — Elf Druid", CMC: 1, Oracle: "{T}: Add {G}."}, "hand", false},
		{cards.Card{Name: "Lightning Greaves", TypeLine: "Artifact — Equipment", CMC: 2, Oracle: "Equipped creature has haste and shroud. (It can't be the target of spells or abilities.)\nEquip {0}"}, "hand", false},
		{cards.Card{Name: "Think Twice", TypeLine: "Instant", CMC: 2, Oracle: "Draw a card.\nFlashback {2}{U} (You may cast this card from your graveyard for its flashback cost. Then exile it.)"}, "hand", false},
		{cards.Card{Name: "Swiftfoot Boots", TypeLine: "Artifact — Equipment", CMC: 2, Oracle: "Equipped creature has hexproof and haste. (It can't be the target of spells or abilities your opponents control.)\nEquip {1}"}, "hand", false},
		{cards.Card{Name: "Faerie Mastermind", TypeLine: "Creature — Faerie Rogue", CMC: 2, Oracle: "Flash\nFlying\nWhenever an opponent draws their second card each turn, you draw a card.\n{3}{U}: Each player draws a card."}, "hand", true},
		{cards.Card{Name: "Blasphemous Act", TypeLine: "Sorcery", CMC: 9, Oracle: "Blasphemous Act deals 13 damage to each creature."}, "hand", true},
	}
	for _, c := range cases {
		if got, why := needsWindow(ctx, d, c.c, c.from); got != c.want {
			t.Errorf("needsWindow(%s) = %v (%s), want %v", c.c.Name, got, why, c.want)
		}
	}
}

func TestEmptyStackDropsSpellAnswers(t *testing.T) {
	w := Window{Mana: 6, Options: []string{"Counterspell", "Repulse", "Twincast"}}
	for _, o := range []string{"Counter target spell.", "Return target creature to its owner's hand.\nDraw a card.", "Copy target instant or sorcery spell."} {
		w.OnSpell = append(w.OnSpell, reOnSpell.MatchString(o))
	}
	if got := w.EmptyStack().Options; len(got) != 1 || got[0] != "Repulse" {
		t.Errorf("EmptyStack = %v, want [Repulse]", got)
	}
}
