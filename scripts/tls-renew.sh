#!/bin/zsh
# Refresh the *.ts.net TLS pair deck-remote terminates with.
#
# `tailscale cert` is idempotent and cheap: it returns the cached Let's Encrypt
# cert until it is inside the renewal window, so running this daily costs
# nothing and means the ~90-day expiry never lands on a manual step. deck-remote
# re-reads the pair on the next handshake after the mtime moves (certReloader in
# tls.go), so NO service restart is needed here.
#
# We take the PEMs on STDOUT rather than via --cert-file/--key-file: the macOS
# Tailscale.app CLI is sandboxed and cannot write to ~/.agent-deck ("operation
# not permitted"). Stdout emits the fullchain followed by the private key.
#
# Used by deploy/io.lunascens.deck-remote-cert.plist (daily) and by
# scripts/lan-fallback.sh for the first issuance.
set -euo pipefail

TS="${TAILSCALE_BIN:-$(command -v tailscale 2>/dev/null || echo /Applications/Tailscale.app/Contents/MacOS/Tailscale)}"
TLS_DIR="${DECK_REMOTE_TLS_DIR:-$HOME/.agent-deck/tls}"

HOST="${DECK_REMOTE_HOST:-$("$TS" status --json | python3 -c 'import sys,json;print(json.load(sys.stdin)["Self"]["DNSName"].rstrip("."))')}"
[ -n "$HOST" ] || { echo "!! could not resolve this node's MagicDNS name" >&2; exit 1; }

mkdir -p "$TLS_DIR"
chmod 700 "$TLS_DIR"

# Stage into .new and rename, so a concurrent handshake never sees a half file.
# The combined PEM goes to a file first: `python3 -` would fight the cert for
# stdin, so the splitter reads the path instead.
RAW="$TLS_DIR/$HOST.pem.new"
umask 077
"$TS" cert --cert-file - --key-file - "$HOST" > "$RAW"
python3 -c '
import os, re, sys

raw_path, crt_path, key_path = sys.argv[1:4]
with open(raw_path) as fh:
    pem = fh.read()
# The key block starts the tail of the stream; everything before it is the chain.
m = re.search(r"-----BEGIN (?:[A-Z ]+ )?PRIVATE KEY-----", pem)
if m is None or m.start() == 0:
    sys.exit("!! tailscale cert produced no cert/key pair on stdout")
chain, key = pem[:m.start()].strip() + "\n", pem[m.start():].strip() + "\n"
if "BEGIN CERTIFICATE" not in chain:
    sys.exit("!! tailscale cert produced no certificate chain")
for path, body in ((crt_path, chain), (key_path, key)):
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w") as out:
        out.write(body)
' "$RAW" "$TLS_DIR/$HOST.crt.new" "$TLS_DIR/$HOST.key.new"
rm -f "$RAW"

# Rename key first: the reloader keys off the newest mtime of the pair, so the
# cert landing last is what signals "a complete new pair is ready".
mv -f "$TLS_DIR/$HOST.key.new" "$TLS_DIR/$HOST.key"
mv -f "$TLS_DIR/$HOST.crt.new" "$TLS_DIR/$HOST.crt"

echo "==> $HOST cert refreshed in $TLS_DIR"
openssl x509 -noout -subject -enddate -in "$TLS_DIR/$HOST.crt" | sed 's/^/   /'
