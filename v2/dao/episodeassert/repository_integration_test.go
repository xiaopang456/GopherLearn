package episodeassert

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"GopherAI/internal/testdb"
	"GopherAI/model"
	serviceassert "GopherAI/service/episodeassert"

	"github.com/google/uuid"
	mysqldriver "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// STABILIZE-058 v1.1 Phase A1, production shape.
//
// The pure-function tests in service/episodeassert prove the rules. These prove
// the parts that only a database can: the optimistic version has to be a
// conditional UPDATE and not a read-then-write, the readable-Episode rule has to
// be the same join the rest of the system uses, an assertion has to be dated
// from its own messages, and a store failure has to stay a failure.

func openTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent), TranslateError: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.Session{}, &model.Message{},
		&model.EpisodeMemory{}, &model.EpisodeAssertionState{}); err != nil {
		t.Fatal(err)
	}
	return db
}

type fixture struct {
	db     *gorm.DB
	user   model.User
	other  model.User
	seeded map[string]model.EpisodeMemory
	// message maps a slot key to the message that dates it.
	message map[string]uint
	// suffix keeps assertion identities unique per fixture. They are
	// deterministic by design, so two tests sharing one database would collide
	// on the primary key without it.
	suffix string
}

// seed builds a learner with two cross-session decisions and the messages that
// date them, plus everything the true-invalid checks need.
func seed(t *testing.T, db *gorm.DB) *fixture {
	t.Helper()
	suffix := uuid.NewString()[:8]
	owner := model.User{Username: "s058-" + suffix, Password: "secret"}
	stranger := model.User{Username: "s058-other-" + suffix, Password: "secret"}
	for _, user := range []*model.User{&owner, &stranger} {
		if err := db.Create(user).Error; err != nil {
			t.Fatal(err)
		}
	}
	out := &fixture{db: db, user: owner, other: stranger, suffix: suffix,
		seeded: map[string]model.EpisodeMemory{}, message: map[string]uint{}}

	newCard := func(key string, user model.User, daysAgo int, status model.EpisodeStatus,
		decisions, openLoops []string) model.EpisodeMemory {
		session := model.Session{ID: uuid.NewString(), UserID: user.ID, UserName: user.Username,
			Mode: model.SessionModeAgent, Title: key}
		if err := db.Create(&session).Error; err != nil {
			t.Fatal(err)
		}
		at := time.Now().UTC().AddDate(0, 0, -daysAgo).Truncate(time.Second)
		toEvidence := func(kind model.AssertionKind, texts []string) []model.EpisodeEvidence {
			entries := make([]model.EpisodeEvidence, 0, len(texts))
			for index, text := range texts {
				// Each slot gets its own message, written at its own time, so
				// two decisions in one card can be ordered against each other.
				message := model.Message{SessionID: session.ID, UserName: user.Username,
					Content: text, IsUser: true,
					CreatedAt: at.Add(time.Duration(index) * time.Hour)}
				if err := db.Create(&message).Error; err != nil {
					t.Fatal(err)
				}
				out.message[fmt.Sprintf("%s:%s%d", key, kind.Letter(), index+1)] = message.ID
				entries = append(entries, model.EpisodeEvidence{
					ID:                 model.DeterministicAssertionID(fmt.Sprintf("%s|%s|%s|%d", out.suffix, key, kind, index+1)),
					Text:               text,
					EvidenceMessageIDs: []uint{message.ID},
				})
			}
			return entries
		}
		card := model.EpisodeMemory{
			ID: uuid.NewString(), UserID: user.ID, SessionID: session.ID,
			Title: key, SearchText: key,
			Document: model.EpisodeDocument{
				Synopsis: "概述 " + key, Decisions: toEvidence(model.AssertionDecision, decisions),
				OpenLoops: toEvidence(model.AssertionOpenLoop, openLoops), SourceHandles: []string{"h:" + key},
			},
			SourceLastMessageID: 1, LastMessageAt: at, Version: 1,
			Status: status, IndexStatus: model.EpisodeIndexIndexed,
		}
		if err := db.Create(&card).Error; err != nil {
			t.Fatal(err)
		}
		out.seeded[key] = card
		return card
	}

	newCard("old", owner, 30, model.EpisodeStatusActive, []string{"用分隔符切分"}, []string{"半包还没写"})
	newCard("new", owner, 10, model.EpisodeStatusActive, []string{"改回固定头部"}, nil)
	newCard("legacy", owner, 50, model.EpisodeStatusActive, []string{"令牌桶限流"}, nil)
	newCard("archived", owner, 60, model.EpisodeStatusArchived, []string{"归档结论"}, nil)
	newCard("stranger", stranger, 5, model.EpisodeStatusActive, []string{"别人的决定"}, nil)
	return out
}

