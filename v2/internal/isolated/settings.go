package isolated

import (
	"GopherAI/config"
	"fmt"
	"github.com/BurntSushi/toml"
	"net"
	"strconv"
	"strings"
)

type Settings struct {
	Config *config.Config
	Entry  EntrySettings
}
type EntrySettings struct {
	Isolated     bool   `toml:"isolated"`
	Migrate      bool   `toml:"migrate"`
	ChatKey      string `toml:"chatKey"`
	ChatModel    string `toml:"chatModel"`
	ChatBaseURL  string `toml:"chatBaseURL"`
	EmbeddingKey string `toml:"embeddingKey"`
}

func Load(path string) (*Settings, error) {
	conf, err := config.LoadExplicit(path)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Entry EntrySettings `toml:"isolatedEntry"`
	}
	metadata, err := toml.DecodeFile(path, &envelope)
	if err != nil {
		return nil, err
	}
	for _, key := range [][]string{{"mysqlConfig", "host"}, {"mysqlConfig", "port"}, {"mysqlConfig", "databaseName"}, {"mysqlConfig", "user"}, {"mysqlConfig", "charset"}, {"redisConfig", "host"}, {"redisConfig", "port"}, {"redisConfig", "db"}, {"mainConfig", "host"}, {"mainConfig", "port"}, {"knowledgeConfig", "storageDir"}, {"ragModelConfig", "embeddingModel"}, {"ragModelConfig", "embeddingBaseUrl"}, {"ragModelConfig", "dimension"}} {
		if !metadata.IsDefined(key...) {
			return nil, fmt.Errorf("explicit independent setting required: %s", strings.Join(key, "."))
		}
	}
	s := &Settings{Config: conf, Entry: envelope.Entry}
	return s, s.Validate()
}
func (s *Settings) Validate() error {
	if s == nil || s.Config == nil || !s.Entry.Isolated {
		return fmt.Errorf("isolatedEntry.isolated must be explicitly true")
	}
	c := s.Config
	if c.MysqlHost == "" || c.MysqlPort <= 0 || c.MysqlDatabaseName == "" || c.MysqlUser == "" || c.MysqlCharset == "" {
		return fmt.Errorf("explicit independent MySQL host/port/database/user/charset required")
	}
	if c.RedisHost == "" || c.RedisPort <= 0 {
		return fmt.Errorf("explicit independent Redis host/port required")
	}
	if c.MainConfig.Host == "" || c.MainConfig.Port <= 0 {
		return fmt.Errorf("explicit HTTP host/port required")
	}
	if strings.TrimSpace(s.Entry.ChatKey) == "" || s.Entry.ChatModel == "" || s.Entry.ChatBaseURL == "" || strings.TrimSpace(s.Entry.EmbeddingKey) == "" {
		return fmt.Errorf("explicit model settings required; environment credentials are not read")
	}
	if err := c.AgentConfig.Validate(); err != nil {
		return err
	}
	if err := c.KnowledgeConfig.Validate(); err != nil {
		return err
	}
	if err := c.JwtConfig.Validate(); err != nil {
		return err
	}
	return c.RagModelConfig.Validate()
}
func (s *Settings) Address() string {
	return net.JoinHostPort(s.Config.MainConfig.Host, strconv.Itoa(s.Config.MainConfig.Port))
}
