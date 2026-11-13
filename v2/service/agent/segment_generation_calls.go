package agent

import (
	"fmt"
	"strings"

	"GopherAI/common/rag"
	"GopherAI/model"
)

// How the mode that records the assistant's products fits its input.
//
// The assistant's own words are what make the input big. A batch of
// sixty-four turns carries thirty-two assistant messages, and thirty-two of
// them at six hundred tokens is nearly three times the input budget the
// placeholder mode needs - so the batch is split at the boundary between a
// turn and the answer to it, and each call's assistant excerpts share one
// budget.
//
// The rule here is the one the measurements asked for: the assistant's
// excerpts never make a batch fail. They are shortened, then degraded to the
// placeholder, and only what is left of the learner's own words decides
// whether a call fits.

// assistantExcerptShare is the share of the input budget the assistant's
// excerpts may take in one call.
//
// Two fifths. The excerpts are what the mode adds, and they are what has to
// give way first: a call that carries the learner's words and the board is
// worth more than one that carries a longer excerpt of the assistant's
// answer.
const assistantExcerptShare = 2

// assistantExcerptFloor is the smallest excerpt an assistant message may
// keep.
//
// Below it the excerpt says nothing, and a message the model can see but
// cannot read is worse than a message that is not there: the model counts it
// as something it was shown and could not use. A message that cannot keep
// this much is degraded to the placeholder the mode that is off sends.
const assistantExcerptFloor = 16

// assistantPlaceholder is the body an assistant message carries when it is
// degraded.
//
// It is the same placeholder the mode that does not record the assistant's
// products sends, so a degraded message costs what a plain placeholder costs
// and says the same thing: there was an answer here, and it is not authority.
const assistantPlaceholder = "[assistant body omitted: not authoritative durable user state]"

// shortenedTurnFloor is how much of a turn survives a shortening.
//
// A turn with nothing left in it is not a turn: the model would be shown a
// message id and a blank, and would count it as something it was shown and
// could not use. The floor is what keeps the turn readable.
const shortenedTurnFloor = 32

// generationCall is one model call's worth of a batch.
type generationCall struct {
	// messages are the turns this call covers, in order.
	messages []model.Message
	// excerpts are the assistant's messages of this call, in order.
	excerpts []assistantExcerpt
	// caps are the token caps the excerpts may keep, one per excerpt. A cap
	// below zero is a message that was degraded to the placeholder.
	caps []int
	// degraded is how many of the assistant's messages were degraded.
	degraded int
	// shortened is how many of the learner's own turns were cut to make
	// the call fit.
	shortened int
}

// assistantExcerpt is one assistant message as the generator may show it.
type assistantExcerpt struct {
	// body is the message with the prose around its products dropped.
	body string
	// cost is what the body costs before it is cut.
	cost int
}

// nextCall takes the turns of the next call and the turns that are left.
//
// The call carries as many turns as fit: the board and the previous segment
// are paid for first, then the learner's own words, and what is left is
// shared by the assistant's excerpts. A turn and the answer to it are never
// separated - the answer belongs to the turn it answers.
//
// The estimate is made on the board and the segment this call will really
// carry, not on the ones the batch started with: the board grows as the
// batch goes on, and a call measured against a board that is not there yet
// is a call that does not fit.
func (g *ModelSegmentGenerator) nextCall(remaining []model.Message, whiteboard, previous string) (generationCall, []model.Message) {
	pairs := generationPairs(remaining)
	current := make([]model.Message, 0, len(remaining))
	for _, pair := range pairs {
		candidate := make([]model.Message, 0, len(current)+len(pair))
		candidate = append(candidate, current...)
		candidate = append(candidate, pair...)
		call, fits := g.fitCall(candidate, whiteboard, previous)
		if fits {
			current = candidate
			continue
		}
		if len(current) > 0 {
			closed, _ := g.fitCall(current, whiteboard, previous)
			return closed, remaining[len(current):]
		}
		// The pair does not fit on its own. The assistant's words are what
		// push it over, so they are the ones that go: the body becomes the
		// placeholder and the pair is generated anyway. An input that does
		// not fit is a reason to send less, never a reason to write
		// nothing.
		degraded := g.degradeCall(call)
		if rag.EstimateTokens(g.renderCall(whiteboard, previous, degraded)) > g.payloadBudget() {
			// The learner's own words do not fit either. What the learner
			// said is the one thing that cannot be given up, so it is
			// shortened to what fits and the count travels with the
			// result: a summary that was written from shortened words
			// says so.
			degraded = g.shortenCall(degraded, whiteboard, previous)
		}
		return degraded, remaining[len(pair):]
	}
	closed, _ := g.fitCall(current, whiteboard, previous)
	return closed, nil
}

