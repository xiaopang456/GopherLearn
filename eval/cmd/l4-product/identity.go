package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xiaopang456/GopherLearn/eval/internal/conversation"
)

// scenarioIdentity is stable across preparation and repetitions, but differs
// across runs, configurations, scenarios and conditions.
//
// The user name is derived, so it has to fit the product's own columns: the
// message table keeps the user name in a varchar(20). "l4_" plus sixteen hex
// characters is nineteen characters, which fits and still carries sixty-four
// bits of the digest; the name is an opaque label, the identity is the id.
func scenarioIdentity(run, arm, scenario, condition string) (int64, string, error) {
	return scenarioIdentityForPath(run, arm, scenario, condition, conversation.CardArmModeOff)
}

// scenarioIdentityForPath derives the evaluation user of one attempt.
//
// The path is the cross-session memory the attempt measures: the old Episode
// or the session card. It is part of the digest on purpose. The two paths are
// the two arms of the comparison, and an attempt that ran one of them under
// the same user as the other would read the other one's memories: the card
// arm would find the Episode arm's rows, and the comparison would have
// measured nothing at all.
func scenarioIdentityForPath(run, arm, scenario, condition, path string) (int64, string, error) {
	for _, part := range []string{run, arm, scenario, condition, path} {
		if strings.TrimSpace(part) == "" {
			return 0, "", fmt.Errorf("run, configuration, scenario, condition and memory path are required")
		}
	}
	data, _ := json.Marshal([]string{run, arm, scenario, condition, path})
	digest := sha256.Sum256(data)
	id := int64(binary.BigEndian.Uint64(digest[:8]) & 0x7fffffffffffffff)
	if id == 0 {
		return 0, "", fmt.Errorf("invalid derived identity")
	}
	return id, "l4_" + hex.EncodeToString(digest[:8]), nil
}

// identityInput is everything the command knows about the attempt that needs an
// evaluation user.
type identityInput struct {
	phase         string
	run           string
	arm           string
	qid           string
	condition     string
	rep           int
	knowledgeSeed string
	userID        int64
	userName      string
	// sessionCardMode is the cross-session memory the attempt measures. It is
	// part of the identity, so the two arms of the comparison never share a
	// database.
	sessionCardMode string
}

// deriveScenarioIdentity decides which evaluation user one attempt runs as.
//
// -phase and -rep are seen and deliberately not mixed into the identity: the
// preparation and all repeats of one scenario must restore the same user, and a
// repeat that drifted to a different user would answer from an empty history.
// The four parts that must differ are run, arm, qid and condition, and an
// attempt that asks for one of those to be supplied by hand is refused: an
// explicit identity or a knowledge seed would silently share state between
// scenarios.
func deriveScenarioIdentity(explicit map[string]bool, in identityInput) (int64, string, error) {
	if in.phase == "index" {
		return in.userID, in.userName, nil
	}
	missing := make([]string, 0, 4)
	for _, name := range []string{"run-id", "arm", "qid", "condition"} {
		if !explicit[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return 0, "", fmt.Errorf("scenario identity requires explicit %s", strings.Join(missing, ", "))
	}
	if explicit["user-id"] || explicit["user-name"] {
		return 0, "", fmt.Errorf("scenario runs take a derived identity, not an explicit -user-id or -user-name")
	}
	if in.knowledgeSeed != "" || in.userID != 0 || in.userName != "" {
		return 0, "", fmt.Errorf("scenario runs require an empty knowledge base and derived identity")
	}
	path := strings.TrimSpace(in.sessionCardMode)
	if path == "" {
		path = conversation.CardArmModeOff
	}
	return scenarioIdentityForPath(in.run, in.arm, in.qid, in.condition, path)
}

// cardNamespace is the card index's own namespace for one arm.
//
// The namespace is derived from the arm rather than written by hand, so that
// two arms of the same run cannot be pointed at one index by accident. The
// name stays inside the product's declared family, and the key prefix is the
// one the index is created over: an index over a different prefix would hold
// nothing.
func cardNamespace(arm string) (name, prefix string) {
	digest := sha256.Sum256([]byte(strings.TrimSpace(arm)))
	tag := hex.EncodeToString(digest[:6])
	return "gopherai_session_card_" + tag + "_idx", "gopherai:session_card:" + tag + ":"
}
