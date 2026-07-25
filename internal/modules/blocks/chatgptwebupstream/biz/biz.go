package biz

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	basebiz "ai-proxy/internal/modules/base/biz"
	upclient "ai-proxy/internal/modules/blocks/chatgptwebupstream/internal/client"
	"ai-proxy/internal/modules/blocks/chatgptwebupstream/pkg/common"
	events "ai-proxy/internal/modules/blocks/chatgptwebupstream/pkg/events"
	configevents "ai-proxy/internal/modules/blocks/configruntime/pkg/events"
	"github.com/google/uuid"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

// Upstream owns ChatGPT Web reverse-client capability. Authenticated account
// inspection and image transport are implemented behind the real-token PoC
// gate; production registration remains deferred until live acceptance passes.
type Upstream struct {
	basebiz.Base
	topics   []string
	streamMu sync.Mutex
	streams  map[string]*textStream
}

type textStream struct {
	cancel  context.CancelFunc
	updates chan textStreamUpdate
}

type textStreamUpdate struct {
	delta      string
	done       bool
	errClass   events.ErrorClass
	errMessage string
}

func New(ctx context.Context, hub event.Hub, background task.BackgroundRoutine) (*Upstream, *cd.Error) {
	b := &Upstream{
		Base:    basebiz.New(common.UnitID, hub, background),
		streams: map[string]*textStream{},
	}
	bootstrap, err := configevents.RequestBootstrap(ctx, b.EventHub(), b.ID())
	if err != nil {
		return nil, cd.NewError(cd.IllegalParam, err.Error())
	}
	if !bootstrap.Config.ChatGPTWeb.Enabled {
		return b, nil
	}
	b.topics = []string{
		events.TopicGetUserInfo,
		events.TopicGenerateImage,
		events.TopicEditImage,
		events.TopicResumeImage,
		events.TopicCompleteText,
		events.TopicStartText,
		events.TopicPullText,
		events.TopicCancelText,
	}
	b.SubscribeFunc(events.TopicGetUserInfo, b.handleGetUserInfo)
	b.SubscribeFunc(events.TopicGenerateImage, b.handleGenerateImage)
	b.SubscribeFunc(events.TopicEditImage, b.handleEditImage)
	b.SubscribeFunc(events.TopicResumeImage, b.handleResumeImage)
	b.SubscribeFunc(events.TopicCompleteText, b.handleCompleteText)
	b.SubscribeFunc(events.TopicStartText, b.handleStartText)
	b.SubscribeFunc(events.TopicPullText, b.handlePullText)
	b.SubscribeFunc(events.TopicCancelText, b.handleCancelText)
	return b, nil
}

func (s *Upstream) Run(context.Context) *cd.Error { return nil }

func (s *Upstream) Teardown(context.Context) {
	for _, topic := range s.topics {
		s.UnsubscribeFunc(topic)
	}
	s.streamMu.Lock()
	for _, stream := range s.streams {
		stream.cancel()
	}
	s.streams = map[string]*textStream{}
	s.streamMu.Unlock()
}

func (s *Upstream) handleGetUserInfo(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.GetUserInfoCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid get user info command"))
		return
	}
	client, err := upclient.New(upclient.Config{AccessToken: cmd.AccessToken})
	if err != nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, err.Error()))
		return
	}
	info, err := client.GetUserInfo()
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(events.GetUserInfoResult{Email: info.Email, PlanType: info.PlanType, Quota: info.Quota, RestoreAt: info.RestoreAt}, nil)
}

