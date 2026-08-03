package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	accevents "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/events"
	taskevents "ai-proxy/internal/modules/application/chatgptimagetask/pkg/events"
	tempevents "ai-proxy/internal/modules/application/chatgpttemporarychat/pkg/events"
	"ai-proxy/internal/modules/application/proxyapi/pkg/effectivecatalog"
	imgevents "ai-proxy/internal/modules/blocks/chatgptimagestore/pkg/events"
)

type chatGPTAccountRuntimeStub struct {
	addedTokens        []string
	deletedIDs         []string
	updated            accevents.UpdateByIDCommand
	imageBytes         []string
	imageThumbs        []string
	imageList          imgevents.ListResult
	imageListErr       error
	imageBytesErr      error
	exportedIDs        []string
	exportedItems      []accevents.ExportItem
	retryOwner         string
	retryTaskID        string
	retryBaseURL       string
	temporaryCreate    tempevents.CreateConversationCommand
	temporaryGet       tempevents.GetConversationCommand
	temporaryTurn      tempevents.StartTurnCommand
	temporaryCreateErr error
	temporaryGetErr    error
}

type unavailableChatGPTRuntimeStub struct{ *chatGPTAccountRuntimeStub }

func (unavailableChatGPTRuntimeStub) ChatGPTWebEnabled() bool { return false }

