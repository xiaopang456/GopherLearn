package devmodel

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/xiaopang456/GopherLearn/internal/domain"
)

// The two prompts this model answers are rendered by internal/service. The literals
// below are a second copy of two of their labels, and that coupling is the price of a
// ChatModel seam that carries no metadata about who is asking. It fails loudly: an
// unrecognized prompt is an error, not a guess, so a rename shows up as the quiz
// refusing to generate rather than as a generator's JSON being graded as an answer.
// internal/app's local-model test drives the real prompts through this code, which is
// what keeps the copy honest.
const (
	evidenceSectionLabel = "[证据]"
	topicLabel           = "主题："
	sectionJoiner        = " 出自《"
	sectionTerminator    = "》"
)

// evidenceHeaderLine matches one evidence block's first line as the workflow renders it:
// "[3] 出自《Redis 手册》 · AOF 持久化".
var evidenceHeaderLine = regexp.MustCompile(`^\[(\d+)\] 出自《`)

// evidenceSectionEnds are the labels that follow the evidence block. Recognising them
// keeps a weak-point or history line from being read as notes content.
var evidenceSectionEnds = []string{"[需要补测的薄弱点]", "[已出过的题]", "[必须避开]", "请输出 JSON"}

// generatedQuestion is one entry of the generator's answer. The field set and the key
// names are the contract in internal/service/learning_generate.go, and no extra key is
// representable here - which is the point of building the payload from a struct instead
// of writing it out as text.
type generatedQuestion struct {
	Type            string   `json:"type"`
	Question        string   `json:"question"`
	AssessmentGoal  string   `json:"assessment_goal"`
	ReferenceAnswer string   `json:"reference_answer"`
	Rubric          []string `json:"rubric"`
	Evidence        []int    `json:"evidence"`
}

type generationAnswer struct {
	Questions []generatedQuestion `json:"questions"`
}

// evidenceItem is one note fragment the workflow offered, reduced to what a scripted
// question needs: which number to cite, what to name in the text, and whether a code
// analysis question is even lawful.
type evidenceItem struct {
	ordinal int
	title   string
	hasCode bool
}

type generationPrompt struct {
	topic    string
	evidence []evidenceItem
}

// parseGenerationPrompt recognises the generator's user message.
func parseGenerationPrompt(user string) (generationPrompt, bool) {
	trimmed := strings.TrimSpace(user)
	if !strings.HasPrefix(trimmed, topicLabel) || !strings.Contains(trimmed, evidenceSectionLabel) {
		return generationPrompt{}, false
	}
	parsed := generationPrompt{}
	inEvidence := false
	var current *evidenceItem
	for _, line := range strings.Split(trimmed, "\n") {
		switch {
		case strings.HasPrefix(line, topicLabel):
			parsed.topic = strings.TrimSpace(strings.TrimPrefix(line, topicLabel))
		case strings.HasPrefix(line, evidenceSectionLabel):
			inEvidence = true
		case inEvidence && evidenceEndsLine(line):
			inEvidence = false
		case strings.HasPrefix(line, "["):
			match := evidenceHeaderLine.FindStringSubmatch(line)
			if match == nil {
				continue
			}
			ordinal, err := strconv.Atoi(match[1])
			if err != nil || ordinal <= 0 {
				continue
			}
			parsed.evidence = append(parsed.evidence, evidenceItem{
				ordinal: ordinal, title: evidenceTitle(line),
			})
			// Re-pointed every time rather than held across appends: the slice grows, and
			// a pointer into the old array would silently write into abandoned memory.
			current = &parsed.evidence[len(parsed.evidence)-1]
		case inEvidence && current != nil:
			if strings.Contains(line, "```") {
				current.hasCode = true
			}
		}
	}
	if parsed.topic == "" || len(parsed.evidence) == 0 {
		return generationPrompt{}, false
	}
	return parsed, true
}

// evidenceTitle pulls the document name out of an evidence header line. It is only used
// to make a question point at readable material, so a missing title is not a failure.
func evidenceTitle(line string) string {
	start := strings.Index(line, sectionJoiner)
	if start < 0 {
		return ""
	}
	rest := line[start+len(sectionJoiner):]
	end := strings.Index(rest, sectionTerminator)
	if end <= 0 {
		return ""
	}
	return rest[:end]
}

func evidenceEndsLine(line string) bool {
	for _, label := range evidenceSectionEnds {
		if strings.HasPrefix(line, label) {
			return true
		}
	}
	return false
}

