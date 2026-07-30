// Package store persists and schedules Codex OAuth accounts.
package store

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	events "ai-proxy/internal/modules/blocks/codexaccountpool/pkg/events"
	"ai-proxy/internal/pkg/aiproxystate"
	"github.com/google/uuid"
)

const (
	defaultRateLimitCooldown = time.Minute
	defaultTransientCooldown = 30 * time.Second
)

type cooldown struct {
	Until      time.Time `json:"until"`
	ErrorClass string    `json:"error_class"`
}

type account struct {
	ID                  string              `json:"id"`
	AccessToken         string              `json:"access_token"`
	RefreshToken        string              `json:"refresh_token"`
	IDToken             string              `json:"id_token,omitempty"`
	AccountIDHeader     string              `json:"account_id,omitempty"`
	Email               string              `json:"email,omitempty"`
	PlanType            string              `json:"plan_type,omitempty"`
	Expired             string              `json:"expired,omitempty"`
	Proxy               string              `json:"proxy,omitempty"`
	Status              string              `json:"status"`
	Success             int                 `json:"success"`
	Fail                int                 `json:"fail"`
	CreatedAt           string              `json:"created_at"`
	LastUsedAt          string              `json:"last_used_at,omitempty"`
	LastRefreshAt       string              `json:"last_token_refresh_at,omitempty"`
	LastRefreshErrAt    string              `json:"last_token_refresh_error_at,omitempty"`
	LastRefreshErrClass string              `json:"last_token_refresh_error_class,omitempty"`
	Cooldowns           map[string]cooldown `json:"cooldowns,omitempty"`
}

type Store struct {
	mu        sync.Mutex
	documents *aiproxystate.Documents
	items     map[string]*account
	order     []string
	index     int
}

