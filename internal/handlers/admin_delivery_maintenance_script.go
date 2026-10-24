package handlers

const maintenanceScript = `
<script id="worker-maintenance-script">
(() => {
'use strict';
const base = '/cm/static-delivery/api', csrf = {{.CSRFToken}}, copy = {{maintenanceCopy .Lang}};
const targets = {{.MaintenanceTargets}} || [], $ = id => document.getElementById(id);
const all = selector => Array.from($('worker-maintenance').querySelectorAll(selector));
const dialog = $('maintenance-dialog'), available = new Map(), pendingPreviews = new Map();
const params = new URLSearchParams(location.search), validID = id => /^[a-f0-9]{24}$/.test(id || '');
const zeroID = '000000000000000000000000', hasID = id => !!id && id !== zeroID;
let active = null, returnFocus = null, optionsReady = false, optionsLoading = false, optionsFailed = false, optionsSequence = 0, navigationSequence = 0, legacyRecoveryID = '';
const uuid = () => crypto.randomUUID ? crypto.randomUUID() : Date.now().toString(36) + '-' + Math.random().toString(36).slice(2);
const stateLabel = state => copy['State_' + state] || state;
// Every dialog opening gets a new context, including reopening the same Worker.
// An old read or mutation may finish, but it must not update a later dialog.
const current = context => active === context && dialog.open;
const busy = context => context.busy || context.recoveryBusy || context.recovering;
const canMaintain = context => optionsReady && !!context.option && !context.option.busy && !busy(context);
const canRecover = context => optionsReady && ['ready', 'drifted'].includes(context.target.status) && !hasID(context.target.active_job_id) && !(context.option && context.option.busy) && !busy(context);
const jobURL = id => '/cm/static-delivery/publish?job_id=' + encodeURIComponent(id);
const workflowURL = id => '/cm/tasks?source=workflow&job_id=' + encodeURIComponent(id);

async function api(path, body, key) {
    const controller = new AbortController(), timer = setTimeout(() => controller.abort(), 30000);
    const request = {credentials:'same-origin', signal:controller.signal, headers:{'X-CSRF-Token':csrf}};
    if (body !== undefined) {
        request.method = 'POST';
        request.headers['Content-Type'] = 'application/json';
        request.headers['Idempotency-Key'] = key || '';
        request.headers.Prefer = 'respond-async';
        request.body = JSON.stringify(body);
    }
    try {
        const response = await fetch(base + path, request);
        let data;
        try { data = await response.json(); } catch { throw Error(copy.NoJSON + ' (HTTP ' + response.status + ')'); }
        if (!response.ok) throw Error((data.code || 'HTTP ' + response.status) + ': ' + (data.message || copy.Failed));
        return data;
    } finally { clearTimeout(timer); }
}
function error(context, value) {
    if (current(context)) $('maintenance-error').textContent = value.message || String(value);
}
function targetSummary(context) {
    if (!current(context)) return;
    const target = context.target, option = context.option;
    $('maintenance-title').textContent = target.name;
    $('maintenance-subtitle').textContent = target.worker_name + ' · ' + (target.public_origin || copy.NotSet);
    $('maintenance-account').textContent = option && option.account_name || target.account_ref;
    $('maintenance-target-state').textContent = stateLabel(target.status);
    $('maintenance-target-environment').textContent = target.environment === 'production' ? copy.Production : copy.Test;
    $('maintenance-revision').textContent = (option ? option.revision : target.current_revision) + ' / ' + (option ? option.config_revision : target.config_revision);
    $('maintenance-current-release').textContent = hasID(target.current_release_id) ? target.current_release_id : copy.None;
    $('maintenance-remote-version').textContent = target.remote_version_id || copy.None;
}
function setTab(name, focus) {
    for (const tab of all('[data-maintenance-tab]')) {
        const selected = tab.dataset.maintenanceTab === name;
        tab.setAttribute('aria-selected', selected ? 'true' : 'false');
        tab.tabIndex = selected ? 0 : -1;
        $('maintenance-panel-' + tab.dataset.maintenanceTab).hidden = !selected;
        if (selected && focus) tab.focus();
    }
}
for (const tab of all('[data-maintenance-tab]')) {
    tab.addEventListener('click', () => setTab(tab.dataset.maintenanceTab, false));
    tab.addEventListener('keydown', event => {
        const names = ['pages', 'versions', 'actions'], index = names.indexOf(tab.dataset.maintenanceTab);
        let next;
        if (event.key === 'ArrowRight') next = (index + 1) % names.length;
        if (event.key === 'ArrowLeft') next = (index + names.length - 1) % names.length;
        if (event.key === 'Home') next = 0;
        if (event.key === 'End') next = names.length - 1;
        if (next !== undefined) { event.preventDefault(); setTab(names[next], true); }
    });
}
function closeTarget() {
    navigationSequence++;
    if (active) clearTimeout(active.recoveryTimer);
    active = null;
    dialog.close();
}
$('maintenance-close').addEventListener('click', closeTarget);
dialog.addEventListener('cancel', event => { event.preventDefault(); closeTarget(); });
dialog.addEventListener('close', () => {
    if (dialog.open) return;
    navigationSequence++;
    if (active) clearTimeout(active.recoveryTimer);
    active = null;
    if (returnFocus) returnFocus.focus();
    returnFocus = null;
});
dialog.addEventListener('click', event => { if (event.target === dialog) closeTarget(); });
function availability(context) {
    if (optionsLoading || !optionsReady && !optionsFailed) return copy.Checking;
    if (optionsFailed) return copy.AvailabilityError;
    if (!context.option) return copy.NotReady;
    if (context.option.busy) return copy.Busy;
    return '';
}
function controls(context) {
    if (!current(context)) return;
    const allowed = canMaintain(context);
    for (const id of ['maintenance-operation', 'maintenance-paths', 'maintenance-release', 'maintenance-origin', 'maintenance-environment', 'maintenance-review', 'maintenance-preview', 'maintenance-publish']) $(id).disabled = !allowed;
    const operation = $('maintenance-operation').value;
    $('maintenance-paths').disabled = !allowed || !['withdraw', 'rollback_pages'].includes(operation);
    $('maintenance-release').disabled = !allowed || !operation.startsWith('rollback_');
    $('maintenance-origin').disabled = !allowed || operation !== 'change_origin';
    $('maintenance-environment').disabled = !allowed || operation !== 'change_environment';
    $('maintenance-availability').textContent = availability(context);
    $('maintenance-availability').hidden = !availability(context);
    $('maintenance-consent').disabled = !context.frozen || busy(context);
    $('maintenance-submit').disabled = !allowed || !context.frozen || !$('maintenance-consent').checked;
    $('recovery-preview').disabled = !canRecover(context);
    $('recovery-availability').textContent = canRecover(context) || context.recoveryProof || context.recoveryJob ? '' : copy.RecoveryUnavailable;
    $('recovery-consent').disabled = busy(context) || !context.recoveryProof;
    $('recovery-submit').disabled = !context.recoveryProof || !canRecover(context) || !$('recovery-consent').checked;
    for (const action of context.pageActions) action.button.disabled = !allowed || action.currentOnly && !context.viewingCurrent;
    $('maintenance-history-more').disabled = context.historyLoading;
}
function resetConfirmation(context) {
    if (!current(context)) return;
    context.frozen = null;
    $('maintenance-confirmation').hidden = true;
    $('maintenance-consent').checked = false;
    controls(context);
}
function fields() {
    const op = $('maintenance-operation').value;
    $('maintenance-paths-field').hidden = !['withdraw', 'rollback_pages'].includes(op);
    $('maintenance-history-field').hidden = !op.startsWith('rollback_');
    $('maintenance-origin-field').hidden = op !== 'change_origin';
    $('maintenance-environment-field').hidden = op !== 'change_environment';
}
function formChanged() { if (active) resetConfirmation(active); fields(); }
$('maintenance-operation').addEventListener('change', formChanged);
for (const id of ['maintenance-paths', 'maintenance-release', 'maintenance-origin', 'maintenance-environment', 'maintenance-review']) {
    $(id).addEventListener('input', formChanged);
    $(id).addEventListener('change', formChanged);
}
$('maintenance-consent').addEventListener('change', () => { if (active) controls(active); });
$('recovery-consent').addEventListener('change', () => { if (active) controls(active); });
function filterTargets() {
    const term = $('maintenance-search').value.trim().toLocaleLowerCase();
    let count = 0;
    for (const row of all('[data-maintenance-row]')) {
        const target = targets.find(value => value.id === row.dataset.maintenanceRow);
        const haystack = [target.name, target.worker_name, target.public_origin, target.environment, target.status, stateLabel(target.status)].join(' ').toLocaleLowerCase();
        row.hidden = !haystack.includes(term);
        if (!row.hidden) count++;
    }
    $('maintenance-count').textContent = count + ' / ' + targets.length + ' ' + copy.Count;
    $('maintenance-list-empty').hidden = count > 0;
    $('maintenance-empty-title').textContent = targets.length ? copy.NoMatch : copy.Empty;
    $('maintenance-empty-hint').textContent = targets.length ? '' : copy.EmptyHint;
}
$('maintenance-search').addEventListener('input', filterTargets);
function rowControls() {
    for (const button of all('[data-maintenance-publish]')) {
        const option = available.get(button.dataset.maintenancePublish);
        button.disabled = !optionsReady || !option || option.busy;
        button.title = !optionsReady ? copy.Checking : !option ? copy.NotReady : option.busy ? copy.Busy : copy.PublishHint;
    }
}
async function loadOptions() {
    const sequence = ++optionsSequence;
    optionsLoading = true;
    optionsReady = false;
    optionsFailed = false;
    $('maintenance-reload').disabled = true;
    $('maintenance-list-status').textContent = copy.Checking;
    $('maintenance-list-error').textContent = '';
    rowControls();
    if (active) controls(active);
    try {
        const options = await api('/static-delivery/options');
        if (sequence !== optionsSequence) return;
        available.clear();
        for (const option of options) available.set(option.id, option);
        optionsReady = true;
        $('maintenance-list-status').textContent = copy.ReadyList;
        if (active) {
            const after = available.get(active.id);
            active.option = after;
            if (active.frozen && (!after || after.busy || active.frozen.body.expected_revision !== after.revision || active.frozen.body.expected_config_revision !== after.config_revision)) resetConfirmation(active);
            targetSummary(active);
        }
    } catch (value) {
        if (sequence !== optionsSequence) return;
        optionsFailed = true;
        $('maintenance-list-status').textContent = '';
        $('maintenance-list-error').textContent = copy.AvailabilityError + ' ' + (value.message || value);
    } finally {
        if (sequence === optionsSequence) {
            optionsLoading = false;
            $('maintenance-reload').disabled = false;
            rowControls();
            if (active) controls(active);
        }
    }
}
$('maintenance-reload').addEventListener('click', loadOptions);
for (const button of all('[data-maintenance-publish]')) button.addEventListener('click', () => { if (!button.disabled) location.assign('/cm/content'); });
$('maintenance-publish').addEventListener('click', () => { if (!$('maintenance-publish').disabled) location.assign('/cm/content'); });
function remember(context, key, value) {
    if (!current(context)) return;
    const url = new URL(location.href);
    url.searchParams.set('target_id', context.id);
    if (value) url.searchParams.set(key, value); else url.searchParams.delete(key);
    history.replaceState(null, '', url);
}
function cell(row, text) {
    const node = document.createElement('td');
    if (text !== undefined) node.textContent = text;
    row.append(node);
    return node;
}
function button(parent, text, action) {
    const node = document.createElement('button');
    node.type = 'button'; node.className = 'btn btn-secondary'; node.textContent = text;
    node.addEventListener('click', action); parent.append(node);
    return node;
}
function link(parent, text, href) {
    const node = document.createElement('a');
    node.className = 'btn btn-secondary'; node.textContent = text; node.href = href; parent.append(node);
}
function fillReleases(context) {
    const previous = $('maintenance-release').value, seen = new Set();
    $('maintenance-release').replaceChildren(new Option(copy.ChooseRelease, ''));
    for (const job of context.history) {
        if (job.state !== 'succeeded' || !hasID(job.release_id) || seen.has(job.release_id)) continue;
        seen.add(job.release_id);
        $('maintenance-release').add(new Option(job.release_id + ' · ' + job.created_at, job.release_id));
    }
    $('maintenance-release').value = seen.has(previous) ? previous : '';
}
function renderHistory(context) {
    $('maintenance-history').replaceChildren();
    for (const job of context.history) {
        const row = document.createElement('tr');
        cell(row, hasID(job.release_id) ? job.release_id : copy.None);
        cell(row, job.created_at || copy.None);
        cell(row, stateLabel(job.state) + (job.error_code ? ' · ' + job.error_code : ''));
        const actions = cell(row); actions.className = 'wm-actions';
        link(actions, copy.ViewJob, jobURL(job.id));
        if (job.state === 'succeeded' && hasID(job.release_id)) button(actions, copy.ViewPages, () => { setTab('pages', false); loadPages(context, job); });
        $('maintenance-history').append(row);
    }
    fillReleases(context);
}
async function loadHistory(context) {
    if (!current(context) || context.historyLoading) return;
    // Recovery can refresh this same Worker before its initial history arrives.
    const sequence = ++context.historySequence;
    context.historyLoading = true;
    $('maintenance-history-status').textContent = copy.HistoryLoading;
    controls(context);
    try {
        const result = await api('/static-targets/' + context.id + '/deployments?limit=100&offset=' + context.historyOffset);
        if (!current(context) || sequence !== context.historySequence) return;
        const rows = result.jobs || [], seen = new Set(context.history.map(job => job.id));
        for (const job of rows) if (!seen.has(job.id)) context.history.push(job);
        context.historyOffset += rows.length;
        context.historyMore = rows.length === 100;
        renderHistory(context);
        $('maintenance-history-status').textContent = context.history.length ? '' : copy.HistoryEmpty;
        $('maintenance-history-more').textContent = copy.HistoryMore;
        $('maintenance-history-more').hidden = !context.historyMore;
        if (!context.pagesLoaded && !context.pagesLoading) {
            const source = context.history.find(job => job.state === 'succeeded' && job.release_id === context.target.current_release_id);
            if (source) await loadPages(context, source);
            else $('maintenance-pages-status').textContent = hasID(context.target.current_release_id) ? copy.PagesMissing : copy.PagesEmpty;
        }
    } catch (value) {
        if (!current(context) || sequence !== context.historySequence) return;
        $('maintenance-history-status').textContent = copy.HistoryError + ' ' + (value.message || value);
        $('maintenance-history-more').textContent = copy.HistoryRetry;
        $('maintenance-history-more').hidden = false;
        if (!context.pagesLoaded) $('maintenance-pages-status').textContent = copy.HistoryError;
    } finally {
        if (current(context) && sequence === context.historySequence) { context.historyLoading = false; controls(context); }
    }
}
function selectOperation(context, op, path, release) {
    if (!current(context) || !canMaintain(context)) return;
    resetConfirmation(context);
    $('maintenance-operation').value = op;
    $('maintenance-paths').value = path || '';
    if (release) $('maintenance-release').value = release;
    $('maintenance-review').checked = false;
    fields(); controls(context); setTab('actions', true);
}
async function loadPages(context, source) {
    if (!current(context)) return;
    const sequence = ++context.pageSequence;
    context.pagesLoading = true;
    context.pageSource = source;
    context.viewingCurrent = source.release_id === context.target.current_release_id;
    context.pageActions = [];
    $('maintenance-pages').replaceChildren();
    $('maintenance-pages-heading').textContent = context.viewingCurrent ? copy.Pages : copy.HistoricalPages + ' · ' + source.release_id;
    $('maintenance-pages-status').textContent = copy.PagesLoading;
    $('maintenance-pages-retry').hidden = true;
    try {
        const result = await api('/static-deployments/' + encodeURIComponent(source.id));
        if (!current(context) || sequence !== context.pageSequence) return;
        if (!result.job || result.job.state !== 'succeeded' || result.job.target_id && result.job.target_id !== context.id || result.job.release_id && result.job.release_id !== source.release_id) throw Error(copy.InvalidPreview);
        const pages = result.pages || [];
        for (const page of pages) {
            const row = document.createElement('tr');
            cell(row, page.path); cell(row, page.publication_id || copy.None);
            const actions = cell(row); actions.className = 'wm-actions';
            const withdraw = button(actions, copy.Withdraw, () => selectOperation(context, 'withdraw', page.path));
            const rollback = button(actions, copy.RollbackPage, () => selectOperation(context, 'rollback_pages', page.path, source.release_id));
            context.pageActions.push({button:withdraw, currentOnly:true}, {button:rollback, currentOnly:false});
            $('maintenance-pages').append(row);
        }
        context.pagesLoaded = true;
        $('maintenance-pages-status').textContent = pages.length ? '' : copy.PagesEmpty;
    } catch (value) {
        if (!current(context) || sequence !== context.pageSequence) return;
        $('maintenance-pages-status').textContent = copy.PagesError + ' ' + (value.message || value);
        $('maintenance-pages-retry').hidden = false;
    } finally {
        if (current(context) && sequence === context.pageSequence) { context.pagesLoading = false; controls(context); }
    }
}
$('maintenance-history-more').addEventListener('click', () => { if (active) return loadHistory(active); });
$('maintenance-pages-retry').addEventListener('click', () => { if (active && active.pageSource) return loadPages(active, active.pageSource); });
function request(context) {
    if (!$('maintenance-review').checked) throw Error(copy.ReviewRequired);
    const operation = $('maintenance-operation').value;
    const result = {operation, expected_revision:context.option.revision, expected_config_revision:context.option.config_revision, manual_script_review:true};
    if (['withdraw', 'rollback_pages'].includes(operation)) {
        result.paths = $('maintenance-paths').value.split(/\r?\n/).map(path => path.trim()).filter(Boolean);
        if (!result.paths.length || result.paths.length > 100 || new Set(result.paths).size !== result.paths.length) throw Error(copy.InvalidPaths);
    }
    if (operation.startsWith('rollback_')) {
        result.source_release_id = $('maintenance-release').value;
        if (!validID(result.source_release_id)) throw Error(copy.ReleaseRequired);
    }
    if (operation === 'change_origin') result.new_origin = $('maintenance-origin').value.trim();
    if (operation === 'change_environment') result.new_environment = $('maintenance-environment').value;
    return result;
}
function showPreview(context, result, body) {
    if (!current(context)) return;
    if (!result || !result.confirmation_digest || !Array.isArray(result.changes) || result.target_id && result.target_id !== context.id) throw Error(copy.InvalidPreview);
    const op = copy[{withdraw:'WithdrawMany', rollback_pages:'RollbackPages', rollback_site:'RollbackSite', change_origin:'ChangeOrigin', change_environment:'ChangeEnvironment'}[body.operation]] || body.operation;
    context.frozen = {body:{...body, confirmation_digest:result.confirmation_digest}, key:uuid()};
    $('maintenance-summary').textContent = (context.option && context.option.account_name || context.target.account_ref) + ' / ' + context.target.name + ' / ' + context.target.public_origin + ' · ' + op + ' · ' + result.changes.length + ' ' + copy.Changes + ', ' + result.preserved_pages + ' ' + copy.Preserved;
    $('maintenance-changes').replaceChildren();
    for (const change of result.changes) {
        const item = document.createElement('li'); item.textContent = change.action + ' · ' + change.path + ' → ' + change.url;
        $('maintenance-changes').append(item);
    }
    $('maintenance-diagnostics').replaceChildren();
    $('maintenance-diagnostics-title').hidden = !(result.diagnostics || []).length;
    for (const diagnostic of result.diagnostics || []) {
        const item = document.createElement('li'); item.textContent = diagnostic.code + ': ' + diagnostic.message;
        $('maintenance-diagnostics').append(item);
    }
    $('maintenance-consent').checked = false;
    $('maintenance-confirmation').hidden = false;
    $('maintenance-status').textContent = copy.PreviewReady;
    remember(context, 'preview_job_id', '');
    controls(context);
}
async function waitPreview(context, pending, first) {
    const started = Date.now(); let result = first;
    while (current(context)) {
        result = result || await api('/static-preview-jobs/' + encodeURIComponent(pending.jobID));
        if (!current(context)) return null;
        if (result.target_id && result.target_id !== context.id) throw Error(copy.InvalidPreview);
        if (result.state === 'succeeded' && result.preview) { pendingPreviews.delete(pending.signature); return result.preview; }
        if (['failed', 'cancelled', 'partial', 'needs_review'].includes(result.state)) { pendingPreviews.delete(pending.signature); throw Error(copy.PreviewFailed + ': ' + (result.error_code || stateLabel(result.state))); }
        if (Date.now() - started > 300000) throw Error(copy.PreviewTimeout);
        $('maintenance-status').textContent = copy.Preparing + ' · ' + stateLabel(result.state);
        await new Promise(resolve => setTimeout(resolve, 1500)); result = null;
    }
    return null;
}
function previewLink(context, id) {
    if (!current(context)) return;
    $('maintenance-preview-job').href = workflowURL(id);
    $('maintenance-preview-job').hidden = false;
    remember(context, 'preview_job_id', id);
}
$('maintenance-form').addEventListener('submit', async event => {
    event.preventDefault(); const context = active;
    if (!context || !canMaintain(context)) return;
    resetConfirmation(context); $('maintenance-error').textContent = '';
    let body;
    try { body = request(context); } catch (value) { error(context, value); return; }
    context.busy = true; controls(context);
    $('maintenance-status').textContent = copy.Preparing;
    const signature = JSON.stringify([context.id, body]);
    // Retain uncertain preparation requests across dialog closures and retries.
    let pending = pendingPreviews.get(signature);
    if (!pending) { pending = {signature, key:uuid(), jobID:''}; pendingPreviews.set(signature, pending); }
    try {
        let result = pending.jobID ? {job_id:pending.jobID} : await api('/static-targets/' + context.id + '/deployment-preview', body, pending.key);
        if (result.job_id) pending.jobID = result.job_id;
        if (!current(context)) return;
        if (pending.jobID) { previewLink(context, pending.jobID); result = await waitPreview(context, pending); }
        else pendingPreviews.delete(signature);
        if (current(context) && result) showPreview(context, result, result.request || body);
    } catch (value) { error(context, value); }
    finally { context.busy = false; if (current(context)) controls(context); }
});
$('maintenance-submit').addEventListener('click', async () => {
    const context = active;
    if (!context || !canMaintain(context) || !context.frozen || $('maintenance-submit').disabled) return;
    if (!$('maintenance-consent').checked) { error(context, Error(copy.ConsentRequired)); return; }
    context.busy = true; controls(context); $('maintenance-error').textContent = '';
    const frozen = context.frozen;
    try {
        const result = await api('/static-targets/' + context.id + '/deployments', frozen.body, frozen.key);
        if (!current(context)) return;
        context.option = {...context.option, busy:true}; available.set(context.id, context.option);
        context.recoveryProof = null; $('recovery-confirmation').hidden = true;
        $('maintenance-status').textContent = copy.Submitted;
        $('maintenance-job').href = jobURL(result.job_id); $('maintenance-job').hidden = false;
        $('maintenance-preview-job').hidden = true;
        resetConfirmation(context); rowControls();
    } catch (value) { error(context, value); }
    finally { context.busy = false; if (current(context)) controls(context); }
});
async function resumePreview(context, id) {
    context.busy = true; controls(context); previewLink(context, id); setTab('actions', false);
    try {
        const result = await waitPreview(context, {jobID:id}, null);
        if (!current(context) || !result) return;
        const body = result.request;
        if (!body || !body.operation || body.operation === 'publish') throw Error(copy.InvalidPreview);
        $('maintenance-operation').value = body.operation;
        $('maintenance-paths').value = (body.paths || []).join('\n');
        $('maintenance-release').value = body.source_release_id || '';
        $('maintenance-origin').value = body.new_origin || '';
        $('maintenance-environment').value = body.new_environment || context.target.environment;
        $('maintenance-review').checked = false;
        fields(); showPreview(context, result, body);
    } catch (value) { error(context, value); }
    finally { context.busy = false; if (current(context)) controls(context); }
}
$('recovery-preview').addEventListener('click', async () => {
    const context = active;
    if (!context || !canRecover(context)) return;
    context.recoveryBusy = true; context.recoveryProof = null;
    context.recoveryPreviewKey = context.recoveryPreviewKey || uuid();
    $('recovery-confirmation').hidden = true; $('recovery-consent').checked = false;
    $('recovery-proof').textContent = ''; $('maintenance-error').textContent = ''; $('recovery-status').textContent = copy.RecoveryChecking;
    controls(context);
    try {
        const proof = await api('/static-targets/' + context.id + '/recovery-preview', {}, context.recoveryPreviewKey);
        if (!current(context)) return;
        if (!proof.confirmation_digest || proof.target && proof.target.id && proof.target.id !== context.id) throw Error(copy.InvalidPreview);
        context.recoveryProof = {digest:proof.confirmation_digest}; context.recoveryPreviewKey = '';
        $('recovery-proof').textContent = context.target.name + ' / ' + context.target.public_origin + ' · ' + copy.RecoveryEvidence + ': ' + proof.remote.VersionID + ' · ' + copy.RecoveryRelease + ': ' + proof.release_id;
        $('recovery-confirmation').hidden = false; $('recovery-status').textContent = '';
    } catch (value) { error(context, value); }
    finally { context.recoveryBusy = false; if (current(context)) controls(context); }
});
async function pollRecovery(context, initial) {
    if (!current(context) || !context.recoveryJob) return;
    clearTimeout(context.recoveryTimer); $('recovery-refresh').disabled = true;
    try {
        const result = initial || await api('/static-recovery-jobs/' + encodeURIComponent(context.recoveryJob));
        if (!current(context)) return;
        if (result.target_id && result.target_id !== context.id) throw Error(copy.InvalidPreview);
        $('recovery-status').textContent = stateLabel(result.state) + (result.error_code ? ': ' + result.error_code : '');
        if (result.state === 'succeeded') {
            $('recovery-status').textContent = copy.RecoveryComplete;
            if (hasID(result.delivery_job_id)) { $('recovery-result').href = jobURL(result.delivery_job_id); $('recovery-result').textContent = copy.RecoveryVerified; }
            await refreshRecoveredTarget(context, result.delivery_job_id);
            if (!current(context)) return;
            context.recovering = false;
            $('recovery-refresh').hidden = true; controls(context); return;
        }
        if (['failed', 'cancelled', 'needs_review', 'partial'].includes(result.state)) { context.recovering = false; controls(context); return; }
        context.recoveryTimer = setTimeout(() => pollRecovery(context), 2500);
    } catch (value) {
        if (current(context)) { error(context, value); $('recovery-status').textContent = copy.RecoveryReadError; $('recovery-refresh').hidden = false; }
    } finally { if (current(context)) $('recovery-refresh').disabled = false; }
}
async function refreshRecoveredTarget(context, deliveryJobID) {
    if (!hasID(deliveryJobID)) throw Error(copy.InvalidPreview);
    const snapshot = await api('/static-deployments/' + encodeURIComponent(deliveryJobID));
    if (!current(context)) return;
    const job = snapshot.job;
    if (!job || job.state !== 'succeeded' || job.target_id !== context.id || !hasID(job.release_id)) throw Error(copy.InvalidPreview);
    Object.assign(context.target, {status:'ready', active_job_id:zeroID, current_release_id:job.release_id, remote_version_id:job.checkpoint && job.checkpoint.remote_version_id || ''});
    resetConfirmation(context);
    context.history = []; context.historyOffset = 0; context.historySequence++; context.historyLoading = false;
    context.pagesLoaded = false; context.pagesLoading = false; context.pageSequence++; context.pageActions = [];
    $('maintenance-pages').replaceChildren();
    await loadOptions();
    if (!current(context)) return;
    if (context.option) Object.assign(context.target, {current_revision:context.option.revision, config_revision:context.option.config_revision});
    targetSummary(context);
    await loadHistory(context);
}
$('recovery-refresh').addEventListener('click', () => { if (active) return pollRecovery(active); });
$('recovery-submit').addEventListener('click', async () => {
    const context = active;
    if (!context || !context.recoveryProof || !canRecover(context) || $('recovery-submit').disabled) return;
    if (!$('recovery-consent').checked) { error(context, Error(copy.RecoveryConfirm)); return; }
    context.recoveryBusy = true; controls(context); $('maintenance-error').textContent = '';
    const proof = context.recoveryProof;
    try {
        const result = await api('/static-targets/' + context.id + '/recover', {confirmation_digest:proof.digest, confirmed:true}, proof.digest);
        if (!current(context)) return;
        context.recoveryJob = result.job_id; context.recovering = true;
        remember(context, 'recovery_job_id', result.job_id);
        $('recovery-result').href = workflowURL(result.job_id); $('recovery-result').hidden = false;
        $('recovery-confirmation').hidden = true; $('recovery-status').textContent = copy.RecoverySubmitted;
        context.recoveryProof = null;
        await pollRecovery(context);
    } catch (value) { error(context, value); }
    finally { context.recoveryBusy = false; if (current(context)) controls(context); }
});
async function resumeLegacyRecovery(id) {
    const sequence = ++navigationSequence;
    legacyRecoveryID = id;
    $('maintenance-resumed-recovery').hidden = false;
    $('maintenance-resumed-link').href = '/cm/static-delivery/maintenance?recovery_job_id=' + encodeURIComponent(id);
    $('maintenance-resumed-link').textContent = copy.RecoveryRefresh;
    $('maintenance-list-error').textContent = '';
    try {
        // Older task-center links contain only the durable recovery ID. Resolve
        // its Worker through the same authorized read used for progress polling.
        const result = await api('/static-recovery-jobs/' + encodeURIComponent(id));
        if (sequence !== navigationSequence || active || dialog.open) return;
        const target = targets.find(value => value.id === result.target_id);
        if (!target) {
            if (result.state === 'succeeded' && hasID(result.delivery_job_id)) {
                legacyRecoveryID = '';
                $('maintenance-resumed-link').href = jobURL(result.delivery_job_id);
                $('maintenance-resumed-link').textContent = copy.RecoveryVerified;
                return;
            }
            throw Error(copy.Unknown);
        }
        legacyRecoveryID = '';
        $('maintenance-resumed-recovery').hidden = true;
        await openTarget(target.id, all('[data-maintenance-open]').find(button => button.dataset.maintenanceOpen === target.id), true, result);
    } catch (value) {
        if (sequence === navigationSequence && !active) $('maintenance-list-error').textContent = value.message || String(value);
    }
}
$('maintenance-resumed-link').addEventListener('click', event => {
    if (!legacyRecoveryID) return;
    event.preventDefault();
    resumeLegacyRecovery(legacyRecoveryID);
});
async function openTarget(id, opener, resume, initialRecovery) {
    const target = targets.find(value => value.id === id);
    if (!target) { $('maintenance-list-error').textContent = copy.Unknown; return; }
    navigationSequence++;
    if (active) clearTimeout(active.recoveryTimer);
    const context = {id, target, option:available.get(id), busy:false, recoveryBusy:false, recovering:false, recoveryTimer:null, recoveryJob:'', recoveryProof:null, history:[], historyOffset:0, historyMore:true, historyLoading:false, historySequence:0, pagesLoaded:false, pagesLoading:false, pageSequence:0, pageSource:null, pageActions:[], viewingCurrent:true, frozen:null};
    active = context; returnFocus = opener || $('maintenance-search');
    for (const field of ['maintenance-error', 'maintenance-status', 'maintenance-summary', 'maintenance-history-status', 'recovery-status', 'recovery-proof']) $(field).textContent = '';
    for (const field of ['maintenance-pages', 'maintenance-history', 'maintenance-changes', 'maintenance-diagnostics']) $(field).replaceChildren();
    for (const field of ['maintenance-confirmation', 'maintenance-job', 'maintenance-preview-job', 'maintenance-history-more', 'maintenance-pages-retry', 'recovery-confirmation', 'recovery-result', 'recovery-refresh']) $(field).hidden = true;
    $('maintenance-pages-heading').textContent = copy.Pages;
    $('maintenance-pages-status').textContent = copy.PagesLoading;
    $('maintenance-operation').value = 'withdraw'; $('maintenance-paths').value = ''; $('maintenance-origin').value = '';
    $('maintenance-environment').value = target.environment;
    $('maintenance-release').replaceChildren(new Option(copy.ChooseRelease, ''));
    $('maintenance-review').checked = false; $('maintenance-consent').checked = false; $('recovery-consent').checked = false;
    $('maintenance-recovery').open = false; $('recovery-result').textContent = copy.RecoveryJob;
    fields(); setTab('pages', false);
    if (!dialog.open) dialog.showModal();
    targetSummary(context);
    controls(context);
    const url = new URL(location.href); url.searchParams.set('target_id', id);
    if (!resume) { url.searchParams.delete('preview_job_id'); url.searchParams.delete('recovery_job_id'); }
    history.replaceState(null, '', url);
    if (resume && validID(params.get('recovery_job_id'))) {
        context.recoveryJob = params.get('recovery_job_id'); context.recovering = true;
        $('recovery-result').href = workflowURL(context.recoveryJob); $('recovery-result').hidden = false;
        $('maintenance-recovery').open = true; setTab('actions', false); controls(context); pollRecovery(context, initialRecovery);
    }
    await loadHistory(context);
    if (current(context) && resume && !context.recoveryJob && validID(params.get('preview_job_id'))) await resumePreview(context, params.get('preview_job_id'));
}
for (const button of all('[data-maintenance-open]')) button.addEventListener('click', () => openTarget(button.dataset.maintenanceOpen, button, false));
filterTargets(); fields();
loadOptions();
const requested = params.get('target_id'), recovery = params.get('recovery_job_id');
if (requested) openTarget(requested, all('[data-maintenance-open]').find(button => button.dataset.maintenanceOpen === requested), true);
if (validID(recovery) && !requested) resumeLegacyRecovery(recovery);
})();
</script>
`