// questionAngle is one of the five kinds of question the script can write. Each carries
// its own assessment goal, so "three questions that measure one thing three times" is
// impossible by construction rather than by a similarity check downstream.
//
// An angle resolves to one `wording` for the material it was actually given, and the
// resolver is the only place that choice is made. The first version of this file swapped
// type, goal and rubric while still rendering the question text from the code-reading
// template, which produced a short_answer asking a learner to read a code fragment in
// notes that held none - the wording half of a promise the other three halves had taken
// back. Four of the five angles have no code-dependent variant at all; their two wordings
// are the same value, and `wording` still reads the field it needs rather than guessing.
type questionAngle struct {
	// code is the wording that claims a code fragment is present. It is only lawful when
	// the cited evidence holds a fenced block, which domain.LearningQuestion enforces for
	// the code_analysis type and which nothing enforces for prose.
	code questionWord
	// plain is the wording for material with no fenced block in it.
	plain questionWord
}

// questionWord is one angle's whole claim about the material: what to ask, what the
// question measures, and what an answer has to contain.
type questionWord struct {
	kind   domain.LearningQuestionType
	prompt string
	goal   string
	rubric []string
}

// wording picks the angle's text for one cited fragment. Both fields carry a %s for the
// topic; the rubrics and goals stay distinct per angle so the batch never states one
// measurement twice.
func (a questionAngle) wording(citedHasCode bool) questionWord {
	if a.code.kind == domain.LearningQuestionCodeAnalysis && !citedHasCode {
		return a.plain
	}
	return a.code
}

// sameWording is the four angles that never claim a code fragment, written once so the
// absence of a variant is visible rather than implied.
func sameWording(word questionWord) questionAngle {
	return questionAngle{code: word, plain: word}
}

var questionAngles = []questionAngle{
	sameWording(questionWord{
		kind:   domain.LearningQuestionShortAnswer,
		goal:   "说明%q里各条做法分别解决什么问题",
		prompt: "就材料%q说明：这里记录的几条做法各自解决了什么问题，代价又是什么？",
		rubric: []string{"点出做法各自的目标", "说出至少一项代价"},
	}),
	sameWording(questionWord{
		kind:   domain.LearningQuestionConceptComparison,
		goal:   "比较%q中两种做法的适用条件",
		prompt: "围绕材料%q比较：两种做法分别在什么条件下更合适，失败时表现有何不同？",
		rubric: []string{"给出适用条件", "指出失败表现的差别"},
	}),
	sameWording(questionWord{
		kind:   domain.LearningQuestionShortAnswer,
		goal:   "说出%q的触发条件与实际后果",
		prompt: "根据材料%q回答：什么条件会触发这里的机制，触发之后系统实际做了什么？",
		rubric: []string{"说出触发条件", "说出触发后的实际动作"},
	}),
	{
		code: questionWord{
			kind:   domain.LearningQuestionCodeAnalysis,
			goal:   "读出%q这段配置改变了什么行为",
			prompt: "阅读材料%q中的代码片段：这段配置改变了什么行为，配错了会出现什么现象？",
			rubric: []string{"说出配置项的作用", "说出不当设置的后果"},
		},
		// The plain variant stays a comparison of two *described* settings rather than a
		// softened version of the same claim: prose about appendfsync is still something a
		// learner can be asked about, and the goal and rubric move with the question.
		plain: questionWord{
			kind:   domain.LearningQuestionConceptComparison,
			goal:   "比较%q里两种做法分别带来的后果",
			prompt: "就材料%q比较：文中提到的两种做法各会带来什么后果，为什么不能混为一谈？",
			rubric: []string{"分别说出两种做法的后果", "说出它们不可互换的理由"},
		},
	},
	sameWording(questionWord{
		kind:   domain.LearningQuestionConceptComparison,
		goal:   "辨析%q中容易混淆的两个说法",
		prompt: "辨析材料%q：其中两个容易混为一谈的说法分别指什么，为什么不能互换？",
		rubric: []string{"分别给出两个说法的含义", "说明不能互换的理由"},
	}),
}

// questionFocusClauses and goalFocusClauses carry the per-round variation.
//
// They exist because of a case a wide notebook hides: when only one chunk is admissible,
// every question cites the same evidence, so the workflow's duplicate rules weigh wording
// alone - and a question that differs from last week's only in a numeral is the same
// question. A clause of its own length is what moves the comparison off the thresholds.
//
// Seven entries, and the question angle cycles every five rounds, so 5 and 7 being
// coprime is what makes one slot's (angle, clause) pair unique for 35 rounds before a
// repeat becomes arithmetically possible.
var questionFocusClauses = []string{
	"请用一句因果说明作答，不要罗列名词",
	"请各举一个会出问题的场景来说明差别",
	"请指出被省略的那个前提并说明为什么重要",
	"请把结论放在最前面，再补一句理由",
	"请使用材料里的术语，不要换成日常说法",
	"请说明哪一种情况会让这个答案不再成立",
	"请先给出判据，再用判据评价两种做法",
}