func Open(databasePath, memoryLimit string, threads int) (*Store, error) {
	documents, err := aiproxystate.Open(databasePath, memoryLimit, threads)
	if err != nil {
		return nil, err
	}
	s := &Store{documents: documents, items: map[string]*account{}}
	if err := s.load(); err != nil {
		_ = documents.Close()
		return nil, fmt.Errorf("load codex OAuth account state: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error {
	if s == nil || s.documents == nil {
		return nil
	}
	return s.documents.Close()
}

func (s *Store) load() error {
	rows, err := s.documents.LoadCodexOAuthAccounts()
	if err != nil {
		return err
	}
	for _, row := range rows {
		var item account
		if err := json.Unmarshal(row.Payload, &item); err != nil {
			return fmt.Errorf("decode account %q: %w", row.ID, err)
		}
		if item.ID == "" {
			item.ID = row.ID
		}
		if item.ID == "" || strings.TrimSpace(item.AccessToken) == "" || strings.TrimSpace(item.RefreshToken) == "" {
			continue
		}
		if item.Status == "" {
			item.Status = events.StatusNormal
		}
		s.items[item.ID] = &item
		s.order = append(s.order, item.ID)
	}
	return nil
}

func (s *Store) saveLocked() error {
	rows := make([]aiproxystate.CodexOAuthAccountRow, 0, len(s.order))
	for position, id := range s.order {
		item := s.items[id]
		if item == nil {
			continue
		}
		payload, err := json.Marshal(item)
		if err != nil {
			return err
		}
		rows = append(rows, aiproxystate.CodexOAuthAccountRow{ID: id, Position: position, Payload: payload})
	}
	return s.documents.ReplaceCodexOAuthAccounts(rows)
}

func (s *Store) List() []events.AccountView {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	out := make([]events.AccountView, 0, len(s.order))
	for _, id := range s.order {
		if item := s.items[id]; item != nil {
			out = append(out, toView(item, now))
		}
	}
	return out
}

func (s *Store) Import(inputs []events.CredentialInput) (added, updated, skipped int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Validate the complete batch before changing state. A malformed trailing
	// proxy must not leave earlier credentials half imported in memory.
	normalized := make([]events.CredentialInput, 0, len(inputs))
	seenRefresh := map[string]struct{}{}
	seenAccess := map[string]struct{}{}
	for _, input := range inputs {
		input.AccessToken = strings.TrimSpace(input.AccessToken)
		input.RefreshToken = strings.TrimSpace(input.RefreshToken)
		if input.AccessToken == "" || input.RefreshToken == "" {
			skipped++
			continue
		}
		if _, found := seenRefresh[input.RefreshToken]; found {
			skipped++
			continue
		}
		if _, found := seenAccess[input.AccessToken]; found {
			skipped++
			continue
		}
		if err := validateProxy(input.Proxy); err != nil {
			return 0, 0, skipped, err
		}
		seenRefresh[input.RefreshToken] = struct{}{}
		seenAccess[input.AccessToken] = struct{}{}
		normalized = append(normalized, input)
	}
	for _, input := range normalized {
		var existing *account
		for _, candidate := range s.items {
			if candidate.RefreshToken == input.RefreshToken || candidate.AccessToken == input.AccessToken {
				existing = candidate
				break
			}
		}
		if existing == nil {
			existing = &account{ID: uuid.NewString(), CreatedAt: time.Now().UTC().Format(time.RFC3339), Status: events.StatusNormal}
			s.items[existing.ID] = existing
			s.order = append(s.order, existing.ID)
			added++
		} else {
			updated++
		}
		existing.AccessToken = input.AccessToken
		existing.RefreshToken = input.RefreshToken
		existing.IDToken = strings.TrimSpace(input.IDToken)
		existing.AccountIDHeader = strings.TrimSpace(input.AccountID)
		existing.Email = strings.TrimSpace(input.Email)
		existing.Expired = strings.TrimSpace(input.Expired)
		existing.Proxy = strings.TrimSpace(input.Proxy)
		if existing.Status == "" {
			existing.Status = events.StatusNormal
		}
	}
	if err := s.saveLocked(); err != nil {
		return 0, 0, 0, err
	}
	return added, updated, skipped, nil
}

func (s *Store) Delete(ids []string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	deleted := 0
	for _, id := range unique(ids) {
		if _, found := s.items[id]; !found {
			continue
		}
		delete(s.items, id)
		deleted++
	}
	if deleted == 0 {
		return 0, nil
	}
	order := s.order[:0]
	for _, id := range s.order {
		if _, exists := s.items[id]; exists {
			order = append(order, id)
		}
	}
	s.order = order
	if s.index >= len(s.order) {
		s.index = 0
	}
	return deleted, s.saveLocked()
}

func (s *Store) Update(id string, status, proxy *string) (events.AccountView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item := s.items[strings.TrimSpace(id)]
	if item == nil {
		return events.AccountView{}, fmt.Errorf("account not found")
	}
	if status != nil {
		value := strings.ToLower(strings.TrimSpace(*status))
		if value != events.StatusNormal && value != events.StatusAbnormal && value != events.StatusDisabled {
			return events.AccountView{}, fmt.Errorf("invalid account status")
		}
		item.Status = value
	}
	if proxy != nil {
		if err := validateProxy(*proxy); err != nil {
			return events.AccountView{}, err
		}
		item.Proxy = strings.TrimSpace(*proxy)
	}
	if err := s.saveLocked(); err != nil {
		return events.AccountView{}, err
	}
	return toView(item, time.Now().UTC()), nil
}

func (s *Store) Acquire(model string, exclude []string) (events.AcquireResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	excluded := make(map[string]struct{}, len(exclude))
	for _, id := range exclude {
		excluded[strings.TrimSpace(id)] = struct{}{}
	}
	for offset := 0; offset < len(s.order); offset++ {
		pos := (s.index + offset) % len(s.order)
		item := s.items[s.order[pos]]
		if item == nil || item.Status != events.StatusNormal || strings.TrimSpace(item.AccessToken) == "" {
			continue
		}
		if _, found := excluded[item.ID]; found || cooling(item, model, now) {
			continue
		}
		s.index = (pos + 1) % len(s.order)
		item.LastUsedAt = now.Format(time.RFC3339)
		if err := s.saveLocked(); err != nil {
			return events.AcquireResult{}, err
		}
		return events.AcquireResult{AccountID: item.ID, AccessToken: item.AccessToken, AccountIDHeader: item.AccountIDHeader, Proxy: item.Proxy}, nil
	}
	return events.AcquireResult{}, fmt.Errorf("no eligible Codex OAuth account")
}

func (s *Store) RefreshCredential(id string) (events.CredentialInput, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item := s.items[strings.TrimSpace(id)]
	if item == nil || strings.TrimSpace(item.RefreshToken) == "" {
		return events.CredentialInput{}, false
	}
	return events.CredentialInput{AccessToken: item.AccessToken, RefreshToken: item.RefreshToken, IDToken: item.IDToken, AccountID: item.AccountIDHeader, Email: item.Email, Expired: item.Expired, Proxy: item.Proxy}, true
}

func (s *Store) ApplyRefresh(id string, input events.CredentialInput) (events.RefreshTokenResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item := s.items[strings.TrimSpace(id)]
	if item == nil {
		return events.RefreshTokenResult{}, fmt.Errorf("account not found")
	}
	if strings.TrimSpace(input.AccessToken) == "" {
		return events.RefreshTokenResult{}, fmt.Errorf("refreshed access token is empty")
	}
	item.AccessToken = strings.TrimSpace(input.AccessToken)
	if strings.TrimSpace(input.RefreshToken) != "" {
		item.RefreshToken = strings.TrimSpace(input.RefreshToken)
	}
	if strings.TrimSpace(input.IDToken) != "" {
		item.IDToken = strings.TrimSpace(input.IDToken)
	}
	if strings.TrimSpace(input.AccountID) != "" {
		item.AccountIDHeader = strings.TrimSpace(input.AccountID)
	}
	if strings.TrimSpace(input.Email) != "" {
		item.Email = strings.TrimSpace(input.Email)
	}
	if strings.TrimSpace(input.Expired) != "" {
		item.Expired = strings.TrimSpace(input.Expired)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	item.LastRefreshAt = now
	item.LastRefreshErrAt = ""
	item.LastRefreshErrClass = ""
	if item.Status == events.StatusAbnormal {
		item.Status = events.StatusNormal
	}
	if err := s.saveLocked(); err != nil {
		return events.RefreshTokenResult{}, err
	}
	return events.RefreshTokenResult{AccountID: item.ID, AccessToken: item.AccessToken, AccountIDHeader: item.AccountIDHeader, Proxy: item.Proxy, Refreshed: true}, nil
}

func (s *Store) RecordRefreshFailure(id, errorClass string, permanent bool) (events.RefreshTokenResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item := s.items[strings.TrimSpace(id)]
	if item == nil {
		return events.RefreshTokenResult{}, fmt.Errorf("account not found")
	}
	item.LastRefreshErrAt = time.Now().UTC().Format(time.RFC3339)
	item.LastRefreshErrClass = strings.TrimSpace(errorClass)
	if permanent {
		item.Status = events.StatusAbnormal
	}
	if err := s.saveLocked(); err != nil {
		return events.RefreshTokenResult{}, err
	}
	return events.RefreshTokenResult{AccountID: item.ID, PermanentFailure: permanent, ErrorClass: errorClass}, nil
}

func (s *Store) RecordResult(id, model string, success bool, errorClass string, retryAfterSeconds int) (events.AccountView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item := s.items[strings.TrimSpace(id)]
	if item == nil {
		return events.AccountView{}, fmt.Errorf("account not found")
	}
	if success {
		item.Success++
	} else {
		item.Fail++
		switch strings.TrimSpace(errorClass) {
		case events.ErrorInvalidToken:
			item.Status = events.StatusAbnormal
		case events.ErrorRateLimit, events.ErrorTimeout, events.ErrorNetwork, events.ErrorUpstream:
			until := time.Now().UTC().Add(defaultTransientCooldown)
			if errorClass == events.ErrorRateLimit {
				until = time.Now().UTC().Add(defaultRateLimitCooldown)
			}
			if retryAfterSeconds > 0 {
				until = time.Now().UTC().Add(time.Duration(retryAfterSeconds) * time.Second)
			}
			if item.Cooldowns == nil {
				item.Cooldowns = map[string]cooldown{}
			}
			item.Cooldowns[strings.TrimSpace(model)] = cooldown{Until: until, ErrorClass: errorClass}
		}
	}
	if err := s.saveLocked(); err != nil {
		return events.AccountView{}, err
	}
	return toView(item, time.Now().UTC()), nil
}

func (s *Store) Health() events.HealthResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	result := events.HealthResult{Total: len(s.items)}
	for _, item := range s.items {
		switch item.Status {
		case events.StatusDisabled:
			result.Disabled++
		case events.StatusAbnormal:
			result.Abnormal++
		default:
			if !cooling(item, "", now) {
				result.Available++
			}
		}
	}
	return result
}

func (s *Store) View(id string) (events.AccountView, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item := s.items[strings.TrimSpace(id)]
	if item == nil {
		return events.AccountView{}, false
	}
	return toView(item, time.Now().UTC()), true
}

// ViewByRefreshToken is intentionally available only inside the account-pool
// owner. It lets OAuth completion return the newly persisted redacted view
// even when the token response does not contain an email claim.
func (s *Store) ViewByRefreshToken(refreshToken string) (events.AccountView, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	refreshToken = strings.TrimSpace(refreshToken)
	for _, item := range s.items {
		if item != nil && item.RefreshToken == refreshToken {
			return toView(item, time.Now().UTC()), true
		}
	}
	return events.AccountView{}, false
}

// RefreshDue returns credentials that have a parseable expiry inside the lead
// window. Credentials without expiry metadata are still refreshed on a 401,
// but a periodic scan never hammers them merely because it lacks a deadline.
func (s *Store) RefreshDue(now time.Time, lead time.Duration) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if lead < 0 {
		lead = 0
	}
	due := make([]string, 0)
	for _, id := range s.order {
		item := s.items[id]
		if item == nil || item.Status != events.StatusNormal || strings.TrimSpace(item.RefreshToken) == "" {
			continue
		}
		expiresAt, ok := parseExpiry(item.Expired)
		if ok && !expiresAt.After(now.Add(lead)) {
			due = append(due, item.ID)
		}
	}
	return due
}

func cooling(item *account, model string, now time.Time) bool {
	if item == nil || len(item.Cooldowns) == 0 {
		return false
	}
	for key, value := range item.Cooldowns {
		if !now.Before(value.Until) {
			continue
		}
		if key == "" || key == strings.TrimSpace(model) {
			return true
		}
	}
	return false
}

func parseExpiry(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed.UTC(), true
	}
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil || seconds <= 0 {
		return time.Time{}, false
	}
	if seconds > 1_000_000_000_000 {
		seconds /= 1000
	}
	return time.Unix(seconds, 0).UTC(), true
}

func toView(item *account, now time.Time) events.AccountView {
	view := events.AccountView{ID: item.ID, Email: item.Email, PlanType: item.PlanType, Status: item.Status, Success: item.Success, Fail: item.Fail, CreatedAt: item.CreatedAt, LastUsedAt: item.LastUsedAt, LastTokenRefreshAt: item.LastRefreshAt, LastTokenRefreshErrorAt: item.LastRefreshErrAt, LastTokenRefreshErrorClass: item.LastRefreshErrClass}
	for model, value := range item.Cooldowns {
		if now.After(value.Until) {
			continue
		}
		view.Cooldowns = append(view.Cooldowns, events.CooldownView{Model: model, Until: value.Until.Format(time.RFC3339), ErrorClass: value.ErrorClass})
	}
	sort.Slice(view.Cooldowns, func(i, j int) bool { return view.Cooldowns[i].Model < view.Cooldowns[j].Model })
	return view
}

func validateProxy(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("invalid account proxy URL")
	}
	return nil
}

func unique(values []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, found := seen[value]; found {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
