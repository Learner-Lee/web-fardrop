package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
)

const tunnelHost = "fardrop-origin"
const tunnelPath = base + "_origin"
const tunnelFrame = 64 << 10

type wsStream struct {
	*websocket.Conn
	reader io.Reader
}

func (s *wsStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if s.reader == nil {
			kind, reader, err := s.NextReader()
			if err != nil {
				return 0, err
			}
			if kind != websocket.BinaryMessage {
				return 0, errors.New("expected binary Origin tunnel data")
			}
			s.reader = reader
		}
		n, err := s.reader.Read(p)
		if err == io.EOF {
			s.reader = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

func (s *wsStream) Write(p []byte) (int, error) {
	written := 0
	for written < len(p) {
		end := min(written+tunnelFrame, len(p))
		if err := s.SetWriteDeadline(time.Now().Add(20 * time.Second)); err != nil {
			return written, err
		}
		if err := s.WriteMessage(websocket.BinaryMessage, p[written:end]); err != nil {
			return written, err
		}
		written = end
	}
	return written, nil
}

func tunnelConfig() *yamux.Config {
	config := yamux.DefaultConfig()
	config.KeepAliveInterval = 12 * time.Second
	config.ConnectionWriteTimeout = 20 * time.Second
	config.StreamOpenTimeout = 12 * time.Second
	config.AcceptBacklog = 64
	return config
}

type originTunnel struct {
	mu      sync.Mutex
	session *yamux.Session
	secret  string
}

func (t *originTunnel) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	t.mu.Lock()
	session := t.session
	t.mu.Unlock()
	if session == nil {
		return nil, errors.New("Origin tunnel unavailable")
	}
	return session.OpenStream()
}

func (t *originTunnel) accept(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != "" || !secretsMatch(r.Header.Get(edgeHeader), t.secret) {
		http.NotFound(w, r)
		return
	}
	upgrader := websocket.Upgrader{HandshakeTimeout: 8 * time.Second}
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c.SetReadLimit(tunnelFrame)
	session, err := yamux.Server(&wsStream{Conn: c}, tunnelConfig())
	if err != nil {
		_ = c.Close()
		return
	}
	t.mu.Lock()
	old := t.session
	t.session = session
	t.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	log.Print("FarDrop Origin tunnel connected")
	<-session.CloseChan()
	t.mu.Lock()
	if t.session == session {
		t.session = nil
	}
	t.mu.Unlock()
	log.Print("FarDrop Origin tunnel disconnected")
}

func keepOriginTunnel(ctx context.Context, address, target, secret string) {
	for ctx.Err() == nil {
		err := connectOriginTunnel(ctx, address, target, secret)
		if ctx.Err() != nil {
			return
		}
		log.Printf("FarDrop Origin tunnel reconnecting: %v", err)
		timer := time.NewTimer(3 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}

func connectOriginTunnel(ctx context.Context, address, target, secret string) error {
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	header := http.Header{}
	header.Set(edgeHeader, secret)
	c, response, err := dialer.DialContext(ctx, address, header)
	if err != nil {
		if response != nil {
			_ = response.Body.Close()
		}
		return err
	}
	defer c.Close()
	c.SetReadLimit(tunnelFrame)
	session, err := yamux.Client(&wsStream{Conn: c}, tunnelConfig())
	if err != nil {
		return err
	}
	defer session.Close()
	for {
		stream, err := session.AcceptStreamWithContext(ctx)
		if err != nil {
			return err
		}
		go forwardOriginStream(stream, target)
	}
}

func forwardOriginStream(stream *yamux.Stream, target string) {
	defer stream.Close()
	dialer := net.Dialer{Timeout: 3 * time.Second}
	conn, err := dialer.Dial("tcp", target)
	if err != nil {
		return
	}
	defer conn.Close()
	go func() {
		_, _ = io.Copy(conn, stream)
		_ = conn.(*net.TCPConn).CloseWrite()
	}()
	_, _ = io.Copy(stream, conn)
}
