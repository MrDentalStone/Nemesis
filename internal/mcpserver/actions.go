package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/MrDentalStone/Nemesis/internal/cards"
	"github.com/MrDentalStone/Nemesis/internal/content"
	"github.com/MrDentalStone/Nemesis/internal/importer"
)

// registerActions adds the tools that move your own objects. Every one
// of them is refused by the pad unless the pad has a seat and isn't STOPped.
func registerActions(s *mcp.Server, d Deps) {
	call := func(ctx context.Context, op string, params map[string]any) (string, error) {
		var out map[string]any
		if err := d.Pad.Call(ctx, op, params, &out); err != nil {
			return "", err
		}
		b, _ := json.Marshal(out)
		return string(b), nil
	}
	simple := func(name, desc, op string) {
		mcp.AddTool(s, &mcp.Tool{Name: name, Description: desc},
			func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
				r, err := call(ctx, op, nil)
				if err != nil {
					return nil, nil, err
				}
				return text(r), nil, nil
			})
	}

	type drawIn struct {
		N      int    `json:"n,omitempty" jsonschema:"cards to draw (default 1)"`
		Reason string `json:"reason,omitempty" jsonschema:"the card or rule that makes you draw, e.g. 'Ponder' (needed after begin_turn, which already drew for the turn)"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "draw", Description: "Draw n cards for a card effect. Not for your turn's draw (begin_turn does it) or your opening hand (import_deck deals it). Returns the hand."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in drawIn) (*mcp.CallToolResult, any, error) {
			n := in.N
			if n <= 0 {
				n = 1
			}
			turnState.Lock()
			drew := turnState.drew
			turnState.Unlock()
			if drew && strings.TrimSpace(in.Reason) == "" {
				return nil, nil, fmt.Errorf("not drawn: begin_turn already drew your card for this turn. Only if a card makes you draw, call draw again with reason set to that card")
			}
			r, err := call(ctx, "draw", map[string]any{"n": n})
			if err != nil {
				return nil, nil, err
			}
			if !drew {
				setDrew(true) // the turn's draw, held back by begin_turn
			}
			// deal() animates; the hand zone only owns the cards once they land
			select {
			case <-time.After(1800 * time.Millisecond):
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
			h, err := d.Pad.Hand(ctx)
			if err != nil {
				return text(r), nil, nil
			}
			var b strings.Builder
			b.WriteString(r + "\n")
			fmt.Fprintf(&b, "Hand of %s (%d cards)\n", h.Seat, len(h.Cards))
			for _, rc := range h.Cards {
				b.WriteString(renderCard(rc.Parse(), rc.Zone))
			}
			return text(b.String()), nil, nil
		})

	type beginIn struct {
		SkipDraw bool `json:"skip_draw,omitempty" jsonschema:"skip the draw (the first player's first turn in a 2-player game)"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "begin_turn", Description: "Start your turn in one call: untap everything, draw 1 (unless skip_draw), then return your hand and the whole table. If a card on the table acts on untapping, upkeep or the draw step (Mesmeric Orb, Maralen), the draw is held and listed so you resolve those first. Say your upkeep triggers yourself."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in beginIn) (*mcp.CallToolResult, any, error) {
			var b strings.Builder
			// the pad must know it is your turn, or end_turn refuses later
			// (a turn handed over in chat never went through the marker)
			if _, err := call(ctx, "start_turn", nil); err != nil {
				return nil, nil, err
			}
			setDrew(true)
			r, err := call(ctx, "untap", map[string]any{"all": true})
			if err != nil {
				return nil, nil, err
			}
			var untapped struct {
				Untapped []string `json:"untapped"`
			}
			_ = json.Unmarshal([]byte(r), &untapped)
			fmt.Fprintf(&b, "Untapped everything (%d permanents were tapped).\n", len(untapped.Untapped))
			// setRotationSmooth takes about a second; a snapshot read before it
			// lands still shows the cards tapped
			select {
			case <-time.After(1200 * time.Millisecond):
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
			var held []string
			if !in.SkipDraw {
				if snap, err := d.Pad.Snapshot(ctx); err == nil {
					held = beforeDraw(snap)
				}
			}
			if len(held) > 0 {
				setDrew(false)
				b.WriteString("NOT drawn yet. Before your draw, these act on your untap step, upkeep or draw:\n")
				for _, h := range held {
					b.WriteString("  - " + h + "\n")
				}
				b.WriteString("Say and resolve them now (mills, life loss, a replaced draw), then take your turn's draw with draw() unless one of them replaces it.\n")
			} else if !in.SkipDraw {
				if _, err := call(ctx, "draw", map[string]any{"n": 1}); err != nil {
					return nil, nil, err
				}
				// deal() animates; the hand zone only owns the card once it lands
				select {
				case <-time.After(1800 * time.Millisecond):
				case <-ctx.Done():
					return nil, nil, ctx.Err()
				}
				b.WriteString("Drew 1.\n")
			}
			h, err := d.Pad.Hand(ctx)
			if err != nil {
				return nil, nil, err
			}
			fmt.Fprintf(&b, "\nHand of %s (%d cards)\n", h.Seat, len(h.Cards))
			for _, rc := range h.Cards {
				b.WriteString(renderCard(rc.Parse(), rc.Zone))
			}
			snap, err := d.Pad.Snapshot(ctx)
			if err != nil {
				return nil, nil, err
			}
			b.WriteString("\n" + renderSnapshot(snap))
			if len(held) > 0 {
				b.WriteString("\n(That is your hand and the table. Do not untap again; your turn's draw is still to come, after the cards listed at the top.)")
			} else {
				b.WriteString("\n(That is your hand and the table: no need for my_hand or table_state now. Do not untap or draw again.)")
			}
			return text(b.String()), nil, nil
		})

	simple("shuffle_library", "Shuffle your library (forgets anything look saw).", "shuffle")
	simple("reset_board", "NEW GAME: destroy all of your cards, tokens and counter tiles in your zones and hands, set your life to 40 and zero the commander damage you have taken (via the table's own reset). Then import_deck. Never touches a human's area.", "reset_board")
	simple("mulligan", "Return your hand to the library, shuffle, draw 7. Then bottom cards with move(to=library_bottom).", "mulligan")

	type playIn struct {
		GUID string `json:"guid" jsonschema:"card guid from my_hand or table_state (command zone)"`
		Row  string `json:"row,omitempty" jsonschema:"land | creature | other | stack (default other). Use stack for instants/sorceries and anything that should be announced before it resolves."`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "play", Description: "Put a card from your hand or command zone onto your battlefield (land/creature/other row) or onto the stack spot. Say what you're doing first."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in playIn) (*mcp.CallToolResult, any, error) {
			r, err := call(ctx, "play", map[string]any{"guid": in.GUID, "row": in.Row})
			if err != nil {
				return nil, nil, err
			}
			return text(r), nil, nil
		})

	type guidsIn struct {
		GUIDs []string `json:"guids" jsonschema:"guids of permanents on your battlefield"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "tap", Description: "Tap permanents on your battlefield (mana, attackers, abilities)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in guidsIn) (*mcp.CallToolResult, any, error) {
			r, err := call(ctx, "tap", map[string]any{"guids": in.GUIDs})
			if err != nil {
				return nil, nil, err
			}
			return text(r), nil, nil
		})

	type untapIn struct {
		GUIDs []string `json:"guids,omitempty" jsonschema:"specific permanents to untap"`
		All   bool     `json:"all,omitempty" jsonschema:"untap everything on your battlefield (untap step)"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "untap", Description: "Untap your permanents - specific guids, or all=true for the untap step."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in untapIn) (*mcp.CallToolResult, any, error) {
			r, err := call(ctx, "untap", map[string]any{"guids": in.GUIDs, "all": in.All})
			if err != nil {
				return nil, nil, err
			}
			return text(r), nil, nil
		})

	type guidIn struct {
		GUID string `json:"guid"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "flip", Description: "Flip one of your cards (transform DFCs, morph face-up, etc.)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in guidIn) (*mcp.CallToolResult, any, error) {
			r, err := call(ctx, "flip", map[string]any{"guid": in.GUID})
			if err != nil {
				return nil, nil, err
			}
			return text(r), nil, nil
		})

	type moveIn struct {
		GUID string `json:"guid" jsonschema:"one of your objects (hand, battlefield, graveyard, exile, command zone, or a library card from look)"`
		To   string `json:"to" jsonschema:"graveyard | exile | command | hand | stack | battlefield | library_top | library_bottom"`
		Row  string `json:"row,omitempty" jsonschema:"when to=battlefield: land | creature | other"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "move", Description: "Move one of your objects between zones: resolve a spell from the stack to the graveyard/battlefield, discard, exile, return to hand, put on top/bottom of library, commander back to the command zone."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in moveIn) (*mcp.CallToolResult, any, error) {
			r, err := call(ctx, "move", map[string]any{"guid": in.GUID, "to": in.To, "row": in.Row})
			if err != nil {
				return nil, nil, err
			}
			return text(r), nil, nil
		})

	type searchIn struct {
		Query     string `json:"query" jsonschema:"case-insensitive text matched against card name and type line, e.g. 'forest', 'basic land', 'Eternal Witness'"`
		N         int    `json:"n,omitempty" jsonschema:"how many matching cards to take (default 1)"`
		To        string `json:"to,omitempty" jsonschema:"battlefield (default) or hand"`
		Row       string `json:"row,omitempty" jsonschema:"battlefield row: land, creature, other (default other)"`
		Tapped    bool   `json:"tapped,omitempty" jsonschema:"put onto the battlefield tapped"`
		Desc      bool   `json:"desc,omitempty" jsonschema:"also match the oracle text"`
		NoShuffle bool   `json:"no_shuffle,omitempty" jsonschema:"skip the shuffle afterwards (almost never)"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "search_library", Description: "Search your OWN library for cards matching a query (tutors, ramp like Cultivate/Wood Elves/Skyshroud Claim) and put them onto the battlefield or into hand, then shuffle. Reports only the cards taken and the number of matches. Only when a card effect allows it; say which card."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in searchIn) (*mcp.CallToolResult, any, error) {
			n := in.N
			if n <= 0 {
				n = 1
			}
			var out struct {
				Taken []struct {
					GUID, Nick, Desc string
				} `json:"taken"`
				Matched  int    `json:"matched"`
				To       string `json:"to"`
				Shuffled bool   `json:"shuffled"`
			}
			args := map[string]any{"query": in.Query, "n": n, "to": in.To, "row": in.Row, "tapped": in.Tapped, "desc": in.Desc, "noShuffle": in.NoShuffle}
			if err := d.Pad.Call(ctx, "search", args, &out); err != nil {
				return nil, nil, err
			}
			var b strings.Builder
			fmt.Fprintf(&b, "%d cards matched; took %d to %s (shuffled=%v)\n", out.Matched, len(out.Taken), or(out.To, "battlefield"), out.Shuffled)
			for _, c := range out.Taken {
				fmt.Fprintf(&b, "- [%s] %s\n", c.GUID, cards.Parse(c.Nick, c.Desc).Name)
			}
			return text(b.String()), nil, nil
		})

	type markIn struct {
		GUID           string   `json:"guid" jsonschema:"one of your cards on the battlefield"`
		PlusOne        int      `json:"plus_one,omitempty" jsonschema:"delta of +1/+1 counters (negative removes; -1/-1 counters are negative +1/+1 here)"`
		Counters       int      `json:"counters,omitempty" jsonschema:"delta of generic named counters (charge, oil, time...)"`
		Loyalty        int      `json:"loyalty,omitempty" jsonschema:"delta of loyalty on a planeswalker"`
		Power          int      `json:"power,omitempty" jsonschema:"delta to power (non-counter effects you want shown)"`
		Toughness      int      `json:"toughness,omitempty" jsonschema:"delta to toughness"`
		Keywords       []string `json:"keywords,omitempty" jsonschema:"keyword icons to add: flying, first strike, double strike, deathtouch, lifelink, trample, vigilance, menace, reach, haste, hexproof, indestructible, defender, frozen, monstrous, stun, exert, renowned, suspend"`
		RemoveKeywords bool     `json:"remove_keywords,omitempty" jsonschema:"remove the listed keywords instead of adding them"`
		Note           *string  `json:"note,omitempty" jsonschema:"set the card's notepad text (empty string clears)"`
		Phased         *bool    `json:"phased,omitempty" jsonschema:"true = phased out marker on, false = off"`
		Token          *bool    `json:"token,omitempty" jsonschema:"true = mark as a token"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "mark", Description: "Set ON-CARD state on one of your cards the way the table's right-click menu does: +1/+1 counters, generic counters, loyalty, P/T tweaks, keyword icons, a notepad note, phased out, token marker. Prefer this over counter tiles; table_state shows it as 'on-card:'. Deltas add to the current value."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in markIn) (*mcp.CallToolResult, any, error) {
			args := map[string]any{"guid": in.GUID, "plus_one": in.PlusOne, "counters": in.Counters, "loyalty": in.Loyalty,
				"power": in.Power, "toughness": in.Toughness, "keywords": in.Keywords, "remove_keywords": in.RemoveKeywords}
			if in.Note != nil {
				args["note"] = *in.Note
			}
			if in.Phased != nil {
				args["phased"] = *in.Phased
			}
			if in.Token != nil {
				args["token"] = *in.Token
			}
			r, err := call(ctx, "mark", args)
			if err != nil {
				return nil, nil, err
			}
			return text(r), nil, nil
		})

	type revealUntilIn struct {
		Query  string `json:"query" jsonschema:"text matched against name and type line of each revealed card, e.g. 'creature', 'land', 'instant'"`
		MaxCMC int    `json:"max_cmc,omitempty" jsonschema:"cascade: only a card with mana value LESS than this counts as the hit"`
		Max    int    `json:"max,omitempty" jsonschema:"stop after this many cards (default 20)"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "reveal_until", Description: "Reveal cards from the top of your library until one matches the query (cascade, 'reveal until a creature', Gnawbone-style effects). The revealed cards are laid face-up on the stack row and listed; then move() them as the effect says (cast the hit, library_bottom the rest, etc.). Only when a card effect allows it; say which."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in revealUntilIn) (*mcp.CallToolResult, any, error) {
			var out struct {
				Revealed []struct {
					GUID, Nick, Desc string
					CMC              int `json:"cmc"`
				} `json:"revealed"`
				Hit *struct {
					GUID, Nick, Desc string
					CMC              int `json:"cmc"`
				} `json:"hit"`
				Note string `json:"note"`
			}
			args := map[string]any{"query": in.Query, "max": in.Max}
			if in.MaxCMC > 0 {
				args["max_cmc"] = in.MaxCMC
			}
			if err := d.Pad.Call(ctx, "reveal_until", args, &out); err != nil {
				return nil, nil, err
			}
			var b strings.Builder
			fmt.Fprintf(&b, "revealed %d cards (top first):\n", len(out.Revealed))
			for _, c := range out.Revealed {
				fmt.Fprintf(&b, "- [%s] %s (mv %d)\n", c.GUID, cards.Parse(c.Nick, c.Desc).Name, c.CMC)
			}
			if out.Hit != nil {
				fmt.Fprintf(&b, "HIT: [%s] %s\n", out.Hit.GUID, cards.Parse(out.Hit.Nick, out.Hit.Desc).Name)
			} else {
				b.WriteString("no match\n")
			}
			b.WriteString(out.Note + "\n")
			return text(b.String()), nil, nil
		})

	type lookIn struct {
		N int `json:"n,omitempty" jsonschema:"how many cards from the top (default 1)"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "look", Description: "Look at the top n cards of your OWN library (scry, surveil, 'look at the top card'). The cards are taken off the top into your hidden secondary hand zone; put them back with move(to=library_top) in the order you want (last moved = top) or move(to=library_bottom), or keep them where the effect allows. Only use when a card effect allows it, and say so."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in lookIn) (*mcp.CallToolResult, any, error) {
			n := in.N
			if n <= 0 {
				n = 1
			}
			var out struct {
				Cards []struct {
					GUID, Nick, Desc string
				} `json:"cards"`
			}
			if err := d.Pad.Call(ctx, "look", map[string]any{"n": n}, &out); err != nil {
				return nil, nil, err
			}
			var b strings.Builder
			fmt.Fprintf(&b, "Top %d of library (top first):\n", len(out.Cards))
			for _, c := range out.Cards {
				pc := cards.Parse(c.Nick, c.Desc)
				fmt.Fprintf(&b, "- [%s] %s — %s\n", c.GUID, pc.Name, pc.TypeLine)
			}
			return text(b.String()), nil, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "reveal", Description: "Reveal cards (from hand, library via look, etc.) to the whole table: announced in chat and laid face-up on the stack spot. Move them back afterwards."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in guidsIn) (*mcp.CallToolResult, any, error) {
			r, err := call(ctx, "reveal", map[string]any{"guids": in.GUIDs})
			if err != nil {
				return nil, nil, err
			}
			return text(r), nil, nil
		})

	type lifeIn struct {
		Color string `json:"color" jsonschema:"whose life total: a seat colour (White, Red, Yellow, Green, Blue, Purple...)"`
		Delta int    `json:"delta" jsonschema:"change, negative for damage/loss"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "adjust_life", Description: "Change any player's life total on their tracker (damage you deal, life you gain or lose). Announced in chat automatically. Only after the table agrees on the damage."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in lifeIn) (*mcp.CallToolResult, any, error) {
			r, err := call(ctx, "life", map[string]any{"color": in.Color, "delta": in.Delta})
			if err != nil {
				return nil, nil, err
			}
			return text(r), nil, nil
		})

	type resetIn struct {
		Color string `json:"color" jsonschema:"a seat colour (White, Red, Yellow, Green, Blue, Purple...)"`
		Value int    `json:"value,omitempty" jsonschema:"new life total (default 40)"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "reset_life", Description: "Set a seat's life tracker for a new game (default 40). Ask before touching a human's tracker."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in resetIn) (*mcp.CallToolResult, any, error) {
			v := in.Value
			if v <= 0 {
				v = 40
			}
			r, err := call(ctx, "reset_life", map[string]any{"color": in.Color, "value": v})
			if err != nil {
				return nil, nil, err
			}
			return text(r), nil, nil
		})

	type cmdrIn struct {
		From  string `json:"from" jsonschema:"colour whose commander dealt the damage"`
		To    string `json:"to" jsonschema:"colour that took it"`
		Delta int    `json:"delta"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "commander_damage", Description: "Record commander damage on the table's trackers (21 total from one commander = dead). The table deducts it from the victim's life automatically (cmdrDamageAutoLife), so do NOT also adjust_life for the same damage; the reply says lifeAutoDeducted."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in cmdrIn) (*mcp.CallToolResult, any, error) {
			r, err := call(ctx, "cmdr_damage", map[string]any{"from": in.From, "to": in.To, "delta": in.Delta})
			if err != nil {
				return nil, nil, err
			}
			return text(r), nil, nil
		})

	type counterIn struct {
		GUID string `json:"guid" jsonschema:"one of your permanents"`
		Kind string `json:"kind,omitempty" jsonschema:"bag name on the table: '+X/+Y Counter' (default), 'Counter', 'Poison Counter', 'Loyalty Counter', 'Flying', 'Deathtouch', 'Lifelink', 'Haste', 'Monstrous', ... (keyword bags stick a keyword on the card)"`
		N    int    `json:"n,omitempty" jsonschema:"how many (default 1)"`
		P    *int   `json:"p,omitempty" jsonschema:"for a +X/+Y tile: the power value to show (e.g. 3 for +3/+3, or 13 to label a 13/13 Hydra)"`
		T    *int   `json:"t,omitempty" jsonschema:"for a +X/+Y tile: the toughness value to show"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "counter", Description: "Drop counters/keyword tokens from the table's bags onto one of your permanents. A +X/+Y tile can be set to a value with p/t (one tile reading +3/+3 is how the table tracks three counters)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in counterIn) (*mcp.CallToolResult, any, error) {
			n := in.N
			if n <= 0 {
				n = 1
			}
			args := map[string]any{"guid": in.GUID, "kind": in.Kind, "n": n}
			if in.P != nil {
				args["p"] = *in.P
			}
			if in.T != nil {
				args["t"] = *in.T
			}
			r, err := call(ctx, "counter", args)
			if err != nil {
				return nil, nil, err
			}
			return text(r), nil, nil
		})

	type tokenIn struct {
		Query string `json:"query" jsonschema:"Scryfall SEARCH SYNTAX for the token (t:token is added for you). Use an EXACT name with ! plus -is:dfc so a double-faced or similarly named token is not picked: '!Treasure -is:dfc', '!Food -is:dfc', '!Beast pow=3 tou=3 c:g -is:dfc', '!Plant pow=0 tou=1 -is:dfc'. Plain words like 'Beast 3/3' do not work."`
		N     int    `json:"n,omitempty" jsonschema:"how many copies (default 1)"`
		Row   string `json:"row,omitempty" jsonschema:"creature | other (default creature)"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "spawn_token", Description: "Create token cards on your battlefield: found on Scryfall, built by the deck importer."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in tokenIn) (*mcp.CallToolResult, any, error) {
			found, err := d.Scryfall.Search(ctx, "t:token "+in.Query, "")
			if err != nil {
				return nil, nil, err
			}
			if len(found) == 0 {
				return nil, nil, fmt.Errorf("no token matched %q on Scryfall", in.Query)
			}
			tok := found[0]
			line := fmt.Sprintf("1 %s (%s) %s", tok.Name, strings.ToUpper(tok.Set), tok.CollectorNumber)
			piles, issues, err := d.Importer.Build(ctx, line)
			if err != nil {
				return nil, nil, err
			}
			if len(piles) == 0 {
				return nil, nil, fmt.Errorf("the importer could not build %q (%s)", line, strings.Join(issues, "; "))
			}
			card, nick, err := importer.FirstCard(piles[0])
			if err != nil {
				return nil, nil, err
			}
			n := in.N
			if n <= 0 {
				n = 1
			}
			var guids []string
			for i := 0; i < n; i++ {
				var out struct {
					GUID string `json:"guid"`
				}
				if err := d.Pad.Call(ctx, "token", map[string]any{"json": string(card), "row": in.Row}, &out); err != nil {
					return nil, nil, err
				}
				guids = append(guids, out.GUID)
			}
			return text(fmt.Sprintf("spawned %d x %s: %s", n, cards.Parse(nick, "").Name, strings.Join(guids, ", "))), nil, nil
		})

	type importIn struct {
		Deck string `json:"deck,omitempty" jsonschema:"a deck id from list_decks (preferred)"`
		Data string `json:"data,omitempty" jsonschema:"or: a decklist (a 'Commander' header line for the commander, 'Deck' for the 99) or an Archidekt/Moxfield/Scryfall deck URL"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "import_deck", Description: "Build a deck with the table's deck importer, place it in your library and command zone, and deal your opening 7 (waits ~5-20 s for the piles). Then decide keep or mulligan; deck_info has the pilot notes."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in importIn) (*mcp.CallToolResult, any, error) {
			data, note := in.Data, ""
			if in.Deck != "" {
				dk := d.Content.Decks[in.Deck]
				if dk == nil {
					return nil, nil, fmt.Errorf("no deck %q; list_decks shows the ids", in.Deck)
				}
				data, note = dk.List, " ("+dk.Name+")"
			}
			if strings.TrimSpace(data) == "" {
				return nil, nil, fmt.Errorf("give deck (an id from list_decks) or data (a list or URL)")
			}
			args := map[string]any{"data": data, "url": d.Importer.BaseURL}
			if cm := content.Commanders(data); len(cm) > 0 {
				args["commanders"] = cm
			}
			if _, err := call(ctx, "import", args); err != nil {
				return nil, nil, err
			}
			hit, seen := Await(ctx, d, 45*time.Second, func(e Event) bool { return e.Str("ev") == "import" })
			if hit == nil {
				return text("importing" + note + "; the importer is slow today. An [import] event follows when the piles land; then draw(7, reason='opening hand')." + meanwhile(seen)), nil, nil
			}
			if !hit.Bool("ok") {
				return nil, nil, fmt.Errorf("import failed: %s", hit.Str("error"))
			}
			// the piles settle for a moment before the library deals cleanly
			select {
			case <-time.After(2 * time.Second):
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
			if _, err := call(ctx, "draw", map[string]any{"n": 7}); err != nil {
				return text(Render(hit) + note + "\nCould not deal the opening hand: " + err.Error()), nil, nil
			}
			select {
			case <-time.After(2500 * time.Millisecond):
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
			var b strings.Builder
			b.WriteString(Render(hit) + note + "\nDealt your opening 7 (do not draw again). Keep or mulligan, and say which.\n")
			if h, err := d.Pad.Hand(ctx); err == nil {
				fmt.Fprintf(&b, "Hand (%d cards)\n", len(h.Cards))
				for _, rc := range h.Cards {
					b.WriteString(renderCard(rc.Parse(), rc.Zone))
				}
			}
			return text(b.String() + meanwhile(seen)), nil, nil
		})

	type endIn struct {
		Force bool `json:"force,omitempty" jsonschema:"only when a human says the marker is stuck on you"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "end_turn", Description: "Pass the turn to the next player (TTS turn marker), after your end step. If it fails, the turn was NOT passed: say so, never claim you passed."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in endIn) (*mcp.CallToolResult, any, error) {
			var out struct {
				PassedTo string `json:"passedTo"`
			}
			if err := d.Pad.Call(ctx, "end_turn", map[string]any{"force": in.Force}, &out); err != nil {
				return nil, nil, fmt.Errorf("turn NOT passed: %v. If it is your turn, begin_turn marks it on the pad first", err)
			}
			setDrew(false)
			return text("turn passed to " + out.PassedTo + "; your turn is over, do nothing more until something wakes you"), nil, nil
		})

	type castIn struct {
		GUID   string   `json:"guid" jsonschema:"the spell: a card in your hand or command zone"`
		Tap    []string `json:"tap,omitempty" jsonschema:"your mana sources to tap for it"`
		Note   string   `json:"note,omitempty" jsonschema:"targets or modes, e.g. 'targeting White's Arahbo'"`
		To     string   `json:"to,omitempty" jsonschema:"where it goes when it resolves: creature | other | graveyard (default: by type, instants and sorceries to the graveyard)"`
		Window *int     `json:"window,omitempty" jsonschema:"leave out: the server holds Game Changers, mana value 4+, spells that affect other players and your commander for 8 s, and resolves the rest at once"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "cast", Description: "Cast a spell in one call: tap the mana, announce it, put it on the stack, give the table a quiet window to respond " +
		"(they drop a card on your board or say 'wait'), then resolve it onto your battlefield or into the graveyard. " +
		"Returns 'resolved' (carry out its effect now if it has one) or the response, with the spell left on the stack for you to finish. Lands use play."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in castIn) (*mcp.CallToolResult, any, error) {
			if len(in.Tap) > 0 {
				if err := d.Pad.Call(ctx, "tap", map[string]any{"guids": in.Tap}, nil); err != nil {
					return nil, nil, err
				}
			}
			// the card's text first (hand or command zone), to tell a quick
			// spell from one that deserves a window
			var desc string
			if h, err := d.Pad.Hand(ctx); err == nil {
				for _, rc := range h.Cards {
					if rc.GUID == in.GUID {
						desc = rc.Desc
					}
				}
			}
			var played struct {
				Played string `json:"played"`
				GUID   string `json:"guid"`
				From   string `json:"from"`
			}
			if err := d.Pad.Call(ctx, "play", map[string]any{"guid": in.GUID, "row": "stack"}, &played); err != nil {
				return nil, nil, err
			}
			c := cards.Parse(played.Played, desc)
			to := in.To
			if to == "" {
				switch {
				case strings.Contains(c.TypeLine, "Instant") || strings.Contains(c.TypeLine, "Sorcery"):
					to = "graveyard"
				case strings.Contains(c.TypeLine, "Creature"):
					to = "creature"
				default:
					to = "other"
				}
			}
			window := 0 // quick spells resolve at once; a dropped counter still counts
			if hold, _ := needsWindow(ctx, d, c, played.From); hold {
				window = 8
			}
			if in.Window != nil && *in.Window >= 0 {
				window = *in.Window
			}
			announce := "Casting " + c.Name
			if in.Note != "" {
				announce += ", " + in.Note
			}
			var asked struct {
				Round int `json:"round"`
			}
			var err error
			if window > 0 {
				err = d.Pad.Call(ctx, "ask", map[string]any{"text": announce, "kind": "responses", "wait": window, "quiet": true}, &asked)
			} else {
				err = d.Pad.Call(ctx, "say", map[string]any{"text": announce}, nil)
			}
			var seen []Event
			if err == nil && window > 0 {
				var hit Event
				hit, seen = Await(ctx, d, time.Duration(window+5)*time.Second, func(e Event) bool {
					id, _ := e["id"].(float64)
					return e.Str("ev") == "round" && int(id) == asked.Round
				})
				if hit != nil && hit.Str("result") == "respond" {
					return text(fmt.Sprintf("RESPONSE: %s\n%s is still on the stack [%s]. Answer it (cast an instant) or let their response resolve; "+
						"then finish yours yourself: move it to the graveyard if it was countered, else to %s. Ask in one line if their intent is unclear.%s",
						Render(hit), c.Name, played.GUID, to, meanwhile(seen))), nil, nil
				}
			} else if err != nil && !strings.Contains(err.Error(), "nobody to ask") {
				return nil, nil, err
			}
			args := map[string]any{"guid": played.GUID, "to": "battlefield", "row": to}
			if to == "graveyard" {
				args = map[string]any{"guid": played.GUID, "to": "graveyard"}
			}
			if err := d.Pad.Call(ctx, "move", args, nil); err != nil {
				return nil, nil, fmt.Errorf("%s resolved but could not be moved off the stack: %w", c.Name, err)
			}
			msg := fmt.Sprintf("%s resolved (no responses) -> %s.", c.Name, to)
			if window == 0 {
				msg = fmt.Sprintf("%s resolved at once (a quick spell, no window) -> %s. If someone drops a counter on your board in the next few seconds, honour it.", c.Name, to)
				noteQuietCast(c.Name, played.GUID, to)
			}
			if to == "graveyard" || in.Note != "" {
				msg += " Carry out its effect now and say what happened."
			} else {
				msg += " If it has an enters effect, carry it out now."
			}
			return text(msg + meanwhile(seen)), nil, nil
		})

	type attackIn struct {
		GUIDs  []string `json:"guids" jsonschema:"attacking creatures (your battlefield)"`
		Target string   `json:"target" jsonschema:"defending colour, or a planeswalker/battle name"`
		Note   string   `json:"note,omitempty" jsonschema:"anything extra to announce (triggers, pump, etc.)"`
		Wait   *int     `json:"wait,omitempty" jsonschema:"seconds to wait for the defender to declare blocks (default 20)"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "attack", Description: "Declare attackers: taps them, announces the attack loudly and waits for the defender's blocks in chat (silence = no blocks). " +
		"Returns what they said; then work out damage, say the numbers and let them move their tracker."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in attackIn) (*mcp.CallToolResult, any, error) {
			var tapped struct {
				Tapped []string `json:"tapped"`
			}
			if err := d.Pad.Call(ctx, "tap", map[string]any{"guids": in.GUIDs}, &tapped); err != nil {
				return nil, nil, err
			}
			names := make([]string, 0, len(tapped.Tapped))
			for _, n := range tapped.Tapped {
				names = append(names, cards.Parse(n, "").Name)
			}
			msg := fmt.Sprintf("Attacking %s with %s.", in.Target, strings.Join(names, ", "))
			if in.Note != "" {
				msg += " " + in.Note
			}
			if err := d.Pad.Call(ctx, "say", map[string]any{"text": msg + " Blocks?", "loud": true}, nil); err != nil {
				return nil, nil, err
			}
			wait := 20
			if in.Wait != nil && *in.Wait >= 0 {
				wait = *in.Wait
			}
			hit, seen := Await(ctx, d, time.Duration(wait)*time.Second, func(e Event) bool {
				// the defender speaking (or anyone, when the target is not a seat)
				// or a card landing on your board in response
				if e.Str("ev") == "play" && e.Str("to") == "your_board" {
					return true
				}
				return e.Str("ev") == "chat" && (e.Str("color") == in.Target || !isColour(in.Target))
			})
			if hit == nil {
				return text(msg + fmt.Sprintf("\nNo reply in %d s: no blocks. Work out the damage, say it, and let them move their tracker.%s", wait, meanwhile(seen))), nil, nil
			}
			return text(msg + "\nReply: " + Render(hit) + "\nCheck the blocks against the rules (flying, menace, tapped), then work out the damage." + meanwhile(seen)), nil, nil
		})
}

func isColour(s string) bool {
	switch s {
	case "White", "Brown", "Red", "Orange", "Yellow", "Green", "Teal", "Blue", "Purple", "Pink":
		return true
	}
	return false
}
