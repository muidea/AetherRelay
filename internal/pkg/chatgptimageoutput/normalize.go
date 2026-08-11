// Package chatgptimageoutput contains the small, owner-neutral transforms
// applied to raster bytes returned by the ChatGPT Web image capability.
//
// ChatGPT Web's conversation protocol does not expose the OpenAI image
// endpoint's size or response-format fields.  Keeping the normalization here
// makes that boundary explicit: a requested WxH size is enforced locally when
// bytes are available, while SVG is never presented as a vector capability.
package chatgptimageoutput

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"strconv"
	"strings"
)

const (
	// MaxOutputPixels keeps a caller-controlled resize from allocating an
	// unbounded image.  It matches the input image safety limit used by the
	// gateway.
	MaxOutputPixels = 40_000_000
	MaxOutputSide   = 8192
)

// Dimensions is an exact raster width/height pair.
type Dimensions struct {
	Width  int
	Height int
}

// RasterInfo describes a decodable raster payload. Format is the name used by
// Go's image registry (for example "png" or "jpeg").
type RasterInfo struct {
	Dimensions
	Format string
}

// ErrSVGUnsupported is returned when a caller asks the ChatGPT Web image
// capability for an SVG/vector result.  The upstream returns raster image
// bytes; wrapping those bytes in an SVG document would still be a bitmap, not
// a vector drawing.
var ErrSVGUnsupported = errors.New("SVG vector output is not supported by ChatGPT Web; it returns raster images only")

// ParseSize accepts the OpenAI-style WxH value (ASCII x or Unicode ×) and
// the special auto value.  An empty or auto value means that no deterministic
// post-processing is requested.
func ParseSize(value string) (Dimensions, bool, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "auto") {
		return Dimensions{}, false, nil
	}
	if strings.EqualFold(value, "svg") || strings.EqualFold(value, "image/svg+xml") {
		return Dimensions{}, false, ErrSVGUnsupported
	}
	normalized := strings.ReplaceAll(value, "×", "x")
	parts := strings.Split(strings.ToLower(normalized), "x")
	if len(parts) != 2 {
		return Dimensions{}, false, fmt.Errorf("image size %q must be auto or WIDTHxHEIGHT", value)
	}
	width, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return Dimensions{}, false, fmt.Errorf("image size %q has invalid width", value)
	}
	height, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return Dimensions{}, false, fmt.Errorf("image size %q has invalid height", value)
	}
	if width <= 0 || height <= 0 {
		return Dimensions{}, false, fmt.Errorf("image size %q must be positive", value)
	}
	if width > MaxOutputSide || height > MaxOutputSide || width > MaxOutputPixels/height {
		return Dimensions{}, false, fmt.Errorf("image size %q exceeds the %d-pixel output limit", value, MaxOutputPixels)
	}
	return Dimensions{Width: width, Height: height}, true, nil
}

// ValidateResponseFormat validates the formats exposed by the OpenAI image
// contract.  SVG is deliberately called out so callers get an actionable
// explanation rather than a generic upstream failure.
func ValidateResponseFormat(value string) error {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "", "b64_json", "url":
		return nil
	case "svg", "image/svg+xml":
		return ErrSVGUnsupported
	default:
		return fmt.Errorf("response_format must be b64_json or url; SVG vector output is not supported by ChatGPT Web")
	}
}

// ValidateRequest checks the portion of the OpenAI image request that the
// ChatGPT Web implementation can enforce locally.  ChatGPT Web has no native
// SVG/vector contract; callers should reject that intent before creating an
// upstream conversation.
func ValidateRequest(prompt, size, responseFormat string) error {
	if strings.TrimSpace(prompt) == "" {
		return fmt.Errorf("prompt is required")
	}
	if HasSVGVectorIntent(prompt) {
		return ErrSVGUnsupported
	}
	if _, _, err := ParseSize(size); err != nil {
		return err
	}
	return ValidateResponseFormat(responseFormat)
}

// HasSVGVectorIntent recognizes an explicit SVG request and wording that asks
// for a vector file/graphic.  A prompt that merely asks for vector-like shapes
// can still produce a raster illustration and is not rejected.
func HasSVGVectorIntent(prompt string) bool {
	lower := strings.ToLower(strings.TrimSpace(prompt))
	if lower == "" {
		return false
	}
	// An explicit SVG token is unambiguous: even if the upstream wraps a
	// bitmap in an SVG container, the result would not be a true vector image.
	if strings.Contains(lower, "svg") {
		return true
	}
	if strings.Contains(lower, "矢量") || strings.Contains(lower, "vector") {
		// “vector-like raster illustration” and equivalent style wording asks
		// for a raster illustration with a visual style, so it remains valid.
		if strings.Contains(lower, "vector-like") || strings.Contains(lower, "矢量风格") || strings.Contains(lower, "矢量样式") {
			return false
		}
		for _, marker := range []string{"format", "file", "output", "export", "graphic", "image", "artwork", "illustration", "logo", "icon", "格式", "文件", "输出", "导出", "图"} {
			if strings.Contains(lower, marker) {
				return true
			}
		}
	}
	return false
}

