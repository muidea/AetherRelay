package clientaccess

import "testing"

func TestNormalizeSelected(t *testing.T) {
	policy, err := Selected([]string{" DeepSeek ", "codexoauth", "deepseek"})
	if err != nil {
		t.Fatal(err)
	}
	if policy.Mode != ModeSelected || len(policy.ProviderIDs) != 2 || policy.ProviderIDs[0] != "codexoauth" || policy.ProviderIDs[1] != "deepseek" {
		t.Fatalf("policy=%+v", policy)
	}
	if !policy.Allows("DEEPSEEK") || policy.Allows("openai") {
		t.Fatalf("unexpected access policy=%+v", policy)
	}
}

func TestPolicyRejectsAmbiguousOrEmptyShapes(t *testing.T) {
	for _, policy := range []Policy{
		{},
		{Mode: ModeAll, ProviderIDs: []string{"deepseek"}},
		{Mode: ModeSelected},
		{Mode: ModeSelected, ProviderIDs: []string{"/invalid"}},
	} {
		if _, err := Normalize(policy); err == nil {
			t.Fatalf("policy %+v unexpectedly accepted", policy)
		}
	}
	if (Policy{}).Allows("deepseek") {
		t.Fatal("zero policy must deny all providers")
	}
}

func TestCloneDoesNotShareProviderIDs(t *testing.T) {
	policy, err := Selected([]string{"deepseek"})
	if err != nil {
		t.Fatal(err)
	}
	clone := Clone(policy)
	clone.ProviderIDs[0] = "openai"
	if policy.ProviderIDs[0] != "deepseek" {
		t.Fatalf("source mutated: %+v", policy)
	}
}
