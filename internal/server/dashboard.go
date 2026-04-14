package server

import (
	_ "embed"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/22or/2nnel/internal/proto"
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
	case path == "/promote" && r.Method == http.MethodPost:
		s.handlePromoteUpload(w, r)
	case strings.HasPrefix(path, "/promote/") && strings.HasSuffix(path, "/logs") && r.Method == http.MethodGet:
		name := strings.TrimSuffix(strings.TrimPrefix(path, "/promote/"), "/logs")
		s.handleDeployLogs(w, r, name)
	case strings.HasPrefix(path, "/promote/") && r.Method == http.MethodDelete:
		name := strings.TrimPrefix(path, "/promote/")
		s.handleDeleteDeploy(w, r, name)
	case strings.HasPrefix(path, "/builds/") && r.Method == http.MethodDelete:
		name := strings.TrimPrefix(path, "/builds/")
		s.pendingBuilds.Delete(name)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true}`)
	case strings.HasPrefix(path, "/clients/") && strings.HasSuffix(path, "/disconnect") && r.Method == http.MethodPost:
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/clients/"), "/disconnect")
		s.handleDisconnect(w, r, id)
	case strings.HasPrefix(path, "/clients/") && strings.HasSuffix(path, "/tunnels") && r.Method == http.MethodPost:
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/clients/"), "/tunnels")
		s.handleAddTunnel(w, r, id)
	case strings.HasPrefix(path, "/clients/") && strings.Contains(path, "/tunnels/") && strings.HasSuffix(path, "/dir") && r.Method == http.MethodPatch:
		rest := strings.TrimPrefix(path, "/clients/")
		parts := strings.SplitN(rest, "/tunnels/", 2)
		if len(parts) == 2 {
			tunnelName := strings.TrimSuffix(parts[1], "/dir")
			s.handleSetTunnelDir(w, r, parts[0], tunnelName)
		} else {
			http.NotFound(w, r)
		}
	case strings.HasPrefix(path, "/clients/") && strings.Contains(path, "/tunnels/") && strings.HasSuffix(path, "/promote") && r.Method == http.MethodPost:
		rest := strings.TrimPrefix(path, "/clients/")
		parts := strings.SplitN(rest, "/tunnels/", 2)
		if len(parts) == 2 {
			tunnelName := strings.TrimSuffix(parts[1], "/promote")
			s.handlePromoteTrigger(w, r, parts[0], tunnelName)
		} else {
			http.NotFound(w, r)
		}
	case strings.HasPrefix(path, "/clients/") && strings.Contains(path, "/tunnels/") && r.Method == http.MethodDelete:
		rest := strings.TrimPrefix(path, "/clients/")
		parts := strings.SplitN(rest, "/tunnels/", 2)
		if len(parts) == 2 {
			s.handleRemoveTunnel(w, r, parts[0], parts[1])
		} else {
			http.NotFound(w, r)
		}
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
	UptimeSeconds float64           `json:"uptime_seconds"`
	ServerTime    string            `json:"server_time"`
	Clients       []sseClient       `json:"clients"`
	Totals        sseTotals         `json:"totals"`
	DeployedApps  []sseDeployedApp  `json:"deployed_apps"`
	PendingBuilds []ssePendingBuild `json:"pending_builds"`
}

type ssePendingBuild struct {
	Name      string   `json:"name"`
	StartedAt string   `json:"started_at"`
	ElapsedS  int      `json:"elapsed_s"`
	Lines     []string `json:"lines"`
	Error     string   `json:"error,omitempty"`
	Done      bool     `json:"done"`
}

type sseDeployedApp struct {
	Name        string   `json:"name"`
	PublicURL   string   `json:"public_url"`
	Port        int      `json:"port"`
	StartedAt   string   `json:"started_at"`
	Requests    int64    `json:"requests"`
	ActiveConns int      `json:"active_conns"`
	RecentLogs  []string `json:"recent_logs"`
}

type sseClient struct {
	ID           string       `json:"id"`
	ShortID      string       `json:"short_id"`
	Name         string       `json:"name"`
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
	CanPromote    bool   `json:"can_promote"`
	Dir           string `json:"dir"`
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
			if t.Type == "http" {
				endpoint = s.buildPublicHTTPURL(t.Endpoint)
			}

			tunnels = append(tunnels, sseTunnel{
				ClientID:      snap.ID,
				Name:          t.Name,
				Type:          t.Type,
				Endpoint:      endpoint,
				LocalAddr:     t.LocalAddr,
				CanPromote:    t.CanPromote,
				Dir:           t.Dir,
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
			Name:         snap.Name,
			Remote:       snap.Remote,
			ConnectedAt:  snap.ConnectedAt.UTC().Format(time.RFC3339),
			ConnectedAgo: fmtDuration(now.Sub(snap.ConnectedAt)),
			Tunnels:      tunnels,
		})
	}

	// Deployed apps
	var deployedApps []sseDeployedApp
	s.deployedApps.Range(func(k, v any) bool {
		app := v.(*deployedApp)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		out, _ := exec.CommandContext(ctx, "docker", "logs", "--tail", "10", "2nnel-"+app.name).CombinedOutput()
		cancel()
		recentLogs := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
		if len(recentLogs) == 1 && recentLogs[0] == "" {
			recentLogs = nil
		}
		deployedApps = append(deployedApps, sseDeployedApp{
			Name:        app.name,
			PublicURL:   s.buildPublicHTTPURL(app.subdomain),
			Port:        app.port,
			StartedAt:   app.startedAt.UTC().Format(time.RFC3339),
			Requests:    app.requests.Load(),
			ActiveConns: int(app.activeConns.Load()),
			RecentLogs:  recentLogs,
		})
		return true
	})

	// Pending builds
	var pendingBuilds []ssePendingBuild
	s.pendingBuilds.Range(func(k, v any) bool {
		b := v.(*pendingBuild)
		lines, errMsg, done := b.snapshot()
		pendingBuilds = append(pendingBuilds, ssePendingBuild{
			Name:      b.name,
			StartedAt: b.startedAt.UTC().Format(time.RFC3339),
			ElapsedS:  int(time.Since(b.startedAt).Seconds()),
			Lines:     lines,
			Error:     errMsg,
			Done:      done,
		})
		return true
	})

	return ssePayload{
		UptimeSeconds: time.Since(startTime).Seconds(),
		ServerTime:    now.UTC().Format(time.RFC3339),
		Clients:       clients,
		Totals:        totals,
		DeployedApps:  deployedApps,
		PendingBuilds: pendingBuilds,
	}
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

// handleSetTunnelDir updates the project directory for a tunnel and notifies the client.
func (s *Server) handleSetTunnelDir(w http.ResponseWriter, r *http.Request, clientID, tunnelName string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var req struct {
		Dir string `json:"dir"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	s.registry.mu.RLock()
	entry, ok := s.registry.byID[clientID]
	s.registry.mu.RUnlock()
	if !ok {
		http.Error(w, "client not found", http.StatusNotFound)
		return
	}

	entry.mu.Lock()
	var found bool
	for _, t := range entry.tunnels {
		if t.name == tunnelName {
			t.dir = req.Dir
			t.hasDir = req.Dir != ""
			found = true
			break
		}
	}
	entry.mu.Unlock()

	if !found {
		http.Error(w, "tunnel not found", http.StatusNotFound)
		return
	}

	if err := entry.sendMsg(proto.TypeSetTunnelDir, proto.SetTunnelDir{
		TunnelName: tunnelName,
		Dir:        req.Dir,
	}); err != nil {
		http.Error(w, "send to client: "+err.Error(), http.StatusInternalServerError)
		return
	}

	slog.Info("tunnel dir updated", "client", clientID, "tunnel", tunnelName, "dir", req.Dir)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true}`)
}

// handlePromoteTrigger sends a TypePromote message to the client, which then
// tarballs its project dir and POSTs it to /_2nnel/promote.
func (s *Server) handlePromoteTrigger(w http.ResponseWriter, r *http.Request, clientID, tunnelName string) {
	s.registry.mu.RLock()
	entry, ok := s.registry.byID[clientID]
	s.registry.mu.RUnlock()
	if !ok {
		http.Error(w, "client not found", http.StatusNotFound)
		return
	}
	if err := entry.sendMsg(proto.TypePromote, proto.Promote{TunnelName: tunnelName}); err != nil {
		http.Error(w, "send to client: "+err.Error(), http.StatusInternalServerError)
		return
	}
	slog.Info("promote triggered", "client", clientID, "tunnel", tunnelName)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true}`)
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
