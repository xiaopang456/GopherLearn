package memory

import (
	"GopherAI/common/code"
	"GopherAI/controller"
	learningdao "GopherAI/dao/learning"
	memorydao "GopherAI/dao/memory"
	"GopherAI/model"
	memoryservice "GopherAI/service/memory"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
)

// memoryTopicCatalogue is built once per process from the same two sources the
// agent-side draft tool sees (graded topics + memory note keys), so the
// learner's attribution selector never offers a key the write path would
// reject for catalogue drift.
var memoryTopicCatalogue struct {
	sync.Once
	catalogue *memoryservice.TopicCatalogue
}

func memoryTopicsCatalogue() *memoryservice.TopicCatalogue {
	memoryTopicCatalogue.Do(func() {
		catalogue, err := memoryservice.NewTopicCatalogue(learningdao.DefaultRepository(), memoryservice.MemoryTopicAdapter{Repository: memorydao.DefaultRepository()}, 100)
		if err != nil {
			log.Printf("memory topic catalogue: %v", err)
			return
		}
		memoryTopicCatalogue.catalogue = catalogue
	})
	return memoryTopicCatalogue.catalogue
}

type memoryTopicView struct {
	Key    string `json:"key"`
	Name   string `json:"name"`
	Graded bool   `json:"graded"`
	Notes  int    `json:"notes"`
}

type memoryTopicsResponse struct {
	controller.Response
	Topics []memoryTopicView `json:"topics"`
}

// Topics lists the memory-side topic catalogue for the draft card's
// attribution selector. Read-only: a new topic comes into existence by
// confirming a note under a new key, never by this endpoint.
func Topics(c *gin.Context) {
	response := new(memoryTopicsResponse)
	catalogue := memoryTopicsCatalogue()
	if catalogue == nil {
		response.Success()
		response.Topics = []memoryTopicView{}
		c.JSON(http.StatusOK, response)
		return
	}
	topics, err := catalogue.List(c.Request.Context(), c.GetInt64("userID"))
	if err != nil {
		c.JSON(http.StatusOK, response.CodeOf(code.CodeServerBusy))
		return
	}
	views := make([]memoryTopicView, 0, len(topics))
	for i := range topics {
		views = append(views, memoryTopicView{Key: topics[i].Key, Name: topics[i].Name, Graded: topics[i].Graded, Notes: topics[i].Notes})
	}
	response.Success()
	response.Topics = views
	c.JSON(http.StatusOK, response)
}

type correctionRequest struct {
	Subject string   `json:"subject" binding:"required"`
	Content string   `json:"content" binding:"required"`
	Tags    []string `json:"tags" binding:"required"`
	Reason  string   `json:"reason" binding:"required"`
}

type deleteRequest struct {
	Reason string `json:"reason" binding:"required"`
}

type createRequest struct {
	Scope          model.MemoryScope      `json:"scope" binding:"required"`
	Subject        string                 `json:"subject" binding:"required"`
	Content        string                 `json:"content" binding:"required"`
	Tags           []string               `json:"tags"`
	TopicKey       string                 `json:"topic_key"`
	Durability     model.MemoryDurability `json:"durability" binding:"required"`
	ValidUntil     *time.Time             `json:"valid_until"`
	IdempotencyKey string                 `json:"idempotency_key" binding:"required"`
	Decision       string                 `json:"decision"`
	TargetMemoryID string                 `json:"target_memory_id"`
	Reason         string                 `json:"reason" binding:"required"`
}

type resolveDraftRequest struct {
	Action         string `json:"action" binding:"required"`
	TargetMemoryID string `json:"target_memory_id"`
	Reason         string `json:"reason"`
	// ExpectedTargetHash is set on the retry after the learner looked at the
	// latest content a modified-target response returned. Empty falls back to
	// the hash pinned on the draft when it captured the conflict.
	ExpectedTargetHash string `json:"expected_target_hash,omitempty"`
}

type editDraftRequest struct {
	Subject    *string                 `json:"subject"`
	Content    *string                 `json:"content"`
	Tags       []string                `json:"tags"`
	TopicKey   *string                 `json:"topic_key"`
	Durability *model.MemoryDurability `json:"durability"`
	ValidUntil *time.Time              `json:"valid_until"`
}

