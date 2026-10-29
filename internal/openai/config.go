// Package openai adapts model.ChatModel onto an OpenAI-compatible HTTP chat
// endpoint. It speaks the wire format only: no other package in this project
// learns that a provider exists, and no provider SDK is imported.
//
// Two rules hold the security boundary. The API key travels in exactly one
// place, the Authorization header, and is never written into a URL, a request
// body, or an error message. And nothing a provider says is treated as
// instruction: response bodies are decoded into data and nothing else.
package openai

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Environment variable names read by ConfigFromEnvironment.
const (
	EnvBaseURL         = "GOPHERLEARN_OPENAI_BASE_URL"
	EnvAPIKey          = "GOPHERLEARN_OPENAI_API_KEY"
	EnvModel           = "GOPHERLEARN_OPENAI_MODEL"
	EnvEnableThinking  = "GOPHERLEARN_OPENAI_ENABLE_THINKING"
	EnvTemperature     = "GOPHERLEARN_OPENAI_TEMPERATURE"
	EnvMaxOutputTokens = "GOPHERLEARN_OPENAI_MAX_OUTPUT_TOKENS"
	EnvTimeout         = "GOPHERLEARN_OPENAI_TIMEOUT"
)

var (
	// ErrMissingBaseURL reports openai_compatible mode without an endpoint.
	ErrMissingBaseURL = errors.New("openai: " + EnvBaseURL + " is required")
	// ErrMissingAPIKey reports openai_compatible mode without a key. A provider
	// that needs no key must not be reachable through this mode by accident.
	ErrMissingAPIKey = errors.New("openai: " + EnvAPIKey + " is required")
	// ErrMissingModel reports openai_compatible mode without a model name.
	ErrMissingModel = errors.New("openai: " + EnvModel + " is required")
	// ErrInvalidBaseURL reports an endpoint that is not an http(s) URL with a
	// host. Credentials in the URL are refused outright: the key belongs in the
	// Authorization header, and a URL carrying one ends up in logs.
	ErrInvalidBaseURL = errors.New("openai: " + EnvBaseURL + " must be an http(s) URL without credentials")
	// ErrInvalidTimeout reports an unparseable or non-positive timeout.
	ErrInvalidTimeout = errors.New("openai: " + EnvTimeout + " must be a positive Go duration")
	// ErrInvalidWrapTransport reports a WrapTransport that returns nothing: a
	// caller that meant to observe the requests would otherwise silently get the
	// unwrapped client, which is the one outcome this field must never allow.
	ErrInvalidWrapTransport = errors.New("openai: Config.WrapTransport must return a non-nil http.RoundTripper")
	// ErrInvalidMaxOutputTokens reports a non-positive token budget.
	ErrInvalidMaxOutputTokens = errors.New("openai: " + EnvMaxOutputTokens + " must be a positive integer")
	// ErrInvalidTemperature reports a temperature outside [0, 2].
	ErrInvalidTemperature = errors.New("openai: " + EnvTemperature + " must be a number between 0 and 2")
	// ErrInvalidEnableThinking reports a value that is not a boolean.
	ErrInvalidEnableThinking = errors.New("openai: " + EnvEnableThinking + " must be true or false")
)

// Defaults for the optional knobs. The timeout is generous enough for a long
// answer and short enough that a dead provider cannot hold a request forever.
const (
	DefaultTimeout         = 30 * time.Second
	DefaultMaxOutputTokens = 1024
	// chatCompletionsPath is appended to a base URL that does not already carry
	// it, so an operator can paste either the service root or the full endpoint.
	chatCompletionsPath = "/chat/completions"
)

// Config is the resolved OpenAI-compatible client configuration. Its generation
// knobs are defaults for every call this client makes; a request may still carry
// its own value, which wins. See Client.body for the full precedence.
type Config struct {
	BaseURL string
	APIKey  string
	Model   string
	// Timeout bounds two things: waiting for the response headers, and waiting
	// for the next piece of data once a stream is open. A stream that keeps
	// producing text is never cut off for taking a long time overall.
	Timeout time.Duration
	// Temperature is the default sampling temperature. Zero means "ask nothing
	// of the provider", which leaves the provider's own default in force.
	Temperature float64
	// MaxOutputTokens is the default reply budget.
	MaxOutputTokens int
	// EnableThinking records whether the operator configured the Qwen-style
	// extended field. nil means unset, and then the field never goes on the wire:
	// it is not part of the OpenAI schema, so a strict provider could reject it.
	EnableThinking *bool
	// WrapTransport lets a caller observe and bound the HTTP requests this client
	// actually makes. nil - the default, and what every environment configuration
	// produces - leaves both internal clients built exactly as before.
	//
	// It is a wrapper, not a replacement: the function receives the transport this
	// client would already have used (the shared http.DefaultTransport for
	// non-streaming calls, and a private clone carrying ResponseHeaderTimeout for
	// streams) and returns something that delegates to it. Two reasons the shape
	// matters: the non-streaming client's overall Timeout and the streaming
	// client's header/idle bounds are different contracts, so one injected
	// http.Client could not serve both without giving a long stream a whole-stream
	// deadline; and mutating http.DefaultTransport or sharing one transport between
	// callers would let this client affect, or be affected by, the rest of the
	// process. A wrapper must be safe for concurrent use, must not mutate the
	// transport it is handed, and must return non-nil - returning nil is refused as
	// a configuration error rather than silently falling back.
	//
	// It is a pointer to a one-field struct rather than a bare func because a func
	// field makes Config itself non-comparable, and Config is compared with == by
	// callers (internal/app asserts a refused configuration yields a zero Config).
	// Discovering that during implementation is the point of recording it here.
	WrapTransport *TransportWrapper
}

