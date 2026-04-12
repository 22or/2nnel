package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"text/template"

	"github.com/22or/2nnel/internal/config"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var installCmd = &cobra.Command{
	Use:   "install",
	Short: "Install 2nnel client as a systemd service",
	RunE:  runInstall,
}

func init() {
	installCmd.Flags().String("server", "", "Server WebSocket URL (required)")
	installCmd.Flags().String("auth-token", "", "Auth token")
	installCmd.Flags().StringArray("tunnel", nil, "Tunnel spec (same format as client)")
	installCmd.Flags().String("config-dir", "/etc/2nnel", "Directory for config file")
	clientCmd.AddCommand(installCmd)
}

const serviceTemplate = `[Unit]
Description=2nnel tunnel client
After=network.target network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart={{.BinaryPath}} client -c {{.ConfigFile}}
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
`

func runInstall(cmd *cobra.Command, args []string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("must run as root: sudo %s", os.Args[0])
	}

	f := cmd.Flags()
	serverURL, _ := f.GetString("server")
	authToken, _ := f.GetString("auth-token")
	tunnelSpecs, _ := f.GetStringArray("tunnel")
	configDir, _ := f.GetString("config-dir")

	if serverURL == "" {
		return fmt.Errorf("--server required")
	}

	tunnels, err := parseTunnelSpecs(tunnelSpecs)
	if err != nil {
		return err
	}

	// Find binary path.
	binaryPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find binary: %w", err)
	}
	binaryPath, _ = filepath.EvalSymlinks(binaryPath)

	// Write config file.
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	configFile := filepath.Join(configDir, "client.yaml")
	cfg := &config.ClientConfig{
		Server:     serverURL,
		AuthToken:  authToken,
		Tunnels:    tunnels,
		ConfigFile: configFile,
	}
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	if err := os.WriteFile(configFile, b, 0600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	fmt.Printf("→ Config written to %s\n", configFile)

	// Write systemd service.
	serviceFile := "/etc/systemd/system/2nnel-client.service"
	tmpl, _ := template.New("svc").Parse(serviceTemplate)
	sf, err := os.Create(serviceFile)
	if err != nil {
		return fmt.Errorf("create service file: %w", err)
	}
	_ = tmpl.Execute(sf, map[string]string{
		"BinaryPath": binaryPath,
		"ConfigFile": configFile,
	})
	sf.Close()
	fmt.Printf("→ Service written to %s\n", serviceFile)

	// Enable and start.
	for _, args := range [][]string{
		{"systemctl", "daemon-reload"},
		{"systemctl", "enable", "--now", "2nnel-client"},
	} {
		out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
		if err != nil {
			fmt.Printf("  WARN: %s: %s\n", args, out)
		}
	}

	fmt.Println("✓ 2nnel-client service installed and started")
	fmt.Println("  Logs: journalctl -u 2nnel-client -f")
	return nil
}
