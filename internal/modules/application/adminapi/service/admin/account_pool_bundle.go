package admin

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	accevents "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/events"
	codexevents "ai-proxy/internal/modules/blocks/codexaccountpool/pkg/events"
	"ai-proxy/internal/pkg/accountidentity"
	"github.com/google/uuid"
)

const (
	accountPoolBundleFormat        = "ai-proxy.account-pool-bundle"
	accountPoolBundleSchemaVersion = 2
)

type accountPoolBundle struct {
	Format        string                   `json:"format"`
	SchemaVersion int                      `json:"schema_version"`
	ExportedAt    string                   `json:"exported_at"`
	Accounts      []accountPoolBundleEntry `json:"accounts"`
	Replace       bool                     `json:"replace,omitempty"`
}

type accountPoolBundleEntry struct {
	AccountRef string                    `json:"account_ref"`
	Identity   accountPoolBundleIdentity `json:"identity"`
	Slots      accountPoolBundleSlots    `json:"slots"`
}

type accountPoolBundleIdentity struct {
	Email string `json:"email,omitempty"`
}

type accountPoolBundleSlots struct {
	ChatGPT *accountPoolBundleChatGPT `json:"chatgpt_web,omitempty"`
	Codex   *accountPoolBundleCodex   `json:"codex_cli,omitempty"`
}

type accountPoolBundleChatGPT struct {
	CredentialType string `json:"credential_type,omitempty"`
	AccountID      string `json:"account_id,omitempty"`
	IdentityKey    string `json:"identity_key,omitempty"`
	Email          string `json:"email,omitempty"`
	AccessToken    string `json:"access_token"`
	RefreshToken   string `json:"refresh_token"`
	IDToken        string `json:"id_token,omitempty"`
	Expired        string `json:"expired,omitempty"`
	Proxy          string `json:"proxy,omitempty"`
}

type accountPoolBundleCodex struct {
	CredentialType string `json:"credential_type,omitempty"`
	AccountID      string `json:"account_id,omitempty"`
	IdentityKey    string `json:"identity_key,omitempty"`
	Email          string `json:"email,omitempty"`
	AccessToken    string `json:"access_token"`
	RefreshToken   string `json:"refresh_token"`
	IDToken        string `json:"id_token,omitempty"`
	Expired        string `json:"expired,omitempty"`
	Proxy          string `json:"proxy,omitempty"`
}

type accountPoolBundleImportResult struct {
	Accounts       int                          `json:"accounts"`
	ChatGPT        accountPoolBundleStoreResult `json:"chatgpt_web"`
	Codex          accountPoolBundleStoreResult `json:"codex_cli"`
	Conflicts      []accountPoolBundleConflict  `json:"conflicts,omitempty"`
	Error          *accountPoolBundleError      `json:"error,omitempty"`
	PartialSuccess bool                         `json:"partial_success"`
}

type accountPoolBundleError struct {
	Message string `json:"message"`
}

type accountPoolBundleStoreResult struct {
	Added     int                         `json:"added"`
	Updated   int                         `json:"updated"`
	Skipped   int                         `json:"skipped"`
	Conflicts []accountPoolBundleConflict `json:"conflicts,omitempty"`
	Error     string                      `json:"error,omitempty"`
}

// accountPoolBundleConflict is deliberately metadata-only. In particular,
// it must never contain a credential value or a token-derived diagnostic.
type accountPoolBundleConflict struct {
	AccountRef string `json:"account_ref"`
	Slot       string `json:"slot"`
	Reason     string `json:"reason"`
}

func bundleEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

func bundleGroupKey(identity, email, kind, id string) string {
	if email = bundleEmail(email); email != "" {
		return "email:" + email
	}
	if identity = strings.TrimSpace(identity); identity != "" {
		return "identity:" + identity
	}
	return kind + ":" + strings.TrimSpace(id)
}

