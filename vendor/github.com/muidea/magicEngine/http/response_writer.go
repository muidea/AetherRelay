package http

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
)

type ResponseWriter interface {
	http.ResponseWriter
	Status() int
	Written() bool
	Size() int
}

func NewResponseWriter(rw http.ResponseWriter) ResponseWriter {
	return &responseWriter{responseWriter: rw}
}

type responseWriter struct {
	responseWriter http.ResponseWriter
	status         int
	size           int
}

func (rw *responseWriter) Header() http.Header {
	return rw.responseWriter.Header()
}

func (rw *responseWriter) WriteHeader(s int) {
	rw.responseWriter.WriteHeader(s)
	rw.status = s
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	if !rw.Written() {
		rw.WriteHeader(http.StatusOK)
	}
	size, err := rw.responseWriter.Write(b)
	rw.size += size
	return size, err
}

func (rw *responseWriter) Status() int {
	return rw.status
}

func (rw *responseWriter) Size() int {
	return rw.size
}

func (rw *responseWriter) Written() bool {
	return rw.status != 0
}

func (rw *responseWriter) Flush() {
	if flusher, ok := rw.responseWriter.(http.Flusher); ok {
		if !rw.Written() {
			rw.WriteHeader(http.StatusOK)
		}
		flusher.Flush()
	}
}

// Hijack preserves protocol upgrades through the accounting wrapper. Marking
// the response written prevents the route context from appending a 204 after a
// successful upgrade.
func (rw *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := rw.responseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("underlying response writer does not support hijacking: %w", http.ErrNotSupported)
	}
	conn, buffer, err := hijacker.Hijack()
	if err == nil && !rw.Written() {
		rw.status = http.StatusSwitchingProtocols
	}
	return conn, buffer, err
}

// Unwrap lets http.ResponseController discover optional interfaces implemented
// by the listener's original ResponseWriter.
func (rw *responseWriter) Unwrap() http.ResponseWriter {
	return rw.responseWriter
}
