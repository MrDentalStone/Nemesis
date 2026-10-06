package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/MrDentalStone/Nemesis/internal/cards"
	"github.com/MrDentalStone/Nemesis/internal/pad"
)

// Event is one table event as the pad sent it (ev, color, text...).
type Event map[string]any

func (e Event) Str(k string) string { v, _ := e[k].(string); return v }
func (e Event) Bool(k string) bool  { v, _ := e[k].(bool); return v }

// NextEvents waits up to timeout for table events: first whatever the pad
// buffered while nothing was listening, then live ones. Plays by other
// players come back with your response window worked out (window,
// canRespond), so neither a chat client nor the agent has to read the table
// to know whether a play needs an answer. note is a catch-up remark for the
// caller to show ("3 events recovered").
func NextEvents(ctx context.Context, d Deps, timeout time.Duration) (note string, evs []Event) {
	if n, err := d.Pad.Drain(ctx); err != nil && !errors.Is(err, pad.ErrNoPad) {
		note = "(catch-up failed: " + err.Error() + ")"
	} else if n > 0 {
		note = fmt.Sprintf("(%d events recovered from the pad buffer)", n)
	}
	raws := collectEvents(ctx, d.TTS, timeout)
	plays, myTurn := false, false
	for _, raw := range raws {
		if bytes.Contains(raw, []byte(`"ev":"pad"`)) {
			reconfigure() // a fresh or reloaded pad: send name and modes again
		}
		var e Event
		if json.Unmarshal(raw, &e) != nil {
			continue
		}
		delete(e, "t")
		delete(e, "seq")
		delete(e, "epoch")
		if e.Str("ev") == "play" {
			plays = true
			if e.Str("to") == "your_board" {
				if q := lateResponse(); q != "" {
					e["lateFor"] = q // may be a counter to the quick spell you just cast
				}
			}
		}
		if e.Str("ev") == "turn" && e.Bool("claude") {
			myTurn = true
		}
		evs = append(evs, e)
	}
	if plays || myTurn {
		w, err := ResponseWindow(ctx, d)
		for _, e := range evs {
			if e.Str("ev") == "turn" && e.Bool("claude") && err == nil && w.EmptyStack().Any() {
				// the pad hands the turn over straight from the last player's
				// end step: this is the moment for end-step instants, before
				// begin_turn untaps
				e["endStep"] = w.EmptyStack().String()
			}
			if e.Str("ev") != "play" {
				continue
			}
			switch {
			case err != nil:
				e["window"] = "unknown (" + err.Error() + ")"
				e["canRespond"] = true
			case isLandPlay(e):
				e["canRespond"] = false
			default:
				e["window"] = w.String()
				e["canRespond"] = w.Any()
			}
		}
	}
	return note, evs
}

func isLandPlay(e Event) bool {
	if e.Bool("landRow") {
		return true
	}
	c := cards.Parse(e.Str("name"), e.Str("desc"))
	return strings.Contains(c.TypeLine, "Land") && !strings.Contains(c.TypeLine, "Creature")
}

