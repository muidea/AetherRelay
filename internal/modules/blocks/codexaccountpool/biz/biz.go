// Package biz implements Codex OAuth account-pool use cases.
package biz

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	basebiz "aetherrelay/internal/modules/base/biz"
	"aetherrelay/internal/modules/blocks/codexaccountpool/internal/oauth"
	"aetherrelay/internal/modules/blocks/codexaccountpool/internal/store"
	"aetherrelay/internal/modules/blocks/codexaccountpool/pkg/common"
	events "aetherrelay/internal/modules/blocks/codexaccountpool/pkg/events"
	configevents "aetherrelay/internal/modules/blocks/configruntime/pkg/events"
	"aetherrelay/internal/pkg/aetherrelaycredential"
	"github.com/google/uuid"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

const (
	oauthSessionTTL           = 10 * time.Minute
	oauthSessionMax           = 64
	refreshLead               = 5 * time.Minute
	defaultSessionAffinityTTL = time.Hour
	defaultAccountConcurrency = 4
)

type sessionBinding struct {
	accountID string
	expiresAt time.Time
}

type oauthSession struct {
	verifier string
	state    string
	proxy    string
	created  time.Time
}

type refreshFlight struct {
	done   chan struct{}
	result events.RefreshTokenResult
	err    error
}

type Account struct {
	basebiz.Base
	store         *store.Store
	topics        []string
	stopping      atomic.Bool
	shutdownCtx   context.Context
	shutdown      context.CancelFunc
	refreshEvery  time.Duration
	oauthTimeout  time.Duration
	refreshMu     sync.Mutex
	refreshes     map[string]*refreshFlight
	oauthMu       sync.Mutex
	oauthSessions map[string]oauthSession
	scheduleMu    sync.Mutex
	sessions      map[string]sessionBinding
	leases        map[string]string
	inflight      map[string]int
	now           func() time.Time
}

func New(ctx context.Context, hub event.Hub, background task.BackgroundRoutine) (*Account, *cd.Error) {
	bootstrap, err := configevents.RequestBootstrap(ctx, hub, common.UnitID)
	if err != nil {
		return nil, cd.NewError(cd.IllegalParam, err.Error())
	}
	if err := os.MkdirAll(bootstrap.Config.State.Dir, 0o700); err != nil {
		return nil, cd.NewError(cd.Unexpected, "create Codex OAuth state directory: "+err.Error())
	}
	credentialCodec, err := aetherrelaycredential.FromEnvironment()
	if err != nil {
		return nil, cd.NewError(cd.IllegalParam, err.Error())
	}
	st, err := store.Open(bootstrap.Config.State.Database, bootstrap.Config.State.MemoryLimit, bootstrap.Config.State.Threads, credentialCodec)
	if err != nil {
		return nil, cd.NewError(cd.Unexpected, "open Codex OAuth account state: "+err.Error())
	}
	return newAccount(hub, background, st, time.Duration(bootstrap.Config.CodexOAuth.RefreshAccountIntervalMinute)*time.Minute, bootstrap.Config.RequestTimeout), nil
}

