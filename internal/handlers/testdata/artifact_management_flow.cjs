const {readFileSync} = require('node:fs');
const vm = require('node:vm');
const assert = require('node:assert/strict');
const html = readFileSync(0, 'utf8');
const script = [...html.matchAll(/<script>([\s\S]*?)<\/script>/g)].map(x => x[1]).find(x => x.includes("const root = document.getElementById('artifact-management')"));
assert(script, 'the artifact script must be rendered');

const nodes = new Map();
let document;
class Element {
  constructor(id = '') {
    this.id = id; this.children = []; this.listeners = new Map(); this.dataset = {};
    this.hidden = false; this.disabled = false; this.checked = false; this.required = false;
    this.value = ''; this.textContent = ''; this.isConnected = true; this.open = false;
    this.buttons = []; this.checkboxes = []; this.attrs = {};
  }
  append(...children) { this.children.push(...children); }
  remove() { this.removed = true; }
  replaceChildren(...children) { this.children = children; }
  addEventListener(name, fn) { if (!this.listeners.has(name)) this.listeners.set(name, []); this.listeners.get(name).push(fn); }
  async fire(name, event = {}) {
    event.target ||= this;
    event.preventDefault ||= () => { event.prevented = true; };
    for (const fn of this.listeners.get(name) || []) await fn(event);
    return event;
  }
  querySelectorAll(selector) {
    if (selector === 'input[type="checkbox"]') return this.checkboxes;
    if (selector === '[data-artifact-close]') return this.closeButtons || [];
    if (selector.startsWith('button')) return this.buttons;
    return [];
  }
  setAttribute(key, value) { this.attrs[key] = value; }
  removeAttribute(key) { delete this.attrs[key]; }
  checkValidity() { return !this.consent || !this.consent.required || this.consent.checked; }
  reportValidity() { this.reportedInvalid = true; }
  focus() { document.activeElement = this; }
  showModal() { this.open = true; }
  close() { this.open = false; void this.fire('close'); }
}
for (const match of html.matchAll(/id="([^"]+)"/g)) nodes.set(match[1], new Element(match[1]));
const $ = id => nodes.get(id);
const detail = $('artifact-detail-dialog'), cleanup = $('artifact-cleanup-dialog'), resume = $('artifact-resume-dialog');
const dialogs = [detail, cleanup, resume];
for (const dialog of dialogs) dialog.closeButtons = [new Element()];
detail.checkboxes = [$('artifact-unpin-confirm')];
const pinForm = $('artifact-pin-form');
pinForm.consent = $('artifact-unpin-confirm');
pinForm.buttons = [$('artifact-pin-submit')];
const previewForm = new Element(), cleanupForm = new Element();
previewForm.buttons = [new Element()]; cleanupForm.buttons = [new Element()];
const manualForm = new Element(), manualPin = new Element();
manualPin.name = 'action'; manualPin.value = 'pin'; manualForm.buttons = [manualPin];
const forms = [pinForm, previewForm, cleanupForm, manualForm];
const openResume = new Element(); openResume.dataset.artifactOpen = 'artifact-resume-dialog';
const detailButtons = [];
for (const match of html.matchAll(/<button[^>]*data-artifact-detail="([^"]+)"[^>]*>/g)) {
  const button = new Element(); button.dataset.artifactDetail = match[1];
  if (match[0].includes('data-artifact-focus="pin"')) button.dataset.artifactFocus = 'pin';
  detailButtons.push(button);
}
const firstHash = 'a'.repeat(64), secondHash = 'd'.repeat(64);
const firstButton = detailButtons.find(x => x.dataset.artifactDetail === firstHash);
const secondButton = detailButtons.find(x => x.dataset.artifactDetail === secondHash);
assert(firstButton && secondButton, 'fixture must render multiple real artifact rows');
const requests = [], windowEvents = new Map();
document = {
  activeElement: firstButton,
  getElementById: id => nodes.get(id),
  createElement: () => new Element(),
  querySelectorAll(selector) {
    if (selector === '.artifact-dialog') return dialogs;
    if (selector === '.artifact-dialog[data-open-on-load]') return [];
    if (selector === '[data-artifact-open]') return [openResume];
    if (selector === '[data-artifact-detail]') return detailButtons;
    if (selector === '[data-artifact-form]') return forms;
    throw Error('unexpected selector ' + selector);
  }
};
vm.runInNewContext(script, {
  document, window: {addEventListener: (name, fn) => windowEvents.set(name, fn)},
  URL, AbortController, location: {href: 'https://cms.example/cm/artifacts'},
  setTimeout: () => 1, clearTimeout() {},
  fetch: (url, options) => new Promise(resolve => requests.push({url, options, resolve})),
});
function reply(index, hash, pinned, extra = {}) {
  const request = requests[index];
  assert.match(request.url, /format=json/);
  assert.equal(request.options.credentials, 'same-origin');
  request.resolve({ok: true, json: async () => ({
    hash, pinned, state: 'available', size: 123, mime: 'text/html', created_at: '2026-10-24T08:00:00Z',
    retention_until: '2027-01-22T08:00:00Z', reference_count: 1,
    protection_reasons: ['referenced'], references: [{kind: 'current_release', id: '000000000000000000000001'}],
    ...extra,
  })});
}

