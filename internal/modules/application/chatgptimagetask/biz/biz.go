package biz

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	acccommon "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/common"
	accevents "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/events"
	"ai-proxy/internal/modules/application/chatgptimagetask/internal/store"
	"ai-proxy/internal/modules/application/chatgptimagetask/pkg/common"
	events "ai-proxy/internal/modules/application/chatgptimagetask/pkg/events"
	proxycommon "ai-proxy/internal/modules/application/proxyapi/pkg/common"
	proxyevents "ai-proxy/internal/modules/application/proxyapi/pkg/events"
	basebiz "ai-proxy/internal/modules/base/biz"
	imgcommon "ai-proxy/internal/modules/blocks/chatgptimagestore/pkg/common"
	imgevents "ai-proxy/internal/modules/blocks/chatgptimagestore/pkg/events"
	upcommon "ai-proxy/internal/modules/blocks/chatgptwebupstream/pkg/common"
	upevents "ai-proxy/internal/modules/blocks/chatgptwebupstream/pkg/events"
	configevents "ai-proxy/internal/modules/blocks/configruntime/pkg/events"
	"ai-proxy/internal/pkg/chatgptimageinput"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

type ImageTask struct {
	basebiz.Base
	store   *store.Store
	topics  []string
	runMu   sync.Mutex
	running map[string]*taskRun
}

type taskRun struct{ cancel context.CancelFunc }

func New(ctx context.Context, hub event.Hub, background task.BackgroundRoutine) (*ImageTask, *cd.Error) {
	b := &ImageTask{
		Base:    basebiz.New(common.UnitID, hub, background),
		running: map[string]*taskRun{},
	}
	bootstrap, err := configevents.RequestBootstrap(ctx, b.EventHub(), b.ID())
	if err != nil {
		return nil, cd.NewError(cd.IllegalParam, err.Error())
	}
	if err := os.MkdirAll(bootstrap.Config.State.Dir, 0o700); err != nil {
		return nil, cd.NewError(cd.Unexpected, err.Error())
	}
	b.store, err = store.Open(bootstrap.Config.State.Database, bootstrap.Config.State.MemoryLimit, bootstrap.Config.State.Threads)
	if err != nil {
		return nil, cd.NewError(cd.Unexpected, "open chatgpt image task state: "+err.Error())
	}
	b.topics = []string{
		events.TopicSubmitGeneration,
		events.TopicSubmitEdit,
		events.TopicList,
		events.TopicResumePoll,
		events.TopicRetryGeneration,
		events.TopicCancel,
		events.TopicDelete,
		events.TopicDeleteOwner,
	}
	b.SubscribeFunc(events.TopicSubmitGeneration, b.handleSubmitGeneration)
	b.SubscribeFunc(events.TopicSubmitEdit, b.handleSubmitEdit)
	b.SubscribeFunc(events.TopicList, b.handleList)
	b.SubscribeFunc(events.TopicResumePoll, b.handleResumePoll)
	b.SubscribeFunc(events.TopicRetryGeneration, b.handleRetryGeneration)
	b.SubscribeFunc(events.TopicCancel, b.handleCancel)
	b.SubscribeFunc(events.TopicDelete, b.handleDelete)
	b.SubscribeFunc(events.TopicDeleteOwner, b.handleDeleteOwner)
	return b, nil
}

func (s *ImageTask) Run(context.Context) *cd.Error { return nil }

func (s *ImageTask) Teardown(context.Context) {
	s.runMu.Lock()
	for _, run := range s.running {
		run.cancel()
	}
	s.running = map[string]*taskRun{}
	s.runMu.Unlock()
	for _, topic := range s.topics {
		s.UnsubscribeFunc(topic)
	}
	if s.store != nil {
		_ = s.store.Close()
	}
	s.store = nil
}

