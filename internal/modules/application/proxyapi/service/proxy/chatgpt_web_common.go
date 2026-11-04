package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgptfail"
	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgpttext"
	"aetherrelay/internal/modules/application/proxyapi/pkg/effectivecatalog"
	archive "aetherrelay/internal/pkg/aetherrelayarchive"
	"aetherrelay/internal/pkg/chatgpttokenusage"
)

func (h *Handler) writeChatGPTWebAPIError(w http.ResponseWriter, round *archive.Round, r *http.Request, start time.Time, provider, model string, stream bool, status int, apiErr APIError, fail *streamFail, tok tokenUsage) {
	if fail == nil {
		fail = newStreamFailWithCode(streamKindError, apiErr.Code, apiErr.Code+": "+apiErr.Message, nil, false)
	}
	if apiErr.FailureClass == "" {
		apiErr.FailureClass = chatGPTFailureCode(fail)
	}
	if fail.FailureClass == "" {
		fail.FailureClass = apiErr.FailureClass
	}
	if fail.Retryable == nil {
		fail.Retryable = apiErr.Retryable
	}
	if fail.RetryAfterSeconds == 0 {
		fail.RetryAfterSeconds = apiErr.RetryAfterSeconds
	}
	if apiErr.Code == ErrorCodeConversionUnsupported || apiErr.Code == ErrorCodeConversionLimitExceeded {
		if len(apiErr.UnsupportedFeatures) == 0 && apiErr.Feature != "" {
			apiErr.UnsupportedFeatures = []string{apiErr.Feature}
		}
		if round != nil {
			round.SetUnsupportedFeatures(apiErr.UnsupportedFeatures)
		}
	}
	if fail.ErrorCode == ErrorCodeInvalidRequest {
		apiErr.Code = ErrorCodeInvalidRequest
	}
	if apiErr.ClientProtocol == "" {
		apiErr.ClientProtocol = clientProtocolFromRequest(r)
	}
	if apiErr.ClientEndpoint == "" && r != nil && r.URL != nil {
		apiErr.ClientEndpoint = NormalizeClientEndpoint(r.URL.Path)
	}
	if apiErr.Model == "" {
		apiErr.Model = model
	}
	writeClientProtocolError(w, status, apiErr.ClientProtocol, apiErr)
	if apiErr.Type == "" {
		apiErr.Type = openAIErrorType(apiErr.Code)
	}
	body, _ := json.Marshal(APIErrorResponse{Error: apiErr})
	body = append(body, '\n')
	_ = h.writeArchiveResponse(round, "response.json", body)
	h.settleChatGPTWeb(round, r, provider, model, stream, status, time.Since(start), tok, fail)
}

func (h *Handler) settleChatGPTWeb(round *archive.Round, r *http.Request, provider, model string, stream bool, status int, duration time.Duration, tok tokenUsage, fail *streamFail) {
	if provider == "" {
		provider = effectivecatalog.BuiltinProviderID
	}
	if round != nil && round.UpstreamProtocol == "" {
		path := pathOrEmpty(r)
		round.SetTransportPlan(
			RouteLabel(r),
			NormalizeClientEndpoint(path),
			ClientProtocolForPath(path),
			effectivecatalog.BuiltinProviderID,
			"chatgptweb",
			TransportModeNative,
		)
	}
	if round != nil && fail != nil {
		var typed *chatgptfail.Failure
		if errors.As(fail.Err, &typed) {
			round.UpstreamStatus = typed.UpstreamStatus
		}
	}
	outcome := outcomeFromStreamFail(fail, status)
	h.recordAndPrintFail(round, r, provider, model, stream, status, duration, tok, fail)
	msg := ""
	if fail != nil {
		msg = fail.Error()
	}
	h.writeArchiveMetadata(round, provider, model, stream, status, duration, tok, "response.json", msg, "", outcome)
}

func estimateChatGPTTextUsage(request chatgpttext.Request, completion string) tokenUsage {
	parts := make([]string, 0, len(request.Messages))
	for _, message := range request.Messages {
		parts = append(parts, message.Content)
	}
	estimated := tokenusage.EstimateChatTextUsage(parts, completion)
	if estimated == nil {
		return tokenUsage{Estimated: true, Known: true}
	}
	return tokenUsage{
		PromptTokens:     estimated.PromptTokens,
		CompletionTokens: estimated.CompletionTokens,
		TotalTokens:      estimated.TotalTokens,
		Estimated:        true,
		Known:            true,
	}
}

func streamFailFromChatGPTErr(err error) *streamFail {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return streamFailFromKind(chatgptfail.KindClientCanceled, "client canceled", err)
	}
	if f, ok := chatgpttext.AsFailure(err); ok && f != nil {
		return streamFailFromKind(f.Kind, "chatgpt web request failed: "+string(f.Kind), f)
	}
	return streamFailFromKind(chatgptfail.KindUpstream, "chatgpt web upstream request failed", err)
}

func streamFailFromKind(kind chatgptfail.Kind, message string, err error) *streamFail {
	var outcomeKind streamKind
	switch chatgptfail.Outcome(kind) {
	case "success":
		return nil
	case "client_canceled":
		outcomeKind = streamKindClientCanceled
	case "client_write":
		outcomeKind = streamKindClientWrite
	case "upstream_failed":
		outcomeKind = streamKindUpstreamFailed
	case "error":
		outcomeKind = streamKindError
	default:
		outcomeKind = streamKindError
	}
	return newStreamFailWithCode(outcomeKind, chatgptfail.ErrorCode(kind), message, err, chatgptfail.CountUpstream(kind))
}

func statusForChatGPTFailure(fail *streamFail) int {
	if fail == nil {
		return http.StatusOK
	}
	switch fail.ErrorCode {
	case chatgptfail.ErrorCode(chatgptfail.KindProviderUnavailable):
		return http.StatusServiceUnavailable
	case chatgptfail.ErrorCode(chatgptfail.KindClientCanceled):
		return 499
	case ErrorCodeInvalidRequest:
		return http.StatusBadRequest
	default:
		return http.StatusBadGateway
	}
}

func chatGPTFailureCode(fail *streamFail) string {
	if fail == nil {
		return "unclassified"
	}
	if fail.ErrorCode != "" {
		return fail.ErrorCode
	}
	return string(fail.Kind)
}

func pathOrEmpty(r *http.Request) string {
	if r == nil || r.URL == nil {
		return ""
	}
	return r.URL.Path
}

func unsupportedChatGPTWebFeature(feature string) *APIError {
	return &APIError{
		Code:    ErrorCodeConversionUnsupported,
		Message: fmt.Sprintf("chatgpt web does not support %s", feature),
		Feature: feature,
	}
}

func uniqueSortedFeatures(features []string) []string {
	if len(features) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(features))
	result := make([]string, 0, len(features))
	for _, feature := range features {
		feature = strings.TrimSpace(feature)
		if feature == "" {
			continue
		}
		if _, exists := seen[feature]; exists {
			continue
		}
		seen[feature] = struct{}{}
		result = append(result, feature)
	}
	sort.Strings(result)
	return result
}
