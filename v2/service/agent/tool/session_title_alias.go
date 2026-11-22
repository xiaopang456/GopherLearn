package agenttool

import (
	"strings"

	serviceagent "GopherAI/service/agent"
)

// lockedSessionAlias maps the locked conversation's TITLE to its id.
//
// The locked block shows the model both the title and the id, and the model
// keeps naming the title in the session parameter ("Evaluation conversation").
// The id is what authorises a read, and a title is not an id: without this the
// same-session read is refused with session_not_allowed and the turn answers
// from the compressed block alone.
//
// The mapping is deliberately narrow. It only exists while a lock is in
// effect, it only maps to the LOCKED id, and it requires the title to match
// case-insensitively. A title of any other conversation maps to nothing, so a
// model that names the wrong conversation still gets nothing.
func lockedSessionAlias(run serviceagent.RunContext, requested string) string {
	locked := strings.TrimSpace(run.LockedSessionID)
	title := strings.TrimSpace(run.LockedSessionTitle)
	requested = strings.TrimSpace(requested)
	if locked == "" || title == "" || requested == "" {
		return ""
	}
	if strings.EqualFold(requested, title) {
		return locked
	}
	return ""
}
