package isolated

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	episodeDAO "GopherAI/dao/episode"
	"GopherAI/dao/message"
	sessionDAO "GopherAI/dao/session"
	"GopherAI/internal/testdb"
	"GopherAI/model"
	agentService "GopherAI/service/agent"
	agentTool "GopherAI/service/agent/tool"
	episodeService "GopherAI/service/episode"

	"github.com/google/uuid"
	redisCli "github.com/redis/go-redis/v9"
	mysqldriver "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// The cross-session lock, against a real database and a real Redis.
//
// The two cases are the ones the learner's hand is on:
//
//   - A target that never made it into the candidate list is still reachable
//     from their own list, and it opens.
//   - A clue they wrote reaches the target and locks it.
//
// The first one is the case a candidate list cannot cover: the target has no
// thin card, so no search could ever have offered it. It is the learner's own
// conversation list that reaches it, and the server that locks it.

// lockWorld is one test's world: a real MySQL, a real Redis, and the
// product's own cross-session parts.
type lockWorld struct {
	db         *gorm.DB
	client     *redisCli.Client
	index      *episodeDAO.RedisThinCardIndex
	repository *episodeDAO.Repository
	retriever  *episodeService.ThinCardRetriever
	reader     *agentService.DatabaseCrossSessionReader
	locator    agentService.CrossSessionLocator
	user       model.User
	suffix     string
	current    model.Session
}

func newLockWorld(t *testing.T) *lockWorld {
	t.Helper()
	address := os.Getenv("GOPHERAI_REDIS_TEST_ADDR")
	if address == "" {
		t.Skip("set GOPHERAI_REDIS_TEST_ADDR to run the cross session lock integration tests")
	}
	dsn := testdb.RequireMySQLDSN(t)
	db, err := gorm.Open(mysqldriver.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent), TranslateError: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(&model.User{}, &model.Session{}, &model.Message{},
		&model.EpisodeMemory{}, &model.SessionSummarySegment{}); err != nil {
		t.Fatal(err)
	}
	// The protocol is RESP2 on purpose: the index's own inspection fails on
	// RESP3, and an index that cannot be inspected cannot be verified.
	client := redisCli.NewClient(&redisCli.Options{
		Addr: address, Password: os.Getenv("GOPHERAI_REDIS_TEST_PASSWORD"), Protocol: 2})
	t.Cleanup(func() { _ = client.Close() })
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	indexName := "gopherai_thin_lock_it_" + suffix
	keyPrefix := "gopherai:thin_lock:it:" + suffix + ":"
	index, err := episodeDAO.NewRedisThinCardIndex(client, indexName, keyPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if err = index.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("ensure the thin card index: %v", err)
	}
	t.Cleanup(func() { dropLockIndex(client, indexName, keyPrefix) })
	repository := episodeDAO.NewRepository(db)
	retriever, err := episodeService.NewThinCardRetriever(index,
		episodeDAO.ThinCardLoader{Repository: repository}, 5)
	if err != nil {
		t.Fatal(err)
	}
	store, err := agentService.NewDatabaseCrossSessionStore(
		sessionDAO.NewRepository(db), message.NewRepository(db))
	if err != nil {
		t.Fatal(err)
	}
	reader, err := agentService.NewDatabaseCrossSessionReader(store)
	if err != nil {
		t.Fatal(err)
	}
	user := model.User{Username: "lock-" + suffix[:8], Password: "secret"}
	if err = db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	current := model.Session{ID: uuid.NewString(), UserID: user.ID, UserName: user.Username,
		Mode: model.SessionModeAgent, Title: "现在这次"}
	if err = db.Create(&current).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Unscoped().Where("user_id = ?", user.ID).Delete(&model.EpisodeMemory{})
		db.Unscoped().Where("user_id = ?", user.ID).Delete(&model.SessionSummarySegment{})
		db.Unscoped().Where("session_id = ?", current.ID).Delete(&model.Message{})
		db.Unscoped().Where("user_id = ?", user.ID).Delete(&model.Session{})
		db.Unscoped().Where("id = ?", user.ID).Delete(&model.User{})
	})
	return &lockWorld{
		db: db, client: client, index: index, repository: repository, suffix: suffix,
		retriever: retriever, reader: reader, user: user, current: current,
		locator: agentService.CrossSessionLocatorFunc(
			func(ctx context.Context, userID int64, currentSession, clue string) ([]agentService.SessionCandidate, error) {
				found, recallErr := retriever.Recall(ctx, userID, currentSession,
					agentTool.ThinCardQueryFor(clue, clue))
				if recallErr != nil {
					return nil, recallErr
				}
				out := make([]agentService.SessionCandidate, 0, len(found))
				for _, candidate := range found {
					out = append(out, agentService.SessionCandidate{
						SessionID:    candidate.SessionID,
						Title:        candidate.Title,
						Topics:       append([]string{}, candidate.Topics...),
						LastTalkedAt: candidate.LastTalkedAt,
					})
				}
				return out, nil
			}),
	}
}

