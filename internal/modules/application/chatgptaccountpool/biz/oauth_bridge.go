// Package biz implements account-pool use cases.
package biz

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	events "aetherrelay/internal/modules/application/chatgptaccountpool/pkg/events"
	"github.com/google/uuid"
)

const (
	oauthBridgeTTL    = 10 * time.Minute
	oauthBridgeMax    = 64
	oauthAuthorizeURL = "https://auth.openai.com/api/accounts/authorize"
	oauthClientID     = "app_2SKx67EdpoN0G6j64rFvigXD"
	oauthRedirectURI  = "https://platform.openai.com/auth/callback"
	oauthAuth0Client  = "eyJuYW1lIjoiYXV0aDAtc3BhLWpzIiwidmVyc2lvbiI6IjEuMjEuMCJ9"
)

type oauthBridgeSession struct {
	verifier, state string
	targetID        string
	created         time.Time
}
type oauthBridge struct {
	mu       sync.Mutex
	sessions map[string]oauthBridgeSession
}

func (b *oauthBridge) start(emailHint, targetID string) (events.OAuthStartResult, error) {
	verifier, err := oauthRandom(64)
	if err != nil {
		return events.OAuthStartResult{}, err
	}
	nonce, err := oauthRandom(24)
	if err != nil {
		return events.OAuthStartResult{}, err
	}
	sessionID := uuid.NewString()
	state := sessionID + "." + nonce
	sum := sha256.Sum256([]byte(verifier))
	params := url.Values{"issuer": {"https://auth.openai.com"}, "client_id": {oauthClientID}, "audience": {"https://api.openai.com/v1"}, "redirect_uri": {oauthRedirectURI}, "device_id": {uuid.NewString()}, "screen_hint": {"login_or_signup"}, "max_age": {"0"}, "scope": {"openid profile email offline_access"}, "response_type": {"code"}, "response_mode": {"query"}, "state": {state}, "nonce": {nonce}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}, "auth0Client": {oauthAuth0Client}}
	if emailHint = strings.TrimSpace(emailHint); emailHint != "" {
		params.Set("login_hint", emailHint)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneLocked(time.Now())
	if b.sessions == nil {
		b.sessions = map[string]oauthBridgeSession{}
	}
	b.sessions[sessionID] = oauthBridgeSession{verifier: verifier, state: state, targetID: strings.TrimSpace(targetID), created: time.Now()}
	return events.OAuthStartResult{SessionID: sessionID, AuthorizeURL: oauthAuthorizeURL + "?" + params.Encode(), ExpiresIn: int(oauthBridgeTTL.Seconds()), RedirectURIPrefix: oauthRedirectURI}, nil
}

func (b *oauthBridge) finish(sessionID, callback string) (string, string, string, string, error) {
	code, state, err := oauthCodeFromCallback(callback)
	if err != nil {
		return "", "", "", "", err
	}
	if stateSession := strings.SplitN(state, ".", 2)[0]; stateSession != "" {
		sessionID = stateSession
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return "", "", "", "", fmt.Errorf("oauth session_id or callback state is required")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneLocked(time.Now())
	session, ok := b.sessions[sessionID]
	if !ok {
		return "", "", "", "", fmt.Errorf("oauth session expired or does not exist")
	}
	if state != "" && state != session.state {
		return "", "", "", "", fmt.Errorf("oauth state mismatch")
	}
	return code, session.verifier, sessionID, session.targetID, nil
}

func (b *oauthBridge) consume(sessionID string) {
	b.mu.Lock()
	delete(b.sessions, sessionID)
	b.mu.Unlock()
}

func (b *oauthBridge) pruneLocked(now time.Time) {
	for id, session := range b.sessions {
		if now.Sub(session.created) > oauthBridgeTTL {
			delete(b.sessions, id)
		}
	}
	for len(b.sessions) >= oauthBridgeMax {
		var oldest string
		var at time.Time
		for id, session := range b.sessions {
			if oldest == "" || session.created.Before(at) {
				oldest, at = id, session.created
			}
		}
		delete(b.sessions, oldest)
	}
}

func oauthRandom(bytes int) (string, error) {
	data := make([]byte, bytes)
	if _, err := rand.Read(data); err != nil {
		return "", fmt.Errorf("generate oauth random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func oauthCodeFromCallback(value string) (string, string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", "", fmt.Errorf("oauth callback or code is required")
	}
	if !strings.HasPrefix(value, "http://") && !strings.HasPrefix(value, "https://") {
		return value, "", nil
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", "", fmt.Errorf("parse oauth callback: %w", err)
	}
	query := parsed.Query()
	code, state := strings.TrimSpace(query.Get("code")), strings.TrimSpace(query.Get("state"))
	if code == "" {
		if message := strings.TrimSpace(query.Get("error_description")); message != "" {
			return "", "", fmt.Errorf("oauth callback: %s", message)
		}
		return "", "", fmt.Errorf("oauth callback has no code")
	}
	return code, state, nil
}
