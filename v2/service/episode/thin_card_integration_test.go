package episode

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	episodedao "GopherAI/dao/episode"
	"GopherAI/internal/testdb"
	"GopherAI/model"

	"github.com/google/uuid"
	redisCli "github.com/redis/go-redis/v9"
	mysqldriver "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// The thin card's background path, against a real database and a real Redis.
//
// The three claims below are the ones the whole card arm rests on, and none
// of them can be shown with a fake: a card that is written to the store and
// not to the index, a card whose index write failed and never recovers, and a
// card that does not learn about a summary that landed.
//
// They run the product's own parts - the same loader, the same projector, the
// same indexer, the same index and the same retriever - so what is measured
// here is the deployment's behaviour and not a stand-in's.
//
// They cost nothing: the thin card is derived by the program, so no model is
// asked for anything here.

// thinCardWorld is one test's whole world: a real MySQL, a real Redis and
// the product's own thin card parts.
type thinCardWorld struct {
	db         *gorm.DB
	client     *redisCli.Client
	index      *episodedao.RedisThinCardIndex
	repository *episodedao.Repository
	indexName  string
	keyPrefix  string
	user       model.User
	session    model.Session
}

func newThinCardWorld(t *testing.T) *thinCardWorld {
	t.Helper()
	address := os.Getenv("GOPHERAI_REDIS_TEST_ADDR")
	if address == "" {
		t.Skip("set GOPHERAI_REDIS_TEST_ADDR to run the thin card integration tests")
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
	indexName := "gopherai_thin_card_it_" + suffix
	keyPrefix := "gopherai:thin_card:it:" + suffix + ":"
	index, err := episodedao.NewRedisThinCardIndex(client, indexName, keyPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if err = index.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("ensure the thin card index: %v", err)
	}
	t.Cleanup(func() { dropThinCardIndex(client, indexName, keyPrefix) })
	user := model.User{Username: "thin-" + suffix[:8], Password: "secret"}
	if err = db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	session := model.Session{ID: uuid.NewString(), UserID: user.ID,
		UserName: user.Username, Mode: model.SessionModeAgent, Title: "缓存池的调参"}
	if err = db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Unscoped().Where("user_id = ?", user.ID).Delete(&model.EpisodeMemory{})
		db.Unscoped().Where("session_id = ?", session.ID).Delete(&model.SessionSummarySegment{})
		db.Unscoped().Where("session_id = ?", session.ID).Delete(&model.Message{})
		db.Unscoped().Where("id = ?", session.ID).Delete(&model.Session{})
		db.Unscoped().Where("id = ?", user.ID).Delete(&model.User{})
	})
	return &thinCardWorld{
		db: db, client: client, index: index,
		repository: episodedao.NewRepository(db),
		indexName:  indexName, keyPrefix: keyPrefix,
		user: user, session: session,
	}
}