func (s *ImageTask) handleSubmitGeneration(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.SubmitGenerationCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid submit generation command"))
		return
	}
	if cmd.ClientTaskID == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "client_task_id required"))
		return
	}
	if cmd.Model == "" {
		cmd.Model = "gpt-image-2"
	}
	view, created, err := s.store.GetOrCreateGeneration(cmd.OwnerID, cmd.ClientTaskID, cmd.Prompt, cmd.Model, cmd.Size, cmd.Quality)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	if created {
		ownerID, taskID, prompt, model, size, quality, baseURL := cmd.OwnerID, cmd.ClientTaskID, cmd.Prompt, cmd.Model, cmd.Size, cmd.Quality, cmd.BaseURL
		s.startTask(ownerID, taskID, func(ctx context.Context) {
			s.runGeneration(ctx, ownerID, taskID, prompt, model, size, quality, baseURL)
		})
	}
	result.Set(events.SubmitResult{Task: view}, nil)
}

func (s *ImageTask) handleSubmitEdit(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.SubmitEditCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid submit edit command"))
		return
	}
	if cmd.ClientTaskID == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "client_task_id required"))
		return
	}
	if cmd.Model == "" {
		cmd.Model = "gpt-image-2"
	}
	view, created, err := s.store.GetOrCreateEdit(cmd.OwnerID, cmd.ClientTaskID, cmd.Prompt, cmd.Model, cmd.Size, cmd.Quality)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	if created {
		ownerID, taskID, prompt, model, size, quality, baseURL, images := cmd.OwnerID, cmd.ClientTaskID, cmd.Prompt, cmd.Model, cmd.Size, cmd.Quality, cmd.BaseURL, append([]string(nil), cmd.Images...)
		s.startTask(ownerID, taskID, func(ctx context.Context) {
			s.runEdit(ctx, ownerID, taskID, prompt, model, size, quality, baseURL, images)
		})
	}
	result.Set(events.SubmitResult{Task: view}, nil)
}

func (s *ImageTask) handleList(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.ListCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid list command"))
		return
	}
	items, missing := s.store.List(cmd.OwnerID, cmd.TaskIDs)
	result.Set(events.ListResult{Items: items, MissingIDs: missing}, nil)
}

func (s *ImageTask) handleResumePoll(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.ResumePollCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid resume poll command"))
		return
	}
	resume, ok := s.store.GetResumeInfo(cmd.OwnerID, cmd.TaskID)
	if !ok {
		result.Set(nil, cd.NewError(cd.Unexpected, "task not found"))
		return
	}
	view := resume.Task
	if !isResumableConversationFailure(view) || resume.AccountID == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "task is not resumable"))
		return
	}
	if cmd.ExtraTimeoutSecs <= 0 {
		cmd.ExtraTimeoutSecs = 30
	}
	if cmd.ExtraTimeoutSecs < 5 || cmd.ExtraTimeoutSecs > 120 {
		result.Set(nil, cd.NewError(cd.IllegalParam, "extra timeout must be between 5 and 120 seconds"))
		return
	}
	s.store.MarkRunning(cmd.OwnerID, cmd.TaskID, "resuming_poll")
	view, _ = s.store.Get(cmd.OwnerID, cmd.TaskID)
	ownerID, taskID, conversationID, accountID, baseURL := cmd.OwnerID, cmd.TaskID, resume.Task.ConversationID, resume.AccountID, ""
	s.startTask(ownerID, taskID, func(ctx context.Context) {
		s.runResumePoll(ctx, ownerID, taskID, conversationID, accountID, cmd.ExtraTimeoutSecs, baseURL)
	})
	result.Set(events.ResumePollResult{Task: view}, nil)
}

func (s *ImageTask) handleRetryGeneration(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.RetryGenerationCommand)
	if !ok || strings.TrimSpace(cmd.OwnerID) == "" || strings.TrimSpace(cmd.TaskID) == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid retry generation command"))
		return
	}
	view, found := s.store.Get(cmd.OwnerID, cmd.TaskID)
	if !found {
		result.Set(nil, cd.NewError(cd.IllegalParam, "task not found"))
		return
	}
	if !isRetryableBootstrapFailure(view) {
		result.Set(nil, cd.NewError(cd.IllegalParam, "task is not safe to retry"))
		return
	}
	view, retried, err := s.store.RetryGeneration(cmd.OwnerID, cmd.TaskID)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	if !retried {
		result.Set(nil, cd.NewError(cd.IllegalParam, "task is not safe to retry"))
		return
	}
	ownerID, taskID, prompt, model, size, quality, baseURL := cmd.OwnerID, cmd.TaskID, view.Prompt, view.Model, view.Size, view.Quality, cmd.BaseURL
	s.startTask(ownerID, taskID, func(ctx context.Context) {
		s.runGeneration(ctx, ownerID, taskID, prompt, model, size, quality, baseURL)
	})
	result.Set(events.RetryGenerationResult{Task: view}, nil)
}

