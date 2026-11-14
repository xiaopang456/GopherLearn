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

// theRunNeeds is what this run needs the configuration to say.
func theRunNeeds() ExpectedConfiguration {
	return ExpectedConfiguration{
		SummarySegmentMode:      "on",
		SummaryAssistantContent: "on",
		SummarySegmentDebugDump: true,
	}
}

// The configuration a run needs passes the check.
func TestTheConfigurationTheRunNeedsPassesTheCheck(t *testing.T) {
	root := aCheckoutWith(t, `
[agentConfig]
summarySegmentMode = "on"
summaryAssistantContent = "on"
summarySegmentDebugDump = true
`)
	report, err := CheckProductConfiguration(ConfigurationCheck{Repository: root})
	if err != nil {
		t.Fatalf("the check refused a good configuration: %v", err)
	}
	if report.SummarySegmentMode != "on" {
		t.Fatalf("the mode = %q, want on", report.SummarySegmentMode)
	}
	if report.SummaryAssistantContent != "on" {
		t.Fatalf("the assistant-content switch = %q, want on", report.SummaryAssistantContent)
	}
	if !report.SummarySegmentDebugDump {
		t.Fatal("the debug dump is off")
	}
	if !report.MatchesExpected(theRunNeeds()) {
		t.Fatalf("the report does not match what the run needs: %+v", report)
	}
}

// The configuration of the run of 2026-11-14 does not pass.
//
// This is the counter-example that matters: the file was there, the tree was
// clean, and the run measured the wrong arm anyway.
func TestTheConfigurationWithoutTheAgentConfigDoesNotPass(t *testing.T) {
	root := aCheckoutWith(t, "")
	report, err := CheckProductConfiguration(ConfigurationCheck{Repository: root})
	if err != nil {
		t.Fatalf("the check could not read the configuration: %v", err)
	}
	if report.SummaryAssistantContent == "on" {
		t.Fatalf("the assistant-content switch = %q, want the closed arm's value", report.SummaryAssistantContent)
	}
	if report.SummarySegmentDebugDump {
		t.Fatal("the debug dump is on although the configuration says nothing about it")
	}
	if report.MatchesExpected(theRunNeeds()) {
		t.Fatal("the report matches what the run needs, although nothing was configured")
	}
}

// The mode is checked too, and not only the other two switches.
//
// The mode decides which arm is measured: the old rolling summary, the
// shadow, or the layered summary the window reads back. A run that needs the
// layered summary and gets the shadow has measured a different thing, and
// the other two switches being right does not make it the right run.
func TestTheModeIsCheckedAndNotOnlyTheOtherTwoSwitches(t *testing.T) {
	for _, mode := range []string{"off", "shadow"} {
		root := aCheckoutWith(t, `
[agentConfig]
summarySegmentMode = "`+mode+`"
summaryAssistantContent = "on"
summarySegmentDebugDump = true
`)
		report, err := CheckProductConfiguration(ConfigurationCheck{Repository: root})
		if err != nil {
			t.Fatalf("the check could not read the configuration: %v", err)
		}
		if report.SummarySegmentMode != mode {
			t.Fatalf("the mode = %q, want %q", report.SummarySegmentMode, mode)
		}
		if report.SummaryAssistantContent != "on" || !report.SummarySegmentDebugDump {
			t.Fatalf("the other two switches are wrong, so the mode is not what failed: %+v", report)
		}
		if report.MatchesExpected(theRunNeeds()) {
			t.Fatalf("the mode %q passed the check", mode)
		}
	}
}

// The overlay decides the mode the product will run in.
//
// The entry file is what the product reads, and the overlay is what the run
// hands the product on top of it. A check that read the entry file alone
// would report the mode of a run that never happens.
func TestTheOverlayDecidesTheModeTheProductWillRunIn(t *testing.T) {
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
	report, err := CheckProductConfiguration(ConfigurationCheck{
		Repository: root, ScaledConfig: overlay,
	})
	if err != nil {
		t.Fatalf("the check could not read the configuration: %v", err)
	}
	if report.SummarySegmentMode != "off" {
		t.Fatalf("the mode = %q, want the overlay's off", report.SummarySegmentMode)
	}
	if report.MatchesExpected(theRunNeeds()) {
		t.Fatal("the configuration matches what the run needs, although the overlay turned the mode off")
	}
}

// The overlay can turn the mode on as well.
//
// The check reads the configuration the run will measure with, whichever
// file that configuration comes from.
func TestTheOverlayCanTurnTheModeOn(t *testing.T) {
	root := aCheckoutWith(t, `
[agentConfig]
summaryAssistantContent = "on"
summarySegmentDebugDump = true
`)
	overlay := anOverlay(t, `
[agentConfig]
summarySegmentMode = "on"
`)
	report, err := CheckProductConfiguration(ConfigurationCheck{
		Repository: root, ScaledConfig: overlay,
	})
	if err != nil {
		t.Fatalf("the check could not read the configuration: %v", err)
	}
	if report.SummarySegmentMode != "on" {
		t.Fatalf("the mode = %q, want the overlay's on", report.SummarySegmentMode)
	}
	if !report.MatchesExpected(theRunNeeds()) {
		t.Fatalf("the report does not match what the run needs: %+v", report)
	}
}