type listResponse struct {
	controller.Response
	Memories      []memoryView `json:"memories"`
	PendingDrafts []draftView  `json:"pendingDrafts"`
}

type memoryResponse struct {
	controller.Response
	Memory    *memoryView   `json:"memory,omitempty"`
	Revision  *revisionView `json:"revision,omitempty"`
	Draft     *draftView    `json:"draft,omitempty"`
	Conflicts []memoryView  `json:"conflicts,omitempty"`
	Replay    bool          `json:"replay,omitempty"`
	// Closed reports the edit-path outcome where the re-check judged the
	// final content identical to an active memory: the draft was ended by the
	// system and the existing entry stays effective.
	Closed       bool   `json:"closed,omitempty"`
	CancelReason string `json:"cancel_reason,omitempty"`
}

type draftView struct {
	ID          string                 `json:"id"`
	Scope       model.MemoryScope      `json:"scope"`
	Subject     string                 `json:"subject"`
	Content     string                 `json:"content"`
	Tags        model.StringList       `json:"tags"`
	TopicKey    string                 `json:"topic_key,omitempty"`
	Durability  model.MemoryDurability `json:"durability"`
	ValidUntil  *time.Time             `json:"valid_until,omitempty"`
	ConflictIDs model.StringList       `json:"conflict_ids"`
	// ConflictTargets hydrate the conflict rows so the card can show the old
	// content and its save date beside the replace decision, instead of bare
	// IDs.
	ConflictTargets []memoryView            `json:"conflict_targets,omitempty"`
	Status          model.MemoryDraftStatus `json:"status"`
	MemoryID        string                  `json:"memory_id,omitempty"`
	CancelReason    string                  `json:"cancel_reason,omitempty"`
	Source          string                  `json:"source"`
	SessionID       string                  `json:"session_id,omitempty"`
	// 自动候选来源契约（草稿上的候选字段，确认后随正式记忆保留）。
	ProfileClass      model.MemoryProfileClass `json:"profile_class,omitempty"`
	EvidenceMessages  model.StringList         `json:"evidence_messages,omitempty"`
	AssistantMessages model.StringList         `json:"assistant_messages,omitempty"`
	StatementTime     *time.Time               `json:"statement_time,omitempty"`
	CreatedAt         time.Time                `json:"created_at"`
}

type revisionResponse struct {
	controller.Response
	Revisions []revisionView `json:"revisions"`
}

type memoryView struct {
	ID          string            `json:"id"`
	Scope       model.MemoryScope `json:"scope"`
	Subject     string            `json:"subject"`
	Content     string            `json:"content"`
	ContentHash string            `json:"content_hash,omitempty"`
	Tags        model.StringList  `json:"tags"`
	TopicKey    string            `json:"topic_key"`
	// 自动候选来源契约（随确认保留到正式记忆的字段）。
	ProfileClass      model.MemoryProfileClass   `json:"profile_class,omitempty"`
	EvidenceMessages  model.StringList           `json:"evidence_messages,omitempty"`
	AssistantMessages model.StringList           `json:"assistant_messages,omitempty"`
	StatementTime     *time.Time                 `json:"statement_time,omitempty"`
	SourceSession     string                     `json:"source_session,omitempty"`
	Durability        model.MemoryDurability     `json:"durability"`
	ValidUntil        *time.Time                 `json:"valid_until,omitempty"`
	Source            string                     `json:"source"`
	Confidence        float64                    `json:"confidence"`
	Importance        int                        `json:"importance"`
	Status            model.LongTermMemoryStatus `json:"status"`
	UserConfirmed     bool                       `json:"user_confirmed"`
	LastUsedAt        *time.Time                 `json:"last_used_at,omitempty"`
	UsageCount        uint64                     `json:"usage_count"`
	CreatedAt         time.Time                  `json:"created_at"`
	UpdatedAt         time.Time                  `json:"updated_at"`
}