func (h *Handler) exportAccountPoolBundle(w http.ResponseWriter, r *http.Request) {
	if h.chatGPT == nil || h.codex == nil {
		writeError(w, http.StatusServiceUnavailable, "account pool is unavailable")
		return
	}
	web, err := h.chatGPT.ListChatGPTAccounts(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	codex, err := h.codex.ListCodexAccounts(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	groups := make(map[string]*accountPoolBundleEntry)
	order := make([]string, 0, len(web)+len(codex))
	get := func(key, email string) *accountPoolBundleEntry {
		if v := groups[key]; v != nil {
			return v
		}
		v := &accountPoolBundleEntry{AccountRef: newBundleAccountRef(), Identity: accountPoolBundleIdentity{Email: strings.TrimSpace(email)}}
		groups[key] = v
		order = append(order, key)
		return v
	}
	for _, view := range web {
		items, e := h.chatGPT.ExportChatGPTAccounts(r.Context(), []string{view.ID})
		if e != nil || len(items.Items) != 1 {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("chatgpt_web account %q is not exportable", view.ID))
			return
		}
		item := items.Items[0]
		effectiveEmail := firstNonEmpty(view.Email, item.Email)
		effectiveIdentity := firstNonEmpty(view.IdentityKey, accountidentity.Key(item.AccountID, effectiveEmail))
		key := bundleGroupKey(effectiveIdentity, effectiveEmail, "chatgpt_web", view.ID)
		row := get(key, effectiveEmail)
		if row.Slots.ChatGPT != nil {
			writeError(w, http.StatusBadRequest, "duplicate chatgpt_web slot for account")
			return
		}
		row.Slots.ChatGPT = &accountPoolBundleChatGPT{CredentialType: "chatgpt_web", AccountID: item.AccountID, IdentityKey: effectiveIdentity, Email: item.Email, AccessToken: item.AccessToken, RefreshToken: item.RefreshToken, IDToken: item.IDToken, Expired: item.Expired, Proxy: item.Proxy}
		if row.Identity.Email == "" {
			row.Identity.Email = effectiveEmail
		}
	}
	for _, view := range codex {
		items, e := h.codex.ExportCodexAccounts(r.Context(), []string{view.ID})
		if e != nil || len(items.Items) != 1 {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("codex_cli account %q is not exportable", view.ID))
			return
		}
		item := items.Items[0]
		effectiveEmail := firstNonEmpty(view.Email, item.Email)
		effectiveIdentity := firstNonEmpty(view.IdentityKey, accountidentity.Key(item.AccountID, effectiveEmail))
		key := bundleGroupKey(effectiveIdentity, effectiveEmail, "codex_cli", view.ID)
		row := get(key, effectiveEmail)
		if row.Slots.Codex != nil {
			writeError(w, http.StatusBadRequest, "duplicate codex_cli slot for account")
			return
		}
		row.Slots.Codex = &accountPoolBundleCodex{CredentialType: "codex_oauth", AccountID: item.AccountID, IdentityKey: effectiveIdentity, Email: item.Email, AccessToken: item.AccessToken, RefreshToken: item.RefreshToken, IDToken: item.IDToken, Expired: item.Expired, Proxy: item.Proxy}
		if row.Identity.Email == "" {
			row.Identity.Email = effectiveEmail
		}
	}
	accounts := make([]accountPoolBundleEntry, 0, len(order))
	for _, key := range order {
		accounts = append(accounts, *groups[key])
	}
	exportedAt := time.Now().UTC().Truncate(time.Second)
	payload := accountPoolBundle{Format: accountPoolBundleFormat, SchemaVersion: accountPoolBundleSchemaVersion, ExportedAt: exportedAt.Format(time.RFC3339), Accounts: accounts}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", bundleExportContentDisposition(bundleExportArtifactAccountPool, accountPoolBundleSchemaVersion, bundleExportProfileComplete, exportedAt))
	writeJSON(w, http.StatusOK, payload)
}

func newBundleAccountRef() string { return "acct_" + uuid.NewString() }

func bundleTextValid(value string, max int) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= max && !strings.ContainsFunc(value, unicode.IsControl)
}

