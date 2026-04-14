package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"

	"github.com/22or/2nnel/internal/client"
	"github.com/22or/2nnel/internal/config"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

var rootCmd = &cobra.Command{
	Use:   "2nnel",
	Short: "2nnel tunnel client",
	Long:  "2nnel connects local services to a 2nnel-server through outbound-only WebSocket connections.",
	RunE:  runClient,
}

func init() {
	rootCmd.Flags().String("server", "", "Server WebSocket URL (e.g. wss://example.com)")
	rootCmd.Flags().StringArray("tunnel", nil, "Tunnel spec: name:local_addr[:tcp:remote_port]")
	rootCmd.Flags().String("auth-token", "", "Auth token for server")
	rootCmd.Flags().StringP("config", "c", "", "Config file path (YAML)")
	rootCmd.Flags().String("dir", "", "Project directory for promote (single-tunnel mode only)")
	rootCmd.Flags().String("name", "", "Display name for this client in the dashboard")

	rootCmd.AddCommand(shareCmd)
	rootCmd.AddCommand(installCmd)
}

// ── Client (root) ─────────────────────────────────────────────────────────────

func runClient(cmd *cobra.Command, _ []string) error {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfgFile, _ := cmd.Flags().GetString("config")
	nameFlag, _ := cmd.Flags().GetString("name")

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
		cfg.ConfigFile = cfgFile
	} else {
		serverURL, _ := cmd.Flags().GetString("server")
		authToken, _ := cmd.Flags().GetString("auth-token")
		tunnelSpecs, _ := cmd.Flags().GetStringArray("tunnel")
		dirFlag, _ := cmd.Flags().GetString("dir")

		if serverURL == "" {
			return fmt.Errorf("--server is required (or use -c <config.yaml>)")
		}

		tunnels, err := parseTunnelSpecs(tunnelSpecs)
		if err != nil {
			return err
		}
		if dirFlag != "" && len(tunnels) == 1 {
			tunnels[0].Dir = dirFlag
		}

		cfg = &config.ClientConfig{
			Server:    serverURL,
			AuthToken: authToken,
			Tunnels:   tunnels,
		}
	}

	// Resolve client name: flag > config > prompt > hostname fallback.
	if nameFlag != "" {
		cfg.Name = nameFlag
	}
	if cfg.Name == "" {
		cfg.Name = resolveClientName(cfg.ConfigFile != "")
		// Persist if we have a config file and a non-empty name.
		if cfg.ConfigFile != "" && cfg.Name != "" {
			if b, err := yaml.Marshal(cfg); err == nil {
				_ = os.WriteFile(cfg.ConfigFile, b, 0600)
			}
		}
	}
	fmt.Printf("→ Client name: %s\n", cfg.Name)

	c := client.New(cfg)
	return c.Run()
}

// resolveClientName prompts the user to name this client when stdin is a TTY.
// Falls back to the machine hostname otherwise. suggestedPersist is informational —
// it slightly changes the prompt text.
func resolveClientName(persist bool) string {
	host, _ := os.Hostname()
	if host == "" {
		host = "client"
	}
	if !isInteractiveStdin() {
		return host
	}
	if persist {
		fmt.Printf("Name this client [%s]: ", host)
	} else {
		fmt.Printf("Name this client (shown in dashboard) [%s]: ", host)
	}
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return host
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return host
	}
	return line
}

// isInteractiveStdin returns true if stdin is a character device (TTY).
func isInteractiveStdin() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// ── Share ──────────────────────────────────────────────────────────────────────

var shareCmd = &cobra.Command{
	Use:   "share <file>",
	Short: "Instantly share a local file via a public URL",
	Args:  cobra.ExactArgs(1),
	RunE:  runShare,
}

func init() {
	shareCmd.Flags().String("server", "", "Server WebSocket URL (wss:// or ws:// for dev)")
	shareCmd.Flags().String("auth-token", "", "Auth token")
	shareCmd.Flags().String("name", "", "Subdomain name (default: share-<random>)")
	_ = shareCmd.MarkFlagRequired("server")
}

