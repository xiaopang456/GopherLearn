package episode

// The receive step is in ONE place.
//
// The generator and the exported seam used to be two answers to "is this card
// storable?", and they had already drifted: the seam skipped the evidence
// check and the topic normalisation, so the card it produced was not the card
// the product stores.
//
// These cases pin the two entrances to the same document, for the inputs
// where they used to differ: a topic whose key and label disagree, and two
// topics whose keys normalise to the same thing.
//
// The materials here are ANONYMOUS AND SYNTHETIC.

import (
	"context"
	"errors"
	"testing"
	"time"

	"GopherAI/model"
	"github.com/cloudwego/eino/schema"
)

// aCardWithDisagreeingTopics has a topic whose key and label say different
// things.
//
// The server derives the key from the label, so the card it stores is not the
// card the model wrote. An entrance that skipped that derivation stored a
// different document.
func aCardWithDisagreeingTopics() string {
	return `{
		"version":1,
		"title":"题量",
		"synopsis":"讨论了每轮题量。",
		"topics":[{"key":"error-sentinel","label":"哨兵值与错误分类"}],
		"arrangements":[{"text":"每轮9道题","status":"stated","message_ids":[1]}],
		"assistant_products":[],"open_items":[]}`
}

// aCardWithTwoTopicsThatNormaliseToTheSameKey has two topics whose keys
// normalise to one.
//
// The server keeps one of them. An entrance that skipped the derivation kept
// both - and two topics with the same key are a card the server refuses.
func aCardWithTwoTopicsThatNormaliseToTheSameKey() string {
	return `{
		"version":1,
		"title":"题量",
		"synopsis":"讨论了每轮题量。",
		"topics":[{"key":"Concurrency","label":"并发"},{"key":"concurrency","label":"并发"}],
		"arrangements":[{"text":"每轮9道题","status":"stated","message_ids":[1]}],
		"assistant_products":[],"open_items":[]}`
}

// The generator and the seam produce the SAME card.
//
// The generator is the production path and the seam is what the read side
// replays with, so a difference between them is a difference between what is
// stored and what is measured.
func TestTheGeneratorAndTheSeamProduceTheSameCard(t *testing.T) {
	now := time.Now()
	evidence := cardEvidenceFor(
		model.Message{ID: 1, SessionID: "session-card", IsUser: true, Content: "题量", CreatedAt: now},
	)
	for name, arguments := range map[string]string{
		"disagreeing topics":  aCardWithDisagreeingTopics(),
		"same normalised key": aCardWithTwoTopicsThatNormaliseToTheSameKey(),
		"plain":               `{"version":1,"title":"题量","synopsis":"讨论了每轮题量。","topics":[],"arrangements":[{"text":"每轮9道题","status":"stated","message_ids":[1]}],"assistant_products":[],"open_items":[]}`,
	} {
		chat := &cardChat{responses: []*schema.Message{cardToolResponse(arguments)}}
		generator, err := NewModelCardGenerator(chat, 4096, 1200)
		if err != nil {
			t.Fatal(err)
		}
		generated, genErr := generator.Generate(context.Background(), evidence)
		received, recErr := ReceiveCardDocument(arguments, evidence)
		if (genErr == nil) != (recErr == nil) {
			t.Fatalf("%s: the generator says %v and the seam says %v", name, genErr, recErr)
		}
		if genErr != nil {
			continue
		}
		if len(generated.Topics) != len(received.Topics) {
			t.Fatalf("%s: the generator kept %d topics and the seam kept %d: %+v vs %+v",
				name, len(generated.Topics), len(received.Topics), generated.Topics, received.Topics)
		}
		for index := range generated.Topics {
			if generated.Topics[index] != received.Topics[index] {
				t.Fatalf("%s: topic %d is %+v in the generator and %+v in the seam",
					name, index, generated.Topics[index], received.Topics[index])
			}
		}
		if generated.Synopsis != received.Synopsis || generated.Title != received.Title {
			t.Fatalf("%s: the descriptions differ: %q/%q vs %q/%q",
				name, generated.Title, generated.Synopsis, received.Title, received.Synopsis)
		}
	}
}

// The receive happens in ONE place.
//
// Two places that each answer "is this card storable?" is how the write side
// and the read side drift apart. The generator calls the seam's inner
// function, and the seam calls it too - and the case above proves the two
// entrances agree, which is the drift that matters.
func TestTheGeneratorAndTheSeamRefuseTheSameCard(t *testing.T) {
	now := time.Now()
	evidence := cardEvidenceFor(
		model.Message{ID: 1, SessionID: "session-card", IsUser: true, Content: "题量", CreatedAt: now},
	)
	// A card that cites a message it was never shown.
	forged := `{"version":1,"title":"题量","synopsis":"讨论了每轮题量。","topics":[],
		"arrangements":[{"text":"每轮9道题","status":"stated","message_ids":[99]}],
		"assistant_products":[],"open_items":[]}`
	chat := &cardChat{responses: []*schema.Message{cardToolResponse(forged)}}
	generator, err := NewModelCardGenerator(chat, 4096, 1200)
	if err != nil {
		t.Fatal(err)
	}
	_, genErr := generator.Generate(context.Background(), evidence)
	_, recErr := ReceiveCardDocument(forged, evidence)
	if genErr == nil || recErr == nil {
		t.Fatalf("a forged citation was accepted: the generator says %v and the seam says %v", genErr, recErr)
	}
	if !errors.Is(recErr, ErrCardSourceBoundary) {
		t.Fatalf("the refusal is %v, want the source boundary", recErr)
	}
}
