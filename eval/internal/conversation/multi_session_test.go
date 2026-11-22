package conversation

import (
	"context"
	"os"
	"testing"
	"time"

	"GopherAI/model"
	agent "GopherAI/service/agent"
	"gorm.io/gorm"
)

// multiStubGenerator answers the summary call without a model.
type multiStubGenerator struct{}

func (g *multiStubGenerator) GenerateSummary(context.Context, string, []model.Message) (agent.SummaryGeneration, error) {
	return agent.SummaryGeneration{Content: `{"schema_version":2,"current_goal":"multi conversation preparation"}`}, nil
}

// The attempt's conversations all sit in one database, and the retrieval has
// to choose between them.
//
// This is the test that says so: several conversations are warmed, all of
// them are captured, and all of them are restored into the database the
// question is asked in. An attempt whose database held only the conversation
// the question is about would measure nothing about the choosing, and this
// test is what would notice.
func TestSeveralConversationsShareOneDatabase(t *testing.T) {
	if os.Getenv("GL_SANDBOX_TEST") != "1" {
		t.Skip("requires a local Docker daemon and preloaded MySQL image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	s, err := NewSandbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		closeCtx, c := context.WithTimeout(context.Background(), 15*time.Second)
		defer c()
		if err := s.Close(closeCtx); err != nil {
			t.Error(err)
		}
	}()
	if err = s.InitProductSchema(ctx); err != nil {
		t.Fatal(err)
	}
	hash := "not-a-real-hash"
	if err = s.SeedProduct(ctx, model.User{ID: 7, Username: "multi-learner", Password: hash}, nil, ""); err != nil {
		t.Fatal(err)
	}
	// The gate asks for as many conversations as the attempt carries.
	for _, id := range []string{"multi-a", "multi-b", "multi-c"} {
		if err = (guardedStore{s}).with(ctx, func(tx *gorm.DB) error {
			return tx.Create(&model.Session{ID: id, UserID: 7, UserName: "multi-learner",
				Mode: model.SessionModeAgent, Title: "Evaluation conversation"}).Error
		}); err != nil {
			t.Fatal(err)
		}
	}
	histories := []History{
		{SessionID: "multi-a", Turns: []Turn{testTurn(0)}},
		{SessionID: "multi-b", Turns: []Turn{testTurn(1)}},
		{SessionID: "multi-c", Turns: []Turn{testTurn(2)}},
	}
	prepared, err := s.PrepareMany(ctx, 7, "multi-learner", histories, &multiStubGenerator{}, agent.DefaultSummaryConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared) == 0 {
		t.Fatal("nothing was prepared")
	}
	snapshot, err := s.CaptureMany(ctx, 7, []string{"multi-a", "multi-b", "multi-c"})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Sessions) != 3 {
		t.Fatalf("captured %d conversation(s)", len(snapshot.Sessions))
	}
	// The restored database holds every conversation of the attempt.
	target, err := NewSandbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		closeCtx, c := context.WithTimeout(context.Background(), 15*time.Second)
		defer c()
		if err := target.Close(closeCtx); err != nil {
			t.Error(err)
		}
	}()
	if err = target.InitProductSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err = target.SeedProduct(ctx, model.User{ID: 7, Username: "multi-learner", Password: hash}, nil, ""); err != nil {
		t.Fatal(err)
	}
	if err = target.Restore(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	var sessions int64
	if err = target.db.Model(&model.Session{}).Where("user_id = ?", 7).Count(&sessions).Error; err != nil {
		t.Fatal(err)
	}
	if sessions != 3 {
		t.Fatalf("the restored database holds %d conversation(s), want 3", sessions)
	}
	var messages int64
	if err = target.db.Model(&model.Message{}).Where("user_name = ?", "multi-learner").Count(&messages).Error; err != nil {
		t.Fatal(err)
	}
	if messages == 0 {
		t.Fatal("the restored database holds no message")
	}
}
