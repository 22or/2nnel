package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/22or/2nnel/internal/proto"
	"github.com/hashicorp/yamux"
)

// handleHTTP routes public HTTP(S) traffic to the registered client tunnel.
func (s *Server) handleHTTP(w http.ResponseWriter, r *http.Request) {
	subdomain := extractSubdomain(r.Host, s.cfg.Domain)
	if subdomain == "" {
		s.serveIndex(w, r)
		return
	}

	entry, ok := s.registry.lookupHTTP(subdomain)
	if !ok {
		// Check deployed apps before returning 502.
		if val, ok := s.deployedApps.Load(subdomain); ok {
			s.serveDeployedApp(w, r, val.(*deployedApp))
			return
		}
		http.Error(w, fmt.Sprintf("no tunnel registered for subdomain %q", subdomain), http.StatusBadGateway)
		return
	}

	var te *tunnelEntry
	s.registry.mu.RLock()
	for _, t := range entry.tunnels {
		if t.subdomain == subdomain {
			te = t
			break
		}
	}
	s.registry.mu.RUnlock()

	if te == nil {
		http.Error(w, "tunnel entry missing", http.StatusInternalServerError)
		return
	}

	// WebSocket: hijack and tunnel raw bytes over yamux stream.
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		s.handleWSProxy(w, r, entry, te)
		return
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = "http"
			req.URL.Host = te.localAddr
			req.Header.Set("X-Forwarded-Host", req.Host)
			req.Header.Set("X-Forwarded-For", req.RemoteAddr)
		},
		Transport:     &yamuxTransport{session: entry.session, te: te},
		FlushInterval: 100 * time.Millisecond,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Warn("proxy error", "subdomain", subdomain, "err", err)
			http.Error(w, "tunnel error: "+err.Error(), http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

// extractSubdomain extracts the leftmost label from host if it ends in "."+base.
func extractSubdomain(host, base string) string {
	if idx := strings.LastIndex(host, ":"); idx >= 0 {
		host = host[:idx]
	}
	if base == "" {
		if idx := strings.Index(host, "."); idx > 0 {
			return host[:idx]
		}
		return ""
	}
	suffix := "." + base
	if !strings.HasSuffix(host, suffix) {
		return ""
	}
	sub := host[:len(host)-len(suffix)]
	if strings.Contains(sub, ".") {
		return ""
	}
	return sub
}

// yamuxTransport implements http.RoundTripper over a yamux session.
type yamuxTransport struct {
	session *yamux.Session
	te      *tunnelEntry
}

func (t *yamuxTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.te.requests.Add(1)
	t.te.activeConns.Add(1)
	defer t.te.activeConns.Add(-1)

	stream, err := t.session.Open()
	if err != nil {
		return nil, fmt.Errorf("open yamux stream: %w", err)
	}

	// Wrap with byte counter.
	cs := &countingStream{ReadWriteCloser: stream, te: t.te}

	// Write stream header.
	hdr := proto.StreamHeader{TunnelName: t.te.name, LocalAddr: t.te.localAddr}
	hdrBytes, _ := json.Marshal(hdr)
	hdrBytes = append(hdrBytes, '\n')
	if _, err := cs.Write(hdrBytes); err != nil {
		_ = stream.Close()
		return nil, fmt.Errorf("write stream header: %w", err)
	}

	if err := req.Write(cs); err != nil {
		_ = stream.Close()
		return nil, fmt.Errorf("write request: %w", err)
	}

	br := bufio.NewReader(cs)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		_ = stream.Close()
		return nil, fmt.Errorf("read response: %w", err)
	}

	resp.Body = &streamBody{ReadCloser: resp.Body, stream: stream}
	return resp, nil
}

// countingStream wraps an io.ReadWriteCloser and updates tunnel byte counters.
type countingStream struct {
	io.ReadWriteCloser
	te *tunnelEntry
}

func (c *countingStream) Write(p []byte) (int, error) {
	n, err := c.ReadWriteCloser.Write(p)
	c.te.bytesIn.Add(int64(n))
	return n, err
}

func (c *countingStream) Read(p []byte) (int, error) {
	n, err := c.ReadWriteCloser.Read(p)
	c.te.bytesOut.Add(int64(n))
	return n, err
}

// streamBody closes the underlying yamux stream when the response body is closed.
type streamBody struct {
	io.ReadCloser
	stream io.Closer
}

func (b *streamBody) Close() error {
	err := b.ReadCloser.Close()
	_ = b.stream.Close()
	return err
}

// handleWSProxy tunnels a WebSocket upgrade request raw over a yamux stream.
func (s *Server) handleWSProxy(w http.ResponseWriter, r *http.Request, entry *clientEntry, te *tunnelEntry) {
	stream, err := entry.session.Open()
	if err != nil {
		http.Error(w, "open tunnel stream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer stream.Close()

	// Write stream header so client knows which tunnel to dial.
	hdr := proto.StreamHeader{TunnelName: te.name, LocalAddr: te.localAddr}
	hdrBytes, _ := json.Marshal(hdr)
	hdrBytes = append(hdrBytes, '\n')
	if _, err := stream.Write(hdrBytes); err != nil {
		http.Error(w, "write stream header: "+err.Error(), http.StatusBadGateway)
		return
	}

	// Forward the original HTTP upgrade request to the client's local service.
	if err := r.Write(stream); err != nil {
		http.Error(w, "write request: "+err.Error(), http.StatusBadGateway)
		return
	}

	// Hijack the client connection so we can pipe raw bytes.
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		http.Error(w, "hijack: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer conn.Close()

	te.activeConns.Add(1)
	defer te.activeConns.Add(-1)

	// cs counts bytes; use pipe (no te) to avoid double-counting.
	cs := &countingStream{ReadWriteCloser: stream, te: te}
	pipe(conn, cs)
}