func newAccount(hub event.Hub, background task.BackgroundRoutine, st *store.Store, refreshEvery, oauthTimeout time.Duration) *Account {
	shutdownCtx, shutdown := context.WithCancel(context.Background())
	b := &Account{Base: basebiz.New(common.UnitID, hub, background), store: st, shutdownCtx: shutdownCtx, shutdown: shutdown, refreshEvery: refreshEvery, oauthTimeout: oauthTimeout, refreshes: map[string]*refreshFlight{}, oauthSessions: map[string]oauthSession{}, sessions: map[string]sessionBinding{}, leases: map[string]string{}, inflight: map[string]int{}, now: time.Now}
	if st == nil {
		return b
	}
	b.topics = []string{events.TopicList, events.TopicImport, events.TopicDelete, events.TopicUpdate, events.TopicAcquire, events.TopicRelease, events.TopicRecordResult, events.TopicRecordTransportCapability, events.TopicRefreshToken, events.TopicRefreshByID, events.TopicExportByID, events.TopicHealth, events.TopicOAuthStart, events.TopicOAuthFinish, events.TopicListDiscoveryCandidates, events.TopicPutModelSnapshot, events.TopicRecordModelDiscoveryFailure, events.TopicCatalogSnapshot, events.TopicListUsageCandidates, events.TopicPutUsageSnapshot, events.TopicMergeUsageSnapshot, events.TopicRecordUsageFailure}
	b.SubscribeFunc(events.TopicList, b.handleList)
	b.SubscribeFunc(events.TopicImport, b.handleImport)
	b.SubscribeFunc(events.TopicDelete, b.handleDelete)
	b.SubscribeFunc(events.TopicUpdate, b.handleUpdate)
	b.SubscribeFunc(events.TopicAcquire, b.handleAcquire)
	b.SubscribeFunc(events.TopicRelease, b.handleRelease)
	b.SubscribeFunc(events.TopicRecordResult, b.handleRecordResult)
	b.SubscribeFunc(events.TopicRecordTransportCapability, b.handleRecordTransportCapability)
	b.SubscribeFunc(events.TopicRefreshToken, b.handleRefreshToken)
	b.SubscribeFunc(events.TopicRefreshByID, b.handleRefreshByID)
	b.SubscribeFunc(events.TopicExportByID, b.handleExportByID)
	b.SubscribeFunc(events.TopicHealth, b.handleHealth)
	b.SubscribeFunc(events.TopicOAuthStart, b.handleOAuthStart)
	b.SubscribeFunc(events.TopicOAuthFinish, b.handleOAuthFinish)
	b.SubscribeFunc(events.TopicListDiscoveryCandidates, b.handleListDiscoveryCandidates)
	b.SubscribeFunc(events.TopicPutModelSnapshot, b.handlePutModelSnapshot)
	b.SubscribeFunc(events.TopicRecordModelDiscoveryFailure, b.handleRecordModelDiscoveryFailure)
	b.SubscribeFunc(events.TopicCatalogSnapshot, b.handleCatalogSnapshot)
	b.SubscribeFunc(events.TopicListUsageCandidates, b.handleListUsageCandidates)
	b.SubscribeFunc(events.TopicPutUsageSnapshot, b.handlePutUsageSnapshot)
	b.SubscribeFunc(events.TopicMergeUsageSnapshot, b.handleMergeUsageSnapshot)
	b.SubscribeFunc(events.TopicRecordUsageFailure, b.handleRecordUsageFailure)
	return b
}

func (s *Account) Run(ctx context.Context) *cd.Error {
	if s.store != nil && s.refreshEvery > 0 {
		s.Timer(ctx, s.refreshEvery, 0, s.refreshExpiring)
	}
	return nil
}

func (s *Account) Teardown(context.Context) {
	s.stopping.Store(true)
	s.shutdown()
	for _, topic := range s.topics {
		s.UnsubscribeFunc(topic)
	}
	s.scheduleMu.Lock()
	s.sessions = map[string]sessionBinding{}
	s.leases = map[string]string{}
	s.inflight = map[string]int{}
	s.scheduleMu.Unlock()
	if s.store != nil {
		_ = s.store.Close()
	}
}

func (s *Account) handleList(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	if _, ok := ev.Data().(events.ListCommand); !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid Codex account list command"))
		return
	}
	result.Set(events.ListResult{Items: s.store.List()}, nil)
}

func (s *Account) handleImport(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.ImportCommand)
	if !ok || len(cmd.Accounts) == 0 {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid Codex account import command"))
		return
	}
	added, updated, skipped, accountIDs, err := s.store.ImportWithIDs(cmd.Accounts)
	if err != nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, err.Error()))
		return
	}
	result.Set(events.ImportResult{Added: added, Updated: updated, Skipped: skipped, AccountIDs: accountIDs}, nil)
}

func (s *Account) handleDelete(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.DeleteCommand)
	if !ok || len(cmd.IDs) == 0 {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid Codex account delete command"))
		return
	}
	deleted, err := s.store.Delete(cmd.IDs)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(events.DeleteResult{Deleted: deleted}, nil)
}

