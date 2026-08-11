package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	http "github.com/bogdanfinn/fhttp"
	"github.com/google/uuid"
)

const (
	maxSearchQueryBytes  = 64 << 10
	maxSearchSSEBytes    = 4 << 20
	maxSearchDocument    = 4 << 20
	maxSearchTextBytes   = 256 << 10
	maxSearchSourceCount = 32
	// searchPrepareRetryDelay is intentionally small: an EOF before the
	// prepare response is often a transient proxy or edge connection close,
	// while a long retry would only consume the caller's search budget.
	searchPrepareRetryDelay = 250 * time.Millisecond
)

var searchURLPattern = regexp.MustCompile(`https?://[^\s<>"'）)\]]+`)

// SearchRequest is a deliberately narrow projection of one forced Web search
// interaction. The Web upstream owns all transient conversation state.
type SearchRequest struct {
	Model string
	Query string
}

type SearchSource struct {
	Title   string
	URL     string
	Snippet string
}

type SearchResult struct {
	ConversationID string
	ActualModel    string
	Text           string
	Sources        []SearchSource
}

// Search creates an isolated ChatGPT Web conversation with search forced on,
// then polls its bounded document projection until the assistant answer has
// reached a terminal state. It never returns the raw conversation document.
func (c *Client) Search(ctx context.Context, request SearchRequest) (SearchResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	result, err := c.searchOnce(ctx, request)
	if !retryableSearchPrepareFailure(err) || c.newSearchClient == nil {
		return result, err
	}
	if !waitForSearchPrepareRetry(ctx) {
		// Preserve the classified transport error. The caller can then record a
		// meaningful account result rather than treating a canceled backoff as
		// an unrelated upstream failure.
		return result, err
	}
	retryClient, retryClientErr := c.newSearchClient()
	if retryClientErr != nil {
		return result, err
	}
	return retryClient.searchOnce(ctx, request)
}

// searchOnce owns one isolated browser session. Search retries must create a
// new Client and invoke this method directly so a broken keep-alive/TLS path is
// never reused.
func (c *Client) searchOnce(ctx context.Context, request SearchRequest) (SearchResult, error) {
	query := strings.TrimSpace(request.Query)
	if query == "" {
		return SearchResult{}, fmt.Errorf("search query is required")
	}
	if len(query) > maxSearchQueryBytes {
		return SearchResult{}, fmt.Errorf("search query exceeds %d KiB", maxSearchQueryBytes>>10)
	}
	// A search is a standalone upstream interaction.  Never use the static
	// browser root (or let two requests share one) because ChatGPT Web may use
	// that root to associate requests with account-level conversation state.
	// The same root is intentionally reused only for the prepare/start pair of
	// this one request.
	rootMessageID := uuid.NewString()
	conduit, err := c.prepareSearch(ctx, request.Model, query, rootMessageID)
	if err != nil {
		return SearchResult{}, err
	}
	requirements, err := c.ChatRequirements()
	if err != nil {
		return SearchResult{}, err
	}
	conversationID, err := c.startSearch(ctx, request.Model, query, conduit, rootMessageID, requirements)
	if err != nil {
		return SearchResult{}, err
	}
	return c.pollSearch(ctx, conversationID)
}

func retryableSearchPrepareFailure(err error) bool {
	var upstream *Error
	if !errors.As(err, &upstream) || upstream.Operation != "search_prepare" {
		return false
	}
	return upstream.Class == TLS || upstream.Class == Timeout
}