// Render is one event as a short line for the model. Raw JSON spends tokens
// on keys and quoting; these lines say the same in plain words.
func Render(e Event) string {
	s := e.Str
	who := func() string {
		if n := s("name"); n != "" && s("ev") != "play" {
			return s("color") + " (" + n + ")"
		}
		if n := s("player"); n != "" {
			return s("color") + " (" + n + ")"
		}
		return or(s("color"), "someone")
	}
	switch s("ev") {
	case "chat":
		return fmt.Sprintf("[chat] %s: %q", who(), s("text"))
	case "attack":
		return fmt.Sprintf("[attack] %s tapped on their turn: %s", who(), strings.Join(attackers(e), ", "))
	case "play":
		c := cards.Parse(s("name"), s("desc"))
		line := fmt.Sprintf("[play] %s: %s", who(), cardLine(c))
		if s("to") == "your_board" {
			line = fmt.Sprintf("[play] %s put %s ON YOUR BOARD (from %s)", who(), cardLine(c), s("from"))
			if q := s("lateFor"); q != "" {
				line += "; it may answer your quick spell " + q + ": if it counters it, move that card to your graveyard"
			}
		} else {
			line += fmt.Sprintf(" (%s -> %s)", s("from"), s("to"))
		}
		if e.Bool("faceDown") {
			line += " face-down"
		}
		if c.Oracle != "" && !isLandPlay(e) {
			line += " | " + oneLineText(c.Oracle, 220)
		}
		switch {
		case isLandPlay(e):
		case e.Bool("canRespond"):
			line += " || you could respond: " + s("window")
		default:
			line += " || nothing to respond with"
		}
		return line
	case "turn":
		if e.Bool("claude") {
			line := "[turn] YOUR turn (" + s("color") + ")"
			if w := s("endStep"); w != "" {
				line += ". Before begin_turn it is still " + or(s("prev"), "the last player") +
					"'s end step: you could play " + w + " (or flashback from your graveyard)"
			}
			return line
		}
		return "[turn] " + s("color") + "'s turn"
	case "round":
		line := fmt.Sprintf("[round] %q: %s", s("text"), s("result"))
		if b := s("by"); b != "" {
			line += " by " + b
		}
		if c := s("card"); c != "" {
			cc := cards.Parse(c, s("cardDesc"))
			line += " with " + cardLine(cc) + " dropped on your board"
			if cc.Oracle != "" {
				line += " | " + oneLineText(cc.Oracle, 220)
			}
		}
		if a := s("answers"); a != "" {
			line += " (" + a + ")"
		}
		if e.Bool("timedOut") {
			line += " after silence"
		}
		return line
	case "button":
		where := "admin pad"
		if s("pad") == "player" {
			where = s("color") + "'s player pad"
		}
		return fmt.Sprintf("[button] %s on the %s (%s)", s("id"), where, or(s("name"), s("color")))
	case "zone":
		if s("dir") == "move" {
			return fmt.Sprintf("[zone] your %s moved from %s to %s", s("name"),
				strings.TrimSuffix(s("from"), "Zone"), strings.TrimSuffix(s("zone"), "Zone"))
		}
		return fmt.Sprintf("[zone] your %s %s %s", s("name"), map[string]string{"enter": "entered", "leave": "left"}[s("dir")],
			strings.TrimSuffix(s("zone"), "Zone"))
	case "log":
		return "[log] " + s("text")
	case "mode":
		return fmt.Sprintf("[mode] play mode is now %s (set by %s)", s("name"), s("by"))
	case "new_player":
		return fmt.Sprintf("[new_player] %s flagged as new player: %v", s("color"), e["on"])
	case "seat":
		return "[seat] your seat is now " + s("color")
	case "import":
		if e.Bool("ok") {
			return fmt.Sprintf("[import] deck import finished (%v piles; problems: %s)", e["piles"], or(s("problems"), "none"))
		}
		return "[import] deck import FAILED: " + s("error")
	case "pad":
		return "[pad] the Nemesis pad " + or(s("what"), "changed")
	}
	raw, _ := json.Marshal(e)
	return string(raw)
}

// attackers lists the creatures in an ev=attack ("Arahbo (2/2)").
func attackers(e Event) []string {
	var list []struct {
		Name string `json:"name"`
		Desc string `json:"desc"`
	}
	_ = json.Unmarshal([]byte(e.Str("cards")), &list)
	out := make([]string, 0, len(list))
	for _, a := range list {
		c := cards.Parse(a.Name, a.Desc)
		if c.PT != "" {
			out = append(out, c.Name+" ("+c.PT+")")
		} else {
			out = append(out, c.Name)
		}
	}
	return out
}

// Await waits up to timeout for the event done accepts. Everything else that
// arrives meanwhile comes back in seen, so the caller can show it: these
// events are consumed here and nobody else will see them.
func Await(ctx context.Context, d Deps, timeout time.Duration, done func(Event) bool) (hit Event, seen []Event) {
	deadline := time.Now().Add(timeout)
	for hit == nil {
		left := time.Until(deadline)
		if left <= 0 || ctx.Err() != nil {
			return nil, seen
		}
		_, evs := NextEvents(ctx, d, left)
		for _, e := range evs {
			if hit == nil && done(e) {
				hit = e
				continue
			}
			seen = append(seen, e)
		}
	}
	return hit, seen
}

