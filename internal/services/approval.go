package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/metavert-io/lightcms/v7/internal/database"
	"github.com/metavert-io/lightcms/v7/internal/models"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ApprovalService manages approval workflows and requests.
type ApprovalService struct {
	db             *database.DB
	contentService *ContentService
	commentService *CommentService
	webhookService *WebhookService
}

var ErrApprovalNotPending = errors.New("approval request is not pending")

// NewApprovalService creates a new ApprovalService.
func NewApprovalService(db *database.DB, cs *ContentService, cms *CommentService, ws *WebhookService) *ApprovalService {
	return &ApprovalService{
		db:             db,
		contentService: cs,
		commentService: cms,
		webhookService: ws,
	}
}

// ==================== Workflow CRUD ====================

func (s *ApprovalService) CreateWorkflow(ctx context.Context, wf models.ApprovalWorkflow) (*models.ApprovalWorkflow, error) {
	now := time.Now()
	wf.CreatedAt = now
	wf.UpdatedAt = now
	id, err := s.db.InsertOne(ctx, "approval_workflows", &wf)
	if err != nil {
		return nil, fmt.Errorf("creating workflow: %w", err)
	}
	wf.ID = id
	return &wf, nil
}

func (s *ApprovalService) GetWorkflow(ctx context.Context, id primitive.ObjectID) (*models.ApprovalWorkflow, error) {
	var wf models.ApprovalWorkflow
	if err := s.db.FindOne(ctx, "approval_workflows", bson.M{"_id": id}, &wf); err != nil {
		return nil, err
	}
	return &wf, nil
}

func (s *ApprovalService) UpdateWorkflow(ctx context.Context, id primitive.ObjectID, wf models.ApprovalWorkflow) error {
	update := bson.M{"$set": bson.M{
		"name":          wf.Name,
		"description":   wf.Description,
		"trigger":       wf.Trigger,
		"trigger_value": wf.TriggerValue,
		"approvers":     wf.Approvers,
		"mode":          wf.Mode,
		"updated_at":    time.Now(),
	}}
	return s.db.UpdateOne(ctx, "approval_workflows", bson.M{"_id": id}, update)
}

func (s *ApprovalService) DeleteWorkflow(ctx context.Context, id primitive.ObjectID) error {
	return s.db.DeleteOne(ctx, "approval_workflows", bson.M{"_id": id})
}

func (s *ApprovalService) ListWorkflows(ctx context.Context) ([]models.ApprovalWorkflow, error) {
	opts := options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}})
	cursor, err := s.db.FindMany(ctx, "approval_workflows", bson.M{}, opts)
	if err != nil {
		return nil, err
	}
	var workflows []models.ApprovalWorkflow
	if err := cursor.All(ctx, &workflows); err != nil {
		return nil, err
	}
	if workflows == nil {
		workflows = []models.ApprovalWorkflow{}
	}
	return workflows, nil
}

// ==================== Approval Request Lifecycle ====================

// SubmitContentForApproval creates an ApprovalRequest for a content item and
// marks the content as pending_approval. Called when a Contributor saves/publishes,
// or when an Editor explicitly submits content to a workflow.
func (s *ApprovalService) SubmitContentForApproval(ctx context.Context,
	content *models.Content,
	submitterID primitive.ObjectID, submitterEmail string,
) (*models.ApprovalRequest, error) {
	var req *models.ApprovalRequest
	err := s.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		var err error
		req, err = s.submitContentAtomic(sc, content, submitterID, submitterEmail)
		return err
	})
	return req, err
}

