package mcpserver

import (
	"context"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/MrDentalStone/Nemesis/internal/cards"
)

// Which of your spells get a response window. A casual table does not want
// "any responses?" on every Sol Ring or Preordain, so a spell is held on the
// stack only when it could matter to someone else: a Game Changer (Scryfall's
// list, the one the brackets use), mana value 4 or more, anything that
// interacts with other players, or your commander. The rest resolve at once;
// a counter dropped on your board shortly after still counts (lateResponse).

const windowMV = 4

var reInteracts = regexp.MustCompile(`(?i)\btarget\b|\bdestroy|\bexile\b|counter target|gain control|each opponent|each player|` +
	`each other player|sacrifices?\b|deals? \d+ damage|deals? x damage|-\d+/-\d+|return .* to (its|their) owners?'? hand|can't (attack|block|cast)`)

// Text that mentions targets or exile without touching anyone: reminder text
// in parentheses (flashback's "then exile it", shroud's "can't be the
// target") and protection wording (hexproof, ward).
var reNoInteraction = regexp.MustCompile(`(?i)\([^)]*\)|can't be the targets? of[^.]*|becomes the target of[^.]*`)

// needsWindow says whether to hold the spell for responses, and why.
func needsWindow(ctx context.Context, d Deps, c cards.Card, from string) (bool, string) {
	mv := c.CMC
	if c.ManaCost != "" {
		mv = manaIn(c.ManaCost)
	}
	switch {
	case from == "command":
		return true, "your commander"
	case isGameChanger(ctx, d, c.Name):
		return true, "a Game Changer"
	case mv >= windowMV:
		return true, fmt.Sprintf("mana value %d", mv)
	case reInteracts.MatchString(reNoInteraction.ReplaceAllString(c.Oracle, "")):
		return true, "it affects other players"
	}
	return false, ""
}

var gameChangers = struct {
	sync.Mutex
	known map[string]bool
}{known: map[string]bool{}}

// isGameChanger asks Scryfall once per card name; unknown or offline is no.
func isGameChanger(ctx context.Context, d Deps, name string) bool {
	if d.Scryfall == nil || name == "" {
		return false
	}
	gameChangers.Lock()
	gc, ok := gameChangers.known[name]
	gameChangers.Unlock()
	if ok {
		return gc
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	found, _, err := d.Scryfall.Collection(ctx, []string{name})
	if err != nil {
		return false
	}
	gc = found[name].GameChanger
	gameChangers.Lock()
	gameChangers.known[name] = gc
	gameChangers.Unlock()
	return gc
}

// The turn as the server sees it: begin_turn's draw (so a second draw needs
// a reason), and the last spell that resolved without a window.
var turnState struct {
	sync.Mutex
	drew      bool
	quietName string
	quietGUID string
	quietAt   time.Time
	quietTo   string
}

func setDrew(v bool) {
	turnState.Lock()
	turnState.drew = v
	turnState.Unlock()
}

func noteQuietCast(name, guid, to string) {
	turnState.Lock()
	turnState.quietName, turnState.quietGUID, turnState.quietAt, turnState.quietTo = name, guid, time.Now(), to
	turnState.Unlock()
}

// lateResponse: the quick spell a card dropped on your board may be
// answering ("" when none was cast in the last 20 s).
func lateResponse() string {
	turnState.Lock()
	defer turnState.Unlock()
	if turnState.quietName == "" || time.Since(turnState.quietAt) > 20*time.Second {
		return ""
	}
	return turnState.quietName + " [" + turnState.quietGUID + "], now in " + turnState.quietTo
}
