# Theme options — design note (2026-10-20, PREVIEW)

Operator ask: fewer rounded corners, denser layout, maybe a light theme, and
opencode-style turns (own input on its own background, model output plain).
Answer: four axes as `data-*` attributes on `<html>`, driven by CSS custom
properties. That covers the request without full theming. The current look
stays the default until the operator picks an option.

## How to try it on the phone

- **Settings tab → "Appearance · PREVIEW"**: tap a preset (Classic / Opencode /
  Dense / Paper), or mix the single axes (Colour, Density, Corners, Your turns).
  The choice applies immediately and is saved on that device only
  (`localStorage['dr-theme']`).
- **Link form**: open `https://<deck-remote>/?preview=opencode`
  (also `dense`, `paper`; `off` or `classic` resets). The parameter is removed
  from the URL after it is applied.
- While a non-default look is on, the Deck title shows a dashed
  `preview: <name>` chip.

## Axes

| attribute | values (first = default) | what it changes |
|---|---|---|
| `data-theme` | `dark` · `opencode` · `light` | palette tokens (`--bg … --cyan`, `--title`, `--code-bg`, …) |
| `data-density` | `comfortable` · `compact` | card/message/convo padding, gaps, message font 14→13.5px, title 26→22px |
| `data-shape` | `round` · `flat` | `--round` multiplier on every fixed radius (1 → .15), card radius 16→2px |
| `data-turns` | `rail` · `block` | `rail`: today's gutter `›`/`‹` with an accent rail on replies. `block`: own turn on a tinted `--turn-me-bg` block with a coloured left edge, replies plain with no rail |

`data-theme="dark"` is in the shipped HTML. For the other three axes, the default
is "attribute absent". Every override selector matches only a non-default value,
so the default rendering is the old CSS.

Compact does not touch two values: `--tap` (44px iOS hit targets) and the
composer's 16px font (iOS focus-zoom).

## Presets

| preset | colour | density | shape | turns | screenshot description (390px wide) |
|---|---|---|---|---|---|
| **Classic** | dark (Tokyo Night) | comfortable | round | rail | Today's app, unchanged: navy background, 16px-rounded cards with 13px padding, white titles, blue rail and `‹` on replies, dim `›` on your turns. |
| **Opencode** | opencode (#0a0a0a, peach #fab283 primary) | compact | flat | block | Near-black, neutral greys, square-cornered cards roughly 20–25% shorter (estimate from the padding/gap tokens). Your messages sit in a #1e1e1e block with a blue left edge. Replies are plain text with no gutter. Send button and highlights are peach. Closest to opencode's TUI. |
| **Dense** | dark (Tokyo Night) | compact | flat | block | Same palette as today with Opencode's layout: tight square cards, more sessions per screen, own turns on a #24283b block. |
| **Paper** | light | comfortable | round | block | Off-white page, white cards with a soft border, near-black text, darker Tokyo hues (blue #2e6bd6, orange needs-you). Own turns on a pale blue-grey block. For daylight and outdoor use. |

Rendered on a static harness with headless Chrome while building this: no
clipped text, and the code blocks, chips and approve pill stay readable in all four.

## Tokens

New in `:root`. Each one defaults to the literal it replaced:

- colour roles: `--title` (#fff), `--on-accent` (#0b0f1a), `--code-bg`, `--grabber`,
  `--btn-off-bg`/`--btn-off-fg`, `--toast-bg`, `--vbanner-info-bg`,
  `--appbar-fade`/`--tabbar-fade`, `--turn-me-bg`, `--turn-me-edge`
- shape: `--round` (1). Every `border-radius:Npx` became `calc(Npx*var(--round))`
  (52 sites); 50% circles (LEDs, dots, knobs) are untouched.
- density: `--pad-card`, `--gap-card`, `--gap-card-in`, `--min-card`, `--gap-section`,
  `--pad-msg`, `--font-msg`, `--lh-msg`, `--pad-convo`, `--pad-deck`,
  `--title-size`, `--font-reply`
- existing palette tokens (`--bg --panel --card --card-hi --text --fg --muted
  --muted-2 --border --border-hi --accent --green --red --yellow --purple
  --orange --cyan`) are re-pointed per theme.

**Proof that the default is unchanged.** Substitute each new token's default back into the
base stylesheet and drop the appended option block. The result is byte-identical
to HEAD f0c3cc7's `<style>` (script: substitute and diff; result `IDENTICAL`).
The only default-visible addition is the new Settings section itself.

## Known limits

- Tinted backgrounds built from `rgba(122,162,247,…)` and similar (chips, code
  spans, live-activity band, approve dock) keep the Tokyo hues in every theme.
  On Opencode they read slightly bluish. If Opencode wins, tokenise them
  (`--accent-tint` etc.).
- The Tower tab is only partly tokenised (light theme has explicit fixes).
  `terminal.html` is not themed.
- iOS reads `apple-mobile-web-app-status-bar-style` at install time. On Paper
  the status bar stays light-on-translucent. `theme-color` is updated live.
- The choice is per device, not synced through the server.

## Recommendation

Make **Dense** the new default, with **Opencode** as the second choice.
Dense keeps the palette the operator has used all along (state colours,
needs-you orange, working blue). It also delivers the three things asked for:
flat corners, visibly more cards per screen, and opencode-style own-turn
blocks. Opencode's neutral palette is the closest match to the reference, but
its peach accent overlaps the orange "needs you" state colour. Keep **Paper** as
a user toggle rather than the default.

Once picked, a small follow-up turns the chosen axes into defaults: flip the
`:root` values and drop the preview chip and label. Keep the axes as user
toggles.