func (s *chatGPTAccountRuntimeStub) ListChatGPTAccounts(context.Context) ([]accevents.AccountView, error) {
	return []accevents.AccountView{{
		ID:                         "account-1",
		Email:                      "operator@example.invalid",
		Status:                     "正常",
		Quota:                      7,
		RestoreAt:                  "2026-07-27T01:02:03Z",
		ImageInflight:              1,
		Success:                    9,
		Fail:                       2,
		CreatedAt:                  "2026-07-26T01:02:03Z",
		LastTokenRefreshAt:         "2026-07-27T00:30:00Z",
		LastTokenRefreshErrorAt:    "2026-07-27T01:00:00Z",
		LastTokenRefreshErrorClass: "rate_limit",
		TextCooldowns:              []accevents.TextCooldownView{{Model: "gpt-5", Until: "2026-07-27T01:03:03Z", ErrorClass: "rate_limit"}},
		ImageCooldowns:             []accevents.ImageCooldownView{{Model: "gpt-image-2", Until: "2026-07-27T01:03:03Z", ErrorClass: "timeout"}},
		AccessToken:                "token-very-secret",
		Proxy:                      "http://private.invalid",
	}}, nil
}
func (s *chatGPTAccountRuntimeStub) AddChatGPTAccounts(_ context.Context, tokens []string, _ string) (accevents.AddResult, error) {
	s.addedTokens = append([]string(nil), tokens...)
	return accevents.AddResult{Added: len(tokens)}, nil
}
func (s *chatGPTAccountRuntimeStub) DeleteChatGPTAccounts(_ context.Context, ids []string) (accevents.DeleteResult, error) {
	s.deletedIDs = append([]string(nil), ids...)
	return accevents.DeleteResult{Deleted: len(ids)}, nil
}
func (s *chatGPTAccountRuntimeStub) UpdateChatGPTAccount(_ context.Context, command accevents.UpdateByIDCommand) (accevents.UpdateResult, error) {
	s.updated = command
	return accevents.UpdateResult{Item: accevents.AccountView{ID: command.ID, AccessToken: "token-very-secret", Proxy: "http://private.invalid"}}, nil
}
func (s *chatGPTAccountRuntimeStub) ExportChatGPTAccounts(_ context.Context, ids []string) (accevents.ExportResult, error) {
	s.exportedIDs = append([]string(nil), ids...)
	return accevents.ExportResult{Items: append([]accevents.ExportItem(nil), s.exportedItems...)}, nil
}
func (s *chatGPTAccountRuntimeStub) RefreshChatGPTAccountsByID(context.Context, []string) (accevents.RefreshResult, error) {
	return accevents.RefreshResult{}, nil
}
func (s *chatGPTAccountRuntimeStub) ChatGPTAccountRefreshProgress(context.Context, string) (accevents.RefreshProgress, error) {
	return accevents.RefreshProgress{}, nil
}
func (s *chatGPTAccountRuntimeStub) StartChatGPTOAuth(context.Context, string) (accevents.OAuthStartResult, error) {
	return accevents.OAuthStartResult{}, nil
}
func (s *chatGPTAccountRuntimeStub) FinishChatGPTOAuth(context.Context, string, string) (accevents.OAuthFinishResult, error) {
	return accevents.OAuthFinishResult{}, nil
}
func (s *chatGPTAccountRuntimeStub) ListChatGPTImages(context.Context, string, string, string) (imgevents.ListResult, error) {
	if s.imageListErr != nil {
		return imgevents.ListResult{}, s.imageListErr
	}
	if len(s.imageList.Items) > 0 {
		return s.imageList, nil
	}
	return imgevents.ListResult{}, nil
}
func (s *chatGPTAccountRuntimeStub) ChatGPTImageStorage(context.Context) (imgevents.StorageStatsResult, error) {
	return imgevents.StorageStatsResult{}, nil
}
func (s *chatGPTAccountRuntimeStub) ListChatGPTImageTags(context.Context) (imgevents.ListTagsResult, error) {
	return imgevents.ListTagsResult{}, nil
}
func (s *chatGPTAccountRuntimeStub) SetChatGPTImageTags(context.Context, string, []string) (imgevents.SetTagsResult, error) {
	return imgevents.SetTagsResult{}, nil
}
func (s *chatGPTAccountRuntimeStub) DeleteChatGPTImages(context.Context, []string) (imgevents.DeleteResult, error) {
	return imgevents.DeleteResult{}, nil
}
func (s *chatGPTAccountRuntimeStub) GetChatGPTImageBytes(_ context.Context, path string) ([]byte, error) {
	s.imageBytes = append(s.imageBytes, path)
	if s.imageBytesErr != nil {
		return nil, s.imageBytesErr
	}
	return []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}, nil
}
func (s *chatGPTAccountRuntimeStub) GetChatGPTImageThumbnail(_ context.Context, path string) ([]byte, error) {
	s.imageThumbs = append(s.imageThumbs, path)
	if s.imageBytesErr != nil {
		return nil, s.imageBytesErr
	}
	return []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}, nil
}
func (s *chatGPTAccountRuntimeStub) SubmitChatGPTImageGeneration(context.Context, taskevents.SubmitGenerationCommand) (taskevents.SubmitResult, error) {
	return taskevents.SubmitResult{}, nil
}
func (s *chatGPTAccountRuntimeStub) SubmitChatGPTImageEdit(context.Context, taskevents.SubmitEditCommand) (taskevents.SubmitResult, error) {
	return taskevents.SubmitResult{}, nil
}
func (s *chatGPTAccountRuntimeStub) ListChatGPTImageTasks(context.Context, string, []string) (taskevents.ListResult, error) {
	return taskevents.ListResult{}, nil
}
func (s *chatGPTAccountRuntimeStub) ResumeChatGPTImageTask(context.Context, string, string, int) (taskevents.ResumePollResult, error) {
	return taskevents.ResumePollResult{}, nil
}
func (s *chatGPTAccountRuntimeStub) RetryChatGPTImageGeneration(_ context.Context, ownerID, taskID, baseURL string) (taskevents.RetryGenerationResult, error) {
	s.retryOwner, s.retryTaskID, s.retryBaseURL = ownerID, taskID, baseURL
	return taskevents.RetryGenerationResult{Task: taskevents.TaskView{ID: taskID, Status: taskevents.StatusQueued, Mode: "generate", Progress: "retrying_submission"}}, nil
}
func (s *chatGPTAccountRuntimeStub) ChatGPTEffectiveCatalog(context.Context) (effectivecatalog.Snapshot, error) {
	return effectivecatalog.Empty(), nil
}

