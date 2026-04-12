package server

import (
	"context"
	cryptotls "crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/22or/2nnel/internal/config"
	"github.com/22or/2nnel/internal/proto"
	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
)

// Server is the 2nnel relay server.
type Server struct {
	cfg          *config.ServerConfig
	registry     *Registry
	upgrader     websocket.Upgrader
	deployedApps sync.Map // key: name string → *deployedApp
}

// New creates a Server with cfg.
func New(cfg *config.ServerConfig) *Server {
	return &Server{
		cfg:      cfg,
		registry: newRegistry(),
		upgrader: websocket.Upgrader{
			CheckOrigin:     func(r *http.Request) bool { return true },
			ReadBufferSize:  65536,
			WriteBufferSize: 65536,
		},
	}
}

// Run starts the server and blocks until SIGTERM/SIGINT.
func (s *Server) Run() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleControl)
	mux.Handle("/_2nnel/", s.adminAuth(http.HandlerFunc(s.handleAdmin)))
	mux.HandleFunc("/", s.handleHTTP)

	var srv *http.Server
	var ln net.Listener
	var err error

	addr := fmt.Sprintf(":%d", s.cfg.Port)

	if s.cfg.Dev {
		ln, err = net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("listen %s: %w", addr, err)
		}
		slog.Info("listening (plain HTTP)", "addr", addr)
		srv = &http.Server{Handler: mux}
	} else {
		tls, err := buildTLSConfig(s.cfg)
		if err != nil {
			return fmt.Errorf("TLS setup: %w", err)
		}
		baseLn, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("listen %s: %w", addr, err)
		}
		ln = cryptotls.NewListener(baseLn, tls.cfg)
		slog.Info("listening (HTTPS/WSS)", "addr", addr)
		srv = &http.Server{Handler: mux, TLSConfig: tls.cfg}

	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		slog.Info("shutdown signal")
		shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		s.stopAllDeployedApps()
		_ = srv.Shutdown(shutCtx)
		s.registry.closeAll()
	}()

	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// pickTCPPort finds the first free port in the configured TCP range.
func (s *Server) pickTCPPort() (int, error) {
	if s.cfg.TCPPortMin == 0 {
		return 0, fmt.Errorf("no TCP port range configured — use --tcp-port-range (e.g. 2200-2300)")
	}
	for port := s.cfg.TCPPortMin; port <= s.cfg.TCPPortMax; port++ {
		if !s.registry.isPortTaken(port) {
			return port, nil
		}
	}
	return 0, fmt.Errorf("no free port in range %d-%d", s.cfg.TCPPortMin, s.cfg.TCPPortMax)
}

