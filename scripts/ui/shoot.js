const { chromium } = require(process.env.PW);
const OUT = process.argv[2], BASE = process.argv[3];
(async () => {
  const browser = await chromium.launch();
  const mk = async (h) => browser.newContext({ viewport: { width: 390, height: h || 844 }, deviceScaleFactor: 2, isMobile: true, hasTouch: true, serviceWorkers: 'block', colorScheme: 'dark' });
  const ctx = await mk();
  const page = await ctx.newPage();
  const metrics = {};
  const probe = async (name) => {
    metrics[name] = await page.evaluate(() => {
      const sel = 'button,a,select,input,textarea,.card,.group-head,.tab,.toggle,.set-row,.notif-open,.grabber,summary';
      const small = [];
      document.querySelectorAll(sel).forEach((e) => {
        const r = e.getBoundingClientRect(); if (!r.width || !r.height) return;
        const st = getComputedStyle(e); if (st.visibility === 'hidden') return;
        if (r.width < 44 || r.height < 44) small.push(`${e.tagName.toLowerCase()}.${(e.className||'').toString().split(' ').join('.')} ${e.id?('#'+e.id):''} ${Math.round(r.width)}x${Math.round(r.height)} "${(e.textContent||e.placeholder||'').trim().slice(0,24)}"`);
      });
      const smallFont = [];
      document.querySelectorAll('input,select,textarea').forEach((e) => { const r=e.getBoundingClientRect(); if(!r.width) return; const fs=parseFloat(getComputedStyle(e).fontSize); if (fs < 16) smallFont.push(`${e.tagName.toLowerCase()}.${e.className} ${fs}px`); });
      const tiny = {}; document.querySelectorAll('body *').forEach((e)=>{ if(!e.childNodes.length) return; const hasText=[...e.childNodes].some(n=>n.nodeType===3&&n.textContent.trim()); if(!hasText) return; const r=e.getBoundingClientRect(); if(!r.width) return; const fs=parseFloat(getComputedStyle(e).fontSize); if(fs<12){const k=(e.className||e.tagName).toString().split(' ')[0]+' '+fs+'px'; tiny[k]=(tiny[k]||0)+1;} });
      return { docOverflowX: document.documentElement.scrollWidth > innerWidth, small: [...new Set(small)], smallFont, tinyText: tiny };
    });
  };
  const shot = (n) => page.screenshot({ path: `${OUT}/${n}.png` });

  await page.goto(BASE + '/?token=fake'); await page.waitForTimeout(1200);
  await shot('01-deck'); await probe('deck');
  await page.evaluate(() => { const s = document.querySelector('#screenDeck'); s.scrollTop = s.scrollHeight; }); await page.waitForTimeout(200);
  await shot('02-deck-scrolled');

  await page.goto(BASE + '/'); await page.waitForTimeout(1000); await page.click('.card[data-id=s1]'); await page.waitForTimeout(1500);
  await shot('03-sheet-approval-default-scroll'); await probe('sheetApproval');
  await page.evaluate(() => { document.querySelector('#convo').scrollTop = 0; }); await page.waitForTimeout(300);
  await shot('04-sheet-approval-scrolled-to-top');
  const scrolls = await page.evaluate(() => { const c=document.querySelector('#convo'); const a=document.querySelector('#approveMount'); return {convoH:c.clientHeight, scrollH:c.scrollHeight, approveTop:a.offsetTop, approveH:a.offsetHeight}; });
  metrics.approvalPlacement = scrolls;

  await page.goto(BASE + '/'); await page.waitForTimeout(1000); await page.click('.card[data-id=s2]'); await page.waitForTimeout(1500);
  await shot('05-sheet-codex-working'); await probe('sheetCodex');
  metrics.chromeHeights = await page.evaluate(() => { const g=(q)=>{const e=document.querySelector(q); return e && !e.hidden ? Math.round(e.getBoundingClientRect().height) : 0}; return {handle:g('#sheetHandle'), convo:g('#convo'), banner:g('#workingBanner'), live:g('#liveActivity'), composerWrap:g('.composer-wrap')}; });
  await page.goto(BASE + '/'); await page.waitForTimeout(1000); await page.click('.card[data-id=s3]'); await page.waitForTimeout(1500);
  await shot('06-sheet-stalled');

  await page.goto(BASE + '/'); await page.waitForTimeout(1000);
  await page.click('.tab[data-tab=inbox]'); await page.waitForTimeout(800);
  await shot('07-inbox'); await probe('inbox');
  await page.click('.tab[data-tab=settings]'); await page.waitForTimeout(600);
  await shot('08-settings'); await probe('settings');

  // keyboard approximation: iOS keeps the layout viewport; emulate the visible area shrinking to ~500px
  const ctx2 = await mk(844); const p2 = await ctx2.newPage();
  await p2.goto(BASE + '/?token=fake'); await p2.waitForTimeout(1000); await p2.click('.card[data-id=s2]'); await p2.waitForTimeout(1500);
  await p2.setViewportSize({ width: 390, height: 520 }); await p2.focus('.composer textarea'); await p2.keyboard.type('/co'); await p2.waitForTimeout(400);
  await p2.screenshot({ path: `${OUT}/09-sheet-keyboard-approx-520h.png` });

  // light-mode preference check
  const ctx3 = await browser.newContext({ viewport: { width: 390, height: 844 }, deviceScaleFactor: 2, isMobile: true, colorScheme: 'light', serviceWorkers: 'block' });
  const p3 = await ctx3.newPage(); await p3.goto(BASE + '/?token=fake'); await p3.waitForTimeout(1000);
  metrics.lightModeBg = await p3.evaluate(() => getComputedStyle(document.body).backgroundColor);
  require('fs').writeFileSync(`${OUT}/metrics.json`, JSON.stringify(metrics, null, 2));
  await browser.close();
})().catch((e) => { console.error(e); process.exit(1); });
