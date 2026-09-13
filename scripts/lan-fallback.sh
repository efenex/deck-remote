#!/bin/zsh
# Make deck-remote reachable on ONE origin whether or not Tailscale is up.
#
# WHY one origin: the PWA's service worker and its Web Push subscription are
# bound to the origin. A separate LAN hostname would be a second install with a
# second subscription. So we keep https://<host>.ts.net (no port) for both paths
# and change only how it is reached:
#
#   Tailscale ON   phone -> MagicDNS -> 100.x  :443 --pf rdr--> 127.0.0.1:8443
#   Tailscale OFF  phone -> LAN DNS  -> 192.x  :443 --pf rdr--> 127.0.0.1:8443
#
# deck-remote terminates TLS itself with a `tailscale cert` pair (a real Let's
# Encrypt cert — nothing to install on the phone) and stays an unprivileged
# launchd AGENT on loopback; pf owns the privileged :443.
#
# Idempotent, macOS-only. Needs sudo for the pf steps. The router-side DNS
# override is the one part this cannot do — it is printed at the end.
#
# Rollback: scripts/lan-fallback.sh --off
set -euo pipefail

[ "$(uname -s)" = "Darwin" ] || { echo "!! macOS only" >&2; exit 1; }

TS="${TAILSCALE_BIN:-$(command -v tailscale 2>/dev/null || echo /Applications/Tailscale.app/Contents/MacOS/Tailscale)}"
REPO="${0:A:h:h}"
LABEL="${DECK_REMOTE_LABEL:-io.lunascens.deck-remote}"
CERT_LABEL="io.lunascens.deck-remote-cert"
PF_LABEL="io.lunascens.pf-deckremote"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
CERT_PLIST="$HOME/Library/LaunchAgents/$CERT_LABEL.plist"
PF_PLIST="/Library/LaunchDaemons/$PF_LABEL.plist"
ANCHOR="/etc/pf.anchors/io.lunascens.deckremote"
ANCHOR_NAME="io.lunascens.deckremote"
TLS_PORT="${DECK_REMOTE_TLS_PORT:-8443}"
TLS_DIR="${DECK_REMOTE_TLS_DIR:-$HOME/.agent-deck/tls}"
UID_N="$(id -u)"

# launchd WATCHES ~/Library/LaunchAgents, so writing a plist there can auto-load
# it before our explicit bootstrap runs — the bootstrap then loses the race and
# reports "125: Domain does not support specified action" or "5: Input/output
# error" while the job is in fact loaded and healthy. Trusting the exit code
# therefore aborts a working install. Assert the END STATE instead: after this
# returns, the label is loaded from this plist, whoever won the race.
reload_agent() { # domain label plist [sudo]
  local domain="$1" label="$2" plist="$3" pre="${4:-}"
  $pre launchctl bootout "$domain/$label" 2>/dev/null || true
  local i=0
  while $pre launchctl print "$domain/$label" >/dev/null 2>&1; do
    i=$((i + 1))
    [ "$i" -ge 50 ] && { echo "!! $label would not unload" >&2; return 1; }
    sleep 0.1
  done
  # Keep the error: if the end-state check below fails we need it to diagnose,
  # and swallowing it is what made an earlier failure opaque. Error 5 (EIO) and
  # 125 (EDOM) both just mean "launchd's directory watcher got there first" —
  # benign, because the assertion below is what actually decides success.
  local boot_err
  boot_err="$($pre launchctl bootstrap "$domain" "$plist" 2>&1)" || true
  # Allow 15s: the watcher can take several seconds to publish the label, and
  # a 5s window produced a false "failed to load" on a job that loaded fine.
  i=0
  while ! $pre launchctl print "$domain/$label" >/dev/null 2>&1; do
    i=$((i + 1))
    if [ "$i" -ge 150 ]; then
      echo "!! $label failed to load from $plist" >&2
      [ -n "$boot_err" ] && echo "   bootstrap said: $boot_err" >&2
      return 1
    fi
    sleep 0.1
  done
  # Loaded — but launchd may have auto-loaded a STALE copy from its own scan.
  local loaded
  loaded="$($pre launchctl print "$domain/$label" 2>/dev/null | awk -F'= ' '/^\tpath = /{print $2; exit}')"
  if [ "$loaded" != "$plist" ]; then
    echo "!! $label loaded from $loaded, expected $plist" >&2
    return 1
  fi
}

