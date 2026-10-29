package service

import (
	"math"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// The matcher decides which stored topic, if any, a live question is about. These cases
// are the boundary it draws, stated as data rather than as prose: each row says either
// "this question names this topic" or "it does not, and saying so would be a lie".

func TestTopicMatcherNormalizesBeforeComparing(t *testing.T) {
	// One topic spelled six ways must produce one answer. Case, spacing, full-width
	// punctuation and hyphenation are all things a learner types without thinking, and a
	// gate that only fires on the exact stored string would go quiet on almost every
	// real question.
	const topic = "Redis 持久化"
	for _, question := range []string{
		topic,
		"redis 持久化",
		"REDIS  持久化",
		"redis-持久化",
		"「Redis」持久化！",
		"  Redis　持久化  ",
		"Redis持久化",
	} {
		if score := newLearningTopicMatcher(question).coverage(topic); score < 1 {
			t.Fatalf("question %q scored %v, want a full match against %q", question, score, topic)
		}
	}
	// The same question against a differently-punctuated topic name is the same answer:
	// the normalization is shared, not applied to one side only.
	if score := newLearningTopicMatcher(topic).coverage("REDIS！！持久化"); score < 1 {
		t.Fatalf("topic-side punctuation lost the match: %v", score)
	}

	// And the same has to hold when an English keyword is the *only* thing carrying the
	// match. The block above can be satisfied by the Chinese half of a topic name alone,
	// so on its own it would keep passing with the question-side normalisation deleted -
	// which is exactly how that mutant first survived a run. Here the topic's Chinese
	// part shares no bigram with the question, so an unnormalised "GO" simply does not
	// equal the stored "go" and the case fails loudly.
	const asciiCarried = "Go 上下文与调度器"
	for _, question := range []string{
		"GO 的 goroutine 泄漏怎么查？",
		"go 的 goroutine 泄漏怎么查",
		"「GO」goroutine 泄漏怎么查！",
		"GO,goroutine 泄漏怎么查",
	} {
		if score := newLearningTopicMatcher(question).coverage(asciiCarried); score < 1 {
			t.Fatalf("question %q scored %v against %q, want the keyword to carry it",
				question, score, asciiCarried)
		}
	}
	// The other side of the same rule: a different keyword is not the stored one, however
	// it is capitalised.
	if _, loaded := newLearningTopicMatcher("RUST 的 goroutine 泄漏").match(asciiCarried); loaded {
		t.Fatal("an unrelated keyword loaded the topic")
	}
}

func TestTopicMatcherRequiresTheQuestionToSayTheTopic(t *testing.T) {
	cases := []struct {
		name     string
		question string
		topic    string
		want     float64
		matches  bool
	}{
		{
			name:     "an English keyword inside a Chinese sentence",
			question: "Redis 的持久化怎么做？", topic: "Redis 持久化",
			matches: true,
		},
		{
			name:     "only the qualifier is dropped",
			question: "缓存穿透怎么解决？", topic: "Redis 缓存",
			// The motivating case: a learner drilled on "Redis 缓存" asks about 缓存
			// and means exactly that. Averaging over the whole name would reject it.
			matches: true,
		},
		{
			name:     "a stray letter is not a keyword",
			question: "讲讲 G 这个字母。", topic: "G",
			// A one-rune part is too common to be evidence, so a topic named that
			// shortly can never be loaded by accident.
			matches: false,
		},
		{
			name:     "a short acronym inside a longer word",
			question: "google 的搜索怎么做？", topic: "Go 调度",
			// Whole-word only, or "go" is found inside "google" and every Go judgement
			// in the database lands on a question about a company.
			matches: false,
		},
		{
			name:     "the same acronym written on its own",
			question: "go 的调度器是什么？", topic: "Go 调度",
			matches: true,
		},
		{
			name:     "a neighbouring subject shares no bigram",
			question: "Kafka 的分区再平衡怎么触发？", topic: "Redis 持久化",
			matches: false,
		},
		{
			name:     "one bigram out of three is not a match",
			question: "粘包是怎么发生的？", topic: "RPC 粘包处理",
			// 粘包 is present and 处理 is not: below the line, because a question that
			// asks what a thing is has not committed to the topic that names it.
			matches: false, want: 1.0 / 3.0,
		},
		{
			name:     "the same topic named completely",
			question: "rpc 粘包处理有哪些方案？", topic: "RPC 粘包处理",
			matches: true,
		},
		{
			name:     "the keyword carries it and the Chinese half is absent",
			question: "Redis 的淘汰策略怎么选？", topic: "Redis 持久化",
			// Scoring takes the best part of a topic name, not its last one. This question
			// states "Redis" and nothing of 持久化, so a score read off the final part would
			// be zero and the note would vanish for a perfectly on-topic question.
			matches: true,
		},
		{
			name:     "nothing typed at all",
			question: "   ", topic: "Redis 持久化",
			matches: false,
		},
		{
			name:     "a topic with nothing stored",
			question: "Redis 持久化", topic: "!!!",
			matches: false,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			matcher := newLearningTopicMatcher(testCase.question)
			score := matcher.coverage(testCase.topic)
			_, loaded := matcher.match(testCase.topic)
			if loaded != testCase.matches {
				t.Fatalf("match(%q) loaded = %v, want %v (coverage %v)",
					testCase.topic, !testCase.matches, testCase.matches, score)
			}
			if testCase.want > 0 && math.Abs(score-testCase.want) > 0.001 {
				t.Fatalf("coverage = %v, want %v", score, testCase.want)
			}
		})
	}
}

func TestTopicMatcherScoreIsBoundedAndRepeatable(t *testing.T) {
	// The half is a judgement about the cost of a wrong topic, so it is pinned as a
	// literal: a silent move of the line changes which notes reach a model.
	if domain.LearningTopicMatchMinCoverage != 0.5 {
		t.Fatalf("coverage floor = %v, want the documented 0.5", domain.LearningTopicMatchMinCoverage)
	}
	questions := []string{
		"Redis 持久化里 RDB 和 AOF 各失去什么？",
		"缓存穿透",
		"完全不相关的东西",
		"",
	}
	topics := []string{"Redis 持久化", "Go 调度器", "Kafka", "Redis 缓存"}
	for _, question := range questions {
		matcher := newLearningTopicMatcher(question)
		for _, topic := range topics {
			first := matcher.coverage(topic)
			if first < 0 || first > 1 {
				t.Fatalf("coverage %v for (%q,%q) is outside 0..1", first, question, topic)
			}
			for repeat := range 3 {
				if again := matcher.coverage(topic); again != first {
					t.Fatalf("repeat %d of (%q,%q) scored %v, want %v", repeat, question, topic, again, first)
				}
			}
		}
	}
}

func TestSplitLearningTopicPartsBreaksAtScriptAndSeparator(t *testing.T) {
	for _, testCase := range []struct {
		value string
		want  []string
	}{
		{"redis-持久化", []string{"redis", "持久化"}},
		{"rpc粘包处理", []string{"rpc", "粘包处理"}},
		{"aof_重写", []string{"aof", "重写"}},
		{"调度", []string{"调度"}},
		{"", nil},
		{"---", nil},
	} {
		got := splitLearningTopicParts(testCase.value)
		if len(got) != len(testCase.want) {
			t.Fatalf("%q -> %q, want %q", testCase.value, got, testCase.want)
		}
		for index := range got {
			if got[index] != testCase.want[index] {
				t.Fatalf("%q -> part %d = %q, want %q", testCase.value, index, got[index], testCase.want[index])
			}
		}
	}
}
