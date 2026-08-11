package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
)

const maxDownloadedImageBytes = 40 << 20

var realImageFileID = regexp.MustCompile(`\bfile_00000000[a-f0-9]{24}\b`)

// ImagePollOptions controls the bounded wait for an asynchronous Web image
// task. Zero values select the same conservative cadence as the Python path.
type ImagePollOptions struct {
	Timeout     time.Duration
	InitialWait time.Duration
	Interval    time.Duration
}

// DownloadedImage is a completed image with the upstream URL retained only as
// a value; callers can persist Bytes in imagestore without a transport handle.
type DownloadedImage struct {
	Bytes []byte
	URL   string
}

// PollImageResults fetches conversation documents until at least one image
// file/attachment identifier is visible. It accepts IDs already observed in
// SSE so a delayed document commit does not discard useful information.
func (c *Client) PollImageResults(ctx context.Context, conversationID string, initial ImageStreamResult, options ImagePollOptions) (ImageStreamResult, error) {
	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		conversationID = strings.TrimSpace(initial.ConversationID)
	}
	if conversationID == "" {
		return ImageStreamResult{}, fmt.Errorf("image poll: conversation ID is required")
	}
	options = normalizedPollOptions(options)
	result := initial
	result.ConversationID = conversationID
	deadline := time.Now().Add(options.Timeout)
	if options.InitialWait > 0 {
		if err := waitForPoll(ctx, minDuration(options.InitialWait, time.Until(deadline))); err != nil {
			return ImageStreamResult{}, fmt.Errorf("image poll: %w", err)
		}
	}
	for {
		document, err := c.get("/backend-api/conversation/"+conversationID, "image_poll_conversation")
		if err == nil {
			var decoded map[string]any
			if decodeErr := json.Unmarshal(document, &decoded); decodeErr != nil {
				return ImageStreamResult{}, fmt.Errorf("decode image conversation: %w", decodeErr)
			}
			found := ImageStreamResult{ConversationID: conversationID}
			collectImageConversationValues(decoded, &found)
			mergeImageStreamResult(&result, found)
			if len(result.FileIDs) > 0 || len(result.SedimentIDs) > 0 {
				return result, nil
			}
		} else if !retryableImagePollError(err) {
			return ImageStreamResult{}, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		if err := waitForPoll(ctx, minDuration(options.Interval, remaining)); err != nil {
			return ImageStreamResult{}, fmt.Errorf("image poll: %w", err)
		}
	}
	return ImageStreamResult{}, fmt.Errorf("image poll: timed out waiting for image results (%s)", conversationID)
}

// collectImageConversationValues deliberately scopes document parsing to image
// tool/assistant outputs.  A full conversation also contains the user's image
// edit inputs, which must never be returned as newly generated images.
func collectImageConversationValues(document map[string]any, result *ImageStreamResult) {
	mapping, ok := document["mapping"].(map[string]any)
	if !ok {
		return
	}
	for _, rawNode := range mapping {
		node, ok := rawNode.(map[string]any)
		if !ok {
			continue
		}
		message, ok := node["message"].(map[string]any)
		if !ok {
			continue
		}
		author, _ := message["author"].(map[string]any)
		role, _ := author["role"].(string)
		role = strings.ToLower(strings.TrimSpace(role))
		if role != "tool" && role != "assistant" {
			continue
		}
		content := message["content"]
		metadata := message["metadata"]
		isImageGeneration := mapString(metadata, "async_task_type") == "image_gen"
		if role == "assistant" && !isImageGeneration && !hasImageAssetPointer(content) && !hasImageAssetPointer(metadata) {
			continue
		}
		collectImageReferenceIDs(content, result)
		collectImageReferenceIDs(metadata, result)
	}
}

func mapString(value any, key string) string {
	object, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	text, _ := object[key].(string)
	return text
}

func hasImageAssetPointer(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		if contentType, _ := typed["content_type"].(string); contentType == "image_asset_pointer" {
			return true
		}
		if pointer, _ := typed["asset_pointer"].(string); strings.HasPrefix(pointer, "file-service://") || strings.HasPrefix(pointer, "sediment://") {
			return true
		}
		for _, child := range typed {
			if hasImageAssetPointer(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if hasImageAssetPointer(child) {
				return true
			}
		}
	}
	return false
}