// meanwhile renders what else happened while a tool waited.
func meanwhile(evs []Event) string {
	if len(evs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nMeanwhile at the table:\n")
	for _, e := range evs {
		b.WriteString(Render(e) + "\n")
	}
	return b.String()
}

func cardLine(c cards.Card) string {
	out := c.Name
	var bits []string
	if c.TypeLine != "" {
		bits = append(bits, c.TypeLine)
	}
	if c.ManaCost != "" {
		bits = append(bits, c.ManaCost)
	} else if c.CMC > 0 {
		bits = append(bits, "mv "+strconv.Itoa(c.CMC))
	}
	if c.PT != "" {
		bits = append(bits, c.PT)
	}
	if len(bits) > 0 {
		out += " (" + strings.Join(bits, ", ") + ")"
	}
	return out
}

func oneLineText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// Window is what you could do at instant speed right now: instants and
// flash cards in hand, non-mana activated abilities on your untapped
// permanents, and the mana you have up.
type Window struct {
	Mana    int
	Options []string
	// OnSpell marks the options that only answer a spell (Counterspell,
	// Twincast): useless with an empty stack, as in an end step.
	OnSpell []bool
}

func (w Window) Any() bool { return len(w.Options) > 0 }

func (w Window) String() string {
	if !w.Any() {
		return fmt.Sprintf("nothing at instant speed (%d mana up)", w.Mana)
	}
	return fmt.Sprintf("%s; %d mana up", strings.Join(w.Options, ", "), w.Mana)
}

// EmptyStack is the window with nothing on the stack to answer.
func (w Window) EmptyStack() Window {
	out := Window{Mana: w.Mana}
	for i, o := range w.Options {
		if i < len(w.OnSpell) && w.OnSpell[i] {
			continue
		}
		out.Options = append(out.Options, o)
		out.OnSpell = append(out.OnSpell, false)
	}
	return out
}

var reOnSpell = regexp.MustCompile(`(?i)^(counter|copy) target [^.]*spell`)

var (
	reCostSym  = regexp.MustCompile(`\{([^}]+)\}`)
	reLoyalty  = regexp.MustCompile(`^[+−-]?\d+:|^[+−-]X:`)
	reFlash    = regexp.MustCompile(`(?m)^Flash\b`)
	reFreeCast = regexp.MustCompile(`(?i)rather than pay this spell's mana cost|without paying its mana cost`)
)

// ResponseWindow reads your hand and battlefield. It errs on the side of
// waking you: a card it cannot cost is listed.
func ResponseWindow(ctx context.Context, d Deps) (Window, error) {
	var w Window
	snap, err := d.Pad.Snapshot(ctx)
	if err != nil {
		return w, err
	}
	var me *pad.Seat
	for i := range snap.Seats {
		if snap.Seats[i].IsClaude {
			me = &snap.Seats[i]
		}
	}
	if me == nil {
		return w, errors.New("no seat")
	}
	type ability struct {
		name, cost string
		n          int
	}
	var abilities []ability
	for _, p := range me.Battlefield {
		if p.Type != "Card" || p.FaceDown {
			continue
		}
		c := cards.Parse(p.Nick, p.Desc)
		land := p.Row == "land" || strings.Contains(c.TypeLine, "Land")
		mana := land && c.Oracle == "" // a basic with no rules text on the card
		for _, line := range strings.Split(c.Oracle, "\n") {
			cost, effect, ok := strings.Cut(strings.TrimSpace(line), ":")
			if !ok || !strings.Contains(cost, "{") || reLoyalty.MatchString(line) || len(cost) > 60 {
				continue
			}
			effect = strings.TrimSpace(effect)
			taps := strings.Contains(cost, "{T}")
			if strings.HasPrefix(effect, "Add ") {
				mana = mana || taps
				continue
			}
			if land || (taps && p.Tapped) || strings.Contains(strings.ToLower(effect), "only as a sorcery") ||
				strings.HasPrefix(cost, "Equip") || strings.HasPrefix(cost, "Level up") {
				continue
			}
			abilities = append(abilities, ability{c.Name, cost, manaIn(cost)})
		}
		if mana && !p.Tapped {
			w.Mana++ // one per source: enough to tell whether a response is affordable
		}
	}
	h, err := d.Pad.Hand(ctx)
	if err != nil {
		return w, err
	}
	for _, rc := range h.Cards {
		if rc.Zone == "tokens" {
			continue
		}
		c := rc.Parse().Card
		instant := strings.Contains(c.TypeLine, "Instant") || reFlash.MatchString(c.Oracle)
		if !instant || strings.Contains(c.TypeLine, "Land") {
			continue
		}
		cost := c.CMC
		if c.ManaCost != "" {
			cost = manaIn(c.ManaCost)
		}
		if cost <= w.Mana || reFreeCast.MatchString(c.Oracle) {
			w.Options = append(w.Options, fmt.Sprintf("%s (%s, mv %d)", c.Name, kindOf(c), cost))
			w.OnSpell = append(w.OnSpell, reOnSpell.MatchString(c.Oracle))
		}
	}
	for _, a := range abilities {
		if a.n <= w.Mana {
			w.Options = append(w.Options, fmt.Sprintf("%s ability %s", a.name, a.cost))
			w.OnSpell = append(w.OnSpell, false)
		}
	}
	return w, nil
}

func kindOf(c cards.Card) string {
	if strings.Contains(c.TypeLine, "Instant") {
		return "instant"
	}
	return "flash"
}

// manaIn counts the mana in a cost: {2}{U}{U} is 4, {T} and {X} are 0.
func manaIn(cost string) int {
	n := 0
	for _, m := range reCostSym.FindAllStringSubmatch(cost, -1) {
		sym := m[1]
		if v, err := strconv.Atoi(sym); err == nil {
			n += v
			continue
		}
		switch sym {
		case "T", "Q", "X", "Y", "E":
			continue
		}
		n++
	}
	return n
}
