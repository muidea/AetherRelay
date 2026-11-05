package proxy

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
)

const codexClientWriteTimeout = 30 * time.Second

// Reset the deadline for each write, rather than imposing an absolute lifetime
// on long streams. Cancellation interrupts a write already blocked in net/http.
type codexClientResponseWriter struct {
	http.ResponseWriter
	ctx     context.Context
	timeout time.Duration
	mu      sync.Mutex // Serializes cancellation with deadline renewal, not Write.
}

func (w *codexClientResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *codexClientResponseWriter) prepareWrite() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.ctx.Err(); err != nil {
		return err
	}
	err := http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Now().Add(w.timeout))
	if errors.Is(err, http.ErrNotSupported) {
		return nil // In-memory adapters do not own a network connection.
	}
	return err
}

func (w *codexClientResponseWriter) Write(p []byte) (int, error) {
	if err := w.prepareWrite(); err != nil {
		return 0, err
	}
	return w.ResponseWriter.Write(p)
}

func (w *codexClientResponseWriter) FlushError() error {
	if err := w.prepareWrite(); err != nil {
		return err
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *codexClientResponseWriter) Flush() { _ = w.FlushError() }

func guardCodexClientWrites(w http.ResponseWriter, ctx context.Context, timeout time.Duration) (http.ResponseWriter, func()) {
	writer := &codexClientResponseWriter{ResponseWriter: w, ctx: ctx, timeout: timeout}
	canceled := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(canceled)
		writer.mu.Lock()
		defer writer.mu.Unlock()
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now())
	})
	return writer, func() {
		if !stop() {
			<-canceled
		}
		// Leave the deadline in place for net/http's final buffered write after
		// ServeHTTP returns. net/http clears it before reusing the connection.
	}
}