func (s *ApprovalService) submitContentAtomic(ctx context.Context, content *models.Content, submitterID primitive.ObjectID, submitterEmail string) (*models.ApprovalRequest, error) {
	var stored models.Content
	if err := s.db.FindOne(ctx, "content", bson.M{"_id": content.ID, "deleted": bson.M{"$ne": true}}, &stored); err != nil {
		return nil, fmt.Errorf("approval subject: %w", err)
	}
	content = &stored
	// Check if there's already a pending request for this content
	existing, lookupErr := s.GetPendingRequestForContent(ctx, content.ID)
	if lookupErr != nil && lookupErr != mongo.ErrNoDocuments {
		return nil, lookupErr
	}
	if existing != nil {
		return existing, nil // idempotent
	}

	wf, err := s.matchWorkflow(ctx, content)
	if err != nil {
		return nil, fmt.Errorf("matching workflow: %w", err)
	}

	req := models.ApprovalRequest{
		SnapshotVersion:  1,
		ContentVersion:   content.CurrentVersion,
		WorkflowMode:     models.WorkflowModeConcurrent,
		ContentID:        content.ID,
		ContentTitle:     content.Title,
		ContentPath:      content.FullPath,
		SubmittedByID:    submitterID,
		SubmittedByEmail: submitterEmail,
		Status:           models.ApprovalStatusPending,
		CurrentStep:      0,
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}

	if wf != nil {
		req.WorkflowMode = wf.Mode
		req.WorkflowID = &wf.ID
		req.Approvers = wf.Approvers
		if wf.Mode == models.WorkflowModeSequential {
			req.RequiredApprovals = len(wf.Approvers)
		} else {
			// Concurrent: require majority by default; workflow could specify otherwise
			req.RequiredApprovals = len(wf.Approvers)/2 + 1
		}
	}

	id, err := s.db.InsertOne(ctx, "approval_requests", &req)
	if err != nil {
		return nil, fmt.Errorf("creating approval request: %w", err)
	}
	req.ID = id

	// Mark content as pending approval
	if err := s.db.UpdateOne(ctx, "content", bson.M{"_id": content.ID},
		bson.M{"$set": bson.M{"pending_approval": true, "updated_at": time.Now()}}); err != nil {
		return nil, err
	}

	// Fire webhook
	if s.webhookService != nil {
		payload := map[string]interface{}{
			"request_id":         req.ID.Hex(),
			"content_id":         content.ID.Hex(),
			"content_title":      content.Title,
			"content_path":       content.FullPath,
			"submitted_by_email": submitterEmail,
		}
		if wf != nil {
			payload["workflow_id"] = wf.ID.Hex()
			payload["workflow_name"] = wf.Name
		}
		s.db.AfterCommit(ctx, func(ctx context.Context) { s.webhookService.FireEvent(ctx, "content.pending_approval", payload) })
	}

	return &req, nil
}

// SubmitAssetForReview creates an ApprovalRequest for an asset uploaded by a Contributor.
func (s *ApprovalService) SubmitAssetForReview(ctx context.Context,
	asset *models.Asset,
	submitterID primitive.ObjectID, submitterEmail string,
) (*models.ApprovalRequest, error) {
	req := models.ApprovalRequest{
		ContentTitle:     asset.Filename,
		ContentPath:      asset.FullPath,
		AssetID:          &asset.ID,
		SubmittedByID:    submitterID,
		SubmittedByEmail: submitterEmail,
		Status:           models.ApprovalStatusPending,
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}
	id, err := s.db.InsertOne(ctx, "approval_requests", &req)
	if err != nil {
		return nil, fmt.Errorf("creating asset review request: %w", err)
	}
	req.ID = id

	if s.webhookService != nil {
		go s.webhookService.FireEvent(context.Background(), "asset.pending_review", map[string]interface{}{
			"request_id":         req.ID.Hex(),
			"asset_id":           asset.ID.Hex(),
			"filename":           asset.Filename,
			"submitted_by_email": submitterEmail,
		})
	}

	return &req, nil
}

// GetPendingRequestForContent returns the active (pending) approval request for a content item.
func (s *ApprovalService) GetPendingRequestForContent(ctx context.Context, contentID primitive.ObjectID) (*models.ApprovalRequest, error) {
	var req models.ApprovalRequest
	err := s.db.FindOne(ctx, "approval_requests",
		bson.M{"content_id": contentID, "status": models.ApprovalStatusPending}, &req)
	if err != nil {
		return nil, err
	}
	return &req, nil
}

// GetRequest fetches an approval request by ID.
func (s *ApprovalService) GetRequest(ctx context.Context, id primitive.ObjectID) (*models.ApprovalRequest, error) {
	var req models.ApprovalRequest
	if err := s.db.FindOne(ctx, "approval_requests", bson.M{"_id": id}, &req); err != nil {
		return nil, err
	}
	return &req, nil
}

// ListPending returns all pending approval requests, newest first.
func (s *ApprovalService) ListPending(ctx context.Context) ([]models.ApprovalRequest, error) {
	return s.listRequests(ctx, bson.M{"status": models.ApprovalStatusPending})
}

// ListMyQueue returns pending requests where the given user is the current/next approver.
// For default requests (no workflow), any editor/admin sees them (callers filter by role).
// For workflow requests: sequential → user must be the approver at CurrentStep;
// concurrent → user must be in the approvers list.
func (s *ApprovalService) ListMyQueue(ctx context.Context, userID primitive.ObjectID) ([]models.ApprovalRequest, error) {
	filter := bson.M{
		"status": models.ApprovalStatusPending,
		"$or": []bson.M{
			// No workflow — visible to any editor/admin (caller must check role)
			{"workflow_id": bson.M{"$exists": false}},
			{"workflow_id": nil},
			// User is in the approvers list for this request
			{"approvers.user_id": userID},
		},
	}
	return s.listRequests(ctx, filter)
}

