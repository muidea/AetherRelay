package imageinput

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
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
