#!/usr/bin/env python3
"""tower_model.py — the tower TUI's model as JSON, for deck-remote's Tower tab.

  python3 -I scripts/tower_model.py --root <tower checkout> [--file overview.json]

Imports scripts/tower_tui.py from the tower checkout and reuses its model, section folds,
STATUS and PALETTE. Detail parsing stays local: only actual lines starting with `! ` are
commands; prose and inline code are never split heuristically. Read-only: it never writes
anything (writes go through journal.py, see tower_actions.go).
"""
import argparse
import json
import os
import re
import sys


_LEAD = re.compile(r"^(?:\(\d{1,2}\)|\([a-z]\)|\d{1,2}[.)]|NEED \d+|Q\d{1,3})\s")


def blocks(text):
    """Keep real lines intact, recognizing commands before stripping indent or bullets."""
    out = []
    for raw in (text or "").split("\n"):
        s = raw.strip()
        if not s:
            if out and out[-1]["k"] != "gap":
                out.append({"k": "gap", "t": ""})
        elif raw.startswith("! "):
            out.append({"k": "cmd", "t": "! " + raw[2:].strip()})
        elif s[:2] in ("- ", "* "):
            out.append({"k": "bullet", "t": s[2:].lstrip()})
        else:
            out.append({"k": "item" if _LEAD.match(s + " ") else "p", "t": s})
    while out and out[-1]["k"] == "gap":
        out.pop()
    return out


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

    ov = m.o if isinstance(getattr(m, "o", None), dict) else {}

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
                # Match the TUI's items-first copy list, but never inherit its heuristic detail commands.
                o["bangs"] = [x.strip() for x in it["items"] if isinstance(x, str) and x.strip()] or [
                    b["t"] for b in bl if b["k"] == "cmd"]
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
                o.update({"tag": it["tag"], "meta": it["meta"], "summary": it.get("summary"), "blocks": bl,
                          # a sequence ask's feed block {"rev", "title", "steps"[, "source"]}: the PWA offers runner mode on it
                          "seq": it.get("seq")})
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
    out = {"header": m.header(), "generated_at": m.o.get("generated_at"), "sections": sections,
           "palette": pal, "legend": legend, "gaq_stale": bool((m.o.get("ga_queue") or {}).get("stale"))}
    out.update(backlog_extras(ov))
    print(json.dumps(out))
    return 0


def backlog_extras(ov):
    """overview-backlog keys passed through from overview.json for the Tower tab's BACKLOG section.
    Absent keys (an older tower) are simply left out: the tab then shows no section."""
    out = {}
    bl = ov.get("backlog")
    if isinstance(bl, list):
        out["backlog"] = [b for b in bl if isinstance(b, dict) and isinstance(b.get("id"), str) and b["id"]]
    c = ov.get("counts")
    if isinstance(c, dict):
        cc = {k: c[k] for k in ("backlog", "suggested", "suggested_folded") if k in c}
        if cc:
            out["counts"] = cc
    reqs = ov.get("requests")
    if isinstance(reqs, list):
        # "open" is the TUI's own test (tower_tui.queued_picks): status == "open" exactly, so a declined or
        # free-text status never keeps a row "queued"
        out["open_requests"] = [r["id"] for r in reqs if isinstance(r, dict) and isinstance(r.get("id"), str)
                                and r.get("status") == "open"]
    return out


if __name__ == "__main__":
    sys.exit(main())
