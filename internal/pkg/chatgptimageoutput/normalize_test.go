package chatgptimageoutput

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
)

func TestParseSize(t *testing.T) {
	if _, requested, err := ParseSize("auto"); err != nil || requested {
		t.Fatalf("auto = requested=%v err=%v", requested, err)
	}
	for _, value := range []string{"1024x1024", "1536×1024", " 32 x 16 "} {
		got, requested, err := ParseSize(value)
		if err != nil || !requested || got.Width <= 0 || got.Height <= 0 {
			t.Fatalf("size %q = %+v requested=%v err=%v", value, got, requested, err)
		}
	}
	if _, _, err := ParseSize("wide"); err == nil {
		t.Fatal("invalid size accepted")
	}
	if _, _, err := ParseSize("svg"); !errors.Is(err, ErrSVGUnsupported) {
		t.Fatalf("svg err=%v", err)
	}
}

func TestNormalizeProducesExactPNGDimensions(t *testing.T) {
	var source bytes.Buffer
	input := image.NewRGBA(image.Rect(0, 0, 4, 2))
	input.Set(0, 0, color.RGBA{R: 255, A: 255})
	if err := png.Encode(&source, input); err != nil {
		t.Fatal(err)
	}
	output, dims, err := Normalize(source.Bytes(), "3x5")
	if err != nil {
		t.Fatal(err)
	}
	if dims != (Dimensions{Width: 3, Height: 5}) {
		t.Fatalf("dims=%+v", dims)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(output))
	if err != nil || format != "png" || config.Width != 3 || config.Height != 5 {
		t.Fatalf("config=%+v format=%q err=%v", config, format, err)
	}
}

func TestNormalizeAutoReportsRasterDimensionsAndFormat(t *testing.T) {
	var source bytes.Buffer
	input := image.NewRGBA(image.Rect(0, 0, 7, 3))
	if err := png.Encode(&source, input); err != nil {
		t.Fatal(err)
	}
	output, dims, err := Normalize(source.Bytes(), "auto")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output, source.Bytes()) || dims != (Dimensions{Width: 7, Height: 3}) {
		t.Fatalf("auto output dims=%+v bytes_equal=%v", dims, bytes.Equal(output, source.Bytes()))
	}
	info, err := DecodeRasterInfo(output)
	if err != nil || info.Format != "png" || info.Width != 7 || info.Height != 3 {
		t.Fatalf("info=%+v err=%v", info, err)
	}
}

func TestNormalizeFailsClosedForSVGAndUndecodableBytes(t *testing.T) {
	if _, _, err := Normalize([]byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), "64x64"); !errors.Is(err, ErrSVGUnsupported) {
		t.Fatalf("svg err=%v", err)
	}
	if _, _, err := Normalize([]byte("not an image"), "64x64"); err == nil || !strings.Contains(err.Error(), "decode raster") {
		t.Fatalf("invalid raster err=%v", err)
	}
}

func TestValidateResponseFormatExplainsSVGBoundary(t *testing.T) {
	if err := ValidateResponseFormat("svg"); !errors.Is(err, ErrSVGUnsupported) {
		t.Fatalf("svg format err=%v", err)
	}
	if err := ValidateResponseFormat("url"); err != nil {
		t.Fatalf("url err=%v", err)
	}
}

func TestHasSVGVectorIntentIsConservative(t *testing.T) {
	for _, prompt := range []string{"导出 SVG 格式文件", "return a vector graphic as SVG", "生成矢量图文件"} {
		if !HasSVGVectorIntent(prompt) {
			t.Fatalf("prompt was not recognized: %q", prompt)
		}
	}
	if HasSVGVectorIntent("a colorful vector-like raster illustration") {
		t.Fatal("artistic vector wording was rejected")
	}
}

func TestValidateRequestRejectsExplicitSVGIntent(t *testing.T) {
	for _, prompt := range []string{"生成 SVG", "create a vector graphic", "导出矢量图文件"} {
		if err := ValidateRequest(prompt, "auto", ""); !errors.Is(err, ErrSVGUnsupported) {
			t.Fatalf("prompt %q err=%v", prompt, err)
		}
	}
}