func (s *chatGPTAccountRuntimeStub) CreateTemporaryConversation(_ context.Context, command tempevents.CreateConversationCommand) (tempevents.ConversationResult, error) {
	s.temporaryCreate = command
	if s.temporaryCreateErr != nil {
		return tempevents.ConversationResult{}, s.temporaryCreateErr
	}
	return tempevents.ConversationResult{Conversation: tempevents.ConversationView{ID: "conversation-1", Model: command.Model, Status: tempevents.StatusIdle}}, nil
}
func (s *chatGPTAccountRuntimeStub) ListTemporaryConversations(context.Context, tempevents.ListConversationsCommand) (tempevents.ListConversationsResult, error) {
	return tempevents.ListConversationsResult{}, nil
}
func (s *chatGPTAccountRuntimeStub) GetTemporaryConversation(_ context.Context, command tempevents.GetConversationCommand) (tempevents.ConversationDetailResult, error) {
	s.temporaryGet = command
	if s.temporaryGetErr != nil {
		return tempevents.ConversationDetailResult{}, s.temporaryGetErr
	}
	return tempevents.ConversationDetailResult{Conversation: tempevents.ConversationView{ID: command.ConversationID, Status: tempevents.StatusIdle}}, nil
}
func (s *chatGPTAccountRuntimeStub) GetTemporaryMessageImage(context.Context, tempevents.GetMessageImageCommand) (tempevents.GetMessageImageResult, error) {
	return tempevents.GetMessageImageResult{Bytes: []byte{0x89, 0x50, 0x4e, 0x47}, ContentType: "image/png"}, nil
}
func (s *chatGPTAccountRuntimeStub) GetTemporaryMessageAttachment(context.Context, tempevents.GetMessageAttachmentCommand) (tempevents.GetMessageAttachmentResult, error) {
	return tempevents.GetMessageAttachmentResult{}, nil
}
func (s *chatGPTAccountRuntimeStub) StartTemporaryTurn(_ context.Context, command tempevents.StartTurnCommand) (tempevents.StartTurnResult, error) {
	s.temporaryTurn = command
	return tempevents.StartTurnResult{Conversation: tempevents.ConversationView{ID: command.ConversationID}, UserMessage: tempevents.MessageView{ID: "message-1", Content: command.Content}, AssistantMessage: tempevents.MessageView{ID: "turn-1"}, TurnID: "turn-1"}, nil
}
func (s *chatGPTAccountRuntimeStub) PullTemporaryTurn(context.Context, tempevents.PullTurnCommand) (tempevents.PullTurnResult, error) {
	return tempevents.PullTurnResult{}, nil
}
func (s *chatGPTAccountRuntimeStub) CancelTemporaryTurn(context.Context, tempevents.CancelTurnCommand) (tempevents.CancelTurnResult, error) {
	return tempevents.CancelTurnResult{}, nil
}
func (s *chatGPTAccountRuntimeStub) DeleteTemporaryConversation(context.Context, tempevents.DeleteConversationCommand) (tempevents.DeleteConversationResult, error) {
	return tempevents.DeleteConversationResult{}, nil
}