func (f *fixture) assertionID(key string, kind model.AssertionKind, ordinal int) string {
	return model.DeterministicAssertionID(fmt.Sprintf("%s|%s|%s|%d", f.suffix, key, kind, ordinal))
}

func (f *fixture) governor(t *testing.T) *serviceassert.Governor {
	t.Helper()
	repository, err := NewRepository(f.db)
	if err != nil {
		t.Fatal(err)
	}
	governor, err := serviceassert.NewGovernor(repository)
	if err != nil {
		t.Fatal(err)
	}
	return governor
}

// 1-5: a real cross-session replacement, read back through the real chain.
func TestRealChainReplacementAndReadback(t *testing.T) {
	db := openTestDB(t)
	f := seed(t, db)
	governor := f.governor(t)
	ctx := context.Background()

	oldCard, newCard := f.seeded["old"], f.seeded["new"]
	if err := governor.MarkReplaced(ctx, serviceassert.ReplaceRequest{
		UserID:          f.user.ID,
		SourceEpisodeID: oldCard.ID, SourceAssertionID: f.assertionID("old", model.AssertionDecision, 1),
		TargetEpisodeID: newCard.ID, TargetAssertionID: f.assertionID("new", model.AssertionDecision, 1),
	}); err != nil {
		t.Fatalf("a legitimate replacement was refused: %v", err)
	}

	oldItems, err := governor.Resolve(ctx, f.user.ID, oldCard)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]model.Assertion{}
	for _, item := range oldItems {
		states[item.ID] = item
	}
	replaced := states[f.assertionID("old", model.AssertionDecision, 1)]
	if replaced.State != model.AssertionReplaced {
		t.Fatalf("the old decision reads as %s", replaced.State)
	}
	if replaced.Text == "" || !replaced.State.CanEstablishHistory() {
		t.Fatal("a replaced decision must stay readable as history")
	}
	if replaced.State.CanEstablishPresent() {
		t.Fatal("a replaced decision must not establish the present")
	}
	// The open loop on the same card keeps its own state.
	openLoop := states[f.assertionID("old", model.AssertionOpenLoop, 1)]
	if openLoop.State != model.AssertionUnknown {
		t.Fatalf("the sibling open loop became %s", openLoop.State)
	}

	// Marking the new decision current is what lets a current-state question be
	// answered; nothing does that automatically.
	if err := governor.MarkCurrent(ctx, f.user.ID, newCard.ID,
		f.assertionID("new", model.AssertionDecision, 1), 0); err != nil {
		t.Fatal(err)
	}
	newItems, _ := governor.Resolve(ctx, f.user.ID, newCard)
	for _, item := range newItems {
		if item.ID == f.assertionID("new", model.AssertionDecision, 1) && !item.State.CanEstablishPresent() {
			t.Fatalf("the current decision reads as %s", item.State)
		}
	}
}

// 6: legacy rows with no governance read as unknown, never current.
func TestLegacyRowsReadAsUnknown(t *testing.T) {
	db := openTestDB(t)
	f := seed(t, db)
	items, err := f.governor(t).Resolve(context.Background(), f.user.ID, f.seeded["legacy"])
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.State != model.AssertionUnknown {
			t.Fatalf("%s read as %s", item.ID, item.State)
		}
	}
}

