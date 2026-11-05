package biz

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"aetherrelay/internal/modules/application/chatgpttemporarychat/internal/store"
	"aetherrelay/internal/modules/application/chatgpttemporarychat/pkg/common"
	events "aetherrelay/internal/modules/application/chatgpttemporarychat/pkg/events"
	proxycommon "aetherrelay/internal/modules/application/proxyapi/pkg/common"
	proxyevents "aetherrelay/internal/modules/application/proxyapi/pkg/events"
	basebiz "aetherrelay/internal/modules/base/biz"
	configevents "aetherrelay/internal/modules/blocks/configruntime/pkg/events"
	"aetherrelay/internal/pkg/chatattachment"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

type TemporaryChat struct {
	basebiz.Base
	store  *store.Store
	topics []string

	turnMu   sync.Mutex
	turns    map[string]*turnRuntime
	stopping bool

	turnTimeout time.Duration
	stopCh      chan struct{}
	stopOnce    sync.Once
	purgeWG     sync.WaitGroup
	turnWG      sync.WaitGroup
}

type turnRuntime struct {
	ownerID           string
	conversationID    string
	turnID            string
	userSequence      int64
	assistantSequence int64
	model             string
	webSearch         bool
	done              bool
	errorClass        string
	errorMessage      string
	cancelRequested   bool
	requestCancel     context.CancelFunc
	updates           chan turnUpdate
}

type turnUpdate struct {
	delta        string
	actualModel  string
	done         bool
	message      *events.MessageView
	errorClass   string
	errorMessage string
}

func New(ctx context.Context, hub event.Hub, background task.BackgroundRoutine) (*TemporaryChat, *cd.Error) {
	b := &TemporaryChat{
		Base:   basebiz.New(common.UnitID, hub, background),
		turns:  map[string]*turnRuntime{},
		stopCh: make(chan struct{}),
	}
	bootstrap, err := configevents.RequestBootstrap(ctx, b.EventHub(), b.ID())
	if err != nil {
		return nil, cd.NewError(cd.IllegalParam, err.Error())
	}
	if !bootstrap.Config.ChatGPTWeb.TemporaryChat.Enabled {
		return b, nil
	}
	tc := bootstrap.Config.ChatGPTWeb.TemporaryChat
	b.turnTimeout = time.Duration(tc.TurnTimeoutSeconds) * time.Second
	b.store, err = store.Open(bootstrap.Config.State.Database, bootstrap.Config.State.MemoryLimit, bootstrap.Config.State.Threads, store.Config{
		RetentionDays:              tc.RetentionDays,
		MaxConversations:           tc.MaxConversations,
		MaxMessagesPerConversation: tc.MaxMessagesPerConversation,
		MaxMessageBytes:            tc.MaxMessageBytes,
	})
	if err != nil {
		return nil, cd.NewError(cd.Unexpected, "open temporary chat state: "+err.Error())
	}
	if _, err := b.store.InterruptStreaming(); err != nil {
		return nil, cd.NewError(cd.Unexpected, "recover temporary chat streams: "+err.Error())
	}
	b.topics = []string{
		events.TopicCreate,
		events.TopicList,
		events.TopicGet,
		events.TopicStartTurn,
		events.TopicPullTurn,
		events.TopicCancelTurn,
		events.TopicDelete,
		events.TopicGetImage,
		events.TopicGetAttachment,
	}
	b.SubscribeFunc(events.TopicCreate, b.handleCreate)
	b.SubscribeFunc(events.TopicList, b.handleList)
	b.SubscribeFunc(events.TopicGet, b.handleGet)
	b.SubscribeFunc(events.TopicStartTurn, b.handleStartTurn)
	b.SubscribeFunc(events.TopicPullTurn, b.handlePullTurn)
	b.SubscribeFunc(events.TopicCancelTurn, b.handleCancelTurn)
	b.SubscribeFunc(events.TopicDelete, b.handleDelete)
	b.SubscribeFunc(events.TopicGetImage, b.handleGetImage)
	b.SubscribeFunc(events.TopicGetAttachment, b.handleGetAttachment)
	b.purgeWG.Add(1)
	if err := b.AsyncTask(func() {
		defer b.purgeWG.Done()
		b.purgeLoop()
	}); err != nil {
		b.purgeWG.Done()
		b.Teardown(context.Background())
		return nil, cd.NewError(cd.ResourceExhausted, "temporary chat maintenance unavailable")
	}
	return b, nil
}

