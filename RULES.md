# Commander rules guide for the AI at the table

Read this before the first game of a session and re-read the checklists on
every opponent turn. The humans will test you on purpose. Verify before you
accept a claim, politely, and accept the table's ruling if they overrule you.

## 1. Turn structure

Untap, Upkeep, Draw, Main 1, Combat (beginning, declare attackers, declare
blockers, damage, end of combat), Main 2, End step, Cleanup.

- First-turn draw: in a **2-player** game the player who goes first skips their
  first draw. In a **3+ player** game nobody skips it (CR 103.8).
- One land per turn, only in your own main phase with an empty stack.
- Mana empties at the end of each step and phase.
- Damage on creatures wears off in the cleanup step. "Until end of turn"
  effects end then too.
- Each player gets priority before anything resolves. Your spells get a short
  response window from `cast`; players respond by dropping a card on your board
  or saying "wait". The stack resolves last in, first out.

## 2. Summoning sickness (the one you got tested on)

A creature can **attack** or use a **{T}/{Q} ability** only if its controller
has controlled it continuously since the start of their most recent turn.
"Haste" ignores this.

- Applies to **everyone's** creatures, including the opponents'. A creature
  cast during an opponent's turn cannot attack that same turn.
- Mana dorks cast this turn cannot tap for mana this turn.
- It does **not** stop blocking, and it does not stop abilities without the
  tap symbol.
- A creature that changes control gets sick again for the new controller.
- Flipping, transforming or having counters added does not reset it.

**How to check:** at the start of each opponent's turn, run `table_state` and
remember which creatures were already on their battlefield. Only those (plus
anything with haste) can attack this turn. If a new one is declared as an
attacker, say so: "Kaheera came down this turn, she is summoning sick, right?"

## 3. Reading cards precisely

- **"Each other creature you control"** does not pump the source. Kaheera, the
  Orphanguard is a 3/2 and gives +1/+1 to *other* Cats, Beasts and so on.
  **"Each creature you control"** or "creatures you control" includes itself.
- A lord's bonus and granted keywords apply only while the source is on the
  battlefield and only to the listed types.
- Power/toughness = printed, then +1/+1 and -1/-1 counters, then static
  effects (anthems, equipment, auras), then until-end-of-turn pumps.
- Legend rule: two legendary permanents with the same name under one
  controller, that player keeps one. It is per player, not per table.
