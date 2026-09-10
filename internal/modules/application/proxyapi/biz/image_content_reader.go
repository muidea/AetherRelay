package biz

import (
	"context"
	"fmt"
	"io"

	imgevents "aetherrelay/internal/modules/blocks/chatgptimagestore/pkg/events"
)

// The adapter owns only its cursor. File descriptors never cross EventHub;
// each bounded read is opened and closed by the image-store owner.
type imageContentReader struct {
	ctx          context.Context
	proxy        *Proxy
	command      imgevents.OpenContentCommand
	size, offset int64
	closed       bool
}

func (r *imageContentReader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, fmt.Errorf("image reader closed")
	}
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if r.offset >= r.size {
		return 0, io.EOF
	}
	cmd := r.command
	cmd.Offset = r.offset
	cmd.Length = min(len(p), imgevents.MaxContentChunkBytes)
	result, err := r.proxy.readImageContent(r.ctx, cmd)
	if err != nil {
		return 0, err
	}
	if len(result.Bytes) == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	n := copy(p, result.Bytes)
	r.offset += int64(n)
	return n, nil
}
func (r *imageContentReader) Seek(offset int64, whence int) (int64, error) {
	if r.closed {
		return 0, fmt.Errorf("image reader closed")
	}
	base := int64(0)
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		base = r.offset
	case io.SeekEnd:
		base = r.size
	default:
		return 0, fmt.Errorf("invalid seek origin")
	}
	next := base + offset
	if next < 0 || (offset > 0 && next < base) {
		return 0, fmt.Errorf("invalid image offset")
	}
	r.offset = next
	return next, nil
}
func (r *imageContentReader) Close() error { r.closed = true; return nil }