type revisionView struct {
	ID        string                    `json:"id"`
	MemoryID  string                    `json:"memory_id,omitempty"`
	Action    model.MemoryDreamAction   `json:"action"`
	Actor     model.MemoryRevisionActor `json:"actor"`
	Reason    string                    `json:"reason"`
	CreatedAt time.Time                 `json:"created_at"`
}

func List(c *gin.Context) {
	service, err := memoryservice.DefaultGovernanceService()
	if err != nil {
		respondError(c, err)
		return
	}
	scopeValue := strings.TrimSpace(c.Query("scope"))
	if scopeValue == "" { // compatibility for clients that still send the old parameter
		scopeValue = strings.TrimSpace(c.Query("type"))
	}
	switch scopeValue {
	case "profile":
		scopeValue = string(model.MemoryScopeGlobal)
	case "topic_note":
		scopeValue = string(model.MemoryScopeTopic)
	}
	scope := model.MemoryScope(scopeValue)
	status := model.LongTermMemoryStatus(strings.TrimSpace(c.DefaultQuery("status", string(model.LongTermMemoryActive))))
	if (scope != "" && !scope.Valid()) || (status != "" && !status.Valid()) {
		response := new(controller.Response)
		c.JSON(http.StatusBadRequest, response.CodeOf(code.CodeInvalidParams))
		return
	}
	result, err := service.List(c.Request.Context(), memoryservice.ListRequest{UserID: c.GetInt64("userID"), Scope: scope, Status: status})
	if err != nil {
		respondError(c, err)
		return
	}
	views := make([]memoryView, 0, len(result.Memories))
	for i := range result.Memories {
		views = append(views, *toMemoryView(&result.Memories[i]))
	}
	drafts, err := service.PendingDrafts(c.Request.Context(), c.GetInt64("userID"))
	if err != nil {
		respondError(c, err)
		return
	}
	draftViews := make([]draftView, 0, len(drafts))
	if len(drafts) > 0 {
		conflictIndex, hydrateErr := hydrateConflictTargets(c.Request.Context(), service, c.GetInt64("userID"), drafts)
		if hydrateErr != nil {
			respondError(c, hydrateErr)
			return
		}
		for i := range drafts {
			draftViews = append(draftViews, *toDraftView(&drafts[i], conflictIndex))
		}
	}
	response := listResponse{Memories: views, PendingDrafts: draftViews}
	response.Success()
	c.JSON(http.StatusOK, response)
}

func Create(c *gin.Context) {
	request := new(createRequest)
	if err := c.ShouldBindJSON(request); err != nil {
		respondError(c, err)
		return
	}
	service, err := memoryservice.DefaultGovernanceService()
	if err != nil {
		respondError(c, err)
		return
	}
	result, err := service.Create(c.Request.Context(), memoryservice.CreateRequest{
		UserID: c.GetInt64("userID"), Decision: request.Decision, TargetMemoryID: request.TargetMemoryID, Reason: request.Reason,
		Input: memoryservice.ExplicitMemoryInput{Scope: request.Scope, Subject: request.Subject, Content: request.Content, Tags: request.Tags, TopicKey: request.TopicKey, Durability: request.Durability, ValidUntil: request.ValidUntil, IdempotencyKey: request.IdempotencyKey},
	})
	if err != nil {
		respondError(c, err)
		return
	}
	conflicts := make([]memoryView, 0, len(result.Conflicts))
	for i := range result.Conflicts {
		conflicts = append(conflicts, *toMemoryView(&result.Conflicts[i]))
	}
	response := memoryResponse{Memory: toMemoryView(result.Memory), Revision: toRevisionView(result.Revision), Conflicts: conflicts, Replay: result.Replay}
	response.Success()
	c.JSON(http.StatusOK, response)
}