// 7: after an insert, a delete and a reorder, the governed relation still names
// the same fact.
func TestGovernanceSurvivesReprojection(t *testing.T) {
	db := openTestDB(t)
	f := seed(t, db)
	governor := f.governor(t)
	ctx := context.Background()
	oldCard, newCard := f.seeded["old"], f.seeded["new"]

	if err := governor.MarkReplaced(ctx, serviceassert.ReplaceRequest{
		UserID:          f.user.ID,
		SourceEpisodeID: oldCard.ID, SourceAssertionID: f.assertionID("old", model.AssertionDecision, 1),
		TargetEpisodeID: newCard.ID, TargetAssertionID: f.assertionID("new", model.AssertionDecision, 1),
	}); err != nil {
		t.Fatal(err)
	}
	governed := f.assertionID("old", model.AssertionDecision, 1)

	prior, err := governor.ResolveWithTimes(ctx, f.user.ID, oldCard)
	if err != nil {
		t.Fatal(err)
	}
	// The projector re-emits: a new decision first, the governed one reworded,
	// and the open loop dropped.
	reprojected := oldCard
	reprojected.Document.Decisions = []model.EpisodeEvidence{
		{Text: "先复述粘包成因", EvidenceMessageIDs: []uint{f.message["old:d1"]}},
		{Text: "当时改用分隔符来切分消息", EvidenceMessageIDs: []uint{f.message["old:d1"]}},
	}
	reprojected.Document.OpenLoops = nil

	// Two candidates now cite the same message and neither keeps its wording,
	// so the reconciler must refuse rather than guess.
	if _, err := model.ReconcileAssertionIdentities(prior, reprojected); err == nil {
		t.Fatal("an ambiguous reprojection was resolved by guessing")
	}

	// The unambiguous version: the new decision cites its own message.
	reprojected.Document.Decisions[0].EvidenceMessageIDs = []uint{f.message["old:o1"]}
	after, err := model.ReconcileAssertionIdentities(prior, reprojected)
	if err != nil {
		t.Fatalf("a resolvable reprojection was refused: %v", err)
	}
	if orphans := model.OrphanedGovernance([]string{governed}, after); len(orphans) != 0 {
		t.Fatalf("the governed assertion was orphaned: %v", orphans)
	}
	for _, item := range after {
		if item.ID == governed && item.Text != "当时改用分隔符来切分消息" {
			t.Fatalf("the governed identity now names %q", item.Text)
		}
	}
}

// 8: the optimistic version is enforced by the database.
func TestVersionConflictIsEnforcedByTheDatabase(t *testing.T) {
	db := openTestDB(t)
	f := seed(t, db)
	governor := f.governor(t)
	ctx := context.Background()
	newCard := f.seeded["new"]
	target := f.assertionID("new", model.AssertionDecision, 1)

	if err := governor.MarkCurrent(ctx, f.user.ID, newCard.ID, target, 0); err != nil {
		t.Fatal(err)
	}
	// A second writer that still believes there is no row.
	if err := governor.MarkCurrent(ctx, f.user.ID, newCard.ID, target, 0); !errors.Is(err, serviceassert.ErrVersionConflict) {
		t.Fatalf("a stale insert was accepted: %v", err)
	}
	// A writer with a stale version.
	if err := governor.MarkCurrent(ctx, f.user.ID, newCard.ID, target, 5); !errors.Is(err, serviceassert.ErrVersionConflict) {
		t.Fatalf("a stale update was accepted: %v", err)
	}
	if err := governor.MarkCurrent(ctx, f.user.ID, newCard.ID, target, 1); err != nil {
		t.Fatalf("a correctly versioned update was refused: %v", err)
	}
}

