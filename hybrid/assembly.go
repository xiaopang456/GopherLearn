// Package hybrid assembles the integrated entry: one chat model, shared by Agent turns and conversation turns.
package hybrid

import (
	"GopherAI/config"
	"GopherAI/entry"
	"context"
	"errors"
	"github.com/xiaopang456/GopherLearn/conversation"
	"gorm.io/gorm"
	"net/http"
)

type Application struct {
	Agent   *entry.Application
	Chat    *conversation.Service
	Handler http.Handler
}

func (a *Application) Close() error { return a.Agent.Close() }

// Assemble uses the exact database opened by the original V2 assembly.
//
// A nil chat model is the production shape: this build carries one engine, and the
// learning Agent's chat model answers a conversation turn as well as an Agent turn. The
// ordinary service is still constructed when a caller hands it a model, which is how the
// store and the retired path stay under test after the entry stopped wiring them.
func Assemble(ctx context.Context, settings *entry.Settings, deps entry.Dependencies, chat conversation.Model, options conversation.Options) (*Application, error) {
	if deps.Infrastructure.OpenDatabase == nil {
		return nil, errors.New("hybrid: V2 infrastructure required")
	}
	var db *gorm.DB
	open := deps.Infrastructure.OpenDatabase
	deps.Infrastructure.OpenDatabase = func(ctx context.Context, c *config.Config) (*gorm.DB, error) {
		var err error
		db, err = open(ctx, c)
		return db, err
	}
	agent, err := entry.Assemble(ctx, settings, deps)
	if err != nil {
		return nil, err
	}
	var ordinary *conversation.Service
	if chat != nil {
		if ordinary, err = conversation.New(&Store{db: db}, chat, options); err != nil {
			return nil, errors.Join(err, agent.Close())
		}
	}
	app := &Application{Agent: agent, Chat: ordinary}
	app.Handler = newHTTPHandler(db, ordinary, agent)
	return app, nil
}
