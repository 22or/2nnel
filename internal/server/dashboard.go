package server

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/22or/2nnel/internal/proto"
	"github.com/gorilla/websocket"
)

//go:embed web/index.html
var dashboardHTML string

// handleAdmin dispatches /_2nnel/* routes.
func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/_2nnel")

	switch {
	case path == "" || path == "/":
		s.serveIndex(w, r)
	case path == "/metrics":
		s.handleMetrics(w, r)
	case path == "/stream":
		s.handleSSE(w, r)
	case strings.HasPrefix(path, "/clients/") && strings.HasSuffix(path, "/disconnect") && r.Method == http.MethodPost:
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/clients/"), "/disconnect")
		s.handleDisconnect(w, r, id)
	case strings.HasPrefix(path, "/clients/") && strings.HasSuffix(path, "/tunnels") && r.Method == http.MethodPost:
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/clients/"), "/tunnels")
		s.handleAddTunnel(w, r, id)
	case strings.HasPrefix(path, "/clients/") && strings.Contains(path, "/tunnels/") && r.Method == http.MethodDelete:
		rest := strings.TrimPrefix(path, "/clients/")
		parts := strings.SplitN(rest, "/tunnels/", 2)
		if len(parts) == 2 {
			s.handleRemoveTunnel(w, r, parts[0], parts[1])
		} else {
			http.NotFound(w, r)
		}
	case strings.HasPrefix(path, "/tcp/"):
		name := strings.TrimPrefix(path, "/tcp/")
		s.handleTCPOverWS(w, r, name)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, dashboardHTML)
}

// handleSSE streams live registry snapshots as Server-Sent Events.
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()

	send := func() {
		payload := s.buildSSEPayload()
		b, err := json.Marshal(payload)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}

	send() // immediate first frame
	for {
		select {
		case <-tick.C:
			send()
		case <-r.Context().Done():
			return
		}
	}
}

type ssePayload struct {
	UptimeSeconds float64          `json:"uptime_seconds"`
	ServerTime    string           `json:"server_time"`
	Clients       []sseClient      `json:"clients"`
	Totals        sseTotals        `json:"totals"`
}

type sseClient struct {
	ID           string       `json:"id"`
	ShortID      string       `json:"short_id"`
	Remote       string       `json:"remote"`
	ConnectedAt  string       `json:"connected_at"`
	ConnectedAgo string       `json:"connected_ago"`
	Tunnels      []sseTunnel  `json:"tunnels"`
}

type sseTunnel struct {
	ClientID      string `json:"client_id"`
	Name          string `json:"name"`
	Type          string `json:"type"`
	Endpoint      string `json:"endpoint"`
	LocalAddr     string `json:"local_addr"`
	BytesIn       int64  `json:"bytes_in"`
	BytesOut      int64  `json:"bytes_out"`
	BytesInHuman  string `json:"bytes_in_human"`
	BytesOutHuman string `json:"bytes_out_human"`
	Requests      int64  `json:"requests"`
	ActiveConns   int    `json:"active_conns"`
}

type sseTotals struct {
	Clients      int   `json:"clients"`
	HTTPTunnels  int   `json:"http_tunnels"`
	TCPTunnels   int   `json:"tcp_tunnels"`
	TotalBytesIn int64 `json:"total_bytes_in"`
	TotalBytesOut int64 `json:"total_bytes_out"`
}

func (s *Server) buildSSEPayload() ssePayload {
	snaps := s.registry.snapshot()
	now := time.Now()

	var totals sseTotals
	totals.Clients = len(snaps)

	clients := make([]sseClient, 0, len(snaps))
	for _, snap := range snaps {
		shortID := snap.ID
		if len(shortID) > 8 {
			shortID = shortID[:8]
		}

		tunnels := make([]sseTunnel, 0, len(snap.Tunnels))
		for _, t := range snap.Tunnels {
			if t.Type == "http" {
				totals.HTTPTunnels++
			} else {
				totals.TCPTunnels++
			}
			totals.TotalBytesIn += t.BytesIn
			totals.TotalBytesOut += t.BytesOut

			endpoint := t.Endpoint
			if t.Type == "http" && s.cfg.Domain != "" {
				scheme := "https"
				if s.cfg.Dev {
					scheme = "http"
				}
				endpoint = scheme + "://" + t.Endpoint + "." + s.cfg.Domain
				if s.cfg.Dev && s.cfg.Port != 80 && s.cfg.Port != 443 {
					endpoint += fmt.Sprintf(":%d", s.cfg.Port)
				}
			}

			tunnels = append(tunnels, sseTunnel{
				ClientID:      snap.ID,
				Name:          t.Name,
				Type:          t.Type,
				Endpoint:      endpoint,
				LocalAddr:     t.LocalAddr,
				BytesIn:       t.BytesIn,
				BytesOut:      t.BytesOut,
				BytesInHuman:  fmtBytes(t.BytesIn),
				BytesOutHuman: fmtBytes(t.BytesOut),
				Requests:      t.Requests,
				ActiveConns:   t.ActiveConns,
			})
		}

		clients = append(clients, sseClient{
			ID:           snap.ID,
			ShortID:      shortID,
			Remote:       snap.Remote,
			ConnectedAt:  snap.ConnectedAt.UTC().Format(time.RFC3339),
			ConnectedAgo: fmtDuration(now.Sub(snap.ConnectedAt)),
			Tunnels:      tunnels,
		})
	}

	return ssePayload{
		UptimeSeconds: time.Since(startTime).Seconds(),
		ServerTime:    now.UTC().Format(time.RFC3339),
		Clients:       clients,
		Totals:        totals,
	}
}

