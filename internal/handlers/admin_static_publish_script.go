package handlers

const staticPublishScript = `
<script>
(()=>{'use strict';
const base='/cm/static-delivery/api', selected={{.SelectedIDs}}||[], csrf={{.CSRFToken}}, text={{$P}}, $=id=>document.getElementById(id);
const params=new URLSearchParams(location.search),resumedJob=params.get('job_id'),resumedPreview=params.get('preview_job_id'),requestedTarget=params.get('target_id');
let exportID='',exportJobID='',exportKey='',exportPromise=null,targets=[],target=null,preview=null,request=null,pendingPreview=null;
let operation='',loadingOptions=false,submitted=false,viewVersion=0,historyVersion=0,optionsVersion=0,jobID='',pollTimer=null,pollVersion=0,polling='',cancelKey='';
function key(){return crypto.randomUUID?crypto.randomUUID():Date.now().toString(36)+'-'+Math.random().toString(36).slice(2)}
function updateURL(change){const url=new URL(location.href);change(url.searchParams);history.replaceState(null,'',url)}
function clearError(){$('delivery-error').textContent='';$('delivery-error').hidden=true}
function error(e){if(e.stale)return;$('delivery-error').textContent=e.message||String(e);$('delivery-error').hidden=false}
function current(version){if(version!==viewVersion){const e=Error('stale');e.stale=true;throw e}}
function targetBlocked(){return !!target&&target.busy&&!$('queue-mode').checked}
function syncControls(){
 const locked=!!operation||loadingOptions,reading=!!jobID&&polling===pollVersion+':'+jobID;
 if($('publish-account')){
  $('publish-account').disabled=locked||!targets.length;$('publish-target').disabled=locked||!$('publish-account').value;
  $('script-review').disabled=locked;$('queue-mode').disabled=locked;
  $('preview-start').disabled=locked||(!pendingPreview?.restored&&(!target||!selected.length||targetBlocked()));$('reload-targets').disabled=locked;
 }
 $('standalone-export').disabled=locked||!selected.length;$('deploy-start').disabled=locked||!preview||submitted;
 $('refresh-job').disabled=locked||reading;$('cancel-job').disabled=locked||reading;
}
async function api(path,body,idem){
 const controller=new AbortController(),timer=setTimeout(()=>controller.abort(),30000);
 const opt={credentials:'same-origin',signal:controller.signal,headers:{'X-CSRF-Token':csrf}};
 if(body!==undefined){opt.method='POST';opt.headers['Content-Type']='application/json';opt.body=JSON.stringify(body);if(path.endsWith('/deployment-preview'))opt.headers.Prefer='respond-async';if(idem)opt.headers['Idempotency-Key']=idem}
 try{const r=await fetch(base+path,opt);let value;try{value=await r.json()}catch{throw Error(text.jsonError+r.status)}if(!r.ok){const e=Error((value.code||'HTTP '+r.status)+': '+(value.message||text.requestFailed));e.terminal=r.status<500&&r.status!==429;throw e}return value}finally{clearTimeout(timer)}
}
function downloads(id){exportID=id;$('downloads').hidden=false;$('zip-link').href=base+'/static-exports/'+encodeURIComponent(id)+'/download?format=zip';$('html-link').href=base+'/static-exports/'+encodeURIComponent(id)+'/download?format=html'}
async function ensureExport(){
 if(exportID)return;if(exportPromise)return exportPromise;
 exportPromise=(async()=>{
  if(!selected.length)throw Error(text.selectRequired);
  $('export-status').textContent=text.exportPreparing;
  if(!exportJobID){exportKey=exportKey||key();const value=await api('/static-exports',{items:selected.map(content_id=>({content_id}))},exportKey);exportJobID=value.job_id}
  const started=Date.now();
  while(Date.now()-started<300000){
   const data=await api('/static-deployments/'+encodeURIComponent(exportJobID)),job=data.job;
   $('export-status').textContent=text.exportState+(text[job.state]||job.state);
   if(job.state==='succeeded'){downloads(job.export_id);$('export-status').textContent=text.filesReady;return}
   if(['failed','blocked','cancelled'].includes(job.state)){exportJobID='';exportKey='';throw Error(text.exportFailed+(job.error_code||text[job.state]))}
   await new Promise(resolve=>setTimeout(resolve,1500));
  }
  throw Error(text.exportTimeout);
 })();
 try{return await exportPromise}finally{exportPromise=null}
}
function invalidate(clearAttempt=true){
 viewVersion++;preview=null;request=null;submitted=false;$('preview-panel').hidden=true;$('publish-consent').checked=false;
 if(clearAttempt){pendingPreview=null;updateURL(p=>p.delete('preview_job_id'))}
 syncControls();
}
function clearWatchedJob(){clearTimeout(pollTimer);pollVersion++;jobID='';cancelKey='';$('refresh-job').hidden=true;$('cancel-job').hidden=true;$('job-reference').hidden=true;$('published-links').replaceChildren();$('deployment-status').textContent=text.deploymentIdle;updateURL(p=>p.delete('job_id'))}
function targetLabel(t){return t.name+' · '+(t.environment==='production'?text.production:text.test)+(t.busy?' · '+text.busy:'')}
function targetDetails(){
 $('target-details').textContent=target?target.origin+' · '+(target.environment==='production'?text.production:text.test)+' · '+text.revision+' '+target.revision+' · '+(target.busy?text.busy:text.ready)+(target.busy?'\n'+text.busyHelp:''):'';
}
function markBusy(){if(!target)return;const id=target.id;targets=targets.map(item=>item.id===id?{...item,busy:true}:item);target=targets.find(item=>item.id===id);targetDetails();const option=Array.from($('publish-target').options).find(item=>item.value===id);if(option)option.text=targetLabel(target)}
function fillTargets(account){$('publish-target').replaceChildren(new Option(account?text.chooseTarget:text.chooseAccountFirst,''));for(const item of targets.filter(t=>t.account_ref===account))$('publish-target').add(new Option(targetLabel(item),item.id))}
function chooseTarget(id,preserveJob=false){
 const next=targets.find(t=>t.id===id)||null,changed=target?.id!==next?.id;
 invalidate();if(!preserveJob)clearWatchedJob();target=next;
 if(changed){$('script-review').checked=false;$('queue-mode').checked=false}
 $('publish-target').value=next?next.id:'';targetDetails();
 updateURL(p=>{if(target)p.set('target_id',target.id);else p.delete('target_id')});
 $('job-history').replaceChildren();$('job-history-status').textContent=text.historyChoose;
 syncControls();loadHistory().catch(error);
}
function chooseAccount(account,autoselect=true,preserveJob=false){
 invalidate();if(!preserveJob)clearWatchedJob();target=null;$('publish-account').value=account;$('script-review').checked=false;$('queue-mode').checked=false;fillTargets(account);targetDetails();
 updateURL(p=>p.delete('target_id'));
 $('job-history').replaceChildren();$('job-history-status').textContent=text.historyChoose;
 const candidates=targets.filter(t=>t.account_ref===account);if(autoselect&&candidates.length===1)chooseTarget(candidates[0].id,preserveJob);else syncControls();
}
async function loadHistory(){
 const destination=target,version=++historyVersion;if(!destination)return;
 $('job-history-status').textContent=text.historyLoading;
 try{const data=await api('/static-targets/'+encodeURIComponent(destination.id)+'/deployments?limit=20');if(target?.id!==destination.id||version!==historyVersion)return;
  $('job-history').replaceChildren();$('job-history-status').textContent=data.jobs.length?'':text.historyEmpty;
  for(const job of data.jobs){const li=document.createElement('li'),link=document.createElement('a'),url=new URL(location.href);url.searchParams.set('target_id',destination.id);url.searchParams.set('job_id',job.id);url.searchParams.delete('preview_job_id');link.href=String(url);link.textContent=(text[job.state]||job.state)+' · '+job.id;link.onclick=e=>{if(e)e.preventDefault();if(operation)return;watchJob(job.id)};li.append(link);$('job-history').append(li)}
 }catch(e){if(target?.id===destination.id&&version===historyVersion)throw e}
}
async function options(preselect=requestedTarget){
 if(!$('publish-account'))return;
 const version=++optionsVersion;loadingOptions=true;syncControls();
 try{
  const value=await api('/static-delivery/options');if(version!==optionsVersion)return;targets=value;$('target-details').textContent='';
  $('publish-account').replaceChildren(new Option(text.chooseAccount,''));const seen=new Set();for(const item of targets){if(seen.has(item.account_ref))continue;seen.add(item.account_ref);$('publish-account').add(new Option(item.account_name,item.account_ref))}
  $('reload-targets').hidden=targets.length>0;$('manage-targets').hidden=targets.length>0;
  if(preselect){const found=targets.find(t=>t.id===preselect);if(found){chooseAccount(found.account_ref,false);chooseTarget(found.id)}else{chooseAccount(seen.size===1?[...seen][0]:'',false);$('target-details').textContent=text.unavailableTarget}}
  else if(!resumedJob&&!resumedPreview&&seen.size===1)chooseAccount([...seen][0]);
  if(!targets.length)$('target-details').textContent=text.emptyTargets;
 }catch(e){if(version!==optionsVersion)return;$('reload-targets').hidden=false;error(e)}finally{if(version===optionsVersion){loadingOptions=false;syncControls()}}
}
async function waitPreview(id,version){
 current(version);updateURL(p=>{p.set('preview_job_id',id);p.delete('job_id')});
 const started=Date.now();
 while(Date.now()-started<300000){
  const result=await api('/static-preview-jobs/'+encodeURIComponent(id));current(version);
  if(result.state==='succeeded'&&result.preview)return result.preview;
  if(['failed','cancelled','needs_review','partial'].includes(result.state)){const e=Error(text.previewFailed+(result.error_code||result.state));e.terminal=true;throw e}
  $('preview-start').textContent=result.state==='queued'?text.previewQueued:text.previewPreparing;$('export-status').textContent=$('preview-start').textContent;
  await new Promise(resolve=>setTimeout(resolve,1500));current(version);
 }
 throw Error(text.previewTimeout+id);
}
function showPreview(value,destination,body){
 preview=value;request={...(value.request||body),confirmation_digest:value.confirmation_digest,_key:key()};submitted=false;
 $('preview-summary').textContent=destination.account_name+' / '+destination.name+' / '+destination.origin+' · '+(destination.environment==='production'?text.production:text.test)+' · '+text.changed+': '+value.changes.length+' · '+text.preserved+': '+value.preserved_pages;
 $('preview-changes').replaceChildren();for(const page of value.changes){const li=document.createElement('li');li.textContent=(page.action==='replace'?text.replace:text.add)+page.path+' → '+page.url;$('preview-changes').append(li)}
 $('preview-warnings').replaceChildren();for(const diagnostic of value.diagnostics||[]){const li=document.createElement('li');li.textContent=diagnostic.code+': '+diagnostic.message;$('preview-warnings').append(li)}
 $('publish-consent').checked=false;$('preview-panel').hidden=false;$('export-status').textContent=text.previewReady;syncControls();
}
async function resumePreview(id){
 let version=viewVersion;pendingPreview={signature:'restored:'+id,key:key(),jobID:id,restored:true};operation='preview';clearError();syncControls();
 try{
  const job=await api('/static-preview-jobs/'+encodeURIComponent(id));current(version);const destination=targets.find(t=>t.id===job.target_id);if(!destination){const e=Error(text.unavailableTarget);e.terminal=true;throw e}
  chooseAccount(destination.account_ref,false);chooseTarget(destination.id);version=viewVersion;
  pendingPreview={signature:'restored:'+id,key:key(),jobID:id,restored:true};
  const value=await waitPreview(id,version);current(version);showPreview(value,destination,value.request);pendingPreview=null;$('export-status').textContent=text.previewRestored;
 }catch(e){if(viewVersion===version){if(e.terminal)pendingPreview=null;error(e)}}finally{operation='';$('preview-start').textContent=pendingPreview?text.retryPreview:text.preview;syncControls()}
}
function remember(id){if(id!==jobID)cancelKey='';jobID=id;updateURL(p=>{p.set('job_id',id);p.delete('preview_job_id')});$('refresh-job').hidden=false;$('job-reference').textContent=text.job+': '+id;$('job-reference').hidden=false}
function watchJob(id){clearTimeout(pollTimer);pollVersion++;invalidate();remember(id);return poll()}
function cancellation(job){$('cancel-job').hidden=!['queued','preparing','uploading','version_ready','blocked'].includes(job.state)||!!(job.checkpoint&&job.checkpoint.activation_intent_at)}
async function refreshCurrentTarget(id,version){
 if(!target)return;
 const currentID=target.id,value=await api('/static-delivery/options');if(jobID!==id||version!==pollVersion)return;targets=value;const next=targets.find(t=>t.id===currentID)||null;
 target=next;invalidate();fillTargets($('publish-account').value);$('publish-target').value=next?next.id:'';targetDetails();if(!next)$('target-details').textContent=text.unavailableTarget;syncControls();await loadHistory();
}
async function poll(){
 const id=jobID,version=pollVersion,stamp=version+':'+id;if(!id||polling===stamp)return;polling=stamp;syncControls();
 try{
  const data=await api('/static-deployments/'+encodeURIComponent(id));if(jobID!==id||version!==pollVersion)return;const job=data.job;
  if(job.target_id&&target?.id!==job.target_id&&$('publish-account')){const destination=targets.find(t=>t.id===job.target_id);if(destination){chooseAccount(destination.account_ref,false,true);chooseTarget(destination.id,true);remember(id)}}
  cancellation(job);const label=text[job.state]||job.state;
  if(job.kind!=='export'&&!['succeeded','failed','blocked','cancelled'].includes(job.state)&&target?.id===job.target_id)markBusy();
  if(job.kind==='export'){$('export-status').textContent=label+(job.error_code?': '+job.error_code:'');if(job.state==='succeeded')downloads(job.export_id)}
  else{
   $('deployment-status').textContent=(job.state!=='succeeded'&&job.checkpoint&&job.checkpoint.deadline_exceeded?text.deadline:job.state==='succeeded'?text.success:label)+(job.error_code?': '+job.error_code:'');
   if(job.state!=='succeeded'&&job.candidate_origin)$('deployment-status').textContent+=' · '+text.candidate+job.candidate_origin;
   $('published-links').replaceChildren();
   if(job.state==='succeeded')for(const page of data.pages||[]){const li=document.createElement('li'),link=document.createElement('a'),copy=document.createElement('button');link.textContent=page.path;link.href=page.url;link.target='_blank';link.rel='noopener noreferrer';copy.className='btn btn-secondary';copy.textContent=text.copy;copy.onclick=async()=>{try{await navigator.clipboard.writeText(page.url);copy.textContent=text.copied}catch{error(Error(text.copyFailed))}};li.append(link);li.append(copy);$('published-links').append(li)}
  }
  if(['succeeded','failed','blocked','cancelled'].includes(job.state)){clearTimeout(pollTimer);if(job.kind!=='export')try{await refreshCurrentTarget(id,version)}catch(e){error(e)}return}
  pollTimer=setTimeout(poll,2500);
 }catch(e){if(jobID===id&&version===pollVersion){error(e);$('deployment-status').textContent=text.readFailed}}finally{if(polling===stamp)polling='';syncControls()}
}
$('standalone-export').onclick=async()=>{if($('standalone-export').disabled)return;operation='export';clearError();syncControls();try{await ensureExport()}catch(e){error(e)}finally{operation='';syncControls()}};
$('refresh-job').onclick=()=>{clearTimeout(pollTimer);clearError();return poll()};
$('cancel-job').onclick=async()=>{if($('cancel-job').disabled)return;operation='cancel';clearError();syncControls();try{cancelKey=cancelKey||key();await api('/static-deployments/'+encodeURIComponent(jobID)+'/cancel',{},cancelKey);clearTimeout(pollTimer);await poll();await loadHistory()}catch(e){error(e)}finally{operation='';syncControls()}};
if($('publish-account')){
 $('publish-account').onchange=()=>{chooseAccount($('publish-account').value);clearError()};
 $('publish-target').onchange=()=>{chooseTarget($('publish-target').value);clearError()};
 $('script-review').onchange=()=>invalidate();$('queue-mode').onchange=()=>invalidate();
 $('reload-targets').onclick=()=>{clearError();return options(target?.id||'')};
 $('preview-start').onclick=async()=>{
  if($('preview-start').disabled)return;
  if(pendingPreview?.restored)return resumePreview(pendingPreview.jobID);
  invalidate(false);clearError();operation='preview';syncControls();const version=viewVersion,destination=target;
  try{
   if(!destination||!selected.length)throw Error(text.targetRequired);if(!$('script-review').checked)throw Error(text.reviewRequired);
   $('preview-start').textContent=text.exportPreparing;await ensureExport();current(version);
   const body={export_ids:[exportID],expected_revision:destination.revision,expected_config_revision:destination.config_revision,manual_script_review:true};if($('queue-mode').checked)body.queue_mode='merge_nonconflicting';
   const signature=destination.id+'\n'+JSON.stringify(body);if(!pendingPreview||pendingPreview.signature!==signature)pendingPreview={signature,key:key(),jobID:''};const attempt=pendingPreview;
   $('preview-start').textContent=text.previewPreparing;$('export-status').textContent=text.previewPreparing;
   let value;if(attempt.jobID)value=await waitPreview(attempt.jobID,version);else{value=await api('/static-targets/'+encodeURIComponent(destination.id)+'/deployment-preview',body,attempt.key);current(version);if(value.job_id){attempt.jobID=value.job_id;value=await waitPreview(attempt.jobID,version)}}
   current(version);showPreview(value,destination,body);pendingPreview=null;
  }catch(e){if(viewVersion===version){if(e.terminal)pendingPreview=null;error(e)}}finally{operation='';$('preview-start').textContent=pendingPreview?text.retryPreview:text.preview;syncControls()}
 };
}
$('deploy-start').onclick=async()=>{
 if($('deploy-start').disabled)return;
 const version=viewVersion,destination=target,confirmation=request;
 try{
  if(!preview||!confirmation||!$('publish-consent').checked)throw Error(text.consentRequired);
  clearError();operation='submit';syncControls();const {_key,...body}=confirmation,value=await api('/static-targets/'+encodeURIComponent(destination.id)+'/deployments',body,_key);current(version);
  submitted=true;markBusy();remember(value.job_id);$('deployment-status').textContent=text.submitted;await poll();
 }catch(e){if(viewVersion===version)error(e)}finally{operation='';syncControls()}
};
if(!selected.length)$('export-status').textContent=text.emptySelection;
syncControls();options().then(()=>{if(resumedJob&&/^[a-f0-9]{24}$/.test(resumedJob)){remember(resumedJob);return poll()}if(resumedPreview&&/^[a-f0-9]{24}$/.test(resumedPreview)&&$('preview-start'))return resumePreview(resumedPreview)}).catch(error);
})();
</script>
`
