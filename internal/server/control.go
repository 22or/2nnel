package server

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/22or/2nnel/internal/proto"
	"github.com/hashicorp/yamux"
)

// controlHandler manages a single client's control session.
type controlHandler struct {
	server   *Server
	session  *yamux.Session
	remote   string
	clientID string
}

func (h *controlHandler) run() {
	// Client opens the first yamux stream as the control channel.
	ctrlStream, err := h.session.Accept()
	if err != nil {
		slog.Error("accept control stream", "remote", h.remote, "err", err)
		_ = h.session.Close()
		return
	}

	ctrl := proto.NewControlConn(ctrlStream)

	h.clientID, err = h.authenticate(ctrl)
	if err != nil {
		slog.Warn("auth failed", "remote", h.remote, "err", err)
		_ = ctrlStream.Close()
		_ = h.session.Close()
		return
	}

	slog.Info("client authenticated", "id", h.clientID, "remote", h.remote)

	entry := &clientEntry{
		id:          h.clientID,
		remote:      h.remote,
		connectedAt: time.Now(),
		session:     h.session,
		ctrl:        ctrl,
	}
	h.server.registry.addClient(entry)
	defer h.server.registry.removeClient(h.clientID)
	defer func() {
		_ = h.session.Close()
		slog.Info("client disconnected", "id", h.clientID, "remote", h.remote)
	}()

	h.loop(ctrl)
}

func (h *controlHandler) authenticate(ctrl *proto.ControlConn) (string, error) {
	env, err := ctrl.Recv()
	if err != nil {
		return "", fmt.Errorf("read auth: %w", err)
	}
	if env.Type != proto.TypeAuth {
		return "", fmt.Errorf("expected auth, got %q", env.Type)
	}
	var authMsg proto.Auth
	if err := env.Unmarshal(&authMsg); err != nil {
		return "", fmt.Errorf("decode auth: %w", err)
	}

	token := h.server.cfg.AuthToken
	if token != "" && authMsg.Token != token {
		_ = ctrl.Send(proto.TypeAuthError, proto.AuthError{Error: "invalid token"})
		return "", fmt.Errorf("invalid token")
	}

	clientID := newID()
	if err := ctrl.Send(proto.TypeAuthAck, proto.AuthAck{ClientID: clientID}); err != nil {
		return "", fmt.Errorf("send auth_ack: %w", err)
	}
	return clientID, nil
}

func (h *controlHandler) loop(ctrl *proto.ControlConn) {
	lastSeen := time.Now()
	const deadline = 90 * time.Second

	// Periodic heartbeat + timeout check.
	go func() {
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for range tick.C {
			if h.session.IsClosed() {
				return
			}
			if time.Since(lastSeen) > deadline {
				slog.Warn("client heartbeat timeout", "id", h.clientID)
				_ = h.session.Close()
				return
			}
			_ = ctrl.Send(proto.TypeHeartbeat, proto.Heartbeat{Timestamp: time.Now().UnixMilli()})
		}
	}()

	for {
		env, err := ctrl.Recv()
		if err != nil {
			if err != io.EOF {
				slog.Info("control stream closed", "id", h.clientID, "err", err)
			}
			return
		}
		lastSeen = time.Now()

		switch env.Type {
		case proto.TypeRegisterTunnel:
			var reg proto.RegisterTunnel
			if err := env.Unmarshal(&reg); err != nil {
				slog.Error("decode RegisterTunnel", "err", err)
				continue
			}
			h.handleRegister(ctrl, &reg)

		case proto.TypeHeartbeat:
			// lastSeen already updated

		default:
			slog.Warn("unknown control message", "type", env.Type)
		}
	}
}

func (h *controlHandler) handleRegister(ctrl *proto.ControlConn, reg *proto.RegisterTunnel) {
	switch reg.Type {
	case "http":
		h.registerHTTP(ctrl, reg)
	case "tcp":
		h.registerTCP(ctrl, reg)
	default:
		_ = ctrl.Send(proto.TypeTunnelError, proto.TunnelError{
			Name:  reg.Name,
			Error: fmt.Sprintf("unknown tunnel type %q", reg.Type),
		})
	}
}