func (s *ImageTask) handleCancel(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.CancelCommand)
	if !ok || strings.TrimSpace(cmd.OwnerID) == "" || strings.TrimSpace(cmd.TaskID) == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid cancel command"))
		return
	}
	view, cancelled, err := s.store.CancelActive(cmd.OwnerID, cmd.TaskID)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	if !cancelled {
		result.Set(nil, cd.NewError(cd.IllegalParam, "task is not cancellable"))
		return
	}
	s.cancelTask(cmd.OwnerID, cmd.TaskID)
	result.Set(events.CancelResult{Task: view}, nil)
}

func (s *ImageTask) handleDelete(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.DeleteCommand)
	if !ok || strings.TrimSpace(cmd.OwnerID) == "" || strings.TrimSpace(cmd.TaskID) == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid delete command"))
		return
	}
	deleted, err := s.store.DeleteTerminal(cmd.OwnerID, cmd.TaskID)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	if !deleted {
		result.Set(nil, cd.NewError(cd.IllegalParam, "task is not deletable"))
		return
	}
	s.cancelTask(cmd.OwnerID, cmd.TaskID)
	result.Set(events.DeleteResult{Deleted: true}, nil)
}

func (s *ImageTask) handleDeleteOwner(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.DeleteOwnerCommand)
	if !ok || strings.TrimSpace(cmd.OwnerID) == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid delete owner command"))
		return
	}
	s.runMu.Lock()
	for key, run := range s.running {
		if strings.HasPrefix(key, cmd.OwnerID+"\x00") {
			run.cancel()
		}
	}
	s.runMu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.runMu.Lock()
		active := false
		for key := range s.running {
			if strings.HasPrefix(key, cmd.OwnerID+"\x00") {
				active = true
				break
			}
		}
		s.runMu.Unlock()
		if !active {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	deleted, err := s.store.DeleteOwner(cmd.OwnerID)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(events.DeleteOwnerResult{Deleted: deleted}, nil)
}

func imageTaskRunKey(ownerID, taskID string) string { return ownerID + "\x00" + taskID }

func (s *ImageTask) startTask(ownerID, taskID string, execute func(context.Context)) {
	ctx, cancel := context.WithCancel(context.Background())
	run := &taskRun{cancel: cancel}
	key := imageTaskRunKey(ownerID, taskID)
	s.runMu.Lock()
	if previous := s.running[key]; previous != nil {
		previous.cancel()
	}
	s.running[key] = run
	s.runMu.Unlock()
	s.AsyncTask(func() {
		defer func() {
			cancel()
			s.runMu.Lock()
			if s.running[key] == run {
				delete(s.running, key)
			}
			s.runMu.Unlock()
		}()
		if !s.store.IsActive(ownerID, taskID) {
			return
		}
		execute(ctx)
	})
}

func (s *ImageTask) cancelTask(ownerID, taskID string) {
	s.runMu.Lock()
	run := s.running[imageTaskRunKey(ownerID, taskID)]
	s.runMu.Unlock()
	if run != nil {
		run.cancel()
	}
}

// isResumableConversationFailure accepts any terminal failure after an
// upstream conversation was created. Resuming only polls that conversation;
// it never submits a second image-generation request. This also recovers
// records produced by earlier versions that mistakenly stored "<nil>" as an
// error after a successful submission.
func isResumableConversationFailure(task events.TaskView) bool {
	status := strings.ToLower(strings.TrimSpace(task.Status))
	return (status == events.StatusError || status == "failed") && strings.TrimSpace(task.ConversationID) != ""
}

func isRetryableBootstrapFailure(task events.TaskView) bool {
	if task.Status != events.StatusError || task.Mode != "generate" || task.ConversationID != "" {
		return false
	}
	return isBootstrapTransportError(task.Error)
}