func (s *TemporaryChat) Run(context.Context) *cd.Error { return nil }

// Stop accepted workers before BackgroundRoutine waits for them. No database
// or EventHub dependency is released in this phase.
func (s *TemporaryChat) BeginShutdown(context.Context) {
	s.turnMu.Lock()
	s.stopping = true
	requestCancels := make([]context.CancelFunc, 0, len(s.turns))
	for _, turn := range s.turns {
		turn.cancelRequested = true
		if turn.requestCancel != nil {
			requestCancels = append(requestCancels, turn.requestCancel)
		}
	}
	s.turnMu.Unlock()
	s.stopOnce.Do(func() { close(s.stopCh) })
	for _, cancel := range requestCancels {
		cancel()
	}
}

func (s *TemporaryChat) Teardown(ctx context.Context) {
	s.BeginShutdown(ctx)
	for _, topic := range s.topics {
		s.UnsubscribeFunc(topic)
	}
	// Stream workers must publish their interrupted terminal state before the
	// DuckDB handle is closed. This prevents shutdown races and nil stores.
	s.turnWG.Wait()
	s.purgeWG.Wait()
	s.turnMu.Lock()
	s.turns = map[string]*turnRuntime{}
	s.turnMu.Unlock()
	if s.store != nil {
		if _, err := s.store.InterruptStreaming(); err != nil {
			slog.Warn("temporary chat interrupt on teardown failed", "error_class", "store")
		}
		if err := s.store.CloseContext(ctx); err != nil {
			panic(cd.NewError(cd.Unexpected, "close durable store: "+err.Error()))
		}
	}
	s.store = nil
}

func (s *TemporaryChat) purgeLoop() {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
		}
		if s.store == nil {
			return
		}
		if _, err := s.store.PurgeExpired(time.Now().UTC()); err != nil {
			slog.Warn("temporary chat purge failed", "error_class", "store")
		}
	}
}

func (s *TemporaryChat) handleCreate(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	if s.store == nil {
		result.Set(nil, cd.NewError(cd.Unexpected, "temporary chat unavailable"))
		return
	}
	cmd, ok := ev.Data().(events.CreateConversationCommand)
	if !ok || strings.TrimSpace(cmd.OwnerID) == "" || strings.TrimSpace(cmd.Model) == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid create conversation command"))
		return
	}
	value, catalogErr := s.SendEvent(event.NewEventWithContext(proxyevents.TopicFeatureCatalog, s.ID(), proxycommon.UnitID, event.NewHeader(), ev.Context(), proxyevents.FeatureCatalogCommand{})).Get()
	if catalogErr != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, "feature catalog unavailable"))
		return
	}
	catalog, ok := value.(proxyevents.FeatureCatalogResult)
	if !ok {
		result.Set(nil, cd.NewError(cd.Unexpected, "invalid feature catalog"))
		return
	}
	provider := ""
	for _, model := range catalog.TextModels {
		if model.ID == strings.TrimSpace(cmd.Model) && len(model.Providers) > 0 {
			provider = model.Providers[0].Name
			break
		}
	}
	if provider == "" {
		result.Set(nil, cd.NewError(cd.Unexpected, "no compatible provider for selected model"))
		return
	}
	view, err := s.store.CreateConversation(cmd.OwnerID, cmd.Model, cmd.ThinkingEffort, cmd.SystemPrompt, provider)
	if err != nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, err.Error()))
		return
	}
	result.Set(events.ConversationResult{Conversation: view}, nil)
}

func (s *TemporaryChat) handleList(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	if s.store == nil {
		result.Set(nil, cd.NewError(cd.Unexpected, "temporary chat unavailable"))
		return
	}
	cmd, ok := ev.Data().(events.ListConversationsCommand)
	if !ok || strings.TrimSpace(cmd.OwnerID) == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid list conversations command"))
		return
	}
	list, err := s.store.ListConversations(cmd.OwnerID, cmd.Cursor, cmd.Limit)
	if err != nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, err.Error()))
		return
	}
	result.Set(list, nil)
}