// prepareAccountPoolBundleImport validates and normalizes the complete file
// before either account-pool owner is called. This is important because the
// two stores do not share a transaction: malformed data must not allow the
// first store to be modified before the second store is checked.
func prepareAccountPoolBundleImport(payload accountPoolBundle) (chat []accevents.ExportItem, codex []codexevents.CredentialInput, conflicts []accountPoolBundleConflict, err error) {
	if payload.Format != accountPoolBundleFormat || payload.SchemaVersion != accountPoolBundleSchemaVersion {
		return nil, nil, nil, fmt.Errorf("unsupported account pool bundle format or schema_version")
	}
	if len(payload.Accounts) == 0 || len(payload.Accounts) > maxAccountImportItems {
		return nil, nil, nil, fmt.Errorf("accounts must contain 1 to 1000 entries")
	}
	seenRefs := make(map[string]int, len(payload.Accounts))
	seenCredentials := map[string]map[string]string{
		"chatgpt_web": {},
		"codex_cli":   {},
	}
	seenAccess := map[string]map[string]string{
		"chatgpt_web": {},
		"codex_cli":   {},
	}
	seenAccountIDs := map[string]map[string]string{
		"chatgpt_web": {},
		"codex_cli":   {},
	}
	seenEmails := make(map[string]string, len(payload.Accounts))

	addConflict := func(accountRef, slot, reason string) {
		conflicts = append(conflicts, accountPoolBundleConflict{AccountRef: accountRef, Slot: slot, Reason: reason})
	}
	rememberAccountID := func(accountRef, kind, accountID string) {
		if accountID = strings.TrimSpace(accountID); accountID == "" {
			return
		}
		if previous, exists := seenAccountIDs[kind][accountID]; exists && previous != accountRef {
			addConflict(accountRef, kind, "same upstream account_id is assigned to another account_ref")
			return
		}
		seenAccountIDs[kind][accountID] = accountRef
	}
	validateSlotText := func(value string, max int, field string) error {
		if value == "" {
			return nil
		}
		if len(value) > max || strings.ContainsFunc(value, unicode.IsControl) {
			return fmt.Errorf("%s is invalid", field)
		}
		return nil
	}
	for i := range payload.Accounts {
		account := &payload.Accounts[i]
		account.AccountRef = strings.TrimSpace(account.AccountRef)
		if !bundleTextValid(account.AccountRef, 128) {
			return nil, nil, nil, fmt.Errorf("accounts[%d].account_ref is invalid", i)
		}
		if previous, exists := seenRefs[account.AccountRef]; exists {
			addConflict(account.AccountRef, "account", fmt.Sprintf("duplicate account_ref (also used by accounts[%d])", previous))
		} else {
			seenRefs[account.AccountRef] = i
		}
		account.Identity.Email = strings.TrimSpace(account.Identity.Email)
		if err := validateSlotText(account.Identity.Email, 320, fmt.Sprintf("accounts[%d].identity.email", i)); err != nil {
			return nil, nil, nil, err
		}
		if account.Slots.ChatGPT == nil && account.Slots.Codex == nil {
			return nil, nil, nil, fmt.Errorf("accounts[%d] must contain at least one slot", i)
		}
		if account.Slots.ChatGPT != nil && account.Slots.Codex != nil {
			webEmail := bundleEmail(firstNonEmpty(account.Slots.ChatGPT.Email, account.Identity.Email))
			codexEmail := bundleEmail(firstNonEmpty(account.Slots.Codex.Email, account.Identity.Email))
			if webEmail == "" || codexEmail == "" || webEmail != codexEmail {
				addConflict(account.AccountRef, "account", "dual-slot account requires the same non-empty email")
			}
		}

		if slot := account.Slots.ChatGPT; slot != nil {
			const kind = "chatgpt_web"
			if value := strings.ToLower(strings.TrimSpace(slot.CredentialType)); value != "" && value != kind {
				return nil, nil, nil, fmt.Errorf("accounts[%d].slots.chatgpt_web has invalid credential_type", i)
			}
			slot.CredentialType = kind
			slot.AccountID = strings.TrimSpace(slot.AccountID)
			slot.IdentityKey = strings.TrimSpace(slot.IdentityKey)
			slot.Email = strings.TrimSpace(slot.Email)
			slot.AccessToken = strings.TrimSpace(slot.AccessToken)
			slot.RefreshToken = strings.TrimSpace(slot.RefreshToken)
			slot.IDToken = strings.TrimSpace(slot.IDToken)
			slot.Expired = strings.TrimSpace(slot.Expired)
			slot.Proxy = strings.TrimSpace(slot.Proxy)
			if err := validateSlotText(slot.AccountID, 512, fmt.Sprintf("accounts[%d].slots.chatgpt_web.account_id", i)); err != nil {
				return nil, nil, nil, err
			}
			rememberAccountID(account.AccountRef, kind, slot.AccountID)
			if err := validateSlotText(slot.IdentityKey, 256, fmt.Sprintf("accounts[%d].slots.chatgpt_web.identity_key", i)); err != nil {
				return nil, nil, nil, err
			}
			if err := validateSlotText(slot.Email, 320, fmt.Sprintf("accounts[%d].slots.chatgpt_web.email", i)); err != nil {
				return nil, nil, nil, err
			}
			if slot.AccessToken == "" || slot.RefreshToken == "" {
				return nil, nil, nil, fmt.Errorf("accounts[%d].slots.chatgpt_web requires access_token and refresh_token", i)
			}
			if err := validateSlotText(slot.AccessToken, 1<<20, "chatgpt_web credential"); err != nil {
				return nil, nil, nil, err
			}
			if err := validateSlotText(slot.RefreshToken, 1<<20, "chatgpt_web credential"); err != nil {
				return nil, nil, nil, err
			}
			if err := validateSlotText(slot.IDToken, 1<<20, "chatgpt_web credential"); err != nil {
				return nil, nil, nil, err
			}
			if err := validateSlotText(slot.Expired, 128, fmt.Sprintf("accounts[%d].slots.chatgpt_web.expired", i)); err != nil {
				return nil, nil, nil, err
			}
			if err := validateSlotText(slot.Proxy, 2048, fmt.Sprintf("accounts[%d].slots.chatgpt_web.proxy", i)); err != nil {
				return nil, nil, nil, err
			}
			if err := validateBundleProxy(slot.Proxy, fmt.Sprintf("accounts[%d].slots.chatgpt_web.proxy", i)); err != nil {
				return nil, nil, nil, err
			}
			if previous, exists := seenCredentials[kind][slot.RefreshToken]; exists {
				addConflict(account.AccountRef, kind, fmt.Sprintf("duplicate refresh credential (also used by %s)", previous))
			} else {
				seenCredentials[kind][slot.RefreshToken] = account.AccountRef
			}
			if previous, exists := seenAccess[kind][slot.AccessToken]; exists {
				addConflict(account.AccountRef, kind, fmt.Sprintf("duplicate access credential (also used by %s)", previous))
			} else {
				seenAccess[kind][slot.AccessToken] = account.AccountRef
			}
			if email := bundleEmail(firstNonEmpty(slot.Email, account.Identity.Email)); email != "" {
				if previous, exists := seenEmails[email]; exists && previous != account.AccountRef {
					addConflict(account.AccountRef, kind, "same email is assigned to another account_ref")
				} else {
					seenEmails[email] = account.AccountRef
				}
			}
			chat = append(chat, accevents.ExportItem{CredentialType: kind, AccountID: slot.AccountID, Email: firstNonEmpty(slot.Email, account.Identity.Email), AccessToken: slot.AccessToken, RefreshToken: slot.RefreshToken, IDToken: slot.IDToken, Expired: slot.Expired, Proxy: slot.Proxy})
		}

		if slot := account.Slots.Codex; slot != nil {
			const kind = "codex_cli"
			credentialType := strings.ToLower(strings.TrimSpace(slot.CredentialType))
			if credentialType != "" && credentialType != kind && credentialType != "codex_oauth" {
				return nil, nil, nil, fmt.Errorf("accounts[%d].slots.codex_cli has invalid credential_type", i)
			}
			slot.CredentialType = "codex_oauth"
			slot.AccountID = strings.TrimSpace(slot.AccountID)
			slot.IdentityKey = strings.TrimSpace(slot.IdentityKey)
			slot.Email = strings.TrimSpace(slot.Email)
			slot.AccessToken = strings.TrimSpace(slot.AccessToken)
			slot.RefreshToken = strings.TrimSpace(slot.RefreshToken)
			slot.IDToken = strings.TrimSpace(slot.IDToken)
			slot.Expired = strings.TrimSpace(slot.Expired)
			slot.Proxy = strings.TrimSpace(slot.Proxy)
			if err := validateSlotText(slot.AccountID, 512, fmt.Sprintf("accounts[%d].slots.codex_cli.account_id", i)); err != nil {
				return nil, nil, nil, err
			}
			rememberAccountID(account.AccountRef, kind, slot.AccountID)
			if err := validateSlotText(slot.IdentityKey, 256, fmt.Sprintf("accounts[%d].slots.codex_cli.identity_key", i)); err != nil {
				return nil, nil, nil, err
			}
			if err := validateSlotText(slot.Email, 320, fmt.Sprintf("accounts[%d].slots.codex_cli.email", i)); err != nil {
				return nil, nil, nil, err
			}
			if slot.AccessToken == "" || slot.RefreshToken == "" {
				return nil, nil, nil, fmt.Errorf("accounts[%d].slots.codex_cli requires access_token and refresh_token", i)
			}
			if err := validateSlotText(slot.AccessToken, 1<<20, "codex_cli credential"); err != nil {
				return nil, nil, nil, err
			}
			if err := validateSlotText(slot.RefreshToken, 1<<20, "codex_cli credential"); err != nil {
				return nil, nil, nil, err
			}
			if err := validateSlotText(slot.IDToken, 1<<20, "codex_cli credential"); err != nil {
				return nil, nil, nil, err
			}
			if err := validateSlotText(slot.Expired, 128, fmt.Sprintf("accounts[%d].slots.codex_cli.expired", i)); err != nil {
				return nil, nil, nil, err
			}
			if err := validateSlotText(slot.Proxy, 2048, fmt.Sprintf("accounts[%d].slots.codex_cli.proxy", i)); err != nil {
				return nil, nil, nil, err
			}
			if err := validateBundleProxy(slot.Proxy, fmt.Sprintf("accounts[%d].slots.codex_cli.proxy", i)); err != nil {
				return nil, nil, nil, err
			}
			if previous, exists := seenCredentials[kind][slot.RefreshToken]; exists {
				addConflict(account.AccountRef, kind, fmt.Sprintf("duplicate refresh credential (also used by %s)", previous))
			} else {
				seenCredentials[kind][slot.RefreshToken] = account.AccountRef
			}
			if previous, exists := seenAccess[kind][slot.AccessToken]; exists {
				addConflict(account.AccountRef, kind, fmt.Sprintf("duplicate access credential (also used by %s)", previous))
			} else {
				seenAccess[kind][slot.AccessToken] = account.AccountRef
			}
			if email := bundleEmail(firstNonEmpty(slot.Email, account.Identity.Email)); email != "" {
				if previous, exists := seenEmails[email]; exists && previous != account.AccountRef {
					addConflict(account.AccountRef, kind, "same email is assigned to another account_ref")
				} else {
					seenEmails[email] = account.AccountRef
				}
			}
			codex = append(codex, codexevents.CredentialInput{CredentialType: kind, AccountID: slot.AccountID, Email: firstNonEmpty(slot.Email, account.Identity.Email), AccessToken: slot.AccessToken, RefreshToken: slot.RefreshToken, IDToken: slot.IDToken, Expired: slot.Expired, Proxy: slot.Proxy})
		}
	}
	if len(chat) > maxAccountImportItems || len(codex) > maxAccountImportItems {
		return nil, nil, nil, fmt.Errorf("each credential slot type may contain at most 1000 accounts")
	}
	return chat, codex, conflicts, nil
}

