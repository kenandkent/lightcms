package handlers

import (
	"context"
	"github.com/metavert-io/lightcms/v7/internal/product/dependencies"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"net/http"
	"time"
)

type dependencyInventory interface {
	QueryAsset(context.Context, string) (dependencies.Result, error)
	Frozen(context.Context, primitive.ObjectID) ([]bson.M, error)
}

func (h *Handler) AdminDependencies(w http.ResponseWriter, r *http.Request) {
	if !h.deliveryAdminAllowed(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	reader, ok := h.dependencyIndex.(dependencyInventory)
	if !ok {
		http.Error(w, "依赖索引暂不可用", 503)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	kind, value := r.URL.Query().Get("kind"), r.URL.Query().Get("value")
	data := map[string]any{"Kind": kind, "Value": value}
	var err error
	if len(value) > 2048 {
		http.Error(w, "查询值过长", 400)
		return
	}
	if value != "" {
		switch kind {
		case "asset":
			data["Result"], err = reader.QueryAsset(ctx, value)
		case "publication":
			id, e := primitive.ObjectIDFromHex(value)
			if e != nil {
				http.Error(w, "请输入内容 ID", 400)
				return
			}
			data["Frozen"], err = reader.Frozen(ctx, id)
		case "template":
			id, e := primitive.ObjectIDFromHex(value)
			if e != nil {
				http.Error(w, "请输入模板 ID", 400)
				return
			}
			data["Result"], err = h.dependencyIndex.Query(ctx, id, "")
		case "snippet":
			data["Result"], err = h.dependencyIndex.Query(ctx, primitive.NilObjectID, value)
		default:
			http.Error(w, "无效的查询类型", 400)
			return
		}
	}
	if err != nil {
		data["Error"] = "索引尚未就绪或读取失败，稍后刷新重试；不会修改内容或发布版本。"
		h.dependencyIndex.Wake()
	}
	h.renderAdmin(w, r, "dependencies", data)
}
func init() {
	adminTemplates["dependencies"] = adminLayoutStart + `
<div class="page-header"><h1>依赖索引</h1><a href="/cm/operations" class="btn btn-secondary">运行状态</a></div>
<section class="card" style="padding:24px"><p>只读清单，每次最多显示 50 条。当前内容索引约每 5 分钟刷新；它不是发布或删除依据。素材按已登记资源路径匹配，可能保守多报。历史 Publication 只展示其原有冻结来源，不用当前模板反推旧依赖。</p>
<form method="get"><label>查询类型 <select name="kind"><option value="template" {{if eq .Kind "template"}}selected{{end}}>模板 ID → 当前内容</option><option value="snippet" {{if eq .Kind "snippet"}}selected{{end}}>片段名称 → 当前内容</option><option value="asset" {{if eq .Kind "asset"}}selected{{end}}>素材路径 → 当前内容</option><option value="publication" {{if eq .Kind "publication"}}selected{{end}}>内容 ID → 历史冻结来源</option></select></label><label style="display:block;margin:20px 0">ID、名称或资源路径 <input name="value" value="{{.Value}}" maxlength="2048" style="width:100%"></label><button class="btn btn-primary">查询</button></form>
{{if .Error}}<p role="alert">{{.Error}}</p>{{end}}{{with .Result}}<p>索引时间 {{.IndexedAt}}；当前引用 {{.Total}} 篇，其中已发布标记 {{.Published}} 篇。后续变更可能尚未进入本索引。</p><ul>{{range .Items}}<li>{{.Title}} · {{.FullPath}} · {{.ID.Hex}}</li>{{end}}</ul>{{end}}
{{range .Frozen}}<article style="padding:20px 0;border-top:1px solid var(--border)"><p>Publication {{.publication_id}} · 内容版本 {{.content_version}} · {{.status}}</p><p>模板版本 {{.template_version_id}} · 依赖摘要 {{.render_dependencies_hash}}</p><p>原有冻结依赖 {{.dependency_snapshot}}</p><small>索引时间 {{.indexed_at}}。缺失的旧记录保持未知，不伪造历史。</small></article>{{end}}</section>` + adminLayoutEnd
}