func TestChatGPTAccountAdminUsesStableIDsAndRedactsList(t *testing.T) {
	runtime := &chatGPTAccountRuntimeStub{}
	handler := NewHandler("", &testRuntime{}).WithChatGPTRuntime(runtime)

	list := httptest.NewRequest(http.MethodGet, "/admin/api/chatgpt/accounts", nil)
	list.RemoteAddr = "127.0.0.1:1234"
	listRecorder := httptest.NewRecorder()
	handler.ServeHTTP(listRecorder, list)
	body := listRecorder.Body.String()
	if listRecorder.Code != http.StatusOK || strings.Contains(body, "very-secret") || strings.Contains(body, "private.invalid") {
		t.Fatalf("list=%d %s", listRecorder.Code, listRecorder.Body.String())
	}
	for _, field := range []string{`"restore_at":"2026-07-27T01:02:03Z"`, `"image_inflight":1`, `"success":9`, `"fail":2`, `"created_at":"2026-07-26T01:02:03Z"`, `"last_token_refresh_at":"2026-07-27T00:30:00Z"`, `"last_token_refresh_error_at":"2026-07-27T01:00:00Z"`, `"last_token_refresh_error_class":"rate_limit"`, `"text_cooldowns":[{"model":"gpt-5","until":"2026-07-27T01:03:03Z","error_class":"rate_limit"}]`, `"image_cooldowns":[{"model":"gpt-image-2","until":"2026-07-27T01:03:03Z","error_class":"timeout"}]`} {
		if !strings.Contains(body, field) {
			t.Fatalf("list response missing account operations field %q: %s", field, body)
		}
	}

	add := httptest.NewRequest(http.MethodPost, "/admin/api/chatgpt/accounts", strings.NewReader(`{"tokens":["new-token"]}`))
	add.RemoteAddr = "127.0.0.1:1234"
	add.Header.Set("X-AI-Proxy-Admin", "1")
	addRecorder := httptest.NewRecorder()
	handler.ServeHTTP(addRecorder, add)
	if addRecorder.Code != http.StatusCreated || len(runtime.addedTokens) != 1 {
		t.Fatalf("add=%d tokens=%v", addRecorder.Code, runtime.addedTokens)
	}

	update := httptest.NewRequest(http.MethodPatch, "/admin/api/chatgpt/accounts/account-1", strings.NewReader(`{"status":"禁用","proxy":""}`))
	update.RemoteAddr = "127.0.0.1:1234"
	update.Header.Set("X-AI-Proxy-Admin", "1")
	updateRecorder := httptest.NewRecorder()
	handler.ServeHTTP(updateRecorder, update)
	if updateRecorder.Code != http.StatusOK || runtime.updated.ID != "account-1" || runtime.updated.Status == nil || *runtime.updated.Status != "禁用" || runtime.updated.Proxy == nil || *runtime.updated.Proxy != "" || strings.Contains(updateRecorder.Body.String(), "very-secret") {
		t.Fatalf("update=%d command=%+v body=%s", updateRecorder.Code, runtime.updated, updateRecorder.Body.String())
	}
}

func TestChatGPTImageTaskRetryGeneration(t *testing.T) {
	runtime := &chatGPTAccountRuntimeStub{}
	handler := NewHandler("", &testRuntime{}).WithChatGPTRuntime(runtime)
	req := httptest.NewRequest(http.MethodPost, "/admin/api/chatgpt/image-tasks/task-1/retry-generation", strings.NewReader(`{"owner_id":"owner-1"}`))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AI-Proxy-Admin", "1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || runtime.retryOwner != "owner-1" || runtime.retryTaskID != "task-1" || runtime.retryBaseURL != "http://example.com" || !strings.Contains(rec.Body.String(), `"retrying_submission"`) {
		t.Fatalf("retry status=%d owner=%q task=%q base=%q body=%s", rec.Code, runtime.retryOwner, runtime.retryTaskID, runtime.retryBaseURL, rec.Body.String())
	}
}

func TestChatGPTAccountExportUsesCompatiblePayloadShape(t *testing.T) {
	runtime := &chatGPTAccountRuntimeStub{exportedItems: []accevents.ExportItem{{
		Type: "codex", Email: "export@example.invalid", AccountID: "account-export",
		AccessToken: "access-export", RefreshToken: "refresh-export", IDToken: "id-export",
	}}}
	handler := NewHandler("", &testRuntime{}).WithChatGPTRuntime(runtime)
	req := httptest.NewRequest(http.MethodPost, "/admin/api/chatgpt/accounts/export", strings.NewReader(`{"ids":["account-export"]}`))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AI-Proxy-Admin", "1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" || !strings.Contains(rec.Header().Get("Content-Disposition"), "codex-accounts.json") || len(runtime.exportedIDs) != 1 || runtime.exportedIDs[0] != "account-export" {
		t.Fatalf("export status=%d headers=%v ids=%v body=%s", rec.Code, rec.Header(), runtime.exportedIDs, rec.Body.String())
	}
	var item accevents.ExportItem
	if err := json.Unmarshal(rec.Body.Bytes(), &item); err != nil || item.AccessToken != "access-export" || strings.Contains(rec.Body.String(), `"items"`) {
		t.Fatalf("export payload=%s err=%v item=%+v", rec.Body.String(), err, item)
	}

	emptyHandler := NewHandler("", &testRuntime{}).WithChatGPTRuntime(&chatGPTAccountRuntimeStub{})
	emptyReq := httptest.NewRequest(http.MethodPost, "/admin/api/chatgpt/accounts/export", strings.NewReader(`{"ids":["account-empty"]}`))
	emptyReq.RemoteAddr = "127.0.0.1:1234"
	emptyReq.Header.Set("X-AI-Proxy-Admin", "1")
	emptyRec := httptest.NewRecorder()
	emptyHandler.ServeHTTP(emptyRec, emptyReq)
	if emptyRec.Code != http.StatusBadRequest || !strings.Contains(emptyRec.Body.String(), "no complete accounts") {
		t.Fatalf("empty export status=%d body=%s", emptyRec.Code, emptyRec.Body.String())
	}
}