// generationPairs splits the turns into the pairs a call may not break.
//
// A pair is a turn of the learner and the answer to it. The answer belongs to
// the turn it answers, and a call that carried one without the other would
// ask the model to summarize a reply to something it was never shown.
func generationPairs(messages []model.Message) [][]model.Message {
	pairs := make([][]model.Message, 0, len(messages))
	for index := 0; index < len(messages); index++ {
		message := messages[index]
		if !message.IsUser {
			// An assistant message with no turn of the learner's before it
			// stands on its own.
			pairs = append(pairs, []model.Message{message})
			continue
		}
		pair := []model.Message{message}
		if index+1 < len(messages) && !messages[index+1].IsUser {
			pair = append(pair, messages[index+1])
			index++
		}
		pairs = append(pairs, pair)
	}
	return pairs
}

// fitCall allocates the assistant excerpts of a call and reports whether the
// call's input fits the budget.
//
// The budget the excerpts share is what is left after the fixed parts of the
// input have been paid for: the board, the previous segment, the learner's
// own words and the placeholder every degraded message costs. The share of
// the input budget is the ceiling on top of that.
func (g *ModelSegmentGenerator) fitCall(messages []model.Message, whiteboard, previous string) (generationCall, bool) {
	call := generationCall{messages: messages}
	call.excerpts = assistantExcerptsOf(messages)
	// The floor is what the call costs when every assistant message is
	// degraded. Nothing can be cheaper than that, so it is what the budget
	// is measured against.
	floor := g.degradeCall(call)
	floorCost := rag.EstimateTokens(g.renderCall(whiteboard, previous, floor))
	budget := g.payloadBudget()
	share := budget * assistantExcerptShare / 5
	available := budget - floorCost
	if available < 0 {
		available = 0
	}
	if available < share {
		share = available
	}
	call.caps = allocateAssistantExcerpts(call.excerpts, share)
	return call, rag.EstimateTokens(g.renderCall(whiteboard, previous, call)) <= budget
}

// degradeCall gives up the assistant excerpts of a call.
//
// Every assistant message becomes the placeholder the mode that is off sends.
// The batch is generated anyway: what does not fit is a reason to send less.
func (g *ModelSegmentGenerator) degradeCall(call generationCall) generationCall {
	call.caps = make([]int, len(call.excerpts))
	for index := range call.caps {
		call.caps[index] = -1
	}
	call.degraded = len(call.excerpts)
	return call
}

// shortenCall cuts the learner's own words until the call fits.
//
// This is the floor under the floor. A turn that is longer than the whole
// budget cannot be split - there is nothing to split it into - and dropping
// the batch would lose the turn entirely, after the tokens were paid for.
//
// The words are shortened instead, and the count of what was shortened
// travels with the result: a summary that was written from shortened words
// says so.
func (g *ModelSegmentGenerator) shortenCall(call generationCall, whiteboard, previous string) generationCall {
	budget := g.payloadBudget()
	for attempt := 0; attempt < 4; attempt++ {
		cost := rag.EstimateTokens(g.renderCall(whiteboard, previous, call))
		if cost <= budget {
			break
		}
		overflow := cost - budget
		progressed := false
		for index := range call.messages {
			message := &call.messages[index]
			if !message.IsUser {
				continue
			}
			tokens := rag.EstimateTokens(message.Content)
			if tokens <= shortenedTurnFloor {
				continue
			}
			keep := tokens - overflow
			if keep < shortenedTurnFloor {
				keep = shortenedTurnFloor
			}
			if keep >= tokens {
				continue
			}
			message.Content = prefixWithinTokens(message.Content, keep)
			overflow -= tokens - keep
			call.shortened++
			progressed = true
			if overflow <= 0 {
				break
			}
		}
		if !progressed {
			break
		}
	}
	return call
}

// assistantExcerptsOf reads the assistant's messages of a call.
func assistantExcerptsOf(messages []model.Message) []assistantExcerpt {
	excerpts := make([]assistantExcerpt, 0, len(messages)/2)
	for _, message := range messages {
		if message.IsUser {
			continue
		}
		body := renderAssistantBody(message.Content, assistantBodyTokens)
		cost := rag.EstimateTokens(body)
		if cost > assistantBodyTokens {
			cost = assistantBodyTokens
		}
		excerpts = append(excerpts, assistantExcerpt{body: body, cost: cost})
	}
	return excerpts
}

