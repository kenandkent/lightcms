const {readFileSync}=require('node:fs');
const vm=require('node:vm');
const assert=require('node:assert/strict');
const html=readFileSync(0,'utf8');
const script=[...html.matchAll(/<script>([\s\S]*?)<\/script>/g)].map(x=>x[1]).find(x=>x.includes("const base='/cm/static-delivery/api'"));
async function run(fail,standalone=false,asyncPreview=false,previewFailure=false,queued=false,resumed=false,uncertain=false){
 const nodes=new Map();
 for(const match of html.matchAll(/id="([^"]+)"/g))nodes.set(match[1],{value:'',textContent:'',disabled:false,hidden:true,checked:false,children:[],get options(){return this.children},add(x){this.children.push(x)},replaceChildren(){this.children=[]},append(x){this.children.push(x)}});
 assert(!nodes.has('export-start'));
 assert(nodes.has('standalone-export'),'standalone download must remain available without a Worker');
 const target={id:'target1',account_ref:'account1',account_name:'Account',name:'Reports',origin:'https://reports.example',environment:'test',revision:0,config_revision:1,busy:queued};
 const calls=[];let keys=0;
 const context={document:{getElementById:id=>nodes.get(id),createElement:()=>({children:[],append(x){this.children.push(x)}})},location:{search:resumed?'?preview_job_id=000000000000000000000123':'',href:'https://cms.example/cm/static-delivery/publish'},URL,URLSearchParams,AbortController,history:{replaceState(){}},crypto:{randomUUID:()=> 'unique-key-'+(++keys)},Option:function(text,value){this.text=text;this.value=value},setTimeout:(fn,ms)=>{if(ms<30000)queueMicrotask(fn);return 1},clearTimeout(){},fetch:async(url,options)=>{
  const body=options.body?JSON.parse(options.body):null;calls.push({url,body,idem:options.headers['Idempotency-Key']});
  if(uncertain&&url.endsWith('/deployment-preview')&&calls.filter(c=>c.url.endsWith('/deployment-preview')).length===1)throw Error('response lost');
  if(resumed&&url.endsWith('/static-preview-jobs/000000000000000000000123')&&calls.filter(c=>c.url===url).length>1)assert.equal(nodes.get('publish-account').disabled,true,'resume must lock target until frozen preview is restored');
  let data;
  if(url.endsWith('/options'))data=[target];
  else if(url.includes('/deployments?'))data={jobs:[]};
  else if(url.endsWith('/static-exports'))data={job_id:'export-job'};
  else if(url.endsWith('/static-deployments/export-job'))data={job:fail?{state:'failed',error_code:'STATIC_EXPORT_FAILED'}:{state:'succeeded',export_id:'export1'}};
  else if(url.endsWith('/deployment-preview')){assert.deepEqual(body.export_ids,['export1']);assert.equal(options.headers.Prefer,'respond-async');assert(options.headers['Idempotency-Key']);data=asyncPreview?{job_id:'preview-job',state:'queued'}:{changes:[{path:'/a',action:'add',url:target.origin+'/a'}],preserved_pages:0,confirmation_digest:'digest'}}
  else if(url.endsWith('/static-preview-jobs/preview-job'))data=previewFailure?{state:'failed',error_code:'PREVIEW_TEST_FAILURE'}:{state:'succeeded',preview:{changes:[{path:'/a',action:'add',url:target.origin+'/a'}],preserved_pages:0,confirmation_digest:'digest'}};
  else if(url.endsWith('/static-preview-jobs/000000000000000000000123'))data={target_id:target.id,state:'succeeded',preview:{request:{export_ids:['export1'],expected_revision:0,expected_config_revision:1,manual_script_review:true},changes:[{path:'/a',action:'add',url:target.origin+'/a'}],preserved_pages:0,confirmation_digest:'restored-digest'}};
  else if(url.endsWith('/deployments')){assert.equal(body.confirmation_digest,'digest');data={job_id:'publish-job'}}
  else if(url.endsWith('/static-deployments/publish-job'))data={job:{kind:'deploy',state:'succeeded'},pages:[{path:'/a',url:'https://verified.example/a'}]};
  else throw Error('unexpected request '+url);
  return {ok:true,json:async()=>data};
 }};
 vm.runInNewContext(script,context);
 await new Promise(resolve=>setImmediate(resolve));
 if(resumed){assert.equal(nodes.get('preview-panel').hidden,false);assert.equal(nodes.get('publish-consent').checked,false);assert.equal(calls.filter(c=>c.body).length,0,'refresh must resume reads, not create export or deployment');return}
 if(standalone){
  const first=nodes.get('standalone-export').onclick();
  await nodes.get('standalone-export').onclick();await first;
  assert.equal(calls.filter(c=>c.url.endsWith('/static-exports')).length,1);
  assert.equal(calls.filter(c=>c.url.endsWith('/deployment-preview')).length,0);
  assert.equal(nodes.get('downloads').hidden,false);
  return;
 }
 nodes.get('publish-account').value='account1';nodes.get('publish-account').onchange();
 nodes.get('publish-target').value='target1';nodes.get('publish-target').onchange();
 if(queued){assert.equal(nodes.get('preview-start').disabled,true);nodes.get('queue-mode').checked=true;nodes.get('queue-mode').onchange()}
 assert.equal(nodes.get('preview-start').disabled,false,'preview must work before export exists');
 nodes.get('script-review').checked=true;
 const first=nodes.get('preview-start').onclick();
 await nodes.get('preview-start').onclick(); // double-click must not create a second export
 await first;
 if(uncertain){await nodes.get('preview-start').onclick();const posts=calls.filter(c=>c.url.endsWith('/deployment-preview'));assert.equal(posts.length,2);assert.equal(posts[0].idem,posts[1].idem,'uncertain response must reuse preview key')}
 assert.equal(calls.filter(c=>c.url.endsWith('/static-exports')).length,1);
 assert.equal(calls.filter(c=>c.url.endsWith('/deployment-preview')).length,fail?0:uncertain?2:1);
 if(queued)assert.equal(calls.find(c=>c.url.endsWith('/deployment-preview')).body.queue_mode,'merge_nonconflicting');
 assert.equal(nodes.get('preview-start').disabled,false);
 if(fail)assert.match(nodes.get('delivery-error').textContent,/STATIC_EXPORT_FAILED/);
 else if(previewFailure){assert.match(nodes.get('delivery-error').textContent,/PREVIEW_TEST_FAILURE/);assert.equal(nodes.get('preview-panel').hidden,true)}
 else {
  assert.equal(nodes.get('preview-panel').hidden,false);assert.match(nodes.get('zip-link').href,/export1\/download/);
  nodes.get('publish-consent').checked=true;
  const submitting=nodes.get('deploy-start').onclick();await nodes.get('deploy-start').onclick();await submitting;
  assert.equal(calls.filter(c=>c.url.endsWith('/deployments')).length,1,'double click must submit one confirmed job');
  assert.equal(nodes.get('published-links').children[0].children[0].href,'https://verified.example/a','share URL must come from verified release, not target origin');
  nodes.get('deploy-start').disabled=true; // previous successful submission
  await nodes.get('preview-start').onclick();
  assert.equal(nodes.get('deploy-start').disabled,false,'a fresh preview must enable confirmation');
 }
}
(async()=>{await run(false);await run(true);await run(false,true);await run(false,false,true);await run(false,false,true,true);await run(false,false,true,false,true);await run(false,false,true,false,false,true);await run(false,false,true,false,false,false,true)})().catch(e=>{console.error(e);process.exitCode=1});