func (s *Account) handleUpdate(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.UpdateCommand)
	if !ok || strings.TrimSpace(cmd.ID) == "" || (cmd.Status == nil && cmd.Proxy == nil && cmd.FingerprintMode == nil) {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid Codex account update command"))
		return
	}
	item, err := s.store.Update(cmd.ID, cmd.Status, cmd.Proxy, cmd.FingerprintMode)
	if err != nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, err.Error()))
		return
	}
	result.Set(events.UpdateResult{Item: item}, nil)
}

func (s *Account) handleAcquire(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.AcquireCommand)
	if !ok || strings.TrimSpace(cmd.Model) == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid Codex account acquire command"))
		return
	}
	if s.stopping.Load() {
		result.Set(nil, cd.NewError(cd.Unexpected, "Codex account pool is stopping"))
		return
	}
	exclude := append([]string(nil), cmd.Exclude...)
	s.scheduleMu.Lock()
	defer s.scheduleMu.Unlock()
	now := s.now().UTC()
	preferred := strings.TrimSpace(cmd.PreferredID)
	if binding, ok := s.sessions[strings.TrimSpace(cmd.SessionHash)]; ok {
		if binding.expiresAt.After(now) {
			preferred = binding.accountID
		} else {
			delete(s.sessions, strings.TrimSpace(cmd.SessionHash))
		}
	}
	for accountID, count := range s.inflight {
		if count >= defaultAccountConcurrency {
			exclude = append(exclude, accountID)
		}
	}
	item, err := s.store.AcquirePreferredTransport(cmd.Model, exclude, preferred, cmd.Transport)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, "Codex account unavailable"))
		return
	}
	leaseID := uuid.NewString()
	s.leases[leaseID] = item.AccountID
	s.inflight[item.AccountID]++
	if sessionHash := strings.TrimSpace(cmd.SessionHash); sessionHash != "" {
		s.sessions[sessionHash] = sessionBinding{accountID: item.AccountID, expiresAt: now.Add(defaultSessionAffinityTTL)}
	}
	item.LeaseID = leaseID
	result.Set(item, nil)
}

func (s *Account) handleRelease(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.ReleaseCommand)
	if !ok || strings.TrimSpace(cmd.LeaseID) == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid Codex account release command"))
		return
	}
	s.scheduleMu.Lock()
	accountID, found := s.leases[cmd.LeaseID]
	if found {
		delete(s.leases, cmd.LeaseID)
		if s.inflight[accountID] <= 1 {
			delete(s.inflight, accountID)
		} else {
			s.inflight[accountID]--
		}
	}
	s.scheduleMu.Unlock()
	result.Set(events.ReleaseResult{Released: found}, nil)
}

func (s *Account) handleRecordResult(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.RecordResultCommand)
	if !ok || strings.TrimSpace(cmd.AccountID) == "" || strings.TrimSpace(cmd.Model) == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid Codex account result command"))
		return
	}
	item, err := s.store.RecordResult(cmd.AccountID, cmd.Model, cmd.Success, cmd.ErrorClass, cmd.RetryAfterSeconds, cmd.QuotaExhausted, cmd.QuotaResetAt)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(events.RecordResultResult{Account: item}, nil)
}

func (s *Account) handleRecordTransportCapability(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.RecordTransportCapabilityCommand)
	if !ok || strings.TrimSpace(cmd.AccountID) == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid Codex transport capability command"))
		return
	}
	item, err := s.store.RecordTransportCapability(cmd.AccountID, cmd.Transport, cmd.Supported)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(events.RecordTransportCapabilityResult{Account: item}, nil)
}

func (s *Account) handleRefreshToken(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.RefreshTokenCommand)
	if !ok || strings.TrimSpace(cmd.AccountID) == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid Codex token refresh command"))
		return
	}
	refreshed, err := s.refreshToken(ev.Context(), cmd.AccountID)
	if err != nil {
		result.Set(refreshed, cd.NewError(cd.Unexpected, "Codex token refresh failed"))
		return
	}
	result.Set(refreshed, nil)
}

