package handlers

func init() {
	adminTemplateFuncMap["staticPublishText"] = staticPublishText
	adminTemplates["static_delivery_publish"] = adminLayoutStart + staticPublishBody + adminLayoutEnd
}

const staticPublishBody = `
{{$P := staticPublishText .Lang}}
<div class="page-header"><h1>{{$P.title}}</h1><a class="btn btn-secondary" href="/cm/content">{{$P.back}}</a></div>
<style>
.static-publish{max-width:1040px}.static-publish .card{padding:24px;margin-bottom:20px}.static-publish h2{font-size:1.15rem;margin:0 0 12px}.static-publish p,.static-publish li{line-height:1.6;overflow-wrap:anywhere}.static-publish p{margin:8px 0}.static-publish ul{padding-left:24px;margin:12px 0}.static-publish li+li{margin-top:8px}.static-publish summary{cursor:pointer;font-weight:600;min-height:32px}.static-publish .publish-help{color:var(--text-muted);font-size:.9rem}.static-publish .publish-destination{display:grid;grid-template-columns:1fr 1.4fr;gap:16px;margin:20px 0 12px}.static-publish .publish-destination label{font-size:.9rem;color:var(--text-muted)}.static-publish select{display:block;width:100%;padding:10px 12px;margin-top:6px;min-height:44px}.static-publish .publish-check{display:flex;align-items:flex-start;gap:10px;margin:16px 0;line-height:1.6}.static-publish input[type=checkbox]{flex-shrink:0;width:18px;height:18px;margin:4px 0 0}.static-publish .publish-actions{display:flex;flex-wrap:wrap;gap:8px;align-items:center;margin-top:16px}.static-publish .btn{min-height:44px;white-space:normal}.static-publish .publish-target-detail{padding:12px 16px;background:var(--bg-tertiary);border-radius:var(--radius);white-space:pre-line}.static-publish .publish-target-detail:empty{display:none}.static-publish .publish-error{color:var(--danger);white-space:pre-wrap}.static-publish #published-links:empty,.static-publish #preview-warnings:empty{display:none}.static-publish #job-history a{overflow-wrap:anywhere}.static-publish [hidden]{display:none!important}@media(max-width:640px){.static-publish .card{padding:20px}.static-publish .publish-destination{grid-template-columns:1fr;gap:12px}.static-publish .publish-actions{align-items:stretch;flex-direction:column}.static-publish .publish-actions .btn{width:100%}}
</style>
<div class="static-publish">
{{if .ModuleError}}<div class="card" role="alert">{{.ModuleError}}</div>{{else}}
<section class="card">
<details {{if le (len .Selection) 3}}open{{end}}><summary id="selection-summary">{{len .Selection}} {{$P.selectedPublished}}</summary><ul>{{range .Selection}}<li>{{.Title}}</li>{{end}}</ul></details>
<p class="publish-help">{{if .SelectedIDs}}{{$P.selectionHelp}}{{else}}{{$P.emptySelection}}{{end}}</p>
</section>
<section class="card"><h2>{{$P.targetTitle}}</h2><p class="publish-help">{{$P.targetHelp}}</p>
{{if .CanPublish}}
<div class="publish-destination"><label for="publish-account">{{$P.account}}<select id="publish-account"><option value="">{{$P.chooseAccount}}</option></select></label><label for="publish-target">{{$P.worker}}<select id="publish-target" disabled><option value="">{{$P.chooseAccountFirst}}</option></select></label></div>
<p id="target-details" class="publish-target-detail" role="status" aria-live="polite">{{$P.loadingTargets}}</p>
<div class="publish-actions"><button id="reload-targets" class="btn btn-secondary" hidden>{{$P.reloadTargets}}</button><a id="manage-targets" href="/cm/static-delivery" hidden>{{$P.manageTargets}}</a></div>
<label class="publish-check"><input type="checkbox" id="script-review"><span>{{$P.scriptReview}}</span></label>
<details><summary class="publish-help">{{$P.queue}}</summary><label class="publish-check"><input type="checkbox" id="queue-mode"><span>{{$P.queueHelp}}</span></label></details>
<div class="publish-actions"><button id="preview-start" class="btn btn-primary" disabled>{{$P.preview}}</button><span class="publish-help">{{$P.previewHelp}}</span></div>
{{else}}<p>{{$P.noPermission}}</p>{{end}}
</section>
<section class="card" id="preview-panel" hidden><h2>{{$P.previewTitle}}</h2><p id="preview-summary"></p><ul id="preview-changes"></ul><ul id="preview-warnings"></ul><label class="publish-check"><input type="checkbox" id="publish-consent"><span>{{$P.consent}}</span></label><div class="publish-actions"><button id="deploy-start" class="btn btn-primary" disabled>{{$P.deploy}}</button></div></section>
<section class="card" id="publish-progress"><h2>{{$P.progressTitle}}</h2><p id="export-status" role="status" aria-live="polite">{{$P.exportIdle}}</p><p id="deployment-status" role="status" aria-live="polite">{{$P.deploymentIdle}}</p><p id="delivery-error" class="publish-error" role="alert" hidden></p><p id="job-reference" class="publish-help" hidden></p><div class="publish-actions"><button id="refresh-job" class="btn btn-secondary" hidden>{{$P.refresh}}</button><button id="cancel-job" class="btn btn-secondary" hidden>{{$P.cancel}}</button></div><ul id="published-links"></ul><p class="publish-help">{{$P.progressHelp}}</p></section>
<details class="card" id="download-panel"><summary>{{$P.downloadTitle}}</summary><p class="publish-help">{{$P.downloadHelp}}</p><div class="publish-actions"><button id="standalone-export" class="btn btn-secondary">{{$P.standalone}}</button></div><div id="downloads" hidden><div class="publish-actions"><a id="zip-link" class="btn btn-secondary">{{$P.zip}}</a><a id="html-link" class="btn btn-secondary">{{$P.html}}</a></div></div></details>
{{if .CanPublish}}<section class="card"><h2>{{$P.historyTitle}}</h2><p class="publish-help">{{$P.historyHelp}}</p><p id="job-history-status" class="publish-help">{{$P.historyChoose}}</p><ul id="job-history"></ul></section>{{end}}
` + staticPublishScript + `
{{end}}</div>`
