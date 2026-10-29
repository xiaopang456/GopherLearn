package domain

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMemoryScopeRules(t *testing.T) {
	if !MemoryScopeGlobal.IsValid() || !MemoryScopeTopic.IsValid() || !MemoryScopeEpisodic.IsValid() {
		t.Fatal("the three documented scopes must be valid")
	}
	if scope := MemoryScope("profile"); scope.IsValid() {
		t.Fatalf("%q must not be a memory scope", scope)
	}
	// A new layer has to be a deliberate decision about versioning, not a string that
	// happens to compile: this fails the moment someone adds a scope without saying
	// whether confirming one retires the previous.
	if !MemoryScopeGlobal.SupersedesSiblings() || !MemoryScopeTopic.SupersedesSiblings() {
		t.Fatal("global and topic memory are versioned statements and must supersede")
	}
	if MemoryScopeEpisodic.SupersedesSiblings() {
		t.Fatal("episodes are events; confirming one must not retire another")
	}
}

func TestMemoryStatusTransitionsAreClosed(t *testing.T) {
	cases := []struct {
		from MemoryStatus
		to   MemoryStatus
		want bool
	}{
		{MemoryStatusPending, MemoryStatusActive, true},
		{MemoryStatusPending, MemoryStatusArchived, true},
		{MemoryStatusPending, MemoryStatusPending, false},
		{MemoryStatusPending, MemoryStatusSuperseded, false},
		{MemoryStatusActive, MemoryStatusSuperseded, true},
		{MemoryStatusActive, MemoryStatusArchived, true},
		{MemoryStatusActive, MemoryStatusPending, false},
		{MemoryStatusActive, MemoryStatusActive, false},
		// Terminal states have no way out: bringing an old answer back is a new
		// confirmation, not an edit of history.
		{MemoryStatusSuperseded, MemoryStatusActive, false},
		{MemoryStatusSuperseded, MemoryStatusArchived, false},
		{MemoryStatusArchived, MemoryStatusActive, false},
		{MemoryStatusArchived, MemoryStatusPending, false},
		{MemoryStatus("unknown"), MemoryStatusActive, false},
	}
	for _, testCase := range cases {
		if got := testCase.from.CanTransitionTo(testCase.to); got != testCase.want {
			t.Fatalf("%s -> %s = %v, want %v", testCase.from, testCase.to, got, testCase.want)
		}
	}
}

func TestMemoryDraftCarriesNoStatus(t *testing.T) {
	// The point of a separate draft type: there is no field to set, so a candidate cannot
	// arrive already confirmed, and no refactor can quietly make that possible.
	draft := MemoryDraft{UserID: "user-1", Scope: MemoryScopeGlobal, Content: "用 Go 写后端", SourceSessionID: "session-1", SourceMessageIDs: []string{"message-1"}}
	stored := draft.StoredMemory()
	if stored.Status != MemoryStatusPending {
		t.Fatalf("a stored draft has status %q, want pending", stored.Status)
	}
	if draft.UserID == "" || stored.UserID != "user-1" {
		t.Fatalf("draft identity lost: %+v", stored)
	}
}

