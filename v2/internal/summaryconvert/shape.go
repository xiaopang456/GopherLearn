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
// Classification and conversion are separated on purpose. Classification is
// total - every stored row gets a shape, including one that means "I cannot
// read this". Conversion is partial: only the shapes that can be re-expressed
// without inventing anything are converted, and everything else is reported.
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
	// ShapeProse is the model's plain answer, stored as it came back. Its
	// groups cannot be recovered from the text alone.
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
	// Content is the canonical document to store, in the shape this build's
	// reader accepts.
	Content string
	// ModelCalls counts the calls this conversion needed. A mechanical
	// conversion needs none, and this is the only conversion this package
	// performs, so it is always zero here.
	ModelCalls int
	// DroppedEvidenceIDs names the citations the legacy document carried that
	// this schema cannot hold. A citation this schema cannot express is not
	// quietly thrown away: it is listed, and the item that carried it keeps
	// its text with no ids, which grants no read-back.
	DroppedEvidenceIDs []string
	// LegacyTextRunes counts the characters of legacy_text this schema has no
	// field for. The structured groups survive; the prose copy of the summary
	// from before them does not, and the report says so.
	LegacyTextRunes int
}

// ConvertLegacyDocument re-expresses a pre-schema document as a document of
// this schema. It is mechanical: no text is rewritten and no citation is
// invented, so it spends no model call.
//
// The document this produces is checked by this schema's own reader before it
// is returned. A legacy document the reader will not accept is an error here
// rather than a row that gets written and then skipped forever.
func ConvertLegacyDocument(content string, budget model.SummaryBudget) (Conversion, error) {
	document, err := decodeLegacyDocument(content)
	if err != nil {
		return Conversion{}, fmt.Errorf("decode legacy summary: %w", err)
	}
	converted := model.SessionSummaryDocument{
		SchemaVersion:      model.SessionSummarySchemaV2,
		CurrentGoal:        strings.TrimSpace(document.CurrentGoal),
		ConfirmedDecisions: convertEvidenceGroup(document.ConfirmedDecisions),
		CompletedItems:     convertStrings(document.CompletedItems),
		OpenLoops:          convertEvidenceGroup(document.OpenLoops),
		UserCorrections:    convertEvidenceGroup(document.UserCorrections),
		KeyErrors:          convertStrings(document.KeyErrors),
		References:         convertStrings(document.References),
	}
	// An item whose citations cannot be expressed keeps its text with an empty
	// id array: that is this schema's own shape for a legacy item, and it
	// authorizes nothing on its own.
	dropped := droppedEvidenceIDs(document)
	canonical, err := json.Marshal(converted)
	if err != nil {
		return Conversion{}, fmt.Errorf("encode converted summary: %w", err)
	}
	read, err := model.DecodeSessionSummaryDocument(string(canonical))
	if err != nil {
		return Conversion{}, fmt.Errorf("converted summary is not readable: %w", err)
	}
	if err := read.Validate(budget); err != nil {
		return Conversion{}, fmt.Errorf("converted summary exceeds the budget: %w", err)
	}
	return Conversion{
		Content:            string(canonical),
		DroppedEvidenceIDs: dropped,
		LegacyTextRunes:    len([]rune(strings.TrimSpace(document.LegacyText))),
	}, nil
}

func convertEvidenceGroup(items legacyEvidenceList) model.SessionSummaryEvidenceList {
	converted := make(model.SessionSummaryEvidenceList, 0, len(items))
	for _, item := range items {
		converted = append(converted, model.SessionSummaryEvidence{
			Text:               strings.TrimSpace(item.Text),
			EvidenceMessageIDs: convertEvidenceIDs(item.EvidenceMessageIDs),
		})
	}
	return converted
}

func convertStrings(items []string) []string {
	converted := make([]string, 0, len(items))
	for _, item := range items {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			converted = append(converted, trimmed)
		}
	}
	return converted
}

// convertEvidenceIDs maps the old string citations onto this schema's message
// ids. Only a canonical positive decimal string names a message this build
// holds; anything else is left out of the id array and reported.
func convertEvidenceIDs(ids []string) []uint {
	converted := make([]uint, 0, len(ids))
	for _, id := range ids {
		value, ok := parseMessageID(id)
		if !ok {
			continue
		}
		converted = append(converted, value)
	}
	if len(converted) == 0 {
		return nil
	}
	return converted
}

func droppedEvidenceIDs(document legacyDocument) []string {
	var dropped []string
	for _, group := range []legacyEvidenceList{document.ConfirmedDecisions, document.OpenLoops, document.UserCorrections} {
		for _, item := range group {
			for _, id := range item.EvidenceMessageIDs {
				if _, ok := parseMessageID(id); !ok {
					dropped = append(dropped, id)
				}
			}
		}
	}
	return dropped
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
