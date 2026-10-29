package app

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/xiaopang456/GopherLearn/internal/openai"
)

// Phase 10A's contract, in two halves. The first reads compose.provider.yml and
// .env.provider.example the way an operator would: is the real-provider mode actually
// opt-in, does it arrive with no credential written anywhere tracked, and does it leave the
// topology compose.app.yml describes alone. The second half asks the binary itself what it
// does with the environment that file produces, because "startup fails when the key is
// missing" is only worth believing if the refusal belongs to the code and not to a comment.
//
// Nothing here contacts a provider: ConfigFromEnvironment parses and validates, it never
// dials, so this suite runs with no network, no daemon and no credential.

const (
	providerOverrideFile = "compose.provider.yml"
	providerEnvExample   = ".env.provider.example"
	// A reserved TLD, never resolved: this value only has to look like an endpoint while a
	// configuration is being validated in memory.
	providerTestBaseURL = "https://provider.invalid.test/v1"
	providerTestKey     = "10a-local-check-not-a-real-key"
	providerTestModel   = "some-model-name"
)

func TestProviderOverrideIsOptInAndCarriesNoCredential(t *testing.T) {
	override := repoText(t, providerOverrideFile)
	stack := repoText(t, "compose.app.yml")
	database := repoText(t, "compose.mysql.yml")
	example := repoText(t, providerEnvExample)

	// Default mode must stay reachable without this file existing: neither base file includes
	// it, so the documented command keeps answering from echo with no credential and no extra
	// argument.
	for name, text := range map[string]string{"compose.app.yml": stack, "compose.mysql.yml": database} {
		if strings.Contains(text, providerOverrideFile) {
			t.Errorf("%s references %s, so provider mode is no longer opt-in", name, providerOverrideFile)
		}
		if regexp.MustCompile(`(?m)^\s*include:`).MatchString(text) {
			t.Errorf("%s uses compose `include:`, which would make the override unavoidable", name)
		}
	}

	backend := sectionOf(t, stack, "  backend:")
	for _, required := range []string{
		EnvProvider + ": echo",
		EnvLocalLearningModel + ": deterministic",
	} {
		if !strings.Contains(backend, required) {
			t.Errorf("compose.app.yml lost %q: the default stack must need no provider", required)
		}
	}

	// What the override flips, and nothing else. Reading assignments rather than
	// substring-hunting means a comment that merely mentions a key cannot satisfy the check.
	settings := environmentAssignments(t, override)
	if got := settings[EnvProvider]; got != string(ProviderOpenAI) {
		t.Errorf("%s sets %s to %q, want %q", providerOverrideFile, EnvProvider, got, ProviderOpenAI)
	}
	// The scripted model must be blanked *here*, not merely omitted: compose merges two
	// environment maps per key, so compose.app.yml's `deterministic` survives unless this
	// file overwrites it with nothing. Leaving it set is the two-model-sources bug.
	raw, assigned := settings[EnvLocalLearningModel]
	if !assigned {
		t.Errorf("%s must blank %s explicitly, or the default stack's value is inherited",
			providerOverrideFile, EnvLocalLearningModel)
	}
	if raw != "" {
		t.Errorf("%s leaves the scripted model on: %s=%q", providerOverrideFile, EnvLocalLearningModel, raw)
	}
	inherited := environmentAssignments(t, backend)
	for key := range settings {
		if _, known := inherited[key]; !known {
			t.Errorf("%s introduces a setting (%s) the default stack never had", providerOverrideFile, key)
		}
	}

	// No credential, in either of the two ways that happens: a literal value in a tracked
	// file, or an interpolation whose resolved form `docker compose config` prints. Both
	// failures name only the variable, never the matched text: a test log is exactly as
	// public as the file it is complaining about, and if an operator did paste a real key in,
	// printing it back in the failure would spread the leak rather than report it.
	if match := regexp.MustCompile(`(GOPHERLEARN_OPENAI_[A-Z_]+):\s*\S`).FindStringSubmatch(override); match != nil {
		t.Errorf("%s writes a provider value into the file (key %s has a literal value); it may only arrive from env_file",
			providerOverrideFile, match[1])
	}
	if match := regexp.MustCompile(`(\$\{[A-Za-z_]*(?:OPENAI|PASSWORD|SECRET|TOKEN|DSN|KEY|CREDENTIAL)[A-Za-z_]*).`).FindStringSubmatch(override); match != nil {
		t.Errorf("%s interpolates a secret (variable %s), which docker compose config would print",
			providerOverrideFile, match[1])
	}
	for _, text := range []struct {
		name string
		body string
	}{{providerOverrideFile, override}, {providerEnvExample, example}} {
		if count := regexp.MustCompile(`sk-[A-Za-z0-9_-]{4,}`).FindAllString(text.body, -1); len(count) > 0 {
			t.Errorf("%s contains %d API-key-shaped literal(s); the values are not repeated here", text.name, len(count))
		}
	}

	// Provider values arrive only from the local file, and the file the override names must be
	// the untracked one: pointing it at the tracked example would read blanks forever.
	envFiles := strings.Join(sequenceValues(t, override, "env_file"), "|")
	if !strings.Contains(envFiles, ".env.provider.local") {
		t.Errorf("provider settings must come from an env_file named .env.provider.local, got %q", envFiles)
	}
	if strings.Contains(envFiles, providerEnvExample) {
		t.Errorf("the tracked example must not be what the container reads; it carries no values: %q", envFiles)
	}
	if !strings.Contains(repoText(t, ".gitignore"), "*.local") {
		t.Error(".gitignore no longer ignores *.local, so .env.provider.local could be committed")
	}
	// The default stack's own credential file must stay provider-free, or "no credential
	// needed" would be true of the compose file and false of the run: one leftover
	// GOPHERLEARN_OPENAI_MODEL already rules out the scripted quiz model. Only the variable
	// name is reported, never the line's value.
	for _, line := range strings.Split(repoText(t, ".env.compose.example"), "\n") {
		name := strings.TrimSpace(strings.SplitN(strings.TrimSpace(line), "=", 2)[0])
		if name == openai.EnvBaseURL || name == openai.EnvAPIKey || name == openai.EnvModel {
			t.Errorf(".env.compose.example names a provider setting (%s), so the default stack is no longer credential-free", name)
		}
	}
	for _, line := range strings.Split(example, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "GOPHERLEARN_OPENAI_") || !strings.Contains(trimmed, "=") {
			continue
		}
		if value := strings.TrimSpace(strings.SplitN(trimmed, "=", 2)[1]); value != "" {
			t.Errorf("%s assigns a value to %s; the value is not repeated here", providerEnvExample,
				strings.TrimSpace(strings.SplitN(trimmed, "=", 2)[0]))
		}
	}
	for _, key := range []string{openai.EnvBaseURL, openai.EnvAPIKey, openai.EnvModel} {
		if !strings.Contains(example, key+"=") {
			t.Errorf("%s does not document %s", providerEnvExample, key)
		}
	}

	// Topology is compose.app.yml's business: this file flips a model source and nothing else.
	for _, forbidden := range []string{"ports:", "networks:", "volumes:", "build:", "image:",
		"container_name:", "restart:", "depends_on:"} {
		if strings.Contains(override, forbidden) {
			t.Errorf("%s must not redefine %s - frontend, MySQL and Nginx topology is unchanged",
				providerOverrideFile, forbidden)
		}
	}
	if services := serviceNames(t, override); len(services) != 1 || services[0] != "backend" {
		t.Errorf("%s must define only the backend service, got %v", providerOverrideFile, services)
	}
	for _, forbidden := range []string{"redis", "rabbitmq", "valkey", "minio", "kafka", "nats", "memcached"} {
		if strings.Contains(strings.ToLower(withoutComments(override)), forbidden) {
			t.Errorf("%s mentions %q; this stack runs no such service", providerOverrideFile, forbidden)
		}
	}
}

