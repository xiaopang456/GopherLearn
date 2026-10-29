package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolatedTemplate is the same independent-configuration example the hybrid
// assembly tests load. It stays the single source of the required keys so this
// entry cannot drift into its own idea of what a valid file looks like.
const isolatedTemplate = "../../../tutorial/v2-entry/isolated.example.toml"

// validConfig returns a file that passes every load-stage check, so a run that
// reaches the factories would have to open real resources. Each case below stops
// before that, which is what the assertions prove.
func validConfig(t *testing.T) (path, secret string) {
	t.Helper()
	data, err := os.ReadFile(isolatedTemplate)
	if err != nil {
		t.Fatal(err)
	}
	secret = "synthetic-never-sent-key"
	text := strings.ReplaceAll(string(data), `key = ""`, `key = "`+strings.Repeat(secret, 2)+`"`)
	text = strings.ReplaceAll(text, `chatKey = ""`, `chatKey = "`+secret+`"`)
	text = strings.ReplaceAll(text, `embeddingKey = ""`, `embeddingKey = "`+secret+`"`)
	text += "\n[ordinaryChat]\nGOPHERLEARN_AI_PROVIDER = \"openai_compatible\"\n" +
		"GOPHERLEARN_OPENAI_BASE_URL = \"https://provider.invalid/v1\"\n" +
		"GOPHERLEARN_OPENAI_API_KEY = \"" + secret + "\"\n" +
		"GOPHERLEARN_OPENAI_MODEL = \"synthetic-model\"\n"
	path = filepath.Join(t.TempDir(), "integrated.toml")
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return path, secret
}

func TestCLIUsageRefusalBeforeAnyBanner(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"missing config flag", nil},
		{"blank config value", []string{"-config", "   "}},
		{"positional argument", []string{"extra"}},
		{"unknown flag", []string{"-deploy"}},
	} {
		var out, errOut bytes.Buffer
		if code := runCLI(context.Background(), tc.args, &out, &errOut); code != 2 {
			t.Fatalf("%s: exit %d, want 2", tc.name, code)
		}
		if errOut.Len() == 0 {
			t.Fatalf("%s: refusal printed nothing", tc.name)
		}
		if out.Len() > 0 {
			t.Fatalf("%s: announced an entry it never started: %q", tc.name, out.String())
		}
	}
}

func TestCLIAnnouncesWhichEntryItIsAndKeepsSecretsUnsent(t *testing.T) {
	path, secret := validConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, errOut bytes.Buffer
	code := runCLI(ctx, []string{"-config", path}, &out, &errOut)
	if code == 0 {
		t.Fatal("a cancelled context must not report a completed run")
	}
	if strings.TrimSpace(out.String()) != entryLine {
		t.Fatalf("startup line = %q, want %q", out.String(), entryLine)
	}
	if errOut.Len() == 0 {
		t.Fatal("failed run printed no refusal")
	}
	for _, stream := range []string{out.String(), errOut.String()} {
		if strings.Contains(stream, secret) || strings.Contains(stream, "synthetic-model") {
			t.Fatalf("configuration value reached the operator: %q", stream)
		}
	}
}

func TestCLIInvalidConfigurationDoesNotPrintValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid.toml")
	if err := os.WriteFile(path, []byte("INVALID_PRIVATE_MARKER ["), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := runCLI(context.Background(), []string{"-config", path}, &out, &errOut); code == 0 {
		t.Fatal("invalid configuration was accepted")
	} else if strings.Contains(out.String()+errOut.String(), "INVALID_PRIVATE_MARKER") {
		t.Fatal("invalid configuration echoed its own text")
	}
}