func (s *Upstream) handleGenerateImage(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.GenerateImageCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid generate image command"))
		return
	}
	slog.Debug("chatgpt web image generation received")
	client, err := upclient.New(upclient.Config{AccessToken: cmd.AccessToken})
	if err != nil {
		slog.Warn("chatgpt web image generation failed", "stage", "create_client")
		result.Set(nil, cd.NewError(cd.IllegalParam, err.Error()))
		return
	}
	generated, err := client.GenerateImage(context.Background(), upclient.ImageRequest{Prompt: cmd.Prompt, Model: cmd.Model, Size: cmd.Size, Quality: cmd.Quality}, upclient.ImagePollOptions{})
	if err != nil {
		slog.Warn("chatgpt web image generation failed", "error_class", classifyError(err))
		result.Set(events.GenerateImageResult{ConversationID: generated.ConversationID}, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	images := make([]events.ImageOutput, 0, len(generated.Images))
	for _, image := range generated.Images {
		images = append(images, events.ImageOutput{Bytes: image.Bytes, URL: image.URL, RevisedPrompt: generated.RevisedPrompt, ConversationID: generated.ConversationID})
	}
	result.Set(events.GenerateImageResult{Images: images, ConversationID: generated.ConversationID}, nil)
}

func (s *Upstream) handleEditImage(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.EditImageCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid edit image command"))
		return
	}
	slog.Debug("chatgpt web image edit received")
	client, err := upclient.New(upclient.Config{AccessToken: cmd.AccessToken})
	if err != nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, err.Error()))
		return
	}
	references, err := client.UploadImageReferences(cmd.Images)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	generated, err := client.GenerateImage(context.Background(), upclient.ImageRequest{Prompt: cmd.Prompt, Model: cmd.Model, Size: cmd.Size, Quality: cmd.Quality, References: references}, upclient.ImagePollOptions{})
	if err != nil {
		slog.Warn("chatgpt web image edit failed", "error_class", classifyError(err))
		result.Set(events.EditImageResult{ConversationID: generated.ConversationID}, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	images := make([]events.ImageOutput, 0, len(generated.Images))
	for _, image := range generated.Images {
		images = append(images, events.ImageOutput{Bytes: image.Bytes, URL: image.URL, RevisedPrompt: generated.RevisedPrompt, ConversationID: generated.ConversationID})
	}
	result.Set(events.EditImageResult{Images: images, ConversationID: generated.ConversationID}, nil)
}

func (s *Upstream) handleResumeImage(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.ResumeImageCommand)
	if !ok || cmd.ConversationID == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid resume image command"))
		return
	}
	client, err := upclient.New(upclient.Config{AccessToken: cmd.AccessToken})
	if err != nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, err.Error()))
		return
	}
	timeout := time.Duration(cmd.ExtraTimeoutSecs) * time.Second
	resumed, err := client.ResumeImage(context.Background(), cmd.ConversationID, timeout)
	if err != nil {
		result.Set(events.ResumeImageResult{ConversationID: resumed.ConversationID}, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	images := make([]events.ImageOutput, 0, len(resumed.Images))
	for _, image := range resumed.Images {
		images = append(images, events.ImageOutput{Bytes: image.Bytes, URL: image.URL, RevisedPrompt: resumed.RevisedPrompt, ConversationID: resumed.ConversationID})
	}
	result.Set(events.ResumeImageResult{Images: images, ConversationID: resumed.ConversationID}, nil)
}

func (s *Upstream) handleCompleteText(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.CompleteTextCommand)
	if !ok || len(cmd.Messages) == 0 {
		slog.Warn("chatgpt web text completion rejected", "stage", "validate_command")
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid complete text command"))
		return
	}
	client, err := upclient.New(upclient.Config{AccessToken: cmd.AccessToken})
	if err != nil {
		slog.Warn("chatgpt web text completion failed", "stage", "create_client")
		result.Set(nil, cd.NewError(cd.IllegalParam, err.Error()))
		return
	}
	messages := make([]upclient.TextMessage, 0, len(cmd.Messages))
	for _, message := range cmd.Messages {
		messages = append(messages, upclient.TextMessage{Role: message.Role, Content: message.Content, Images: message.Images})
	}
	completed, err := s.completeText(client, upclient.TextRequest{Model: cmd.Model, Messages: messages, ThinkingEffort: cmd.ThinkingEffort})
	if err != nil {
		class := classifyError(err)
		slog.Warn("chatgpt web text completion failed", "error_class", class)
		result.Set(events.CompleteTextResult{ConversationID: completed.ConversationID, ErrorClass: class}, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(events.CompleteTextResult{ConversationID: completed.ConversationID, Text: completed.Text}, nil)
}

// completeText converts an unexpected transport-library panic into the same
// classified failure boundary as a returned upstream error. It deliberately
// records no panic value because it can originate in a third-party transport.
func (s *Upstream) completeText(client *upclient.Client, request upclient.TextRequest) (completed upclient.TextResult, err error) {
	defer func() {
		if recover() != nil {
			slog.Warn("chatgpt web text completion failed", "stage", "complete_text_panic")
			err = errors.New("chatgpt text transport failed")
			return
		}
	}()
	completed, err = client.CompleteText(context.Background(), request)
	return completed, err
}

func (s *Upstream) handleStartText(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.StartTextCommand)
	if !ok || len(cmd.Messages) == 0 || cmd.AccessToken == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid start text command"))
		return
	}
	streamID := uuid.NewString()
	ctx, cancel := context.WithCancel(context.Background())
	stream := &textStream{cancel: cancel, updates: make(chan textStreamUpdate, 64)}
	s.streamMu.Lock()
	s.streams[streamID] = stream
	s.streamMu.Unlock()
	s.AsyncTask(func() { s.runTextStream(ctx, streamID, stream, cmd) })
	result.Set(events.StartTextResult{StreamID: streamID}, nil)
}

