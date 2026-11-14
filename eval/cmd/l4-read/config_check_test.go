package main

// The check, at the level a run meets it.
//
// The command is the gate a run passes through before it spends anything, so
// what matters here is not only what the check finds but what it does with
// it: a configuration that does not say what the run needs has to end the
// command with a failure, and the report it printed has to say so as well.
//
// None of these calls a model, a database or a proxy.

import (
	"bytes"
	"encoding/json"
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

// anOverlay is the scaled configuration a run hands the product.
func anOverlay(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "layered.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// theCheck runs the command the way a run runs it and reads its report.
func theCheck(t *testing.T, args ...string) (error, map[string]any, string) {
	t.Helper()
	var out bytes.Buffer
	err := runConfigCheck(args, &out)
	text := out.String()
	report := map[string]any{}
	if at := strings.Index(text, "{"); at >= 0 {
		if decodeErr := json.Unmarshal([]byte(text[at:]), &report); decodeErr != nil {
			t.Fatalf("the check printed something that is not a report: %v\n%s", decodeErr, text)
		}
	}
	return err, report, text
}

// theRunNeeds is what this run needs the configuration to say.
func theRunNeeds() []string {
	return []string{
		"-expect-summary-segment-mode", "on",
		"-expect-assistant-content", "on",
		"-expect-debug-dump", "on",
	}
}

// The configuration a run needs passes the check.
func TestTheCheckPassesTheConfigurationTheRunNeeds(t *testing.T) {
	root := aCheckoutWith(t, `
[agentConfig]
summarySegmentMode = "on"
summaryAssistantContent = "on"
summarySegmentDebugDump = true
`)
	err, report, text := theCheck(t, append([]string{"-repository", root}, theRunNeeds()...)...)
	if err != nil {
		t.Fatalf("the check refused a good configuration: %v\n%s", err, text)
	}
	if report["matches"] != true {
		t.Fatalf("the report does not match what the run needs: %s", text)
	}
	if report["summary_segment_mode"] != "on" {
		t.Fatalf("the report does not name the mode: %s", text)
	}
}

// The overlay the run uses is read with the entry file, in the run's order.
//
// The check has to report the configuration the run will measure with: the
// overlay the run hands the product, applied on top of the entry file.
func TestTheCheckReadsTheOverlayTheRunUses(t *testing.T) {
	root := aCheckoutWith(t, `
[agentConfig]
summarySegmentMode = "on"
summaryAssistantContent = "on"
summarySegmentDebugDump = true
`)
	overlay := anOverlay(t, `
[agentConfig]
contextMaxWhiteboardTokens = 360
`)
	err, report, text := theCheck(t, append([]string{
		"-repository", root, "-scaled-config", overlay,
	}, theRunNeeds()...)...)
	if err != nil {
		t.Fatalf("the check refused a good configuration: %v\n%s", err, text)
	}
	budgets, ok := report["effective_budgets"].(map[string]any)
	if !ok {
		t.Fatalf("the report has no effective budgets: %s", text)
	}
	if budgets["whiteboard_tokens"] != float64(360) {
		t.Fatalf("the whiteboard budget = %v, want the overlay's 360: %s",
			budgets["whiteboard_tokens"], text)
	}
}

// The mode is checked, and a run that needs another mode is refused.
//
// The check has to refuse a configuration that measures a different arm,
// even when the other two switches are exactly what the run needs.
func TestTheCheckRefusesTheOtherModes(t *testing.T) {
	for _, mode := range []string{"off", "shadow"} {
		root := aCheckoutWith(t, `
[agentConfig]
summarySegmentMode = "`+mode+`"
summaryAssistantContent = "on"
summarySegmentDebugDump = true
`)
		err, report, text := theCheck(t, append([]string{"-repository", root}, theRunNeeds()...)...)
		if err == nil {
			t.Fatalf("the check passed the mode %q: %s", mode, text)
		}
		if report["matches"] != false {
			t.Fatalf("the report says the mode %q matches: %s", mode, text)
		}
		if report["summary_segment_mode"] != mode {
			t.Fatalf("the report does not name the mode %q: %s", mode, text)
		}
		if !strings.Contains(err.Error(), "mode") {
			t.Fatalf("the failure does not say what is wrong: %v", err)
		}
	}
}

// The overlay can change the mode under the check.
//
// The entry file is right and the overlay turns the mode off: the run will
// measure the closed arm, and the check has to say so.
func TestTheCheckRefusesAnOverlayThatChangesTheMode(t *testing.T) {
	root := aCheckoutWith(t, `
[agentConfig]
summarySegmentMode = "on"
summaryAssistantContent = "on"
summarySegmentDebugDump = true
`)
	overlay := anOverlay(t, `
[agentConfig]
summarySegmentMode = "off"
`)
	err, report, text := theCheck(t, append([]string{
		"-repository", root, "-scaled-config", overlay,
	}, theRunNeeds()...)...)
	if err == nil {
		t.Fatalf("the check passed an overlay that turns the mode off: %s", text)
	}
	if report["matches"] != false {
		t.Fatalf("the report says the configuration matches: %s", text)
	}
	if report["summary_segment_mode"] != "off" {
		t.Fatalf("the report does not name the overlay's mode: %s", text)
	}
}

// The entry file is wrong and the overlay does not save it.
//
// The other way round: the overlay is right, and the entry file says the
// assistant's products are not recorded. The run would not export the
// evidence, so the check has to refuse.
func TestTheCheckRefusesAnEntryTheOverlayDoesNotSave(t *testing.T) {
	root := aCheckoutWith(t, `
[agentConfig]
summarySegmentMode = "on"
summaryAssistantContent = "off"
summarySegmentDebugDump = true
`)
	overlay := anOverlay(t, `
[agentConfig]
summarySegmentMode = "on"
`)
	err, report, text := theCheck(t, append([]string{
		"-repository", root, "-scaled-config", overlay,
	}, theRunNeeds()...)...)
	if err == nil {
		t.Fatalf("the check passed an entry without the assistant's products: %s", text)
	}
	if report["matches"] != false {
		t.Fatalf("the report says the configuration matches: %s", text)
	}
}

// The check refuses a configuration the product would refuse at startup.
//
// The overlay is applied before the validation, so a configuration that
// does not fit is refused by the check rather than measured.
func TestTheCheckRefusesAConfigurationTheProductWouldRefuse(t *testing.T) {
	root := aCheckoutWith(t, `
[agentConfig]
summarySegmentMode = "on"
summaryAssistantContent = "on"
summarySegmentDebugDump = true
`)
	overlay := anOverlay(t, `
[agentConfig]
contextInputTargetTokens = 2048
contextMaxSummaryTokens = 1100
`)
	err, _, text := theCheck(t, append([]string{
		"-repository", root, "-scaled-config", overlay,
	}, theRunNeeds()...)...)
	if err == nil {
		t.Fatalf("the check passed a configuration the product refuses: %s", text)
	}
}

// A missing overlay is refused rather than read as "the defaults".
func TestTheCheckRefusesAMissingOverlay(t *testing.T) {
	root := aCheckoutWith(t, `
[agentConfig]
summarySegmentMode = "on"
summaryAssistantContent = "on"
summarySegmentDebugDump = true
`)
	err, _, _ := theCheck(t, append([]string{
		"-repository", root, "-scaled-config", filepath.Join(t.TempDir(), "absent.toml"),
	}, theRunNeeds()...)...)
	if err == nil {
		t.Fatal("the check passed a missing overlay")
	}
}

// The report says the final mode and the effective budgets, and no key.
func TestTheReportNamesTheModeAndTheBudgetsAndNoKey(t *testing.T) {
	root := aCheckoutWith(t, `
[agentConfig]
summarySegmentMode = "on"
summaryAssistantContent = "on"
summarySegmentDebugDump = true
`)
	_, report, text := theCheck(t, append([]string{"-repository", root}, theRunNeeds()...)...)
	if report["summary_segment_mode"] != "on" {
		t.Fatalf("the report does not name the final mode: %s", text)
	}
	if _, ok := report["effective_budgets"]; !ok {
		t.Fatalf("the report has no effective budgets: %s", text)
	}
	for _, secret := range []string{
		"synthetic-offline-key-with-at-least-32-characters",
		"placeholder-proxy-injected",
		"synthetic-never-sent",
	} {
		if strings.Contains(text, secret) {
			t.Fatalf("the report carries a key: %s", text)
		}
	}
}
