// Run-bang phase 1 on the Tower tab (docs/design/run-bang.md §3, §9 D4). Pure helpers:
// a `! ` item's run metadata (tower_model.py "rb", which is tower_tui.run_choices) in, the
// badge / offer out; plus the SSE frame parser for the run stream. Loaded before app.js;
// the same file is required by node --test (runbang.test.js).
(function (root) {
  'use strict';

  // tower_tui.SIG_BAD, so the phone says what the laptop says.
  const SIG_BAD = {
    unsigned: '✗unsigned', expired: 'expired', tampered: '✗tampered', 'wrong-kid': '✗wrong kid', 'missing-key': '✗no key',
    'sig-v1': '✗re-sign v2', 'sig-stale-params': '✗stale cwd/timeout',
  };
  // tower scripts/runbang/sign.py HINTS ("<id>" = the ask id).
  const HINTS = {
    'sig-v1': 're-sign as v2: run_bang.py resign --ask <id>',
    'sig-stale-params': 'cwd or timeout changed after signing: re-sign (journal.py add --kind ask ... --sign, or run_bang.py resign)',
  };
  const PHASE2 = new Set(['mut', 'secret']); // runnable from the phone after their own review
  const RUN_CLASSES = new Set(['ro']); // phase 1

  function hint(reason, ask) { return HINTS[reason] ? HINTS[reason].replace('<id>', ask || '<id>') : ''; }

  // badge(rb, ask, nowSec) -> { cls, mark, tone: ok|warn|urgent, hint, offer: run|phase2|never|blocked, why }
  // offer: run = a Run button; phase2 = copy-only, "needs phase 2"; never = never phone-runnable
  // (destructive, interactive, copy); blocked = an ro line the runner would refuse now (why).
  function badge(rb, ask, nowSec) {
    if (!rb || typeof rb !== 'object') return null;
    const cls = rb.class || 'copy';
    let sig = rb.sig || 'unsigned';
    if (sig === 'ok' && typeof rb.sig_exp === 'number' && rb.sig_exp <= (nowSec || Date.now() / 1000)) sig = 'expired';
    const ok = sig === 'ok';
    const out = {
      cls, sig, mark: ok ? '✓' : (SIG_BAD[sig] || '✗' + sig),
      tone: ok ? 'ok' : (sig === 'tampered' || sig === 'sig-stale-params' ? 'urgent' : 'warn'),
      hint: hint(sig, ask), offer: 'run', why: '',
    };
    if (!RUN_CLASSES.has(cls)) {
      out.offer = PHASE2.has(cls) ? 'phase2' : 'never';
      out.why = PHASE2.has(cls) ? 'class ' + cls + ': copy-only here, needs phase 2' : 'class ' + cls + ': copy-only, never run from the phone';
    } else if (!ok || !rb.ok) {
      out.offer = 'blocked';
      out.why = rb.why || ('signature: ' + sig);
      if (!out.hint) out.hint = hint(String(rb.why || '').replace(/^signature: /, ''), ask);
    }
    return out;
  }

  // runState(run) -> null | { glyph, tone, text } for the item's latest run (feed / done event).
  function runState(run) {
    if (!run || typeof run !== 'object') return null;
    if (run.state === 'running') return { glyph: '⏳', tone: 'warn', text: 'running (attempt ' + (run.attempt || 1) + ')' };
    if (run.state === 'refused') return { glyph: '⊘', tone: 'dim', text: 'refused: ' + (run.reason || '') };
    if (run.state !== 'done') return null;
    if (run.rc === 0) return { glyph: '✓', tone: 'ok', text: 'ran, rc=0' };
    return { glyph: '✗', tone: 'urgent', text: 'rc=' + run.rc + (run.stale ? ' (runner lost: stale)' : '') };
  }

  // parseSSE(buf) -> { events: [{event, data}], rest }: complete frames out, the partial tail kept.
  function parseSSE(buf) {
    const events = [];
    let rest = buf;
    for (;;) {
      const i = rest.indexOf('\n\n');
      if (i < 0) break;
      const frame = rest.slice(0, i);
      rest = rest.slice(i + 2);
      let event = 'message';
      const data = [];
      frame.split('\n').forEach((ln) => {
        if (ln.startsWith('event: ')) event = ln.slice(7);
        else if (ln.startsWith('data: ')) data.push(ln.slice(6));
      });
      let d = data.join('\n');
      try { d = JSON.parse(d); } catch (_) { /* keep the raw string */ }
      events.push({ event, data: d });
    }
    return { events, rest };
  }

  const api = { badge, runState, parseSSE, hint, SIG_BAD, HINTS };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.DeckRunBang = api;
})(typeof window !== 'undefined' ? window : globalThis);
