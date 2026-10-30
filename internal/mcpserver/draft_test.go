package mcpserver

import (
	"reflect"
	"strings"
	"testing"

	"github.com/MrDentalStone/Nemesis/internal/pad"
)

func TestParseCubeList(t *testing.T) {
	got := parseCubeList("# my cube\nDeck\n2 Llanowar Elves\n1x Lightning Bolt (2X2) 117\nCounterspell\n\nSideboard:\n// note\nFire // Ice\n")
	want := []string{"Llanowar Elves", "Llanowar Elves", "Lightning Bolt", "Counterspell", "Fire // Ice"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestCubeURL(t *testing.T) {
	for in, want := range map[string]string{
		"vintagecube": "https://cubecobra.com/cube/api/cubelist/vintagecube",
		"https://cubecobra.com/cube/overview/abc-123": "https://cubecobra.com/cube/api/cubelist/abc-123",
		"https://cubecobra.com/cube/list/abc_123?x=1": "https://cubecobra.com/cube/api/cubelist/abc_123",
		"https://example.com/my-cube.txt":             "https://example.com/my-cube.txt",
	} {
		got, err := cubeURL(in)
		if err != nil || got != want {
			t.Errorf("cubeURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := cubeURL("not a cube id"); err == nil {
		t.Error("a name with spaces should not pass as a cube id")
	}
}

func TestDraftKey(t *testing.T) {
	for in, want := range map[string]string{
		"Bonecrusher Giant // Stomp":          "bonecrusher giant",
		"Llanowar Elves\nCreature — Elf 1CMC": "llanowar elves",
		"  Counterspell ":                     "counterspell",
	} {
		if got := draftKey(in); got != want {
			t.Errorf("draftKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderDraftPack(t *testing.T) {
	rememberCard("Llanowar Elves", "{G}", "common")
	rememberCard("Shivan Dragon", "{4}{R}{R}", "rare")
	e := Event{"ev": "draft", "what": "pack", "round": 1.0, "rounds": 3.0, "pick": 2.0, "need": 2.0, "pool": 2.0,
		"cards": `[{"guid":"a1","nick":"Llanowar Elves\nCreature — Elf Druid 1CMC","desc":"{T}: Add {G}.\n[b]1/1[/b]"},` +
			`{"guid":"b2","nick":"Shivan Dragon\nCreature — Dragon 6CMC","desc":"Flying\n{R}: Shivan Dragon gets +1/+0 until end of turn.\n[b]5/5[/b]"}]`}
	out := Render(e)
	for _, want := range []string{"pack 1 of 3, pick 2: take 2", "[a1] Llanowar Elves {G} — Creature — Elf Druid 1/1", "[b2] Shivan Dragon {4}{R}{R}", "(rare)", "Flying"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
}

func TestPoolSummary(t *testing.T) {
	rememberCard("Grizzly Bears", "{1}{G}", "common")
	rememberCard("Lightning Bolt", "{R}", "common")
	out := poolSummary([]pad.RawCard{
		{GUID: "g1", Nick: "Grizzly Bears\nCreature — Bear 2CMC"},
		{GUID: "g2", Nick: "Lightning Bolt\nInstant 1CMC"},
		{GUID: "g3", Nick: "Forest\nBasic Land — Forest"},
	})
	for _, want := range []string{"3 cards: 1 creatures, 1 lands", "R 1, G 1", "1: 1, 2: 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
}