// dropLockIndex removes the test's own index and its keys.
func dropLockIndex(client *redisCli.Client, indexName, keyPrefix string) {
	ctx := context.Background()
	_ = client.Do(ctx, "FT.DROPINDEX", indexName).Err()
	var cursor uint64
	for {
		keys, next, err := client.Scan(ctx, cursor, keyPrefix+"*", 100).Result()
		if err != nil {
			return
		}
		if len(keys) > 0 {
			_ = client.Del(ctx, keys...).Err()
		}
		cursor = next
		if cursor == 0 {
			return
		}
	}
}

// target is the id of one of the test's earlier conversations.
//
// It carries the run's own suffix, because the shared test database holds the
// rows of every run that ever used it, and a fixed id would collide with the
// row a previous run left behind.
func (w *lockWorld) target(name string) string {
	return "l4r2-" + w.suffix[:8] + "-" + name
}

// writeEarlier writes an earlier conversation with a summary of its own.
func (w *lockWorld) writeEarlier(t *testing.T, sessionID, title, topic, point string) {
	t.Helper()
	earlier := model.Session{ID: sessionID, UserID: w.user.ID, UserName: w.user.Username,
		Mode: model.SessionModeAgent, Title: title, CreatedAt: time.Now().UTC().Add(-time.Hour),
		UpdatedAt: time.Now().UTC().Add(-time.Hour)}
	if err := w.db.Create(&earlier).Error; err != nil {
		t.Fatalf("write the earlier session: %v", err)
	}
	content, err := json.Marshal(model.SessionSummarySegmentContent{
		SchemaVersion: model.SessionSummarySegmentSchemaV1,
		Topic:         topic,
		Points: []model.SessionSummarySegmentPoint{
			{Text: point, EvidenceMessageIDs: []uint{1}},
		},
	})
	if err != nil {
		t.Fatalf("encode the segment: %v", err)
	}
	segment := model.SessionSummarySegment{
		UserID: w.user.ID, SessionID: sessionID, Seq: 1, StartMessageID: 1, EndMessageID: 2,
		StartAt: time.Now().UTC().Add(-time.Hour), EndAt: time.Now().UTC().Add(-time.Hour),
		Content: string(content), WhiteboardChanges: "null",
		IndexStatus: model.SessionSummarySegmentIndexPending, TokenEstimate: 40,
	}
	if err := w.db.Create(&segment).Error; err != nil {
		t.Fatalf("write the segment: %v", err)
	}
}

// writeCard writes a thin card for a session and puts it into the index.
//
// It is the product's own writer: the row through the card repository and the
// index through the card index.
func (w *lockWorld) writeCard(t *testing.T, sessionID, title, topic string) {
	t.Helper()
	document := model.SessionCardDocument{
		Version: model.SessionThinCardContentVersion,
		Title:   title,
		Topics:  []model.EpisodeTopic{{Key: topic, Label: topic}},
	}
	card := &model.EpisodeMemory{
		ID: uuid.NewString(), UserID: w.user.ID, SessionID: sessionID,
		ContentKind: model.EpisodeContentSessionCard, Title: title,
		Card: document, SearchText: episodeService.ThinCardSearchText(document),
		TopicKeys:           document.CanonicalTopicKeys(),
		SourceLastMessageID: 1, LastMessageAt: time.Now().UTC().Add(-time.Hour),
		Status: model.EpisodeStatusActive, IndexStatus: model.EpisodeIndexPending,
	}
	if err := w.repository.Save(context.Background(), card, 0); err != nil {
		t.Fatalf("write the card: %v", err)
	}
	if err := w.index.Store(context.Background(), *card); err != nil {
		t.Fatalf("write the card into the index: %v", err)
	}
	// The card is indexed now, and the row says so: a card whose row still
	// says pending is not a candidate, because the index does not hold it.
	status := episodeDAO.CardIndexSource{Repository: w.repository}
	if err := status.UpdateIndexStatus(context.Background(), w.user.ID, card.ID, card.Version,
		model.EpisodeIndexIndexed); err != nil {
		t.Fatalf("mark the card indexed: %v", err)
	}
}

