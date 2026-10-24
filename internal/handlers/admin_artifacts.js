(() => {
  'use strict';
  const root = document.getElementById('artifact-management');
  if (!root) return;
  const text = {{artifactText .Lang}}, canPin = {{.CanPin}}, language = {{.Lang}};
  const $ = id => document.getElementById(id);
  const detailDialog = $('artifact-detail-dialog');
  const pinForm = $('artifact-pin-form');
  const openers = new Map();
  const submitterActions = new Map();
  let sequence = 0, pending = null, submissionPending = false;

  function resetConsent(dialog) {
    for (const input of dialog.querySelectorAll('input[type="checkbox"]')) input.checked = false;
  }
  function openDialog(dialog, opener) {
    resetConsent(dialog);
    openers.set(dialog, opener || document.activeElement);
    if (!dialog.open) dialog.showModal();
  }
  for (const dialog of document.querySelectorAll('.artifact-dialog')) {
    dialog.addEventListener('close', () => {
      resetConsent(dialog);
      if (dialog === detailDialog) {
        sequence++;
        if (pending) pending.abort();
        pending = null;
        $('artifact-detail-content').hidden = true;
        $('artifact-pin-submit').disabled = true;
        $('artifact-pin-hash').value = '';
      }
      const opener = openers.get(dialog);
      if (opener && opener.isConnected) opener.focus();
    });
    dialog.addEventListener('click', event => {
      if (event.target === dialog && !submissionPending) dialog.close();
    });
    for (const button of dialog.querySelectorAll('[data-artifact-close]')) {
      button.addEventListener('click', () => dialog.close());
    }
  }
  for (const button of document.querySelectorAll('[data-artifact-open]')) {
    button.addEventListener('click', () => openDialog($(button.dataset.artifactOpen), button));
  }

  function addField(label, value, code) {
    const term = document.createElement('dt'), description = document.createElement('dd');
    term.textContent = label;
    if (code) {
      const node = document.createElement('code');
      node.textContent = value;
      description.append(node);
    } else description.textContent = value;
    $('artifact-detail-fields').append(term, description);
  }
  function date(value) {
    if (!value || String(value).startsWith('0001-')) return '—';
    const parsed = new Date(value);
    return Number.isNaN(parsed.getTime()) ? '—' : parsed.toLocaleString(language === 'en' ? 'en-GB' : 'zh-CN', {timeZone: 'UTC', hour12: false}) + ' UTC';
  }
  function showDetail(detail, focusPin) {
    $('artifact-detail-fields').replaceChildren();
    addField(text.hash, detail.hash, true);
    addField(text.type, detail.mime || text.not_recorded);
    addField(text.size, String(detail.size) + ' bytes');
    addField(text.state, (text[detail.state] || detail.state) + (detail.failed ? ' · ' + text.failed : ''));
    addField(text.created, date(detail.created_at));
    addField(text.retention_until, date(detail.retention_until));
    addField(text.refs, String(detail.reference_count || 0) + ((detail.reference_count || 0) >= 100 && detail.references_truncated ? '+' : ''));
    $('artifact-protection-reasons').replaceChildren();
    const reasons = detail.protection_reasons || [];
    for (const reason of reasons.length ? reasons : [detail.cleanup_eligible ? 'eligible' : 'retained']) {
      const item = document.createElement('li');
      item.textContent = text[reason] || text.retained;
      $('artifact-protection-reasons').append(item);
    }
    $('artifact-references').replaceChildren();
    for (const ref of detail.references || []) {
      const item = document.createElement('li');
      item.textContent = (text[ref.kind] || text.other_reference) + (ref.id ? ' · ' + ref.id : '') + ' · ' + (ref.expires_at ? date(ref.expires_at) : text.no_expiry);
      $('artifact-references').append(item);
    }
    if (!(detail.references || []).length) {
      const item = document.createElement('li');
      item.textContent = text.not_referenced;
      $('artifact-references').append(item);
    }
    $('artifact-reference-limit').hidden = !detail.references_truncated;
    $('artifact-pin-section').hidden = !canPin || detail.state !== 'available';
    $('artifact-pin-action').value = detail.pinned ? 'unpin' : 'pin';
    $('artifact-pin-hash').value = detail.hash;
    $('artifact-unpin-confirm').checked = false;
    $('artifact-unpin-confirm').required = !!detail.pinned;
    $('artifact-unpin-consent').hidden = !detail.pinned;
    $('artifact-pin-submit').textContent = detail.pinned ? text.unpin : text.pin;
    $('artifact-pin-submit').disabled = submissionPending;
    $('artifact-detail-content').hidden = false;
    if (focusPin && !$('artifact-pin-section').hidden) {
      (detail.pinned ? $('artifact-unpin-confirm') : $('artifact-pin-submit')).focus();
    }
  }
  async function loadDetail(hash, opener, focusPin) {
    const requestSequence = ++sequence;
    if (pending) pending.abort();
    const controller = new AbortController();
    pending = controller;
    $('artifact-detail-content').hidden = true;
    $('artifact-detail-error').textContent = '';
    $('artifact-detail-status').textContent = text.loading;
    $('artifact-pin-submit').disabled = true;
    $('artifact-pin-hash').value = '';
    $('artifact-unpin-confirm').checked = false;
    openDialog(detailDialog, opener);
    const timer = setTimeout(() => controller.abort(), 15000);
    try {
      const url = new URL('/cm/artifacts', location.href);
      url.searchParams.set('format', 'json');
      url.searchParams.set('hash', hash);
      url.searchParams.set('lang', language);
      const response = await fetch(url.pathname + url.search, {credentials: 'same-origin', signal: controller.signal, headers: {Accept: 'application/json'}});
      const detail = await response.json();
      if (sequence !== requestSequence || !detailDialog.open) return;
      if (!response.ok) {
        $('artifact-detail-error').textContent = detail.error || text.detail_error;
        return;
      }
      if (detail.hash !== hash) throw Error('mismatched artifact');
      showDetail(detail, focusPin);
    } catch (_) {
      if (sequence === requestSequence && detailDialog.open) $('artifact-detail-error').textContent = text.detail_error;
    } finally {
      clearTimeout(timer);
      if (sequence === requestSequence && detailDialog.open) $('artifact-detail-status').textContent = '';
      if (pending === controller) pending = null;
    }
  }
  for (const button of document.querySelectorAll('[data-artifact-detail]')) {
    button.addEventListener('click', () => loadDetail(button.dataset.artifactDetail, button, button.dataset.artifactFocus === 'pin'));
  }
  const forms = document.querySelectorAll('[data-artifact-form]');
  for (const form of forms) {
    form.addEventListener('submit', event => {
      if (submissionPending || (form === pinForm && (!$('artifact-pin-hash').value || $('artifact-detail-content').hidden))) {
        event.preventDefault();
        return;
      }
      if (!form.checkValidity()) {
        event.preventDefault();
        form.reportValidity();
        return;
      }
      // A disabled submitter is not included in a native form submission.
      // Preserve the legacy manual-pin action before disabling the controls.
      if (event.submitter && event.submitter.name === 'action') {
        const action = document.createElement('input');
        action.type = 'hidden'; action.name = 'action'; action.value = event.submitter.value;
        form.append(action);
        submitterActions.set(form, action);
      }
      submissionPending = true;
      form.setAttribute('aria-busy', 'true');
      for (const item of forms) for (const button of item.querySelectorAll('button[type="submit"],button:not([type])')) button.disabled = true;
    });
  }
  window.addEventListener('pageshow', event => {
    if (!event.persisted) return;
    submissionPending = false;
    for (const action of submitterActions.values()) action.remove();
    submitterActions.clear();
    for (const form of forms) {
      form.removeAttribute('aria-busy');
      for (const button of form.querySelectorAll('button[type="submit"],button:not([type])')) button.disabled = false;
    }
    $('artifact-pin-submit').disabled = $('artifact-detail-content').hidden;
    for (const dialog of document.querySelectorAll('.artifact-dialog')) resetConsent(dialog);
  });
  for (const dialog of document.querySelectorAll('.artifact-dialog[data-open-on-load]')) {
    if (dialog.open) dialog.close();
    openDialog(dialog);
  }
})();
