// Package summaryconvert reads the session summaries that are stored in a shape
// this build cannot read, and turns them into the shape it can.
//
// Two old writers put other shapes into the same column this build reads: the
// ordinary conversation chain stored the model's prose answer as it came back,
// and the structured summarizer before this build's schema stored a document
// whose evidence ids are strings. Both are unreadable here, so a session with
// either one stops compacting: the rolling summary reports the stored summary
// as unusable and leaves it alone forever.
//
// Conversion is mechanical and lossless. No model is called: the prose becomes
// paragraph items of this schema's string groups, and the old structured
// document keeps every group, item, citation and its legacy_text. A row that
// cannot be carried over without dropping something is refused and reported,
// never partially written.
package summaryconvert

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"GopherAI/model"
)

// Shape names what one stored summary's content actually is.
type Shape string

const (
	// ShapeEmpty is a row with nothing in it. There is nothing to convert,
	// because a session that has never been summarised starts a fresh summary
	// on its own.
	ShapeEmpty Shape = "empty"
	// ShapeCurrent is a document of this schema. It is left alone.
	ShapeCurrent Shape = "current"
	// ShapeLegacyJSON is the structured document the summarizer wrote before
	// this schema: the same groups, but evidence ids are strings, and a
	// legacy_text carrying the prose that came before it.
	ShapeLegacyJSON Shape = "legacy_json"
	// ShapeProse is the model's plain answer, stored as it came back.
	ShapeProse Shape = "prose"
	// ShapeDamaged is text that looks like a document but is not either shape:
	// unknown fields, trailing JSON, an unsupported schema version. Nothing
	// may be inferred from it.
	ShapeDamaged Shape = "damaged"
)

// Classify says which shape one stored summary's content is, without deciding
// whether it can be converted.
//
// The real reader is the oracle: a content that this schema's own decoder
// accepts is current, whatever it looks like. Only after that fails is the
// legacy shape tried, so a document that is already readable is never rewritten.
func Classify(content string) Shape {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return ShapeEmpty
	}
	if !strings.HasPrefix(trimmed, "{") {
		return ShapeProse
	}
	if _, err := model.DecodeSessionSummaryDocument(trimmed); err == nil {
		return ShapeCurrent
	}
	if _, err := decodeLegacyDocument(trimmed); err == nil {
		return ShapeLegacyJSON
	}
	return ShapeDamaged
}

// legacyEvidence is one evidence item of the pre-schema document: same text,
// message ids as strings.
type legacyEvidence struct {
	Text               string   `json:"text"`
	EvidenceMessageIDs []string `json:"evidence_message_ids"`
}

// legacyEvidenceList accepts both shapes the old writer stored: the objects
// above, and the older plain string array whose entries carry no ids at all.
type legacyEvidenceList []legacyEvidence

func (l *legacyEvidenceList) UnmarshalJSON(data []byte) error {
	var values []json.RawMessage
	if err := json.Unmarshal(data, &values); err != nil {
		return err
	}
	items := make(legacyEvidenceList, 0, len(values))
	for _, value := range values {
		var text string
		if err := json.Unmarshal(value, &text); err == nil {
			items = append(items, legacyEvidence{Text: text})
			continue
		}
		decoder := json.NewDecoder(bytes.NewReader(value))
		decoder.DisallowUnknownFields()
		var item legacyEvidence
		if err := decoder.Decode(&item); err != nil {
			return err
		}
		if err := ensureLegacyEOF(decoder); err != nil {
			return err
		}
		items = append(items, item)
	}
	*l = items
	return nil
}

// legacyDocument is the pre-schema structured summary. The field names are the
// ones this schema uses; the differences are the string ids and legacy_text,
// which this schema has no field for.
type legacyDocument struct {
	SchemaVersion      int                `json:"schema_version,omitempty"`
	CurrentGoal        string             `json:"current_goal"`
	ConfirmedDecisions legacyEvidenceList `json:"confirmed_decisions"`
	CompletedItems     []string           `json:"completed_items"`
	OpenLoops          legacyEvidenceList `json:"open_loops"`
	UserCorrections    legacyEvidenceList `json:"user_corrections"`
	KeyErrors          []string           `json:"key_errors"`
	References         []string           `json:"references"`
	LegacyText         string             `json:"legacy_text,omitempty"`
}

