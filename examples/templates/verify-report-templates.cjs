// Browser behavior checks, using an installed Playwright runtime.
// Render previews first. Local fixture interception needs no running server.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require(process.env.PLAYWRIGHT_MODULE || 'playwright');
const slugs = ['cybersecurity-report','fund-research','venture-research','binance-announcement-style','okx-announcement-style','editorial-news','financial-daily','technology-report'];
const root = path.resolve(__dirname,'../..');
const results = [];
async function serveFixtures(context) {
 await context.route('http://127.0.0.1:18083/**',route => {
  const file = path.join(root,new URL(route.request().url()).pathname);
  if (!file.startsWith(root+path.sep) || !fs.existsSync(file)) return route.fulfill({status:404,body:'Not found'});
  const type = file.endsWith('.html') ? 'text/html' : file.endsWith('.svg') ? 'image/svg+xml' : 'image/png';
  return route.fulfill({contentType:type,body:fs.readFileSync(file)});
 });
}

(async () => {
 const browser = await chromium.launch({headless:true});
 try {
  for (const slug of slugs) {
   for (const [locale,width,height] of [['en-US',1440,1000],['zh-CN',390,844],['de-DE',390,844]]) {
    const context = await browser.newContext({locale,viewport:{width,height},reducedMotion:'reduce'});
    await serveFixtures(context);
    const page = await context.newPage();
    const errors = []; page.on('pageerror',error => errors.push(error.message));
    await page.goto('http://127.0.0.1:18083/bin/template-previews/'+slug+'.html',{waitUntil:'domcontentloaded'});
    const timing = await page.evaluate(async () => {
     const content=document.querySelector('.page'), loader=document.querySelector('.loader');
     const initiallyHidden=getComputedStyle(content).visibility==='hidden';
     const animation=content.getAnimations()[0];
     const delay=animation.effect.getTiming().delay;
     const start=performance.now();
     while(getComputedStyle(content).visibility!=='visible') await new Promise(resolve=>setTimeout(resolve,5));
     return {initiallyHidden,delay,visibleAt:performance.now(),wait:performance.now()-start,loaderHidden:getComputedStyle(loader).visibility==='hidden'};
    });
    assert(timing.initiallyHidden,slug+': content appeared before the loading delay');
    assert.equal(timing.delay,500);
    assert(timing.loaderHidden);
    assert.equal(await page.locator('html').getAttribute('lang'),'en');
    assert.equal(await page.locator('#headline').textContent(),'A clearer view of the next market cycle');
    assert.equal(await page.locator('meta[property="og:title"]').getAttribute('content'),'A clearer view of the next market cycle');
    assert(await page.locator('#toc a').count() >= 4);
    assert.equal(await page.locator('#executive-summary').count(),1,'existing article fragment anchors must survive TOC construction');
    const links = await page.locator('#toc a').evaluateAll(nodes=>nodes.map(node=>node.getAttribute('href')));
    assert.equal(new Set(links).size,links.length);
    for(const link of links) assert.equal(await page.locator(link).count(),1);
    assert(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),slug+': horizontal page overflow');
    if(locale!=='de-DE') await page.screenshot({path:path.join(root,'bin/template-previews',slug+'-'+locale+'.png'),fullPage:true});
    await page.locator('#copy-link').click();
    await page.waitForFunction(()=>document.getElementById('copy-status').textContent.length>0);
    assert.equal(errors.length,0,errors.join('\n'));
    results.push({slug,locale,width,loadingDelay:timing.delay,result:'pass'});
    await context.close();
   }
   // Missing optional translations must retain English, even in a Chinese browser.
   const fallback = await browser.newContext({locale:'zh-TW',viewport:{width:390,height:844}});
   await serveFixtures(fallback);
   const page = await fallback.newPage();
   const rendered = fs.readFileSync(path.join(root,'bin/template-previews',slug+'.html'),'utf8').replace(/<template id="zh-[^"]+">[\s\S]*?<\/template>/g,'');
   await page.route('**/fallback.html',route=>route.fulfill({contentType:'text/html',body:rendered}));
   await page.goto('http://127.0.0.1:18083/fallback.html');
   await page.waitForTimeout(550);
   assert.equal(await page.locator('#headline').textContent(),'A clearer view of the next market cycle');
   assert.equal(await page.locator('#article-content').evaluate(node=>node.closest('[lang]').getAttribute('lang')),'en');
   assert.equal(await page.locator('#copy-link').textContent(),'Copy article link');
   await fallback.close();
   // CSS alone releases content; JavaScript being disabled must not trap readers.
   const noJS = await browser.newContext({javaScriptEnabled:false,viewport:{width:390,height:844}});
   await serveFixtures(noJS);
   const staticPage = await noJS.newPage();
   await staticPage.goto('http://127.0.0.1:18083/bin/template-previews/'+slug+'.html');
   await staticPage.waitForTimeout(550);
   assert(await staticPage.locator('.page').isVisible());
   assert.equal(await staticPage.locator('#headline').textContent(),'A clearer view of the next market cycle');
   await noJS.close();
  }
  fs.writeFileSync(path.join(root,'bin/template-previews/browser-checks.json'),JSON.stringify(results,null,2)+'\n');
  console.log('Passed: 24 locale/layout checks + 8 missing-translation checks + 8 no-JavaScript checks.');
 } finally { await browser.close(); }
})().catch(error => { console.error(error);process.exitCode=1; });