func (s *TemporaryChat) handleGet(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	if s.store == nil {
		result.Set(nil, cd.NewError(cd.Unexpected, "temporary chat unavailable"))
		return
	}
	cmd, ok := ev.Data().(events.GetConversationCommand)
	if !ok || strings.TrimSpace(cmd.OwnerID) == "" || strings.TrimSpace(cmd.ConversationID) == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid get conversation command"))
		return
	}
	detail, err := s.store.GetConversation(cmd.OwnerID, cmd.ConversationID, cmd.BeforeSequence, cmd.Limit)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(detail, nil)
}

func (s *TemporaryChat) handleStartTurn(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	if s.store == nil {
		result.Set(nil, cd.NewError(cd.Unexpected, "temporary chat unavailable"))
		return
	}
	cmd, ok := ev.Data().(events.StartTurnCommand)
	if !ok || strings.TrimSpace(cmd.OwnerID) == "" || strings.TrimSpace(cmd.ConversationID) == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid start turn command"))
		return
	}
	if cmd.WebSearch && (len(cmd.Images) > 0 || len(cmd.Attachments) > 0) {
		result.Set(nil, cd.NewError(cd.IllegalParam, "web search does not support image or file attachments"))
		return
	}
	s.turnMu.Lock()
	stopping := s.stopping
	s.turnMu.Unlock()
	if stopping {
		result.Set(nil, cd.NewError(cd.Unexpected, "temporary chat unavailable"))
		return
	}
	started, err := s.store.StartTurnWithAttachments(cmd.OwnerID, cmd.ConversationID, cmd.Content, cmd.Images, cmd.Attachments)
	if err != nil {
		if strings.Contains(err.Error(), "streaming") || strings.Contains(err.Error(), "recovery") {
			result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
			return
		}
		result.Set(nil, cd.NewError(cd.IllegalParam, err.Error()))
		return
	}
	var messages []proxyevents.FeatureTextMessage
	if cmd.WebSearch {
		// Forced Web search is intentionally an isolated query. Earlier temporary
		// chat messages (including attachments) remain visible in history but are
		// not sent to the dedicated search endpoint.
		messages = []proxyevents.FeatureTextMessage{{Role: "user", Content: cmd.Content}}
	} else {
		detail, err := s.store.GetConversation(cmd.OwnerID, cmd.ConversationID, nil, 200)
		if err != nil {
			_, _ = s.store.CompleteFeatureTurn(cmd.OwnerID, cmd.ConversationID, started.UserSequence, started.AssistantSequence, "", "", false, "store", "failed to load conversation history")
			result.Set(nil, cd.NewError(cd.Unexpected, "failed to load conversation history"))
			return
		}
		messages, err = s.buildFeatureMessages(cmd.OwnerID, cmd.ConversationID, detail, started)
		if err != nil {
			_, _ = s.store.CompleteFeatureTurn(cmd.OwnerID, cmd.ConversationID, started.UserSequence, started.AssistantSequence, "", "", false, "store", "failed to load conversation attachments")
			result.Set(nil, cd.NewError(cd.Unexpected, "failed to load conversation attachments"))
			return
		}
	}
	timeout := s.turnTimeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	requestCtx, cancel := context.WithTimeout(context.Background(), timeout)
	runtime := &turnRuntime{
		ownerID:           cmd.OwnerID,
		conversationID:    cmd.ConversationID,
		turnID:            started.TurnID,
		userSequence:      started.UserSequence,
		assistantSequence: started.AssistantSequence,
		model:             started.Model,
		webSearch:         cmd.WebSearch,
		requestCancel:     cancel,
		updates:           make(chan turnUpdate, 64),
	}
	s.turnMu.Lock()
	if s.stopping {
		s.turnMu.Unlock()
		cancel()
		_, _ = s.store.CompleteFeatureTurn(cmd.OwnerID, cmd.ConversationID, started.UserSequence, started.AssistantSequence, "", "", true, "interrupted", "request interrupted by process shutdown")
		result.Set(nil, cd.NewError(cd.Unexpected, "temporary chat unavailable"))
		return
	}
	s.turns[turnKey(cmd.OwnerID, cmd.ConversationID, started.TurnID)] = runtime
	s.turnWG.Add(1)
	s.turnMu.Unlock()
	if err := s.AsyncTask(func() {
		defer s.turnWG.Done()
		s.runFeatureTurn(requestCtx, runtime, messages, started.ThinkingEffort)
	}); err != nil {
		s.turnWG.Done()
		cancel()
		s.turnMu.Lock()
		delete(s.turns, turnKey(cmd.OwnerID, cmd.ConversationID, started.TurnID))
		s.turnMu.Unlock()
		_, _ = s.store.CompleteFeatureTurn(cmd.OwnerID, cmd.ConversationID, started.UserSequence, started.AssistantSequence, "", "", true, "interrupted", "task admission unavailable")
		result.Set(nil, cd.NewError(cd.ResourceExhausted, "temporary chat task unavailable"))
		return
	}
	result.Set(events.StartTurnResult{
		TurnID:           started.TurnID,
		Conversation:     started.Conversation,
		UserMessage:      started.UserMessage,
		AssistantMessage: started.AssistantMessage,
	}, nil)
}

