// Package entry makes the isolated V2 assembly callable by a separate module.
package entry

import (
	"GopherAI/internal/isolated"
	"context"
	"net/http"
)

type Settings = isolated.Settings
type Dependencies = isolated.Dependencies
type Infrastructure = isolated.Infrastructure
type ChunkStore = isolated.ChunkStore
type Application = isolated.Application

func Load(path string) (*Settings, error) { return isolated.Load(path) }
func Assemble(ctx context.Context, settings *Settings, deps Dependencies) (*Application, error) {
	return isolated.Assemble(ctx, settings, deps)
}

func ProductionDependencies(settings *Settings) Dependencies {
	return isolated.ProductionDependencies(settings)
}

func NewHTTPServer(address string, handler http.Handler) *http.Server {
	return isolated.NewHTTPServer(address, handler)
}
