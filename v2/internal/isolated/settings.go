package isolated

import (
	"GopherAI/config"
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
	Isolated    bool   `toml:"isolated"`
	Migrate     bool   `toml:"migrate"`
	ChatKey     string `toml:"chatKey"`
	ChatModel   string `toml:"chatModel"`
	ChatBaseURL string `toml:"chatBaseURL"`
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
	for _, key := range [][]string{{"mysqlConfig", "host"}, {"mysqlConfig", "port"}, {"mysqlConfig", "databaseName"}, {"mysqlConfig", "user"}, {"mysqlConfig", "charset"}, {"redisConfig", "host"}, {"redisConfig", "port"}, {"redisConfig", "db"}, {"mainConfig", "host"}, {"mainConfig", "port"}, {"knowledgeConfig", "storageDir"}, {"ragModelConfig", "embeddingModel"}, {"ragModelConfig", "embeddingBaseUrl"}, {"ragModelConfig", "embeddingApiKey"}, {"ragModelConfig", "dimension"}} {
		if !metadata.IsDefined(key...) {
			return nil, config.Refusef("explicit independent setting required: %s", strings.Join(key, "."))
		}
	}
	s := &Settings{Config: conf, Entry: envelope.Entry}
	return s, s.Validate()
}
func (s *Settings) Validate() error {
	if s == nil || s.Config == nil || !s.Entry.Isolated {
		return config.Refusef("isolatedEntry.isolated must be explicitly true")
	}
	c := s.Config
	if c.MysqlHost == "" || c.MysqlPort <= 0 || c.MysqlDatabaseName == "" || c.MysqlUser == "" || c.MysqlCharset == "" {
		return config.Refusef("explicit independent MySQL host/port/database/user/charset required")
	}
	if c.RedisHost == "" || c.RedisPort <= 0 {
		return config.Refusef("explicit independent Redis host/port required")
	}
	if c.MainConfig.Host == "" || c.MainConfig.Port <= 0 {
		return config.Refusef("explicit HTTP host/port required")
	}
	// A key is required here rather than in the RAG section's own validation, the
	// way the chat key is: a default configuration must not carry a secret, and a
	// tool that only reads the model name and address (the schema tool, an offline
	// harness) must still be able to load one. This is the entry that spends the
	// key, so this is where its absence is refused.
	if strings.TrimSpace(s.Entry.ChatKey) == "" || s.Entry.ChatModel == "" || s.Entry.ChatBaseURL == "" || strings.TrimSpace(c.RagEmbeddingAPIKey) == "" {
		return config.Refusef("explicit model settings required; environment credentials are not read")
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
