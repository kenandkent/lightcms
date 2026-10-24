package handlers

import (
	"bytes"
	"context"
	"fmt"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"html/template"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTaskCenterRejectsUnauthenticatedAndEscapesDiagnostics(t *testing.T) {
	h := &Handler{}
	w := httptest.NewRecorder()
	h.AdminTasksPage(w, httptest.NewRequest("GET", "/cm/tasks", nil))
	if w.Code != 403 {
		t.Fatalf("unauthenticated status %d", w.Code)
	}
	tmpl, err := template.New("tasks").Funcs(adminTemplateFuncMap).Parse(adminTemplates["tasks"])
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = tmpl.Execute(&out, map[string]any{"Lang": "zh", "Tasks": []taskRow{{ID: "one", Kind: "import", State: "failed", ErrorCode: "<script>secret()</script>"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "任务中心") || strings.Contains(out.String(), "<script>secret()") {
		t.Fatal("missing task center or unescaped diagnostic")
	}
}

func TestTaskCenterBusinessLinksAndMetadataOnlyDetail(t *testing.T) {
	h, cleanup := newTestHandler(t)
	defer cleanup()
	ctx := context.Background()
	id, wid := primitive.NewObjectID(), primitive.NewObjectID()
	if _, err := h.db.InsertOne(ctx, "import_jobs", bson.M{"_id": id, "workflow_id": wid, "status": "done", "started_at": time.Now(), "authority": bson.M{"secret": "must-not-display"}}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.AdminTasksPage(w, sessionReq("GET", "/cm/tasks?source=imports", nil, nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "source=workflow&amp;job_id="+wid.Hex()) || strings.Contains(w.Body.String(), "must-not-display") {
		t.Fatalf("business link or metadata projection failed: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	h.AdminTasksPage(w, sessionReq("GET", "/cm/tasks?source=imports&job_id="+id.Hex(), nil, nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "业务任务摘要") || strings.Contains(w.Body.String(), "must-not-display") {
		t.Fatalf("legacy detail missing or leaked source: %d", w.Code)
	}
	if _, err := h.db.InsertOne(ctx, "workflow_jobs", bson.M{"_id": wid, "kind": "import", "state": "succeeded", "created_at": time.Now(), "input_ref": id.Hex()}); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 101; n++ {
		if _, err := h.db.InsertOne(ctx, "workflow_items", bson.M{"job_id": wid, "key": fmt.Sprintf("%08d", n), "done": true, "input": bson.M{"secret": "must-not-display"}}); err != nil {
			t.Fatal(err)
		}
	}
	w = httptest.NewRecorder()
	h.AdminTasksPage(w, sessionReq("GET", "/cm/tasks?source=workflow&job_id="+wid.Hex(), nil, nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "after_item=00000099") || strings.Contains(w.Body.String(), "must-not-display") {
		t.Fatalf("bounded item navigation failed: %d", w.Code)
	}
	w = httptest.NewRecorder()
	h.AdminTasksPage(w, sessionReq("GET", "/cm/tasks?source=workflow&job_id="+wid.Hex()+"&after_item=00000099", nil, nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "00000100") || strings.Contains(w.Body.String(), "下一批条目") {
		t.Fatalf("last item page failed: %d", w.Code)
	}
}
