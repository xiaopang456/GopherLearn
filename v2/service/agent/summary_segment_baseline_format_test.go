package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// The stored format of the commit f1996a0, frozen.
//
// The code below is a copy of that commit's `v2/model/session_summary_segment.go`
// and `v2/model/session_whiteboard.go`: the structures, the checks and the
// decoders, with the names changed so that they can live beside the current
// ones.
//
// It is a copy rather than a call on purpose. The question these tests ask
// is "would the version that was in the field last week still read what this
// one writes", and the only honest way to answer it is to run that version's
// code. Calling today's decoder would answer a different question - it would
// say what today's code does - and today's code already knows about the two
// new fields, so it would happily read a document the old one refuses.
//
// The names carry the commit they came from. Nothing here is called by
// anything but the tests that freeze the format.

// The limits of that commit, as it had them.
const (
	baselineSegmentSchemaV1      = 1
	baselineSegmentPointLimit    = 20
	baselineSegmentPointMaxRunes = 500
	baselineSegmentTermLimit     = 32
	baselineSegmentTermMaxRunes  = 100
	baselineSegmentEvidenceLimit = 32
	baselineBoardSchemaV1        = 1
	baselineBoardItemLimit       = 20
	baselineBoardTextMaxRunes    = 200
	baselineBoardEvidenceLimit   = 8
)

// baselineSegmentPoint is a point of a segment as f1996a0 stored it.
//
// There is no source field: that commit could only hold what the learner
// said, and a document that carries a source is a document it refuses.
type baselineSegmentPoint struct {
	Text               string `json:"text"`
	EvidenceMessageIDs []uint `json:"evidence_message_ids,omitempty"`
}

// baselineSegmentContent is a segment body as f1996a0 stored it.
type baselineSegmentContent struct {
	SchemaVersion       int                    `json:"schema_version"`
	Topic               string                 `json:"topic"`
	Points              []baselineSegmentPoint `json:"points"`
	Names               []string               `json:"names,omitempty"`
	Numbers             []string               `json:"numbers,omitempty"`
	MigratedFromVersion uint64                 `json:"migrated_from_version,omitempty"`
}

// baselineBoardEntry is one board entry as f1996a0 stored it.
//
// There is no origin field, for the same reason the point has no source.
type baselineBoardEntry struct {
	Key                string `json:"key,omitempty"`
	Text               string `json:"text"`
	Value              string `json:"value,omitempty"`
	State              string `json:"state,omitempty"`
	SourceMessageIDs   []uint `json:"source_message_ids,omitempty"`
	UpdatedAtMessageID uint   `json:"updated_at_message_id,omitempty"`
}

// baselineBoardGoal is the board's goal as f1996a0 stored it.
type baselineBoardGoal struct {
	Text               string `json:"text"`
	SourceMessageIDs   []uint `json:"source_message_ids,omitempty"`
	UpdatedAtMessageID uint   `json:"updated_at_message_id,omitempty"`
}

// baselineBoardDocument is the board as f1996a0 stored it.
type baselineBoardDocument struct {
	SchemaVersion       int                  `json:"schema_version"`
	Goal                *baselineBoardGoal   `json:"goal,omitempty"`
	Constraints         []baselineBoardEntry `json:"constraints,omitempty"`
	Plan                []baselineBoardEntry `json:"plan,omitempty"`
	OpenItems           []baselineBoardEntry `json:"open_items,omitempty"`
	MigratedFromVersion uint64               `json:"migrated_from_version,omitempty"`
}

// decodeBaselineSegmentContent is the segment decoder of f1996a0.
//
// Unknown fields are refused rather than ignored: a body that grows a field
// without this decoder knowing about it is a body whose meaning has changed
// underneath the code that reads it.
func decodeBaselineSegmentContent(content string) (baselineSegmentContent, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return baselineSegmentContent{}, fmt.Errorf("summary segment content is empty")
	}
	if !strings.HasPrefix(content, "{") {
		return baselineSegmentContent{}, fmt.Errorf("summary segment must be a JSON object")
	}
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	var document baselineSegmentContent
	if err := decoder.Decode(&document); err != nil {
		return baselineSegmentContent{}, fmt.Errorf("decode summary segment: %w", err)
	}
	if err := ensureBaselineSegmentJSONEOF(decoder); err != nil {
		return baselineSegmentContent{}, err
	}
	if document.SchemaVersion != baselineSegmentSchemaV1 {
		return baselineSegmentContent{}, fmt.Errorf("unsupported summary segment schema version %d", document.SchemaVersion)
	}
	if err := document.Validate(); err != nil {
		return baselineSegmentContent{}, err
	}
	return document, nil
}

// decodeBaselineBoardDocument is the board decoder of f1996a0.
func decodeBaselineBoardDocument(content string) (baselineBoardDocument, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return baselineBoardDocument{}, fmt.Errorf("session whiteboard is empty")
	}
	if !strings.HasPrefix(content, "{") {
		return baselineBoardDocument{}, fmt.Errorf("session whiteboard must be a JSON object")
	}
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	var document baselineBoardDocument
	if err := decoder.Decode(&document); err != nil {
		return baselineBoardDocument{}, fmt.Errorf("decode session whiteboard: %w", err)
	}
	if err := ensureBaselineBoardJSONEOF(decoder); err != nil {
		return baselineBoardDocument{}, err
	}
	if document.SchemaVersion != baselineBoardSchemaV1 {
		return baselineBoardDocument{}, fmt.Errorf("unsupported session whiteboard schema version %d", document.SchemaVersion)
	}
	if err := document.Validate(); err != nil {
		return baselineBoardDocument{}, err
	}
	return document, nil
}