// A target that never made it into the candidate list is still reachable.
//
// The target has no thin card, so no search could ever have offered it: the
// candidate list is what one lightweight search found, and the learner's own
// list is the ground truth. The pick arrives as the target's id, and the
// server locks it and reads its record.
func TestATargetThatMissedTheCandidatesIsStillReachable(t *testing.T) {
	w := newLockWorld(t)
	w.writeEarlier(t, w.target("a1-03"), "洄漪", "三道题", "第二道练的是读写拆分")

	// The card index does not hold the target: it has no card at all.
	if _, found, err := w.retriever.CardOfSession(context.Background(), w.user.ID, w.target("a1-03")); err != nil {
		t.Fatalf("look the target up: %v", err)
	} else if found {
		t.Fatal("the target is in the candidate list: the case under test is not the case under test")
	}

	clarification := &model.SessionClarification{
		ClarificationID: "clarification-1",
		Question:        "上次你给我排的那三道题里，第二道练的是什么？",
		AllowFreeText:   true, AllowHistoryBrowse: true,
	}
	resolution := agentService.ResolveSessionLock(context.Background(), w.locator, w.reader,
		w.user.ID, w.current.ID, clarification, w.target("a1-03"))
	if !resolution.Resolved() {
		t.Fatalf("the pick locked nothing: %+v", resolution)
	}
	if resolution.Locked.SessionID != w.target("a1-03") {
		t.Fatalf("the lock is %q", resolution.Locked.SessionID)
	}
	if !strings.Contains(resolution.Locked.Evidence, "读写拆分") {
		t.Fatalf("the lock carries no evidence: %q", resolution.Locked.Evidence)
	}
}

// A clue the learner wrote reaches the target.
//
// The clue is read by the card index - one lightweight lookup, no model call -
// and a clue that names one conversation locks it.
func TestAClueTheLearnerWroteReachesTheTarget(t *testing.T) {
	w := newLockWorld(t)
	w.writeEarlier(t, w.target("b2-07"), "浔漳", "缓存池", "缓存池的最大连接数是 100")
	w.writeCard(t, w.target("b2-07"), "浔漳", "缓存池")

	clarification := &model.SessionClarification{
		ClarificationID: "clarification-1",
		Question:        "上次那个缓存池的最大连接数是多少？",
		AllowFreeText:   true, AllowHistoryBrowse: true,
	}
	resolution := agentService.ResolveSessionLock(context.Background(), w.locator, w.reader,
		w.user.ID, w.current.ID, clarification, "缓存池")
	if !resolution.Resolved() {
		t.Fatalf("the clue locked nothing: %+v", resolution)
	}
	if resolution.Locked.SessionID != w.target("b2-07") {
		t.Fatalf("the lock is %q, want the conversation the clue named", resolution.Locked.SessionID)
	}
	if !strings.Contains(resolution.Locked.Evidence, "100") {
		t.Fatalf("the lock carries no evidence: %q", resolution.Locked.Evidence)
	}
}

// A conversation of somebody else is refused, even when the index holds it.
//
// The index is the learner's own, and a card row is the learner's own; the
// ownership check is what says so, and it is asked before anything is read.
func TestAConversationOfSomebodyElseIsRefused(t *testing.T) {
	w := newLockWorld(t)
	other := model.User{Username: "lock-other", Password: "secret"}
	if err := w.db.Create(&other).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.db.Unscoped().Where("id = ?", other.ID).Delete(&model.User{}) })
	foreign := model.Session{ID: "session-foreign", UserID: other.ID, UserName: other.Username,
		Mode: model.SessionModeAgent, Title: "别人的会话"}
	if err := w.db.Create(&foreign).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.db.Unscoped().Where("id = ?", foreign.ID).Delete(&model.Session{}) })

	clarification := &model.SessionClarification{
		ClarificationID: "clarification-1",
		Question:        "上次你给我排的那三道题里，第二道练的是什么？",
		AllowFreeText:   true, AllowHistoryBrowse: true,
	}
	resolution := agentService.ResolveSessionLock(context.Background(), w.locator, w.reader,
		w.user.ID, w.current.ID, clarification, "session-foreign")
	if resolution.Resolved() {
		t.Fatalf("a conversation of somebody else was locked: %+v", resolution.Locked)
	}
}
