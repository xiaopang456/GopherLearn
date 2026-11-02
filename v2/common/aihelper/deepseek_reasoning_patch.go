package aihelper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"github.com/cloudwego/eino/schema"
)

// reasoningContentFragment is what the patch adds to an assistant turn: the
// field, and an empty value.
//
// DeepSeek rejects a thinking-mode request whose earlier assistant turn
// carries tool calls but no reasoning_content field:
//
//	"The `reasoning_content` in the thinking mode must be passed back to the API."
//
// A turn that did not think has no content to pass back, and the SDK drops
// empty strings on the way out, so the field cannot be sent through the
// message struct. It has to be added after the request has been serialized.
//
// An empty string is the honest value for a turn that did not think: the
// field has to be there, and an empty string is accepted where a missing
// field and a null are both refused.
const reasoningContentFragment = `,"reasoning_content":""`

// deepSeekReasoningPatch is the request payload modifier that adds the
// missing reasoning_content field to the turns that need one.
//
// It only touches a turn that has all three of these: it is an assistant
// turn, it carries tool calls, and it has no reasoning_content field of its
// own. A turn that thought carries its own content back and is left alone,
// whatever that content is - a string, an empty string or a null.
//
// The patch is a byte edit rather than a re-encoding. Only the turns that
// need the field change, and they change by one inserted fragment: every
// other byte of the request - the field order, the number formats, the
// whitespace - stays as the client wrote it.
//
// A body this patch cannot read is a body this patch must not guess at. The
// request goes out unchanged and the failure is logged; the request is not
// failed over a patch.
//
// The same holds for a body the patch would break: the patched bytes are
// checked before they leave, and a body that is not valid JSON any more is
// thrown away in favour of the original.
func deepSeekReasoningPatch(_ context.Context, _ []*schema.Message, rawBody []byte) ([]byte, error) {
	patched, err := patchMissingReasoningContent(rawBody)
	if err != nil {
		// The log line names the failure and nothing else. The request body
		// is the user's content and does not belong in a log.
		if errors.Is(err, errInvalidPatchedBody) {
			log.Printf("[aihelper] deepseek reasoning patch produced invalid json, sent original")
		} else {
			log.Printf("[aihelper] deepseek reasoning_content patch skipped: error_type=%T", err)
		}
		return rawBody, nil
	}
	return patched, nil
}

// errInvalidPatchedBody reports that the patched request body would not be
// valid JSON, so the patch was thrown away and the original body is the one
// that goes out.
var errInvalidPatchedBody = errors.New("the patched request body is not valid json")

// patchMissingReasoningContent adds an empty reasoning_content field to every
// assistant turn that carries tool calls and has no such field.
//
// The body is read, not decoded: the turns are located by byte range and the
// new field is inserted into the bytes as they are. Decoding the body into a
// generic map and encoding it again would change the field order and the
// number formats of every other value in the request, which is more than the
// patch is allowed to do.
//
// Whatever happens, the bytes that come back are the ones that may be sent:
// the patched body when the patch worked, and the original body when it did
// not. An error says which of the two it is, and the caller sends the
// original either way.
func patchMissingReasoningContent(body []byte) ([]byte, error) {
	return patchMissingReasoningContentWith(body, []byte(reasoningContentFragment))
}

