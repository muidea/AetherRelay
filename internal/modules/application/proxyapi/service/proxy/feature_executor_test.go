package proxy

import (
	"context"
	"errors"
	"testing"

	"ai-proxy/internal/modules/application/proxyapi/pkg/chatgptfail"
	"ai-proxy/internal/modules/application/proxyapi/pkg/chatgptimage"
	"ai-proxy/internal/modules/application/proxyapi/pkg/chatgpttext"
	proxyevents "ai-proxy/internal/modules/application/proxyapi/pkg/events"
	"ai-proxy/internal/pkg/aiproxyconfig"
	"ai-proxy/internal/pkg/aiproxyusage"
	"ai-proxy/internal/pkg/chatattachment"
	"ai-proxy/internal/pkg/chatgpttokenusage"
)

func TestFeatureCatalogUsesCapabilityCompatibleProviderChains(t *testing.T) {
	cfg := config.Config{
		Providers: map[string]config.Provider{
			"text-primary": {
				Name: "text-primary", Protocol: "openai", Models: []string{"shared-text"}, Priority: 200,
				EndpointCapabilities: []string{config.EndpointCapabilityChatCompletions},
			},
			"text-backup": {
				Name: "text-backup", Protocol: "anthropic", Models: []string{"shared-text"}, Priority: 100,
				EndpointCapabilities: []string{config.EndpointCapabilityMessages},
			},
			"image": {
				Name: "image", Protocol: "openai", Models: []string{"image-model"}, Priority: 150,
				EndpointCapabilities: []string{config.EndpointCapabilityImages},
			},
		},
		ModelCatalog: map[string]config.ModelInfo{
			"shared-text": {
				ID: "shared-text", ContextWindowTokens: 8192, MaxOutputTokens: 4096,
				Operations: []string{config.ModelOperationChatCompletions}, RouteOwner: "text-primary", RouteOwners: []string{"text-backup", "text-primary"},
			},
			"image-model": {
				ID: "image-model", ContextWindowTokens: 8192, MaxOutputTokens: 4096,
				Operations: []string{config.ModelOperationImageGenerations}, RouteOwner: "image", RouteOwners: []string{"image"},
			},
			"unroutable": {
				ID: "unroutable", ContextWindowTokens: 8192, MaxOutputTokens: 4096,
				Operations: []string{config.ModelOperationEmbeddings}, RouteOwner: "text-primary", RouteOwners: []string{"text-primary"},
			},
		},
	}
	h := &Handler{cfg: cfg}
	catalog := h.FeatureCatalog(context.Background())
	if len(catalog.TextModels) != 1 || catalog.TextModels[0].ID != "shared-text" {
		t.Fatalf("text models=%+v", catalog.TextModels)
	}
	providers := catalog.TextModels[0].Providers
	if len(providers) != 2 || providers[0].Name != "text-primary" || providers[1].Name != "text-backup" {
		t.Fatalf("text providers=%+v", providers)
	}
	if len(catalog.ImageModels) != 1 || catalog.ImageModels[0].ID != "image-model" {
		t.Fatalf("image models=%+v", catalog.ImageModels)
	}
	if len(catalog.ImageEditModels) != 1 || catalog.ImageEditModels[0].ID != "image-model" {
		t.Fatalf("image edit models=%+v", catalog.ImageEditModels)
	}
}

func TestExecuteFeatureTextSendsFilesThroughResponses(t *testing.T) {
	var received chatgpttext.Request
	exec := chatGPTTextExecutorStub{complete: func(_ context.Context, request chatgpttext.Request) (chatgpttext.Result, error) {
		received = request
		return chatgpttext.Result{Text: "done", ActualModel: "gpt-5"}, nil
	}}
	h := newChatGPTWebHandler(t, usage.NewMemoryStore(), exec)
	catalog := h.FeatureCatalog(context.Background())
	if len(catalog.TextModels) != 1 || len(catalog.TextModels[0].Providers) != 1 || !catalog.TextModels[0].Providers[0].SupportsFiles {
		t.Fatalf("catalog=%+v", catalog)
	}
	out, err := h.ExecuteFeatureText(context.Background(), proxyevents.ExecuteFeatureTextCommand{OwnerID: "owner", Model: "gpt-5", Messages: []proxyevents.FeatureTextMessage{{Role: "user", Files: []chatattachment.File{{Name: "notes.md", ContentType: "text/markdown", Bytes: []byte("# hello")}}}}})
	if err != nil || out.Text != "done" || len(received.Messages) != 1 || len(received.Messages[0].Files) != 1 || received.Messages[0].Files[0].Name != "notes.md" {
		t.Fatalf("out=%+v received=%+v err=%v", out, received, err)
	}
}

func TestExecuteFeatureImagePreservesChatGPTRecoveryMetadata(t *testing.T) {
	exec := chatGPTImageExecutorStub{generate: func(context.Context, chatgptimage.Request) (chatgptimage.Result, error) {
		return chatgptimage.Result{
			Created:        1,
			Data:           []chatgptimage.Data{{B64JSON: "aaa"}},
			Usage:          &tokenusage.Usage{TotalTokens: 8},
			ConversationID: "conversation-1",
			AccountID:      "account-1",
		}, nil
	}}
	h := newChatGPTImageHandler(t, usage.NewMemoryStore(), exec)
	out, err := h.ExecuteFeatureImage(context.Background(), proxyevents.ExecuteFeatureImageCommand{OwnerID: "owner", Model: "gpt-image-2", Prompt: "cat"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Provider != "chatgptweb" || out.ConversationID != "conversation-1" || out.AccountID != "account-1" || out.Usage == nil || out.Usage.TotalTokens != 8 || len(out.Data) != 1 {
		t.Fatalf("result=%+v", out)
	}
}

func TestExecuteFeatureImagePreservesPartialRecoveryMetadata(t *testing.T) {
	exec := chatGPTImageExecutorStub{generate: func(context.Context, chatgptimage.Request) (chatgptimage.Result, error) {
		return chatgptimage.Result{ConversationID: "conversation-1", AccountID: "account-1"}, chatgptfail.New(chatgptfail.KindUpstream, errors.New("timeout"))
	}}
	h := newChatGPTImageHandler(t, usage.NewMemoryStore(), exec)
	out, err := h.ExecuteFeatureImage(context.Background(), proxyevents.ExecuteFeatureImageCommand{OwnerID: "owner", Model: "gpt-image-2", Prompt: "cat"})
	if err == nil || out.Provider != "chatgptweb" || out.ConversationID != "conversation-1" || out.AccountID != "account-1" {
		t.Fatalf("result=%+v err=%v", out, err)
	}
}
