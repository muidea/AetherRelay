package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	accevents "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/events"
	taskevents "ai-proxy/internal/modules/application/chatgptimagetask/pkg/events"
	"ai-proxy/internal/modules/application/proxyapi/pkg/effectivecatalog"
	imgevents "ai-proxy/internal/modules/blocks/chatgptimagestore/pkg/events"
)

type chatGPTAccountRuntimeStub struct {
	addedTokens   []string
	deletedIDs    []string
	updated       accevents.UpdateByIDCommand
	imageBytes    []string
	imageThumbs   []string
	imageList     imgevents.ListResult
	imageListErr  error
	imageBytesErr error
	exportedIDs   []string
	exportedItems []accevents.ExportItem
}

type unavailableChatGPTRuntimeStub struct{ *chatGPTAccountRuntimeStub }

func (unavailableChatGPTRuntimeStub) ChatGPTWebEnabled() bool { return false }

func (s *chatGPTAccountRuntimeStub) ListChatGPTAccounts(context.Context) ([]accevents.AccountView, error) {
	return []accevents.AccountView{{
		ID:            "account-1",
		Email:         "operator@example.invalid",
		Status:        "正常",
		Quota:         7,
		RestoreAt:     "2026-07-27T01:02:03Z",
		ImageInflight: 1,
		Success:       9,
		Fail:          2,
		CreatedAt:     "2026-07-26T01:02:03Z",
		AccessToken:   "token-very-secret",
		Proxy:         "http://private.invalid",
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
func (s *chatGPTAccountRuntimeStub) ChatGPTEffectiveCatalog(context.Context) (effectivecatalog.Snapshot, error) {
	return effectivecatalog.Empty(), nil
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
	for _, field := range []string{`"restore_at":"2026-07-27T01:02:03Z"`, `"image_inflight":1`, `"success":9`, `"fail":2`, `"created_at":"2026-07-26T01:02:03Z"`} {
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