// 10: cross-user, archived and unreadable cards stay refused through this door
// too, so governance cannot become a second way in.
func TestUnreadableCardsAreRefused(t *testing.T) {
	db := openTestDB(t)
	f := seed(t, db)
	governor := f.governor(t)
	ctx := context.Background()
	oldCard := f.seeded["old"]

	for name, request := range map[string]serviceassert.ReplaceRequest{
		"cross user": {UserID: f.user.ID,
			SourceEpisodeID: oldCard.ID, SourceAssertionID: f.assertionID("old", model.AssertionDecision, 1),
			TargetEpisodeID: f.seeded["stranger"].ID, TargetAssertionID: f.assertionID("stranger", model.AssertionDecision, 1)},
		"archived target": {UserID: f.user.ID,
			SourceEpisodeID: oldCard.ID, SourceAssertionID: f.assertionID("old", model.AssertionDecision, 1),
			TargetEpisodeID: f.seeded["archived"].ID, TargetAssertionID: f.assertionID("archived", model.AssertionDecision, 1)},
		"missing card": {UserID: f.user.ID,
			SourceEpisodeID: oldCard.ID, SourceAssertionID: f.assertionID("old", model.AssertionDecision, 1),
			TargetEpisodeID: uuid.NewString(), TargetAssertionID: f.assertionID("new", model.AssertionDecision, 1)},
	} {
		if err := governor.MarkReplaced(ctx, request); err == nil {
			t.Errorf("%s: the relation was accepted", name)
		}
	}

	// And the other learner cannot read this learner's governance.
	repository, _ := NewRepository(db)
	states, err := repository.LoadStates(ctx, f.other.ID,
		[]string{f.assertionID("old", model.AssertionDecision, 1)})
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 0 {
		t.Fatalf("another learner read %d governance rows", len(states))
	}
}

// Assertions are dated from their own messages, so two decisions written an
// hour apart inside one session are orderable.
func TestAssertionTimesComeFromTheirOwnMessages(t *testing.T) {
	db := openTestDB(t)
	f := seed(t, db)
	repository, _ := NewRepository(db)
	ctx := context.Background()

	times, err := repository.MessageTimes(ctx, f.user.ID,
		[]uint{f.message["old:d1"], f.message["old:o1"]})
	if err != nil {
		t.Fatal(err)
	}
	if len(times) != 2 {
		t.Fatalf("dated %d of 2 messages", len(times))
	}
	// A stranger's message is not datable by this learner.
	strangerTimes, err := repository.MessageTimes(ctx, f.user.ID, []uint{f.message["stranger:d1"]})
	if err != nil {
		t.Fatal(err)
	}
	if len(strangerTimes) != 0 {
		t.Fatal("another learner's message was dated")
	}
}

