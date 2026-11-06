package conversation

import "testing"

// The read side's report is only as good as the three parsers that read the
// window back, and as the scorer that decides whether the answer used the
// value the board says is current. Each is checked against the renderer's real
// output and the answer shapes the model really produced, rather than against
// a shape invented here: a parser that agrees with a fixture the renderer
// never writes measures nothing.

func TestAReplacedFigureIsScoredAgainstWhatTheQuestionAsksFor(t *testing.T) {
	cases := []struct {
		name     string
		question string
		answer   string
		value    string
		want     bool
	}{
		{
			name:     "a question about the original is answered by naming the change",
			question: "“梧苑”计划我最初每次读多少页？",
			answer:   "你最初定的是每次读 15 页，后来改成了 30 页。",
			value:    "30页",
			want:     false,
		},
		{
			name:     "a question about the current value is answered by saying what it replaced",
			question: "“楠濑”现在要求材料提前几天交？",
			answer:   "“楠濑”现在要求提前 5 天交材料；之前说的 3 天已不作数。",
			value:    "3天",
			want:     false,
		},
		{
			name:     "the answer gives the replaced figure as the current one",
			question: "“楠濑”现在要求材料提前几天交？",
			answer:   "现在要求 3 天交材料。",
			value:    "3天",
			want:     true,
		},
		{
			name:     "the answer quotes the original wrongly and calls it original",
			question: "“梧苑”计划我最初每次读多少页？",
			answer:   "你最初定的是 30 页。",
			value:    "30页",
			want:     true,
		},
		{
			name:     "a change marker far away does not excuse the figure",
			question: "“梧苑”计划我最初每次读多少页？",
			answer:   "规定是你的计划后来调整过一次，现在每次读 30 页，这一点已经写进计划表里了。",
			value:    "30页",
			want:     true,
		},
		{
			name:     "the excusing words of one tense do not excuse the figure in the other",
			question: "“楠濑”现在要求材料提前几天交？",
			answer:   "后来改成了 3 天交材料。",
			value:    "3天",
			want:     true,
		},
		{
			name:     "a question with only one value in the session excuses nothing",
			question: "我那一叠需要重做的卡片，固定标签叫什么？",
			answer:   "标签叫 芩涧，之前想叫 别的名字。",
			value:    "别的名字",
			want:     true,
		},
		{
			name:     "a value the answer never names is not an answer given with it",
			question: "“梧苑”计划我最初每次读多少页？",
			answer:   "你最初定的是 15 页。",
			value:    "30页",
			want:     false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			markers := markersFor(tenseOf(c.question))
			if got := mentionsOutsidePhrasing(c.answer, c.value, markers); got != c.want {
				t.Fatalf("mentionsOutsidePhrasing(%q, %q) with tense %q = %t, want %t",
					c.answer, c.value, tenseOf(c.question), got, c.want)
			}
		})
	}
}

func TestTheQuestionTenseIsReadFromTheWordsTheScenarioUses(t *testing.T) {
	cases := []struct {
		question string
		want     string
	}{
		{"“梧苑”计划我最初每次读多少页？", tenseOriginal},
		{"“楠濑”现在要求材料提前几天交？", tenseCurrent},
		{"“栗矶”计划现在每次读多少页？", tenseCurrent},
		{"我那一叠需要重做的卡片，固定标签叫什么？", tenseUnknown},
	}
	for _, c := range cases {
		t.Run(c.question, func(t *testing.T) {
			if got := tenseOf(c.question); got != c.want {
				t.Fatalf("tenseOf(%q) = %q, want %q", c.question, got, c.want)
			}
		})
	}
}

// TestAReplacedFigureNamedFirstIsNotAPass guards the one case the phrasing
// rule gets wrong on its own.
//
// "最初 30 页，后来 15 页" answers a question about the original with the
// figure that was replaced. The figure sits next to 后来, so the phrasing rule
// calls it a remark about a change and would pass an answer that is wrong.
// The order is what separates the two readings, and the driver reports the
// case as ambiguous rather than starting to guess.
func TestAReplacedFigureNamedFirstIsNotAPass(t *testing.T) {
	cases := []struct {
		name     string
		answer   string
		inverted bool
	}{
		{
			name:     "the replaced figure is the answer",
			answer:   "你最初是 30 页，后来改成 15 页。",
			inverted: true,
		},
		{
			name:     "the accepted figure is the answer",
			answer:   "你最初定的是 15 页，后来改成了 30 页。",
			inverted: false,
		},
		{
			name:     "only the accepted figure is named",
			answer:   "你最初定的是 15 页。",
			inverted: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mentionsBefore(c.answer, []string{"30页"}, []string{"15页"}); got != c.inverted {
				t.Fatalf("mentionsBefore(%q) = %t, want %t", c.answer, got, c.inverted)
			}
		})
	}
}

