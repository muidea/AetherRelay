package biz

import (
	"aetherrelay/internal/modules/blocks/chatgptimagestore/internal/store"
	"aetherrelay/internal/modules/blocks/chatgptimagestore/pkg/common"
	events "aetherrelay/internal/modules/blocks/chatgptimagestore/pkg/events"
	"bytes"
	"context"
	"github.com/muidea/magicCommon/event"
	"testing"
	"time"
)

func TestContentCommandBoundedAndDetectsReplacement(t *testing.T) {
	s := &ImageStore{store: store.New(t.TempDir())}
	data := bytes.Repeat([]byte("sample"), 100000)
	saved, err := s.store.Save(data, "", "client")
	if err != nil {
		t.Fatal(err)
	}
	hub := event.NewHub(4)
	defer hub.Terminate(context.Background())
	obs := event.NewSimpleObserver(common.UnitID, hub)
	obs.Subscribe(events.TopicOpenContent, s.handleOpenContent)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	send := func(cmd events.OpenContentCommand) (events.OpenContentResult, bool) {
		t.Helper()
		res := hub.Send(event.NewEventWithContext(events.TopicOpenContent, "test", common.UnitID, event.NewHeader(), ctx, cmd))
		value, e := res.Get()
		if e != nil {
			return events.OpenContentResult{}, false
		}
		return value.(events.OpenContentResult), true
	}
	cmd := events.OpenContentCommand{APIKeyID: "client", RelativePath: saved.RelativePath}
	meta, ok := send(cmd)
	if !ok || !meta.Found || meta.Size != int64(len(data)) || len(meta.Bytes) != 0 {
		t.Fatalf("metadata: %+v", meta)
	}
	cmd.Version = meta.Version
	cmd.Offset = 17
	cmd.Length = events.MaxContentChunkBytes
	part, ok := send(cmd)
	if !ok || !bytes.Equal(part.Bytes, data[17:17+cmd.Length]) {
		t.Fatal("bounded content mismatch")
	}
	cmd.Length++
	if _, ok := send(cmd); ok {
		t.Fatal("oversized read accepted")
	}
	cmd.Length = 32
	cmd.Version = "stale"
	if _, ok := send(cmd); ok {
		t.Fatal("stale version accepted")
	}
	cmd.Version = meta.Version
	cmd.APIKeyID = "other"
	missing, ok := send(cmd)
	if !ok || missing.Found {
		t.Fatal("cross-scope read succeeded")
	}
}
