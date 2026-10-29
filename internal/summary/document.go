package summary

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

// The structured session summary, schema version 2. It is a port of the source
// project's model.SessionSummaryDocument (model/session_summary.go), adapted to
// this project's string message ids.
//
// The schema exists for one reason: a plain-prose summary cannot point back at
// the messages it was compressed from, so nothing in it can be read back or
// audited. The v2 document keeps the prose fields - goal, decisions, completed
// work, open loops, corrections, errors, references - and adds per-item
// evidence ids naming the stored user messages each item was drawn from. Those
// ids are what the read-back authorization is built from, which is why every
// producer-side rule below refuses to let an id in that the summarizer was not
// shown.

const (
	// SchemaV2 is the only schema version this server writes and accepts.
	SchemaV2 = 2
	// ItemLimit caps each list field, so one summary cannot grow without bound
	// by splitting its content across many small items.
	ItemLimit = 20
	// ItemMaxRunes caps one item's text.
	ItemMaxRunes = 1000
	// EvidenceLimit caps how many message ids one evidence item may cite.
	EvidenceLimit = 32
	// SourceLimit caps the distinct message ids one document may cite in total.
	SourceLimit = 64
)

// Evidence is one auditable summary item: what the summarizer concluded, and
// which stored user messages it was drawn from.
//
// Empty EvidenceMessageIDs are accepted only for items preserved from a legacy
// (schema v1) summary; producers must separately prove that such an item was
// carried over unchanged. A newly invented item with no ids would be an
// unverifiable claim wearing the schema's clothes, and decodeSummaryDocument
// refuses it.
type Evidence struct {
	Text               string   `json:"text"`
	EvidenceMessageIDs []string `json:"evidence_message_ids"`
}

type EvidenceList []Evidence

// UnmarshalJSON accepts both the legacy string-array form already stored in
// older summaries and the v2 evidence-object form. MarshalJSON always emits
// the v2 object representation, so one round trip migrates a record's shape
// without changing its content.
func (l *EvidenceList) UnmarshalJSON(data []byte) error {
	var values []json.RawMessage
	if err := json.Unmarshal(data, &values); err != nil {
		return err
	}
	items := make(EvidenceList, 0, len(values))
	for _, value := range values {
		var legacy string
		if err := json.Unmarshal(value, &legacy); err == nil {
			items = append(items, Evidence{Text: legacy})
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(value))
		decoder.DisallowUnknownFields()
		var item Evidence
		if err := decoder.Decode(&item); err != nil {
			return err
		}
		if err := ensureDocumentJSONEOF(decoder); err != nil {
			return err
		}
		items = append(items, item)
	}
	*l = items
	return nil
}

// Document is one structured session summary.
type Document struct {
	SchemaVersion      int          `json:"schema_version,omitempty"`
	CurrentGoal        string       `json:"current_goal"`
	ConfirmedDecisions EvidenceList `json:"confirmed_decisions"`
	CompletedItems     []string     `json:"completed_items"`
	OpenLoops          EvidenceList `json:"open_loops"`
	UserCorrections    EvidenceList `json:"user_corrections"`
	KeyErrors          []string     `json:"key_errors"`
	References         []string     `json:"references"`
	// LegacyText is the exact pre-schema plain-text summary retained during its first structured upgrade. It has no evidence IDs and never grants read-back.
	LegacyText string `json:"legacy_text,omitempty"`
}

// DecodeDocument parses one stored summary as a strict v2 document. Unknown
// fields, trailing JSON and unsupported schema versions are refused: a
// document this server cannot fully understand must not be partially trusted.
func DecodeDocument(content string) (Document, error) {
	content = strings.TrimSpace(content)
	if !strings.HasPrefix(content, "{") {
		return Document{}, fmt.Errorf("session summary must be a JSON object")
	}
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	var document Document
	if err := decoder.Decode(&document); err != nil {
		return Document{}, fmt.Errorf("decode session summary: %w", err)
	}
	if err := ensureDocumentJSONEOF(decoder); err != nil {
		return Document{}, err
	}
	if document.SchemaVersion != 0 && document.SchemaVersion != SchemaV2 {
		return Document{}, fmt.Errorf("unsupported session summary schema version %d", document.SchemaVersion)
	}
	if err := document.Validate(); err != nil {
		return Document{}, err
	}
	return document, nil
}

