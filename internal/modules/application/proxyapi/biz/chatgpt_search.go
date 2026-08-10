package biz

import (
	"context"
	"fmt"
	"strings"

	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgptfail"
	"aetherrelay/internal/modules/application/proxyapi/pkg/chatgptsearch"
	upcommon "aetherrelay/internal/modules/blocks/chatgptwebupstream/pkg/common"
	upevents "aetherrelay/internal/modules/blocks/chatgptwebupstream/pkg/events"
	"github.com/muidea/magicCommon/event"
)

// Search reuses the text account pool operation, including account-scoped
// model filtering, result feedback, cooldown and one refresh-before-retry.
// A search never exposes partial output, so it is safe to retry only when the
// first forced-search conversation did not establish usable result state.
func (s *Proxy) Search(ctx context.Context, request chatgptsearch.Request) (out chatgptsearch.Result, err error) {
	if strings.TrimSpace(request.Query) == "" {
		return chatgptsearch.Result{}, chatgptfail.New(chatgptfail.KindInternal, fmt.Errorf("search query is required"))
	}
	account, err := s.acquireChatGPTTextToken(ctx, request.Model)
	if err != nil {
		return chatgptsearch.Result{}, err
	}
	defer func() { s.recordChatGPTTextResult(ctx, account.Account.ID, request.Model, err) }()

	out, err = s.searchChatGPTOnce(ctx, account.AccessToken, account.Account.Proxy, request)
	if !isInvalidChatGPTTextFailure(err) {
		return out, err
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return out, mapContextError(contextErr)
	}
	refreshed, permanentFailure, refreshErr := s.refreshChatGPTTextToken(ctx, account.AccessToken)
	if refreshErr != nil {
		return out, refreshErr
	}
	if permanentFailure {
		s.removeInvalidChatGPTTextToken(ctx, account.AccessToken)
		return out, err
	}
	out, err = s.searchChatGPTOnce(ctx, refreshed.AccessToken, refreshed.Account.Proxy, request)
	if isInvalidChatGPTTextFailure(err) {
		s.removeInvalidChatGPTTextToken(ctx, refreshed.AccessToken)
	}
	return out, err
}

func (s *Proxy) searchChatGPTOnce(ctx context.Context, token, proxy string, request chatgptsearch.Request) (chatgptsearch.Result, error) {
	value, eventErr := s.SendEvent(event.NewEventWithContext(upevents.TopicSearch, s.ID(), upcommon.UnitID, event.NewHeader(), ctx, upevents.SearchCommand{
		AccessToken: token, Proxy: proxy, Model: request.Model, Query: request.Query,
	})).Get()
	if eventErr != nil {
		partial, isPartial := value.(upevents.SearchResult)
		out := chatgptsearch.Result{}
		if isPartial {
			out = searchResultFromUpstream(partial)
			if partial.ErrorClass != "" {
				return out, mapUpstreamTextFailure(partial.ErrorClass, eventErr)
			}
		}
		return out, chatgptfail.New(chatgptfail.KindUpstream, fmt.Errorf("chatgpt search failed"))
	}
	searched, ok := value.(upevents.SearchResult)
	if !ok {
		return chatgptsearch.Result{}, chatgptfail.New(chatgptfail.KindInternal, fmt.Errorf("invalid chatgpt search result"))
	}
	out := searchResultFromUpstream(searched)
	if searched.ErrorClass != "" {
		return out, mapUpstreamTextFailure(searched.ErrorClass, fmt.Errorf("chatgpt search failed"))
	}
	if strings.TrimSpace(out.Text) == "" {
		return out, chatgptfail.New(chatgptfail.KindUpstream, fmt.Errorf("chatgpt search returned no answer"))
	}
	return out, nil
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