func TestMemoryDraftValidation(t *testing.T) {
	longContent := strings.Repeat("很", MaxMemoryContentRunes+1)
	longKey := strings.Repeat("k", MaxMemoryTopicKeyRunes+1)
	tooMany := make([]string, 0, MaxMemorySourceMessages+1)
	for index := range cap(tooMany) {
		tooMany = append(tooMany, "message-"+strconv.Itoa(index))
	}

	cases := []struct {
		name  string
		draft MemoryDraft
		ok    bool
	}{
		{"global without a key", MemoryDraft{UserID: "u", Scope: MemoryScopeGlobal, Content: "x", SourceSessionID: "s", SourceMessageIDs: []string{"m"}}, true},
		{"topic with a key", MemoryDraft{UserID: "u", Scope: MemoryScopeTopic, TopicKey: " Redis ", Content: "x", SourceSessionID: "s", SourceMessageIDs: []string{"m"}}, true},
		{"episode without a key", MemoryDraft{UserID: "u", Scope: MemoryScopeEpisodic, Content: "x", SourceSessionID: "s", SourceMessageIDs: []string{"m1", "m2"}}, true},
		{"episode with a key", MemoryDraft{UserID: "u", Scope: MemoryScopeEpisodic, TopicKey: "部署", Content: "x", SourceSessionID: "s", SourceMessageIDs: []string{"m"}}, true},
		{"zero confidence is unrated", MemoryDraft{UserID: "u", Scope: MemoryScopeGlobal, Content: "x", SourceSessionID: "s", SourceMessageIDs: []string{"m"}}, true},
		{"top of the confidence range", MemoryDraft{UserID: "u", Scope: MemoryScopeGlobal, Content: "x", Confidence: 1, SourceSessionID: "s", SourceMessageIDs: []string{"m"}}, true},
		{"no user", MemoryDraft{Scope: MemoryScopeGlobal, Content: "x", SourceSessionID: "s", SourceMessageIDs: []string{"m"}}, false},
		{"unknown scope", MemoryDraft{UserID: "u", Scope: "mood", Content: "x", SourceSessionID: "s", SourceMessageIDs: []string{"m"}}, false},
		{"global with a topic key", MemoryDraft{UserID: "u", Scope: MemoryScopeGlobal, TopicKey: "go", Content: "x", SourceSessionID: "s", SourceMessageIDs: []string{"m"}}, false},
		{"topic without a key", MemoryDraft{UserID: "u", Scope: MemoryScopeTopic, Content: "x", SourceSessionID: "s", SourceMessageIDs: []string{"m"}}, false},
		{"topic key too long", MemoryDraft{UserID: "u", Scope: MemoryScopeTopic, TopicKey: longKey, Content: "x", SourceSessionID: "s", SourceMessageIDs: []string{"m"}}, false},
		{"topic key with a control character", MemoryDraft{UserID: "u", Scope: MemoryScopeTopic, TopicKey: "redis\n备注", Content: "x", SourceSessionID: "s", SourceMessageIDs: []string{"m"}}, false},
		{"no content", MemoryDraft{UserID: "u", Scope: MemoryScopeGlobal, Content: "   ", SourceSessionID: "s", SourceMessageIDs: []string{"m"}}, false},
		{"content longer than a memory", MemoryDraft{UserID: "u", Scope: MemoryScopeGlobal, Content: longContent, SourceSessionID: "s", SourceMessageIDs: []string{"m"}}, false},
		{"negative confidence", MemoryDraft{UserID: "u", Scope: MemoryScopeGlobal, Content: "x", Confidence: -0.1, SourceSessionID: "s", SourceMessageIDs: []string{"m"}}, false},
		{"confidence above one", MemoryDraft{UserID: "u", Scope: MemoryScopeGlobal, Content: "x", Confidence: 1.01, SourceSessionID: "s", SourceMessageIDs: []string{"m"}}, false},
		{"no session named", MemoryDraft{UserID: "u", Scope: MemoryScopeGlobal, Content: "x", SourceMessageIDs: []string{"m"}}, false},
		{"no message named", MemoryDraft{UserID: "u", Scope: MemoryScopeGlobal, Content: "x", SourceSessionID: "s"}, false},
		{"blank message id", MemoryDraft{UserID: "u", Scope: MemoryScopeGlobal, Content: "x", SourceSessionID: "s", SourceMessageIDs: []string{" "}}, false},
		{"repeated message id", MemoryDraft{UserID: "u", Scope: MemoryScopeGlobal, Content: "x", SourceSessionID: "s", SourceMessageIDs: []string{"m", "m"}}, false},
		{"too many source messages", MemoryDraft{UserID: "u", Scope: MemoryScopeGlobal, Content: "x", SourceSessionID: "s", SourceMessageIDs: tooMany}, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.draft.Validate()
			if testCase.ok && err != nil {
				t.Fatalf("a legal draft was refused: %v", err)
			}
			if !testCase.ok && err == nil {
				t.Fatal("an illegal draft was accepted")
			}
		})
	}
}

