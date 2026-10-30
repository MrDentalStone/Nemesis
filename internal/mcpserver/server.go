// Package mcpserver registers the Nemesis tools. Handlers are thin: validate,
// call the pad, render. The pad enforces fairness; this layer never asks it
// for anything it would refuse.
package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/MrDentalStone/Nemesis/internal/cards"
	"github.com/MrDentalStone/Nemesis/internal/content"
	"github.com/MrDentalStone/Nemesis/internal/importer"
	"github.com/MrDentalStone/Nemesis/internal/pad"
	"github.com/MrDentalStone/Nemesis/internal/scryfall"
	"github.com/MrDentalStone/Nemesis/internal/ttsapi"
)

type Deps struct {
	TTS      *ttsapi.Client
	Pad      *pad.Pad
	Importer *importer.Client
	Scryfall *scryfall.Client
	Content  *content.Library
	Docs     fs.FS  // PLAYBOOK.md, RULES.md
	Name     string // the AI's name at the table
	Dev      bool
}

// chatLineMax is about one line of TTS chat at the default size. TTS keeps
// no chat history, so longer lines push the table's own news off screen.
const chatLineMax = 100

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

// Register adds every tool to s.
func Register(s *mcp.Server, d Deps) {
	watchPlays(d)
	mcp.AddTool(s, &mcp.Tool{
		Name: "table_state",
		Description: "Everything public at the table: per seat life, commander damage, hand and library COUNTS, graveyard, exile, command zone, " +
			"and battlefield with tapped, face-down, counter tiles and on-card state. Other hands and all libraries are never listed, by design.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		// name and modes for the pad; an old pad without configure still plays
		note := ""
		if err := configure(ctx, d); err != nil && !errors.Is(err, pad.ErrNoPad) {
			note = fmt.Sprintf("NOTE: could not configure the pad (%v). It is probably an old version: "+
				"ask the table to delete it and spawn Nemesis again from Objects > Saved Objects.\n", err)
		}
		note += autoUpdate(ctx, d)
		snap, err := d.Pad.Snapshot(ctx)
		if err != nil {
			return nil, nil, err
		}
		return text(note + renderSnapshot(snap)), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "my_hand",
		Description: "Your own hand (and companion/secondary hand zone) with full card text. Only your seat is ever readable.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		h, err := d.Pad.Hand(ctx)
		if err != nil {
			return nil, nil, err
		}
		var b strings.Builder
		renderHand(&b, h)
		return text(b.String()), nil, nil
	})

	type sayIn struct {
		Text string `json:"text" jsonschema:"what to say in table chat, as yourself, the player"`
		Loud bool   `json:"loud,omitempty" jsonschema:"also flash it on everyone's screen (use for attacks, big plays, questions)"`
		To   string `json:"to,omitempty" jsonschema:"whisper to this seat colour only (secret numbers, hidden votes); the table sees that you whispered, not what"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "say",
		Description: "Speak in TTS chat as yourself: one short line (TTS keeps no chat history, long lines scroll away). Announce intent before acting, answer questions. With to=<colour>, whisper to that seat only (secret choices).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in sayIn) (*mcp.CallToolResult, any, error) {
		if err := d.Pad.Call(ctx, "say", map[string]any{"text": in.Text, "loud": in.Loud, "to": in.To}, nil); err != nil {
			return nil, nil, err
		}
		long := ""
		if n := len([]rune(in.Text)); n > chatLineMax {
			long = fmt.Sprintf(" (%d characters: too long for TTS chat, keep lines under %d)", n, chatLineMax)
		}
		if in.To != "" {
			return text("whispered to " + in.To + long), nil, nil
		}
		return text("said" + long), nil, nil
	})

	type askIn struct {
		Text  string   `json:"text" jsonschema:"the question, e.g. 'Casting Craterhoof Behemoth, any responses?' or 'Want me to apply agreed damage to your tracker?'"`
		Kind  string   `json:"kind,omitempty" jsonschema:"responses (default) or yesno"`
		Seats []string `json:"seats,omitempty" jsonschema:"colours to ask (default: every seated human)"`
		Wait  *int     `json:"wait,omitempty" jsonschema:"seconds of silence that settle it (default: the pad's setting, 15, or 30 for yesno and pads; 0 waits for every answer)"`
		Pads  bool     `json:"pads,omitempty" jsonschema:"a heavy decision: put a Yes / No / Wait pad with a countdown on each asked seat's mat (if the table has decision pads on). Only for decisions that really matter"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name: "ask_table",
		Description: "Ask the table loudly and WAIT for the answer: one per seat, from chat ('no', 'go' pass; 'wait', 'hold' respond; 'yes'/'no'), a card dropped on your board, or a decision pad (pads=true). " +
			"Silence for the wait settles it. Returns all_no (go on), respond by <colour> (stop, let them act; it names the card if they dropped one), or answered/timeout with answers like 'Blue=yes, White=no'. " +
			"For your own spells use cast, which has its own quiet window; ask only for triggers and real questions.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in askIn) (*mcp.CallToolResult, any, error) {
		var out struct {
			Round int      `json:"round"`
			Kind  string   `json:"kind"`
			Asked []string `json:"asked"`
			Pads  bool     `json:"pads"`
			Wait  int      `json:"wait"`
		}
		params := map[string]any{"text": in.Text, "kind": in.Kind, "pads": in.Pads}
		if len(in.Seats) > 0 {
			params["seats"] = in.Seats
		}
		if in.Wait != nil {
			params["wait"] = *in.Wait
		}
		if err := d.Pad.Call(ctx, "ask", params, &out); err != nil {
			return nil, nil, err
		}
		// wait for the answer here: a model that gets control back while the
		// round is open tends to carry on (and end its turn mid-combat)
		limit := 50 * time.Second // under the shortest client tool limit
		if out.Wait > 0 && time.Duration(out.Wait+5)*time.Second < limit {
			limit = time.Duration(out.Wait+5) * time.Second
		}
		hit, seen, paused := awaitRound(ctx, d, out.Round, limit, limit)
		if hit == nil {
			why := "no answer yet"
			if paused {
				why = "a player pressed Wait on their pad"
			}
			return text(fmt.Sprintf("round #%d asked %s: %s after %s, still open (wait_for_event for the [round] line, or cancel_round)%s",
				out.Round, strings.Join(out.Asked, ", "), why, limit, meanwhile(seen))), nil, nil
		}
		return text(Render(hit) + meanwhile(seen)), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "cancel_round",
		Description: "Close the open ask_table round without a result (the question no longer matters, or players answered in chat).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		var out struct {
			Cancelled bool `json:"cancelled"`
			ID        int  `json:"id"`
		}
		if err := d.Pad.Call(ctx, "cancel_round", nil, &out); err != nil {
			return nil, nil, err
		}
		if !out.Cancelled {
			return text("no round was open"), nil, nil
		}
		return text(fmt.Sprintf("round #%d cancelled", out.ID)), nil, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "table_layout",
		Description: "Diagnostic: where things sit around your seat in mat terms (along = the mat's forward axis, right = its right axis, from its centre): " +
			"every non-card object within reach with its name, scale, script functions and buttons, the cards there, your life tracker and the Winston spots. For fitting the pad to a table; not for play.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		var out json.RawMessage
		if err := d.Pad.Call(ctx, "layout", nil, &out); err != nil {
			return nil, nil, err
		}
		var buf bytes.Buffer
		if err := json.Indent(&buf, out, "", " "); err != nil {
			return text(string(out)), nil, nil
		}
		return text(buf.String()), nil, nil
	})

	type claimIn struct {
		Color string `json:"color" jsonschema:"a seat colour the table has (White, Red, Yellow, Green, Blue, Purple...) - must be unseated"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "claim_seat",
		Description: "Ask to sit at a colour. A human must press Yes on the pad before it takes effect; wait_for_event reports the 'seat' event.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in claimIn) (*mcp.CallToolResult, any, error) {
		if err := d.Pad.Call(ctx, "claim", map[string]any{"color": in.Color}, nil); err != nil {
			return nil, nil, err
		}
		return text("asked for " + in.Color + "; waiting for a human to press Yes on the pad"), nil, nil
	})

	type waitIn struct {
		TimeoutSeconds int `json:"timeout_seconds,omitempty" jsonschema:"seconds to wait for the first event (default 45, max 600; stay under your client's tool-call limit). Nothing is lost between calls."`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name: "wait_for_event",
		Description: "Block until something happens at the table, then return the events, one line each ([chat], [play], [attack], [turn], [round], [hold], [countered], [button], [zone], [log]...). " +
			"Land drops and other players' turn changes do not wake you; they come summed up with the next event. Use this during other players' turns instead of polling.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in waitIn) (*mcp.CallToolResult, any, error) {
		to := in.TimeoutSeconds
		if to <= 0 {
			to = 45
		}
		if to > 600 {
			to = 600
		}
		// every wake is a whole model turn: land drops and other players'
		// turn changes wait for something that needs you, then come along
		// as one summary line
		deadline := time.Now().Add(time.Duration(to) * time.Second)
		var notes []string
		var minor, evs []Event
		for len(evs) == 0 && ctx.Err() == nil {
			left := time.Until(deadline)
			if left <= 0 {
				break
			}
			note, got := NextEvents(ctx, d, left)
			if note != "" {
				notes = append(notes, note)
			}
			for _, e := range got {
				if minorEvent(e) {
					minor = append(minor, e)
				} else {
					evs = append(evs, e)
				}
			}
		}
		var b strings.Builder
		for _, n := range notes {
			b.WriteString(n + "\n")
		}
		if len(minor) > 0 {
			b.WriteString(summarise(minor) + "\n")
		}
		if len(evs) == 0 && len(minor) == 0 {
			b.WriteString("no events in " + fmt.Sprint(to) + "s")
		}
		for _, e := range evs {
			b.WriteString(Render(e) + "\n")
		}
		return text(b.String()), nil, nil
	})

	type cardIn struct {
		GUID string `json:"guid,omitempty" jsonschema:"a face-up card anyone has on the battlefield, in a graveyard, in exile or in a command zone (the [guid] in table_state): returns its full text"`
		Nick string `json:"nick,omitempty" jsonschema:"or: the card's TTS nickname as shown by table_state"`
		Desc string `json:"desc,omitempty" jsonschema:"its TTS description, if you have it"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "parse_card",
		Description: "Full text of a public card: pass its guid from table_state (any player's face-up battlefield, graveyard, exile or command zone card) when a [play] line or your memory is not enough. Or parse a TTS nickname/description you already have.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in cardIn) (*mcp.CallToolResult, any, error) {
		if in.GUID != "" {
			snap, err := d.Pad.Snapshot(ctx)
			if err != nil {
				return nil, nil, err
			}
			nick, desc, ok := publicCard(snap, in.GUID)
			if !ok {
				return nil, nil, fmt.Errorf("no face-up public card [%s] on the table (hands, libraries and face-down cards stay hidden)", in.GUID)
			}
			return text(renderCard(pad.Card{GUID: in.GUID, Card: cards.Parse(nick, desc)}, "")), nil, nil
		}
		if in.Nick == "" {
			return nil, nil, fmt.Errorf("give a guid or a nick")
		}
		return text(renderCard(pad.Card{Card: cards.Parse(in.Nick, in.Desc)}, "")), nil, nil
	})

	type logIn struct {
		N int `json:"n,omitempty" jsonschema:"how many recent lines (default 50)"`
	}
	mcp.AddTool(s, &mcp.Tool{
		Name:        "tts_log",
		Description: "Recent print/error output captured from Tabletop Simulator's Lua. Check this when a tool misbehaves.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in logIn) (*mcp.CallToolResult, any, error) {
		n := in.N
		if n <= 0 {
			n = 50
		}
		var b strings.Builder
		for _, e := range d.TTS.Log(n) {
			fmt.Fprintf(&b, "%s %-5s %s\n", e.Time.Format("15:04:05"), e.Kind, e.Text)
		}
		if b.Len() == 0 {
			b.WriteString("(empty)")
		}
		return text(b.String()), nil, nil
	})

	registerActions(s, d)
	registerDraft(s, d)
	registerContent(s, d)
	registerSetup(s, d)

	if d.Dev {
		type luaIn struct {
			Script string `json:"script" jsonschema:"Lua to run in the Global context; must return a value"`
		}
		mcp.AddTool(s, &mcp.Tool{
			Name:        "exec_lua",
			Description: "DEV ONLY: run arbitrary Lua in TTS. Not available in play mode.",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, in luaIn) (*mcp.CallToolResult, any, error) {
			raw, err := d.TTS.Exec(ctx, in.Script)
			if err != nil {
				return nil, nil, err
			}
			return text(string(raw)), nil, nil
		})
	}
}