func (s *TemporaryChat) buildFeatureMessages(ownerID, conversationID string, detail events.ConversationDetailResult, started store.TurnStart) ([]proxyevents.FeatureTextMessage, error) {
	messages := make([]proxyevents.FeatureTextMessage, 0, len(detail.Messages)+1)
	if strings.TrimSpace(started.SystemPrompt) != "" {
		messages = append(messages, proxyevents.FeatureTextMessage{Role: "system", Content: started.SystemPrompt})
	}
	failedSequences := map[int64]struct{}{}
	for _, message := range detail.Messages {
		if message.ID == started.AssistantMessage.ID {
			continue
		}
		if message.Status == events.MessageStatusError || message.Status == events.MessageStatusCancelled || message.Status == events.MessageStatusInterrupted {
			failedSequences[message.Sequence] = struct{}{}
			if message.Role == "assistant" {
				failedSequences[message.Sequence-1] = struct{}{}
			}
		}
	}
	for _, message := range detail.Messages {
		if message.ID == started.AssistantMessage.ID {
			continue
		}
		if _, failed := failedSequences[message.Sequence]; failed {
			continue
		}
		item := proxyevents.FeatureTextMessage{Role: message.Role, Content: message.Content}
		if message.ID == started.UserMessage.ID {
			item.Images = started.Images
			item.Files = started.Files
		} else if message.Role == "user" {
			for _, image := range message.Images {
				row, found, err := s.store.GetMessageImage(ownerID, conversationID, message.ID, image.ID)
				if err != nil {
					return nil, fmt.Errorf("load historical image %s: %w", image.ID, err)
				}
				if !found {
					return nil, fmt.Errorf("load historical image %s: not found", image.ID)
				}
				item.Images = append(item.Images, row.Bytes)
			}
			for _, attachment := range message.Attachments {
				row, found, err := s.store.GetMessageAttachment(ownerID, conversationID, message.ID, attachment.ID)
				if err != nil {
					return nil, fmt.Errorf("load historical attachment %s: %w", attachment.ID, err)
				}
				if !found {
					return nil, fmt.Errorf("load historical attachment %s: not found", attachment.ID)
				}
				item.Files = append(item.Files, chatattachment.File{Name: row.FileName, ContentType: row.ContentType, Bytes: row.Bytes})
			}
		}
		if strings.TrimSpace(item.Content) == "" && len(item.Images) == 0 && len(item.Files) == 0 {
			continue
		}
		messages = append(messages, item)
	}
	return messages, nil
}

