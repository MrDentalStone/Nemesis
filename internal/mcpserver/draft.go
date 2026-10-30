package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/MrDentalStone/Nemesis/internal/cards"
	"github.com/MrDentalStone/Nemesis/internal/pad"
)

// Drafting. The pad gets the packs (a set's boosters from the importer, or a
// cube's packs it makes itself), deals them into hand zones and passes them;
// this side checks the set or reads the cube and renders what reaches your
// own hand or pool. Nothing here knows another drafter's pack.

// draftCards remembers mana cost and rarity per card for the session: the
// importer's nicknames carry the type line and mana value but no cost, and
// colours decide most picks.
var draftCards = struct {
	sync.Mutex
	m map[string]draftCard
}{m: map[string]draftCard{}}

type draftCard struct{ Cost, Rarity string }

func rememberCard(name, cost, rarity string) {
	draftCards.Lock()
	draftCards.m[draftKey(name)] = draftCard{Cost: cost, Rarity: rarity}
	draftCards.Unlock()
}

func cardInfo(name string) (draftCard, bool) {
	draftCards.Lock()
	defer draftCards.Unlock()
	c, ok := draftCards.m[draftKey(name)]
	return c, ok
}

// recallCosts looks up the cards the session does not know yet (the server
// restarted mid-draft, or a set Scryfall missed at the start); best effort.
func recallCosts(ctx context.Context, d Deps, lists ...[]pad.RawCard) {
	if d.Scryfall == nil {
		return
	}
	var ask []string
	seen := map[string]bool{}
	for _, list := range lists {
		for _, rc := range list {
			name := cards.Parse(rc.Nick, rc.Desc).Name
			if _, ok := cardInfo(name); ok || name == "" || seen[name] {
				continue
			}
			seen[name] = true
			ask = append(ask, name)
		}
	}
	if len(ask) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	found, _, err := d.Scryfall.Collection(ctx, ask)
	if err != nil {
		return
	}
	for n, c := range found {
		rememberCard(n, c.Cost(), c.Rarity)
	}
}

// draftKey matches a card across Scryfall and the importer: lower case, the
// front face only ("Bonecrusher Giant // Stomp" is "bonecrusher giant").
func draftKey(name string) string {
	name, _, _ = strings.Cut(name, "\n")
	name, _, _ = strings.Cut(name, " // ")
	return strings.ToLower(strings.TrimSpace(name))
}

// setPool is a set's booster cards by rarity (no basics), remembered for
// their mana costs and rarities. The importer builds the packs themselves.
func setPool(ctx context.Context, d Deps, code string) (map[string][]string, error) {
	if d.Scryfall == nil {
		return nil, fmt.Errorf("no Scryfall client")
	}
	list, err := d.Scryfall.SearchAll(ctx, "set:"+code+" is:booster -t:basic", 10)
	if err == nil && len(list) == 0 {
		// older sets have no booster flag on Scryfall
		list, err = d.Scryfall.SearchAll(ctx, "set:"+code+" -t:basic -is:digital", 10)
	}
	if err != nil {
		return nil, err
	}
	pool := map[string][]string{}
	seen := map[string]bool{}
	for _, c := range list {
		if seen[c.Name] {
			continue
		}
		seen[c.Name] = true
		r := c.Rarity
		switch r {
		case "common", "uncommon", "rare", "mythic":
		default:
			r = "rare"
		}
		pool[r] = append(pool[r], c.Name)
		rememberCard(c.Name, c.Cost(), c.Rarity)
	}
	if len(pool["common"]) == 0 {
		return nil, fmt.Errorf("Scryfall has no booster commons for set %q", code)
	}
	return pool, nil
}

