package client

import (
	"testing"
)

func TestParseModelsResponseProjectsChatAndImageOperations(t *testing.T) {
	body := []byte(`{
  "models": [
    {
      "slug": "gpt-5",
      "created": 1710000000,
      "owned_by": "openai",
      "tags": ["gpt"]
    },
    {
      "slug": "gpt-image-2",
      "created": 1720000000,
      "owned_by": "openai",
      "tags": ["image_gen"],
      "product_features": {"image_gen": {"enabled": true}},
      "enabled_tools": ["image_generation"]
    },
    {
      "slug": "image-only-model",
      "tags": ["image_generation_only", "image_gen"],
      "product_features": {"image_generation": true}
    },
    {
      "slug": "no-ops-unknown",
      "tags": [],
      "product_features": {}
    },
    {
      "slug": "gpt-5",
      "created": 1
    },
	{
	  "id": "id-only-model",
	  "created": 3
	},
    {
      "slug": "  ",
      "id": ""
    }
  ]
}`)
	models, err := ParseModelsResponse(body)
	if err != nil {
		t.Fatalf("ParseModelsResponse: %v", err)
	}
	byID := map[string]ModelDescriptor{}
	for _, model := range models {
		byID[model.ID] = model
	}
	if len(byID) != 4 {
		t.Fatalf("expected 4 projected models, got %d: %+v", len(byID), models)
	}
	if got := byID["gpt-5"]; got.ID == "" || !hasOp(got, ModelOperationChatCompletions) || hasOp(got, ModelOperationImageGenerations) || got.CreatedAt != 1710000000 || got.OwnedBy != "openai" {
		t.Fatalf("gpt-5 projection=%+v", got)
	}
	if got := byID["gpt-image-2"]; !hasOp(got, ModelOperationChatCompletions) || !hasOp(got, ModelOperationImageGenerations) {
		t.Fatalf("gpt-image-2 should support chat+image, got %+v", got)
	}
	if got := byID["image-only-model"]; hasOp(got, ModelOperationChatCompletions) || !hasOp(got, ModelOperationImageGenerations) {
		t.Fatalf("image-only-model projection=%+v", got)
	}
	if _, exists := byID["id-only-model"]; exists {
		t.Fatal("id-only upstream entry must not be projected")
	}
	if got := byID["no-ops-unknown"]; !hasOp(got, ModelOperationChatCompletions) || hasOp(got, ModelOperationImageGenerations) {
		// listed by /backend-api/models with a slug => chat_completions only
		t.Fatalf("no-ops-unknown projection=%+v", got)
	}
}

func TestParseModelsResponseRejectsInvalidJSON(t *testing.T) {
	if _, err := ParseModelsResponse([]byte(`{`)); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestParseModelsResponseDedupsAndSorts(t *testing.T) {
	body := []byte(`{"models":[{"slug":"z-model"},{"slug":"a-model"},{"slug":"a-model"}]}`)
	models, err := ParseModelsResponse(body)
	if err != nil {
		t.Fatalf("ParseModelsResponse: %v", err)
	}
	if len(models) != 2 || models[0].ID != "a-model" || models[1].ID != "z-model" {
		t.Fatalf("sorted models=%+v", models)
	}
}

func hasOp(model ModelDescriptor, op ModelOperation) bool {
	for _, item := range model.Operations {
		if item == op {
			return true
		}
	}
	return false
}
