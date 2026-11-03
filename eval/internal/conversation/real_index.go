package conversation

// Building the shared evaluation index. The corpus is uploaded through the
// product's own document API, so the chunks, the document metadata and the
// index itself are the ones the product produces. The seed written afterwards
// is what every later run restores, and the storage tree is copied so that a
// run never reads the directory the builder wrote.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"GopherAI/common/memoryrecall"
	"GopherAI/model"
	"GopherAI/utils"
)

// IndexReport describes what the builder did.
type IndexReport struct {
	Documents   int                  `json:"documents"`
	Calls       []ModelCall          `json:"model_calls"`
	UsageByRole map[string]RoleUsage `json:"usage_by_role"`
	SeedSHA256  string               `json:"seed_sha256"`
	StorageDir  string               `json:"storage_dir"`
}

// BuildSharedIndex uploads the corpus into the shared evaluation index and
// writes the seed of the resulting documents.
func BuildSharedIndex(ctx context.Context, repository string, env RealEnvironment, corpusDir, seedPath, storageDir string) (report IndexReport, resultErr error) {
	if err := env.Validate(); err != nil {
		return report, err
	}
	if strings.TrimSpace(corpusDir) == "" || strings.TrimSpace(seedPath) == "" || strings.TrimSpace(storageDir) == "" {
		return report, fmt.Errorf("the index build needs the corpus, the seed path and the storage directory")
	}
	files, err := corpusFiles(corpusDir)
	if err != nil {
		return report, err
	}
	if len(files) == 0 {
		return report, fmt.Errorf("the corpus has no document")
	}
	build, err := os.MkdirTemp("", "gl-index-build-")
	if err != nil {
		return report, err
	}
	defer os.RemoveAll(build)
	settings, err := realSettings(repository, filepath.Join(build, "storage"), env)
	if err != nil {
		return report, err
	}
	s, err := NewSandbox(ctx)
	if err != nil {
		return report, err
	}
	defer func() { resultErr = errorsJoinReport(resultErr, closeSandbox(s)) }()
	if err = s.InitProductSchema(ctx); err != nil {
		return report, err
	}
	password, err := randomHex(24)
	if err != nil {
		return report, err
	}
	hash, err := utils.HashPassword(password)
	if err != nil {
		return report, err
	}
	if err = s.SeedProduct(ctx, model.User{ID: env.UserID, Username: env.UserName, Password: hash}, nil, settings.Config.KnowledgeConfig.StorageDir); err != nil {
		return report, err
	}
	ledger := &ModelLedger{}
	roles, err := LoadRolePrompts(repository, memoryrecall.ReadMode(settings.Config.MemoryReadMode))
	if err != nil {
		return report, err
	}
	wire := realWire(ledger, roles, env.ProxyBaseURL)
	defer wire.Close()
	connection, err := s.ConnectionInfo(ctx)
	if err != nil {
		return report, err
	}
	settings.Config.MysqlHost = connection.Host
	settings.Config.MysqlPort, err = strconv.Atoi(connection.Port)
	if err != nil {
		return report, fmt.Errorf("the evaluation database port is invalid")
	}
	settings.Config.MysqlDatabaseName = connection.Database
	settings.Config.MysqlUser = connection.User
	settings.Config.MysqlPassword = connection.Password
	settings.Config.RedisHost, settings.Config.RedisPort, err = splitRedisAddress(env.RedisAddress)
	if err != nil {
		return report, err
	}
	settings.Entry.ChatBaseURL = wire.URL + "/v1"
	settings.Config.RagRerankEndpoint = wire.URL + "/v1/rerank"
	settings.Config.RagBaseUrl = wire.URL + "/v1"
	settings.Config.RagEmbeddingBaseURL = wire.URL + "/v1"
	settings.Config.JwtConfig.Key, err = randomHex(32)
	if err != nil {
		return report, err
	}
	port, err := loopbackPort()
	if err != nil {
		return report, err
	}
	settings.Config.MainConfig.Host = "127.0.0.1"
	settings.Config.MainConfig.Port = port
	path := filepath.Join(build, "product.toml")
	oldToken, hadToken := os.LookupEnv("RAG_RERANK_AUTH_TOKEN")
	if err = os.Setenv("RAG_RERANK_AUTH_TOKEN", settings.Config.RagRerankToken); err != nil {
		return report, err
	}
	defer func() {
		if hadToken {
			os.Setenv("RAG_RERANK_AUTH_TOKEN", oldToken)
		} else {
			os.Unsetenv("RAG_RERANK_AUTH_TOKEN")
		}
	}()
	if err = writeEntryConfig(path, settings, wire.URL); err != nil {
		return report, err
	}
	product := startStandardProduct(path)
	defer func() { resultErr = errorsJoinReport(resultErr, product.stop()) }()
	client := &http.Client{Timeout: 5 * time.Minute}
	base := "http://" + settings.Address()
	token, err := loginStandardProduct(ctx, product, client, base, env.UserName, password)
	if err != nil {
		return report, err
	}
	ids := make([]string, 0, len(files))
	for _, file := range files {
		id, uploadErr := uploadDocument(ctx, client, base, token, file)
		if uploadErr != nil {
			return report, uploadErr
		}
		ids = append(ids, id)
	}
	deadline := time.Now().Add(60 * time.Minute)
	for _, id := range ids {
		for {
			status, statusErr := documentStatus(ctx, client, base, token, id)
			if statusErr != nil {
				return report, statusErr
			}
			if status == "ready" {
				break
			}
			if status == "failed" {
				return report, fmt.Errorf("document %s could not be indexed", id)
			}
			if time.Now().After(deadline) {
				return report, fmt.Errorf("document %s did not finish indexing in time", id)
			}
			select {
			case <-ctx.Done():
				return report, ctx.Err()
			case <-time.After(3 * time.Second):
			}
		}
	}
	if err = product.stop(); err != nil {
		return report, err
	}
	seed, err := DumpKnowledgeSeed(ctx, s.db, env.UserID, settings.Config.KnowledgeConfig.StorageDir)
	if err != nil {
		return report, err
	}
	if err = CopyStorageTree(settings.Config.KnowledgeConfig.StorageDir, storageDir); err != nil {
		return report, err
	}
	if err = WriteKnowledgeSeed(seedPath, seed); err != nil {
		return report, err
	}
	digest, err := sha256File(seedPath)
	if err != nil {
		return report, err
	}
	report.Documents = len(seed.Documents)
	report.SeedSHA256 = digest
	report.StorageDir = storageDir
	report.Calls = ledger.Calls()
	report.UsageByRole, err = SummarizeCalls(report.Calls)
	return report, err
}

