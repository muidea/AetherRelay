package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tiktoken-go/tokenizer"
)

const (
	responsesInputItemTokenOverhead = 3
	responsesContentPartOverhead    = 1
)

type responsesInputTokensRequest struct {
	Model        string            `json:"model"`
	Instructions string            `json:"instructions,omitempty"`
	Input        json.RawMessage   `json:"input,omitempty"`
	Tools        []json.RawMessage `json:"tools,omitempty"`
	ToolChoice   json.RawMessage   `json:"tool_choice,omitempty"`
}

type responsesInputTokensResponse struct {
	Object      string `json:"object"`
	InputTokens int    `json:"input_tokens"`
}

type responsesInputTokenItem struct {
	Type      string          `json:"type,omitempty"`
	Role      string          `json:"role,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	ID        string          `json:"id,omitempty"`
}

type responsesInputTokenContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

// handleResponsesInputTokens validates the same model/access contract as
// POST /v1/responses, then estimates locally. It never acquires an account,
// reads provider credentials, or sends an upstream request.
func (h *Handler) handleResponsesInputTokens(w http.ResponseWriter, r *http.Request, requestID string) {
	started := time.Now()
	round := archiveRoundFromContext(r.Context())
	raw, err := h.readLimitedBody(w, r)
	if err != nil {
		status, code := http.StatusBadRequest, ErrorCodeInvalidRequest
		if isRequestTooLarge(err) {
			status, code = http.StatusRequestEntityTooLarge, ErrorCodeRequestTooLarge
		}
		h.writeArchivedAPIError(w, round, r, started, "", "", false, status, APIError{Code: code, Message: err.Error()})
		return
	}
	defer r.Body.Close()
	if err := h.writeArchiveRequest(round, raw); err != nil {
		h.writeArchivedAPIError(w, round, r, started, "", "", false, http.StatusInternalServerError, APIError{Code: ErrorCodeProxyInternalError, Message: "archive request failed"})
		return
	}
	h.archiveAndLogClientRequest(round, r, len(raw))

	var request responsesInputTokensRequest
	if len(bytes.TrimSpace(raw)) == 0 || json.Unmarshal(raw, &request) != nil {
		h.writeArchivedAPIError(w, round, r, started, "", "", false, http.StatusBadRequest, APIError{Code: ErrorCodeInvalidRequest, Message: "invalid JSON request body"})
		return
	}
	request.Model = strings.TrimSpace(request.Model)
	routeRequest := r.Clone(r.Context())
	routeURL := *r.URL
	routeURL.Path = "/v1/responses"
	routeRequest.URL = &routeURL
	if _, apiErr := h.resolveTransportPlans(routeRequest, request.Model); apiErr != nil {
		h.writeArchivedAPIError(w, round, r, started, "", request.Model, false, statusForAPIError(apiErr), *apiErr)
		return
	}

	count, err := estimateResponsesInputTokens(request)
	if err != nil {
		h.writeArchivedAPIError(w, round, r, started, "", request.Model, false, http.StatusBadRequest, APIError{Code: ErrorCodeInvalidRequest, Message: "cannot estimate input tokens: " + err.Error()})
		return
	}
	payload, err := json.Marshal(responsesInputTokensResponse{Object: "response.input_tokens", InputTokens: count})
	if err != nil {
		h.writeArchivedAPIError(w, round, r, started, "", request.Model, false, http.StatusInternalServerError, APIError{Code: ErrorCodeProxyInternalError, Message: "encode input token response"})
		return
	}
	payload = append(payload, '\n')
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
	_ = h.writeArchiveResponse(round, "response.json", payload)
	duration := time.Since(started)
	h.recordAndPrint(round, r, "", request.Model, false, http.StatusOK, duration, tokenUsage{}, "")
	h.writeArchiveMetadata(round, "", request.Model, false, http.StatusOK, duration, tokenUsage{}, "response.json", "", "", "success")
	_ = requestID
}

func estimateResponsesInputTokens(request responsesInputTokensRequest) (int, error) {
	codec, err := responsesInputTokensCodec(request.Model)
	if err != nil {
		return 0, err
	}
	total := 0
	add := func(value string) error {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil
		}
		count, err := codec.Count(value)
		if err != nil {
			return err
		}
		total += count
		return nil
	}
	if err := add(request.Instructions); err != nil {
		return 0, err
	}
	inputCount, err := estimateResponsesInputValue(codec, request.Input)
	if err != nil {
		return 0, err
	}
	total += inputCount
	for _, tool := range request.Tools {
		compacted, err := compactResponsesInputTokensJSON(tool)
		if err != nil {
			return 0, err
		}
		if err := add(compacted); err != nil {
			return 0, err
		}
	}
	if len(bytes.TrimSpace(request.ToolChoice)) > 0 {
		compacted, err := compactResponsesInputTokensJSON(request.ToolChoice)
		if err != nil {
			return 0, err
		}
		if err := add(compacted); err != nil {
			return 0, err
		}
	}
	if total < 1 {
		return 1, nil
	}
	return total, nil
}

func estimateResponsesInputValue(codec tokenizer.Codec, raw json.RawMessage) (int, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return 0, nil
	}
	var plain string
	if err := json.Unmarshal(raw, &plain); err == nil {
		return codec.Count(plain)
	}
	var items []responsesInputTokenItem
	if err := json.Unmarshal(raw, &items); err == nil {
		return estimateResponsesInputItems(codec, items)
	}
	compacted, err := compactResponsesInputTokensJSON(raw)
	if err != nil {
		return 0, err
	}
	return codec.Count(compacted)
}

func estimateResponsesInputItems(codec tokenizer.Codec, items []responsesInputTokenItem) (int, error) {
	total := 0
	add := func(value string) error {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil
		}
		count, err := codec.Count(value)
		if err != nil {
			return err
		}
		total += count
		return nil
	}
	for _, item := range items {
		total += responsesInputItemTokenOverhead
		for _, value := range []string{item.Role, item.Name, item.Arguments, item.CallID, item.ID} {
			if err := add(value); err != nil {
				return 0, err
			}
		}
		if item.Type != "" && item.Type != "message" {
			if err := add(item.Type); err != nil {
				return 0, err
			}
		}
		if len(bytes.TrimSpace(item.Output)) > 0 {
			var output string
			if err := json.Unmarshal(item.Output, &output); err != nil {
				output, err = compactResponsesInputTokensJSON(item.Output)
				if err != nil {
					return 0, err
				}
			}
			if err := add(output); err != nil {
				return 0, err
			}
		}
		if len(bytes.TrimSpace(item.Content)) == 0 {
			continue
		}
		var content string
		if err := json.Unmarshal(item.Content, &content); err == nil {
			if err := add(content); err != nil {
				return 0, err
			}
			continue
		}
		var parts []responsesInputTokenContentPart
		if err := json.Unmarshal(item.Content, &parts); err == nil {
			for _, part := range parts {
				total += responsesContentPartOverhead
				switch part.Type {
				case "input_text", "output_text", "text":
					err = add(part.Text)
				case "input_image":
					err = add(responsesInputImageText(part.ImageURL))
				default:
					err = add(part.Type)
				}
				if err != nil {
					return 0, err
				}
			}
			continue
		}
		compacted, err := compactResponsesInputTokensJSON(item.Content)
		if err != nil {
			return 0, err
		}
		if err := add(compacted); err != nil {
			return 0, err
		}
	}
	return total, nil
}

func responsesInputImageText(imageURL string) string {
	trimmed := strings.TrimSpace(imageURL)
	if strings.HasPrefix(strings.ToLower(trimmed), "data:") {
		if comma := strings.IndexByte(trimmed, ','); comma > 0 {
			return trimmed[:comma]
		}
	}
	return trimmed
}

func compactResponsesInputTokensJSON(raw json.RawMessage) (string, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", nil
	}
	var compacted bytes.Buffer
	if err := json.Compact(&compacted, raw); err != nil {
		return "", err
	}
	return compacted.String(), nil
}

func responsesInputTokensCodec(model string) (tokenizer.Codec, error) {
	encoding := tokenizer.O200kBase
	normalized := strings.ToLower(strings.TrimSpace(model))
	if strings.HasPrefix(normalized, "gpt-3.5") ||
		(strings.HasPrefix(normalized, "gpt-4") && !strings.HasPrefix(normalized, "gpt-4o") && !strings.HasPrefix(normalized, "gpt-4.1")) ||
		strings.HasPrefix(normalized, "text-embedding-") {
		encoding = tokenizer.Cl100kBase
	}
	codec, err := tokenizer.Get(encoding)
	if err != nil {
		return nil, fmt.Errorf("load %s tokenizer: %w", encoding, err)
	}
	return codec, nil
}