// collectEvents waits up to timeout for the first event, then drains
// whatever else arrived within the settle window (0: only what is queued).
func collectEvents(ctx context.Context, c *ttsapi.Client, timeout, settle time.Duration) []json.RawMessage {
	var out []json.RawMessage
	first := time.NewTimer(timeout)
	defer first.Stop()
	select {
	case e := <-c.Events:
		out = append(out, e)
	case <-first.C:
		return nil
	case <-ctx.Done():
		return nil
	}
	quiet := time.After(settle)
	for {
		select {
		case e := <-c.Events:
			out = append(out, e)
		case <-quiet:
			if settle == 0 {
				// still take what is already queued
				for {
					select {
					case e := <-c.Events:
						out = append(out, e)
					default:
						return out
					}
				}
			}
			return out
		}
	}
}

func renderCard(c pad.Card, zone string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "- [%s] %s", c.GUID, c.Name)
	if c.ManaCost != "" {
		fmt.Fprintf(&b, "  %s", c.ManaCost)
	} else if c.CMC > 0 {
		fmt.Fprintf(&b, "  (cmc %d)", c.CMC)
	}
	if c.TypeLine != "" {
		fmt.Fprintf(&b, "  — %s", c.TypeLine)
	}
	if c.PT != "" {
		fmt.Fprintf(&b, "  %s", c.PT)
	}
	switch zone {
	case "secondary":
		b.WriteString("  [secondary hand]")
	case "tokens":
		b.WriteString("  [token zone]")
	}
	b.WriteByte('\n')
	if c.Oracle != "" {
		for _, line := range strings.Split(c.Oracle, "\n") {
			if strings.TrimSpace(line) != "" {
				b.WriteString("    " + line + "\n")
			}
		}
	}
	return b.String()
}

