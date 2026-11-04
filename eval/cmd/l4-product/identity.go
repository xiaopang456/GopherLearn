package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// scenarioIdentity is stable across preparation and repetitions, but differs
// across runs, configurations, scenarios and conditions.
func scenarioIdentity(run, arm, scenario, condition string) (int64, string, error) {
	for _, part := range []string{run, arm, scenario, condition} {
		if strings.TrimSpace(part) == "" {
			return 0, "", fmt.Errorf("run, configuration, scenario and condition are required")
		}
	}
	data, _ := json.Marshal([]string{run, arm, scenario, condition})
	digest := sha256.Sum256(data)
	id := int64(binary.BigEndian.Uint64(digest[:8]) & 0x7fffffffffffffff)
	if id == 0 {
		return 0, "", fmt.Errorf("invalid derived identity")
	}
	return id, "l4_" + hex.EncodeToString(digest[:16]), nil
}
