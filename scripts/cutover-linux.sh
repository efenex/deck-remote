#!/bin/bash
# Bring deck-remote online over the tailnet (HTTPS) and front it with Tailscale.
# Linux / systemd (user) counterpart to scripts/cutover.sh (macOS / launchd).
#
# Prerequisites:
#   1. Tailscale admin console -> enable HTTPS Certificates + MagicDNS (required
#      for iOS Web Push / a secure context).
#   2. A single agent-deck instance per profile ([instances] allow_multiple=false).
#      Do NOT run a second headless `agent-deck web` — see README "Operational note".
#   3. A systemd user unit for deck-remote installed at
#      ~/.config/systemd/user/<UNIT>.service (see deploy/ for a template).
#
# Idempotent. Linux / systemd --user.
set -euo pipefail

TS="$(command -v tailscale 2>/dev/null || echo tailscale)"
PORT="${DECK_REMOTE_PORT:-8781}"
UNIT="${DECK_REMOTE_UNIT:-deck-remote}"

HOST="$("$TS" status --json | python3 -c 'import sys,json;print(json.load(sys.stdin)["Self"]["DNSName"].rstrip("."))')"
echo "==> tailnet host: $HOST"

# 1. HTTPS certs must be enabled (otherwise tailscale serve HTTPS / iOS push fail).
if ! "$TS" status --json | python3 -c 'import sys,json;exit(0 if json.load(sys.stdin).get("CertDomains") else 1)'; then
  echo "!! Enable HTTPS Certificates + MagicDNS in the Tailscale admin console, then re-run." >&2
  exit 1
fi

# 2. (Re)load the deck-remote systemd user service.
UNITFILE="$HOME/.config/systemd/user/$UNIT.service"
if [ ! -f "$UNITFILE" ]; then
  echo "!! Missing $UNITFILE — copy deploy/deck-remote.service there and edit the paths." >&2
  exit 1
fi
systemctl --user daemon-reload
systemctl --user enable "$UNIT"
systemctl --user restart "$UNIT"
echo "==> deck-remote service loaded on 127.0.0.1:$PORT"

# 3. Front it with Tailscale (HTTPS on the *.ts.net cert). If your Tailscale
#    version differs: `"$TS" serve --bg --https=443 http://127.0.0.1:$PORT`.
"$TS" serve --bg "$PORT"
echo "==> serving https://$HOST/ -> 127.0.0.1:$PORT"

code="$(curl -s -o /dev/null -w '%{http_code}' "https://$HOST/healthz" || true)"
echo "==> https://$HOST/healthz -> $code"
echo
echo "On the phone (Safari): open  https://$HOST/?token=<your-token>"
echo "then Share > Add to Home Screen, launch from the icon, and enable notifications."
echo
echo "Logs:     journalctl --user -u $UNIT -f"
echo "Rollback: systemctl --user disable --now $UNIT ; $TS serve --bg off"