// allocateAssistantExcerpts shares a budget among the assistant's messages.
//
// The share is proportional to what each message would take on its own, so a
// long answer keeps more of itself than a short one, and the ceiling of six
// hundred tokens a message may keep still applies on top of it.
//
// A message that cannot keep the floor is degraded to the placeholder: it is
// shown as something the model was not shown, which is the truth, rather than
// as an empty line, which is not.
func allocateAssistantExcerpts(excerpts []assistantExcerpt, budget int) []int {
	caps := make([]int, len(excerpts))
	if len(excerpts) == 0 {
		return caps
	}
	total := 0
	for _, excerpt := range excerpts {
		total += excerpt.cost
	}
	if total <= budget {
		for index, excerpt := range excerpts {
			caps[index] = excerpt.cost
		}
		return caps
	}
	assigned := 0
	for index, excerpt := range excerpts {
		share := budget * excerpt.cost / total
		if share > assistantBodyTokens {
			share = assistantBodyTokens
		}
		caps[index] = share
		assigned += share
	}
	// What the proportional shares left over goes to the messages that were
	// cut the most, so the budget is spent rather than lost.
	for index := range excerpts {
		if assigned >= budget {
			break
		}
		room := assistantBodyTokens - caps[index]
		if room <= 0 {
			continue
		}
		give := room
		if give > budget-assigned {
			give = budget - assigned
		}
		caps[index] += give
		assigned += give
	}
	for index := range caps {
		if caps[index] < assistantExcerptFloor {
			caps[index] = -1
		}
	}
	return caps
}

// renderCall builds the payload of one call.
//
// It is the payload the whole batch used to be sent as, with one difference:
// the assistant's excerpts are cut to what this call can afford.
func (g *ModelSegmentGenerator) renderCall(whiteboard, previous string, call generationCall) string {
	return renderSegmentInputWithExcerpts(whiteboard, previous, call.messages, g.payloadBudget(),
		g.assistantContent, call.caps)
}

// payloadBudget is what the payload of a request may cost.
//
// The limit is on the whole request and the system prompt is part of it, so
// the prompt is paid for first. Everything that sizes a payload - the calls
// that are cut, the excerpts that are shared out, the turns that are
// shortened - is sized against this number, so what is measured is what is
// sent.
func (g *ModelSegmentGenerator) payloadBudget() int {
	return segmentPayloadBudget(g.segmentPrompt(), g.maxInputTokens)
}

// describeCall names what a call is, for the logs.
func (call generationCall) describeCall() string {
	return fmt.Sprintf("messages=%d degraded=%d", len(call.messages), call.degraded)
}

// renderSegmentInputWithExcerpts builds the payload the model reads, with the
// assistant's excerpts cut to the caps the call may keep.
//
// The three parts are labelled and separated, because the model has to know
// which of them is authoritative for what: the board is what is true now, the
// previous segment is what has already been said, and the messages are what
// happened since.
//
// The assistant's messages are the difference between the two modes. With the
// mode off they are replaced by a placeholder, because a segment that
// recorded them would be recording something the learner never said. With the
// mode on they are shown, trimmed down to what a product is made of and to
// what the call can afford.
func renderSegmentInputWithExcerpts(whiteboard, previous string, messages []model.Message,
	maxTokens int, assistantContent bool, caps []int) string {
	var out strings.Builder
	out.WriteString("[WHITEBOARD] current effective state (empty on the first batch):\n")
	out.WriteString(prefixWithinTokens(strings.TrimSpace(whiteboard), maxTokens/3))
	out.WriteString("\n[PREVIOUS SEGMENT] the previous segment, do not repeat it (empty on the first batch):\n")
	out.WriteString(prefixWithinTokens(strings.TrimSpace(previous), maxTokens/3))
	out.WriteString("\n[MESSAGES] the turns to summarize now, in order:\n")
	assistantIndex := 0
	for _, message := range messages {
		role := "user"
		content := strings.TrimSpace(message.Content)
		if !message.IsUser {
			role = "assistant_sources_only"
			content = assistantPlaceholder
			if assistantContent {
				role = "assistant"
				cap := assistantBodyTokens
				if caps != nil {
					cap = -1
					if assistantIndex < len(caps) {
						cap = caps[assistantIndex]
					}
				}
				if cap < 0 {
					content = assistantPlaceholder
				} else {
					content = renderAssistantBody(message.Content, cap)
				}
				assistantIndex++
			}
		}
		out.WriteString(fmt.Sprintf("[message_id=%d role=%s] %s", message.ID, role, content))
		out.WriteByte('\n')
	}
	return out.String()
}