func collectImageReferenceIDs(value any, result *ImageStreamResult) {
	switch typed := value.(type) {
	case string:
		collectImagePointer(typed, result)
		for _, fileID := range realImageFileID.FindAllString(typed, -1) {
			addImageValue(&result.FileIDs, fileID)
		}
	case map[string]any:
		for _, child := range typed {
			collectImageReferenceIDs(child, result)
		}
	case []any:
		for _, child := range typed {
			collectImageReferenceIDs(child, result)
		}
	}
}

// DownloadImageResults resolves each image reference to a short-lived URL and
// downloads it. URL discovery and response parsing remain private to upstream.
func (c *Client) DownloadImageResults(result ImageStreamResult) ([]DownloadedImage, error) {
	// Conversation documents can contain incidental, expired URLs (for example
	// previews attached to a tool message).  The Web client resolves the stable
	// file-service/sediment references first, so do the same here.  Direct URLs
	// remain a fallback for streams that genuinely provide no stable reference.
	urls := make([]string, 0, len(result.FileIDs)+len(result.SedimentIDs))
	for _, fileID := range result.FileIDs {
		if fileID == "file_upload" {
			continue
		}
		url, err := c.fileDownloadURL(fileID)
		if err != nil {
			continue
		}
		addImageValue(&urls, url)
	}
	for _, attachmentID := range result.SedimentIDs {
		url, err := c.attachmentDownloadURL(result.ConversationID, attachmentID)
		if err != nil {
			continue
		}
		addImageValue(&urls, url)
	}
	if len(urls) == 0 {
		for _, url := range result.URLs {
			addImageValue(&urls, url)
		}
	}
	if len(urls) == 0 {
		return nil, fmt.Errorf("image download: no URLs")
	}
	images := make([]DownloadedImage, 0, len(urls))
	var lastErr error
	for _, url := range urls {
		data, err := c.downloadImage(url)
		if err != nil {
			lastErr = err
			continue
		}
		duplicate := false
		for _, image := range images {
			if bytes.Equal(image.Bytes, data) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			images = append(images, DownloadedImage{Bytes: data, URL: url})
		}
	}
	if len(images) == 0 {
		return nil, lastErr
	}
	return images, nil
}

func normalizedPollOptions(options ImagePollOptions) ImagePollOptions {
	if options == (ImagePollOptions{}) {
		return ImagePollOptions{Timeout: 2 * time.Minute, InitialWait: 10 * time.Second, Interval: 10 * time.Second}
	}
	if options.Timeout <= 0 {
		options.Timeout = 2 * time.Minute
	}
	if options.InitialWait < 0 {
		options.InitialWait = 0
	}
	if options.Interval <= 0 {
		options.Interval = 10 * time.Second
	}
	return options
}