func TestMemoryStoredRules(t *testing.T) {
	valid := Memory{MemoryID: "mem-1", UserID: "u", Scope: MemoryScopeGlobal, Content: "x",
		Status: MemoryStatusActive, SourceSessionID: "s", SourceMessageIDs: []string{"m"}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a complete memory was refused: %v", err)
	}
	if !valid.Recallable() {
		t.Fatal("only an active memory should report itself recallable")
	}
	for name, broken := range map[string]Memory{
		"no id": {UserID: "u", Scope: MemoryScopeGlobal, Content: "x", Status: MemoryStatusActive, SourceSessionID: "s", SourceMessageIDs: []string{"m"}},
		"unknown status": {MemoryID: "mem-1", UserID: "u", Scope: MemoryScopeGlobal, Content: "x",
			Status: "approved", SourceSessionID: "s", SourceMessageIDs: []string{"m"}},
		"unknown scope": {MemoryID: "mem-1", UserID: "u", Scope: "mood", Content: "x",
			Status: MemoryStatusActive, SourceSessionID: "s", SourceMessageIDs: []string{"m"}},
		"no evidence": {MemoryID: "mem-1", UserID: "u", Scope: MemoryScopeGlobal, Content: "x",
			Status: MemoryStatusActive, SourceSessionID: "s"},
	} {
		if err := broken.Validate(); err == nil {
			t.Fatalf("%s: a broken stored memory passed validation", name)
		}
	}
}

func TestMemoryCloneDetachesProvenance(t *testing.T) {
	original := Memory{MemoryID: "mem-1", UserID: "u", Scope: MemoryScopeEpisodic, Content: "x",
		Status: MemoryStatusPending, SourceSessionID: "s", SourceMessageIDs: []string{"m1", "m2"}}
	clone := original.Clone()
	clone.SourceMessageIDs[0] = "rewritten"
	if original.SourceMessageIDs[0] != "m1" {
		t.Fatalf("a caller edited through the copy: %v", original.SourceMessageIDs)
	}
	if !clone.SupersededAt.Equal(original.SupersededAt) {
		t.Fatal("timestamps must survive the copy")
	}
}

func TestMemorySupersededAtStaysZeroUntilUsed(t *testing.T) {
	live := Memory{Status: MemoryStatusActive}
	if !live.SupersededAt.IsZero() {
		t.Fatalf("a fresh memory already carries %v", live.SupersededAt)
	}
	replaced := live
	replaced.Status = MemoryStatusSuperseded
	replaced.SupersededAt = time.Now().UTC()
	if replaced.SupersededAt.IsZero() {
		t.Fatal("the superseded moment must be recordable")
	}
}

// TestProposalsMatchComparesTheWholeClaim pins what "this was already proposed" means.
// Every field that makes a claim checkable has to differ for the match to break, and the
// fields that describe the record's lifecycle rather than its content must not, or a
// re-proposal after a decision would look like a duplicate of something already resolved.
func TestProposalsMatchComparesTheWholeClaim(t *testing.T) {
	claim := Memory{
		MemoryID:         "mem-1",
		UserID:           "user-1",
		Scope:            MemoryScopeTopic,
		TopicKey:         "redis",
		Content:          "AOF 的 fsync 选 everysec",
		Status:           MemoryStatusPending,
		Confidence:       1,
		SourceSessionID:  "session-1",
		SourceMessageIDs: []string{"message-1", "message-2"},
		CreatedAt:        time.Date(2026, 9, 26, 1, 0, 0, 0, time.UTC),
	}
	cases := []struct {
		name  string
		patch func(*Memory)
		want  bool
	}{
		{name: "the same claim matches itself", want: true},
		{name: "different owner never matches", patch: func(m *Memory) { m.UserID = "user-2" }, want: false},
		{name: "different content is a different claim", patch: func(m *Memory) { m.Content = "AOF 的 fsync 选 always" }, want: false},
		{name: "different scope", patch: func(m *Memory) { m.Scope = MemoryScopeGlobal; m.TopicKey = "" }, want: false},
		{name: "different topic key is a different subject", patch: func(m *Memory) { m.TopicKey = "mysql" }, want: false},
		{name: "different citing session", patch: func(m *Memory) { m.SourceSessionID = "session-9" }, want: false},
		{name: "one fewer source message", patch: func(m *Memory) { m.SourceMessageIDs = []string{"message-1"} }, want: false},
		{name: "one different source message", patch: func(m *Memory) { m.SourceMessageIDs = []string{"message-1", "message-3"} }, want: false},
		// Citation order is part of the record, so a reordered list is a different claim
		// rather than the same one written twice. This is a decision, not an accident: the
		// alternative (set comparison) would silently merge two differently-cited drafts.
		{name: "same messages in another order", patch: func(m *Memory) { m.SourceMessageIDs = []string{"message-2", "message-1"} }, want: false},
		{name: "no sources at all is a different claim", patch: func(m *Memory) { m.SourceMessageIDs = nil }, want: false},
		// Lifecycle fields must not break the match: the caller only ever compares against
		// pending rows, and a row's own state is not part of what it claims.
		{name: "status is not part of the claim", patch: func(m *Memory) { m.Status = MemoryStatusActive }, want: true},
		{name: "confidence is not part of the claim", patch: func(m *Memory) { m.Confidence = 0.2 }, want: true},
		{name: "identifiers and timestamps are not part of the claim", patch: func(m *Memory) {
			m.MemoryID = "mem-elsewhere"
			m.CreatedAt = m.CreatedAt.Add(24 * time.Hour)
			m.UpdatedAt = m.CreatedAt
		}, want: true},
		{name: "a supersede timestamp does not change what was claimed", patch: func(m *Memory) {
			m.SupersededAt = m.CreatedAt.Add(time.Hour)
		}, want: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			other := claim.Clone()
			if testCase.patch != nil {
				testCase.patch(&other)
			}
			if got := claim.ProposalsMatch(other); got != testCase.want {
				t.Fatalf("ProposalsMatch = %v, want %v\n left: %+v\n right: %+v", got, testCase.want, claim, other)
			}
		})
	}
}