func TestChatGPTImageListRewritesToAdminContentURLs(t *testing.T) {
	runtime := &chatGPTAccountRuntimeStub{
		imageList: imgevents.ListResult{Items: []imgevents.ImageItem{{
			Path: "2026-07-26/demo.png",
			Name: "demo.png",
			URL:  "http://evil.example/images/2026-07-26/demo.png",
		}}},
	}
	handler := NewHandler("", &testRuntime{}).WithChatGPTRuntime(runtime)
	req := httptest.NewRequest(http.MethodGet, "/admin/api/chatgpt/images", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("list images=%d %s", rec.Code, body)
	}
	if strings.Contains(body, "evil.example") || strings.Contains(body, `"/images/`) {
		t.Fatalf("list still exposes public image URLs: %s", body)
	}
	if !strings.Contains(body, "/admin/api/chatgpt/images/content?path=") || !strings.Contains(body, "thumb=1") {
		t.Fatalf("list missing admin content URLs: %s", body)
	}
	var response struct {
		Items []imgevents.ImageItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || len(response.Items) != 1 || response.Items[0].Path != "2026-07-26/demo.png" {
		t.Fatalf("list JSON shape is not consumable: items=%#v err=%v body=%s", response.Items, err, body)
	}
}

func TestChatGPTImageContentEndpoint(t *testing.T) {
	runtime := &chatGPTAccountRuntimeStub{}
	handler := NewHandler("", &testRuntime{}).WithChatGPTRuntime(runtime)

	okReq := httptest.NewRequest(http.MethodGet, "/admin/api/chatgpt/images/content?path=2026-07-26%2Fdemo.png&thumb=1", nil)
	okReq.RemoteAddr = "127.0.0.1:1234"
	okRec := httptest.NewRecorder()
	handler.ServeHTTP(okRec, okReq)
	if okRec.Code != http.StatusOK || okRec.Header().Get("Cache-Control") != "no-store" || len(runtime.imageThumbs) != 1 || runtime.imageThumbs[0] != "2026-07-26/demo.png" {
		t.Fatalf("content ok=%d cache=%q thumbs=%v body=%d", okRec.Code, okRec.Header().Get("Cache-Control"), runtime.imageThumbs, okRec.Body.Len())
	}

	badReq := httptest.NewRequest(http.MethodGet, "/admin/api/chatgpt/images/content?path=../secret.png", nil)
	badReq.RemoteAddr = "127.0.0.1:1234"
	badRec := httptest.NewRecorder()
	handler.ServeHTTP(badRec, badReq)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("traversal path should be rejected, got %d %s", badRec.Code, badRec.Body.String())
	}

	absReq := httptest.NewRequest(http.MethodGet, "/admin/api/chatgpt/images/content?path=%2Fetc%2Fpasswd", nil)
	absReq.RemoteAddr = "127.0.0.1:1234"
	absRec := httptest.NewRecorder()
	handler.ServeHTTP(absRec, absReq)
	if absRec.Code != http.StatusBadRequest {
		t.Fatalf("absolute path should be rejected, got %d %s", absRec.Code, absRec.Body.String())
	}

	nilHandler := NewHandler("", &testRuntime{})
	unavail := httptest.NewRequest(http.MethodGet, "/admin/api/chatgpt/images/content?path=a.png", nil)
	unavail.RemoteAddr = "127.0.0.1:1234"
	unavailRec := httptest.NewRecorder()
	nilHandler.ServeHTTP(unavailRec, unavail)
	if unavailRec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil runtime should 503, got %d %s", unavailRec.Code, unavailRec.Body.String())
	}
}

