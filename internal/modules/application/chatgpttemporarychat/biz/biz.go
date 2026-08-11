package biz

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	acccommon "aetherrelay/internal/modules/application/chatgptaccountpool/pkg/common"
	accevents "aetherrelay/internal/modules/application/chatgptaccountpool/pkg/events"
	"aetherrelay/internal/modules/application/chatgpttemporarychat/internal/store"
	"aetherrelay/internal/modules/application/chatgpttemporarychat/pkg/common"
	events "aetherrelay/internal/modules/application/chatgpttemporarychat/pkg/events"
	proxycommon "aetherrelay/internal/modules/application/proxyapi/pkg/common"
	proxyevents "aetherrelay/internal/modules/application/proxyapi/pkg/events"
	basebiz "aetherrelay/internal/modules/base/biz"
	upcommon "aetherrelay/internal/modules/blocks/chatgptwebupstream/pkg/common"
	upevents "aetherrelay/internal/modules/blocks/chatgptwebupstream/pkg/events"
	configevents "aetherrelay/internal/modules/blocks/configruntime/pkg/events"
	usageevents "aetherrelay/internal/modules/blocks/usageruntime/pkg/events"
	"aetherrelay/internal/pkg/aetherrelayconfig"
	"aetherrelay/internal/pkg/aetherrelayusage"
	"aetherrelay/internal/pkg/chatattachment"
	"aetherrelay/internal/pkg/chatgpttokenusage"
	"github.com/google/uuid"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

type TemporaryChat struct {
	basebiz.Base
	store  *store.Store
	usage  usage.Store
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
	streamID          string
	startCommand      upevents.StartTextCommand
	refreshRetried    bool
	content           string
	actualModel       string
	usageEventID      string
	model             string
	systemPrompt      string
	userContent       string
	firstTurn         bool
	webSearch         bool
	startedAt         time.Time
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
	usageStore, usageErr := usageevents.RequestStore(ctx, b.EventHub(), b.ID())
	if usageErr != nil {
		_ = b.store.Close()
		b.store = nil
		return nil, cd.NewError(cd.Unexpected, "usage store unavailable: "+usageErr.Error())
	}
	b.usage = usageStore
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
	b.AsyncTask(func() {
		defer b.purgeWG.Done()
		b.purgeLoop()
	})
	return b, nil
}

func (s *TemporaryChat) Run(context.Context) *cd.Error { return nil }

