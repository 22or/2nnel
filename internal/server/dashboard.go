package server

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
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
	Name         string `json:"name"`
	Type         string `json:"type"`
	Endpoint     string `json:"endpoint"`
	LocalAddr    string `json:"local_addr"`
	BytesIn      int64  `json:"bytes_in"`
	BytesOut     int64  `json:"bytes_out"`
	BytesInHuman string `json:"bytes_in_human"`
	BytesOutHuman string `json:"bytes_out_human"`
	Requests     int64  `json:"requests"`
	ActiveConns  int    `json:"active_conns"`
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

			tunnels = append(tunnels, sseTunnel{
				Name:          t.Name,
				Type:          t.Type,
				Endpoint:      t.Endpoint,
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