func (s *TemporaryChat) runFeatureTurn(ctx context.Context, runtime *turnRuntime, messages []proxyevents.FeatureTextMessage, thinkingEffort string) {
	defer runtime.requestCancel()
	value, requestErr := s.SendEvent(event.NewEventWithContext(proxyevents.TopicExecuteFeatureText, s.ID(), proxycommon.UnitID, event.NewHeader(), ctx, proxyevents.ExecuteFeatureTextCommand{
		OwnerID:        runtime.ownerID,
		Model:          runtime.model,
		Messages:       messages,
		ThinkingEffort: thinkingEffort,
		WebSearch:      runtime.webSearch,
	})).Get()
	response, ok := value.(proxyevents.ExecuteFeatureTextResult)
	cancelled, stopping := s.turnFlags(runtime)
	errorClass, errorMessage := "", ""
	if cancelled || stopping || ctx.Err() != nil {
		cancelled = true
		if stopping {
			errorClass, errorMessage = "interrupted", "request interrupted by process shutdown"
		} else {
			errorClass, errorMessage = "cancelled", "cancelled by user"
		}
	} else if requestErr != nil || !ok {
		errorClass = strings.TrimSpace(response.ErrorClass)
		if errorClass == "" {
			errorClass = "provider_unavailable"
		}
		errorMessage = safeTurnError(errorClass)
	}
	content, actualModel := response.Text, response.ActualModel
	if response.Provider != "" {
		_ = s.store.SetProvider(runtime.ownerID, runtime.conversationID, response.Provider)
	}
	if errorClass == "" && content != "" {
		_ = s.store.UpdateAssistantDelta(runtime.ownerID, runtime.conversationID, runtime.assistantSequence, content)
		s.publishTurn(runtime, turnUpdate{delta: content, actualModel: actualModel})
	}
	completed, err := s.store.CompleteFeatureTurn(runtime.ownerID, runtime.conversationID, runtime.userSequence, runtime.assistantSequence, content, actualModel, cancelled, errorClass, errorMessage)
	if err != nil {
		s.publishTurn(runtime, turnUpdate{done: true, errorClass: "store", errorMessage: "failed to persist turn"})
	} else {
		message := completed.Message
		s.publishTurn(runtime, turnUpdate{done: true, message: &message, actualModel: actualModel, errorClass: errorClass, errorMessage: errorMessage})
	}
	s.turnMu.Lock()
	delete(s.turns, turnKey(runtime.ownerID, runtime.conversationID, runtime.turnID))
	s.turnMu.Unlock()
}

func (s *TemporaryChat) turnFlags(runtime *turnRuntime) (cancelled, stopping bool) {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	return runtime.cancelRequested, s.stopping
}

func safeTurnError(errorClass string) string {
	switch strings.ToLower(strings.TrimSpace(errorClass)) {
	case "tls":
		return "upstream TLS connection failed"
	case "timeout":
		return "upstream request timed out"
	case "invalid_token":
		return "original account is no longer valid"
	case "rate_limit":
		return "original account is rate limited"
	case "content_policy":
		return "upstream content policy rejected the request"
	case "invalid_request":
		return "text request parameters are unsupported"
	case "text_model_length":
		return "upstream model stopped at its output limit"
	case "text_incomplete":
		return "upstream text completion was not confirmed"
	case "text_protocol":
		return "upstream text protocol or snapshot was inconsistent"
	case "text_budget":
		return "local text processing budget was exceeded"
	case "provider_unavailable":
		return "no compatible provider completed the request"
	default:
		return "upstream request failed"
	}
}

func (s *TemporaryChat) handlePullTurn(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.PullTurnCommand)
	if !ok || cmd.OwnerID == "" || cmd.ConversationID == "" || cmd.TurnID == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid pull turn command"))
		return
	}
	s.turnMu.Lock()
	runtime := s.turns[turnKey(cmd.OwnerID, cmd.ConversationID, cmd.TurnID)]
	s.turnMu.Unlock()
	if runtime == nil {
		// Turn may have completed between polls; surface the latest assistant message.
		detail, err := s.store.GetConversation(cmd.OwnerID, cmd.ConversationID, nil, 200)
		if err != nil {
			result.Set(nil, cd.NewError(cd.Unexpected, "turn not found"))
			return
		}
		for i := len(detail.Messages) - 1; i >= 0; i-- {
			message := detail.Messages[i]
			if message.ID == cmd.TurnID || message.TurnID == cmd.TurnID {
				result.Set(events.PullTurnResult{Done: true, Message: &message, ActualModel: message.ActualModel, ErrorClass: message.ErrorClass, ErrorMessage: message.ErrorMessage}, nil)
				return
			}
		}
		result.Set(nil, cd.NewError(cd.Unexpected, "turn not found"))
		return
	}
	timeout := time.Duration(cmd.TimeoutMillis) * time.Millisecond
	if timeout < 250*time.Millisecond || timeout > 15*time.Second {
		timeout = time.Second
	}
	select {
	case update := <-runtime.updates:
		result.Set(events.PullTurnResult{
			Delta:        update.delta,
			ActualModel:  update.actualModel,
			Done:         update.done,
			Message:      update.message,
			ErrorClass:   update.errorClass,
			ErrorMessage: update.errorMessage,
		}, nil)
	case <-time.After(timeout):
		result.Set(events.PullTurnResult{}, nil)
	}
}

