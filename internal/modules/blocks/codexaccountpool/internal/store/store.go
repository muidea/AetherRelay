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
	"ai-proxy/internal/pkg/accountidentity"
	"ai-proxy/internal/pkg/aiproxycredential"
	"ai-proxy/internal/pkg/aiproxystate"
	"github.com/google/uuid"
)

const (
	defaultRateLimitCooldown = time.Minute
	defaultTransientCooldown = 30 * time.Second
	modelDiscoveryRetryBase  = 30 * time.Second
	modelDiscoveryRetryMax   = 5 * time.Minute
	secureDocumentScope      = "codex_oauth_accounts"
)

type cooldown struct {
	Until      time.Time `json:"until"`
	ErrorClass string    `json:"error_class"`
}

type quotaObservation struct {
	State      string `json:"state"`
	ObservedAt string `json:"observed_at"`
	ResetAt    string `json:"reset_at,omitempty"`
}

type account struct {
	ID                  string                      `json:"id"`
	AccessToken         string                      `json:"access_token"`
	RefreshToken        string                      `json:"refresh_token"`
	IDToken             string                      `json:"id_token,omitempty"`
	AccountIDHeader     string                      `json:"account_id,omitempty"`
	Email               string                      `json:"email,omitempty"`
	PlanType            string                      `json:"plan_type,omitempty"`
	Expired             string                      `json:"expired,omitempty"`
	Proxy               string                      `json:"proxy,omitempty"`
	Status              string                      `json:"status"`
	Success             int                         `json:"success"`
	Fail                int                         `json:"fail"`
	CreatedAt           string                      `json:"created_at"`
	LastUsedAt          string                      `json:"last_used_at,omitempty"`
	LastRefreshAt       string                      `json:"last_token_refresh_at,omitempty"`
	LastRefreshErrAt    string                      `json:"last_token_refresh_error_at,omitempty"`
	LastRefreshErrClass string                      `json:"last_token_refresh_error_class,omitempty"`
	Cooldowns           map[string]cooldown         `json:"cooldowns,omitempty"`
	QuotaObservations   map[string]quotaObservation `json:"quota_observations,omitempty"`
	// ModelSnapshot is the constrained account-scoped Codex capability cache.
	// It never contains raw upstream JSON or credentials beyond this account.
	ModelSnapshot           *events.AccountModelSnapshot `json:"model_snapshot,omitempty"`
	ModelDiscoveryFailures  int                          `json:"model_discovery_failures,omitempty"`
	ModelDiscoveryRetryAt   string                       `json:"model_discovery_retry_at,omitempty"`
	ModelDiscoveryLastError string                       `json:"model_discovery_last_error,omitempty"`
	// UsageSnapshot keeps only the allowlisted upstream usage projection. It
	// deliberately excludes raw upstream JSON and all request credentials.
	UsageSnapshot       *events.AccountUsageSnapshot `json:"usage_snapshot,omitempty"`
	UsageRefreshErrorAt string                       `json:"usage_refresh_error_at,omitempty"`
	UsageRefreshError   string                       `json:"usage_refresh_error,omitempty"`
}

type Store struct {
	mu          sync.Mutex
	documents   *aiproxystate.Documents
	credentials *aiproxycredential.Codec
	items       map[string]*account
	order       []string
	index       int
	// catalogVersion increments whenever an account's routing eligibility or
	// cached model capability changes.
	catalogVersion uint64
}

func Open(databasePath, memoryLimit string, threads int, codec *aiproxycredential.Codec) (*Store, error) {
	if codec == nil {
		return nil, fmt.Errorf("account credential codec is required")
	}
	documents, err := aiproxystate.Open(databasePath, memoryLimit, threads)
	if err != nil {
		return nil, err
	}
	s := &Store{documents: documents, credentials: codec, items: map[string]*account{}}
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
	return s.loadEncrypted()
}

