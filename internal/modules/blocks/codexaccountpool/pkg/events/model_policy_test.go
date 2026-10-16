package events

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestModelBlacklistJSONPresenceAndValidation(t *testing.T) {
	for _, tc := range []struct {
		body    string
		want    ModelBlacklist
		invalid bool
	}{
		{body: `{}`},
		{body: `{"blocked_models":[]}`, want: ModelBlacklist{}},
		{body: `{"blocked_models":[" z ","a","z","A"]}`, want: ModelBlacklist{"A", "a", "z"}},
		{body: `{"blocked_models":null}`, invalid: true},
		{body: `{"blocked_models":"gpt-sol"}`, invalid: true},
		{body: `{"blocked_models":[null]}`, invalid: true},
		{body: `{"blocked_models":[" "]}`, invalid: true},
		{body: `{"blocked_models":["bad\nmodel"]}`, invalid: true},
		{body: `{"blocked_models":["` + strings.Repeat("字", 86) + `"]}`, invalid: true},
	} {
		var input CredentialInput
		err := json.Unmarshal([]byte(tc.body), &input)
		if (err != nil) != tc.invalid || !tc.invalid && !reflect.DeepEqual(input.BlockedModels, tc.want) {
			t.Fatalf("body=%s got=%v err=%v", tc.body, input.BlockedModels, err)
		}
	}
	if _, err := NormalizeBlockedModels(make([]string, 257)); err == nil {
		t.Fatal("unbounded list accepted")
	}
	for _, policy := range []ModelBlacklist{nil, {}} {
		encoded, err := json.Marshal(CredentialInput{BlockedModels: policy})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), `"blocked_models":[]`) != (policy != nil) {
			t.Fatalf("policy presence lost during encoding: %s", encoded)
		}
	}
}
