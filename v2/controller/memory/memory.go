package memory

import (
	"GopherAI/common/code"
	"GopherAI/controller"
	memorydao "GopherAI/dao/memory"
	"GopherAI/model"
	memoryservice "GopherAI/service/memory"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
)

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
}

type draftView struct {
	ID          string                  `json:"id"`
	Scope       model.MemoryScope       `json:"scope"`
	Subject     string                  `json:"subject"`
	Content     string                  `json:"content"`
	Tags        model.StringList        `json:"tags"`
	TopicKey    string                  `json:"topic_key,omitempty"`
	Durability  model.MemoryDurability  `json:"durability"`
	ValidUntil  *time.Time              `json:"valid_until,omitempty"`
	ConflictIDs model.StringList        `json:"conflict_ids"`
	Status      model.MemoryDraftStatus `json:"status"`
	MemoryID    string                  `json:"memory_id,omitempty"`
	CreatedAt   time.Time               `json:"created_at"`
}

type revisionResponse struct {
	controller.Response
	Revisions []revisionView `json:"revisions"`
}

type memoryView struct {
	ID            string                     `json:"id"`
	Scope         model.MemoryScope          `json:"scope"`
	Subject       string                     `json:"subject"`
	Content       string                     `json:"content"`
	Tags          model.StringList           `json:"tags"`
	TopicKey      string                     `json:"topic_key"`
	Durability    model.MemoryDurability     `json:"durability"`
	ValidUntil    *time.Time                 `json:"valid_until,omitempty"`
	Source        string                     `json:"source"`
	Confidence    float64                    `json:"confidence"`
	Importance    int                        `json:"importance"`
	Status        model.LongTermMemoryStatus `json:"status"`
	UserConfirmed bool                       `json:"user_confirmed"`
	LastUsedAt    *time.Time                 `json:"last_used_at,omitempty"`
	UsageCount    uint64                     `json:"usage_count"`
	CreatedAt     time.Time                  `json:"created_at"`
	UpdatedAt     time.Time                  `json:"updated_at"`
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
	if scopeValue == "" { // compatibility for pre-AGENT-027 clients
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
	for i := range drafts {
		draftViews = append(draftViews, *toDraftView(&drafts[i]))
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
	result, err := service.ConfirmDraft(c.Request.Context(), memoryservice.ConfirmDraftRequest{UserID: c.GetInt64("userID"), DraftID: c.Param("id"), Action: request.Action, TargetMemoryID: request.TargetMemoryID, Reason: request.Reason})
	if err != nil {
		respondError(c, err)
		return
	}
	response := memoryResponse{Draft: toDraftView(&result.Draft), Memory: toMemoryView(result.Memory), Revision: toRevisionView(result.Revision), Replay: result.Replay}
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
		Tags: memory.Tags, TopicKey: memory.TopicKey, Source: memorySourceLabel(memory.SourceRefs),
		Durability: memory.Durability, ValidUntil: memory.ValidUntil,
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

func toDraftView(draft *model.MemoryDraft) *draftView {
	if draft == nil {
		return nil
	}
	return &draftView{ID: draft.ID, Scope: draft.Scope, Subject: draft.Subject, Content: draft.Content, Tags: draft.Tags, TopicKey: draft.TopicKey, Durability: draft.Durability, ValidUntil: draft.ValidUntil, ConflictIDs: draft.ConflictIDs, Status: draft.Status, MemoryID: draft.MemoryID, CreatedAt: draft.CreatedAt}
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
	case errors.Is(err, memoryservice.ErrInvalidCorrection), errors.Is(err, memoryservice.ErrInvalidDreamOutput), errors.Is(err, memoryservice.ErrInvalidExplicitWrite):
		status, businessCode = http.StatusBadRequest, code.CodeInvalidParams
	case errors.Is(err, memoryservice.ErrDreamNotConfigured):
		status, businessCode = http.StatusServiceUnavailable, code.CodeServerBusy
	}
	response := new(controller.Response)
	c.JSON(status, response.CodeOf(businessCode))
}
