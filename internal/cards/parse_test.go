package cards

import "testing"

func TestParseKlrmngr(t *testing.T) {
	c := Parse("Arahbo, Roar of the World\nLegendary Creature ? Cat Avatar 5CMC",
		"Eminence — Whenever another Cat you control attacks...\n[b]5/5[/b]")
	if c.Name != "Arahbo, Roar of the World" || c.TypeLine != "Legendary Creature — Cat Avatar" || c.CMC != 5 {
		t.Fatalf("%+v", c)
	}
	if c.PT != "5/5" || c.Oracle != "Eminence — Whenever another Cat you control attacks..." {
		t.Fatalf("%+v", c)
	}
}

func TestParseKlrmngrLand(t *testing.T) {
	c := Parse("Forest\nBasic Land — Forest 0CMC", "")
	if c.Name != "Forest" || c.TypeLine != "Basic Land — Forest" || c.CMC != 0 || c.Oracle != "" {
		t.Fatalf("%+v", c)
	}
}

func TestParsePlainName(t *testing.T) {
	c := Parse("Delver of Secrets", "{U}\nCreature — Human Wizard\n\nAt the beginning of your upkeep, look at the top card of your library.")
	if c.ManaCost != "{U}" || c.TypeLine != "Creature — Human Wizard" || c.Oracle == "" || c.Name != "Delver of Secrets" {
		t.Fatalf("%+v", c)
	}
}

func TestParsePlainNameLand(t *testing.T) {
	c := Parse("Command Tower", "Land\n\n{T}: Add one mana of any color in your commander's color identity.")
	if c.TypeLine != "Land" || c.Oracle == "" || c.ManaCost != "" {
		t.Fatalf("%+v", c)
	}
}

func TestParseFallback(t *testing.T) {
	c := Parse("Mystery", "Some text [b]2/2[/b]")
	if c.Name != "Mystery" || c.PT != "2/2" || c.Oracle != "Some text" {
		t.Fatalf("%+v", c)
	}
}
