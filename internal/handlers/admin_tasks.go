package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/metavert-io/lightcms/v7/internal/product/jobs"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Task views deliberately project metadata only. They never deserialize job
// inputs, credentials, release bodies or arbitrary executor result documents.
type taskRow struct {
	ID, Kind, State, ErrorCode, Link string
	CreatedAt                        time.Time
	Completed, Total                 int
}
type taskSource struct{ Collection, State, Kind string }

var taskSources = map[string]taskSource{
	"workflow":  {"workflow_jobs", "state", "workflow"},
	"delivery":  {"static_delivery_jobs", "state", "delivery"},
	"imports":   {"import_jobs", "status", "import"},
	"upgrades":  {"template_upgrade_jobs", "status", "template_upgrade"},
	"approvals": {"approval_requests", "status", "approval"},
}

func (h *Handler) AdminTasksPage(w http.ResponseWriter, r *http.Request) {
	if !h.deliveryAdminAllowed(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	source := r.URL.Query().Get("source")
	if source == "" {
		source = "workflow"
	}
	spec, ok := taskSources[source]
	if !ok {
		http.Error(w, "未知任务分类", 400)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	timeField := "created_at"
	if source == "imports" {
		timeField = "started_at"
	}
	filter := bson.M{}
	if before := r.URL.Query().Get("before"); before != "" {
		id, err := primitive.ObjectIDFromHex(before)
		if err != nil {
			http.Error(w, "游标格式错误", 400)
			return
		}
		var anchor struct {
			CreatedAt time.Time `bson:"created_at"`
			StartedAt time.Time `bson:"started_at"`
		}
		if err = h.db.Collection(spec.Collection).FindOne(ctx, bson.M{"_id": id}, options.FindOne().SetProjection(bson.M{timeField: 1})).Decode(&anchor); err != nil {
			http.Error(w, "任务游标已失效，请返回第一页", 400)
			return
		}
		if source == "imports" {
			anchor.CreatedAt = anchor.StartedAt
		}
		filter["$or"] = bson.A{bson.M{timeField: bson.M{"$lt": anchor.CreatedAt}}, bson.M{timeField: anchor.CreatedAt, "_id": bson.M{"$lt": id}}}
	}
	projection := bson.M{"_id": 1, "kind": 1, spec.State: 1, timeField: 1, "completed": 1, "total": 1, "error_code": 1, "workflow_id": 1}
	cursor, err := h.db.Collection(spec.Collection).Find(ctx, filter, options.Find().SetProjection(projection).SetSort(bson.D{{Key: timeField, Value: -1}, {Key: "_id", Value: -1}}).SetLimit(51))
	if err != nil {
		http.Error(w, "暂时无法读取任务", 503)
		return
	}
	defer cursor.Close(ctx)
	rows := []taskRow{}
	for cursor.Next(ctx) {
		var record struct {
			ID         primitive.ObjectID `bson:"_id"`
			Kind       string             `bson:"kind"`
			State      string             `bson:"state"`
			Status     string             `bson:"status"`
			CreatedAt  time.Time          `bson:"created_at"`
			StartedAt  time.Time          `bson:"started_at"`
			Completed  int                `bson:"completed"`
			Total      int                `bson:"total"`
			ErrorCode  string             `bson:"error_code"`
			WorkflowID primitive.ObjectID `bson:"workflow_id"`
		}
		if err = cursor.Decode(&record); err != nil {
			http.Error(w, "任务记录无法读取", 503)
			return
		}
		if record.Kind == "" {
			record.Kind = spec.Kind
		}
		if source == "imports" {
			record.CreatedAt = record.StartedAt
		}
		if record.State == "" {
			record.State = record.Status
		}
		row := taskRow{ID: record.ID.Hex(), Kind: record.Kind, State: record.State, CreatedAt: record.CreatedAt, Completed: record.Completed, Total: record.Total, ErrorCode: record.ErrorCode}
		row.Link = "/cm/tasks?source=" + source + "&job_id=" + row.ID
		if !record.WorkflowID.IsZero() {
			row.Link = "/cm/tasks?source=workflow&job_id=" + record.WorkflowID.Hex()
		}
		if source == "delivery" {
			row.Link = "/cm/static-delivery/publish?job_id=" + row.ID
		}
		if source == "workflow" {
			row.Link = "/cm/tasks?source=workflow&job_id=" + row.ID
		}
		rows = append(rows, row)
	}
	if cursor.Err() != nil {
		http.Error(w, "任务读取中断", 503)
		return
	}
	data := map[string]any{"Tasks": rows, "Source": source}
	if rawID := r.URL.Query().Get("job_id"); rawID != "" && source != "workflow" && source != "delivery" {
		id, err := primitive.ObjectIDFromHex(rawID)
		if err != nil {
			http.Error(w, "任务标识错误", 400)
			return
		}
		var legacy bson.M
		fields := bson.M{"_id": 1, "status": 1, timeField: 1, "error_msg": 1, "execution_error": 1, "workflow_id": 1, "total_pages": 1, "created": 1, "updated": 1, "failed": 1, "skipped": 1, "template_id": 1, "to_version": 1, "subject_id": 1, "subject_type": 1}
		if err := h.db.Collection(spec.Collection).FindOne(ctx, bson.M{"_id": id}, options.FindOne().SetProjection(fields)).Decode(&legacy); err != nil {
			status := http.StatusServiceUnavailable
			if errors.Is(err, mongo.ErrNoDocuments) {
				status = http.StatusNotFound
			}
			http.Error(w, "任务记录不存在或暂不可用", status)
			return
		}
		data["LegacyDetail"] = legacy
	}
	if len(rows) > 50 {
		data["Tasks"] = rows[:50]
		data["Next"] = fmt.Sprintf("/cm/tasks?source=%s&before=%s", source, rows[49].ID)
	}
	if rawID := r.URL.Query().Get("job_id"); rawID != "" && source == "workflow" {
		id, err := primitive.ObjectIDFromHex(rawID)
		if err != nil {
			http.Error(w, "任务标识错误", 400)
			return
		}
		repo := jobs.NewRepository(h.db, 1)
		job, err := repo.Get(ctx, id)
		if err != nil {
			http.Error(w, "任务不存在", 404)
			return
		}
		// Job's JSON/HTML view contains no frozen inputs or credentials.
		data["Detail"] = taskRow{ID: id.Hex(), Kind: job.Kind, State: job.State, ErrorCode: job.ErrorCode, CreatedAt: job.CreatedAt, Completed: job.Completed, Total: job.Total}
		data["SourceRef"], data["Deadline"], data["LeaseUntil"] = job.InputRef, job.Deadline, job.LeaseUntil
		if job.Kind == "delivery_preview" {
			data["ResumeLink"] = "/cm/static-delivery/publish?preview_job_id=" + id.Hex()
		}
		if job.Kind == "delivery_recovery" {
			data["ResumeLink"] = "/cm/static-delivery/maintenance?recovery_job_id=" + id.Hex()
		}
		itemFilter := bson.M{"job_id": id}
		if after := r.URL.Query().Get("after_item"); after != "" {
			if len(after) > 2048 {
				http.Error(w, "条目游标过长", 400)
				return
			}
			itemFilter["key"] = bson.M{"$gt": after}
		}
		items, err := h.db.Collection("workflow_items").Find(ctx, itemFilter, options.Find().SetProjection(bson.M{"_id": 0, "key": 1, "done": 1, "result.content_id": 1, "result.path": 1, "result.error": 1, "result.failed": 1}).SetSort(bson.D{{Key: "key", Value: 1}}).SetLimit(101))
		if err != nil {
			http.Error(w, "无法读取逐项进度", 503)
			return
		}
		var itemRows []bson.M
		err = items.All(ctx, &itemRows)
		items.Close(ctx)
		if err != nil {
			http.Error(w, "无法读取逐项进度", 503)
			return
		}
		if len(itemRows) > 100 {
			after, _ := itemRows[99]["key"].(string)
			data["ItemNext"] = "/cm/tasks?source=workflow&job_id=" + id.Hex() + "&after_item=" + url.QueryEscape(after)
			itemRows = itemRows[:100]
		}
		data["Items"] = itemRows
		events, err := h.db.Collection("workflow_events").Find(ctx, bson.M{"job_id": id}, options.Find().SetProjection(bson.M{"at": 1, "state": 1, "code": 1}).SetSort(bson.D{{Key: "at", Value: -1}}).SetLimit(100))
		if err != nil {
			http.Error(w, "无法读取任务事件", 503)
			return
		}
		defer events.Close(ctx)
		var history []jobs.Event
		if err = events.All(ctx, &history); err != nil {
			http.Error(w, "无法读取任务事件", 503)
			return
		}
		data["Events"] = history
	}
	h.renderAdmin(w, r, "tasks", data)
}

func init() {
	adminTemplates["tasks"] = adminLayoutStart + `
<div class="page-header"><h1>任务中心</h1><a href="/cm/tasks" class="btn btn-secondary">刷新任务</a></div>
<p>这里读取本系统的任务记录，不会访问 Worker 页面。发布链接仅在公开访问验证通过后显示。</p>
<nav aria-label="任务分类" style="display:flex;flex-wrap:wrap;gap:12px;margin:24px 0"><a href="/cm/tasks?source=workflow">后台任务</a><a href="/cm/tasks?source=delivery">静态导出与发布</a><a href="/cm/tasks?source=imports">导入记录</a><a href="/cm/tasks?source=upgrades">模板升级</a><a href="/cm/tasks?source=approvals">审批</a></nav>
{{with .LegacyDetail}}<section class="card" style="padding:24px;margin-bottom:24px"><h2>业务任务摘要</h2><p style="overflow-wrap:anywhere">{{.}}</p><p>历史任务可能没有可恢复执行输入。这里仅展示原记录；不会据此猜测断点重做，也不会自动发布。</p></section>{{end}}
{{with .Detail}}<section class="card" style="padding:24px;margin-bottom:24px"><h2>任务详情</h2><p style="overflow-wrap:anywhere">{{.Kind}} · {{.ID}}</p><p>状态：{{.State}} · 已处理 {{.Completed}} / {{.Total}}</p>{{if .ErrorCode}}<p role="alert">{{.ErrorCode}}</p>{{end}}<p>完成的条目不会重复执行；需要核实的外部操作不会自动强制解锁。</p></section>{{end}}
{{if .SourceRef}}<section class="card" style="padding:24px;margin-bottom:24px"><h3>冻结来源与执行边界</h3><p>来源记录 {{.SourceRef}} · 截止 {{.Deadline}} · 最近续租有效至 {{.LeaseUntil}}</p>{{if .ResumeLink}}<a class="btn btn-secondary" href="{{.ResumeLink}}">继续查看准备或恢复结果</a>{{end}}<p>以下只读摘要最多显示 100 项，不加载原始上传或凭证。</p><ul>{{range .Items}}<li>{{.key}} · 已完成 {{.done}} · {{.result}}</li>{{end}}</ul>{{if .ItemNext}}<a class="btn btn-secondary" href="{{.ItemNext}}">下一批条目</a>{{end}}</section>{{end}}
{{if .Events}}<details><summary>最近事件（最多100条）</summary><ul>{{range .Events}}<li>{{.At}} · {{.State}} {{.Code}}</li>{{end}}</ul></details>{{end}}
<div style="overflow-x:auto"><table><thead><tr><th>任务 / 标识</th><th>状态</th><th>进度</th><th>创建时间</th><th>诊断</th></tr></thead><tbody>{{range .Tasks}}<tr><td>{{.Kind}}<br>{{if .Link}}<a href="{{.Link}}">{{.ID}}</a>{{else}}{{.ID}}{{end}}</td><td>{{.State}}</td><td>{{if .Total}}{{.Completed}} / {{.Total}}{{else}}—{{end}}</td><td>{{.CreatedAt}}</td><td>{{.ErrorCode}}</td></tr>{{else}}<tr><td colspan="5">当前分类还没有任务。导入、合并、升级和静态发布会在这里留下记录。</td></tr>{{end}}</tbody></table></div>
{{if .Next}}<a class="btn btn-secondary" href="{{.Next}}">较早的任务</a>{{end}}
` + adminLayoutEnd
}
