package agent

import (
	learningdao "GopherAI/dao/learning"
	"GopherAI/model"
	"testing"
)

func attemptText(topicKey, question string, missing ...string) learningdao.TopicAttemptRecord {
	return learningdao.TopicAttemptRecord{
		LearningAttempt: model.LearningAttempt{TopicKey: topicKey, MissingPoints: missing},
		QuestionText:    question,
	}
}

// A term shared by two topics cannot decide between them. Keeping it would make
// every question about either topic reach both.
func TestSharedTermsAreNotEvidence(t *testing.T) {
	terms := buildTopicTerms([]learningdao.TopicAttemptRecord{
		attemptText("rabbitmq-消息确认", "消费者设置 autoAck=true 后崩溃，消息会怎样？"),
		attemptText("kafka-消费组", "消费者组重平衡时 autoAck 的语义是什么？"),
	})
	// autoack appears under both, so it identifies neither.
	if hit := terms.hits("rabbitmq-消息确认", "autoAck 为什么危险", minDistinctiveBigrams); hit != 0 {
		t.Fatalf("a term used by two topics must not be evidence, got %d", hit)
	}
	// 崩溃 belongs to the first alone, but one Han bigram is not enough on its own.
	if hit := terms.hits("rabbitmq-消息确认", "崩溃了会怎样", minDistinctiveBigrams); hit != 0 {
		t.Fatalf("a lone Han bigram must not be evidence, got %d", hit)
	}
}

// The first version of this file counted both kinds of term alike and took
// correct rejection from 0.917 to 0.417: common words like 服务 and 分组 became
// "distinctive" simply because one topic happened to use them.
func TestIdentifierCountsAloneButHanBigramsNeedTwo(t *testing.T) {
	// Three topics, because a term unique to one of two is not distinctive and
	// the index refuses to treat it as evidence.
	terms := buildTopicTerms([]learningdao.TopicAttemptRecord{
		attemptText("go-并发", "goroutine 之间通过 channel 通信时阻塞行为有何不同？", "数据竞争"),
		attemptText("redis-ttl", "key 过期后是立刻删除吗？惰性删除与定期删除的触发时机？"),
		attemptText("rabbitmq-确认", "autoAck 为什么危险？"),
	})

	if hit := terms.hits("go-并发", "goroutine 怎么退出", minDistinctiveBigrams); hit == 0 {
		t.Fatal("one ASCII identifier is rare enough to stand alone")
	}
	// 惰性 and 删除 both come from the same topic, so two bigrams coincide.
	if hit := terms.hits("redis-ttl", "惰性删除和定期删除的区别", minDistinctiveBigrams); hit == 0 {
		t.Fatal("two Han bigrams from one topic are evidence")
	}
	// 竞争 alone is one bigram: cheap, and exactly the shape that produced the
	// seven false matches.
	if hit := terms.hits("go-并发", "市场竞争激烈吗", minDistinctiveBigrams); hit != 0 {
		t.Fatalf("a single Han bigram must not fire, got %d", hit)
	}
}

func TestStopTermsNeverBecomeEvidence(t *testing.T) {
	terms := buildTopicTerms([]learningdao.TopicAttemptRecord{
		attemptText("gin-路由分组", "路由分组的实现是什么情况下需要使用的问题？"),
		attemptText("redis-ttl", "key 过期后是立刻删除吗？"),
		attemptText("rabbitmq-确认", "autoAck 为什么危险？"),
	})
	// Every one of these is in the stop list, so a question built from them
	// alone reaches nothing.
	if hit := terms.hits("gin-路由分组", "这个需要使用什么，什么情况下的问题", minDistinctiveBigrams); hit != 0 {
		t.Fatalf("stop terms must not accumulate into a match, got %d", hit)
	}
}