func ResolveDraft(c *gin.Context) {
	request := new(resolveDraftRequest)
	if err := c.ShouldBindJSON(request); err != nil {
		respondError(c, err)
		return
	}
	service, err := memoryservice.DefaultGovernanceService()
	if err != nil {
		respondError(c, err)
		return
	}
	result, err := service.ConfirmDraft(c.Request.Context(), memoryservice.ConfirmDraftRequest{
		UserID: c.GetInt64("userID"), DraftID: c.Param("id"), Action: request.Action, TargetMemoryID: request.TargetMemoryID,
		Reason: request.Reason, ExpectedTargetHash: request.ExpectedTargetHash,
	})
	if err != nil {
		if errors.Is(err, memorydao.ErrMemoryTargetModified) {
			// The replace target changed after the draft captured it. Answer
			// 409 with the latest row so the learner can look at it and
			// re-confirm; the official memory keeps its current value.
			respondTargetModified(c, service, c.GetInt64("userID"), request.TargetMemoryID)
			return
		}
		respondError(c, err)
		return
	}
	response := memoryResponse{Draft: toDraftView(&result.Draft, nil), Memory: toMemoryView(result.Memory), Revision: toRevisionView(result.Revision), Replay: result.Replay}
	response.Success()
	c.JSON(http.StatusOK, response)
}

func respondTargetModified(c *gin.Context, service *memoryservice.GovernanceService, userID int64, targetID string) {
	conflicts := make([]memoryView, 0, 1)
	if target := strings.TrimSpace(targetID); target != "" {
		if row, err := service.MemoriesByIDs(c.Request.Context(), userID, []string{target}); err == nil && len(row) == 1 {
			conflicts = append(conflicts, *toMemoryView(&row[0]))
		}
	}
	response := memoryResponse{Conflicts: conflicts}
	response.CodeOf(code.MemoryTargetModified)
	c.JSON(http.StatusConflict, response)
}

func EditDraft(c *gin.Context) {
	request := new(editDraftRequest)
	if err := c.ShouldBindJSON(request); err != nil {
		respondError(c, err)
		return
	}
	service, err := memoryservice.DefaultGovernanceService()
	if err != nil {
		respondError(c, err)
		return
	}
	result, err := service.EditDraft(c.Request.Context(), memoryservice.EditDraftRequest{
		UserID: c.GetInt64("userID"), DraftID: c.Param("id"),
		Subject: request.Subject, Content: request.Content, Tags: request.Tags,
		TopicKey: request.TopicKey, Durability: request.Durability, ValidUntil: request.ValidUntil,
	})
	if err != nil {
		respondError(c, err)
		return
	}
	targets := make([]memoryView, 0, len(result.Targets))
	targetIndex := make(map[string]memoryView, len(result.Targets))
	for i := range result.Targets {
		targets = append(targets, *toMemoryView(&result.Targets[i]))
		targetIndex[result.Targets[i].ID] = targets[i]
	}
	response := memoryResponse{Draft: toDraftView(result.Draft, targetIndex), Conflicts: targets, Closed: result.Closed, CancelReason: result.Draft.CancelReason}
	response.Success()
	c.JSON(http.StatusOK, response)
}

func Archive(c *gin.Context) {
	request := new(deleteRequest)
	if err := c.ShouldBindJSON(request); err != nil {
		respondError(c, err)
		return
	}
	service, err := memoryservice.DefaultGovernanceService()
	if err != nil {
		respondError(c, err)
		return
	}
	memory, revision, err := service.Archive(c.Request.Context(), c.GetInt64("userID"), c.Param("id"), request.Reason)
	if err != nil {
		respondError(c, err)
		return
	}
	response := memoryResponse{Memory: toMemoryView(memory), Revision: toRevisionView(revision)}
	response.Success()
	c.JSON(http.StatusOK, response)
}

func Revisions(c *gin.Context) {
	service, err := memoryservice.DefaultGovernanceService()
	if err != nil {
		respondError(c, err)
		return
	}
	revisions, err := service.Revisions(c.Request.Context(), c.GetInt64("userID"), c.Param("id"))
	if err != nil {
		respondError(c, err)
		return
	}
	views := make([]revisionView, 0, len(revisions))
	for i := range revisions {
		views = append(views, *toRevisionView(&revisions[i]))
	}
	response := revisionResponse{Revisions: views}
	response.Success()
	c.JSON(http.StatusOK, response)
}