func runShare(cmd *cobra.Command, args []string) error {
	filePath := args[0]
	info, err := os.Stat(filePath)
	if err != nil {
		return fmt.Errorf("file not found: %w", err)
	}
	if info.IsDir() {
		return fmt.Errorf("%q is a directory — share supports files only", filePath)
	}

	f := cmd.Flags()
	serverURL, _ := f.GetString("server")
	authToken, _ := f.GetString("auth-token")
	name, _ := f.GetString("name")
	if name == "" {
		name = "share-" + randomHex(4)
	}

	filename := filepath.Base(filePath)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	localPort := ln.Addr().(*net.TCPAddr).Port
	localAddr := fmt.Sprintf("127.0.0.1:%d", localPort)

	mux := http.NewServeMux()
	mux.HandleFunc("/"+filename, func(w http.ResponseWriter, r *http.Request) {
		fh, err := os.Open(filePath)
		if err != nil {
			http.Error(w, "file unavailable", http.StatusInternalServerError)
			return
		}
		defer fh.Close()
		st, _ := fh.Stat()
		w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
		http.ServeContent(w, r, filename, st.ModTime(), fh)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/"+filename, http.StatusFound)
	})
	go func() { _ = http.Serve(ln, mux) }()

	publicURL := sharePublicURL(serverURL, name, filename)
	fmt.Printf("Sharing %q at: %s\n", filename, publicURL)
	fmt.Println("Press Ctrl+C to stop.")

	cfg := &config.ClientConfig{
		Server:    serverURL,
		AuthToken: authToken,
		Tunnels: []config.TunnelConfig{{
			Name:      name,
			Local:     localAddr,
			Type:      "http",
			Subdomain: name,
		}},
	}
	return client.New(cfg).Run()
}

func sharePublicURL(serverURL, subdomain, filename string) string {
	u, err := url.Parse(serverURL)
	if err != nil {
		return fmt.Sprintf("https://%s.?/%s", subdomain, filename)
	}
	scheme := "https"
	if u.Scheme == "ws" {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s.%s/%s", scheme, subdomain, u.Host, filename)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ── Install ────────────────────────────────────────────────────────────────────

var installCmd = &cobra.Command{
	Use:   "install",
	Short: "Install 2nnel as a systemd service",
	RunE:  runInstall,
}

func init() {
	installCmd.Flags().String("server", "", "Server WebSocket URL (required)")
	installCmd.Flags().String("auth-token", "", "Auth token")
	installCmd.Flags().StringArray("tunnel", nil, "Tunnel spec (same format as root command)")
	installCmd.Flags().String("config-dir", "/etc/2nnel", "Directory for config file")
	installCmd.Flags().String("name", "", "Display name for this client in the dashboard")
}

const serviceTemplate = `[Unit]
Description=2nnel tunnel client
After=network.target network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart={{.BinaryPath}} -c {{.ConfigFile}}
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
`

func runInstall(cmd *cobra.Command, _ []string) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("must run as root: sudo %s", os.Args[0])
	}

	f := cmd.Flags()
	serverURL, _ := f.GetString("server")
	authToken, _ := f.GetString("auth-token")
	tunnelSpecs, _ := f.GetStringArray("tunnel")
	configDir, _ := f.GetString("config-dir")
	name, _ := f.GetString("name")

	if serverURL == "" {
		return fmt.Errorf("--server required")
	}

	tunnels, err := parseTunnelSpecs(tunnelSpecs)
	if err != nil {
		return err
	}

	if name == "" {
		name = resolveClientName(true)
	}

	binaryPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find binary: %w", err)
	}
	binaryPath, _ = filepath.EvalSymlinks(binaryPath)

	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	configFile := filepath.Join(configDir, "client.yaml")
	cfg := &config.ClientConfig{
		Name:       name,
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

	serviceFile := "/etc/systemd/system/2nnel.service"
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

	for _, a := range [][]string{
		{"systemctl", "daemon-reload"},
		{"systemctl", "enable", "--now", "2nnel"},
	} {
		out, err := exec.Command(a[0], a[1:]...).CombinedOutput()
		if err != nil {
			fmt.Printf("  WARN: %v: %s\n", a, out)
		}
	}

	fmt.Println("✓ 2nnel service installed and started")
	fmt.Println("  Logs: journalctl -u 2nnel -f")
	return nil
}

// ── Shared helpers ─────────────────────────────────────────────────────────────

// parseTunnelSpecs parses tunnel specs:
//   - HTTP: name:local_addr        (e.g. web:localhost:3000)
//   - TCP:  name:local_addr:tcp[:remote_port]  (e.g. ssh:localhost:22:tcp:2222)
func parseTunnelSpecs(specs []string) ([]config.TunnelConfig, error) {
	var tunnels []config.TunnelConfig
	for _, spec := range specs {
		parts := strings.Split(spec, ":")
		if len(parts) < 3 {
			return nil, fmt.Errorf("invalid tunnel spec %q: want name:host:port or name:host:port:tcp:remote_port", spec)
		}
		name := parts[0]
		if (len(parts) == 4 || len(parts) == 5) && parts[3] == "tcp" {
			localAddr := parts[1] + ":" + parts[2]
			remotePort := 0
			if len(parts) == 5 {
				var err error
				remotePort, err = strconv.Atoi(parts[4])
				if err != nil {
					return nil, fmt.Errorf("invalid remote port in %q: %w", spec, err)
				}
			}
			tunnels = append(tunnels, config.TunnelConfig{
				Name:       name,
				Local:      localAddr,
				Type:       "tcp",
				RemotePort: remotePort,
			})
		} else {
			localAddr := strings.Join(parts[1:], ":")
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
