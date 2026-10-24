package handlers

import (
	"bytes"
	"encoding/json"
	"html/template"
	"net/http/httptest"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	sd "github.com/metavert-io/lightcms/v7/internal/product/staticdelivery"
	nethtml "golang.org/x/net/html"
)

func renderWorkerMaintenance(t *testing.T, lang string) string {
	t.Helper()
	tmpl, err := template.New("maintenance").Funcs(adminTemplateFuncMap).Parse(adminTemplates["delivery_maintenance"])
	if err != nil {
		t.Fatal(err)
	}
	targets := []sd.Target{
		{Record: sd.Record{ID: mustObjID(t, "000000000000000000000001")}, Name: "Alpha", WorkerName: "alpha-worker", Origin: "https://alpha.example.com", Environment: "test", Status: "ready", ConfigRevision: 2, CurrentRevision: 7, CurrentReleaseID: mustObjID(t, "000000000000000000000101")},
		{Record: sd.Record{ID: mustObjID(t, "000000000000000000000002")}, Name: "Bravo", WorkerName: "bravo-worker", Origin: "https://bravo.example.com", Environment: "production", Status: "ready", ConfigRevision: 3, CurrentRevision: 8, CurrentReleaseID: mustObjID(t, "000000000000000000000102")},
		{Record: sd.Record{ID: mustObjID(t, "000000000000000000000003")}, Name: "Disabled", WorkerName: "disabled-worker", Origin: "https://disabled.example.com", Environment: "test", Status: "disabled"},
		{Record: sd.Record{ID: mustObjID(t, "000000000000000000000004")}, Name: "Archived", WorkerName: "archived-worker", Environment: "test", Status: "archived"},
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, map[string]any{"Lang": lang, "CSRFToken": `csrf-"quote`, "MaintenanceTargets": targets, "RecoveryTargets": targets}); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

type workerDOMNode struct {
	Tag      string            `json:"tag"`
	Attrs    map[string]string `json:"attrs"`
	Text     string            `json:"text"`
	Children []*workerDOMNode  `json:"children"`
}

func workerDOMTree(n *nethtml.Node) *workerDOMNode {
	if n.Type == nethtml.TextNode {
		return &workerDOMNode{Tag: "#text", Text: n.Data}
	}
	x := &workerDOMNode{Tag: n.Data, Attrs: map[string]string{}}
	for _, a := range n.Attr {
		x.Attrs[a.Key] = a.Val
	}
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		x.Children = append(x.Children, workerDOMTree(child))
	}
	return x
}

func workerFindNode(n *nethtml.Node, id string) *nethtml.Node {
	for _, a := range n.Attr {
		if a.Key == "id" && a.Val == id {
			return n
		}
	}
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if found := workerFindNode(child, id); found != nil {
			return found
		}
	}
	return nil
}

func TestDeliveryMaintenanceListsEveryTargetAndUsesLocalizedDialog(t *testing.T) {
	for _, tc := range []struct{ lang, title, empty string }{{"zh", "Worker 页面维护", "尚未添加 Worker 发布目标"}, {"en", "Worker page maintenance", "No Worker targets yet"}} {
		t.Run(tc.lang, func(t *testing.T) {
			html := renderWorkerMaintenance(t, tc.lang)
			doc, err := nethtml.Parse(strings.NewReader(html))
			if err != nil {
				t.Fatal(err)
			}
			root := workerFindNode(doc, "worker-maintenance")
			if root == nil {
				t.Fatal("Worker maintenance needs a searchable target list")
			}
			for _, id := range []string{"maintenance-search", "maintenance-target-rows", "maintenance-list-empty", "maintenance-dialog"} {
				if workerFindNode(root, id) == nil {
					t.Fatalf("missing list or dialog control %s", id)
				}
			}
			dialog := workerFindNode(root, "maintenance-dialog")
			if dialog.Data != "dialog" {
				t.Fatal("maintenance details must retain native keyboard/focus dialog semantics")
			}
			for _, id := range []string{"maintenance-form", "maintenance-pages", "maintenance-history", "maintenance-confirmation", "recovery-confirmation"} {
				if workerFindNode(dialog, id) == nil {
					t.Fatalf("%s must be inside the target details dialog", id)
				}
			}
			var b bytes.Buffer
			_ = nethtml.Render(&b, root)
			for _, want := range []string{tc.title, tc.empty, "Alpha", "Bravo", "Disabled", "Archived"} {
				if !strings.Contains(b.String(), want) {
					t.Fatalf("missing localized list content %q", want)
				}
			}
			if tc.lang == "en" && strings.Contains(b.String(), "确认执行") {
				t.Fatal("English maintenance dialog contains untranslated action copy")
			}
		})
	}
}