func (s *Store) loadEncrypted() error {
	rows, err := s.documents.LoadSecureDocuments(secureDocumentScope)
	if err != nil {
		return err
	}
	for _, row := range rows {
		payload, err := s.credentials.Open(secureDocumentScope, row.ID, row.Payload)
		if err != nil {
			return fmt.Errorf("decrypt account %q: %w", row.ID, err)
		}
		var item account
		if err := json.Unmarshal(payload, &item); err != nil {
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
	return s.saveEncryptedLocked()
}

func (s *Store) saveEncryptedLocked() error {
	rows := make([]aiproxystate.SecureDocumentRow, 0, len(s.order))
	for position, id := range s.order {
		item := s.items[id]
		if item == nil {
			continue
		}
		payload, err := json.Marshal(item)
		if err != nil {
			return err
		}
		sealed, err := s.credentials.Seal(secureDocumentScope, id, payload)
		if err != nil {
			return err
		}
		rows = append(rows, aiproxystate.SecureDocumentRow{ID: id, Position: position, Payload: sealed})
	}
	return s.documents.ReplaceSecureDocuments(secureDocumentScope, rows)
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
	added, updated, skipped, _, err = s.ImportWithIDs(inputs)
	return added, updated, skipped, err
}

// ImportWithIDs imports credentials and returns the stable IDs affected by
// this batch. Management orchestration uses these IDs to scope follow-up
// discovery and usage work to the imported accounts only.
func (s *Store) ImportWithIDs(inputs []events.CredentialInput) (added, updated, skipped int, ids []string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Validate the complete batch before changing state. A malformed trailing
	// proxy must not leave earlier credentials half imported in memory.
	normalized := make([]events.CredentialInput, 0, len(inputs))
	seenRefresh := map[string]struct{}{}
	seenAccess := map[string]struct{}{}
	for _, input := range inputs {
		if kind := strings.ToLower(strings.TrimSpace(input.CredentialType)); kind != "" && kind != "codex_cli" {
			return 0, 0, 0, nil, fmt.Errorf("credential_type %q cannot be imported into Codex OAuth", kind)
		}
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
			return 0, 0, skipped, nil, err
		}
		seenRefresh[input.RefreshToken] = struct{}{}
		seenAccess[input.AccessToken] = struct{}{}
		normalized = append(normalized, input)
	}
	changed := false
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
		ids = append(ids, existing.ID)
		changed = true
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
		// An import may replace a credential for a different account. Do not
		// route from a capability snapshot or show a usage observation learned
		// with the old credential.
		existing.ModelSnapshot = nil
		existing.ModelDiscoveryFailures = 0
		existing.ModelDiscoveryRetryAt = ""
		existing.ModelDiscoveryLastError = ""
		existing.UsageSnapshot = nil
		existing.UsageRefreshErrorAt = ""
		existing.UsageRefreshError = ""
	}
	if err := s.saveLocked(); err != nil {
		return 0, 0, 0, nil, err
	}
	if changed {
		s.bumpCatalogLocked()
	}
	return added, updated, skipped, unique(ids), nil
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
	s.bumpCatalogLocked()
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
	changed := false
	if status != nil {
		value := strings.ToLower(strings.TrimSpace(*status))
		if value != events.StatusNormal && value != events.StatusAbnormal && value != events.StatusDisabled {
			return events.AccountView{}, fmt.Errorf("invalid account status")
		}
		if item.Status != value {
			item.Status = value
			changed = true
		}
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
	if changed {
		s.bumpCatalogLocked()
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
		if _, found := excluded[item.ID]; found || cooling(item, model, now) || !accountSupportsModel(item, model, now) {
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

func (s *Store) ExportByIDs(ids []string) []events.CredentialInput {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]events.CredentialInput, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		item := s.items[id]
		if item == nil || strings.TrimSpace(item.AccessToken) == "" || strings.TrimSpace(item.RefreshToken) == "" {
			continue
		}
		result = append(result, events.CredentialInput{
			CredentialType: "codex_cli",
			AccessToken:    item.AccessToken, RefreshToken: item.RefreshToken, IDToken: item.IDToken,
			AccountID: item.AccountIDHeader, Email: item.Email, Expired: item.Expired, Proxy: item.Proxy,
		})
	}
	return result
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
	statusChanged := false
	if item.Status == events.StatusAbnormal {
		item.Status = events.StatusNormal
		statusChanged = true
	}
	if err := s.saveLocked(); err != nil {
		return events.RefreshTokenResult{}, err
	}
	if statusChanged {
		s.bumpCatalogLocked()
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
	if err := s.saveLocked(); err != nil {
		return events.RefreshTokenResult{}, err
	}
	return events.RefreshTokenResult{AccountID: item.ID, PermanentFailure: permanent, ErrorClass: errorClass}, nil
}

func (s *Store) RecordResult(id, model string, success bool, errorClass string, retryAfterSeconds int, quotaExhausted bool, quotaResetAt string) (events.AccountView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item := s.items[strings.TrimSpace(id)]
	if item == nil {
		return events.AccountView{}, fmt.Errorf("account not found")
	}
	now := time.Now().UTC()
	statusChanged := false
	model = strings.TrimSpace(model)
	if success {
		item.Success++
		delete(item.QuotaObservations, model)
		if item.Status == events.StatusAbnormal {
			item.Status = events.StatusNormal
			statusChanged = true
		}
	} else {
		item.Fail++
		if quotaExhausted && model != "" {
			if item.QuotaObservations == nil {
				item.QuotaObservations = map[string]quotaObservation{}
			}
			item.QuotaObservations[model] = quotaObservation{
				State:      "exhausted",
				ObservedAt: now.Format(time.RFC3339),
				ResetAt:    normalizeQuotaResetAt(quotaResetAt),
			}
		}
		switch strings.TrimSpace(errorClass) {
		case events.ErrorInvalidToken:
			if item.Status != events.StatusAbnormal {
				item.Status = events.StatusAbnormal
				statusChanged = true
			}
		case events.ErrorRateLimit, events.ErrorTimeout, events.ErrorNetwork, events.ErrorUpstream:
			until := now.Add(defaultTransientCooldown)
			if errorClass == events.ErrorRateLimit {
				until = now.Add(defaultRateLimitCooldown)
			}
			if retryAfterSeconds > 0 {
				until = now.Add(time.Duration(retryAfterSeconds) * time.Second)
			}
			// A verified Codex usage-limit reset is more precise than Retry-After.
			// Keep this account/model out of selection until that upstream-provided
			// recovery point, even when the generic retry hint is capped.
			if quotaExhausted {
				if resetAt, ok := parseExpiry(quotaResetAt); ok && resetAt.After(now) {
					until = resetAt
				}
			}
			if item.Cooldowns == nil {
				item.Cooldowns = map[string]cooldown{}
			}
			item.Cooldowns[model] = cooldown{Until: until, ErrorClass: errorClass}
		}
	}
	if err := s.saveLocked(); err != nil {
		return events.AccountView{}, err
	}
	if statusChanged {
		s.bumpCatalogLocked()
	}
	return toView(item, now), nil
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
	view := events.AccountView{
		ID:                         item.ID,
		IdentityKey:                accountidentity.Key(item.AccountIDHeader, item.Email),
		Email:                      item.Email,
		PlanType:                   item.PlanType,
		Status:                     item.Status,
		Success:                    item.Success,
		Fail:                       item.Fail,
		CreatedAt:                  item.CreatedAt,
		LastUsedAt:                 item.LastUsedAt,
		LastTokenRefreshAt:         item.LastRefreshAt,
		LastTokenRefreshErrorAt:    item.LastRefreshErrAt,
		LastTokenRefreshErrorClass: item.LastRefreshErrClass,
		ModelDiscoveryRetryAt:      item.ModelDiscoveryRetryAt,
		ModelDiscoveryLastError:    item.ModelDiscoveryLastError,
		UsageRefreshErrorAt:        item.UsageRefreshErrorAt,
		UsageRefreshError:          item.UsageRefreshError,
	}
	if item.ModelSnapshot != nil {
		snapshot := normalizeSnapshot(item.ID, *item.ModelSnapshot)
		view.ModelSnapshot = &snapshot
	}
	if item.UsageSnapshot != nil {
		snapshot := normalizeUsageSnapshot(*item.UsageSnapshot)
		view.UsageSnapshot = &snapshot
	}
	for model, value := range item.Cooldowns {
		if now.After(value.Until) {
			continue
		}
		view.Cooldowns = append(view.Cooldowns, events.CooldownView{Model: model, Until: value.Until.Format(time.RFC3339), ErrorClass: value.ErrorClass})
	}
	sort.Slice(view.Cooldowns, func(i, j int) bool { return view.Cooldowns[i].Model < view.Cooldowns[j].Model })
	for model, value := range item.QuotaObservations {
		if value.State != "exhausted" {
			continue
		}
		if resetAt, err := time.Parse(time.RFC3339, value.ResetAt); err == nil && !resetAt.After(now) {
			continue
		}
		view.QuotaObservations = append(view.QuotaObservations, events.QuotaObservation{Model: model, State: value.State, ObservedAt: value.ObservedAt, ResetAt: value.ResetAt})
	}
	sort.Slice(view.QuotaObservations, func(i, j int) bool { return view.QuotaObservations[i].Model < view.QuotaObservations[j].Model })
	return view
}

func normalizeQuotaResetAt(value string) string {
	resetAt, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if err != nil {
		return ""
	}
	return resetAt.UTC().Format(time.RFC3339)
}

// ListDiscoveryCandidates returns normal accounts for automatic discovery. An
// explicit operator selection may also retry an abnormal account with its
// current access token; disabled remains an operator-owned state. Credential
// fields are strictly EventHub-internal and cannot reach Admin.
func (s *Store) ListDiscoveryCandidates(accountIDs []string) events.ListDiscoveryCandidatesResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	requested := make(map[string]struct{}, len(accountIDs))
	for _, accountID := range accountIDs {
		if accountID = strings.TrimSpace(accountID); accountID != "" {
			requested[accountID] = struct{}{}
		}
	}
	out := events.ListDiscoveryCandidatesResult{Version: s.catalogVersion}
	for _, id := range s.order {
		item := s.items[id]
		if item == nil || strings.TrimSpace(item.AccessToken) == "" {
			continue
		}
		if item.Status != events.StatusNormal && (len(requested) == 0 || item.Status != events.StatusAbnormal) {
			continue
		}
		if len(requested) > 0 {
			if _, found := requested[item.ID]; !found {
				continue
			}
		}
		needs := item.ModelSnapshot == nil || snapshotExpired(item.ModelSnapshot, now)
		out.Candidates = append(out.Candidates, events.DiscoveryCandidate{
			AccountID:       item.ID,
			AccessToken:     item.AccessToken,
			AccountIDHeader: item.AccountIDHeader,
			Proxy:           item.Proxy,
			NeedsDiscovery:  needs,
			DiscoveryDue:    needs && modelDiscoveryRetryDue(item, now),
		})
	}
	return out
}

func (s *Store) PutModelSnapshot(accountID string, snapshot events.AccountModelSnapshot) (uint64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return s.catalogVersion, false, fmt.Errorf("account id is required")
	}
	item := s.items[accountID]
	if item == nil {
		return s.catalogVersion, false, nil
	}
	clean := normalizeSnapshot(accountID, snapshot)
	item.ModelSnapshot = &clean
	item.ModelDiscoveryFailures = 0
	item.ModelDiscoveryRetryAt = ""
	item.ModelDiscoveryLastError = ""
	if item.Status == events.StatusAbnormal {
		item.Status = events.StatusNormal
	}
	if err := s.saveLocked(); err != nil {
		return s.catalogVersion, false, err
	}
	s.bumpCatalogLocked()
	return s.catalogVersion, true, nil
}

func (s *Store) RecordModelDiscoveryFailure(accountID, message string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return "", false, fmt.Errorf("account id is required")
	}
	item := s.items[accountID]
	if item == nil {
		return "", false, nil
	}
	item.ModelDiscoveryFailures++
	retryAt := time.Now().UTC().Add(modelDiscoveryRetryDelay(item.ModelDiscoveryFailures))
	item.ModelDiscoveryRetryAt = retryAt.Format(time.RFC3339)
	item.ModelDiscoveryLastError = bounded(message, 256)
	if err := s.saveLocked(); err != nil {
		return "", false, err
	}
	return item.ModelDiscoveryRetryAt, true, nil
}

