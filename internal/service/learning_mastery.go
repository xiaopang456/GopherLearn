package service

import (
	"context"
	"strings"

	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/responder"
)

// The mastery note is how a stored projection reaches a live turn, and the shape of it
// is the point: prose, no numbers, one topic, and an instruction not to repeat itself.
//
// A learner asking about Redis replication should get an explanation pitched at what
// they already demonstrated, and they should not get "you scored 77 on 3 attempts". A
// number shown through a model becomes a number the model will quote back, and a
// three-attempt average is not a fact about a person worth quoting. So the projection
// is translated into bands here, at the only place that reads it, and the digits stay in
// the record its owner can look at directly.
//
// Two gates decide whether a note exists at all, and both must pass: the question has to
// name the topic (learning_topic_match.go), and the topic's answers have to be recent
// enough to describe the person asking now. Recency alone never selects a note.
const (
	// masteryNoteWeakPointLimit and masteryNoteWeakPointRunes bound how much of the
	// record reaches a prompt. The agent needs the shape of the gap, not an inventory.
	masteryNoteWeakPointLimit = 3
	masteryNoteWeakPointRunes = 60
)

// MasteryReader is the collaborator MessageService consults for a learning-agent turn.
type MasteryReader interface {
	// MasteryNote returns prose for the topic this question is about, or an empty
	// string when there is nothing reliable, recent or on-subject to say. An empty
	// note is the ordinary case, not a failure.
	MasteryNote(ctx context.Context, userID, question string) (string, error)
}

// MasteryNote describes the topic this question is asking about, for the agent that will
// explain the next thing to the learner.
//
// The question is a required argument and there is no fallback that ignores it. An
// earlier version took whichever topic had been drilled most recently, which reads well
// in a demo and is a false statement in production: the server telling an agent about
// somebody's Redis gaps while they are asking about Kafka. Now a topic can only be named
// if the question names it too, and "nothing matched" is answered by silence rather than
// by the next-best guess.
//
// Among the topics that do match, the one whose name the question reproduces most
// completely wins; equal coverage falls back to the store's most-recent-first order, so
// the choice is a function of the question and the record and nothing else.
func (s *LearningService) MasteryNote(ctx context.Context, userID, question string) (string, error) {
	if s.store == nil {
		return "", ErrInternal
	}
	if strings.TrimSpace(userID) == "" {
		return "", ErrUnauthorized
	}
	matcher := newLearningTopicMatcher(question)
	// A question with nothing in it cannot name a topic, so the read is skipped rather
	// than performed and answered empty. This is not a shortcut around the gate: the
	// gate still decides every case that reaches it.
	if matcher.normalized == "" {
		return "", nil
	}
	recent, err := s.store.ListTopicProgress(ctx, userID, domain.LearningMasteryCandidateTopics)
	if err != nil {
		return "", mapLearningError(err)
	}
	now := s.now()
	var (
		best      string
		bestScore float64
		// found is what separates "no topic has been considered yet" from "the best one
		// scored zero". Ranking on `score <= bestScore` alone would reject a zero score,
		// which is the same outcome the floor is supposed to decide - and a gate that was
		// removed would look identical to one that was never passed.
		found bool
	)
	for _, progress := range recent {
		score, names := matcher.match(progress.TopicKey)
		if !names {
			continue
		}
		if now.Sub(progress.LastActive()) > domain.LearningMasteryStaleAfter {
			continue
		}
		// Strictly greater, so the first candidate wins a tie and the listing's own
		// recency order breaks it. >= would hand the note to the last match instead,
		// which is a different answer for the same input depending on nothing but scan
		// direction.
		if found && score <= bestScore {
			continue
		}
		note := renderMasteryNote(progress)
		if note == "" {
			continue
		}
		best, bestScore, found = note, score, true
	}
	return best, nil
}

// renderMasteryNote turns a projection into the prose an agent reads. It returns an
// empty string for anything that should not be said: a topic with too few valid attempts
// is not a description of a learner, and a level this code does not know is not a
// statement to make.
func renderMasteryNote(progress domain.TopicProgress) string {
	if !progress.IsReliable() {
		return ""
	}
	topic := strings.TrimSpace(progress.Topic)
	if topic == "" {
		topic = progress.TopicKey
	}
	var body string
	switch progress.Level {
	case domain.TopicProgressMastered:
		// Recent answers hold up, so the useful move is to go deeper rather than to
		// repeat the introduction.
		body = "在《" + topic + "》这个主题上，学习者最近的回答稳定成立，能说清主要机制和它们之间的取舍。" +
			"可以直接进入边界情形、与相邻机制的对比和迁移应用，不必再复述基础结论。"
	case domain.TopicProgressDeveloping:
		body = "在《" + topic + "》这个主题上，学习者最近的回答方向正确但还不完整。" +
			"讲解时先补上下面还缺的要点，再引到相邻机制：" + masteryGapPhrase(progress.WeakPoints) + "。"
	default:
		return ""
	}
	return body + masteryNoteCaution
}

// masteryGapPhrase selects, bounds and joins the recorded gaps. The cap is here rather
// than left to the caller because this text goes into a prompt, where a list of eight
// items stops being a direction and becomes noise.
func masteryGapPhrase(weakPoints []string) string {
	phrases := make([]string, 0, masteryNoteWeakPointLimit)
	for _, point := range weakPoints {
		trimmed := strings.TrimSpace(point)
		if trimmed == "" {
			continue
		}
		phrases = append(phrases, clipLearningRunes(trimmed, masteryNoteWeakPointRunes))
		if len(phrases) == masteryNoteWeakPointLimit {
			break
		}
	}
	if len(phrases) == 0 {
		return "还说不清的地方"
	}
	return strings.Join(phrases, "；")
}

// masteryNoteCaution is what stops a server-side judgement from becoming conversation
// content. It travels with every note because the note is the only place the learner's
// record is exposed to a model, and a model repeats what it was given.
const masteryNoteCaution = "这段判断只用于安排讲解的深浅。不要向学习者复述它，不要评价学习者，" +
	"也不要提到系统做过测验、评分或统计；学习者没有主动问起自己的进度时，这些都不说。"

// WithMasteryContext installs the reader a learning-agent turn consults. A nil reader is
// the ordinary configuration for a deployment without the quiz workflow: agent turns
// then carry no note at all, which is the same shape as a learner with nothing to report.
func WithMasteryContext(reader MasteryReader) MessageServiceOption {
	return func(service *MessageService) { service.mastery = reader }
}

// masteryNoteFor reads the note for one prepared turn.
//
// A read failure leaves the turn without a note rather than failing the turn. The note
// changes how deep an explanation starts; it does not change what is true, and a learner
// should not lose the ability to ask a question because a projection could not be read.
// Matching is lexical, so this consults no model: a turn whose question names no practised
// topic spends exactly zero extra requests on the feature.
func (s *MessageService) masteryNoteFor(ctx context.Context, request responder.Request,
	strategy domain.AnswerStrategy) string {
	if s.mastery == nil || strategy != domain.AnswerStrategyAgent {
		return ""
	}
	note, err := s.mastery.MasteryNote(ctx, request.UserID, request.Content)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(note)
}
