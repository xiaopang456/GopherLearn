package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/xiaopang456/GopherLearn/internal/repository"
	"github.com/xiaopang456/GopherLearn/internal/storage"
)

func TestNewHTTPServerTimeoutsProtectHeadersAndKeepSSEWritesOpen(t *testing.T) {
	server := newHTTPServer(":8080", http.NewServeMux())
	if server.ReadHeaderTimeout != 5*time.Second {
		t.Fatalf("ReadHeaderTimeout=%s, want 5s", server.ReadHeaderTimeout)
	}
	if server.IdleTimeout != 60*time.Second {
		t.Fatalf("IdleTimeout=%s, want 60s", server.IdleTimeout)
	}
	if server.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout=%s, want zero for long-lived SSE", server.WriteTimeout)
	}
}

func env(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func TestOpenStorageKeepsLocalDevelopmentOnMemory(t *testing.T) {
	stores, config, err := openStorage(context.Background(), env(map[string]string{}), productionStorageOptions())
	if err != nil {
		t.Fatal(err)
	}
	if config.Mode != storage.ModeMemory {
		t.Fatalf("mode = %q, want memory", config.Mode)
	}
	if _, ok := stores.Repository.(*repository.InMemoryRepository); !ok {
		t.Fatalf("store = %T, want the in-memory repository", stores.Repository)
	}
	if stores.Knowledge == nil {
		t.Fatal("memory startup produced no knowledge store, so knowledge turns cannot run locally")
	}
}

// TestOpenStorageRefusesToStartMySQLWithoutADSN proves the startup rule that
// matters for a deployment: selecting mysql without a connection string must
// stop the process. The production connector is used, and nothing is dialed
// because the configuration is rejected first.
func TestOpenStorageRefusesToStartMySQLWithoutADSN(t *testing.T) {
	stores, _, err := openStorage(context.Background(), env(map[string]string{"GOPHERLEARN_STORAGE": "mysql"}), productionStorageOptions())
	if !errors.Is(err, storage.ErrMissingDSN) {
		t.Fatalf("error = %v, want ErrMissingDSN", err)
	}
	if stores.Repository != nil || stores.Knowledge != nil {
		t.Fatal("a rejected storage configuration still produced stores")
	}
}

func TestOpenStorageRefusesAnUnknownMode(t *testing.T) {
	// None of these normalize to memory or mysql, so the real MySQL connector
	// is never reached and no socket is opened.
	for _, value := range []string{"postgres", "mysqlx", "memory-ish", "cockroachdb"} {
		stores, _, err := openStorage(context.Background(), env(map[string]string{
			"GOPHERLEARN_STORAGE":   value,
			"GOPHERLEARN_MYSQL_DSN": "stub-user:stub-password@tcp(stub-host:3306)/stub-db",
		}), productionStorageOptions())
		if !errors.Is(err, storage.ErrUnsupportedMode) {
			t.Errorf("storage=%q error = %v, want ErrUnsupportedMode", value, err)
		}
		if stores.Repository != nil || stores.Knowledge != nil {
			t.Errorf("storage=%q produced stores despite the rejection", value)
		}
	}
}

func TestOpenStoragePropagatesMySQLStartupFailureWithoutLeakingConfig(t *testing.T) {
	secret := "stub-user:stub-password@tcp(stub-host:3306)/stub-db"
	called := false
	stores, storageConfig, err := openStorage(context.Background(), env(map[string]string{
		"GOPHERLEARN_STORAGE":   "mysql",
		"GOPHERLEARN_MYSQL_DSN": secret,
	}), storage.Options{NewMySQL: func(context.Context, storage.Config) (storage.Stores, error) {
		called = true
		return storage.Stores{}, errors.New("dial failed for " + secret)
	}})
	if err == nil {
		t.Fatal("startup succeeded although the connector failed")
	}
	if !called {
		t.Fatal("mysql mode never consulted the connector")
	}
	if stores.Repository != nil || stores.Knowledge != nil {
		t.Fatal("a failed mysql startup returned stores")
	}
	if _, isInMemory := stores.Repository.(*repository.InMemoryRepository); isInMemory {
		t.Fatal("mysql startup silently downgraded to memory")
	}
	// openStorage keeps the raw error so sentinels survive, and main logs the
	// sanitized form. What is logged must never carry the connection string.
	logged := storageConfig.Sanitize(err)
	for _, secret := range []string{secret, "stub-password", "stub-user"} {
		if strings.Contains(logged.Error(), secret) {
			t.Fatalf("the startup log would print %q: %v", secret, logged)
		}
	}
	if !strings.Contains(logged.Error(), "dial failed") {
		t.Fatalf("sanitizing removed the operator's diagnosis: %v", logged)
	}
}

func TestProductionStorageOptionsWiresTheMySQLConnector(t *testing.T) {
	if productionStorageOptions().NewMySQL == nil {
		t.Fatal("the binary must be able to build a MySQL repository")
	}
}

func TestStorageSummaryStatesWhatMemoryCosts(t *testing.T) {
	memory := storageSummary(storage.Config{Mode: storage.ModeMemory})
	if !strings.Contains(memory, "memory") || !strings.Contains(memory, "lost on restart") {
		t.Fatalf("memory summary = %q, want the mode and its consequence", memory)
	}
	mysql := storageSummary(storage.Config{Mode: storage.ModeMySQL, MySQLDSN: "secret"})
	if mysql != "storage=mysql" {
		t.Fatalf("mysql summary = %q, want only the mode name", mysql)
	}
	if strings.Contains(mysql, "secret") {
		t.Fatalf("startup log carried the DSN: %q", mysql)
	}
}
