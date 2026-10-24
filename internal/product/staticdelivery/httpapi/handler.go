// Package httpapi is shared by authenticated REST and session/CSRF admin routes.
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gorilla/mux"
	"github.com/metavert-io/lightcms/v7/internal/auth"
	sd "github.com/metavert-io/lightcms/v7/internal/product/staticdelivery"
	ex "github.com/metavert-io/lightcms/v7/internal/product/staticexport"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

type Handlers struct {
	Commands       *sd.CommandService
	Exports        *ex.Service
	ActorExtractor func(*http.Request) (*auth.SessionUser, bool)
}

func Register(r *mux.Router, h *Handlers) {
	r.HandleFunc("/static-delivery/options", h.options).Methods("GET")
	r.HandleFunc("/static-exports", h.export).Methods("POST")
	r.HandleFunc("/static-exports/{id}", h.exportStatus).Methods("GET")
	r.HandleFunc("/static-exports/{id}/download", h.download).Methods("GET")
	r.HandleFunc("/static-targets/{id}/deployment-preview", h.preview).Methods("POST")
	r.HandleFunc("/static-targets/{id}/recovery-preview", h.recoveryPreview).Methods("POST")
	r.HandleFunc("/static-targets/{id}/recover", h.recover).Methods("POST")
	r.HandleFunc("/static-recovery-jobs/{id}", h.recoveryJob).Methods("GET")
	r.HandleFunc("/static-preview-jobs/{id}", h.previewJob).Methods("GET")
	r.HandleFunc("/static-targets/{id}/deployments", h.deploy).Methods("POST")
	r.HandleFunc("/static-targets/{id}/deployments", h.history).Methods("GET")
	r.HandleFunc("/static-deployments/{id}", h.job).Methods("GET")
	r.HandleFunc("/static-deployments/{id}/cancel", h.cancel).Methods("POST")
}
func (h *Handlers) recoveryPreview(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r, true)
	if !ok || !h.ready(w) {
		return
	}
	target, ok := id(w, r)
	if !ok {
		return
	}
	var request struct{}
	if !decode(w, r, &request) {
		return
	}
	proof, err := h.Commands.PreviewRecovery(r.Context(), a, target)
	if err != nil {
		fail(w, err)
		return
	}
	reply(w, 200, proof)
}
func (h *Handlers) recover(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r, true)
	if !ok || !h.ready(w) {
		return
	}
	target, ok := id(w, r)
	if !ok {
		return
	}
	var request struct {
		Digest    string `json:"confirmation_digest"`
		Confirmed bool   `json:"confirmed"`
	}
	if !decode(w, r, &request) {
		return
	}
	job, err := h.Commands.SubmitRecovery(r.Context(), a, target, request.Digest, request.Confirmed)
	if err != nil {
		fail(w, err)
		return
	}
	reply(w, 202, job)
}
func (h *Handlers) recoveryJob(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r, true)
	if !ok || !h.ready(w) {
		return
	}
	jobID, ok := id(w, r)
	if !ok {
		return
	}
	result, err := h.Commands.GetRecoveryJob(r.Context(), a, jobID)
	if err != nil {
		fail(w, err)
		return
	}
	reply(w, 200, result)
}
func (h *Handlers) history(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r, true)
	if !ok || !h.ready(w) {
		return
	}
	v, ok := id(w, r)
	if !ok {
		return
	}
	limit, offset := int64(20), int64(0)
	var e error
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, e = strconv.ParseInt(raw, 10, 64)
		if e != nil {
			reply(w, 400, map[string]any{"code": sd.CodeInvalidRequest})
			return
		}
	}
	if raw := r.URL.Query().Get("offset"); raw != "" {
		offset, e = strconv.ParseInt(raw, 10, 64)
		if e != nil {
			reply(w, 400, map[string]any{"code": sd.CodeInvalidRequest})
			return
		}
	}
	jobs, e := h.Commands.History(r.Context(), a, v, offset, limit)
	if e != nil {
		fail(w, e)
		return
	}
	reply(w, 200, map[string]any{"jobs": jobs, "limit": limit, "offset": offset})
}
func (h *Handlers) cancel(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r, false)
	if !ok {
		return
	}
	var q struct{}
	if !decode(w, r, &q) || !h.ready(w) {
		return
	}
	v, ok := id(w, r)
	if !ok {
		return
	}
	j, e := h.Commands.Cancel(r.Context(), a, v, r.Header.Get("Idempotency-Key"))
	if e != nil {
		fail(w, e)
		return
	}
	reply(w, 200, map[string]any{"job": j})
}
func reply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, err error) {
	message := err.Error()
	if sd.CodeOf(err) == sd.CodeInternal {
		message = "静态发布内部操作未完成，请联系管理员并提供任务ID"
	}
	reply(w, sd.StatusForError(err), map[string]any{"error": true, "code": sd.CodeOf(err), "message": message})
}
func (h *Handlers) actor(w http.ResponseWriter, r *http.Request, publish bool) (sd.ActorSnapshot, bool) {
	var u *auth.SessionUser
	var ok bool
	if h.ActorExtractor != nil {
		u, ok = h.ActorExtractor(r)
	}
	if !ok || u == nil || u.SandboxOnly || !auth.UserHasPermission(u, auth.PermContentView) || (publish && !auth.UserHasPermission(u, auth.PermContentPublish)) {
		reply(w, 403, map[string]any{"error": true, "code": sd.CodePermissionDenied, "message": "需要内容查看及对应发布权限"})
		return sd.ActorSnapshot{}, false
	}
	return sd.ActorSnapshot{UserID: u.ID, Owner: u.OperationOwner(), Role: u.Role, Scopes: u.Scopes}, true
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		reply(w, 400, map[string]any{"code": sd.CodeInvalidRequest, "message": "请求字段或JSON格式不正确"})
		return false
	}
	if e := d.Decode(new(any)); !errors.Is(e, io.EOF) {
		reply(w, 400, map[string]any{"code": sd.CodeInvalidRequest, "message": "请求必须只有一个JSON值"})
		return false
	}
	return true
}
func (h *Handlers) ready(w http.ResponseWriter) bool {
	if h.Commands == nil {
		reply(w, 503, map[string]any{"code": sd.CodeModuleUnavailable, "message": "内容静态发布模块暂不可用"})
		return false
	}
	return true
}
func id(w http.ResponseWriter, r *http.Request) (primitive.ObjectID, bool) {
	v, e := primitive.ObjectIDFromHex(mux.Vars(r)["id"])
	if e != nil {
		reply(w, 400, map[string]any{"code": sd.CodeInvalidRequest, "message": "记录ID不正确"})
		return v, false
	}
	return v, true
}
func accepted(w http.ResponseWriter, j sd.Job) {
	reply(w, 202, map[string]any{"operation_id": j.OperationID, "job_id": j.ID, "export_id": j.ExportID, "state": j.State, "status_url": "/api/v1/static-deployments/" + j.ID.Hex()})
}
func (h *Handlers) export(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r, false)
	if !ok {
		return
	}
	var q struct {
		Items []ex.Item `json:"items"`
	}
	if !decode(w, r, &q) || !h.ready(w) {
		return
	}
	j, e := h.Commands.SubmitExport(r.Context(), a, r.Header.Get("Idempotency-Key"), q.Items)
	if e != nil {
		fail(w, e)
		return
	}
	accepted(w, j)
}
func (h *Handlers) exportStatus(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r, false)
	if !ok || !h.ready(w) {
		return
	}
	v, ok := id(w, r)
	if !ok {
		return
	}
	x, e := h.Commands.GetExport(r.Context(), a, v)
	if e != nil {
		fail(w, e)
		return
	}
	reply(w, 200, x)
}
func (h *Handlers) preview(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r, true)
	if !ok {
		return
	}
	q, valid := decodeDeployment(w, r)
	if !valid || !h.ready(w) {
		return
	}
	v, ok := id(w, r)
	if !ok {
		return
	}
	if strings.Contains(r.Header.Get("Prefer"), "respond-async") && h.Commands.AsyncPreviewAvailable() {
		x, e := h.Commands.SubmitPreview(r.Context(), a, r.Header.Get("Idempotency-Key"), v, q)
		if e != nil {
			fail(w, e)
			return
		}
		reply(w, http.StatusAccepted, x)
		return
	}
	x, e := h.Commands.PreviewDeployment(r.Context(), a, v, q)
	if e != nil {
		fail(w, e)
		return
	}
	reply(w, 200, x)
}
func (h *Handlers) deploy(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r, true)
	if !ok {
		return
	}
	q, valid := decodeDeployment(w, r)
	if !valid || !h.ready(w) {
		return
	}
	v, ok := id(w, r)
	if !ok {
		return
	}
	j, e := h.Commands.SubmitDeployment(r.Context(), a, r.Header.Get("Idempotency-Key"), v, q)
	if e != nil {
		fail(w, e)
		return
	}
	accepted(w, j)
}