// waitForPoll is shared by the image and search document pollers. Callers
// add their operation name so cancellation errors do not claim that a search
// was an image poll (or vice versa).
func waitForPoll(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("image poll: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

func minDuration(left, right time.Duration) time.Duration {
	if left < right {
		return left
	}
	return right
}

func retryableImagePollError(err error) bool {
	upstream, ok := err.(*Error)
	if !ok {
		return false
	}
	if upstream.Class == RateLimit {
		return true
	}
	if upstream.Class != Upstream {
		return false
	}
	return upstream.StatusCode == 404 || upstream.StatusCode == 409 || upstream.StatusCode == 423 || upstream.StatusCode >= 500
}

func mergeImageStreamResult(target *ImageStreamResult, source ImageStreamResult) {
	if target.ConversationID == "" {
		target.ConversationID = source.ConversationID
	}
	for _, value := range source.FileIDs {
		addImageValue(&target.FileIDs, value)
	}
	for _, value := range source.SedimentIDs {
		addImageValue(&target.SedimentIDs, value)
	}
	for _, value := range source.URLs {
		addImageValue(&target.URLs, value)
	}
	if target.RevisedPrompt == "" {
		target.RevisedPrompt = source.RevisedPrompt
	}
}

func (c *Client) fileDownloadURL(fileID string) (string, error) {
	data, err := c.get("/backend-api/files/"+fileID+"/download", "image_file_download_url")
	if err != nil {
		return "", err
	}
	return decodeDownloadURL(data, "image file download")
}

func (c *Client) attachmentDownloadURL(conversationID, attachmentID string) (string, error) {
	if strings.TrimSpace(conversationID) == "" {
		return "", fmt.Errorf("image attachment download: conversation ID is required")
	}
	data, err := c.get("/backend-api/conversation/"+conversationID+"/attachment/"+attachmentID+"/download", "image_attachment_download_url")
	if err != nil {
		return "", err
	}
	return decodeDownloadURL(data, "image attachment download")
}

func decodeDownloadURL(data []byte, operation string) (string, error) {
	var payload struct {
		DownloadURL string `json:"download_url"`
		URL         string `json:"url"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", fmt.Errorf("decode %s: %w", operation, err)
	}
	if payload.DownloadURL != "" {
		payload.URL = payload.DownloadURL
	}
	if !isHTTPURL(payload.URL) {
		return "", fmt.Errorf("%s: missing download URL", operation)
	}
	return payload.URL, nil
}

func (c *Client) downloadImage(downloadURL string) ([]byte, error) {
	if !isHTTPURL(downloadURL) {
		return nil, fmt.Errorf("image download: invalid URL")
	}
	req, err := http.NewRequest(http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, fmt.Errorf("image download request: %w", err)
	}
	// The image task contract is raster-only.  Prefer PNG/JPEG and do not ask
	// the Web origin for SVG/AVIF variants that the in-process decoder cannot
	// verify or resize deterministically.
	req.Header.Set("accept", "image/png,image/jpeg,image/gif,image/*;q=0.8,*/*;q=0.1")
	// The Python reference downloads short-lived image URLs through its browser
	// session.  Preserve that browser identity here as well: some ChatGPT image
	// origins return a deliberately indistinguishable 404 when the session is
	// absent.  The URL is obtained only from an authenticated ChatGPT file or
	// attachment endpoint and is retained solely inside this client.
	req.Header.Set("authorization", "Bearer "+c.accessToken)
	req.Header.Set("origin", c.baseURL)
	req.Header.Set("referer", c.baseURL+"/")
	req.Header.Set("user-agent", c.userAgent)
	req.Header.Set("accept-language", "zh-CN,zh;q=0.9,en;q=0.8,en-US;q=0.7")
	req.Header.Set("cache-control", "no-cache")
	req.Header.Set("pragma", "no-cache")
	req.Header.Set("priority", "u=1, i")
	req.Header.Set("sec-ch-ua", `"Google Chrome";v="133", "Chromium";v="133", "Not_A Brand";v="24"`)
	req.Header.Set("sec-ch-ua-arch", `"x86"`)
	req.Header.Set("sec-ch-ua-bitness", `"64"`)
	req.Header.Set("sec-ch-ua-mobile", "?0")
	req.Header.Set("sec-ch-ua-platform", `"Windows"`)
	req.Header.Set("sec-fetch-dest", "empty")
	req.Header.Set("sec-fetch-mode", "cors")
	req.Header.Set("sec-fetch-site", "same-origin")
	req.Header.Set("oai-device-id", c.deviceID)
	req.Header.Set("oai-session-id", c.sessionID)
	req.Header.Set("oai-language", "zh-CN")
	if c.cookie != "" {
		req.Header.Set("cookie", c.cookie)
	}
	response, err := c.doer.Do(req)
	if err != nil {
		return nil, classifyTransport("image_download", err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxDownloadedImageBytes+1))
	if err != nil {
		return nil, classifyTransport("image_download", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, classifyStatus("image_download", response.StatusCode)
	}
	if len(data) > maxDownloadedImageBytes {
		return nil, fmt.Errorf("image download: image exceeds %d MiB", maxDownloadedImageBytes>>20)
	}
	return data, nil
}
