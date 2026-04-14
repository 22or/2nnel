package proto

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// Control message types.
const (
	TypeAuth             = "auth"
	TypeAuthAck          = "auth_ack"
	TypeAuthError        = "auth_error"
	TypeRegisterTunnel   = "register_tunnel"
	TypeTunnelRegistered = "tunnel_registered"
	TypeTunnelError      = "tunnel_error"
	TypeHeartbeat        = "heartbeat"
	TypeAddTunnel        = "add_tunnel"
	TypeRemoveTunnel     = "remove_tunnel"
	TypePromote          = "promote"          // server→client: trigger promote upload
	TypePromoteError     = "promote_error"    // client→server: promote failed
	TypeSetTunnelDir     = "set_tunnel_dir"   // server→client: update project dir for a tunnel
)

// Envelope wraps all control-channel messages.
type Envelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// Unmarshal unpacks the Data field into v.
func (e *Envelope) Unmarshal(v any) error {
	return json.Unmarshal(e.Data, v)
}

// Auth is sent by client immediately after opening the control stream.
type Auth struct {
	Token   string `json:"token"`
	Version string `json:"version"`
	Name    string `json:"name"` // user-provided client name (display only)
}

// AuthAck is the server's success response to Auth.
type AuthAck struct {
	ClientID string `json:"client_id"`
}

// AuthError is the server's failure response to Auth.
type AuthError struct {
	Error string `json:"error"`
}

// RegisterTunnel is sent by client for each tunnel it wants.
type RegisterTunnel struct {
	Name       string `json:"name"`
	Type       string `json:"type"`        // "http" or "tcp"
	Subdomain  string `json:"subdomain"`   // for http tunnels
	RemotePort int    `json:"remote_port"` // for tcp tunnels
	LocalAddr  string `json:"local_addr"`
	HasDir     bool   `json:"has_dir"` // project dir configured; promote available
	Dir        string `json:"dir"`     // client-side project directory path (for display)
}

// TunnelRegistered is sent by server when a tunnel is ready.
type TunnelRegistered struct {
	Name      string `json:"name"`
	PublicURL string `json:"public_url"`
}

// TunnelError is sent by server when tunnel registration fails.
type TunnelError struct {
	Name  string `json:"name"`
	Error string `json:"error"`
}

// Heartbeat is sent by both sides periodically.
type Heartbeat struct {
	Timestamp int64 `json:"timestamp"`
}

// AddTunnel is sent server→client to dynamically register a new tunnel.
type AddTunnel struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	LocalAddr  string `json:"local_addr"`
	Subdomain  string `json:"subdomain,omitempty"`
	RemotePort int    `json:"remote_port,omitempty"`
}

// RemoveTunnel is sent server→client to tear down a tunnel by name.
type RemoveTunnel struct {
	Name string `json:"name"`
}

// Promote is sent server→client to trigger a promote upload for a tunnel.
type Promote struct {
	TunnelName string `json:"tunnel_name"`
}

// PromoteError is sent client→server when promote cannot proceed.
type PromoteError struct {
	TunnelName string `json:"tunnel_name"`
	Error      string `json:"error"`
}

// SetTunnelDir is sent server→client to update the project directory for a tunnel.
type SetTunnelDir struct {
	TunnelName string `json:"tunnel_name"`
	Dir        string `json:"dir"` // empty string clears the dir
}

// StreamHeader is the first thing written on every data stream by the server.
// The client reads it to know which local address to dial.
type StreamHeader struct {
	TunnelName string `json:"tunnel_name"`
	LocalAddr  string `json:"local_addr"`
}

// ControlConn wraps a ReadWriter with JSON encode/decode for control messages.
// A single ControlConn must not be used for concurrent reads or concurrent writes,
// but concurrent read + write is fine.
type ControlConn struct {
	enc   *json.Encoder
	dec   *json.Decoder
	wmu   sync.Mutex // guards enc (writes)
}

// NewControlConn wraps rw.
func NewControlConn(rw io.ReadWriter) *ControlConn {
	return &ControlConn{
		enc: json.NewEncoder(rw),
		dec: json.NewDecoder(rw),
	}
}

// Send encodes msgType+data and writes it to the underlying writer.
func (c *ControlConn) Send(msgType string, data any) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.enc.Encode(Envelope{Type: msgType, Data: payload})
}

// Recv blocks until a message is available and returns it.
func (c *ControlConn) Recv() (*Envelope, error) {
	var env Envelope
	if err := c.dec.Decode(&env); err != nil {
		return nil, err
	}
	return &env, nil
}