func (h *controlHandler) registerHTTP(ctrl *proto.ControlConn, reg *proto.RegisterTunnel) {
	subdomain := reg.Subdomain
	if subdomain == "" {
		subdomain = reg.Name
	}
	if h.server.registry.isSubdomainTaken(subdomain) {
		_ = ctrl.Send(proto.TypeTunnelError, proto.TunnelError{
			Name:  reg.Name,
			Error: fmt.Sprintf("subdomain %q already in use", subdomain),
		})
		return
	}
	if _, taken := h.server.deployedApps.Load(subdomain); taken {
		_ = ctrl.Send(proto.TypeTunnelError, proto.TunnelError{
			Name:  reg.Name,
			Error: fmt.Sprintf("subdomain %q already in use by a deployed app", subdomain),
		})
		return
	}

	te := &tunnelEntry{
		name:       reg.Name,
		tunnelType: "http",
		subdomain:  subdomain,
		localAddr:  reg.LocalAddr,
	}
	h.server.registry.registerHTTP(h.clientID, subdomain, te)

	publicURL := h.server.buildPublicHTTPURL(subdomain)
	_ = ctrl.Send(proto.TypeTunnelRegistered, proto.TunnelRegistered{
		Name:      reg.Name,
		PublicURL: publicURL,
	})
	slog.Info("HTTP tunnel registered", "client", h.clientID, "subdomain", subdomain, "url", publicURL)
}

func (h *controlHandler) registerTCP(ctrl *proto.ControlConn, reg *proto.RegisterTunnel) {
	port := reg.RemotePort
	if port == 0 {
		var err error
		port, err = h.server.pickTCPPort()
		if err != nil {
			_ = ctrl.Send(proto.TypeTunnelError, proto.TunnelError{Name: reg.Name, Error: err.Error()})
			return
		}
	}

	if len(h.server.cfg.AllowedPorts) > 0 {
		allowed := false
		for _, p := range h.server.cfg.AllowedPorts {
			if p == port {
				allowed = true
				break
			}
		}
		if !allowed {
			_ = ctrl.Send(proto.TypeTunnelError, proto.TunnelError{
				Name: reg.Name, Error: fmt.Sprintf("port %d not in allowed list", port),
			})
			return
		}
	}

	if h.server.registry.isPortTaken(port) {
		_ = ctrl.Send(proto.TypeTunnelError, proto.TunnelError{
			Name: reg.Name, Error: fmt.Sprintf("port %d already in use", port),
		})
		return
	}

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		_ = ctrl.Send(proto.TypeTunnelError, proto.TunnelError{
			Name: reg.Name, Error: fmt.Sprintf("listen port %d: %s", port, err),
		})
		return
	}

	te := &tunnelEntry{
		name:       reg.Name,
		tunnelType: "tcp",
		remotePort: port,
		localAddr:  reg.LocalAddr,
		tcpLn:      ln,
	}
	h.server.registry.registerTCP(h.clientID, port, te)

	var publicURL string
	if h.server.cfg.Domain != "" {
		publicURL = fmt.Sprintf("%s:%d", h.server.cfg.Domain, port)
	} else {
		publicURL = fmt.Sprintf("(port: %d)", port)
	}

	_ = ctrl.Send(proto.TypeTunnelRegistered, proto.TunnelRegistered{
		Name: reg.Name, PublicURL: publicURL,
	})
	slog.Info("TCP tunnel registered", "client", h.clientID, "port", port, "local", reg.LocalAddr)

	go h.serveTCP(ln, te)
}

func (h *controlHandler) serveTCP(ln net.Listener, te *tunnelEntry) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if !h.session.IsClosed() {
				slog.Error("TCP accept", "port", te.remotePort, "err", err)
			}
			return
		}
		go h.forwardTCP(conn, te)
	}
}

func (h *controlHandler) forwardTCP(conn net.Conn, te *tunnelEntry) {
	defer conn.Close()

	stream, err := h.session.Open()
	if err != nil {
		slog.Error("open yamux stream", "tunnel", te.name, "err", err)
		return
	}
	defer stream.Close()

	if err := writeStreamHdr(stream, te.name, te.localAddr); err != nil {
		slog.Error("write stream header", "err", err)
		return
	}

	te.activeConns.Add(1)
	defer te.activeConns.Add(-1)

	slog.Info("TCP forward", "port", te.remotePort, "remote", conn.RemoteAddr())
	pipeCount(conn, stream, te)
}
