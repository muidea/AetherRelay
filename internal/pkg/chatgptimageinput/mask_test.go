package imageinput

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func TestCompositeMasksUsesAlphaAndReusesFinalMask(t *testing.T) {
	source := encodePNG(t, image.NewNRGBA(image.Rect(0, 0, 2, 1)))
	base, _, err := image.Decode(bytes.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	base.(*image.NRGBA).SetNRGBA(0, 0, color.NRGBA{R: 10, G: 20, B: 30, A: 255})
	base.(*image.NRGBA).SetNRGBA(1, 0, color.NRGBA{R: 40, G: 50, B: 60, A: 255})
	source = encodePNG(t, base)
	mask := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	mask.SetNRGBA(0, 0, color.NRGBA{A: 0})
	mask.SetNRGBA(1, 0, color.NRGBA{A: 255})

	outputs, err := CompositeMasks([][]byte{source, source}, [][]byte{encodePNG(t, mask)})
	if err != nil {
		t.Fatal(err)
	}
	if len(outputs) != 2 {
		t.Fatalf("outputs=%d", len(outputs))
	}
	decoded, _, err := image.Decode(bytes.NewReader(outputs[0]))
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, leftAlpha := decoded.At(0, 0).RGBA()
	_, _, _, rightAlpha := decoded.At(1, 0).RGBA()
	if leftAlpha != 0 || rightAlpha != 0xffff {
		t.Fatalf("mask alpha=%x,%x", leftAlpha, rightAlpha)
	}
}

func TestCompositeMasksRejectsInvalidPayload(t *testing.T) {
	if _, err := CompositeMasks([][]byte{[]byte("not-image")}, [][]byte{[]byte("not-image")}); err == nil {
		t.Fatal("invalid images unexpectedly succeeded")
	}
}

func encodePNG(t *testing.T, value image.Image) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, value); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
