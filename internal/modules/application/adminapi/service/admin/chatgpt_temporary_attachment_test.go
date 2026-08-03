package admin

import (
	"bytes"
	"mime/multipart"
	"net/http/httptest"
	"testing"
)

func TestDecodeTemporaryTurnAcceptsSupportedFileAttachment(t *testing.T) {
	var payload bytes.Buffer
	writer := multipart.NewWriter(&payload)
	if err := writer.WriteField("content", "summarize"); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("attachments", "notes.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("# hello")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/turns", &payload)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	body, images, attachments, err := decodeTemporaryTurnBody(httptest.NewRecorder(), req)
	if err != nil || body.Content != "summarize" || len(images) != 0 || len(attachments) != 1 || attachments[0].Name != "notes.md" || attachments[0].ContentType != "text/markdown" {
		t.Fatalf("body=%+v images=%d attachments=%+v err=%v", body, len(images), attachments, err)
	}
}

func TestDecodeTemporaryTurnRejectsUnsupportedFileAttachment(t *testing.T) {
	var payload bytes.Buffer
	writer := multipart.NewWriter(&payload)
	part, err := writer.CreateFormFile("attachments", "run.exe")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("MZ"))
	_ = writer.Close()
	req := httptest.NewRequest("POST", "/turns", &payload)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	if _, _, _, err := decodeTemporaryTurnBody(httptest.NewRecorder(), req); err == nil {
		t.Fatal("unsupported attachment was accepted")
	}
}