// Normalize resizes a raster payload to requested WxH dimensions.  It returns
// the original bytes for an empty/auto request.  Explicit dimensions require a
// decodable raster; failing closed is important because otherwise the API
// would claim to have honored a size that it did not actually apply.
func Normalize(payload []byte, requestedSize string) ([]byte, Dimensions, error) {
	target, requested, err := ParseSize(requestedSize)
	if err != nil {
		return nil, Dimensions{}, err
	}
	if len(payload) == 0 {
		if requested {
			return nil, Dimensions{}, fmt.Errorf("cannot normalize image size: image bytes are empty")
		}
		return payload, Dimensions{}, nil
	}
	if looksLikeSVG(payload) {
		return nil, Dimensions{}, ErrSVGUnsupported
	}
	info, err := DecodeRasterInfo(payload)
	if err != nil {
		return nil, Dimensions{}, err
	}
	if !requested {
		return payload, info.Dimensions, nil
	}
	decoded, _, err := image.Decode(bytes.NewReader(payload))
	if err != nil {
		return nil, Dimensions{}, fmt.Errorf("cannot normalize image size: decode raster: %w", err)
	}
	resized := resizeCrop(decoded, target.Width, target.Height)
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, resized); err != nil {
		return nil, Dimensions{}, fmt.Errorf("cannot normalize image size: encode PNG: %w", err)
	}
	return encoded.Bytes(), target, nil
}

// DecodeRasterInfo validates and describes a raster payload without changing
// its bytes.  It deliberately does not register SVG or other vector formats.
func DecodeRasterInfo(payload []byte) (RasterInfo, error) {
	if len(payload) == 0 {
		return RasterInfo{}, fmt.Errorf("image bytes are empty")
	}
	if looksLikeSVG(payload) {
		return RasterInfo{}, ErrSVGUnsupported
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(payload))
	if err != nil {
		return RasterInfo{}, fmt.Errorf("cannot normalize image size: decode raster dimensions: %w", err)
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > MaxOutputSide || config.Height > MaxOutputSide || config.Width > MaxOutputPixels/config.Height {
		return RasterInfo{}, fmt.Errorf("cannot normalize image size: source dimensions are unsafe")
	}
	return RasterInfo{Dimensions: Dimensions{Width: config.Width, Height: config.Height}, Format: strings.ToLower(format)}, nil
}

func looksLikeSVG(payload []byte) bool {
	trimmed := bytes.TrimSpace(bytes.TrimPrefix(payload, []byte{0xef, 0xbb, 0xbf}))
	if len(trimmed) == 0 {
		return false
	}
	lower := strings.ToLower(string(trimmed))
	return strings.HasPrefix(lower, "<svg") ||
		(strings.HasPrefix(lower, "<?xml") && strings.Contains(lower, "<svg"))
}

// resizeCrop first crops the source to the requested aspect ratio and then
// performs deterministic bilinear sampling.  This avoids stretching a
// portrait into a landscape (or vice versa) while guaranteeing exact output
// dimensions.
func resizeCrop(source image.Image, targetWidth, targetHeight int) image.Image {
	bounds := source.Bounds()
	sourceWidth, sourceHeight := bounds.Dx(), bounds.Dy()
	cropWidth, cropHeight := sourceWidth, sourceHeight
	// Compare ratios without floating point overflow.
	if int64(sourceWidth)*int64(targetHeight) > int64(sourceHeight)*int64(targetWidth) {
		cropWidth = max(1, sourceHeight*targetWidth/targetHeight)
	} else if int64(sourceWidth)*int64(targetHeight) < int64(sourceHeight)*int64(targetWidth) {
		cropHeight = max(1, sourceWidth*targetHeight/targetWidth)
	}
	cropMinX := bounds.Min.X + (sourceWidth-cropWidth)/2
	cropMinY := bounds.Min.Y + (sourceHeight-cropHeight)/2
	destination := image.NewRGBA(image.Rect(0, 0, targetWidth, targetHeight))
	for y := 0; y < targetHeight; y++ {
		sourceY := float64(cropMinY) + (float64(y)+0.5)*float64(cropHeight)/float64(targetHeight) - 0.5
		for x := 0; x < targetWidth; x++ {
			sourceX := float64(cropMinX) + (float64(x)+0.5)*float64(cropWidth)/float64(targetWidth) - 0.5
			destination.SetRGBA(x, y, bilinearAt(source, sourceX, sourceY, bounds))
		}
	}
	return destination
}

func bilinearAt(source image.Image, x, y float64, bounds image.Rectangle) color.RGBA {
	if x < float64(bounds.Min.X) {
		x = float64(bounds.Min.X)
	}
	if y < float64(bounds.Min.Y) {
		y = float64(bounds.Min.Y)
	}
	maxX, maxY := bounds.Max.X-1, bounds.Max.Y-1
	if x > float64(maxX) {
		x = float64(maxX)
	}
	if y > float64(maxY) {
		y = float64(maxY)
	}
	x0, y0 := int(x), int(y)
	x1, y1 := x0+1, y0+1
	if x1 > maxX {
		x1 = maxX
	}
	if y1 > maxY {
		y1 = maxY
	}
	fx, fy := x-float64(x0), y-float64(y0)
	r00, g00, b00, a00 := source.At(x0, y0).RGBA()
	r10, g10, b10, a10 := source.At(x1, y0).RGBA()
	r01, g01, b01, a01 := source.At(x0, y1).RGBA()
	r11, g11, b11, a11 := source.At(x1, y1).RGBA()
	return color.RGBA{
		R: uint8(interpolate(interpolate(r00, r10, fx), interpolate(r01, r11, fx), fy) >> 8),
		G: uint8(interpolate(interpolate(g00, g10, fx), interpolate(g01, g11, fx), fy) >> 8),
		B: uint8(interpolate(interpolate(b00, b10, fx), interpolate(b01, b11, fx), fy) >> 8),
		A: uint8(interpolate(interpolate(a00, a10, fx), interpolate(a01, a11, fx), fy) >> 8),
	}
}

func interpolate(left, right uint32, fraction float64) uint32 {
	return uint32(float64(left)*(1-fraction) + float64(right)*fraction + 0.5)
}

func max(left, right int) int {
	if left > right {
		return left
	}
	return right
}
