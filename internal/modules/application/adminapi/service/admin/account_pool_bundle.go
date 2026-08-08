package admin

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	accevents "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/events"
	codexevents "ai-proxy/internal/modules/blocks/codexaccountpool/pkg/events"
)

const accountPoolBundleFormat = "ai-proxy.account-pool-bundle"

type accountPoolBundle struct {
	Format        string                   `json:"format"`
	SchemaVersion int                      `json:"schema_version"`
	ExportedAt    string                   `json:"exported_at"`
	Accounts      []accountPoolBundleEntry `json:"accounts"`
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
	PartialSuccess bool                         `json:"partial_success"`
}

type accountPoolBundleStoreResult struct {
	Added   int    `json:"added"`
	Updated int    `json:"updated"`
	Skipped int    `json:"skipped"`
	Error   string `json:"error,omitempty"`
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
		key := bundleGroupKey(view.IdentityKey, view.Email, "chatgpt_web", view.ID)
		row := get(key, view.Email)
		if row.Slots.ChatGPT != nil {
			writeError(w, http.StatusBadRequest, "duplicate chatgpt_web slot for account")
			return
		}
		items, e := h.chatGPT.ExportChatGPTAccounts(r.Context(), []string{view.ID})
		if e != nil || len(items.Items) != 1 {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("chatgpt_web account %q is not exportable", view.ID))
			return
		}
		item := items.Items[0]
		row.Slots.ChatGPT = &accountPoolBundleChatGPT{CredentialType: "chatgpt_web", AccountID: item.AccountID, IdentityKey: view.IdentityKey, Email: item.Email, AccessToken: item.AccessToken, RefreshToken: item.RefreshToken, IDToken: item.IDToken, Expired: item.Expired, Proxy: item.Proxy}
		if row.Identity.Email == "" {
			row.Identity.Email = item.Email
		}
	}
	for _, view := range codex {
		key := bundleGroupKey(view.IdentityKey, view.Email, "codex_cli", view.ID)
		row := get(key, view.Email)
		if row.Slots.Codex != nil {
			writeError(w, http.StatusBadRequest, "duplicate codex_cli slot for account")
			return
		}
		items, e := h.codex.ExportCodexAccounts(r.Context(), []string{view.ID})
		if e != nil || len(items.Items) != 1 {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("codex_cli account %q is not exportable", view.ID))
			return
		}
		item := items.Items[0]
		row.Slots.Codex = &accountPoolBundleCodex{CredentialType: "codex_oauth", AccountID: item.AccountID, IdentityKey: view.IdentityKey, Email: item.Email, AccessToken: item.AccessToken, RefreshToken: item.RefreshToken, IDToken: item.IDToken, Expired: item.Expired, Proxy: item.Proxy}
		if row.Identity.Email == "" {
			row.Identity.Email = item.Email
		}
	}
	accounts := make([]accountPoolBundleEntry, 0, len(order))
	for _, key := range order {
		accounts = append(accounts, *groups[key])
	}
	payload := accountPoolBundle{Format: accountPoolBundleFormat, SchemaVersion: 2, ExportedAt: time.Now().UTC().Format(time.RFC3339), Accounts: accounts}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", `attachment; filename="account-pool-bundle.json"`)
	writeJSON(w, http.StatusOK, payload)
}

func newBundleAccountRef() string { return fmt.Sprintf("acct_%d", time.Now().UTC().UnixNano()) }

func (h *Handler) importAccountPoolBundle(w http.ResponseWriter, r *http.Request) {
	if h.chatGPT == nil || h.codex == nil {
		writeError(w, http.StatusServiceUnavailable, "account pool is unavailable")
		return
	}
	var payload accountPoolBundle
	if !decodeAdminBody(w, r, &payload) {
		return
	}
	if payload.Format != accountPoolBundleFormat || payload.SchemaVersion != 2 {
		writeError(w, http.StatusBadRequest, "unsupported account pool bundle format or schema_version")
		return
	}
	if len(payload.Accounts) == 0 || len(payload.Accounts) > maxAccountImportItems {
		writeError(w, http.StatusBadRequest, "accounts must contain 1 to 1000 entries")
		return
	}
	chat := make([]accevents.ExportItem, 0)
	codex := make([]codexevents.CredentialInput, 0)
	for i, account := range payload.Accounts {
		if strings.TrimSpace(account.AccountRef) == "" || (account.Slots.ChatGPT == nil && account.Slots.Codex == nil) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("accounts[%d] must contain account_ref and at least one slot", i))
			return
		}
		if s := account.Slots.ChatGPT; s != nil {
			if s.AccessToken == "" || s.RefreshToken == "" {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("accounts[%d].slots.chatgpt_web requires access_token and refresh_token", i))
				return
			}
			chat = append(chat, accevents.ExportItem{CredentialType: "chatgpt_web", AccountID: s.AccountID, Email: firstNonEmpty(s.Email, account.Identity.Email), AccessToken: s.AccessToken, RefreshToken: s.RefreshToken, IDToken: s.IDToken, Expired: s.Expired, Proxy: s.Proxy})
		}
		if s := account.Slots.Codex; s != nil {
			if s.AccessToken == "" || s.RefreshToken == "" {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("accounts[%d].slots.codex_cli requires access_token and refresh_token", i))
				return
			}
			codex = append(codex, codexevents.CredentialInput{CredentialType: "codex_cli", AccountID: s.AccountID, Email: firstNonEmpty(s.Email, account.Identity.Email), AccessToken: s.AccessToken, RefreshToken: s.RefreshToken, IDToken: s.IDToken, Expired: s.Expired, Proxy: s.Proxy})
		}
	}
	result := accountPoolBundleImportResult{Accounts: len(payload.Accounts)}
	anySuccess, anyFailure := false, false
	if len(chat) > 0 {
		v, err := h.chatGPT.AddChatGPTAccounts(r.Context(), nil, chat, "oauth_import")
		if err != nil {
			result.ChatGPT.Error = err.Error()
			anyFailure = true
		} else {
			result.ChatGPT.Added, result.ChatGPT.Updated, result.ChatGPT.Skipped = v.Added, v.Updated, v.Skipped
			anySuccess = true
		}
	}
	if len(codex) > 0 {
		v, err := h.codex.ImportCodexAccounts(r.Context(), codex)
		if err != nil {
			result.Codex.Error = err.Error()
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