if [ "${1:-}" = "--off" ]; then
  echo "==> rolling back to tailscale serve"
  sudo launchctl bootout "system/$PF_LABEL" 2>/dev/null || true
  sudo rm -f "$PF_PLIST" "$ANCHOR"
  sudo python3 - "$ANCHOR_NAME" <<'PY'
import sys, re
name = sys.argv[1]
src = open("/etc/pf.conf").read()
out = "".join(l for l in src.splitlines(keepends=True) if name not in l)
if out != src:
    open("/etc/pf.conf", "w").write(out)
    print("   /etc/pf.conf: anchor lines removed")
PY
  sudo pfctl -f /etc/pf.conf 2>/dev/null || true
  launchctl bootout "gui/$UID_N/$CERT_LABEL" 2>/dev/null || true
  rm -f "$CERT_PLIST"
  python3 -c '
import plistlib, sys
path = sys.argv[1]
with open(path, "rb") as fh:
    pl = plistlib.load(fh)
args = pl["ProgramArguments"]
for flag in ("--tls-cert", "--tls-key"):
    if flag in args:
        i = args.index(flag)
        del args[i:i + 2]
if "--listen" in args:
    args[args.index("--listen") + 1] = "127.0.0.1:8781"
with open(path, "wb") as fh:
    plistlib.dump(pl, fh)
print("   restored:", " ".join(args[1:]))
' "$PLIST"
  reload_agent "gui/$UID_N" "$LABEL" "$PLIST"
  "$TS" serve --bg 8781
  echo "==> back on tailscale serve -> 127.0.0.1:8781"
  exit 0
fi

HOST="$("$TS" status --json | python3 -c 'import sys,json;print(json.load(sys.stdin)["Self"]["DNSName"].rstrip("."))')"
LAN_IP="$(ipconfig getifaddr en0 2>/dev/null || ipconfig getifaddr en1 2>/dev/null || true)"
[ -n "$LAN_IP" ] || { echo "!! no LAN IPv4 on en0/en1" >&2; exit 1; }
LAN_CIDR="${LAN_IP%.*}.0/24"
echo "==> host=$HOST  tailnet-serve->self-TLS on 127.0.0.1:$TLS_PORT  lan=$LAN_IP ($LAN_CIDR)"

# 0. pf needs root LATER, but acquire it NOW. Failing at step 4 leaves
# deck-remote already moved to the TLS port with pf unapplied and
# `tailscale serve` still pointing at the old one — i.e. the phone is down.
# Note `sudo` cannot prompt without a TTY, so this also catches being run from
# a non-interactive shell. Do NOT run this whole script under sudo: $HOME would
# become root's, the launchd AGENTS would land in the wrong domain, and the
# cert pair would be written root-owned (deck-remote runs as you and would
# refuse to start).
if ! sudo -v; then
  echo "!! need sudo for the pf steps — run this from a real terminal (not under sudo)" >&2
  exit 1
fi

# 1. Cert. tailscale cert needs HTTPS Certificates + MagicDNS enabled.
"$REPO/scripts/tls-renew.sh"

# 2. Daily renewal agent (certReloader picks it up with no restart).
sed -e "s#/Users/CHANGEME/path/to/deck-remote#$REPO#g" \
    -e "s#/Users/CHANGEME#$HOME#g" \
    "$REPO/deploy/io.lunascens.deck-remote-cert.plist" > "$CERT_PLIST"
plutil -lint "$CERT_PLIST" >/dev/null
reload_agent "gui/$UID_N" "$CERT_LABEL" "$CERT_PLIST"
echo "==> $CERT_LABEL loaded (daily 04:17)"

# 2b. deck-remote runs as YOU, and newCertReloader is fatal on a read error, so
# an unreadable pair means the service refuses to start AFTER we have already
# booted it out — a silent outage. Fail here instead, while it is still up.
# (A pair left root-owned by an earlier sudo run is the way this happens.)
for f in "$TLS_DIR/$HOST.crt" "$TLS_DIR/$HOST.key"; do
  [ -r "$f" ] || { echo "!! $f is not readable as $(whoami): $(ls -l "$f" 2>&1)" >&2; exit 1; }
