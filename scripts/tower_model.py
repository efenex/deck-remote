#!/usr/bin/env python3
"""tower_model.py — the tower TUI's model as JSON, for deck-remote's Tower tab.

  python3 -I scripts/tower_model.py --root <tower checkout> [--file overview.json]

Imports scripts/tower_tui.py from the tower checkout and reuses its own build_items /
split_detail / section_folds / STATUS / PALETTE, so the phone shows exactly what the TUI
derives (sections, folds, multiline detail, `! ` lines, status glyphs and colours) and can
never drift from it. Read-only: it never writes anything (writes go through journal.py,
see tower_actions.go).
"""
import argparse
import json
import os
import sys


def xterm_hex(n):
    """xterm-256 index -> #rrggbb (None for the terminal default, -1)."""
    if n is None or n < 0:
        return None
    base16 = ["000000", "800000", "008000", "808000", "000080", "800080", "008080", "c0c0c0",
              "808080", "ff0000", "00ff00", "ffff00", "0000ff", "ff00ff", "00ffff", "ffffff"]
    if n < 16:
        return "#" + base16[n]
    if n < 232:
        n -= 16
        lv = [0, 95, 135, 175, 215, 255]
        return "#%02x%02x%02x" % (lv[n // 36], lv[(n // 6) % 6], lv[n % 6])
    g = 8 + (n - 232) * 10
    return "#%02x%02x%02x" % (g, g, g)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--root", required=True)
    ap.add_argument("--file")
    a = ap.parse_args()
    sys.path.insert(0, os.path.join(a.root, "scripts"))
    import tower_tui as tt

    feed = a.file or os.path.join(a.root, tt.FEED)
    m = tt.Model(feed, os.path.join(a.root, "journal.jsonl"))
    if not m.load():
        print(json.dumps({"error": m.error}))
        return 0

    def blocks(text):
        return tt.split_detail(text or "")

    sections = []
    for n, (sid, title, items, folded) in enumerate(m.visible(), 1):
        hdr = m.header_item(n, sid, title, items, folded)
        out_items = []
        for it in items:
            st = it["status"]
            glyph, style = tt.STATUS.get(st, tt.STATUS[""])
            o = {k: it[k] for k in ("kind", "id", "key", "head", "attr", "folded", "fold_ok", "confirm_ok", "bangs", "copy_text", "status")}
            o["glyph"], o["style"] = glyph, style
            o["group"] = it.get("group")
            k = it["kind"]
            if k == "ask":
                bl = blocks(it["detail"])
                metas = it.get("items_meta") or []
                # run-bang (deck-remote docs/design/run-bang.md): the TUI's own phase-1 rules per `! ` item, so the
                # phone's badges and Run offer cannot drift from the laptop's. Only `items` entries carry "rb": a `! `
                # inside the detail text stays copy-only (extraction boundary, §3.1). An older tower without
                # run_choices degrades to copy-only.
                choices = iter(tt.run_choices(it) if hasattr(tt, "run_choices") else [])
                for k, x in enumerate(it["items"]):      # the TUI prints the `items` list as commands under the detail
                    b = {"k": "cmd", "t": x}
                    meta = metas[k] if k < len(metas) and isinstance(metas[k], dict) else {}
                    if isinstance(x, str) and x.startswith("! ") and hasattr(tt, "run_choices"):
                        c = next(choices, None)
                        if c is not None and meta.get("bang"):
                            b["rb"] = {"index": c["index"], "sha256": c["sha"], "class": c["cls"], "sig": c["sig"], "ok": c["ok"],
                                       "why": c["why"], "run": meta.get("run"), "sig_exp": meta.get("sig_exp"), "learnings": meta.get("learnings") or []}
                    bl.append(b)
                o.update({"tag": it["tag"], "meta": it["meta"], "summary": it.get("summary"), "blocks": bl})
            elif k == "request":
                lead, trail, text = it["lead"]
                o.update({"lead": lead.strip(), "text": text, "trail": trail.strip(), "tail": it["tail"],
                          "blocks": blocks(it["copy_text"]), "tail_blocks": blocks(it["tail"])})
            elif k == "suggest":
                o.update({"title": it["title"], "meta": it["meta"], "prompt": it["prompt"], "task": it.get("task"),
                          "why_blocks": blocks(it["why"]), "action_blocks": blocks(it["action"])})
            else:
                o["body"] = [blocks(b) for b in it["body"] if b]
            out_items.append(o)
        sections.append({"id": sid, "title": title, "count": hdr["count"], "urgent": hdr["urgent"], "folded": hdr["folded"] if folded else None,
                         "items": out_items})
    pal = {}
    for name, (fg256, _fg8, flags) in tt.PALETTE.items():
        pal[name] = {"fg": xterm_hex(fg256), "dim": "d" in flags, "bold": "b" in flags, "underline": "u" in flags}
    legend = [{"status": s, "glyph": tt.STATUS[s][0], "style": tt.STATUS[s][1], "label": label} for s, label in tt.STATUS_LEGEND]
    print(json.dumps({"header": m.header(), "generated_at": m.o.get("generated_at"), "sections": sections,
                      "palette": pal, "legend": legend, "gaq_stale": bool((m.o.get("ga_queue") or {}).get("stale"))}))
    return 0


if __name__ == "__main__":
    sys.exit(main())
