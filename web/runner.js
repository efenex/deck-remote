// Runner mode v1 on the Tower tab (tower reports/runbang-runner-mode/interface.md; design §10/§10.1).
// A runner is a sequence ask compiled from a runner file; run/verify gives it a `runner` block. The
// sheet shows the file (path + sha8), the revision, every step (title, class, phone/laptop) and the
// prelude size, offers the existing Arm… flow when the runner needs an arm, then one Face ID starts
// runner mode (an elevation). While elevated: a banner, the pending gate with its buttons, the
// running step's output through the existing run stream, polling every 2 s with backoff, "End
// runner mode", and a screen wake lock while the sheet is visible.
//
// The pure models (sheetModel, gateView, banner, backoff, currentRun, decisionBody, Poller) are
// node-testable (runner.test.js); mount(deps) holds the DOM and is wired from app.js. Loaded after
// runbang.js, before app.js. Never a keystroke into a pane: decisions go to POST runner/gate with
// the gate's nonce, which only GET runner/gate carries (verify, readable by run:ro, never does).
(function (root) {
  'use strict';

  const POLL_MS = 2000;
  const POLL_MAX_MS = 30000;
  // the runner's own "this elevation is over" answers: a result, unlike a dropped connection
  const OVER = new Set(['no-elevation', 'wrong-device', 'ended', 'expired', 'no-loop']);

  // refusals the Arm… flow fixes (the runner's prelude counts as a prog step under the arm)
  const ARM_FIX = (refused) => /^arm-/.test(String(refused || '')) || refused === 'runner-prelude-unarmed';
  const sha8 = (s) => String(s || '').slice(0, 8);
  const isoMin = (sec) => (typeof sec === 'number' && isFinite(sec) ? new Date(sec * 1000).toISOString().slice(11, 16) + 'Z' : '');

  // sheetModel(top, askId[, caps {prog}]) -> the runner sheet's model from one run/verify response (caps.prog false:
  // an arm-needing runner is not startable on this phone):
  // { ok, why, title, source: {path, sha8}, rev, steps: [{index, title, class, where}], preludeLines,
  //   needsArm, armState, offerArm, startable, resume, elevation }
  // offerArm: the runner needs an arm this revision lacks (the sheet offers Arm…); resume: this
  // phone's own live elevation (deck-remote restarted, or the sheet was closed): go straight to it.
  function sheetModel(top, askId, caps) {
    const bad = (why) => ({ ok: false, why, steps: [] });
    if (!top || typeof top !== 'object') return bad('no answer from the runner');
    if (top.ask !== askId) return bad('verify names another ask');
    const rn = top.runner;
    if (!rn || typeof rn !== 'object' || !rn.source) return bad('This ask is not a runner (verify has no runner block): use the per-step Run.');
    const seq = top.seq && typeof top.seq === 'object' ? top.seq : {};
    const items = Array.isArray(top.items) ? top.items : [];
    const titles = new Map((Array.isArray(seq.steps) ? seq.steps : []).map((s) => [s.index, s.title || '']));
    const laptop = new Set(Array.isArray(rn.laptop_steps) ? rn.laptop_steps : []);
    const phone = new Set(Array.isArray(rn.phone_steps) ? rn.phone_steps : []);
    const steps = items.map((it) => ({
      index: it.index, title: titles.get(it.index) || '', class: it.class || '?',
      where: laptop.has(it.index) ? 'laptop' : phone.has(it.index) ? 'phone' : '?',
      run: it.run && typeof it.run === 'object' ? it.run : null,
    }));
    const armState = (seq.arm && seq.arm.state) || 'missing';
    const el = rn.elevation && typeof rn.elevation === 'object' ? rn.elevation : { state: 'none' };
    const mine = el.state === 'elevated' && !!el.device && el.device === top.arm_device;
    const out = {
      ok: true, why: '', title: seq.title || askId,
      source: { path: rn.source.path || '', sha8: sha8(rn.source.sha256) },
      rev: seq.rev || '', steps, preludeLines: rn.prelude_lines || 0, pointer: seq.pointer,
      prelude: typeof rn.prelude === 'string' ? rn.prelude : null, cd: typeof rn.cd === 'string' ? rn.cd : null,
      needsArm: !!rn.needs_arm, armState, offerArm: false, startable: false, resume: mine, elevation: el,
    };
    if (rn.file && rn.file !== 'ok') { out.why = 'The runner file no longer matches its signed sha256 (' + rn.file + '): re-sign it on the laptop.'; return out; }
    if (seq.sig !== 'ok') { out.why = 'The sequence signature is ' + (seq.sig || 'missing') + ': re-sign it on the laptop.'; return out; }
    if (!/^[0-9a-f]{16}$/.test(out.rev)) { out.why = 'verify reports no revision for this runner'; return out; }
    if (el.state === 'elevated') {
      if (!mine) out.why = 'Runner mode is on from another device (' + (el.device || '?') + '): end it there or at the laptop.';
      return out;
    }
    if (out.needsArm && caps && caps.prog === false) {
      out.why = 'This runner has steps that need the Vault arm, and this phone lacks the run:prog capability: run it at the laptop.';
      return out;
    }
    if (out.needsArm && armState !== 'armed') {
      out.offerArm = true;
      out.why = 'This runner needs a Vault-signed arm of its revision first (arm-' + armState + ').';
      return out;
    }
    if (seq.state === 'complete' || seq.pointer === null) { out.why = 'Every step has run: nothing left for runner mode.'; return out; }
    out.startable = true;
    return out;
  }

  // gateView(g) -> null (nothing pending) | { kind, step, attempt, title, class, buttons: [{decision, label, primary}], word, wordLen, note }
  // per the interface: run → run|skip|quit; failed → retry|skip|quit; word → run (with the retyped
  // word)|skip|quit; laptop → skip|quit ("do it at the laptop, or Skip"). An unknown kind: quit only.
  function gateView(g) {
    if (!g || g.pending !== true) return null;
    const b = (decision, label, primary) => ({ decision, label, primary: !!primary });
    // body: the step's exact text, control/format characters already ⟨U+XXXX⟩ (tower); drawn with textContent only
    const v = { kind: g.kind, step: g.step, attempt: g.attempt || 1, title: g.title || '', class: g.class || '', buttons: [], word: false, wordLen: 0, note: '',
      body: typeof g.body === 'string' ? g.body : null, sha8: typeof g.sha256 === 'string' ? g.sha256.slice(0, 8) : '' };
    switch (g.kind) {
      case 'run':
        v.buttons = [b('run', 'Run', true), b('skip', 'Skip'), b('quit', 'Quit')];
        break;
      case 'failed':
        v.note = 'The last attempt failed.';
        if (typeof g.word_len === 'number') { // a failed destructive step: Retry needs the word again
          v.word = true;
          v.wordLen = g.word_len;
          v.note += ' Destructive step: retype its word to retry.';
        }
        v.buttons = [b('retry', 'Retry', true), b('skip', 'Skip'), b('quit', 'Quit')];
        break;
      case 'word':
        v.word = true;
        v.wordLen = typeof g.word_len === 'number' ? g.word_len : 0;
        v.note = 'Destructive step: retype its word to run it.';
        v.buttons = [b('run', 'Run', true), b('skip', 'Skip'), b('quit', 'Quit')];
        break;
      case 'laptop':
        v.note = 'Laptop step: do it at the laptop, or Skip.';
        v.buttons = [b('skip', 'Skip'), b('quit', 'Quit')];
        break;
      default:
        v.note = 'Unknown gate kind ' + g.kind + ': answer it at the laptop, or Quit.';
        v.buttons = [b('quit', 'Quit')];
    }
    return v;
  }

  // decisionBody(elevation, gate, decision, word) -> the POST runner/gate body, or throws with a
  // message for the sheet. Checks only what the phone can: the decision is one the gate offers, and a
  // word gate's run carries 1..64 printable characters. The runner checks the rest (nonce, word).
  function decisionBody(elevation, gate, decision, word) {
    const v = gateView(gate);
    if (!v) throw new Error('no gate is pending');
    if (!v.buttons.some((x) => x.decision === decision)) throw new Error(decision + ' is not offered at this gate');
    if (!/^[0-9a-f]{32}$/.test(String(gate.nonce || ''))) throw new Error('the gate carries no nonce: reload');
    const body = { elevation, nonce: gate.nonce, decision };
    if (v.word && (decision === 'run' || decision === 'retry')) {
      const w = String(word == null ? '' : word);
      if (!/^[\x21-\x7e]{1,64}$/.test(w)) throw new Error('retype the word (up to 64 printable characters, no spaces)');
      body.word = w;
    }
    return body;
  }

  // currentRun(gate, top) -> {step, id} | null: the step whose output to stream. The gate poll's
  // `running` wins; else a step verify reports running.
  function currentRun(gate, top) {
    if (gate && gate.running && typeof gate.running.id === 'string' && Number.isInteger(gate.running.step)) return { step: gate.running.step, id: gate.running.id };
    for (const it of (top && Array.isArray(top.items) ? top.items : [])) {
      if (it.run && it.run.state === 'running' && typeof it.run.id === 'string') return { step: it.index, id: it.run.id };
    }
    return null;
  }

  // banner(m, gate, conn) -> one line: "Runner <title> · step k/n · ends HH:MMZ (idle HH:MMZ)" + the connection state.
  function banner(m, gate, conn) {
    const n = m.steps.length;
    let k = null;
    if (gate && Number.isInteger(gate.step)) k = gate.step;
    else if (gate && gate.running && Number.isInteger(gate.running.step)) k = gate.running.step;
    else if (Number.isInteger(m.pointer)) k = m.pointer;
    const el = m.elevation || {};
    const parts = ['Runner ' + m.title];
    if (k !== null) parts.push('step ' + (k + 1) + '/' + n);
    if (el.exp) parts.push('ends ' + isoMin(el.exp) + (el.idle_exp ? ' (idle ' + isoMin(el.idle_exp) + ')' : ''));
    if (conn && conn.reconnecting) parts.push('reconnecting…');
    else if (gate && gate.pending) parts.push('waiting for you');
    return parts.join(' · ');
  }

  // cleanComplete(reason, top) -> true when the runner ended because every step ran (reason from
  // Poller: "ended (complete)") AND verify shows each step's latest run done with rc 0. A skipped,
  // failed, quit or expired runner is not clean: its sheet stays open with the reason.
  function cleanComplete(reason, top) {
    if (!/^ended \(complete\)$/.test(String(reason || ''))) return false;
    const items = top && Array.isArray(top.items) ? top.items : [];
    return items.length > 0 && items.every((it) => it && it.run && it.run.state === 'done' && it.run.rc === 0);
  }

  // backoff(fails) -> ms until the next poll: 2 s, then doubling to 30 s while errors repeat.
  function backoff(fails) {
    if (!fails) return POLL_MS;
    return Math.min(POLL_MAX_MS, POLL_MS * Math.pow(2, Math.min(fails, 5)));
  }

  // Poller: one tick = GET runner/gate + GET run/verify. deps.gate() / deps.verify() return the JSON
  // or throw (Error with .status and .data, as app.js's api() does). tick() resolves to the next
  // delay in ms, or null when the elevation is over (the runner said so). A network error or a 5xx
  // never ends anything: deck-remote may be restarting; the state says reconnecting and the delay
  // backs off. onUpdate({gate, top, conn: {reconnecting, fails, over, reason}}).
  class Poller {
    constructor(deps) { this.deps = deps; this.fails = 0; this.over = false; this.reason = ''; this.gate = null; this.top = null; }
    async tick() {
      if (this.over) return null;
      let reason = '';
      try {
        this.gate = await this.deps.gate();
        this.top = await this.deps.verify();
        this.fails = 0;
        const el = (this.top && this.top.runner && this.top.runner.elevation) || {};
        if (el.state && el.state !== 'elevated') { this.over = true; reason = el.state + (el.reason ? ' (' + el.reason + ')' : ''); }
      } catch (e) {
        const refused = e && e.status === 403 && e.data && e.data.refused;
        if (refused && OVER.has(refused)) { this.over = true; reason = refused; }
        else if (e && e.status === 404) { this.over = true; reason = 'the ask is closed'; } // verify: no longer open (a restart never answers 404)
        else this.fails++;
      }
      if (this.over) this.reason = reason;
      const conn = { reconnecting: this.fails > 0, fails: this.fails, over: this.over, reason };
      if (this.deps.onUpdate) this.deps.onUpdate({ gate: this.gate, top: this.top, conn });
      return this.over ? null : backoff(this.fails);
    }
  }

  // --- DOM ----------------------------------------------------------------------------------------
  // mount(deps) -> { open(askId) }. deps: { api, el, authHeaders, toast, openArmSheet, loadTower, R }
  // with R = DeckRunBang (requestOptions, credentialJSON, parseSSE, runState, visibleParts).
  function mount(deps) {
    const { api, el, toast } = deps;
    const R = deps.R;
    let live = null; // the one runner-mode view: {askId, elevation, poller, timer, sheet, back, bannerEl, ...}

    async function wakeOn(v) {
      if (!(typeof navigator !== 'undefined' && navigator.wakeLock && navigator.wakeLock.request)) return;
      try { v.wake = await navigator.wakeLock.request('screen'); } catch (_) { v.wake = null; /* not allowed now: fine */ }
    }
    function wakeOff(v) { try { if (v.wake) v.wake.release(); } catch (_) { /* gone */ } v.wake = null; }

    async function faceID(askId, rev) {
      if (!(window.PublicKeyCredential && navigator.credentials)) throw new Error('no passkeys here (needs the https tailnet URL)');
      const ch = await api('POST', '/api/rc/tower/runner/challenge', { ask: askId, rev });
      const cred = await navigator.credentials.get({ publicKey: R.requestOptions(ch.options) });
      if (!cred) throw new Error('Face ID cancelled');
      return R.credentialJSON(cred);
    }

    function fact(box, k, v) {
      const d = el('div', 'rbfact');
      d.setAttribute('data-rm', 'fact-' + k);
      d.append(el('span', 'rbk', k), el('span', 'rbv', v));
      box.append(d);
    }

    // preludeBlock: the prelude in full before Face ID, open the first time this phone sees this
    // revision's prelude (localStorage), collapsible after.
    function preludeBlock(askId, m) {
      const KEY = 'deckRunnerPreludeSeen';
      let seen = {};
      try { seen = JSON.parse(localStorage.getItem(KEY) || '{}') || {}; } catch (_) { seen = {}; }
      const id = askId + '@' + m.rev;
      const det = el('details', 'rmprelude');
      det.setAttribute('data-rm', 'prelude');
      det.open = !seen[id];
      det.append(el('summary', 'rbnote', 'prelude (' + m.preludeLines + ' lines, runs before every step)'));
      const pre = el('pre', 'rbline rmcode');
      pre.textContent = m.prelude;
      det.append(pre);
      try {
        seen[id] = Date.now();
        const keep = Object.keys(seen).sort((a, b) => seen[b] - seen[a]).slice(0, 50);
        localStorage.setItem(KEY, JSON.stringify(Object.fromEntries(keep.map((k) => [k, seen[k]]))));
      } catch (_) { /* a convenience */ }
      return det;
    }

    function offerArm(askId, note, btns, close) {
      if (deps.canArm && deps.canArm()) {
        const arm = el('button', 'tbtn tprimary', 'Arm…');
        arm.addEventListener('click', () => { close(); deps.openArmSheet(askId, null); });
        btns.append(arm);
      } else note.textContent += ' This phone cannot arm (needs run:prog + prof:power): arm on the laptop.';
    }

    function stepList(m) {
      const box = el('div', 'rmsteps');
      box.setAttribute('data-rm', 'steps');
      m.steps.forEach((s) => {
        const row = el('div', 'rmstep');
        row.setAttribute('data-rm', 'step-' + s.index);
        const rs = s.run ? R.runState(s.run) : null;
        row.append(el('span', 'rmidx', String(s.index + 1)), el('span', 'rmtitle', s.title || '(untitled)'),
          el('span', 'trbcls trb-' + s.class, s.class), el('span', 'rmwhere rm-' + s.where, s.where === 'laptop' ? '💻 laptop' : s.where === 'phone' ? '📱 phone' : '?'));
        if (rs) row.append(el('span', 'rmrs trb-' + rs.tone, rs.glyph));
        box.append(row);
      });
      return box;
    }

    // open(askId): the runner sheet from a fresh run/verify.
    async function open(askId) {
      if (live && live.askId === askId) { show(live); return; }
      if (live) { toast('Runner mode', 'Runner mode is on for ' + live.askId + ': end it first.'); show(live); return; }
      const back = el('div', 'rbback');
      const sh = el('div', 'rbsheet');
      sh.setAttribute('data-rm', 'sheet');
      back.append(sh);
      const close = () => back.remove();
      back.addEventListener('click', (e) => { if (e.target === back) close(); });
      sh.append(el('div', 'rbtitle', 'Runner mode'));
      const facts = el('div', 'rbfacts', 'checking with the runner…');
      const note = el('div', 'rbnote', '');
      note.setAttribute('data-rm', 'note');
      const btns = el('div', 'rbbtns');
      const cancel = el('button', 'tbtn', 'Cancel');
      cancel.addEventListener('click', close);
      btns.append(cancel);
      sh.append(facts, note, btns);
      document.body.append(back);
      let top;
      let m;
      try {
        top = await api('GET', '/api/rc/tower/run/verify?ask=' + encodeURIComponent(askId));
        m = sheetModel(top, askId, { prog: !deps.canProg || deps.canProg() });
      } catch (e) { facts.textContent = 'check failed: ' + (e.message || e); return; }
      facts.textContent = '';
      if (!m.ok) { note.textContent = m.why; return; }
      fact(facts, 'runner', m.title);
      fact(facts, 'file', m.source.path + ' · sha256 ' + m.source.sha8);
      fact(facts, 'revision', m.rev + ' · ' + m.steps.length + ' steps');
      fact(facts, 'prelude', m.preludeLines + ' line' + (m.preludeLines === 1 ? '' : 's') + ' (exports and helpers run before each step)');
      fact(facts, 'cd', m.cd === null ? '(none: the runner file\'s directory)' : m.cd);
      if (m.prelude !== null) facts.append(preludeBlock(askId, m));
      fact(facts, 'arm', m.needsArm ? (m.armState === 'armed' ? '✓ armed' : 'needs an arm (' + m.armState + ')') : 'not needed (ro/mut only)');
      sh.insertBefore(stepList(m), note);
      if (m.resume) { close(); enter(askId, m.elevation.id, m); return; }
      if (m.offerArm) {
        note.textContent = m.why;
        offerArm(askId, note, btns, close);
        return;
      }
      if (!m.startable) { note.textContent = m.why; return; }
      note.textContent = 'One Face ID starts runner mode for this revision (idle 15 min, at most 2 h). Each step still waits for your tap; laptop steps stay at the laptop.';
      const go = el('button', 'tbtn tprimary', 'Start with Face ID');
      go.setAttribute('data-rm', 'start');
      btns.append(go);
      go.addEventListener('click', async () => {
        go.disabled = true;
        go.textContent = '…';
        try {
          const assertion = await faceID(askId, m.rev);
          const res = await api('POST', '/api/rc/tower/runner/start', { ask: askId, rev: m.rev, assertion });
          close();
          m.elevation = { state: 'elevated', id: res.elevation, exp: res.exp, idle_exp: res.idle_exp };
          enter(askId, res.elevation, m);
        } catch (e) {
          const resume = e.data && e.data.elevation;
          if (resume) { close(); enter(askId, resume, m); return; }
          note.textContent = 'Refused: ' + (e.message || e) + (e.data && e.data.hint ? '\n↳ ' + e.data.hint : '');
          if (e.data && ARM_FIX(e.data.refused)) { go.remove(); offerArm(askId, note, btns, close); return; }
          go.textContent = 'Start with Face ID';
          go.disabled = false;
        }
      });
    }

    // enter: the elevated view (sheet + banner), polling until the runner says it is over.
    function enter(askId, elevation, m) {
      const v = { askId, elevation, m, gate: null, top: null, conn: {}, wake: null, streamId: '', streamCtl: null, sentFor: '' };
      live = v;
      v.back = el('div', 'rbback');
      v.sheet = el('div', 'rbsheet rmsheet');
      v.sheet.setAttribute('data-rm', 'elevated');
      v.back.append(v.sheet);
      v.back.addEventListener('click', (e) => { if (e.target === v.back) hide(v); });
      v.head = el('div', 'rmbanner', '');
      v.head.setAttribute('data-rm', 'banner');
      v.gateBox = el('div', 'rmgate');
      v.gateBox.setAttribute('data-rm', 'gate');
      v.outHead = el('div', 'rbnote', '');
      v.out = el('pre', 'rbout');
      v.out.setAttribute('data-rm', 'out');
      v.status = el('div', 'rbstatus', '');
      v.steps = el('div', '');
      const btns = el('div', 'rbbtns');
      const min = el('button', 'tbtn', 'Hide');
      min.addEventListener('click', () => hide(v));
      v.endBtn = el('button', 'tbtn', 'End runner mode');
      v.endBtn.setAttribute('data-rm', 'end');
      v.endBtn.addEventListener('click', () => end(v));
      btns.append(min, v.endBtn);
      // fixed-size sheet (CSS .rmsheet): banner on top, buttons at the bottom, only this area scrolls/changes
      v.detail = el('div', 'rmdetail');
      v.detail.setAttribute('data-rm', 'detail');
      v.detail.append(v.gateBox, v.outHead, v.out, v.status, v.steps);
      v.sheet.append(v.head, v.detail, btns);
      v.pill = el('div', 'rmpill', '');
      v.pill.setAttribute('data-rm', 'pill');
      v.pill.addEventListener('click', () => show(v));
      document.body.append(v.pill);
      v.poller = new Poller({
        gate: () => api('GET', '/api/rc/tower/runner/gate?elevation=' + encodeURIComponent(elevation)),
        verify: () => api('GET', '/api/rc/tower/run/verify?ask=' + encodeURIComponent(askId)),
        onUpdate: (u) => render(v, u),
      });
      v.onVis = () => { if (document.visibilityState === 'visible' && v.back.isConnected) wakeOn(v); };
      document.addEventListener('visibilitychange', v.onVis);
      show(v);
      render(v, { gate: null, top: null, conn: {} });
      loop(v);
    }

    async function loop(v) {
      if (live !== v) return;
      const next = await v.poller.tick();
      if (live !== v) return;
      if (next === null) { finish(v, v.poller.reason); return; }
      v.timer = setTimeout(() => loop(v), next);
    }

    function show(v) { if (!v.back.isConnected) document.body.append(v.back); v.pill.hidden = true; wakeOn(v); }
    function hide(v) { v.back.remove(); v.pill.hidden = false; wakeOff(v); }

    function render(v, u) {
      v.gate = u.gate;
      v.conn = u.conn || {};
      if (u.top) {
        v.top = u.top;
        const m2 = sheetModel(u.top, v.askId, { prog: !deps.canProg || deps.canProg() });
        if (m2.ok) { v.m = m2; v.steps.replaceChildren(stepList(m2)); }
      }
      const line = banner(v.m, v.gate, v.conn);
      v.head.textContent = line;
      v.pill.textContent = '▶ ' + line;
      v.pill.classList.toggle('rmwait', !!(v.gate && v.gate.pending));
      if (v.conn.over) { finish(v, v.conn.reason); return; }
      drawGate(v);
      const cur = currentRun(v.gate, v.top);
      if (cur && cur.id !== v.streamId) stream(v, cur);
    }

    function drawGate(v) {
      const g = gateView(v.gate);
      const key = g ? [v.gate.nonce, g.kind, g.step].join(':') : 'none';
      if (key === v.gateKey) return; // keep a half-typed word across polls
      v.gateKey = key;
      v.gateBox.replaceChildren();
      if (!g) {
        if (v.conn.reconnecting) v.gateBox.append(el('div', 'rbnote', 'reconnecting to deck-remote… the runner keeps waiting at its gate'));
        return;
      }
      v.gateBox.append(el('div', 'rbtitle', 'Step ' + (g.step + 1) + ': ' + (g.title || '(untitled)')));
      const meta = el('div', 'rbnote', 'class ' + g.class + (g.attempt > 1 ? ' · attempt ' + g.attempt : ''));
      v.gateBox.append(meta);
      if (g.note) v.gateBox.append(el('div', g.word ? 'rbwarn' : 'rbnote', g.note));
      if (g.body !== null) {
        const pre = el('pre', 'rbline rmcode');
        pre.setAttribute('data-rm', 'body');
        pre.textContent = g.body; // already escaped by tower; never HTML
        v.gateBox.append(el('div', 'rbnote', 'the exact step' + (g.sha8 ? ' · sha256 ' + g.sha8 : '')), pre);
      } else v.gateBox.append(el('div', 'rbwarn', 'the runner sent no step text: check it on the laptop before you run it'));
      let input = null;
      if (g.word) {
        input = document.createElement('input');
        input.type = 'text';
        input.maxLength = 64;
        input.autocomplete = 'off';
        input.setAttribute('autocapitalize', 'off');
        input.spellcheck = false;
        input.placeholder = 'retype the word' + (g.wordLen ? ' (' + g.wordLen + ' characters)' : '');
        input.className = 'rmword';
        input.setAttribute('data-rm', 'word');
        v.gateBox.append(input);
      }
      const row = el('div', 'rbbtns');
      const msg = el('div', 'rbnote', '');
      g.buttons.forEach((b) => {
        const btn = el('button', 'tbtn' + (b.primary ? ' tprimary' : ''), b.label);
        btn.setAttribute('data-rm', 'decide-' + b.decision);
        btn.addEventListener('click', async () => {
          if (b.decision === 'quit' && !confirm('Quit this runner here? The steps left stay open for the laptop.')) return;
          let body;
          try { body = decisionBody(v.elevation, v.gate, b.decision, input && input.value); } catch (e) { msg.textContent = e.message; return; }
          row.querySelectorAll('button').forEach((x) => { x.disabled = true; });
          try {
            await api('POST', '/api/rc/tower/runner/gate', body);
            if (input) input.value = '';
            msg.textContent = '✓ ' + b.label.toLowerCase() + ' sent';
          } catch (e) {
            msg.textContent = 'Refused: ' + (e.message || e) + (e.data && e.data.hint ? '\n↳ ' + e.data.hint : '');
            row.querySelectorAll('button').forEach((x) => { x.disabled = false; });
          }
        });
        row.append(btn);
      });
      v.gateBox.append(row, msg);
    }

    // stream: the step's output through GET run/stream. A dropped stream is not a result: the
    // next poll that still names this run id reopens it (from the top, the output is redrawn).
    async function stream(v, cur) {
      if (v.streamCtl) v.streamCtl.abort();
      const ctl = new AbortController();
      v.streamCtl = ctl;
      v.streamId = cur.id;
      v.out.textContent = '';
      v.outHead.textContent = 'step ' + (cur.step + 1) + ' output · ' + cur.id;
      v.status.textContent = '⏳ running…';
      v.status.className = 'rbstatus';
      const q = '?ask=' + encodeURIComponent(v.askId) + '&item=' + cur.step + '&id=' + encodeURIComponent(cur.id);
      let buf = '';
      let done = false;
      try {
        const res = await fetch('/api/rc/tower/run/stream' + q, { headers: deps.authHeaders(), signal: ctl.signal });
        if (!res.ok) throw new Error('HTTP ' + res.status);
        const rd = res.body.getReader();
        const dec = new TextDecoder();
        for (;;) {
          const { value, done: eof } = await rd.read();
          if (eof) break;
          const p = R.parseSSE(buf + dec.decode(value, { stream: true }));
          buf = p.rest;
          for (const ev of p.events) {
            if (ev.event === 'line') v.out.textContent += ev.data + '\n';
            else if (ev.event === 'capped') v.out.textContent += '[' + ev.data.note + ']\n';
            else if (ev.event === 'done') {
              done = true;
              const rs = R.runState(ev.data);
              v.status.textContent = rs ? rs.glyph + ' ' + rs.text : 'finished';
              v.status.className = 'rbstatus trb-' + (rs ? rs.tone : 'dim');
            }
          }
          v.out.scrollTop = v.out.scrollHeight;
        }
      } catch (e) {
        if (e.name === 'AbortError') return;
      }
      if (!done && v.streamCtl === ctl) {
        v.status.textContent = 'output stream lost: reconnecting (the step goes on)';
        v.streamId = ''; // the next poll naming this run reopens it
      }
    }

    async function end(v) {
      if (!confirm('End runner mode? The tmux window stays; the laptop can resume.')) return;
      v.endBtn.disabled = true;
      try {
        await api('POST', '/api/rc/tower/runner/end', { elevation: v.elevation });
        finish(v, 'ended (operator)');
      } catch (e) {
        v.endBtn.disabled = false;
        toast('End runner mode', 'Refused: ' + (e.message || e));
      }
    }

    function finish(v, reason) {
      if (live !== v) return;
      live = null;
      clearTimeout(v.timer);
      if (v.streamCtl) v.streamCtl.abort();
      document.removeEventListener('visibilitychange', v.onVis);
      wakeOff(v);
      v.pill.remove();
      if (cleanComplete(reason, v.top)) { // every step ran rc=0: nothing left to read or press, close the view
        v.back.remove();
        toast('Runner mode', 'complete: every step ran rc=0');
        if (deps.loadTower) deps.loadTower();
        return;
      }
      v.gateBox.replaceChildren(el('div', 'rbnote', 'Runner mode is over' + (reason ? ': ' + reason : '') + '.'));
      v.endBtn.hidden = true;
      if (!v.back.isConnected) toast('Runner mode', 'over' + (reason ? ': ' + reason : ''));
      if (deps.loadTower) deps.loadTower();
    }

    return { open, active: () => (live ? { askId: live.askId, elevation: live.elevation } : null) };
  }

  const api = { ARM_FIX, cleanComplete, sheetModel, gateView, decisionBody, currentRun, banner, backoff, Poller, mount, POLL_MS, POLL_MAX_MS };
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  else root.DeckRunner = api;
})(typeof window !== 'undefined' ? window : globalThis);
