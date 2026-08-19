// Package biz tests account-pool use cases.
package biz

import (
	"net/url"
	"strings"
	"testing"
)

func TestOAuthBridgeStartAndFinishUsesMatchingState(t *testing.T) {
	bridge := oauthBridge{}
	started, err := bridge.start("user@example.invalid", "local-account")
	if err != nil {
		t.Fatal(err)
	}
	if started.SessionID == "" || started.ExpiresIn != 600 || !strings.HasPrefix(started.AuthorizeURL, oauthAuthorizeURL+"?") {
		t.Fatalf("start=%#v", started)
	}
	parsed, err := url.Parse(started.AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if query.Get("code_challenge_method") != "S256" || query.Get("login_hint") != "user@example.invalid" || query.Get("device_id") == "" || query.Get("auth0Client") != oauthAuth0Client {
		t.Fatalf("query=%v", query)
	}
	code, verifier, sessionID, targetID, err := bridge.finish("", oauthRedirectURI+"?code=code-value&state="+url.QueryEscape(query.Get("state")))
	if err != nil || code != "code-value" || verifier == "" || sessionID != started.SessionID || targetID != "local-account" {
		t.Fatalf("code=%q verifier=%q session=%q target=%q err=%v", code, verifier, sessionID, targetID, err)
	}
	bridge.consume(sessionID)
	if _, _, _, _, err := bridge.finish(started.SessionID, "code-value"); err == nil {
		t.Fatal("consumed oauth session was accepted")
	}
}

func TestOAuthBridgeRejectsMismatchedState(t *testing.T) {
	bridge := oauthBridge{}
	started, err := bridge.start("", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := bridge.finish(started.SessionID, oauthRedirectURI+"?code=code-value&state=wrong"); err == nil {
		t.Fatal("mismatched state was accepted")
	}
}