func (s *Account) handleRefreshByID(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.RefreshByIDCommand)
	if !ok || len(cmd.IDs) == 0 {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid Codex account refresh command"))
		return
	}
	out := events.RefreshByIDResult{}
	seen := make(map[string]struct{}, len(cmd.IDs))
	for _, id := range cmd.IDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		refreshed, err := s.refreshToken(ev.Context(), id)
		if err != nil || !refreshed.Refreshed {
			out.Failed++
		} else {
			out.Refreshed++
		}
		if item, found := s.store.View(id); found {
			out.Items = append(out.Items, item)
		}
	}
	result.Set(out, nil)
}

func (s *Account) handleExportByID(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.ExportByIDCommand)
	if !ok || len(cmd.IDs) == 0 || len(cmd.IDs) > 1000 {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid Codex account export command"))
		return
	}
	result.Set(events.ExportByIDResult{Items: s.store.ExportByIDs(cmd.IDs)}, nil)
}

func (s *Account) handleHealth(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	if _, ok := ev.Data().(events.HealthCommand); !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid Codex account health command"))
		return
	}
	result.Set(s.store.Health(), nil)
}

func (s *Account) handleListDiscoveryCandidates(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	command, ok := ev.Data().(events.ListDiscoveryCandidatesCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid Codex discovery candidates command"))
		return
	}
	result.Set(s.store.ListDiscoveryCandidates(command.AccountIDs), nil)
}

func (s *Account) handlePutModelSnapshot(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.PutModelSnapshotCommand)
	if !ok || strings.TrimSpace(cmd.AccountID) == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid Codex model snapshot command"))
		return
	}
	version, found, err := s.store.PutModelSnapshot(cmd.AccountID, cmd.Snapshot)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(events.PutModelSnapshotResult{Version: version, OK: found}, nil)
}

func (s *Account) handleRecordModelDiscoveryFailure(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.RecordModelDiscoveryFailureCommand)
	if !ok || strings.TrimSpace(cmd.AccountID) == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid Codex discovery failure command"))
		return
	}
	retryAt, found, err := s.store.RecordModelDiscoveryFailure(cmd.AccountID, cmd.ErrorClass)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(events.RecordModelDiscoveryFailureResult{RetryAt: retryAt, OK: found}, nil)
}

func (s *Account) handleCatalogSnapshot(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	if _, ok := ev.Data().(events.CatalogSnapshotCommand); !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid Codex catalog snapshot command"))
		return
	}
	result.Set(s.store.CatalogSnapshot(), nil)
}

func (s *Account) handleListUsageCandidates(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	command, ok := ev.Data().(events.ListUsageCandidatesCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid Codex usage candidate command"))
		return
	}
	result.Set(s.store.ListUsageCandidates(command.AccountIDs), nil)
}

func (s *Account) handlePutUsageSnapshot(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	command, ok := ev.Data().(events.PutUsageSnapshotCommand)
	if !ok || strings.TrimSpace(command.AccountID) == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid Codex usage snapshot command"))
		return
	}
	updated, err := s.store.PutUsageSnapshot(command.AccountID, command.Snapshot)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(events.PutUsageSnapshotResult{OK: updated}, nil)
}

func (s *Account) handleMergeUsageSnapshot(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	command, ok := ev.Data().(events.MergeUsageSnapshotCommand)
	if !ok || strings.TrimSpace(command.AccountID) == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid merge usage snapshot command"))
		return
	}
	updated, err := s.store.MergeUsageSnapshot(command.AccountID, command.Snapshot)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(events.MergeUsageSnapshotResult{OK: updated}, nil)
}

func (s *Account) handleRecordUsageFailure(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	command, ok := ev.Data().(events.RecordUsageFailureCommand)
	if !ok || strings.TrimSpace(command.AccountID) == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid Codex usage failure command"))
		return
	}
	updated, err := s.store.RecordUsageFailure(command.AccountID, command.Error)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(events.RecordUsageFailureResult{OK: updated}, nil)
}

func (s *Account) handleOAuthStart(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.OAuthStartCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid Codex OAuth start command"))
		return
	}
	out, err := s.startOAuth(cmd)
	if err != nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, err.Error()))
		return
	}
	result.Set(out, nil)
}