func (s *TemporaryChat) handleCancelTurn(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.CancelTurnCommand)
	if !ok || cmd.OwnerID == "" || cmd.ConversationID == "" || cmd.TurnID == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid cancel turn command"))
		return
	}
	s.turnMu.Lock()
	runtime := s.turns[turnKey(cmd.OwnerID, cmd.ConversationID, cmd.TurnID)]
	if runtime != nil {
		runtime.cancelRequested = true
		requestCancel := runtime.requestCancel
		s.turnMu.Unlock()
		if requestCancel != nil {
			requestCancel()
		}
	} else {
		s.turnMu.Unlock()
	}
	// Wait briefly for the turn worker to publish a terminal update.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.turnMu.Lock()
		_, active := s.turns[turnKey(cmd.OwnerID, cmd.ConversationID, cmd.TurnID)]
		s.turnMu.Unlock()
		if !active {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	detail, err := s.store.GetConversation(cmd.OwnerID, cmd.ConversationID, nil, 200)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	for i := len(detail.Messages) - 1; i >= 0; i-- {
		message := detail.Messages[i]
		if message.ID == cmd.TurnID || message.TurnID == cmd.TurnID {
			result.Set(events.CancelTurnResult{Message: message}, nil)
			return
		}
	}
	result.Set(events.CancelTurnResult{}, nil)
}

func (s *TemporaryChat) handleDelete(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	if s.store == nil {
		result.Set(nil, cd.NewError(cd.Unexpected, "temporary chat unavailable"))
		return
	}
	cmd, ok := ev.Data().(events.DeleteConversationCommand)
	if !ok || cmd.OwnerID == "" || cmd.ConversationID == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid delete conversation command"))
		return
	}
	s.turnMu.Lock()
	for key, runtime := range s.turns {
		if runtime.ownerID == cmd.OwnerID && runtime.conversationID == cmd.ConversationID {
			runtime.cancelRequested = true
			if runtime.requestCancel != nil {
				runtime.requestCancel()
			}
			delete(s.turns, key)
		}
	}
	s.turnMu.Unlock()
	if err := s.store.DeleteConversation(cmd.OwnerID, cmd.ConversationID); err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(events.DeleteConversationResult{Deleted: true}, nil)
}

func (s *TemporaryChat) handleGetImage(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	if s.store == nil {
		result.Set(nil, cd.NewError(cd.Unexpected, "temporary chat unavailable"))
		return
	}
	cmd, ok := ev.Data().(events.GetMessageImageCommand)
	if !ok || strings.TrimSpace(cmd.OwnerID) == "" || strings.TrimSpace(cmd.ConversationID) == "" || strings.TrimSpace(cmd.MessageID) == "" || strings.TrimSpace(cmd.ImageID) == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid temporary message image command"))
		return
	}
	image, found, err := s.store.GetMessageImage(cmd.OwnerID, cmd.ConversationID, cmd.MessageID, cmd.ImageID)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	if !found {
		result.Set(nil, cd.NewError(cd.IllegalParam, "temporary message image not found"))
		return
	}
	result.Set(events.GetMessageImageResult{Bytes: image.Bytes, ContentType: image.ContentType}, nil)
}

func (s *TemporaryChat) handleGetAttachment(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	if s.store == nil {
		result.Set(nil, cd.NewError(cd.Unexpected, "temporary chat unavailable"))
		return
	}
	cmd, ok := ev.Data().(events.GetMessageAttachmentCommand)
	if !ok || strings.TrimSpace(cmd.OwnerID) == "" || strings.TrimSpace(cmd.ConversationID) == "" || strings.TrimSpace(cmd.MessageID) == "" || strings.TrimSpace(cmd.AttachmentID) == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid temporary message attachment command"))
		return
	}
	attachment, found, err := s.store.GetMessageAttachment(cmd.OwnerID, cmd.ConversationID, cmd.MessageID, cmd.AttachmentID)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	if !found {
		result.Set(nil, cd.NewError(cd.IllegalParam, "temporary message attachment not found"))
		return
	}
	result.Set(events.GetMessageAttachmentResult{Bytes: attachment.Bytes, FileName: attachment.FileName, ContentType: attachment.ContentType}, nil)
}

func (s *TemporaryChat) publishTurn(runtime *turnRuntime, update turnUpdate) {
	select {
	case runtime.updates <- update:
	default:
	}
}

func turnKey(ownerID, conversationID, turnID string) string {
	return ownerID + "/" + conversationID + "/" + turnID
}
