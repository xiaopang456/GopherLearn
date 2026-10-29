package hybrid

import (
	"GopherAI/common/rag"
	dao "GopherAI/dao/knowledge"
	"GopherAI/entry"
	"GopherAI/model"
	"GopherAI/utils"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	redisclient "github.com/redis/go-redis/v9"
	"github.com/xiaopang456/GopherLearn/conversation"
	"gorm.io/gorm"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type knowledgeFaultChunks struct {
	testChunks
	indexFail   atomic.Bool
	cleanupFail atomic.Bool
}

func (f *knowledgeFaultChunks) Store(context.Context, []rag.EmbeddedChunk) error {
	if f.indexFail.Load() {
		return errors.New("offline index failure")
	}
	return nil
}
func (f *knowledgeFaultChunks) DeleteDocumentVersion(context.Context, int64, string, int) error {
	if f.cleanupFail.Load() {
		return errors.New("offline cleanup failure")
	}
	return nil
}
func knowledgeFixture(t *testing.T, scan int, chunks *knowledgeFaultChunks) (*Application, *gorm.DB, string, string, string) {
	t.Helper()
	db := sqliteDatabase(t)
	settings := testSettings(t)
	settings.Config.KnowledgeConfig.ScanIntervalSeconds = scan
	migrations := 0
	deps := dependencies(t, db, &scriptedChatModel{}, &chunks.testChunks, &migrations)
	deps.Chunks = func(_ *redisclient.Client, _ int) (entry.ChunkStore, error) { return chunks, nil }
	app, err := Assemble(context.Background(), settings, deps, &ordinaryModel{}, conversation.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := app.Close(); err != nil {
			t.Error(err)
		}
	})
	hash, err := utils.HashPassword("synthetic-password")
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []model.User{{ID: 1, Username: "learner", Email: "learner@invalid.example", Password: hash}, {ID: 2, Username: "other", Email: "other@invalid.example", Password: hash}} {
		if err := db.Create(&u).Error; err != nil {
			t.Fatal(err)
		}
	}
	return app, db, loginHTTP(t, app.Handler, "learner@invalid.example"), loginHTTP(t, app.Handler, "other@invalid.example"), settings.Config.KnowledgeConfig.StorageDir
}
func knowledgeRequest(t *testing.T, h http.Handler, verb, path, token, name, content string) (int, map[string]any) {
	t.Helper()
	var body bytes.Buffer
	kind := ""
	if name != "" {
		writer := multipart.NewWriter(&body)
		file, err := writer.CreateFormFile("file", name)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(file, content)
		writer.Close()
		kind = writer.FormDataContentType()
	}
	req := httptest.NewRequest(verb, path, &body)
	req.Header.Set("Authorization", "Bearer "+token)
	if kind != "" {
		req.Header.Set("Content-Type", kind)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	return w.Code, result
}
func docView(t *testing.T, data map[string]any) map[string]any {
	t.Helper()
	view, ok := data["document"].(map[string]any)
	if !ok {
		t.Fatalf("native document %v", data)
	}
	return view
}

func TestKnowledgeNativeHTTPSizeIsolationAndExactSource(t *testing.T) {
	app, db, token, other, storageRoot := knowledgeFixture(t, 3600, &knowledgeFaultChunks{})
	const path = "/api/v1/knowledge/documents"
	status, result := knowledgeRequest(t, app.Handler, "POST", path, token, "large.txt", strings.Repeat("x", 5<<20))
	if status != 201 || docView(t, result)["file_size"] != float64(5<<20) {
		t.Fatalf("5 MiB unreachable %d %v", status, result)
	}
	if status, _ := knowledgeRequest(t, app.Handler, "POST", path, token, "too-large.txt", strings.Repeat("x", (5<<20)+1)); status != 413 {
		t.Fatalf("over limit %d", status)
	}
	status, result = knowledgeRequest(t, app.Handler, "POST", path, token, "source.md", "# 原文\r\n正文\r\n")
	if status != 201 {
		t.Fatal(status, result)
	}
	id := docView(t, result)["id"].(string)
	item := path + "/" + id
	if status, _ := knowledgeRequest(t, app.Handler, "POST", path, token, "source.md", "changed"); status != 409 {
		t.Fatal("name duplicate", status)
	}
	if status, _ := knowledgeRequest(t, app.Handler, "POST", path, token, "copy.md", "# 原文\r\n正文\r\n"); status != 409 {
		t.Fatal("hash duplicate", status)
	}
	if status, _ := knowledgeRequest(t, app.Handler, "POST", path, other, "source.md", "# 原文\r\n正文\r\n"); status != 201 {
		t.Fatal("cross-user upload", status)
	}
	_, otherList := knowledgeRequest(t, app.Handler, "GET", path, other, "", "")
	if len(otherList["documents"].([]any)) != 1 {
		t.Fatal("list isolation", otherList)
	}
	for _, verb := range []string{"GET", "PUT", "DELETE"} {
		if status, _ := knowledgeRequest(t, app.Handler, verb, item, other, "source.md", "wrong"); status != 404 {
			t.Fatalf("cross user %s=%d", verb, status)
		}
	}
	repo := dao.NewRepository(db)
	ctx := context.Background()
	claimed, err := repo.ClaimForIndexing(ctx, id, 1, 1)
	if err != nil || !claimed {
		t.Fatal(claimed, err)
	}
	if err := repo.CompleteIndexing(ctx, id, 1, 1, model.KnowledgeStatusReady, 1, ""); err != nil {
		t.Fatal(err)
	}
	source := item + "/source?version=1&start_line=1&end_line=2"
	if status, result := knowledgeRequest(t, app.Handler, "GET", source, token, "", ""); status != 200 || result["data"].(map[string]any)["lines"].([]any)[1].(map[string]any)["text"] != "正文" {
		t.Fatal("exact source", status, result)
	}
	if status, _ := knowledgeRequest(t, app.Handler, "GET", source, other, "", ""); status != 404 {
		t.Fatal("owner source", status)
	}
	if status, _ := knowledgeRequest(t, app.Handler, "GET", item+"/source?version=2", token, "", ""); status != 409 {
		t.Fatal("version substitution", status)
	}
	if status, _ := knowledgeRequest(t, app.Handler, "GET", item+"/source?version=1&start_line=1&end_line=99", token, "", ""); status != 400 {
		t.Fatal("line bound", status)
	}
	status, result = knowledgeRequest(t, app.Handler, "PUT", item, token, "source.md", "# 原文\r\n正文\r\n")
	if status != 200 || result["unchanged"] != true {
		t.Fatal("unchanged", status, result)
	}
	status, result = knowledgeRequest(t, app.Handler, "PUT", item, token, "source.md", "# 新版\n新正文\n")
	if status != 202 || docView(t, result)["active_version"] != float64(1) {
		t.Fatal("stage update", status, result)
	}
	if status, result := knowledgeRequest(t, app.Handler, "GET", source, token, "", ""); status != 200 || result["data"].(map[string]any)["lines"].([]any)[1].(map[string]any)["text"] != "正文" {
		t.Fatal("staged old source", status, result)
	}
	if status, _ := knowledgeRequest(t, app.Handler, "PUT", item, token, "source.md", "another"); status != 409 {
		t.Fatal("busy", status)
	}
	if _, err := repo.StageUpdate(ctx, id, 1, 1, dao.DocumentUpdate{}, 100<<20); !errors.Is(err, dao.ErrDocumentChanged) {
		t.Fatal("original DAO must reject stale expectedVersion", err)
	}
	row, err := repo.GetByIDAndUserID(ctx, id, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(storageRoot, row.CleanupStoragePath)); err != nil {
		t.Fatal(err)
	}
	if status, result := knowledgeRequest(t, app.Handler, "GET", source, token, "", ""); status != 404 || result["error"].(map[string]any)["code"] != "document_source_missing" {
		t.Fatal("must not fake original", status, result)
	}
	claimed, err = repo.ClaimForIndexing(ctx, id, 1, 2)
	if err != nil || !claimed {
		t.Fatal(claimed, err)
	}
	if err := repo.CompleteIndexing(ctx, id, 1, 2, model.KnowledgeStatusReady, 1, ""); err != nil {
		t.Fatal(err)
	}
	if status, _ := knowledgeRequest(t, app.Handler, "GET", source, token, "", ""); status != 409 {
		t.Fatal("old citation must not select latest active source", status)
	}
	if status, data := knowledgeRequest(t, app.Handler, "GET", item+"/source?version=2&start_line=1&end_line=2", token, "", ""); status != 200 || data["data"].(map[string]any)["lines"].([]any)[1].(map[string]any)["text"] != "新正文" {
		t.Fatal("new exact source", status, data)
	}
}

func waitDocumentStatus(t *testing.T, h http.Handler, token, path, state string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		status, data := knowledgeRequest(t, h, "GET", path, token, "", "")
		if status != 200 {
			t.Fatal(status, data)
		}
		view := docView(t, data)
		if view["index_status"] == state {
			return view
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("original worker did not reach", state)
	return nil
}
func TestKnowledgeOriginalWorkersFailRetainRetryAndCleanup(t *testing.T) {
	chunks := &knowledgeFaultChunks{}
	chunks.indexFail.Store(true)
	app, db, token, _, storageRoot := knowledgeFixture(t, 1, chunks)
	status, data := knowledgeRequest(t, app.Handler, "POST", "/api/v1/knowledge/documents", token, "worker.md", "# 内容\n原文\n")
	if status != 201 {
		t.Fatal(status, data)
	}
	id := docView(t, data)["id"].(string)
	path := "/api/v1/knowledge/documents/" + id
	failed := waitDocumentStatus(t, app.Handler, token, path, "failed")
	if failed["active_version"] != float64(0) {
		t.Fatal(failed)
	}
	row, err := dao.NewRepository(db).GetByIDAndUserID(context.Background(), id, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(storageRoot, row.StoragePath)); err != nil {
		t.Fatal("failure deleted submitted source", err)
	}
	chunks.indexFail.Store(false)
	if status, _ := knowledgeRequest(t, app.Handler, "POST", path+"/retry", token, "", ""); status != 202 {
		t.Fatal(status)
	}
	waitDocumentStatus(t, app.Handler, token, path, "ready")
	if status, _ := knowledgeRequest(t, app.Handler, "POST", path+"/retry", token, "", ""); status != 409 {
		t.Fatal("nonretryable", status)
	}
	chunks.cleanupFail.Store(true)
	if status, _ := knowledgeRequest(t, app.Handler, "DELETE", path, token, "", ""); status != 202 {
		t.Fatal(status)
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		err = db.Where("id = ? AND user_id = ?", id, 1).First(&row).Error
		if err != nil {
			t.Fatal(err)
		}
		if row.CleanupAttempts > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if row.IndexStatus != model.KnowledgeStatusDeleting || row.CleanupAttempts == 0 {
		t.Fatalf("cleanup failure hidden %+v", row)
	}
	if _, err := os.Stat(filepath.Join(storageRoot, row.StoragePath)); err != nil {
		t.Fatal("cleanup failure lost source", err)
	}
}

func TestKnowledgeFrontendAPIThroughLocalSocket(t *testing.T) {
	app, _, _, _, _ := knowledgeFixture(t, 1, &knowledgeFaultChunks{})
	server := httptest.NewServer(app.Handler)
	defer server.Close()
	command := exec.Command("node", "../web/test/helpers/hybridKnowledgeClient.mjs")
	command.Env = append(os.Environ(), "HYBRID_TEST_URL="+server.URL)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("current frontend native API: %v\n%s", err, output)
	}
}
