package client

import (
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/22or/2nnel/internal/config"
	"github.com/22or/2nnel/internal/proto"
	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
)

// session holds the live connection state for one connect attempt.
type session struct {
	cfg    *config.ClientConfig
	cfgMu  sync.Mutex
	ws     *websocket.Conn
	mux    *yamux.Session
	ctrl   *proto.ControlConn
	onSave func(*config.ClientConfig) // called after tunnel list changes
}

func newSession(cfg *config.ClientConfig) *session {
	return &session{cfg: cfg}
}


// connect dials the server, upgrades to yamux, authenticates, and registers tunnels.
func (s *session) connect() error {
	dialer := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
	}

	ws, _, err := dialer.Dial(s.cfg.Server+"/ws", nil)
	if err != nil {
		return fmt.Errorf("dial %s: %w", s.cfg.Server, err)
	}

	conn := proto.NewWSConn(ws)
	mux, err := yamux.Client(conn, proto.YamuxConf())
	if err != nil {
		_ = ws.Close()
		return fmt.Errorf("yamux client: %w", err)
	}

	// Open the control stream (first yamux stream, client-initiated).
	ctrlStream, err := mux.Open()
	if err != nil {
		_ = mux.Close()
		return fmt.Errorf("open control stream: %w", err)
	}

	ctrl := proto.NewControlConn(ctrlStream)

	// Authenticate.
	if err := ctrl.Send(proto.TypeAuth, proto.Auth{
		Token:   s.cfg.AuthToken,
		Version: "1.0",
	}); err != nil {
		_ = mux.Close()
		return fmt.Errorf("send auth: %w", err)
	}

	env, err := ctrl.Recv()
	if err != nil {
		_ = mux.Close()
		return fmt.Errorf("recv auth ack: %w", err)
	}
	switch env.Type {
	case proto.TypeAuthAck:
		var ack proto.AuthAck
		_ = env.Unmarshal(&ack)
		slog.Info("authenticated", "client_id", ack.ClientID)
	case proto.TypeAuthError:
		var ae proto.AuthError
		_ = env.Unmarshal(&ae)
		_ = mux.Close()
		return fmt.Errorf("auth rejected: %s", ae.Error)
	default:
		_ = mux.Close()
		return fmt.Errorf("unexpected message type %q during auth", env.Type)
	}

	// Register tunnels.
	for _, t := range s.cfg.Tunnels {
		subdomain := t.Subdomain
		if subdomain == "" {
			subdomain = t.Name
		}
		msg := proto.RegisterTunnel{
			Name:       t.Name,
			Type:       t.Type,
			Subdomain:  subdomain,
			RemotePort: t.RemotePort,
			LocalAddr:  t.Local,
			HasDir:     t.Dir != "",
		}
		if err := ctrl.Send(proto.TypeRegisterTunnel, msg); err != nil {
			_ = mux.Close()
			return fmt.Errorf("register tunnel %q: %w", t.Name, err)
		}
	}

	// Read registration responses.
	for range s.cfg.Tunnels {
		env, err := ctrl.Recv()
		if err != nil {
			_ = mux.Close()
			return fmt.Errorf("recv tunnel registration: %w", err)
		}
		switch env.Type {
		case proto.TypeTunnelRegistered:
			var tr proto.TunnelRegistered
			_ = env.Unmarshal(&tr)
			slog.Info("tunnel ready", "name", tr.Name, "url", tr.PublicURL)
		case proto.TypeTunnelError:
			var te proto.TunnelError
			_ = env.Unmarshal(&te)
			slog.Error("tunnel registration failed", "name", te.Name, "err", te.Error)
		}
	}

	s.ws = ws
	s.mux = mux
	s.ctrl = ctrl
	return nil
}

// run handles ongoing control messages and incoming data streams.
// Blocks until the session is dead.
func (s *session) run() {
	done := make(chan struct{})

	// Accept data streams from server and proxy them.
	go func() {
		defer close(done)
		s.acceptLoop()
	}()

	// Handle heartbeats and other control messages.
	s.controlLoop()
	<-done
}

func (s *session) controlLoop() {
	for {
		env, err := s.ctrl.Recv()
		if err != nil {
			if err != io.EOF {
				slog.Info("control loop ended", "err", err)
			}
			_ = s.mux.Close()
			return
		}

		switch env.Type {
		case proto.TypeHeartbeat:
			_ = s.ctrl.Send(proto.TypeHeartbeat, proto.Heartbeat{Timestamp: time.Now().UnixMilli()})
		case proto.TypeAddTunnel:
			var at proto.AddTunnel
			if err := env.Unmarshal(&at); err == nil {
				s.dynamicAdd(at)
			}
		case proto.TypeRemoveTunnel:
			var rt proto.RemoveTunnel
			if err := env.Unmarshal(&rt); err == nil {
				s.dynamicRemove(rt.Name)
			}
		case proto.TypePromote:
			var p proto.Promote
			if err := env.Unmarshal(&p); err == nil {
				go s.handlePromote(p)
			}
		case proto.TypeTunnelRegistered:
			var tr proto.TunnelRegistered
			_ = env.Unmarshal(&tr)
			slog.Info("tunnel ready", "name", tr.Name, "url", tr.PublicURL)
		case proto.TypeTunnelError:
			var te proto.TunnelError
			_ = env.Unmarshal(&te)
			slog.Error("tunnel error", "name", te.Name, "err", te.Error)
		default:
			slog.Debug("unhandled control message", "type", env.Type)
		}
	}
}

func (s *session) acceptLoop() {
	for {
		stream, err := s.mux.Accept()
		if err != nil {
			if !s.mux.IsClosed() {
				slog.Error("accept stream", "err", err)
			}
			return
		}
		go handleDataStream(stream, s.cfg)
	}
}

// dynamicAdd registers a new tunnel at runtime (sent by server from dashboard).
func (s *session) dynamicAdd(at proto.AddTunnel) {
	msg := proto.RegisterTunnel{
		Name:       at.Name,
		Type:       at.Type,
		LocalAddr:  at.LocalAddr,
		Subdomain:  at.Subdomain,
		RemotePort: at.RemotePort,
	}
	if msg.Subdomain == "" && msg.Type == "http" {
		msg.Subdomain = msg.Name
	}
	if err := s.ctrl.Send(proto.TypeRegisterTunnel, msg); err != nil {
		slog.Error("dynamic add tunnel failed", "name", at.Name, "err", err)
		return
	}
	// Persist immediately; response handled in controlLoop switch.
	s.cfgMu.Lock()
	s.cfg.Tunnels = append(s.cfg.Tunnels, config.TunnelConfig{
		Name:       at.Name,
		Local:      at.LocalAddr,
		Type:       at.Type,
		Subdomain:  at.Subdomain,
		RemotePort: at.RemotePort,
	})
	cfg := s.cfg
	s.cfgMu.Unlock()
	if s.onSave != nil {
		s.onSave(cfg)
	}
}

// dynamicRemove unregisters a tunnel at runtime.
func (s *session) dynamicRemove(name string) {
	s.cfgMu.Lock()
	tunnels := make([]config.TunnelConfig, 0, len(s.cfg.Tunnels))
	for _, t := range s.cfg.Tunnels {
		if t.Name != name {
			tunnels = append(tunnels, t)
		}
	}
	s.cfg.Tunnels = tunnels
	cfg := s.cfg
	s.cfgMu.Unlock()
	slog.Info("tunnel removed", "name", name)
	if s.onSave != nil {
		s.onSave(cfg)
	}
}
