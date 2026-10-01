// Admin Editor and Publication UX (plan Task 15, Lane H; spec §8.4, §9.5,
// §12.3, §12.6, §13, §16.6, §18.4–§18.5, §20.4, §31).
//
// This file owns the Admin presentation layer for template-driven pages. It
// calls the SAME shared services as REST and never redefines them:
//   - templatecontract.ValidateData for field errors (Task 4);
//   - templatecontract.TemplateVersion / models.TemplateField shapes (Task 3);
//   - generation.UpgradePreview/UpgradeJob + RestoreAndPublish/RevertLive
//     service methods (Task 12) via the shared generation.Service wired
//     once in cmd/server/main.go (Task 16C);
//   - publication.Publish/Rollback via the Task 8 saga (Admin publish goes
//     through PublicationService, never GenerateStaticPage).
//
// No SPA and no second frontend: all output is server-rendered HTML using the
// existing Admin layout constants, and all mutations go through the
// in-process product services. Routes are registered in cmd/server/main.go
// (Task 16C); the handlers below are additive methods on *Handler sharing
// the runtime in handlers.go.
package handlers

import (
	"fmt"
	"html"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/generation"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/storage"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"
	"github.com/jonradoff/lightcms/v7/internal/services"

	"github.com/gorilla/mux"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// ---------------------------------------------------------------------------
// Template selector (spec §31.2): thumbnail, name, category, description,
// required fields.
// ---------------------------------------------------------------------------

// RequiredFieldNames returns the required field names of tmpl in definition
// order. It reads the shared models.Template shape owned by Task 2.
func RequiredFieldNames(tmpl models.Template) []string {
	var out []string
	for _, f := range tmpl.Fields {
		if f.Required {
			out = append(out, f.Name)
		}
	}
	return out
}

// TemplateSelectorCardHTML renders one template picker card: name, category,
// description and required fields (spec §31.2).
func TemplateSelectorCardHTML(tmpl models.Template) template.HTML {
	var b strings.Builder
	b.WriteString(`<a href="/cm/content/new/` + html.EscapeString(tmpl.ID.Hex()) + `" class="template-card">`)
	b.WriteString("<h3>" + html.EscapeString(tmpl.Name) + "</h3>")
	if tmpl.Category != "" {
		b.WriteString(`<span class="template-category">` + html.EscapeString(tmpl.Category) + "</span>")
	}
	if tmpl.Description != "" {
		b.WriteString("<p>" + html.EscapeString(tmpl.Description) + "</p>")
	}
	if req := RequiredFieldNames(tmpl); len(req) > 0 {
		b.WriteString(`<p class="template-required">必填字段：` + html.EscapeString(strings.Join(req, ", ")) + "</p>")
	}
	b.WriteString("</a>")
	return template.HTML(b.String())
}

// ---------------------------------------------------------------------------
// Product field inputs (spec §8.4): url, number, boolean, date, image,
// richtext, markdown (+ legacy text/textarea/rawhtml/select) with shared
// Task 4 validation errors surfaced inline (spec §11.4).
// ---------------------------------------------------------------------------

// FieldErrorsFor returns the shared diagnostics for one field.
func FieldErrorsFor(field string, errs []templatecontract.FieldError) []templatecontract.FieldError {
	var out []templatecontract.FieldError
	for _, e := range errs {
		if e.Field == field {
			out = append(out, e)
		}
	}
	return out
}

// ValidateAdminData runs the SAME shared validator as REST/generation so the
// Admin form and the API can never disagree (spec §11.4).
func ValidateAdminData(v templatecontract.TemplateVersion, data map[string]any) ([]templatecontract.FieldError, []templatecontract.FieldWarning) {
	return templatecontract.ValidateData(v, data)
}

// fieldErrorHTML renders shared FieldError diagnostics for one field.
func fieldErrorHTML(field string, errs []templatecontract.FieldError) string {
	var b strings.Builder
	for _, e := range FieldErrorsFor(field, errs) {
		b.WriteString(`<p class="field-error" data-field="` + html.EscapeString(field) + `">`)
		b.WriteString(html.EscapeString(e.Code) + ": " + html.EscapeString(e.Message))
		b.WriteString("</p>")
	}
	return b.String()
}

// fieldHelpHTML renders description/example help (spec §31: field help).
func fieldHelpHTML(f models.TemplateField) string {
	var b strings.Builder
	if f.Description != "" {
		b.WriteString(`<p class="help-text">` + html.EscapeString(f.Description) + "</p>")
	}
	if f.Example != "" {
		b.WriteString(`<p class="help-text">示例：<code>` + html.EscapeString(f.Example) + "</code></p>")
	}
	return b.String()
}

func stringValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprint(t)
	}
}

