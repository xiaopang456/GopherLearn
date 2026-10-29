package summary

// Controls says which of the migrated summarizing rules apply to one turn.
//
// The reason it exists is scope. The reference project's summary step had a token
// threshold, a token input bound, a stage deadline and a failure cooldown; this project's
// rolling summary had none of them. Adopting all four for every strategy would have
// changed when ordinary chat turns pay for a summarizing call, how long they wait for it,
// and whether a failed refresh silences the session afterwards - a change to conversation
// behaviour made in the middle of a migration whose subject was the learning agent's
// context window.
//
// So the controls travel with the turn instead of living in the shared policy, and a
// deployment configures the same numbers for both. The zero value is the legacy set:
// a caller that forgets to ask for the migrated behaviour gets what every strategy did
// before this package existed, which is the direction that cannot silently spend more
// calls or end more turns.
type Controls struct {
	// TokenTrigger lets an estimated-token threshold make a refresh due on its own,
	// beside the message count.
	TokenTrigger bool
	// TokenInputCap bounds one summarizing request in estimated tokens, counting the
	// previous summary and the framing around it.
	TokenInputCap bool
	// StageDeadline caps the refresh by a share of the turn's remaining time rather
	// than by the model call's own timeout alone.
	StageDeadline bool
	// FailureCooldown stops a session re-attempting a refresh for a while after one
	// failed.
	FailureCooldown bool
}

// LegacyControls is the behaviour every strategy had before the migration: message-count
// triggering, message-and-rune input bounds, no stage deadline of its own, no cooldown.
func LegacyControls() Controls { return Controls{} }

// MigratedControls is the full set, used by the learning agent.
func MigratedControls() Controls {
	return Controls{
		TokenTrigger:    true,
		TokenInputCap:   true,
		StageDeadline:   true,
		FailureCooldown: true,
	}
}