// TestTheRawMentionSurvivesThePhrasingRule keeps the two readings apart: a
// reader who distrusts the heuristic needs to see that the forbidden figure
// was there at all, whatever the heuristic made of it.
func TestTheRawMentionSurvivesThePhrasingRule(t *testing.T) {
	answer := "你最初定的是 15 页，后来改成了 30 页。"
	if !mentionsAny(answer, []string{"30页"}) {
		t.Fatal("the raw reading lost a figure the answer names")
	}
	if mentionsOutsidePhrasing(answer, "30页", markersFor(tenseOf("我最初每次读多少页？"))) {
		t.Fatal("the phrasing rule did not excuse a figure it should have")
	}
}

func TestTheBlocksTheBuilderSendsAreReadBackByTheirOwnHeaders(t *testing.T) {
	block := "Summary segments (compressed record of earlier turns in this session; the Current state block above is what is true now):\n" +
		"[第 2 段 · 消息 25-48 · 2026-02-08 09:40]\n" +
		"话题：备份与演练\n" +
		"- 计划每次读 15 页 〔第 46 轮已改为 30 页，以当前状态为准〕 [message_ids=25,26]\n" +
		"\n[第 3 段 · 消息 49-66 · 2026-02-08 10:20]\n" +
		"话题：演练节奏\n" +
		"- 故障演练按人员流动定 [message_ids=49]\n"
	seqs := segmentSeqsIn(block)
	if len(seqs) != 2 || seqs[0] != 2 || seqs[1] != 3 {
		t.Fatalf("segment numbers = %v, want [2 3]", seqs)
	}
	marks := supersessionMarksIn(block)
	if len(marks) != 1 || marks[0] != "〔第 46 轮已改为 30 页，以当前状态为准〕" {
		t.Fatalf("marks = %v, want the one the second segment carries", marks)
	}
}

// TestTheBoardHeaderIsNotCountedAsAReplacedValue guards the mark counter: the
// header explains the notation by printing it, and a window with a board would
// otherwise always report a replaced value.
func TestTheBoardHeaderIsNotCountedAsAReplacedValue(t *testing.T) {
	header := `Current state (server state; what is true now; a value carrying 〔…已改为…〕 was changed later, so the unmarked value is the current one):` +
		"\n- 阅读计划 [active]: 每次读 30 页 (value: 30页) [message_ids=45]"
	if marks := supersessionMarksIn(header); len(marks) != 0 {
		t.Fatalf("marks = %v, want none: the header only describes the notation", marks)
	}
}

// TestTheVisibleIDsAreTheOnesTheWindowPrinted guards the authorization check:
// the ids that may be read back are the ones the sent text carries, and the
// check must find them where the renderer puts them rather than where a
// segment's range happens to be.
func TestTheVisibleIDsAreTheOnesTheWindowPrinted(t *testing.T) {
	block := "- 阅读计划 [active]: 每次读 30 页 (value: 30页) [message_ids=45,46]\n" +
		"- 故障演练按人员流动定 [message_ids=49]"
	ids := evidenceIDsIn(block)
	want := []uint{45, 46, 49}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	for index, id := range want {
		if ids[index] != id {
			t.Fatalf("ids = %v, want %v", ids, want)
		}
	}
	if ids := evidenceIDsIn("no marker here"); len(ids) != 0 {
		t.Fatalf("ids = %v, want none without a marker", ids)
	}
	if ids := evidenceIDsIn("[message_ids=0]"); len(ids) != 0 {
		t.Fatalf("ids = %v, want none: zero is not a message", ids)
	}
}

// TestABlockTheRendererNoLongerWritesYieldsNothing keeps the parsers honest
// about their own failure: a guess would put a segment number in the report
// that the window never carried.
func TestABlockTheRendererNoLongerWritesYieldsNothing(t *testing.T) {
	if seqs := segmentSeqsIn("第 2 段 · 消息 25-48"); len(seqs) != 0 {
		t.Fatalf("segment numbers = %v, want none without the renderer's bracket", seqs)
	}
	if seqs := segmentSeqsIn("[第 段 · 消息 25-48]"); len(seqs) != 0 {
		t.Fatalf("segment numbers = %v, want none without a number", seqs)
	}
}