// publicCard finds a face-up card anyone can see by guid: a permanent, or a
// card in a graveyard, exile or command zone.
func publicCard(s *pad.Snapshot, guid string) (nick, desc string, ok bool) {
	for _, seat := range s.Seats {
		for _, p := range seat.Battlefield {
			if p.GUID == guid && !p.FaceDown && p.Nick != "" {
				return p.Nick, p.Desc, true
			}
		}
		for _, pile := range [][]pad.RawCard{seat.Graveyard, seat.Exile, seat.Command} {
			for _, c := range pile {
				if c.GUID == guid && !c.FaceDown {
					return c.Nick, c.Desc, true
				}
			}
		}
	}
	return "", "", false
}

func renderSnapshot(s *pad.Snapshot) string {
	var b strings.Builder
	whose := s.Turns.Active
	if s.Turns.ClaudeTurn {
		whose = "YOU (" + s.Turns.Active + ")"
	} else if !s.Turns.Enabled {
		whose += " (marker off)"
	}
	fmt.Fprintf(&b, "Turn: %s (next %s). Pad: you=%s stopped=%v pad %s\n",
		whose, s.Turns.Next, or(s.Pad.Seat, "none"), s.Pad.Stopped, s.Pad.Version)
	if len(s.Pad.Pending) > 0 && string(s.Pad.Pending) != "null" {
		fmt.Fprintf(&b, "Pending request on pad: %s\n", s.Pad.Pending)
	}
	if s.Pad.ModeName != "" {
		pads := "off"
		if s.Pad.DecisionPads {
			pads = "on"
		}
		fmt.Fprintf(&b, "Play mode: %s (mode_rules). New players: %s. Decision pads: %s\n",
			s.Pad.ModeName, or(strings.Join(s.Pad.NewPlayers, ", "), "none"), pads)
	}
	if r := s.Pad.Round; r != nil {
		fmt.Fprintf(&b, "Open %s round #%d %q: waiting on %s\n", r.Kind, r.ID, r.Text, or(strings.Join(r.WaitingOn, ", "), "nobody"))
	}
	for _, seat := range s.Seats {
		if !seat.Seated && !seat.IsClaude && seat.LibraryCount == 0 && len(seat.Battlefield) == 0 && seat.HandCount == 0 {
			fmt.Fprintf(&b, "\n== %s: empty seat\n", seat.Color)
			continue
		}
		who := seat.Name
		if seat.IsClaude {
			who = "YOU"
		} else if !seat.Seated {
			who = "(nobody seated)"
		}
		life := "?"
		if seat.Life != nil {
			life = fmt.Sprint(*seat.Life)
		}
		fmt.Fprintf(&b, "\n== %s: %s — life %s, hand %d", seat.Color, who, life, seat.HandCount)
		if seat.Hand2Count > 0 {
			fmt.Fprintf(&b, " (+%d secondary)", seat.Hand2Count)
		}
		fmt.Fprintf(&b, ", library %d", seat.LibraryCount)
		if seat.TokenZoneCount > 0 {
			fmt.Fprintf(&b, ", %d in token zone", seat.TokenZoneCount)
		}
		for _, k := range []string{"poison", "energy", "experience"} {
			if v := seat.PlayerCounters[k]; v != 0 {
				fmt.Fprintf(&b, ", %s %d", k, v)
			}
		}
		b.WriteByte('\n')
		if why := seatOut(seat); why != "" && !seat.IsClaude {
			// CR 800.4a: a player who leaves takes everything they own with
			// them, and control effects they made end
			b.WriteString("   OUT OF THE GAME (" + why + "): everything they own has left the game, so ignore the cards still shown for them; anything of theirs you control leaves too (move it off your board).\n")
		}
		var cd []string
		for _, c := range seat.CommanderDamage {
			if c.Amount != nil && *c.Amount > 0 {
				cd = append(cd, fmt.Sprintf("%d from %s", *c.Amount, c.From))
			}
		}
		if len(cd) > 0 {
			fmt.Fprintf(&b, "   commander damage taken: %s\n", strings.Join(cd, ", "))
		}
		if len(seat.Command) > 0 {
			b.WriteString("   command zone: " + names(seat.Command) + "\n")
		}
		if len(seat.Battlefield) > 0 {
			b.WriteString("   battlefield:\n")
			perms := seat.Battlefield
			if !seat.IsClaude {
				// you never touch their lands, so a count by name does: the
				// biggest table_state saving on a late-game board
				var line string
				line, perms = landSummary(perms)
				if line != "" {
					b.WriteString("     " + line + "\n")
				}
			}
			for _, p := range perms {
				fmt.Fprintf(&b, "     [%s] %s", p.GUID, oneLine(p))
				b.WriteByte('\n')
			}
		}
		if len(seat.Graveyard) > 0 {
			b.WriteString("   graveyard: " + names(seat.Graveyard) + "\n")
		}
		if len(seat.Exile) > 0 {
			b.WriteString("   exile: " + names(seat.Exile) + "\n")
		}
	}
	return b.String()
}

