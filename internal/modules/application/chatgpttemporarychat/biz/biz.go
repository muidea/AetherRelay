package biz

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	acccommon "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/common"
	accevents "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/events"
	"ai-proxy/internal/modules/application/chatgpttemporarychat/internal/store"
	"ai-proxy/internal/modules/application/chatgpttemporarychat/pkg/common"
	events "ai-proxy/internal/modules/application/chatgpttemporarychat/pkg/events"
	basebiz "ai-proxy/internal/modules/base/biz"
	upcommon "ai-proxy/internal/modules/blocks/chatgptwebupstream/pkg/common"
	upevents "ai-proxy/internal/modules/blocks/chatgptwebupstream/pkg/events"
	configevents "ai-proxy/internal/modules/blocks/configruntime/pkg/events"
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
	streamID          string
	content           string
	done              bool
	errorClass        string
	errorMessage      string
	cancelRequested   bool
	updates           chan turnUpdate
}

type turnUpdate struct {
	delta        string
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
	if !bootstrap.Config.ChatGPTWeb.Enabled || !bootstrap.Config.ChatGPTWeb.TemporaryChat.Enabled {
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
	}
	b.SubscribeFunc(events.TopicCreate, b.handleCreate)
	b.SubscribeFunc(events.TopicList, b.handleList)
	b.SubscribeFunc(events.TopicGet, b.handleGet)
	b.SubscribeFunc(events.TopicStartTurn, b.handleStartTurn)
	b.SubscribeFunc(events.TopicPullTurn, b.handlePullTurn)
	b.SubscribeFunc(events.TopicCancelTurn, b.handleCancelTurn)
	b.SubscribeFunc(events.TopicDelete, b.handleDelete)
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
	for _, turn := range s.turns {
		if turn.streamID != "" {
			streamIDs = append(streamIDs, turn.streamID)
		}
	}
	s.turnMu.Unlock()
	s.stopOnce.Do(func() { close(s.stopCh) })
	for _, streamID := range streamIDs {
		s.cancelUpstream(streamID)
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
	account, err := s.acquireTextAccount("", cmd.Model)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	view, err := s.store.CreateConversation(cmd.OwnerID, cmd.Model, cmd.ThinkingEffort, cmd.SystemPrompt, account.Account.ID)
	if err != nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, err.Error()))
		return
	}
	view.AccountDisplay = maskAccount(account.Account)
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
	s.turnMu.Lock()
	stopping := s.stopping
	s.turnMu.Unlock()
	if stopping {
		result.Set(nil, cd.NewError(cd.Unexpected, "temporary chat unavailable"))
		return
	}
	started, err := s.store.StartTurn(cmd.OwnerID, cmd.ConversationID, cmd.Content)
	if err != nil {
		if strings.Contains(err.Error(), "streaming") || strings.Contains(err.Error(), "recovery") {
			result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
			return
		}
		result.Set(nil, cd.NewError(cd.IllegalParam, err.Error()))
		return
	}
	account, err := s.acquireTextAccount(started.AccountID, started.Model)
	if err != nil {
		_, _ = s.store.CompleteTurn(cmd.OwnerID, cmd.ConversationID, started.UserSequence, started.AssistantSequence, "", "", "", false, false, false, "provider_unavailable", "original account unavailable")
		result.Set(nil, cd.NewError(cd.Unexpected, "original account unavailable; create a new conversation"))
		return
	}
	messages := make([]upevents.TextMessage, 0, 2)
	if started.UpstreamConversationID == "" && strings.TrimSpace(started.SystemPrompt) != "" {
		messages = append(messages, upevents.TextMessage{Role: "system", Content: started.SystemPrompt})
	}
	messages = append(messages, upevents.TextMessage{Role: "user", Content: cmd.Content})
	streamValue, streamErr := s.SendEvent(event.NewEvent(upevents.TopicStartText, s.ID(), upcommon.UnitID, nil, upevents.StartTextCommand{
		AccessToken:     account.AccessToken,
		Model:           started.Model,
		Messages:        messages,
		ThinkingEffort:  started.ThinkingEffort,
		ConversationID:  started.UpstreamConversationID,
		ParentMessageID: started.ParentMessageID,
		TimeoutMillis:   int(s.turnTimeout / time.Millisecond),
	})).Get()
	if streamErr != nil {
		_, _ = s.store.CompleteTurn(cmd.OwnerID, cmd.ConversationID, started.UserSequence, started.AssistantSequence, "", "", "", false, false, false, "upstream", "failed to start upstream stream")
		s.recordTextResult(started.AccountID, false, "upstream")
		result.Set(nil, cd.NewError(cd.Unexpected, "failed to start upstream stream"))
		return
	}
	startedStream, ok := streamValue.(upevents.StartTextResult)
	if !ok || startedStream.StreamID == "" {
		_, _ = s.store.CompleteTurn(cmd.OwnerID, cmd.ConversationID, started.UserSequence, started.AssistantSequence, "", "", "", false, false, false, "upstream", "invalid upstream stream")
		result.Set(nil, cd.NewError(cd.Unexpected, "invalid upstream stream"))
		return
	}
	runtime := &turnRuntime{
		ownerID:           cmd.OwnerID,
		conversationID:    cmd.ConversationID,
		turnID:            started.TurnID,
		userSequence:      started.UserSequence,
		assistantSequence: started.AssistantSequence,
		streamID:          startedStream.StreamID,
		updates:           make(chan turnUpdate, 64),
	}
	s.turnMu.Lock()
	if s.stopping {
		s.turnMu.Unlock()
		s.cancelUpstream(startedStream.StreamID)
		_, _ = s.store.CompleteTurn(cmd.OwnerID, cmd.ConversationID, started.UserSequence, started.AssistantSequence, "", "", "", false, true, true, "interrupted", "stream interrupted by process shutdown")
		result.Set(nil, cd.NewError(cd.Unexpected, "temporary chat unavailable"))
		return
	}
	s.turns[turnKey(cmd.OwnerID, cmd.ConversationID, started.TurnID)] = runtime
	s.turnWG.Add(1)
	s.turnMu.Unlock()
	accountID := started.AccountID
	s.AsyncTask(func() {
		defer s.turnWG.Done()
		s.runTurn(runtime, accountID)
	})
	result.Set(events.StartTurnResult{
		TurnID:           started.TurnID,
		Conversation:     started.Conversation,
		UserMessage:      started.UserMessage,
		AssistantMessage: started.AssistantMessage,
	}, nil)
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
		if update.Delta != "" {
			runtime.content += update.Delta
			_ = s.store.UpdateAssistantDelta(runtime.ownerID, runtime.conversationID, runtime.assistantSequence, runtime.content)
			s.publishTurn(runtime, turnUpdate{delta: update.Delta})
		}
		if update.Done {
			if update.ErrorClass != "" {
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
	s.turnMu.Lock()
	delete(s.turns, turnKey(runtime.ownerID, runtime.conversationID, runtime.turnID))
	s.turnMu.Unlock()
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
				result.Set(events.PullTurnResult{Done: true, Message: &message, ErrorClass: message.ErrorClass, ErrorMessage: message.ErrorMessage}, nil)
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
		s.turnMu.Unlock()
		s.cancelUpstream(streamID)
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

func (s *TemporaryChat) acquireTextAccount(accountID, model string) (accevents.AcquireTextAccountResult, error) {
	if accountID != "" {
		value, err := s.SendEvent(event.NewEvent(accevents.TopicAcquireTextAccount, s.ID(), acccommon.UnitID, nil, accevents.AcquireTextAccountCommand{
			AccountID: accountID,
			Model:     model,
			Operation: accevents.ModelOperationChatCompletions,
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
		Model:     model,
		Operation: accevents.ModelOperationChatCompletions,
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

func maskAccount(account accevents.AccountView) string {
	if email := strings.TrimSpace(account.Email); email != "" {
		parts := strings.Split(email, "@")
		if len(parts) == 2 && len(parts[0]) > 2 {
			return parts[0][:2] + "***@" + parts[1]
		}
		return email
	}
	id := strings.TrimSpace(account.ID)
	if len(id) <= 8 {
		return id
	}
	return id[:4] + "…" + id[len(id)-4:]
}
