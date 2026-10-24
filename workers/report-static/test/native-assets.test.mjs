// Runs against Cloudflare's real local assets router/workerd, not a mocked
// ASSETS.fetch. Optional toolchain lives only under the project's ignored
// runtime directory; neither Wrangler nor Node is a CMS runtime dependency.
import test from 'node:test';
import assert from 'node:assert/strict';
import {createRequire} from 'node:module';
import {readFile,writeFile,mkdir,mkdtemp,rm} from 'node:fs/promises';
import path from 'node:path';

const toolchain=process.env.STATIC_DELIVERY_MINIFLARE_ROOT;
const fixture=process.env.STATIC_DELIVERY_NATIVE_FIXTURE;
test('native assets serve actual release paths without invoking user Worker',async()=>{
 assert.ok(toolchain&&fixture,'set STATIC_DELIVERY_MINIFLARE_ROOT and STATIC_DELIVERY_NATIVE_FIXTURE for the native gate');
 const require=createRequire(path.join(path.resolve(toolchain),'package.json'));
 const {Miniflare,convertV4MiniflareOptions}=require('miniflare');
 const input=JSON.parse(await readFile(fixture,'utf8'));
 const root=await mkdtemp(path.join(path.dirname(fixture),'native-assets-'));
 let mf;
 try{
  for(const file of input.Files){const dest=path.join(root,'public',file.PublicPath.slice(1));await mkdir(path.dirname(dest),{recursive:true});await writeFile(dest,Buffer.from(file.Data,'base64'))}
 const assetsOnly=input.Modules.some(m=>m.Name==='delivery-assets-only.json');
 const config=JSON.parse(Buffer.from(input.Modules.find(m=>m.Name===(assetsOnly?'delivery-assets-only.json':'delivery-assets-config.json')).Data,'base64'));
  // Miniflare v5 reads these files itself; its v4 compatibility converter
  // intentionally does not forward pre-parsed header/redirect tables.
  await writeFile(path.join(root,'public','_headers'),config._headers);
  await writeFile(path.join(root,'public','_redirects'),config._redirects);
  const headers={version:2,rules:{}};let pattern;
  for(const line of config._headers.split('\n')){if(!line)continue;if(!line.startsWith(' ')){pattern=line;headers.rules[pattern]={set:{}}}else{const idx=line.indexOf(':');headers.rules[pattern].set[line.slice(0,idx).trim()]=line.slice(idx+1).trim()}}
  const redirects={version:1,staticRules:{},rules:{}};
  for(const line of config._redirects.trim().split('\n').filter(Boolean)){const[from,to,status]=line.split(' ');redirects.staticRules[from]={to,status:Number(status),lineNumber:1}}
  const options={name:'reports',modules:true,script:`export default {fetch(){return new Response('USER_WORKER_INVOKED',{headers:{'X-Probe-Worker':'yes'}})}}`,compatibilityDate:'2026-10-19',cf:false,assets:{directory:path.join(root,'public'),binding:'ASSETS',run_worker_first:config.run_worker_first,routerConfig:{has_user_worker:!assetsOnly},assetConfig:{html_handling:config.html_handling,not_found_handling:config.not_found_handling,headers,redirects}}};
  mf=new Miniflare(convertV4MiniflareOptions?{...convertV4MiniflareOptions(options),resourcePersistencePath:path.join(root,'state'),resourceTmpPath:path.join(root,'tmp'),unsafeDevRegistryPath:path.join(root,'registry.json'),telemetry:{enabled:false}}:options);
  for(const url of ['/','/news/a','/news/a?utm=test','/news/a.html','/news/b/index','/%E4%B8%AD%E6%96%87']){
   const response=await mf.dispatchFetch('http://localhost'+url,{redirect:'manual'});
   assert.equal(response.status,200,url);assert.equal(response.headers.get('x-probe-worker'),null,url);
   const canonical=decodeURIComponent(new URL(url,'http://localhost').pathname);
   const expectedPath=canonical==='/'?'/.__lc_delivery/root.html':canonical;
   const expected=input.Files.find(f=>f.PublicPath===expectedPath);
   assert.ok(expected,url);assert.equal(await response.text(),Buffer.from(expected.Data,'base64').toString(),url);
   assert.ok(response.headers.get('x-lc-release'),url);assert.equal(response.headers.get('x-robots-tag'),'noindex, nofollow',url);
  }
  const health=await mf.dispatchFetch('http://localhost/.__lc_delivery/health');assert.equal(health.headers.get('x-probe-worker'),assetsOnly?null:'yes');
  const image=input.Files.find(f=>f.PublicPath.startsWith('/_lc-assets/'));
  assert.ok(image,'fixture must exercise a hashed resource');
  const asset=await mf.dispatchFetch('http://localhost'+image.PublicPath);
  assert.equal(asset.headers.get('x-probe-worker'),null);
  assert.equal(asset.headers.get('content-type'),'image/png');
  assert.equal(asset.headers.get('cache-control'),'public,max-age=31536000,immutable');
  assert.deepEqual(Buffer.from(await asset.arrayBuffer()),Buffer.from(image.Data,'base64'));
  const head=await mf.dispatchFetch('http://localhost/news/a',{method:'HEAD'});assert.equal(head.status,200);assert.equal(await head.text(),'');assert.ok(head.headers.get('etag'));
  const hit=await mf.dispatchFetch('http://localhost/news/a',{headers:{'If-None-Match':head.headers.get('etag')}});assert.equal(hit.status,304);assert.equal(await hit.text(),'');
  const missing=await mf.dispatchFetch('http://localhost/missing');assert.equal(missing.headers.get('x-probe-worker'),assetsOnly?null:'yes');
  if(assetsOnly){
   assert.equal(missing.status,404);
   const identity=await health.json();assert.ok(identity.release_id);
   for(const alias of ['/news/a/','/news/b/index.html','/%E4%B8%AD%E6%96%87.html']){
    const response=await mf.dispatchFetch('http://localhost'+alias+'?utm=test',{redirect:'manual'});
    assert.equal(response.status,308,alias);assert.equal(response.headers.get('x-probe-worker'),null);
    const dest=new URL(response.headers.get('location'),'http://localhost');assert.equal(dest.search,'?utm=test');
    assert.equal((await mf.dispatchFetch(dest)).status,200);
   }
   return;
  }
  // Second phase uses the actual embedded fallback handler and route table.
  await mf.dispose();mf=null;
  const routes=JSON.parse(Buffer.from(input.Modules.find(m=>m.Name==='routes.json').Data,'base64'));
  const handler=Buffer.from(input.Modules.find(m=>m.Name==='handler.mjs').Data,'base64').toString();
  const actual={...options,script:handler.replace('export function createHandler','function createHandler')+'\nexport default createHandler('+JSON.stringify(routes)+');'};
  mf=new Miniflare(convertV4MiniflareOptions?{...convertV4MiniflareOptions(actual),resourcePersistencePath:path.join(root,'state2'),resourceTmpPath:path.join(root,'tmp2'),unsafeDevRegistryPath:path.join(root,'registry2.json'),telemetry:{enabled:false}}:actual);
  const identity=await mf.dispatchFetch('http://localhost/.__lc_delivery/health');
  assert.deepEqual(await identity.json(),{protocol:1,installation_id:routes.installation_id,target_id:routes.target_id,release_id:routes.release_id});
  for(const p of ['/missing','/cm','/api/content','/mcp','/.__lc_delivery/missing'])assert.equal((await mf.dispatchFetch('http://localhost'+p)).status,404,p);
  const alias=await mf.dispatchFetch('http://localhost/news/a/?utm=test',{redirect:'manual'});assert.equal(alias.status,308);assert.equal(alias.headers.get('location'),'/news/a?utm=test');
  const explicit=await mf.dispatchFetch('http://localhost/news/a.html',{redirect:'manual'});assert.equal(explicit.status,200,'real .html article must not redirect to a different article');
 }finally{if(mf)await mf.dispose();await rm(root,{recursive:true,force:true})}
});
