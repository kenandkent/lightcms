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
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/jonradoff/lightcms/v7/internal/auth"
	"github.com/jonradoff/lightcms/v7/internal/build"
	"github.com/jonradoff/lightcms/v7/internal/i18n"
	"github.com/jonradoff/lightcms/v7/internal/models"
	"github.com/jonradoff/lightcms/v7/internal/product/generation"
	"github.com/jonradoff/lightcms/v7/internal/product/idempotency"
	"github.com/jonradoff/lightcms/v7/internal/product/publication"
	"github.com/jonradoff/lightcms/v7/internal/product/storage"
	"github.com/jonradoff/lightcms/v7/internal/product/templatecontract"
	"github.com/jonradoff/lightcms/v7/internal/services"

	"github.com/gorilla/csrf"
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

// linkedCodeList renders each ID as a link to its content edit page — the
// post-merge action for a requires_publish entry is "open the page and hit
// Publish" (Wave 4C puts the button there).
func linkedCodeList(ids []string) string {
	cp := append([]string(nil), ids...)
	sort.Strings(cp)
	var b strings.Builder
	for i, id := range cp {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(`<a href="/cm/content/` + html.EscapeString(id) + `"><code>` +
			html.EscapeString(id) + "</code></a>")
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
		b.WriteString("<p>待发布： " + linkedCodeList(r.RequiresPublish) + "</p>")
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
// retryable error (spec: failed publish leaves prior URL available). Hidden
// fields passed in hidden are re-emitted inside the retry form so a retry
// replays the SAME request (idempotency key + expected_active_id) instead of
// failing validation again — `action=""` posts back to the current handler.
func FailedPublishHTML(priorURL, code string, retryable bool, hidden ...url.Values) template.HTML {
	var b strings.Builder
	b.WriteString(`<div class="notice notice-failed">`)
	b.WriteString("<p>发布失败：<code>" + html.EscapeString(code) + "</code>.</p>")
	if priorURL != "" {
		b.WriteString("<p>之前的线上 URL 仍在服务：")
		b.WriteString(`<a href="` + html.EscapeString(priorURL) + `">` + html.EscapeString(priorURL) + "</a></p>")
	}
	if retryable {
		b.WriteString(`<form method="POST" action="">`)
		for _, vals := range hidden {
			keys := make([]string, 0, len(vals))
			for k := range vals {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				for _, v := range vals[k] {
					b.WriteString(`<input type="hidden" name="` + html.EscapeString(k) +
						`" value="` + html.EscapeString(v) + `">`)
				}
			}
		}
		b.WriteString(`<button type="submit" class="btn btn-primary">重试发布</button></form>`)
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
// plus the explicit start control. The start form must post to the POST-only
// upgrade-start route for templateID — the preview URL itself is GET-only
// (main.go), so action="" would 405.
func UpgradePreviewHTML(p generation.UpgradePreview, templateID string) template.HTML {
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
	b.WriteString(`<form method="POST" action="/cm/templates/` + html.EscapeString(templateID) +
		`/upgrade-start"><button type="submit" class="btn btn-primary">启动升级任务</button></form>`)
	b.WriteString("</div>")
	return template.HTML(b.String())
}

// UpgradeJobHTML renders the durable job with per-item outcomes and
// retry/resume controls. runURL must be the explicit /cm/upgrade-jobs/{id}/run
// endpoint: posting back to the upgrade-start URL would create a NEW job
// instead of running the existing one.
func UpgradeJobHTML(j generation.UpgradeJob, runURL string) template.HTML {
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
		b.WriteString(`<form method="POST" action="` + html.EscapeString(runURL) +
			`"><button type="submit" class="btn btn-primary">重试 / 恢复失败项</button></form>`)
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

// newIdempotencyKey mints a render-time idempotency key (16 random bytes,
// hex-encoded). Because the key is generated when the page renders, a
// double-submit of the SAME page replays the completed operation instead of
// minting a second Publication. On crypto/rand failure it returns "" so the
// handler rejects the submit (IDEMPOTENCY_KEY_REQUIRED) rather than mutating
// without idempotency.
func newIdempotencyKey() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(b[:])
}

// RestoreRevertActionsHTML renders the three distinct version actions.
// activePubID is the currently-active Publication (empty when none); it is
// emitted as the expected_active_id lost-update guard on the mutating forms.
func RestoreRevertActionsHTML(contentID string, version int64, sourcePublicationID, activePubID string) template.HTML {
	hidden := func() string {
		var b strings.Builder
		b.WriteString(`<input type="hidden" name="idempotency_key" value="` +
			html.EscapeString(newIdempotencyKey()) + `">`)
		if activePubID != "" {
			b.WriteString(`<input type="hidden" name="expected_active_id" value="` +
				html.EscapeString(activePubID) + `">`)
		}
		return b.String()
	}
	var b strings.Builder
	b.WriteString(`<div class="restore-revert-actions">`)
	// Restore as draft: draft-only field restore. The registered route is
	// /revert (RevertContentVersion) — there is no restore-draft route.
	b.WriteString(`<form method="POST" action="/cm/content/` + html.EscapeString(contentID) +
		`/versions/` + strconv.FormatInt(version, 10) + `/revert" style="display:inline">`)
	b.WriteString(`<button type="submit" class="btn btn-sm btn-outline" title="仅将版本数据恢复为草稿，线上页面不受影响">恢复为草稿</button></form> `)
	b.WriteString(`<form method="POST" action="/cm/content/` + html.EscapeString(contentID) +
		`/versions/` + strconv.FormatInt(version, 10) + `/restore_and_publish" style="display:inline">`)
	b.WriteString(hidden())
	b.WriteString(`<button type="submit" class="btn btn-sm btn-primary" title="将历史版本数据重新渲染为新的发布">恢复并发布 (restore_and_publish)</button></form> `)
	if sourcePublicationID != "" {
		b.WriteString(`<form method="POST" action="/cm/content/` + html.EscapeString(contentID) +
			`/publications/` + html.EscapeString(sourcePublicationID) + `/revert_live" style="display:inline">`)
		b.WriteString(hidden())
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
		// R01: the V3 actor must carry the session role — an empty Role
		// grants nothing under Actor.Can.
		a.Role = u.Role
	}
	return a
}

// adminRequirePerm gates an Admin product handler behind one or more RBAC
// permissions (Wave 4A). The caller has already handled the anonymous
// redirect; here an authenticated session whose role lacks the permission
// gets a hard 403 — never a silent pass-through. Unknown roles hold no
// permissions and are forbidden too.
func (h *Handler) adminRequirePerm(w http.ResponseWriter, r *http.Request, perms ...string) bool {
	user, ok := h.auth.GetCurrentUser(r)
	if !ok {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return false
	}
	for _, perm := range perms {
		if !auth.HasPermission(user.Role, perm) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return false
		}
	}
	return true
}

// adminRetryHidden collects the mutating form fields a failure page must
// re-emit so the retry replays the SAME operation (idempotency key) under the
// SAME lost-update precondition (expected_active_id).
func adminRetryHidden(r *http.Request) url.Values {
	v := url.Values{}
	for _, k := range []string{"idempotency_key", "expected_active_id"} {
		if val := strings.TrimSpace(r.FormValue(k)); val != "" {
			v.Set(k, val)
		}
	}
	return v
}

// adminExpectedActive parses the expected_active_id form precondition.
// Returns (nil, true) when absent, (id, true) when valid, (nil, false) when
// present but malformed. Malformed values fail CLOSED (tampered preconditions
// must not silently downgrade to unconstrained publishes).
func adminExpectedActive(r *http.Request) (*primitive.ObjectID, bool) {
	raw := strings.TrimSpace(r.FormValue("expected_active_id"))
	if raw == "" {
		return nil, true
	}
	id, err := primitive.ObjectIDFromHex(raw)
	if err != nil {
		return nil, false
	}
	return &id, true
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

// writeAdminProductPage renders one Admin product page inside the shared
// Admin layout. The layout is template text ({{i18n}}, {{.CSRFField}}, ...),
// so it must be EXECUTED with the same request data renderAdmin supplies —
// writing it verbatim would ship the placeholders as literal page content and
// the logout form would lose its CSRF field. The layout+body shell is parsed
// once; body is trusted server-generated HTML and never re-parsed.
var (
	productPageTmplOnce sync.Once
	productPageTmpl     *template.Template
	productPageTmplErr  error
)

func productPageTemplate() (*template.Template, error) {
	productPageTmplOnce.Do(func() {
		productPageTmpl, productPageTmplErr = template.New("admin_product_page").
			Funcs(adminTemplateFuncMap).
			Parse(adminLayoutStart + `<div class="page-header"><h1>{{.Title}}</h1></div>{{.Body}}` + adminLayoutEnd)
	})
	return productPageTmpl, productPageTmplErr
}

// stampCSRFTokens inserts the gorilla/csrf hidden field into every POST form
// of the rendered body. The publication forms are plain-HTML helpers with no
// {{.CSRFField}} slot, and the /cm router (main.go csrf.Protect) rejects
// token-less POSTs with 403. An empty token (no middleware, e.g. direct
// handler calls in tests) stamps nothing.
func stampCSRFTokens(body, token string) string {
	if token == "" {
		return body
	}
	field := `<input type="hidden" name="gorilla.csrf.Token" value="` + html.EscapeString(token) + `">`
	const open = `<form method="POST"`
	var b strings.Builder
	rest := body
	for {
		i := strings.Index(rest, open)
		if i < 0 {
			b.WriteString(rest)
			return b.String()
		}
		j := strings.IndexByte(rest[i:], '>')
		if j < 0 {
			b.WriteString(rest)
			return b.String()
		}
		end := i + j + 1
		b.WriteString(rest[:end])
		b.WriteString(field)
		rest = rest[end:]
	}
}

func (h *Handler) writeAdminProductPage(w http.ResponseWriter, r *http.Request, title, body string) {
	body = stampCSRFTokens(body, csrf.Token(r))
	tmpl, err := productPageTemplate()
	if err == nil {
		data := map[string]interface{}{
			"Title":                title,
			"Body":                 template.HTML(body),
			"IsAuthenticated":      h.auth.IsAuthenticated(r),
			"Lang":                 i18n.LangFromRequest(r),
			"CSRFToken":            csrf.Token(r),
			"CSRFField":            csrf.TemplateField(r),
			"AppVersion":           build.GetVersion(),
			"CopilotEnabled":       false,
			"UnreadMessageCount":   0,
			"PendingApprovalCount": 0,
		}
		if user, ok := h.auth.GetCurrentUser(r); ok {
			data["CurrentUser"] = user
			data["CopilotEnabled"] = h.anthropicAPIKey != "" && auth.HasPermission(user.Role, auth.PermContentEdit)
		}
		data["UnreadMessageCount"], _ = h.db.Count(r.Context(), "contact_messages", bson.M{"read": false})
		if h.approvalService != nil {
			data["PendingApprovalCount"] = h.approvalService.CountPending(r.Context())
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		if err := tmpl.Execute(w, data); err != nil {
			log.Printf("admin product page %q execute error: %v", title, err)
		}
		return
	}
	// Layout failed to parse (should be impossible): fall back to the legacy
	// raw composition rather than serving an empty page.
	log.Printf("admin product page layout parse error: %v", err)
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
	if !h.adminRequirePerm(w, r, auth.PermContentEdit, auth.PermContentPublish) {
		return
	}
	vars := mux.Vars(r)
	contentID, err := primitive.ObjectIDFromHex(vars["id"])
	if err != nil {
		http.Error(w, "Invalid ID", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	// Stamp editor identity + provenance (human/ui) so the minted
	// Publication record attributes this admin publish (session rollback
	// selects targets by provenance).
	adminAuthor := false
	if u, ok := h.auth.GetCurrentUser(r); ok {
		ctx = services.WithEditorEmail(ctx, u.Email)
		adminAuthor = u.Role == "admin"
	}
	ctx = services.WithProvenance(ctx, services.Provenance{Actor: "human", Via: "ui"})
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
		h.writeAdminProductPage(w, r, "发布", string(FailedPublishHTML(
			h.adminPriorURL(r, contentID, content.FullPath), "TEMPLATE_VERSION_NOT_FOUND", false,
			adminRetryHidden(r))))
		return
	}
	expected, ok := adminExpectedActive(r)
	if !ok {
		h.writeAdminProductPage(w, r, "发布", string(FailedPublishHTML(
			h.adminPriorURL(r, contentID, content.FullPath), "EXPECTED_ACTIVE_ID_INVALID", false)))
		return
	}
	svc := h.adminPublicationService()

	// Idempotent admin publish (M6): the form carries a render-time
	// idempotency_key. Same key + same body replays the cached result
	// instead of minting a second Publication; an expired lease is taken
	// over and resumed from the durable snapshot. Unwired installs (no
	// idempotency service) keep the legacy direct publish.
	var opID *primitive.ObjectID
	var opAttempt int64
	if h.idempotencyService != nil {
		out := h.adminPublishIdem(ctx, r, content, tv.ID, expected)
		if out.failCode != "" {
			h.writeAdminProductPage(w, r, "发布", string(FailedPublishHTML(
				h.adminPriorURL(r, contentID, content.FullPath), out.failCode,
				out.failRetryable, adminRetryHidden(r))))
			return
		}
		if out.replay != nil {
			h.writeAdminProductPage(w, r, "发布", string(PublishResultHTML(
				PublishDisplay{Result: *out.replay, TemplateVersion: tv.Version})))
			return
		}
		opID, opAttempt = &out.op.ID, out.op.Attempt
		if out.resume {
			// Takeover resume: the form's expectation may be stale (the
			// crashed attempt could have committed). Re-read the live
			// active as the CAS expectation — ActivateCAS still enforces
			// it atomically, so a concurrent publish conflicts instead of
			// being silently superseded.
			if live, lerr := publication.NewRepository(h.db, nil).GetActive(ctx, contentID); lerr == nil && live != nil {
				id := live.ID
				expected = &id
			} else {
				expected = nil
			}
		}
	}
	res, perr := svc.Publish(ctx, publication.PublishRequest{
		ContentID: contentID, ContentVersion: content.CurrentVersion,
		TemplateVersionID: tv.ID, ExpectedActiveID: expected, Reason: "admin publish",
		Actor: "human", Via: "ui", AuthorIsAdmin: adminAuthor, IdempotencyRecord: opID,
	})
	if perr != nil {
		if opID != nil {
			// Release (never complete) the lease so the failure-page
			// retry can take over the same attempt.
			_, _ = h.idempotencyService.Complete(ctx, *opID, opAttempt,
				adminPublishErrStatus(perr), map[string]any{"code": publication.CodeOf(perr)}, false)
		}
		h.writeAdminProductPage(w, r, "发布", string(FailedPublishHTML(
			h.adminPriorURL(r, contentID, content.FullPath), publication.CodeOf(perr),
			PublishErrorRetryable(perr), adminRetryHidden(r))))
		return
	}
	if opID != nil {
		cache := map[string]any{
			"publication_id": res.PublicationID.Hex(), "public_url": res.PublicURL,
			"full_path": res.FullPath, "content_version": float64(res.ContentVersion),
			"content_id": res.ContentID.Hex(),
		}
		if _, cerr := h.idempotencyService.Complete(ctx, *opID, opAttempt, 200, cache, false); cerr != nil {
			// A completed publish that fails to cache is still a success —
			// the page is live. Surface the result; the retry will replay
			// or take over.
			log.Printf("admin publish: cache completion failed for op %s: %v", opID.Hex(), cerr)
		}
	}
	h.writeAdminProductPage(w, r, "发布", string(PublishResultHTML(PublishDisplay{Result: res, TemplateVersion: tv.Version})))
}

// adminIdemOutcome is the resolved idempotency posture for an admin publish.
type adminIdemOutcome struct {
	op            idempotency.Operation // valid when proceeding
	resume        bool                  // takeover resume: re-read the live active
	replay        *publication.PublicationResult
	failCode      string
	failRetryable bool
}

// adminPublishIdem Begins (or takes over) the idempotency operation for an
// admin publish POST carrying a render-time idempotency_key.
func (h *Handler) adminPublishIdem(ctx context.Context, r *http.Request, content models.Content, tvID primitive.ObjectID, expected *primitive.ObjectID) adminIdemOutcome {
	key := strings.TrimSpace(r.FormValue("idempotency_key"))
	if key == "" {
		// Fail closed: the form always renders a key; a missing key means
		// a hand-built POST that would mint unrepeatable publications.
		return adminIdemOutcome{failCode: generation.CodeIdempotencyKeyRequired}
	}
	owner := "admin-ui"
	if u, ok := h.auth.GetCurrentUser(r); ok {
		if u.ID != "" {
			owner = "admin-ui:" + u.ID
		} else if u.Email != "" {
			owner = "admin-ui:" + u.Email
		}
	}
	expHex := ""
	if expected != nil {
		expHex = expected.Hex()
	}
	raw, _ := json.Marshal(map[string]any{
		"content_id": content.ID.Hex(), "content_version": content.CurrentVersion,
		"template_version_id": tvID.Hex(), "expected_active_id": expHex,
	})
	opPath := "/cm/content/" + content.ID.Hex() + "/publish"
	op, berr := h.idempotencyService.Begin(ctx, owner, "POST", opPath, key, raw)
	if berr == nil {
		if op.Replay {
			if res, ok := h.adminCachedPublish(ctx, op); ok {
				return adminIdemOutcome{replay: res}
			}
			return adminIdemOutcome{failCode: string(publication.CodeInternal)}
		}
		return adminIdemOutcome{op: op}
	}
	if idempotency.CodeOf(berr) != idempotency.CodeLeaseExpired {
		return adminIdemOutcome{
			failCode:      adminIdemBeginCode(berr),
			failRetryable: idempotency.CodeOf(berr) == idempotency.CodeInProgress,
		}
	}
	// Expired lease: CAS-takeover and resume from the durable snapshot
	// instead of wedging on REQUEST_IN_PROGRESS.
	top, terr := h.idempotencyService.TakeOverByKey(ctx, owner, "POST", opPath, key)
	if terr != nil {
		if top.Replay {
			if res, ok := h.adminCachedPublish(ctx, top); ok {
				return adminIdemOutcome{replay: res}
			}
			return adminIdemOutcome{failCode: string(publication.CodeInternal)}
		}
		return adminIdemOutcome{
			failCode:      adminIdemBeginCode(terr),
			failRetryable: idempotency.CodeOf(terr) == idempotency.CodeInProgress,
		}
	}
	if top.PublicationID != nil {
		// The crashed attempt may already have committed: if its
		// publication is active, cache the response and return it —
		// never mint a second Publication.
		if pub, gerr := publication.NewRepository(h.db, nil).GetByID(ctx, *top.PublicationID); gerr == nil && pub != nil && pub.Status == publication.StatusActive {
			res := publication.PublicationResult{
				PublicationID: pub.ID, ContentID: pub.ContentID,
				ContentVersion: pub.ContentVersion, FullPath: pub.FullPath,
				PublicURL: pub.PublicURL, ContentHash: pub.ContentHash,
			}
			_, _ = h.idempotencyService.Complete(ctx, top.ID, top.Attempt, 200, map[string]any{
				"publication_id": pub.ID.Hex(), "public_url": pub.PublicURL,
				"full_path": pub.FullPath, "content_version": float64(pub.ContentVersion),
				"content_id": pub.ContentID.Hex(),
			}, false)
			return adminIdemOutcome{replay: &res}
		}
	}
	return adminIdemOutcome{op: top, resume: true}
}

// adminCachedPublish rebuilds the success display from a replayed
// idempotency response.
func (h *Handler) adminCachedPublish(ctx context.Context, op idempotency.Operation) (*publication.PublicationResult, bool) {
	m := op.Response
	if m == nil {
		return nil, false
	}
	str := func(k string) string {
		if v, ok := m[k].(string); ok {
			return v
		}
		return ""
	}
	num := func(k string) int64 {
		switch v := m[k].(type) {
		case float64:
			return int64(v)
		case int64:
			return v
		case int:
			return int64(v)
		}
		return 0
	}
	pubID, err := primitive.ObjectIDFromHex(str("publication_id"))
	if err != nil {
		return nil, false
	}
	contentID, err := primitive.ObjectIDFromHex(str("content_id"))
	if err != nil {
		return nil, false
	}
	return &publication.PublicationResult{
		PublicationID: pubID, ContentID: contentID,
		ContentVersion: num("content_version"), FullPath: str("full_path"),
		PublicURL: str("public_url"),
	}, true
}

// adminIdemBeginCode maps idempotency Begin failures to admin failure codes.
func adminIdemBeginCode(err error) string {
	switch idempotency.CodeOf(err) {
	case idempotency.CodeConflict:
		return idempotency.CodeConflict
	case idempotency.CodeInProgress:
		return idempotency.CodeInProgress
	default:
		return string(publication.CodeInternal)
	}
}

// adminPublishErrStatus maps saga failures to a release-only completion
// status (the lease is released, never completed, so the retry takes over).
func adminPublishErrStatus(err error) int {
	switch publication.CodeOf(err) {
	case string(publication.CodeConflict):
		return 409
	case string(publication.CodePagePublishInProgress):
		return 429
	default:
		return 500
	}
}

// AdminProductPublications lists publication history with active/failed state
// and rollback actions (spec §31.5).
func (h *Handler) AdminProductPublications(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsAuthenticated(r) {
		http.Redirect(w, r, "/cm/login", http.StatusSeeOther)
		return
	}
	if !h.adminRequirePerm(w, r, auth.PermContentView) {
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
	// The active Publication is the expected_active_id the mutating forms
	// guard against a lost update; fetch it once for the whole page.
	activePubID := ""
	if active, aerr := repo.GetActive(r.Context(), contentID); aerr == nil && active != nil {
		activePubID = active.ID.Hex()
	}
	var b strings.Builder
	b.WriteString("<h3>发布历史</h3>")
	b.WriteString(`<table><thead><tr><th>ID</th><th>状态</th><th>内容版本</th><th>模板版本</th><th>操作</th></tr></thead><tbody>`)
	for _, p := range history {
		b.WriteString("<tr><td><code>" + html.EscapeString(p.ID.Hex()) + "</code></td><td>" +
			html.EscapeString(string(p.Status)) + "</td><td>" + strconv.FormatInt(p.ContentVersion, 10) +
			"</td><td>" + strconv.FormatInt(p.TemplateVersion, 10) + "</td><td>" +
			string(RestoreRevertActionsHTML(contentID.Hex(), p.ContentVersion, p.ID.Hex(), activePubID)) + "</td></tr>")
	}
	b.WriteString("</tbody></table>")
	h.writeAdminProductPage(w, r, "发布记录", b.String())
}

// AdminProductUpgradePreview renders the read-only template upgrade preview
// (never regenerates live pages).
func (h *Handler) AdminProductUpgradePreview(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsAuthenticated(r) {
		http.Redirect(w, r, "/cm/login", http.StatusSeeOther)
		return
	}
	if !h.adminRequirePerm(w, r, auth.PermTemplateView) {
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
	h.writeAdminProductPage(w, r, "升级预览", string(UpgradePreviewHTML(prev, tmpl.ID.Hex())))
}

// AdminProductUpgradeStart creates the durable upgrade job (no live pages
// touched until Run).
func (h *Handler) AdminProductUpgradeStart(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsAuthenticated(r) {
		http.Redirect(w, r, "/cm/login", http.StatusSeeOther)
		return
	}
	if !h.adminRequirePerm(w, r, auth.PermTemplateEdit, auth.PermContentPublish) {
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
	h.writeAdminProductPage(w, r, "升级任务", string(UpgradeJobHTML(job,
		"/cm/upgrade-jobs/"+job.ID.Hex()+"/run")))
}

// AdminProductUpgradeRun processes pending/failed job items via
// PublicationService.Publish (one new Publication per page) with retry/resume.
func (h *Handler) AdminProductUpgradeRun(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsAuthenticated(r) {
		http.Redirect(w, r, "/cm/login", http.StatusSeeOther)
		return
	}
	if !h.adminRequirePerm(w, r, auth.PermTemplateEdit, auth.PermContentPublish) {
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
	h.writeAdminProductPage(w, r, "升级任务", string(UpgradeJobHTML(job,
		"/cm/upgrade-jobs/"+jobID.Hex()+"/run")))
}

// AdminProductRestoreAndPublish re-renders historical version data into a NEW
// Publication (spec §18.4). Distinct from RevertLive.
func (h *Handler) AdminProductRestoreAndPublish(w http.ResponseWriter, r *http.Request) {
	if !h.auth.IsAuthenticated(r) {
		http.Redirect(w, r, "/cm/login", http.StatusSeeOther)
		return
	}
	if !h.adminRequirePerm(w, r, auth.PermContentEdit, auth.PermContentPublish) {
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
	idemKey := strings.TrimSpace(r.FormValue("idempotency_key"))
	genCtx := generation.WithIdempotency(ctx, generation.IdempotencyParams{
		Owner: actor.Owner(), Method: "POST",
		Path: "/cm/content/" + contentID.Hex() + "/versions/" + vars["version"] + "/restore_and_publish",
		Key:  idemKey,
	})
	if idemKey == "" {
		h.writeAdminProductPage(w, r, "恢复并发布", string(FailedPublishHTML("", "IDEMPOTENCY_KEY_REQUIRED", false)))
		return
	}
	// Same lost-update guard AdminProductPublish honors: a stale
	// expected_active_id rejects the restore instead of racing a newer
	// activation.
	expected, ok := adminExpectedActive(r)
	if !ok {
		h.writeAdminProductPage(w, r, "恢复并发布", string(FailedPublishHTML("", "EXPECTED_ACTIVE_ID_INVALID", false)))
		return
	}
	res, err := h.adminGenerationService().RestoreAndPublish(genCtx, actor, contentID, version, expected)
	if err != nil {
		var prior string
		var probe models.Content
		if derr := h.db.FindOne(ctx, "content", bson.M{"_id": contentID}, &probe); derr == nil {
			prior = h.adminPriorURL(r, contentID, probe.FullPath)
		}
		h.writeAdminProductPage(w, r, "恢复并发布", string(FailedPublishHTML(prior,
			generation.CodeOf(err), PublishErrorRetryable(err), adminRetryHidden(r))))
		return
	}
	h.writeAdminProductPage(w, r, "恢复并发布", string(PublishResultHTML(PublishDisplay{
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
	if !h.adminRequirePerm(w, r, auth.PermContentEdit, auth.PermContentPublish) {
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
	idemKey := strings.TrimSpace(r.FormValue("idempotency_key"))
	if idemKey == "" {
		h.writeAdminProductPage(w, r, "回滚线上", string(FailedPublishHTML("", "IDEMPOTENCY_KEY_REQUIRED", false)))
		return
	}
	actor := h.adminActor(r)
	ctx := r.Context()
	genCtx := generation.WithIdempotency(ctx, generation.IdempotencyParams{
		Owner: actor.Owner(), Method: "POST",
		Path: "/cm/content/" + contentID.Hex() + "/publications/" + sourceID.Hex() + "/revert_live",
		Key:  idemKey,
	})
	expected, ok := adminExpectedActive(r)
	if !ok {
		h.writeAdminProductPage(w, r, "回滚线上", string(FailedPublishHTML("", "EXPECTED_ACTIVE_ID_INVALID", false)))
		return
	}
	res, err := h.adminGenerationService().RevertLive(genCtx, actor, contentID, sourceID, expected)
	if err != nil {
		var prior string
		var probe models.Content
		if derr := h.db.FindOne(ctx, "content", bson.M{"_id": contentID}, &probe); derr == nil {
			prior = h.adminPriorURL(r, contentID, probe.FullPath)
		}
		h.writeAdminProductPage(w, r, "回滚线上", string(FailedPublishHTML(prior,
			generation.CodeOf(err), PublishErrorRetryable(err), adminRetryHidden(r))))
		return
	}
	h.writeAdminProductPage(w, r, "回滚线上", string(PublishResultHTML(PublishDisplay{
		Result: publication.PublicationResult{
			ContentVersion: res.ContentVersion, FullPath: res.FullPath, PublicURL: res.PublicURL,
		}, TemplateVersion: 0,
	})))
}