// TestProviderModeRefusesBadStartupCombinations asks the code, not the compose file, what
// happens to the environment the override builds. Every row is a configuration that must stop
// the process before it serves anything: provider mode never quietly answers from echo, and
// the scripted quiz model never rides into a paid deployment.
func TestProviderModeRefusesBadStartupCombinations(t *testing.T) {
	full := map[string]string{
		EnvProvider:           string(ProviderOpenAI),
		openai.EnvBaseURL:     providerTestBaseURL,
		openai.EnvAPIKey:      providerTestKey,
		openai.EnvModel:       providerTestModel,
		EnvLocalLearningModel: "",
	}
	without := func(keys ...string) map[string]string {
		skip := map[string]bool{}
		for _, key := range keys {
			skip[key] = true
		}
		values := map[string]string{}
		for key, value := range full {
			if !skip[key] {
				values[key] = value
			}
		}
		return values
	}

	cases := []struct {
		name   string
		values map[string]string
		want   error
	}{
		{
			name: "provider plus the scripted quiz model",
			values: map[string]string{
				EnvProvider: string(ProviderOpenAI), openai.EnvBaseURL: providerTestBaseURL,
				openai.EnvAPIKey: providerTestKey, openai.EnvModel: providerTestModel,
				EnvLocalLearningModel: "deterministic",
			},
			want: ErrLocalLearningModelNeedsEcho,
		},
		{
			// Even echo refuses when one provider setting was left behind: the scripted
			// model may only run where nothing was configured to dial out, so a
			// half-cleaned environment cannot claim both.
			name: "echo with one provider setting left in",
			values: map[string]string{
				EnvProvider: "echo", EnvLocalLearningModel: "deterministic", openai.EnvModel: providerTestModel,
			},
			want: ErrLocalLearningModelNeedsEcho,
		},
		{"provider without a base url", without(openai.EnvBaseURL), openai.ErrMissingBaseURL},
		{"provider without a key", without(openai.EnvAPIKey), openai.ErrMissingAPIKey},
		{"provider without a model", without(openai.EnvModel), openai.ErrMissingModel},
		{
			name: "provider key that is only whitespace",
			values: map[string]string{
				EnvProvider: string(ProviderOpenAI), openai.EnvBaseURL: providerTestBaseURL,
				openai.EnvAPIKey: "   ", openai.EnvModel: providerTestModel,
			},
			want: openai.ErrMissingAPIKey,
		},
		{
			name: "provider url carrying credentials",
			values: map[string]string{
				EnvProvider:       string(ProviderOpenAI),
				openai.EnvBaseURL: "https://user:hunter2@provider.invalid.test/v1",
				openai.EnvAPIKey:  providerTestKey, openai.EnvModel: providerTestModel,
			},
			want: openai.ErrInvalidBaseURL,
		},
		{
			name: "provider url with no scheme",
			values: map[string]string{
				EnvProvider: string(ProviderOpenAI), openai.EnvBaseURL: "provider.invalid.test",
				openai.EnvAPIKey: providerTestKey, openai.EnvModel: providerTestModel,
			},
			want: openai.ErrInvalidBaseURL,
		},
		{
			name:   "provider url that is not a url at all",
			values: map[string]string{EnvProvider: string(ProviderOpenAI), openai.EnvBaseURL: ":/not a url", openai.EnvAPIKey: providerTestKey, openai.EnvModel: providerTestModel},
			want:   openai.ErrInvalidBaseURL,
		},
		{"unknown provider value", map[string]string{EnvProvider: "anthropic"}, ErrUnsupportedProvider},
		{
			name:   "unknown scripted model value",
			values: map[string]string{EnvProvider: "echo", EnvLocalLearningModel: "gpt"},
			want:   ErrUnsupportedLocalLearningModel,
		},
		{
			name:   "provider temperature out of range",
			values: map[string]string{EnvProvider: string(ProviderOpenAI), openai.EnvBaseURL: providerTestBaseURL, openai.EnvAPIKey: providerTestKey, openai.EnvModel: providerTestModel, openai.EnvTemperature: "3"},
			want:   openai.ErrInvalidTemperature,
		},
		{
			name:   "provider timeout unparsable",
			values: map[string]string{EnvProvider: string(ProviderOpenAI), openai.EnvBaseURL: providerTestBaseURL, openai.EnvAPIKey: providerTestKey, openai.EnvModel: providerTestModel, openai.EnvTimeout: "soon"},
			want:   openai.ErrInvalidTimeout,
		},
		{
			// The two shapes that must start: the default stack as compose.app.yml
			// describes it, and provider mode with a filled-in local file.
			name:   "default stack, no provider",
			values: map[string]string{EnvProvider: "echo", EnvLocalLearningModel: "deterministic"},
			want:   nil,
		},
		{name: "provider fully configured", values: full, want: nil},
		{name: "provider fully configured, scripted key absent entirely", values: without(EnvLocalLearningModel), want: nil},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			lookup := func(key string) string { return test.values[key] }
			config, err := ConfigFromEnvironment(lookup)
			if test.want == nil {
				if err != nil {
					t.Fatalf("refused a configuration that must start: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("accepted a configuration that must stop startup: provider=%q scripted=%q",
					config.Provider, config.LocalLearningModel)
			}
			if !errors.Is(err, test.want) {
				t.Errorf("refused for the wrong reason: got %v, want %v", err, test.want)
			}
			// A refusal is what the process prints before it exits, so a key in this text is a
			// key in a log and eventually in an evidence file.
			if strings.Contains(err.Error(), providerTestKey) {
				t.Errorf("startup refusal contains the api key: %s", err)
			}
			if sanitized := config.Model.Sanitize(err); sanitized != nil && strings.Contains(sanitized.Error(), providerTestKey) {
				t.Errorf("sanitized refusal contains the api key: %s", sanitized)
			}
			if sanitized := config.Model.Sanitize(err); sanitized != nil && strings.Contains(sanitized.Error(), "hunter2") {
				t.Errorf("sanitized refusal leaks url credentials: %s", sanitized)
			}
		})
	}
}