// ParseStored reads one stored summary row for display and read-back
// authorization, accepting both record shapes the store may hold.
//
// A structured v2 document decodes normally. Anything else - the plain-prose
// summaries this project wrote before the schema existed, or a structured
// record damaged after storage - is reported as legacy with the original text
// intact, so existing records keep working exactly as they did and no
// deployment has to clear or migrate its summary table to upgrade. A damaged
// JSON record loses its evidence ids (it grants nothing) but never its text.
func ParseStored(content string) (Document, bool) {
	content = strings.TrimSpace(content)
	if !strings.HasPrefix(content, "{") {
		return Document{}, false
	}
	document, err := DecodeDocument(content)
	if err != nil {
		return Document{}, false
	}
	return document, true
}

func ensureDocumentJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("decode trailing session summary: %w", err)
	}
	return fmt.Errorf("session summary contains trailing JSON")
}

// Validate enforces the shape limits. They are the same limits the producer
// enforces on its own output; checking them again at decode time means a
// record edited by hand cannot smuggle an oversized or malformed item past the
// read-back authorization.
func (d *Document) Validate() error {
	if d == nil || len([]rune(strings.TrimSpace(d.CurrentGoal))) > ItemMaxRunes {
		return fmt.Errorf("invalid session summary goal")
	}
	for _, group := range []EvidenceList{d.ConfirmedDecisions, d.OpenLoops, d.UserCorrections} {
		if len(group) > ItemLimit {
			return fmt.Errorf("session summary has too many evidence items")
		}
		for _, item := range group {
			if err := item.Validate(); err != nil {
				return err
			}
		}
	}
	for _, group := range [][]string{d.CompletedItems, d.KeyErrors, d.References} {
		if len(group) > ItemLimit {
			return fmt.Errorf("session summary has too many items")
		}
		for _, item := range group {
			if len([]rune(strings.TrimSpace(item))) > ItemMaxRunes {
				return fmt.Errorf("session summary item is too long")
			}
		}
	}
	if len([]rune(strings.TrimSpace(d.LegacyText))) > ItemMaxRunes*ItemLimit {
		return fmt.Errorf("legacy session summary is too long")
	}
	if len(d.EvidenceMessageIDs()) > SourceLimit {
		return fmt.Errorf("session summary has too many evidence messages")
	}
	return nil
}

