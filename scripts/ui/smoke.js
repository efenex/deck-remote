const { chromium } = require(process.env.PW);
(async () => { const b = await chromium.launch(); const c = await b.newContext({viewport:{width:390,height:844},serviceWorkers:'block'}); const p = await c.newPage();
 const errs=[]; p.on('pageerror', e => errs.push(e.message)); p.on('console', m => { if (m.type()==='error') errs.push('console: '+m.text()); });
 await p.goto(process.argv[2] + '/?token=fake'); await p.waitForTimeout(1200);
 for (const id of ['s1','s2','s3','s4','s6']) { await p.click('.card[data-id='+id+']').catch(()=>{}); await p.waitForTimeout(900); await p.click('#sheetBack').catch(()=>{}); await p.waitForTimeout(200); }
 for (const t of ['inbox','settings','deck']) { await p.click('.tab[data-tab='+t+']'); await p.waitForTimeout(400); }
 console.log('errors:', JSON.stringify(errs)); await b.close(); })();
