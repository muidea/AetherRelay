// Package imageinput validates and decodes image payloads accepted by the
// image-task boundary. It deliberately has no HTTP or EventHub dependency.
package imageinput

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"strings"

	"github.com/gabriel-vasile/mimetype"
)

const (
	maxDecodedImageBytes = 20 << 20

	// MaxChatImageCount and MaxChatImageBytes bound an image-bearing chat
	// turn before it can consume upload capacity on a ChatGPT Web account.
	// They are intentionally fixed protocol limits rather than user-facing
	// runtime knobs.
	MaxChatImageCount = 4
	MaxChatImageBytes = 20 << 20
	// MaxChatImagePixels bounds decoded image dimensions before an attachment
	// reaches the ChatGPT Web upload chain. It prevents tiny compressed image
	// payloads from requesting unbounded raster allocation downstream.
	MaxChatImagePixels = 40_000_000
)

// Image is a validated image payload suitable for a ChatGPT Web attachment.
// MIMEType is detected from the decoded bytes; callers must not trust the
// media type claimed by a user-provided data URI or multipart upload.
type Image struct {
	Bytes    []byte
	MIMEType string
}

// DecodeBase64Images accepts standard base64 or a base64 data URI. It rejects
// empty and oversized values before an image task consumes account capacity.
func DecodeBase64Images(values []string) ([][]byte, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("at least one image is required")
	}
	images := make([][]byte, 0, len(values))
	for index, value := range values {
		encoded, err := base64Part(value)
		if err != nil {
			return nil, fmt.Errorf("image %d: %w", index+1, err)
		}
		image, err := decode(encoded)
		if err != nil {
			return nil, fmt.Errorf("image %d: invalid base64", index+1)
		}
		if len(image) == 0 {
			return nil, fmt.Errorf("image %d: empty image", index+1)
		}
		if len(image) > maxDecodedImageBytes {
			return nil, fmt.Errorf("image %d: image exceeds %d MiB", index+1, maxDecodedImageBytes>>20)
		}
		images = append(images, image)
	}
	return images, nil
}

// DecodeDataURLImage accepts the OpenAI Chat Completions image_url subset
// supported by the ChatGPT Web adapter: a base64-encoded image data URI. It
// intentionally does not fetch remote URLs, avoiding an SSRF surface on the
// proxy. The declared MIME type and decoded bytes must both be supported.
func DecodeDataURLImage(value string) (Image, error) {
	value = strings.TrimSpace(value)
	header, encoded, ok := strings.Cut(value, ",")
	if !ok || !strings.HasPrefix(strings.ToLower(header), "data:") {
		return Image{}, fmt.Errorf("image_url.url must be a base64 image data URI")
	}
	meta := strings.TrimSpace(header[len("data:"):])
	parts := strings.Split(meta, ";")
	if len(parts) < 2 || !strings.EqualFold(strings.TrimSpace(parts[0]), "image/png") && !strings.EqualFold(strings.TrimSpace(parts[0]), "image/jpeg") && !strings.EqualFold(strings.TrimSpace(parts[0]), "image/gif") && !strings.EqualFold(strings.TrimSpace(parts[0]), "image/webp") {
		return Image{}, fmt.Errorf("image_url.url has unsupported image MIME type")
	}
	base64Encoded := false
	for _, part := range parts[1:] {
		if strings.EqualFold(strings.TrimSpace(part), "base64") {
			base64Encoded = true
			break
		}
	}
	if !base64Encoded {
		return Image{}, fmt.Errorf("image_url.url must be base64 encoded")
	}
	decoded, err := decode(encoded)
	if err != nil {
		return Image{}, fmt.Errorf("image_url.url has invalid base64")
	}
	return ValidateImage(decoded)
}

// ValidateImage verifies a raw uploaded image and returns the detected MIME
// type. It is used for multipart Admin uploads as well as decoded data URIs.
func ValidateImage(value []byte) (Image, error) {
	if len(value) == 0 {
		return Image{}, fmt.Errorf("empty image")
	}
	if len(value) > maxDecodedImageBytes {
		return Image{}, fmt.Errorf("image exceeds %d MiB", maxDecodedImageBytes>>20)
	}
	mimeType := strings.ToLower(mimetype.Detect(value).String())
	switch mimeType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		width, height, err := imageDimensions(value, mimeType)
		if err != nil || width <= 0 || height <= 0 {
			return Image{}, fmt.Errorf("invalid %s image", mimeType)
		}
		if width > MaxChatImagePixels/height {
			return Image{}, fmt.Errorf("image dimensions exceed %d pixels", MaxChatImagePixels)
		}
		return Image{Bytes: value, MIMEType: mimeType}, nil
	default:
		return Image{}, fmt.Errorf("unsupported image MIME type %q", mimeType)
	}
}

func imageDimensions(value []byte, mimeType string) (int, int, error) {
	if mimeType != "image/webp" {
		config, _, err := image.DecodeConfig(bytes.NewReader(value))
		return config.Width, config.Height, err
	}
	return webpDimensions(value)
}

// webpDimensions validates enough of the WebP container to obtain its canvas
// dimensions without importing an upstream-owner internal package.
func webpDimensions(value []byte) (int, int, error) {
	if len(value) < 30 || string(value[:4]) != "RIFF" || string(value[8:12]) != "WEBP" {
		return 0, 0, fmt.Errorf("invalid WebP header")
	}
	switch string(value[12:16]) {
	case "VP8X":
		return 1 + int(value[24]) + int(value[25])<<8 + int(value[26])<<16,
			1 + int(value[27]) + int(value[28])<<8 + int(value[29])<<16, nil
	case "VP8 ":
		if value[23] != 0x9d || value[24] != 0x01 || value[25] != 0x2a {
			return 0, 0, fmt.Errorf("invalid VP8 frame")
		}
		return int(binary.LittleEndian.Uint16(value[26:28]) & 0x3fff), int(binary.LittleEndian.Uint16(value[28:30]) & 0x3fff), nil
	case "VP8L":
		if len(value) < 25 || value[20] != 0x2f {
			return 0, 0, fmt.Errorf("invalid VP8L frame")
		}
		bits := binary.LittleEndian.Uint32(value[21:25])
		return 1 + int(bits&0x3fff), 1 + int((bits>>14)&0x3fff), nil
	default:
		return 0, 0, fmt.Errorf("unsupported WebP chunk")
	}
}

func base64Part(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("empty image")
	}
	if !strings.HasPrefix(strings.ToLower(value), "data:") {
		return value, nil
	}
	header, encoded, ok := strings.Cut(value, ",")
	if !ok || !strings.Contains(strings.ToLower(header), ";base64") {
		return "", fmt.Errorf("data URI must be base64 encoded")
	}
	return encoded, nil
}

func decode(value string) ([]byte, error) {
	if result, err := base64.StdEncoding.DecodeString(value); err == nil {
		return result, nil
	}
	return base64.RawStdEncoding.DecodeString(value)
}
