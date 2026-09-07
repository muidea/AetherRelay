package proxy

import (
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"aetherrelay/internal/modules/application/proxyapi/pkg/effectivecatalog"
	"aetherrelay/internal/pkg/aetherrelayclientaccess"
	config "aetherrelay/internal/pkg/aetherrelayconfig"
	usage "aetherrelay/internal/pkg/aetherrelayusage"
)

var codexProfileCases = []struct {
	id, defaultEffort, multiAgent string
	maxContext                    int
	lite                          bool
	efforts                       []string
}{
	{"gpt-6-astra", "medium", "v2", 872000, true, []string{"low", "medium", "high", "xhigh", "max", "ultra"}},
	{"gpt-5.6-sol", "low", "v2", 872000, true, []string{"low", "medium", "high", "xhigh", "max", "ultra"}},
	{"gpt-5.6-terra", "medium", "v2", 872000, true, []string{"low", "medium", "high", "xhigh", "max", "ultra"}},
	{"gpt-5.6-luna", "medium", "v1", 872000, true, []string{"low", "medium", "high", "xhigh", "max"}},
	{"gpt-5.5", "medium", "", 272000, false, []string{"low", "medium", "high", "xhigh"}},
	{"gpt-5.4-mini", "medium", "", 272000, false, []string{"low", "medium", "high", "xhigh"}},
}

func TestCodexProfilesAndExampleMetadataAgree(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source")
	}
	path := filepath.Join(filepath.Dir(source), "..", "..", "..", "..", "..", "..", "config.example.yaml")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range codexProfileCases {
		t.Run(tc.id, func(t *testing.T) {
			metadata := cfg.ModelMetadata[tc.id]
			if metadata.ContextWindowTokens != 272000 || metadata.MaxContextWindowTokens != tc.maxContext || metadata.MaxOutputTokens != 128000 ||
				!metadata.ReasoningDeclared || !metadata.ReasoningSupported || metadata.ReasoningDefaultEffort != tc.defaultEffort || !reflect.DeepEqual(metadata.ReasoningEfforts, tc.efforts) ||
				!metadata.NativeResponsesDeclared || !metadata.NativeResponsesTools || !metadata.NativeResponsesImages {
				t.Fatalf("example metadata=%+v", metadata)
			}
			if tc.id != "gpt-5.6-luna" && len(metadata.ConversionCapabilities) != 0 {
				t.Fatalf("new conversion capabilities: %+v", metadata.ConversionCapabilities)
			}
			for _, withMetadata := range []bool{false, true} {
				input := config.Config{}
				if withMetadata {
					input = cfg
				}
				snap := effectivecatalog.BuildWithCodex(input, effectivecatalog.CatalogInput{}, effectivecatalog.CatalogInput{
					Version: 1, AvailableAccounts: 1, Models: []effectivecatalog.PoolModel{{ID: tc.id}},
				})
				manifest := buildCodexModelsManifest(snap, clientaccess.All(), "0.153.4")
				if len(manifest.Models) != 1 {
					t.Fatalf("manifest=%+v", manifest)
				}
				model := manifest.Models[0]
				efforts := make([]string, 0, len(model.SupportedReasoningLevels))
				for _, level := range model.SupportedReasoningLevels {
					efforts = append(efforts, level.Effort)
				}
				if model.Slug != tc.id || model.ContextWindow != 272000 || model.MaxContextWindow != tc.maxContext || model.UseResponsesLite != tc.lite ||
					model.DefaultReasoningLevel != tc.defaultEffort || !reflect.DeepEqual(efforts, tc.efforts) || model.MultiAgentVersion != tc.multiAgent ||
					!model.PreferWebsockets || !model.SupportsSearchTool || !model.SupportsImageDetailOriginal || !reflect.DeepEqual(model.InputModalities, []string{"text", "image"}) {
					t.Fatalf("withMetadata=%t model=%+v", withMetadata, model)
				}
				if tc.id != "gpt-6-astra" && model.MultiAgentReasoningEffort != "" {
					t.Fatalf("invented multi-agent effort: %+v", model)
				}
				if withMetadata {
					record := buildModelsListResponse(snap, clientaccess.All()).Data[0]
					if record.ContextWindowTokens != model.ContextWindow || record.MaxContextWindowTokens != model.MaxContextWindow || record.MaxOutputTokens != 128000 ||
						record.Capabilities.Codex.FunctionTools != "supported" || record.Capabilities.Codex.ImageInput != "supported" {
						t.Fatalf("models list disagrees: %+v", record)
					}
					handler := NewHandler(mustHandlerConfig(input), usage.NewMemoryStore(), nil, nil)
					for _, effort := range efforts {
						if err := handler.applyModelReasoning(tc.id, map[string]any{"reasoning": map[string]any{"effort": effort}}); err != nil {
							t.Fatalf("advertised effort %s rejected: %v", effort, err)
						}
					}
				}
				old := buildCodexModelsManifest(snap, clientaccess.All(), "0.143.0").Models[0]
				for _, level := range old.SupportedReasoningLevels {
					if level.Effort == "max" || level.Effort == "ultra" {
						t.Fatalf("old client received unsupported effort: %+v", old)
					}
				}
			}
		})
	}
	// Profiles and metadata never create routable models themselves.
	empty := effectivecatalog.BuildWithCodex(cfg, effectivecatalog.CatalogInput{}, effectivecatalog.CatalogInput{})
	if len(buildCodexModelsManifest(empty, clientaccess.All(), "0.153.4").Models) != 0 {
		t.Fatal("metadata created models without a route")
	}
}

