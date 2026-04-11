package cmd

import (
	"log/slog"
	"os"

	"github.com/22or/2nnel/internal/config"
	"github.com/22or/2nnel/internal/server"
	"github.com/spf13/cobra"
)

var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "Run the 2nnel relay server",
	RunE:  runServer,
}

func init() {
	serverCmd.Flags().String("domain", "", "Base domain for HTTP tunnels (e.g. example.com)")
	serverCmd.Flags().Int("port", 443, "Public HTTPS port")
	serverCmd.Flags().String("auth-token", "", "Shared secret for client authentication")
	serverCmd.Flags().Bool("dev", false, "Dev mode: plain HTTP on port 8080, no TLS")
	serverCmd.Flags().String("tls-cert", "", "Path to TLS certificate (PEM)")
	serverCmd.Flags().String("tls-key", "", "Path to TLS private key (PEM)")
	serverCmd.Flags().String("acme-cache", "/tmp/2nnel-certs", "Directory for Let's Encrypt cert cache")
	serverCmd.Flags().IntSlice("allowed-ports", nil, "Allowed TCP ports for TCP tunnels (empty = all)")
}

func runServer(cmd *cobra.Command, args []string) error {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	f := cmd.Flags()
	domain, _ := f.GetString("domain")
	port, _ := f.GetInt("port")
	authToken, _ := f.GetString("auth-token")
	dev, _ := f.GetBool("dev")
	tlsCert, _ := f.GetString("tls-cert")
	tlsKey, _ := f.GetString("tls-key")
	acmeCache, _ := f.GetString("acme-cache")
	allowedPorts, _ := f.GetIntSlice("allowed-ports")

	cfg := &config.ServerConfig{
		Domain:       domain,
		Port:         port,
		AuthToken:    authToken,
		Dev:          dev,
		TLSCert:      tlsCert,
		TLSKey:       tlsKey,
		ACMECache:    acmeCache,
		AllowedPorts: allowedPorts,
	}

	if dev {
		// Default to 8080 in dev mode only if port is still at its TLS default.
		if !cmd.Flags().Changed("port") {
			cfg.Port = 8080
		}
		slog.Info("dev mode: plain HTTP", "port", cfg.Port)
	}

	s := server.New(cfg)
	return s.Run()
}
