package proto

import (
	"bytes"
	"io"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// WSConn adapts a *websocket.Conn to net.Conn so yamux can use it.
// WebSocket is message-based; this adapter buffers the current message
// and presents a stream interface.
type WSConn struct {
	ws     *websocket.Conn
	reader io.Reader // current message reader
	rmu    sync.Mutex
	wmu    sync.Mutex
}

// NewWSConn wraps ws as a net.Conn.
func NewWSConn(ws *websocket.Conn) net.Conn {
	return &WSConn{ws: ws}
}

func (c *WSConn) Read(b []byte) (int, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()

	for {
		if c.reader != nil {
			n, err := c.reader.Read(b)
			if err == io.EOF {
				c.reader = nil
				if n > 0 {
					return n, nil
				}
				continue
			}
			return n, err
		}
		_, msg, err := c.ws.ReadMessage()
		if err != nil {
			return 0, err
		}
		c.reader = bytes.NewReader(msg)
	}
}

func (c *WSConn) Write(b []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	// Copy so caller can reuse b immediately.
	buf := make([]byte, len(b))
	copy(buf, b)
	if err := c.ws.WriteMessage(websocket.BinaryMessage, buf); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *WSConn) Close() error                       { return c.ws.Close() }
func (c *WSConn) LocalAddr() net.Addr                { return c.ws.LocalAddr() }
func (c *WSConn) RemoteAddr() net.Addr               { return c.ws.RemoteAddr() }
func (c *WSConn) SetDeadline(t time.Time) error      { return c.ws.SetReadDeadline(t) }
func (c *WSConn) SetReadDeadline(t time.Time) error  { return c.ws.SetReadDeadline(t) }
func (c *WSConn) SetWriteDeadline(t time.Time) error { return c.ws.SetWriteDeadline(t) }
