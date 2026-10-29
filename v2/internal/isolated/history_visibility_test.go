package isolated

import (
	"GopherAI/model"
	"context"
	"encoding/json"
	"fmt"
	"github.com/cloudwego/eino/schema"
	"gorm.io/gorm"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func assertHistoryCapability(t *testing.T, chat *captureModel, want bool) {
	t.Helper()
	requests := chat.all()
	if len(requests) == 0 {
		t.Fatal("no actual model request")
	}
	found := false
	if !want {
		for _, r := range requests {
			for _, info := range r.Tools {
				if info["name"] == "read_history_message" {
					t.Fatalf("unexpected history capability in an actual request: %s", requestText(chat))
				}
			}
		}
	}
	for _, info := range requests[0].Tools {
		if info["name"] != "read_history_message" {
			continue
		}
		found = true
		expected, err := os.ReadFile("testdata/history-with-grant.json")
		if err != nil {
			t.Fatal(err)
		}
		actual, err := json.Marshal(info)
		if err != nil {
			t.Fatal(err)
		}
		var a, b any
		if json.Unmarshal(actual, &a) != nil || json.Unmarshal(expected, &b) != nil || !reflect.DeepEqual(a, b) {
			t.Fatalf("original history description/Schema changed: %s", actual)
		}
	}
	t.Logf("ACTUAL_HISTORY_VISIBILITY expected=%v found=%v requests=%s", want, found, requestText(chat))
	if found != want {
		t.Fatalf("actual first request history capability=%v want=%v: %s", found, want, requestText(chat))
	}
}
func seedHistorySummary(t *testing.T, db *gorm.DB, session string, document model.SessionSummaryDocument, watermark uint) {
	t.Helper()
	content, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Create(&model.SessionSummary{UserID: 1, SessionID: session, Content: string(content), CoveredMessageID: watermark, Version: 1, TokenEstimate: 100}).Error; err != nil {
		t.Fatal(err)
	}
}
func seedHistoryEpisode(t *testing.T, db *gorm.DB, session string, id uint, synopsis string) {
	t.Helper()
	episode := model.EpisodeMemory{ID: "visibility-episode", UserID: 1, SessionID: session, Title: "之前的决定", Document: model.EpisodeDocument{Synopsis: synopsis, SourceHandles: []string{fmt.Sprintf("message:%d", id)}, Decisions: []model.EpisodeEvidence{{Text: "离线核对原实现", EvidenceMessageIDs: []uint{id}}}}, SearchText: "离线核对原实现", TopicKeys: model.StringList{}, SourceLastMessageID: id, LastMessageAt: time.Now().Add(-time.Hour), Version: 1, Status: model.EpisodeStatusActive, IndexStatus: model.EpisodeIndexIndexed}
	if err := db.Create(&episode).Error; err != nil {
		t.Fatal(err)
	}
}
func TestHistoryCapabilityRequiresVisibleUsableSummaryGrantThroughHTTP(t *testing.T) {
	for _, kind := range []string{"trimmed", "undecodable", "past-watermark", "zero-version", "legacy-no-summary-grants"} {
		t.Run(kind, func(t *testing.T) {
			s := testSettings(t)
			if kind != "legacy-no-summary-grants" {
				toolMode(s)
			}
			if kind == "trimmed" {
				s.Config.AgentConfig.ContextMaxSummaryTokens = 128
			}
			app, db, chat, token := newFixture(t, s, func(context.Context, int) (*schema.Message, error) {
				return schema.AssistantMessage("你好", nil), nil
			})
			session := createSession(t, app, token, "create-summary-visibility")
			message := model.Message{SessionID: session, UserName: "offline-user", Content: "离线约束", IsUser: true}
			if err := db.Create(&message).Error; err != nil {
				t.Fatal(err)
			}
			goal := "核对约束"
			if kind == "trimmed" {
				goal = strings.Repeat("长目标前缀", 500)
			}
			cited := message.ID
			if kind == "past-watermark" {
				cited = message.ID + 100
			}
			seedHistorySummary(t, db, session, model.SessionSummaryDocument{SchemaVersion: model.SessionSummarySchemaV2, CurrentGoal: goal, ConfirmedDecisions: model.SessionSummaryEvidenceList{{Text: "离线约束", EvidenceMessageIDs: []uint{cited}}}}, message.ID)
			if kind == "undecodable" {
				if err := db.Model(&model.SessionSummary{}).Where("session_id = ?", session).Update("content", "not-json evidence_message_ids").Error; err != nil {
					t.Fatal(err)
				}
			}
			if kind == "zero-version" {
				if err := db.Model(&model.SessionSummary{}).Where("session_id = ?", session).Update("version", 0).Error; err != nil {
					t.Fatal(err)
				}
			}
			runHTTP(t, app, token, session, "run-summary-visibility", "你好")
			assertHistoryCapability(t, chat, false)
			if kind == "trimmed" {
				shown := false
				for _, request := range chat.all() {
					for _, m := range request.Messages {
						if strings.HasPrefix(m.Content, "Session summary (") {
							shown = true
							if strings.Contains(m.Content, "evidence_message_ids") {
								t.Fatal("fixture did not trim cited summary tail")
							}
						}
					}
				}
				if !shown {
					t.Fatal("no actual clipped summary block")
				}
			}
		})
	}
}
func TestUnavailableEpisodeGrantDoesNotExposeHistoryThroughHTTP(t *testing.T) {
	for _, kind := range []string{"trimmed", "archived", "zero-version"} {
		t.Run(kind, func(t *testing.T) {
			s := testSettings(t)
			s.Config.AgentConfig.EpisodeRecallMode = "inject_enabled"
			app, db, chat, token := newFixture(t, s, func(context.Context, int) (*schema.Message, error) {
				return schema.AssistantMessage("没有可用原句", nil), nil
			})
			prior := createSession(t, app, token, "create-unavailable-prior")
			message := model.Message{SessionID: prior, UserName: "offline-user", Content: "PRIVATE_EPISODE_ORIGINAL", IsUser: true}
			if err := db.Create(&message).Error; err != nil {
				t.Fatal(err)
			}
			synopsis := "上次决定"
			if kind == "trimmed" {
				synopsis = strings.Repeat("超长回忆前缀", 1200)
			}
			seedHistoryEpisode(t, db, prior, message.ID, synopsis)
			if kind == "archived" {
				if err := db.Model(&model.EpisodeMemory{}).Where("id = ?", "visibility-episode").Update("status", "archived").Error; err != nil {
					t.Fatal(err)
				}
			}
			if kind == "zero-version" {
				if err := db.Model(&model.EpisodeMemory{}).Where("id = ?", "visibility-episode").Update("version", 0).Error; err != nil {
					t.Fatal(err)
				}
			}
			current := createSession(t, app, token, "create-unavailable-current")
			result := runHTTP(t, app, token, current, "run-unavailable-episode", "上次我的原话是什么")
			if kind == "zero-version" {
				if result["status_code"] != float64(5003) || len(chat.all()) != 0 {
					t.Fatalf("invalid Episode version must be rejected before model request: %+v %s", result, requestText(chat))
				}
				t.Log("ORIGINAL_CONTEXT_REJECTED invalid Episode version before model request")
			} else {
				assertHistoryCapability(t, chat, false)
			}
		})
	}
}
func TestHistoryExecutionRevalidatesOriginalRulesThroughHTTP(t *testing.T) {
	for _, fault := range []string{"version-change", "episode-owner-change", "source-session-owner-change", "ungranted-id", "ungranted-owned-id", "message-role-change"} {
		t.Run(fault, func(t *testing.T) {
			s := testSettings(t)
			s.Config.AgentConfig.EpisodeRecallMode = "inject_enabled"
			var db *gorm.DB
			var chat *captureModel
			var grantedID, foreignID, ungrantedID uint
			var prior string
			app, d, c, token := newFixture(t, s, func(_ context.Context, n int) (*schema.Message, error) {
				if n == 1 {
					// Fault injection after ContextBuilder and Tools have built the actual
					// model request, before original handler store revalidation. No grant is supplied.
					assertHistoryCapability(t, chat, true)
					var err error
					switch fault {
					case "version-change":
						err = db.Model(&model.EpisodeMemory{}).Where("id = ?", "visibility-episode").Update("version", 2).Error
					case "episode-owner-change":
						err = db.Model(&model.EpisodeMemory{}).Where("id = ?", "visibility-episode").Update("user_id", 2).Error
					case "message-role-change":
						err = db.Model(&model.Message{}).Where("id = ?", grantedID).Update("is_user", false).Error
					case "source-session-owner-change":
						err = db.Model(&model.Session{}).Where("id = ?", prior).Update("user_id", 2).Error
					}
					if err != nil {
						t.Fatal(err)
					}
					requested := grantedID
					if fault == "ungranted-id" {
						requested = foreignID
					}
					if fault == "ungranted-owned-id" {
						requested = ungrantedID
					}
					return toolCall("read_history_message", fmt.Sprintf(`{"message_ids":[%d]}`, requested)), nil
				}
				return readAnswer(chat)
			})
			db = d
			chat = c
			prior = createSession(t, app, token, "create-revalidate-prior")
			message := model.Message{SessionID: prior, UserName: "offline-user", Content: "PRIVATE_GRANTED_ORIGINAL", IsUser: true}
			if err := db.Create(&message).Error; err != nil {
				t.Fatal(err)
			}
			grantedID = message.ID
			seedHistoryEpisode(t, db, prior, grantedID, "上次决定")
			ungranted := model.Message{SessionID: prior, UserName: "offline-user", Content: "PRIVATE_UNGRANTED_ORIGINAL", IsUser: true}
			if err := db.Create(&ungranted).Error; err != nil {
				t.Fatal(err)
			}
			ungrantedID = ungranted.ID
			other := model.Session{ID: "foreign-session", UserID: 2, UserName: "foreign-user", Mode: model.SessionModeAgent}
			if err := db.Create(&other).Error; err != nil {
				t.Fatal(err)
			}
			foreign := model.Message{SessionID: other.ID, UserName: "foreign-user", Content: "PRIVATE_FOREIGN_ORIGINAL", IsUser: true}
			if err := db.Create(&foreign).Error; err != nil {
				t.Fatal(err)
			}
			foreignID = foreign.ID
			current := createSession(t, app, token, "create-revalidate-current")
			result := runHTTP(t, app, token, current, "run-revalidate", "上次我的原话是什么")
			if result["toolCalls"] != float64(1) {
				t.Fatalf("original history handler did not run: %+v", result)
			}
			requests := chat.all()
			if len(requests) < 2 {
				t.Fatal("no subsequent actual model request")
			}
			found := false
			for _, m := range requests[1].Messages {
				if m.Role != schema.Tool || m.ToolName != "read_history_message" {
					continue
				}
				var output struct {
					Messages       []json.RawMessage `json:"messages"`
					Refused        []uint            `json:"refused"`
					RefusalReasons []struct {
						Reason string `json:"reason"`
					} `json:"refusal_reasons"`
				}
				if err := json.Unmarshal([]byte(m.Content), &output); err != nil {
					t.Fatal(err)
				}
				if len(output.Messages) != 0 {
					t.Fatalf("refused raw message leaked: %s", m.Content)
				}
				if fault == "source-session-owner-change" || fault == "message-role-change" {
					found = true
				} else {
					requested := grantedID
					if fault == "ungranted-id" {
						requested = foreignID
					}
					if fault == "ungranted-owned-id" {
						requested = ungrantedID
					}
					if len(output.Refused) != 1 || output.Refused[0] != requested {
						t.Fatalf("missing original refusal: %s", m.Content)
					}
					found = true
					if fault == "version-change" && (len(output.RefusalReasons) != 1 || output.RefusalReasons[0].Reason != "episode_version_mismatch") {
						t.Fatalf("wrong original version refusal: %s", m.Content)
					}
				}
			}
			if !found {
				t.Fatalf("no actual history refusal observation: %s", requestText(chat))
			}
			if strings.Contains(requestText(chat), "PRIVATE_GRANTED_ORIGINAL") || strings.Contains(requestText(chat), "PRIVATE_FOREIGN_ORIGINAL") || strings.Contains(requestText(chat), "PRIVATE_UNGRANTED_ORIGINAL") {
				t.Fatal("unauthorized original reached model")
			}
		})
	}
}
