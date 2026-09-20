package archive

import (
	"os"
	"testing"
	"time"
)

func TestObservationRoundNeverWrites(t *testing.T) {
	r := NewObservationRound()
	r.Dir = t.TempDir()
	r.SetConversionDegraded(true)
	r.SetTransportPlan("messages", "/v1/messages", "anthropic", "codexoauth", "codex_oauth_responses", "anthropic_to_codex_responses")
	r.SetConversionDuration(time.Millisecond)
	r.SetUpstreamHeaders(200, "", -1, "", time.Second)
	if r.FullContent() || !r.ConversionDegraded || r.ConversionLevel != 2 || r.UpstreamDuration != time.Second {
		t.Fatalf("round=%+v", r)
	}
	for _, err := range []error{r.WriteRequest([]byte(`{}`)), r.WriteJSON("request.meta.json", map[string]any{}), r.WriteResponse("response.json", []byte(`{}`)), r.WriteMetadata(Metadata{HTTPStatus: 200})} {
		if err != nil {
			t.Fatal(err)
		}
	}
	w, err := r.CreateResponseWriter("response.sse")
	if err != nil {
		t.Fatal(err)
	}
	if w != nil {
		if _, err = w.Write([]byte("private")); err != nil {
			t.Fatal(err)
		}
		if err = w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	files, err := os.ReadDir(r.Dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("archive disabled but files created: %v %v", files, err)
	}
}
