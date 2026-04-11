package proto

import (
	"io"
	"time"

	"github.com/hashicorp/yamux"
)

// YamuxConf returns the shared yamux session config used by both client and server.
func YamuxConf() *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = true
	cfg.KeepAliveInterval = 30 * time.Second
	cfg.LogOutput = io.Discard
	return cfg
}
