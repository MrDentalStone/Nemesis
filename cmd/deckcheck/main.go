// deckcheck validates the decks in decks/*.md against Scryfall: exactly 100
// cards, singleton apart from basics (and cards that allow any number),
// every card real and Commander-legal, inside the commander's colour
// identity, a Game Changer count that fits the deck's bracket, and modes
// that exist in modes/.
//
//	go run ./cmd/deckcheck                 # every deck in ./decks
//	go run ./cmd/deckcheck decks/x.md ...  # just these
//	go run ./cmd/deckcheck -importer       # also build each deck on the deck importer
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MrDentalStone/Nemesis/internal/content"
	"github.com/MrDentalStone/Nemesis/internal/importer"
	"github.com/MrDentalStone/Nemesis/internal/scryfall"
)

var basics = map[string]bool{
	"Plains": true, "Island": true, "Swamp": true, "Mountain": true, "Forest": true, "Wastes": true,
	"Snow-Covered Plains": true, "Snow-Covered Island": true, "Snow-Covered Swamp": true,
	"Snow-Covered Mountain": true, "Snow-Covered Forest": true, "Snow-Covered Wastes": true,
}

var viaImporter = flag.Bool("importer", false, "also build every deck on the deck importer (one request per deck)")
var importerURL = flag.String("importer-url", importer.DefaultBaseURL, "deck importer server")

func main() {
	flag.Parse()
	args := flag.Args()
	lib := content.Load(os.DirFS("."))
	for _, e := range lib.Errors {
		fmt.Println(e)
	}
	var decks []*content.Deck
	if len(args) > 0 {
		for _, f := range args {
			id := strings.TrimSuffix(filepath.Base(f), ".md")
			d := lib.Decks[id]
			if d == nil {
				fmt.Printf("%s: not loaded (see errors above)\n", f)
				os.Exit(1)
			}
			decks = append(decks, d)
		}
	} else {
		for _, d := range lib.Decks {
			decks = append(decks, d)
		}
		sort.Slice(decks, func(i, j int) bool { return decks[i].ID < decks[j].ID })
	}
	sf := scryfall.New("deckcheck")
	var imp *importer.Client
	if *viaImporter {
		imp = importer.New(*importerURL, "deckcheck")
	}
	ok := len(lib.Errors) == 0
	for _, d := range decks {
		ok = check(sf, imp, lib, d) && ok
	}
	if !ok {
		os.Exit(1)
	}
}

func check(sf *scryfall.Client, imp *importer.Client, lib *content.Library, d *content.Deck) bool {
	var problems []string
	add := func(f string, a ...any) { problems = append(problems, fmt.Sprintf(f, a...)) }

	counts := map[string]int{}
	var order []string
	total := 0
	for _, line := range strings.Split(d.List, "\n") {
		t := strings.TrimSpace(line)
		switch strings.ToLower(strings.TrimSuffix(t, ":")) {
		case "", "commander", "deck":
			continue
		}
		n, name, ok := content.CardLine(t)
		if !ok {
			add("unreadable line: %s", t)
			continue
		}
		if counts[name] == 0 {
			order = append(order, name)
		}
		counts[name] += n
		total += n
	}
	if total != 100 {
		add("%d cards, not 100", total)
	}
	cmdrs := content.Commanders(d.List)
	if len(cmdrs) == 0 {
		add("no Commander section")
	}
	for _, m := range d.Modes {
		if lib.Modes[m] == nil {
			add("unknown mode %q", m)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	found, missing, err := sf.Collection(ctx, order)
	if err != nil {
		add("scryfall: %v", err)
	}
	for _, m := range missing {
		add("unknown card: %s", m)
	}

	identity := map[string]bool{}
	for _, c := range cmdrs {
		card, ok := found[c]
		if !ok {
			continue
		}
		for _, col := range card.ColorIdentity {
			identity[col] = true
		}
		if !strings.Contains(card.TypeLine, "Legendary") && !strings.Contains(card.OracleText, "can be your commander") {
			add("commander %s is not legendary", c)
		}
	}
	var gcs []string
	for _, name := range order {
		card, ok := found[name]
		if !ok {
			continue
		}
		if l := card.Legalities["commander"]; l != "legal" {
			add("%s is %s in Commander", name, strings.ReplaceAll(l, "_", " "))
		}
		for _, col := range card.ColorIdentity {
			if !identity[col] {
				add("%s is outside the commander's colour identity (%s)", name, col)
				break
			}
		}
		if counts[name] > 1 && !basics[name] && !anyNumber(card) {
			add("not singleton: %d x %s", counts[name], name)
		}
		if card.GameChanger {
			gcs = append(gcs, name)
		}
	}
	if b, err := strconv.Atoi(strings.TrimSpace(strings.Split(d.Bracket, "-")[0])); err == nil {
		if b <= 2 && len(gcs) > 0 {
			add("bracket %s allows no Game Changers: %s", d.Bracket, strings.Join(gcs, ", "))
		}
		if b == 3 && !strings.Contains(d.Bracket, "-") && len(gcs) > 3 {
			add("bracket 3 allows up to 3 Game Changers, has %d: %s", len(gcs), strings.Join(gcs, ", "))
		}
	} else {
		add("bracket %q is not a number", d.Bracket)
	}

	if imp != nil {
		ictx, icancel := context.WithTimeout(context.Background(), 2*time.Minute)
		piles, issues, err := imp.Build(ictx, d.List)
		icancel()
		if err != nil {
			add("importer: %v", err)
		}
		for _, is := range issues {
			add("importer: %s", is)
		}
		if err == nil && len(piles) < 2 {
			add("importer returned %d piles, want a library and a commander", len(piles))
		}
	}

	if len(problems) == 0 {
		gc := ""
		if len(gcs) > 0 {
			gc = fmt.Sprintf(", Game Changers: %s", strings.Join(gcs, ", "))
		}
		fmt.Printf("%s: OK (100 cards, bracket %s%s)\n", d.Source, d.Bracket, gc)
		return true
	}
	fmt.Printf("%s:\n  - %s\n", d.Source, strings.Join(problems, "\n  - "))
	return false
}

// Relentless Rats, Shadowborn Apostle, Seven Dwarves and friends.
func anyNumber(c scryfall.Card) bool {
	t := strings.ToLower(c.OracleText)
	return strings.Contains(t, "a deck can have any number of cards named") ||
		strings.Contains(t, "a deck can have up to")
}
