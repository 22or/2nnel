package cmd

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"github.com/22or/2nnel/internal/client"
	"github.com/22or/2nnel/internal/config"
	"github.com/spf13/cobra"
)

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

	// Start a local file server on a random port.
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
	// Redirect bare root to the file path.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/"+filename, http.StatusFound)
	})

	go func() { _ = http.Serve(ln, mux) }()

	// Compute and print the public URL before connecting.
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

// sharePublicURL computes the expected public URL from the server WebSocket URL.
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
