package hybrid

import (
	"GopherAI/common/rag"
	agentController "GopherAI/controller/agent"
	messageDAO "GopherAI/dao/message"
	agentService "GopherAI/service/agent"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xiaopang456/GopherLearn/conversation"
	"io"
	"net/http"
	"strconv"
	"sync"
)

type publishedBlock struct {
	id, text, kind, publication string
	round                       int
}
type agentWireSink struct {
	mu       sync.Mutex
	writer   http.ResponseWriter
	cancel   context.CancelFunc
	blocks   []*publishedBlock
	terminal bool
	writeErr error
	steps    bool
	failure  *agentService.Event
}

func (s *agentWireSink) emit(ctx context.Context, name string, payload map[string]any) error {
	if s.writeErr != nil {
		return s.writeErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	bytes, err := json.Marshal(payload)
	if err == nil {
		frame := "event: " + name + "\ndata: " + string(bytes) + "\n\n"
		var n int
		n, err = io.WriteString(s.writer, frame)
		if err == nil && n != len(frame) {
			err = io.ErrShortWrite
		}
	}
	if err != nil {
		s.writeErr = err
		s.cancel()
		return err
	}
	s.writer.(http.Flusher).Flush()
	return nil
}
func (s *agentWireSink) Observe(ctx context.Context, e agentService.PublicText) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.terminal {
		return errors.New("public text after terminal event")
	}
	var b *publishedBlock
	if e.Phase == "agent.text.started" {
		b = &publishedBlock{id: fmt.Sprintf("%s:public:%d", e.RunID, e.Round), round: e.Round, publication: e.Publication}
		s.blocks = append(s.blocks, b)
	} else {
		for _, v := range s.blocks {
			if v.round == e.Round && v.id == fmt.Sprintf("%s:public:%d", e.RunID, e.Round) {
				b = v
				break
			}
		}
		if b == nil {
			return errors.New("public model block has no opening")
		}
	}
	payload := map[string]any{"run_id": e.RunID, "block_id": b.id, "round": e.Round, "publication": e.Publication}
	if e.Content != "" {
		payload["content"] = e.Content
		b.text += e.Content
	}
	if e.Kind != "" {
		payload["kind"] = e.Kind
		b.kind = e.Kind
	}
	return s.emit(ctx, e.Phase, payload)
}
func (s *agentWireSink) Send(ctx context.Context, e agentService.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	payload := map[string]any{"run_id": e.RunID}
	switch e.Type {
	case agentService.EventRunStarted:
		return s.emit(ctx, "message.started", payload)
	case agentService.EventAnswerDelta:
		if !s.steps {
			payload["content"] = e.Content
			payload["publication"] = "validated_snapshot"
			return s.emit(ctx, "message.delta", payload)
		}
	case agentService.EventStepStarted, agentService.EventToolStarted, agentService.EventToolResult:
		if !s.steps {
			return nil
		}
		if e.Sequence != 0 {
			payload["sequence"] = e.Sequence
		}
		if e.StepType != "" {
			payload["step_type"] = e.StepType
		}
		if e.ToolName != "" {
			payload["tool_name"] = e.ToolName
		}
		if e.DurationMillis != 0 {
			payload["duration_millis"] = e.DurationMillis
		}
		// Native tool-result summaries may contain raw evidence. Only public
		// progress metadata is translated; arguments/results never reach this wire.
		return s.emit(ctx, string(e.Type), payload)
	case agentService.EventWaitingConfirmation:
		if !s.steps {
			return nil
		}
		if e.MemoryDraft != nil {
			d := e.MemoryDraft
			payload["memory_draft"] = map[string]any{"draft_id": d.DraftID, "scope": d.Scope, "content": d.Content, "topic_key": d.TopicKey}
		}
		return s.emit(ctx, "waiting_confirmation", payload)
	case agentService.EventRunFailed:
		// Classification uses the original returned error, including acquisition
		// busy/conflict errors. Preserve the native event's public failure detail.
		s.failure = &e
	}
	// answer_delta is already a complete validated answer, not provider fragments.
	// run_completed is deferred until Service.Run returns and the associated
	// persisted assistant is read. Replays have no fabricated steps or blocks.
	return nil
}
func (s *httpSurface) agentStream(w http.ResponseWriter, r *http.Request, uid int64, name string) {
	if !noQuery(w, r) {
		return
	}
	var input struct {
		Content         string                              `json:"content"`
		RequestID       string                              `json:"requestId"`
		KnowledgePolicy rag.KnowledgePolicy                 `json:"knowledgePolicy"`
		TopicConsent    agentService.TopicConsentSubmission `json:"topicConsent"`
		StreamSteps     bool                                `json:"stream_steps"`
	}
	if err := conversation.DecodeRequest(w, r, &input); err != nil {
		apiError(w, 400, "invalid_request", "request body is invalid")
		return
	}
	request := agentService.RunRequest{UserID: uid, UserName: name, SessionID: sessionID(r), Question: input.Content, RequestID: input.RequestID, KnowledgePolicy: input.KnowledgePolicy, TopicConsent: input.TopicConsent}
	if err := agentService.ValidateRunRequestForAssembly(request); err != nil {
		apiError(w, 400, "invalid_request", "Agent request is invalid")
		return
	}
	if _, ok := w.(http.Flusher); !ok {
		apiError(w, 500, "internal_error", "streaming is unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	sink := &agentWireSink{writer: w, cancel: cancel, steps: input.StreamSteps}
	if input.StreamSteps {
		ctx = agentService.WithPublicTextObserver(ctx, sink.Observe)
	}
	result, err := s.agent.Agent.Run(ctx, request, sink)
	if err == nil {
		var rowErr error
		row, rowErr := messageDAO.NewRepository(s.db).ReadOwnedRunAssistant(ctx, uid, sessionID(r), result.RunID)
		if rowErr != nil {
			err = rowErr
		} else if row.Content != result.Answer {
			err = errors.New("persisted Agent answer disagrees with RunResult")
		} else {
			sink.mu.Lock()
			payload := map[string]any{"run_id": result.RunID, "message_id": strconv.FormatUint(uint64(row.ID), 10), "content": row.Content, "original_v2": result}
			if len(result.Sources) > 0 {
				payload["sources"] = result.Sources
			}
			publication := "validated_snapshot"
			if result.Replayed {
				publication = "replay"
			} else {
				for i := len(sink.blocks) - 1; i >= 0; i-- {
					b := sink.blocks[i]
					if b.kind == "answer_candidate" && b.text == row.Content {
						payload["final_block_id"] = b.id
						publication = b.publication
						break
					}
				}
			}
			payload["publication"] = publication
			err = sink.emit(ctx, "message.completed", payload)
			if err == nil {
				sink.terminal = true
			}
			sink.mu.Unlock()
		}
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if err != nil && !sink.terminal && sink.writeErr == nil && ctx.Err() == nil {
		native := agentController.ErrorCodeForAssembly(err)
		payload := map[string]any{"run_id": result.RunID, "code": "agent_run_failed", "native_code": native, "message": native.Msg()}
		if sink.failure != nil {
			payload["message"] = sink.failure.Message
			payload["termination_reason"] = sink.failure.TerminationReason
		}
		_ = sink.emit(ctx, "message.failed", payload)
		sink.terminal = true
	}
}
