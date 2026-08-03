package proxy

import (
	"context"
	"testing"

	"ai-proxy/internal/pkg/aiproxyconfig"
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
