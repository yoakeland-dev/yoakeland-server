package main

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sync"
	"time"

	"github.com/coder/websocket"

	"yoakeland/internal/protocol"
)

// Browser clients (SPEC.md §2a). Served on the remote TLS listener so they
// share its port, certificate and on/off switch. One http.Server per
// connection keeps acceptLoop's connection tracking exact.
func serveWeb(ctx context.Context, conn net.Conn, network string, deps connDeps, lookupToken func(token string) (name string, rec TokenRecord, ok bool)) {
	mux := http.NewServeMux()
	mux.Handle("/", spaHandler(deps.webRoot))
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		// Default options reject cross-origin upgrades.
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ws.SetReadLimit(protocol.MaxFrameSize + 1) // + the newline
		serveAccepted(ctx, websocket.NetConn(ctx, ws, websocket.MessageText), network, deps, lookupToken)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	_ = srv.Serve(newOneConnListener(conn))
}

// Unknown paths get index.html; the app routes client-side.
func spaHandler(root string) http.Handler {
	if root == "" {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "this yoakeland server has no web UI (start it with -web-root)", http.StatusNotFound)
		})
	}
	files := http.FileServer(http.Dir(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fi, err := os.Stat(filepath.Join(root, filepath.FromSlash(path.Clean("/"+r.URL.Path)))); err != nil || fi.IsDir() {
			r.URL.Path = "/"
		}
		files.ServeHTTP(w, r)
	})
}

// Reads through r so peeked bytes aren't lost.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// Serve returns once the connection closes, including after a /ws hijack.
type oneConnListener struct {
	conn net.Conn
	once sync.Once
	done chan struct{}
}

type closeNotifyConn struct {
	net.Conn
	once *sync.Once
	done chan struct{}
}

func (c *closeNotifyConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.done) })
	return err
}

func newOneConnListener(conn net.Conn) *oneConnListener {
	done := make(chan struct{})
	return &oneConnListener{conn: &closeNotifyConn{Conn: conn, once: &sync.Once{}, done: done}, done: done}
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	var c net.Conn
	l.once.Do(func() { c = l.conn })
	if c != nil {
		return c, nil
	}
	<-l.done
	return nil, net.ErrClosed
}

func (l *oneConnListener) Close() error   { return nil }
func (l *oneConnListener) Addr() net.Addr { return l.conn.LocalAddr() }