// Validate checks one evidence item's shape. An id must be present, positive
// and unique inside the item: a duplicate or zero id would make "which message
// backed this" ambiguous exactly where the answer has to be exact.
func (e Evidence) Validate() error {
	if strings.TrimSpace(e.Text) == "" || len([]rune(strings.TrimSpace(e.Text))) > ItemMaxRunes || len(e.EvidenceMessageIDs) > EvidenceLimit {
		return fmt.Errorf("invalid session summary evidence")
	}
	seen := make(map[string]struct{}, len(e.EvidenceMessageIDs))
	for _, id := range e.EvidenceMessageIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			return fmt.Errorf("session summary evidence id must not be empty")
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("duplicate session summary evidence id %s", id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

// EvidenceMessageIDs returns every distinct message id the document cites,
// sorted. This is the set read-back authorization is built from.
func (d Document) EvidenceMessageIDs() []string {
	seen := make(map[string]struct{})
	for _, group := range []EvidenceList{d.ConfirmedDecisions, d.OpenLoops, d.UserCorrections} {
		for _, item := range group {
			for _, id := range item.EvidenceMessageIDs {
				trimmed := strings.TrimSpace(id)
				if trimmed == "" {
					continue
				}
				seen[trimmed] = struct{}{}
			}
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// HasEvidence reports whether the document cites any message at all. A
// document without ids is a valid summary; it simply authorizes no read-back.
func (d Document) HasEvidence() bool {
	return len(d.EvidenceMessageIDs()) > 0
}

// summaryEvidenceKey is how an evidence id array appears in the rendered
// summary block. The visibility scanner looks for exactly this key, so a
// number that happens to appear in goal prose cannot authorize a read.
const summaryEvidenceKey = `"evidence_message_ids"`

// VisibleEvidenceIDs keeps the cited ids whose number token is complete in the
// rendered block. It is a port of the source project's
// visibleSummaryEvidenceIDs (service/agent/context_builder.go), adapted to
// string ids.
//
// The rendered text is what this turn actually sends, not the stored row: a
// long summary is cut to the window budget, and a cut document still decodes
// from the store while its tail is absent from the prompt. Granting an id the
// budget trimmed away would let the model read back a citation it was never
// shown. The scanner reads only the arrays that follow an
// "evidence_message_ids" key; a cut anywhere - before the key finishes, between
// the key and its array, after the bracket, or mid-number - yields fewer ids
// and never a wrong one. The intersection with the document's own cited ids
// keeps this a pure tightening: nothing an untruncated summary could not
// already grant.
func VisibleEvidenceIDs(rendered string, cited []string) []string {
	visible := make(map[string]struct{}, len(cited))
	for at := 0; at < len(rendered); {
		offset := strings.Index(rendered[at:], summaryEvidenceKey)
		if offset < 0 {
			break
		}
		cursor := at + offset + len(summaryEvidenceKey)
		for _, id := range jsonIDTokens(rendered, cursor) {
			visible[id] = struct{}{}
		}
		at = cursor
	}
	if len(visible) == 0 {
		return nil
	}
	ids := make([]string, 0, len(cited))
	for _, id := range cited {
		if _, ok := visible[id]; ok {
			ids = append(ids, id)
		}
	}
	return ids
}

// jsonIDTokens reads the complete string ids of the array that starts after the
// cursor, which must be `:` then `[`. It stops at the first character that
// cannot belong to such an array, which is also what a mid-array truncation
// looks like.
func jsonIDTokens(text string, cursor int) []string {
	cursor = skipJSONSpace(text, cursor)
	if cursor >= len(text) || text[cursor] != ':' {
		return nil
	}
	cursor = skipJSONSpace(text, cursor+1)
	if cursor >= len(text) || text[cursor] != '[' {
		return nil
	}
	cursor = skipJSONSpace(text, cursor+1)
	ids := make([]string, 0, 8)
	for cursor < len(text) {
		switch text[cursor] {
		case '"':
			end := cursor + 1
			for end < len(text) && text[end] != '"' {
				if text[end] == '\\' {
					end++
				}
				end++
			}
			if end >= len(text) {
				// Unterminated string: the block was cut mid-id.
				return ids
			}
			var value string
			if err := json.Unmarshal([]byte(text[cursor:end+1]), &value); err == nil && strings.TrimSpace(value) != "" {
				ids = append(ids, strings.TrimSpace(value))
			}
			cursor = skipJSONSpace(text, end+1)
		case ',':
			cursor = skipJSONSpace(text, cursor+1)
		case ']':
			return ids
		default:
			// Anything else cannot belong to a JSON string array: treat it as
			// the end, the same way a truncated number ends v2's scanner.
			return ids
		}
	}
	return ids
}

func skipJSONSpace(text string, cursor int) int {
	for cursor < len(text) {
		switch text[cursor] {
		case ' ', '\t', '\n', '\r':
			cursor++
		default:
			return cursor
		}
	}
	return cursor
}