func decodeLegacyDocument(content string) (legacyDocument, error) {
	var document legacyDocument
	decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(content)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return legacyDocument{}, err
	}
	if err := ensureLegacyEOF(decoder); err != nil {
		return legacyDocument{}, err
	}
	if document.SchemaVersion != 0 && document.SchemaVersion != model.SessionSummarySchemaV2 {
		return legacyDocument{}, fmt.Errorf("unsupported legacy schema version %d", document.SchemaVersion)
	}
	return document, nil
}

func ensureLegacyEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("decode trailing legacy summary: %w", err)
	}
	return errors.New("legacy summary contains trailing JSON")
}

// Conversion is one stored summary turned into this schema's document.
type Conversion struct {
	// Content is the canonical JSON of the document this build's reader
	// accepted. It is produced by decoding the converted document and encoding
	// that decoded value, so what is stored is exactly what was verified.
	Content string
}

// ConvertProse re-expresses a plain-text summary as paragraph items of this
// schema's string groups. Nothing is rewritten and nothing is inferred: every
// character of the prose lands in exactly one item, and only overlong text is
// cut, at a paragraph boundary when one fits and at the item limit otherwise.
func ConvertProse(content string, budget model.SummaryBudget) (Conversion, error) {
	if strings.TrimSpace(content) == "" {
		return Conversion{}, errors.New("the row holds no summary")
	}
	return finish(model.SessionSummaryDocument{
		SchemaVersion:  model.SessionSummarySchemaV2,
		CompletedItems: paragraphItems(content),
	}, budget)
}

// ConvertLegacyDocument re-expresses a pre-schema document as a document of
// this schema. It is mechanical: no text is rewritten and no citation is
// invented, so it spends no model call.
//
// Every field has to survive: the old structured groups keep their text and
// citations, legacy_text becomes paragraph items of completed_items, and an
// item that would not fit is cut rather than dropped. A citation this schema
// cannot express refuses the whole row instead of being quietly discarded,
// because a row that loses a citation is not the row that was read.
func ConvertLegacyDocument(content string, budget model.SummaryBudget) (Conversion, error) {
	document, err := decodeLegacyDocument(content)
	if err != nil {
		return Conversion{}, fmt.Errorf("decode legacy summary: %w", err)
	}
	if len([]rune(document.CurrentGoal)) > model.SessionSummaryItemMaxRunes {
		return Conversion{}, fmt.Errorf("the goal is longer than %d characters", model.SessionSummaryItemMaxRunes)
	}
	converted := model.SessionSummaryDocument{
		SchemaVersion: model.SessionSummarySchemaV2,
		CurrentGoal:   document.CurrentGoal,
	}
	if converted.ConfirmedDecisions, err = convertEvidence(document.ConfirmedDecisions); err != nil {
		return Conversion{}, fmt.Errorf("confirmed_decisions: %w", err)
	}
	if converted.OpenLoops, err = convertEvidence(document.OpenLoops); err != nil {
		return Conversion{}, fmt.Errorf("open_loops: %w", err)
	}
	if converted.UserCorrections, err = convertEvidence(document.UserCorrections); err != nil {
		return Conversion{}, fmt.Errorf("user_corrections: %w", err)
	}
	converted.CompletedItems = appendStringGroup(nil, document.CompletedItems)
	converted.KeyErrors = appendStringGroup(nil, document.KeyErrors)
	converted.References = appendStringGroup(nil, document.References)
	// legacy_text is the prose this structured document replaced. This schema
	// has no field for it, so it is carried into completed_items as paragraph
	// items: the facts it holds stay readable instead of being summarised as a
	// count of characters nobody can read.
	if strings.TrimSpace(document.LegacyText) != "" {
		converted.CompletedItems = appendStringGroup(converted.CompletedItems, []string{document.LegacyText})
	}
	return finish(converted, budget)
}

// finish is the verification gate for every conversion: this build's own reader
// decodes the document, the deployment's budget checks it, and the bytes that
// are returned are the canonical encoding of that decoded value. A document the
// reader refuses is never written.
func finish(document model.SessionSummaryDocument, budget model.SummaryBudget) (Conversion, error) {
	encoded, err := json.Marshal(document)
	if err != nil {
		return Conversion{}, fmt.Errorf("encode converted summary: %w", err)
	}
	read, err := model.DecodeSessionSummaryDocument(string(encoded))
	if err != nil {
		return Conversion{}, fmt.Errorf("converted summary is not readable: %w", err)
	}
	if err := read.Validate(budget); err != nil {
		return Conversion{}, fmt.Errorf("converted summary exceeds the budget: %w", err)
	}
	canonical, err := json.Marshal(read)
	if err != nil {
		return Conversion{}, fmt.Errorf("encode verified summary: %w", err)
	}
	return Conversion{Content: string(canonical)}, nil
}

