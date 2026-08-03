package chatattachment

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	MaxFileCount = 4
	MaxFileBytes = 20 << 20
)

type File struct {
	Name        string
	ContentType string
	Bytes       []byte
}

func Validate(data []byte, fileName, declaredType string) (File, error) {
	if len(data) == 0 {
		return File{}, fmt.Errorf("empty attachment")
	}
	if len(data) > MaxFileBytes {
		return File{}, fmt.Errorf("attachment exceeds %d MiB", MaxFileBytes>>20)
	}
	name := safeName(fileName)
	ext := strings.ToLower(filepath.Ext(name))
	declared := strings.ToLower(strings.TrimSpace(strings.Split(declaredType, ";")[0]))
	switch ext {
	case ".pdf":
		if !bytes.HasPrefix(data, []byte("%PDF-")) {
			return File{}, fmt.Errorf("invalid PDF attachment")
		}
		return File{Name: name, ContentType: "application/pdf", Bytes: append([]byte(nil), data...)}, nil
	case ".txt", ".md", ".markdown", ".csv":
		if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
			return File{}, fmt.Errorf("attachment must be valid UTF-8 text")
		}
		contentType := map[string]string{".txt": "text/plain", ".md": "text/markdown", ".markdown": "text/markdown", ".csv": "text/csv"}[ext]
		if declared != "" && declared != "application/octet-stream" && !strings.HasPrefix(declared, "text/") && declared != "application/csv" && !(ext == ".csv" && declared == "application/vnd.ms-excel") {
			return File{}, fmt.Errorf("attachment content type %q does not match %s", declared, ext)
		}
		return File{Name: name, ContentType: contentType, Bytes: append([]byte(nil), data...)}, nil
	default:
		return File{}, fmt.Errorf("only PDF, TXT, Markdown, and CSV attachments are supported")
	}
}

func safeName(value string) string {
	value = strings.TrimSpace(filepath.Base(strings.ReplaceAll(value, "\\", "/")))
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value)
	if value == "" || value == "." {
		return "attachment.txt"
	}
	if len(value) > 255 {
		ext := filepath.Ext(value)
		value = strings.TrimSpace(value[:max(1, 255-len(ext))]) + ext
	}
	return value
}
