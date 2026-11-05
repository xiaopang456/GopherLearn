package config

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// A startup entry prints the reason only for refusals that name a key and quote no
// value. These hold both halves: a marked refusal survives being wrapped and is
// still readable as its own sentence, and a failure whose text comes out of the
// decoder is not offered to the terminal at all.

func TestRefusalKeepsItsSentenceThroughWrapping(t *testing.T) {
	refused := Refusef("explicit independent setting required: %s", "ragModelConfig.embeddingApiKey")
	wrapped := fmt.Errorf("load configuration stage 2: %w", refused)
	if got := SafeRefusalMessage(wrapped); got != refused.Error() {
		t.Fatalf("wrapping lost the sentence: %q", got)
	}
	var refusal *Refusal
	if !errors.As(wrapped, &refusal) {
		t.Fatal("the wrapped error no longer reads as a Refusal")
	}
	if sentence := refused.Error(); !strings.HasPrefix(sentence, "explicit independent setting required") {
		t.Fatalf("sentence changed shape: %q", sentence)
	}
}

// The generic line is what an operator gets for anything not marked, including a
// real decode failure. That is not shyness about errors: the decoder quotes the
// token it choked on, which is content from the operator's file.
func TestDecodeFailureIsNotOfferedAsASafeRefusal(t *testing.T) {
	path := write(t, "[isolatedEntry]\nisolated = true\nSENTINEL_TOKEN_THAT_MUST_STAY_UNPRINTED 3\n")
	_, err := LoadExplicit(path)
	if err == nil {
		t.Fatal("a malformed file loaded")
	}
	if got := SafeRefusalMessage(err); got != "" {
		t.Fatalf("an unmarked failure was offered to the terminal: %q", got)
	}
	// What the decoder actually said is recorded here because it is the reason
	// this class stays generic. Measured on 2026-11-05 against the pinned
	// BurntSushi/toml: the sentence names the key path and echoes a character of
	// the token it choked on - "expected key separator '=', but got 'S' instead".
	// A fragment of a secret is still a fragment of a secret, so nothing of this
	// message reaches an operator.
	if text := err.Error(); text == "" || !strings.Contains(text, "line") {
		t.Fatalf("expected a decoder message naming where it failed: %q", text)
	}
}

func TestRetiredSettingsAreMarkedSoTheEntryCanPrintThem(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"base url", currentRagSection + "baseUrl = \"https://chat.example/v1\"\n", "ragModelConfig.baseUrl"},
		{"embedding key", currentRagSection + "[isolatedEntry]\nisolated = true\nembeddingKey = \"synthetic-key-never-sent\"\n", "isolatedEntry.embeddingKey"},
	} {
		_, err := LoadExplicit(write(t, tc.body))
		if err == nil {
			t.Fatalf("%s: the retired key loaded", tc.name)
		}
		message := SafeRefusalMessage(err)
		if message == "" {
			t.Fatalf("%s: the refusal is not marked safe, so the entry will hide it", tc.name)
		}
		if !strings.Contains(message, "retired configuration setting") || !strings.Contains(message, tc.want) {
			t.Fatalf("%s: sentence lost its content: %q", tc.name, message)
		}
		for _, marker := range []string{"synthetic-key-never-sent", "https://chat.example/v1"} {
			if strings.Contains(message, marker) {
				t.Fatalf("%s: the printed sentence quotes a value (%s): %q", tc.name, marker, message)
			}
		}
	}
}

func TestSafeRefusalMessageAnswersNothingForPlainErrors(t *testing.T) {
	for _, err := range []error{nil, errors.New("redis: connection refused"), fmt.Errorf("wrapped: %w", errors.New("boom"))} {
		if got := SafeRefusalMessage(err); got != "" {
			t.Fatalf("a plain error was reported as a safe refusal: %q", got)
		}
	}
}
