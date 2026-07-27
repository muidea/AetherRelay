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
	}
	b.SubscribeFunc(events.TopicSubmitGeneration, b.handleSubmitGeneration)
	b.SubscribeFunc(events.TopicSubmitEdit, b.handleSubmitEdit)
	b.SubscribeFunc(events.TopicList, b.handleList)
	b.SubscribeFunc(events.TopicResumePoll, b.handleResumePoll)
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
	if view.Status != events.StatusError || view.ConversationID == "" || resume.AccountID == "" || !isPollTimeout(view.Error) {
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

func isPollTimeout(message string) bool {
	message = strings.ToLower(message)
	return strings.Contains(message, "timeout") || strings.Contains(message, "timed out") || strings.Contains(message, "超时")
}

func (s *ImageTask) runGeneration(ownerID, taskID, prompt, model, size, quality, baseURL string) {
	start := time.Now()
	s.store.MarkRunning(ownerID, taskID, "getting_account")

	// acquire account
	accEv := event.NewEvent(accevents.TopicAcquireImageToken, s.ID(), acccommon.UnitID, nil, accevents.AcquireImageTokenCommand{})
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
	genEv := event.NewEvent(upevents.TopicGenerateImage, s.ID(), upcommon.UnitID, nil, upevents.GenerateImageCommand{
		AccessToken: token,

		Prompt:  prompt,
		Model:   model,
		Size:    size,
		Quality: quality,
	})
	genRes := s.SendEvent(genEv)
	genVal, genErr := genRes.Get()
	if genErr != nil {
		conversationID := ""
		if partial, ok := genVal.(upevents.GenerateImageResult); ok {
			conversationID = partial.ConversationID
		}
		s.markImageResult(token, false)
		s.store.MarkError(ownerID, taskID, genErr.Error(), conversationID)
		return
	}
	genOut, ok := genVal.(upevents.GenerateImageResult)
	if !ok {
		s.store.MarkError(ownerID, taskID, "invalid generate result", "")
		return
	}

	s.store.MarkProgress(ownerID, taskID, "receiving_image")
	data, persistErr := s.persistImageOutputs(genOut.Images, baseURL)
	if persistErr != nil {
		s.markImageResult(token, true)
		s.store.MarkError(ownerID, taskID, persistErr.Error(), genOut.ConversationID)
		return
	}

	s.markImageResult(token, true)
	s.store.MarkSuccess(ownerID, taskID, data, genOut.ConversationID, genOut.Usage, time.Since(start).Milliseconds())
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
	accEv := event.NewEvent(accevents.TopicAcquireImageToken, s.ID(), acccommon.UnitID, nil, accevents.AcquireImageTokenCommand{})
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

		Prompt:  prompt,
		Model:   model,
		Size:    size,
		Quality: quality,
		Images:  images,
	})
	editRes := s.SendEvent(editEv)
	editVal, editErr := editRes.Get()
	if editErr != nil {
		conversationID := ""
		if partial, ok := editVal.(upevents.EditImageResult); ok {
			conversationID = partial.ConversationID
		}
		s.markImageResult(token, false)
		s.store.MarkError(ownerID, taskID, editErr.Error(), conversationID)
		return
	}
	editOut, ok := editVal.(upevents.EditImageResult)
	if !ok {
		s.store.MarkError(ownerID, taskID, "invalid edit result", "")
		return
	}

	data, persistErr := s.persistImageOutputs(editOut.Images, baseURL)
	if persistErr != nil {
		s.markImageResult(token, true)
		s.store.MarkError(ownerID, taskID, persistErr.Error(), editOut.ConversationID)
		return
	}
	s.markImageResult(token, true)
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
	resumeRes := s.SendEvent(event.NewEvent(upevents.TopicResumeImage, s.ID(), upcommon.UnitID, nil, upevents.ResumeImageCommand{AccessToken: token, ConversationID: conversationID, ExtraTimeoutSecs: extraTimeoutSecs}))
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
		s.markImageResult(token, true)
		s.store.MarkError(ownerID, taskID, persistErr.Error(), conversationID)
		return
	}
	s.markImageResult(token, true)
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

func (s *ImageTask) markImageResult(accessToken string, success bool) {
	value, err := s.SendEvent(event.NewEvent(accevents.TopicMarkImageResult, s.ID(), acccommon.UnitID, nil, accevents.MarkImageResultCommand{AccessToken: accessToken, Success: success})).Get()
	if err != nil {
		return
	}
	if _, ok := value.(accevents.MarkImageResultResult); !ok {
		return
	}
}
