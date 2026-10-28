# Multi-select TUI evidence (G15)

Verified 2026-10-28 in **Claude Code 2.1.312**, using a throwaway Claude session
in `/private/tmp`, tmux server `dr-g15`, session `fixture`, 120 × 45 cells.
The production tmux server and deck-remote service were not touched.

The existing `askq-multi-single-pane.txt` was a real 2026-10-21 capture but did
not establish key semantics. The new `askq-multi-*.txt` captures below are raw
`tmux -L dr-g15 capture-pane -pt fixture` output, with unused trailing blank
screen rows removed, not simulated screens.

## Observed transitions

The initial tool call had Multi / “Pick several?” with One (First), Two
(Second), Three (Third), and a second single-select Solo / “Pick one?” question.

| Input | Observed result | Capture |
| --- | --- | --- |
| Space on first row | Checks One; stays on question | `askq-multi-toggled.txt` |
| Down, Enter | Moves to Two, checks it; stays on question | visible in subsequent digit capture |
| `3` | Checks Three without moving cursor from Two | `askq-multi-digit.txt` |
| `4` | Checks Other without focusing its editor | inspected live before next transition |
| Down, Down, literal `custom words` | Focuses Other and edits its text | `askq-multi-other-editing.txt` |
| Enter | Unchecks Other; stays in editor | inspected live |
| Enter, Down | Rechecks Other, focuses unnumbered Next | `askq-multi-next.txt` |
| Enter on Next | Advances to Solo | inspected live |
| Left | Returns to Multi, retains selections/text, resets cursor to first row | inspected live |
| Right | Advances to Solo with selections retained | `askq-multi-right.txt` |
| `1` on Solo | Selects Yes and reaches review | `askq-multi-review.txt` |
| Left twice, individually delivered `1`, `2`, `3`, `4` | Clears all checkboxes | `askq-multi-empty.txt` |
| Right twice | Review warns “You have not answered all questions”; no answer for Multi | `askq-multi-empty-review.txt` |
| Left twice, `3`, then `1`, Right twice | Review lists Three before One (toggle order, not row order) | `askq-multi-order-review.txt` |
| `1` on review | Submits answers; Claude echoes Three/One and Yes | `askq-multi-submitted.txt` |

Distinct digit toggles need distinct input events. Sending `1 2 3 4` together
in one `tmux send-keys` call left the selections unchanged, whereas individually
delivered digits toggled them. The implementation sends and verifies each one.

Then a second AskUserQuestion contained **only** Multi / “Pick several?” /
One, Two, Three, with `multiSelect=true`:

- `2`, then Right reaches review even though the footer only advertises ↑/↓:
  `askq-multi-single-review.txt`.
- Left, Down three times, typing `extra choice` into previously unchecked Other
  **automatically checks it**: `askq-multi-other-auto.txt`.
- Right while editing Other only moves its text cursor; it does not change tabs.
- Down leaves the editor for Submit; Enter reaches review, retaining both Two
  and the custom text: `askq-multi-other-review.txt`.
- Returning to Other and erasing all 12 characters with BSpace restores
  `Type something` and unchecks it. The footer can still advertise ctrl+g when
  Next/Submit is focused; parser editing state therefore uses the actual row.
- Up exits Other to the preceding option. The automated answer walker below
  exercised this transition and then Right successfully.

## Independent implementation check

`TestMultiSelectLiveFixture` ran the actual Go parser, key builder, per-key
snapshot checks, review verification and submit against that real TUI. Starting
with Two checked and cursor on Three, it sent:

```text
text:1, text:2, text:3, Down, text:phone answer, Up, Right, text:1
```

It passed, and Claude echoed **One, Three, phone answer**. Saved output:
`askq-multi-walker-submitted.txt`. The isolated tmux server was then destroyed.

To repeat, launch a separate tmux server with a session named `fixture`, run
Claude with only AskUserQuestion enabled, request the exact single-question
fixture above, and leave the cursor outside Other. Then run:

```sh
DECK_REMOTE_ASK_FIXTURE_TMUX=your-isolated-server go test -run '^TestMultiSelectLiveFixture$' -v .
```

This test sends keys and submits the fixture. It is skipped by default and
explicitly refuses the default tmux server. Normal tests exercise captured panes
and a simulator grounded in these transitions.

Empty selection is rejected before any key is sent because Claude treats it as
unanswered. More than eight supplied options (plus Other) retain the existing
read-only fallback: multi-digit shortcuts were not verified by this fixture.
