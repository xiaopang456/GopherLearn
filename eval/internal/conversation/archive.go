package conversation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"

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
	payload, err := json.Marshal(snapshotPayload{snapshot, snapshot.Session.CreationRequestKey, snapshot.Session.CreationInputHash})
	if err != nil {
		return err
	}
	hash := sha256.Sum256(payload)
	data, err := json.Marshal(snapshotEnvelope{1, hex.EncodeToString(hash[:]), payload})
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
	if env.Version != 1 {
		return result, fmt.Errorf("unsupported snapshot version")
	}
	sum := sha256.Sum256(env.Payload)
	if hex.EncodeToString(sum[:]) != env.SHA256 {
		return result, fmt.Errorf("snapshot digest mismatch")
	}
	var payload snapshotPayload
	if err = json.Unmarshal(env.Payload, &payload); err != nil {
		return result, err
	}
	result = payload.Snapshot
	result.Session.CreationRequestKey = payload.CreationRequestKey
	result.Session.CreationInputHash = payload.CreationInputHash
	if err = result.Validate(); err != nil {
		return Snapshot{}, err
	}
	return result, nil
}
