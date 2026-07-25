// Package imageinput validates and decodes image payloads accepted by the
// image-task boundary. It deliberately has no HTTP or EventHub dependency.
package imageinput

import (
	"encoding/base64"
	"fmt"
	"strings"
)

const maxDecodedImageBytes = 20 << 20

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