var (
	reSetCode = regexp.MustCompile(`^[a-z0-9]{2,6}(-jumpstart)?$`)
	reCount   = regexp.MustCompile(`^(\d+)\s*x?\s+(.+)$`)
	reSetPin  = regexp.MustCompile(`\s+\([A-Za-z0-9]{2,6}\)(\s+\S+)?\s*$`)
	reCubeID  = regexp.MustCompile(`^[A-Za-z0-9_-]{2,64}$`)
	reCubeURL = regexp.MustCompile(`cubecobra\.com/cube/[a-z]+/([A-Za-z0-9_-]+)`)
)

// parseCubeList reads a cube: one card per line, "2 Name" or "2x Name" for
// copies, set pins and section headers and comments ignored.
func parseCubeList(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") || strings.HasSuffix(line, ":") {
			continue
		}
		switch strings.ToLower(line) {
		case "deck", "sideboard", "commander", "maybeboard", "mainboard":
			continue
		}
		n := 1
		if m := reCount.FindStringSubmatch(line); m != nil {
			fmt.Sscan(m[1], &n)
			line = m[2]
		}
		line = strings.TrimSpace(reSetPin.ReplaceAllString(line, ""))
		for i := 0; i < n && line != ""; i++ {
			out = append(out, line)
		}
	}
	return out
}

// cubeURL turns a CubeCobra id or link into its plain-text list; any other
// http(s) link is fetched as it is.
func cubeURL(s string) (string, error) {
	s = strings.TrimSpace(s)
	if m := reCubeURL.FindStringSubmatch(s); m != nil {
		return "https://cubecobra.com/cube/api/cubelist/" + m[1], nil
	}
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return s, nil
	}
	if reCubeID.MatchString(s) {
		return "https://cubecobra.com/cube/api/cubelist/" + s, nil
	}
	return "", fmt.Errorf("cube %q: give a CubeCobra id or link, a URL to a plain list, or the list itself in list", s)
}

