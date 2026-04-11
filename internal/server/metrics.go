package server

import (
	"encoding/json"
	"net/http"
	"time"
)

var startTime = time.Now()

// metricsResponse is the JSON payload from /metrics.
type metricsResponse struct {
	UptimeSeconds  float64        `json:"uptime_seconds"`
	ConnectedClients int          `json:"connected_clients"`
	ActiveTunnels  tunnelCounts   `json:"active_tunnels"`
}

type tunnelCounts struct {
	HTTP int `json:"http"`
	TCP  int `json:"tcp"`
}

// handleMetrics serves a JSON snapshot of server state.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	s.registry.mu.RLock()
	clients := len(s.registry.byID)
	httpTunnels := len(s.registry.bySubdomain)
	tcpTunnels := len(s.registry.byPort)
	s.registry.mu.RUnlock()

	resp := metricsResponse{
		UptimeSeconds:    time.Since(startTime).Seconds(),
		ConnectedClients: clients,
		ActiveTunnels: tunnelCounts{
			HTTP: httpTunnels,
			TCP:  tcpTunnels,
		},
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
