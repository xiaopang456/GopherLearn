package conversation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"GopherAI/entry"
)

type Provenance struct {
	Revision       string `json:"revision"`
	CheckoutSHA256 string `json:"checkout_sha256"`
	Dirty          bool   `json:"dirty"`
	SnapshotSHA256 string `json:"snapshot_sha256"`
	ConfigSHA256   string `json:"config_sha256"`
}

// collectProvenance hashes tracked source/configuration files, never credentials
// or private external data. Infrastructure addresses and per-attempt credentials
// are excluded from the functional configuration digest.
func collectProvenance(ctx context.Context, repository string, s *entry.Settings, snapshot Snapshot) (out Provenance, err error) {
	git := func(args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "git", append([]string{"-C", repository}, args...)...).Output()
	}
	revision, err := git("rev-parse", "HEAD")
	if err != nil {
		return out, fmt.Errorf("read evaluation revision")
	}
	out.Revision = strings.TrimSpace(string(revision))
	paths, err := git("ls-files", "-z", "--", "eval", "v2", "hybrid", "internal", "conversation", "go.mod", "go.sum", "tutorial/v2-entry")
	if err != nil {
		return out, fmt.Errorf("list source files")
	}
	hash := sha256.New()
	for _, path := range strings.Split(string(paths), "\x00") {
		if path == "" {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(repository, filepath.FromSlash(path)))
		if readErr != nil {
			return out, fmt.Errorf("read tracked source")
		}
		fmt.Fprintf(hash, "%s\x00%s\x00", path, digest(data))
	}
	out.CheckoutSHA256 = hex.EncodeToString(hash.Sum(nil))
	status, err := git("status", "--porcelain")
	if err != nil {
		return out, fmt.Errorf("read checkout state")
	}
	out.Dirty = len(status) > 0
	payload, err := json.Marshal(snapshotPayload{snapshot, snapshot.Session.CreationRequestKey, snapshot.Session.CreationInputHash})
	if err != nil {
		return out, err
	}
	out.SnapshotSHA256 = digest(payload)
	knowledge := s.Config.KnowledgeConfig
	knowledge.StorageDir = ""
	functional := map[string]any{"agent": s.Config.AgentConfig, "knowledge": knowledge, "summary_policy": ProductSummaryConfig(s.Config), "chat_model": s.Entry.ChatModel, "thinking_dialect": s.Config.RagChatThinkingDialect, "embedding_model": s.Config.RagEmbeddingModel, "dimension": s.Config.RagDimension, "rerank_enabled": s.Config.RagRerankEnabled, "rerank_provider": s.Config.RagRerankProvider, "rerank_model": s.Config.RagRerankModel}
	encoded, err := json.Marshal(functional)
	if err != nil {
		return out, err
	}
	out.ConfigSHA256 = digest(encoded)
	return out, nil
}
