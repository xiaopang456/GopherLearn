package model

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const EpisodeSourceRefLimit = 64

// EpisodeSourceRef identifies the exact version of an Episode that was used
// to produce derived conversation text. IDs without versions are insufficient:
// an updated source must invalidate answers projected from its older content.
type EpisodeSourceRef struct {
	EpisodeID string `json:"episode_id"`
	Version   uint64 `json:"version"`
}

type EpisodeSourceRefs []EpisodeSourceRef

func (refs EpisodeSourceRefs) Value() (driver.Value, error) {
	if refs == nil {
		refs = EpisodeSourceRefs{}
	}
	if err := refs.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(refs)
}

func (refs *EpisodeSourceRefs) Scan(value any) error {
	if value == nil {
		*refs = EpisodeSourceRefs{}
		return nil
	}
	var data []byte
	switch typed := value.(type) {
	case []byte:
		data = typed
	case string:
		data = []byte(typed)
	default:
		return fmt.Errorf("scan episode source refs: unsupported type %T", value)
	}
	if len(data) == 0 || string(data) == "null" {
		*refs = EpisodeSourceRefs{}
		return nil
	}
	if err := json.Unmarshal(data, refs); err != nil {
		return fmt.Errorf("scan episode source refs: %w", err)
	}
	if *refs == nil {
		*refs = EpisodeSourceRefs{}
	}
	return refs.Validate()
}

func (refs EpisodeSourceRefs) Validate() error {
	if len(refs) > EpisodeSourceRefLimit {
		return fmt.Errorf("episode source refs exceed %d", EpisodeSourceRefLimit)
	}
	seen := make(map[string]uint64, len(refs))
	for _, ref := range refs {
		id := strings.TrimSpace(ref.EpisodeID)
		if id == "" || ref.Version == 0 {
			return fmt.Errorf("invalid episode source ref")
		}
		if version, exists := seen[id]; exists {
			if version != ref.Version {
				return fmt.Errorf("episode source %q has conflicting versions", id)
			}
			return fmt.Errorf("duplicate episode source %q", id)
		}
		seen[id] = ref.Version
	}
	return nil
}

// MergeEpisodeSourceRefs returns a deterministic dependency closure. It never
// truncates: silently dropping an ancestor would make deletion fail open.
func MergeEpisodeSourceRefs(groups ...EpisodeSourceRefs) (EpisodeSourceRefs, error) {
	byID := make(map[string]uint64)
	for _, group := range groups {
		for _, ref := range group {
			id := strings.TrimSpace(ref.EpisodeID)
			if id == "" || ref.Version == 0 {
				return nil, fmt.Errorf("invalid episode source ref")
			}
			if version, exists := byID[id]; exists && version != ref.Version {
				return nil, fmt.Errorf("episode source %q has conflicting versions", id)
			}
			byID[id] = ref.Version
		}
	}
	if len(byID) > EpisodeSourceRefLimit {
		return nil, fmt.Errorf("episode source refs exceed %d", EpisodeSourceRefLimit)
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	refs := make(EpisodeSourceRefs, 0, len(ids))
	for _, id := range ids {
		refs = append(refs, EpisodeSourceRef{EpisodeID: id, Version: byID[id]})
	}
	return refs, nil
}
