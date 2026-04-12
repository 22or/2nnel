package client

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"

	"github.com/22or/2nnel/internal/config"
	"github.com/22or/2nnel/internal/proto"
)

// handleDataStream reads the StreamHeader from a server-opened yamux stream,
// dials the local service, and proxies bytes bidirectionally.
func handleDataStream(stream io.ReadWriteCloser, cfg *config.ClientConfig) {
	defer stream.Close()

	// Read the stream header (first JSON line).
	br := bufio.NewReader(stream)
	line, err := br.ReadBytes('\n')
	if err != nil {
		slog.Error("read stream header", "err", err)
		return
	}

	var hdr proto.StreamHeader
	if err := json.Unmarshal(line, &hdr); err != nil {
		slog.Error("parse stream header", "err", err)
		return
	}

	// Find local address and tunnel type from config.
	localAddr := hdr.LocalAddr
	tunnelType := "http" // default; used for error response shape
	for _, t := range cfg.Tunnels {
		if t.Name == hdr.TunnelName {
			if localAddr == "" {
				localAddr = t.Local
			}
			tunnelType = t.Type
			break
		}
	}
	if localAddr == "" {
		slog.Error("no local addr for tunnel", "name", hdr.TunnelName)
		return
	}

	local, err := net.Dial("tcp", localAddr)
	if err != nil {
		slog.Error("dial local service", "addr", localAddr, "err", err)
		// For HTTP tunnels the server is waiting on http.ReadResponse — write a
		// proper 502 so it gets a valid response instead of unexpected EOF.
		if tunnelType != "tcp" {
			msg := "service unavailable: " + err.Error()
			fmt.Fprintf(stream, "HTTP/1.1 502 Bad Gateway\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
				len(msg), msg)
		}
		return
	}
	defer local.Close()

	slog.Info("proxying stream", "tunnel", hdr.TunnelName, "local", localAddr)

	// Proxy: stream → local, local → stream.
	// Use io.MultiReader so buffered bytes from header read are not lost.
	done := make(chan struct{}, 2)
	cp := func(dst io.Writer, src io.Reader) {
		_, _ = io.Copy(dst, src)
		done <- struct{}{}
	}

	// From server stream (br has buffered bytes post-header) → local service.
	go cp(local, br)
	// From local service → server stream.
	go cp(stream, local)

	<-done
}
