"""Static server for deck-remote web/ with faked /api/rc/* (no secrets, no agent-deck)."""
import json, sys, time, os
from http.server import ThreadingHTTPServer, SimpleHTTPRequestHandler
from urllib.parse import urlparse, parse_qs

WEB = sys.argv[1]; PORT = int(sys.argv[2])
now = time.time()
LONG = ("I traced the flake to the retry wrapper in `internal/sync/backoff.go`: it reuses the same context after cancel.\n\n"
        "```go\nfunc (r *Retrier) Do(ctx context.Context, fn func(context.Context) error) error {\n\tfor attempt := 0; attempt < r.max; attempt++ {\n\t\tif err := fn(ctx); err == nil { return nil } else if errors.Is(err, context.Canceled) { return fmt.Errorf(\"retry aborted after %d attempts: %w\", attempt, err) }\n\t}\n\treturn ErrExhausted\n}\n```\n\n"
        "## Next steps\n- Thread a fresh child ctx per attempt\n- Add a regression test `TestRetrierFreshCtxPerAttempt`\n- Re-run `go test ./internal/sync/... -race -count=50`\n\n**Want me to apply this?**")
QUOTA = [dict(groups=["ligna / services", "deck-remote"], fiveHour=dict(pct=86, resetsAt=int(now)+3720), sevenDay=dict(pct=52, resetsAt=int(now)+3*86400+4*3600), asOf=int(now)-30),
         dict(groups=["infra / blackbird"], fiveHour=dict(pct=8, resetsAt=int(now)+9000), sevenDay=dict(pct=31, resetsAt=int(now)+2*86400), asOf=int(now)-5400)]
S = [
 dict(id="s1", title="ligna-fleet-dispatcher-refactor", path="~/work/ligna/services/dispatcher", group="ligna / services", tool="claude",
      lastReply="Done — 14 files changed, all tests green. PR #482 opened against main with the migration notes.", working=False, lastActivity=now-600,
      needsAttention=True, capabilities=dict(ask=True), ctx=dict(pct=91, tokens=910000, window=1000000, model="Opus 5.5", asOf=now-30)),
 dict(id="s2", title="codex-rollout-parser", path="~/work/deck-remote", group="deck-remote", tool="codex",
      lastReply="", working=True, activity="Reticulating… (4m 12s · ↓ 18.2k tokens · esc to interrupt)", currentTool="Bash(go test ./... -run TestCodexRollout -count=1 -v 2>&1 | tail -80)",
      lastActivity=now-5, capabilities=dict(ask=True, steer=True, interrupt=True)),
 dict(id="s3", title="agentmemory-summarize", path="~/work/agentmemory", group="agentmemory", tool="claude",
      lastReply="", working=True, stalled=True, activity="Pondering… (10m 0s · ↓ 2.1k tokens)", currentTool="Task(Explore: find all summarize callers)", lastActivity=now-620, capabilities=dict(ask=True),
      ctx=dict(pct=74, tokens=148000, window=200000, model="Sonnet 4.5", asOf=now-90)),
 dict(id="s4", title="blackbird-tailscale-services", path="~/infra/blackbird", group="infra / blackbird", tool="claude",
      lastReply="The serve config is applied; `tailscale serve status` shows the svc:deck host bound to :443.", working=False, lastActivity=now-3600*3, capabilities=dict(ask=True),
      ctx=dict(pct=12, tokens=120000, window=1000000, model="Opus 5.5", asOf=now-600)),
 dict(id="s5", title="docs-sweep", path="~/work/ligna/docs", group="ligna / docs", tool="claude",
      lastReply="Rewrote 6 pages; see the diff summary below.", working=False, lastActivity=now-86400*3, capabilities=dict(ask=True)),
 dict(id="s6", title="gemini-scratch", path="~/scratch/gemini", group="scratch", tool="gemini",
      lastReply="", working=False, lastActivity=0, capabilities=dict(ask=False), degradedReason="Terminal-only harness — open terminal"),
]
HIST = {"s1": [dict(role="user", content="why does the dispatcher test flake under -race?", ts=now-1500),
               dict(role="reply", content=LONG, ts=now-1400),
               dict(role="user", content="yes apply it, and open a PR", ts=now-1300),
               dict(role="reply", content="Applied. Opening the PR now — I need to run `gh pr create`.", ts=now-700)]*2,
        "s2": [dict(role="user", content="add a parser for the 0.145 rollout format", ts=now-400),
               dict(role="commentary", content="Reading codex_rollout.go and the testdata fixtures first.", ts=now-380),
               dict(role="commentary", content="Fixture rollout-0.145.jsonl has a new `response_item.reasoning` shape; adding a case.", ts=now-200)]}
