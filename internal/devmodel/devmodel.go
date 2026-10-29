// Package devmodel holds chat models that are not a provider.
//
// Everything here answers from a script: it opens no connection, reads no credential,
// and spends nothing. That makes it safe to run a real feature against locally, which is
// the only reason it exists. It is not a simulation of a model's judgement, and a score
// or a question coming out of it says nothing about the subject being studied - it says
// the plumbing carried a request and a response without losing either.
//
// The seam is deliberately narrow. Only the quiz workflow may be built on these types,
// they implement model.ChatModel and not model.ToolChatModel, so they cannot be wired
// into an agent loop even by mistake, and nothing in this package is reachable from a
// configuration that names a real provider.
package devmodel

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/xiaopang456/GopherLearn/internal/model"
)

// LearningModelName is the only value this package serves, and the value of
// GOPHERLEARN_LOCAL_LEARNING_MODEL that turns the seam on.
const LearningModelName = "deterministic"

// ErrUnrecognizedRequest reports a prompt that is neither the generator's nor the
// grader's. It is a failure rather than a default: a request handed to the wrong script
// would produce a generator's JSON inside a grading call, and the quiz would then be
// scored against text nobody wrote as a grade. The workflow surfaces it as
// learning_generation_failed / learning_grading_failed, which is the honest answer -
// this build cannot produce that content.
var ErrUnrecognizedRequest = errors.New("devmodel: request is not a learning prompt this model knows")

// Learning is the scripted question generator and scorer behind the quiz workflow.
//
// The two counters exist so a test can say which calls a turn really made. They are not
// a budget: the workflow bounds its own retries, and a model that cannot bill anything
// has no business enforcing a ceiling on someone else's behalf.
type Learning struct {
	mutex       sync.Mutex
	generations int
	gradings    int
}

// NewLearning builds the scripted learning model. It takes no configuration because it
// contacts nothing; an option list here would only be a place for a future default to
// lie about.
func NewLearning() *Learning { return &Learning{} }

// Calls reports how many generation and grading completions this instance produced.
func (m *Learning) Calls() (generations, gradings int) {
	m.mutex.Lock()
	defer m.mutex.Unlock()
	return m.generations, m.gradings
}

// Complete answers one learning request.
func (m *Learning) Complete(ctx context.Context, request model.Request) (model.Completion, error) {
	if err := ctx.Err(); err != nil {
		return model.Completion{}, err
	}
	if err := request.Validate(); err != nil {
		return model.Completion{}, err
	}
	user, found := request.LastUserContent()
	if !found {
		return model.Completion{}, fmt.Errorf("%w: no user turn", ErrUnrecognizedRequest)
	}
	// The grader's prompt is a JSON object the workflow built, so it is recognised by
	// shape and never by a role word. Anything else that carries an evidence block is a
	// generator's prompt. The order matters only in that a grading payload never holds
	// one, so the two tests cannot both match.
	if payload, ok := parseGradingPrompt(user); ok {
		m.mutex.Lock()
		m.gradings++
		m.mutex.Unlock()
		return model.Completion{Content: renderGrade(payload), FinishReason: model.FinishStop}, nil
	}
	if input, ok := parseGenerationPrompt(user); ok {
		m.mutex.Lock()
		round := m.generations + 1
		m.generations++
		m.mutex.Unlock()
		return model.Completion{Content: renderQuestions(input, round), FinishReason: model.FinishStop}, nil
	}
	return model.Completion{}, fmt.Errorf("%w: neither an evidence block nor a grading payload", ErrUnrecognizedRequest)
}

// Stream hands the whole scripted answer over as one delta. The streaming path exists
// because model.ChatModel requires it, and the quiz workflow uses Complete - so what is
// tested here is the interface contract, not a claim that generation is streamed.
func (m *Learning) Stream(ctx context.Context, request model.Request, onDelta model.DeltaHandler) (model.Completion, error) {
	completion, err := m.Complete(ctx, request)
	if err != nil {
		return model.Completion{}, err
	}
	if completion.Content == "" {
		return model.Completion{}, model.ErrEmptyCompletion
	}
	if onDelta != nil {
		if err := onDelta(completion.Content); err != nil {
			return model.Completion{}, err
		}
	}
	return completion, nil
}