// CountPending returns the total number of pending approval requests.
func (s *ApprovalService) CountPending(ctx context.Context) int64 {
	n, _ := s.db.Count(ctx, "approval_requests", bson.M{"status": models.ApprovalStatusPending})
	return n
}

// Approve records an approval decision. Publication remains an explicit,
// independent operation even when the final approval is received.
func (s *ApprovalService) Approve(ctx context.Context, requestID, approverID primitive.ObjectID, approverEmail, comment string) error {
	return s.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		req, err := s.GetRequest(sc, requestID)
		if err != nil {
			return err
		}
		if err := s.validateReviewer(sc, req, approverID); err != nil {
			return err
		}
		for _, d := range req.Decisions {
			if d.UserID == approverID {
				return fmt.Errorf("reviewer has already decided this request")
			}
		}
		decision := models.ApprovalDecision{UserID: approverID, UserEmail: approverEmail, Decision: "approved", Comment: comment, DecidedAt: time.Now()}
		approved := 1
		seen := map[primitive.ObjectID]bool{}
		for _, d := range req.Decisions {
			if d.Decision == "approved" && !seen[d.UserID] {
				seen[d.UserID] = true
				approved++
			}
		}
		set := bson.M{"updated_at": time.Now()}
		if req.WorkflowMode == models.WorkflowModeSequential {
			set["current_step"] = req.CurrentStep + 1
		}
		complete := approved >= req.RequiredApprovals || req.WorkflowID == nil
		if complete {
			set["status"] = models.ApprovalStatusApproved
		}
		if err := s.db.UpdateOne(sc, "approval_requests", bson.M{"_id": requestID, "status": models.ApprovalStatusPending}, bson.M{"$push": bson.M{"decisions": decision}, "$set": set}); err != nil {
			return err
		}
		if complete {
			return s.clearPending(sc, req)
		}
		return nil
	})
}

// Rules are frozen when the request is submitted. Legacy workflow requests
// must be cancelled and resubmitted rather than borrowing today's rules.
func (s *ApprovalService) validateReviewer(ctx context.Context, req *models.ApprovalRequest, userID primitive.ObjectID) error {
	if req.Status != models.ApprovalStatusPending {
		return fmt.Errorf("approval request is not pending")
	}
	if userID.IsZero() || userID == req.SubmittedByID {
		return fmt.Errorf("a non-submitting reviewer is required")
	}
	if req.WorkflowID != nil {
		if req.SnapshotVersion == 0 {
			return fmt.Errorf("legacy approval has no frozen rules; cancel and resubmit for review")
		}
		eligible := false
		for _, a := range req.Approvers {
			if a.UserID == userID {
				eligible = true
			}
		}
		if !eligible {
			return fmt.Errorf("reviewer is not assigned to this request")
		}
		if req.WorkflowMode == models.WorkflowModeSequential && (req.CurrentStep >= len(req.Approvers) || req.Approvers[req.CurrentStep].UserID != userID) {
			return fmt.Errorf("it is not your turn to approve this request")
		}
	}
	if req.ContentVersion > 0 && !req.ContentID.IsZero() {
		var content models.Content
		if err := s.db.FindOne(ctx, "content", bson.M{"_id": req.ContentID}, &content); err != nil {
			return err
		}
		if content.CurrentVersion != req.ContentVersion {
			return fmt.Errorf("content changed after submission; cancel and resubmit for review")
		}
	}
	return nil
}

func (s *ApprovalService) clearPending(ctx context.Context, req *models.ApprovalRequest) error {
	if !req.ContentID.IsZero() {
		n, err := s.db.Count(ctx, "approval_requests", bson.M{"content_id": req.ContentID, "status": models.ApprovalStatusPending})
		if err != nil {
			return err
		}
		if err := s.db.UpdateOne(ctx, "content", bson.M{"_id": req.ContentID}, bson.M{"$set": bson.M{"pending_approval": n > 0, "updated_at": time.Now()}}); err != nil {
			return err
		}
	}
	if req.AssetID != nil {
		n, err := s.db.Count(ctx, "approval_requests", bson.M{"asset_id": *req.AssetID, "status": models.ApprovalStatusPending})
		if err != nil {
			return err
		}
		return s.db.UpdateOne(ctx, "assets", bson.M{"_id": *req.AssetID}, bson.M{"$set": bson.M{"pending_review": n > 0, "updated_at": time.Now()}})
	}
	return nil
}

