package imageinput

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

func TestDecodeBase64Images(t *testing.T) {
	images, err := DecodeBase64Images([]string{"aGVsbG8=", "data:image/png;base64,d29ybGQ="})
	if err != nil {
		t.Fatal(err)
	}
	if string(images[0]) != "hello" || string(images[1]) != "world" {
		t.Fatalf("decoded=%q", images)
	}
}

func TestDecodeBase64ImagesRejectsInvalidInput(t *testing.T) {
	for _, input := range []string{"", "data:image/png,hello", "%%%"} {
		if _, err := DecodeBase64Images([]string{input}); err == nil {
			t.Fatalf("input %q unexpectedly succeeded", input)
		}
	}
}

func TestDecodeDataURLImageValidatesDeclaredAndDetectedImage(t *testing.T) {
	var pngData bytes.Buffer
	pngImage := image.NewRGBA(image.Rect(0, 0, 1, 1))
	pngImage.SetRGBA(0, 0, color.RGBA{R: 1, G: 2, B: 3, A: 255})
	if err := png.Encode(&pngData, pngImage); err != nil {
		t.Fatal(err)
	}
	image, err := DecodeDataURLImage("data:image/png;base64," + base64.StdEncoding.EncodeToString(pngData.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if image.MIMEType != "image/png" || !bytes.Equal(image.Bytes, pngData.Bytes()) {
		t.Fatalf("image=%+v", image)
	}
	for _, value := range []string{
		"https://images.example.test/demo.png",
		"data:text/plain;base64,aGVsbG8=",
		"data:image/png,hello",
		"data:image/png;base64,aGVsbG8=",
	} {
		if _, err := DecodeDataURLImage(value); err == nil {
			t.Fatalf("input %q unexpectedly succeeded", value)
		}
	}
}

func TestValidateImageRejectsExcessivePixelDimensions(t *testing.T) {
	header := make([]byte, 8+4+4+13+4)
	copy(header, []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a})
	binary.BigEndian.PutUint32(header[8:12], 13)
	copy(header[12:16], "IHDR")
	binary.BigEndian.PutUint32(header[16:20], 7000)
	binary.BigEndian.PutUint32(header[20:24], 6000)
	header[24], header[25], header[26] = 8, 6, 0
	binary.BigEndian.PutUint32(header[29:33], crc32.ChecksumIEEE(header[12:29]))
	if _, err := ValidateImage(header); err == nil || !strings.Contains(err.Error(), "pixels") {
		t.Fatalf("oversized image err=%v", err)
	}
}
