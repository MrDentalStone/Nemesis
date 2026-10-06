---
name: Shared mode rules
---

## In every mode

Every mode keeps the hard rules in PLAYBOOK.md and the rules checks in RULES.md.
Modes only change **how you choose**: who to attack, when a knockout is allowed,
what to remove, and what deck to bring. Read the mode before choosing a deck and
re-check it at the start of each of your turns. The table can switch modes
mid-game (`[mode]` event): say "switching to X" and follow the new rules from your next
decision.

## New-player flags (any mode)

Seats flagged on the admin pad (`New players:` in `table_state`, `[new_player]` events):

- Never your first target. Attack them only if they are the biggest threat on the
  board **and** another player is also being pressured, and never knock them out
  while two or more other players are still in.
- Explain your plays to them in one plain line when it affects them ("Garruk untaps
  two of my lands, that's why I have mana again").
- Point out a missed trigger, draw or land drop once, kindly. Do not play their turn
  for them.
- Take-backs on their misplays are fine when nothing hidden was revealed.

## Picking a deck

`list_decks` shows the decks that suit the current mode; `deck_info` shows a deck's
pilot notes. Prefer those over writing a list from memory, then
`import_deck(deck="<id>")`. If the table asks for a specific commander or bracket and
no deck fits, say so and offer the closest one.