func validateBundleProxy(value, field string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("%s is invalid", field)
	}
	return nil
}

type bundleTargetView struct {
	ID          string
	IdentityKey string
	Email       string
}

func resolveBundleTarget(accountID, email string, views []bundleTargetView, replace bool) (targetID, conflictReason string) {
	accountID = strings.TrimSpace(accountID)
	email = bundleEmail(email)
	// Only an explicitly supplied upstream account_id can establish an exact
	// identity comparison. When the bundle carries email only, the email
	// fallback below is authoritative; deriving a hash from the email here
	// would incorrectly look different from a target whose identity key was
	// derived from its upstream account_id.
	hasAccountID := accountID != ""
	incomingIdentity := ""
	if hasAccountID {
		incomingIdentity = accountidentity.Key(accountID, email)
	}
	if incomingIdentity != "" {
		exact := make([]bundleTargetView, 0, 1)
		for _, view := range views {
			if strings.TrimSpace(view.IdentityKey) == incomingIdentity {
				exact = append(exact, view)
			}
		}
		if len(exact) == 1 {
			return exact[0].ID, ""
		}
		if len(exact) > 1 {
			return "", "multiple existing slots match account_id"
		}
	}
	if email == "" {
		return "", ""
	}
	emailMatches := make([]bundleTargetView, 0, 1)
	for _, view := range views {
		if bundleEmail(view.Email) == email {
			emailMatches = append(emailMatches, view)
		}
	}
	if len(emailMatches) == 0 {
		return "", ""
	}
	if len(emailMatches) > 1 {
		return "", "multiple existing slots match email"
	}
	candidate := emailMatches[0]
	if hasAccountID && incomingIdentity != "" && strings.TrimSpace(candidate.IdentityKey) != incomingIdentity {
		if replace {
			return candidate.ID, ""
		}
		return "", "email matches an existing slot with a different account_id"
	}
	return candidate.ID, ""
}

