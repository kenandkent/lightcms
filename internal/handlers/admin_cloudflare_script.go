package handlers

const cloudflareManagementScript = `
<script>
(() => {
 'use strict';
 const accounts={{.Accounts}}||[], L={{.CF}}||{}, openAccount={{.OpenAccount}}, $=id=>document.getElementById(id);
 if (!$('cf-account-dialog')) return;
 let current=null, opener=null;
 const zero='000000000000000000000000', base='/cm/static-delivery/';
 const button=(value,handler)=>{const b=document.createElement('button');b.type='button';b.className='btn btn-secondary';b.textContent=value;b.onclick=handler;return b};
 const text=(tag,value,cls)=>{const e=document.createElement(tag);e.textContent=value;if(cls)e.className=cls;return e};
 function open(dialog,source){opener=source||document.activeElement;dialog.showModal()}
 for(const dialog of document.querySelectorAll('.cf-dialog')){
  dialog.addEventListener('close',()=>{if(opener&&opener.isConnected)opener.focus();for(const input of dialog.querySelectorAll('input[type=password]'))input.value=''});
  dialog.addEventListener('click',event=>{if(event.target.closest('[data-cf-close]'))dialog.close()});
 }
 $('cf-add-account').onclick=event=>open($('cf-add-account-dialog'),event.currentTarget);
 $('cf-account-search').oninput=()=>{const q=$('cf-account-search').value.toLowerCase().trim();let visible=0;for(const row of document.querySelectorAll('[data-cf-account-row]')){row.hidden=!row.dataset.search.toLowerCase().includes(q);if(!row.hidden)visible++}$('cf-account-no-match').hidden=visible>0||accounts.length===0};
 function hideEditors(){$('cf-worker-editor').hidden=true;$('cf-worker-delete').hidden=true}
 function actionForm(target,action,label,consent){
  const f=document.createElement('form');f.method='post';f.action=base+'targets/'+target.id+'/'+action;
  const token=document.querySelector('input[name="gorilla.csrf.Token"]');if(token)f.append(token.cloneNode(true));
  const account=document.createElement('input');account.type='hidden';account.name='account_ref';account.value=current.id;f.append(account);
  if(consent){const check=document.createElement('input');check.type='checkbox';check.name='confirm_initialize';check.value='yes';check.required=true;const labelNode=text('label','', 'cf-consent');labelNode.append(check,document.createTextNode(L.initializeHelp));f.append(labelNode)}
  const submit=document.createElement('button');submit.className='btn btn-secondary';submit.textContent=label;f.append(submit);return f;
 }
 function renderWorkers(){
  const body=$('cf-worker-rows');body.replaceChildren();const q=$('cf-worker-search').value.toLowerCase().trim();
  const rows=(current.workers||[]).filter(w=>(w.name+' '+w.worker_name+' '+w.public_origin).toLowerCase().includes(q));
  if(!rows.length){const tr=document.createElement('tr'),td=text('td',q?L.noMatch:L.emptyWorkers,'cf-empty');td.colSpan=5;tr.append(td);body.append(tr);return}
  for(const worker of rows){
   const tr=document.createElement('tr'),identity=document.createElement('td');identity.append(text('strong',worker.name),text('span',worker.worker_name,'cf-id'));tr.append(identity);
   const origin=document.createElement('td');if(worker.public_origin){try{const url=new URL(worker.public_origin);if(url.protocol==='https:'){const a=text('a',worker.public_origin);a.href=url.href;a.target='_blank';a.rel='noopener noreferrer';origin.append(a)}else origin.textContent=worker.public_origin}catch(_){origin.textContent=worker.public_origin}}else origin.textContent='—';tr.append(origin,text('td',L[worker.environment]||worker.environment));
   const status=document.createElement('td');status.append(text('span',L[worker.status]||worker.status,'cf-status'+(worker.status==='ready'?' cf-status-ready':'')),text('span',L.siteRevision+' '+worker.current_revision,'cf-id'));tr.append(status);
   const actions=document.createElement('td'),group=document.createElement('div');group.className='cf-actions';const busy=worker.status==='initializing'||(worker.active_job_id&&worker.active_job_id!==zero);
   const edit=button(L.edit,()=>editWorker(worker));edit.disabled=busy||worker.status==='archived';const remove=button(L.delete,()=>deleteWorker(worker));remove.disabled=busy;remove.classList.add('cf-danger');group.append(edit,remove);
   const more=document.createElement('details');more.className='cf-row-options';more.append(text('summary',L.more));
   if(worker.status==='draft'||worker.status==='initializing')more.append(actionForm(worker,'initialize',L.initialize,true));
   else if(worker.status==='archived')more.append(actionForm(worker,'restore',L.restore));
   else {more.append(actionForm(worker,'verify',L.verifyWorker),actionForm(worker,worker.status==='disabled'?'enable':'disable',worker.status==='disabled'?L.enable:L.disable))}
   if(worker.status!=='draft'){const maintenance=text('a',L.maintenance,'btn btn-secondary');maintenance.href=base+'maintenance?target_id='+encodeURIComponent(worker.id);more.append(maintenance)}
   actions.append(group,more);if(busy)actions.append(text('span',L.busy,'cf-id'));tr.append(actions);body.append(tr);
  }
 }
 function showAccount(account,source){
  current=account;hideEditors();$('cf-worker-search').value='';$('cf-account-title').textContent=account.name;$('cf-account-id').textContent=account.account_id;renderWorkers();
  $('cf-add-worker').disabled=account.status==='archived';$('cf-account-settings').open=false;
  const check=account.verification||{};$('cf-credential-status').textContent=L.revision+' '+account.credential_revision+' · '+(check.readable?L.readable:L.unverified)+' · '+(check.write_verified?L.writeVerified:L.writeUnverified);
  $('cf-account-verify').action=base+'accounts/'+account.id+'/verify';$('cf-account-verify').reset();$('cf-account-toggle').action=base+'accounts/'+account.id+'/'+(account.status==='archived'?'restore':account.status==='enabled'?'disable':'enable');$('cf-account-toggle-label').textContent=account.status==='archived'?L.restore:account.status==='enabled'?L.disable:L.enable;
  $('cf-account-rotate').action=base+'accounts/'+account.id+'/rotate';$('cf-account-rotate').reset();$('cf-account-rotate').hidden=account.status==='archived';
  open($('cf-account-dialog'),source);
 }
 function editWorker(worker){
  hideEditors();const form=$('cf-worker-form');form.reset();form.action=base+'targets'+(worker?'/'+worker.id+'/update':'');$('cf-worker-account').value=current.id;$('cf-worker-revision').value=worker?worker.config_revision:'';$('cf-worker-management-revision').value=worker?worker.management_revision||0:'';
  $('cf-worker-display-name').value=worker?worker.name:'';$('cf-worker-name').value=worker?worker.worker_name:'';$('cf-worker-name').readOnly=!!worker&&worker.status!=='draft';
  const published=!!worker&&(worker.current_revision>0||worker.current_release_id&&worker.current_release_id!==zero);$('cf-worker-origin').value=worker?worker.public_origin:'';$('cf-worker-origin').readOnly=!!published;
  $('cf-worker-environment').value=worker?worker.environment:'test';$('cf-worker-environment').disabled=!!published;$('cf-worker-environment-value').value=$('cf-worker-environment').value;
  $('cf-worker-help').textContent=worker?L.editHelp:L.draftHelp;$('cf-worker-editor-title').textContent=worker?L.editWorker:L.addWorker;$('cf-worker-editor').hidden=false;$('cf-worker-display-name').focus();
 }
 function deleteWorker(worker){hideEditors();$('cf-worker-delete-form').reset();$('cf-worker-delete-form').action=base+'targets/'+worker.id+'/delete';$('cf-worker-delete-account').value=current.id;$('cf-worker-delete-name').textContent=worker.name+' / '+worker.worker_name;$('cf-worker-delete').hidden=false;$('cf-worker-delete').querySelector('input[type=checkbox]').focus()}
 $('cf-worker-environment').onchange=()=>{$('cf-worker-environment-value').value=$('cf-worker-environment').value};$('cf-worker-search').oninput=renderWorkers;$('cf-add-worker').onclick=()=>editWorker(null);$('cf-worker-cancel').onclick=hideEditors;$('cf-worker-delete-cancel').onclick=hideEditors;
 for(const b of document.querySelectorAll('[data-cf-account]'))b.onclick=()=>{const account=accounts.find(a=>a.id===b.dataset.cfAccount);if(account)showAccount(account,b)};
 for(const b of document.querySelectorAll('[data-cf-delete-account]'))b.onclick=()=>{const account=accounts.find(a=>a.id===b.dataset.cfDeleteAccount);if(!account)return;$('cf-delete-account-name').textContent=account.name+' / '+account.account_id;const f=$('cf-delete-account-form');f.reset();f.action=base+'accounts/'+account.id+'/delete';open($('cf-delete-account-dialog'),b)};
 document.addEventListener('submit',event=>{const form=event.target;if(!form.closest('.cf-dialog'))return;for(const b of form.querySelectorAll('button:not([type=button])'))b.disabled=true});
 window.addEventListener('pageshow',()=>{for(const b of document.querySelectorAll('.cf-dialog button:disabled'))if(b.type==='submit')b.disabled=false});
 const initial=accounts.find(a=>a.id===openAccount);if(initial)showAccount(initial);
})();
</script>
`