func waitForSearchPrepareRetry(ctx context.Context) bool {
	timer := time.NewTimer(searchPrepareRetryDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (c *Client) prepareSearch(ctx context.Context, model, query, rootMessageID string) (string, error) {
	type message struct {
		ID     string `json:"id"`
		Author struct {
			Role string `json:"role"`
		} `json:"author"`
		Content struct {
			ContentType string   `json:"content_type"`
			Parts       []string `json:"parts"`
		} `json:"content"`
	}
	type payload struct {
		Action             string `json:"action"`
		ForkFromSharedPost bool   `json:"fork_from_shared_post"`
		ParentMessageID    string `json:"parent_message_id"`
		Model              string `json:"model"`
		ClientPrepareState string `json:"client_prepare_state"`
		TimezoneOffsetMins int    `json:"timezone_offset_min"`
		Timezone           string `json:"timezone"`
		ConversationMode   struct {
			Kind string `json:"kind"`
		} `json:"conversation_mode"`
		SystemHints                []string `json:"system_hints"`
		PartialQuery               message  `json:"partial_query"`
		SupportsBuffering          bool     `json:"supports_buffering"`
		SupportedEncodings         []string `json:"supported_encodings"`
		HistoryAndTrainingDisabled bool     `json:"history_and_training_disabled"`
		ClientContextualInfo       struct {
			AppName string `json:"app_name"`
		} `json:"client_contextual_info"`
	}
	p := payload{
		Action: "next", ParentMessageID: strings.TrimSpace(rootMessageID), Model: textModelSlug(model), ClientPrepareState: "success",
		TimezoneOffsetMins: -480, Timezone: "Asia/Shanghai", SystemHints: []string{"search"}, SupportsBuffering: true,
		SupportedEncodings: []string{"v1"}, HistoryAndTrainingDisabled: true,
	}
	if p.ParentMessageID == "" {
		p.ParentMessageID = uuid.NewString()
	}
	p.ConversationMode.Kind = "primary_assistant"
	p.PartialQuery.ID = uuid.NewString()
	p.PartialQuery.Author.Role = "user"
	p.PartialQuery.Content.ContentType = "text"
	p.PartialQuery.Content.Parts = []string{query}
	p.ClientContextualInfo.AppName = "chatgpt.com"
	body, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("encode search prepare: %w", err)
	}
	response, err := c.searchRequest(ctx, http.MethodPost, "/backend-api/f/conversation/prepare", body, Requirements{}, "no-token", "application/json", "search_prepare")
	if err != nil {
		return "", err
	}
	var decoded struct {
		ConduitToken string `json:"conduit_token"`
	}
	if err := json.Unmarshal(response, &decoded); err != nil {
		return "", fmt.Errorf("decode search prepare: %w", err)
	}
	if strings.TrimSpace(decoded.ConduitToken) == "" {
		return "", fmt.Errorf("search prepare: missing conduit token")
	}
	return strings.TrimSpace(decoded.ConduitToken), nil
}

func (c *Client) startSearch(ctx context.Context, model, query, conduit, rootMessageID string, requirements Requirements) (string, error) {
	type message struct {
		ID     string `json:"id"`
		Author struct {
			Role string `json:"role"`
		} `json:"author"`
		CreateTime float64 `json:"create_time"`
		Content    struct {
			ContentType string   `json:"content_type"`
			Parts       []string `json:"parts"`
		} `json:"content"`
		Metadata struct {
			DeveloperModeConnectorIDs []string `json:"developer_mode_connector_ids"`
			SelectedGitHubRepos       []string `json:"selected_github_repos"`
			SelectedAllGitHubRepos    bool     `json:"selected_all_github_repos"`
			SystemHints               []string `json:"system_hints"`
			SerializationMetadata     struct {
				CustomSymbolOffsets []int `json:"custom_symbol_offsets"`
			} `json:"serialization_metadata"`
		} `json:"metadata"`
	}
	type payload struct {
		Action             string    `json:"action"`
		Messages           []message `json:"messages"`
		ParentMessageID    string    `json:"parent_message_id"`
		Model              string    `json:"model"`
		ClientPrepareState string    `json:"client_prepare_state"`
		TimezoneOffsetMins int       `json:"timezone_offset_min"`
		Timezone           string    `json:"timezone"`
		ConversationMode   struct {
			Kind string `json:"kind"`
		} `json:"conversation_mode"`
		EnableFollowups            bool     `json:"enable_message_followups"`
		SystemHints                []string `json:"system_hints"`
		SupportsBuffering          bool     `json:"supports_buffering"`
		SupportedEncodings         []string `json:"supported_encodings"`
		ForceUseSearch             bool     `json:"force_use_search"`
		ClientReportedSource       string   `json:"client_reported_search_source"`
		HistoryAndTrainingDisabled bool     `json:"history_and_training_disabled"`
		ClientContextualInfo       struct {
			IsDarkMode      bool    `json:"is_dark_mode"`
			TimeSinceLoaded int     `json:"time_since_loaded"`
			PageHeight      int     `json:"page_height"`
			PageWidth       int     `json:"page_width"`
			PixelRatio      float64 `json:"pixel_ratio"`
			ScreenHeight    int     `json:"screen_height"`
			ScreenWidth     int     `json:"screen_width"`
			AppName         string  `json:"app_name"`
		} `json:"client_contextual_info"`
		ParagenOverride     string `json:"paragen_cot_summary_display_override"`
		ForceParallelSwitch string `json:"force_parallel_switch"`
	}
	msg := message{ID: uuid.NewString(), CreateTime: float64(time.Now().UnixNano()) / float64(time.Second)}
	msg.Author.Role = "user"
	msg.Content.ContentType, msg.Content.Parts = "text", []string{query}
	msg.Metadata.DeveloperModeConnectorIDs = []string{}
	msg.Metadata.SelectedGitHubRepos = []string{}
	msg.Metadata.SystemHints = []string{"search"}
	msg.Metadata.SerializationMetadata.CustomSymbolOffsets = []int{}
	p := payload{
		Action: "next", Messages: []message{msg}, ParentMessageID: strings.TrimSpace(rootMessageID), Model: textModelSlug(model),
		ClientPrepareState: "success", TimezoneOffsetMins: -480, Timezone: "Asia/Shanghai", EnableFollowups: true,
		SystemHints: []string{}, SupportsBuffering: true, SupportedEncodings: []string{"v1"}, ForceUseSearch: true,
		ClientReportedSource: "conversation_composer_web_icon", ParagenOverride: "allow", ForceParallelSwitch: "auto",
	}
	if p.ParentMessageID == "" {
		p.ParentMessageID = uuid.NewString()
	}
	p.HistoryAndTrainingDisabled = true
	p.ConversationMode.Kind = "primary_assistant"
	p.ClientContextualInfo = struct {
		IsDarkMode      bool    `json:"is_dark_mode"`
		TimeSinceLoaded int     `json:"time_since_loaded"`
		PageHeight      int     `json:"page_height"`
		PageWidth       int     `json:"page_width"`
		PixelRatio      float64 `json:"pixel_ratio"`
		ScreenHeight    int     `json:"screen_height"`
		ScreenWidth     int     `json:"screen_width"`
		AppName         string  `json:"app_name"`
	}{false, 36, 925, 886, 2, 1440, 2560, "chatgpt.com"}
	body, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("encode search conversation: %w", err)
	}
	req, err := c.newImageRequest(http.MethodPost, "/backend-api/f/conversation", body, requirements, conduit, "text/event-stream")
	if err != nil {
		return "", err
	}
	if requirements.TurnstileToken != "" {
		req.Header.Set("openai-sentinel-turnstile-token", requirements.TurnstileToken)
	}
	if requirements.SOToken != "" {
		req.Header.Set("openai-sentinel-so-token", requirements.SOToken)
	}
	req = req.WithContext(ctx)
	response, err := c.doer.Do(req)
	if err != nil {
		return "", classifyTransport("search_conversation", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", classifyStatusResponse("search_conversation", response.StatusCode, response.Body)
	}
	conversationID, err := parseSearchConversationID(ctx, io.LimitReader(response.Body, maxSearchSSEBytes))
	if err != nil {
		return "", err
	}
	return conversationID, nil
}

func (c *Client) searchRequest(ctx context.Context, method, path string, body []byte, requirements Requirements, conduit, accept, operation string) ([]byte, error) {
	req, err := c.newImageRequest(method, path, body, requirements, conduit, accept)
	if err != nil {
		return nil, err
	}
	if requirements.Token == "" {
		// prepare and conversation polling are authenticated browser requests,
		// but are not chat turns and must not carry an empty sentinel header.
		req.Header.Del("openai-sentinel-chat-requirements-token")
	}
	req = req.WithContext(ctx)
	response, err := c.doer.Do(req)
	if err != nil {
		return nil, classifyTransport(operation, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, classifyStatusResponse(operation, response.StatusCode, response.Body)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxSearchDocument+1))
	if err != nil {
		return nil, classifyTransport(operation, err)
	}
	if len(data) > maxSearchDocument {
		return nil, fmt.Errorf("%s: response exceeds %d MiB", operation, maxSearchDocument>>20)
	}
	return data, nil
}

func parseSearchConversationID(ctx context.Context, reader io.Reader) (string, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), maxTextSSELineBytes)
	dataLines := make([]string, 0, 2)
	conversationID := ""
	flush := func() {
		if len(dataLines) == 0 || conversationID != "" {
			dataLines = dataLines[:0]
			return
		}
		payload := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		var value any
		if json.Unmarshal([]byte(payload), &value) == nil {
			conversationID = searchString(value, "conversation_id")
		}
	}
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		line := scanner.Text()
		if line == "" {
			flush()
			if conversationID != "" {
				return conversationID, nil
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read search SSE: %w", err)
	}
	flush()
	if conversationID == "" {
		return "", fmt.Errorf("search conversation: conversation ID not found in SSE")
	}
	return conversationID, nil
}