// TestDefaultAndProviderConfigsAreDistinguishable pins what the two modes resolve to from the
// files themselves, so a future edit to the override cannot quietly make provider mode mean
// echo, or make the default stack start naming a provider.
func TestDefaultAndProviderConfigsAreDistinguishable(t *testing.T) {
	override := environmentAssignments(t, repoText(t, providerOverrideFile))
	provider, err := ConfigFromEnvironment(func(key string) string {
		switch key {
		case openai.EnvBaseURL:
			return providerTestBaseURL
		case openai.EnvAPIKey:
			return providerTestKey
		case openai.EnvModel:
			return providerTestModel
		default:
			return override[key]
		}
	})
	if err != nil {
		t.Fatalf("provider mode refused to start: %v", err)
	}
	if provider.Provider != ProviderOpenAI {
		t.Errorf("provider mode resolved to %q, want %q", provider.Provider, ProviderOpenAI)
	}
	if provider.LocalLearningModel != "" {
		t.Errorf("provider mode still runs the scripted quiz model: %q", provider.LocalLearningModel)
	}
	if provider.Model.APIKey != providerTestKey {
		t.Error("provider mode did not read the key from the environment")
	}
	if summary := provider.Model.Summary(); strings.Contains(summary, providerTestKey) {
		t.Errorf("the startup summary prints the api key: %s", summary)
	}

	stack := environmentAssignments(t, sectionOf(t, repoText(t, "compose.app.yml"), "  backend:"))
	def, err := ConfigFromEnvironment(func(key string) string { return stack[key] })
	if err != nil {
		t.Fatalf("the default stack refused to start: %v", err)
	}
	if def.Provider != ProviderEcho || def.LocalLearningModel != "deterministic" {
		t.Errorf("default stack resolved to provider=%q scripted=%q, want echo + deterministic",
			def.Provider, def.LocalLearningModel)
	}
	for _, key := range []string{openai.EnvBaseURL, openai.EnvAPIKey, openai.EnvModel} {
		if strings.TrimSpace(stack[key]) != "" {
			t.Errorf("the default stack names %s, so a credential-free run would be refused", key)
		}
	}
}

