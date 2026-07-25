package proxy

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"testing"
)

func TestParseChatGPTImageMultipartReadsImageAndMask(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("prompt", "edit"); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("n", "1"); err != nil {
		t.Fatal(err)
	}
	image, err := writer.CreateFormFile("image", "image.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(image, "image-bytes"); err != nil {
		t.Fatal(err)
	}
	mask, err := writer.CreateFormFile("mask", "mask.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(mask, "mask-bytes"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/v1/images/edits", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	parsed, images, masks, err := parseChatGPTImageMultipart(rec, req, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Prompt != "edit" || parsed.N != 1 || len(images) != 1 || len(masks) != 1 || string(images[0]) != "image-bytes" || string(masks[0]) != "mask-bytes" {
		t.Fatalf("parsed=%+v images=%q masks=%q", parsed, images, masks)
	}
}
