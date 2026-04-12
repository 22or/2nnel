package client

import (
	"log/slog"
	"math"
	"os"
	"time"

	"github.com/22or/2nnel/internal/config"
	"gopkg.in/yaml.v3"
)

// Client is the 2nnel tunnel client.
type Client struct {
	cfg *config.ClientConfig
}

// New creates a Client with cfg.
func New(cfg *config.ClientConfig) *Client {
	return &Client{cfg: cfg}
}

// Run connects to the server and maintains the connection forever,
// reconnecting with exponential backoff on failure.
func (c *Client) Run() error {
	const (
		baseDelay = 1 * time.Second
		maxDelay  = 60 * time.Second
		factor    = 2.0
	)

	var save func(*config.ClientConfig)
	if c.cfg.ConfigFile != "" {
		save = func(cfg *config.ClientConfig) {
			if err := saveConfig(cfg); err != nil {
				slog.Warn("save config failed", "err", err)
			}
		}
	}

	attempt := 0
	for {
		slog.Info("connecting to server", "url", c.cfg.Server, "attempt", attempt+1)

		sess := newSession(c.cfg)
		sess.onSave = save

		err := sess.connect()
		if err != nil {
			delay := time.Duration(math.Min(
				float64(baseDelay)*math.Pow(factor, float64(attempt)),
				float64(maxDelay),
			))
			slog.Warn("connection failed, retrying", "err", err, "in", delay)
			time.Sleep(delay)
			attempt++
			continue
		}

		slog.Info("connected", "server", c.cfg.Server)
		attempt = 0

		// run blocks until the session dies.
		sess.run()
		slog.Warn("session ended, reconnecting")
	}
}

// saveConfig writes cfg to cfg.ConfigFile as YAML.
func saveConfig(cfg *config.ClientConfig) error {
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(cfg.ConfigFile, b, 0600)
}
