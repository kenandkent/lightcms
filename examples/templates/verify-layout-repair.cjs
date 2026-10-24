// Real-browser regression for the nine templates, including served response
// source (not just the repaired DOM) and long-title/table stress cases.
const fs=require('node:fs'),path=require('node:path'),assert=require('node:assert/strict');
const {chromium}=require(process.env.PLAYWRIGHT_MODULE||'playwright');
const root=path.resolve(__dirname,'../..');
const base=process.env.TEMPLATE_BASE_URL||'http://127.0.0.1:50491';
const inventory=JSON.parse(fs.readFileSync(process.env.TEMPLATE_SNAPSHOT||path.join(root,'bin/template-repair-20261009/local.before.json'),'utf8'));
const destination=path.resolve(process.env.TEMPLATE_SCREENSHOT_DIR||path.join(root,'bin/template-repair-20261009/local-after'));
function contrast(a,b){function lum(color){const values=color.match(/[\d.]+/g).slice(0,3).map(Number).map(c=>{c/=255;return c<=.04045?c/12.92:((c+.055)/1.055)**2.4;});return .2126*values[0]+.7152*values[1]+.0722*values[2];}const x=lum(a),y=lum(b);return(Math.max(x,y)+.05)/(Math.min(x,y)+.05);}
(async()=>{fs.mkdirSync(destination,{recursive:true});const browser=await chromium.launch({headless:true});const results=[];try{
 for(const template of inventory.templates){
  const article=inventory.content.find(c=>c.template_id===template.id&&c.published&&!c.deleted&&!c.fork_id);assert(article,'Missing published page for '+template.slug);
  const rawResponse=await fetch(base+article.full_path,{signal:AbortSignal.timeout(15000)});const raw=await rawResponse.text();assert.equal(rawResponse.status,200);assert.equal((raw.match(/<!doctype/gi)||[]).length,1,template.slug+': nested document / theme wrapper');assert(!raw.includes('/static/css/main.css'),template.slug+': foreign theme CSS');
  if(template.slug==='crypto-analysis'&&/^#{1,6}\s/m.test(article.data.body)&&!/<[a-z!/][^>]*>/i.test(article.data.body))assert(/<h[1-6][\s>]/.test(raw),'Crypto Markdown body was not rendered');
  for(const [width,height]of [[1440,1100],[768,1024],[390,844],[320,844]]){
   const context=await browser.newContext({viewport:{width,height},locale:'en-US',reducedMotion:'reduce'});
   await context.route(base+'/**',async route=>{const response=await fetch(route.request().url());await route.fulfill({status:response.status,contentType:response.headers.get('content-type')||'text/html',body:Buffer.from(await response.arrayBuffer())});});
   const page=await context.newPage();const errors=[];page.on('pageerror',e=>errors.push(e.message));await page.goto(base+article.full_path,{waitUntil:'domcontentloaded'});await page.waitForTimeout(600);
   const geometry=await page.evaluate(()=>{const header=document.querySelector('.hero,.article-header'),title=document.getElementById('headline'),meta=document.querySelector('.meta,.article-meta'),story=document.querySelector('.story'),aside=document.querySelector('.sidebar,.side-note');const rect=e=>{const r=e.getBoundingClientRect();return{x:r.x,y:r.y,right:r.right,bottom:r.bottom,width:r.width,height:r.height};};return{width:innerWidth,scrollWidth:document.documentElement.scrollWidth,header:rect(header),title:rect(title),meta:rect(meta),story:rect(story),aside:rect(aside),titleColor:getComputedStyle(title).color,background:getComputedStyle(document.body).backgroundColor,overlay:getComputedStyle(header,'::after').content,asidePadding:parseFloat(getComputedStyle(aside).paddingLeft),articleHeadingSizes:Array.from(story.querySelectorAll('h1,h2,h3')).map(e=>parseFloat(getComputedStyle(e).fontSize)),bodyText:document.body.innerText};});
   assert(geometry.scrollWidth<=width,template.slug+': horizontal overflow at '+width);assert(geometry.story.x>=23,template.slug+': content touches left edge');assert(geometry.story.right<=width-23,template.slug+': content touches right edge');assert(geometry.meta.bottom<=geometry.story.y-20,template.slug+': meta/body overlap');assert(['none','normal','""'].includes(geometry.overlay),template.slug+': hero overlay');assert(geometry.asidePadding>=19,template.slug+': sidebar lacks padding');assert(geometry.articleHeadingSizes.every(size=>size<=32),template.slug+': oversized body headings');assert(contrast(geometry.titleColor,geometry.background)>=4.5,template.slug+': poor headline contrast');assert(!geometry.bodyText.includes('\ufffd'),template.slug+': replacement-character glyph');assert.equal(errors.length,0,errors.join(';'));
   if(width===1440||width===390){await page.screenshot({path:path.join(destination,template.slug+'-'+width+'.png'),fullPage:true});await page.screenshot({path:path.join(destination,template.slug+'-'+width+'-viewport.png')});}
   await page.emulateMedia({media:'print'});
   const printColors=await page.locator('.story h1,.story h2,.story strong').evaluateAll(nodes=>nodes.map(e=>getComputedStyle(e).color));
   assert(printColors.every(color=>contrast(color,'rgb(255, 255, 255)')>=4.5),template.slug+': low-contrast printed headings');
   await page.emulateMedia({media:'screen'});
   // Long authored body title, table and code stay contained on mobile.
   await page.locator('.story').evaluate(story=>{story.insertAdjacentHTML('afterbegin','<h1>A very long authored body title should remain a section heading instead of a second oversized hero</h1><table><tr>'+Array.from({length:6},(_,i)=>'<th>Column '+i+'</th>').join('')+'</tr><tr>'+Array.from({length:6},()=>'<td>1234567890</td>').join('')+'</tr></table><pre><code>'+('long_unbroken_token_'.repeat(30))+'</code></pre>');});
   assert(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth),'Stress content escaped the viewport');
   results.push({slug:template.slug,width,status:'passed'});await context.close();
  }
 }
 fs.writeFileSync(path.join(destination,'results.json'),JSON.stringify(results,null,2)+'\n');console.log('Passed 36 actual-page layout cases (9 templates x 4 widths), including content stress checks.');
}finally{await browser.close();}})().catch(e=>{console.error(e.message);process.exitCode=1});