func bundleTargetViews(items []accevents.AccountView) []bundleTargetView {
	views := make([]bundleTargetView, 0, len(items))
	for _, item := range items {
		views = append(views, bundleTargetView{ID: item.ID, IdentityKey: item.IdentityKey, Email: item.Email})
	}
	return views
}

func codexBundleTargetViews(items []codexevents.AccountView) []bundleTargetView {
	views := make([]bundleTargetView, 0, len(items))
	for _, item := range items {
		views = append(views, bundleTargetView{ID: item.ID, IdentityKey: item.IdentityKey, Email: item.Email})
	}
	return views
}

func resolveAccountPoolBundleTargets(payload accountPoolBundle, chat []accevents.ExportItem, codex []codexevents.CredentialInput, webViews []accevents.AccountView, codexViews []codexevents.AccountView) (conflicts []accountPoolBundleConflict) {
	chatRefs := make(map[string]string, len(chat))
	codexRefs := make(map[string]string, len(codex))
	for _, account := range payload.Accounts {
		if account.Slots.ChatGPT != nil {
			chatRefs[strings.TrimSpace(account.Slots.ChatGPT.AccessToken)] = account.AccountRef
		}
		if account.Slots.Codex != nil {
			codexRefs[strings.TrimSpace(account.Slots.Codex.AccessToken)] = account.AccountRef
		}
	}
	webTargets := bundleTargetViews(webViews)
	webTargetRefs := make(map[string]string, len(chat))
	for i := range chat {
		ref := chatRefs[chat[i].AccessToken]
		target, reason := resolveBundleTarget(chat[i].AccountID, chat[i].Email, webTargets, payload.Replace)
		if reason != "" {
			conflicts = append(conflicts, accountPoolBundleConflict{AccountRef: ref, Slot: "chatgpt_web", Reason: reason})
			continue
		}
		if target != "" {
			if previous, exists := webTargetRefs[target]; exists && previous != ref {
				conflicts = append(conflicts, accountPoolBundleConflict{AccountRef: ref, Slot: "chatgpt_web", Reason: "multiple bundle accounts target the same existing slot"})
				continue
			}
			webTargetRefs[target] = ref
		}
		chat[i].TargetID = target
	}
	codexTargets := codexBundleTargetViews(codexViews)
	codexTargetRefs := make(map[string]string, len(codex))
	for i := range codex {
		ref := codexRefs[codex[i].AccessToken]
		target, reason := resolveBundleTarget(codex[i].AccountID, codex[i].Email, codexTargets, payload.Replace)
		if reason != "" {
			conflicts = append(conflicts, accountPoolBundleConflict{AccountRef: ref, Slot: "codex_cli", Reason: reason})
			continue
		}
		if target != "" {
			if previous, exists := codexTargetRefs[target]; exists && previous != ref {
				conflicts = append(conflicts, accountPoolBundleConflict{AccountRef: ref, Slot: "codex_cli", Reason: "multiple bundle accounts target the same existing slot"})
				continue
			}
			codexTargetRefs[target] = ref
		}
		codex[i].TargetID = target
	}
	return conflicts
}