func Correct(c *gin.Context) {
	request := new(correctionRequest)
	if err := c.ShouldBindJSON(request); err != nil {
		respondError(c, err)
		return
	}
	service, err := memoryservice.DefaultGovernanceService()
	if err != nil {
		respondError(c, err)
		return
	}
	memory, revision, err := service.Correct(c.Request.Context(), memoryservice.CorrectionRequest{
		UserID: c.GetInt64("userID"), MemoryID: c.Param("id"), Subject: request.Subject,
		Content: request.Content, Tags: request.Tags, Reason: request.Reason,
	})
	if err != nil {
		respondError(c, err)
		return
	}
	response := memoryResponse{Memory: toMemoryView(memory), Revision: toRevisionView(revision)}
	response.Success()
	c.JSON(http.StatusOK, response)
}

func Delete(c *gin.Context) {
	request := new(deleteRequest)
	if err := c.ShouldBindJSON(request); err != nil {
		respondError(c, err)
		return
	}
	service, err := memoryservice.DefaultGovernanceService()
	if err != nil {
		respondError(c, err)
		return
	}
	revision, err := service.Delete(c.Request.Context(), c.GetInt64("userID"), c.Param("id"), request.Reason)
	if err != nil {
		respondError(c, err)
		return
	}
	response := memoryResponse{Revision: toRevisionView(revision)}
	response.Success()
	c.JSON(http.StatusOK, response)
}

func toMemoryView(memory *model.LongTermMemory) *memoryView {
	if memory == nil {
		return nil
	}
	return &memoryView{
		ID: memory.ID, Scope: memory.Scope, Subject: memory.Subject, Content: memory.Content,
		ContentHash: memory.ContentHash,
		Tags:        memory.Tags, TopicKey: memory.TopicKey, Source: memorySourceLabel(memory.SourceRefs),
		ProfileClass: memory.ProfileClass, EvidenceMessages: memory.EvidenceMessages,
		AssistantMessages: memory.AssistantMessages, StatementTime: memory.StatementTime,
		SourceSession: memory.SourceSession,
		Durability:    memory.Durability, ValidUntil: memory.ValidUntil,
		Confidence: memory.Confidence, Importance: memory.Importance, Status: memory.Status, UserConfirmed: memory.UserConfirmed,
		LastUsedAt: memory.LastUsedAt, UsageCount: memory.UsageCount, CreatedAt: memory.CreatedAt, UpdatedAt: memory.UpdatedAt,
	}
}

func toRevisionView(revision *model.MemoryRevision) *revisionView {
	if revision == nil || revision.ID == "" {
		return nil
	}
	return &revisionView{
		ID: revision.ID, MemoryID: revision.MemoryID, Action: revision.Action, Actor: revision.Actor, Reason: revision.Reason,
		CreatedAt: revision.CreatedAt,
	}
}

func memorySourceLabel(refs model.StringList) string {
	for _, ref := range refs {
		switch {
		case strings.HasPrefix(ref, "user_api:"):
			return "用户直接创建"
		case strings.HasPrefix(ref, "memory_draft:"), strings.HasPrefix(ref, "agent_run:"):
			return "对话确认"
		case strings.HasPrefix(ref, "user_revision:"):
			return "用户纠正"
		case strings.HasPrefix(ref, "learning_attempt:"):
			return "学习记录"
		}
	}
	if len(refs) > 0 {
		return "系统整理"
	}
	return "未知"
}

func toDraftView(draft *model.MemoryDraft, conflictIndex map[string]memoryView) *draftView {
	if draft == nil {
		return nil
	}
	targets := make([]memoryView, 0, len(draft.ConflictIDs))
	for _, id := range draft.ConflictIDs {
		if view, ok := conflictIndex[id]; ok {
			targets = append(targets, view)
		}
	}
	return &draftView{
		ID: draft.ID, Scope: draft.Scope, Subject: draft.Subject, Content: draft.Content, Tags: draft.Tags,
		TopicKey: draft.TopicKey, Durability: draft.Durability, ValidUntil: draft.ValidUntil,
		ConflictIDs: draft.ConflictIDs, ConflictTargets: targets, Status: draft.Status, MemoryID: draft.MemoryID,
		CancelReason: draft.CancelReason, Source: draftSourceLabel(draft), SessionID: draft.SessionID,
		ProfileClass: draft.ProfileClass, EvidenceMessages: draft.EvidenceMessages,
		AssistantMessages: draft.AssistantMessages, StatementTime: draft.StatementTime, CreatedAt: draft.CreatedAt,
	}
}

