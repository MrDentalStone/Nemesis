package mcpserver

import (
	"strings"
	"testing"

	"github.com/MrDentalStone/Nemesis/internal/pad"
)

func life(n int) *int { return &n }

func TestBeforeDraw(t *testing.T) {
	snap := &pad.Snapshot{Seats: []pad.Seat{
		{Color: "White", Life: life(18), Battlefield: []pad.Permanent{
			{GUID: "orb", Nick: "Mesmeric Orb\nArtifact 2CMC", Desc: "Whenever a permanent becomes untapped, that permanent's controller mills a card."},
			{GUID: "ring", Nick: "Sol Ring\nArtifact 1CMC", Desc: "{T}: Add {C}{C}."},
		}},
		{Color: "Purple", Life: life(30), Battlefield: []pad.Permanent{
			{GUID: "mar", Nick: "Maralen of the Mornsong\nLegendary Creature — Elf Wizard 3CMC", Desc: "Players can't draw cards.\nAt the beginning of each player's draw step, that player loses 3 life, searches their library for a card, puts it into their hand, then shuffles."},
			{GUID: "fd", Nick: "(face-down card)", FaceDown: true},
			{GUID: "own", Nick: "Phyrexian Arena\nEnchantment 3CMC", Desc: "At the beginning of your upkeep, you draw a card and you lose 1 life."},
		}},
		{Color: "Red", IsClaude: true, Life: life(30), Battlefield: []pad.Permanent{
			{GUID: "tithe", Nick: "Smothering Tithe\nEnchantment 4CMC", Desc: "Whenever an opponent draws a card, that player may pay {2}. If the player doesn't, you create a Treasure token."},
			{GUID: "arena", Nick: "Phyrexian Arena\nEnchantment 3CMC", Desc: "At the beginning of your upkeep, you draw a card and you lose 1 life."},
		}},
		{Color: "Blue", Life: life(0), Battlefield: []pad.Permanent{
			{GUID: "orb2", Nick: "Mesmeric Orb\nArtifact 2CMC", Desc: "Whenever a permanent becomes untapped, that permanent's controller mills a card."},
		}},
	}}
	got := strings.Join(beforeDraw(snap), "\n")
	for _, want := range []string{"[orb]", "[mar]", "[arena] (yours)"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in:\n%s", want, got)
		}
	}
	// Sol Ring does nothing then; Purple's Arena is Purple's upkeep; Tithe is
	// mine and watches opponents; Blue is out of the game
	for _, not := range []string{"[ring]", "[own]", "[tithe]", "[orb2]", "[fd]"} {
		if strings.Contains(got, not) {
			t.Errorf("%s should not be listed:\n%s", not, got)
		}
	}
}

func TestRenderTurnFlags(t *testing.T) {
	if got := Render(Event{"ev": "turn", "color": "Blue", "tookBack": true}); !strings.Contains(got, "YOUR turn is over") {
		t.Errorf("took back: %q", got)
	}
	if got := Render(Event{"ev": "turn", "color": "Red", "claude": true, "first": true}); !strings.Contains(got, "who goes first") {
		t.Errorf("first turn: %q", got)
	}
}

func TestPublicCard(t *testing.T) {
	snap := &pad.Snapshot{Seats: []pad.Seat{{Color: "Blue",
		Battlefield: []pad.Permanent{{GUID: "ita", Nick: "Itazura", Desc: "long text"}, {GUID: "fd", Nick: "(face-down card)", FaceDown: true}},
		Exile:       []pad.RawCard{{GUID: "hid", Nick: "Secret", Desc: "x", FaceDown: true}, {GUID: "ex", Nick: "Expropriate", Desc: "vote"}},
	}}}
	if n, _, ok := publicCard(snap, "ita"); !ok || n != "Itazura" {
		t.Errorf("battlefield card not found")
	}
	if _, _, ok := publicCard(snap, "ex"); !ok {
		t.Errorf("exiled card not found")
	}
	for _, g := range []string{"fd", "hid", "nope"} {
		if _, _, ok := publicCard(snap, g); ok {
			t.Errorf("%s should stay hidden", g)
		}
	}
}

func TestSeatOut(t *testing.T) {
	scooped := pad.Seat{Color: "Blue", Life: life(31), Battlefield: []pad.Permanent{{GUID: "ts", Type: "Generic", Nick: "Turn Skipper"}}}
	if seatOut(scooped) == "" {
		t.Error("a Turn Skipper on the mat should mark the seat out")
	}
	if seatOut(pad.Seat{Color: "White", Life: life(-2)}) == "" {
		t.Error("life below 0 should mark the seat out")
	}
	if why := seatOut(pad.Seat{Color: "Purple", Life: life(26)}); why != "" {
		t.Errorf("a live seat is out: %s", why)
	}
	snap := &pad.Snapshot{Seats: []pad.Seat{scooped}}
	snap.Seats[0].Battlefield = append(snap.Seats[0].Battlefield, pad.Permanent{GUID: "orb", Nick: "Mesmeric Orb\nArtifact 2CMC", Desc: "Whenever a permanent becomes untapped, that permanent's controller mills a card."})
	if got := beforeDraw(snap); len(got) != 0 {
		t.Errorf("an out seat's Orb is gone: %v", got)
	}
}
