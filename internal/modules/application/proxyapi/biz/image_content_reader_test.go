package biz

import (
	proxycommon "aetherrelay/internal/modules/application/proxyapi/pkg/common"
	basebiz "aetherrelay/internal/modules/base/biz"
	imgcommon "aetherrelay/internal/modules/blocks/chatgptimagestore/pkg/common"
	events "aetherrelay/internal/modules/blocks/chatgptimagestore/pkg/events"
	"bytes"
	"context"
	"github.com/muidea/magicCommon/event"
	"io"
	"testing"
)

func TestImageReaderChunksSeekAndCancellation(t *testing.T) {
	hub := event.NewHub(4)
	defer hub.Terminate(context.Background())
	data := bytes.Repeat([]byte("abcdef"), 100000)
	obs := event.NewSimpleObserver(imgcommon.UnitID, hub)
	obs.Subscribe(events.TopicOpenContent, func(ev event.Event, res event.Result) {
		cmd := ev.Data().(events.OpenContentCommand)
		if cmd.Length > events.MaxContentChunkBytes {
			t.Error("unbounded content command")
		}
		end := min(int(cmd.Offset)+cmd.Length, len(data))
		res.Set(events.OpenContentResult{Found: true, Name: "image.png", Size: int64(len(data)), Version: "v1", Bytes: append([]byte(nil), data[cmd.Offset:int64(end)]...)}, nil)
	})
	p := &Proxy{Base: basebiz.New(proxycommon.UnitID, hub, nil)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	content, err := p.OpenImage(ctx, "client", "image.png")
	if err != nil {
		t.Fatal(err)
	}
	defer content.Reader.Close()
	got, err := io.ReadAll(content.Reader)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read all: %d %v", len(got), err)
	}
	if _, err := content.Reader.Seek(-23, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	got, err = io.ReadAll(content.Reader)
	if err != nil || !bytes.Equal(got, data[len(data)-23:]) {
		t.Fatal("range mismatch")
	}
	cancel()
	if _, err := content.Reader.Read(make([]byte, 1)); err == nil {
		t.Fatal("canceled read succeeded")
	}
}