func fetchText(ctx context.Context, url string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Nemesis (+https://github.com/MrDentalStone/Nemesis)")
	req.Header.Set("Accept", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", url, resp.Status)
	}
	return string(raw), nil
}

// cubePool reads a cube and looks its cards up for costs; names Scryfall
// does not know stay in (the importer may still build them) and are listed.
func cubePool(ctx context.Context, d Deps, cube, list string) ([]string, []string, error) {
	text := list
	if strings.TrimSpace(text) == "" {
		u, err := cubeURL(cube)
		if err != nil {
			return nil, nil, err
		}
		if text, err = fetchText(ctx, u); err != nil {
			return nil, nil, fmt.Errorf("could not read the cube: %w", err)
		}
	}
	names := parseCubeList(text)
	if len(names) == 0 {
		return nil, nil, fmt.Errorf("the cube list is empty")
	}
	var missing []string
	if d.Scryfall != nil {
		uniq := map[string]bool{}
		var ask []string
		for _, n := range names {
			if !uniq[n] {
				uniq[n] = true
				ask = append(ask, n)
			}
		}
		found, miss, err := d.Scryfall.Collection(ctx, ask)
		if err == nil {
			for n, c := range found {
				rememberCard(n, c.Cost(), c.Rarity)
			}
			missing = miss
		}
	}
	return names, missing, nil
}

// draftFormats names the formats the way the table hears them.
var draftFormats = map[string]string{"booster": "booster draft", "pick_two": "pick-two draft", "winston": "Winston draft"}

func registerDraft(s *mcp.Server, d Deps) {
	type startIn struct {
		Set      string   `json:"set,omitempty" jsonschema:"a set code for set boosters, e.g. 'dsk' or 'mh3' (Scryfall's codes)"`
		Cube     string   `json:"cube,omitempty" jsonschema:"or: a CubeCobra cube id or link, or a URL to a plain list (one card per line)"`
		List     string   `json:"list,omitempty" jsonschema:"or: the cube itself, one card per line ('2 Llanowar Elves' works)"`
		Format   string   `json:"format,omitempty" jsonschema:"auto (default: Winston for 2 drafters, pick-two for 3-4, booster for 5 or more) | booster | pick_two | winston"`
		Packs    int      `json:"packs,omitempty" jsonschema:"packs per drafter (default 3; Winston's stack holds twice that many packs' worth)"`
		PackSize int      `json:"pack_size,omitempty" jsonschema:"cards per pack (default 15)"`
		Seats    []string `json:"seats,omitempty" jsonschema:"the drafting colours, if not every seated player (you are always in)"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "start_draft", Description: "Run a draft at the table: the pad builds the packs on the importer and deals them into every drafter's hand. " +
		"Picks per pass follow the number of drafters (Winston for 2, pick-two for 3-4, booster for 5+) unless format says otherwise. " +
		"Agree the set or cube with the table first and reset_board before it. Waits for the packs (up to 2 min) and returns your first pack or pile. playbook(part=draft) has how to draft."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in startIn) (*mcp.CallToolResult, any, error) {
			n := 0
			for _, v := range []string{in.Set, in.Cube, in.List} {
				if strings.TrimSpace(v) != "" {
					n++
				}
			}
			if n != 1 {
				return nil, nil, fmt.Errorf("give exactly one of set, cube or list")
			}
			args := map[string]any{"format": in.Format, "url": d.Importer.BaseURL}
			if in.Packs > 0 {
				args["packs"] = in.Packs
			}
			if in.PackSize > 0 {
				args["size"] = in.PackSize
			}
			if len(in.Seats) > 0 {
				args["seats"] = in.Seats
			}
			source, note := "", ""
			if in.Set != "" {
				code := strings.ToLower(strings.TrimSpace(in.Set))
				if !reSetCode.MatchString(code) {
					return nil, nil, fmt.Errorf("%q is not a set code (Scryfall's, like dsk or mh3)", in.Set)
				}
				// the importer makes the boosters; Scryfall only adds mana costs
				// and rarities to the pack lines, so a miss there is no failure
				if _, err := setPool(ctx, d, code); err != nil {
					note = "\nScryfall did not answer for this set (" + err.Error() + "): pack lines show mana value only."
				}
				args["set"] = code
				source = strings.ToUpper(code) + " play boosters from the importer (basic lands taken out)"
			} else {
				names, missing, err := cubePool(ctx, d, in.Cube, in.List)
				if err != nil {
					return nil, nil, err
				}
				args["cube"] = names
				source = fmt.Sprintf("a %d-card cube", len(names))
				if len(missing) > 0 {
					note = fmt.Sprintf("\nScryfall did not know %d cube cards (they may still build): %s", len(missing), strings.Join(first(missing, 10), ", "))
				}
			}
			var out struct {
				Format string   `json:"format"`
				Seats  []string `json:"seats"`
				Packs  int      `json:"packs"`
				Size   int      `json:"size"`
				Picks  int      `json:"picks"`
				Stack  int      `json:"stack"`
			}
			if err := d.Pad.Call(ctx, "draft_start", args, &out); err != nil {
				return nil, nil, err
			}
			var b strings.Builder
			fmt.Fprintf(&b, "%s from %s for %s", draftFormats[out.Format], source, strings.Join(out.Seats, ", "))
			if out.Format == "winston" && out.Stack > 0 {
				fmt.Fprintf(&b, ", a %d-card stack", out.Stack)
			} else {
				fmt.Fprintf(&b, ", %d packs of about %d, %d pick(s) a pass", out.Packs, out.Size, out.Picks)
			}
			b.WriteString(note + "\n")
			hit, seen := Await(ctx, d, 2*time.Minute, func(e Event) bool {
				w := e.Str("what")
				return e.Str("ev") == "draft" && (w == "ready" || w == "error")
			})
			if hit == nil {
				b.WriteString("The packs are still building; a [draft] event follows when they are ready." + meanwhile(seen))
				return text(b.String()), nil, nil
			}
			if hit.Str("what") == "error" {
				return nil, nil, fmt.Errorf("the packs could not be built: %s", hit.Str("error"))
			}
			b.WriteString(Render(hit) + "\n")
			more, seen2 := Await(ctx, d, 8*time.Second, func(e Event) bool {
				w := e.Str("what")
				return e.Str("ev") == "draft" && (w == "pack" || w == "winston")
			})
			if more != nil {
				b.WriteString(Render(more) + "\n")
			} else if out.Format == "winston" {
				b.WriteString("The other drafter looks first; your pile comes as a [draft] event.\n")
			}
			return text(b.String() + meanwhile(append(seen, seen2...))), nil, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "draft_view", Description: "The draft as you may see it: format, pack and pick, who the table waits on, the pack in your hand (or your Winston pile on your turn), and your pool with its colours and curve."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			var v draftView
			if err := d.Pad.Call(ctx, "draft_view", nil, &v); err != nil {
				return nil, nil, err
			}
			recallCosts(ctx, d, v.Pool, v.Pack, v.Cards)
			return text(renderDraftView(&v)), nil, nil
		})

	type pickIn struct {
		GUIDs []string `json:"guids,omitempty" jsonschema:"booster and pick-two: the card(s) you take from the pack in your hand"`
		Take  *bool    `json:"take,omitempty" jsonschema:"Winston: true takes the pile you are looking at, false passes it (it gets a card and you look at the next pile; passing the last pile takes the stack's top card)"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "draft_pick", Description: "Make your draft pick. Booster and pick-two: guids from the [draft] pack. Winston: take=true or false. " +
		"Picks go face-down to your pool on your mat; the next pack comes as a [draft] event (this waits up to 20 s for it)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in pickIn) (*mcp.CallToolResult, any, error) {
			args := map[string]any{}
			if in.Take != nil {
				args["take"] = *in.Take
			} else {
				if len(in.GUIDs) == 0 {
					return nil, nil, fmt.Errorf("give guids (booster, pick-two) or take (Winston)")
				}
				args["guids"] = in.GUIDs
			}
			var out struct {
				Picked    []string      `json:"picked"`
				Pool      int           `json:"pool"`
				WaitingOn []string      `json:"waitingOn"`
				Took      []pad.RawCard `json:"took"`
				Passed    bool          `json:"passed"`
				Pile      int           `json:"pile"`
				Sizes     string        `json:"sizes"`
				Stack     int           `json:"stack"`
				Cards     []pad.RawCard `json:"cards"`
				Blind     *pad.RawCard  `json:"blind"`
			}
			if err := d.Pad.Call(ctx, "draft_pick", args, &out); err != nil {
				return nil, nil, err
			}
			var b strings.Builder
			switch {
			case in.Take != nil && *in.Take:
				fmt.Fprintf(&b, "Took the pile (%d): %s. The other drafter is up; your next pile comes as a [draft] event.\n", len(out.Took), rawNames(out.Took))
				return text(b.String()), nil, nil
			case out.Passed && out.Pile > 0:
				fmt.Fprintf(&b, "Passed. Now looking at pile %d (piles %s, stack %d):\n", out.Pile, out.Sizes, out.Stack)
				for _, c := range out.Cards {
					b.WriteString(draftCardLine(c) + "\n")
				}
				b.WriteString("Take it or pass with draft_pick(take=...).\n")
				return text(b.String()), nil, nil
			case out.Passed:
				if out.Blind != nil {
					fmt.Fprintf(&b, "Passed the last pile and took the stack's top card: %s\n", strings.TrimPrefix(draftCardLine(*out.Blind), "  - "))
				} else {
					b.WriteString("Passed the last pile; the stack was empty, so nothing more this turn.\n")
				}
				return text(b.String()), nil, nil
			}
			fmt.Fprintf(&b, "Picked %s (pool %d).", strings.Join(out.Picked, ", "), out.Pool)
			if len(out.WaitingOn) > 0 {
				fmt.Fprintf(&b, " Waiting on %s.", strings.Join(out.WaitingOn, ", "))
			}
			b.WriteString("\n")
			hit, seen := Await(ctx, d, 20*time.Second, func(e Event) bool {
				w := e.Str("what")
				return e.Str("ev") == "draft" && (w == "pack" || w == "done")
			})
			if hit != nil {
				b.WriteString(Render(hit) + "\n")
			} else {
				b.WriteString("The next pack comes as a [draft] event: wait_for_event.\n")
			}
			return text(b.String() + meanwhile(seen)), nil, nil
		})

	type buildIn struct {
		Main   []string       `json:"main" jsonschema:"guids of the pool cards for your main deck (draft_view lists them); the rest becomes your sideboard"`
		Basics map[string]int `json:"basics,omitempty" jsonschema:"basic lands to add, e.g. {\"Forest\": 9, \"Mountain\": 8}"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "draft_build", Description: "After the draft: your chosen pool cards plus basic lands become your library (shuffled), the rest your sideboard beside it, " +
		"your life goes to 20 and your opening 7 is dealt. Limited decks are 40 cards or more, usually 17 lands."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in buildIn) (*mcp.CallToolResult, any, error) {
			total := len(in.Main)
			for name, k := range in.Basics {
				switch name {
				case "Plains", "Island", "Swamp", "Mountain", "Forest", "Wastes":
				default:
					return nil, nil, fmt.Errorf("%q is not a basic land", name)
				}
				total += k
			}
			if total < 40 {
				return nil, nil, fmt.Errorf("that is %d cards; a limited deck has at least 40", total)
			}
			var out struct {
				Main    int `json:"main"`
				Missing int `json:"missing"`
			}
			if err := d.Pad.Call(ctx, "draft_build", map[string]any{"main": in.Main, "basics": in.Basics, "url": d.Importer.BaseURL}, &out); err != nil {
				return nil, nil, err
			}
			resetCommanderTax()
			hit, seen := Await(ctx, d, time.Minute, func(e Event) bool { return e.Str("ev") == "draft" && e.Str("what") == "built" })
			var b strings.Builder
			if out.Missing > 0 {
				fmt.Fprintf(&b, "%d of your main guids were not in your pool (they were left out).\n", out.Missing)
			}
			if hit == nil {
				b.WriteString("The basics are still building; when the [draft] built event comes, draw(7, reason='opening hand')." + meanwhile(seen))
				return text(b.String()), nil, nil
			}
			b.WriteString(Render(hit) + "\n")
			if err := sleepCtx(ctx, 2*time.Second); err != nil {
				return nil, nil, err
			}
			var drew struct {
				Drew int `json:"drew"`
			}
			if err := d.Pad.Call(ctx, "draw", map[string]any{"n": 7}, &drew); err != nil {
				b.WriteString("Could not deal the opening hand: " + err.Error() + "\n")
				return text(b.String() + meanwhile(seen)), nil, nil
			}
			b.WriteString("Life set to 20. Dealt your opening 7 (do not draw again). Keep or mulligan, and say which. " +
				"The others set their own life to 20; nobody has a commander.\n")
			if h, err := dealt(ctx, d, 7); err == nil {
				renderHand(&b, h)
			}
			return text(b.String() + meanwhile(seen)), nil, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "cancel_draft", Description: "Call the draft off (the table agreed to stop): the pad stops dealing and passing, and its cards stay where they are."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			var out struct {
				Cancelled bool `json:"cancelled"`
			}
			if err := d.Pad.Call(ctx, "draft_cancel", nil, &out); err != nil {
				return nil, nil, err
			}
			if !out.Cancelled {
				return text("no draft was on"), nil, nil
			}
			return text("draft cancelled"), nil, nil
		})
}

func first(list []string, n int) []string {
	if len(list) > n {
		return append(list[:n:n], fmt.Sprintf("and %d more", len(list)-n))
	}
	return list
}

func rawNames(cs []pad.RawCard) string {
	var out []string
	for _, c := range cs {
		out = append(out, cards.Parse(c.Nick, c.Desc).Name)
	}
	return strings.Join(out, ", ")
}

// draftCardLine is one card of a pack, pile or pool: what a pick needs.
func draftCardLine(rc pad.RawCard) string {
	c := cards.Parse(rc.Nick, rc.Desc)
	var b strings.Builder
	fmt.Fprintf(&b, "  - [%s] %s", rc.GUID, c.Name)
	info, ok := cardInfo(c.Name)
	switch {
	case ok && info.Cost != "":
		b.WriteString(" " + info.Cost)
	case c.ManaCost != "":
		b.WriteString(" " + c.ManaCost)
	case c.CMC > 0:
		fmt.Fprintf(&b, " (mv %d)", c.CMC)
	}
	if c.TypeLine != "" {
		b.WriteString(" — " + c.TypeLine)
	}
	if c.PT != "" {
		b.WriteString(" " + c.PT)
	}
	if ok && (info.Rarity == "rare" || info.Rarity == "mythic") {
		b.WriteString(" (" + info.Rarity + ")")
	}
	if c.Oracle != "" {
		b.WriteString(": " + oneLineText(c.Oracle, 220))
	}
	return b.String()
}

func parseCardList(raw string) []pad.RawCard {
	var list []pad.RawCard
	_ = json.Unmarshal([]byte(raw), &list)
	return list
}

func renderDraftEvent(e Event) string {
	s := e.Str
	n := func(k string) int { f, _ := e[k].(float64); return int(f) }
	var b strings.Builder
	switch s("what") {
	case "ready":
		fmt.Fprintf(&b, "[draft] the packs are ready: %s for %s", draftFormats[s("format")], s("seats"))
		if p := s("problems"); p != "" {
			b.WriteString(" (problems: " + p + ")")
		}
		if f := s("first"); f != "" {
			b.WriteString("; " + f + " picks first")
		}
	case "pack":
		fmt.Fprintf(&b, "[draft] pack %d of %d, pick %d: take %d with draft_pick(guids) (your pool: %d)", n("round"), n("rounds"), n("pick"), n("need"), n("pool"))
		for _, c := range parseCardList(s("cards")) {
			b.WriteString("\n" + draftCardLine(c))
		}
	case "winston":
		fmt.Fprintf(&b, "[draft] Winston, your turn: pile %d (piles %s, stack %d, your pool %d). draft_pick(take=true) takes it, take=false passes", n("pile"), s("sizes"), n("stack"), n("pool"))
		for _, c := range parseCardList(s("cards")) {
			b.WriteString("\n" + draftCardLine(c))
		}
	case "took":
		if e.Bool("blind") {
			fmt.Fprintf(&b, "[draft] %s passed every pile and took the stack's top card", s("color"))
		} else {
			fmt.Fprintf(&b, "[draft] %s took pile %d (%d cards)", s("color"), n("pile"), n("n"))
		}
	case "done":
		fmt.Fprintf(&b, "[draft] the draft is over (your pool: %d cards): draft_view, then draft_build your deck", n("pool"))
	case "built":
		fmt.Fprintf(&b, "[draft] your deck is built: %d pool cards and %d basics in your library (%d cards), %d in your sideboard", n("main"), n("basics"), n("library"), n("sideboard"))
		if p := s("problems"); p != "" {
			b.WriteString(" (problems: " + p + ")")
		}
	case "error":
		b.WriteString("[draft] the packs could NOT be built: " + s("error"))
	default:
		raw, _ := json.Marshal(e)
		b.Write(raw)
	}
	return b.String()
}

type draftView struct {
	Draft     bool          `json:"draft"`
	Format    string        `json:"format"`
	Phase     string        `json:"phase"`
	Round     int           `json:"round"`
	Rounds    int           `json:"rounds"`
	Pick      int           `json:"pick"`
	Picks     int           `json:"picks"`
	Seats     []string      `json:"seats"`
	WaitingOn []string      `json:"waitingOn"`
	Pool      []pad.RawCard `json:"pool"`
	Pack      []pad.RawCard `json:"pack"`
	Need      int           `json:"need"`
	Turn      string        `json:"turn"`
	Pile      int           `json:"pile"`
	Sizes     string        `json:"sizes"`
	Stack     int           `json:"stack"`
	Cards     []pad.RawCard `json:"cards"`
}

func renderDraftView(v *draftView) string {
	if !v.Draft {
		return "No draft on the table. start_draft runs one."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s for %s: ", draftFormats[v.Format], strings.Join(v.Seats, ", "))
	switch {
	case v.Phase == "building":
		b.WriteString("building the packs\n")
	case v.Phase == "done":
		b.WriteString("over, build your deck with draft_build\n")
	case v.Format == "winston":
		fmt.Fprintf(&b, "%s's turn, pile %d (piles %s, stack %d)\n", v.Turn, v.Pile, v.Sizes, v.Stack)
	default:
		fmt.Fprintf(&b, "pack %d of %d, pick %d", v.Round, v.Rounds, v.Pick)
		if len(v.WaitingOn) > 0 {
			fmt.Fprintf(&b, ", waiting on %s", strings.Join(v.WaitingOn, ", "))
		}
		b.WriteString("\n")
	}
	if len(v.Pack) > 0 {
		fmt.Fprintf(&b, "Your pack (take %d):\n", v.Need)
		for _, c := range v.Pack {
			b.WriteString(draftCardLine(c) + "\n")
		}
	}
	if len(v.Cards) > 0 {
		fmt.Fprintf(&b, "Pile %d, yours to take or pass:\n", v.Pile)
		for _, c := range v.Cards {
			b.WriteString(draftCardLine(c) + "\n")
		}
	}
	b.WriteString(poolSummary(v.Pool))
	return b.String()
}

// poolSummary lists your picks with the colours and curve a deck is built
// from: coloured mana symbols per colour, and non-land cards by mana value.
func poolSummary(pool []pad.RawCard) string {
	if len(pool) == 0 {
		return "Your pool is empty.\n"
	}
	sorted := append([]pad.RawCard(nil), pool...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return cards.Parse(sorted[i].Nick, sorted[i].Desc).Name < cards.Parse(sorted[j].Nick, sorted[j].Desc).Name
	})
	pips := map[rune]int{}
	curve := map[int]int{}
	creatures, lands := 0, 0
	var lines []string
	for _, rc := range sorted {
		c := cards.Parse(rc.Nick, rc.Desc)
		lines = append(lines, draftCardLine(pad.RawCard{GUID: rc.GUID, Nick: rc.Nick}))
		if strings.Contains(c.TypeLine, "Land") {
			lands++
			continue
		}
		if strings.Contains(c.TypeLine, "Creature") {
			creatures++
		}
		cost := c.ManaCost
		if info, ok := cardInfo(c.Name); ok && info.Cost != "" {
			cost = info.Cost
		}
		for _, r := range cost {
			if strings.ContainsRune("WUBRG", r) {
				pips[r]++
			}
		}
		mv := c.CMC
		if cost != "" {
			mv = manaIn(cost)
		}
		curve[min(mv, 6)]++
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Your pool (%d cards: %d creatures, %d lands):\n", len(pool), creatures, lands)
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
	var col []string
	for _, r := range "WUBRG" {
		if pips[r] > 0 {
			col = append(col, fmt.Sprintf("%c %d", r, pips[r]))
		}
	}
	var cv []string
	for mv := 0; mv <= 6; mv++ {
		if curve[mv] > 0 {
			label := fmt.Sprint(mv)
			if mv == 6 {
				label = "6+"
			}
			cv = append(cv, fmt.Sprintf("%s: %d", label, curve[mv]))
		}
	}
	fmt.Fprintf(&b, "Coloured pips: %s. Curve (non-land): %s\n", strings.Join(col, ", "), strings.Join(cv, ", "))
	return b.String()
}