// handleTCPOverWS proxies a named TCP tunnel over a WebSocket connection.
// Allows TCP tunnels to be reached through port 443 (e.g. SSH through corporate VPN).
// Endpoint: GET /_2nnel/tcp/{tunnel-name}  (WebSocket upgrade)
func (s *Server) handleTCPOverWS(w http.ResponseWriter, r *http.Request, tunnelName string) {
	// Find the client entry that owns this tunnel.
	var entry *clientEntry
	var te *tunnelEntry
	s.registry.mu.RLock()
	for _, e := range s.registry.byID {
		e.mu.RLock()
		for _, t := range e.tunnels {
			if t.name == tunnelName && t.tunnelType == "tcp" {
				entry = e
				te = t
				break
			}
		}
		e.mu.RUnlock()
		if entry != nil {
			break
		}
	}
	s.registry.mu.RUnlock()

	if entry == nil {
		http.Error(w, fmt.Sprintf("no TCP tunnel %q registered", tunnelName), http.StatusNotFound)
		return
	}

	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()

	stream, err := entry.session.Open()
	if err != nil {
		_ = ws.WriteMessage(websocket.CloseMessage, []byte("tunnel unavailable"))
		return
	}
	defer stream.Close()

	if err := writeStreamHdr(stream, te.name, te.localAddr); err != nil {
		return
	}

	te.activeConns.Add(1)
	defer te.activeConns.Add(-1)

	pipe(proto.NewWSConn(ws), &countingStream{ReadWriteCloser: stream, te: te})
}

// handleAddTunnel relays an AddTunnel command to a connected client.
func (s *Server) handleAddTunnel(w http.ResponseWriter, r *http.Request, clientID string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var msg proto.AddTunnel
	if err := json.Unmarshal(body, &msg); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if msg.Name == "" || msg.LocalAddr == "" || (msg.Type != "http" && msg.Type != "tcp") {
		http.Error(w, "name, local_addr, and type (http|tcp) required", http.StatusBadRequest)
		return
	}

	s.registry.mu.RLock()
	entry, ok := s.registry.byID[clientID]
	s.registry.mu.RUnlock()
	if !ok {
		http.Error(w, "client not found", http.StatusNotFound)
		return
	}
	if err := entry.sendMsg(proto.TypeAddTunnel, msg); err != nil {
		http.Error(w, "send to client: "+err.Error(), http.StatusInternalServerError)
		return
	}
	slog.Info("add tunnel relayed", "client", clientID, "tunnel", msg.Name)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true}`)
}

// handleRemoveTunnel removes a tunnel from registry and notifies the client.
func (s *Server) handleRemoveTunnel(w http.ResponseWriter, r *http.Request, clientID, tunnelName string) {
	s.registry.mu.RLock()
	entry, ok := s.registry.byID[clientID]
	s.registry.mu.RUnlock()
	if !ok {
		http.Error(w, "client not found", http.StatusNotFound)
		return
	}
	_ = entry.sendMsg(proto.TypeRemoveTunnel, proto.RemoveTunnel{Name: tunnelName})
	s.registry.removeTunnelByName(clientID, tunnelName)
	slog.Info("tunnel removed", "client", clientID, "tunnel", tunnelName)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true}`)
}

// handleDisconnect forcibly closes a client session.
func (s *Server) handleDisconnect(w http.ResponseWriter, r *http.Request, clientID string) {
	if !s.registry.disconnectClient(clientID) {
		http.Error(w, "client not found", http.StatusNotFound)
		return
	}
	slog.Info("admin disconnect", "client", clientID)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"disconnected":%q}`, clientID)
}

// fmtDuration formats a duration for human display (e.g. "3m 24s").
func fmtDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	sec := int(d.Seconds()) % 60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm %ds", m, sec)
	default:
		return fmt.Sprintf("%ds", sec)
	}
}
