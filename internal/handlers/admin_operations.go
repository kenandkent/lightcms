package handlers

import (
	"context"
	"github.com/metavert-io/lightcms/v7/internal/product/artifacts"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"net/http"
	"runtime"
	"time"
)

func (h *Handler) Livez(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("alive\n"))
}
func (h *Handler) Readyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if h.db == nil || h.db.Collection("content").Database().Client().Ping(ctx, nil) != nil {
		http.Error(w, "core database unavailable", 503)
		return
	}
	_, _ = w.Write([]byte("ready\n"))
}

type operationCount struct {
	State string `bson:"_id"`
	Count int64  `bson:"count"`
	Bytes int64  `bson:"bytes"`
}

func (h *Handler) AdminOperations(w http.ResponseWriter, r *http.Request) {
	if !h.deliveryAdminAllowed(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	data := map[string]any{"At": time.Now().UTC()}
	for name, collection := range map[string]string{"WorkflowOldest": "workflow_jobs", "DeliveryOldest": "static_delivery_jobs"} {
		var queued struct {
			At time.Time `bson:"created_at"`
		}
		if err := h.db.Collection(collection).FindOne(ctx, bson.M{"state": "queued"}, options.FindOne().SetProjection(bson.M{"created_at": 1}).SetSort(bson.D{{Key: "created_at", Value: 1}})).Decode(&queued); err == nil && !queued.At.IsZero() {
			data[name] = time.Since(queued.At).Round(time.Second)
		}
	}
	for name, collection := range map[string]string{"Workflows": "workflow_jobs", "Delivery": "static_delivery_jobs", "Objects": "artifact_objects"} {
		group := bson.M{"_id": "$state", "count": bson.M{"$sum": 1}}
		if name == "Objects" {
			group["bytes"] = bson.M{"$sum": "$size"}
		}
		cur, err := h.db.Collection(collection).Aggregate(ctx, mongo.Pipeline{{{Key: "$group", Value: group}}}, options.Aggregate().SetMaxTime(4*time.Second))
		if err != nil {
			data["Error"] = "部分统计暂时不可用，原有内容服务不受影响。"
			continue
		}
		var counts []operationCount
		if err = cur.All(ctx, &counts); err != nil {
			data["Error"] = "部分统计暂时不可用。"
		} else {
			data[name] = counts
		}
		cur.Close(ctx)
	}
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	data["HeapMiB"] = memory.HeapAlloc >> 20
	data["GoMemoryMiB"] = memory.Sys >> 20
	data["Goroutines"] = runtime.NumGoroutine()
	if h.staticDelivery != nil && h.staticDelivery.Capacity != nil {
		data["Capacity"] = h.staticDelivery.Capacity.Stats()
	}
	if h.staticDelivery != nil && h.staticDelivery.Unavailable != "" {
		data["ModuleError"] = "静态发布模块降级；不影响原站点内容读写。"
	}
	h.renderAdmin(w, r, "operations", data)
}

type capacityReporter interface {
	Stats() artifacts.AdmissionStats
}

func init() {
	adminTemplates["operations"] = adminLayoutStart + `
<div class="page-header"><h1>运行状态</h1><a href="/cm/operations" class="btn btn-secondary">刷新</a></div><p>统计时间：{{.At}}。只读取本地任务与存储目录记录，不轮询 Cloudflare 公开页面。</p>{{if .Error}}<p role="alert">{{.Error}}</p>{{end}}{{if .ModuleError}}<p role="status">{{.ModuleError}}</p>{{end}}
<section class="card" style="padding:24px"><h2>进程与构建容量</h2><p>Go 堆内存 {{.HeapMiB}} MiB；Go 运行时管理内存 {{.GoMemoryMiB}} MiB；协程 {{.Goroutines}}。这些值不是整个进程 RSS。</p>{{with .Capacity}}<p>准备任务已预留 {{.Used}} bytes / {{.Limit}} bytes；历史预留峰值 {{.Peak}} bytes；当前容量等待 {{.Waiters}} 个，累计等待 {{.WaitNanos}} ns。</p>{{end}}<p><a href="/livez">进程存活</a> · <a href="/readyz">核心数据库就绪</a> · <a href="/healthz">详细健康状态</a></p></section>
<section class="card" style="padding:24px;margin-top:24px"><h2>后台任务</h2><p>后台队列最老等待：{{if .WorkflowOldest}}{{.WorkflowOldest}}{{else}}无{{end}}；静态发布队列最老等待：{{if .DeliveryOldest}}{{.DeliveryOldest}}{{else}}无{{end}}。</p><ul>{{range .Workflows}}<li>{{.State}}：{{.Count}}</li>{{else}}<li>暂无任务记录。</li>{{end}}</ul><h2>静态发布</h2><ul>{{range .Delivery}}<li>{{.State}}：{{.Count}}</li>{{else}}<li>暂无发布记录。</li>{{end}}</ul><a href="/cm/tasks">查看任务详情</a> · <a href="/cm/dependencies">依赖索引</a></section>
<section class="card" style="padding:24px;margin-top:24px"><h2>对象目录统计</h2><ul>{{range .Objects}}<li>{{.State}}：{{.Count}} 个，登记大小 {{.Bytes}} bytes</li>{{else}}<li>暂无对象。</li>{{end}}</ul><p>deleted 为删除墓碑，其登记大小不是当前磁盘占用。这里不包含旧包、日志或系统文件。</p><a href="/cm/artifacts">预览可清理文件</a></section>
` + adminLayoutEnd
}