// RenderProductField renders the Admin input for one template field,
// including the MVP-new url/number/boolean types (spec §8.4). errs are the
// shared templatecontract diagnostics; matching ones render inline.
func RenderProductField(f models.TemplateField, value any, errs []templatecontract.FieldError) template.HTML {
	var b strings.Builder
	id := "field_" + f.Name
	val := stringValue(value)
	req := ""
	if f.Required {
		req = " required"
	}
	label := f.Label
	if label == "" {
		label = f.Name
	}
	b.WriteString(`<div class="form-group">`)
	b.WriteString(`<label for="` + html.EscapeString(id) + `">` + html.EscapeString(label))
	if f.Required {
		b.WriteString(" *")
	}
	b.WriteString("</label>")
	ph := ""
	if f.Placeholder != "" {
		ph = ` placeholder="` + html.EscapeString(f.Placeholder) + `"`
	}
	switch f.Type {
	case "url":
		b.WriteString(`<input type="url" id="` + html.EscapeString(id) + `" name="` + html.EscapeString(id) +
			`" value="` + html.EscapeString(val) + `"` + ph + req + ">")
	case "number":
		b.WriteString(`<input type="number" id="` + html.EscapeString(id) + `" name="` + html.EscapeString(id) +
			`" value="` + html.EscapeString(val) + `"` + ph + req + ">")
	case "boolean":
		checked := ""
		if v, ok := value.(bool); ok && v {
			checked = " checked"
		} else if _, ok := value.(bool); !ok && f.Default == "true" && value == nil {
			checked = " checked"
		}
		// Hidden+checkbox pattern: unchecked boxes still submit a value so the
		// server can distinguish "explicit false" from "field missing".
		b.WriteString(`<input type="hidden" name="` + html.EscapeString(id) + `" value="off">`)
		b.WriteString(`<label class="checkbox-label"><input type="checkbox" id="` + html.EscapeString(id) +
			`" name="` + html.EscapeString(id) + `" value="on"` + checked + "> " + html.EscapeString(label) + "</label>")
	case "date":
		b.WriteString(`<input type="date" id="` + html.EscapeString(id) + `" name="` + html.EscapeString(id) +
			`" value="` + html.EscapeString(val) + `"` + req + ">")
	case "image":
		if val != "" {
			b.WriteString(`<div class="current-image"><img src="` + html.EscapeString(val) +
				`" alt="当前图片" style="max-width: 200px; margin-bottom: 0.5rem;"></div>`)
		}
		b.WriteString(`<input type="file" id="` + html.EscapeString(id) + `" name="` + html.EscapeString(id) + `" accept="image/*">`)
	case "richtext":
		b.WriteString(`<textarea id="` + html.EscapeString(id) + `" name="` + html.EscapeString(id) +
			`" class="richtext" data-field-type="richtext"` + req + ">" + html.EscapeString(val) + "</textarea>")
	case "markdown":
		b.WriteString(`<textarea id="` + html.EscapeString(id) + `" name="` + html.EscapeString(id) +
			`" rows="10" data-field-type="markdown"` + ph + req + ">" + html.EscapeString(val) + "</textarea>")
	case "textarea":
		b.WriteString(`<textarea id="` + html.EscapeString(id) + `" name="` + html.EscapeString(id) +
			`" rows="4"` + ph + req + ">" + html.EscapeString(val) + "</textarea>")
	case "rawhtml":
		b.WriteString(`<textarea id="` + html.EscapeString(id) + `" name="` + html.EscapeString(id) +
			`" rows="12" class="code-editor"` + ph + req + ">" + html.EscapeString(val) + "</textarea>")
	case "select":
		b.WriteString(`<select id="` + html.EscapeString(id) + `" name="` + html.EscapeString(id) + `"` + req + ">")
		b.WriteString(`<option value="">请选择…</option>`)
		for _, opt := range strings.Split(f.Options, ",") {
			opt = strings.TrimSpace(opt)
			if opt == "" {
				continue
			}
			sel := ""
			if opt == val {
				sel = " selected"
			}
			b.WriteString(`<option value="` + html.EscapeString(opt) + `"` + sel + ">" + html.EscapeString(opt) + "</option>")
		}
		b.WriteString("</select>")
	default: // text and any legacy type fall back to a text input.
		b.WriteString(`<input type="text" id="` + html.EscapeString(id) + `" name="` + html.EscapeString(id) +
			`" value="` + html.EscapeString(val) + `"` + ph + req + ">")
	}
	b.WriteString(fieldHelpHTML(f))
	b.WriteString(fieldErrorHTML(f.Name, errs))
	b.WriteString("</div>")
	return template.HTML(b.String())
}

