package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/MrDentalStone/Nemesis/internal/cards"
	"github.com/MrDentalStone/Nemesis/internal/content"
	"github.com/MrDentalStone/Nemesis/internal/importer"
	"github.com/MrDentalStone/Nemesis/internal/pad"
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
	mcp.AddTool(s, &mcp.Tool{Name: "draw", Description: "Draw n cards for a card effect. Not for your turn's draw (begin_turn does it) or your opening hand (import_deck deals it). Returns the new cards and the rest of your hand by name."},
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
			before, err := d.Pad.Hand(ctx)
			if err != nil {
				return nil, nil, err
			}
			var out struct {
				Drew        int `json:"drew"`
				LibraryLeft int `json:"libraryLeft"`
			}
			if err := d.Pad.Call(ctx, "draw", map[string]any{"n": n}, &out); err != nil {
				return nil, nil, err
			}
			if !drew {
				setDrew(true) // the turn's draw, held back by begin_turn
			}
			h, err := dealt(ctx, d, len(before.Cards)+out.Drew)
			if err != nil {
				return nil, nil, err
			}
			had := map[string]bool{}
			for _, rc := range before.Cards {
				had[rc.GUID] = true
			}
			var b strings.Builder
			fmt.Fprintf(&b, "Drew %d (%d left in your library):\n", out.Drew, out.LibraryLeft)
			var rest []string
			for _, rc := range h.Cards {
				if had[rc.GUID] {
					rest = append(rest, rc.Parse().Name+" ["+rc.GUID+"]")
				} else {
					b.WriteString(renderCard(rc.Parse(), rc.Zone))
				}
			}
			fmt.Fprintf(&b, "Rest of your hand (%d): %s\n", len(rest), strings.Join(rest, ", "))
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
			untappedAt := time.Now()
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
			}
			h, err := d.Pad.Hand(ctx)
			if err != nil {
				return nil, nil, err
			}
			if len(held) == 0 && !in.SkipDraw {
				if _, err := call(ctx, "draw", map[string]any{"n": 1}); err != nil {
					return nil, nil, err
				}
				if h, err = dealt(ctx, d, len(h.Cards)+1); err != nil {
					return nil, nil, err
				}
				b.WriteString("Drew 1.\n")
			}
			b.WriteString("\n")
			renderHand(&b, h)
			// setRotationSmooth takes about a second; a snapshot read before it
			// lands still shows the cards tapped
			if err := sleepCtx(ctx, 1200*time.Millisecond-time.Since(untappedAt)); err != nil {
				return nil, nil, err
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
		Row  string `json:"row,omitempty" jsonschema:"land | creature | other | stack (default other)"`
		Say  string `json:"say,omitempty" jsonschema:"one short chat line to say first (saves a say call)"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "play", Description: "Put a card from your hand or command zone onto your battlefield (land/creature/other row) or onto the stack spot. Spells go through cast; this is for lands and putting cards into play."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in playIn) (*mcp.CallToolResult, any, error) {
			if err := announce(ctx, d, in.Say); err != nil {
				return nil, nil, err
			}
			r, err := call(ctx, "play", map[string]any{"guid": in.GUID, "row": in.Row})
			if err != nil {
				return nil, nil, err
			}
			return text(r), nil, nil
		})

	type guidsIn struct {
		GUIDs []string `json:"guids" jsonschema:"guids of permanents on your battlefield"`
		Say   string   `json:"say,omitempty" jsonschema:"one short chat line to say first (saves a say call)"`
	}
	type tapIn struct {
		GUIDs []string `json:"guids" jsonschema:"guids of permanents on your battlefield"`
		Mana  bool     `json:"mana,omitempty" jsonschema:"they pay for mana: a Treasure or other sacrifice-for-mana source is sacrificed instead of tapped (cast tap= does this for you)"`
		Say   string   `json:"say,omitempty" jsonschema:"one short chat line to say first (saves a say call)"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "tap", Description: "Tap permanents on your battlefield (mana, attackers, abilities). mana=true sacrifices Treasures and the like."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in tapIn) (*mcp.CallToolResult, any, error) {
			if err := announce(ctx, d, in.Say); err != nil {
				return nil, nil, err
			}
			r, err := call(ctx, "tap", map[string]any{"guids": in.GUIDs, "mana": in.Mana})
			if err != nil {
				return nil, nil, err
			}
			return text(r), nil, nil
		})

	type untapIn struct {
		GUIDs []string `json:"guids,omitempty" jsonschema:"specific permanents to untap"`
		All   bool     `json:"all,omitempty" jsonschema:"untap everything on your battlefield (untap step)"`
		Say   string   `json:"say,omitempty" jsonschema:"one short chat line to say first (saves a say call)"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "untap", Description: "Untap your permanents - specific guids, or all=true for the untap step."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in untapIn) (*mcp.CallToolResult, any, error) {
			if err := announce(ctx, d, in.Say); err != nil {
				return nil, nil, err
			}
			r, err := call(ctx, "untap", map[string]any{"guids": in.GUIDs, "all": in.All})
			if err != nil {
				return nil, nil, err
			}
			return text(r), nil, nil
		})

	type guidIn struct {
		GUID string `json:"guid"`
		Say  string `json:"say,omitempty" jsonschema:"one short chat line to say first (saves a say call)"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "flip", Description: "Flip one of your cards (transform DFCs, morph face-up, etc.)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in guidIn) (*mcp.CallToolResult, any, error) {
			if err := announce(ctx, d, in.Say); err != nil {
				return nil, nil, err
			}
			r, err := call(ctx, "flip", map[string]any{"guid": in.GUID})
			if err != nil {
				return nil, nil, err
			}
			return text(r), nil, nil
		})

	type moveIn struct {
		GUID  string   `json:"guid,omitempty" jsonschema:"one of your objects (hand, battlefield, graveyard, exile, command zone, or a library card from look)"`
		GUIDs []string `json:"guids,omitempty" jsonschema:"or several, moved in this order (to library_top: the last one ends on top)"`
		To    string   `json:"to" jsonschema:"graveyard | exile | command | hand | stack | battlefield | library_top | library_bottom"`
		Row   string   `json:"row,omitempty" jsonschema:"when to=battlefield: land | creature | other"`
		Say   string   `json:"say,omitempty" jsonschema:"one short chat line to say first (saves a say call)"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "move", Description: "Move one of your objects between zones: resolve a spell from the stack to the graveyard/battlefield, discard, exile, return to hand, put on top/bottom of library, commander back to the command zone."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in moveIn) (*mcp.CallToolResult, any, error) {
			guids := in.GUIDs
			if in.GUID != "" {
				guids = append([]string{in.GUID}, guids...)
			}
			if len(guids) == 0 {
				return nil, nil, errors.New("give guid or guids")
			}
			if err := announce(ctx, d, in.Say); err != nil {
				return nil, nil, err
			}
			var lines []string
			for i, g := range guids {
				r, err := call(ctx, "move", map[string]any{"guid": g, "to": in.To, "row": in.Row})
				if err != nil {
					if i == 0 {
						return nil, nil, err
					}
					lines = append(lines, fmt.Sprintf("STOPPED at [%s]: %v (the ones before it moved)", g, err))
					break
				}
				lines = append(lines, r)
			}
			return text(strings.Join(lines, "\n")), nil, nil
		})

	type searchIn struct {
		Query     string `json:"query" jsonschema:"case-insensitive text matched against card name and type line, e.g. 'forest', 'basic land', 'Eternal Witness'"`
		N         int    `json:"n,omitempty" jsonschema:"how many matching cards to take (default 1)"`
		To        string `json:"to,omitempty" jsonschema:"battlefield (default) or hand"`
		Row       string `json:"row,omitempty" jsonschema:"battlefield row: land, creature, other (default other)"`
		Tapped    bool   `json:"tapped,omitempty" jsonschema:"put onto the battlefield tapped"`
		Desc      bool   `json:"desc,omitempty" jsonschema:"also match the oracle text"`
		Basic     bool   `json:"basic,omitempty" jsonschema:"only basic lands: set it for 'search for a basic land' effects (Terramorphic Expanse, Cultivate, Rampant Growth), or 'forest' also matches Breeding Pool"`
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
			args := map[string]any{"query": in.Query, "n": n, "to": in.To, "row": in.Row, "tapped": in.Tapped, "desc": in.Desc, "basic": in.Basic, "noShuffle": in.NoShuffle}
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
		Keywords       []string `json:"keywords,omitempty" jsonschema:"keyword icons to add (flying, trample, haste, hexproof, indestructible...; also frozen, monstrous, stun, exert)"`
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
		Query     string `json:"query" jsonschema:"the token's exact name ('Treasure', 'Food', 'Clue') or Scryfall SEARCH SYNTAX (t:token is added for you) when several share a name: '!Beast pow=3 tou=3 c:g -is:dfc', '!Plant pow=0 tou=1 -is:dfc'. A plain name is searched as that exact name. Plain words like 'Beast 3/3' do not work."`
		N         int    `json:"n,omitempty" jsonschema:"how many separate cards (default 1): only for tokens that differ"`
		Count     int    `json:"count,omitempty" jsonschema:"the usual way: ONE card that stands for this many tokens, with the count on its counter and in its note"`
		Row       string `json:"row,omitempty" jsonschema:"creature | other (default: creature for creature tokens, other for the rest)"`
		Power     *int   `json:"power,omitempty" jsonschema:"the token's real power when the card found differs (Phelddagrif's Hippo is a 1/1, Scryfall's Hippo a 3/3): the card is marked to show it"`
		Toughness *int   `json:"toughness,omitempty" jsonschema:"the token's real toughness, as for power"`
		Note      string `json:"note,omitempty" jsonschema:"notepad text for the token, e.g. 'Hippo for Blue (Phelddagrif)' when it is a gift for another player"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "spawn_token", Description: "Create tokens on your battlefield (found on Scryfall, built by the deck importer). For several alike, pass count: one card plus a counter. power/toughness fix a token whose Scryfall card has other stats; note labels it. A token for another player lands on your mat: ask them to drag it over."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in tokenIn) (*mcp.CallToolResult, any, error) {
			card, nick, err := tokenCard(ctx, d, in.Query)
			if err != nil {
				return nil, nil, err
			}
			parsed := cards.Parse(nick, "")
			name := parsed.Name
			if in.Row == "" && !strings.Contains(parsed.TypeLine, "Creature") {
				in.Row = "other"
			}
			n := in.N
			if n <= 0 || in.Count > 1 {
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
			note := in.Note
			if in.Count > 1 && note == "" {
				note = fmt.Sprintf("%d %s tokens", in.Count, name)
			}
			if in.Count <= 1 && note == "" && in.Power == nil && in.Toughness == nil {
				return text(fmt.Sprintf("spawned %d x %s (%s): %s", n, name, parsed.TypeLine, strings.Join(guids, ", "))), nil, nil
			}
			var fixed []string
			for _, g := range guids {
				args := map[string]any{"guid": g, "token": true}
				if note != "" {
					args["note"] = note
				}
				if in.Count > 1 {
					args["counters"] = in.Count
				}
				var got struct {
					Now struct {
						BasePower     *float64 `json:"basePower"`
						BaseToughness *float64 `json:"baseToughness"`
					} `json:"now"`
				}
				if err := markLanded(ctx, d, args, &got); err != nil {
					return text(fmt.Sprintf("spawned %s [%s], but could not mark it (%v): mark it yourself", name, g, err)), nil, nil
				}
				pt := map[string]any{"guid": g}
				if in.Power != nil && got.Now.BasePower != nil && *in.Power != int(*got.Now.BasePower) {
					pt["power"] = *in.Power - int(*got.Now.BasePower)
				}
				if in.Toughness != nil && got.Now.BaseToughness != nil && *in.Toughness != int(*got.Now.BaseToughness) {
					pt["toughness"] = *in.Toughness - int(*got.Now.BaseToughness)
				}
				if len(pt) > 1 {
					if err := d.Pad.Call(ctx, "mark", pt, nil); err != nil {
						return text(fmt.Sprintf("spawned %s [%s], but could not set its P/T (%v): mark(power=, toughness=) yourself", name, g, err)), nil, nil
					}
				}
				fixed = append(fixed, g)
			}
			msg := fmt.Sprintf("spawned %d x %s: %s", n, name, strings.Join(fixed, ", "))
			if in.Count > 1 {
				msg = fmt.Sprintf("spawned %s [%s] with %d counters", name, fixed[0], in.Count)
			}
			if in.Power != nil || in.Toughness != nil {
				msg += " (P/T marked to the token's real stats)"
			}
			if note != "" {
				msg += ": " + note
			}
			return text(msg), nil, nil
		})

	type importIn struct {
		Deck string `json:"deck,omitempty" jsonschema:"a deck id from list_decks (preferred)"`
		Data string `json:"data,omitempty" jsonschema:"or: a decklist (a 'Commander' header line for the commander, 'Deck' for the 99) or an Archidekt/Moxfield/Scryfall deck URL"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "import_deck", Description: "Build a deck with the table's deck importer, place it in your library and command zone, and deal your opening 7 (waits ~5-20 s for the piles). Then decide keep or mulligan; deck_info has the pilot notes."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in importIn) (*mcp.CallToolResult, any, error) {
			resetCommanderTax()
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
			if err := sleepCtx(ctx, 2*time.Second); err != nil {
				return nil, nil, err
			}
			if _, err := call(ctx, "draw", map[string]any{"n": 7}); err != nil {
				return text(Render(hit) + note + "\nCould not deal the opening hand: " + err.Error()), nil, nil
			}
			var b strings.Builder
			b.WriteString(Render(hit) + note + "\nDealt your opening 7 (do not draw again). Keep or mulligan, and say which.\n")
			if h, err := dealt(ctx, d, 7); err == nil {
				renderHand(&b, h)
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
		GUID      string   `json:"guid" jsonschema:"the spell: a card in your hand or command zone"`
		Tap       []string `json:"tap,omitempty" jsonschema:"your mana sources to tap for it; a Treasure (or another source that sacrifices itself for mana) is sacrificed, and a counted token card loses one"`
		Note      string   `json:"note,omitempty" jsonschema:"targets or modes, e.g. 'targeting White's Arahbo'"`
		To        string   `json:"to,omitempty" jsonschema:"where it goes when it resolves: creature | other | graveyard (default: by type, instants and sorceries to the graveyard)"`
		Window    *int     `json:"window,omitempty" jsonschema:"leave out (the server picks: 30 s with decision pads for heavy hitters, 8 s for spells that matter, 0 for the rest)"`
		RespondTo string   `json:"respond_to,omitempty" jsonschema:"the guid from a [play] line: you cast this in response to that player's spell (announced loudly, always with a window)"`
		Counters  bool     `json:"counters,omitempty" jsonschema:"with respond_to: your spell counters theirs; once yours resolves, its owner gets Countered / Not countered buttons on their card"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "cast", Description: "Cast a spell in one call: tap the mana, announce it, put it on the stack, give the table a quiet window to respond " +
		"(they drop a card on your board or say 'wait'), then resolve it onto your battlefield or into the graveyard. " +
		"Returns 'resolved' (carry out its effect now if it has one) or the response, with the spell left on the stack for you to finish. Lands use play. " +
		"In response to another player's spell, pass respond_to (and counters=true for a counterspell)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in castIn) (*mcp.CallToolResult, any, error) {
			var target *playInfo
			if in.RespondTo != "" {
				if target = knownPlay(in.RespondTo); target == nil {
					target = &playInfo{name: "their spell", to: "battlefield"}
				}
				// your answer replaces the hold's "carry on"
				_ = d.Pad.Call(ctx, "release", map[string]any{"quiet": true}, nil)
			} else if in.Counters {
				return nil, nil, errors.New("counters needs respond_to: the guid of the spell you counter")
			}
			paid := ""
			if len(in.Tap) > 0 {
				var tapped struct {
					Sacrificed []string `json:"sacrificed"`
				}
				if err := d.Pad.Call(ctx, "tap", map[string]any{"guids": in.Tap, "mana": true}, &tapped); err != nil {
					return nil, nil, err
				}
				if len(tapped.Sacrificed) > 0 {
					paid = "\nSacrificed for mana: " + strings.Join(tapped.Sacrificed, ", ") + "."
				}
			}
			var played struct {
				Played string `json:"played"`
				Desc   string `json:"desc"` // the card's text, to tell a quick spell from one that deserves a window
				GUID   string `json:"guid"`
				From   string `json:"from"`
			}
			if err := d.Pad.Call(ctx, "play", map[string]any{"guid": in.GUID, "row": "stack"}, &played); err != nil {
				return nil, nil, err
			}
			c := cards.Parse(played.Played, played.Desc)
			if nth, tax := commanderTax(played.GUID, played.From); tax > 0 {
				paid += fmt.Sprintf("\nCommander tax: this is cast #%d from the command zone, so it costs {%d} more than printed. "+
					"If the mana you tapped did not cover that, tap %d more sources now.", nth, tax, tax)
			}
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
			// a board wipe or a huge threat: the players get a decision pad
			// on their mat and longer to think (Wait pauses the countdown)
			heavy, why := false, ""
			if target == nil {
				if heavy, why = heavyHitter(d.Docs, c); heavy {
					window = 30
				}
			}
			if target != nil {
				window = 8 // they may answer your answer
			}
			if in.Window != nil && *in.Window >= 0 {
				window = *in.Window
			}
			announce := "Casting " + c.Name
			if target != nil {
				announce = fmt.Sprintf("In response to %s's %s: %s", or(target.color, "that"), target.name, c.Name)
			}
			if in.Note != "" {
				announce += ", " + in.Note
			}
			var asked struct {
				Round int  `json:"round"`
				Pads  bool `json:"pads"`
			}
			var err error
			if window > 0 {
				ask := map[string]any{"text": announce, "kind": "responses", "wait": window, "quiet": true, "loud": target != nil}
				if heavy {
					ask["text"] = announce + ". Respond?"
					ask["pads"] = true
				}
				err = d.Pad.Call(ctx, "ask", ask, &asked)
			} else {
				err = d.Pad.Call(ctx, "say", map[string]any{"text": announce, "loud": target != nil}, nil)
			}
			var seen []Event
			if err == nil && window > 0 {
				var hit Event
				var paused bool
				hit, seen, paused = awaitRound(ctx, d, asked.Round, time.Duration(window+5)*time.Second, 3*time.Minute)
				if paused {
					return text(fmt.Sprintf("HELD: a player pressed Wait on their decision pad and is still thinking about %s, which stays on the stack [%s]. "+
						"wait_for_event for its [round] line: on 'respond' let them act, on all_no move it to %s yourself and carry out its effect.%s",
						c.Name, played.GUID, to, meanwhile(seen)) + paid), nil, nil
				}
				if hit != nil && hit.Str("result") == "respond" {
					return text(fmt.Sprintf("RESPONSE: %s\n%s is still on the stack [%s]. Answer it (cast an instant) or let their response resolve; "+
						"then finish yours yourself: move it to the graveyard if it was countered, else to %s. Ask in one line if their intent is unclear.%s",
						Render(hit), c.Name, played.GUID, to, meanwhile(seen)) + paid), nil, nil
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
			if asked.Pads {
				msg = fmt.Sprintf("%s resolved (no responses on the decision pads; %s) -> %s.", c.Name, why, to)
			}
			if window == 0 {
				msg = fmt.Sprintf("%s resolved at once (a quick spell, no window) -> %s. If someone drops a counter on your board in the next few seconds, honour it.", c.Name, to)
				noteQuietCast(c.Name, played.GUID, to)
			}
			if in.Counters {
				return text(msg + " " + settleCounter(ctx, d, in.RespondTo, target) + meanwhile(seen) + paid), nil, nil
			}
			if to == "graveyard" || in.Note != "" {
				msg += " Carry out its effect now; say the result only if the table cannot see it, in a few words."
			} else {
				msg += " If it has an enters effect, carry it out now."
			}
			return text(msg + meanwhile(seen) + paid), nil, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "pass", Description: "Let a spell the table is holding for you resolve: says 'no response, carry on' and lifts the hold at once (a hold also runs out by itself)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			var out struct {
				Released any    `json:"released"`
				Name     string `json:"name"`
			}
			if err := d.Pad.Call(ctx, "release", map[string]any{}, &out); err != nil {
				return nil, nil, err
			}
			if out.Name == "" {
				return text("nothing was held (it ran out or you answered already); stay quiet"), nil, nil
			}
			return text("passed: " + out.Name + " resolves"), nil, nil
		})

	type attackIn struct {
		GUIDs  []string `json:"guids" jsonschema:"attacking creatures (your battlefield)"`
		Target string   `json:"target" jsonschema:"defending colour, or a planeswalker/battle name"`
		Note   string   `json:"note,omitempty" jsonschema:"anything extra to announce (triggers, pump, etc.)"`
		Wait   *int     `json:"wait,omitempty" jsonschema:"seconds to wait for the defender to declare blocks (default 45; humans need time to work out blocks)"`
	}
	mcp.AddTool(s, &mcp.Tool{Name: "attack", Description: "Declare attackers: taps them, announces the attack loudly and waits for the defender's blocks in chat (silence = no blocks). " +
		"Returns what they said; then work out damage, say the numbers in one line and let them move their tracker."},
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
			wait := 45
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

// settleCounter puts the Countered / Not countered buttons on the countered
// card and waits for its owner. A spell that went to the graveyard already
// did its thing at the table, so its owner is asked to undo it instead.
func settleCounter(ctx context.Context, d Deps, guid string, t *playInfo) string {
	if t.to == "graveyard" {
		msg := fmt.Sprintf("%s is countered. %s: please undo it", t.name, or(t.color, "Everyone"))
		_ = d.Pad.Call(ctx, "say", map[string]any{"text": msg, "loud": true}, nil)
		return t.name + " is countered: it is already in the graveyard, so you asked its owner to undo its effect."
	}
	var out struct {
		Owner  string `json:"owner"`
		InHand bool   `json:"inHand"`
	}
	if err := d.Pad.Call(ctx, "countered", map[string]any{"guid": guid}, &out); err != nil {
		return fmt.Sprintf("%s is countered, but the buttons failed (%v): ask its owner in chat to put it in their graveyard.", t.name, err)
	}
	if out.InHand {
		msg := fmt.Sprintf("%s: %s is countered, so graveyard rather than hand please", or(t.color, "Owner"), t.name)
		_ = d.Pad.Call(ctx, "say", map[string]any{"text": msg}, nil)
		return t.name + " is countered, but its owner already picked it back up into their hand; you asked them to put it in their graveyard."
	}
	hit, seen := Await(ctx, d, 30*time.Second, func(e Event) bool {
		return e.Str("ev") == "countered" && e.Str("guid") == guid
	})
	if hit == nil {
		return fmt.Sprintf("%s is countered; %s has Countered / Not countered buttons on it and has not pressed one yet (a [countered] event will say).",
			t.name, out.Owner) + meanwhile(seen)
	}
	return Render(hit) + meanwhile(seen)
}

func isColour(s string) bool {
	switch s {
	case "White", "Brown", "Red", "Orange", "Yellow", "Green", "Teal", "Blue", "Purple", "Pink":
		return true
	}
	return false
}

// sleepCtx waits for dur (nothing when it is 0 or less) unless ctx ends first.
func sleepCtx(ctx context.Context, dur time.Duration) error {
	if dur <= 0 {
		return nil
	}
	select {
	case <-time.After(dur):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// dealt waits until your hand holds want cards: deal() animates, and the hand
// zone only owns a card once it lands (about a second). It gives up after
// 3 s and returns what the hand holds by then.
func dealt(ctx context.Context, d Deps, want int) (*pad.HandReply, error) {
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := sleepCtx(ctx, 250*time.Millisecond); err != nil {
			return nil, err
		}
		h, err := d.Pad.Hand(ctx)
		if err != nil || len(h.Cards) >= want || time.Now().After(deadline) {
			return h, err
		}
	}
}

// renderHand lists a hand with full card text.
func renderHand(b *strings.Builder, h *pad.HandReply) {
	fmt.Fprintf(b, "Hand of %s (%d cards)\n", h.Seat, len(h.Cards))
	for _, rc := range h.Cards {
		b.WriteString(renderCard(rc.Parse(), rc.Zone))
	}
}

// announce says text in chat before an action ("" says nothing).
func announce(ctx context.Context, d Deps, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return d.Pad.Call(ctx, "say", map[string]any{"text": text}, nil)
}

// tokenCards remembers each token query's card for the session: Treasure and
// Goblin come up again and again, and each lookup is a Scryfall search plus
// an importer build.
var tokenCards = struct {
	sync.Mutex
	m map[string][2]string // query -> card JSON, nickname
}{m: map[string][2]string{}}

// tokenQuery turns a plain name ("Treasure") into an exact-name search: Scryfall
// ranks a bare word by full-text relevance, and "Treasure" once returned a
// Dinosaur token whose text mentions Treasure.
func tokenQuery(q string) string {
	q = strings.TrimSpace(q)
	if q == "" || strings.ContainsAny(q, `!:=<>"`) || strings.Contains(q, " -") || strings.HasPrefix(q, "-") {
		return q
	}
	return `!"` + q + `" -is:dfc`
}

// markLanded marks a card the pad just spawned. It counts as yours only once
// it has landed on your mat, so the first calls can fail: retry briefly.
func markLanded(ctx context.Context, d Deps, args map[string]any, out any) error {
	var err error
	for try := 0; try < 8; try++ {
		if err = sleepCtx(ctx, 300*time.Millisecond); err != nil {
			return err
		}
		if err = d.Pad.Call(ctx, "mark", args, out); err == nil {
			return nil
		}
	}
	return err
}

func tokenCard(ctx context.Context, d Deps, query string) (json.RawMessage, string, error) {
	tokenCards.Lock()
	hit, ok := tokenCards.m[query]
	tokenCards.Unlock()
	if ok {
		return json.RawMessage(hit[0]), hit[1], nil
	}
	found, err := d.Scryfall.Search(ctx, "t:token "+tokenQuery(query), "")
	if err != nil {
		return nil, "", err
	}
	if len(found) == 0 {
		return nil, "", fmt.Errorf("no token matched %q on Scryfall", query)
	}
	tok := found[0]
	line := fmt.Sprintf("1 %s (%s) %s", tok.Name, strings.ToUpper(tok.Set), tok.CollectorNumber)
	piles, issues, err := d.Importer.Build(ctx, line)
	if err != nil {
		return nil, "", err
	}
	if len(piles) == 0 {
		return nil, "", fmt.Errorf("the importer could not build %q (%s)", line, strings.Join(issues, "; "))
	}
	card, nick, err := importer.FirstCard(piles[0])
	if err != nil {
		return nil, "", err
	}
	tokenCards.Lock()
	tokenCards.m[query] = [2]string{string(card), nick}
	tokenCards.Unlock()
	return card, nick, nil
}
