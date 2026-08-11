package biz

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	accevents "aetherrelay/internal/modules/application/chatgptaccountpool/pkg/events"
	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgptfail"
	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgptsearch"
	upcommon "aetherrelay/internal/modules/blocks/chatgptwebupstream/pkg/common"
	upevents "aetherrelay/internal/modules/blocks/chatgptwebupstream/pkg/events"
	"github.com/muidea/magicCommon/event"
)

// Search reuses the text account pool operation, including account-scoped
// model filtering, result feedback, cooldown and one refresh-before-retry.
// It may fail over exactly once only after a retryable prepare failure. At
// that point no ChatGPT search conversation has been established, so retrying
// with a different account/proxy cannot create a duplicate visible answer.
func (s *Proxy) Search(ctx context.Context, request chatgptsearch.Request) (out chatgptsearch.Result, err error) {
	if strings.TrimSpace(request.Query) == "" {
		return chatgptsearch.Result{}, chatgptfail.New(chatgptfail.KindInternal, fmt.Errorf("search query is required"))
	}
	account, err := s.acquireChatGPTTextToken(ctx, request.Model)
	if err != nil {
		return chatgptsearch.Result{}, err
	}
	attempt, err := s.searchChatGPTWithAccount(ctx, account, request)
	out = attempt.result
	s.recordChatGPTTextResult(ctx, account.Account.ID, request.Model, err)
	if err == nil || !retryableSearchPrepareFailure(attempt, err) {
		return out, err
	}

	fallback, fallbackErr := s.acquireAlternativeSearchAccount(ctx, request.Model, attempt)
	if fallbackErr != nil {
		// The initial classified error is more actionable than a generic
		// no-account result after all remaining candidates were excluded.
		return out, err
	}
	attempt, err = s.searchChatGPTWithAccount(ctx, fallback, request)
	out = attempt.result
	s.recordChatGPTTextResult(ctx, fallback.Account.ID, request.Model, err)
	return out, err
}

type chatGPTSearchAttempt struct {
	result      chatgptsearch.Result
	operation   string
	accessToken string
	proxy       string
}

func (s *Proxy) searchChatGPTWithAccount(ctx context.Context, account accevents.AcquireTextTokenResult, request chatgptsearch.Request) (chatGPTSearchAttempt, error) {
	attempt, err := s.searchChatGPTOnce(ctx, account.AccessToken, account.Account.Proxy, request)
	if !isInvalidChatGPTTextFailure(err) {
		return attempt, err
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return attempt, mapContextError(contextErr)
	}
	refreshed, permanentFailure, refreshErr := s.refreshChatGPTTextToken(ctx, account.AccessToken)
	if refreshErr != nil {
		return attempt, refreshErr
	}
	if permanentFailure {
		s.removeInvalidChatGPTTextToken(ctx, account.AccessToken)
		return attempt, err
	}
	attempt, err = s.searchChatGPTOnce(ctx, refreshed.AccessToken, refreshed.Account.Proxy, request)
	if isInvalidChatGPTTextFailure(err) {
		s.removeInvalidChatGPTTextToken(ctx, refreshed.AccessToken)
	}
	return attempt, err
}