// The budgets the report carries are the ones the window renders with.
//
// The configured numbers and the effective numbers are different numbers: a
// summary that records the assistant's products makes a segment larger, and
// the segment blocks' budgets follow it. A report that listed the configured
// numbers would say what the file says and not what the product will do.
func TestTheEffectiveBudgetsAreTheOnesTheWindowRendersWith(t *testing.T) {
	root := aCheckoutWith(t, `
[agentConfig]
summarySegmentMode = "on"
summaryAssistantContent = "on"
summarySegmentDebugDump = true
`)
	overlay := anOverlay(t, `
[agentConfig]
contextMaxWhiteboardTokens = 360
contextMaxSegmentTokens = 300
`)
	report, err := CheckProductConfiguration(ConfigurationCheck{
		Repository: root, ScaledConfig: overlay,
	})
	if err != nil {
		t.Fatalf("the check could not read the configuration: %v", err)
	}
	if report.Budgets.WhiteboardTokens != 360 {
		t.Fatalf("the whiteboard budget = %d, want the overlay's 360", report.Budgets.WhiteboardTokens)
	}
	// The configured segment size is the product's default 300, and the
	// assistant's products make it 450 - so the effective number is the one
	// the window gets, and it is not the configured one.
	if report.Budgets.SegmentTokens != 450 {
		t.Fatalf("the segment budget = %d, want the effective 450", report.Budgets.SegmentTokens)
	}
	if report.Budgets.InputTargetTokens <= 0 || report.Budgets.ModelWindowTokens <= 0 {
		t.Fatalf("the window budgets are missing: %+v", report.Budgets)
	}
}

// The report carries no key of any kind.
//
// It is printed where the run's output is, and a report that carried a key
// would put one in every log of every run that uses it.
func TestTheReportCarriesNoKey(t *testing.T) {
	root := aCheckoutWith(t, `
[agentConfig]
summarySegmentMode = "on"
summaryAssistantContent = "on"
summarySegmentDebugDump = true
`)
	report, err := CheckProductConfiguration(ConfigurationCheck{Repository: root})
	if err != nil {
		t.Fatalf("the check could not read the configuration: %v", err)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	// The values the loader filled in with placeholders must not be in the
	// report: they are the only secrets the run's configuration holds.
	for _, secret := range []string{
		"synthetic-offline-key-with-at-least-32-characters",
		"placeholder-proxy-injected",
		"synthetic-never-sent",
	} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("the report carries a key: %s", encoded)
		}
	}
	// And no field of the report is a place a key could hide.
	var walk func(value any)
	walk = func(value any) {
		switch typed := value.(type) {
		case map[string]any:
			for name, child := range typed {
				if strings.Contains(strings.ToLower(name), "key") ||
					strings.Contains(strings.ToLower(name), "secret") ||
					strings.Contains(strings.ToLower(name), "password") ||
					strings.Contains(strings.ToLower(name), "credential") {
					t.Fatalf("the report has a field a key could hide in: %q", name)
				}
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	var decoded any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	walk(decoded)
}

// A missing isolated entry is refused, not read as "the defaults".
func TestAMissingConfigurationIsRefused(t *testing.T) {
	root := t.TempDir()
	report, err := CheckProductConfiguration(ConfigurationCheck{Repository: root})
	if err == nil {
		t.Fatalf("the check accepted a checkout without a configuration: %+v", report)
	}
	if report.ConfigurationExists {
		t.Fatal("the report says the configuration is there")
	}
}

// A missing overlay is refused too.
//
// A run that hands the product an overlay the check cannot read is a run
// whose configuration nobody verified.
func TestAMissingOverlayIsRefused(t *testing.T) {
	root := aCheckoutWith(t, `
[agentConfig]
summarySegmentMode = "on"
summaryAssistantContent = "on"
summarySegmentDebugDump = true
`)
	report, err := CheckProductConfiguration(ConfigurationCheck{
		Repository: root, ScaledConfig: filepath.Join(t.TempDir(), "absent.toml"),
	})
	if err == nil {
		t.Fatalf("the check accepted a missing overlay: %+v", report)
	}
	if report.ScaledConfigExists {
		t.Fatal("the report says the overlay is there")
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
	report, err := CheckProductConfiguration(ConfigurationCheck{Repository: root})
	if err != nil {
		t.Fatalf("the check could not read the configuration: %v", err)
	}
	if report.SummaryAssistantContent == "on" {
		t.Fatal("the check read the switches out of a section the loader ignores")
	}
	if report.SummarySegmentDebugDump {
		t.Fatal("the check read the debug dump out of a section the loader ignores")
	}
	// The mode's own default is already on, so the wrong section is caught by
	// the two switches that are off by default; the mode's own reading is
	// covered by the overlay tests above.
}
