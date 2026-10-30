# Drafting at this table

The table can draft as well as play Commander. You draft with the same pad: it
builds the packs on the deck importer, deals them into everyone's hand zone and
passes them on. You see your own pack and pool and nothing else, the same as at a
real table.

## Formats: the number of drafters decides

| Drafters | Format | How it goes |
|---|---|---|
| 2 | **Winston** | One stack of 90 cards and three face-down piles between you. On your turn you look at pile 1: take it, or pass it (it gets the stack's top card) and look at pile 2, then pile 3. Passing pile 3 takes the stack's top card blind. Then it is the other drafter's turn. |
| 3 or 4 | **Pick-two** | Booster draft, but you take **two** cards each time a pack reaches you. Packs come back around often. |
| 5 or more | **Booster** | One card a pass, 3 packs each, pack 2 passes the other way. |

`start_draft` picks the format from the drafters; `format=` overrides it (pick-two
works for 2 too, booster for 3 or 4) when the table asks.

## Setting up

1. Agree the cards with the table: a **set** (`set="dsk"`, Scryfall's code; the
   importer makes real play boosters with the set's own rarities, 3 per drafter;
   the pad takes the basic lands out, so a pack is about 13 cards, and the table
   keeps a box of basics; Winston gets nine boosters cut to a 90-card stack;
   `set="one-jumpstart"` works too) or a **cube**
   (`cube=` a CubeCobra id or link, or `list=` the cube itself). Also who drafts,
   if not everyone seated.
2. `reset_board` first: your library spot must be empty for the deck you build
   later, and packs land on the mats.
3. `start_draft(...)`. It waits for the packs (up to 2 minutes), tells the table
   how to pick, and returns your first pack or pile.

What the humans do (the pad tells them too): their pack lands in their hand; they
take their pick out of their hand, face-down onto their mat. Packs move on once
everyone has picked, so a slow drafter holds the table. In Winston their pile goes
to their hand and a Take / Pass pad appears on their mat (chat `take` or `pass`
and the admin pad's buttons work too).

## Picking

Each pack arrives as a `[draft]` event with every card, its cost and text. Pick
with `draft_pick(guids=[...])` (Winston: `draft_pick(take=true|false)`), then
`wait_for_event`. Do not chat during the draft beyond a word if someone asks; picks
are private, so never say what you took or what you passed.

- **Early (pack 1, the first 4 or 5 picks): take the most powerful card.** Bombs
  (a rare that wins the game if unanswered) over efficient removal over evasive or
  value creatures over plain creatures over tricks and filler.
- **Read what flows.** Good cards of one colour still in pack 1's later picks mean
  your neighbour is not in it. Settle into **two colours** by early pack 2 and take
  the best card **in your colours** from then on, unless a bomb or premium removal
  shows up that is worth a splash.
- **Curve and creatures.** A limited deck wants about 15 creatures and plenty of
  2 and 3 drops. By pack 3, fill what is missing (two-drops, removal) over
  another six-drop.
- Fixing (dual lands, mana rocks) is a late pick unless you mean to splash.
- **Pick-two:** take the two best cards for you; with 3 or 4 drafters a pack comes
  back quickly, so note what is likely to come round again and take the card that
  will not.
- **Winston:** weigh the whole pile, not its best card: three playables in your
  colours beat one decent card. There is only one other drafter, so every card you
  pass may end up in their deck: taking a pile to deny a bomb is fair. Remember
  piles you passed; they grow by one card each pass. The blind stack card is an
  average card, so pass pile 3 only when it is weak.

`draft_view` shows the draft at any time: the pack or pile in front of you, who the
table waits on, and your pool with its coloured pips and curve.

## Building your deck

The draft ends with `[draft] the draft is over`. Then:

1. `draft_view`: your pool with guids, colours and curve.
2. Choose **23 spells and 17 basics** (40 cards). 16 lands with a low curve, 18
   with many five-drops. Two colours; a splash only for a bomb or removal and only
   with fixing. Roughly: 1-2 one-drops, 5-6 two-drops, 4-5 three-drops, 3-4
   four-drops, 2-3 five-drops or more. Split the basics by coloured pips.
3. `draft_build(main=[guids], basics={"Forest": 9, "Mountain": 8})`. Your library
   is built and shuffled, the rest of the pool becomes your sideboard beside it,
   your life goes to 20 and your opening 7 is dealt.

Say one short line when you are ready ("Deck built, ready when you are"). The
humans build their own decks; give them time.

## Playing limited

Same table, same tools, different game:

- **20 life**, no commander, no commander damage, 40-card libraries. Everyone sets
  their own life; remind the table once.
- In a two-player game the player who goes first skips their first draw
  (`begin_turn(skip_draw=true)`).
- Games are short and every card counts: trading creatures and using removal on the
  biggest threat is normal. The table's play mode still sets how hard you play.
- A match is often best of three. **Do not `reset_board` between games of a
  match**: it destroys your drafted deck. Ask the table how they want to handle
  it (gather the cards back by hand, or call the match after one game).
