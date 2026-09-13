#!/bin/zsh
# Keep the router's DNS override for the deck-remote origin alive.
#
# WHY this exists on the Mac and not the router: the ASUS ZenWiFi Pro ET12 runs
# stock ASUSWRT, which has NO persistence hook — /etc is a symlink into tmpfs,
# crond's crontab dir is tmpfs, and /sbin/rc has no /jffs/scripts or
# dnsmasq.conf.add support (the only script hook, script_usbhotplug, needs the
# USB port this model lacks). /jffs persists but nothing in it runs at boot. So
# the override is volatile by construction and something external must restore
# it. Asuswrt-Merlin would solve this properly but has no ET12 build.
#
# The override itself is one /etc/hosts line: dnsmasq is started with no
# `no-hosts`, so it answers from /etc/hosts, and SIGHUP re-reads hosts files
# (unlike dnsmasq.conf, where an `address=` line would need a full restart).
#
# Idempotent and quiet: it only touches the router when the record is actually
# missing, so the normal path is a single DNS query.
#
#   lan-dns-keeper.sh            check, and repair if needed
#   lan-dns-keeper.sh --once     same, but log even when healthy
#   lan-dns-keeper.sh --remove   drop the override from the router
set -euo pipefail

# Machine-specific values live outside the repo: launchd passes no env, so
# they come from this file (or the environment when run by hand).
ENV_FILE="${DECK_REMOTE_LANDNS_ENV:-$HOME/.config/deck-remote/landns.env}"
[ -r "$ENV_FILE" ] && source "$ENV_FILE"

ROUTER="${DECK_REMOTE_ROUTER:?set DECK_REMOTE_ROUTER (router LAN IP)}"
SSH_PORT="${DECK_REMOTE_ROUTER_SSH_PORT:-22}"
SSH_KEY="${DECK_REMOTE_ROUTER_KEY:-$HOME/.ssh/id_ed25519}"
HOST="${DECK_REMOTE_HOST:?set DECK_REMOTE_HOST (<host>.<tailnet>.ts.net)}"
TARGET="${DECK_REMOTE_LAN_IP:?set DECK_REMOTE_LAN_IP (this Mac's LAN IP)}"
MARKER="deck-remote-lan-fallback"

log() { print -r -- "$(date '+%Y-%m-%dT%H:%M:%S%z') lan-dns-keeper: $*"; }

rsh() { ssh -i "$SSH_KEY" -p "$SSH_PORT" -o BatchMode=yes -o ConnectTimeout=8 \
            -o StrictHostKeyChecking=accept-new "$ROUTER" "$@"; }

# What the router currently hands out for the name.
resolved() { dig +short +time=3 +tries=1 "@$ROUTER" "$HOST" 2>/dev/null | head -1; }

if [ "${1:-}" = "--remove" ]; then
  rsh "sed -i '/$MARKER/d' /etc/hosts && killall -HUP dnsmasq"
  log "override removed from $ROUTER"
  exit 0
fi

cur="$(resolved || true)"
if [ "$cur" = "$TARGET" ]; then
  [ "${1:-}" = "--once" ] && log "healthy ($HOST -> $cur)"
  exit 0
fi

log "override missing ($HOST -> '${cur:-NXDOMAIN}', want $TARGET) — reapplying"

# Append only if absent, then reload. Kept as one remote command so a dropped
# connection cannot leave the line in place without the HUP.
rsh "grep -q '$MARKER' /etc/hosts || echo '$TARGET $HOST # $MARKER' >> /etc/hosts; killall -HUP dnsmasq"

sleep 1
cur="$(resolved || true)"
if [ "$cur" = "$TARGET" ]; then
  log "restored ($HOST -> $cur)"
else
  log "!! reapply did NOT take: $HOST -> '${cur:-NXDOMAIN}'"
  exit 1
fi