ATT = {"s1": dict(pending=True, kind="approval", id="att-1", text="Bash command\n\n  gh pr create --base main --head feat/dispatcher-ctx --title \"dispatcher: fresh ctx per retry attempt\" --body-file /tmp/pr-body.md\n\nDo you want to proceed?\n❯ 1. Yes\n  2. Yes, and don't ask again for gh pr create commands in /Users/me/work/ligna\n  3. No, and tell Claude what to do differently (esc)", actions=["approve-once","approve-prefix","decline"])}
NOTIF = [dict(id="n1", title="ligna-fleet-dispatcher-refactor", kind="approval", at=int((now-600)*1000), sessionId="s1", body="Permission: Bash(gh pr create --base main --head feat/dispatcher-ctx …)"),
         dict(id="n2", title="agentmemory-summarize", kind="stall", at=int((now-60)*1000), sessionId="s3", body="Spinner frozen for 10m: `Pondering… (10m 0s · ↓ 2.1k tokens)`"),
         dict(id="n3", title="blackbird-tailscale-services", kind="reply", at=int((now-3600*3)*1000), sessionId="s4", body=LONG),
         dict(id="n4", title="deck-remote", kind="test", at=int((now-86400)*1000), body="Test push")]

class H(SimpleHTTPRequestHandler):
    def __init__(s, *a, **k): super().__init__(*a, directory=WEB, **k)
    def log_message(s, *a): pass
    def j(s, obj, code=200):
        b = json.dumps(obj).encode(); s.send_response(code)
        s.send_header("Content-Type","application/json"); s.send_header("Content-Length",str(len(b))); s.end_headers(); s.wfile.write(b)
    def do_POST(s): s.j({"ok": True})
    def do_GET(s):
        u = urlparse(s.path); q = parse_qs(u.query); p = u.path; sid = (q.get("id") or [""])[0]
        if p == "/api/rc/events":
            s.send_response(200); s.send_header("Content-Type","text/event-stream"); s.end_headers()
            try:
                while True: s.wfile.write(b": ping\n\n"); s.wfile.flush(); time.sleep(5)
            except Exception: return
        if p == "/api/rc/sessions": return s.j({"sessions": S, "quota": QUOTA})
        if p == "/api/rc/profiles": return s.j({"profiles":[{"name":"default","isDefault":True},{"name":"work"}],"current":"default","proxyProfile":"default"})
        if p == "/api/rc/history":
            off = int((q.get("offset") or ["0"])[0]); lim = int((q.get("limit") or ["50"])[0])
            if os.environ.get("SLOWHIST") and lim == int(os.environ.get("SLOWLIM","30")): time.sleep(float(os.environ["SLOWHIST"]))
            if off > 0: return s.j({"messages": [], "hasMore": False})
            h = HIST.get(sid, []); return s.j({"messages": h[-lim:], "hasMore": False})
        if p == "/api/rc/activity":
            x = next((v for v in S if v["id"]==sid), {}); return s.j({k: x.get(k) for k in ("working","activity","currentTool","stalled")})
        if p == "/api/rc/attention": return s.j(ATT.get(sid, {"pending": False}))
        if p == "/api/rc/reply":
            x = next((v for v in S if v["id"]==sid), {}); return s.j({"content": x.get("lastReply",""), "timestamp": now})
        if p == "/api/rc/notifications": return s.j({"notifications": NOTIF})
        if p == "/api/rc/push/config": return s.j({"enabled": True, "publicKey": "BEl62iUYgUivxIkv69yViEuiBIa"})
        if p == "/api/rc/devices/whoami": return s.j({"name":"iphone","admin":False})
        if p.startswith("/api/"): return s.j({})
        return super().do_GET()

ThreadingHTTPServer(("127.0.0.1", PORT), H).serve_forever()
