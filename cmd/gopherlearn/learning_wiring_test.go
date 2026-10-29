package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/app"
	"github.com/xiaopang456/GopherLearn/internal/devmodel"
	"github.com/xiaopang456/GopherLearn/internal/domain"
	"github.com/xiaopang456/GopherLearn/internal/knowledge"
	"github.com/xiaopang456/GopherLearn/internal/model"
	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/service"
	"github.com/xiaopang456/GopherLearn/internal/storage"
)

// buildLearningService is the one startup decision that turns
// GOPHERLEARN_LOCAL_LEARNING_MODEL into a quiz a browser can drive, so the three ways it
// can come out are pinned here instead of being read off main.

// refusedChat is a provider client reporting one distinctive failure, which is how a test
// can see which client the workflow was built over without reaching into a service for a
// field it does not expose.
type refusedChat struct{ message string }

func (c refusedChat) Complete(context.Context, model.Request) (model.Completion, error) {
	return model.Completion{}, errors.New(c.message)
}

func (c refusedChat) Stream(_ context.Context, _ model.Request, _ model.DeltaHandler) (model.Completion, error) {
	return model.Completion{}, errors.New(c.message)
}

// quizFixture is a process's worth of stores: one learner, one learning-agent
// conversation, and notes long enough to survive the evidence rules. A quiz asked to
// generate therefore reaches its model instead of stopping earlier for want of material.
func quizFixture(t *testing.T) (storage.Stores, *service.KnowledgeService, string, string) {
	t.Helper()
	ctx := context.Background()
	repo := repository.NewInMemoryRepository()
	user, err := repo.CreateUser(ctx, &domain.User{Email: "local@example.test", DisplayName: "local",
		PasswordHash: "hash-not-used"})
	if err != nil {
		t.Fatal(err)
	}
	session, err := repo.CreateSession(ctx, &domain.Session{UserID: user.ID, Title: "本地联调",
		Mode: domain.SessionModeLearningAgent})
	if err != nil {
		t.Fatal(err)
	}
	store := knowledge.NewMemoryStore()
	ingest := service.NewKnowledgeService(store)
	collection, err := store.CreateCollection(ctx, &domain.Collection{UserID: user.ID, Title: "Redis 手册"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.AddDocument(ctx, user.ID, collection.ID, service.DocumentInput{
		Title: "persistence.md", Format: string(domain.DocumentFormatMarkdown),
		Content: "## RDB 快照与它的代价\n\n" +
			"RDB 把某一时刻的全部数据写成一份压缩的二进制文件，恢复时 Redis 直接把这文件读回内存，所以冷启动很快，" +
			"也适合拿去做异地备份和全量存档。代价是两次快照之间的写入可能在崩溃时全部丢失，因为 Redis 只能按配置的间隔触发快照，" +
			"间隔越长丢得越多；而 fork 出来做快照时的写时复制，在大数据集上还会带来可感知的抖动。\n\n" +
			"## AOF 追加日志为什么丢得更少\n\n" +
			"AOF 把每条改变数据的命令按协议追加到日志末尾，重启时顺序重放这份日志来重建数据。追加是顺序写，并且可以按策略尽快落盘，" +
			"所以它的持久性比快照好得多；代价是文件通常比同等的 RDB 大不少，重放也比读回一份二进制慢，而且日志会一直增长，" +
			"需要按设定比例触发重写，用当前内存状态生成一份新日志替掉旧的。",
	}); err != nil {
		t.Fatal(err)
	}
	return storage.Stores{Repository: repo, Knowledge: store}, ingest, user.ID, session.ID
}

func TestEchoWithoutTheLocalSwitchOffersNoQuizWorkflow(t *testing.T) {
	stores, titles, userID, sessionID := quizFixture(t)
	config, err := app.ConfigFromEnvironment(env(map[string]string{}))
	if err != nil {
		t.Fatal(err)
	}
	assembly, err := app.Assemble(stores.Knowledge, config, app.ProductionOptions())
	if err != nil {
		t.Fatal(err)
	}
	if built := buildLearningService(assembly, stores, stores.Knowledge, titles, nil); built != nil {
		t.Fatal("a keyless echo build got a quiz workflow, so the routes would describe a quiz nobody generated")
	}
	// The same absence one layer down: with no model at all the workflow refuses rather
	// than inventing a quiz. This is why returning nil above is a feature being off, not
	// a route being broken.
	bare := service.NewLearningService(stores.Repository, nil, app.NewRetrievalPipeline(stores.Knowledge), titles, titles)
	if _, err := bare.Start(context.Background(), userID, sessionID, "Redis 持久化", nil); !errors.Is(err, service.ErrLearningModelNotConfigured) {
		t.Fatalf("start with no model = %v, want the wiring refusal", err)
	}
}

func TestLocalSwitchBuildsTheQuizWorkflowInEchoMode(t *testing.T) {
	stores, titles, userID, sessionID := quizFixture(t)
	config, err := app.ConfigFromEnvironment(env(map[string]string{
		app.EnvLocalLearningModel: devmodel.LearningModelName,
	}))
	if err != nil {
		t.Fatal(err)
	}
	assembly, err := app.Assemble(stores.Knowledge, config, app.ProductionOptions())
	if err != nil {
		t.Fatal(err)
	}
	quiz := buildLearningService(assembly, stores, stores.Knowledge, titles, nil)
	if quiz == nil {
		t.Fatal("the local switch produced no quiz workflow")
	}
	view, err := quiz.Start(context.Background(), userID, sessionID, "Redis 持久化", nil)
	if err != nil {
		t.Fatalf("the scripted workflow could not start on real notes: %v", err)
	}
	if view.Question == nil || view.TotalQuestions != domain.LearningQuestionCount {
		t.Fatalf("start returned no first card: %+v", view)
	}
}

// TestConfiguredProviderWinsOverTheLocalScript is the precedence rule. The configuration
// gate makes the two impossible together; this covers an assembly built by hand, and it
// checks the direction that matters - a process holding a real provider must never
// quietly answer a quiz from a script.
func TestConfiguredProviderWinsOverTheLocalScript(t *testing.T) {
	stores, titles, userID, sessionID := quizFixture(t)
	quiz := buildLearningService(app.Assembly{
		Model:         refusedChat{message: "provider-dialled"},
		LearningModel: devmodel.NewLearning(),
	}, stores, stores.Knowledge, titles, nil)
	if quiz == nil {
		t.Fatal("no workflow was built although a provider client was present")
	}
	_, err := quiz.Start(context.Background(), userID, sessionID, "Redis 持久化", nil)
	if !errors.Is(err, service.ErrLearningGeneration) || !strings.Contains(err.Error(), "provider-dialled") {
		t.Fatalf("error = %v, want the provider client's own failure rather than the script's", err)
	}
}