// ParseAdminFieldData converts posted Admin form values into product data.
// values and present are keyed by full input name ("field_<name>"); present
// reports r.Form key presence for boolean checkboxes.
// Boolean semantics follow spec §8.4: an optional boolean whose checkbox is
// absent stays missing (no key); a field with Default "true" materializes
// true on create.
func ParseAdminFieldData(fields []models.TemplateField, values map[string]string, present map[string]bool) map[string]any {
	out := make(map[string]any, len(fields))
	for _, f := range fields {
		key := "field_" + f.Name
		if f.Type == "boolean" {
			if !present[key] {
				if strings.EqualFold(strings.TrimSpace(f.Default), "true") {
					out[f.Name] = true
				}
				continue
			}
			v := strings.ToLower(strings.TrimSpace(values[key]))
			out[f.Name] = v == "on" || v == "true" || v == "1"
			continue
		}
		if f.Type == "number" {
			raw := strings.TrimSpace(values[key])
			if raw == "" {
				continue
			}
			if n, err := strconv.ParseFloat(raw, 64); err == nil {
				out[f.Name] = n
			} else {
				out[f.Name] = raw // shared validator reports the mismatch.
			}
			continue
		}
		if v, ok := values[key]; ok {
			out[f.Name] = v
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Published-page edit routing (spec §12.3, §31.3): unpublished edits Content,
// published edits create/reuse an active Fork Draft; the UI states live is
// unchanged and offers Diff/Review/Merge/Publish.
// ---------------------------------------------------------------------------

// SelectForkAction routes an Admin edit: unpublished content edits in place,
// published content opens (or reuses) a Fork Draft. The notice copy states
// the live page is unchanged.
func SelectForkAction(published bool, existingForkID string) (action, notice string) {
	if !published {
		return "edit_content", ""
	}
	if existingForkID != "" {
		return "reuse_fork", "正在编辑草稿——线上页面不受影响。继续编辑分支 " + existingForkID + "。"
	}
	return "open_fork", "正在编辑草稿——线上页面不受影响。保存将创建分支草稿，发布是单独的操作。"
}

// ForkEditBannerHTML renders the draft-vs-live banner for the edit page.
func ForkEditBannerHTML(published bool, forkID string) template.HTML {
	if !published {
		return template.HTML(`<div class="notice notice-draft">正在编辑未发布草稿。</div>`)
	}
	msg := "正在编辑草稿——线上页面不受影响。"
	if forkID != "" {
		msg += " 分支：" + forkID + "。"
	}
	msg += "保存不会覆盖线上页面，请先合并再发布。"
	return template.HTML(`<div class="notice notice-fork">` + html.EscapeString(msg) + "</div>")
}

// ---------------------------------------------------------------------------
// Fork merge display (spec §12.6): merge drafts, publishing is separate.
// Merge creates versions and requires_publish entries; canonical HTML is
// untouched until an explicit Publish.
// ---------------------------------------------------------------------------

// ForkMergeDisplay mirrors the product ForkMergeResult display subset: per the
// shared contract, published merges land in RequiresPublish and the merge
// never rewrites live bytes.
type ForkMergeDisplay struct {
	Updated         int
	Created         int
	ContentIDs      []string
	RequiresPublish []string
}

func codeList(ids []string) string {
	cp := append([]string(nil), ids...)
	sort.Strings(cp)
	var b strings.Builder
	for i, id := range cp {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(`<code>` + html.EscapeString(id) + "</code>")
	}
	return b.String()
}

// MergeResultHTML renders the merge outcome: requires_publish entries plus an
// explicit statement that canonical HTML is unchanged pending Publish.
func MergeResultHTML(r ForkMergeDisplay) template.HTML {
	var b strings.Builder
	b.WriteString(`<div class="notice notice-merge">`)
	b.WriteString("<p>已合并 " + strconv.Itoa(r.Updated) + " 个更新、" + strconv.Itoa(r.Created) + " 个新建。")
	b.WriteString("合并的是草稿；发布是单独的操作。</p>")
	if len(r.RequiresPublish) > 0 {
		b.WriteString("<p>待发布： " + codeList(r.RequiresPublish) + "</p>")
	}
	b.WriteString("<p>在发布之前，线上正式 HTML 不会变化。</p>")
	b.WriteString("</div>")
	return template.HTML(b.String())
}

// ---------------------------------------------------------------------------
// Publish result display (spec §16.6, §31.5): Public URL, Publication ID,
// Content/Template versions. Failure preserves the prior URL with a retryable
// error; the failed Publication is never activated (Task 8 saga guarantee).
// ---------------------------------------------------------------------------

// PublishDisplay pairs a Task 8 PublicationResult with the resolved template
// version for Admin display.
type PublishDisplay struct {
	Result          publication.PublicationResult
	TemplateVersion int64
}

// PublishResultHTML renders a successful publish: URL, IDs, versions.
func PublishResultHTML(d PublishDisplay) template.HTML {
	var b strings.Builder
	b.WriteString(`<div class="notice notice-published">`)
	b.WriteString("<p>已发布。公开 URL： ")
	b.WriteString(`<a href="` + html.EscapeString(d.Result.PublicURL) + `">` + html.EscapeString(d.Result.PublicURL) + "</a></p>")
	b.WriteString("<ul>")
	b.WriteString("<li>发布 ID：<code>" + html.EscapeString(d.Result.PublicationID.Hex()) + "</code></li>")
	b.WriteString("<li>内容 ID：<code>" + html.EscapeString(d.Result.ContentID.Hex()) + "</code></li>")
	b.WriteString("<li>内容版本：" + strconv.FormatInt(d.Result.ContentVersion, 10) + "</li>")
	b.WriteString("<li>模板版本：" + strconv.FormatInt(d.TemplateVersion, 10) + "</li>")
	b.WriteString("<li>路径：<code>" + html.EscapeString(d.Result.FullPath) + "</code></li>")
	b.WriteString("</ul></div>")
	return template.HTML(b.String())
}

// publishRetryableCodes are the failure stages after which a retry with a NEW
// Publication is safe (the saga marks the attempt failed and keeps the old
// active + canonical).
var publishRetryableCodes = map[string]bool{
	"PUBLICATION_STAGE_FAILED":    true,
	"PUBLICATION_VERIFY_FAILED":   true,
	"PUBLICATION_ACTIVATE_FAILED": true,
	"PAGE_PUBLISH_IN_PROGRESS":    true,
}

// PublishErrorRetryable reports whether an Admin publish failure is retryable.
func PublishErrorRetryable(err error) bool {
	if err == nil {
		return false
	}
	if code := publication.CodeOf(err); code != "" {
		return publishRetryableCodes[code]
	}
	if code := generation.CodeOf(err); code != "" {
		return generation.Retryable(code)
	}
	return false
}

// FailedPublishHTML keeps the prior live URL available and shows the
// retryable error (spec: failed publish leaves prior URL available).
func FailedPublishHTML(priorURL, code string, retryable bool) template.HTML {
	var b strings.Builder
	b.WriteString(`<div class="notice notice-failed">`)
	b.WriteString("<p>发布失败：<code>" + html.EscapeString(code) + "</code>.</p>")
	if priorURL != "" {
		b.WriteString("<p>之前的线上 URL 仍在服务：")
		b.WriteString(`<a href="` + html.EscapeString(priorURL) + `">` + html.EscapeString(priorURL) + "</a></p>")
	}
	if retryable {
		b.WriteString(`<form method="POST" action=""><button type="submit" class="btn btn-primary">重试发布</button></form>`)
	} else {
		b.WriteString("<p>请修复上述错误后重试。</p>")
	}
	b.WriteString("</div>")
	return template.HTML(b.String())
}

// ---------------------------------------------------------------------------
// Template versioning notice (spec §9.5, §31.4): HTML change creates a new
// immutable TemplateVersion and never regenerates live pages; upgrade is an
// explicit job.
// ---------------------------------------------------------------------------

// TemplateVersionNoticeHTML states that a template save created a new version
// while existing live pages are untouched pending an explicit upgrade.
func TemplateVersionNoticeHTML(oldVersion, newVersion int64) template.HTML {
	msg := fmt.Sprintf("模板已保存为版本 %d（原 %d）。现有线上页面不受影响，请先运行升级预览，再启动正式升级任务重新发布。", newVersion, oldVersion)
	return template.HTML(`<div class="notice notice-version">` + html.EscapeString(msg) +
		` <a href="" class="btn btn-sm btn-outline">升级预览</a></div>`)
}

// ---------------------------------------------------------------------------
// Upgrade Preview (read-only) + durable Upgrade Job UI (Task 12 contract).
// Preview writes nothing; the job publishes per page via PublicationService
// with per-item outcomes and retry/resume.
// ---------------------------------------------------------------------------

// UpgradePreviewHTML renders the read-only preview: what WOULD republish,
// plus the explicit start control.
func UpgradePreviewHTML(p generation.UpgradePreview) template.HTML {
	var b strings.Builder
	b.WriteString(`<div class="upgrade-preview">`)
	b.WriteString("<h3>升级预览： " + html.EscapeString(p.Template) + " v" +
		strconv.FormatInt(p.FromVersion, 10) + " → v" + strconv.FormatInt(p.ToVersion, 10) + "</h3>")
	b.WriteString("<p>" + strconv.Itoa(p.WouldRepublish) + " of " + strconv.Itoa(p.TotalPages) + " 个页面需要重新发布。仅预览——线上页面不受影响。</p>")
	b.WriteString(`<table><thead><tr><th>路径</th><th>当前模板</th><th>目标</th><th>结果</th></tr></thead><tbody>`)
	items := append([]generation.UpgradePreviewItem(nil), p.Items...)
	sort.Slice(items, func(i, j int) bool { return items[i].FullPath < items[j].FullPath })
	for _, it := range items {
		outcome := "已是最新"
		if it.WouldRepublish {
			outcome = "需要重新发布"
		} else if it.ValidationErrors > 0 {
			outcome = "已拦截：存在校验错误"
		}
		b.WriteString("<tr><td><code>" + html.EscapeString(it.FullPath) + "</code></td><td>v" +
			strconv.FormatInt(it.ActiveTemplateV, 10) + "</td><td>v" + strconv.FormatInt(it.TargetTemplateV, 10) +
			"</td><td>" + html.EscapeString(outcome) + "</td></tr>")
	}
	b.WriteString("</tbody></table>")
	b.WriteString(`<form method="POST" action=""><button type="submit" class="btn btn-primary">启动升级任务</button></form>`)
	b.WriteString("</div>")
	return template.HTML(b.String())
}

// UpgradeJobHTML renders the durable job with per-item outcomes and
// retry/resume controls.
func UpgradeJobHTML(j generation.UpgradeJob) template.HTML {
	var b strings.Builder
	b.WriteString(`<div class="upgrade-job">`)
	b.WriteString("<h3>升级任务 <code>" + html.EscapeString(j.ID.Hex()) + "</code> — " + html.EscapeString(string(j.Status)) + "</h3>")
	b.WriteString("<p>Template " + html.EscapeString(j.TemplateSlug) + " v" + strconv.FormatInt(j.FromVersion, 10) +
		" → v" + strconv.FormatInt(j.ToVersion, 10) + "。每个页面生成独立的发布记录，可恢复任务以重试失败项。</p>")
	b.WriteString(`<table><thead><tr><th>路径</th><th>状态</th><th>尝试次数</th><th>详情</th></tr></thead><tbody>`)
	items := append([]generation.UpgradeJobItem(nil), j.Items...)
	sort.Slice(items, func(i, j int) bool { return items[i].FullPath < items[j].FullPath })
	for _, it := range items {
		detail := ""
		if it.PublicationID != nil {
			detail = "发布 <code>" + html.EscapeString(it.PublicationID.Hex()) + "</code>"
		} else if it.Error != "" {
			detail = html.EscapeString(it.Error)
		}
		b.WriteString("<tr><td><code>" + html.EscapeString(it.FullPath) + "</code></td><td>" +
			html.EscapeString(string(it.Status)) + "</td><td>" + strconv.Itoa(it.Attempts) + "</td><td>" + detail + "</td></tr>")
	}
	b.WriteString("</tbody></table>")
	if j.Status == generation.UpgradeJobRunning || j.Status == generation.UpgradeJobPartial {
		b.WriteString(`<form method="POST" action=""><button type="submit" class="btn btn-primary">重试 / 恢复失败项</button></form>`)
	}
	b.WriteString("</div>")
	return template.HTML(b.String())
}

// ---------------------------------------------------------------------------
// Restore vs revert (spec §13, §18.4–§18.5): separate buttons, distinct
// outcomes. Restore as Draft only recovers edit state; Restore and Publish
// re-renders a historical ContentVersion into a NEW Publication; Revert Live
// (exact rollback) restores a historical Publication's retained bytes into a
// NEW Publication.
// ---------------------------------------------------------------------------

// RestoreRevertActionsHTML renders the three distinct version actions.
func RestoreRevertActionsHTML(contentID string, version int64, sourcePublicationID string) template.HTML {
	var b strings.Builder
	b.WriteString(`<div class="restore-revert-actions">`)
	b.WriteString(`<form method="POST" action="/cm/content/` + html.EscapeString(contentID) +
		`/versions/` + strconv.FormatInt(version, 10) + `/restore-draft" style="display:inline">`)
	b.WriteString(`<button type="submit" class="btn btn-sm btn-outline" title="仅将版本数据恢复为草稿，线上页面不受影响">恢复为草稿</button></form> `)
	b.WriteString(`<form method="POST" action="/cm/content/` + html.EscapeString(contentID) +
		`/versions/` + strconv.FormatInt(version, 10) + `/restore_and_publish" style="display:inline">`)
	b.WriteString(`<button type="submit" class="btn btn-sm btn-primary" title="将历史版本数据重新渲染为新的发布">恢复并发布 (restore_and_publish)</button></form> `)
	if sourcePublicationID != "" {
		b.WriteString(`<form method="POST" action="/cm/content/` + html.EscapeString(contentID) +
			`/publications/` + html.EscapeString(sourcePublicationID) + `/revert_live" style="display:inline">`)
		b.WriteString(`<button type="submit" class="btn btn-sm btn-secondary" title="将该历史发布的完整保留字节恢复为新的发布">回滚线上到该发布 (revert_live)</button></form>`)
		b.WriteString("<p class=\"help-text\">restore_and_publish 重新渲染草稿数据，revert_live 恢复完整保留字节。</p>")
	}
	b.WriteString("</div>")
	return template.HTML(b.String())
}

// ---------------------------------------------------------------------------
// Admin HTTP handlers. Same services as REST: generation.Service for
// draft/publish/upgrade/restore/revert orchestration and the Task 8
// publication saga for live activation. Pre-Task-16 these are reachable only
// when the integration owner registers routes; behavior and display match the
// REST contract already.
// ---------------------------------------------------------------------------

// adminActor builds a generation.Actor from the Admin session user.
func (h *Handler) adminActor(r *http.Request) generation.Actor {
	a := generation.Actor{Authenticated: true, Via: "ui", ActorKind: "human"}
	if u, ok := h.auth.GetCurrentUser(r); ok {
		a.ID = u.ID
		a.Email = u.Email
		a.IsAdmin = u.Role == "admin"
	}
	return a
}

// adminGenerationService builds the same orchestrator REST uses (Task 12).
// Task 16C: prefer the shared runtime wired once in main.go; the local
// construction below is the unwired (unit-test) fallback only.
func (h *Handler) adminGenerationService() *generation.Service {
	if h.generationService != nil {
		return h.generationService
	}
	return generation.NewService(h.db, generation.Options{})
}

// adminPublicationService builds the Task 8 saga over the MVP filesystem
// store (spec §17.2). Task 16C wires the shared runtime in main.go via
// SetPublicationRuntime; the local construction is the unwired fallback.
func (h *Handler) adminPublicationService() *publication.Service {
	if h.publicationService != nil {
		return h.publicationService
	}
	repo := publication.NewRepository(h.db, nil)
	store := storage.NewFilesystemStore("content")
	return publication.NewService(h.db, repo, store, publication.Options{BuildSHA: "admin"})
}

// adminPriorURL returns the best-effort current live URL for failure display:
// the active Publication's URL, else the content path under baseURL. It never
// fails the request — failed publish must still show a URL when one exists.
func (h *Handler) adminPriorURL(r *http.Request, contentID primitive.ObjectID, fullPath string) string {
	repo := publication.NewRepository(h.db, nil)
	if active, err := repo.GetActive(r.Context(), contentID); err == nil && active != nil {
		// Publication has no stored URL field in this build; resolve from the
		// canonical path under the configured base URL.
		_ = active
	}
	base := strings.TrimSuffix(h.baseURL, "/")
	if fullPath == "" {
		return base
	}
	if !strings.HasPrefix(fullPath, "/") {
		fullPath = "/" + fullPath
	}
	return base + fullPath
}

func (h *Handler) writeAdminProductPage(w http.ResponseWriter, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, adminLayoutStart+`<div class="page-header"><h1>`+html.EscapeString(title)+
		"</h1></div>"+body+adminLayoutEnd)
}

// AdminProductPublish publishes one content item through PublicationService
// and displays the Publication ID + Public URL (spec §16.6, §31.5). It pins
// the current content/template versions and honors an optional
// expected_active_id precondition; failures preserve the prior URL.
func (h *Handler) AdminProductPublish(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsAuthenticated(r) {
		http.Redirect(w, r, "/cm/login", http.StatusSeeOther)
		return
	}
	vars := mux.Vars(r)
	contentID, err := primitive.ObjectIDFromHex(vars["id"])
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	var content models.Content
	if err := h.db.FindOne(ctx, "content", bson.M{"_id": contentID}, &content); err != nil {
		http.Error(w, "Content not found", http.StatusNotFound)
		return
	}
	var tmpl models.Template
	if err := h.db.FindOne(ctx, "templates", bson.M{"_id": content.TemplateID}, &tmpl); err != nil {
		http.Error(w, "Template not found", http.StatusNotFound)
		return
	}
	tv, err := templatecontract.NewService(h.db).GetCurrent(ctx, tmpl.Slug)
	if err != nil {
		h.writeAdminProductPage(w, "发布", string(FailedPublishHTML(
			h.adminPriorURL(r, contentID, content.FullPath), "TEMPLATE_VERSION_NOT_FOUND", false)))
		return
	}
	var expected *primitive.ObjectID
	if raw := strings.TrimSpace(r.FormValue("expected_active_id")); raw != "" {
		if id, err := primitive.ObjectIDFromHex(raw); err == nil {
			expected = &id
		}
	}
	svc := h.adminPublicationService()
	res, perr := svc.Publish(ctx, publication.PublishRequest{
		ContentID: contentID, ContentVersion: content.CurrentVersion,
		TemplateVersionID: tv.ID, ExpectedActiveID: expected, Reason: "admin publish",
	})
	if perr != nil {
		h.writeAdminProductPage(w, "发布", string(FailedPublishHTML(
			h.adminPriorURL(r, contentID, content.FullPath), publication.CodeOf(perr), PublishErrorRetryable(perr))))
		return
	}
	h.writeAdminProductPage(w, "发布", string(PublishResultHTML(PublishDisplay{Result: res, TemplateVersion: tv.Version})))
}

// AdminProductPublications lists publication history with active/failed state
// and rollback actions (spec §31.5).
func (h *Handler) AdminProductPublications(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsAuthenticated(r) {
		http.Redirect(w, r, "/cm/login", http.StatusSeeOther)
		return
	}
	vars := mux.Vars(r)
	contentID, err := primitive.ObjectIDFromHex(vars["id"])
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	repo := publication.NewRepository(h.db, nil)
	history, err := repo.ListHistory(r.Context(), contentID)
	if err != nil {
		http.Error(w, "Failed to load publications", http.StatusInternalServerError)
		return
	}
	var b strings.Builder
	b.WriteString("<h3>发布历史</h3>")
	b.WriteString(`<table><thead><tr><th>ID</th><th>状态</th><th>内容版本</th><th>模板版本</th><th>操作</th></tr></thead><tbody>`)
	for _, p := range history {
		b.WriteString("<tr><td><code>" + html.EscapeString(p.ID.Hex()) + "</code></td><td>" +
			html.EscapeString(string(p.Status)) + "</td><td>" + strconv.FormatInt(p.ContentVersion, 10) +
			"</td><td>" + strconv.FormatInt(p.TemplateVersion, 10) + "</td><td>" +
			string(RestoreRevertActionsHTML(contentID.Hex(), p.ContentVersion, p.ID.Hex())) + "</td></tr>")
	}
	b.WriteString("</tbody></table>")
	h.writeAdminProductPage(w, "发布记录", b.String())
}

// AdminProductUpgradePreview renders the read-only template upgrade preview
// (never regenerates live pages).
func (h *Handler) AdminProductUpgradePreview(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsAuthenticated(r) {
		http.Redirect(w, r, "/cm/login", http.StatusSeeOther)
		return
	}
	vars := mux.Vars(r)
	tmplID, err := primitive.ObjectIDFromHex(vars["id"])
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	var tmpl models.Template
	if err := h.db.FindOne(r.Context(), "templates", bson.M{"_id": tmplID}, &tmpl); err != nil {
		http.Error(w, "Template not found", http.StatusNotFound)
		return
	}
	prev, err := h.adminGenerationService().PreviewUpgrade(r.Context(), h.adminActor(r), tmpl.Slug)
	if err != nil {
		http.Error(w, "Upgrade preview failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.writeAdminProductPage(w, "升级预览", string(UpgradePreviewHTML(prev)))
}

// AdminProductUpgradeStart creates the durable upgrade job (no live pages
// touched until Run).
func (h *Handler) AdminProductUpgradeStart(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsAuthenticated(r) {
		http.Redirect(w, r, "/cm/login", http.StatusSeeOther)
		return
	}
	vars := mux.Vars(r)
	tmplID, err := primitive.ObjectIDFromHex(vars["id"])
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	var tmpl models.Template
	if err := h.db.FindOne(r.Context(), "templates", bson.M{"_id": tmplID}, &tmpl); err != nil {
		http.Error(w, "Template not found", http.StatusNotFound)
		return
	}
	job, err := h.adminGenerationService().StartUpgradeJob(r.Context(), h.adminActor(r), tmpl.Slug)
	if err != nil {
		http.Error(w, "Start upgrade job failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.writeAdminProductPage(w, "升级任务", string(UpgradeJobHTML(job)))
}

// AdminProductUpgradeRun processes pending/failed job items via
// PublicationService.Publish (one new Publication per page) with retry/resume.
func (h *Handler) AdminProductUpgradeRun(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsAuthenticated(r) {
		http.Redirect(w, r, "/cm/login", http.StatusSeeOther)
		return
	}
	jobID, err := primitive.ObjectIDFromHex(mux.Vars(r)["jobID"])
	if err != nil {
		http.Error(w, "Invalid job ID", http.StatusBadRequest)
		return
	}
	job, err := h.adminGenerationService().RunUpgradeJob(r.Context(), h.adminActor(r), jobID)
	if err != nil {
		http.Error(w, "Run upgrade job failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	h.writeAdminProductPage(w, "升级任务", string(UpgradeJobHTML(job)))
}

// AdminProductRestoreAndPublish re-renders historical version data into a NEW
// Publication (spec §18.4). Distinct from RevertLive.
func (h *Handler) AdminProductRestoreAndPublish(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsAuthenticated(r) {
		http.Redirect(w, r, "/cm/login", http.StatusSeeOther)
		return
	}
	vars := mux.Vars(r)
	contentID, err := primitive.ObjectIDFromHex(vars["id"])
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	version, err := strconv.ParseInt(vars["version"], 10, 64)
	if err != nil {
		http.Error(w, "Invalid version", http.StatusBadRequest)
		return
	}
	ctx := services.WithProvenance(r.Context(), services.Provenance{Actor: "human", Via: "ui"})
	if u, ok := h.auth.GetCurrentUser(r); ok {
		ctx = services.WithEditorEmail(ctx, u.Email)
	}
	actor := h.adminActor(r)
	genCtx := generation.WithIdempotency(ctx, generation.IdempotencyParams{
		Owner: actor.Owner(), Method: "POST",
		Path: "/cm/content/" + contentID.Hex() + "/versions/" + vars["version"] + "/restore_and_publish",
		Key:  strings.TrimSpace(r.FormValue("idempotency_key")),
	})
	if strings.TrimSpace(r.FormValue("idempotency_key")) == "" {
		h.writeAdminProductPage(w, "恢复并发布", string(FailedPublishHTML("", "IDEMPOTENCY_KEY_REQUIRED", false)))
		return
	}
	res, err := h.adminGenerationService().RestoreAndPublish(genCtx, actor, contentID, version, nil)
	if err != nil {
		var prior string
		var probe models.Content
		if derr := h.db.FindOne(ctx, "content", bson.M{"_id": contentID}, &probe); derr == nil {
			prior = h.adminPriorURL(r, contentID, probe.FullPath)
		}
		h.writeAdminProductPage(w, "恢复并发布", string(FailedPublishHTML(prior, generation.CodeOf(err), PublishErrorRetryable(err))))
		return
	}
	h.writeAdminProductPage(w, "恢复并发布", string(PublishResultHTML(PublishDisplay{
		Result: publication.PublicationResult{
			ContentVersion: res.ContentVersion, FullPath: res.FullPath, PublicURL: res.PublicURL,
		}, TemplateVersion: 0,
	})))
}

// AdminProductRevertLive performs the exact rollback: a NEW Publication
// restoring the source publication's retained bytes (spec §18.5). Distinct
// from RestoreAndPublish.
func (h *Handler) AdminProductRevertLive(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsAuthenticated(r) {
		http.Redirect(w, r, "/cm/login", http.StatusSeeOther)
		return
	}
	vars := mux.Vars(r)
	contentID, err := primitive.ObjectIDFromHex(vars["id"])
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	sourceID, err := primitive.ObjectIDFromHex(vars["publicationID"])
	if err != nil {
		http.Error(w, "Invalid publication ID", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(r.FormValue("idempotency_key")) == "" {
		h.writeAdminProductPage(w, "回滚线上", string(FailedPublishHTML("", "IDEMPOTENCY_KEY_REQUIRED", false)))
		return
	}
	actor := h.adminActor(r)
	ctx := r.Context()
	genCtx := generation.WithIdempotency(ctx, generation.IdempotencyParams{
		Owner: actor.Owner(), Method: "POST",
		Path: "/cm/content/" + contentID.Hex() + "/publications/" + sourceID.Hex() + "/revert_live",
		Key:  strings.TrimSpace(r.FormValue("idempotency_key")),
	})
	res, err := h.adminGenerationService().RevertLive(genCtx, actor, contentID, sourceID, nil)
	if err != nil {
		var prior string
		var probe models.Content
		if derr := h.db.FindOne(ctx, "content", bson.M{"_id": contentID}, &probe); derr == nil {
			prior = h.adminPriorURL(r, contentID, probe.FullPath)
		}
		h.writeAdminProductPage(w, "回滚线上", string(FailedPublishHTML(prior, generation.CodeOf(err), PublishErrorRetryable(err))))
		return
	}
	h.writeAdminProductPage(w, "回滚线上", string(PublishResultHTML(PublishDisplay{
		Result: publication.PublicationResult{
			ContentVersion: res.ContentVersion, FullPath: res.FullPath, PublicURL: res.PublicURL,
		}, TemplateVersion: 0,
	})))
}
