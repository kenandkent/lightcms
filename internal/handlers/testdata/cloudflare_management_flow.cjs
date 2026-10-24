const fs=require('node:fs'),vm=require('node:vm'),assert=require('node:assert/strict');
const html=fs.readFileSync(0,'utf8');
const source=[...html.matchAll(/<script>([\s\S]*?)<\/script>/g)].map(m=>m[1]).find(s=>s.includes('const accounts='));
assert.ok(source,'management script missing');
const accounts=JSON.parse(source.match(/const accounts=([\s\S]*?), L=/)[1].replace(/\|\|\[\]$/,''));
const labels=JSON.parse(source.match(/, L=([\s\S]*?), openAccount=/)[1].replace(/\|\|\{\}$/,''));
const nodes=new Map();let focused=null;
class Element{
 constructor(tag='div'){this.tagName=tag.toUpperCase();this.children=[];this.listeners={};this.dataset={};this.value='';this.hidden=false;this.disabled=false;this.checked=false;this.isConnected=true;this.classList={add:()=>{}}}
 set innerHTML(_){throw Error('dynamic data must use textContent')}
 append(...children){this.children.push(...children);for(const c of children)if(c&&typeof c==='object')c.parent=this}
 replaceChildren(...c){this.children=[];this.append(...c)}
 addEventListener(name,fn){(this.listeners[name]||=[]).push(fn)}
 emit(name){for(const f of this.listeners[name]||[])f({target:this})}
 showModal(){this.open=true}
 close(){this.open=false;this.emit('close')}
 focus(){focused=this}
 cloneNode(){return Object.assign(new Element(this.tagName),{name:this.name,value:this.value,type:this.type})}
 querySelector(sel){return this.querySelectorAll(sel)[0]||null}
 querySelectorAll(sel){const all=[];const walk=n=>{for(const c of n.children){if(!c||typeof c!=='object')continue;if(sel==='input[type=checkbox]'&&c.type==='checkbox'||sel==='input[type=password]'&&c.type==='password'||sel.startsWith('button')&&c.tagName==='BUTTON'||sel==='form'&&c.tagName==='FORM'||sel==='input'&&c.tagName==='INPUT')all.push(c);walk(c)}};walk(this);return all}
 closest(sel){if(sel==='.cf-dialog')return nodes.get('cf-account-dialog');return null}
 reset(){for(const e of this.querySelectorAll('input')){e.value='';e.checked=false}}
}
for(const m of html.matchAll(/<([a-z][a-z0-9]*)\b[^>]*\bid="([^"]+)"[^>]*>/g)){const e=new Element(m[1]);e.id=m[2];nodes.set(e.id,e)}
const $=id=>{assert.ok(nodes.has(id),'missing fixture node '+id);return nodes.get(id)};
const accountButtons=accounts.map(a=>{const e=new Element('button');e.dataset.cfAccount=a.id;return e});
const deleteButtons=accounts.map(a=>{const e=new Element('button');e.dataset.cfDeleteAccount=a.id;return e});
const dialogs=[...nodes.values()].filter(e=>e.tagName==='DIALOG');
const token=new Element('input');token.name='gorilla.csrf.Token';token.value='csrf-proof';
const check=new Element('input');check.type='checkbox';$('cf-worker-delete').append(check);$('cf-worker-delete-form').append(check);
const accountCheck=new Element('input');accountCheck.type='checkbox';$('cf-delete-account-form').append(accountCheck);
for(const id of ['cf-worker-form','cf-worker-delete-form','cf-delete-account-form']){const b=new Element('button');b.type='submit';$(id).append(b)}
const docListeners={};
const document={activeElement:accountButtons[0],getElementById:id=>nodes.get(id)||null,createElement:tag=>new Element(tag),createTextNode:t=>({textContent:t}),querySelector:()=>token,querySelectorAll:sel=>sel==='.cf-dialog'?dialogs:sel==='[data-cf-account]'?accountButtons:sel==='[data-cf-delete-account]'?deleteButtons:[],addEventListener:(n,f)=>docListeners[n]=f};
vm.runInNewContext(source,{document,window:{addEventListener:()=>{}},URL,console});
accountButtons[0].onclick();assert.equal($('cf-account-dialog').open,true);assert.equal($('cf-account-title').textContent,accounts[0].name);
function collect(n){return [n,...(n.children||[]).flatMap(c=>c&&typeof c==='object'?collect(c):[])]}
let elements=collect($('cf-worker-rows'));
assert.ok(elements.some(e=>e.textContent===accounts[0].workers[0].worker_name));
assert.ok(!elements.some(e=>e.textContent===accounts[1].workers[0].worker_name),'another account leaked into Worker list');
for(const form of elements.filter(e=>e.tagName==='FORM')){assert.ok(form.children.some(e=>e.name==='gorilla.csrf.Token'&&e.value==='csrf-proof'));assert.ok(form.children.some(e=>e.name==='account_ref'&&e.value===accounts[0].id))}
elements.find(e=>e.tagName==='BUTTON'&&e.textContent===labels.edit).onclick();
assert.equal($('cf-worker-form').action,'/cm/static-delivery/targets/'+accounts[0].workers[0].id+'/update');assert.equal($('cf-worker-account').value,accounts[0].id);assert.equal($('cf-worker-revision').value,1);assert.equal($('cf-worker-name').readOnly,false);
elements.find(e=>e.tagName==='BUTTON'&&e.textContent===labels.delete).onclick();check.checked=true;
elements.find(e=>e.tagName==='BUTTON'&&e.textContent===labels.delete).onclick();assert.equal(check.checked,false,'delete consent survived opening a new action');assert.equal($('cf-worker-delete-account').value,accounts[0].id);
$('cf-account-dialog').close();assert.equal(focused,accountButtons[0],'focus not restored');
accountButtons[1].onclick();assert.equal($('cf-worker-editor').hidden,true);assert.equal($('cf-worker-delete').hidden,true);
elements=collect($('cf-worker-rows'));elements.find(e=>e.tagName==='BUTTON'&&e.textContent===labels.edit).onclick();assert.equal($('cf-worker-name').readOnly,true);assert.equal($('cf-worker-origin').readOnly,true);assert.equal($('cf-worker-environment').disabled,true);
$('cf-add-worker').onclick();assert.equal($('cf-worker-name').readOnly,false);assert.equal($('cf-worker-origin').readOnly,false);assert.equal($('cf-worker-environment').disabled,false);assert.equal($('cf-worker-form').action,'/cm/static-delivery/targets');assert.equal($('cf-worker-account').value,accounts[1].id);assert.equal($('cf-worker-environment-value').value,'test');
deleteButtons[0].onclick();accountCheck.checked=true;deleteButtons[1].onclick();assert.equal(accountCheck.checked,false);assert.equal($('cf-delete-account-form').action,'/cm/static-delivery/accounts/'+accounts[1].id+'/delete');
docListeners.submit({target:$('cf-worker-form')});assert.equal($('cf-worker-form').querySelector('button').disabled,true);
console.log('account isolation, CRUD context, CSRF, readonly boundaries, consent reset and focus restoration passed');