// corpusFiles lists the documents of a corpus directory in a stable order.
func corpusFiles(dir string) ([]string, error) {
	var files []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".md", ".txt", ".markdown":
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

// uploadDocument sends one file to the product's document API.
func uploadDocument(ctx context.Context, client *http.Client, base, token, path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", filepath.Base(path))
	if err != nil {
		return "", err
	}
	if _, err = part.Write(content); err != nil {
		return "", err
	}
	if err = writer.Close(); err != nil {
		return "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/knowledge/documents", body)
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("the product refused a document with HTTP %d", response.StatusCode)
	}
	var envelope struct {
		Document struct {
			ID string `json:"id"`
		} `json:"document"`
	}
	if err = json.Unmarshal(raw, &envelope); err != nil {
		return "", err
	}
	if envelope.Document.ID == "" {
		return "", fmt.Errorf("the product accepted a document without an identifier")
	}
	return envelope.Document.ID, nil
}

// documentStatus reads the indexing state of one document.
func documentStatus(ctx context.Context, client *http.Client, base, token, id string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/knowledge/documents/"+id+"/processing", nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var envelope struct {
		Status string `json:"status"`
	}
	if err = json.Unmarshal(raw, &envelope); err != nil {
		return "", err
	}
	return envelope.Status, nil
}

func errorsJoinReport(err error, other error) error {
	if err == nil {
		return other
	}
	if other == nil {
		return err
	}
	return fmt.Errorf("%v; %w", err, other)
}

// loopbackPort reserves a free loopback port for the product service.
func loopbackPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port, nil
}

// sha256File returns the digest of a file.
func sha256File(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:]), nil
}
