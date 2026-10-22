#!/usr/bin/env bash
# Pair a phone with deck-remote by QR (pair.go): asks the server for a single-use pairing code
# and prints it as a terminal QR. On the phone: open deck-remote (the home-screen app), tap
# "Scan pairing QR" (Settings → Forget token first if it already has one). The code works once,
# for 3 minutes; an act token may then enrol Face ID without the 6-digit code.
#
#   scripts/pair-phone.sh [name] [caps]       default: iphone  act,run:ro,run:mut
#
# Env: DECK_REMOTE_URL (https://blackbird.taildcdada.ts.net:8443), DECK_REMOTE_TOKEN_FILE
# (~/.agent-deck/web-token). The request goes to 127.0.0.1 (the offer is laptop-only); the URL's
# host is only the TLS name. The bearer never appears in argv.
set -euo pipefail
name="${1:-iphone}"
caps="${2:-act,run:ro,run:mut}"
url="${DECK_REMOTE_URL:-https://blackbird.taildcdada.ts.net:8443}"
tokfile="${DECK_REMOTE_TOKEN_FILE:-$HOME/.agent-deck/web-token}"
command -v qrencode >/dev/null || { echo "qrencode not found (brew install qrencode)" >&2; exit 1; }
[ -r "$tokfile" ] || { echo "no web token at $tokfile" >&2; exit 1; }
hostport="${url#*://}"; hostport="${hostport%%/*}"
host="${hostport%%:*}"; port="${hostport##*:}"
[ "$port" = "$hostport" ] && port=443
body=$(python3 -I -c 'import json,sys; print(json.dumps({"name": sys.argv[1], "caps": [c for c in sys.argv[2].split(",") if c]}))' "$name" "$caps")
resp=$(curl -sS --fail-with-body --resolve "$host:$port:127.0.0.1" \
  -H @<(printf 'Authorization: Bearer %s\n' "$(tr -d '\r\n' < "$tokfile")") \
  -H 'Content-Type: application/json' -d "$body" "$url/api/rc/devices/pair") || {
  echo "pair offer refused: $resp" >&2; exit 1; }
qr=$(printf '%s' "$resp" | python3 -I -c 'import json,sys; print(json.load(sys.stdin)["url"])')
printf '%s' "$qr" | qrencode -t ansiutf8
printf '%s' "$resp" | python3 -I -c '
import json, sys, time
d = json.load(sys.stdin)
until = time.strftime("%H:%M:%SZ", time.gmtime(time.time() + d["expiresIn"]))
print("pair %s, caps %s: scan from inside deck-remote (token screen, Scan pairing QR); single use, valid until %s" % (d["name"], ",".join(d["caps"]) or "read-only", until))'