// ListUsageCandidates returns routable accounts for an unscoped refresh. An
// explicit operator selection may also retry an abnormal account because the
// usage flow can refresh an invalid credential once and recover it. Disabled
// remains an operator-owned state and is never bypassed. Credential fields
// remain restricted to the account-pool/proxy EventHub path.
func (s *Store) ListUsageCandidates(accountIDs []string) events.ListUsageCandidatesResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	requested := make(map[string]struct{}, len(accountIDs))
	for _, accountID := range accountIDs {
		if accountID = strings.TrimSpace(accountID); accountID != "" {
			requested[accountID] = struct{}{}
		}
	}
	result := events.ListUsageCandidatesResult{}
	for _, id := range s.order {
		item := s.items[id]
		if item == nil || strings.TrimSpace(item.AccessToken) == "" {
			continue
		}
		if item.Status != events.StatusNormal && (len(requested) == 0 || item.Status != events.StatusAbnormal) {
			continue
		}
		if len(requested) > 0 {
			if _, found := requested[item.ID]; !found {
				continue
			}
		}
		result.Candidates = append(result.Candidates, events.UsageCandidate{
			AccountID:       item.ID,
			AccessToken:     item.AccessToken,
			AccountIDHeader: item.AccountIDHeader,
			Proxy:           item.Proxy,
		})
	}
	return result
}

