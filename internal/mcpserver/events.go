package mcpserver

import (
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
	return nextEvents(ctx, d, timeout, true, 300*time.Millisecond)
}

// nextEvents is NextEvents with the catch-up drain optional (a waiting tool
// drains once, not on every wake) and the settle window after the first
// event set by the caller (a waiting tool wants its hit at once).
func nextEvents(ctx context.Context, d Deps, timeout time.Duration, drain bool, settle time.Duration) (note string, evs []Event) {
	if drain {
		if n, err := d.Pad.Drain(ctx); err != nil && !errors.Is(err, pad.ErrNoPad) {
			note = "(catch-up failed: " + err.Error() + ")"
		} else if n > 0 {
			note = fmt.Sprintf("(%d events recovered from the pad buffer)", n)
		}
	}
	raws := collectEvents(ctx, d.TTS, timeout, settle)
	plays, myTurn := false, false
	for _, raw := range raws {
		var e Event
		if json.Unmarshal(raw, &e) != nil {
			continue
		}
		if e.Str("ev") == "pad" {
			reconfigure() // a fresh or reloaded pad: send name and modes again
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
		if e.Str("ev") == "draft" && e.Str("cards") != "" {
			recallCosts(ctx, d, parseCardList(e.Str("cards"))) // a restart mid-draft forgot them
		}
		evs = append(evs, e)
	}
	if plays || myTurn {
		// one read of your hand and battlefield per batch, and none for a
		// play the server already looked at as it arrived (interrupt.go)
		var cached *Window
		var cachedErr error
		window := func() (Window, error) {
			if cached == nil && cachedErr == nil {
				w, err := ResponseWindow(ctx, d)
				cached, cachedErr = &w, err
			}
			return *cached, cachedErr
		}
		for _, e := range evs {
			if e.Str("ev") == "turn" && e.Bool("claude") {
				w, err := window()
				if err != nil || !w.EmptyStack().Any() {
					continue
				}
				// the pad hands the turn over straight from the last player's
				// end step: this is the moment for end-step instants, before
				// begin_turn untaps
				e["endStep"] = w.EmptyStack().String()
			}
			if e.Str("ev") != "play" {
				continue
			}
			if isLandPlay(e) {
				e["canRespond"] = false
				continue
			}
			var w Window
			var err error
			if pi := playFor(e.Str("guid")); pi != nil && pi.ok && e.Str("to") != "your_board" {
				w = pi.w
				e["held"] = pi.held
			} else {
				w, err = window()
			}
			switch {
			case err != nil:
				e["window"] = "unknown (" + err.Error() + ")"
				e["canRespond"] = true
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
		return renderAttack(e, who())
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
			if o := s("onto"); o != "" {
				line += fmt.Sprintf(", dropped on %s's board: %s cast it, aimed at %s", o, s("color"), o)
			}
		}
		if e.Bool("faceDown") {
			line += " face-down"
		}
		if c.Oracle != "" && !isLandPlay(e) {
			n := playTextMax
			if !e.Bool("canRespond") && s("to") != "your_board" {
				n = quietPlayTextMax // parse_card(guid) has the rest
			}
			line += " | " + oneLineText(c.Oracle, n)
		}
		switch {
		case isLandPlay(e):
		case e.Bool("held"):
			line += fmt.Sprintf(" || THE TABLE IS HELD FOR YOU, answer now: you could respond: %s (cast with respond_to=%s), or pass", s("window"), s("guid"))
		case e.Bool("canRespond"):
			line += " || you could respond: " + s("window")
			if g := s("guid"); g != "" {
				line += " (respond_to=" + g + ")"
			}
		default:
			line += " || nothing to respond with"
		}
		return line
	case "turn":
		if e.Bool("claude") {
			line := "[turn] YOUR turn (" + s("color") + ")"
			if e.Bool("first") {
				line += ". This is your FIRST turn since the game was set up: if the table has not agreed who goes first, ask before begin_turn"
			}
			if w := s("endStep"); w != "" {
				line += ". Before begin_turn it is still " + or(s("prev"), "the last player") +
					"'s end step: you could play " + w + " (or flashback from your graveyard)"
			}
			return line
		}
		if e.Bool("tookBack") {
			return "[turn] " + s("color") + "'s turn: the table moved the marker off you, so YOUR turn is over (undo anything the table did not accept)"
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
	case "round_pause":
		if e.Bool("paused") {
			return fmt.Sprintf("[round] %s pressed Wait on their decision pad: the countdown is paused", s("color"))
		}
		return fmt.Sprintf("[round] %s let the countdown run again (%v s left)", s("color"), e["left"])
	case "button":
		where := "admin pad"
		if s("pad") == "decision" {
			where = s("color") + "'s decision pad"
		}
		return fmt.Sprintf("[button] %s on the %s (%s)", s("id"), where, or(s("name"), s("color")))
	case "zone":
		if s("dir") == "move" {
			return fmt.Sprintf("[zone] your %s moved from %s to %s", s("name"),
				strings.TrimSuffix(s("from"), "Zone"), strings.TrimSuffix(s("zone"), "Zone"))
		}
		return fmt.Sprintf("[zone] your %s %s %s", s("name"), map[string]string{"enter": "entered", "leave": "left"}[s("dir")],
			strings.TrimSuffix(s("zone"), "Zone"))
	case "hold":
		return fmt.Sprintf("[hold] your hold on %s ran out (%v s) and the table carried on: it resolved", s("name"), e["secs"])
	case "countered":
		switch s("result") {
		case "accepted":
			return fmt.Sprintf("[countered] %s's %s went to their graveyard", s("color"), s("name"))
		case "refused":
			return fmt.Sprintf("[countered] %s pressed Not countered on %s: ask what answers your counter", s("color"), s("name"))
		}
		return fmt.Sprintf("[countered] nobody settled %s's %s: remind them in chat", s("color"), s("name"))
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
	case "draft":
		return renderDraftEvent(e)
	}
	raw, _ := json.Marshal(e)
	return string(raw)
}

// renderAttack: the attack pad decides whether tapped creatures are an
// attack on you. Without a press (and with other opponents to hit) it is
// someone else's fight; minorEvent keeps that line from waking you.
func renderAttack(e Event, who string) string {
	list := strings.Join(attackers(e), ", ")
	switch {
	case e.Bool("takenBack"):
		return fmt.Sprintf("[attack] the attack pad was pressed again: %s is NOT attacking you after all", e.Str("color"))
	case e.Bool("noTaps"):
		return fmt.Sprintf("[attack] %s pressed your attack pad: they are ATTACKING YOU, but no creature of theirs tapped (vigilance?). "+
			"table_state to see their board, then say your blocks", who)
	case e.Bool("pressed"), e.Bool("late"):
		return fmt.Sprintf("[attack] %s is ATTACKING YOU (attack pad) with: %s. Check each attacker, then say your blocks", who, list)
	case e.Bool("onMe"):
		return fmt.Sprintf("[attack] %s is ATTACKING YOU (you are their only opponent) with: %s. Check each attacker, then say your blocks", who, list)
	case e["onMe"] != nil:
		return fmt.Sprintf("[attack] %s tapped %s, not at you (your attack pad is dark): stay quiet", who, list)
	}
	return fmt.Sprintf("[attack] %s tapped on their turn: %s", who, list)
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
	for first := true; hit == nil; first = false {
		left := time.Until(deadline)
		if left <= 0 || ctx.Err() != nil {
			return nil, seen
		}
		_, evs := nextEvents(ctx, d, left, first, 0)
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

// playTextMax keeps a [play] line readable but long enough for wordy cards
// (Itazura, Lingering Wick ran past the old 220). A play you cannot answer
// gets less: parse_card reads the rest when it matters.
const (
	playTextMax      = 700
	quietPlayTextMax = 300
)

// minorEvent: news that does not need you now. wait_for_event keeps waiting
// and sums these up with the next event that does.
func minorEvent(e Event) bool {
	switch e.Str("ev") {
	case "play":
		return isLandPlay(e) && e.Str("to") != "your_board"
	case "turn":
		return !e.Bool("claude") && !e.Bool("tookBack")
	case "attack":
		// tapped creatures with the attack pad dark: not your fight
		return e["onMe"] != nil && !e.Bool("onMe") && !e.Bool("takenBack")
	case "round_pause":
		return true
	case "log":
		// the table's own chatter (land counters, the importer's progress);
		// Lua errors and anything about life, damage or the game still wake you
		return e.Str("kind") != "error" && !reLogNews.MatchString(e.Str("text"))
	}
	return false
}

// summaryMax caps the Meanwhile line: a burst of table chatter is not news.
const summaryMax = 6

var (
	reLogNews = regexp.MustCompile(`(?i)life|damage|poison|scoop|concede|turn|nemesis|error`)
	reBBCode  = regexp.MustCompile(`\[(?:[0-9A-Fa-f]{6}|-|/?[bius])\]`)
)

// summarise is one line for the minor events: "Meanwhile: Blue played
// Forest; Red's turn".
func summarise(evs []Event) string {
	var parts []string
	for _, e := range evs {
		switch e.Str("ev") {
		case "play":
			parts = append(parts, e.Str("color")+" played "+cards.Parse(e.Str("name"), "").Name)
		case "turn":
			parts = append(parts, e.Str("color")+"'s turn")
		case "attack":
			parts = append(parts, fmt.Sprintf("%s attacked someone else with %d", e.Str("color"), len(attackers(e))))
		case "round_pause":
			parts = append(parts, e.Str("color")+" paused a decision pad")
		case "log":
			parts = append(parts, "log "+oneLineText(reBBCode.ReplaceAllString(e.Str("text"), ""), 60))
		}
	}
	if len(parts) > summaryMax {
		parts = append(parts[:summaryMax], fmt.Sprintf("and %d more", len(parts)-summaryMax))
	}
	return "Meanwhile: " + strings.Join(parts, "; ")
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
	// Targets is, for an OnSpell option, what it may target ("noncreature
	// spell", "instant or sorcery spell"); "spell" for any.
	Targets []string
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

var reOnSpell = regexp.MustCompile(`(?i)^(?:counter|copy) target ([^.]*?spell)`)

var cardTypes = []string{"creature", "artifact", "enchantment", "instant", "sorcery", "planeswalker", "battle"}

// canTarget says whether an answer restricted to target ("noncreature
// spell", "instant or sorcery spell", "creature spell") can take a spell of
// typeLine. Words it does not know (multicoloured, mana value) count as yes.
func canTarget(target, typeLine string) bool {
	t, tl := strings.ToLower(target), strings.ToLower(typeLine)
	var want []string
	for _, ct := range cardTypes {
		if strings.Contains(t, "non"+ct) {
			if strings.Contains(tl, ct) {
				return false
			}
			continue
		}
		if strings.Contains(t, ct) {
			want = append(want, ct)
		}
	}
	if len(want) == 0 {
		return true
	}
	for _, ct := range want {
		if strings.Contains(tl, ct) {
			return true
		}
	}
	return false
}

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
	field, hand, err := windowRead(ctx, d)
	if err != nil {
		return w, err
	}
	type ability struct {
		name, cost string
		n          int
	}
	var abilities []ability
	for _, p := range field {
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
	for _, rc := range hand {
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
			m := reOnSpell.FindStringSubmatch(c.Oracle)
			w.OnSpell = append(w.OnSpell, m != nil)
			if m != nil {
				w.Targets = append(w.Targets, m[1])
			} else {
				w.Targets = append(w.Targets, "")
			}
		}
	}
	for _, a := range abilities {
		if a.n <= w.Mana {
			w.Options = append(w.Options, fmt.Sprintf("%s ability %s", a.name, a.cost))
			w.OnSpell = append(w.OnSpell, false)
			w.Targets = append(w.Targets, "")
		}
	}
	return w, nil
}

// windowRead is your battlefield and hand: one light pad call, or a full
// snapshot and the hand from a pad older than v1.2.0.
func windowRead(ctx context.Context, d Deps) ([]pad.Permanent, []pad.RawCard, error) {
	if r, err := d.Pad.Window(ctx); err == nil {
		return r.Battlefield, r.Cards, nil
	} else if !strings.Contains(err.Error(), "unknown op") {
		return nil, nil, err
	}
	snap, err := d.Pad.Snapshot(ctx)
	if err != nil {
		return nil, nil, err
	}
	var field []pad.Permanent
	found := false
	for _, s := range snap.Seats {
		if s.IsClaude {
			field, found = s.Battlefield, true
		}
	}
	if !found {
		return nil, nil, errors.New("no seat")
	}
	h, err := d.Pad.Hand(ctx)
	if err != nil {
		return nil, nil, err
	}
	return field, h.Cards, nil
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

// awaitRound waits for round id to settle. A Wait press on a decision pad
// stops the pad's countdown, so the deadline follows it: while anyone holds
// it paused the wait runs on up to maxWait in all, and once it resumes, for
// the seconds the pad has left. paused reports a round still held at the end.
func awaitRound(ctx context.Context, d Deps, id int, limit, maxWait time.Duration) (hit Event, seen []Event, paused bool) {
	start := time.Now()
	deadline := start.Add(limit)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return nil, seen, paused
		}
		var got []Event
		hit, got = Await(ctx, d, left, func(e Event) bool {
			n, _ := e["id"].(float64)
			ev := e.Str("ev")
			return (ev == "round" || ev == "round_pause") && int(n) == id
		})
		seen = append(seen, got...)
		if hit == nil {
			return nil, seen, paused
		}
		if hit.Str("ev") == "round" {
			return hit, seen, false
		}
		seen = append(seen, hit)
		paused = hit.Bool("anyPaused")
		if paused {
			deadline = start.Add(maxWait)
		} else {
			secs, _ := hit["left"].(float64)
			deadline = time.Now().Add(time.Duration(secs+5) * time.Second)
		}
	}
}
