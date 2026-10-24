package handlers

import "net/http"

func (h *Handler) AdminDeliveryMaintenance(w http.ResponseWriter, r *http.Request) {
	if !h.deliveryAdminAllowed(w, r) {
		return
	}
	if h.staticDelivery == nil || h.staticDelivery.Commands == nil || h.staticDelivery.Targets == nil {
		http.Error(w, "静态发布暂不可用", 503)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	targets, err := h.staticDelivery.Targets.List(r.Context())
	if err != nil {
		http.Error(w, "无法读取发布目标", 503)
		return
	}
	h.renderAdmin(w, r, "delivery_maintenance", map[string]any{"RecoveryTargets": targets})
}
func init() {
	adminTemplates["delivery_maintenance"] = adminLayoutStart + `
<div class="page-header"><h1>Worker 页面维护</h1><a class="btn btn-secondary" href="/cm/static-delivery">账号与目标</a></div>
<div style="max-width:1000px"><p>这些操作只改变 Worker 副本，不改变原站点文章。回滚使用已保存的历史产物，不套用今天的模板。每次操作都需要先预览，再确认。</p>
<section class="card" style="padding:24px"><h2>1. 选择目标与操作</h2>
<form id="maintenance-form"><p><label>Worker 目标 <select id="maintenance-target" required style="max-width:100%;min-height:44px"><option value="">请选择目标</option></select></label></p>
<p><label>操作 <select id="maintenance-operation" style="min-height:44px"><option value="withdraw">撤下指定页面</option><option value="rollback_pages">回滚指定页面</option><option value="rollback_site">回滚整个站点</option><option value="change_origin">更换公开访问域名</option><option value="change_environment">切换测试／生产环境</option></select></label></p>
<p id="maintenance-paths-field"><label>页面路径（每行一个，最多100个）<textarea id="maintenance-paths" rows="4" placeholder="/reports/example" style="display:block;width:100%;margin-top:8px"></textarea></label></p>
<p id="maintenance-history-field" hidden><label>历史版本 <select id="maintenance-release" style="max-width:100%;min-height:44px"><option value="">请选择已成功发布的版本</option></select></label></p>
<p id="maintenance-origin-field" hidden><label>新的 HTTPS 公开地址 <input id="maintenance-origin" type="url" placeholder="https://reports.example.com"></label><br>请先在 Cloudflare 将域名绑定到同一个 Worker。本系统会核实归属，不修改 DNS。</p>
<p id="maintenance-environment-field" hidden><label>环境 <select id="maintenance-environment"><option value="test">测试（禁止收录）</option><option value="production">生产（允许收录）</option></select></label></p>
<p><label><input id="maintenance-review" type="checkbox" required> 已确认静态脚本与资源独立；理解撤下／回滚将改变对外可见内容</label></p><button id="maintenance-preview" class="btn btn-primary">预览变更</button></form></section>
<section id="maintenance-confirmation" class="card" style="padding:24px;margin-top:24px" hidden><h2>2. 核对变更</h2><p id="maintenance-summary"></p><ul id="maintenance-changes"></ul><p><label><input type="checkbox" id="maintenance-consent"> 确认以上目标、操作和全部页面变更</label></p><button id="maintenance-submit" class="btn btn-primary">确认执行</button></section>
<p id="maintenance-status" role="status" aria-live="polite"></p><p id="maintenance-error" role="alert"></p><a id="maintenance-job" class="btn btn-secondary" hidden>查看发布任务与验证后的访问链接</a>
<details class="card" style="padding:24px;margin-top:24px"><summary>核实并恢复本地与远端版本记录</summary><p>仅接受同一专用 Worker 上、本系统曾上传且可以完整验证的版本。不上传、不激活、不接管未知站点；有未决激活时必须先核实原任务。</p><label>目标 <select id="recovery-target"><option value="">请选择目标</option>{{range .RecoveryTargets}}<option value="{{.ID.Hex}}">{{.Name}} · {{.Status}}</option>{{end}}</select></label><button class="btn btn-secondary" id="recovery-preview">预览恢复证据</button><p id="recovery-proof"></p><div id="recovery-confirmation" hidden><label><input type="checkbox" id="recovery-consent"> 确认重新验证以上远端版本并更新本地记录</label><button class="btn btn-secondary" id="recovery-submit">确认核实与恢复</button></div><p id="recovery-status" role="status" aria-live="polite"></p><a id="recovery-result" class="btn btn-secondary" hidden>查看恢复任务</a></details></div>
<script>
(()=>{'use strict';const base='/cm/static-delivery/api',csrf={{.CSRFToken}},$=id=>document.getElementById(id);let targets=[],frozen=null,busy=false,previewJob='',previewKey='',fingerprint='';
const uuid=()=>crypto.randomUUID();
async function api(path,body,key){const ctl=new AbortController(),timer=setTimeout(()=>ctl.abort(),30000);try{const response=await fetch(base+path,{method:body?'POST':'GET',credentials:'same-origin',signal:ctl.signal,headers:{'Content-Type':'application/json','X-CSRF-Token':csrf,'Idempotency-Key':key||'','Prefer':'respond-async'},body:body?JSON.stringify(body):undefined});const data=await response.json();if(!response.ok)throw Error((data.code||response.status)+'：'+(data.message||'操作未完成'));return data}finally{clearTimeout(timer)}}
function fail(e){$('maintenance-error').textContent=e.message||String(e)}
function reset(){frozen=null;$('maintenance-confirmation').hidden=true;$('maintenance-consent').checked=false}
function fields(){reset();const op=$('maintenance-operation').value;$('maintenance-paths-field').hidden=!['withdraw','rollback_pages'].includes(op);$('maintenance-history-field').hidden=!op.startsWith('rollback_');$('maintenance-origin-field').hidden=op!=='change_origin';$('maintenance-environment-field').hidden=op!=='change_environment'}
function lock(value){busy=value;for(const id of ['maintenance-target','maintenance-operation','maintenance-paths','maintenance-release','maintenance-origin','maintenance-environment','maintenance-review','maintenance-preview'])$(id).disabled=value}
$('maintenance-operation').onchange=fields;
for(const id of ['maintenance-paths','maintenance-release','maintenance-origin','maintenance-environment','maintenance-review'])$(id).onchange=reset;
$('maintenance-target').onchange=async()=>{reset();$('maintenance-release').replaceChildren(new Option('请选择已成功发布的版本',''));const id=$('maintenance-target').value;if(!id)return;try{const history=await api('/static-targets/'+id+'/deployments?limit=100');if($('maintenance-target').value!==id)return;const seen=new Set();for(const job of history.jobs||[]){if(job.state==='succeeded'&&job.release_id&&!seen.has(job.release_id)){seen.add(job.release_id);$('maintenance-release').add(new Option(job.release_id+' · '+job.created_at,job.release_id))}}}catch(e){fail(e)}};
$('maintenance-form').onsubmit=async event=>{event.preventDefault();if(busy)return;reset();lock(true);$('maintenance-error').textContent='';try{
 const target=targets.find(t=>t.id===$('maintenance-target').value);if(!target)throw Error('请选择发布目标');if(!$('maintenance-review').checked)throw Error('请确认静态资源与操作影响');
 const operation=$('maintenance-operation').value,req={operation,expected_revision:target.revision,expected_config_revision:target.config_revision,manual_script_review:true};
 if(['withdraw','rollback_pages'].includes(operation))req.paths=$('maintenance-paths').value.split(/\r?\n/).map(s=>s.trim()).filter(Boolean);
 if(operation.startsWith('rollback_'))req.source_release_id=$('maintenance-release').value;
 if(operation==='change_origin')req.new_origin=$('maintenance-origin').value.trim();if(operation==='change_environment')req.new_environment=$('maintenance-environment').value;
 const next=JSON.stringify([target.id,req]);if(fingerprint!==next){fingerprint=next;previewJob='';previewKey=uuid()}
 $('maintenance-status').textContent='正在准备预览，不会执行远端发布…';let result=previewJob?{job_id:previewJob}:await api('/static-targets/'+target.id+'/deployment-preview',req,previewKey);
 if(result.job_id){previewJob=result.job_id;const start=Date.now();while(true){result=await api('/static-preview-jobs/'+previewJob);if(result.state==='succeeded'){result=result.preview;previewJob='';previewKey='';fingerprint='';break}if(['failed','cancelled','partial','needs_review'].includes(result.state)){previewJob='';previewKey='';fingerprint='';throw Error('预览失败：'+(result.error_code||result.state))}if(Date.now()-start>300000)throw Error('预览仍未完成，再次点击将读取同一任务。');await new Promise(resolve=>setTimeout(resolve,1500))}}
 frozen={target:target.id,body:{...req,confirmation_digest:result.confirmation_digest},key:uuid()};$('maintenance-summary').textContent=target.account_name+' / '+target.name+' / '+operation+'；变更 '+result.changes.length+' 页，保留 '+result.preserved_pages+' 页。';$('maintenance-changes').replaceChildren();for(const change of result.changes){const li=document.createElement('li');li.textContent=change.action+' · '+change.path+' → '+change.url;$('maintenance-changes').append(li)}$('maintenance-confirmation').hidden=false;$('maintenance-status').textContent='预览完成，请核对后确认。';$('maintenance-submit').disabled=false;
 }catch(e){fail(e)}finally{lock(false)}};
$('maintenance-submit').onclick=async()=>{if(!frozen||$('maintenance-submit').disabled)return;if(!$('maintenance-consent').checked){fail(Error('请勾选确认'));return}$('maintenance-submit').disabled=true;try{const job=await api('/static-targets/'+frozen.target+'/deployments',frozen.body,frozen.key);$('maintenance-status').textContent='任务已提交，只有公开访问验证通过后才算成功。';$('maintenance-job').href='/cm/static-delivery/publish?job_id='+encodeURIComponent(job.job_id);$('maintenance-job').hidden=false;reset()}catch(e){fail(e);$('maintenance-submit').disabled=false}};
let recoveryProof=null,recoveryID='',recoveryBusy=false;
$('recovery-target').onchange=()=>{recoveryProof=null;$('recovery-confirmation').hidden=true;$('recovery-consent').checked=false};
$('recovery-preview').onclick=async()=>{if(recoveryBusy)return;recoveryBusy=true;$('recovery-preview').disabled=true;recoveryProof=null;$('recovery-confirmation').hidden=true;$('recovery-consent').checked=false;try{const id=$('recovery-target').value;if(!id)throw Error('请选择恢复目标');const proof=await api('/static-targets/'+id+'/recovery-preview',{},uuid());if($('recovery-target').value!==id)return;recoveryProof={id,digest:proof.confirmation_digest};$('recovery-proof').textContent=proof.target.name+' / '+proof.target.public_origin+'；已知远端版本 '+proof.remote.VersionID+'；来源 release '+proof.release_id+'；确认后还会验证全部公开文件。';$('recovery-confirmation').hidden=false}catch(e){fail(e)}finally{recoveryBusy=false;$('recovery-preview').disabled=false}};
async function pollRecovery(){try{const result=await api('/static-recovery-jobs/'+recoveryID);$('recovery-status').textContent=result.state+(result.error_code?'：'+result.error_code:'');if(result.state==='succeeded'){$('recovery-result').href='/cm/static-delivery/publish?job_id='+result.delivery_job_id;$('recovery-result').textContent='恢复完成，查看已验证访问链接';return}if(['failed','cancelled','needs_review','partial'].includes(result.state))return;setTimeout(pollRecovery,2500)}catch(e){fail(e);$('recovery-status').textContent='状态读取中断。可刷新页面继续查看同一恢复任务。'}}
$('recovery-submit').onclick=async()=>{if(recoveryBusy||!recoveryProof)return;if(!$('recovery-consent').checked){fail(Error('请勾选恢复确认'));return}recoveryBusy=true;$('recovery-submit').disabled=true;try{const result=await api('/static-targets/'+recoveryProof.id+'/recover',{confirmation_digest:recoveryProof.digest,confirmed:true},recoveryProof.digest);recoveryID=result.job_id;const url=new URL(location.href);url.searchParams.set('recovery_job_id',recoveryID);history.replaceState(null,'',url);$('recovery-result').href='/cm/tasks?source=workflow&job_id='+recoveryID;$('recovery-result').hidden=false;$('recovery-confirmation').hidden=true;await pollRecovery()}catch(e){fail(e)}finally{recoveryBusy=false;$('recovery-submit').disabled=false}};
const resumed=new URLSearchParams(location.search).get('recovery_job_id');if(resumed&&/^[a-f0-9]{24}$/.test(resumed)){recoveryID=resumed;$('recovery-result').href='/cm/tasks?source=workflow&job_id='+recoveryID;$('recovery-result').hidden=false;pollRecovery()}
api('/static-delivery/options').then(rows=>{targets=rows;for(const t of rows)$('maintenance-target').add(new Option(t.account_name+' / '+t.name+' · '+t.origin,t.id))}).catch(fail);fields();
})();
</script>
` + adminLayoutEnd
}