// PutUsageSnapshot saves a sanitized, bounded usage observation. A successful
// observation clears only its previous refresh error; it does not manipulate
// routing cooldowns, which remain driven by real request outcomes.
func (s *Store) PutUsageSnapshot(accountID string, snapshot events.AccountUsageSnapshot) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item := s.items[strings.TrimSpace(accountID)]
	if item == nil {
		return false, nil
	}
	clean := normalizeUsageSnapshot(snapshot)
	item.UsageSnapshot = &clean
	item.UsageRefreshErrorAt = ""
	item.UsageRefreshError = ""
	if clean.PlanType != "" {
		item.PlanType = clean.PlanType
	}
	statusChanged := false
	if item.Status == events.StatusAbnormal {
		item.Status = events.StatusNormal
		statusChanged = true
	}
	if err := s.saveLocked(); err != nil {
		return false, err
	}
	if statusChanged {
		s.bumpCatalogLocked()
	}
	return true, nil
}

// RecordUsageFailure preserves the last successful usage snapshot. The
// bounded error is only operational context for the Admin view.
func (s *Store) RecordUsageFailure(accountID, message string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item := s.items[strings.TrimSpace(accountID)]
	if item == nil {
		return false, nil
	}
	item.UsageRefreshErrorAt = time.Now().UTC().Format(time.RFC3339)
	item.UsageRefreshError = bounded(message, 160)
	if err := s.saveLocked(); err != nil {
		return false, err
	}
	return true, nil
}