func (s *ImageTask) runGeneration(ctx context.Context, ownerID, taskID, prompt, model, size, quality, baseURL string) {
	start := time.Now()
	s.store.MarkRunning(ownerID, taskID, "selecting_provider")
	value, executeErr := s.SendEvent(event.NewEventWithContext(proxyevents.TopicExecuteFeatureImage, s.ID(), proxycommon.UnitID, event.NewHeader(), ctx, proxyevents.ExecuteFeatureImageCommand{
		OwnerID: ownerID, Model: model, Prompt: prompt, Size: size, Quality: quality,
	})).Get()
	if executeErr != nil {
		if partial, ok := value.(proxyevents.ExecuteFeatureImageResult); ok {
			s.store.SetProvider(ownerID, taskID, partial.Provider)
			s.store.SetAccountID(ownerID, taskID, partial.AccountID)
			s.store.MarkError(ownerID, taskID, executeErr.Error(), partial.ConversationID)
		} else {
			s.store.MarkError(ownerID, taskID, executeErr.Error(), "")
		}
		return
	}
	generated, ok := value.(proxyevents.ExecuteFeatureImageResult)
	if !ok || len(generated.Data) == 0 {
		s.store.MarkError(ownerID, taskID, "invalid feature image result", "")
		return
	}
	s.store.SetProvider(ownerID, taskID, generated.Provider)
	s.store.SetAccountID(ownerID, taskID, generated.AccountID)
	s.store.MarkProgress(ownerID, taskID, "receiving_image")
	outputs := make([]upevents.ImageOutput, 0, len(generated.Data))
	for _, item := range generated.Data {
		outputs = append(outputs, featureImageOutput(item))
	}
	data, persistErr := s.persistImageOutputs(ctx, ownerID, outputs, baseURL)
	if persistErr != nil {
		s.store.MarkError(ownerID, taskID, persistErr.Error(), "")
		return
	}
	s.store.MarkSuccess(ownerID, taskID, data, generated.ConversationID, generated.Usage, time.Since(start).Milliseconds())
}

// generateWithBootstrapRetry retries only the first, pre-conversation
// bootstrap transport failure. Once a conversation may exist, a blind retry
// could create a duplicate image and remains an explicit operator action.
func (s *ImageTask) generateWithBootstrapRetry(ownerID, taskID, token, proxy, prompt, model, size, quality string) (any, *cd.Error) {
	for attempt := 0; attempt < 2; attempt++ {
		result := s.SendEvent(event.NewEvent(upevents.TopicGenerateImage, s.ID(), upcommon.UnitID, nil, upevents.GenerateImageCommand{
			AccessToken: token,
			Proxy:       proxy,
			Prompt:      prompt,
			Model:       model,
			Size:        size,
			Quality:     quality,
		}))
		value, err := result.Get()
		if err == nil || attempt == 1 || !isBootstrapTransportError(err.Error()) {
			return value, err
		}
		s.store.MarkProgress(ownerID, taskID, "retrying_bootstrap")
		time.Sleep(time.Second)
	}
	return nil, cd.NewError(cd.Unexpected, "image generation retry exhausted")
}

func isBootstrapTransportError(message string) bool {
	message = strings.ToLower(message)
	return strings.Contains(message, "bootstrap: tls:") || strings.Contains(message, "bootstrap: timeout:")
}

