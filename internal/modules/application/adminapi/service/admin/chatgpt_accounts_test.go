package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	accevents "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/events"
	taskevents "ai-proxy/internal/modules/application/chatgptimagetask/pkg/events"
	imgevents "ai-proxy/internal/modules/blocks/chatgptimagestore/pkg/events"
)

type chatGPTAccountRuntimeStub struct {
	addedTokens []string
	deletedIDs  []string
	updated     accevents.UpdateByIDCommand
}

func (s *chatGPTAccountRuntimeStub) ListChatGPTAccounts(context.Context) ([]accevents.AccountView, error) {
	return []accevents.AccountView{{ID: "account-1", AccessToken: "token-very-secret", Proxy: "http://private.invalid"}}, nil
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
func (s *chatGPTAccountRuntimeStub) ExportChatGPTAccounts(context.Context, []string) (accevents.ExportResult, error) {
	return accevents.ExportResult{}, nil
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

func TestChatGPTAccountAdminUsesStableIDsAndRedactsList(t *testing.T) {
	runtime := &chatGPTAccountRuntimeStub{}
	handler := NewHandler("", &testRuntime{}).WithChatGPTRuntime(runtime)

	list := httptest.NewRequest(http.MethodGet, "/admin/api/chatgpt/accounts", nil)
	list.RemoteAddr = "127.0.0.1:1234"
	listRecorder := httptest.NewRecorder()
	handler.ServeHTTP(listRecorder, list)
	if listRecorder.Code != http.StatusOK || strings.Contains(listRecorder.Body.String(), "very-secret") || strings.Contains(listRecorder.Body.String(), "private.invalid") {
		t.Fatalf("list=%d %s", listRecorder.Code, listRecorder.Body.String())
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