// patchMissingReasoningContentWith is the patch with the inserted text
// spelled out.
//
// The text is a parameter so that the test can hand it text that would make
// the body invalid and check that the request still goes out unchanged. The
// production path always inserts reasoningContentFragment.
func patchMissingReasoningContentWith(body, fragment []byte) ([]byte, error) {
	members, err := topLevelMembers(body)
	if err != nil {
		return body, err
	}
	// The last messages member wins, the same way the JSON decoders in this
	// stack read a body with the key twice.
	var messages *jsonMember
	for i := range members {
		if members[i].key == "messages" {
			messages = &members[i]
		}
	}
	if messages == nil {
		// A request without messages has no turn to patch. This is not a
		// failure, so it is not logged as one.
		return body, nil
	}
	elements, err := arrayElements(body, messages.valueStart, messages.valueEnd)
	if err != nil {
		return body, err
	}

	patched := make([]byte, 0, len(body)+len(fragment)*len(elements))
	patched = append(patched, body[:messages.valueStart]...)
	copied := messages.valueStart
	for _, element := range elements {
		insertAt, needed, err := reasoningContentInsertion(body, element)
		if err != nil {
			return body, err
		}
		if !needed {
			patched = append(patched, body[copied:element.end]...)
			copied = element.end
			continue
		}
		patched = append(patched, body[copied:insertAt]...)
		patched = append(patched, fragment...)
		patched = append(patched, body[insertAt:element.end]...)
		copied = element.end
	}
	patched = append(patched, body[copied:messages.valueEnd]...)
	patched = append(patched, body[messages.valueEnd:]...)

	// The way out is checked as well as the way in. The patch edits bytes, so
	// a body it cannot read correctly could leave a document that is not JSON
	// any more. A request that is not JSON is a request the provider refuses
	// with a message nobody can act on, and the original body is the better
	// thing to send.
	if !json.Valid(patched) {
		return body, errInvalidPatchedBody
	}
	return patched, nil
}

// reasoningContentInsertion reports where the field has to be inserted into
// one turn of the messages array, and whether it has to be inserted at all.
//
// The insertion point is the closing brace of the turn, so the field becomes
// the last one and every field that was there keeps its place.
func reasoningContentInsertion(body []byte, element jsonSpan) (int, bool, error) {
	if body[element.start] != '{' {
		return 0, false, nil
	}
	members, err := objectMembers(body, element.start, element.end)
	if err != nil {
		return 0, false, err
	}
	if len(members) == 0 {
		return 0, false, nil
	}
	role := ""
	hasToolCalls := false
	hasReasoningContent := false
	for _, member := range members {
		switch member.key {
		case "role":
			role, err = decodeJSONString(body[member.valueStart:member.valueEnd])
			if err != nil {
				return 0, false, err
			}
		case "tool_calls":
			// A turn that asks for tools is the turn DeepSeek wants the
			// field on. A null value is not a set of tool calls.
			hasToolCalls = !bytes.Equal(bytes.TrimSpace(body[member.valueStart:member.valueEnd]), []byte("null"))
		case "reasoning_content":
			// The turn already says what it thought. Whatever the value is,
			// it is the answer and this patch has no opinion about it.
			hasReasoningContent = true
		}
	}
	if role != "assistant" || !hasToolCalls || hasReasoningContent {
		return 0, false, nil
	}
	closing := element.end - 1
	if body[closing] != '}' {
		return 0, false, fmt.Errorf("assistant turn does not end with a closing brace")
	}
	return closing, true, nil
}

// jsonMember is one member of a JSON object: its key and the byte range of
// its value.
type jsonMember struct {
	key        string
	valueStart int
	valueEnd   int
}

// jsonSpan is a byte range inside a JSON document.
type jsonSpan struct {
	start int
	end   int
}

// topLevelMembers lists the members of the object the request body is.
func topLevelMembers(body []byte) ([]jsonMember, error) {
	end, err := skipJSONValue(body, 0, len(body))
	if err != nil {
		return nil, err
	}
	if skipJSONSpace(body, end, len(body)) != len(body) {
		return nil, fmt.Errorf("the request body has bytes after the JSON document")
	}
	return objectMembers(body, 0, end)
}

// objectMembers lists the members of the JSON object that starts at start
// and ends before end.
func objectMembers(body []byte, start, end int) ([]jsonMember, error) {
	if start >= end || body[start] != '{' {
		return nil, fmt.Errorf("expected a JSON object")
	}
	var members []jsonMember
	i := start + 1
	for {
		i = skipJSONSpace(body, i, end)
		if i >= end {
			return nil, fmt.Errorf("unterminated JSON object")
		}
		if body[i] == '}' {
			return members, nil
		}
		if len(members) > 0 {
			if body[i] != ',' {
				return nil, fmt.Errorf("expected a comma between the members of a JSON object")
			}
			i = skipJSONSpace(body, i+1, end)
		}
		keyEnd, err := skipJSONString(body, i, end)
		if err != nil {
			return nil, err
		}
		key, err := decodeJSONString(body[i:keyEnd])
		if err != nil {
			return nil, err
		}
		i = skipJSONSpace(body, keyEnd, end)
		if i >= end || body[i] != ':' {
			return nil, fmt.Errorf("expected a colon after the key of a JSON object")
		}
		i = skipJSONSpace(body, i+1, end)
		valueEnd, err := skipJSONValue(body, i, end)
		if err != nil {
			return nil, err
		}
		members = append(members, jsonMember{key: key, valueStart: i, valueEnd: valueEnd})
		i = valueEnd
	}
}

