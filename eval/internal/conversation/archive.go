package conversation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"GopherAI/model"
	"github.com/xiaopang456/GopherLearn/eval/internal/pathguard"
)

type snapshotPayload struct {
	Snapshot           Snapshot `json:"snapshot"`
	CreationRequestKey *string  `json:"creation_request_key,omitempty"`
	CreationInputHash  string   `json:"creation_input_hash,omitempty"`
}
type snapshotEnvelope struct {
	Version int             `json:"version"`
	SHA256  string          `json:"sha256"`
	Payload json.RawMessage `json:"payload"`
}

// WriteSnapshot creates a new private file, never overwriting an existing result.
func WriteSnapshot(path string, snapshot Snapshot) error {
	if err := snapshot.Validate(); err != nil {
		return err
	}
	if err := pathguard.Check("snapshot", path); err != nil {
		return err
	}
	primary, _ := snapshot.Primary()
	payload, err := json.Marshal(snapshotPayload{Snapshot: snapshot,
		CreationRequestKey: primary.Session.CreationRequestKey,
		CreationInputHash:  primary.Session.CreationInputHash})
	if err != nil {
		return err
	}
	hash := sha256.Sum256(payload)
	data, err := json.Marshal(snapshotEnvelope{2, hex.EncodeToString(hash[:]), payload})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// ReadSnapshot verifies its digest before decoding or accepting its content.
//
// Version 1 carried exactly one conversation and version 2 carries a list.
// Both are read, because a run that was prepared before the list existed is
// still a run, and refusing its archive would make the older evidence
// unreadable.
func ReadSnapshot(path string) (Snapshot, error) {
	var result Snapshot
	if err := pathguard.Check("snapshot", path); err != nil {
		return result, err
	}
	f, err := os.Open(path)
	if err != nil {
		return result, err
	}
	defer f.Close()
	// Bound the allocation for malformed or unexpectedly large files.
	b, err := io.ReadAll(io.LimitReader(f, 16<<20+1))
	if err != nil {
		return result, err
	}
	if len(b) > 16<<20 {
		return result, fmt.Errorf("snapshot too large")
	}
	var env snapshotEnvelope
	if err = json.Unmarshal(b, &env); err != nil {
		return result, err
	}
	if env.Version != 1 && env.Version != 2 {
		return result, fmt.Errorf("unsupported snapshot version")
	}
	sum := sha256.Sum256(env.Payload)
	if hex.EncodeToString(sum[:]) != env.SHA256 {
		return result, fmt.Errorf("snapshot digest mismatch")
	}
	var payload snapshotPayload
	if env.Version == 1 {
		// The first version named one conversation at the top level of the
		// snapshot object. It is read into the list shape here, so that the
		// rest of the harness only ever sees one shape.
		var legacy struct {
			Snapshot struct {
				Session  model.Session         `json:"session"`
				Messages []model.Message       `json:"messages"`
				Summary  *model.SessionSummary `json:"summary,omitempty"`
			} `json:"snapshot"`
			CreationRequestKey *string `json:"creation_request_key,omitempty"`
			CreationInputHash  string  `json:"creation_input_hash,omitempty"`
		}
		if err = json.Unmarshal(env.Payload, &legacy); err != nil {
			return result, err
		}
		result.Sessions = []SessionSnapshot{{
			Session: legacy.Snapshot.Session, Messages: legacy.Snapshot.Messages, Summary: legacy.Snapshot.Summary,
		}}
		primary, _ := result.Primary()
		primary.Session.CreationRequestKey = legacy.CreationRequestKey
		primary.Session.CreationInputHash = legacy.CreationInputHash
		result.Sessions[0] = primary
	} else {
		if err = json.Unmarshal(env.Payload, &payload); err != nil {
			return result, err
		}
		result = payload.Snapshot
		primary, _ := result.Primary()
		primary.Session.CreationRequestKey = payload.CreationRequestKey
		primary.Session.CreationInputHash = payload.CreationInputHash
		result.Sessions[0] = primary
	}
	if len(result.Sessions) == 0 {
		return result, fmt.Errorf("the archive carries no conversation")
	}
	if err = result.Validate(); err != nil {
		return Snapshot{}, err
	}
	return result, nil
}