// environmentAssignments collects `KEY: value` pairs at any indentation from these small
// files. Text rather than a YAML parse on purpose: the thing that must hold is what compose
// merges and what an operator reads, and empty values have to be distinguishable from absent
// ones - which a decode into map[string]string would erase.
func environmentAssignments(t *testing.T, text string) map[string]string {
	t.Helper()
	values := map[string]string{}
	for _, line := range strings.Split(withoutComments(text), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "-") {
			continue
		}
		key, raw, found := strings.Cut(trimmed, ":")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		if !strings.HasPrefix(key, "GOPHERLEARN_") {
			continue
		}
		values[key] = yamlScalar(raw)
	}
	return values
}

// yamlScalar unquotes one YAML scalar so `KEY: ""` reads as the empty string it sets, while
// an absent key stays distinguishable by not being in the map at all.
func yamlScalar(raw string) string {
	value := strings.TrimSpace(raw)
	if len(value) >= 2 && (value[0] == '"' && value[len(value)-1] == '"' || value[0] == '\'' && value[len(value)-1] == '\'') {
		return value[1 : len(value)-1]
	}
	return value
}

// sequenceValues lists the `- item` entries under one mapping key, for reading env_file.
func sequenceValues(t *testing.T, text, key string) []string {
	t.Helper()
	marker := key + ":"
	start := strings.Index(text, marker)
	if start < 0 {
		t.Fatalf("%q is missing", marker)
	}
	var values []string
	for _, line := range strings.Split(text[start+len(marker):], "\n") {
		trimmed := strings.TrimSpace(line)
		// The remainder of the key line may hold nothing but a merge tag (`env_file:
		// !override`), which is not the end of the sequence.
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "!") {
			continue
		}
		if !strings.HasPrefix(trimmed, "- ") {
			break
		}
		values = append(values, yamlScalar(strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))))
	}
	if len(values) == 0 {
		t.Fatalf("%q has no sequence entries", marker)
	}
	return values
}

// serviceNames lists the services one compose file defines.
func serviceNames(t *testing.T, text string) []string {
	t.Helper()
	start := strings.Index(text, "services:")
	if start < 0 {
		t.Fatal("no services: section")
	}
	var names []string
	for _, line := range strings.Split(text[start:], "\n") {
		if match := regexp.MustCompile(`^  ([a-z][a-z0-9_-]*):\s*$`).FindStringSubmatch(line); match != nil {
			names = append(names, match[1])
		}
	}
	if len(names) == 0 {
		t.Fatal("services: section defines nothing")
	}
	return names
}