// adminAuth wraps an admin handler with optional token check.
func (s *Server) adminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AuthToken != "" {
			tok := r.URL.Query().Get("token")
			if tok == "" {
				user, pass, ok := r.BasicAuth()
				if ok {
					tok = pass
					_ = user
				}
			}
			if tok == "" {
				bearer := r.Header.Get("Authorization")
				if strings.HasPrefix(bearer, "Bearer ") {
					tok = strings.TrimPrefix(bearer, "Bearer ")
				}
			}
			if tok != s.cfg.AuthToken {
				w.Header().Set("WWW-Authenticate", `Basic realm="2nnel admin"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// buildPublicHTTPURL constructs the full public URL for an HTTP tunnel subdomain.
func (s *Server) buildPublicHTTPURL(subdomain string) string {
	cfg := s.cfg
	if cfg.Domain == "" {
		return fmt.Sprintf("(subdomain: %s)", subdomain)
	}
	scheme := "https"
	if cfg.Dev {
		scheme = "http"
	}
	if (scheme == "https" && cfg.Port == 443) || (scheme == "http" && cfg.Port == 80) {
		return fmt.Sprintf("%s://%s.%s", scheme, subdomain, cfg.Domain)
	}
	return fmt.Sprintf("%s://%s.%s:%d", scheme, subdomain, cfg.Domain, cfg.Port)
}

// handleControl upgrades a WebSocket connection from a client and manages the control session.
func (s *Server) handleControl(w http.ResponseWriter, r *http.Request) {
	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("WS upgrade", "err", err)
		return
	}
	slog.Info("client connected", "remote", ws.RemoteAddr())

	conn := proto.NewWSConn(ws)
	session, err := yamux.Server(conn, proto.YamuxConf())
	if err != nil {
		slog.Error("yamux init", "err", err)
		_ = ws.Close()
		return
	}

	h := &controlHandler{
		server:  s,
		session: session,
		remote:  ws.RemoteAddr().String(),
	}
	h.run()
}

// ─── Registry ────────────────────────────────────────────────────────────────

// Registry stores active client sessions indexed by subdomain and TCP port.
type Registry struct {
	mu          sync.RWMutex
	bySubdomain map[string]*clientEntry
	byPort      map[int]*clientEntry
	byID        map[string]*clientEntry
}

type clientEntry struct {
	id          string
	remote      string
	connectedAt time.Time
	session     *yamux.Session
	mu          sync.RWMutex
	tunnels     []*tunnelEntry
	ctrl        *proto.ControlConn
	ctrlMu      sync.Mutex
}

// sendMsg sends a control message to this client.
func (e *clientEntry) sendMsg(msgType string, v any) error {
	e.ctrlMu.Lock()
	defer e.ctrlMu.Unlock()
	if e.ctrl == nil {
		return fmt.Errorf("no control connection")
	}
	return e.ctrl.Send(msgType, v)
}

// tunnelEntry holds config + live metrics for one registered tunnel.
type tunnelEntry struct {
	name       string
	tunnelType string // "http" | "tcp"
	subdomain  string
	remotePort int
	localAddr  string
	tcpLn      net.Listener // non-nil for TCP tunnels

	// metrics — all atomic for lock-free reads by dashboard
	bytesIn     atomic.Int64
	bytesOut    atomic.Int64
	requests    atomic.Int64 // HTTP only
	activeConns atomic.Int32
}

func newRegistry() *Registry {
	return &Registry{
		bySubdomain: make(map[string]*clientEntry),
		byPort:      make(map[int]*clientEntry),
		byID:        make(map[string]*clientEntry),
	}
}

func (reg *Registry) addClient(entry *clientEntry) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.byID[entry.id] = entry
}

func (reg *Registry) removeClient(id string) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	entry, ok := reg.byID[id]
	if !ok {
		return
	}
	entry.mu.RLock()
	tunnels := make([]*tunnelEntry, len(entry.tunnels))
	copy(tunnels, entry.tunnels)
	entry.mu.RUnlock()

	for _, t := range tunnels {
		if t.tunnelType == "http" {
			delete(reg.bySubdomain, t.subdomain)
		} else {
			delete(reg.byPort, t.remotePort)
			if t.tcpLn != nil {
				_ = t.tcpLn.Close()
			}
		}
	}
	delete(reg.byID, id)
}

func (reg *Registry) registerHTTP(clientID, subdomain string, te *tunnelEntry) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	entry := reg.byID[clientID]
	reg.bySubdomain[subdomain] = entry
	entry.mu.Lock()
	entry.tunnels = append(entry.tunnels, te)
	entry.mu.Unlock()
}

func (reg *Registry) registerTCP(clientID string, port int, te *tunnelEntry) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	entry := reg.byID[clientID]
	reg.byPort[port] = entry
	entry.mu.Lock()
	entry.tunnels = append(entry.tunnels, te)
	entry.mu.Unlock()
}

func (reg *Registry) lookupHTTP(subdomain string) (*clientEntry, bool) {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	e, ok := reg.bySubdomain[subdomain]
	return e, ok
}

func (reg *Registry) isSubdomainTaken(subdomain string) bool {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	_, ok := reg.bySubdomain[subdomain]
	return ok
}

func (reg *Registry) isPortTaken(port int) bool {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	_, ok := reg.byPort[port]
	return ok
}

// removeTunnelByName removes a single tunnel from a client by tunnel name.
func (reg *Registry) removeTunnelByName(clientID, tunnelName string) bool {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	entry, ok := reg.byID[clientID]
	if !ok {
		return false
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	for i, t := range entry.tunnels {
		if t.name == tunnelName {
			if t.tunnelType == "http" {
				delete(reg.bySubdomain, t.subdomain)
			} else {
				delete(reg.byPort, t.remotePort)
				if t.tcpLn != nil {
					_ = t.tcpLn.Close()
				}
			}
			entry.tunnels = append(entry.tunnels[:i], entry.tunnels[i+1:]...)
			return true
		}
	}
	return false
}

// disconnectClient closes a client session by ID. Returns false if not found.
func (reg *Registry) disconnectClient(id string) bool {
	reg.mu.RLock()
	entry, ok := reg.byID[id]
	reg.mu.RUnlock()
	if !ok {
		return false
	}
	_ = entry.session.Close()
	return true
}

func (reg *Registry) closeAll() {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	for _, e := range reg.byID {
		_ = e.session.Close()
		e.mu.RLock()
		for _, t := range e.tunnels {
			if t.tcpLn != nil {
				_ = t.tcpLn.Close()
			}
		}
		e.mu.RUnlock()
	}
}

// snapshot returns a point-in-time copy of registry state for the dashboard.
func (reg *Registry) snapshot() []clientSnapshot {
	reg.mu.RLock()
	defer reg.mu.RUnlock()

	out := make([]clientSnapshot, 0, len(reg.byID))
	for _, e := range reg.byID {
		e.mu.RLock()
		ts := make([]tunnelSnapshot, 0, len(e.tunnels))
		for _, t := range e.tunnels {
			endpoint := t.subdomain
			if t.tunnelType == "tcp" {
				endpoint = fmt.Sprintf(":%d", t.remotePort)
			}
			ts = append(ts, tunnelSnapshot{
				Name:        t.name,
				Type:        t.tunnelType,
				Endpoint:    endpoint,
				LocalAddr:   t.localAddr,
				BytesIn:     t.bytesIn.Load(),
				BytesOut:    t.bytesOut.Load(),
				Requests:    t.requests.Load(),
				ActiveConns: int(t.activeConns.Load()),
			})
		}
		e.mu.RUnlock()
		out = append(out, clientSnapshot{
			ID:          e.id,
			Remote:      e.remote,
			ConnectedAt: e.connectedAt,
			Tunnels:     ts,
		})
	}
	return out
}

type clientSnapshot struct {
	ID          string
	Remote      string
	ConnectedAt time.Time
	Tunnels     []tunnelSnapshot
}

type tunnelSnapshot struct {
	Name        string
	Type        string
	Endpoint    string
	LocalAddr   string
	BytesIn     int64
	BytesOut    int64
	Requests    int64
	ActiveConns int
}