// CatalogSnapshot builds the deduplicated union across routable accounts and
// non-expired snapshots. A model is published only if the selected account can
// actually be acquired for it.
func (s *Store) CatalogSnapshot() events.CatalogSnapshotResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	byID := make(map[string]*events.CatalogModel)
	available := 0
	var latest time.Time
	for _, id := range s.order {
		item := s.items[id]
		if item == nil || item.Status != events.StatusNormal {
			continue
		}
		available++
		if item.ModelSnapshot == nil || snapshotExpired(item.ModelSnapshot, now) {
			continue
		}
		if discoveredAt, err := time.Parse(time.RFC3339, item.ModelSnapshot.DiscoveredAt); err == nil && discoveredAt.After(latest) {
			latest = discoveredAt
		}
		for _, model := range item.ModelSnapshot.Models {
			modelID := strings.TrimSpace(model.ID)
			if !validSnapshotModelID(modelID) {
				continue
			}
			entry := byID[modelID]
			if entry == nil {
				entry = &events.CatalogModel{ID: modelID, CreatedAt: model.CreatedAt, OwnedBy: strings.TrimSpace(model.OwnedBy)}
				byID[modelID] = entry
			}
			if model.CreatedAt > entry.CreatedAt {
				entry.CreatedAt = model.CreatedAt
			}
			if entry.OwnedBy == "" {
				entry.OwnedBy = strings.TrimSpace(model.OwnedBy)
			}
			entry.AccountIDs = appendUnique(entry.AccountIDs, item.ID)
		}
	}
	out := events.CatalogSnapshotResult{Version: s.catalogVersion, AvailableAccounts: available}
	if !latest.IsZero() {
		out.UpdatedAt = latest.UTC().Format(time.RFC3339)
	}
	for _, entry := range byID {
		sort.Strings(entry.AccountIDs)
		out.Models = append(out.Models, *entry)
	}
	sort.Slice(out.Models, func(i, j int) bool { return out.Models[i].ID < out.Models[j].ID })
	return out
}

