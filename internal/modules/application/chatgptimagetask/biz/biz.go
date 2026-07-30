package biz

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	acccommon "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/common"
	accevents "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/events"
	"ai-proxy/internal/modules/application/chatgptimagetask/internal/store"
	"ai-proxy/internal/modules/application/chatgptimagetask/pkg/common"
	events "ai-proxy/internal/modules/application/chatgptimagetask/pkg/events"
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
	store  *store.Store
	topics []string
}

func New(ctx context.Context, hub event.Hub, background task.BackgroundRoutine) (*ImageTask, *cd.Error) {
	b := &ImageTask{
		Base: basebiz.New(common.UnitID, hub, background),
	}
	bootstrap, err := configevents.RequestBootstrap(ctx, b.EventHub(), b.ID())
	if err != nil {
		return nil, cd.NewError(cd.IllegalParam, err.Error())
	}
	if !bootstrap.Config.ChatGPTWeb.Enabled {
		return b, nil
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
	}
	b.SubscribeFunc(events.TopicSubmitGeneration, b.handleSubmitGeneration)
	b.SubscribeFunc(events.TopicSubmitEdit, b.handleSubmitEdit)
	b.SubscribeFunc(events.TopicList, b.handleList)
	b.SubscribeFunc(events.TopicResumePoll, b.handleResumePoll)
	b.SubscribeFunc(events.TopicRetryGeneration, b.handleRetryGeneration)
	return b, nil
}

func (s *ImageTask) Run(context.Context) *cd.Error { return nil }