func TestChatGPTAdminReturnsUnavailableWhenFeatureDisabled(t *testing.T) {
	handler := NewHandler("", &testRuntime{}).WithChatGPTRuntime(unavailableChatGPTRuntimeStub{&chatGPTAccountRuntimeStub{}})
	req := httptest.NewRequest(http.MethodPost, "/admin/api/chatgpt/accounts", strings.NewReader(`{"tokens":["redacted"]}`))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AI-Proxy-Admin", "1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "chatgpt web is not enabled") {
		t.Fatalf("disabled feature should return 503, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestTemporaryChatAdminUsesServerOwnerAndNoStore(t *testing.T) {
	runtime := &chatGPTAccountRuntimeStub{}
	handler := NewHandler("", &testRuntime{}).WithChatGPTRuntime(runtime)
	req := httptest.NewRequest(http.MethodPost, "/admin/api/chatgpt/temporary-conversations", strings.NewReader(`{"model":"gpt-5","owner_id":"forged-owner"}`))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AI-Proxy-Admin", "1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated || rec.Header().Get("Cache-Control") != "no-store" || runtime.temporaryCreate.OwnerID != "admin" || runtime.temporaryCreate.Model != "gpt-5" {
		t.Fatalf("status=%d cache=%q command=%+v body=%s", rec.Code, rec.Header().Get("Cache-Control"), runtime.temporaryCreate, rec.Body.String())
	}
}

func TestTemporaryChatExpiredConversationMapsToGone(t *testing.T) {
	runtime := &chatGPTAccountRuntimeStub{temporaryGetErr: fmt.Errorf("conversation expired")}
	handler := NewHandler("", &testRuntime{}).WithChatGPTRuntime(runtime)
	req := httptest.NewRequest(http.MethodGet, "/admin/api/chatgpt/temporary-conversations/conversation-1", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusGone || rec.Header().Get("Cache-Control") != "no-store" || runtime.temporaryGet.OwnerID != "admin" {
		t.Fatalf("status=%d cache=%q command=%+v body=%s", rec.Code, rec.Header().Get("Cache-Control"), runtime.temporaryGet, rec.Body.String())
	}
}

func TestTemporaryChatTurnAcceptsMultipartImagesAndServesOwnerScopedContent(t *testing.T) {
	runtime := &chatGPTAccountRuntimeStub{}
	handler := NewHandler("", &testRuntime{}).WithChatGPTRuntime(runtime)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("content", "inspect this"); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("images", "sample.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41, 0x54, 0x08, 0xd7, 0x63, 0xf8, 0xcf, 0xc0, 0xf0, 0x1f, 0x00, 0x05, 0x80, 0x02, 0x3f, 0x91, 0xc3, 0xf3, 0xe1, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/chatgpt/temporary-conversations/conversation-1/turns", &body)
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-AI-Proxy-Admin", "1")
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || runtime.temporaryTurn.OwnerID != "admin" || runtime.temporaryTurn.Content != "inspect this" || len(runtime.temporaryTurn.Images) != 1 || runtime.temporaryTurn.Images[0].ContentType != "image/png" {
		t.Fatalf("status=%d command=%+v body=%s", rec.Code, runtime.temporaryTurn, rec.Body.String())
	}

	imageReq := httptest.NewRequest(http.MethodGet, "/admin/api/chatgpt/temporary-conversations/conversation-1/messages/message-1/images/image-1", nil)
	imageReq.RemoteAddr = "127.0.0.1:1234"
	imageRec := httptest.NewRecorder()
	handler.ServeHTTP(imageRec, imageReq)
	if imageRec.Code != http.StatusOK || imageRec.Header().Get("Content-Type") != "image/png" || imageRec.Header().Get("Cache-Control") != "no-store" || imageRec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("image status=%d headers=%v body=%x", imageRec.Code, imageRec.Header(), imageRec.Body.Bytes())
	}
}