func accountSupportsModel(item *account, model string, now time.Time) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	if item == nil || item.ModelSnapshot == nil || snapshotExpired(item.ModelSnapshot, now) {
		return false
	}
	for _, entry := range item.ModelSnapshot.Models {
		if entry.ID == model {
			return true
		}
	}
	return false
}

func snapshotExpired(snapshot *events.AccountModelSnapshot, now time.Time) bool {
	if snapshot == nil || strings.TrimSpace(snapshot.ExpiresAt) == "" {
		return snapshot == nil
	}
	expiresAt, err := time.Parse(time.RFC3339, snapshot.ExpiresAt)
	return err != nil || !expiresAt.After(now)
}

func modelDiscoveryRetryDue(item *account, now time.Time) bool {
	if item == nil || strings.TrimSpace(item.ModelDiscoveryRetryAt) == "" {
		return true
	}
	retryAt, err := time.Parse(time.RFC3339, item.ModelDiscoveryRetryAt)
	return err != nil || !retryAt.After(now)
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

func normalizeSnapshot(accountID string, snapshot events.AccountModelSnapshot) events.AccountModelSnapshot {
	out := events.AccountModelSnapshot{
		AccountID:    accountID,
		DiscoveredAt: strings.TrimSpace(snapshot.DiscoveredAt),
		ExpiresAt:    strings.TrimSpace(snapshot.ExpiresAt),
	}
	if out.DiscoveredAt == "" {
		out.DiscoveredAt = time.Now().UTC().Format(time.RFC3339)
	}
	seen := make(map[string]struct{}, len(snapshot.Models))
	for _, model := range snapshot.Models {
		id := strings.TrimSpace(model.ID)
		if !validSnapshotModelID(id) {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		out.Models = append(out.Models, events.AccountModelEntry{ID: id, CreatedAt: model.CreatedAt, OwnedBy: bounded(model.OwnedBy, 128)})
	}
	sort.Slice(out.Models, func(i, j int) bool { return out.Models[i].ID < out.Models[j].ID })
	return out
}

func normalizeUsageSnapshot(snapshot events.AccountUsageSnapshot) events.AccountUsageSnapshot {
	out := events.AccountUsageSnapshot{
		PlanType:   bounded(snapshot.PlanType, 64),
		ObservedAt: normalizeUsageTime(snapshot.ObservedAt, time.Now().UTC()),
		ExpiresAt:  normalizeUsageTime(snapshot.ExpiresAt, time.Time{}),
	}
	seen := make(map[string]struct{}, len(snapshot.Windows))
	for _, window := range snapshot.Windows {
		window.ID = bounded(window.ID, 96)
		if window.ID == "" {
			continue
		}
		if _, exists := seen[window.ID]; exists {
			continue
		}
		seen[window.ID] = struct{}{}
		window.Label = bounded(window.Label, 128)
		if window.UsedPercentKnown {
			if window.UsedPercent != window.UsedPercent {
				window.UsedPercent = 0
				window.UsedPercentKnown = false
			} else if window.UsedPercent < 0 {
				window.UsedPercent = 0
			} else if window.UsedPercent > 100 {
				window.UsedPercent = 100
			}
		} else {
			window.UsedPercent = 0
		}
		if window.WindowSeconds < 0 || window.WindowSeconds > 366*24*60*60 {
			window.WindowSeconds = 0
		}
		window.ResetAt = normalizeUsageTime(window.ResetAt, time.Time{})
		out.Windows = append(out.Windows, window)
	}
	sort.Slice(out.Windows, func(i, j int) bool { return out.Windows[i].ID < out.Windows[j].ID })
	return out
}

func normalizeUsageTime(value string, fallback time.Time) string {
	value = strings.TrimSpace(value)
	if value != "" {
		if parsed, err := time.Parse(time.RFC3339, value); err == nil {
			return parsed.UTC().Format(time.RFC3339)
		}
	}
	if !fallback.IsZero() {
		return fallback.UTC().Format(time.RFC3339)
	}
	return ""
}

func validSnapshotModelID(id string) bool {
	if id == "" || len(id) > 256 {
		return false
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func bounded(value string, limit int) string {
	value = strings.TrimSpace(value)
	if limit > 0 && len(value) > limit {
		return value[:limit]
	}
	return value
}

func (s *Store) bumpCatalogLocked() { s.catalogVersion++ }

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