func (s *ImageTask) Teardown(context.Context) {
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
		s.AsyncTask(func() {
			s.runGeneration(ownerID, taskID, prompt, model, size, quality, baseURL)
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
		s.AsyncTask(func() {
			s.runEdit(ownerID, taskID, prompt, model, size, quality, baseURL, images)
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
	s.AsyncTask(func() {
		s.runResumePoll(ownerID, taskID, conversationID, accountID, cmd.ExtraTimeoutSecs, baseURL)
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
	s.AsyncTask(func() {
		s.runGeneration(ownerID, taskID, prompt, model, size, quality, baseURL)
	})
	result.Set(events.RetryGenerationResult{Task: view}, nil)
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

func (s *ImageTask) runGeneration(ownerID, taskID, prompt, model, size, quality, baseURL string) {
	start := time.Now()
	s.store.MarkRunning(ownerID, taskID, "getting_account")

	// acquire account
	accEv := event.NewEvent(accevents.TopicAcquireImageToken, s.ID(), acccommon.UnitID, nil, accevents.AcquireImageTokenCommand{Model: model, Operation: accevents.ModelOperationImageGenerations})
	accRes := s.SendEvent(accEv)
	accVal, accErr := accRes.Get()
	if accErr != nil {
		s.store.MarkError(ownerID, taskID, accErr.Error(), "")
		return
	}
	accOut, ok := accVal.(accevents.AcquireImageTokenResult)
	if !ok {
		s.store.MarkError(ownerID, taskID, "invalid acquire result", "")
		return
	}
	token := accOut.AccessToken
	s.store.SetAccountID(ownerID, taskID, accOut.Account.ID)
	defer func() {
		s.releaseImageSlot(token)
	}()

	s.store.MarkProgress(ownerID, taskID, "starting_generation")
	genVal, genErr := s.generateWithBootstrapRetry(ownerID, taskID, token, accOut.Account.Proxy, prompt, model, size, quality)
	if genErr != nil {
		recovered := false
		invalidRemoved := false
		conversationID, class := generationFailure(genVal)
		if class == upevents.ErrClassInvalidToken && conversationID == "" {
			refreshed, permanent, refreshErr := s.refreshImageToken(token)
			if refreshErr == nil && !permanent {
				s.store.MarkProgress(ownerID, taskID, "retrying_after_oauth_refresh")
				genVal, genErr = s.generateWithBootstrapRetry(ownerID, taskID, refreshed.AccessToken, refreshed.Account.Proxy, prompt, model, size, quality)
				if genErr == nil {
					recovered = true
				} else {
					conversationID, class = generationFailure(genVal)
				}
			} else if permanent {
				s.removeInvalidImageToken(token)
				invalidRemoved = true
			} else {
				class = upevents.ErrClassUpstream
			}
		}
		if !recovered {
			if class == upevents.ErrClassInvalidToken && !invalidRemoved {
				s.removeInvalidImageToken(token)
			}
			if class == "" {
				class = upevents.ErrClassUpstream
			}
			s.markImageResult(token, model, false, string(class))
			s.store.MarkError(ownerID, taskID, genErr.Error(), conversationID)
			return
		}
	}
	genOut, ok := genVal.(upevents.GenerateImageResult)
	if !ok {
		s.store.MarkError(ownerID, taskID, "invalid generate result", "")
		return
	}

	s.store.MarkProgress(ownerID, taskID, "receiving_image")
	data, persistErr := s.persistImageOutputs(genOut.Images, baseURL)
	if persistErr != nil {
		s.markImageResult(token, model, true, "")
		s.store.MarkError(ownerID, taskID, persistErr.Error(), genOut.ConversationID)
		return
	}

	s.markImageResult(token, model, true, "")
	s.store.MarkSuccess(ownerID, taskID, data, genOut.ConversationID, genOut.Usage, time.Since(start).Milliseconds())
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

func (s *ImageTask) runEdit(ownerID, taskID, prompt, model, size, quality, baseURL string, encodedImages []string) {
	start := time.Now()
	s.store.MarkRunning(ownerID, taskID, "decoding_images")
	images, err := imageinput.DecodeBase64Images(encodedImages)
	if err != nil {
		s.store.MarkError(ownerID, taskID, err.Error(), "")
		return
	}

	s.store.MarkProgress(ownerID, taskID, "getting_account")
	accEv := event.NewEvent(accevents.TopicAcquireImageToken, s.ID(), acccommon.UnitID, nil, accevents.AcquireImageTokenCommand{Model: model, Operation: accevents.ModelOperationImageGenerations})
	accRes := s.SendEvent(accEv)
	accVal, accErr := accRes.Get()
	if accErr != nil {
		s.store.MarkError(ownerID, taskID, accErr.Error(), "")
		return
	}
	accOut, ok := accVal.(accevents.AcquireImageTokenResult)
	if !ok {
		s.store.MarkError(ownerID, taskID, "invalid acquire result", "")
		return
	}
	token := accOut.AccessToken
	s.store.SetAccountID(ownerID, taskID, accOut.Account.ID)
	defer func() {
		s.releaseImageSlot(token)
	}()

	s.store.MarkProgress(ownerID, taskID, "starting_edit")
	editEv := event.NewEvent(upevents.TopicEditImage, s.ID(), upcommon.UnitID, nil, upevents.EditImageCommand{
		AccessToken: token,
		Proxy:       accOut.Account.Proxy,

		Prompt:  prompt,
		Model:   model,
		Size:    size,
		Quality: quality,
		Images:  images,
	})
	editRes := s.SendEvent(editEv)
	editVal, editErr := editRes.Get()
	if editErr != nil {
		recovered := false
		invalidRemoved := false
		conversationID, class := editFailure(editVal)
		if class == upevents.ErrClassInvalidToken && conversationID == "" {
			refreshed, permanent, refreshErr := s.refreshImageToken(token)
			if refreshErr == nil && !permanent {
				s.store.MarkProgress(ownerID, taskID, "retrying_after_oauth_refresh")
				editVal, editErr = s.executeEdit(refreshed.AccessToken, refreshed.Account.Proxy, prompt, model, size, quality, images)
				if editErr == nil {
					recovered = true
				} else {
					conversationID, class = editFailure(editVal)
				}
			} else if permanent {
				s.removeInvalidImageToken(token)
				invalidRemoved = true
			} else {
				class = upevents.ErrClassUpstream
			}
		}
		if !recovered {
			if class == upevents.ErrClassInvalidToken && !invalidRemoved {
				s.removeInvalidImageToken(token)
			}
			if class == "" {
				class = upevents.ErrClassUpstream
			}
			s.markImageResult(token, model, false, string(class))
			s.store.MarkError(ownerID, taskID, editErr.Error(), conversationID)
			return
		}
	}
	editOut, ok := editVal.(upevents.EditImageResult)
	if !ok {
		s.store.MarkError(ownerID, taskID, "invalid edit result", "")
		return
	}

	data, persistErr := s.persistImageOutputs(editOut.Images, baseURL)
	if persistErr != nil {
		s.markImageResult(token, model, true, "")
		s.store.MarkError(ownerID, taskID, persistErr.Error(), editOut.ConversationID)
		return
	}
	s.markImageResult(token, model, true, "")
	s.store.MarkSuccess(ownerID, taskID, data, editOut.ConversationID, editOut.Usage, time.Since(start).Milliseconds())
}

func (s *ImageTask) runResumePoll(ownerID, taskID, conversationID, accountID string, extraTimeoutSecs int, baseURL string) {
	start := time.Now()
	accRes := s.SendEvent(event.NewEvent(accevents.TopicAcquireImageAccount, s.ID(), acccommon.UnitID, nil, accevents.AcquireImageAccountCommand{AccountID: accountID}))
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
	resumeRes := s.SendEvent(event.NewEvent(upevents.TopicResumeImage, s.ID(), upcommon.UnitID, nil, upevents.ResumeImageCommand{AccessToken: token, Proxy: accOut.Account.Proxy, ConversationID: conversationID, ExtraTimeoutSecs: extraTimeoutSecs}))
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
	data, persistErr := s.persistImageOutputs(resumed.Images, baseURL)
	if persistErr != nil {
		s.markImageResult(token, "", true, "")
		s.store.MarkError(ownerID, taskID, persistErr.Error(), conversationID)
		return
	}
	s.markImageResult(token, "", true, "")
	s.store.MarkSuccess(ownerID, taskID, data, conversationID, nil, time.Since(start).Milliseconds())
}

func (s *ImageTask) persistImageOutputs(images []upevents.ImageOutput, baseURL string) ([]events.ImageData, error) {
	data := make([]events.ImageData, 0, len(images))
	for _, image := range images {
		item := events.ImageData{URL: image.URL, B64JSON: image.B64JSON, RevisedPrompt: image.RevisedPrompt}
		if len(image.Bytes) > 0 {
			saveEv := event.NewEvent(imgevents.TopicSave, s.ID(), imgcommon.UnitID, nil, imgevents.SaveCommand{Bytes: image.Bytes, BaseURL: baseURL})
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
