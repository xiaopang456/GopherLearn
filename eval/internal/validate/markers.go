package validate

// RefusalMarkers are the phrases the product refuses outright.
//
// The list and the matching rule are copied from the product
// (v2/service/agent/runner.go, requiresExactHistoryEvidence): the question is
// lowercased and trimmed, then each marker is tested with a plain substring
// match. There is no word boundary, so "逐字" inside a longer phrase counts.
//
// A question containing any of these can never be answered from a document, so
// the generator must not produce one.
var RefusalMarkers = []string{
	"原话",
	"一字不差",
	"逐字",
	"原文",
	"我说了什么",
	"我怎么说",
	"当时怎么说",
}

// HistoryRecallMarkers are the phrases the product treats as a question about
// the learner's own past (v2/service/agent/runner.go, isHistoryRecallQuestion).
//
// They are not refusals, but a document corpus cannot answer them: the product
// routes them to memory recall instead of a knowledge search. A question
// carrying one of these would measure the wrong thing, so they are reported
// as warnings.
var HistoryRecallMarkers = []string{
	"你还记得", "还记得我", "我当时", "我之前", "我上次", "之前我", "上次我",
	"我的原话", "我说了什么", "我怎么说", "当时怎么说", "一字不差", "逐字", "原文",
	"之前我们", "前面我们", "本次对话", "当前对话", "本会话", "当前会话",
}