func TestDeliveryMaintenanceUIBehavior(t *testing.T) {
	html := renderWorkerMaintenance(t, "en")
	doc, err := nethtml.Parse(strings.NewReader(html))
	if err != nil {
		t.Fatal(err)
	}
	root := workerFindNode(doc, "worker-maintenance")
	script := workerFindNode(doc, "worker-maintenance-script")
	if root == nil || script == nil || script.FirstChild == nil {
		t.Fatal("missing executable Worker maintenance list UI")
	}
	for _, scenario := range []string{"list_and_focus", "stale_responses", "confirmation_and_retries", "history_pagination", "operation_payloads", "page_action_controls", "recovery_retries", "recovery_completion", "recovery_resume_race", "availability_failure", "preview_resume", "preview_resume_waiting_options", "legacy_recovery_progress", "legacy_recovery_complete", "legacy_recovery_stale_open", "legacy_recovery_stale_closed"} {
		t.Run(scenario, func(t *testing.T) {
			payload, err := json.Marshal(map[string]any{"script": script.FirstChild.Data, "tree": workerDOMTree(root), "scenario": scenario})
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("node", "-e", workerMaintenanceDOMHarness)
			cmd.Stdin = bytes.NewReader(payload)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("Worker maintenance behavior: %v\n%s", err, out)
			}
		})
	}
}

