package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	enginehttp "github.com/muidea/magicEngine/http"
)

type codexWritePipeListener struct {
	conn   net.Conn
	taken  bool
	closed chan struct{}
	once   sync.Once
}

func (l *codexWritePipeListener) Accept() (net.Conn, error) {
	if !l.taken {
		l.taken = true
		return l.conn, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}

func (l *codexWritePipeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *codexWritePipeListener) Addr() net.Addr { return l.conn.LocalAddr() }

func serveCodexWritePipe(t *testing.T, handler http.Handler, state func(net.Conn, http.ConnState)) net.Conn {
	t.Helper()
	serverConn, client := net.Pipe()
	listener := &codexWritePipeListener{conn: serverConn, closed: make(chan struct{})}
	server := &http.Server{Handler: handler, ConnState: state}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
		<-done
	})
	return client
}

func TestCodexBufferedFinalWriteRetainsDeadline(t *testing.T) {
	closed, returned := make(chan struct{}), make(chan struct{})
	client := serveCodexWritePipe(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(returned)
		w = &archiveResponseWriter{ResponseWriter: enginehttp.NewResponseWriter(w)}
		w, finish := guardCodexClientWrites(w, r.Context(), 20*time.Millisecond)
		defer finish()
		if _, err := w.Write([]byte(`{"ok":true}`)); err != nil {
			t.Error(err)
		}
	}), func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			close(closed)
		}
	})
	if _, err := io.WriteString(client, "GET / HTTP/1.1\r\nHost: test\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("small response was not buffered")
	}
	// The client never reads. The implicit final flush happens after the guard
	// returns and must still time out, allowing net/http to close the connection.
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("net/http final buffered write lost its deadline")
	}
}

func TestCodexWriteDeadlineDoesNotLeakIntoReusedConnection(t *testing.T) {
	requests := 0
	client := serveCodexWritePipe(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			var finish func()
			w, finish = guardCodexClientWrites(w, r.Context(), 20*time.Millisecond)
			defer finish()
		}
		_, _ = w.Write([]byte("ok"))
	}), nil)
	if err := client.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(client)
	for i := 0; i < 2; i++ {
		if _, err := io.WriteString(client, "GET / HTTP/1.1\r\nHost: test\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		response, err := http.ReadResponse(reader, nil)
		if err != nil {
			t.Fatal("reused connection retained an old write deadline", err)
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil || string(body) != "ok" {
			t.Fatal("response failed", err)
		}
		if i == 0 {
			time.Sleep(60 * time.Millisecond)
		}
	}
}

// net.Pipe models a connected client that keeps its socket open without reading.
type stalledClientWriter struct {
	net.Conn
	header  http.Header
	entered chan struct{}
	once    sync.Once
}

func (w *stalledClientWriter) Header() http.Header { return w.header }
func (w *stalledClientWriter) WriteHeader(int)     {}
func (w *stalledClientWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	return w.Conn.Write(p)
}
func (w *stalledClientWriter) FlushError() error {
	_, err := w.Write([]byte("flush"))
	return err
}

func TestCodexClientWriteAndFlushUnblockOnDeadlineOrCancel(t *testing.T) {
	for _, operation := range []string{"write", "flush"} {
		for _, reason := range []string{"deadline", "cancel"} {
			t.Run(operation+"/"+reason, func(t *testing.T) {
				server, client := net.Pipe()
				defer server.Close()
				defer client.Close()
				base := &stalledClientWriter{Conn: server, header: http.Header{}, entered: make(chan struct{})}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				timeout := 20 * time.Millisecond
				if reason == "cancel" {
					timeout = time.Minute
				}
				// Exercise the real gateway and archive wrapper chain: a Flush-only
				// wrapper would otherwise swallow the underlying timeout error.
				gateway := enginehttp.NewResponseWriter(base)
				archived := &archiveResponseWriter{ResponseWriter: gateway}
				w, finish := guardCodexClientWrites(archived, ctx, timeout)
				defer finish()
				done := make(chan error, 1)
				go func() {
					if operation == "flush" {
						done <- http.NewResponseController(w).Flush()
					} else {
						_, err := w.Write([]byte("event"))
						done <- err
					}
				}()
				select {
				case <-base.entered:
				case <-time.After(time.Second):
					t.Fatal("write did not start")
				}
				if reason == "cancel" {
					cancel()
				}
				select {
				case err := <-done:
					var ne net.Error
					if !errors.As(err, &ne) || !ne.Timeout() {
						t.Fatalf("blocked client write returned %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("client write retained the stream after cancellation/deadline")
				}
			})
		}
	}
}
