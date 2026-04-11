package cmd

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/22or/2nnel/internal/client"
	"github.com/22or/2nnel/internal/config"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var clientCmd = &cobra.Command{
	Use:   "client",
	Short: "Run the 2nnel client",
	RunE:  runClient,
}

func init() {
	clientCmd.Flags().String("server", "", "Server WebSocket URL (e.g. wss://example.com)")
	clientCmd.Flags().StringArray("tunnel", nil, "Tunnel spec: name:local_addr[:tcp:remote_port]")
	clientCmd.Flags().String("auth-token", "", "Auth token for server")
	clientCmd.Flags().StringP("config", "c", "", "Config file path (YAML)")
}

func runClient(cmd *cobra.Command, args []string) error {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfgFile, _ := cmd.Flags().GetString("config")

	var cfg *config.ClientConfig

	if cfgFile != "" {
		viper.SetConfigFile(cfgFile)
		if err := viper.ReadInConfig(); err != nil {
			return fmt.Errorf("read config: %w", err)
		}
		cfg = &config.ClientConfig{}
		if err := viper.Unmarshal(cfg); err != nil {
			return fmt.Errorf("parse config: %w", err)
		}
	} else {
		serverURL, _ := cmd.Flags().GetString("server")
		authToken, _ := cmd.Flags().GetString("auth-token")
		tunnelSpecs, _ := cmd.Flags().GetStringArray("tunnel")

		if serverURL == "" {
			return fmt.Errorf("--server is required")
		}

		tunnels, err := parseTunnelSpecs(tunnelSpecs)
		if err != nil {
			return err
		}

		cfg = &config.ClientConfig{
			Server:    serverURL,
			AuthToken: authToken,
			Tunnels:   tunnels,
		}
	}

	c := client.New(cfg)
	return c.Run()
}

// parseTunnelSpecs parses tunnel specs:
//   - HTTP: name:local_addr  (e.g. web:localhost:3000)
//   - TCP:  name:local_addr:tcp:remote_port  (e.g. ssh:localhost:22:tcp:2222)
func parseTunnelSpecs(specs []string) ([]config.TunnelConfig, error) {
	var tunnels []config.TunnelConfig
	for _, spec := range specs {
		parts := strings.Split(spec, ":")
		// Minimum: name:host:port → 3 parts
		if len(parts) < 3 {
			return nil, fmt.Errorf("invalid tunnel spec %q: want name:host:port or name:host:port:tcp:remote_port", spec)
		}

		name := parts[0]
		// Check if TCP: name:host:port:tcp:remote_port → 5 parts
		if len(parts) == 5 && parts[3] == "tcp" {
			remotePort, err := strconv.Atoi(parts[4])
			if err != nil {
				return nil, fmt.Errorf("invalid remote port in %q: %w", spec, err)
			}
			localAddr := parts[1] + ":" + parts[2]
			tunnels = append(tunnels, config.TunnelConfig{
				Name:       name,
				Local:      localAddr,
				Type:       "tcp",
				RemotePort: remotePort,
			})
		} else {
			// HTTP: name:host:port
			localAddr := strings.Join(parts[1:], ":")
			// subdomain = name
			tunnels = append(tunnels, config.TunnelConfig{
				Name:      name,
				Local:     localAddr,
				Type:      "http",
				Subdomain: name,
			})
		}
	}
	return tunnels, nil
}