var goalFocusClauses = []string{
	"要能区分前提与结论",
	"要能落到一个具体故障场景",
	"要能说出被省略的条件",
	"要能对比两个对象的取舍",
	"要能用自己的话复述机制",
	"要能指出常见的误解",
	"要能给出可验证的判断依据",
}

// renderQuestions writes the generator's answer for one quiz: exactly
// domain.LearningQuestionCount questions, citing only the ordinals the workflow showed.
//
// `round` counts generation calls on this instance, so consecutive quizzes on one topic
// differ in angle, wording and citation. It is not a claim that the questions are
// better or worse - a script cannot know what it is asking about.
func renderQuestions(prompt generationPrompt, round int) string {
	answers := generationAnswer{Questions: make([]generatedQuestion, 0, domain.LearningQuestionCount)}
	for slot := 0; slot < domain.LearningQuestionCount; slot++ {
		// The angle window slides by the question count each round, so a second quiz on
		// one topic asks different kinds of question, and the fifth round wrapping back to
		// the first angle is covered by the rotating clause and citation below.
		angle := questionAngles[((round-1)*domain.LearningQuestionCount+slot)%len(questionAngles)]
		cited := prompt.cite(slot, round)
		// One resolver, one wording: the question text, the type, the goal and the rubric
		// are the same decision about the same cited fragment.
		word := angle.wording(prompt.hasCode(cited))
		answers.Questions = append(answers.Questions, generatedQuestion{
			Type: string(word.kind),
			AssessmentGoal: fmt.Sprintf(word.goal, prompt.topic) + "；" +
				goalFocusClauses[(slot+round)%len(goalFocusClauses)],
			Question: fmt.Sprintf("%s（%s）",
				fmt.Sprintf(word.prompt, prompt.topic),
				roundLabel(round, slot)),
			ReferenceAnswer: fmt.Sprintf(
				"本地脚本参考答案，不是模型判断：围绕主题%q中%s，答出机制本身、它成立的前提以及代价，任一条说不出都算不完整。",
				prompt.topic, strings.Join(titleNames(prompt, cited), "、")),
			Rubric:   append([]string(nil), word.rubric...),
			Evidence: cited,
		})
	}
	encoded, err := json.Marshal(answers)
	if err != nil {
		// Every field here is a string or an int this package built, so a failure would be
		// a programming fault. Returning the error as an answer would store garbage.
		return `{"questions":[]}`
	}
	return string(encoded)
}

// roundLabel renders the quiz's position in this process's generation order, plus the
// rotating focus clause. Both are visible in the question text on purpose: a learner
// looking at a scripted quiz should be able to tell it is one.
func roundLabel(round, slot int) string {
	return fmt.Sprintf("第%s组·%s", chineseNumeral(round), questionFocusClauses[(slot+round)%len(questionFocusClauses)])
}

var chineseNumerals = []rune("一二三四五六七八九十")

func chineseNumeral(value int) string {
	if value <= 0 {
		return strconv.Itoa(value)
	}
	if value <= len(chineseNumerals) {
		return string(chineseNumerals[value-1])
	}
	return strconv.Itoa(value)
}

// cite picks the evidence ordinals one question is written from. Rotating by both slot
// and round moves the backing material between quizzes, which the workflow's own
// duplicate rules weigh as heavily as the wording.
func (prompt generationPrompt) cite(slot, round int) []int {
	total := len(prompt.evidence)
	if total == 0 {
		return nil
	}
	primary := prompt.evidence[(slot+round)%total].ordinal
	cited := []int{primary}
	if slot == 1 && total > 2 {
		second := prompt.evidence[(slot+round+1)%total].ordinal
		if second != primary {
			cited = append(cited, second)
		}
	}
	return cited
}

// hasCode reports whether any cited fragment holds a code fence, which is what makes a
// code analysis question lawful at all.
func (prompt generationPrompt) hasCode(ordinals []int) bool {
	for _, item := range prompt.evidence {
		for _, ordinal := range ordinals {
			if item.ordinal == ordinal && item.hasCode {
				return true
			}
		}
	}
	return false
}

func titleNames(prompt generationPrompt, ordinals []int) []string {
	names := make([]string, 0, len(ordinals))
	for _, ordinal := range ordinals {
		for _, item := range prompt.evidence {
			if item.ordinal != ordinal {
				continue
			}
			if item.title != "" {
				names = append(names, "《"+item.title+"》")
				break
			}
		}
	}
	if len(names) == 0 {
		names = append(names, "所引证据")
	}
	return names
}
