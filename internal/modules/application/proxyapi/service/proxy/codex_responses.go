package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"ai-proxy/internal/modules/application/proxyapi/pkg/codexresponses"
	"ai-proxy/internal/modules/application/proxyapi/pkg/effectivecatalog"
	archivepkg "ai-proxy/internal/pkg/aiproxyarchive"
)

// handleCodexOAuthResponses relays the native Responses object rather than
// converting it through the ChatGPT Web message-tree adapter. This preserves
// native output items and tools within the documented HTTP Responses P0.
func (h *Handler) handleCodexOAuthResponses(w http.ResponseWriter, r *http.Request, started time.Time, provider, model string, raw []byte, body map[string]any, stream bool) {
	round := archiveRoundFromContext(r.Context())
	executor := h.codexResponses
	if executor == nil {
		h.writeCodexResponsesError(w, r, round, started, provider, model, stream, codexresponses.NewFailure(codexresponses.KindProviderUnavailable, 0, fmt.Errorf("Codex Responses executor is unavailable")))
		return
	}
	request := codexresponses.Request{Model: model, Body: bytes.Clone(raw)}
	if !stream {
		response, err := executor.CompleteCodexResponses(r.Context(), request)
		if err != nil {
			h.writeCodexResponsesError(w, r, round, started, provider, model, false, err)
			return
		}
		copyCodexHeaders(w.Header(), response.Headers)
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(response.Body); err != nil {
			failure := codexresponses.NewFailure(codexresponses.KindClientWrite, 0, err)
			tok := codexResponseUsage(response.Body, body)
			h.recordAndPrintFail(round, r, provider, model, false, http.StatusOK, time.Since(started), tok, streamFailFromCodex(failure))
			h.writeArchiveMetadata(round, provider, model, false, http.StatusOK, time.Since(started), tok, "response.json", failure.Error(), "", outcomeFromStreamFail(streamFailFromCodex(failure), http.StatusOK))
			return
		}
		_ = h.writeArchiveResponse(round, "response.json", response.Body)
		tok := codexResponseUsage(response.Body, body)
		h.recordAndPrint(round, r, provider, model, false, http.StatusOK, time.Since(started), tok, "")
		h.writeArchiveMetadata(round, provider, model, false, http.StatusOK, time.Since(started), tok, "response.json", "", "", "success")
		return
	}

	streamStarted := false
	var archive bytes.Buffer
	accumulator := newResponsesStreamAccumulator(model)
	maxStream, _ := h.streamLimits()
	accumulator.SetMaxContent(maxStream)
	var total int64
	startStream := func(info codexresponses.StreamStart) error {
		if streamStarted {
			return nil
		}
		copyCodexHeaders(w.Header(), info.Headers)
		prepareSSEHeaders(w.Header())
		w.WriteHeader(http.StatusOK)
		streamStarted = true
		return nil
	}
	emit := func(line []byte) error {
		total += int64(len(line))
		if total > maxStream {
			return codexresponses.NewFailure(codexresponses.KindProtocol, 0, fmt.Errorf("stream exceeds limit of %d bytes", maxStream))
		}
		accumulator.TrackSSELine(line)
		archive.Write(line)
		if _, err := w.Write(line); err != nil {
			return err
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		if failure := streamFailFromTerminal(parseTerminalSSELine(streamProtoResponses, line)); failure != nil {
			return codexresponses.NewFailure(codexresponses.KindUpstream, 0, failure)
		}
		return nil
	}
	err := executor.StreamCodexResponses(r.Context(), request, startStream, emit)
	if err != nil {
		if !streamStarted {
			h.writeCodexResponsesError(w, r, round, started, provider, model, true, err)
			return
		}
		failure := streamFailFromCodexError(err)
		tok := accumulator.FinalizeUsage(body)
		_ = h.writeArchiveResponse(round, "response.sse", archive.Bytes())
		h.recordAndPrintFail(round, r, provider, model, true, http.StatusOK, time.Since(started), tok, failure)
		h.writeArchiveMetadata(round, provider, model, true, http.StatusOK, time.Since(started), tok, "response.sse", failure.Error(), "", outcomeFromStreamFail(failure, http.StatusOK))
		return
	}
	if !streamStarted {
		_ = startStream(codexresponses.StreamStart{})
	}
	_ = h.writeArchiveResponse(round, "response.sse", archive.Bytes())
	tok := accumulator.FinalizeUsage(body)
	h.recordAndPrint(round, r, provider, model, true, http.StatusOK, time.Since(started), tok, "")
	h.writeArchiveMetadata(round, provider, model, true, http.StatusOK, time.Since(started), tok, "response.sse", "", "", "success")
}

func (h *Handler) writeCodexResponsesError(w http.ResponseWriter, r *http.Request, round *archivepkg.Round, started time.Time, provider, model string, stream bool, err error) {
	failure := streamFailFromCodexError(err)
	status := http.StatusBadGateway
	code := ErrorCodeUpstreamUnavailable
	switch failure.ErrorCode {
	case string(codexresponses.KindProviderUnavailable):
		status = http.StatusServiceUnavailable
		code = ErrorCodeProviderUnavailable
	case string(codexresponses.KindRateLimit):
		status = http.StatusTooManyRequests
	case string(codexresponses.KindTimeout):
		status = http.StatusGatewayTimeout
	case string(codexresponses.KindClientCanceled):
		status = 499
	}
	if failure.ErrorCode == string(codexresponses.KindInvalidToken) {
		code = ErrorCodeUpstreamUnavailable
	}
	h.writeArchivedAPIError(w, round, r, started, provider, model, stream, status, APIError{Code: code, Message: "Codex OAuth response failed: " + failure.ErrorCode, Model: model, ClientProtocol: ClientProtocolOpenAI, ClientEndpoint: NormalizeClientEndpoint(r.URL.Path), Operation: OperationForPath(r.URL.Path), UpstreamProtocol: effectivecatalog.CodexOAuthProviderID})
}

func copyCodexHeaders(target http.Header, headers []codexresponses.Header) {
	for _, header := range headers {
		name, value := strings.TrimSpace(header.Name), strings.TrimSpace(header.Value)
		if name == "Content-Type" || name == "X-Request-ID" {
			if value != "" {
				target.Set(name, value)
			}
		}
	}
}

func codexResponseUsage(body []byte, request map[string]any) tokenUsage {
	var payload struct {
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(body, &payload) == nil {
		if usage, ok := usageFromRaw(payload.Usage); ok {
			return usage
		}
	}
	return tokenUsage{PromptTokens: estimatePromptTokens(request), CompletionTokens: estimateCompletionTokensFromResponse(body), Estimated: true, Known: true}
}

func streamFailFromCodexError(err error) *streamFail {
	failure, ok := codexresponses.AsFailure(err)
	if !ok || failure == nil {
		return newStreamFailWithCode(streamKindUpstreamFailed, string(codexresponses.KindUpstream), "Codex upstream failed", err, true)
	}
	return streamFailFromCodex(failure)
}

func streamFailFromCodex(failure *codexresponses.Failure) *streamFail {
	if failure == nil {
		return nil
	}
	var kind streamKind
	countUpstream := false
	switch failure.Kind {
	case codexresponses.KindClientCanceled:
		kind = streamKindClientCanceled
	case codexresponses.KindClientWrite:
		kind = streamKindClientWrite
	case codexresponses.KindProtocol:
		kind = streamKindProtocol
	case codexresponses.KindRateLimit, codexresponses.KindInvalidToken, codexresponses.KindTimeout, codexresponses.KindNetwork, codexresponses.KindUpstream:
		kind, countUpstream = streamKindUpstreamFailed, true
	default:
		kind = streamKindError
	}
	return newStreamFailWithCode(kind, string(failure.Kind), failure.Error(), failure, countUpstream)
}