func (c *Client) pollSearch(ctx context.Context, conversationID string) (SearchResult, error) {
	deadline := time.Now().Add(5 * time.Minute)
	var last SearchResult
	stable := 0
	for {
		if err := ctx.Err(); err != nil {
			return SearchResult{}, err
		}
		document, err := c.searchRequest(ctx, http.MethodGet, "/backend-api/conversation/"+url.PathEscape(conversationID), nil, Requirements{}, "", "application/json", "search_poll")
		if err != nil {
			if !retryableSearchPollError(err) {
				return SearchResult{}, err
			}
		} else {
			result, terminal, parseErr := parseSearchDocument(conversationID, document)
			if parseErr != nil {
				return SearchResult{}, parseErr
			}
			if result.Text != "" {
				if terminal {
					return result, nil
				}
				if result.Text == last.Text {
					stable++
				} else {
					stable = 0
				}
				last = result
				if stable >= 2 {
					return result, nil
				}
			}
		}
		if time.Until(deadline) <= 0 {
			if last.Text != "" {
				return last, nil
			}
			return SearchResult{}, fmt.Errorf("search poll: timed out waiting for result")
		}
		if err := waitForPoll(ctx, minDuration(3*time.Second, time.Until(deadline))); err != nil {
			return SearchResult{}, fmt.Errorf("search poll: %w", err)
		}
	}
}