func (s *Proxy) searchChatGPTOnce(ctx context.Context, token, proxy string, request chatgptsearch.Request) (chatGPTSearchAttempt, error) {
	attempt := chatGPTSearchAttempt{accessToken: strings.TrimSpace(token), proxy: strings.TrimSpace(proxy)}
	value, eventErr := s.SendEvent(event.NewEventWithContext(upevents.TopicSearch, s.ID(), upcommon.UnitID, event.NewHeader(), ctx, upevents.SearchCommand{
		AccessToken: token, Proxy: proxy, Model: request.Model, Query: request.Query,
	})).Get()
	if eventErr != nil {
		partial, isPartial := value.(upevents.SearchResult)
		if isPartial {
			attempt.result = searchResultFromUpstream(partial)
			attempt.operation = strings.TrimSpace(partial.ErrorOperation)
			if partial.ErrorClass != "" {
				return attempt, mapUpstreamTextFailure(partial.ErrorClass, eventErr)
			}
		}
		return attempt, chatgptfail.New(chatgptfail.KindUpstream, fmt.Errorf("chatgpt search failed"))
	}
	searched, ok := value.(upevents.SearchResult)
	if !ok {
		return attempt, chatgptfail.New(chatgptfail.KindInternal, fmt.Errorf("invalid chatgpt search result"))
	}
	attempt.result = searchResultFromUpstream(searched)
	attempt.operation = strings.TrimSpace(searched.ErrorOperation)
	if searched.ErrorClass != "" {
		return attempt, mapUpstreamTextFailure(searched.ErrorClass, fmt.Errorf("chatgpt search failed"))
	}
	if strings.TrimSpace(attempt.result.Text) == "" {
		return attempt, chatgptfail.New(chatgptfail.KindUpstream, fmt.Errorf("chatgpt search returned no answer"))
	}
	return attempt, nil
}

func retryableSearchPrepareFailure(attempt chatGPTSearchAttempt, err error) bool {
	if strings.TrimSpace(attempt.operation) != "search_prepare" || attempt.result.ConversationID != "" || attempt.result.Text != "" {
		return false
	}
	var failure *chatgptfail.Failure
	if !errors.As(err, &failure) {
		return false
	}
	return failure.Kind == chatgptfail.KindTLS || failure.Kind == chatgptfail.KindTimeout
}

// acquireAlternativeSearchAccount keeps the fallback away from the failed
// account and from the same configured proxy endpoint. Proxy credentials never
// leave this internal scheduler; comparison only guides candidate selection.
func (s *Proxy) acquireAlternativeSearchAccount(ctx context.Context, model string, failed chatGPTSearchAttempt) (accevents.AcquireTextTokenResult, error) {
	excluded := []string{failed.accessToken}
	seen := map[string]struct{}{failed.accessToken: {}}
	for {
		candidate, err := s.acquireChatGPTTextTokenExcluding(ctx, model, excluded)
		if err != nil {
			return accevents.AcquireTextTokenResult{}, err
		}
		token := strings.TrimSpace(candidate.AccessToken)
		if token == "" {
			return accevents.AcquireTextTokenResult{}, chatgptfail.New(chatgptfail.KindProviderUnavailable, fmt.Errorf("chatgpt fallback account is unavailable"))
		}
		if _, duplicate := seen[token]; duplicate {
			return accevents.AcquireTextTokenResult{}, chatgptfail.New(chatgptfail.KindProviderUnavailable, fmt.Errorf("chatgpt fallback account repeated"))
		}
		seen[token] = struct{}{}
		if !sameChatGPTProxyEndpoint(failed.proxy, candidate.Account.Proxy) {
			return candidate, nil
		}
		excluded = append(excluded, token)
	}
}

func sameChatGPTProxyEndpoint(left, right string) bool {
	left, right = strings.TrimSpace(left), strings.TrimSpace(right)
	if left == "" || right == "" {
		return false
	}
	leftURL, leftErr := url.Parse(left)
	rightURL, rightErr := url.Parse(right)
	if leftErr != nil || rightErr != nil || leftURL.Host == "" || rightURL.Host == "" {
		return left == right
	}
	return strings.EqualFold(leftURL.Scheme, rightURL.Scheme) && strings.EqualFold(leftURL.Host, rightURL.Host)
}

func searchResultFromUpstream(result upevents.SearchResult) chatgptsearch.Result {
	out := chatgptsearch.Result{ConversationID: result.ConversationID, ActualModel: result.ActualModel, Text: result.Text}
	if len(result.Sources) == 0 {
		return out
	}
	out.Sources = make([]chatgptsearch.Source, 0, len(result.Sources))
	for _, source := range result.Sources {
		out.Sources = append(out.Sources, chatgptsearch.Source{Title: source.Title, URL: source.URL, Snippet: source.Snippet})
	}
	return out
}