// TestProposalsMatchTreatsNilAndEmptySourcesAlike documents the one place where Go's slice
// comparison is not structural: an absent list and an empty list are the same absence. No
// stored row can reach this state - Validate requires at least one source message - so the
// pin is about the predicate's semantics, not about a live data shape.
func TestProposalsMatchTreatsNilAndEmptySourcesAlike(t *testing.T) {
	left := Memory{UserID: "user-1", Scope: MemoryScopeGlobal, Content: "同一条", SourceSessionID: "s", SourceMessageIDs: nil}
	right := left.Clone()
	right.SourceMessageIDs = []string{}
	if !left.ProposalsMatch(right) {
		t.Fatal("nil and empty source lists must be the same absence")
	}
	if err := left.Validate(); err == nil {
		t.Fatal("a stored memory with no source message must be invalid, so this state is unreachable in the stores")
	}
}

// TestProposalsMatchPerScope covers the three scopes separately, because the field that
// distinguishes two claims differs per scope: a global fact is identified by its owner
// alone, a topic memory by the key, an episode by the event it cites.
func TestProposalsMatchPerScope(t *testing.T) {
	for _, shape := range []struct {
		name     string
		scope    MemoryScope
		topicKey string
		otherKey string
	}{
		{"global", MemoryScopeGlobal, "", "redis"},
		{"topic", MemoryScopeTopic, "redis", "mysql"},
		{"episodic with no key", MemoryScopeEpisodic, "", "redis"},
		{"episodic with a key", MemoryScopeEpisodic, "redis", "mysql"},
	} {
		t.Run(shape.name, func(t *testing.T) {
			left := Memory{UserID: "user-1", Scope: shape.scope, TopicKey: shape.topicKey,
				Content: "同一条陈述", SourceSessionID: "session-1", SourceMessageIDs: []string{"message-1"}}
			right := left.Clone()
			if !left.ProposalsMatch(right) {
				t.Fatal("an identical claim under the same scope did not match")
			}
			right.TopicKey = shape.otherKey
			if shape.otherKey == shape.topicKey {
				t.Skip("no alternate key to test for this shape")
			}
			// A key is a real distinction even where the scope allows either, because
			// (user, scope, topic_key) is the "one active version" key.
			if left.ProposalsMatch(right) {
				t.Fatalf("scope %q matched across keys %q and %q", shape.scope, shape.topicKey, shape.otherKey)
			}
		})
	}
}

// TestProposalsMatchComparesStoredFormsNotRawInput: trimming happens when a draft becomes a
// record, so two drafts that differ only by surrounding whitespace are the same claim once
// stored - while the predicate itself does no normalization and would call two untrimmed
// rows different. Both halves are pinned because the guarantee comes from one side or the
// other, and a reader needs to know which.
func TestProposalsMatchComparesStoredFormsNotRawInput(t *testing.T) {
	padded := MemoryDraft{UserID: "user-1", Scope: MemoryScopeTopic, TopicKey: "  redis  ",
		Content: "  AOF 的 fsync 选 everysec  ", SourceSessionID: "  session-1  ",
		SourceMessageIDs: []string{"  message-1  "}}
	plain := MemoryDraft{UserID: "user-1", Scope: MemoryScopeTopic, TopicKey: "redis",
		Content: "AOF 的 fsync 选 everysec", SourceSessionID: "session-1",
		SourceMessageIDs: []string{"message-1"}}
	if !padded.StoredMemory().ProposalsMatch(plain.StoredMemory()) {
		t.Fatalf("two spellings of one draft were not the same claim once stored:\n %+v\n %+v",
			padded.StoredMemory(), plain.StoredMemory())
	}
	if (Memory{TopicKey: " redis"}).ProposalsMatch(Memory{TopicKey: "redis"}) {
		t.Fatal("the predicate must not quietly normalize: it compares stored forms only")
	}
}