// Reject records a rejection decision, sets the request status to rejected,
// clears the pending_approval flag on content, and auto-posts a comment.
func (s *ApprovalService) Reject(ctx context.Context, requestID, approverID primitive.ObjectID, approverEmail, approverDisplayName, rejectComment string) error {
	return s.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		req, err := s.GetRequest(sc, requestID)
		if err != nil {
			return err
		}
		if err := s.validateReviewer(sc, req, approverID); err != nil {
			return err
		}
		decision := models.ApprovalDecision{UserID: approverID, UserEmail: approverEmail, Decision: "rejected", Comment: rejectComment, DecidedAt: time.Now()}
		if err := s.db.UpdateOne(sc, "approval_requests", bson.M{"_id": requestID, "status": models.ApprovalStatusPending}, bson.M{
			"$push": bson.M{"decisions": decision}, "$set": bson.M{"status": models.ApprovalStatusRejected, "updated_at": time.Now()},
		}); err != nil {
			return err
		}
		if err := s.clearPending(sc, req); err != nil {
			return err
		}
		if s.commentService != nil && !req.ContentID.IsZero() && rejectComment != "" {
			s.db.AfterCommit(sc, func(ctx context.Context) {
				s.commentService.Create(ctx, req.ContentID, approverID, approverEmail, approverDisplayName, "❌ Rejected: "+rejectComment, nil)
			})
		}
		return nil
	})
}

// Cancel marks an approval request as cancelled.
func (s *ApprovalService) Cancel(ctx context.Context, requestID primitive.ObjectID) error {
	return s.db.WithTransaction(ctx, func(sc mongo.SessionContext) error {
		req, err := s.GetRequest(sc, requestID)
		if err != nil {
			return err
		}
		if req.Status == models.ApprovalStatusCancelled {
			return s.clearPending(sc, req)
		}
		if req.Status != models.ApprovalStatusPending {
			return ErrApprovalNotPending
		}
		if err := s.db.UpdateOne(sc, "approval_requests", bson.M{"_id": requestID, "status": models.ApprovalStatusPending},
			bson.M{"$set": bson.M{"status": models.ApprovalStatusCancelled, "updated_at": time.Now()}}); err != nil {
			return err
		}
		return s.clearPending(sc, req)
	})
}

// ==================== Internals ====================

// matchWorkflow finds the best-matching workflow for a content item.
// Priority: template_id > folder_path > tag > all_contributor
func (s *ApprovalService) matchWorkflow(ctx context.Context, content *models.Content) (*models.ApprovalWorkflow, error) {
	var workflows []models.ApprovalWorkflow
	cursor, err := s.db.FindMany(ctx, "approval_workflows", bson.M{}, nil)
	if err != nil {
		return nil, err
	}
	if err := cursor.All(ctx, &workflows); err != nil {
		return nil, err
	}

	var byTemplateID, byFolderPath, byTag, byAllContributor *models.ApprovalWorkflow

	for i, wf := range workflows {
		switch wf.Trigger {
		case models.WorkflowTriggerTemplateID:
			if wf.TriggerValue == content.TemplateID.Hex() {
				byTemplateID = &workflows[i]
			}
		case models.WorkflowTriggerFolderPath:
			if strings.HasPrefix(content.FolderPath, wf.TriggerValue) {
				byFolderPath = &workflows[i]
			}
		case models.WorkflowTriggerTag:
			for _, tag := range content.Tags {
				if tag == wf.TriggerValue {
					byTag = &workflows[i]
					break
				}
			}
		case models.WorkflowTriggerAllContributor:
			byAllContributor = &workflows[i]
		}
	}

	if byTemplateID != nil {
		return byTemplateID, nil
	}
	if byFolderPath != nil {
		return byFolderPath, nil
	}
	if byTag != nil {
		return byTag, nil
	}
	return byAllContributor, nil // may be nil (use default any-editor behavior)
}

func (s *ApprovalService) listRequests(ctx context.Context, filter bson.M) ([]models.ApprovalRequest, error) {
	opts := options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}})
	cursor, err := s.db.FindMany(ctx, "approval_requests", filter, opts)
	if err != nil {
		return nil, err
	}
	var reqs []models.ApprovalRequest
	if err := cursor.All(ctx, &reqs); err != nil {
		return nil, err
	}
	if reqs == nil {
		reqs = []models.ApprovalRequest{}
	}
	return reqs, nil
}
