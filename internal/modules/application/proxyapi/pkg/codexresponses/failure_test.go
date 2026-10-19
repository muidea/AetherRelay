package codexresponses

import (
	"errors"
	"fmt"
	"testing"
)

func TestAsFailureRejectsTypedNilAndPreservesFailures(t *testing.T) {
	var empty *Failure
	failure := NewFailure(KindTimeout, 0, nil)
	for _, tc := range []struct {
		name string
		err  error
		want *Failure
	}{
		{"nil", nil, nil},
		{"typed nil", empty, nil},
		{"wrapped typed nil", fmt.Errorf("wrapped: %w", empty), nil},
		{"ordinary", errors.New("other"), nil},
		{"failure", failure, failure},
		{"wrapped failure", fmt.Errorf("wrapped: %w", failure), failure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := AsFailure(tc.err)
			if got != tc.want || ok != (tc.want != nil) {
				t.Fatalf("got=(%p,%v) want=%p", got, ok, tc.want)
			}
		})
	}
}
