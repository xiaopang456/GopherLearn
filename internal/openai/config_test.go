package openai

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func env(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

// fullEnvironment is the minimum a valid openai_compatible deployment needs.
func fullEnvironment(extra map[string]string) map[string]string {
	values := map[string]string{
		EnvBaseURL: "https://api.example.test/v1",
		EnvAPIKey:  "sk-test-key-8f2c",
		EnvModel:   "some-chat-model",
	}
	for key, value := range extra {
		values[key] = value
	}
	return values
}

func TestConfigFromEnvironmentRequiresEveryCredentialField(t *testing.T) {
	for _, missing := range []string{EnvBaseURL, EnvAPIKey, EnvModel} {
		values := fullEnvironment(nil)
		delete(values, missing)
		_, err := ConfigFromEnvironment(env(values))
		if err == nil {
			t.Fatalf("configuration without %s was accepted", missing)
		}
		want := map[string]error{EnvBaseURL: ErrMissingBaseURL, EnvAPIKey: ErrMissingAPIKey, EnvModel: ErrMissingModel}[missing]
		if !errors.Is(err, want) {
			t.Fatalf("missing %s error = %v, want %v", missing, err, want)
		}
	}
	// A whitespace-only value is the same as no value: it cannot accidentally
	// become a bearer token of " ".
	if _, err := ConfigFromEnvironment(env(fullEnvironment(map[string]string{EnvAPIKey: "   "}))); !errors.Is(err, ErrMissingAPIKey) {
		t.Fatalf("blank key error = %v, want ErrMissingAPIKey", err)
	}
	if _, err := ConfigFromEnvironment(nil); !errors.Is(err, ErrMissingBaseURL) {
		t.Fatalf("nil lookup error = %v, want a required-field failure", err)
	}
}

func TestConfigFromEnvironmentAppliesDefaultsAndOverrides(t *testing.T) {
	config, err := ConfigFromEnvironment(env(fullEnvironment(nil)))
	if err != nil {
		t.Fatal(err)
	}
	if config.Timeout != DefaultTimeout || config.MaxOutputTokens != DefaultMaxOutputTokens {
		t.Fatalf("defaults not applied: %#v", config)
	}
	if config.EnableThinking != nil || config.Temperature != 0 {
		t.Fatalf("optional knobs must stay unset by default: %#v", config)
	}
	if config.EnableThinkingSetting() != "unset" {
		t.Fatalf("unset thinking flag reported as %q", config.EnableThinkingSetting())
	}

	config, err = ConfigFromEnvironment(env(fullEnvironment(map[string]string{
		EnvEnableThinking:  "true",
		EnvTemperature:     "0.3",
		EnvMaxOutputTokens: "256",
		EnvTimeout:         "45s",
	})))
	if err != nil {
		t.Fatal(err)
	}
	if config.EnableThinking == nil || !*config.EnableThinking {
		t.Fatalf("enable_thinking = %v, want an explicit true", config.EnableThinking)
	}
	if config.Temperature != 0.3 || config.MaxOutputTokens != 256 || config.Timeout != 45*time.Second {
		t.Fatalf("overrides not applied: %#v", config)
	}
	if config.EnableThinkingSetting() != "true" {
		t.Fatalf("summary state = %q", config.EnableThinkingSetting())
	}
}

func TestConfigFromEnvironmentRefusesUnusableValues(t *testing.T) {
	cases := []struct {
		name  string
		set   map[string]string
		would error
	}{
		{"negative tokens", map[string]string{EnvMaxOutputTokens: "-1"}, ErrInvalidMaxOutputTokens},
		{"zero tokens", map[string]string{EnvMaxOutputTokens: "0"}, ErrInvalidMaxOutputTokens},
		{"tokens not a number", map[string]string{EnvMaxOutputTokens: "many"}, ErrInvalidMaxOutputTokens},
		{"temperature above range", map[string]string{EnvTemperature: "2.5"}, ErrInvalidTemperature},
		{"temperature negative", map[string]string{EnvTemperature: "-0.1"}, ErrInvalidTemperature},
		{"temperature not a number", map[string]string{EnvTemperature: "hot"}, ErrInvalidTemperature},
		{"timeout without unit", map[string]string{EnvTimeout: "30"}, ErrInvalidTimeout},
		{"timeout zero", map[string]string{EnvTimeout: "0s"}, ErrInvalidTimeout},
		{"thinking not a bool", map[string]string{EnvEnableThinking: "maybe"}, ErrInvalidEnableThinking},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := ConfigFromEnvironment(env(fullEnvironment(testCase.set))); !errors.Is(err, testCase.would) {
				t.Fatalf("error = %v, want %v", err, testCase.would)
			}
		})
	}
}