- Companion (Kaheera's first ability): the player pays {3} as a sorcery to put
  it from outside the game into their hand, then casts it normally. Once cast
  it is an ordinary creature.
- **Evolve** puts the +1/+1 counter on the evolving creature itself, not on the
  bigger creature that entered. A 0/3 Dinosaur Egg evolves into a 1/4; the
  3/3 Beast that triggered it stays 3/3.
- **Extort**: a triggered cost the caster may pay once per spell they cast;
  each opponent loses 1, they gain that much. It is their spell, their mana.
- **Discover N** (on death, etc.): exile from the top until a nonland card with
  mana value N or less, cast it free or put it in hand.
- Equipment stays attached until moved; the table has no attach marker, so
  note it on the creature and ask "is that equipped?" when it matters (Boots
  on a commander means hexproof: target her before the equip resolves).

## 4. Counters on the table

Players track counters **on the card** through the table's right-click menu
(Pie's Encoder modules): +1/+1 counters, generic counters, loyalty, keyword
icons, a notepad, phased-out and token markers. `table_state` reads that
state and prints it after the card line, together with any physical tiles:

```
[8ec98a] Ocelot Pride — Creature — Cat 1/1  on-card: +1/+1 x2 (now 3/3); lifelink
[2b984e] Helpful Hunter — Creature — Cat 1/1  counters: +1/+1 [22a752]
```

- `on-card: +1/+1 xN (now P/T)` is the Encoder's count; the `now` value is
  printed P/T plus counters plus any P/T tweak the player entered. Static
  effects (lords, anthems, equipment) are **not** included: add those yourself.
- `loyalty N` is a planeswalker's loyalty. `N counters` are generic named
  counters (charge, oil, time...). `PHASED OUT` means it is not there this turn.
- Keyword icons are granted keywords. A `note:` is free text the player wrote.
- A `+X/+Y` tile shows its total as its name (one tile can read `+3/+3`); a
  `-1/-1` tile subtracts. Dice show their face as `d6=4`.
- Put counters on **your own** cards with `mark(guid, plus_one=2)`, keywords
  with `mark(keywords=["flying"])`, loyalty with `mark(loyalty=-3)`. Use
  `counter` tiles only if the Encoder is missing. Never touch a human's card;
  ask them.
- If the table state looks wrong (a tile between two cards, a count that does
  not match what was said), ask rather than guess.

## 5. Combat

1. **Declare attackers**: the attacking player taps them (vigilance: no tap)
   and names a defending player or planeswalker per creature. Only untapped,
   non-sick creatures. Attack triggers (Pathbreaker Ibex) go on the stack now.
2. **Declare blockers**: the defender picks untapped creatures. A creature
   blocks one attacker unless it says otherwise. Several blockers on one
   attacker are fine. Flying is blocked only by flying or reach (a 3/3 Beast
   cannot block a 1/1 Spirit with flying; the most common wrong block at this
   table). Menace needs two or more blockers. Blocking does not tap the
   blocker. A creature that attacked on its controller's turn stays tapped
   through your turn and cannot block.
3. **First strike / double strike** damage step happens first, then regular
   damage. A first striker that kills its blocker takes no damage back unless
   the blocker also had first strike.
4. **Damage**: the attacker assigns lethal damage to blockers in order;
   **trample** pushes the excess to the player; **deathtouch** makes 1 damage
   lethal. **Lifelink** gains life at the same time damage is dealt (not a
   trigger). Unblocked attackers hit the player.
5. **Commander damage**: 21 combat damage from one commander kills, tracked
   per commander per player. Use `commander_damage`; the table's tracker
   deducts the life itself, so do not also `adjust_life` for it.
6. Blocked creatures stay blocked even if the blocker is removed; without
   trample they deal no damage.

Before applying any life change: say the blocks, say the numbers, wait for
agreement, then `adjust_life` yourself for your own tracker. Players adjust
their own life totals: state the new total and let them. Ask once whether
they want you to apply agreed combat and commander damage for them; only a
yes lets you touch their tracker, and only for agreed numbers.

## 6. State-based actions (checked constantly, nobody has to ask)

- Toughness 0 or less: dies, no regeneration. Lethal damage marked: dies.
- Life 0 or less, 10 poison, 21 commander damage from one commander, or
  drawing from an empty library: that player loses.
- Legend rule. Auras with no legal target go to the graveyard. Tokens outside
  the battlefield cease to exist.
- A player who loses or scoops leaves the game: every object they own leaves
  with them (Mesmeric Orb stops milling), spells and abilities they control
  end, and control-changing effects they made end (CR 800.4a). Cards of
  theirs you control leave the game too.
- Commander going to graveyard, exile, hand or library: its owner may put it
  in the command zone instead (so Chaos Warp on a commander is a bounce with
  a tax, not a shuffle). Commander tax: +{2} for each previous cast from the
  command zone this game. Say "command zone or graveyard, your call".

## 7. Spells and timing

- Sorceries, creatures, planeswalkers, artifacts, enchantments, lands: your
  main phase, empty stack. **Flash** and **instants**: any time you have
  priority, including opponents' turns and in response.
- A counterspell targets a spell on the stack. Once a spell has resolved it
  is too late. "Any responses?" before resolving is how you give that window.
- Triggered abilities ("when", "whenever", "at") use the stack; announce them.
  Static abilities ("creatures you control get +1/+1") do not.
- Hexproof: can't be the target of opponents' spells or abilities. Shroud:
  nobody's. Ward: the targeting player pays or the spell is countered.
- Indestructible stops "destroy" and lethal damage; it does not stop
  sacrifice, exile, -X/-X or bounce. Protection from X: can't be damaged,
  enchanted, equipped, blocked or targeted by X.
- Regeneration, phasing, morph and other odd mechanics: ask the table if
  unsure.

## 8. Commander format specifics

- 100 cards, singleton except basics, 40 life, colour identity from the
  commander. Mulligan: the official rule is the London mulligan with no free
  one; many pods allow one free. Ask the table before deciding.
- The commander starts in the command zone and can be cast from there as
  often as you can pay the tax.
- Partner and Background commanders share the command zone; count their
  damage separately.
- Experience and poison counters belong to players; keep them with
  `Experience Counter` or `Poison Counter` tiles near the life tracker.

## 9. Bracket etiquette (Commander Brackets)

- Bracket 3 ("upgraded"): no mass land destruction, no extra-turn chaining,
  few tutors, up to three Game Changers, no two-card infinite combos before
  roughly turn 7. Match what the table asked for.
- Playing to win is expected. Killing a mana rock on turn 3 is fine; so is a
  joke about it.
- House formats appear; ask the rules before building. The "Krenk off": Krenko,
  Mob Boss as commander, four turns each, no attackers declared (combat phase
  still happens), one Game Changer per deck, most Goblins on the battlefield at
  the end of the fourth turn wins. Infinite combos were allowed.
- Tokens at this table are one card plus an on-card counter for the count, in
  every format. It saves everyone's PC.

## 10. Every-opponent-turn checklist

1. Their turn starts: `table_state`. Note which creatures are present and
   which are tapped.
2. They declare attackers: are they untapped? Were they on the board at the
   start of this turn, or do they have haste? Compute each P/T from printed
   plus counters plus statics. State the numbers back.
3. Decide blocks. Say them. Agree damage. Apply your own life.
3b. They declare blocks on your attack: can each blocker legally block that
   attacker (flying, menace, untapped)? Say the rule in one line if not, and
   let them re-declare.
4. Watch `[zone]` events for your permanents moving; acknowledge in chat.
5. Before your turn: untap, upkeep triggers, draw, then act.

## 11. Every-own-turn checklist

- Creatures you played this turn cannot attack without haste, and dorks
  cannot tap for mana. Keep a short list of this-turn arrivals. A creature
  cast last turn is fine this turn, even without haste.
- Opponents' taxes on you are yours to apply: Sheoldred's 2 per draw, extort,
  Castle Locthwain's life loss. Say the number and `adjust_life` yourself.
- Pay commander tax when recasting your commander.
- Announce triggers; ask for responses (`ask_table`) only before ETBs that really
  matter.
- Pass with `end_turn` after main 2; no end-step question needed.
