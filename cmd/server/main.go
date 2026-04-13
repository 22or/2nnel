package main

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/22or/2nnel/internal/config"
	"github.com/22or/2nnel/internal/server"
	"github.com/spf13/cobra"
)

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

var rootCmd = &cobra.Command{
	Use:   "2nnel-server",
	Short: "2nnel relay server",
	Long:  "2nnel-server accepts tunnel connections and proxies public traffic to connected clients.",
	RunE:  runServer,
}

var checkCmd = &cobra.Command{
	Use:   "check",
	Short: "Check server dependencies (Docker, Nixpacks)",
	RunE:  runCheck,
}

func init() {
	rootCmd.AddCommand(checkCmd)
	rootCmd.Flags().String("domain", "", "Base domain for HTTP tunnels (e.g. example.com)")
	rootCmd.Flags().Int("port", 443, "Public HTTPS port")
	rootCmd.Flags().String("auth-token", "", "Shared secret for client authentication")
	rootCmd.Flags().Bool("dev", false, "Dev mode: plain HTTP on port 8080, no TLS")
	rootCmd.Flags().String("tls-cert", "", "Path to TLS certificate (PEM)")
	rootCmd.Flags().String("tls-key", "", "Path to TLS private key (PEM)")
	rootCmd.Flags().String("acme-cache", "/tmp/2nnel-certs", "Directory for Let's Encrypt cert cache")
	rootCmd.Flags().IntSlice("allowed-ports", nil, "Allowed TCP ports for TCP tunnels (empty = all)")
	rootCmd.Flags().String("tcp-port-range", "", "Port range for auto-assigned TCP tunnels (e.g. 2200-2300)")
	rootCmd.Flags().String("deploy-dir", "", "Base directory for deployed app files (default: system temp dir)")
}

func runServer(cmd *cobra.Command, _ []string) error {
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
	tcpPortRange, _ := f.GetString("tcp-port-range")
	deployDir, _ := f.GetString("deploy-dir")

	tcpPortMin, tcpPortMax, err := parseTCPPortRange(tcpPortRange)
	if err != nil {
		return err
	}

	cfg := &config.ServerConfig{
		Domain:       domain,
		Port:         port,
		AuthToken:    authToken,
		Dev:          dev,
		TLSCert:      tlsCert,
		TLSKey:       tlsKey,
		ACMECache:    acmeCache,
		AllowedPorts: allowedPorts,
		TCPPortMin:   tcpPortMin,
		TCPPortMax:   tcpPortMax,
		DeployDir:    deployDir,
	}

	if dev && !cmd.Flags().Changed("port") {
		cfg.Port = 8080
		slog.Info("dev mode: plain HTTP", "port", cfg.Port)
	}

	s := server.New(cfg)
	return s.Run()
}

func runCheck(_ *cobra.Command, _ []string) error {
	allOK := true

	out, err := exec.Command("docker", "version", "--format", "{{.Server.Version}}").Output()
	if err != nil {
		fmt.Printf("✗ Docker: not found (%v)\n", err)
		allOK = false
	} else {
		fmt.Printf("✓ Docker %s\n", strings.TrimSpace(string(out)))
	}

	out, err = exec.Command("nixpacks", "--version").Output()
	if err != nil {
		fmt.Printf("✗ Nixpacks: not found (%v)\n", err)
		allOK = false
	} else {
		fmt.Printf("✓ Nixpacks %s\n", strings.TrimSpace(string(out)))
	}

	if allOK {
		fmt.Println("  Ready for promote.")
	} else {
		fmt.Println("\nInstall missing deps:")
		fmt.Println("  Docker:   apt install docker.io")
		fmt.Println("  Nixpacks: curl -sSL https://nixpacks.com/install.sh | bash")
	}
	return nil
}

func parseTCPPortRange(s string) (min, max int, err error) {
	if s == "" {
		return 0, 0, nil
	}
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("--tcp-port-range must be MIN-MAX (e.g. 2200-2300)")
	}
	min, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid --tcp-port-range: %w", err)
	}
	max, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid --tcp-port-range: %w", err)
	}
	if min >= max {
		return 0, 0, fmt.Errorf("--tcp-port-range min must be < max")
	}
	return min, max, nil
}