(async () => {
  // Deliberately resolve a request after aborting it: the request identity must
  // still fence the result even if a network/client ignores AbortSignal.
  const first = firstButton.fire('click');
  assert.equal(detail.open, true);
  assert.equal($('artifact-detail-content').hidden, true);
  const second = secondButton.fire('click');
  assert.equal(requests[0].options.signal.aborted, true);
  reply(1, secondHash, false);
  await second;
  assert.equal($('artifact-pin-hash').value, secondHash);
  assert.equal($('artifact-pin-action').value, 'pin');
  reply(0, firstHash, true);
  await first;
  assert.equal($('artifact-pin-hash').value, secondHash, 'a late result must not replace the currently selected artifact');
  assert.equal($('artifact-unpin-confirm').required, false);

  const pinned = firstButton.fire('click'); reply(2, firstHash, true); await pinned;
  $('artifact-unpin-confirm').checked = true;
  detail.close();
  assert.equal($('artifact-unpin-confirm').checked, false);
  assert.equal($('artifact-pin-hash').value, '');
  assert.equal(document.activeElement, firstButton, 'closing the modal must restore focus');

  const closed = secondButton.fire('click');
  detail.close(); reply(3, secondHash, false); await closed;
  assert.equal($('artifact-detail-content').hidden, true, 'closed dialogs must ignore late results');
  assert.equal($('artifact-pin-hash').value, '');

  const failed = firstButton.fire('click');
  requests[4].resolve({ok: false, json: async () => ({error: 'metadata unavailable'})});
  await failed;
  assert.equal($('artifact-detail-content').hidden, true);
  assert.equal($('artifact-pin-submit').disabled, true);
  assert.equal($('artifact-detail-error').textContent, 'metadata unavailable');

  const load = firstButton.fire('click'); reply(5, firstHash, true); await load;
  let submit = await pinForm.fire('submit');
  assert.equal(submit.prevented, true, 'unpin must require a fresh explicit checkbox');
  assert.equal(pinForm.reportedInvalid, true);
  $('artifact-unpin-confirm').checked = true;
  submit = await pinForm.fire('submit');
  assert.equal(submit.prevented, undefined);
  assert.equal($('artifact-pin-submit').disabled, true);
  assert.equal((await pinForm.fire('submit')).prevented, true, 'repeated submission must be stopped');
  assert.equal((await previewForm.fire('submit')).prevented, true, 'another form must not submit while navigation is pending');

  windowEvents.get('pageshow')({persisted: true});
  assert.equal($('artifact-unpin-confirm').checked, false, 'bfcache restoration must discard destructive consent');
  assert.equal($('artifact-pin-submit').disabled, false);
  await manualForm.fire('submit', {submitter: manualPin});
  assert.equal(manualForm.children.filter(node => node.name === 'action' && !node.removed).length, 1, 'the disabled manual submitter must retain its action');
  windowEvents.get('pageshow')({persisted: true});
  assert.equal(manualForm.children.filter(node => node.name === 'action' && !node.removed).length, 0, 'history restoration must remove the previous submitter action');
  await openResume.fire('click');
  assert.equal(resume.open, true, 'resuming interrupted cleanup remains available');
})().catch(error => { console.error(error); process.exitCode = 1; });
