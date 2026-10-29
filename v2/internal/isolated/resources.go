package isolated

import (
	"GopherAI/common/mysql"
	rediscommon "GopherAI/common/redis"
	"GopherAI/config"
	"context"
	"fmt"
	redisclient "github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"net"
	"strconv"
)

type Infrastructure struct {
	OpenDatabase       func(context.Context, *config.Config) (*gorm.DB, error)
	InitializeDatabase func(*gorm.DB) error
	OpenRedis          func(*config.Config) (*redisclient.Client, error)
}

func ProductionInfrastructure() Infrastructure {
	return Infrastructure{
		OpenDatabase: func(_ context.Context, c *config.Config) (*gorm.DB, error) {
			if err := config.Use(c); err != nil {
				return nil, err
			}
			return mysql.OpenConfigured()
		},
		InitializeDatabase: mysql.InitializeSchema,
		OpenRedis: func(c *config.Config) (*redisclient.Client, error) {
			return redisclient.NewClient(&redisclient.Options{Addr: net.JoinHostPort(c.RedisHost, strconv.Itoa(c.RedisPort)), Password: c.RedisPassword, DB: c.RedisDb, Protocol: 2}), nil
		},
	}
}
func openResources(ctx context.Context, s *Settings, infra Infrastructure, l *Lifecycle) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if infra.OpenDatabase == nil || infra.InitializeDatabase == nil || infra.OpenRedis == nil {
		return fmt.Errorf("incomplete infrastructure factories")
	}
	if err := config.Use(s.Config); err != nil {
		return err
	}
	db, err := infra.OpenDatabase(ctx, s.Config)
	if err != nil {
		return fmt.Errorf("open isolated database: %w", err)
	}
	if db == nil {
		return fmt.Errorf("database factory returned nil")
	}
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	l.closers = append(l.closers, func() error { mysql.DB = nil; return sqlDB.Close() })
	mysql.DB = db
	if s.Entry.Migrate {
		if err := infra.InitializeDatabase(db); err != nil {
			return fmt.Errorf("initialize isolated schema: %w", err)
		}
	}
	client, err := infra.OpenRedis(s.Config)
	if err != nil {
		return err
	}
	if client == nil {
		return fmt.Errorf("Redis factory returned nil")
	}
	rediscommon.Rdb = client
	l.closers = append(l.closers, func() error { rediscommon.Rdb = nil; return client.Close() })
	return nil
}
