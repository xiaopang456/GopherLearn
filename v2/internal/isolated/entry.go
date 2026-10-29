package isolated

import (
	"context"
	"errors"
	"net/http"
	"time"
)

func Run(ctx context.Context, path string) error {
	s, err := Load(path)
	if err != nil {
		return err
	}
	app, err := Assemble(context.Background(), s, ProductionDependencies(s))
	if err != nil {
		return err
	}
	defer app.Close()
	return serve(ctx, app.Lifecycle, newHTTPServer(s.Address(), app.Handler))
}
func serve(signalCtx context.Context, l *Lifecycle, server *http.Server) error {
	errorsCh := make(chan error, 1)
	go func() { errorsCh <- server.ListenAndServe() }()
	var serveErr error
	select {
	case <-signalCtx.Done():
	case serveErr = <-errorsCh:
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	shutdownErr := server.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		shutdownErr = errors.Join(shutdownErr, server.Close())
	}
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	return errors.Join(serveErr, shutdownErr, l.Close())
}
