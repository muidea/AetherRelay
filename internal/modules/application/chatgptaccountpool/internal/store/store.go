// Package store provides account-pool persistence.
package store

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	events "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/events"
	"ai-proxy/internal/pkg/aiproxystate"
)

const (
	StatusNormal   = "正常"
	StatusLimited  = "限流"
	StatusAbnormal = "异常"
	StatusDisabled = "禁用"

	modelDiscoveryRetryBase = 30 * time.Second
	modelDiscoveryRetryMax  = 5 * time.Minute

	// Keep the account/model scheduling behavior compatible with the source
	// gateway's legacy transient cooldown. A 429 is intentionally treated as
	// the same short, persisted recovery window here: ChatGPT Web does not
	// expose a reliable retry-after value on every text transport.
	textRateLimitCooldown  = time.Minute
	textTransientCooldown  = time.Minute
	imageRateLimitCooldown = time.Minute
	imageTransientCooldown = time.Minute
)

type Account struct {
	ID           string `json:"id"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Email        string `json:"email,omitempty"`
	Password     string `json:"password,omitempty"`
	Type         string `json:"type,omitempty"`
	SourceType   string `json:"source_type,omitempty"`
	Status       string `json:"status"`
	Quota        int    `json:"quota"`
	Proxy        string `json:"proxy,omitempty"`
	LastUsedAt   string `json:"last_used_at,omitempty"`
	CreatedAt    string `json:"created_at,omitempty"`
	// ModelSnapshot is a derived capability projection from ChatGPT Web model
	// enumeration. It is persisted in a constrained form and never holds raw
	// upstream payloads or tokens. Missing snapshots mean "pending discovery".
	ModelSnapshot *events.AccountModelSnapshot `json:"-"`
	// Extra retains Python-owned fields (OAuth id_token, refresh progress,
	// import metadata, etc.) when Go updates its account-pool fields.
	Extra map[string]any `json:"-"`
}

type Store struct {
	mu            sync.Mutex
	documents     *aiproxystate.Documents
	items         map[string]*Account // access_token -> account
	aliases       map[string]string   // retired access token -> current token
	order         []string
	index         int
	imageInflight map[string]int
	concurrency   int
	// catalogVersion increments when discovery-relevant account state changes
	// (add/delete/status/snapshot). Proxy discovery watches this value.
	catalogVersion uint64
}

// Open creates the account owner's state store and reports every DuckDB
// failure to the module startup path.
func Open(databasePath, memoryLimit string, threads, concurrency int) (*Store, error) {
	if concurrency < 1 {
		concurrency = 3
	}
	s := &Store{
		items:         map[string]*Account{},
		aliases:       map[string]string{},
		imageInflight: map[string]int{},
		concurrency:   concurrency,
	}
	documents, err := aiproxystate.Open(databasePath, memoryLimit, threads)
	if err != nil {
		return nil, err
	}
	s.documents = documents
	if err := s.load(); err != nil {
		_ = documents.Close()
		return nil, fmt.Errorf("load account state: %w", err)
	}
	return s, nil
}

// New is retained for direct package tests. Production startup must call Open
// so a state failure is returned to the module lifecycle instead of hidden.
func New(databasePath string, concurrency int) *Store {
	s, err := Open(databasePath, "128MB", 1, concurrency)
	if err != nil {
		panic(err)
	}
	return s
}

type TokenRefreshCandidate struct {
	AccessToken  string
	RefreshToken string
	Proxy        string
	Reason       string // expiring | keepalive
}

// OAuthRefreshCredential is an owner-internal projection used by account biz
// to renew a token. It is never sent through the public EventHub contract.
type OAuthRefreshCredential struct {
	AccessToken  string
	RefreshToken string
	Proxy        string
}

// PasswordLoginCandidate is an account-owner-only credential projection. It
// must not cross the EventHub boundary or be returned from HTTP handlers.
type PasswordLoginCandidate struct {
	AccountID   string
	AccessToken string
	Email       string
	Password    string
	Proxy       string
}

type PasswordLoginSkip struct {
	AccountID string
	Reason    string
}

func (s *Store) load() error {
	if s.documents == nil {
		return fmt.Errorf("state documents are unavailable")
	}
	rows, err := s.documents.LoadAccounts()
	if err != nil {
		return err
	}
	for _, row := range rows {
		var item map[string]any
		if err := json.Unmarshal(row.Payload, &item); err != nil {
			return fmt.Errorf("decode account %q: %w", row.AccessToken, err)
		}
		acc := mapToAccount(item)
		if acc.AccessToken == "" {
			acc.AccessToken = row.AccessToken
		}
		if acc.AccessToken == "" {
			continue
		}
		s.items[acc.AccessToken] = acc
		s.order = append(s.order, acc.AccessToken)
	}
	return nil
}

func mapToAccount(m map[string]any) *Account {
	acc := &Account{
		ID:           asString(m["id"]),
		AccessToken:  firstString(m, "access_token", "accessToken"),
		RefreshToken: firstString(m, "refresh_token", "refreshToken"),
		Email:        asString(m["email"]),
		Password:     asString(m["password"]),
		Type:         asString(m["type"]),
		SourceType:   asString(m["source_type"]),
		Status:       asString(m["status"]),
		Proxy:        asString(m["proxy"]),
		LastUsedAt:   asString(m["last_used_at"]),
		CreatedAt:    asString(m["created_at"]),
		Extra:        cloneMap(m),
	}
	acc.ModelSnapshot = snapshotFromExtra(acc.Extra)
	if acc.Status == "" {
		acc.Status = StatusNormal
	}
	if acc.SourceType == "" {
		acc.SourceType = "web"
	}
	if acc.ID == "" && acc.AccessToken != "" {
		acc.ID = shortID(acc.AccessToken)
	}
	acc.Quota = asInt(m["quota"])
	return acc
}

func (s *Store) saveLocked() error {
	if s.documents == nil {
		return fmt.Errorf("state documents are unavailable")
	}
	rows := make([]aiproxystate.AccountRow, 0, len(s.order))
	for position, token := range s.order {
		if acc, ok := s.items[token]; ok {
			payload, err := json.Marshal(accountToMap(acc))
			if err != nil {
				return err
			}
			rows = append(rows, aiproxystate.AccountRow{AccessToken: token, Position: position, Payload: payload})
		}
	}
	return s.documents.ReplaceAccounts(rows)
}

func (s *Store) Close() error {
	if s == nil || s.documents == nil {
		return nil
	}
	return s.documents.Close()
}

func accountToMap(acc *Account) map[string]any {
	ret := cloneMap(acc.Extra)
	if ret == nil {
		ret = map[string]any{}
	}
	ret["id"] = acc.ID
	ret["access_token"] = acc.AccessToken
	ret["refresh_token"] = acc.RefreshToken
	ret["email"] = acc.Email
	ret["password"] = acc.Password
	ret["type"] = acc.Type
	ret["source_type"] = acc.SourceType
	ret["status"] = acc.Status
	ret["quota"] = acc.Quota
	ret["proxy"] = acc.Proxy
	ret["last_used_at"] = acc.LastUsedAt
	ret["created_at"] = acc.CreatedAt
	if acc.ModelSnapshot != nil {
		ret["model_snapshot"] = snapshotToMap(acc.ModelSnapshot)
	} else {
		delete(ret, "model_snapshot")
	}
	return ret
}

func (s *Store) List() []events.AccountView {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]events.AccountView, 0, len(s.order))
	for _, token := range s.order {
		acc := s.items[token]
		if acc == nil {
			continue
		}
		view := toView(acc, true)
		view.ImageInflight = s.imageInflight[token]
		out = append(out, view)
	}
	return out
}

func (s *Store) Export(tokens []string) []events.ExportItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exportLocked(tokens)
}

// ExportByIDs is the public-management selector. The conversion from stable
// account ID to a credential happens entirely inside the account owner.
func (s *Store) ExportByIDs(ids []string) []events.ExportItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exportLocked(s.tokensForIDsLocked(ids))
}

func (s *Store) exportLocked(tokens []string) []events.ExportItem {
	selected := map[string]bool{}
	for _, token := range tokens {
		if token = s.resolveTokenLocked(trim(token)); token != "" {
			selected[token] = true
		}
	}
	items := make([]events.ExportItem, 0)
	for _, token := range s.order {
		if len(selected) > 0 && !selected[token] {
			continue
		}
		acc := s.items[token]
		if acc == nil {
			continue
		}
		idToken := extraString(acc, "id_token")
		if acc.AccessToken != "" && acc.RefreshToken != "" && idToken != "" {
			accessClaims := jwtClaims(acc.AccessToken)
			idClaims := jwtClaims(idToken)
			authClaims := mapValue(accessClaims["https://api.openai.com/auth"])
			profileClaims := mapValue(accessClaims["https://api.openai.com/profile"])
			email := firstNonEmpty(acc.Email, asString(profileClaims["email"]), asString(idClaims["email"]))
			accountID := firstNonEmpty(extraString(acc, "account_id"), asString(authClaims["chatgpt_account_id"]), extraString(acc, "user_id"))
			items = append(items, events.ExportItem{
				Type:         firstNonEmpty(extraString(acc, "export_type"), "codex"),
				Email:        email,
				AccountID:    accountID,
				AccessToken:  acc.AccessToken,
				RefreshToken: acc.RefreshToken,
				IDToken:      idToken,
				Expired:      jwtTimestamp(accessClaims, "exp"),
				LastRefresh:  jwtTimestamp(accessClaims, "iat"),
				Password:     acc.Password,
			})
		}
	}
	return items
}

func extraString(acc *Account, key string) string {
	if acc == nil || acc.Extra == nil {
		return ""
	}
	return asString(acc.Extra[key])
}

func extraInt(acc *Account, key string) int {
	if acc == nil || acc.Extra == nil {
		return 0
	}
	return asInt(acc.Extra[key])
}

func jwtClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return map[string]any{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return map[string]any{}
	}
	claims := map[string]any{}
	if json.Unmarshal(payload, &claims) != nil {
		return map[string]any{}
	}
	return claims
}

func mapValue(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = trim(value); value != "" {
			return value
		}
	}
	return ""
}

func jwtTimestamp(claims map[string]any, key string) string {
	value, found := claims[key]
	if !found {
		return ""
	}
	seconds := int64(asInt(value))
	return time.Unix(seconds, 0).In(time.FixedZone("CST", 8*60*60)).Format(time.RFC3339)
}

// RefreshCandidates returns only the ChatGPT Web-compatible accounts whose
// quota/status can be reconciled through the upstream. It is an owner-internal
// read: callers must never retain or persist the returned access token.
func (s *Store) RefreshCandidates() []events.AccountView {
	return s.refreshCandidates(nil)
}

// RefreshCandidatesFor returns the subset selected by the caller. It keeps
// selection and eligibility inside the account owner, including aliases left
// behind by access-token rotation.
func (s *Store) RefreshCandidatesFor(tokens []string) []events.AccountView {
	return s.refreshCandidates(tokens)
}

// RefreshCandidatesForIDs resolves the admin-facing stable IDs under the
// account owner's lock; access tokens never cross the admin boundary.
func (s *Store) RefreshCandidatesForIDs(ids []string) []events.AccountView {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshCandidatesLocked(s.tokensForIDsLocked(ids))
}

func (s *Store) refreshCandidates(tokens []string) []events.AccountView {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshCandidatesLocked(tokens)
}

func (s *Store) refreshCandidatesLocked(tokens []string) []events.AccountView {
	selected := map[string]bool{}
	hasSelection := len(tokens) > 0
	for _, token := range tokens {
		if token = s.resolveTokenLocked(trim(token)); token != "" {
			selected[token] = true
		}
	}
	out := make([]events.AccountView, 0, len(s.order))
	for _, token := range s.order {
		acc := s.items[token]
		if hasSelection && !selected[token] {
			continue
		}
		if acc == nil || !isWebCompatibleSource(acc.SourceType) {
			continue
		}
		if acc.Status != StatusNormal && acc.Status != StatusLimited {
			continue
		}
		out = append(out, toView(acc, true))
	}
	return out
}

// SourceType describes how an account entered the pool, not which upstream
// protocol it can use. OAuth and password flows both yield ChatGPT Web
// credentials and must be eligible for the same status calibration as a
// token imported as "web".
func isWebCompatibleSource(sourceType string) bool {
	switch strings.ToLower(trim(sourceType)) {
	case "", "web", "oauth_login", "password":
		return true
	default:
		return false
	}
}

// TokenRefreshCandidates selects at most keepaliveLimit opportunistic
// refreshes, in addition to every JWT that is within refreshSkew. It never
// selects disabled accounts and uses only owner-local credentials.
func (s *Store) TokenRefreshCandidates(now time.Time, refreshSkew, keepaliveAfter, errorBackoff time.Duration, keepaliveLimit int) []TokenRefreshCandidate {
	s.mu.Lock()
	defer s.mu.Unlock()
	if keepaliveLimit < 1 {
		keepaliveLimit = 1
	}
	expiring := make([]TokenRefreshCandidate, 0)
	keepalive := make([]TokenRefreshCandidate, 0, keepaliveLimit)
	for _, token := range s.order {
		acc := s.items[token]
		if acc == nil || acc.Status == StatusDisabled || acc.Status == StatusAbnormal || trim(acc.RefreshToken) == "" {
			continue
		}
		candidate := TokenRefreshCandidate{AccessToken: token, RefreshToken: acc.RefreshToken, Proxy: acc.Proxy}
		if expiry, ok := jwtTime(token, "exp"); ok && !expiry.After(now.Add(refreshSkew)) {
			candidate.Reason = "expiring"
			expiring = append(expiring, candidate)
			continue
		}
		if len(keepalive) >= keepaliveLimit || recentExtraTime(acc, "last_token_refresh_error_at", now, errorBackoff) {
			continue
		}
		anchor, ok := extraTime(acc, "last_token_refresh_at")
		if !ok {
			anchor, ok = jwtTime(token, "iat")
		}
		if !ok {
			anchor, ok = parseTime(acc.CreatedAt)
		}
		if !ok || !anchor.Add(keepaliveAfter).After(now) {
			candidate.Reason = "keepalive"
			keepalive = append(keepalive, candidate)
		}
	}
	return append(expiring, keepalive...)
}

// OAuthRefreshCredentialFor resolves a potentially retired access token and
// returns its current refresh credential to account biz. Callers must keep the
// returned value inside the account-pool process; it is deliberately not an
// EventHub or HTTP projection.
func (s *Store) OAuthRefreshCredentialFor(token string) (OAuthRefreshCredential, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	token = s.resolveTokenLocked(trim(token))
	acc := s.items[token]
	if acc == nil || trim(acc.RefreshToken) == "" {
		return OAuthRefreshCredential{}, false
	}
	return OAuthRefreshCredential{AccessToken: token, RefreshToken: acc.RefreshToken, Proxy: acc.Proxy}, true
}

// ViewForAccessToken returns the current account view for an access-token
// alias. It is used only by account biz after a refresh-token rotation.
func (s *Store) ViewForAccessToken(token string) (events.AccountView, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	token = s.resolveTokenLocked(trim(token))
	acc := s.items[token]
	if acc == nil {
		return events.AccountView{}, false
	}
	return toView(acc, true), true
}

// PasswordLoginCandidates resolves requested accounts inside the owner and
// deliberately returns only to account biz. Missing credentials are reported
// as token-free skip records so callers can expose progress safely.
func (s *Store) PasswordLoginCandidates(tokens []string) ([]PasswordLoginCandidate, []PasswordLoginSkip) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	candidates := make([]PasswordLoginCandidate, 0, len(tokens))
	skipped := make([]PasswordLoginSkip, 0)
	for _, requested := range tokens {
		token := s.resolveTokenLocked(trim(requested))
		if token == "" || seen[token] {
			if token == "" {
				skipped = append(skipped, PasswordLoginSkip{Reason: "account not found"})
			}
			continue
		}
		seen[token] = true
		acc := s.items[token]
		if acc == nil {
			skipped = append(skipped, PasswordLoginSkip{Reason: "account not found"})
			continue
		}
		if trim(acc.Email) == "" || trim(acc.Password) == "" {
			skipped = append(skipped, PasswordLoginSkip{AccountID: acc.ID, Reason: "email or password is unavailable"})
			continue
		}
		candidates = append(candidates, PasswordLoginCandidate{AccountID: acc.ID, AccessToken: token, Email: acc.Email, Password: acc.Password, Proxy: acc.Proxy})
	}
	return candidates, skipped
}

// ApplyRefreshedToken atomically rotates an account key while carrying its
// image slots forward. The alias lets already-running requests release their
// old token's slot after the refresh completes.
func (s *Store) ApplyRefreshedToken(oldToken, newToken, refreshToken, idToken string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	oldToken = s.resolveTokenLocked(oldToken)
	acc, ok := s.items[oldToken]
	if !ok {
		return "", false, nil
	}
	newToken = trim(newToken)
	if newToken == "" {
		return oldToken, false, nil
	}
	if existing, exists := s.items[newToken]; exists && newToken != oldToken && existing != acc {
		return oldToken, false, fmt.Errorf("refreshed access token already belongs to another account")
	}
	if refreshToken = trim(refreshToken); refreshToken != "" {
		acc.RefreshToken = refreshToken
	}
	if acc.Extra == nil {
		acc.Extra = map[string]any{}
	}
	if idToken = trim(idToken); idToken != "" {
		acc.Extra["id_token"] = idToken
	}
	now := time.Now().UTC().Format(time.RFC3339)
	acc.Extra["last_token_refresh_at"] = now
	delete(acc.Extra, "last_token_refresh_error")
	delete(acc.Extra, "last_token_refresh_error_at")
	delete(acc.Extra, "last_token_refresh_error_class")
	rotated := newToken != oldToken
	if rotated {
		delete(s.items, oldToken)
		s.items[newToken] = acc
		acc.AccessToken = newToken
		for i, token := range s.order {
			if token == oldToken {
				s.order[i] = newToken
				break
			}
		}
		if inflight := s.imageInflight[oldToken]; inflight > 0 {
			s.imageInflight[newToken] += inflight
			delete(s.imageInflight, oldToken)
		}
		s.aliases[oldToken] = newToken
		// Model visibility is credential-scoped. Do not route a newly rotated
		// token from a snapshot that was enumerated with its predecessor.
		acc.ModelSnapshot = nil
	}
	if err := s.saveLocked(); err != nil {
		return oldToken, false, err
	}
	if rotated {
		s.bumpCatalogLocked()
	}
	return newToken, rotated, nil
}

// ApplyPasswordLogin atomically replaces a credential set obtained through
// the password-login flow. Existing in-flight image slots and old-token
// aliases survive rotation exactly as they do for refresh-token rotation.
func (s *Store) ApplyPasswordLogin(oldToken, newToken, refreshToken, idToken, email, accountID string) (events.AccountView, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	oldToken = s.resolveTokenLocked(oldToken)
	acc, ok := s.items[oldToken]
	if !ok {
		return events.AccountView{}, false, nil
	}
	newToken = trim(newToken)
	if newToken == "" {
		return events.AccountView{}, false, fmt.Errorf("password login returned an empty access token")
	}
	if existing, exists := s.items[newToken]; exists && newToken != oldToken && existing != acc {
		return events.AccountView{}, false, fmt.Errorf("password login access token already belongs to another account")
	}
	if refreshToken = trim(refreshToken); refreshToken != "" {
		acc.RefreshToken = refreshToken
	}
	if acc.Extra == nil {
		acc.Extra = map[string]any{}
	}
	if idToken = trim(idToken); idToken != "" {
		acc.Extra["id_token"] = idToken
	}
	if accountID = trim(accountID); accountID != "" {
		acc.Extra["account_id"] = accountID
	}
	if email = trim(email); email != "" {
		acc.Email = email
	}
	acc.SourceType = "password"
	acc.Status = StatusNormal
	acc.ModelSnapshot = nil
	acc.Extra["last_token_refresh_at"] = time.Now().UTC().Format(time.RFC3339)
	delete(acc.Extra, "last_token_refresh_error")
	delete(acc.Extra, "last_token_refresh_error_at")
	delete(acc.Extra, "last_token_refresh_error_class")
	if newToken != oldToken {
		delete(s.items, oldToken)
		s.items[newToken] = acc
		acc.AccessToken = newToken
		for i, token := range s.order {
			if token == oldToken {
				s.order[i] = newToken
				break
			}
		}
		if inflight := s.imageInflight[oldToken]; inflight > 0 {
			s.imageInflight[newToken] += inflight
			delete(s.imageInflight, oldToken)
		}
		s.aliases[oldToken] = newToken
	}
	if err := s.saveLocked(); err != nil {
		return events.AccountView{}, false, err
	}
	s.bumpCatalogLocked()
	return toView(acc, true), true, nil
}

func (s *Store) Disable(token string) (events.AccountView, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	token = s.resolveTokenLocked(token)
	acc, ok := s.items[token]
	if !ok {
		return events.AccountView{}, false, nil
	}
	changed := acc.Status != StatusDisabled || acc.Quota != 0
	acc.Status, acc.Quota = StatusDisabled, 0
	if err := s.saveLocked(); err != nil {
		return events.AccountView{}, false, err
	}
	if changed {
		s.bumpCatalogLocked()
	}
	return toView(acc, true), true, nil
}

// RecordTokenRefreshFailure saves only a bounded failure category and its
// timestamp. Raw OAuth error text can contain upstream or proxy diagnostics,
// so it must not become account state or a management read-model field.
func (s *Store) RecordTokenRefreshFailure(token, class string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	token = s.resolveTokenLocked(token)
	acc, ok := s.items[token]
	if !ok {
		return nil
	}
	if acc.Extra == nil {
		acc.Extra = map[string]any{}
	}
	class = bounded(trim(class), 64)
	if class == "" {
		class = "unavailable"
	}
	delete(acc.Extra, "last_token_refresh_error")
	acc.Extra["last_token_refresh_error_class"] = class
	acc.Extra["last_token_refresh_error_at"] = time.Now().UTC().Format(time.RFC3339)
	return s.saveLocked()
}

// ApplyUpstreamInfo updates only account-owned status projections. Disabled
// and abnormal accounts remain operator-controlled until explicitly changed.
func (s *Store) ApplyUpstreamInfo(token, email, planType string, quota int, restoreAt string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc, ok := s.items[token]
	if !ok || acc.Status == StatusDisabled || acc.Status == StatusAbnormal {
		return false, nil
	}
	if email = trim(email); email != "" {
		acc.Email = email
	}
	if planType = trim(planType); planType != "" {
		acc.Type = planType
	}
	previousStatus, previousQuota := acc.Status, acc.Quota
	acc.Quota = quota
	if quota > 0 {
		acc.Status = StatusNormal
	} else {
		acc.Status = StatusLimited
	}
	if acc.Extra == nil {
		acc.Extra = map[string]any{}
	}
	if restoreAt = trim(restoreAt); restoreAt != "" {
		acc.Extra["restore_at"] = restoreAt
	} else {
		delete(acc.Extra, "restore_at")
	}
	delete(acc.Extra, "last_refresh_error")
	delete(acc.Extra, "last_refresh_error_at")
	if err := s.saveLocked(); err != nil {
		return false, err
	}
	if acc.Status != previousStatus || acc.Quota != previousQuota {
		s.bumpCatalogLocked()
	}
	return true, nil
}

func (s *Store) RecordRefreshError(token, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	acc, ok := s.items[token]
	if !ok {
		return nil
	}
	if acc.Extra == nil {
		acc.Extra = map[string]any{}
	}
	acc.Extra["last_refresh_error"] = bounded(trim(message), 512)
	acc.Extra["last_refresh_error_at"] = time.Now().UTC().Format(time.RFC3339)
	return s.saveLocked()
}

func (s *Store) Add(tokens []string, sourceType string) (added, skipped int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() {
		if added > 0 {
			s.bumpCatalogLocked()
		}
	}()
	if sourceType == "" {
		sourceType = "web"
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, token := range tokens {
		token = trim(token)
		if token == "" {
			continue
		}
		if _, exists := s.items[token]; exists {
			skipped++
			continue
		}
		acc := &Account{
			ID:          shortID(token),
			AccessToken: token,
			SourceType:  sourceType,
			Status:      StatusNormal,
			Quota:       0,
			CreatedAt:   now,
		}
		s.items[token] = acc
		s.order = append(s.order, token)
		added++
	}
	if added > 0 {
		err = s.saveLocked()
	}
	return
}

// AddOAuth persists the complete OAuth token set. Refresh/id tokens never
// leave the account owner through the public AccountView projection.
func (s *Store) AddOAuth(accessToken, refreshToken, idToken string) (events.AccountView, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	accessToken, refreshToken = trim(accessToken), trim(refreshToken)
	if accessToken == "" || refreshToken == "" {
		return events.AccountView{}, false, fmt.Errorf("oauth access and refresh tokens are required")
	}
	acc, exists := s.items[accessToken]
	if !exists {
		acc = &Account{ID: shortID(accessToken), AccessToken: accessToken, RefreshToken: refreshToken, SourceType: "oauth_login", Status: StatusNormal, CreatedAt: time.Now().UTC().Format(time.RFC3339), Extra: map[string]any{}}
		s.items[accessToken] = acc
		s.order = append(s.order, accessToken)
	} else {
		acc.RefreshToken, acc.SourceType = refreshToken, "oauth_login"
	}
	if acc.Extra == nil {
		acc.Extra = map[string]any{}
	}
	if idToken = trim(idToken); idToken != "" {
		acc.Extra["id_token"] = idToken
	}
	if err := s.saveLocked(); err != nil {
		return events.AccountView{}, false, err
	}
	if !exists {
		s.bumpCatalogLocked()
	}
	return toView(acc, true), !exists, nil
}

func (s *Store) Delete(tokens []string) (deleted int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteLocked(tokens)
}

func (s *Store) DeleteByIDs(ids []string) (deleted int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteLocked(s.tokensForIDsLocked(ids))
}

func (s *Store) deleteLocked(tokens []string) (deleted int, err error) {
	defer func() {
		if deleted > 0 {
			s.bumpCatalogLocked()
		}
	}()
	set := map[string]struct{}{}
	for _, t := range tokens {
		set[trim(t)] = struct{}{}
	}
	newOrder := s.order[:0]
	for _, token := range s.order {
		if _, ok := set[token]; ok {
			delete(s.items, token)
			delete(s.imageInflight, token)
			deleted++
			continue
		}
		newOrder = append(newOrder, token)
	}
	s.order = newOrder
	if deleted > 0 {
		err = s.saveLocked()
	}
	return
}

func (s *Store) Update(token string, typ, status string, quota *int, proxy string) (events.AccountView, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	token = s.resolveTokenLocked(token)
	acc, ok := s.items[token]
	if !ok {
		return events.AccountView{}, false, nil
	}
	changed := false
	if typ != "" && acc.Type != typ {
		acc.Type = typ
		changed = true
	}
	if status != "" && acc.Status != status {
		acc.Status = status
		changed = true
	}
	if quota != nil && acc.Quota != *quota {
		acc.Quota = *quota
		changed = true
	}
	if proxy != "" || proxy == "" && proxy != acc.Proxy {
		// allow explicit clear only when caller passes sentinel later; for now set if non-empty
		if proxy != "" {
			acc.Proxy = proxy
		}
	}
	if err := s.saveLocked(); err != nil {
		return events.AccountView{}, false, err
	}
	if changed {
		s.bumpCatalogLocked()
	}
	return toView(acc, true), true, nil
}

// UpdateByID supports explicit patch semantics: nil preserves a field while
// a non-nil empty string intentionally clears it (notably proxy).
func (s *Store) UpdateByID(id string, typ, status *string, quota *int, proxy *string) (events.AccountView, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	token, found := s.tokenForIDLocked(trim(id))
	if !found {
		return events.AccountView{}, false, nil
	}
	acc := s.items[token]
	if acc == nil {
		return events.AccountView{}, false, nil
	}
	changed := false
	if typ != nil {
		value := trim(*typ)
		if acc.Type != value {
			acc.Type = value
			changed = true
		}
	}
	if status != nil {
		value := trim(*status)
		if !validStatus(value) {
			return events.AccountView{}, false, fmt.Errorf("invalid account status")
		}
		if acc.Status != value {
			acc.Status = value
			changed = true
		}
	}
	if quota != nil {
		if *quota < 0 {
			return events.AccountView{}, false, fmt.Errorf("quota must not be negative")
		}
		if acc.Quota != *quota {
			acc.Quota = *quota
			changed = true
		}
	}
	if proxy != nil {
		acc.Proxy = trim(*proxy)
	}
	if err := s.saveLocked(); err != nil {
		return events.AccountView{}, false, err
	}
	if changed {
		s.bumpCatalogLocked()
	}
	return toView(acc, true), true, nil
}

func (s *Store) AcquireImageToken(planType, sourceType string, exclude []string, model, capability string) (events.AccountView, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ex := toSet(exclude)
	n := len(s.order)
	if n == 0 {
		return events.AccountView{}, false
	}
	for i := 0; i < n; i++ {
		s.index = (s.index + 1) % n
		token := s.order[s.index]
		if _, bad := ex[token]; bad {
			continue
		}
		acc := s.items[token]
		if acc == nil {
			continue
		}
		if acc.Status != StatusNormal {
			continue
		}
		if acc.Quota <= 0 {
			continue
		}
		if planType != "" && acc.Type != planType {
			continue
		}
		if sourceType != "" && acc.SourceType != sourceType {
			continue
		}
		if s.imageInflight[token] >= s.concurrency {
			continue
		}
		if imageCooldownActiveLocked(acc, model, time.Now().UTC()) {
			continue
		}
		if !accountSupportsModelLocked(acc, model, capability) {
			continue
		}
		s.imageInflight[token]++
		acc.LastUsedAt = time.Now().UTC().Format(time.RFC3339)
		return toView(acc, true), true
	}
	return events.AccountView{}, false
}

// AcquireImageAccount reacquires a specific account ID while preserving the
// same quota and per-account image-slot checks as normal acquisition.
func (s *Store) AcquireImageAccount(accountID string) (events.AccountView, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	accountID = trim(accountID)
	if accountID == "" {
		return events.AccountView{}, false
	}
	for _, token := range s.order {
		acc := s.items[token]
		if acc == nil || acc.ID != accountID {
			continue
		}
		if acc.Status != StatusNormal || acc.Quota <= 0 || s.imageInflight[token] >= s.concurrency {
			return events.AccountView{}, false
		}
		s.imageInflight[token]++
		acc.LastUsedAt = time.Now().UTC().Format(time.RFC3339)
		return toView(acc, true), true
	}
	return events.AccountView{}, false
}

func (s *Store) ReleaseImageSlot(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	token = s.resolveTokenLocked(token)
	if s.imageInflight[token] > 0 {
		s.imageInflight[token]--
	}
}

// MarkImageResult updates success/failure accounting and a model-scoped image
// recovery window. It mirrors text recovery semantics while deliberately
// keeping image cooling independent from text scheduling.
func (s *Store) MarkImageResult(token, model string, success bool, errorClass string) (events.AccountView, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	token = s.resolveTokenLocked(token)
	acc, ok := s.items[token]
	if !ok {
		return events.AccountView{}, false
	}
	changed := false
	if acc.Extra == nil {
		acc.Extra = map[string]any{}
	}
	if success {
		if acc.Quota > 0 {
			acc.Quota--
			changed = true
			if acc.Quota == 0 {
				acc.Status = StatusLimited
			}
		}
		acc.Extra["success"] = extraInt(acc, "success") + 1
		clearImageCooldownLocked(acc, model)
	} else {
		acc.Extra["fail"] = extraInt(acc, "fail") + 1
		now := time.Now().UTC()
		switch strings.ToLower(strings.TrimSpace(errorClass)) {
		case "invalid_token":
			acc.Status = StatusAbnormal
			acc.Quota = 0
			changed = true
		case "rate_limit":
			setImageCooldownLocked(acc, model, now.Add(imageRateLimitCooldown), "rate_limit")
		case "tls", "timeout", "upstream":
			setImageCooldownLocked(acc, model, now.Add(imageTransientCooldown), strings.ToLower(strings.TrimSpace(errorClass)))
		}
	}
	_ = s.saveLocked()
	if changed {
		s.bumpCatalogLocked()
	}
	view := toView(acc, true)
	view.ImageInflight = s.imageInflight[token]
	return view, true
}

const imageCooldownExtraKey = "image_cooldowns"

func imageCooldownActiveLocked(acc *Account, model string, now time.Time) bool {
	return cooldownActiveLocked(acc, imageCooldownExtraKey, model, now)
}

func setImageCooldownLocked(acc *Account, model string, until time.Time, errorClass string) {
	setCooldownLocked(acc, imageCooldownExtraKey, model, until, errorClass)
}

func clearImageCooldownLocked(acc *Account, model string) {
	clearCooldownLocked(acc, imageCooldownExtraKey, model)
}

func (s *Store) AcquireTextToken(exclude []string, model, capability string) (events.AccountView, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ex := toSet(exclude)
	n := len(s.order)
	if n == 0 {
		return events.AccountView{}, false
	}
	for i := 0; i < n; i++ {
		s.index = (s.index + 1) % n
		token := s.order[s.index]
		if _, bad := ex[token]; bad {
			continue
		}
		acc := s.items[token]
		if acc == nil {
			continue
		}
		if acc.Status == StatusDisabled || acc.Status == StatusAbnormal {
			continue
		}
		if textCooldownActiveLocked(acc, model, time.Now().UTC()) {
			continue
		}
		if !accountSupportsModelLocked(acc, model, capability) {
			continue
		}
		acc.LastUsedAt = time.Now().UTC().Format(time.RFC3339)
		return toView(acc, true), true
	}
	return events.AccountView{}, false
}

// AcquireTextAccount reacquires a specific account ID for text turns. It does
// not touch image in-flight slots and only requires the account to remain usable
// for the requested model/capability.
func (s *Store) AcquireTextAccount(accountID, model, capability string) (events.AccountView, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	accountID = trim(accountID)
	if accountID == "" {
		return events.AccountView{}, false
	}
	for _, token := range s.order {
		acc := s.items[token]
		if acc == nil || acc.ID != accountID {
			continue
		}
		if acc.Status == StatusDisabled || acc.Status == StatusAbnormal {
			return events.AccountView{}, false
		}
		if textCooldownActiveLocked(acc, model, time.Now().UTC()) {
			return events.AccountView{}, false
		}
		if !accountSupportsModelLocked(acc, model, capability) {
			return events.AccountView{}, false
		}
		acc.LastUsedAt = time.Now().UTC().Format(time.RFC3339)
		return toView(acc, true), true
	}
	return events.AccountView{}, false
}

// RecordTextResult updates success/fail counters and account/model recovery
// windows for one final text turn. invalid_token transitions the account to
// abnormal. rate_limit, tls, timeout and generic upstream failures apply a
// short model-scoped cooldown; content-policy and client-side outcomes are
// intentionally not scheduled by this owner method.
func (s *Store) RecordTextResult(accountID, model string, success bool, errorClass string) (events.AccountView, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	accountID = trim(accountID)
	if accountID == "" {
		return events.AccountView{}, false
	}
	var acc *Account
	for _, token := range s.order {
		candidate := s.items[token]
		if candidate != nil && candidate.ID == accountID {
			acc = candidate
			break
		}
	}
	if acc == nil {
		return events.AccountView{}, false
	}
	if acc.Extra == nil {
		acc.Extra = map[string]any{}
	}
	now := time.Now().UTC()
	if success {
		acc.Extra["success"] = extraInt(acc, "success") + 1
		clearTextCooldownLocked(acc, model)
	} else {
		acc.Extra["fail"] = extraInt(acc, "fail") + 1
		switch strings.ToLower(strings.TrimSpace(errorClass)) {
		case "invalid_token":
			acc.Status = StatusAbnormal
			acc.Quota = 0
			s.bumpCatalogLocked()
		case "rate_limit":
			setTextCooldownLocked(acc, model, now.Add(textRateLimitCooldown), "rate_limit")
		case "tls", "timeout", "upstream":
			setTextCooldownLocked(acc, model, now.Add(textTransientCooldown), strings.ToLower(strings.TrimSpace(errorClass)))
		}
	}
	_ = s.saveLocked()
	return toView(acc, true), true
}

const textCooldownExtraKey = "text_cooldowns"

func textCooldownKey(model string) string {
	if model = strings.ToLower(trim(model)); model != "" {
		return model
	}
	return "*"
}

func textCooldownActiveLocked(acc *Account, model string, now time.Time) bool {
	return cooldownActiveLocked(acc, textCooldownExtraKey, model, now)
}

func cooldownActiveLocked(acc *Account, extraKey, model string, now time.Time) bool {
	if acc == nil || acc.Extra == nil {
		return false
	}
	cooldowns, ok := acc.Extra[extraKey].(map[string]any)
	if !ok {
		return false
	}
	for _, key := range []string{textCooldownKey(model), "*"} {
		entry, ok := cooldowns[key].(map[string]any)
		if !ok {
			continue
		}
		until, ok := parseTime(asString(entry["until"]))
		if ok && until.After(now) {
			return true
		}
	}
	return false
}

func setTextCooldownLocked(acc *Account, model string, until time.Time, errorClass string) {
	setCooldownLocked(acc, textCooldownExtraKey, model, until, errorClass)
}

func setCooldownLocked(acc *Account, extraKey, model string, until time.Time, errorClass string) {
	if acc == nil {
		return
	}
	if acc.Extra == nil {
		acc.Extra = map[string]any{}
	}
	cooldowns, ok := acc.Extra[extraKey].(map[string]any)
	if !ok {
		cooldowns = map[string]any{}
		acc.Extra[extraKey] = cooldowns
	}
	key := textCooldownKey(model)
	if existing, ok := cooldowns[key].(map[string]any); ok {
		if current, valid := parseTime(asString(existing["until"])); valid && current.After(time.Now().UTC()) {
			// Concurrent requests that fail during an already-open window must
			// not continually extend it and black out a healthy recovery.
			return
		}
	}
	cooldowns[key] = map[string]any{
		"until":       until.UTC().Format(time.RFC3339),
		"error_class": errorClass,
	}
}

func clearTextCooldownLocked(acc *Account, model string) {
	clearCooldownLocked(acc, textCooldownExtraKey, model)
}

func clearCooldownLocked(acc *Account, extraKey, model string) {
	if acc == nil || acc.Extra == nil {
		return
	}
	cooldowns, ok := acc.Extra[extraKey].(map[string]any)
	if !ok {
		return
	}
	delete(cooldowns, textCooldownKey(model))
	if len(cooldowns) == 0 {
		delete(acc.Extra, extraKey)
	}
}

func (s *Store) RemoveInvalid(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	token = s.resolveTokenLocked(token)
	acc, ok := s.items[token]
	if !ok {
		return false
	}
	acc.Status = StatusAbnormal
	acc.Quota = 0
	_ = s.saveLocked()
	s.bumpCatalogLocked()
	return true
}

func (s *Store) Health() events.HealthResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	var h events.HealthResult
	h.Total = len(s.items)
	for _, acc := range s.items {
		switch acc.Status {
		case StatusNormal:
			h.Normal++
		case StatusLimited:
			h.Limited++
		case StatusAbnormal:
			h.Abnormal++
		case StatusDisabled:
			h.Disabled++
		}
	}
	return h
}

func toView(acc *Account, withToken bool) events.AccountView {
	v := events.AccountView{
		ID:                         acc.ID,
		Email:                      acc.Email,
		Type:                       acc.Type,
		SourceType:                 acc.SourceType,
		Status:                     acc.Status,
		Quota:                      acc.Quota,
		RestoreAt:                  extraString(acc, "restore_at"),
		Success:                    extraInt(acc, "success"),
		Fail:                       extraInt(acc, "fail"),
		CreatedAt:                  acc.CreatedAt,
		Proxy:                      acc.Proxy,
		LastUsedAt:                 acc.LastUsedAt,
		LastTokenRefreshAt:         extraString(acc, "last_token_refresh_at"),
		LastTokenRefreshErrorAt:    extraString(acc, "last_token_refresh_error_at"),
		LastTokenRefreshErrorClass: extraString(acc, "last_token_refresh_error_class"),
		TextCooldowns:              activeTextCooldowns(acc, time.Now().UTC()),
		ImageCooldowns:             activeImageCooldowns(acc, time.Now().UTC()),
	}
	if withToken {
		v.AccessToken = acc.AccessToken
	}
	return v
}

// activeTextCooldowns projects only currently-active, non-sensitive recovery
// windows for the admin read model. Expired records may remain in Extra until
// the next account mutation, but they must never be rendered as live state.
func activeTextCooldowns(acc *Account, now time.Time) []events.TextCooldownView {
	entries := activeCooldownEntries(acc, textCooldownExtraKey, now)
	items := make([]events.TextCooldownView, 0, len(entries))
	for _, entry := range entries {
		items = append(items, events.TextCooldownView{Model: entry.Model, Until: entry.Until, ErrorClass: entry.ErrorClass})
	}
	return items
}

func activeImageCooldowns(acc *Account, now time.Time) []events.ImageCooldownView {
	entries := activeCooldownEntries(acc, imageCooldownExtraKey, now)
	items := make([]events.ImageCooldownView, 0, len(entries))
	for _, entry := range entries {
		items = append(items, events.ImageCooldownView{Model: entry.Model, Until: entry.Until, ErrorClass: entry.ErrorClass})
	}
	return items
}

type cooldownEntry struct {
	Model      string
	Until      string
	ErrorClass string
}

func activeCooldownEntries(acc *Account, extraKey string, now time.Time) []cooldownEntry {
	if acc == nil || acc.Extra == nil {
		return nil
	}
	cooldowns, ok := acc.Extra[extraKey].(map[string]any)
	if !ok {
		return nil
	}
	items := make([]cooldownEntry, 0, len(cooldowns))
	for key, raw := range cooldowns {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		until, valid := parseTime(asString(entry["until"]))
		if !valid || !until.After(now) {
			continue
		}
		model := key
		if model == "*" {
			model = ""
		}
		items = append(items, cooldownEntry{
			Model:      model,
			Until:      until.Format(time.RFC3339),
			ErrorClass: asString(entry["error_class"]),
		})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Until != items[j].Until {
			return items[i].Until < items[j].Until
		}
		return items[i].Model < items[j].Model
	})
	return items
}

// CatalogVersion returns the current discovery-relevant generation.
func (s *Store) CatalogVersion() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.catalogVersion
}

func (s *Store) bumpCatalogLocked() {
	s.catalogVersion++
}

// ListDiscoveryCandidates returns accounts that may participate in model
// discovery. Tokens are for EventHub-internal discovery only.
func (s *Store) ListDiscoveryCandidates() (events.ListDiscoveryCandidatesResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := events.ListDiscoveryCandidatesResult{Version: s.catalogVersion}
	now := time.Now().UTC()
	for _, token := range s.order {
		acc := s.items[token]
		if acc == nil {
			continue
		}
		if acc.Status == StatusDisabled || acc.Status == StatusAbnormal {
			continue
		}
		needs := acc.ModelSnapshot == nil || snapshotExpired(acc.ModelSnapshot, now)
		out.Candidates = append(out.Candidates, events.DiscoveryCandidate{
			AccountID:      acc.ID,
			AccessToken:    acc.AccessToken,
			Proxy:          acc.Proxy,
			Status:         acc.Status,
			NeedsDiscovery: needs,
			DiscoveryDue:   needs && modelDiscoveryRetryDue(acc, now),
		})
	}
	return out, nil
}

// PutModelSnapshot stores a constrained per-account model capability snapshot
// and bumps the catalog version.
func (s *Store) PutModelSnapshot(accountID string, snap events.AccountModelSnapshot) (uint64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	accountID = trim(accountID)
	if accountID == "" {
		return s.catalogVersion, false, fmt.Errorf("account id is required")
	}
	token, ok := s.tokenForIDLocked(accountID)
	if !ok {
		return s.catalogVersion, false, nil
	}
	acc := s.items[token]
	if acc == nil {
		return s.catalogVersion, false, nil
	}
	clean := normalizeSnapshot(accountID, snap)
	acc.ModelSnapshot = &clean
	if acc.Extra != nil {
		delete(acc.Extra, "model_discovery_failures")
		delete(acc.Extra, "model_discovery_retry_at")
		delete(acc.Extra, "model_discovery_last_error")
	}
	s.bumpCatalogLocked()
	if err := s.saveLocked(); err != nil {
		return s.catalogVersion, false, err
	}
	return s.catalogVersion, true, nil
}

// RecordModelDiscoveryFailure persistently throttles failed model discovery so
// the fast watcher only retries the affected account when its backoff expires.
func (s *Store) RecordModelDiscoveryFailure(accountID, message string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	accountID = trim(accountID)
	if accountID == "" {
		return "", false, fmt.Errorf("account id is required")
	}
	token, ok := s.tokenForIDLocked(accountID)
	if !ok || s.items[token] == nil {
		return "", false, nil
	}
	acc := s.items[token]
	if acc.Extra == nil {
		acc.Extra = map[string]any{}
	}
	failures := extraInt(acc, "model_discovery_failures") + 1
	retryAt := time.Now().UTC().Add(modelDiscoveryRetryDelay(failures))
	acc.Extra["model_discovery_failures"] = failures
	acc.Extra["model_discovery_retry_at"] = retryAt.Format(time.RFC3339)
	acc.Extra["model_discovery_last_error"] = bounded(trim(message), 512)
	if err := s.saveLocked(); err != nil {
		return "", false, err
	}
	return retryAt.Format(time.RFC3339), true, nil
}

// CatalogSnapshot returns the model union across healthy accounts with
// non-expired snapshots.
func (s *Store) CatalogSnapshot() events.CatalogSnapshotResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	byID := map[string]*events.CatalogModel{}
	available := 0
	var latest time.Time
	for _, token := range s.order {
		acc := s.items[token]
		if acc == nil {
			continue
		}
		if acc.Status == StatusDisabled || acc.Status == StatusAbnormal {
			continue
		}
		available++
		if acc.ModelSnapshot == nil || snapshotExpired(acc.ModelSnapshot, now) {
			continue
		}
		if ts, err := time.Parse(time.RFC3339, acc.ModelSnapshot.DiscoveredAt); err == nil && ts.After(latest) {
			latest = ts
		}
		for _, model := range acc.ModelSnapshot.Models {
			id := strings.TrimSpace(model.ID)
			if id == "" {
				continue
			}
			entry, ok := byID[id]
			if !ok {
				entry = &events.CatalogModel{ID: id, CreatedAt: model.CreatedAt, OwnedBy: model.OwnedBy}
				byID[id] = entry
			}
			for _, op := range model.Capabilities {
				op = strings.TrimSpace(op)
				if op == "" {
					continue
				}
				if !containsString(entry.Capabilities, op) {
					entry.Capabilities = append(entry.Capabilities, op)
				}
			}
			if !containsString(entry.AccountIDs, acc.ID) {
				entry.AccountIDs = append(entry.AccountIDs, acc.ID)
			}
			if model.CreatedAt > entry.CreatedAt {
				entry.CreatedAt = model.CreatedAt
			}
			if entry.OwnedBy == "" {
				entry.OwnedBy = model.OwnedBy
			}
		}
	}
	out := events.CatalogSnapshotResult{Version: s.catalogVersion, AvailableAccounts: available}
	if !latest.IsZero() {
		out.UpdatedAt = latest.UTC().Format(time.RFC3339)
	}
	for _, entry := range byID {
		out.Models = append(out.Models, *entry)
	}
	// stable order for deterministic Admin/tests
	sort.Slice(out.Models, func(i, j int) bool { return out.Models[i].ID < out.Models[j].ID })
	return out
}

func accountSupportsModelLocked(acc *Account, model, capability string) bool {
	model = strings.TrimSpace(model)
	capability = strings.TrimSpace(capability)
	if model == "" && capability == "" {
		// Pre-discovery / non-catalog callers keep legacy acquire behavior.
		return true
	}
	if acc == nil || acc.ModelSnapshot == nil {
		return false
	}
	if snapshotExpired(acc.ModelSnapshot, time.Now().UTC()) {
		return false
	}
	for _, entry := range acc.ModelSnapshot.Models {
		if model != "" && entry.ID != model {
			continue
		}
		if capability == "" {
			return true
		}
		for _, op := range entry.Capabilities {
			if op == capability {
				return true
			}
		}
	}
	return false
}

func snapshotExpired(snap *events.AccountModelSnapshot, now time.Time) bool {
	if snap == nil {
		return true
	}
	exp := strings.TrimSpace(snap.ExpiresAt)
	if exp == "" {
		return false
	}
	ts, err := time.Parse(time.RFC3339, exp)
	if err != nil {
		return true
	}
	return !ts.After(now)
}

func modelDiscoveryRetryDue(acc *Account, now time.Time) bool {
	if acc == nil {
		return false
	}
	retryAt := extraString(acc, "model_discovery_retry_at")
	if retryAt == "" {
		return true
	}
	ts, err := time.Parse(time.RFC3339, retryAt)
	return err != nil || !ts.After(now)
}

func modelDiscoveryRetryDelay(failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	delay := modelDiscoveryRetryBase
	for attempt := 1; attempt < failures && delay < modelDiscoveryRetryMax; attempt++ {
		delay *= 2
	}
	if delay > modelDiscoveryRetryMax {
		return modelDiscoveryRetryMax
	}
	return delay
}

func normalizeSnapshot(accountID string, snap events.AccountModelSnapshot) events.AccountModelSnapshot {
	out := events.AccountModelSnapshot{
		AccountID:    accountID,
		DiscoveredAt: strings.TrimSpace(snap.DiscoveredAt),
		ExpiresAt:    strings.TrimSpace(snap.ExpiresAt),
	}
	if out.DiscoveredAt == "" {
		out.DiscoveredAt = time.Now().UTC().Format(time.RFC3339)
	}
	seen := map[string]struct{}{}
	for _, model := range snap.Models {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ops := make([]string, 0, len(model.Capabilities))
		for _, op := range model.Capabilities {
			op = strings.TrimSpace(op)
			if op != events.ModelCapabilityTextGeneration && op != events.ModelCapabilityImageGeneration {
				continue
			}
			if !containsString(ops, op) {
				ops = append(ops, op)
			}
		}
		if len(ops) == 0 {
			continue
		}
		out.Models = append(out.Models, events.AccountModelEntry{
			ID:           id,
			Capabilities: ops,
			CreatedAt:    model.CreatedAt,
			OwnedBy:      strings.TrimSpace(model.OwnedBy),
		})
	}
	return out
}

func snapshotFromExtra(extra map[string]any) *events.AccountModelSnapshot {
	if extra == nil {
		return nil
	}
	raw, ok := extra["model_snapshot"]
	if !ok || raw == nil {
		return nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		// tolerate re-encoded JSON objects after nested marshal
		encoded, err := json.Marshal(raw)
		if err != nil {
			return nil
		}
		var snap events.AccountModelSnapshot
		if json.Unmarshal(encoded, &snap) != nil || len(snap.Models) == 0 {
			return nil
		}
		return &snap
	}
	snap := events.AccountModelSnapshot{
		AccountID:    asString(m["account_id"]),
		DiscoveredAt: asString(m["discovered_at"]),
		ExpiresAt:    asString(m["expires_at"]),
	}
	rawModels, _ := m["models"].([]any)
	for _, item := range rawModels {
		mm, ok := item.(map[string]any)
		if !ok {
			continue
		}
		entry := events.AccountModelEntry{
			ID:        asString(mm["id"]),
			CreatedAt: int64(asInt(mm["created_at"])),
			OwnedBy:   asString(mm["owned_by"]),
		}
		switch ops := mm["capabilities"].(type) {
		case []any:
			for _, op := range ops {
				if s := asString(op); s != "" {
					entry.Capabilities = append(entry.Capabilities, s)
				}
			}
		case []string:
			entry.Capabilities = append(entry.Capabilities, ops...)
		}
		if entry.ID == "" || len(entry.Capabilities) == 0 {
			continue
		}
		snap.Models = append(snap.Models, entry)
	}
	if len(snap.Models) == 0 {
		return nil
	}
	return &snap
}

func snapshotToMap(snap *events.AccountModelSnapshot) map[string]any {
	if snap == nil {
		return nil
	}
	models := make([]map[string]any, 0, len(snap.Models))
	for _, model := range snap.Models {
		models = append(models, map[string]any{
			"id":           model.ID,
			"capabilities": append([]string(nil), model.Capabilities...),
			"created_at":   model.CreatedAt,
			"owned_by":     model.OwnedBy,
		})
	}
	return map[string]any{
		"account_id":    snap.AccountID,
		"models":        models,
		"discovered_at": snap.DiscoveredAt,
		"expires_at":    snap.ExpiresAt,
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func shortID(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:12]
}

func asString(v any) string {
	if v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	default:
		return ""
	}
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s := asString(m[k]); s != "" {
			return s
		}
	}
	return ""
}

func cloneMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	ret := make(map[string]any, len(source))
	for key, value := range source {
		ret[key] = value
	}
	return ret
}

func asInt(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case json.Number:
		i, _ := t.Int64()
		return int(i)
	default:
		return 0
	}
}

func trim(v string) string {
	for len(v) > 0 && (v[0] == ' ' || v[0] == '\t' || v[0] == '\n') {
		v = v[1:]
	}
	for len(v) > 0 {
		c := v[len(v)-1]
		if c != ' ' && c != '\t' && c != '\n' {
			break
		}
		v = v[:len(v)-1]
	}
	return v
}

func bounded(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func toSet(items []string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, item := range items {
		out[trim(item)] = struct{}{}
	}
	return out
}

func validStatus(value string) bool {
	switch value {
	case StatusNormal, StatusLimited, StatusAbnormal, StatusDisabled:
		return true
	default:
		return false
	}
}

func (s *Store) tokensForIDsLocked(ids []string) []string {
	seen := map[string]struct{}{}
	tokens := make([]string, 0, len(ids))
	for _, id := range ids {
		token, found := s.tokenForIDLocked(trim(id))
		if !found {
			continue
		}
		if _, duplicate := seen[token]; duplicate {
			continue
		}
		seen[token] = struct{}{}
		tokens = append(tokens, token)
	}
	return tokens
}

// tokenForIDLocked rejects an ambiguous legacy data file rather than applying
// a destructive operation to an arbitrary account.
func (s *Store) tokenForIDLocked(id string) (string, bool) {
	if id == "" {
		return "", false
	}
	var token string
	for _, candidate := range s.order {
		acc := s.items[candidate]
		if acc == nil || acc.ID != id {
			continue
		}
		if token != "" {
			return "", false
		}
		token = candidate
	}
	return token, token != ""
}

func (s *Store) resolveTokenLocked(token string) string {
	seen := map[string]struct{}{}
	for token != "" {
		next, ok := s.aliases[token]
		if !ok {
			return token
		}
		if _, duplicate := seen[token]; duplicate {
			return token
		}
		seen[token] = struct{}{}
		token = next
	}
	return token
}

func jwtTime(token, field string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var values struct {
		Exp int64 `json:"exp"`
		Iat int64 `json:"iat"`
	}
	if json.Unmarshal(payload, &values) != nil {
		return time.Time{}, false
	}
	value := values.Exp
	if field == "iat" {
		value = values.Iat
	}
	if value <= 0 {
		return time.Time{}, false
	}
	return time.Unix(value, 0).UTC(), true
}

func extraTime(acc *Account, key string) (time.Time, bool) {
	if acc.Extra == nil {
		return time.Time{}, false
	}
	value, _ := acc.Extra[key].(string)
	return parseTime(value)
}

func recentExtraTime(acc *Account, key string, now time.Time, within time.Duration) bool {
	value, ok := extraTime(acc, key)
	return ok && now.Sub(value) < within
}

func parseTime(value string) (time.Time, bool) {
	parsed, err := time.Parse(time.RFC3339, trim(value))
	if err != nil {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}