done
echo "==> cert pair readable as $(whoami)"

# 3. Point deck-remote at the TLS port + cert pair, in place.
[ -f "$PLIST" ] || { echo "!! missing $PLIST" >&2; exit 1; }
python3 - "$PLIST" "$TLS_PORT" "$TLS_DIR/$HOST.crt" "$TLS_DIR/$HOST.key" <<'PY'
import plistlib, sys
path, port, crt, key = sys.argv[1:5]
with open(path, "rb") as fh:
    pl = plistlib.load(fh)
args = pl["ProgramArguments"]
def setflag(flag, value):
    if flag in args:
        args[args.index(flag) + 1] = value
    else:
        args.extend([flag, value])
setflag("--listen", f"127.0.0.1:{port}")
setflag("--tls-cert", crt)
setflag("--tls-key", key)
with open(path, "wb") as fh:
    plistlib.dump(pl, fh)
print("   ProgramArguments:", " ".join(args[1:]))
PY
reload_agent "gui/$UID_N" "$LABEL" "$PLIST"
echo "==> $LABEL reloaded on https://127.0.0.1:$TLS_PORT"

# 4. pf: rdr :443 -> loopback TLS port, restricted to tailnet + this LAN.
sed "s#192.168.50.0/24#$LAN_CIDR#" "$REPO/deploy/pf-deckremote.anchor" \
  | sed "s#port 8443#port $TLS_PORT#g" | sudo tee "$ANCHOR" >/dev/null
sudo chmod 644 "$ANCHOR"

# rdr-anchor must precede filter anchors, so splice it after the last existing
# rdr-anchor rather than appending; `load anchor` goes at the end.
sudo python3 - "$ANCHOR_NAME" "$ANCHOR" <<'PY'
import sys
name, path = sys.argv[1], sys.argv[2]
lines = open("/etc/pf.conf").read().splitlines(keepends=True)
if any(name in l for l in lines):
    print("   /etc/pf.conf: already wired")
else:
    last_rdr = max((i for i, l in enumerate(lines) if l.startswith("rdr-anchor")), default=-1)
    lines.insert(last_rdr + 1, f'rdr-anchor "{name}"\n')
    lines.append(f'load anchor "{name}" from "{path}"\n')
    open("/etc/pf.conf", "w").writelines(lines)
    print("   /etc/pf.conf: anchor wired")
PY
sudo pfctl -n -f /etc/pf.conf     # parse-check BEFORE enabling, so a typo cannot wedge pf
sudo pfctl -E -f /etc/pf.conf 2>&1 | sed 's/^/   /'

# 5. Boot persistence (macOS ships pf disabled).
sudo cp "$REPO/deploy/$PF_LABEL.plist" "$PF_PLIST"
sudo chown root:wheel "$PF_PLIST"; sudo chmod 644 "$PF_PLIST"
reload_agent "system" "$PF_LABEL" "$PF_PLIST" sudo
echo "==> $PF_LABEL loaded (pf enabled at boot)"

# 6. tailscale serve is now redundant — pf carries the tailnet path too.
"$TS" serve --bg off 2>/dev/null || true

echo
echo "==> local check:"
curl -sk -o /dev/null -w '    https://127.0.0.1:%{http_code}\n' "https://127.0.0.1:$TLS_PORT/healthz" || true
sudo pfctl -a "$ANCHOR_NAME" -s nat 2>/dev/null | sed 's/^/    /'
cat <<EOF

==> REMAINING MANUAL STEP (router, one time):
    Add a DNS override + a DHCP reservation on your router:
        $HOST  ->  $LAN_IP
    ASUS/Merlin: LAN > DHCP Server > Custom dnsmasq config:
        address=/$HOST/$LAN_IP
    Pi-hole/AdGuard: add it as a local DNS A record and make the router hand
    that resolver out over DHCP.
    Reserve $LAN_IP for this Mac so the override cannot go stale.

==> verify from the phone, on home Wi-Fi:
    Tailscale ON  -> https://$HOST/  works (as today)
    Tailscale OFF -> https://$HOST/  works (via the LAN DNS override)
    Same origin both ways, so the existing PWA install and push subscription
    keep working. Note pf does not redirect this Mac's own traffic to itself,
    so testing from the Mac against $HOST:443 will fail — that is expected.
EOF