func (h *Handlers) previewJob(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r, true)
	if !ok || !h.ready(w) {
		return
	}
	v, ok := id(w, r)
	if !ok {
		return
	}
	result, err := h.Commands.GetPreviewJob(r.Context(), a, v)
	if err != nil {
		fail(w, err)
		return
	}
	reply(w, http.StatusOK, result)
}

// Zero is a valid initial site revision; distinguish it from an omitted/null
// precondition instead of silently treating a missing field as revision zero.
func decodeDeployment(w http.ResponseWriter, r *http.Request) (sd.DeployRequest, bool) {
	var wire struct {
		QueueMode              string               `json:"queue_mode,omitempty"`
		Operation              string               `json:"operation,omitempty"`
		Paths                  []string             `json:"paths,omitempty"`
		SourceReleaseID        string               `json:"source_release_id,omitempty"`
		NewOrigin              string               `json:"new_origin,omitempty"`
		NewEnvironment         string               `json:"new_environment,omitempty"`
		ExportIDs              []primitive.ObjectID `json:"export_ids"`
		ExpectedRevision       *int64               `json:"expected_revision"`
		ExpectedConfigRevision *int64               `json:"expected_config_revision"`
		ConfirmationDigest     string               `json:"confirmation_digest,omitempty"`
		ManualScriptReview     bool                 `json:"manual_script_review"`
	}
	if !decode(w, r, &wire) {
		return sd.DeployRequest{}, false
	}
	if wire.ExpectedRevision == nil || wire.ExpectedConfigRevision == nil {
		reply(w, 428, map[string]any{"code": sd.CodeConfirmationRequired, "message": "必须明确提供 expected_revision 和 expected_config_revision"})
		return sd.DeployRequest{}, false
	}
	return sd.DeployRequest{QueueMode: wire.QueueMode, Operation: wire.Operation, Paths: wire.Paths, SourceReleaseID: wire.SourceReleaseID, NewOrigin: wire.NewOrigin, NewEnvironment: wire.NewEnvironment, ExportIDs: wire.ExportIDs, ExpectedRevision: *wire.ExpectedRevision, ExpectedConfigRevision: *wire.ExpectedConfigRevision, ConfirmationDigest: wire.ConfirmationDigest, ManualScriptReview: wire.ManualScriptReview}, true
}
func (h *Handlers) job(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r, false)
	if !ok || !h.ready(w) {
		return
	}
	v, ok := id(w, r)
	if !ok {
		return
	}
	j, e := h.Commands.GetJob(r.Context(), a, v)
	if e != nil {
		fail(w, e)
		return
	}
	out := map[string]any{"job": j}
	if j.Kind != "export" {
		links, e := h.Commands.JobLinks(r.Context(), a, j.ID)
		if e == nil {
			out["pages"] = links
		}
	}
	reply(w, 200, out)
}
func (h *Handlers) options(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r, true)
	if !ok || !h.ready(w) {
		return
	}
	x, e := h.Commands.Options(r.Context(), a)
	if e != nil {
		fail(w, e)
		return
	}
	reply(w, 200, x)
}
func (h *Handlers) download(w http.ResponseWriter, r *http.Request) {
	a, ok := h.actor(w, r, false)
	if !ok || !h.ready(w) {
		return
	}
	v, ok := id(w, r)
	if !ok {
		return
	}
	record, e := h.Commands.GetExport(r.Context(), a, v)
	if e != nil {
		fail(w, e)
		return
	}
	if record.State != "ready" || h.Exports == nil {
		reply(w, 410, map[string]any{"code": sd.CodeArtifactUnavailable, "message": "导出包尚未就绪或不可用"})
		return
	}
	artifact, e := h.Exports.Load(r.Context(), v)
	if e != nil {
		reply(w, 410, map[string]any{"code": sd.CodeArtifactUnavailable, "message": "导出文件缺失或校验失败"})
		return
	}
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "zip"
	}
	if format != "zip" && format != "html" {
		reply(w, 400, map[string]any{"code": sd.CodeInvalidRequest})
		return
	}
	if format == "html" && (len(artifact.Manifest.Pages) != 1 || len(artifact.Manifest.Files) != 1 || len(artifact.Manifest.External) > 0) {
		reply(w, 422, map[string]any{"code": sd.CodeDependencyUnresolved, "message": "该报告有独立资源，请下载ZIP"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", `attachment; filename="report-`+v.Hex()+`.`+format+`"`)
	if format == "html" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = h.Exports.WriteHTML(r.Context(), v, w)
	} else {
		w.Header().Set("Content-Type", "application/zip")
		_ = h.Exports.WriteZIP(r.Context(), v, w)
	}
}