func (s *ImageTask) runEdit(ctx context.Context, ownerID, taskID, prompt, model, size, quality, baseURL string, encodedImages []string) {
	start := time.Now()
	s.store.MarkRunning(ownerID, taskID, "decoding_images")
	images, err := imageinput.DecodeBase64Images(encodedImages)
	if err != nil {
		s.store.MarkError(ownerID, taskID, err.Error(), "")
		return
	}

	s.store.MarkProgress(ownerID, taskID, "selecting_provider")
	value, executeErr := s.SendEvent(event.NewEventWithContext(proxyevents.TopicExecuteFeatureImage, s.ID(), proxycommon.UnitID, event.NewHeader(), ctx, proxyevents.ExecuteFeatureImageCommand{
		OwnerID: ownerID, Model: model, Prompt: prompt, Size: size, Quality: quality, Images: images,
	})).Get()
	if executeErr != nil {
		if partial, ok := value.(proxyevents.ExecuteFeatureImageResult); ok {
			s.store.SetProvider(ownerID, taskID, partial.Provider)
			s.store.SetAccountID(ownerID, taskID, partial.AccountID)
			s.store.MarkError(ownerID, taskID, executeErr.Error(), partial.ConversationID)
		} else {
			s.store.MarkError(ownerID, taskID, executeErr.Error(), "")
		}
		return
	}
	edited, ok := value.(proxyevents.ExecuteFeatureImageResult)
	if !ok || len(edited.Data) == 0 {
		s.store.MarkError(ownerID, taskID, "invalid feature image result", "")
		return
	}
	s.store.SetProvider(ownerID, taskID, edited.Provider)
	s.store.SetAccountID(ownerID, taskID, edited.AccountID)
	outputs := make([]upevents.ImageOutput, 0, len(edited.Data))
	for _, item := range edited.Data {
		outputs = append(outputs, featureImageOutput(item))
	}
	data, persistErr := s.persistImageOutputs(ctx, ownerID, outputs, baseURL)
	if persistErr != nil {
		s.store.MarkError(ownerID, taskID, persistErr.Error(), "")
		return
	}
	s.store.MarkSuccess(ownerID, taskID, data, edited.ConversationID, edited.Usage, time.Since(start).Milliseconds())
}

func featureImageOutput(item proxyevents.FeatureImageData) upevents.ImageOutput {
	output := upevents.ImageOutput{URL: item.URL, B64JSON: item.B64JSON, RevisedPrompt: item.RevisedPrompt}
	if item.B64JSON != "" {
		if decoded, err := base64.StdEncoding.DecodeString(item.B64JSON); err == nil {
			output.Bytes = decoded
		}
	}
	return output
}

func (s *ImageTask) runResumePoll(ctx context.Context, ownerID, taskID, conversationID, accountID string, extraTimeoutSecs int, baseURL string) {
	start := time.Now()
	accRes := s.SendEvent(event.NewEventWithContext(accevents.TopicAcquireImageAccount, s.ID(), acccommon.UnitID, event.NewHeader(), ctx, accevents.AcquireImageAccountCommand{AccountID: accountID}))
	accVal, accErr := accRes.Get()
	if accErr != nil {
		s.store.MarkError(ownerID, taskID, accErr.Error(), conversationID)
		return
	}
	accOut, ok := accVal.(accevents.AcquireImageTokenResult)
	if !ok {
		s.store.MarkError(ownerID, taskID, "invalid saved account acquire result", conversationID)
		return
	}
	token := accOut.AccessToken
	defer func() {
		s.releaseImageSlot(token)
	}()

	s.store.MarkProgress(ownerID, taskID, "resuming_poll")
	resumeRes := s.SendEvent(event.NewEventWithContext(upevents.TopicResumeImage, s.ID(), upcommon.UnitID, event.NewHeader(), ctx, upevents.ResumeImageCommand{AccessToken: token, Proxy: accOut.Account.Proxy, ConversationID: conversationID, ExtraTimeoutSecs: extraTimeoutSecs}))
	resumeVal, resumeErr := resumeRes.Get()
	if resumeErr != nil {
		if partial, ok := resumeVal.(upevents.ResumeImageResult); ok && partial.ConversationID != "" {
			conversationID = partial.ConversationID
		}
		s.store.MarkError(ownerID, taskID, resumeErr.Error(), conversationID)
		return
	}
	resumed, ok := resumeVal.(upevents.ResumeImageResult)
	if !ok {
		s.store.MarkError(ownerID, taskID, "invalid resume image result", conversationID)
		return
	}
	if resumed.ConversationID != "" {
		conversationID = resumed.ConversationID
	}
	s.store.MarkProgress(ownerID, taskID, "receiving_image")
	data, persistErr := s.persistImageOutputs(ctx, ownerID, resumed.Images, baseURL)
	if persistErr != nil {
		s.markImageResult(token, "", true, "")
		s.store.MarkError(ownerID, taskID, persistErr.Error(), conversationID)
		return
	}
	s.markImageResult(token, "", true, "")
	s.store.MarkSuccess(ownerID, taskID, data, conversationID, nil, time.Since(start).Milliseconds())
}

