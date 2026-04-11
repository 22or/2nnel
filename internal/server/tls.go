package server

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"

	"github.com/22or/2nnel/internal/config"
	"golang.org/x/crypto/acme/autocert"
)

type tlsResult struct {
	cfg     *tls.Config
	manager *autocert.Manager // non-nil when using autocert
}

// buildTLSConfig returns TLS config + optional autocert manager.
// Priority: custom cert/key > autocert.
func buildTLSConfig(cfg *config.ServerConfig) (*tlsResult, error) {
	if cfg.TLSCert != "" && cfg.TLSKey != "" {
		cert, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
		if err != nil {
			return nil, fmt.Errorf("load TLS key pair: %w", err)
		}
		return &tlsResult{
			cfg: &tls.Config{
				Certificates: []tls.Certificate{cert},
				MinVersion:   tls.VersionTLS12,
			},
		}, nil
	}

	if cfg.Domain == "" {
		return nil, fmt.Errorf("--domain required for autocert (or use --dev / --tls-cert+--tls-key)")
	}

	manager := &autocert.Manager{
		Cache:  autocert.DirCache(cfg.ACMECache),
		Prompt: autocert.AcceptTOS,
		HostPolicy: func(ctx context.Context, host string) error {
			if host == cfg.Domain || strings.HasSuffix(host, "."+cfg.Domain) {
				return nil
			}
			return fmt.Errorf("host %q not allowed", host)
		},
	}

	// manager.TLSConfig() correctly wires GetCertificate + TLS-ALPN-01 challenge
	// handling. Building a manual tls.Config with GetCertificate and "acme-tls/1"
	// in NextProtos without the challenge handler causes "certificate obtained,
	// but could not be installed".
	tlsCfg := manager.TLSConfig()
	tlsCfg.MinVersion = tls.VersionTLS12

	return &tlsResult{
		cfg:     tlsCfg,
		manager: manager,
	}, nil
}