// arrayElements lists the elements of the JSON array that starts at start
// and ends before end.
func arrayElements(body []byte, start, end int) ([]jsonSpan, error) {
	if start >= end || body[start] != '[' {
		return nil, fmt.Errorf("expected a JSON array")
	}
	var elements []jsonSpan
	i := start + 1
	for {
		i = skipJSONSpace(body, i, end)
		if i >= end {
			return nil, fmt.Errorf("unterminated JSON array")
		}
		if body[i] == ']' {
			return elements, nil
		}
		if len(elements) > 0 {
			if body[i] != ',' {
				return nil, fmt.Errorf("expected a comma between the elements of a JSON array")
			}
			i = skipJSONSpace(body, i+1, end)
		}
		valueEnd, err := skipJSONValue(body, i, end)
		if err != nil {
			return nil, err
		}
		elements = append(elements, jsonSpan{start: i, end: valueEnd})
		i = valueEnd
	}
}

// skipJSONValue returns the index just past the JSON value that starts at i.
func skipJSONValue(body []byte, i, end int) (int, error) {
	if i >= end {
		return 0, fmt.Errorf("the request body ends inside a JSON value")
	}
	switch body[i] {
	case '{':
		j := i + 1
		for {
			j = skipJSONSpace(body, j, end)
			if j >= end {
				return 0, fmt.Errorf("unterminated JSON object")
			}
			if body[j] == '}' {
				return j + 1, nil
			}
			if body[j] == ',' {
				j++
				continue
			}
			keyEnd, err := skipJSONString(body, j, end)
			if err != nil {
				return 0, err
			}
			j = skipJSONSpace(body, keyEnd, end)
			if j >= end || body[j] != ':' {
				return 0, fmt.Errorf("expected a colon after the key of a JSON object")
			}
			j = skipJSONSpace(body, j+1, end)
			valueEnd, err := skipJSONValue(body, j, end)
			if err != nil {
				return 0, err
			}
			j = valueEnd
		}
	case '[':
		j := i + 1
		for {
			j = skipJSONSpace(body, j, end)
			if j >= end {
				return 0, fmt.Errorf("unterminated JSON array")
			}
			if body[j] == ']' {
				return j + 1, nil
			}
			if body[j] == ',' {
				j++
				continue
			}
			valueEnd, err := skipJSONValue(body, j, end)
			if err != nil {
				return 0, err
			}
			j = valueEnd
		}
	case '"':
		return skipJSONString(body, i, end)
	default:
		j := i
		for j < end {
			c := body[j]
			if c == ',' || c == '}' || c == ']' || c == ' ' || c == '\t' || c == '\n' || c == '\r' {
				break
			}
			j++
		}
		if j == i {
			return 0, fmt.Errorf("empty JSON value")
		}
		return j, nil
	}
}

// skipJSONString returns the index just past the JSON string that starts at
// i.
func skipJSONString(body []byte, i, end int) (int, error) {
	if i >= end || body[i] != '"' {
		return 0, fmt.Errorf("expected a JSON string")
	}
	for j := i + 1; j < end; j++ {
		switch body[j] {
		case '\\':
			j++
		case '"':
			return j + 1, nil
		}
	}
	return 0, fmt.Errorf("unterminated JSON string")
}

// skipJSONSpace returns the index of the first byte at or after i that is not
// JSON whitespace.
func skipJSONSpace(body []byte, i, end int) int {
	for i < end {
		switch body[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

// decodeJSONString reads one JSON string value.
func decodeJSONString(raw []byte) (string, error) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", err
	}
	return value, nil
}