func (s *TemporaryChat) Teardown(context.Context) {
	for _, topic := range s.topics {
		s.UnsubscribeFunc(topic)
	}
	s.turnMu.Lock()
	s.stopping = true
	streamIDs := make([]string, 0, len(s.turns))
	requestCancels := make([]context.CancelFunc, 0, len(s.turns))
	for _, turn := range s.turns {
		if turn.streamID != "" {
			streamIDs = append(streamIDs, turn.streamID)
		}
		if turn.requestCancel != nil {
			requestCancels = append(requestCancels, turn.requestCancel)
		}
	}
	s.turnMu.Unlock()
	s.stopOnce.Do(func() { close(s.stopCh) })
	for _, streamID := range streamIDs {
		s.cancelUpstream(streamID)
	}
	for _, cancel := range requestCancels {
		cancel()
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
		_ = s.store.Close()
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
	view, err := s.store.CreateConversation(cmd.OwnerID, cmd.Model, cmd.ThinkingEffort, cmd.SystemPrompt, provider, "")
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
	if started.AccountID != "" {
		if cmd.WebSearch {
			_, _ = s.store.CompleteFeatureTurn(cmd.OwnerID, cmd.ConversationID, started.UserSequence, started.AssistantSequence, "", "", false, "conversion_unsupported", "web search is unavailable for legacy account-pinned conversations")
			result.Set(nil, cd.NewError(cd.IllegalParam, "web search is unavailable for legacy account-pinned conversations; create a new conversation"))
			return
		}
		s.startLegacyTurn(cmd, started, result)
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
		systemPrompt:      started.SystemPrompt,
		userContent:       cmd.Content,
		webSearch:         cmd.WebSearch,
		startedAt:         time.Now().UTC(),
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
	s.AsyncTask(func() {
		defer s.turnWG.Done()
		s.runFeatureTurn(requestCtx, runtime, messages, started.ThinkingEffort)
	})
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

// startLegacyTurn preserves continuation semantics for conversations created
// before feature routing became Provider-neutral. New conversations never pin
// an account and therefore never enter this compatibility path.
func (s *TemporaryChat) startLegacyTurn(cmd events.StartTurnCommand, started store.TurnStart, result event.Result) {
	eventID := uuid.NewString()
	startedAt := time.Now().UTC()
	if err := s.startTurnUsage(cmd.OwnerID, eventID, started.Model, startedAt); err != nil {
		_, _ = s.store.CompleteTurn(cmd.OwnerID, cmd.ConversationID, started.UserSequence, started.AssistantSequence, "", "", "", "", false, false, false, "usage_unavailable", "usage store unavailable")
		result.Set(nil, cd.NewError(cd.Unexpected, "usage store unavailable"))
		return
	}
	account, err := s.acquireTextAccount(started.AccountID, started.Model)
	if err != nil {
		_, _ = s.store.CompleteTurn(cmd.OwnerID, cmd.ConversationID, started.UserSequence, started.AssistantSequence, "", "", "", "", false, false, false, "provider_unavailable", "original account unavailable")
		s.completeTurnUsage(eventID, started.Model, "", cmd.OwnerID, started.SystemPrompt, cmd.Content, started.UpstreamConversationID == "", "", httpStatusServiceUnavailable, "upstream_failed", "provider_unavailable", startedAt, true)
		result.Set(nil, cd.NewError(cd.Unexpected, "original account unavailable; create a new conversation"))
		return
	}
	messages := make([]upevents.TextMessage, 0, 2)
	firstTurn := started.UpstreamConversationID == ""
	if firstTurn && strings.TrimSpace(started.SystemPrompt) != "" {
		messages = append(messages, upevents.TextMessage{Role: "system", Content: started.SystemPrompt})
	}
	messages = append(messages, upevents.TextMessage{Role: "user", Content: cmd.Content, Images: started.Images, Files: started.Files})
	startCommand := upevents.StartTextCommand{
		AccessToken: account.AccessToken, Proxy: account.Account.Proxy, Model: started.Model, Messages: messages,
		ThinkingEffort: started.ThinkingEffort, ConversationID: started.UpstreamConversationID, ParentMessageID: started.ParentMessageID,
		TimeoutMillis: int(s.turnTimeout / time.Millisecond),
	}
	streamID, streamErr := s.startTextStream(startCommand)
	if streamErr != nil {
		_, _ = s.store.CompleteTurn(cmd.OwnerID, cmd.ConversationID, started.UserSequence, started.AssistantSequence, "", "", "", "", false, false, false, "upstream", "failed to start upstream stream")
		s.recordTextResult(started.AccountID, false, "upstream")
		s.completeTurnUsage(eventID, started.Model, "", cmd.OwnerID, started.SystemPrompt, cmd.Content, firstTurn, "", httpStatusBadGateway, "upstream_failed", "upstream", startedAt, true)
		result.Set(nil, cd.NewError(cd.Unexpected, "failed to start upstream stream"))
		return
	}
	runtime := &turnRuntime{
		ownerID: cmd.OwnerID, conversationID: cmd.ConversationID, turnID: started.TurnID,
		userSequence: started.UserSequence, assistantSequence: started.AssistantSequence,
		streamID: streamID, startCommand: startCommand, usageEventID: eventID, model: started.Model,
		systemPrompt: started.SystemPrompt, userContent: cmd.Content, firstTurn: firstTurn, startedAt: startedAt,
		updates: make(chan turnUpdate, 64),
	}
	s.turnMu.Lock()
	if s.stopping {
		s.turnMu.Unlock()
		s.cancelUpstream(streamID)
		_, _ = s.store.CompleteTurn(cmd.OwnerID, cmd.ConversationID, started.UserSequence, started.AssistantSequence, "", "", "", "", false, true, true, "interrupted", "stream interrupted by process shutdown")
		s.completeTurnUsage(eventID, started.Model, "", cmd.OwnerID, started.SystemPrompt, cmd.Content, firstTurn, "", httpStatusServiceUnavailable, "process_interrupted", "process_interrupted", startedAt, true)
		result.Set(nil, cd.NewError(cd.Unexpected, "temporary chat unavailable"))
		return
	}
	s.turns[turnKey(cmd.OwnerID, cmd.ConversationID, started.TurnID)] = runtime
	s.turnWG.Add(1)
	s.turnMu.Unlock()
	s.AsyncTask(func() {
		defer s.turnWG.Done()
		s.runTurn(runtime, started.AccountID)
	})
	result.Set(events.StartTurnResult{TurnID: started.TurnID, Conversation: started.Conversation, UserMessage: started.UserMessage, AssistantMessage: started.AssistantMessage}, nil)
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

func (s *TemporaryChat) startTextStream(command upevents.StartTextCommand) (string, error) {
	value, err := s.SendEvent(event.NewEvent(upevents.TopicStartText, s.ID(), upcommon.UnitID, nil, command)).Get()
	if err != nil {
		return "", fmt.Errorf("start upstream text stream: %w", err)
	}
	started, ok := value.(upevents.StartTextResult)
	if !ok || strings.TrimSpace(started.StreamID) == "" {
		return "", fmt.Errorf("invalid upstream stream")
	}
	return started.StreamID, nil
}

func (s *TemporaryChat) runTurn(runtime *turnRuntime, accountID string) {
	var (
		finalConversationID string
		finalAssistantID    string
		finalErrClass       string
		finalErrMessage     string
		interrupted         bool
		recoveryRequired    bool
	)
	for {
		cancelled, stopping := s.turnFlags(runtime)
		if cancelled {
			s.cancelUpstream(runtime.streamID)
			if stopping {
				interrupted, recoveryRequired = true, true
				finalErrClass, finalErrMessage = "interrupted", "stream interrupted by process shutdown"
			} else {
				finalErrClass, finalErrMessage = "cancelled", "cancelled by user"
			}
			break
		}
		value, err := s.SendEvent(event.NewEvent(upevents.TopicPullText, s.ID(), upcommon.UnitID, nil, upevents.PullTextCommand{
			StreamID:      runtime.streamID,
			TimeoutMillis: 1000,
		})).Get()
		if err != nil {
			cancelled, stopping := s.turnFlags(runtime)
			if cancelled && !stopping {
				finalErrClass, finalErrMessage = "cancelled", "cancelled by user"
			} else if stopping {
				interrupted, recoveryRequired = true, true
				finalErrClass, finalErrMessage = "interrupted", "stream interrupted by process shutdown"
			} else {
				s.cancelUpstream(runtime.streamID)
				finalErrClass, finalErrMessage = "upstream", "upstream stream interrupted"
				interrupted, recoveryRequired = true, true
			}
			break
		}
		update, ok := value.(upevents.PullTextResult)
		if !ok {
			s.cancelUpstream(runtime.streamID)
			finalErrClass, finalErrMessage = "upstream", "invalid upstream stream result"
			interrupted, recoveryRequired = true, true
			break
		}
		if update.ConversationID != "" {
			finalConversationID = update.ConversationID
		}
		if update.AssistantMessageID != "" {
			finalAssistantID = update.AssistantMessageID
		}
		if update.ActualModel != "" {
			runtime.actualModel = update.ActualModel
		}
		if update.Delta != "" {
			runtime.content += update.Delta
			_ = s.store.UpdateAssistantDelta(runtime.ownerID, runtime.conversationID, runtime.assistantSequence, runtime.content)
			s.publishTurn(runtime, turnUpdate{delta: update.Delta, actualModel: runtime.actualModel})
		}
		if update.Done {
			if update.ErrorClass != "" {
				if update.ErrorClass == upevents.ErrClassInvalidToken && runtime.content == "" && !runtime.refreshRetried {
					if restarted, retryFailure := s.retryTurnAfterInvalidToken(runtime); restarted {
						// No assistant delta was exposed, so the caller can safely
						// resume this same turn with the rotated access token.
						continue
					} else if retryFailure != "" {
						finalErrClass = string(retryFailure)
						finalErrMessage = safeTurnError(finalErrClass)
						recoveryRequired = requiresRecovery(finalErrClass)
						interrupted = recoveryRequired
						break
					}
				}
				finalErrClass = string(update.ErrorClass)
				finalErrMessage = safeTurnError(finalErrClass)
				recoveryRequired = requiresRecovery(finalErrClass)
				interrupted = recoveryRequired
			}
			if update.ConversationID != "" {
				finalConversationID = update.ConversationID
			}
			if update.AssistantMessageID != "" {
				finalAssistantID = update.AssistantMessageID
			}
			break
		}
	}
	cancelled := finalErrClass == "cancelled"
	completed, err := s.store.CompleteTurn(
		runtime.ownerID,
		runtime.conversationID,
		runtime.userSequence,
		runtime.assistantSequence,
		runtime.content,
		runtime.actualModel,
		finalConversationID,
		finalAssistantID,
		cancelled,
		interrupted,
		recoveryRequired,
		finalErrClass,
		finalErrMessage,
	)
	if err != nil {
		slog.Warn("temporary chat complete turn failed", "error_class", "store")
		s.publishTurn(runtime, turnUpdate{done: true, errorClass: "store", errorMessage: "failed to persist turn"})
	} else {
		message := completed.Message
		s.publishTurn(runtime, turnUpdate{
			done:         true,
			message:      &message,
			errorClass:   finalErrClass,
			errorMessage: finalErrMessage,
		})
	}
	if !cancelled && !interrupted {
		s.recordTextResult(accountID, finalErrClass == "", finalErrClass)
	}
	outcome, errorCode := usageOutcomeFromTurn(finalErrClass, cancelled, interrupted)
	actual := runtime.actualModel
	if err == nil && completed.Message.ActualModel != "" {
		actual = completed.Message.ActualModel
	}
	s.completeTurnUsage(runtime.usageEventID, runtime.model, actual, runtime.ownerID, runtime.systemPrompt, runtime.userContent, runtime.firstTurn, runtime.content, httpStatusAccepted, outcome, errorCode, runtime.startedAt, true)
	s.turnMu.Lock()
	delete(s.turns, turnKey(runtime.ownerID, runtime.conversationID, runtime.turnID))
	s.turnMu.Unlock()
}

// retryTurnAfterInvalidToken refreshes once before the first assistant delta.
// Reusing the persisted conversation/parent IDs preserves temporary-chat
// continuity while avoiding a duplicate visible assistant response.
func (s *TemporaryChat) retryTurnAfterInvalidToken(runtime *turnRuntime) (bool, upevents.ErrorClass) {
	runtime.refreshRetried = true
	previousStreamID := runtime.streamID
	s.cancelUpstream(previousStreamID)

	value, err := s.SendEvent(event.NewEvent(accevents.TopicRefreshTextToken, s.ID(), acccommon.UnitID, nil, accevents.RefreshTextTokenCommand{
		AccessToken: runtime.startCommand.AccessToken,
	})).Get()
	if err != nil {
		return false, upevents.ErrClassUpstream
	}
	refreshed, ok := value.(accevents.RefreshTextTokenResult)
	if !ok {
		return false, upevents.ErrClassUpstream
	}
	if !refreshed.Refreshed || strings.TrimSpace(refreshed.AccessToken) == "" {
		if refreshed.PermanentFailure {
			return false, upevents.ErrClassInvalidToken
		}
		return false, upevents.ErrClassUpstream
	}
	command := runtime.startCommand
	command.AccessToken = refreshed.AccessToken
	command.Proxy = refreshed.Account.Proxy
	streamID, startErr := s.startTextStream(command)
	if startErr != nil {
		return false, upevents.ErrClassUpstream
	}
	s.turnMu.Lock()
	if runtime.cancelRequested || s.stopping {
		s.turnMu.Unlock()
		s.cancelUpstream(streamID)
		return false, upevents.ErrClassUpstream
	}
	runtime.streamID = streamID
	runtime.startCommand = command
	s.turnMu.Unlock()
	return true, ""
}

func (s *TemporaryChat) turnFlags(runtime *turnRuntime) (cancelled, stopping bool) {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	return runtime.cancelRequested, s.stopping
}

func requiresRecovery(errorClass string) bool {
	switch strings.ToLower(strings.TrimSpace(errorClass)) {
	case "tls", "timeout", "upstream", "interrupted":
		return true
	default:
		return false
	}
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
		streamID := runtime.streamID
		requestCancel := runtime.requestCancel
		s.turnMu.Unlock()
		s.cancelUpstream(streamID)
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
			s.cancelUpstream(runtime.streamID)
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

func (s *TemporaryChat) acquireTextAccount(accountID, model string) (accevents.AcquireTextAccountResult, error) {
	if accountID != "" {
		value, err := s.SendEvent(event.NewEvent(accevents.TopicAcquireTextAccount, s.ID(), acccommon.UnitID, nil, accevents.AcquireTextAccountCommand{
			AccountID:  accountID,
			Model:      model,
			Capability: accevents.ModelCapabilityTextGeneration,
		})).Get()
		if err != nil {
			return accevents.AcquireTextAccountResult{}, fmt.Errorf("original account unavailable")
		}
		result, ok := value.(accevents.AcquireTextAccountResult)
		if !ok || result.AccessToken == "" {
			return accevents.AcquireTextAccountResult{}, fmt.Errorf("original account unavailable")
		}
		return result, nil
	}
	value, err := s.SendEvent(event.NewEvent(accevents.TopicAcquireTextToken, s.ID(), acccommon.UnitID, nil, accevents.AcquireTextTokenCommand{
		Model:      model,
		Capability: accevents.ModelCapabilityTextGeneration,
	})).Get()
	if err != nil {
		return accevents.AcquireTextAccountResult{}, fmt.Errorf("no available text account")
	}
	tokenResult, ok := value.(accevents.AcquireTextTokenResult)
	if !ok || tokenResult.AccessToken == "" {
		return accevents.AcquireTextAccountResult{}, fmt.Errorf("no available text account")
	}
	return accevents.AcquireTextAccountResult{AccessToken: tokenResult.AccessToken, Account: tokenResult.Account}, nil
}

func (s *TemporaryChat) recordTextResult(accountID string, success bool, errorClass string) {
	if accountID == "" {
		return
	}
	_, _ = s.SendEvent(event.NewEvent(accevents.TopicRecordTextResult, s.ID(), acccommon.UnitID, nil, accevents.RecordTextResultCommand{
		AccountID:  accountID,
		Success:    success,
		ErrorClass: errorClass,
	})).Get()
}

func (s *TemporaryChat) cancelUpstream(streamID string) {
	if streamID == "" {
		return
	}
	_, _ = s.SendEvent(event.NewEvent(upevents.TopicCancelText, s.ID(), upcommon.UnitID, nil, upevents.CancelTextCommand{StreamID: streamID})).Get()
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

const (
	httpStatusAccepted           = 202
	httpStatusBadGateway         = 502
	httpStatusServiceUnavailable = 503
)

func (s *TemporaryChat) startTurnUsage(ownerID, eventID, model string, startedAt time.Time) error {
	if s == nil || s.usage == nil {
		return fmt.Errorf("usage store unavailable")
	}
	return s.usage.Start(context.Background(), usage.StartRecord{
		EventID:        eventID,
		StartedAt:      startedAt,
		APIKeyID:       config.BuiltinClientAPIKeyID,
		Operation:      "text_generation",
		Route:          "admin_temporary_chat",
		ClientEndpoint: "/admin/chatgpt/temporary-chat",
		ClientProtocol: "admin",
		Provider:       "chatgptweb",
		Model:          model,
	})
}

func (s *TemporaryChat) completeTurnUsage(eventID, model, actualModel, ownerID, systemPrompt, userContent string, firstTurn bool, completion string, httpStatus int, outcome, errorCode string, startedAt time.Time, stream bool) {
	if s == nil || s.usage == nil || strings.TrimSpace(eventID) == "" {
		return
	}
	parts := make([]string, 0, 2)
	if firstTurn && strings.TrimSpace(systemPrompt) != "" {
		parts = append(parts, systemPrompt)
	}
	parts = append(parts, userContent)
	est := tokenusage.EstimateChatTextUsage(parts, completion)
	prompt, out := 0, 0
	if est != nil {
		prompt, out = est.PromptTokens, est.CompletionTokens
	}
	billingModel := strings.TrimSpace(actualModel)
	if billingModel == "" {
		billingModel = model
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.usage.Complete(ctx, usage.CompleteRecord{
		EventID:          eventID,
		CompletedAt:      time.Now().UTC(),
		Provider:         "chatgptweb",
		Model:            billingModel,
		UpstreamProtocol: "chatgptweb",
		UpstreamEndpoint: "chatgptweb_temporary_chat",
		ConversionMode:   "native",
		InputTokens:      int64(prompt),
		OutputTokens:     int64(out),
		HTTPStatus:       httpStatus,
		Outcome:          outcome,
		ErrorCode:        errorCode,
		Duration:         time.Since(startedAt),
		Stream:           stream,
		Estimated:        true,
	}); err != nil {
		slog.Warn("temporary chat usage complete failed", "event_id", eventID, "error", err)
	}
}

func usageOutcomeFromTurn(errorClass string, cancelled, interrupted bool) (outcome, errorCode string) {
	if cancelled {
		return "client_canceled", "client_canceled"
	}
	if interrupted || errorClass == "interrupted" {
		return "process_interrupted", "process_interrupted"
	}
	if errorClass == "" {
		return "success", ""
	}
	switch strings.ToLower(strings.TrimSpace(errorClass)) {
	case "invalid_token", "rate_limit", "content_policy", "tls", "timeout", "upstream", "provider_unavailable":
		return "upstream_failed", strings.ToLower(strings.TrimSpace(errorClass))
	default:
		return "upstream_failed", "upstream"
	}
}