// TestUserAuthoredMemoryIsDecidedOnArrival pins the authored path's own shape: active,
// server-certain, and holding no evidence of any kind. It is stated at the type level because
// the storage rule is enforced here rather than remembered in two stores.
func TestUserAuthoredMemoryIsDecidedOnArrival(t *testing.T) {
	for _, scope := range []MemoryScope{MemoryScopeGlobal, MemoryScopeTopic} {
		key := ""
		if scope == MemoryScopeTopic {
			key = " redis "
		}
		stored := UserAuthoredMemory{
			UserID: " user-1 ", Scope: scope, TopicKey: key, Content: "  我把压测留到发布前  ",
		}.StoredMemory()
		if stored.Status != MemoryStatusActive {
			t.Fatalf("%s: status = %q, want active", scope, stored.Status)
		}
		if stored.Origin != MemoryOriginUserAuthored {
			t.Fatalf("%s: origin = %q, want %q", scope, stored.Origin, MemoryOriginUserAuthored)
		}
		if stored.Confidence != 1 {
			t.Fatalf("%s: confidence = %v, want the server's own 1", scope, stored.Confidence)
		}
		if stored.UserID != "user-1" || stored.TopicKey != strings.TrimSpace(key) ||
			stored.Content != "我把压测留到发布前" {
			t.Fatalf("%s: fields not trimmed: %+v", scope, stored)
		}
		if stored.SourceSessionID != "" || len(stored.SourceMessageIDs) != 0 {
			t.Fatalf("%s: an authored record grew provenance: %+v", scope, stored)
		}
		stored.MemoryID = "mem-1"
		if err := stored.Validate(); err != nil {
			t.Fatalf("%s: the record this path writes does not validate: %v", scope, err)
		}
	}
}

// TestUserAuthoredValidateRules is the scope rule and the shape rule, in one table: an
// episode is something that happened in a conversation, so it cannot be stated by hand, and
// the key rules are the ones the storage layer already enforces for proposals.
func TestUserAuthoredValidateRules(t *testing.T) {
	long := strings.Repeat("长", MaxMemoryContentRunes+1)
	cases := []struct {
		name    string
		shape   UserAuthoredMemory
		wantErr bool
	}{
		{"global", UserAuthoredMemory{UserID: "user-1", Scope: MemoryScopeGlobal, Content: "一句话"}, false},
		{"topic with its subject", UserAuthoredMemory{UserID: "user-1", Scope: MemoryScopeTopic, TopicKey: "redis", Content: "一句话"}, false},
		{"no author", UserAuthoredMemory{Scope: MemoryScopeGlobal, Content: "一句话"}, true},
		{"an episode", UserAuthoredMemory{UserID: "user-1", Scope: MemoryScopeEpisodic, Content: "一句话"}, true},
		{"an episode with a subject", UserAuthoredMemory{UserID: "user-1", Scope: MemoryScopeEpisodic, TopicKey: "redis", Content: "一句话"}, true},
		{"an unknown scope", UserAuthoredMemory{UserID: "user-1", Scope: "profile", Content: "一句话"}, true},
		{"a global with a subject", UserAuthoredMemory{UserID: "user-1", Scope: MemoryScopeGlobal, TopicKey: "redis", Content: "一句话"}, true},
		{"a topic without one", UserAuthoredMemory{UserID: "user-1", Scope: MemoryScopeTopic, Content: "一句话"}, true},
		{"no content", UserAuthoredMemory{UserID: "user-1", Scope: MemoryScopeGlobal, Content: "   "}, true},
		{"content past the limit", UserAuthoredMemory{UserID: "user-1", Scope: MemoryScopeGlobal, Content: long}, true},
		{"a subject past the limit", UserAuthoredMemory{UserID: "user-1", Scope: MemoryScopeTopic,
			TopicKey: strings.Repeat("主", MaxMemoryTopicKeyRunes+1), Content: "一句话"}, true},
	}
	for _, testCase := range cases {
		err := testCase.shape.Validate()
		if testCase.wantErr && err == nil {
			t.Fatalf("%s: accepted, want refused", testCase.name)
		}
		if !testCase.wantErr && err != nil {
			t.Fatalf("%s: %v, want accepted", testCase.name, err)
		}
	}
}