const workerMaintenanceDOMHarness = `
const assert = require('node:assert/strict'), vm = require('node:vm');
const input = JSON.parse(require('node:fs').readFileSync(0, 'utf8'));
const A='000000000000000000000001', B='000000000000000000000002', D='000000000000000000000003';
const RA='000000000000000000000101', RB='000000000000000000000102', JA='000000000000000000000201', JB='000000000000000000000202';
const P='000000000000000000000301', DEP='000000000000000000000401', REC='000000000000000000000501';
const pending = () => {let resolve, reject;const promise=new Promise((a,b)=>{resolve=a;reject=b});return {promise,resolve,reject}};
const delayedOptions=pending(), staleInitialHistory=pending(), legacyRecovery=pending();let recoveryHistoryReads=0,recoverySeen=false,legacyRecoveryReads=0;
const tick = async () => {for(let i=0;i<12;i++)await new Promise(setImmediate)};
let document;
class Node {
 constructor(tag, attrs={}, text=''){this.tagName=tag.toUpperCase();this.attrs={...attrs};this.children=[];this.dataset={};this.listeners={};this.ownText=text;this.hidden='hidden' in attrs;this.disabled='disabled' in attrs;this.checked=false;this.value=attrs.value||'';this.open=false;for(const [k,v] of Object.entries(attrs))if(k.startsWith('data-'))this.dataset[k.slice(5).replace(/-([a-z])/g,(_,c)=>c.toUpperCase())]=v;}
 append(...nodes){for(const n of nodes){this.children.push(n);if(n&&typeof n==='object')n.parentNode=this}if(this.tagName==='SELECT'&&!this.value){const first=this.children.find(x=>x.tagName==='OPTION');if(first)this.value=first.value}}
 appendChild(n){this.append(n);return n} replaceChildren(...nodes){this.children=[];this.ownText='';this.append(...nodes)}
 get textContent(){return this.ownText+this.children.map(n=>typeof n==='string'?n:n.textContent).join('')} set textContent(v){this.ownText=String(v);this.children=[]}
 get options(){return this.children} add(o){this.append(o)}
 setAttribute(k,v){this.attrs[k]=String(v);if(k==='disabled')this.disabled=true} getAttribute(k){return this.attrs[k]??null} removeAttribute(k){delete this.attrs[k];if(k==='disabled')this.disabled=false}
 addEventListener(k,fn){(this.listeners[k]??=[]).push(fn)}
 async emit(k,event={}){event={target:this,preventDefault(){this.defaultPrevented=true},...event};if(this['on'+k])await this['on'+k](event);for(const fn of this.listeners[k]||[])await fn(event)}
 focus(){document.activeElement=this} showModal(){this.open=true;this.focus()} close(){this.open=false;this.emit('close')}
 matches(q){if(q.startsWith('#'))return this.attrs.id===q.slice(1);const m=q.match(/^\[([^=\]]+)(?:="([^"]*)")?\]$/);return m?(m[1] in this.attrs&&(m[2]===undefined||this.attrs[m[1]]===m[2])):this.tagName.toLowerCase()===q.toLowerCase()}
 querySelectorAll(q){const sels=q.split(',').map(s=>s.trim()), out=[];const walk=n=>{for(const c of n.children||[]){if(typeof c!=='object')continue;if(sels.some(s=>c.matches(s)))out.push(c);walk(c)}};walk(this);return out}
 querySelector(q){return this.querySelectorAll(q)[0]||null} contains(n){return n===this||this.children.some(c=>typeof c==='object'&&c.contains(n))}
}
const build = x => {const n=new Node(x.tag,x.attrs||{},x.text||'');n.append(...(x.children||[]).map(build));return n};
const root=build(input.tree);document={activeElement:null,getElementById:id=>root.attrs.id===id?root:root.querySelector('#'+id),querySelectorAll:q=>root.querySelectorAll(q),createElement:tag=>new Node(tag),addEventListener(){}};
const $=id=>document.getElementById(id), all=q=>root.querySelectorAll(q), open=id=>all('[data-maintenance-open]').find(n=>n.dataset.maintenanceOpen===id);
const requests=[], timers=new Map();let timerID=0, serial=0;
const opts=[{id:A,name:'Alpha',account_name:'Account One',origin:'https://alpha.example.com',environment:'test',revision:7,config_revision:2,busy:false},{id:B,name:'Bravo',account_name:'Account Two',origin:'https://bravo.example.com',environment:'production',revision:8,config_revision:3,busy:input.scenario==='list_and_focus'}];
const histories={ [A]:[{id:JA,state:'succeeded',release_id:RA,created_at:'2026-10-25T00:00:00Z'}], [B]:[{id:JB,state:'succeeded',release_id:RB,created_at:'2026-10-25T01:00:00Z'}]};
const preview={target_id:A,confirmation_digest:'frozen-confirmation',changes:[{action:'withdraw',path:'/alpha',url:'https://alpha.example.com/alpha'}],preserved_pages:2,diagnostics:[]};
let route = async (path,req) => {
 if(path==='/static-delivery/options')return input.scenario==='preview_resume_waiting_options'?delayedOptions.promise:opts;
 if(input.scenario.startsWith('preview_resume')&&path==='/static-preview-jobs/'+P)return {target_id:A,state:'succeeded',preview:{...preview,request:{operation:'withdraw',paths:['/alpha'],manual_script_review:true,expected_revision:7,expected_config_revision:2}}};
 if(input.scenario.startsWith('legacy_recovery')){
  if(path==='/static-recovery-jobs/'+REC){if(input.scenario.includes('stale'))return legacyRecovery.promise;return {target_id:A,job_id:REC,state:input.scenario==='legacy_recovery_progress'&&++legacyRecoveryReads===1?'running':'succeeded',delivery_job_id:DEP}}
  if(path==='/static-deployments/'+DEP)return {job:{id:DEP,target_id:A,state:'succeeded',release_id:RA,checkpoint:{remote_version_id:'restored-version'}},pages:[{path:'/alpha',publication_id:'restored-publication'}]};
 }
 if(input.scenario==='recovery_resume_race'){
  if(path==='/static-recovery-jobs/'+REC){recoverySeen=true;return {state:'succeeded',delivery_job_id:DEP}}
  if(path==='/static-deployments/'+DEP)return {job:{id:DEP,target_id:A,state:'succeeded',release_id:RB,checkpoint:{remote_version_id:'restored-version'}},pages:[{path:'/restored',publication_id:'restored-publication'}]};
  if(path.startsWith('/static-targets/'+A+'/deployments?'))return ++recoveryHistoryReads===1?staleInitialHistory.promise:{jobs:[{id:DEP,state:'succeeded',release_id:RB},...histories[A]]};
 }
 const h=path.match(/^\/static-targets\/([^/]+)\/deployments\?/);if(h)return {jobs:histories[h[1]]||[]};
 if(path.startsWith('/static-deployments/'))return {job:{state:'succeeded'},pages:[{path:path.endsWith(JB)?'/bravo':'/alpha',publication_id:'pub-one',url:'https://alpha.example.com/alpha'}]};
 throw Error('Unexpected request '+path);
};
const url=new URL('https://cms.example.com/cm/static-delivery/maintenance'+(input.scenario==='list_and_focus'?'?target_id='+B:input.scenario.startsWith('preview_resume')?'?target_id='+A+'&preview_job_id='+P:input.scenario==='recovery_resume_race'?'?target_id='+A+'&recovery_job_id='+REC:input.scenario.startsWith('legacy_recovery')?'?recovery_job_id='+REC:''));
const context={document,location:{href:url.href,search:url.search,assign:path=>{context.assigned=path}},history:{replaceState(_,__,u){context.location.href=String(u);context.location.search=new URL(u).search}},URL,URLSearchParams,AbortController,Date,Map,Set,Option:class extends Node{constructor(text,value){super('option',{value},text)}},crypto:{randomUUID:()=> 'key-'+(++serial)},setTimeout:(fn,ms)=>{const id=++timerID;timers.set(id,{fn,ms});return id},clearTimeout:id=>timers.delete(id),fetch:async (raw,req={})=>{const path=String(raw).replace('/cm/static-delivery/api','');requests.push({path,...req});const data=await route(path,req);return {ok:true,status:200,json:async()=>data}}};
vm.runInNewContext(input.script,context);
const body=r=>JSON.parse(r.body||'{}'), count=suffix=>requests.filter(r=>r.path.endsWith(suffix));
const begin = async id=>{await open(id).emit('click');await tick()};
const prepare = async path=>{$('maintenance-operation').value='withdraw';await $('maintenance-operation').emit('change');$('maintenance-paths').value=path;$('maintenance-review').checked=true;await $('maintenance-review').emit('change')};
(async()=>{
 await tick();
 if(input.scenario==='list_and_focus'){
  assert.equal($('maintenance-dialog').open,true,'target_id opens the requested Worker');assert.match($('maintenance-title').textContent,/Bravo/);assert.equal($('maintenance-preview').disabled,true,'busy Worker cannot submit maintenance');
  await $('maintenance-close').emit('click');await tick();$('maintenance-search').value='disabled.example.com';await $('maintenance-search').emit('input');assert.equal(all('[data-maintenance-row]').filter(n=>!n.hidden).length,1,'search matches public origin');
  $('maintenance-search').value='';await $('maintenance-search').emit('input');await begin(D);assert.equal($('maintenance-preview').disabled,true,'disabled targets remain readable without allowing deployment');await $('maintenance-dialog').emit('cancel');await tick();assert.equal(document.activeElement,open(D),'Escape restores focus to the action that opened details');
  $('maintenance-search').value='nothing-matches';await $('maintenance-search').emit('input');assert.equal($('maintenance-list-empty').hidden,false,'filtered empty state remains visible');
  $('maintenance-search').value='';await $('maintenance-search').emit('input');await begin(A);await $('maintenance-tab-pages').emit('keydown',{key:'ArrowRight'});assert.equal($('maintenance-tab-versions').getAttribute('aria-selected'),'true','arrow keys switch the accessible tab');assert.equal(document.activeElement,$('maintenance-tab-versions'));assert.equal($('maintenance-panel-pages').hidden,true);
 }
 if(input.scenario==='stale_responses'){
  const oldHistory=pending(), oldPreview=pending(), oldRecovery=pending();let deferredHistory=true;
  const baseline=route;route=async(path,req)=>{if(deferredHistory&&path.startsWith('/static-targets/'+A+'/deployments?'))return oldHistory.promise;if(path==='/static-targets/'+B+'/deployment-preview')return oldPreview.promise;if(path==='/static-targets/'+A+'/recovery-preview')return oldRecovery.promise;return baseline(path,req)};
  open(A).emit('click');await tick();await $('maintenance-close').emit('click');await begin(B);oldHistory.resolve({jobs:histories[A]});await tick();assert.doesNotMatch($('maintenance-history').textContent,/000000000000000000000101/,'old Worker history cannot replace current history');assert.match($('maintenance-title').textContent,/Bravo/);
  await prepare('/bravo');const previewRun=$('maintenance-form').emit('submit');await tick();await $('maintenance-close').emit('click');deferredHistory=false;await begin(A);oldPreview.resolve({...preview,target_id:B});await previewRun;await tick();assert.equal($('maintenance-confirmation').hidden,true,'old preview cannot open confirmation in another Worker');assert.equal($('maintenance-error').textContent,'');
  const recoveryRun=$('recovery-preview').emit('click');await tick();await $('maintenance-close').emit('click');await begin(B);oldRecovery.resolve({confirmation_digest:'old-proof',target:{name:'Alpha',public_origin:'https://alpha.example.com'},remote:{VersionID:'old'},release_id:RA});await recoveryRun;await tick();assert.equal($('recovery-confirmation').hidden,true,'old recovery evidence cannot appear in the next Worker');assert.equal($('recovery-proof').textContent,'');assert.equal($('maintenance-preview').disabled,false,'stale completion cannot keep the new form locked');
 }
 if(input.scenario==='confirmation_and_retries'){
  let previewAttempts=0, deployAttempts=0;const deployWait=pending(), baseline=route;
  route=async(path,req)=>{if(path==='/static-targets/'+A+'/deployment-preview'){if(++previewAttempts===1)throw Error('network response unknown');return {job_id:P}}if(path==='/static-preview-jobs/'+P)return {state:'succeeded',target_id:A,preview};if(path==='/static-targets/'+A+'/deployments'){if(++deployAttempts===1)return deployWait.promise;return {job_id:DEP}}return baseline(path,req)};
  await begin(A);await prepare('/alpha');$('maintenance-review').checked=false;await $('maintenance-form').emit('submit');assert.equal(count('/deployment-preview').length,0,'manual review is required before preparing a preview');$('maintenance-review').checked=true;
  await $('maintenance-form').emit('submit');await $('maintenance-form').emit('submit');assert.equal(count('/deployment-preview').length,2);assert.equal(count('/deployment-preview')[0].headers['Idempotency-Key'],count('/deployment-preview')[1].headers['Idempotency-Key'],'uncertain preview retries reuse the same key');assert.equal(count('/deployment-preview')[1].headers['X-CSRF-Token'],'csrf-"quote');assert.equal(count('/deployment-preview')[1].headers.Prefer,'respond-async');assert.equal($('maintenance-confirmation').hidden,false);assert.equal($('maintenance-submit').disabled,true,'preview never supplies impact consent');
  await $('maintenance-submit').emit('click');assert.equal(count('/deployments').length,0,'unchecked confirmation cannot submit');$('maintenance-consent').checked=true;await $('maintenance-consent').emit('change');const run=$('maintenance-submit').emit('click');await tick();await $('maintenance-submit').emit('click');assert.equal(count('/deployments').length,1,'double click starts one deployment');deployWait.reject(Error('deployment response unknown'));await run;await $('maintenance-submit').emit('click');assert.equal(count('/deployments').length,2);assert.equal(count('/deployments')[0].headers['Idempotency-Key'],count('/deployments')[1].headers['Idempotency-Key'],'uncertain deploy retries retain the frozen key');assert.equal(count('/deployments')[0].body,count('/deployments')[1].body);assert.equal(body(count('/deployments')[1]).confirmation_digest,'frozen-confirmation');assert.match($('maintenance-job').href,new RegExp(DEP));assert.equal($('maintenance-preview').disabled,true,'accepted deployment locks further maintenance on the Worker');
 }
 if(input.scenario==='history_pagination'){
  const baseline=route;route=async(path,req)=>{if(path.startsWith('/static-targets/'+A+'/deployments?'))return {jobs:path.includes('offset=100')?histories[A]:Array.from({length:100},(_,i)=>({id:'older-'+i,state:'failed',created_at:'2026-10-25T00:00:00Z'}))};return baseline(path,req)};
  await begin(A);assert.equal($('maintenance-history-more').hidden,false,'older history remains accessible');await $('maintenance-history-more').emit('click');await tick();assert.match($('maintenance-pages').textContent,/\/alpha/,'current release is discovered outside the first history page');
 }
 if(input.scenario==='operation_payloads'){
  const baseline=route;route=async(path,req)=>path.endsWith('/deployment-preview')?{...preview,request:body(req)}:baseline(path,req);
  await begin(A);
  const cases=[
   {operation:'withdraw',paths:['/alpha','/reports/two']},
   {operation:'rollback_pages',paths:['/alpha'],source_release_id:RA},
   {operation:'rollback_site',source_release_id:RA},
   {operation:'change_origin',new_origin:'https://reports.example.com'},
   {operation:'change_environment',new_environment:'production'}
  ];
  for(const fields of cases){
   $('maintenance-operation').value=fields.operation;await $('maintenance-operation').emit('change');assert.equal($('maintenance-origin').disabled,fields.operation!=='change_origin','an unrelated URL field cannot block another operation with native form validation');assert.equal($('maintenance-release').disabled,!fields.operation.startsWith('rollback_'));assert.equal($('maintenance-paths').disabled,!['withdraw','rollback_pages'].includes(fields.operation));$('maintenance-paths').value=(fields.paths||['/stale-path']).join('\n');$('maintenance-release').value=RA;$('maintenance-origin').value='https://reports.example.com';$('maintenance-environment').value='production';$('maintenance-review').checked=true;
   await $('maintenance-form').emit('submit');const sent=body(count('/deployment-preview').at(-1));assert.deepEqual(sent,{...fields,expected_revision:7,expected_config_revision:2,manual_script_review:true},fields.operation+' preserves revision fences and sends only relevant fields');assert.equal($('maintenance-confirmation').hidden,false);
   $('maintenance-consent').checked=true;await $('maintenance-consent').emit('change');assert.equal($('maintenance-submit').disabled,false);$('maintenance-paths').value='/changed';await $('maintenance-paths').emit('input');assert.equal($('maintenance-confirmation').hidden,true,'editing the request invalidates its frozen confirmation');assert.equal($('maintenance-submit').disabled,true);assert.equal($('maintenance-consent').checked,false);
  }
  await $('maintenance-publish').emit('click');assert.equal(context.assigned,'/cm/content','HTML publication continues through article selection');assert.equal(count('/deployments').length,0);
 }
 if(input.scenario==='recovery_retries'){
  let tries=0;const progress=pending(), baseline=route;
  route=async(path,req)=>{if(path==='/static-targets/'+A+'/recovery-preview')return {confirmation_digest:'recovery-proof',target:{id:A},remote:{VersionID:'known-version'},release_id:RA};if(path==='/static-targets/'+A+'/recover'){if(++tries===1)throw Error('response unknown');return {job_id:REC}}if(path==='/static-recovery-jobs/'+REC)return progress.promise;return baseline(path,req)};
  await begin(A);await $('recovery-preview').emit('click');assert.match($('recovery-proof').textContent,/known-version/);assert.equal($('recovery-submit').disabled,true);await $('recovery-submit').emit('click');assert.equal(count('/recover').length,0,'recovery requires its own explicit consent');
  $('recovery-consent').checked=true;await $('recovery-consent').emit('change');await $('recovery-submit').emit('click');const retry=$('recovery-submit').emit('click');await tick();assert.equal(count('/recover').length,2);assert.equal(count('/recover')[0].headers['Idempotency-Key'],'recovery-proof');assert.equal(count('/recover')[0].body,count('/recover')[1].body);assert.match($('recovery-result').href,new RegExp(REC));assert.equal(new URL(context.location.href).searchParams.get('target_id'),A);
  await $('maintenance-close').emit('click');await begin(B);progress.resolve({state:'succeeded',delivery_job_id:DEP});await retry;await tick();assert.equal($('recovery-status').textContent,'','completed recovery polling from the old Worker cannot replace current progress');assert.equal($('recovery-result').hidden,true);assert.equal($('maintenance-preview').disabled,false);
 }
 if(input.scenario==='page_action_controls'){
  await begin(A);const rollback=$('maintenance-pages').querySelectorAll('button').find(node=>node.textContent==='Roll back page');await rollback.emit('click');assert.equal($('maintenance-operation').value,'rollback_pages');assert.equal($('maintenance-release').disabled,false,'page rollback opens an editable history selector');assert.equal($('maintenance-paths').value,'/alpha');assert.equal($('maintenance-panel-actions').hidden,false);
  $('maintenance-operation').value='change_origin';await $('maintenance-operation').emit('change');const withdraw=$('maintenance-pages').querySelectorAll('button').find(node=>node.textContent==='Withdraw page');await withdraw.emit('click');assert.equal($('maintenance-operation').value,'withdraw');assert.equal($('maintenance-paths').disabled,false,'switching from a domain change to a page action restores its editable paths');assert.equal($('maintenance-origin').disabled,true);
 }
 if(input.scenario==='recovery_completion'){
  let recovered=false;const baseline=route;
  route=async(path,req)=>{if(path==='/static-targets/'+A+'/recovery-preview')return {confirmation_digest:'recovery-proof',target:{id:A},remote:{VersionID:'known-version'},release_id:RB};if(path==='/static-targets/'+A+'/recover')return {job_id:REC};if(path==='/static-recovery-jobs/'+REC){recovered=true;return {state:'succeeded',delivery_job_id:DEP}}if(path==='/static-delivery/options'&&recovered)return [{...opts[0],revision:8}];if(path==='/static-deployments/'+DEP)return {job:{id:DEP,target_id:A,state:'succeeded',release_id:RB,checkpoint:{remote_version_id:'known-version'}},pages:[{path:'/restored',publication_id:'restored-publication'}]};if(recovered&&path.startsWith('/static-targets/'+A+'/deployments?'))return {jobs:[{id:DEP,state:'succeeded',release_id:RB},...histories[A]]};if(path.endsWith('/deployment-preview'))return {...preview,request:body(req)};return baseline(path,req)};
  await begin(A);await $('recovery-preview').emit('click');$('recovery-consent').checked=true;await $('recovery-consent').emit('change');await $('recovery-submit').emit('click');await tick();assert.match($('recovery-result').href,new RegExp(DEP));assert.equal($('maintenance-current-release').textContent,RB,'recovered release replaces the old current-version summary');assert.match($('maintenance-pages').textContent,/\/restored/,'recovery refreshes page records instead of keeping the old release on screen');await prepare('/alpha');await $('maintenance-form').emit('submit');assert.equal(body(count('/deployment-preview').at(-1)).expected_revision,8,'successful recovery refreshes the incremented revision before more maintenance');
 }
 if(input.scenario==='availability_failure'){
  const baseline=route;route=async(path,req)=>{if(path==='/static-delivery/options')throw Error('availability offline');return baseline(path,req)};
  await $('maintenance-reload').emit('click');await begin(A);assert.match($('maintenance-list-error').textContent,/availability offline/);assert.equal($('maintenance-preview').disabled,true);assert.equal($('maintenance-publish').disabled,true);assert.equal($('recovery-preview').disabled,true,'failed readiness reads also block recovery until the state can be checked');assert.match($('maintenance-pages').textContent,/\/alpha/,'read-only page history stays available');
  route=baseline;await $('maintenance-reload').emit('click');assert.equal($('maintenance-preview').disabled,false,'retrying availability re-enables ready targets');
 }
 if(input.scenario==='recovery_resume_race'){
  assert.match($('maintenance-pages').textContent,/\/restored/,'resumed recovery refreshes its history even while the original history request is pending');staleInitialHistory.resolve({jobs:histories[A]});await tick();assert.match($('maintenance-pages').textContent,/\/restored/,'the earlier history response cannot replace recovered page records');assert.ok($('maintenance-history').querySelectorAll('a').some(node=>node.href.endsWith(DEP)),'the recovered task remains in refreshed history');
 }
 if(input.scenario.startsWith('preview_resume')){
  assert.equal($('maintenance-dialog').open,true);assert.equal($('maintenance-confirmation').hidden,false,'saved asynchronous preview resumes in its Worker dialog');assert.equal($('maintenance-consent').checked,false);assert.equal($('maintenance-submit').disabled,true,'resuming a preview never restores impact consent');assert.equal($('maintenance-paths').value,'/alpha');assert.equal(count('/deployment-preview').length,0,'resuming reads the existing preparation task');
  if(input.scenario==='preview_resume_waiting_options'){assert.equal($('maintenance-preview').disabled,true);delayedOptions.resolve(opts);await tick();assert.equal($('maintenance-confirmation').hidden,false,'slow availability reads cannot discard an otherwise valid saved preview');assert.equal($('maintenance-error').textContent,'');}
 }
 if(input.scenario==='legacy_recovery_progress'||input.scenario==='legacy_recovery_complete'){
  assert.equal($('maintenance-dialog').open,true,'recovery_job_id alone opens the recovery task Worker');assert.match($('maintenance-title').textContent,/Alpha/);assert.equal(new URL(context.location.href).searchParams.get('target_id'),A,'the recovered Worker is included in subsequent share links');
  if(input.scenario==='legacy_recovery_progress'){assert.match($('recovery-status').textContent,/Running/);const next=Array.from(timers.values()).find(timer=>timer.ms===2500);assert.ok(next,'the legacy recovery link continues polling its durable task');await next.fn();await tick()}
  assert.match($('recovery-result').href,new RegExp('/cm/static-delivery/publish\\?job_id='+DEP),'completed legacy recovery reaches the verified deployment links');assert.equal($('recovery-result').hidden,false);assert.equal($('maintenance-error').textContent,'');assert.equal($('maintenance-resumed-recovery').hidden,true,'successful lookup does not send the user back into a task-center loop');
 }
 if(input.scenario.startsWith('legacy_recovery_stale')){
  assert.equal(count('/static-recovery-jobs/'+REC).length,1,'legacy task discovery starts with an authorized metadata read');await begin(B);if(input.scenario.endsWith('closed'))await $('maintenance-close').emit('click');legacyRecovery.resolve({target_id:A,job_id:REC,state:'succeeded',delivery_job_id:DEP});await tick();
  assert.equal($('maintenance-dialog').open,!input.scenario.endsWith('closed'),'late legacy discovery cannot reopen a closed dialog');assert.match($('maintenance-title').textContent,/Bravo/,'late legacy discovery cannot switch the current Worker');assert.equal($('recovery-result').hidden,true);assert.equal($('maintenance-error').textContent,'');
 }
})().catch(e=>{console.error(e);process.exitCode=1});
`

func TestDeliveryMaintenanceHasExplicitPreviewConfirmation(t *testing.T) {
	h := &Handler{}
	w := httptest.NewRecorder()
	h.AdminDeliveryMaintenance(w, httptest.NewRequest("GET", "/cm/static-delivery/maintenance", nil))
	if w.Code != 403 {
		t.Fatalf("unauthenticated: %d", w.Code)
	}
	tmpl, err := template.New("maintenance").Funcs(adminTemplateFuncMap).Parse(adminTemplates["delivery_maintenance"])
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err = tmpl.Execute(&out, map[string]any{"Lang": "zh", "CSRFToken": "csrf"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"rollback_pages", "rollback_site", "withdraw", "change_origin", "change_environment", "confirmation_digest", "respond-async"} {
		if !strings.Contains(out.String(), want) {
			t.Fatal("missing lifecycle action", want)
		}
	}
	for _, script := range regexp.MustCompile(`(?s)<script(?: [^>]*)?>(.*?)</script>`).FindAllStringSubmatch(out.String(), -1) {
		cmd := exec.Command("node", "--check")
		cmd.Stdin = strings.NewReader(script[1])
		if result, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("invalid script: %v %s", err, result)
		}
	}
}
