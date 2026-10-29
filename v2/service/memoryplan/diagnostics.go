package memoryplan

import "time"

// Stage names the earliest point a turn stopped at.
//
// Attribution to exactly one earliest stage is the whole reporting discipline
// here: "the memory was not used" has at least six different causes, and a
// report that cannot tell a catalogue that never listed the record from a
// verifier that saw it and said no is a report that cannot say what to fix.
type Stage string

const (
	StageCatalogue  Stage = "catalogue"
	StagePlanner    Stage = "planner"
	StageCandidates Stage = "candidates"
	StageVerifier   Stage = "verifier"
	StageComplete   Stage = "complete"
)

// Diagnostics is one turn's internal record.
//
// Nothing here carries user text. Topic keys, subjects and bodies stay out on
// purpose: this struct reaches logs and metrics, and a private catalogue in a
// log line is the same leak as a private catalogue in an answer.
type Diagnostics struct {
	Stage  Stage  `json:"stage"`
	Reason string `json:"reason,omitempty"`
	// Reasons are additional non-sensitive codes gathered along the way.
	Reasons []string `json:"reasons,omitempty"`

	CatalogueVersion  string `json:"catalogue_version,omitempty"`
	CatalogueTopics   int    `json:"catalogue_topics"`
	CatalogueEpisodes int    `json:"catalogue_episodes"`
	CatalogueTokens   int    `json:"catalogue_tokens"`
	CatalogueOverflow bool   `json:"catalogue_overflow"`

	PlanHash       string `json:"plan_hash,omitempty"`
	Decision       string `json:"decision,omitempty"`
	Purpose        string `json:"purpose,omitempty"`
	PlanTopicKeys  int    `json:"plan_topic_keys"`
	PlanEpisodeIDs int    `json:"plan_episode_ids"`
	PlanHasQuery   bool   `json:"plan_has_query"`

	CandidateExplicit int `json:"candidate_explicit"`
	CandidateExact    int `json:"candidate_exact"`
	CandidateQuery    int `json:"candidate_query"`
	CandidateLatest   int `json:"candidate_latest"`
	CandidateSemantic int `json:"candidate_semantic"`
	CandidateEpisodes int `json:"candidate_episodes"`
	ReloadRejections  int `json:"reload_rejections"`

	AnswerEvidence  int `json:"relation_answer_evidence"`
	Personalization int `json:"relation_personalization"`
	Contradiction   int `json:"relation_contradicts_premise"`
	RelatedOnly     int `json:"relation_related_only"`
	Insufficient    int `json:"relation_insufficient"`

	InjectedSemantic int `json:"injected_semantic"`
	InjectedEpisodes int `json:"injected_episodes"`

	PlannerCalls  int   `json:"planner_calls"`
	VerifierCalls int   `json:"verifier_calls"`
	ElapsedMS     int64 `json:"elapsed_ms"`
}

func (d *Diagnostics) countRelation(relation Relation) {
	switch relation {
	case RelationAnswer:
		d.AnswerEvidence++
	case RelationPersonalization:
		d.Personalization++
	case RelationContradiction:
		d.Contradiction++
	case RelationRelated:
		d.RelatedOnly++
	case RelationInsufficient:
		d.Insufficient++
	}
}

func (d *Diagnostics) finish(started time.Time, stage Stage, reason string) {
	d.Stage = stage
	if reason != "" {
		d.Reason = reason
	}
	d.ElapsedMS = time.Since(started).Milliseconds()
	if stage != StageComplete {
		ObserveDegradation(stage)
	}
}

// FailedClosed reports whether this turn injected nothing because a stage
// failed, as opposed to injecting nothing because the question needed nothing.
//
// The two are counted apart everywhere. A model that decided "none" correctly is
// the system working; a planner that timed out is the system degraded, and a
// report that adds them together can show an outage as good judgement.
func (d Diagnostics) FailedClosed() bool {
	switch d.Stage {
	case StageComplete:
		return false
	case StageCatalogue, StagePlanner, StageCandidates, StageVerifier:
		return true
	}
	return false
}
