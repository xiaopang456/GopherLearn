package conversation

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"GopherAI/common/memoryrecall"
	"GopherAI/entry"
)

// productRuntime is the product the harness boots for one phase.
//
// Both phases need it: the preparation needs it to publish the projection and
// let the product's own worker write the memories, and the query needs it to
// ask the question. The two phases run it in turn - the preparation stops it
// before it writes the archive, and the query starts it again against the
// restored archive - so the wiring is written once and both phases read the
// same code.
type productRuntime struct {
	settings *entry.Settings
	product  *standardProduct
	client   *http.Client
	base     string
	token    string
	ledger   *ModelLedger
	wire     *httptest.Server
	// restoreToken puts the reranker token back the way it was.
	restoreToken func()
}

// bootProduct points the product's configuration at the sandbox's database and
// at the shared index, starts the product and logs in.
//
// The reranker token is moved into the environment for the lifetime of the
// call and put back afterwards: the product reads it from there, and the
// harness must not leave it behind for the next phase.
func bootProduct(ctx context.Context, repository, storage string, env RealEnvironment,
	s *Sandbox, dir string, ledger *ModelLedger, password string) (*productRuntime, error) {
	settings, err := realSettings(repository, storage, env)
	if err != nil {
		return nil, err
	}
	roles, err := LoadRolePrompts(repository, memoryrecall.ReadMode(settings.Config.MemoryReadMode))
	if err != nil {
		return nil, err
	}
	wire := realWire(ledger, roles, env.ProxyBaseURL)
	connection, err := s.ConnectionInfo(ctx)
	if err != nil {
		wire.Close()
		return nil, err
	}
	settings.Config.MysqlHost = connection.Host
	settings.Config.MysqlPort, err = strconv.Atoi(connection.Port)
	if err != nil {
		wire.Close()
		return nil, err
	}
	settings.Config.MysqlDatabaseName = connection.Database
	settings.Config.MysqlUser = connection.User
	settings.Config.MysqlPassword = connection.Password
	settings.Config.RedisHost, settings.Config.RedisPort, err = splitRedisAddress(env.RedisAddress)
	if err != nil {
		wire.Close()
		return nil, err
	}
	settings.Entry.ChatBaseURL = wire.URL + "/v1"
	settings.Config.RagRerankEndpoint = wire.URL + "/v1/rerank"
	settings.Config.RagEmbeddingBaseURL = wire.URL + "/v1"
	settings.Config.JwtConfig.Key, err = randomHex(32)
	if err != nil {
		wire.Close()
		return nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		wire.Close()
		return nil, err
	}
	address := listener.Addr().(*net.TCPAddr)
	settings.Config.MainConfig.Host = "127.0.0.1"
	settings.Config.MainConfig.Port = address.Port
	listener.Close()

	path := filepath.Join(dir, "product.toml")
	oldToken, hadToken := os.LookupEnv("RAG_RERANK_AUTH_TOKEN")
	if err = os.Setenv("RAG_RERANK_AUTH_TOKEN", settings.Config.RagRerankToken); err != nil {
		wire.Close()
		return nil, err
	}
	// The configuration is written first and loaded second: the loader reads
	// the file the harness just wrote, which is how the run's own settings
	// reach the product.
	if err = writeEntryConfig(path, settings); err != nil {
		wire.Close()
		restoreToken(oldToken, hadToken)
		return nil, err
	}
	loaded, err := entry.Load(path)
	if err != nil {
		wire.Close()
		restoreToken(oldToken, hadToken)
		return nil, err
	}
	if loaded.Config.EpisodeQuietWindowSeconds != 86400 || loaded.Entry.Migrate ||
		!loaded.Config.RagRerankEnabled || loaded.Config.RagRerankEndpoint != wire.URL+"/v1/rerank" {
		wire.Close()
		restoreToken(oldToken, hadToken)
		return nil, fmt.Errorf("unexpected standard entry settings")
	}
	product := startStandardProduct(path)
	client := &http.Client{Timeout: 5 * time.Minute, Transport: &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	base := "http://" + settings.Address()
	token, err := loginStandardProduct(ctx, product, client, base, env.UserName, password)
	if err != nil {
		product.stop()
		client.CloseIdleConnections()
		wire.Close()
		restoreToken(oldToken, hadToken)
		return nil, err
	}
	runtime := &productRuntime{
		settings: settings, product: product, client: client, base: base,
		token: token, ledger: ledger, wire: wire,
	}
	runtime.restoreToken = func() { restoreToken(oldToken, hadToken) }
	return runtime, nil
}

// restoreToken puts the reranker token back the way it was.
func restoreToken(old string, had bool) {
	if had {
		os.Setenv("RAG_RERANK_AUTH_TOKEN", old)
		return
	}
	os.Unsetenv("RAG_RERANK_AUTH_TOKEN")
}

// stop shuts the product down and releases everything it held.
func (r *productRuntime) stop() error {
	if r == nil {
		return nil
	}
	var err error
	if r.product != nil {
		err = errors.Join(err, r.product.stop())
	}
	if r.client != nil {
		r.client.CloseIdleConnections()
	}
	if r.wire != nil {
		r.wire.Close()
	}
	if r.restoreToken != nil {
		r.restoreToken()
	}
	return err
}
