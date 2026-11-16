# Browser e2e lanes

These specs use `node:test` and the pinned `playwright-core`, not the Playwright
Test runner. The npm scripts define the browser projects; no second test runner
or Playwright Test config is needed. Run from the repository root:

```sh
make e2e                    # full suite, system Google Chrome
make e2e-webkit-deps        # npm ci if needed; download pinned WebKit explicitly
make e2e-webkit             # eight mock-fixture specs, WebKit
```

WebKit is installed in `e2e/node_modules/playwright-core/.local-browsers`;
`npm ci` removes it, so rerun `make e2e-webkit-deps` after reinstalling dependencies.
The default Chrome lane still needs no browser download. Both lanes run files
serially (`--test-concurrency=1`) because parallel browsers can starve each other.
For a slow machine, use `E2E_TIMEOUT_MS=60000 make e2e-webkit`; individual test
deadlines still apply.

From `e2e/`, `npm run test:webkit` selects WebKit, `npm run test:chromium` runs the
full Chrome suite, and `npm run test:fixtures` runs just mock fixtures on Chrome
by default. To reproduce a single WebKit spec:

```sh
cd e2e
PLAYWRIGHT_BROWSERS_PATH=0 E2E_BROWSER=webkit node --test --test-concurrency=1 composer.e2e.mjs
```

The WebKit lane includes `offline`, `recovery`, `photos`, `composer`,
`tower-navigation`, `tower-draft`, `redeliver`, and `subagents`. These launch the
loopback Python UI fixture and route stubs, using synthetic tokens and sessions.
Assertions and failures are shared between engines; there are no WebKit skips.
`pair` and `runbang` remain Chrome-only: they build disposable Go servers and use
CDP's virtual WebAuthn authenticator (pairing also uses Chrome camera flags).
They are deliberately outside the WebKit fixture lane, not reported as passes.

Desktop WebKit with mobile viewport/touch emulation does not certify Safari on
an iPhone. Service workers are blocked or stubbed in these specs; keyboard
geometry and lifecycle signals are synthetic. Native photo pickers, camera QR
scanning, Face ID, PWA installation and push delivery need the
[real-iPhone acceptance checklist](../docs/iphone-acceptance.md).
See [Playwright's browser documentation](https://playwright.dev/docs/browsers)
for the bundled WebKit runtime and browser installation behavior.
