package aetherrelaytransport

import "testing"

func TestWebsocketDiagnosticsPreserveFirstFailure(t *testing.T) {
	measured := WebsocketDiagnostics{WebsocketMessageLimitBytes: 1024, WebsocketLastMessageObserved: true, WebsocketLastMessageBytes: 0}
	failed := measured.Merge(WebsocketDiagnostics{WebsocketFailureStage: "read", WebsocketErrorType: "close", WebsocketCloseCode: 1006, WebsocketCloseReceived: true})
	if failed.WebsocketCloseReceived || !failed.WebsocketLastMessageObserved || failed.WebsocketMessageLimitBytes != 1024 {
		t.Fatalf("synthetic close/empty successful message facts lost: %+v", failed)
	}
	cleanup := WebsocketDiagnostics{WebsocketFailureStage: "write", WebsocketErrorType: "canceled", WebsocketLastMessageObserved: true, WebsocketLastMessageBytes: 42}
	if failed.Merge(cleanup) != failed || failed.Merge(WebsocketDiagnostics{}) != failed {
		t.Fatal("cleanup or late progress overwrote the first failure")
	}
}

func TestWebsocketDiagnosticsContainOnlyFiniteFacts(t *testing.T) {
	unsafe := WebsocketDiagnostics{WebsocketFailureStage: "private-payload", WebsocketErrorType: "private-close-text", WebsocketCloseCode: -1, WebsocketCloseReceived: true, WebsocketMessageLimitBytes: -1, WebsocketLastMessageBytes: -1, WebsocketJSONErrorOffset: -1}
	safe := unsafe.Safe()
	if safe.WebsocketFailureStage != "unknown" || safe.WebsocketErrorType != "unknown" || safe.WebsocketCloseCode != 0 || safe.WebsocketCloseReceived || safe.WebsocketMessageLimitBytes != 0 || safe.WebsocketLastMessageBytes != 0 || safe.WebsocketJSONErrorOffset != 0 {
		t.Fatalf("unbounded or invalid facts: %+v", safe)
	}
	for _, code := range []int{1000, 1005, 1006, 1011, 1015} {
		d := (WebsocketDiagnostics{WebsocketCloseCode: code, WebsocketCloseReceived: true}).Safe()
		if d.WebsocketCloseReceived != (code != 1006 && code != 1015) {
			t.Fatalf("incorrect close origin: %+v", d)
		}
	}
}
