package conversation

// The check a run makes on its own configuration before it spends anything.
//
// The run of 2026-11-14 measured the closed arm and said nothing about it:
// the isolated entry of the checkout had no [agentConfig] block, so the
// summary did not record what the assistant produced and the evidence export
// was off. The answers looked fine, so the mistake was found only in the
// log - after the money was spent.
//
// These are the counter-examples: the configurations that must not pass the
// check. None of them calls a model, a database or a proxy, so the whole
// file costs nothing to run.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// aCheckoutWith is a checkout whose isolated entry says what it says.
func aCheckoutWith(t *testing.T, agentConfig string) string {
	t.Helper()
	root := t.TempDir()
	entry := filepath.Join(root, "tutorial", "v2-entry")
	if err := os.MkdirAll(entry, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `[isolatedEntry]
isolated = true
migrate = false
chatKey = ""
chatModel = "explicit-model-name"
chatBaseURL = "https://provider.invalid/v1"
[mainConfig]
host = "127.0.0.1"
port = 18082
[mysqlConfig]
host = "mysql-isolated.invalid"
port = 3306
user = "v2_isolated"
password = ""
databaseName = "v2_isolated"
charset = "utf8mb4"
[redisConfig]
host = "redis-isolated.invalid"
port = 6379
db = 0
password = ""
[jwtConfig]
key = ""
expire_duration = 24
issuer = "v2-isolated"
subject = "v2-isolated-user"
[knowledgeConfig]
storageDir = "data/v2-isolated/knowledge"
[ragModelConfig]
embeddingModel = "explicit-embedding-model"
embeddingBaseUrl = "https://embedding.invalid/v1"
embeddingApiKey = ""
chatModelName = "explicit-model-name"
dimension = 1024
rerankEnabled = false
` + agentConfig
	if err := os.WriteFile(filepath.Join(entry, "isolated.example.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// The configuration a run needs passes the check.
func TestTheConfigurationTheRunNeedsPassesTheCheck(t *testing.T) {
	root := aCheckoutWith(t, `
[agentConfig]
summarySegmentMode = "on"
summaryAssistantContent = "on"
summarySegmentDebugDump = true
`)
	report, err := CheckProductConfiguration(root)
	if err != nil {
		t.Fatalf("the check refused a good configuration: %v", err)
	}
	if report.SummaryAssistantContent != "on" {
		t.Fatalf("the assistant-content switch = %q, want on", report.SummaryAssistantContent)
	}
	if !report.SummarySegmentDebugDump {
		t.Fatal("the debug dump is off")
	}
}

// The configuration of the run of 2026-11-14 does not pass.
//
// This is the counter-example that matters: the file was there, the tree was
// clean, and the run measured the wrong arm anyway.
func TestTheConfigurationWithoutTheAgentConfigDoesNotPass(t *testing.T) {
	root := aCheckoutWith(t, "")
	report, err := CheckProductConfiguration(root)
	if err != nil {
		t.Fatalf("the check could not read the configuration: %v", err)
	}
	if report.SummaryAssistantContent == "on" {
		t.Fatalf("the assistant-content switch = %q, want the closed arm's value", report.SummaryAssistantContent)
	}
	if report.SummarySegmentDebugDump {
		t.Fatal("the debug dump is on although the configuration says nothing about it")
	}
}

// A missing isolated entry is refused, not read as "the defaults".
func TestAMissingConfigurationIsRefused(t *testing.T) {
	root := t.TempDir()
	report, err := CheckProductConfiguration(root)
	if err == nil {
		t.Fatalf("the check accepted a checkout without a configuration: %+v", report)
	}
	if report.ConfigurationExists {
		t.Fatal("the report says the configuration is there")
	}
}

// The check reads the product's own loader, not the text of the file.
//
// A file that says the right thing in a way the loader does not read is a
// configuration the product does not have.
func TestTheCheckReadsTheLoaderAndNotTheTextOfTheFile(t *testing.T) {
	root := aCheckoutWith(t, `
[agentConfig]
summarySegmentMode = "on"
summaryAssistantContent = "on"
summarySegmentDebugDump = true
`)
	// The same file with the switches spelled the way the loader does not
	// read: the section is the wrong one.
	body, err := os.ReadFile(filepath.Join(root, "tutorial", "v2-entry", "isolated.example.toml"))
	if err != nil {
		t.Fatal(err)
	}
	broken := strings.Replace(string(body), "[agentConfig]", "[someOtherSection]", 1)
	if err = os.WriteFile(filepath.Join(root, "tutorial", "v2-entry", "isolated.example.toml"),
		[]byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := CheckProductConfiguration(root)
	if err != nil {
		t.Fatalf("the check could not read the configuration: %v", err)
	}
	if report.SummaryAssistantContent == "on" {
		t.Fatal("the check read the switches out of a section the loader ignores")
	}
	if report.SummarySegmentDebugDump {
		t.Fatal("the check read the debug dump out of a section the loader ignores")
	}
}