func (s *Account) handleOAuthFinish(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.OAuthFinishCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid Codex OAuth finish command"))
		return
	}
	session, code, err := s.finishOAuth(cmd.SessionID, cmd.Callback)
	if err != nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, err.Error()))
		return
	}
	oauthCtx, cancel := s.oauthRequestContext(ev.Context())
	defer cancel()
	tokens, err := oauth.ExchangeAuthorizationCode(oauthCtx, oauth.AuthorizationCodeRequest{Code: code, CodeVerifier: session.verifier, Proxy: session.proxy})
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, "Codex OAuth exchange failed"))
		return
	}
	added, updated, skipped, importErr := s.store.Import([]events.CredentialInput{{CredentialType: "codex_cli", AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken, IDToken: tokens.IDToken, AccountID: tokens.AccountID, Email: tokens.Email, Expired: tokens.Expired, Proxy: session.proxy}})
	if importErr != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, importErr.Error()))
		return
	}
	if added+updated != 1 || skipped != 0 {
		result.Set(nil, cd.NewError(cd.Unexpected, "Codex OAuth response has incomplete credentials"))
		return
	}
	s.consumeOAuth(cmd.SessionID)
	item, _ := s.store.ViewByRefreshToken(tokens.RefreshToken)
	result.Set(events.OAuthFinishResult{Added: added > 0, Item: item}, nil)
}

func (s *Account) refreshToken(ctx context.Context, accountID string) (events.RefreshTokenResult, error) {
	if s.stopping.Load() {
		return events.RefreshTokenResult{}, context.Canceled
	}
	s.refreshMu.Lock()
	if flight := s.refreshes[accountID]; flight != nil {
		s.refreshMu.Unlock()
		<-flight.done
		return flight.result, flight.err
	}
	flight := &refreshFlight{done: make(chan struct{})}
	s.refreshes[accountID] = flight
	s.refreshMu.Unlock()
	result, err := s.refreshTokenOnce(ctx, accountID)
	s.refreshMu.Lock()
	flight.result, flight.err = result, err
	delete(s.refreshes, accountID)
	close(flight.done)
	s.refreshMu.Unlock()
	return result, err
}

func (s *Account) refreshTokenOnce(ctx context.Context, accountID string) (events.RefreshTokenResult, error) {
	credential, found := s.store.RefreshCredential(accountID)
	if !found {
		out, recordErr := s.store.RecordRefreshFailure(accountID, events.ErrorInvalidToken, true)
		if recordErr != nil {
			return out, recordErr
		}
		return out, fmt.Errorf("OAuth refresh credential is unavailable")
	}
	oauthCtx, cancel := s.oauthRequestContext(ctx)
	defer cancel()
	result, err := oauth.Refresh(oauthCtx, oauth.Request{RefreshToken: credential.RefreshToken, Proxy: credential.Proxy})
	if err != nil {
		class, permanent := events.ErrorUpstream, false
		if oauthErr, ok := err.(*oauth.Error); ok {
			class, permanent = oauthErr.Class, oauthErr.Permanent
		}
		out, recordErr := s.store.RecordRefreshFailure(accountID, class, permanent)
		if recordErr != nil {
			return out, recordErr
		}
		return out, err
	}
	return s.store.ApplyRefresh(accountID, events.CredentialInput{AccessToken: result.AccessToken, RefreshToken: result.RefreshToken, IDToken: result.IDToken, AccountID: result.AccountID, Email: result.Email, Expired: result.Expired})
}

func (s *Account) refreshExpiring() {
	if s.stopping.Load() || s.store == nil {
		return
	}
	for _, id := range s.store.RefreshDue(time.Now().UTC(), refreshLead) {
		if s.stopping.Load() {
			return
		}
		_, _ = s.refreshToken(s.shutdownCtx, id)
	}
}

