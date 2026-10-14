const { chromium } = require(process.env.PW);
(async () => { const b = await chromium.launch(); const c = await b.newContext({viewport:{width:390,height:844},serviceWorkers:'block'}); const p = await c.newPage();
 await p.goto(process.argv[2] + '/?token=fake'); await p.waitForTimeout(1200);
 const n = () => p.evaluate(() => document.querySelectorAll('#convo .msg').length);
 await p.click('.card[data-id=s2]'); await p.waitForTimeout(2000); console.log('open1', await n());
 await p.click('#sheetBack'); await p.waitForTimeout(300);
 await p.click('.card[data-id=s2]'); await p.waitForTimeout(2000); console.log('open2', await n());
 await p.click('#sheetBack'); await p.click('.card[data-id=s2]'); await p.waitForTimeout(2000); console.log('open3', await n());
 await b.close(); })();
