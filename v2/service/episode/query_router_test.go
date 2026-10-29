package episode

import (
	"testing"
	"time"
)

func TestQueryRouterRequiresExplicitHistoryIntent(t *testing.T) {
	router := NewQueryRouter()
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.Local)
	cases := []struct {
		name, q string
		history bool
		intent  QueryIntent
		need    bool
	}{
		{"ordinary", "RPC 粘包怎么处理", false, IntentNone, false},
		{"last", "上次我们聊了什么", false, IntentLastSession, true},
		{"last full phrase", "上一次会话我们最后在做什么", false, IntentLastSession, true},
		{"last round", "上一轮聊天的结论是什么", false, IntentLastSession, true},
		{"latest conversation", "最近一次会话在做什么", false, IntentLastSession, true},
		{"previous conversation", "前一个会话的结论是什么", false, IntentLastSession, true},
		{"continue empty", "继续上次的方案", false, IntentLastSession, true},
		{"continue previous time", "接着上一次会话继续", false, IntentLastSession, true},
		{"continue current", "继续上次的方案", true, IntentNone, false},
		{"continue previous time current", "接着上一次会话继续", true, IntentNone, false},
		{"decision", "之前 RPC 粘包最后决定怎么做", false, IntentDecisionHistory, true},
		{"topic", "以前讨论过 Redis 索引吗", false, IntentTopicHistory, true},
		{"time", "上周我们聊了什么", false, IntentTimeRange, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := router.Route(RouteRequest{Question: tc.q, Now: now, CurrentSessionHasHistory: tc.history})
			if got.Intent != tc.intent || got.NeedRecall != tc.need {
				t.Fatalf("route=%+v", got)
			}
		})
	}
}

func TestQueryRouterTimeRangeIsHalfOpen(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.Local)
	got := NewQueryRouter().Route(RouteRequest{Question: "昨天聊了什么", Now: now})
	if got.From.Day() != 19 || got.To.Day() != 20 || !got.From.Before(got.To) {
		t.Fatalf("range=%s..%s", got.From, got.To)
	}
}

func TestQueryRouterExtractsExplicitTopicAndRejectsBroadAcronymIdentifier(t *testing.T) {
	route := NewQueryRouter().Route(RouteRequest{Question: "之前关于 RPC 粘包的决定和未完成项是什么？"})
	if !route.RequireExactTopic || len(route.ExactTopicKeys) != 1 || route.ExactTopicKeys[0] != "rpc-粘包" {
		t.Fatalf("explicit topic route=%+v", route)
	}
	if len(route.ExactIdentifiers) != 0 {
		t.Fatalf("RPC must not be promoted to a rare exact identifier: %+v", route.ExactIdentifiers)
	}
	identifiers := exactIdentifiers("之前 PacketBuffer 和 Content-Length 怎么决定")
	if len(identifiers) != 2 || identifiers[0] != "PacketBuffer" || identifiers[1] != "Content-Length" {
		t.Fatalf("strong identifiers=%v", identifiers)
	}
}

func TestQueryRouterTrimsExplicitTopicQuestionPredicates(t *testing.T) {
	cases := []struct {
		question string
		want     string
	}{
		{"过去关于 TCP 粘包选择了什么方案？", "tcp-粘包"},
		{"以前关于 RabbitMQ 可靠投递怎么取舍的？", "rabbitmq-可靠投递"},
		{"曾经关于 RAG 检索完整性选了什么方案？", "rag-检索完整性"},
		{"过去关于 Docker 部署最终采用了哪种做法？", "docker-部署"},
		{"历史上关于 SSE 断连当时定下了什么方案？", "sse-断连"},
		{"以前围绕 Go 并发最终怎么处理的？", "go-并发"},
		{"曾经关于 Redis 索引当时的取舍是什么？", "redis-索引"},
		{"历史上关于 JWT 鉴权的结论是什么？", "jwt-鉴权"},
		{"之前关于 RPC 粘包的决定和未完成项是什么？", "rpc-粘包"},
		{"以前围绕登录流程最后怎么做？", "登录流程"},
	}
	for _, tc := range cases {
		t.Run(tc.question, func(t *testing.T) {
			route := NewQueryRouter().Route(RouteRequest{Question: tc.question})
			if !route.RequireExactTopic || len(route.ExactTopicKeys) != 1 || route.ExactTopicKeys[0] != tc.want {
				t.Fatalf("route=%+v want topic %q", route, tc.want)
			}
		})
	}
}

func TestQueryRouterKeepsDecisionWordsInsideTopicNames(t *testing.T) {
	cases := []struct {
		question string
		want     string
	}{
		{"之前关于登录方案是什么？", "登录方案"},
		{"之前关于选择排序的决定是什么？", "选择排序"},
		{"之前关于方案选择聊了什么？", "方案选择"},
	}
	for _, tc := range cases {
		t.Run(tc.question, func(t *testing.T) {
			topic, ok := explicitTopicKey(tc.question)
			if !ok || topic != tc.want {
				t.Fatalf("topic=%q ok=%v want=%q", topic, ok, tc.want)
			}
		})
	}
}