func (s *Account) startOAuth(command events.OAuthStartCommand) (events.OAuthStartResult, error) {
	if _, err := url.ParseRequestURI("http://localhost:1455"); err != nil {
		return events.OAuthStartResult{}, err
	}
	if strings.TrimSpace(command.Proxy) != "" {
		parsed, err := url.ParseRequestURI(strings.TrimSpace(command.Proxy))
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return events.OAuthStartResult{}, fmt.Errorf("invalid account proxy URL")
		}
	}
	verifier, err := randomURL(64)
	if err != nil {
		return events.OAuthStartResult{}, err
	}
	nonce, err := randomURL(24)
	if err != nil {
		return events.OAuthStartResult{}, err
	}
	sessionID := uuid.NewString()
	state := sessionID + "." + nonce
	params := url.Values{"client_id": {oauth.ClientID}, "response_type": {"code"}, "redirect_uri": {oauth.RedirectURI}, "scope": {"openid profile email offline_access api.connectors.read api.connectors.invoke"}, "state": {state}, "code_challenge": {oauth.CodeChallenge(verifier)}, "code_challenge_method": {"S256"}, "prompt": {"login"}, "id_token_add_organizations": {"true"}, "codex_cli_simplified_flow": {"true"}}
	if hint := strings.TrimSpace(command.EmailHint); hint != "" {
		params.Set("login_hint", hint)
	}
	s.oauthMu.Lock()
	s.pruneOAuthLocked(time.Now())
	s.oauthSessions[sessionID] = oauthSession{verifier: verifier, state: state, proxy: strings.TrimSpace(command.Proxy), created: time.Now()}
	s.oauthMu.Unlock()
	return events.OAuthStartResult{SessionID: sessionID, AuthorizeURL: oauth.AuthorizeURL + "?" + params.Encode(), ExpiresIn: int(oauthSessionTTL.Seconds()), RedirectURIPrefix: oauth.RedirectURI}, nil
}

func (s *Account) finishOAuth(sessionID, callback string) (oauthSession, string, error) {
	parsed, err := url.Parse(strings.TrimSpace(callback))
	if err != nil {
		return oauthSession{}, "", fmt.Errorf("parse OAuth callback: %w", err)
	}
	if parsed.Scheme != "http" || parsed.Host != "localhost:1455" || parsed.Path != "/auth/callback" {
		return oauthSession{}, "", fmt.Errorf("OAuth callback must use %s", oauth.RedirectURI)
	}
	code, state := strings.TrimSpace(parsed.Query().Get("code")), strings.TrimSpace(parsed.Query().Get("state"))
	if code == "" {
		return oauthSession{}, "", fmt.Errorf("OAuth callback has no code")
	}
	if prefix := strings.SplitN(state, ".", 2)[0]; prefix != "" {
		sessionID = prefix
	}
	s.oauthMu.Lock()
	defer s.oauthMu.Unlock()
	s.pruneOAuthLocked(time.Now())
	session, found := s.oauthSessions[strings.TrimSpace(sessionID)]
	if !found {
		return oauthSession{}, "", fmt.Errorf("OAuth session expired or does not exist")
	}
	if state != session.state {
		return oauthSession{}, "", fmt.Errorf("OAuth state mismatch")
	}
	return session, code, nil
}

func (s *Account) consumeOAuth(sessionID string) {
	s.oauthMu.Lock()
	delete(s.oauthSessions, strings.TrimSpace(sessionID))
	s.oauthMu.Unlock()
}

func (s *Account) pruneOAuthLocked(now time.Time) {
	for id, session := range s.oauthSessions {
		if now.Sub(session.created) > oauthSessionTTL {
			delete(s.oauthSessions, id)
		}
	}
	for len(s.oauthSessions) >= oauthSessionMax {
		var oldID string
		var oldTime time.Time
		for id, session := range s.oauthSessions {
			if oldID == "" || session.created.Before(oldTime) {
				oldID, oldTime = id, session.created
			}
		}
		delete(s.oauthSessions, oldID)
	}
}

func randomURL(size int) (string, error) {
	bytes := make([]byte, size)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

func (s *Account) oauthRequestContext(ctx context.Context) (context.Context, context.CancelFunc) {
	base := context.WithoutCancel(ctx)
	if s.oauthTimeout <= 0 {
		return context.WithCancel(base)
	}
	return context.WithTimeout(base, s.oauthTimeout)
}
