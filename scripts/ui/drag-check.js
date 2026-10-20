// Thread must not pan sideways, even with wide bash/code content injected.
const { chromium } = require(process.env.PW);
(async () => { const b = await chromium.launch(process.env.CHROME ? { executablePath: process.env.CHROME } : {});
 const c = await b.newContext({viewport:{width:390,height:844},hasTouch:true,isMobile:true,serviceWorkers:'block'}); const p = await c.newPage();
 await p.goto(process.argv[2] + '/?token=fake'); await p.waitForTimeout(1200);
 await p.click('.card[data-id=s2]'); await p.waitForTimeout(800);
 const r = await p.evaluate(() => {
  const cv = document.querySelector('#convo'), wide = 'x'.repeat(400);
  const d = document.createElement('div'); d.className = 'msg bash';
  d.innerHTML = '<div class="gutter">$</div><div class="body"><div class="bash-cmd">ls</div><pre class="bash-out">' + wide + '</pre></div>';
  cv.appendChild(d);
  cv.scrollLeft = 50; // programmatic pan must be a no-op
  return { overflowX: getComputedStyle(cv).overflowX, scrollLeft: cv.scrollLeft, preScrolls: d.querySelector('pre').scrollWidth > d.querySelector('pre').clientWidth,
    msgW: [...cv.querySelectorAll('.msg')].every(m => m.getBoundingClientRect().width <= cv.clientWidth) };
 });
 const ok = r.scrollLeft === 0 && r.preScrolls && r.msgW;
 console.log(JSON.stringify(r), ok ? 'OK' : 'FAIL'); await b.close(); process.exit(ok ? 0 : 1); })();