// TransportWrapper carries the request-observation hook. See Config.WrapTransport
// for why it is reached through a pointer.
type TransportWrapper struct {
	RoundTrip func(http.RoundTripper) http.RoundTripper
}

// EnableThinkingSetting names the tri-state for logs and tests.
func (c Config) EnableThinkingSetting() string {
	if c.EnableThinking == nil {
		return "unset"
	}
	if *c.EnableThinking {
		return "true"
	}
	return "false"
}

// ConfigFromEnvironment reads the seven supported variables. It is only called
// when the process selected openai_compatible mode: an unset configuration must
// stop startup there rather than quietly fall back to a placeholder model, which
// would keep a deployment running while it answers nobody from the knowledge
// base it appears to be using.
func ConfigFromEnvironment(lookup func(string) string) (Config, error) {
	if lookup == nil {
		lookup = func(string) string { return "" }
	}
	config := Config{
		BaseURL:         strings.TrimSpace(lookup(EnvBaseURL)),
		APIKey:          strings.TrimSpace(lookup(EnvAPIKey)),
		Model:           strings.TrimSpace(lookup(EnvModel)),
		Timeout:         DefaultTimeout,
		MaxOutputTokens: DefaultMaxOutputTokens,
	}

	if raw := strings.TrimSpace(lookup(EnvEnableThinking)); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, ErrInvalidEnableThinking
		}
		config.EnableThinking = &parsed
	}
	if raw := strings.TrimSpace(lookup(EnvTemperature)); raw != "" {
		parsed, err := strconv.ParseFloat(raw, 64)
		if err != nil || parsed < 0 || parsed > 2 {
			return Config{}, ErrInvalidTemperature
		}
		config.Temperature = parsed
	}
	if raw := strings.TrimSpace(lookup(EnvMaxOutputTokens)); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			return Config{}, ErrInvalidMaxOutputTokens
		}
		config.MaxOutputTokens = parsed
	}
	if raw := strings.TrimSpace(lookup(EnvTimeout)); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed <= 0 {
			return Config{}, ErrInvalidTimeout
		}
		config.Timeout = parsed
	}

	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

// Validate checks the required fields and normalizes nothing: a caller that wants
// trimming must trim before calling.
func (c Config) Validate() error {
	if strings.TrimSpace(c.BaseURL) == "" {
		return ErrMissingBaseURL
	}
	if _, err := c.endpoint(); err != nil {
		return err
	}
	if strings.TrimSpace(c.APIKey) == "" {
		return ErrMissingAPIKey
	}
	if strings.TrimSpace(c.Model) == "" {
		return ErrMissingModel
	}
	if c.Timeout < 0 {
		return ErrInvalidTimeout
	}
	if c.MaxOutputTokens < 0 {
		return ErrInvalidMaxOutputTokens
	}
	if c.Temperature < 0 || c.Temperature > 2 {
		return ErrInvalidTemperature
	}
	return nil
}

// endpoint resolves the absolute chat URL. A base URL that already ends in
// /chat/completions is used as given; anything else is treated as a service root
// and joined with the documented path.
func (c Config) endpoint() (string, error) {
	raw := strings.TrimSpace(c.BaseURL)
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "" && parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.Host == "" {
		return "", fmt.Errorf("%w: %q", ErrInvalidBaseURL, hostOf(raw))
	}
	if parsed.User != nil {
		return "", fmt.Errorf("%w: credentials in the URL are refused", ErrInvalidBaseURL)
	}
	path := strings.TrimRight(parsed.Path, "/")
	if !strings.HasSuffix(path, chatCompletionsPath) {
		path = path + chatCompletionsPath
	}
	parsed.Path = path
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

// hostOf keeps an unusable base URL out of an error message except for its
// host, which is what an operator needs to fix the setting.
func hostOf(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "<unparseable>"
	}
	return parsed.Host
}

// Sanitize removes the API key from an error so a startup failure can be logged
// without publishing a credential. The base URL is not a secret, the key is.
func (c Config) Sanitize(err error) error {
	if err == nil {
		return nil
	}
	key := strings.TrimSpace(c.APIKey)
	if key == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), key, "***"))
}

// Summary describes the active configuration for the startup log without
// revealing the key or any prompt content.
func (c Config) Summary() string {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	maxTokens := c.MaxOutputTokens
	if maxTokens <= 0 {
		maxTokens = DefaultMaxOutputTokens
	}
	return fmt.Sprintf("provider=openai_compatible model=%s base_host=%s temperature=%s max_output_tokens=%d enable_thinking=%s timeout=%s (first byte and stream idle)",
		c.Model, hostOf(c.BaseURL), strconv.FormatFloat(c.Temperature, 'f', -1, 64), maxTokens, c.EnableThinkingSetting(), timeout)
}