func retryableSearchPollError(err error) bool {
	upstream, ok := err.(*Error)
	if !ok {
		return false
	}
	if upstream.Class == RateLimit {
		return true
	}
	return upstream.Class == Upstream && (upstream.StatusCode == 404 || upstream.StatusCode == 409 || upstream.StatusCode == 423 || upstream.StatusCode >= 500)
}

func parseSearchDocument(conversationID string, document []byte) (SearchResult, bool, error) {
	var root map[string]any
	if err := json.Unmarshal(document, &root); err != nil {
		return SearchResult{}, false, fmt.Errorf("decode search conversation: %w", err)
	}
	mapping, _ := root["mapping"].(map[string]any)
	var latest map[string]any
	latestTime := float64(-1)
	var latestWithAnswer map[string]any
	latestWithAnswerTime := float64(-1)
	for _, rawNode := range mapping {
		node, _ := rawNode.(map[string]any)
		message, _ := node["message"].(map[string]any)
		author, _ := message["author"].(map[string]any)
		role, _ := author["role"].(string)
		if strings.ToLower(strings.TrimSpace(role)) != "assistant" {
			continue
		}
		created, _ := message["create_time"].(float64)
		if latest == nil || created >= latestTime {
			latest, latestTime = message, created
		}
		// Search tool invocations are represented by assistant nodes containing
		// text such as search("query"). They are control-plane placeholders, not
		// user-visible answers; do not let one terminate polling or hide the
		// latest answer-bearing node.
		text := searchMessageText(message)
		if text != "" && !isSearchInvocationPlaceholder(text) && (latestWithAnswer == nil || created >= latestWithAnswerTime) {
			latestWithAnswer, latestWithAnswerTime = message, created
		}
	}
	if latest == nil || isSearchInvocationPlaceholder(searchMessageText(latest)) || searchMessageText(latest) == "" {
		latest = latestWithAnswer
	}
	if latest == nil {
		return SearchResult{ConversationID: bounded(conversationID, 512)}, false, nil
	}
	metadata, _ := latest["metadata"].(map[string]any)
	answerText := searchMessageText(latest)
	result := SearchResult{ConversationID: bounded(conversationID, 512), ActualModel: bounded(searchMapString(metadata, "model_slug"), 256), Text: bounded(answerText, maxSearchTextBytes)}
	result.Sources = collectSearchSources(latest)
	for _, found := range searchURLPattern.FindAllString(result.Text, -1) {
		result.Sources = appendSearchSource(result.Sources, SearchSource{URL: found})
	}
	terminal := false
	if finish, _ := metadata["finish_details"].(map[string]any); finish != nil {
		terminal = strings.TrimSpace(searchMapString(finish, "type")) != ""
	}
	if !terminal {
		terminal = strings.TrimSpace(searchMapString(metadata, "status")) == "completed"
	}
	return result, terminal, nil
}