// convertEvidence maps one evidence group. The text is preserved exactly and
// an overlong item is cut into consecutive items that each carry the same
// citations, so nothing is lost and nothing gains a claim the old row did not
// make.
func convertEvidence(items legacyEvidenceList) (model.SessionSummaryEvidenceList, error) {
	converted := make(model.SessionSummaryEvidenceList, 0, len(items))
	for _, item := range items {
		if strings.TrimSpace(item.Text) == "" {
			return nil, errors.New("an evidence item has no text")
		}
		ids, err := convertEvidenceIDs(item.EvidenceMessageIDs)
		if err != nil {
			return nil, err
		}
		for _, chunk := range splitLong(item.Text, model.SessionSummaryItemMaxRunes) {
			converted = append(converted, model.SessionSummaryEvidence{
				Text:               chunk,
				EvidenceMessageIDs: ids,
			})
		}
	}
	return converted, nil
}

// convertEvidenceIDs maps the old string citations onto this schema's message
// ids. A citation that does not name a message this build holds refuses the
// row: dropping it would store a summary that cites less than the row it came
// from, and the operator would only see a count.
func convertEvidenceIDs(ids []string) ([]uint, error) {
	converted := make([]uint, 0, len(ids))
	seen := make(map[uint]struct{}, len(ids))
	for _, id := range ids {
		value, ok := parseMessageID(id)
		if !ok {
			return nil, fmt.Errorf("evidence id %q cannot be expressed as a message id", id)
		}
		if _, duplicate := seen[value]; duplicate {
			return nil, fmt.Errorf("evidence id %q appears twice", id)
		}
		seen[value] = struct{}{}
		converted = append(converted, value)
	}
	if len(converted) == 0 {
		return nil, nil
	}
	return converted, nil
}

// appendStringGroup adds one old string group to a string group of this schema,
// cutting overlong items. Whether the result fits the deployment's budget is
// decided by the reader gate in finish, once, for the whole document.
func appendStringGroup(out, items []string) []string {
	for _, item := range items {
		out = append(out, splitLong(item, model.SessionSummaryItemMaxRunes)...)
	}
	return out
}

// paragraphItems turns prose into paragraph items. A paragraph ends at a blank
// line; text too long for one item is cut at its last newline that fits, or at
// the item limit when there is none. Concatenating the result returns the
// original text.
func paragraphItems(text string) []string {
	var items []string
	for _, paragraph := range strings.SplitAfter(text, "\n\n") {
		if paragraph == "" {
			continue
		}
		cuts := splitLong(paragraph, model.SessionSummaryItemMaxRunes)
		if isBlank(cuts) && len(items) > 0 {
			last := items[len(items)-1]
			if len([]rune(last))+len([]rune(cuts[0])) <= model.SessionSummaryItemMaxRunes {
				items[len(items)-1] = last + cuts[0]
				cuts = cuts[1:]
			}
		}
		items = append(items, cuts...)
	}
	return items
}

func isBlank(items []string) bool {
	return len(items) == 1 && strings.TrimSpace(items[0]) == ""
}

// splitLong cuts text into chunks of at most max runes. It prefers the last
// newline inside the window so a cut lands between lines when one fits, and
// otherwise cuts at the limit. Concatenating the chunks returns the input
// exactly; no rune is skipped, replaced or reordered.
func splitLong(text string, max int) []string {
	if text == "" {
		return nil
	}
	runes := []rune(text)
	chunks := make([]string, 0, len(runes)/max+1)
	for len(runes) > max {
		cut := max
		for index := max; index > 0; index-- {
			if runes[index-1] == '\n' {
				cut = index
				break
			}
		}
		chunks = append(chunks, string(runes[:cut]))
		runes = runes[cut:]
	}
	return append(chunks, string(runes))
}

func parseMessageID(value string) (uint, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, false
	}
	parsed, err := strconv.ParseUint(trimmed, 10, strconv.IntSize)
	if err != nil || parsed == 0 {
		return 0, false
	}
	if strconv.FormatUint(parsed, 10) != trimmed {
		return 0, false
	}
	return uint(parsed), true
}
