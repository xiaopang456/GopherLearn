package episode

import (
	"testing"
	"time"
)

var planClock = time.Date(2026, 9, 1, 9, 0, 0, 0, time.FixedZone("CST", 8*3600))

func planOf(question string) MemoryReadPlan {
	return BuildReadPlan(RouteRequest{Question: question, Now: planClock})
}

// A question that names nothing but the last session keeps the old semantics.
// A question that names the last session *and* a subject must not: that is the
// axis collapse the plan exists to remove.
func TestPlanSeparatesLatestAnyFromLatestMatching(t *testing.T) {
	bare := planOf("上次聊了什么？")
	if !bare.NeedEpisode || bare.TemporalMode != TemporalLatestAny || bare.HasTopicConstraint() {
		t.Fatalf("bare last-session plan=%+v", bare)
	}
	if bare.EvidenceMode != EvidenceAny {
		t.Fatalf("bare last-session must not demand structured evidence: %+v", bare)
	}

	topical := planOf("上次关于缓冲区复用的决定是什么？")
	if topical.TemporalMode != TemporalLatestMatching {
		t.Fatalf("topical last-session plan=%+v", topical)
	}
	if topical.EvidenceMode != EvidenceDecision {
		t.Fatalf("decision evidence lost: %+v", topical)
	}
	if !containsTerm(topical.TopicTerms, "缓冲区复用") && !containsTerm(topical.TopicTerms, "缓冲") {
		t.Fatalf("topic terms lost: %+v", topical.TopicTerms)
	}
}

// Time, topic and evidence type all survive together. The old router kept
// whichever one it matched first.
func TestPlanKeepsDateTopicAndEvidenceTogether(t *testing.T) {
	plan := planOf("2026-08-25 关于向量迁移定了什么？")
	if plan.TemporalMode != TemporalRange {
		t.Fatalf("temporal=%s", plan.TemporalMode)
	}
	if plan.From.IsZero() || !plan.From.Before(plan.To) {
		t.Fatalf("range=%s..%s", plan.From, plan.To)
	}
	if plan.EvidenceMode != EvidenceDecision {
		t.Fatalf("evidence=%s", plan.EvidenceMode)
	}
	if !plan.RequireExactTopic || len(plan.ExactTopicKeys) != 1 || plan.ExactTopicKeys[0] != "向量迁移" {
		t.Fatalf("topic keys=%v", plan.ExactTopicKeys)
	}
}

// The three history routes: an explicit session pointer, a past pointer, and a
// first-person past predicate that is being asked about.
func TestPlanRecognisesHistoryPhrasingsBeyondOneWordList(t *testing.T) {
	cases := []struct {
		question string
		reason   string
	}{
		{"那次聊天最后是什么结果？", "history_session_pointer"},
		{"当初怎么定的消息确认方式？", "history_past_pointer"},
		{"后来纠正了什么理解？", "history_past_pointer"},
		{"消息确认方式我是怎么定下来的？", "implicit_history_decision"},
		{"读缓冲那个遗留问题现在怎么推进？", "implicit_history_open_loop"},
	}
	for _, tc := range cases {
		t.Run(tc.question, func(t *testing.T) {
			plan := planOf(tc.question)
			if !plan.NeedEpisode {
				t.Fatalf("plan=%+v", plan)
			}
			if !containsTerm(plan.ReasonCodes, tc.reason) {
				t.Fatalf("reason codes=%v want %q", plan.ReasonCodes, tc.reason)
			}
		})
	}
}

// The other half of the same rule. An ordinary knowledge question, an ordinary
// past-tense narration and an attributive "the X I set" are not requests for
// personal history, and treating them as such is what puts unrelated sessions
// into answers that never asked for one.
func TestPlanDoesNotInventPersonalHistory(t *testing.T) {
	for _, question := range []string{
		"TCP 粘包是什么？",
		"为什么 RRF 用倒数而不是直接加权？",
		"布隆过滤器的误判率怎么算？",
		"Raft 的选举过程是怎样的？",
		"我给 Kafka 定的消费确认策略是什么？",
		"我现在平时怎么决定分片大小？",
	} {
		t.Run(question, func(t *testing.T) {
			plan := planOf(question)
			if plan.NeedEpisode {
				t.Fatalf("plan=%+v", plan)
			}
			if plan.Reason() != "no_personal_history_intent" {
				t.Fatalf("reason=%v", plan.ReasonCodes)
			}
		})
	}
}

// Shell words are removed before the topic is read, so a question that is all
// shell must not acquire a topic it never had. Faking one turns latest_any into
// latest_matching and empties a read that was answering correctly.
func TestPlanDoesNotFakeATopicOutOfShellWords(t *testing.T) {
	for _, question := range []string{
		"上一次会话我们最后在做什么？",
		"上一轮聊天的结论是什么？",
		"上次那轮聊天我们最后定下来什么了？",
	} {
		t.Run(question, func(t *testing.T) {
			plan := planOf(question)
			if plan.TemporalMode != TemporalLatestAny {
				t.Fatalf("plan=%+v", plan)
			}
			if plan.HasTopicConstraint() {
				t.Fatalf("shell-only question grew a topic: terms=%v keys=%v", plan.TopicTerms, plan.ExactTopicKeys)
			}
		})
	}
}

// The explicit topic ends where the sentence turns back to the speaker. Before
// this the whole remainder became the key, which no episode could ever carry.
func TestPlanCutsExplicitTopicAtTheSentenceBoundary(t *testing.T) {
	cases := []struct{ question, want string }{
		{"关于 分块切片 我之前聊到哪一步了？", "分块切片"},
		{"关于 连接池 之前那次讨论有什么结果？", "连接池"},
		{"关于 gorm软删除 我们讨论过什么？", "gorm软删除"},
		{"关于 jwt鉴权 那次聊天的走向是什么？", "jwt鉴权"},
	}
	for _, tc := range cases {
		t.Run(tc.question, func(t *testing.T) {
			plan := planOf(tc.question)
			if !plan.RequireExactTopic || len(plan.ExactTopicKeys) != 1 || plan.ExactTopicKeys[0] != tc.want {
				t.Fatalf("keys=%v want %q", plan.ExactTopicKeys, tc.want)
			}
		})
	}
}

func TestPlanReadsEvidenceTypeIndependently(t *testing.T) {
	cases := []struct {
		question string
		want     EvidenceMode
	}{
		{"上次关于加锁顺序的决定是什么？", EvidenceDecision},
		{"上次我在锁上纠正了什么？", EvidenceCorrection},
		{"粘包那边还欠着的那件事怎么推进？", EvidenceOpenLoop},
		{"上一次会话里是什么让我改变了看法？", EvidenceAny},
	}
	for _, tc := range cases {
		t.Run(tc.question, func(t *testing.T) {
			if got := planOf(tc.question).EvidenceMode; got != tc.want {
				t.Fatalf("evidence=%s want %s", got, tc.want)
			}
		})
	}
}

// The compatibility view must stay a view. It is allowed to lose axes; it is
// not allowed to be what production reads.
func TestLegacyRouteIsALossyProjectionOfThePlan(t *testing.T) {
	request := RouteRequest{Question: "上次关于缓冲区复用的决定是什么？", Now: planClock}
	plan := NewQueryRouter().Plan(request)
	route := NewQueryRouter().Route(request)
	if plan.TemporalMode != TemporalLatestMatching || plan.EvidenceMode != EvidenceDecision {
		t.Fatalf("plan=%+v", plan)
	}
	if route.Intent != IntentDecisionHistory || !route.NeedRecall {
		t.Fatalf("route=%+v", route)
	}
}

func containsTerm(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
