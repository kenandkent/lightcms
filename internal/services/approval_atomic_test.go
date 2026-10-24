package services

import (
	"context"
	"sync"
	"testing"

	"github.com/metavert-io/lightcms/v7/internal/models"
	"github.com/metavert-io/lightcms/v7/internal/testutil"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestApprovalAtomicDuplicateAndCancel(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	cs := NewContentService(db)
	s := NewApprovalService(db, cs, nil, nil)
	a, b, c := primitive.NewObjectID(), primitive.NewObjectID(), primitive.NewObjectID()
	_, err := s.CreateWorkflow(ctx, models.ApprovalWorkflow{Name: "Review", Trigger: models.WorkflowTriggerAllContributor, Mode: models.WorkflowModeConcurrent, Approvers: []models.WorkflowApprover{{UserID: a}, {UserID: b}, {UserID: c}}})
	if err != nil {
		t.Fatal(err)
	}
	content := &models.Content{Title: "Subject", Slug: "subject"}
	if err := cs.CreateContent(ctx, content); err != nil {
		t.Fatal(err)
	}
	req, err := s.SubmitContentForApproval(ctx, content, primitive.NewObjectID(), "author")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Approve(ctx, req.ID, a, "a", "first"); err != nil {
		t.Fatal(err)
	}
	_ = s.Approve(ctx, req.ID, a, "a", "duplicate")
	req, err = s.GetRequest(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if req.Status != models.ApprovalStatusPending || len(req.Decisions) != 1 {
		t.Fatalf("duplicate vote counted: %+v", req)
	}
	if err := s.Cancel(ctx, req.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := cs.GetContent(ctx, content.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.PendingApproval {
		t.Fatal("cancelled request left content pending")
	}
}

func TestApprovalAtomicFrozenRulesAndNewerPending(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	cs := NewContentService(db)
	s := NewApprovalService(db, cs, nil, nil)
	a, b := primitive.NewObjectID(), primitive.NewObjectID()
	wf, err := s.CreateWorkflow(ctx, models.ApprovalWorkflow{Name: "Two reviewers", Trigger: models.WorkflowTriggerAllContributor, Mode: models.WorkflowModeConcurrent, Approvers: []models.WorkflowApprover{{UserID: a}, {UserID: b}}})
	if err != nil {
		t.Fatal(err)
	}
	c := &models.Content{Title: "Frozen", Slug: "frozen"}
	if err := cs.CreateContent(ctx, c); err != nil {
		t.Fatal(err)
	}
	req, err := s.SubmitContentForApproval(ctx, c, primitive.NewObjectID(), "author")
	if err != nil {
		t.Fatal(err)
	}
	if req.RequiredApprovals != 2 {
		t.Fatal("two reviewers require two votes for strict majority")
	}
	wf.Mode = models.WorkflowModeSequential
	wf.Approvers = []models.WorkflowApprover{{UserID: primitive.NewObjectID()}}
	if err := s.UpdateWorkflow(ctx, wf.ID, *wf); err != nil {
		t.Fatal(err)
	}
	if err := s.Approve(ctx, req.ID, b, "b", ""); err != nil {
		t.Fatal("live workflow changed frozen rules", err)
	}
	if err := s.Approve(ctx, req.ID, a, "a", ""); err != nil {
		t.Fatal(err)
	}
	state, err := s.GetRequest(ctx, req.ID)
	if err != nil || state.Status != models.ApprovalStatusApproved {
		t.Fatalf("not approved: %+v %v", state, err)
	}
	oldID, err := db.InsertOne(ctx, "approval_requests", models.ApprovalRequest{ContentID: c.ID, Status: models.ApprovalStatusPending})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.InsertOne(ctx, "approval_requests", models.ApprovalRequest{ContentID: c.ID, Status: models.ApprovalStatusPending}); err != nil {
		t.Fatal(err)
	}
	if err := s.Cancel(ctx, oldID); err != nil {
		t.Fatal(err)
	}
	var stored models.Content
	if err := db.FindOne(ctx, "content", bson.M{"_id": c.ID}, &stored); err != nil {
		t.Fatal(err)
	}
	if !stored.PendingApproval {
		t.Fatal("cancel cleared a newer pending request")
	}
}

func TestApprovalAtomicUsesStoredSubject(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	cs := NewContentService(db)
	s := NewApprovalService(db, cs, nil, nil)
	c := &models.Content{Title: "Before", Slug: "stored-subject"}
	if err := cs.CreateContent(ctx, c); err != nil {
		t.Fatal(err)
	}
	stale := *c
	c.Title = "After"
	if err := cs.UpdateContent(ctx, c); err != nil {
		t.Fatal(err)
	}
	req, err := s.SubmitContentForApproval(ctx, &stale, primitive.NewObjectID(), "author")
	if err != nil {
		t.Fatal(err)
	}
	if req.ContentVersion != c.CurrentVersion || req.ContentTitle != "After" {
		t.Fatalf("froze stale caller data: %+v", req)
	}
	if _, err := s.SubmitContentForApproval(ctx, &models.Content{ID: primitive.NewObjectID()}, primitive.NewObjectID(), "author"); err == nil {
		t.Fatal("approved nonexistent subject")
	}
}

func TestApprovalAtomicConcurrentDecisions(t *testing.T) {
	db, cleanup := testutil.MustConnectTestDB(t)
	defer cleanup()
	ctx := context.Background()
	cs := NewContentService(db)
	s := NewApprovalService(db, cs, nil, nil)
	a, b := primitive.NewObjectID(), primitive.NewObjectID()
	if _, err := s.CreateWorkflow(ctx, models.ApprovalWorkflow{Name: "Concurrent", Trigger: models.WorkflowTriggerAllContributor, Mode: models.WorkflowModeConcurrent, Approvers: []models.WorkflowApprover{{UserID: a}, {UserID: b}}}); err != nil {
		t.Fatal(err)
	}
	c := &models.Content{Title: "Concurrent", Slug: "concurrent-approval"}
	if err := cs.CreateContent(ctx, c); err != nil {
		t.Fatal(err)
	}
	req, err := s.SubmitContentForApproval(ctx, c, primitive.NewObjectID(), "author")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = s.Approve(ctx, req.ID, a, "a", "") }()
	}
	wg.Wait()
	state, err := s.GetRequest(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Decisions) != 1 || state.Status != models.ApprovalStatusPending {
		t.Fatalf("concurrent duplicate votes: %+v", state)
	}
	wg.Add(2)
	go func() { defer wg.Done(); _ = s.Approve(ctx, req.ID, b, "b", "") }()
	go func() { defer wg.Done(); _ = s.Cancel(ctx, req.ID) }()
	wg.Wait()
	state, err = s.GetRequest(ctx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != models.ApprovalStatusApproved && state.Status != models.ApprovalStatusCancelled {
		t.Fatalf("invalid terminal state: %+v", state)
	}
	stored, err := cs.GetContent(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.PendingApproval || stored.Published {
		t.Fatalf("terminal decision left wrong content state: %+v", stored)
	}
}
