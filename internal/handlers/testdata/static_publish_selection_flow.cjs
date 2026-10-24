const {readFileSync}=require('node:fs');
const vm=require('node:vm');
const assert=require('node:assert/strict');
const html=readFileSync(0,'utf8');
const source=[...html.matchAll(/<script>([\s\S]*?)<\/script>/g)].map(x=>x[1]).find(x=>x.includes("const base='/cm/static-delivery/api'"));
const one={id:'target1',account_ref:'account1',account_name:'Account one',name:'Report one',origin:'https://one.example',environment:'test',revision:4,config_revision:2,busy:false};
const two={...one,id:'target2',name:'Report two',origin:'https://two.example',environment:'production'};
const tick=()=>new Promise(resolve=>setImmediate(resolve));
const deferred=()=>{let resolve;const promise=new Promise(r=>resolve=r);return {promise,resolve}};
function page(config={}){
 const nodes=new Map();
 const element=(tag='',attrs='')=>({tagName:tag.toUpperCase(),value:'',textContent:'',disabled:/\sdisabled(?:\s|>|$)/.test(attrs),hidden:/\shidden(?:\s|>|$)/.test(attrs),checked:false,children:[],get options(){return this.children},add(x){this.children.push(x)},replaceChildren(...items){this.children=[...items];if(this.tagName==='SELECT')this.value=items[0]?.value||''},append(...items){this.children.push(...items)},setAttribute(name,value){this[name]=value},removeAttribute(name){delete this[name]}});
 for(const m of html.matchAll(/<([a-z][a-z0-9-]*)\b([^>]*\bid="([^"]+)"[^>]*)>/gi)){const node=element(m[1],m[2]);node.textContent=html.slice(m.index+m[0].length).split('</'+m[1]+'>')[0].replace(/<[^>]*>/g,'');nodes.set(m[3],node)}
 const calls=[];
 const initial=new URL('https://cms.example/cm/static-delivery/publish'+(config.search||''));
 const location={href:initial.href,search:initial.search};
 let key=0;
 const response=data=>({ok:true,status:200,json:async()=>JSON.parse(JSON.stringify(data))});
 const preview=(target=one,request)=>({request,changes:[{path:'/report',action:'add',url:target.origin+'/report'}],preserved_pages:3,confirmation_digest:'digest-'+target.id});
 const fetch=async(url,options)=>{
  const body=options.body?JSON.parse(options.body):null;
  const call={url,body,headers:options.headers};calls.push(call);
  if(config.respond){const data=await config.respond(call,calls);if(data!==undefined)return response(data)}
  if(url.endsWith('/options'))return response(config.targets||[one]);
  if(url.includes('/deployments?'))return response({jobs:[]});
  if(url.endsWith('/static-exports'))return response({job_id:'export-job'});
  if(url.endsWith('/static-deployments/export-job'))return response({job:{kind:'export',state:'succeeded',export_id:'export1'}});
  if(url.endsWith('/deployment-preview')){assert.deepEqual(body.export_ids,['export1']);assert.equal(body.manual_script_review,true);assert.equal(options.headers.Prefer,'respond-async');assert(options.headers['Idempotency-Key']);return response(preview(url.includes('/target2/')?two:one,body))}
  if(url.endsWith('/deployments'))return response({job_id:'publish-job'});
  if(url.endsWith('/static-deployments/publish-job'))return response({job:{kind:'deploy',state:'succeeded',target_id:one.id},pages:[{path:'/report',url:'https://verified.example/report'}]});
  throw Error('unexpected request '+url);
 };
 const context={document:{getElementById:id=>nodes.get(id),createElement:tag=>element(tag)},location,URL,URLSearchParams,AbortController,history:{replaceState(_state,_title,url){location.href=String(url);location.search=new URL(location.href).search}},crypto:{randomUUID:()=> 'key-'+(++key)},Option:function(text,value){this.text=text;this.textContent=text;this.value=value},setTimeout:(fn,ms)=>{if(ms===1500)queueMicrotask(fn);return 1},clearTimeout(){},fetch,navigator:{clipboard:{writeText:async()=>{}}}};
 vm.runInNewContext(source,context);
 const choose=id=>{const t=(config.targets||[one]).find(t=>t.id===id);nodes.get('publish-account').value=t.account_ref;nodes.get('publish-account').onchange();nodes.get('publish-target').value=id;nodes.get('publish-target').onchange()};
 const prepare=()=>{nodes.get('script-review').checked=true;nodes.get('script-review').onchange();return nodes.get('preview-start').onclick()};
 const deploy=()=>{nodes.get('publish-consent').checked=true;nodes.get('publish-consent').onchange?.();return nodes.get('deploy-start').onclick()};
 return {nodes,calls,location,choose,prepare,deploy,ready:tick()};
}
(async()=>{
 if(process.argv[2]==='empty'){
  const p=page();await p.ready;assert.equal(p.nodes.get('preview-start').disabled,true);assert.equal(p.nodes.get('standalone-export').disabled,true);assert.equal(p.calls.filter(c=>c.body).length,0);return;
 }
 if(process.argv[2]==='download-only'){
  const p=page();await p.ready;assert.equal(p.nodes.has('publish-account'),false);await p.nodes.get('standalone-export').onclick();assert.equal(p.nodes.get('downloads').hidden,false);assert.equal(p.calls.filter(c=>c.url.endsWith('/static-exports')).length,1);assert.equal(p.calls.filter(c=>c.url.endsWith('/options')||c.url.endsWith('/deployment-preview')||c.url.endsWith('/deployments')).length,0);return;
 }
 // A unique available account and Worker should be ready without two manual changes.
 let p=page();await p.ready;
 assert.equal(p.nodes.get('publish-account').value,'account1','a unique account must be selected automatically');
 assert.equal(p.nodes.get('publish-target').value,'target1','a unique Worker must be selected automatically');
 assert.equal(p.nodes.get('script-review').checked,false,'target defaults must not consent to script review');
 assert.equal(p.nodes.get('publish-consent').checked,false,'target defaults must not consent to publication');
 assert.equal(p.calls.filter(c=>c.body).length,0,'opening the page must not create export or deploy jobs');
 await p.prepare();assert.equal(p.nodes.get('preview-panel').hidden,false);assert.equal(p.calls.find(c=>c.url.endsWith('/static-exports')).body.items.length,2);
 await p.nodes.get('deploy-start').onclick();assert.equal(p.calls.filter(c=>c.url.endsWith('/deployments')).length,0,'preview alone must never submit a deployment');

 p=page({targets:[one,two]});await p.ready;
 assert.equal(p.nodes.get('publish-account').value,'account1');assert.equal(p.nodes.get('publish-target').value,'','multiple Workers require an explicit choice');
 assert.equal(p.nodes.get('preview-start').disabled,true);

 p=page({respond:async(c,calls)=>c.url.endsWith('/options')?(calls.filter(x=>x.url===c.url).length===1?[]:[one]):undefined});await p.ready;
 assert.equal(p.nodes.get('preview-start').disabled,true);assert.equal(p.nodes.get('reload-targets').hidden,false);
 await p.nodes.get('reload-targets').onclick();assert.equal(p.nodes.get('publish-target').value,'target1','reloading newly available destinations should recover without refreshing the page');
 p=page({targets:[one,{...two,account_ref:'account2',account_name:'Account two'}]});await p.ready;
 assert.equal(p.nodes.get('publish-account').value,'','multiple accounts require an explicit choice');
 assert.doesNotMatch(p.nodes.get('target-details').textContent,/Loading|正在读取/,'loaded options must stop displaying a loading state');
 p=page({targets:[one,{...two,account_ref:'account2'},{...two,id:'target3',account_ref:'account2'}]});await p.ready;p.choose('target1');p.nodes.get('publish-account').value='account2';p.nodes.get('publish-account').onchange();
 assert.equal(new URL(p.location.href).searchParams.get('target_id'),null,'changing account must remove the previous Worker deep link');
 p=page({targets:[one,two],search:'?target_id=target2'});await p.ready;
 assert.equal(p.nodes.get('publish-target').value,'target2','a current target deep link must preselect its Worker');
 assert.match(p.nodes.get('target-details').textContent,/https:\/\/two.example/);
 p=page({search:'?target_id=removed'});await p.ready;
 assert.equal(p.nodes.get('publish-target').value,'','an unavailable deep link must not silently choose another Worker');
 assert.equal(p.nodes.get('preview-start').disabled,true);

 // Late async previews and histories must not populate a different target.
 const pending=deferred();
 p=page({targets:[one,two],respond:async c=>c.url.endsWith('/target1/deployment-preview')?pending.promise:undefined});await p.ready;p.choose('target1');
 const preparing=p.prepare();await tick();p.choose('target2');
 pending.resolve({changes:[{path:'/old',action:'replace',url:one.origin+'/old'}],preserved_pages:0,confirmation_digest:'old'});await preparing;
 assert.equal(p.nodes.get('preview-panel').hidden,true,'a late preview for the previous Worker must be discarded');
 await p.deploy();assert.equal(p.calls.filter(c=>c.url.endsWith('/deployments')).length,0,'changing Worker must invalidate the previous confirmation');
 const oldHistory=deferred();
 p=page({targets:[one,two],respond:async c=>c.url.includes('/target1/deployments?')?oldHistory.promise:c.url.includes('/target2/deployments?')?{jobs:[{id:'new-worker-job',state:'succeeded'}]}:undefined});await p.ready;p.choose('target1');p.choose('target2');await tick();
 oldHistory.resolve({jobs:[{id:'old-worker-job',state:'succeeded'}]});await tick();
 assert.equal(p.nodes.get('job-history').children.length,1);
 assert.match(p.nodes.get('job-history').children[0].children[0].textContent,/new-worker-job/,'late history must not replace the current Worker history');
 assert.equal(new URL(p.nodes.get('job-history').children[0].children[0].href).searchParams.get('job_id'),'new-worker-job');
 assert.equal(new URL(p.nodes.get('job-history').children[0].children[0].href).searchParams.get('target_id'),'target2');

 // A lost POST response retries the same operation; a lost preview read resumes its job.
 p=page({respond:async(c,calls)=>{if(c.url.endsWith('/deployment-preview')&&calls.filter(x=>x.url===c.url).length===1)throw Error('preview response lost')}});await p.ready;await p.prepare();await p.nodes.get('preview-start').onclick();
 let posts=p.calls.filter(c=>c.url.endsWith('/deployment-preview'));assert.equal(posts.length,2);assert.equal(posts[0].headers['Idempotency-Key'],posts[1].headers['Idempotency-Key']);
 let reads=0;
 p=page({respond:async c=>{if(c.url.endsWith('/deployment-preview'))return {job_id:'preview-job',state:'queued'};if(c.url.endsWith('/static-preview-jobs/preview-job')){if(++reads===1)throw Error('preview read lost');return {state:'succeeded',preview:{changes:[],preserved_pages:3,confirmation_digest:'digest-target1'}}}}});await p.ready;await p.prepare();await p.nodes.get('preview-start').onclick();
 assert.equal(p.calls.filter(c=>c.url.endsWith('/deployment-preview')).length,1,'retry after a failed read must resume the accepted preview job');
 assert.equal(reads,2);assert.equal(p.nodes.get('preview-panel').hidden,false);
 p=page({respond:async(c,calls)=>{if(c.url.endsWith('/deployments')&&calls.filter(x=>x.url===c.url).length===1)throw Error('deployment response lost')}});await p.ready;await p.prepare();await p.deploy();await p.nodes.get('deploy-start').onclick();
 posts=p.calls.filter(c=>c.url.endsWith('/deployments'));assert.equal(posts.length,2);assert.equal(posts[0].headers['Idempotency-Key'],posts[1].headers['Idempotency-Key'],'deployment retry must keep the same confirmed operation key');
 assert.equal(posts[0].body.confirmation_digest,'digest-target1');
 assert.equal(p.nodes.get('published-links').children[0].children[0].href,'https://verified.example/report');

 // An accepted running job must make this Worker busy before the next options refresh.
 p=page({respond:async c=>c.url.endsWith('/static-deployments/publish-job')?{job:{kind:'deploy',state:'activation_unknown',target_id:one.id}}:undefined});
 await p.ready;await p.prepare();await p.deploy();
 assert.equal(p.nodes.get('preview-start').disabled,true,'an accepted job must block another unqueued publication');
 assert.match(p.nodes.get('target-details').textContent,/Pending job|有未决任务/);

 // Refresh recovers saved jobs and previews through reads, preserving explicit confirmation.
 const saved='000000000000000000000123';
 p=page({targets:[one,two],search:'?job_id='+saved,respond:async c=>c.url.endsWith('/static-deployments/'+saved)?{job:{kind:'deploy',state:'succeeded',target_id:two.id},pages:[{path:'/saved',url:'https://verified.example/saved'}]}:undefined});await p.ready;
 assert.equal(p.nodes.get('publish-target').value,'target2');assert.equal(p.nodes.get('refresh-job').hidden,false);
 assert.equal(p.calls.filter(c=>c.body).length,0,'refreshing a saved job must not create another operation');
 let restoreReads=0;
 p=page({search:'?preview_job_id='+saved,respond:async c=>{if(c.url.endsWith('/static-preview-jobs/'+saved)){if(++restoreReads===1)throw Error('restore temporarily unavailable');return {target_id:one.id,state:'succeeded',preview:{request:{export_ids:['export1'],expected_revision:4,expected_config_revision:2,manual_script_review:true},changes:[],preserved_pages:3,confirmation_digest:'saved-digest'}}}}});await p.ready;
 assert.equal(p.nodes.get('preview-start').disabled,false,'a failed refresh must leave a way to resume the same saved preview');
 await p.nodes.get('preview-start').onclick();assert.equal(p.nodes.get('preview-panel').hidden,false);assert.equal(p.nodes.get('publish-consent').checked,false);
 assert.equal(p.calls.filter(c=>c.body).length,0);

 // A preview-restore response received after a target change cannot switch the user back.
 const oldRestore=deferred();
 p=page({targets:[one,two],search:'?preview_job_id='+saved,respond:async c=>c.url.endsWith('/static-preview-jobs/'+saved)?oldRestore.promise:undefined});await p.ready;p.choose('target2');
 oldRestore.resolve({target_id:one.id,state:'succeeded',preview:{request:{manual_script_review:true},changes:[],preserved_pages:0,confirmation_digest:'stale-restored'}});await tick();
 assert.equal(p.nodes.get('publish-target').value,'target2','a stale restore must not select its old Worker');assert.equal(p.nodes.get('preview-panel').hidden,true);
})().catch(e=>{console.error(e);process.exitCode=1});