func (s *Upstream) runTextStream(ctx context.Context, streamID string, stream *textStream, cmd events.StartTextCommand) {
	client, err := upclient.New(upclient.Config{AccessToken: cmd.AccessToken})
	if err == nil {
		messages := make([]upclient.TextMessage, 0, len(cmd.Messages))
		for _, message := range cmd.Messages {
			messages = append(messages, upclient.TextMessage{Role: message.Role, Content: message.Content, Images: message.Images})
		}
		_, err = client.StreamText(ctx, upclient.TextRequest{Model: cmd.Model, Messages: messages, ThinkingEffort: cmd.ThinkingEffort}, func(delta upclient.TextDelta) error {
			return s.publishTextUpdate(ctx, stream, textStreamUpdate{delta: delta.Text})
		})
	}
	update := textStreamUpdate{done: true}
	if err != nil {
		update.errClass, update.errMessage = classifyError(err), err.Error()
	}
	_ = s.publishTextUpdate(ctx, stream, update)
}

func (s *Upstream) publishTextUpdate(ctx context.Context, stream *textStream, update textStreamUpdate) error {
	select {
	case stream.updates <- update:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Upstream) handlePullText(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.PullTextCommand)
	if !ok || cmd.StreamID == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid pull text command"))
		return
	}
	s.streamMu.Lock()
	stream := s.streams[cmd.StreamID]
	s.streamMu.Unlock()
	if stream == nil {
		result.Set(nil, cd.NewError(cd.Unexpected, "text stream not found"))
		return
	}
	timeout := time.Duration(cmd.TimeoutMillis) * time.Millisecond
	if timeout <= 0 || timeout > 15000*time.Millisecond {
		timeout = time.Second
	}
	select {
	case update := <-stream.updates:
		out := events.PullTextResult{Delta: update.delta, Done: update.done, ErrorClass: update.errClass, ErrorMessage: update.errMessage}
		if update.done {
			s.removeTextStream(cmd.StreamID, false)
		}
		result.Set(out, nil)
	case <-time.After(timeout):
		result.Set(events.PullTextResult{}, nil)
	}
}

func (s *Upstream) handleCancelText(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.CancelTextCommand)
	if !ok || cmd.StreamID == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid cancel text command"))
		return
	}
	result.Set(events.CancelTextResult{Cancelled: s.removeTextStream(cmd.StreamID, true)}, nil)
}

func (s *Upstream) removeTextStream(streamID string, cancel bool) bool {
	s.streamMu.Lock()
	stream := s.streams[streamID]
	delete(s.streams, streamID)
	s.streamMu.Unlock()
	if stream != nil && cancel {
		stream.cancel()
	}
	return stream != nil
}

func classifyError(err error) events.ErrorClass {
	var upstreamErr *upclient.Error
	if !errors.As(err, &upstreamErr) {
		return events.ErrClassUpstream
	}
	switch upstreamErr.Class {
	case upclient.InvalidToken:
		return events.ErrClassInvalidToken
	case upclient.RateLimit:
		return events.ErrClassRateLimit
	case upclient.Timeout:
		return events.ErrClassTimeout
	case upclient.TLS:
		return events.ErrClassTLS
	default:
		return events.ErrClassUpstream
	}
}