func TestCodexProfilesHonorExplicitRestrictions(t *testing.T) {
	for _, tc := range codexProfileCases {
		for _, enabled := range []bool{false, true} {
			metadata := config.ModelMetadata{
				ID: tc.id, ContextWindowTokens: 64000, MaxContextWindowTokens: 128000,
				ReasoningDeclared: true, ReasoningSupported: enabled,
				NativeResponsesDeclared: true, NativeResponsesImages: false,
			}
			if enabled {
				metadata.ReasoningDefaultEffort, metadata.ReasoningEfforts = "high", []string{"low", "high"}
			}
			cfg := config.Config{ModelMetadata: map[string]config.ModelMetadata{tc.id: metadata}}
			snap := effectivecatalog.BuildWithCodex(cfg, effectivecatalog.CatalogInput{}, effectivecatalog.CatalogInput{Version: 1, AvailableAccounts: 1, Models: []effectivecatalog.PoolModel{{ID: tc.id}}})
			model := buildCodexModelsManifest(snap, clientaccess.All(), "0.153.4").Models[0]
			if model.ContextWindow != 64000 || model.MaxContextWindow != 128000 || model.DefaultReasoningLevel != metadata.ReasoningDefaultEffort ||
				len(model.SupportedReasoningLevels) != len(metadata.ReasoningEfforts) || !reflect.DeepEqual(model.InputModalities, []string{"text"}) || model.SupportsImageDetailOriginal {
				t.Fatalf("explicit restrictions ignored: %+v", model)
			}
		}
	}
}

func TestCodexProfilesRequireExactKnownID(t *testing.T) {
	for _, id := range []string{"gpt-6", "GPT-6-ASTRA", "gpt-6-astra-new", "gpt-5.6", "gpt-unknown"} {
		if _, known := trustedCodexModelProfile(id); known {
			t.Fatalf("unknown model %q got a trusted profile", id)
		}
		snap := effectivecatalog.BuildWithCodex(config.Config{}, effectivecatalog.CatalogInput{}, effectivecatalog.CatalogInput{Version: 1, AvailableAccounts: 1, Models: []effectivecatalog.PoolModel{{ID: id}}})
		model := buildCodexModelsManifest(snap, clientaccess.All(), "0.153.4").Models[0]
		if model.Slug != id || model.ContextWindow != 128000 || model.MaxContextWindow != 128000 || model.UseResponsesLite || model.SupportsSearchTool {
			t.Fatalf("unknown model was rewritten or expanded: %+v", model)
		}
	}
}