func TestUnknownTopicAndNilIndexAreSafe(t *testing.T) {
	var absent *topicTerms
	if hit := absent.hits("anything", "任何问题", minDistinctiveBigrams); hit != 0 {
		t.Fatalf("a nil index must report nothing, got %d", hit)
	}
	terms := buildTopicTerms(nil)
	if hit := terms.hits("never-indexed", "任何问题", minDistinctiveBigrams); hit != 0 {
		t.Fatalf("an unindexed topic must report nothing, got %d", hit)
	}
	// A record without a topic key cannot be filed, and must not panic.
	if built := buildTopicTerms([]learningdao.TopicAttemptRecord{attemptText("", "无主题")}); len(built.distinctive) != 0 {
		t.Fatalf("a record without a topic key must be skipped: %+v", built.distinctive)
	}
}

func TestExtractTermsSplitsScriptsAndDropsShortWords(t *testing.T) {
	got := extractTerms("Go 用 sync.WaitGroup 控制并发")
	for _, want := range []string{"sync", "waitgroup", "控制", "制并", "并发"} {
		if _, ok := got[want]; !ok {
			t.Errorf("expected term %q in %v", want, keysOf(got))
		}
	}
	// "go" is two characters; identifiers must be at least three so that short
	// fragments do not become evidence.
	if _, ok := got["go"]; ok {
		t.Error("two-character identifiers must be dropped")
	}
}

func keysOf(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	return out
}

// A learner who has been quizzed on two subjects has almost no vocabulary that
// is genuinely characteristic of one of them: any word appearing in one and not
// the other looks unique, whether it names the subject or not. This is not a
// hypothetical — the first real account to reach production had two topics, and
// "帮我讲讲 Go 的 map 实现" was answered with the Codec 注册表 assessment
// because that topic's quiz text mentioned a Type→Factory map.
func TestDistinctivenessNeedsEnoughTopicsToDistinguish(t *testing.T) {
	twoTopics := buildTopicTerms([]learningdao.TopicAttemptRecord{
		attemptText("codec-注册表", "注册表如何保证 Type 到 Factory 的映射唯一？"),
		attemptText("rpc粘包处理", "PacketBuffer 如何判断一条完整消息已经到达？"),
	})
	if hit := twoTopics.hits("codec-注册表", "注册表怎么保证唯一", minDistinctiveBigrams); hit != 0 {
		t.Fatalf("two topics cannot establish distinctiveness, got %d", hit)
	}

	// The same evidence becomes usable once there is a third subject the term
	// is actually absent from.
	threeTopics := buildTopicTerms([]learningdao.TopicAttemptRecord{
		attemptText("codec-注册表", "注册表如何保证 Type 到 Factory 的映射唯一？"),
		attemptText("rpc粘包处理", "PacketBuffer 如何判断一条完整消息已经到达？"),
		attemptText("redis-ttl", "key 过期后是立刻删除吗？"),
	})
	if hit := threeTopics.hits("codec-注册表", "注册表怎么保证唯一", minDistinctiveBigrams); hit == 0 {
		t.Fatal("with three topics the same term is evidence again")
	}
}

// A name is evidence; a keyword is not. Both appear in quiz text, and only the
// first says anything about which subject a question is asking after.
func TestGenericIdentifiersAreNotEvidence(t *testing.T) {
	terms := buildTopicTerms([]learningdao.TopicAttemptRecord{
		attemptText("codec-注册表", "注册表用 map 保存 type 到 factory 的 value 映射"),
		attemptText("rpc粘包处理", "PacketBuffer 如何判断一条完整消息已经到达？"),
		attemptText("redis-ttl", "key 过期后是立刻删除吗？"),
	})
	for _, question := range []string{"Go 的 map 实现", "type 是什么", "value 怎么取"} {
		if hit := terms.hits("codec-注册表", question, minDistinctiveBigrams); hit != 0 {
			t.Fatalf("%q must not claim a topic, got %d", question, hit)
		}
	}
	if hit := terms.hits("rpc粘包处理", "PacketBuffer 怎么用", minDistinctiveBigrams); hit == 0 {
		t.Fatal("a real identifier is still evidence")
	}
}
