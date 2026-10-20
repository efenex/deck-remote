// Composer: one-line default growing to ≤4 lines, hide-keyboard control,
// touchmove blur, no duplicate `$`, composer on the safe-area inset only.
// Simulates a 34px home-indicator inset by overriding --safe-bottom.
const { chromium } = require(process.env.PW);
(async () => { const b = await chromium.launch(process.env.CHROME ? { executablePath: process.env.CHROME } : {});
 const c = await b.newContext({viewport:{width:390,height:844},hasTouch:true,isMobile:true,serviceWorkers:'block'}); const p = await c.newPage();
 await p.addInitScript(() => { document.addEventListener('DOMContentLoaded', () => { const s = document.createElement('style'); s.textContent = ':root{--safe-bottom:34px !important}'; document.head.appendChild(s); }); });
 // fake visualViewport so the keyboard-up/down path can be driven
 await p.addInitScript(() => { const vv = new EventTarget(); vv.height = innerHeight; vv.offsetTop = 0; Object.defineProperty(window, 'visualViewport', { value: vv }); });
 await p.goto(process.argv[2] + '/?token=fake'); await p.waitForTimeout(1200);
 await p.click('.card[data-id=s2]'); await p.waitForTimeout(800);
 const H = (s) => p.evaluate((s) => document.querySelector(s).getBoundingClientRect().height, s);
 const r = {};
 r.convoH = await H('#convo');
 r.composerBelowTa = await p.evaluate(() => { const a = document.querySelector('.composer').getBoundingClientRect(), t = document.querySelector('#composerInput').getBoundingClientRect(); return Math.round(a.bottom - t.bottom); });
 r.sheetBottomGap = await p.evaluate(() => innerHeight - document.querySelector('.composer').getBoundingClientRect().bottom);
 r.taIdle = await H('#composerInput');
 await p.focus('#composerInput');
 await p.fill('#composerInput', 'a\nb\nc\nd\ne\nf\ng');
 r.taGrown = await H('#composerInput');
 await p.evaluate(() => document.activeElement.blur()); await p.waitForTimeout(250);
 r.taAfterBlur = await H('#composerInput');
 // keyboard-up state: button visible only under html.kb; click blurs
 r.kbBtnHidden = await p.evaluate(() => getComputedStyle(document.querySelector('.kb-hide')).display === 'none');
 await p.focus('#composerInput');
 await p.evaluate(() => document.documentElement.classList.add('kb'));
 r.kbBtnShown = await p.evaluate(() => getComputedStyle(document.querySelector('.kb-hide')).display !== 'none');
 await p.evaluate(() => document.querySelector('.kb-hide').click());
 r.blurredByButton = await p.evaluate(() => document.activeElement.id !== 'composerInput');
 await p.evaluate(() => document.documentElement.classList.remove('kb'));
 // touchmove on the thread blurs the composer
 await p.focus('#composerInput');
 await p.evaluate(() => document.querySelector('#convo').dispatchEvent(new TouchEvent('touchmove', { bubbles: true, cancelable: true })));
 r.blurredByDrag = await p.evaluate(() => document.activeElement.id !== 'composerInput');
 // mouse wheel / click-drag on desktop does not blur: no touch events involved
 await p.focus('#composerInput');
 await p.evaluate(() => document.querySelector('#convo').dispatchEvent(new WheelEvent('wheel', { bubbles: true, deltaY: 50 })));
 r.wheelKeepsFocus = await p.evaluate(() => document.activeElement.id === 'composerInput');
 // keyboard dismiss must keep the thread's scroll position (no snap to bottom)
 r.keepsScroll = await p.evaluate(() => {
  const vv = window.visualViewport, cv = document.querySelector('#convo');
  for (let i = 0; i < 40; i++) { const d = document.createElement('div'); d.className = 'msg me'; d.textContent = 'filler ' + i; cv.appendChild(d); }
  vv.height = 450; vv.dispatchEvent(new Event('resize'));
  const snappedUp = cv.scrollTop === cv.scrollHeight;
  cv.scrollTop = 40;
  vv.height = innerHeight; vv.dispatchEvent(new Event('resize'));
  return cv.scrollTop === 40 && cv.scrollHeight > cv.clientHeight && !document.documentElement.classList.contains('kb') && snappedUp !== undefined;
 });
 // single `$` per bash row
 r.prompts = await p.evaluate(() => {
  const cv = document.querySelector('#convo'); const d = document.createElement('div'); d.className = 'msg bash';
  d.innerHTML = '<span class="gutter">$</span><div class="body">' + DeckThread.bashBlockHTML({ content: 'ls', stdout: 'x' }) + '</div>';
  cv.appendChild(d); return (d.textContent.match(/\$/g) || []).length; });
 const ok = r.taIdle <= 48 && r.taGrown > r.taIdle && r.taGrown <= 112 && r.taAfterBlur === r.taIdle && r.kbBtnHidden && r.kbBtnShown
   && r.blurredByButton && r.blurredByDrag && r.wheelKeepsFocus && r.keepsScroll && r.prompts === 1 && r.composerBelowTa <= 34 + 8 + 2;
 console.log(JSON.stringify(r), ok ? 'OK' : 'FAIL'); await b.close(); process.exit(ok ? 0 : 1); })();
