//go:build v2_real_gates

package hybrid

import (
	"GopherAI/entry"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestI0TargetEvidenceClosedMatches(t *testing.T) {
	s, err := entry.Load(filepath.Join("..", "tutorial", "v2-real-gates-plan", "i0-diagnostics", "synthetic.toml"))
	if err != nil {
		t.Fatal("synthetic Load failed")
	}
	c := s.Config
	c.MysqlPassword = "PRIVATE"
	if v := compareI0Target(c, 55107, strings.Repeat("c", 32)); !v["host_loopback_match"] || !v["authorized_port_match"] || !v["batch_schema_match"] {
		t.Fatal("target matches lost")
	}
	c.MysqlHost = "PRIVATE"
	c.MysqlPort = 65069
	c.MysqlDatabaseName = "PRIVATE"
	v := compareI0Target(c, 55107, strings.Repeat("c", 32))
	if v["host_loopback_match"] || v["authorized_port_match"] || v["batch_schema_match"] {
		t.Fatal("target mismatch lost")
	}
	raw, _ := json.Marshal(v)
	if strings.Contains(string(raw), "PRIVATE") {
		t.Fatal("private target leaked")
	}
	if compareI0Target(nil, 0, "")["config_available"] {
		t.Fatal("nil target accepted")
	}
}