// Two writers racing to create the first row: exactly one must win, and the
// loser must be told it lost rather than getting a raw driver error.
func TestConcurrentFirstInsertHasExactlyOneWinner(t *testing.T) {
	db := openTestDB(t)
	f := seed(t, db)
	repository, _ := NewRepository(db)
	ctx := context.Background()
	target := f.assertionID("new", model.AssertionDecision, 1)

	state := model.EpisodeAssertionState{
		AssertionID: target, UserID: f.user.ID, EpisodeID: f.seeded["new"].ID,
		Kind: model.AssertionDecision, State: model.AssertionCurrent,
		OccurredAt: time.Now().UTC(), Version: 1,
	}
	results := make(chan error, 2)
	for attempt := 0; attempt < 2; attempt++ {
		go func() { results <- repository.SaveState(ctx, state, 0) }()
	}
	winners, conflicts := 0, 0
	for attempt := 0; attempt < 2; attempt++ {
		switch err := <-results; {
		case err == nil:
			winners++
		case errors.Is(err, serviceassert.ErrVersionConflict):
			conflicts++
		default:
			t.Fatalf("the losing writer got an unclassifiable error: %v", err)
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("winners=%d conflicts=%d, want exactly one of each", winners, conflicts)
	}
}

// A card whose version moved on invalidates a reference issued against the old
// one, so an answer cannot cite text that has since been rewritten.
func TestEpisodeVersionChangeInvalidatesIssuedReferences(t *testing.T) {
	db := openTestDB(t)
	f := seed(t, db)
	governor := f.governor(t)
	ctx := context.Background()

	governed, err := governor.ExpandCandidates(ctx, f.user.ID, []string{f.seeded["new"].ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(governed.Cards) == 0 {
		t.Fatal("nothing was expanded")
	}
	label := governed.Cards[0].Slots[0].Label
	answer := "某个说法 [" + label + "]。"

	fresh := serviceassert.ValidateAnswer(answer, governed.Registry, nil, governed.Versions, true)
	if len(fresh.VersionMismatch) != 0 {
		t.Fatalf("a current reference was rejected: %+v", fresh)
	}
	// The card is edited after the reference was issued.
	moved := serviceassert.CurrentEpisodeVersions{f.seeded["new"].ID: 99}
	stale := serviceassert.ValidateAnswer(answer, governed.Registry, nil, moved, true)
	if len(stale.VersionMismatch) != 1 {
		t.Fatalf("a reference to a moved card was accepted: %+v", stale)
	}
}

// ExpandCandidates must skip a card the learner may no longer read, without
// blanking the results the rest of the search could support.
func TestExpandCandidatesSkipsUnreadableCards(t *testing.T) {
	db := openTestDB(t)
	f := seed(t, db)
	governor := f.governor(t)
	governed, err := governor.ExpandCandidates(context.Background(), f.user.ID,
		[]string{f.seeded["new"].ID, f.seeded["archived"].ID, f.seeded["stranger"].ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(governed.Cards) != 1 {
		t.Fatalf("expanded %d cards; only the readable one should survive", len(governed.Cards))
	}
	if governed.Cards[0].Title != "new" {
		t.Fatalf("the surviving card is %q", governed.Cards[0].Title)
	}
}

// STABILIZE-058 v1.3: what a re-projection must not lose, read from the real
// tables.
//
// The projector's fail-closed rule is only as good as this query. The half that
// is easy to get wrong is the second one: an assertion can carry no row of its
// own and still be the thing three other decisions were replaced by, and
// deleting it would leave all three relations pointing at nothing.
func TestGovernedAssertionsCoversStatesAndReplacementTargets(t *testing.T) {
	db := openTestDB(t)
	f := seed(t, db)
	repository, err := NewRepository(db)
	if err != nil {
		t.Fatal(err)
	}
	governor := f.governor(t)
	ctx := context.Background()

	oldCard, newCard, legacy := f.seeded["old"], f.seeded["new"], f.seeded["legacy"]
	oldDecision := f.assertionID("old", model.AssertionDecision, 1)
	newDecision := f.assertionID("new", model.AssertionDecision, 1)
	if err := governor.MarkReplaced(ctx, serviceassert.ReplaceRequest{
		UserID:          f.user.ID,
		SourceEpisodeID: oldCard.ID, SourceAssertionID: oldDecision,
		TargetEpisodeID: newCard.ID, TargetAssertionID: newDecision,
	}); err != nil {
		t.Fatal(err)
	}

	// The replaced assertion's own card.
	governed, err := repository.GovernedAssertions(ctx, f.user.ID, oldCard.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := governed[oldDecision]; !ok {
		t.Fatal("the replaced decision is not reported as governed")
	}
	if _, ok := governed[f.assertionID("old", model.AssertionOpenLoop, 1)]; ok {
		t.Fatal("an ungoverned sibling was reported as governed")
	}

	// The card that has no row of its own and is a replacement target.
	target, err := repository.GovernedAssertions(ctx, f.user.ID, newCard.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := target[newDecision]; !ok {
		t.Fatal("a replacement target with no state row of its own was missed")
	}

	// A card nothing points at is free to change.
	untouched, err := repository.GovernedAssertions(ctx, f.user.ID, legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(untouched) != 0 {
		t.Fatalf("an ungoverned card reported %d governed assertions", len(untouched))
	}

	// An unknown row records the absence of a relation. Protecting it would
	// freeze every legacy card against ever being re-derived.
	if err := db.Create(&model.EpisodeAssertionState{
		AssertionID: f.assertionID("legacy", model.AssertionDecision, 1), UserID: f.user.ID,
		EpisodeID: legacy.ID, Kind: model.AssertionDecision, State: model.AssertionUnknown,
		OccurredAt: time.Now().UTC(), Version: 1}).Error; err != nil {
		t.Fatal(err)
	}
	stillFree, err := repository.GovernedAssertions(ctx, f.user.ID, legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stillFree) != 0 {
		t.Fatalf("an unknown row was treated as governance: %v", stillFree)
	}

	// And another learner's relations cannot govern this one's card.
	foreign, err := repository.GovernedAssertions(ctx, f.other.ID, oldCard.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(foreign) != 0 {
		t.Fatal("a stranger could read this card's governance")
	}
}
