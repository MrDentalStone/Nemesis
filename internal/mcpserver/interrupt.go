package mcpserver

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/MrDentalStone/Nemesis/internal/cards"
)

// Interrupts. The humans have no stack: a spell is a card that lands on their
// mat, and by the time the model has read the [play] line the table has moved
// on. So the server looks at every play the moment it arrives (ttsapi's
// OnEvent hook, while the model may still be busy): it works out your
// response window, and when you could counter or copy the spell and the spell
// matters (needsWindow, the same bar your own spells clear), the pad holds
// the table: "Hold on", the card lights up. You answer with
// cast(respond_to=...) or pass; a slow answer runs out on the pad's timer.
//
// The window worked out here is reused by NextEvents, so a play costs one
// read of your hand and battlefield, not two.

// playInfo is what the server worked out about one play.
type playInfo struct {
	done  chan struct{} // closed when w/err/held are set
	w     Window
	err   error
	ok    bool // w was worked out (not for a land)
	held  bool // the pad is holding the table for you
	name  string
	color string
	to    string // battlefield | graveyard | exile
	at    time.Time
}

var seenPlays = struct {
	sync.Mutex
	m map[string]*playInfo
}{m: map[string]*playInfo{}}

// watchPlays hooks the live event stream.
func watchPlays(d Deps) {
	if d.TTS == nil || d.Pad == nil {
		return
	}
	d.TTS.OnEvent(func(raw json.RawMessage) {
		// decode, never match bytes: TTS re-encodes the message, so the
		// spacing and key order are not the pad's
		var e Event
		if json.Unmarshal(raw, &e) != nil || e.Str("ev") != "play" || e.Str("guid") == "" || e.Str("to") == "your_board" {
			return
		}
		pi := &playInfo{done: make(chan struct{}), color: e.Str("color"), to: e.Str("to"), at: time.Now(),
			name: cards.Parse(e.Str("name"), e.Str("desc")).Name}
		seenPlays.Lock()
		for g, old := range seenPlays.m {
			if time.Since(old.at) > 5*time.Minute {
				delete(seenPlays.m, g)
			}
		}
		seenPlays.m[e.Str("guid")] = pi
		seenPlays.Unlock()
		go considerPlay(d, e, pi)
	})
}

func considerPlay(d Deps, e Event, pi *playInfo) {
	defer close(pi.done)
	if isLandPlay(e) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pi.w, pi.err = ResponseWindow(ctx, d)
	pi.ok = pi.err == nil
	if !pi.ok || !holdFor(ctx, d, e, pi.w) {
		return
	}
	err := d.Pad.Call(ctx, "hold", map[string]any{"guid": e.Str("guid"), "name": pi.name, "color": pi.color}, nil)
	pi.held = err == nil
}

// holdFor: hold the table only for a spell being cast (not a land, a
// face-down card or something put onto the battlefield from a graveyard),
// that one of your answers can legally take (Negate passes on creatures),
// and that matters.
func holdFor(ctx context.Context, d Deps, e Event, w Window) bool {
	if e.Bool("faceDown") || e.Str("from") == "graveyard" || e.Str("from") == "battlefield" {
		return false
	}
	c := cards.Parse(e.Str("name"), e.Str("desc"))
	answer := false
	for i := range w.Options {
		if i < len(w.OnSpell) && w.OnSpell[i] {
			// Negate cannot take a creature spell
			answer = answer || i >= len(w.Targets) || canTarget(w.Targets[i], c.TypeLine)
		}
	}
	if !answer {
		return false
	}
	matters, _ := needsWindow(ctx, d, c, e.Str("from"))
	return matters
}

// playFor waits briefly for the server's look at a play; nil if it never
// saw one (a drained event, the CLI) or took too long.
func playFor(guid string) *playInfo {
	seenPlays.Lock()
	pi := seenPlays.m[guid]
	seenPlays.Unlock()
	if pi == nil {
		return nil
	}
	select {
	case <-pi.done:
		return pi
	case <-time.After(5 * time.Second):
		return nil
	}
}

// knownPlay is what the server saw of a play, without waiting.
func knownPlay(guid string) *playInfo {
	seenPlays.Lock()
	defer seenPlays.Unlock()
	return seenPlays.m[guid]
}