// dropThinCardIndex removes the test's own index and its keys.
func dropThinCardIndex(client *redisCli.Client, indexName, keyPrefix string) {
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

// service builds the product's thin card service over this world.
func (w *thinCardWorld) service(t *testing.T, index *episodedao.RedisThinCardIndex) *ThinCardService {
	t.Helper()
	loader, err := NewDatabaseThinCardSourceLoader(w.db)
	if err != nil {
		t.Fatal(err)
	}
	projector, err := NewThinCardProjector(loader, ProgrammaticThinCardDeriver{}, w.repository)
	if err != nil {
		t.Fatal(err)
	}
	indexer, err := NewThinCardIndexer(index, episodedao.CardIndexSource{Repository: w.repository})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewThinCardService(projector, indexer)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// job builds the BACKGROUND path: the projector the worker drives.
func (w *thinCardWorld) job(t *testing.T, index *episodedao.RedisThinCardIndex) *ThinCardJobProjector {
	t.Helper()
	job, err := NewThinCardJobProjector(w.service(t, index))
	if err != nil {
		t.Fatal(err)
	}
	return job
}

// writeMessage writes one message of the session and returns its id.
func (w *thinCardWorld) writeMessage(t *testing.T, content string, isUser bool) uint {
	t.Helper()
	message := model.Message{SessionID: w.session.ID, UserName: w.user.Username,
		Content: content, IsUser: isUser}
	if err := w.db.Create(&message).Error; err != nil {
		t.Fatalf("write the message: %v", err)
	}
	return message.ID
}

// writeSegment writes one summary segment over the given turns.
func (w *thinCardWorld) writeSegment(t *testing.T, seq int, from, to uint, topic string, names []string) {
	t.Helper()
	content, err := json.Marshal(model.SessionSummarySegmentContent{
		SchemaVersion: model.SessionSummarySegmentSchemaV1,
		Topic:         topic,
		Points: []model.SessionSummarySegmentPoint{
			{Text: "缓存池的最大连接数是 100", EvidenceMessageIDs: []uint{from}},
		},
		Names: names,
	})
	if err != nil {
		t.Fatalf("encode the segment: %v", err)
	}
	segment := model.SessionSummarySegment{
		UserID: w.user.ID, SessionID: w.session.ID, Seq: seq,
		StartMessageID: from, EndMessageID: to,
		StartAt: time.Now().UTC(), EndAt: time.Now().UTC(),
		Content: string(content), WhiteboardChanges: "null",
		IndexStatus: model.SessionSummarySegmentIndexPending, TokenEstimate: 40,
	}
	if err := w.db.Create(&segment).Error; err != nil {
		t.Fatalf("write the segment: %v", err)
	}
}

// cardRow reads the session's card row back from the store.
func (w *thinCardWorld) cardRow(t *testing.T) *model.EpisodeMemory {
	t.Helper()
	row, err := w.repository.GetBySessionKind(context.Background(), w.user.ID,
		w.session.ID, model.EpisodeContentSessionCard)
	if err != nil {
		t.Fatalf("read the card row: %v", err)
	}
	return row
}

// foundByText says whether the index finds the card by the given words.
//
// The index may take a moment to catch up with a write, so the question is
// asked again for a short while before it is answered no.
func (w *thinCardWorld) foundByText(t *testing.T, cardID string, terms []string) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		results, err := w.index.SearchByText(context.Background(), w.user.ID, "", terms, 5)
		if err != nil {
			t.Fatalf("search the thin card index: %v", err)
		}
		for _, result := range results {
			if result.CardID == cardID {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// A card with no layered summary behind it is still findable.
//
// This is the base case of the whole arm: a session that has not been
// summarized yet still happened, and the card that says so has to be in the
// index. Before this, the background job wrote the card to the store and
// stopped - so the card existed, the row said it was indexed, and no search
// could find it.
func TestTheBackgroundJobWritesACardThatCanBeFound(t *testing.T) {
	w := newThinCardWorld(t)
	first := w.writeMessage(t, "我们今天聊聊连接池", true)
	last := w.writeMessage(t, "连接池的参数怎么调", false)

	card, err := w.job(t, w.index).Project(context.Background(), w.user.ID, w.session.ID, 0)
	if err != nil {
		t.Fatalf("the background job wrote no card: %v", err)
	}
	if card == nil || !card.Card.IsThin() {
		t.Fatalf("the job did not write a thin card: %+v", card)
	}
	if card.SourceLastMessageID != last {
		t.Fatalf("the card's watermark is %d, want the session's last message %d",
			card.SourceLastMessageID, last)
	}
	if len(card.Card.SegmentSeqs) != 0 || len(card.Card.Topics) != 0 {
		t.Fatalf("a session with no summary produced clues: %+v", card.Card)
	}
	row := w.cardRow(t)
	if row.IndexStatus != model.EpisodeIndexIndexed {
		t.Fatalf("the row's index status is %q, want indexed", row.IndexStatus)
	}
	// The index holds it, and a search finds it.
	if !w.foundByText(t, card.ID, []string{"缓存"}) {
		t.Fatal("the card is in the store and the index did not find it")
	}
	retriever, err := NewThinCardRetriever(w.index,
		episodedao.ThinCardLoader{Repository: w.repository}, 5)
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := retriever.Recall(context.Background(), w.user.ID, "another-session",
		ThinCardQuery{Terms: []string{"缓存"}, Temporal: TemporalNone})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	found := false
	for _, candidate := range candidates {
		if candidate.SessionID == w.session.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("the recall did not return the session: %+v", candidates)
	}
	_ = first
}

// A failed index write is diagnosable and the next attempt makes it good.
//
// The card is already in the store when the index write fails, so the retry
// finds a card whose words did not change. It has to write the index anyway:
// the row still says the index has not caught up with it, and a rule that
// only looked at the words would leave the card out of the index for good.
func TestAFailedIndexWriteIsRecoveredByTheNextAttempt(t *testing.T) {
	w := newThinCardWorld(t)
	w.writeMessage(t, "我们今天聊聊连接池", true)
	w.writeMessage(t, "连接池的参数怎么调", false)

	// The index that cannot be written to: a Redis that is not there.
	broken := redisCli.NewClient(&redisCli.Options{
		Addr: "127.0.0.1:1", Protocol: 2, MaxRetries: -1, DialTimeout: time.Second})
	t.Cleanup(func() { _ = broken.Close() })
	brokenIndex, err := episodedao.NewRedisThinCardIndex(broken, w.indexName, w.keyPrefix)
	if err != nil {
		t.Fatal(err)
	}

	_, err = w.job(t, brokenIndex).Project(context.Background(), w.user.ID, w.session.ID, 0)
	if err == nil {
		t.Fatal("a failed index write was reported as a success")
	}
	if !errors.Is(err, ErrThinCardIndex) {
		t.Fatalf("the error is %v, want it to name the index stage", err)
	}
	// The failure is on the row, not only in the log.
	failed := w.cardRow(t)
	if failed.IndexStatus != model.EpisodeIndexFailed {
		t.Fatalf("the row's index status is %q, want failed", failed.IndexStatus)
	}

	// The next attempt, with an index that works.
	card, err := w.job(t, w.index).Project(context.Background(), w.user.ID, w.session.ID, 0)
	if err != nil {
		t.Fatalf("the retry did not recover: %v", err)
	}
	if card.Version != failed.Version {
		t.Fatalf("the retry rewrote the card: version %d, want %d", card.Version, failed.Version)
	}
	recovered := w.cardRow(t)
	if recovered.IndexStatus != model.EpisodeIndexIndexed {
		t.Fatalf("the row's index status is %q, want indexed", recovered.IndexStatus)
	}
	if !w.foundByText(t, card.ID, []string{"缓存"}) {
		t.Fatal("the card was not written into the index by the retry")
	}
}

// A summary that lands after the card was written joins the card.
//
// The summary is of turns the card has already been told about, so the
// session's last message id does not move. The card's clues do move: the new
// segment's topic, its names and its number are what the card is for, and a
// card that refused the update would keep pointing at the session's older
// state.
func TestANewSummaryJoinsTheCardAtTheSameMessageWatermark(t *testing.T) {
	w := newThinCardWorld(t)
	first := w.writeMessage(t, "我该把读量提到多少", true)
	last := w.writeMessage(t, "每天读 30 页吧", false)

	card, err := w.job(t, w.index).Project(context.Background(), w.user.ID, w.session.ID, 0)
	if err != nil {
		t.Fatalf("the background job wrote no card: %v", err)
	}
	if len(card.Card.SegmentSeqs) != 0 {
		t.Fatalf("the card knows a segment before one was written: %+v", card.Card)
	}

	// The summary lands. The session does not move: the segment covers turns
	// the card has already been told about.
	w.writeSegment(t, 1, first, last, "缓存", []string{"韩梅梅"})

	updated, err := w.job(t, w.index).Project(context.Background(), w.user.ID, w.session.ID, 0)
	if err != nil {
		t.Fatalf("the card did not take the new summary: %v", err)
	}
	if updated.SourceLastMessageID != card.SourceLastMessageID {
		t.Fatalf("the card's watermark moved to %d, want it to stay at %d",
			updated.SourceLastMessageID, card.SourceLastMessageID)
	}
	if updated.Version != card.Version+1 {
		t.Fatalf("the card's version is %d, want %d", updated.Version, card.Version+1)
	}
	if len(updated.Card.SegmentSeqs) != 1 || updated.Card.SegmentSeqs[0] != 1 {
		t.Fatalf("the card's segment refs are %v, want the new one", updated.Card.SegmentSeqs)
	}
	foundTopic := false
	for _, topic := range updated.Card.Topics {
		if topic.Key == "缓存" {
			foundTopic = true
		}
	}
	if !foundTopic {
		t.Fatalf("the card's topics are %+v, want the new summary's topic", updated.Card.Topics)
	}
	foundName := false
	for _, name := range updated.Card.Names {
		if name == "韩梅梅" {
			foundName = true
		}
	}
	if !foundName {
		t.Fatalf("the card's names are %v, want the new summary's name", updated.Card.Names)
	}
	// The new clue is in the index too, under the topic it was filed under.
	results, err := w.index.SearchExactTopics(context.Background(), w.user.ID, "", []string{"缓存"}, 5)
	if err != nil {
		t.Fatalf("search the thin card topics: %v", err)
	}
	found := false
	for _, result := range results {
		if result.CardID == updated.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("the index did not find the card under its new topic: %+v", results)
	}
}