// Validate checks a segment body's shape and bounds, as f1996a0 did.
func (c baselineSegmentContent) Validate() error {
	if c.SchemaVersion != baselineSegmentSchemaV1 {
		return fmt.Errorf("summary segment schema version must be %d", baselineSegmentSchemaV1)
	}
	if len([]rune(strings.TrimSpace(c.Topic))) > 200 {
		return fmt.Errorf("summary segment topic is too long")
	}
	if len(c.Points) > baselineSegmentPointLimit {
		return fmt.Errorf("summary segment has too many points")
	}
	for _, point := range c.Points {
		if err := point.Validate(); err != nil {
			return err
		}
	}
	if len(c.Names) > baselineSegmentTermLimit {
		return fmt.Errorf("summary segment has too many names")
	}
	if len(c.Numbers) > baselineSegmentTermLimit {
		return fmt.Errorf("summary segment has too many numbers")
	}
	for _, term := range append(append([]string{}, c.Names...), c.Numbers...) {
		if len([]rune(strings.TrimSpace(term))) > baselineSegmentTermMaxRunes {
			return fmt.Errorf("summary segment term is too long")
		}
	}
	return nil
}

// Validate checks one point, as f1996a0 did.
func (p baselineSegmentPoint) Validate() error {
	if strings.TrimSpace(p.Text) == "" {
		return fmt.Errorf("summary segment point has no text")
	}
	if len([]rune(strings.TrimSpace(p.Text))) > baselineSegmentPointMaxRunes {
		return fmt.Errorf("summary segment point is too long")
	}
	if len(p.EvidenceMessageIDs) > baselineSegmentEvidenceLimit {
		return fmt.Errorf("summary segment point cites too many messages")
	}
	seen := make(map[uint]struct{}, len(p.EvidenceMessageIDs))
	for _, id := range p.EvidenceMessageIDs {
		if id == 0 {
			return fmt.Errorf("summary segment evidence id must be positive")
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("duplicate summary segment evidence id %d", id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

// Validate checks the board's shape and bounds, as f1996a0 did.
func (d *baselineBoardDocument) Validate() error {
	if d == nil {
		return fmt.Errorf("session whiteboard is nil")
	}
	if d.SchemaVersion != baselineBoardSchemaV1 {
		return fmt.Errorf("session whiteboard schema version must be %d", baselineBoardSchemaV1)
	}
	if d.Goal != nil {
		if err := validateBaselineBoardGoal(*d.Goal); err != nil {
			return err
		}
	}
	for _, group := range [][]baselineBoardEntry{d.Constraints, d.Plan, d.OpenItems} {
		if len(group) > baselineBoardItemLimit {
			return fmt.Errorf("session whiteboard has too many entries")
		}
		for _, entry := range group {
			if err := entry.Validate(); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateBaselineBoardGoal(goal baselineBoardGoal) error {
	if len([]rune(strings.TrimSpace(goal.Text))) > baselineBoardTextMaxRunes {
		return fmt.Errorf("session whiteboard goal is too long")
	}
	return validateBaselineBoardEvidence(goal.SourceMessageIDs)
}

// Validate checks one entry, as f1996a0 did.
func (e baselineBoardEntry) Validate() error {
	if strings.TrimSpace(e.Text) == "" {
		return fmt.Errorf("session whiteboard entry has no text")
	}
	if len([]rune(strings.TrimSpace(e.Text))) > baselineBoardTextMaxRunes {
		return fmt.Errorf("session whiteboard entry is too long")
	}
	if len([]rune(strings.TrimSpace(e.Key))) > baselineBoardTextMaxRunes {
		return fmt.Errorf("session whiteboard entry key is too long")
	}
	if len([]rune(strings.TrimSpace(e.Value))) > baselineBoardTextMaxRunes {
		return fmt.Errorf("session whiteboard entry value is too long")
	}
	switch e.State {
	case "", "open", "done":
	default:
		return fmt.Errorf("session whiteboard entry has an unknown state %q", e.State)
	}
	return validateBaselineBoardEvidence(e.SourceMessageIDs)
}

func validateBaselineBoardEvidence(ids []uint) error {
	if len(ids) > baselineBoardEvidenceLimit {
		return fmt.Errorf("session whiteboard entry cites too many messages")
	}
	seen := make(map[uint]struct{}, len(ids))
	for _, id := range ids {
		if id == 0 {
			return fmt.Errorf("session whiteboard evidence id must be positive")
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("duplicate session whiteboard evidence id %d", id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

// ensureBaselineSegmentJSONEOF is the trailing-JSON check of f1996a0.
func ensureBaselineSegmentJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("decode trailing summary segment: %w", err)
	}
	return fmt.Errorf("summary segment contains trailing JSON")
}

// ensureBaselineBoardJSONEOF is the trailing-JSON check of f1996a0.
func ensureBaselineBoardJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("decode trailing session whiteboard: %w", err)
	}
	return fmt.Errorf("session whiteboard contains trailing JSON")
}
