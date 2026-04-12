package klient

import (
	"bytes"
	"io"
	"net"
	"time"

	"github.com/gorilla/websocket"
)

type wsConn struct {
	ws     *websocket.Conn
	reader io.Reader
}

func (c *wsConn) Read(p []byte) (int, error) {
	for {
		if c.reader != nil {
			n, err := c.reader.Read(p)
			if n > 0 {
				return n, nil
			}
			if err != nil {
				c.reader = nil
				if err != io.EOF {
					return 0, err
				}
			}
		}

		_, msg, err := c.ws.ReadMessage()
		if err != nil {
			return 0, err
		}

		if len(msg) > 0 && msg[0] == 0 {
			c.reader = bytes.NewReader(msg[1:])
		}
		// ignore other channels (like error channel 1)
	}
}

func (c *wsConn) Write(p []byte) (int, error) {
	msg := make([]byte, len(p)+1)
	msg[0] = 0
	copy(msg[1:], p)
	err := c.ws.WriteMessage(websocket.BinaryMessage, msg)
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *wsConn) Close() error {
	return c.ws.Close()
}

func (c *wsConn) LocalAddr() net.Addr {
	return c.ws.LocalAddr()
}

func (c *wsConn) RemoteAddr() net.Addr {
	return c.ws.RemoteAddr()
}

func (c *wsConn) SetDeadline(t time.Time) error {
	if err := c.ws.SetReadDeadline(t); err != nil {
		return err
	}
	return c.ws.SetWriteDeadline(t)
}

func (c *wsConn) SetReadDeadline(t time.Time) error {
	return c.ws.SetReadDeadline(t)
}

func (c *wsConn) SetWriteDeadline(t time.Time) error {
	return c.ws.SetWriteDeadline(t)
}