// draftSourceLabel states where a pending draft came from. Every draft path is
// a conversation proposal today (the manual entry path writes active memory
// directly); the label exists so the page can show it and stay honest when a
// second draft path appears.
func draftSourceLabel(draft *model.MemoryDraft) string {
	return "对话提议"
}

// hydrateConflictTargets loads the active rows referenced by the pending
// drafts in one query, so each card can show the old content it would replace.
func hydrateConflictTargets(ctx context.Context, service *memoryservice.GovernanceService, userID int64, drafts []model.MemoryDraft) (map[string]memoryView, error) {
	ids := make([]string, 0, len(drafts)*2)
	for i := range drafts {
		ids = append(ids, drafts[i].ConflictIDs...)
	}
	index := make(map[string]memoryView, len(ids))
	if len(ids) == 0 {
		return index, nil
	}
	rows, err := service.MemoriesByIDs(ctx, userID, ids)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		index[rows[i].ID] = *toMemoryView(&rows[i])
	}
	return index, nil
}

// isBindingFailure reports whether err came from decoding or validating the
// request body. Such a failure is the caller's mistake, so it is answered as a
// bad request and never as a server failure.
func isBindingFailure(err error) bool {
	var validationErrors validator.ValidationErrors
	var typeError *json.UnmarshalTypeError
	var syntaxError *json.SyntaxError
	switch {
	case errors.As(err, &validationErrors):
		return true
	case errors.As(err, &typeError):
		return true
	case errors.As(err, &syntaxError):
		return true
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return true
	}
	return false
}

func respondError(c *gin.Context, err error) {
	status, businessCode := http.StatusInternalServerError, code.CodeServerBusy
	switch {
	case isBindingFailure(err):
		status, businessCode = http.StatusBadRequest, code.CodeInvalidParams
	case errors.Is(err, memorydao.ErrCandidateNotFound):
		status, businessCode = http.StatusNotFound, code.MemoryCandidateNotFound
	case errors.Is(err, memorydao.ErrMemoryNotFound):
		status, businessCode = http.StatusNotFound, code.MemoryNotFound
	case errors.Is(err, memorydao.ErrDreamBusy):
		status, businessCode = http.StatusConflict, code.MemoryDreamBusy
	case errors.Is(err, memorydao.ErrDreamConflict), errors.Is(err, memorydao.ErrMemoryConflict), errors.Is(err, memorydao.ErrMemoryWriteBusy):
		status, businessCode = http.StatusConflict, code.MemoryConflict
	case errors.Is(err, memoryservice.ErrGlobalMemoryCapacity):
		status, businessCode = http.StatusConflict, code.MemoryConflict
	case errors.Is(err, memorydao.ErrDerivedMemory):
		status, businessCode = http.StatusConflict, code.MemoryDerivedReadOnly
	case errors.Is(err, memorydao.ErrDraftNotFound):
		status, businessCode = http.StatusNotFound, code.MemoryDraftNotFound
	case errors.Is(err, memorydao.ErrMemoryTargetModified):
		// Handled by the resolve endpoint with a conflict payload; anything
		// else reaching this point still answers the same business code.
		status, businessCode = http.StatusConflict, code.MemoryTargetModified
	case errors.Is(err, memorydao.ErrDraftNotPending):
		status, businessCode = http.StatusConflict, code.MemoryConflict
	case errors.Is(err, memorydao.ErrDraftAttributionRequired):
		status, businessCode = http.StatusBadRequest, code.CodeInvalidParams
	case errors.Is(err, memoryservice.ErrInvalidCorrection), errors.Is(err, memoryservice.ErrInvalidDreamOutput), errors.Is(err, memoryservice.ErrInvalidExplicitWrite):
		status, businessCode = http.StatusBadRequest, code.CodeInvalidParams
	case errors.Is(err, memoryservice.ErrDreamNotConfigured):
		status, businessCode = http.StatusServiceUnavailable, code.CodeServerBusy
	}
	response := new(controller.Response)
	c.JSON(status, response.CodeOf(businessCode))
}
