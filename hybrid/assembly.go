// Package hybrid assembles the retained ordinary Chat with the original V2 Agent.
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

// Assemble uses the exact database opened by the original V2 assembly. Ordinary
// Chat gets no Runner, tools, memory provider, or MQ collaborator.
func Assemble(ctx context.Context, settings *entry.Settings, deps entry.Dependencies, chat conversation.Model, options conversation.Options) (*Application, error) {
	if chat == nil || deps.Infrastructure.OpenDatabase == nil {
		return nil, errors.New("hybrid: explicit chat model and V2 infrastructure required")
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
	ordinary, err := conversation.New(&Store{db: db}, chat, options)
	if err != nil {
		return nil, errors.Join(err, agent.Close())
	}
	app := &Application{Agent: agent, Chat: ordinary}
	app.Handler = newHTTPHandler(db, ordinary, agent)
	return app, nil
}
