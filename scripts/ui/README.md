# PWA checks without the server

`mock-api.py` serves `web/` with a faked `/api/rc/*` (static sample sessions,
history, an approval, inbox); no agent-deck, no secrets. The `.js` scripts drive
it with Playwright at 390×844 (`PW` must point at a Playwright install, e.g.
`export PW=$(ls -d ~/.npm/_npx/*/node_modules/playwright | head -1)`).

```sh
python3 -I scripts/ui/mock-api.py web 8790 &
node scripts/ui/smoke.js http://127.0.0.1:8790          # click through, report JS errors
node scripts/ui/reopen-check.js http://127.0.0.1:8790   # reopen a thread 3x: row count must not grow
node scripts/ui/swipe-check.js http://127.0.0.1:8790    # swipe-down / edge-swipe dismiss
node scripts/ui/shoot.js /tmp/shots http://127.0.0.1:8790  # screenshots + tap-target/font metrics
```

Unit tests for the pure helpers: `node --test web/`.
