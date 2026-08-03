package client

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/url"
	"strings"

	"ai-proxy/internal/pkg/chatattachment"
	http "github.com/bogdanfinn/fhttp"
	"github.com/gabriel-vasile/mimetype"
)

const maxUploadImageBytes = 20 << 20

// UploadImage runs the Web file-service protocol and returns an image
// reference suitable for ImageRequest.References. The upload URL is supplied
// by the upstream response and is never exposed beyond this client.
func (c *Client) UploadImage(data []byte, fileName string) (ImageReference, error) {
	metadata, err := inspectUploadImage(data, fileName)
	if err != nil {
		return ImageReference{}, err
	}
	createPayload, err := json.Marshal(struct {
		FileName string `json:"file_name"`
		FileSize int64  `json:"file_size"`
		UseCase  string `json:"use_case"`
		Width    int    `json:"width"`
		Height   int    `json:"height"`
	}{metadata.FileName, metadata.FileSize, "multimodal", metadata.Width, metadata.Height})
	if err != nil {
		return ImageReference{}, fmt.Errorf("encode image upload create: %w", err)
	}
	createBody, err := c.postJSON("/backend-api/files", string(createPayload), "image_upload_create")
	if err != nil {
		return ImageReference{}, err
	}
	var created struct {
		FileID    string `json:"file_id"`
		UploadURL string `json:"upload_url"`
	}
	if err := json.Unmarshal(createBody, &created); err != nil {
		return ImageReference{}, fmt.Errorf("decode image upload create: %w", err)
	}
	if strings.TrimSpace(created.FileID) == "" || !isHTTPURL(created.UploadURL) {
		return ImageReference{}, fmt.Errorf("image upload create: missing file_id or upload_url")
	}
	if err := c.putUpload(created.UploadURL, data, metadata.MIMEType); err != nil {
		return ImageReference{}, err
	}
	if _, err := c.postJSON("/backend-api/files/"+created.FileID+"/uploaded", "{}", "image_upload_confirm"); err != nil {
		return ImageReference{}, err
	}
	metadata.FileID = created.FileID
	return metadata, nil
}

func (c *Client) UploadAttachment(file chatattachment.File) (ImageReference, error) {
	validated, err := chatattachment.Validate(file.Bytes, file.Name, file.ContentType)
	if err != nil {
		return ImageReference{}, fmt.Errorf("file upload: %w", err)
	}
	createPayload, err := json.Marshal(map[string]any{
		"file_name": validated.Name,
		"file_size": len(validated.Bytes),
		"use_case":  "multimodal",
	})
	if err != nil {
		return ImageReference{}, fmt.Errorf("encode file upload create: %w", err)
	}
	createBody, err := c.postJSON("/backend-api/files", string(createPayload), "file_upload_create")
	if err != nil {
		return ImageReference{}, err
	}
	var created struct {
		FileID    string `json:"file_id"`
		UploadURL string `json:"upload_url"`
	}
	if err := json.Unmarshal(createBody, &created); err != nil {
		return ImageReference{}, fmt.Errorf("decode file upload create: %w", err)
	}
	if strings.TrimSpace(created.FileID) == "" || !isHTTPURL(created.UploadURL) {
		return ImageReference{}, fmt.Errorf("file upload create: missing file_id or upload_url")
	}
	if err := c.putUpload(created.UploadURL, validated.Bytes, validated.ContentType); err != nil {
		return ImageReference{}, err
	}
	if _, err := c.postJSON("/backend-api/files/"+created.FileID+"/uploaded", "{}", "file_upload_confirm"); err != nil {
		return ImageReference{}, err
	}
	return ImageReference{FileID: created.FileID, FileName: validated.Name, FileSize: int64(len(validated.Bytes)), MIMEType: validated.ContentType}, nil
}

func (c *Client) putUpload(uploadURL string, data []byte, mimeType string) error {
	req, err := http.NewRequest(http.MethodPut, uploadURL, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("image upload PUT request: %w", err)
	}
	req.Header.Set("content-type", mimeType)
	req.Header.Set("x-ms-blob-type", "BlockBlob")
	req.Header.Set("x-ms-version", "2020-04-08")
	req.Header.Set("origin", c.baseURL)
	req.Header.Set("referer", c.baseURL+"/")
	req.Header.Set("user-agent", c.userAgent)
	req.Header.Set("accept", "application/json, text/plain, */*")
	req.Header.Set("accept-language", "en-US,en;q=0.9")
	response, err := c.doer.Do(req)
	if err != nil {
		return classifyTransport("image_upload_put", err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20)); err != nil {
		return classifyTransport("image_upload_put", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return classifyStatus("image_upload_put", response.StatusCode)
	}
	return nil
}

func inspectUploadImage(data []byte, fileName string) (ImageReference, error) {
	if len(data) == 0 {
		return ImageReference{}, fmt.Errorf("image upload: empty image")
	}
	if len(data) > maxUploadImageBytes {
		return ImageReference{}, fmt.Errorf("image upload: image exceeds %d MiB", maxUploadImageBytes>>20)
	}
	mimeType := mimetype.Detect(data)
	mime := strings.ToLower(mimeType.String())
	if mime != "image/png" && mime != "image/jpeg" && mime != "image/gif" && mime != "image/webp" {
		return ImageReference{}, fmt.Errorf("image upload: unsupported image MIME type %q", mime)
	}
	width, height, err := imageDimensions(data, mime)
	if err != nil || width <= 0 || height <= 0 {
		return ImageReference{}, fmt.Errorf("image upload: invalid %s image", mime)
	}
	fileName = strings.TrimSpace(fileName)
	if fileName == "" {
		fileName = "image" + mimeType.Extension()
	}
	return ImageReference{FileName: fileName, FileSize: int64(len(data)), MIMEType: mime, Width: width, Height: height}, nil
}

func imageDimensions(data []byte, mimeType string) (int, int, error) {
	if mimeType != "image/webp" {
		config, _, err := image.DecodeConfig(bytes.NewReader(data))
		return config.Width, config.Height, err
	}
	return webpDimensions(data)
}

func webpDimensions(data []byte) (int, int, error) {
	if len(data) < 30 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return 0, 0, fmt.Errorf("invalid WebP header")
	}
	chunk := string(data[12:16])
	switch chunk {
	case "VP8X":
		if len(data) < 30 {
			return 0, 0, fmt.Errorf("truncated VP8X")
		}
		return 1 + int(data[24]) + int(data[25])<<8 + int(data[26])<<16, 1 + int(data[27]) + int(data[28])<<8 + int(data[29])<<16, nil
	case "VP8 ":
		if len(data) < 30 || data[23] != 0x9d || data[24] != 0x01 || data[25] != 0x2a {
			return 0, 0, fmt.Errorf("invalid VP8 frame")
		}
		return int(binary.LittleEndian.Uint16(data[26:28]) & 0x3fff), int(binary.LittleEndian.Uint16(data[28:30]) & 0x3fff), nil
	case "VP8L":
		if len(data) < 25 || data[20] != 0x2f {
			return 0, 0, fmt.Errorf("invalid VP8L frame")
		}
		bits := binary.LittleEndian.Uint32(data[21:25])
		return 1 + int(bits&0x3fff), 1 + int((bits>>14)&0x3fff), nil
	default:
		return 0, 0, fmt.Errorf("unsupported WebP chunk %q", chunk)
	}
}

func isHTTPURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}
