package conversation

import (
	"GopherAI/model"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProductSchemaIntegration(t *testing.T) {
	if os.Getenv("GL_SANDBOX_TEST") != "1" {
		t.Skip("requires local Docker MySQL image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, err := NewSandbox(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		c, done := context.WithTimeout(context.Background(), 15*time.Second)
		defer done()
		if err := s.Close(c); err != nil {
			t.Error(err)
		}
	}()
	if err = s.InitProductSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.InitProductSchema(ctx); err == nil {
		t.Fatal("second migration accepted")
	}
	storage := t.TempDir()
	directory := filepath.Join(storage, "7", "synthetic-doc")
	if err = os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "synthetic.txt")
	bytes := []byte("Synthetic immutable learning note.")
	if err = os.WriteFile(path, bytes, 0600); err != nil {
		t.Fatal(err)
	}
	doc := model.KnowledgeDocument{ID: "synthetic-doc", UserID: 7, OriginalName: "synthetic.txt", StoragePath: "7/synthetic-doc/synthetic.txt", FileType: "txt", ContentHash: digest(bytes), FileSize: int64(len(bytes)), Version: 1, ActiveVersion: 1, IndexStatus: model.KnowledgeStatusReady}
	doc.PrepareForCreate()
	bad := doc
	bad.ContentHash = "wrong"
	if err = s.SeedProduct(ctx, model.User{ID: 7, Username: "learner"}, []model.KnowledgeDocument{bad}, storage); err == nil {
		t.Fatal("wrong seed hash accepted")
	}
	if err = s.SeedProduct(ctx, model.User{ID: 7, Username: "learner"}, []model.KnowledgeDocument{doc}, storage); err != nil {
		t.Fatal(err)
	}
	if err = s.Restore(ctx, validSnapshot()); err != nil {
		t.Fatal(err)
	}
	db, err := s.openProductDB(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	if err = sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.ObserveJobs(ctx); err != nil {
		t.Fatalf("product pool closed observer: %v", err)
	}
	if err = s.SeedProduct(ctx, model.User{ID: 7, Username: "learner"}, nil, storage); err == nil {
		t.Fatal("nonempty seed accepted")
	}
}
