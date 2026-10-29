package hybrid

import (
	"GopherAI/entry"
	"context"
	"errors"
	"github.com/BurntSushi/toml"
	"github.com/xiaopang456/GopherLearn/conversation"
	"net"
	"net/http"
	"time"
)

type entrySettings struct {
	v2     *entry.Settings
	client conversation.ClientConfig
	chat   conversation.Options
}

func loadEntry(path string) (*entrySettings, error) {
	v2, err := entry.Load(path)
	if err != nil {
		return nil, err
	}
	// This executable is not a migration command. Do not inherit the native
	// entry's optional migration flag, including when explicitly set to true.
	if v2.Entry.Migrate {
		return nil, errors.New("hybrid entry refuses business migration")
	}
	var cfg struct {
		Ordinary map[string]string `toml:"ordinaryChat"`
	}
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	for _, key := range conversation.OrdinaryConfigKeys() {
		allowed[key] = true
	}
	for key := range cfg.Ordinary {
		if !allowed[key] {
			return nil, errors.New("unsupported ordinaryChat setting")
		}
	}
	client, chat, err := conversation.ConfigureClient(func(key string) string { return cfg.Ordinary[key] })
	if err != nil {
		return nil, err
	}
	return &entrySettings{v2: v2, client: client, chat: chat}, nil
}

type entryRuntime struct {
	dependencies    func(*entry.Settings) entry.Dependencies
	client          func(conversation.ClientConfig) (conversation.Model, error)
	listen          func(string, string) (net.Listener, error)
	server          func(string, http.Handler) *http.Server
	shutdownTimeout time.Duration
}

// Run loads only an explicitly named independent file. Production factories
// are reached only after both native and ordinary configuration validation.
func Run(ctx context.Context, path string) error {
	return runEntry(ctx, path, entryRuntime{entry.ProductionDependencies, conversation.NewClient, net.Listen, entry.NewHTTPServer, 15 * time.Second})
}

func runEntry(ctx context.Context, path string, runtime entryRuntime) (result error) {
	settings, err := loadEntry(path)
	if err != nil {
		return err
	}
	if runtime.dependencies == nil || runtime.client == nil || runtime.listen == nil || runtime.server == nil || runtime.shutdownTimeout <= 0 {
		return errors.New("incomplete hybrid entry factories")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	chat, err := runtime.client(settings.client)
	if err != nil {
		return err
	}
	app, err := Assemble(context.Background(), settings.v2, runtime.dependencies(settings.v2), chat, settings.chat)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, app.Close()) }()
	if err := ctx.Err(); err != nil {
		return err
	}
	server := runtime.server(settings.v2.Address(), app.Handler)
	if server == nil {
		return errors.New("nil hybrid HTTP server")
	}
	listener, err := runtime.listen("tcp", settings.v2.Address())
	if err != nil {
		return err
	}
	if listener == nil {
		return errors.New("nil hybrid listener")
	}
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- server.Serve(listener) }()
	var serveErr error
	var received bool
	select {
	case <-ctx.Done():
	case serveErr = <-serveErrors:
		received = true
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), runtime.shutdownTimeout)
	defer cancel()
	shutdownErr := server.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		shutdownErr = errors.Join(shutdownErr, server.Close())
	}
	if !received {
		serveErr = <-serveErrors
	}
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	return errors.Join(serveErr, shutdownErr)
}