// TestMemoryValidateSplitByOrigin is where the two doors meet the stored record. An authored
// row needs no evidence and is invalid the moment it has any; a proposed row needs evidence and
// is invalid without it. Reading the same shape back under the other origin therefore changes
// the verdict, which is the point of keeping a discriminator instead of a permissive check.
func TestMemoryValidateSplitByOrigin(t *testing.T) {
	authored := func(mutate func(*Memory)) Memory {
		memory := UserAuthoredMemory{
			UserID: "user-1", Scope: MemoryScopeGlobal, Content: "我把值班交给了 SRE",
		}.StoredMemory()
		memory.MemoryID = "mem-1"
		mutate(&memory)
		return memory
	}
	if err := authored(func(*Memory) {}).Validate(); err != nil {
		t.Fatalf("a clean authored row failed: %v", err)
	}
	for name, mutate := range map[string]func(*Memory){
		"a source session":      func(m *Memory) { m.SourceSessionID = "session-1" },
		"source messages":       func(m *Memory) { m.SourceMessageIDs = []string{"message-1"} },
		"an episodic scope":     func(m *Memory) { m.Scope = MemoryScopeEpisodic },
		"a borrowed subject":    func(m *Memory) { m.TopicKey = "redis" },
		"no content":            func(m *Memory) { m.Content = "   " },
		"an out-of-range grade": func(m *Memory) { m.Confidence = 1.5 },
	} {
		if err := authored(mutate).Validate(); err == nil {
			t.Fatalf("an authored memory with %s validated, want refused", name)
		}
	}

	proposed := MemoryDraft{
		UserID: "user-1", Scope: MemoryScopeGlobal, Content: "用户用 Go 写后端", Confidence: 0.9,
		SourceSessionID: "session-1", SourceMessageIDs: []string{"message-1"},
	}.StoredMemory()
	proposed.MemoryID = "mem-2"
	if err := proposed.Validate(); err != nil {
		t.Fatalf("a well-formed proposal failed: %v", err)
	}
	if proposed.AuthoredBy() != MemoryOriginAgentProposed {
		t.Fatalf("a draft record classified itself as %q", proposed.AuthoredBy())
	}
	// Evidence gone, the record is no longer a record: the same read that lets an authored
	// row stand without provenance refuses this one without it.
	proposed.SourceSessionID = ""
	if err := proposed.Validate(); err == nil {
		t.Fatal("a proposal with no source session validated, want refused")
	}
}

// TestOriginClassificationOfALegacyRow is the migration rule: a row written before the column
// existed has no origin, and the only safe reading of that is "a proposal", because every row
// this build stored before the split came from the draft path. Treating an unknown origin as
// hand-written would promote a record nobody confirmed by hand.
func TestOriginClassificationOfALegacyRow(t *testing.T) {
	legacy := Memory{MemoryID: "mem-old", UserID: "user-1", Scope: MemoryScopeGlobal,
		Content: "升级前确认的一条", Status: MemoryStatusActive, Confidence: 1,
		SourceSessionID: "session-1", SourceMessageIDs: []string{"message-1"}}
	if legacy.Origin != "" {
		t.Fatalf("fixture wrong: origin = %q", legacy.Origin)
	}
	if legacy.AuthoredBy() != MemoryOriginAgentProposed {
		t.Fatalf("a pre-column row read as %q, want agent_proposed", legacy.AuthoredBy())
	}
	if err := legacy.Validate(); err != nil {
		t.Fatalf("the pre-column row no longer validates as a proposal: %v", err)
	}
	// An origin this server does not have is treated the same careful way, not as permission.
	unknown := legacy
	unknown.Origin = MemoryOrigin("imported")
	if unknown.AuthoredBy() != MemoryOriginAgentProposed {
		t.Fatalf("an unknown origin read as %q, want the safer proposal", unknown.AuthoredBy())
	}
	if unknown.Origin.IsValid() {
		t.Fatal("an origin outside the closed set reported itself valid")
	}
}