func (s *ImageTask) persistImageOutputs(ctx context.Context, apiKeyID string, images []upevents.ImageOutput, baseURL string) ([]events.ImageData, error) {
	data := make([]events.ImageData, 0, len(images))
	for _, image := range images {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		item := events.ImageData{URL: image.URL, B64JSON: image.B64JSON, RevisedPrompt: image.RevisedPrompt}
		if len(image.Bytes) > 0 {
			saveEv := event.NewEventWithContext(imgevents.TopicSave, s.ID(), imgcommon.UnitID, event.NewHeader(), ctx, imgevents.SaveCommand{APIKeyID: apiKeyID, Bytes: image.Bytes, BaseURL: baseURL})
			saveVal, saveErr := s.SendEvent(saveEv).Get()
			if saveErr != nil {
				return nil, saveErr
			}
			saved, ok := saveVal.(imgevents.SaveResult)
			if !ok || strings.TrimSpace(saved.PublicURL) == "" {
				return nil, fmt.Errorf("invalid saved image result")
			}
			item.URL = saved.PublicURL
		}
		data = append(data, item)
	}
	return data, nil
}

func (s *ImageTask) releaseImageSlot(accessToken string) {
	value, err := s.SendEvent(event.NewEvent(accevents.TopicReleaseImageSlot, s.ID(), acccommon.UnitID, nil, accevents.ReleaseImageSlotCommand{AccessToken: accessToken})).Get()
	if err != nil {
		return
	}
	result, ok := value.(accevents.ReleaseImageSlotResult)
	if !ok || !result.OK {
		return
	}
}

func (s *ImageTask) markImageResult(accessToken, model string, success bool, errorClass string) {
	value, err := s.SendEvent(event.NewEvent(accevents.TopicMarkImageResult, s.ID(), acccommon.UnitID, nil, accevents.MarkImageResultCommand{AccessToken: accessToken, Model: model, Success: success, ErrorClass: errorClass})).Get()
	if err != nil {
		return
	}
	if _, ok := value.(accevents.MarkImageResultResult); !ok {
		return
	}
}

func generationFailure(value any) (string, upevents.ErrorClass) {
	if partial, ok := value.(upevents.GenerateImageResult); ok {
		return partial.ConversationID, partial.ErrorClass
	}
	return "", ""
}

func editFailure(value any) (string, upevents.ErrorClass) {
	if partial, ok := value.(upevents.EditImageResult); ok {
		return partial.ConversationID, partial.ErrorClass
	}
	return "", ""
}

func (s *ImageTask) executeEdit(token, proxy, prompt, model, size, quality string, images [][]byte) (any, *cd.Error) {
	return s.SendEvent(event.NewEvent(upevents.TopicEditImage, s.ID(), upcommon.UnitID, nil, upevents.EditImageCommand{
		AccessToken: token, Proxy: proxy, Prompt: prompt, Model: model, Size: size, Quality: quality, Images: images,
	})).Get()
}

func (s *ImageTask) refreshImageToken(token string) (accevents.RefreshTextTokenResult, bool, error) {
	value, err := s.SendEvent(event.NewEventWithContext(accevents.TopicRefreshTextToken, s.ID(), acccommon.UnitID, event.NewHeader(), context.Background(), accevents.RefreshTextTokenCommand{AccessToken: token})).Get()
	if err != nil {
		return accevents.RefreshTextTokenResult{}, false, err
	}
	refreshed, ok := value.(accevents.RefreshTextTokenResult)
	if !ok {
		return accevents.RefreshTextTokenResult{}, false, fmt.Errorf("invalid refreshed image account result")
	}
	if refreshed.Refreshed && strings.TrimSpace(refreshed.AccessToken) != "" {
		return refreshed, false, nil
	}
	if refreshed.PermanentFailure {
		return refreshed, true, nil
	}
	return refreshed, false, fmt.Errorf("chatgpt oauth refresh temporarily unavailable")
}

func (s *ImageTask) removeInvalidImageToken(token string) {
	if strings.TrimSpace(token) == "" {
		return
	}
	_, _ = s.SendEvent(event.NewEvent(accevents.TopicRemoveInvalid, s.ID(), acccommon.UnitID, nil, accevents.RemoveInvalidCommand{AccessToken: token, Event: "image_generation"})).Get()
}