// isSearchInvocationPlaceholder identifies the assistant control message that
// ChatGPT Web may persist while a forced search is still running. It must not
// be surfaced as the final answer when the upstream marks that node complete.
func isSearchInvocationPlaceholder(text string) bool {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "search(") || !strings.HasSuffix(text, ")") {
		return false
	}
	var query string
	return json.Unmarshal([]byte(strings.TrimSpace(text[len("search("):len(text)-1])), &query) == nil
}

func searchMessageText(message map[string]any) string {
	content := message["content"]
	parts := make([]string, 0, 4)
	switch value := content.(type) {
	case string:
		parts = append(parts, value)
	case map[string]any:
		if text := searchMapString(value, "text"); text != "" {
			parts = append(parts, text)
		}
		if rawParts, ok := value["parts"].([]any); ok {
			for _, raw := range rawParts {
				switch item := raw.(type) {
				case string:
					parts = append(parts, item)
				case map[string]any:
					for _, key := range []string{"text", "summary", "content"} {
						if text := searchMapString(item, key); text != "" {
							parts = append(parts, text)
						}
					}
				}
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func collectSearchSources(value any) []SearchSource {
	sources := []SearchSource{}
	var walk func(any)
	walk = func(current any) {
		if len(sources) >= maxSearchSourceCount {
			return
		}
		switch item := current.(type) {
		case map[string]any:
			metadata, _ := item["metadata"].(map[string]any)
			entry := SearchSource{Title: firstSearchString(item, "title", "name", "source"), URL: firstSearchString(item, "url", "link", "source_url"), Snippet: firstSearchString(item, "snippet", "description", "text")}
			if entry.URL == "" && metadata != nil {
				entry.URL = firstSearchString(metadata, "url", "link", "source_url")
			}
			sources = appendSearchSource(sources, entry)
			for _, child := range item {
				walk(child)
			}
		case []any:
			for _, child := range item {
				walk(child)
			}
		}
	}
	walk(value)
	sort.SliceStable(sources, func(i, j int) bool { return sources[i].URL < sources[j].URL })
	return sources
}

func appendSearchSource(sources []SearchSource, source SearchSource) []SearchSource {
	if len(sources) >= maxSearchSourceCount {
		return sources
	}
	source.URL = strings.TrimSpace(source.URL)
	parsed, err := url.Parse(source.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return sources
	}
	source.URL = bounded(source.URL, 2048)
	source.Title = bounded(strings.TrimSpace(source.Title), 512)
	source.Snippet = bounded(strings.TrimSpace(source.Snippet), 2048)
	for _, existing := range sources {
		if existing.URL == source.URL {
			return sources
		}
	}
	return append(sources, source)
}

func firstSearchString(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if text := searchMapString(value, key); text != "" {
			return text
		}
	}
	return ""
}
func searchMapString(value map[string]any, key string) string {
	text, _ := value[key].(string)
	return strings.TrimSpace(text)
}
func searchString(value any, key string) string {
	switch item := value.(type) {
	case map[string]any:
		if found := searchMapString(item, key); found != "" {
			return found
		}
		for _, child := range item {
			if found := searchString(child, key); found != "" {
				return found
			}
		}
	case []any:
		for _, child := range item {
			if found := searchString(child, key); found != "" {
				return found
			}
		}
	}
	return ""
}