// landSummary folds plain lands (no counters, notes or face-down state) into
// one line, "lands 7 (2 tapped): Forest x3 (1 tapped), Command Tower", and
// returns the rest.
func landSummary(perms []pad.Permanent) (string, []pad.Permanent) {
	type tally struct{ n, tapped int }
	counts := map[string]*tally{}
	var order []string
	var rest []pad.Permanent
	total, tapped := 0, 0
	for _, p := range perms {
		c := cards.Parse(p.Nick, p.Desc)
		isLand := p.Row == "land" || (strings.Contains(c.TypeLine, "Land") && !strings.Contains(c.TypeLine, "Creature"))
		if !isLand || p.Type != "Card" || p.FaceDown || len(p.Counters) > 0 || (p.Enc != nil && encList(p.Enc, c.PT) != "") {
			rest = append(rest, p)
			continue
		}
		t := counts[c.Name]
		if t == nil {
			t = &tally{}
			counts[c.Name] = t
			order = append(order, c.Name)
		}
		t.n++
		total++
		if p.Tapped {
			t.tapped++
			tapped++
		}
	}
	if total == 0 {
		return "", perms
	}
	parts := make([]string, 0, len(order))
	for _, name := range order {
		t := counts[name]
		s := name
		if t.n > 1 {
			s += fmt.Sprintf(" x%d", t.n)
		}
		if t.tapped > 0 {
			if t.n > 1 {
				s += fmt.Sprintf(" (%d tapped)", t.tapped)
			} else {
				s += " (tapped)"
			}
		}
		parts = append(parts, s)
	}
	return fmt.Sprintf("lands %d (%d tapped): %s", total, tapped, strings.Join(parts, ", ")), rest
}