func TestEndpointJoinsTheChatPathOnce(t *testing.T) {
	cases := []struct {
		base string
		want string
	}{
		{"https://api.example.test", "https://api.example.test/chat/completions"},
		{"https://api.example.test/", "https://api.example.test/chat/completions"},
		{"https://api.example.test/compatible-mode/v1", "https://api.example.test/compatible-mode/v1/chat/completions"},
		// Operators paste the full endpoint as often as the root; appending again
		// would produce a URL that 404s forever.
		{"https://api.example.test/v1/chat/completions", "https://api.example.test/v1/chat/completions"},
		{"http://127.0.0.1:8080/v1", "http://127.0.0.1:8080/v1/chat/completions"},
	}
	for _, testCase := range cases {
		config := Config{BaseURL: testCase.base, APIKey: "k", Model: "m"}
		if err := config.Validate(); err != nil {
			t.Fatalf("base %q rejected: %v", testCase.base, err)
		}
		got, err := config.endpoint()
		if err != nil {
			t.Fatal(err)
		}
		if got != testCase.want {
			t.Fatalf("base %q resolved to %q, want %q", testCase.base, got, testCase.want)
		}
	}
}

func TestEndpointRefusesUnusableAndCredentialBearingURLs(t *testing.T) {
	for _, base := range []string{
		"api.example.test",       // no scheme: this would become an http request to a nonsense host
		"ftp://api.example.test", // not an HTTP API
		"https://",               // no host
		"https://sk-secret-key@api.example.test/v1", // credentials belong in the header, not in a URL that ends up in logs
		"/just/a/path", // no host at all
	} {
		config := Config{BaseURL: base, APIKey: "k", Model: "m"}
		if err := config.Validate(); !errors.Is(err, ErrInvalidBaseURL) {
			t.Fatalf("base %q error = %v, want ErrInvalidBaseURL", base, err)
		}
	}
	// A query string on a well-formed base URL is usable; it is simply not carried
	// into the request, so a token in the query cannot ride along.
	config := Config{BaseURL: "https://api.example.test/v1?api_key=sk-abc", APIKey: "k", Model: "m"}
	endpoint, err := config.endpoint()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(endpoint, "api_key") {
		t.Fatalf("resolved endpoint kept the query credential: %q", endpoint)
	}
}

func TestEnableThinkingKeepsThreeDistinctStates(t *testing.T) {
	unset, err := ConfigFromEnvironment(env(fullEnvironment(nil)))
	if err != nil {
		t.Fatal(err)
	}
	if unset.EnableThinking != nil {
		t.Fatalf("unset parsed as %v, want nil so the field stays off the wire", unset.EnableThinking)
	}
	for _, raw := range []string{"false", "true"} {
		config, err := ConfigFromEnvironment(env(fullEnvironment(map[string]string{EnvEnableThinking: raw})))
		if err != nil {
			t.Fatal(err)
		}
		if config.EnableThinking == nil {
			t.Fatalf("%q set the flag to nil", raw)
		}
		if got := config.EnableThinkingSetting(); got != raw {
			t.Fatalf("explicit %q reported as %q", raw, got)
		}
	}
	// A configured false must survive Sanitize/Summary without printing the key.
	off := Config{BaseURL: "https://api.example.test/v1", APIKey: "sk-secret-abc", Model: "m", Timeout: time.Second, EnableThinking: pointerTo(false)}
	if !strings.Contains(off.Summary(), "enable_thinking=false") {
		t.Fatalf("summary = %q", off.Summary())
	}
	if strings.Contains(off.Summary(), "sk-secret-abc") {
		t.Fatalf("summary printed the key: %q", off.Summary())
	}
	if !strings.Contains(unset.Summary(), "enable_thinking=unset") {
		t.Fatalf("unset summary = %q", Config{BaseURL: "https://api.example.test/v1", APIKey: "k", Model: "m"}.Summary())
	}
}

func pointerTo(value bool) *bool { return &value }

func TestSanitizeAndSummaryNeverPrintTheKey(t *testing.T) {
	config := Config{BaseURL: "https://api.example.test/v1", APIKey: "sk-live-secret", Model: "m1", Timeout: 20 * time.Second}
	for _, text := range []string{
		config.Sanitize(errors.New("authority api.example.test rejected sk-live-secret")).Error(),
		config.Summary(),
	} {
		if strings.Contains(text, "sk-live-secret") {
			t.Fatalf("output leaks the key: %q", text)
		}
	}
	summary := config.Summary()
	for _, want := range []string{"m1", "api.example.test", "20s", "enable_thinking=unset"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary %q lost %q", summary, want)
		}
	}
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	if config.Sanitize(nil) != nil {
		t.Fatal("Sanitize invented an error")
	}
}

func TestNewValidatesBeforeItCanSendAnything(t *testing.T) {
	if _, err := New(Config{BaseURL: "https://api.example.test", Model: "m"}); !errors.Is(err, ErrMissingAPIKey) {
		t.Fatalf("error = %v, want ErrMissingAPIKey", err)
	}
	if _, err := New(Config{BaseURL: "not a url", APIKey: "k", Model: "m"}); !errors.Is(err, ErrInvalidBaseURL) {
		t.Fatalf("error = %v, want ErrInvalidBaseURL", err)
	}
	client, err := New(Config{BaseURL: "https://api.example.test/v1", APIKey: "k", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if client.Model() != "m" {
		t.Fatalf("Model() = %q", client.Model())
	}
}
