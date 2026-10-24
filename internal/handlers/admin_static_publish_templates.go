package handlers

func init() {
	adminTemplates["static_delivery_publish"] = adminLayoutStart + staticPublishBody + adminLayoutEnd
}

const staticPublishBody = `
<div class="page-header"><h1>导出静态页面 / 发布到 Cloudflare</h1><a class="btn btn-secondary" href="/cm/content">返回内容管理</a></div>
<style>.static-publish{max-width:1100px}.static-publish .card{padding:24px}.static-publish h2{margin:0 0 16px}.static-publish p,.static-publish li{line-height:1.7;overflow-wrap:anywhere}.static-publish li+li{margin-top:8px}.static-publish .btn,.static-publish select{min-height:44px}.static-publish .btn{margin:4px 8px 4px 0}.static-publish input[type=checkbox]{width:18px;height:18px;vertical-align:middle;margin-right:8px}.static-publish [hidden]{display:none!important}@media(max-width:600px){.static-publish .card{padding:20px}.static-publish .btn{width:100%;white-space:normal}.static-publish ul{padding-left:20px}}</style>
<div class="static-publish">
{{if .ModuleError}}<div class="card" role="alert">{{.ModuleError}}</div>{{else}}
<section class="card"><h2>1. 已选择的文章</h2><p>只使用每篇文章当前已发布的固定版本，不包含未发布草稿，也不会重新执行原站点发布。</p><ul>{{range .Selection}}<li>{{.Title}}</li>{{end}}</ul><p id="export-status" role="status" aria-live="polite">选择发布目标后，点击预览发布将自动准备静态文件。</p><div id="downloads" hidden><a id="zip-link" class="btn btn-secondary">下载 ZIP 静态包</a><a id="html-link" class="btn btn-secondary">下载单 HTML（仅无独立依赖时可用）</a></div></section>
<button id="standalone-export" class="btn btn-secondary">仅准备下载文件（无需 Cloudflare 账号）</button>
<section class="card" style="margin-top:24px"><h2>2. 选择发布目标</h2><p>原站点与 Worker 副本独立管理：本次操作不会修改原站点的发布状态，原站点撤下也不会自动撤下 Worker 副本。</p>
{{if .CanPublish}}<div style="display:grid;grid-template-columns:repeat(auto-fit,minmax(240px,1fr));gap:20px;margin:20px 0"><label>Cloudflare 账号<select id="publish-account" style="display:block;width:100%;padding:12px"><option value="">请选择账号</option></select></label><label>Worker 目标<select id="publish-target" style="display:block;width:100%;padding:12px" disabled><option value="">请先选择账号</option></select></label></div><p id="target-details"></p><label style="display:block;margin:20px 0"><input type="checkbox" id="script-review"> 已确认报告脚本与资源不依赖原 CMS 的接口或动态资源；任意脚本仍需人工审阅</label><label style="display:block;margin:20px 0"><input type="checkbox" id="queue-mode"> 允许非冲突页面排队发布。执行时保留其他任务新增的页面；同路径已变化会失败并要求重新预览。</label><button id="preview-start" class="btn btn-primary" disabled>预览发布</button>{{else}}<p>当前账号没有内容发布权限，可以导出，但不能部署到 Worker。</p>{{end}}</section>
<section class="card" id="preview-panel" style="margin-top:24px" hidden><h2>3. 确认发布影响</h2><p id="preview-summary"></p><ul id="preview-changes"></ul><ul id="preview-warnings"></ul><label style="display:block;margin:20px 0"><input type="checkbox" id="publish-consent"> 确认以上账号、目标与页面变更；同路径文章将被替换，其他文章保留，操作不影响原站点</label><button id="deploy-start" class="btn btn-primary">确认发布</button></section>
<section class="card" style="margin-top:24px"><h2>4. 任务进度与访问链接</h2><p id="deployment-status" role="status" aria-live="polite">尚未提交发布。远端激活与公开访问校验全部成功后才显示发布成功。</p><button id="refresh-job" class="btn btn-secondary" hidden>重新读取任务状态</button><ul id="published-links"></ul><p>排队、导出与发布任务已持久化；关闭页面不会取消任务。同一目标的远端激活依次执行；启用排队后，无冲突页面会合并到最新站点。未决激活仍需先核实。</p></section>
<button id="cancel-job" class="btn btn-secondary" hidden>取消尚未激活的任务</button>
<section class="card" style="margin-top:24px"><h2>目标最近发布记录</h2><p>选择 Worker 后显示最近20次任务。点击任务可以重新查看进度，不会重复部署。</p><ul id="job-history"></ul></section>
<p id="delivery-error" role="alert" style="color:var(--danger);white-space:pre-wrap"></p>
<script>
(()=>{'use strict';
const base='/cm/static-delivery/api', selected={{.SelectedIDs}}, csrf={{.CSRFToken}}, $=id=>document.getElementById(id);
let exportID='',targets=[],target=null,preview=null,request=null,jobID='',pollTimer=null;
let exportPromise=null,exportJobID='',exportKey='';
let pendingPreview=null;
const params=new URLSearchParams(location.search);const resumedJob=params.get('job_id'),resumedPreview=params.get('preview_job_id');
if(!selected.length){$('export-status').textContent='请从内容管理勾选已发布文章后进入；也可以直接选择目标查看最近发布记录。'}
const labels={queued:'排队中',preparing:'准备静态产物',uploading:'上传完整资源及版本',version_ready:'版本已上传，尚未上线',activating:'正在激活版本',verifying:'正在验证公开页面',succeeded:'已完成',blocked:'已阻塞，请处理原因',failed:'失败',cancelled:'已取消',activation_unknown:'激活结果待核实：不会重复激活',active_unverified:'远端已激活，但公开访问校验未通过'};
function key(){return crypto.randomUUID?crypto.randomUUID():Date.now().toString(36)+'-'+Math.random().toString(36).slice(2)}
async function api(path,body,idem){const controller=new AbortController(),timer=setTimeout(()=>controller.abort(),30000);const opt={credentials:'same-origin',signal:controller.signal,headers:{'X-CSRF-Token':csrf}};const preparing=path.endsWith('/deployment-preview');if(body!==undefined){opt.method='POST';opt.headers['Content-Type']='application/json';opt.body=JSON.stringify(body);if(preparing){opt.headers.Prefer='respond-async';const signature=path+'\n'+opt.body;if(!pendingPreview||pendingPreview.signature!==signature)pendingPreview={signature,key:key()};idem=idem||pendingPreview.key}if(idem)opt.headers['Idempotency-Key']=idem}try{const r=await fetch(base+path,opt);let v;try{v=await r.json()}catch{throw Error('服务未返回JSON，HTTP '+r.status)}if(!r.ok){if(preparing&&r.status<500&&r.status!==429)pendingPreview=null;throw Error((v.code||'HTTP '+r.status)+'：'+(v.message||'操作未完成'))}if(preparing){pendingPreview=null;if(v.job_id)return await waitPreview(v.job_id)}return v}finally{clearTimeout(timer)}}
async function waitPreview(id){
 const url=new URL(location.href);url.searchParams.set('preview_job_id',id);history.replaceState(null,'',url);
 const started=Date.now();
 while(Date.now()-started<300000){
  const result=await api('/static-preview-jobs/'+encodeURIComponent(id));
  if(result.state==='succeeded'&&result.preview)return result.preview;
  if(['failed','cancelled','needs_review','partial'].includes(result.state))throw Error('预览未完成：'+(result.error_code||result.state));
  $('preview-start').textContent=result.state==='queued'?'预览排队中…':'正在生成预览…';
  await new Promise(resolve=>setTimeout(resolve,1500));
 }
 throw Error('预览等待超过5分钟，请查看后台任务状态。任务编号：'+id);
}
function error(e){$('delivery-error').textContent=e.message||String(e)}
async function ensureExport(){
 if(exportID)return;
 if(exportPromise)return exportPromise;
 exportPromise=(async()=>{
  if(!selected.length)throw Error('请先从内容管理选择已发布文章');
  $('export-status').textContent='正在准备静态导出…';
  if(!exportJobID){exportKey=exportKey||key();const v=await api('/static-exports',{items:selected.map(content_id=>({content_id}))},exportKey);exportJobID=v.job_id}
  const started=Date.now();
  while(Date.now()-started<300000){
   const data=await api('/static-deployments/'+exportJobID),j=data.job;
   $('export-status').textContent='静态文件：'+(labels[j.state]||j.state);
   if(j.state==='succeeded'){exportID=j.export_id;$('downloads').hidden=false;$('zip-link').href=base+'/static-exports/'+exportID+'/download?format=zip';$('html-link').href=base+'/static-exports/'+exportID+'/download?format=html';return}
   if(['failed','blocked','cancelled'].includes(j.state)){exportJobID='';exportKey='';throw Error('自动导出未完成：'+(j.error_code||labels[j.state]))}
   await new Promise(resolve=>setTimeout(resolve,1500));
  }
  throw Error('静态导出超过5分钟。再次点击将读取同一任务，不会重复创建。');
 })();
 try{return await exportPromise}finally{exportPromise=null}
}
$('standalone-export').disabled=!selected.length;
$('standalone-export').onclick=async()=>{if($('standalone-export').disabled)return;$('standalone-export').disabled=true;$('delivery-error').textContent='';try{await ensureExport()}catch(e){error(e)}finally{$('standalone-export').disabled=!selected.length}};
function cancellation(j){$('cancel-job').hidden=!['queued','preparing','uploading','version_ready','blocked'].includes(j.state)||!!(j.checkpoint&&j.checkpoint.activation_intent_at)}
function remember(id){jobID=id;const u=new URL(location.href);u.searchParams.set('job_id',id);u.searchParams.delete('preview_job_id');history.replaceState(null,'',u);$('refresh-job').hidden=false}
async function loadHistory(){if(!target)return;const data=await api('/static-targets/'+target.id+'/deployments?limit=20');$('job-history').replaceChildren();for(const j of data.jobs){const li=document.createElement('li'),button=document.createElement('button');button.className='btn btn-secondary';button.textContent=(labels[j.state]||j.state)+' · '+j.id;button.onclick=()=>{clearTimeout(pollTimer);remember(j.id);poll()};li.append(button);$('job-history').append(li)}}
function showPreview(value){preview=value;request=value.request||request;$('preview-summary').textContent=target.account_name+' / '+target.name+' / '+target.origin+'；本次 '+preview.changes.length+' 篇，保留其他 '+preview.preserved_pages+' 篇。';$('preview-changes').replaceChildren();for(const p of preview.changes){const li=document.createElement('li');li.textContent=(p.action==='replace'?'替换：':'新增：')+p.path+' → '+p.url;$('preview-changes').append(li)}$('preview-warnings').replaceChildren();for(const d of preview.diagnostics||[]){const li=document.createElement('li');li.textContent=d.code+'：'+d.message;$('preview-warnings').append(li)}$('preview-panel').hidden=false;request.confirmation_digest=preview.confirmation_digest;request._key=key()}
async function resumePreview(id){const job=await api('/static-preview-jobs/'+id);const selectedTarget=targets.find(t=>t.id===job.target_id);if(!selectedTarget)throw Error('原预览目标已不可用，请重新选择目标');$('publish-account').value=selectedTarget.account_ref;$('publish-account').onchange();$('publish-target').value=selectedTarget.id;$('publish-target').onchange();$('preview-start').disabled=true;const controls=[$('publish-account'),$('publish-target'),$('script-review'),$('queue-mode')];controls.forEach(el=>el.disabled=true);try{const value=await waitPreview(id);showPreview(value);$('export-status').textContent='已恢复此前的静态预览。请核对下方实际页面变更，再重新勾选发布确认。'}finally{controls.forEach(el=>el.disabled=false);$('preview-start').textContent='预览发布';$('preview-start').disabled=!target||!selected.length||targetBlocked()}}
function targetBlocked(){return !!target&&target.busy&&!($('queue-mode')&&$('queue-mode').checked)}
if($('queue-mode'))$('queue-mode').onchange=()=>{invalidate();$('preview-start').disabled=!target||!selected.length||targetBlocked()};
function invalidate(){preview=null;request=null;$('preview-panel').hidden=true;$('publish-consent').checked=false;$('deploy-start').disabled=false}
async function options(){if(!$('publish-account'))return;targets=await api('/static-delivery/options');const seen=new Set();for(const t of targets){if(seen.has(t.account_ref))continue;seen.add(t.account_ref);$('publish-account').add(new Option(t.account_name,t.account_ref))}if(!targets.length)$('target-details').textContent='没有已就绪的发布目标，请管理员先完成 Worker 初始化。'}
if($('publish-account')){
$('publish-account').onchange=()=>{invalidate();target=null;$('target-details').textContent='';$('job-history').replaceChildren();$('publish-target').replaceChildren(new Option('请选择 Worker 目标',''));for(const t of targets.filter(t=>t.account_ref===$('publish-account').value)){$('publish-target').add(new Option(t.name+' · '+t.environment+(t.busy?' · 有未决任务':''),t.id))}$('publish-target').disabled=!$('publish-account').value;$('preview-start').disabled=true;};
$('publish-target').onchange=()=>{invalidate();target=targets.find(t=>t.id===$('publish-target').value)||null;$('target-details').textContent=target?target.origin+' · '+(target.environment==='production'?'生产':'测试，不允许搜索引擎收录')+' · 当前站点版本 '+target.revision+(target.busy?' · 正有任务执行或核实中，请在最近记录查看；激活前失败会自动释放':''):'';$('preview-start').disabled=!target||!selected.length||targetBlocked();loadHistory().catch(error);};
$('script-review').onchange=invalidate;
}
async function poll(){try{
const data=await api('/static-deployments/'+jobID),j=data.job; cancellation(j);
const label=labels[j.state]||j.state;
if(j.kind==='export'){
$('export-status').textContent=label+(j.error_code?'：'+j.error_code:'');
if(j.state==='succeeded'){exportID=j.export_id;$('downloads').hidden=false;$('zip-link').href=base+'/static-exports/'+exportID+'/download?format=zip';$('html-link').href=base+'/static-exports/'+exportID+'/download?format=html';if($('preview-start'))$('preview-start').disabled=!target||targetBlocked();clearTimeout(pollTimer);return}
}else{
$('deployment-status').textContent=(j.state!=='succeeded'&&j.checkpoint&&j.checkpoint.deadline_exceeded?'已超时，远端结果待核实；暂不释放目标':j.state==='succeeded'?'发布成功，公开访问验证已通过':label)+(j.error_code?'：'+j.error_code:'');if(j.state!=='succeeded'&&j.candidate_origin)$('deployment-status').textContent+=' · 候选地址（尚未验证）：'+j.candidate_origin;$('published-links').replaceChildren();
if(j.state==='succeeded')for(const p of data.pages||[]){const li=document.createElement('li'),a=document.createElement('a');a.textContent=p.path;a.href=p.url;a.target='_blank';a.rel='noopener noreferrer';li.append(a);const copy=document.createElement('button');copy.className='btn btn-secondary';copy.textContent='复制链接';copy.onclick=async()=>{try{await navigator.clipboard.writeText(p.url);copy.textContent='已复制'}catch{error(Error('复制未完成，请复制旁边链接的地址。'))}};li.append(copy);$('published-links').append(li)}
}
if(['succeeded','failed','blocked','cancelled'].includes(j.state)){clearTimeout(pollTimer);if(j.kind!=='export'&&target){targets=await api('/static-delivery/options');const id=target.id;target=targets.find(t=>t.id===id)||null;if(target){const option=Array.from($('publish-target').options).find(o=>o.value===id);if(option)option.text=target.name+' · '+target.environment+(target.busy?' · 有未决任务':'');$('publish-target').onchange()}else{$('preview-start').disabled=true}}return}
pollTimer=setTimeout(poll,2500)
}catch(e){error(e);$('deployment-status').textContent='状态暂时读取失败。不要重复提交，可点击重新读取任务状态。'}}
$('refresh-job').onclick=()=>{clearTimeout(pollTimer);poll()};
$('cancel-job').onclick=async()=>{try{await api('/static-deployments/'+jobID+'/cancel',{},key());clearTimeout(pollTimer);await poll();await loadHistory()}catch(e){error(e)}};
if($('preview-start'))$('preview-start').onclick=async()=>{if($('preview-start').disabled)return;invalidate();$('delivery-error').textContent='';$('preview-start').disabled=true;const controls=[$('publish-account'),$('publish-target'),$('script-review'),$('queue-mode')];controls.forEach(el=>el.disabled=true);try{if(!target||!selected.length)throw Error('请先选择文章和发布目标');if(!$('script-review').checked)throw Error('请先确认脚本和资源独立性');$('preview-start').textContent='正在准备静态文件…';await ensureExport();$('preview-start').textContent='正在生成预览…';request={export_ids:[exportID],expected_revision:target.revision,expected_config_revision:target.config_revision,manual_script_review:true};if($('queue-mode').checked)request.queue_mode='merge_nonconflicting';preview=await api('/static-targets/'+target.id+'/deployment-preview',request);showPreview(preview)}catch(e){error(e)}finally{controls.forEach(el=>el.disabled=false);$('preview-start').textContent='预览发布';$('preview-start').disabled=!target||!selected.length||targetBlocked()}};
$('deploy-start').onclick=async()=>{if($('deploy-start').disabled)return;try{if(!preview||!request||!$('publish-consent').checked)throw Error('请先预览并勾选发布确认');$('deploy-start').disabled=true;const {_key,...body}=request;const v=await api('/static-targets/'+target.id+'/deployments',body,_key);remember(v.job_id);$('deployment-status').textContent='发布任务已提交';await poll()}catch(e){error(e);$('deploy-start').disabled=false}};
options().then(()=>{if(!resumedJob&&resumedPreview&&/^[a-f0-9]{24}$/.test(resumedPreview))return resumePreview(resumedPreview)}).catch(error);if(resumedJob&&/^[a-f0-9]{24}$/.test(resumedJob)){remember(resumedJob);poll()}
})();
</script>
{{end}}</div>`