func oneLine(p pad.Permanent) string {
	var flags []string
	if p.Tapped {
		flags = append(flags, "TAPPED")
	}
	if p.FaceDown {
		flags = append(flags, "face-down")
	}
	if p.Row == "land" {
		flags = append(flags, "land row")
	}
	if p.Type != "Card" {
		flags = append(flags, strings.ToLower(p.Type))
	}
	if p.Count > 1 {
		flags = append(flags, fmt.Sprintf("pile of %d", p.Count))
	}
	c := cards.Parse(p.Nick, p.Desc)
	s := c.Name
	if c.TypeLine != "" {
		s += " — " + c.TypeLine
	}
	if c.PT != "" {
		s += " " + c.PT
	}
	if len(flags) > 0 {
		s += "  {" + strings.Join(flags, ", ") + "}"
	}
	if len(p.Counters) > 0 {
		s += "  counters: " + counterList(p.Counters)
	}
	if p.Enc != nil {
		if el := encList(p.Enc, c.PT); el != "" {
			s += "  on-card: " + el
		}
	}
	return s
}

// encList renders the Encoder state: "+1/+1 x2 (now 3/3), loyalty 4, flying, note: ...".
func encList(e *pad.EncInfo, printedPT string) string {
	var out []string
	if e.PlusOne != 0 {
		item := fmt.Sprintf("%+d/%+d", e.PlusOne, e.PlusOne)
		if e.PlusOne > 0 {
			item = fmt.Sprintf("+1/+1 x%d", e.PlusOne)
		}
		if bp, bt, ok := basePT(e, printedPT); ok {
			item += fmt.Sprintf(" (now %d/%d)", bp+e.PlusOne+e.Power, bt+e.PlusOne+e.Toughness)
		}
		out = append(out, item)
	} else if e.Power != 0 || e.Toughness != 0 {
		item := fmt.Sprintf("P/T %+d/%+d", e.Power, e.Toughness)
		if bp, bt, ok := basePT(e, printedPT); ok {
			item += fmt.Sprintf(" (now %d/%d)", bp+e.Power, bt+e.Toughness)
		}
		out = append(out, item)
	}
	if e.Loyalty != nil {
		out = append(out, fmt.Sprintf("loyalty %d", *e.Loyalty))
	}
	if e.Counters != 0 {
		out = append(out, fmt.Sprintf("%d counters", e.Counters))
	}
	if e.PiCounter != 0 {
		out = append(out, fmt.Sprintf("counter=%d", e.PiCounter))
	}
	if len(e.Keywords) > 0 {
		out = append(out, strings.Join(e.Keywords, ", "))
	}
	if e.Phased {
		out = append(out, "PHASED OUT")
	}
	if e.Token {
		out = append(out, "token")
	}
	if e.Note != "" {
		out = append(out, fmt.Sprintf("note: %q", e.Note))
	}
	return strings.Join(out, "; ")
}

