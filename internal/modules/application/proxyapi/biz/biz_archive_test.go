package biz

import (
	"os"
	"path/filepath"
	"testing"

	config "aetherrelay/internal/pkg/aetherrelayconfig"
)

func TestNewInteractionRecorderIsOptIn(t *testing.T) {
	root := filepath.Join(t.TempDir(), "interactions")
	recorder, err := newInteractionRecorder(config.Config{InteractionDir: root})
	if err != nil {
		t.Fatal(err)
	}
	if recorder != nil {
		t.Fatal("disabled interaction archive must not create a recorder")
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("disabled interaction archive must not create its directory, stat err=%v", err)
	}
}

func TestNewInteractionRecorderCanRecordMetadataWithoutBodies(t *testing.T) {
	root := filepath.Join(t.TempDir(), "interactions")
	recorder, err := newInteractionRecorder(config.Config{
		ArchiveInteractions:  true,
		InteractionDir:       root,
		InteractionRetention: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if recorder == nil || recorder.FullContent() {
		t.Fatal("metadata-only interaction archive was not configured")
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("enabled interaction archive must create its directory: %v", err)
	}
}