func (h *Handler) importAccountPoolBundle(w http.ResponseWriter, r *http.Request) {
	if h.chatGPT == nil && h.codex == nil {
		writeError(w, http.StatusServiceUnavailable, "account pool is unavailable")
		return
	}
	var payload accountPoolBundle
	if !decodeAdminBody(w, r, &payload) {
		return
	}
	chat, codex, conflicts, err := prepareAccountPoolBundleImport(payload)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(conflicts) > 0 {
		writeAccountPoolBundleConflicts(w, len(payload.Accounts), conflicts)
		return
	}
	if len(chat) > 0 && h.chatGPT == nil {
		writeError(w, http.StatusServiceUnavailable, "ChatGPT Web account pool is unavailable")
		return
	}
	if len(codex) > 0 && h.codex == nil {
		writeError(w, http.StatusServiceUnavailable, "Codex account pool is unavailable")
		return
	}
	var webViews []accevents.AccountView
	var codexViews []codexevents.AccountView
	if len(chat) > 0 {
		webViews, err = h.chatGPT.ListChatGPTAccounts(r.Context())
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
	}
	if len(codex) > 0 {
		codexViews, err = h.codex.ListCodexAccounts(r.Context())
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
	}
	if targetConflicts := resolveAccountPoolBundleTargets(payload, chat, codex, webViews, codexViews); len(targetConflicts) > 0 {
		writeAccountPoolBundleConflicts(w, len(payload.Accounts), targetConflicts)
		return
	}
	result := accountPoolBundleImportResult{Accounts: len(payload.Accounts)}
	anySuccess, anyFailure := false, false
	allSecrets := append(bundleChatGPTSecrets(chat), bundleCodexSecrets(codex)...)
	if len(chat) > 0 {
		v, err := h.chatGPT.AddChatGPTAccounts(r.Context(), nil, chat, "oauth_import")
		if err != nil {
			result.ChatGPT.Error = safeAccountPoolBundleStoreError(err, "ChatGPT Web account import failed", allSecrets)
			anyFailure = true
		} else {
			result.ChatGPT.Added, result.ChatGPT.Updated, result.ChatGPT.Skipped = v.Added, v.Updated, v.Skipped
			anySuccess = true
		}
	}
	if len(codex) > 0 {
		v, err := h.codex.ImportCodexAccounts(r.Context(), codex)
		if err != nil {
			result.Codex.Error = safeAccountPoolBundleStoreError(err, "Codex account import failed", allSecrets)
			anyFailure = true
		} else {
			result.Codex.Added, result.Codex.Updated, result.Codex.Skipped = v.Added, v.Updated, v.Skipped
			anySuccess = true
		}
	}
	result.PartialSuccess = anySuccess && anyFailure
	if anyFailure && !anySuccess {
		writeJSON(w, http.StatusBadGateway, result)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

// safeAccountPoolBundleStoreError keeps the per-store outcome actionable
// without allowing a lower layer to reflect an imported credential. Owner
// implementations normally return generic errors, but the boundary must stay
// safe even when a future adapter includes part of its input in an error.
func safeAccountPoolBundleStoreError(err error, fallback string, secrets []string) string {
	if err == nil {
		return ""
	}
	message := strings.TrimSpace(err.Error())
	if message == "" {
		return fallback
	}
	for _, secret := range secrets {
		if secret = strings.TrimSpace(secret); secret != "" && strings.Contains(message, secret) {
			return fallback
		}
	}
	return message
}

func bundleChatGPTSecrets(items []accevents.ExportItem) []string {
	values := make([]string, 0, len(items)*4)
	for _, item := range items {
		values = append(values, item.AccessToken, item.RefreshToken, item.IDToken, item.Proxy)
	}
	return values
}

func bundleCodexSecrets(items []codexevents.CredentialInput) []string {
	values := make([]string, 0, len(items)*4)
	for _, item := range items {
		values = append(values, item.AccessToken, item.RefreshToken, item.IDToken, item.Proxy)
	}
	return values
}

func writeAccountPoolBundleConflicts(w http.ResponseWriter, accounts int, conflicts []accountPoolBundleConflict) {
	result := accountPoolBundleImportResult{Accounts: accounts, Conflicts: conflicts, Error: &accountPoolBundleError{Message: "account pool bundle contains conflicts"}}
	for _, conflict := range conflicts {
		switch conflict.Slot {
		case "chatgpt_web":
			result.ChatGPT.Conflicts = append(result.ChatGPT.Conflicts, conflict)
		case "codex_cli":
			result.Codex.Conflicts = append(result.Codex.Conflicts, conflict)
		case "account":
			// File-level conflicts are already present in the top-level list.
		}
	}
	writeJSON(w, http.StatusConflict, result)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