// basePT prefers the Encoder's parsed base P/T, else the printed "3/3".
func basePT(e *pad.EncInfo, printed string) (int, int, bool) {
	if e.BasePower != nil && e.BaseToughness != nil {
		return *e.BasePower, *e.BaseToughness, true
	}
	var bp, bt int
	if _, err := fmt.Sscanf(printed, "%d/%d", &bp, &bt); err == nil {
		return bp, bt, true
	}
	return 0, 0, false
}

// counterList renders what sits on a card: "+2/+2 [ad6bf5], Flying [1c2d3e], d6=4 [..]".
func counterList(cs []pad.Counter) string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		label := strings.TrimSpace(c.Name)
		if label == "" {
			label = strings.ToLower(c.Type)
		}
		if c.Value != "" && c.Value != label {
			label += "=" + c.Value
		}
		out = append(out, fmt.Sprintf("%s [%s]", label, c.GUID))
	}
	return strings.Join(out, ", ")
}

// names lists public pile cards as "Name [guid]" so you can act on your
// own (commander from the command zone, a card Eternal Witness returns...).
func names(cs []pad.RawCard) string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		n := cards.Parse(c.Nick, c.Desc).Name
		if c.FaceDown {
			n += " (face-down)"
		}
		if c.GUID != "" {
			n += " [" + c.GUID + "]"
		}
		out = append(out, n)
	}
	return strings.Join(out, ", ")
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
