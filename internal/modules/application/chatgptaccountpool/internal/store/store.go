// Package store provides account-pool persistence.
package store

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	events "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/events"
)

const (
	StatusNormal   = "正常"
	StatusLimited  = "限流"
	StatusAbnormal = "异常"
	StatusDisabled = "禁用"
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
	// Extra retains Python-owned fields (OAuth id_token, refresh progress,
	// import metadata, etc.) when Go updates its account-pool fields.
	Extra map[string]any `json:"-"`
}

type Store struct {
	mu            sync.Mutex
	path          string
	items         map[string]*Account // access_token -> account
	aliases       map[string]string   // retired access token -> current token
	order         []string
	index         int
	imageInflight map[string]int
	concurrency   int
}

func New(path string, concurrency int) *Store {
	if concurrency < 1 {
		concurrency = 3
	}
	s := &Store{
		path:          path,
		items:         map[string]*Account{},
		aliases:       map[string]string{},
		imageInflight: map[string]int{},
		concurrency:   concurrency,
	}
	_ = s.load()
	return s
}

type TokenRefreshCandidate struct {
	AccessToken  string
	RefreshToken string
	Proxy        string
	Reason       string // expiring | keepalive
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
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	switch v := raw.(type) {
	case []any:
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			acc := mapToAccount(m)
			if acc.AccessToken == "" {
				continue
			}
			s.items[acc.AccessToken] = acc
			s.order = append(s.order, acc.AccessToken)
		}
	case map[string]any:
		// dict form token -> account
		for token, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			acc := mapToAccount(m)
			if acc.AccessToken == "" {
				acc.AccessToken = token
			}
			s.items[acc.AccessToken] = acc
			s.order = append(s.order, acc.AccessToken)
		}
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
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	list := make([]map[string]any, 0, len(s.order))
	for _, token := range s.order {
		if acc, ok := s.items[token]; ok {
			list = append(list, accountToMap(acc))
		}
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
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
		out = append(out, toView(acc, true))
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
		return oldToken, false, err
	}
	return newToken, newToken != oldToken, nil
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
	acc.Extra["last_token_refresh_at"] = time.Now().UTC().Format(time.RFC3339)
	delete(acc.Extra, "last_token_refresh_error")
	delete(acc.Extra, "last_token_refresh_error_at")
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
	acc.Status, acc.Quota = StatusDisabled, 0
	if err := s.saveLocked(); err != nil {
		return events.AccountView{}, false, err
	}
	return toView(acc, true), true, nil
}

func (s *Store) RecordTokenRefreshError(token, message string) error {
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
	acc.Extra["last_token_refresh_error"] = bounded(trim(message), 512)
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
	if typ != "" {
		acc.Type = typ
	}
	if status != "" {
		acc.Status = status
	}
	if quota != nil {
		acc.Quota = *quota
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
	if typ != nil {
		acc.Type = trim(*typ)
	}
	if status != nil {
		value := trim(*status)
		if !validStatus(value) {
			return events.AccountView{}, false, fmt.Errorf("invalid account status")
		}
		acc.Status = value
	}
	if quota != nil {
		if *quota < 0 {
			return events.AccountView{}, false, fmt.Errorf("quota must not be negative")
		}
		acc.Quota = *quota
	}
	if proxy != nil {
		acc.Proxy = trim(*proxy)
	}
	if err := s.saveLocked(); err != nil {
		return events.AccountView{}, false, err
	}
	return toView(acc, true), true, nil
}

func (s *Store) AcquireImageToken(planType, sourceType string, exclude []string) (events.AccountView, bool) {
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

func (s *Store) MarkImageResult(token string, success bool) (events.AccountView, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	token = s.resolveTokenLocked(token)
	acc, ok := s.items[token]
	if !ok {
		return events.AccountView{}, false
	}
	if success && acc.Quota > 0 {
		acc.Quota--
		if acc.Quota == 0 {
			acc.Status = StatusLimited
		}
	}
	_ = s.saveLocked()
	return toView(acc, true), true
}

func (s *Store) AcquireTextToken(exclude []string) (events.AccountView, bool) {
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
		acc.LastUsedAt = time.Now().UTC().Format(time.RFC3339)
		return toView(acc, true), true
	}
	return events.AccountView{}, false
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
		ID:         acc.ID,
		Email:      acc.Email,
		Type:       acc.Type,
		SourceType: acc.SourceType,
		Status:     acc.Status,
		Quota:      acc.Quota,
		Proxy:      acc.Proxy,
		LastUsedAt: acc.LastUsedAt,
	}
	if withToken {
		v.AccessToken = acc.AccessToken
	}
	return v
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
