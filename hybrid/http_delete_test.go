package hybrid

import (
	"GopherAI/config"
	"GopherAI/model"
	"context"
	"errors"
	"github.com/redis/go-redis/v9"
	"strings"
	"sync"
	"testing"
	"time"
)

// Only the network DEL operation is substituted. The assembled original Redis
// EpisodeIndex and original Session cascade DAO remain production code.
type httpRedisTransport struct {
	redisTransport
	mu   sync.Mutex
	keys []string
	fail bool
}

func (h *httpRedisTransport) open(*config.Config) (*redis.Client, error) {
	client := redis.NewClient(&redis.Options{Addr: "offline.invalid:6379", MaxRetries: -1})
	client.AddHook(h)
	return client, nil
}
func (h *httpRedisTransport) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	base := h.redisTransport.ProcessHook(next)
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() != "del" {
			return base(ctx, cmd)
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, arg := range cmd.Args()[1:] {
			h.keys = append(h.keys, arg.(string))
		}
		if h.fail {
			err := errors.New("synthetic Redis DEL transport failure")
			cmd.SetErr(err)
			return err
		}
		cmd.(*redis.IntCmd).SetVal(int64(len(cmd.Args()) - 1))
		return nil
	}
}
func TestHTTPSessionDeleteUsesOriginalCascadeAndDerivedIndex(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "cleaned", true: "cleanup-error"}[failure], func(t *testing.T) {
			transport := &httpRedisTransport{fail: failure}
			app, db, _, _, token, other := httpFixtureWithModel(t, nil, transport)
			sid := createHTTP(t, app.Handler, token, "delete aggregate")
			dataHTTP(t, app.Handler, "POST", "/api/v1/sessions/"+sid+"/messages", `{"content":"stored before delete"}`, token)
			now := time.Now().UTC()
			fixtures := []any{
				&model.AgentRun{ID: "delete-run", UserID: 1, SessionID: sid, Status: model.AgentRunCompleted, StartedAt: now},
				&model.AgentStep{UserID: 1, RunID: "delete-run", Sequence: 1, Type: model.AgentStepType("model")},
				&model.SessionSummary{UserID: 1, SessionID: sid, Content: "stored summary", CoveredMessageID: 2, Version: 1},
				&model.EpisodeMemory{ID: "delete-episode", UserID: 1, SessionID: sid, Title: "episode", Status: model.EpisodeStatusArchived, IndexStatus: model.EpisodeIndexIndexed, LastMessageAt: now},
				&model.EpisodeProjectionJob{ID: "delete-projection", UserID: 1, SessionID: sid, Status: model.EpisodeProjectionCompleted, DesiredMessageID: 2, AvailableAt: now},
				&model.MemoryDraft{ID: "delete-draft", UserID: 1, SessionID: sid, RunID: "delete-run", IdempotencyKey: "delete-draft-key", Status: model.MemoryDraftStatus("rejected")},
				&model.MemoryExtractionJob{ID: "delete-memory-job", UserID: 1, SourceSessionID: sid, EventType: model.MemoryEventType("conversation_completed"), SourceID: "delete-source", Status: model.MemoryJobStatus("completed"), AvailableAt: now},
				&model.MemoryCandidate{ID: "delete-candidate", JobID: "delete-memory-job", UserID: 1, Status: model.MemoryCandidateStatus("rejected")},
				&model.MemoryDreamRun{ID: "delete-dream", UserID: 1, CandidateID: "delete-candidate", Status: model.MemoryDreamRunStatus("completed"), StartedAt: now},
			}
			for _, row := range fixtures {
				if err := db.Create(row).Error; err != nil {
					t.Fatal(err)
				}
			}
			st, e, _ := requestHTTP(t, app.Handler, "DELETE", "/api/v1/sessions/"+sid, "", other)
			if st != 404 || e["error"] == nil {
				t.Fatal("foreign cascade accepted")
			}
			st, e, _ = requestHTTP(t, app.Handler, "DELETE", "/api/v1/sessions/"+sid, "", token)
			if failure {
				if st != 503 || e["error"].(map[string]any)["code"] != "derived_cleanup_failed" {
					t.Fatalf("cleanup error hidden %d %v", st, e)
				}
			} else {
				if st != 200 || e["error"] != nil {
					t.Fatalf("delete %d %v", st, e)
				}
			}
			for _, table := range []string{"sessions", "messages", "agent_runs", "agent_steps", "session_summaries", "episode_memories", "episode_projection_jobs", "memory_drafts", "memory_extraction_jobs", "memory_candidates", "memory_dream_runs"} {
				var count int64
				if err := db.Table(table).Count(&count).Error; err != nil || count != 0 {
					t.Fatalf("original cascade left %s count=%d err=%v", table, count, err)
				}
			}
			transport.mu.Lock()
			keys := append([]string(nil), transport.keys...)
			transport.mu.Unlock()
			if len(keys) != 1 || !strings.HasSuffix(keys[0], "1:delete-episode") {
				t.Fatalf("derived cleanup seam discarded IDs: %v", keys)
			}
			st, _, _ = requestHTTP(t, app.Handler, "GET", "/api/v1/sessions/"+sid+"/messages", "", token)
			if st != 404 {
				t.Fatal("deleted history readable")
			}
		})
	}
}
