// Package biz implements account-pool use cases.
package biz

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"ai-proxy/internal/modules/application/chatgptaccountpool/internal/oauth"
	"ai-proxy/internal/modules/application/chatgptaccountpool/internal/store"
	"ai-proxy/internal/modules/application/chatgptaccountpool/pkg/common"
	events "ai-proxy/internal/modules/application/chatgptaccountpool/pkg/events"
	basebiz "ai-proxy/internal/modules/base/biz"
	configevents "ai-proxy/internal/modules/blocks/configruntime/pkg/events"
	"ai-proxy/internal/pkg/aiproxycredential"
	"github.com/google/uuid"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/task"
)

type Account struct {
	basebiz.Base
	store         *store.Store
	topics        []string
	refreshing    atomic.Bool
	stopping      atomic.Bool
	shutdownCtx   context.Context
	shutdown      context.CancelFunc
	refreshEvery  time.Duration
	oauth         oauthClient
	textRefreshMu sync.Mutex
	textRefreshes map[string]*textRefreshFlight
	bridge        oauthBridge
	progressMu    sync.Mutex
	progress      map[string]events.RefreshProgress
	progressAt    map[string]time.Time
}

// oauthClient keeps the request-time refresh path testable while retaining a
// single narrow OAuth implementation in production.
type oauthClient interface {
	Refresh(context.Context, oauth.Request) (oauth.Result, error)
	ExchangeAuthorizationCode(context.Context, oauth.AuthorizationCodeRequest) (oauth.Result, error)
}

type textRefreshFlight struct {
	done   chan struct{}
	result events.RefreshTextTokenResult
	err    error
}

// New obtains the ChatGPT Web runtime configuration through its owner and
// creates only the account-pool's own local state.
func New(ctx context.Context, hub event.Hub, background task.BackgroundRoutine) (*Account, *cd.Error) {
	bootstrap, err := configevents.RequestBootstrap(ctx, hub, common.UnitID)
	if err != nil {
		return nil, cd.NewError(cd.IllegalParam, err.Error())
	}
	if err := os.MkdirAll(bootstrap.Config.State.Dir, 0o700); err != nil {
		return nil, cd.NewError(cd.Unexpected, "create chatgpt web data directory: "+err.Error())
	}
	credentialCodec, err := aiproxycredential.FromEnvironment()
	if err != nil {
		return nil, cd.NewError(cd.IllegalParam, err.Error())
	}
	state, err := store.Open(bootstrap.Config.State.Database, bootstrap.Config.State.MemoryLimit, bootstrap.Config.State.Threads, 3, credentialCodec)
	if err != nil {
		return nil, cd.NewError(cd.Unexpected, "open chatgpt account state: "+err.Error())
	}
	return newAccount(hub, background, state, time.Duration(bootstrap.Config.ChatGPTWeb.RefreshAccountIntervalMinute)*time.Minute), nil
}

func newAccount(hub event.Hub, background task.BackgroundRoutine, st *store.Store, refreshEvery time.Duration) *Account {
	shutdownCtx, shutdown := context.WithCancel(context.Background())
	b := &Account{
		Base:          basebiz.New(common.UnitID, hub, background),
		store:         st,
		shutdownCtx:   shutdownCtx,
		shutdown:      shutdown,
		refreshEvery:  refreshEvery,
		oauth:         oauth.NewClient(),
		textRefreshes: map[string]*textRefreshFlight{},
		progress:      map[string]events.RefreshProgress{},
		progressAt:    map[string]time.Time{},
	}
	if st == nil {
		return b
	}
	b.topics = []string{
		events.TopicList,
		events.TopicAdd,
		events.TopicDelete,
		events.TopicUpdate,
		events.TopicDeleteByID,
		events.TopicUpdateByID,
		events.TopicExportByID,
		events.TopicRefreshByID,
		events.TopicAcquireImageToken,
		events.TopicAcquireImageAccount,
		events.TopicReleaseImageSlot,
		events.TopicMarkImageResult,
		events.TopicAcquireTextToken,
		events.TopicAcquireTextAccount,
		events.TopicRecordTextResult,
		events.TopicRefreshTextToken,
		events.TopicRemoveInvalid,
		events.TopicHealth,
		events.TopicOAuthStart,
		events.TopicOAuthFinish,
		events.TopicExport,
		events.TopicRefresh,
		events.TopicRefreshProgress,
		events.TopicListDiscoveryCandidates,
		events.TopicPutModelSnapshot,
		events.TopicRecordModelDiscoveryFailure,
		events.TopicCatalogSnapshot,
	}
	b.SubscribeFunc(events.TopicList, b.handleList)
	b.SubscribeFunc(events.TopicAdd, b.handleAdd)
	b.SubscribeFunc(events.TopicDelete, b.handleDelete)
	b.SubscribeFunc(events.TopicUpdate, b.handleUpdate)
	b.SubscribeFunc(events.TopicDeleteByID, b.handleDeleteByID)
	b.SubscribeFunc(events.TopicUpdateByID, b.handleUpdateByID)
	b.SubscribeFunc(events.TopicExportByID, b.handleExportByID)
	b.SubscribeFunc(events.TopicRefreshByID, b.handleRefreshByID)
	b.SubscribeFunc(events.TopicAcquireImageToken, b.handleAcquireImage)
	b.SubscribeFunc(events.TopicAcquireImageAccount, b.handleAcquireImageAccount)
	b.SubscribeFunc(events.TopicReleaseImageSlot, b.handleReleaseSlot)
	b.SubscribeFunc(events.TopicMarkImageResult, b.handleMarkImage)
	b.SubscribeFunc(events.TopicAcquireTextToken, b.handleAcquireText)
	b.SubscribeFunc(events.TopicAcquireTextAccount, b.handleAcquireTextAccount)
	b.SubscribeFunc(events.TopicRecordTextResult, b.handleRecordTextResult)
	b.SubscribeFunc(events.TopicRefreshTextToken, b.handleRefreshTextToken)
	b.SubscribeFunc(events.TopicRemoveInvalid, b.handleRemoveInvalid)
	b.SubscribeFunc(events.TopicHealth, b.handleHealth)
	b.SubscribeFunc(events.TopicOAuthStart, b.handleOAuthStart)
	b.SubscribeFunc(events.TopicOAuthFinish, b.handleOAuthFinish)
	b.SubscribeFunc(events.TopicExport, b.handleExport)
	b.SubscribeFunc(events.TopicRefresh, b.handleRefresh)
	b.SubscribeFunc(events.TopicRefreshProgress, b.handleRefreshProgress)
	b.SubscribeFunc(events.TopicListDiscoveryCandidates, b.handleListDiscoveryCandidates)
	b.SubscribeFunc(events.TopicPutModelSnapshot, b.handlePutModelSnapshot)
	b.SubscribeFunc(events.TopicRecordModelDiscoveryFailure, b.handleRecordModelDiscoveryFailure)
	b.SubscribeFunc(events.TopicCatalogSnapshot, b.handleCatalogSnapshot)
	return b
}

func (s *Account) handleRefresh(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	if s.stopping.Load() {
		result.Set(nil, cd.NewError(cd.Unexpected, "account pool is shutting down"))
		return
	}
	cmd, ok := ev.Data().(events.RefreshCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid account refresh command"))
		return
	}
	progressID := uuid.NewString()
	s.putProgress(events.RefreshProgress{ProgressID: progressID, Errors: []events.RefreshError{}})
	if err := s.BackgroundRoutine().AsyncFunction(func() { s.runManualRefresh(progressID, cmd.AccessTokens) }); err != nil {
		s.finishProgress(progressID, err.Error())
		result.Set(nil, cd.NewError(cd.Unexpected, "account refresh task unavailable"))
		return
	}
	result.Set(events.RefreshResult{ProgressID: progressID}, nil)
}

func (s *Account) handleRefreshByID(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	if s.stopping.Load() {
		result.Set(nil, cd.NewError(cd.Unexpected, "account pool is shutting down"))
		return
	}
	cmd, ok := ev.Data().(events.RefreshByIDCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid account refresh-by-id command"))
		return
	}
	progressID := uuid.NewString()
	s.putProgress(events.RefreshProgress{ProgressID: progressID, Errors: []events.RefreshError{}})
	if err := s.BackgroundRoutine().AsyncFunction(func() { s.runManualRefreshByID(progressID, cmd.IDs) }); err != nil {
		s.finishProgress(progressID, err.Error())
		result.Set(nil, cd.NewError(cd.Unexpected, "account refresh task unavailable"))
		return
	}
	result.Set(events.RefreshResult{ProgressID: progressID}, nil)
}

func (s *Account) handleRefreshProgress(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.RefreshProgressCommand)
	if !ok || cmd.ProgressID == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid account refresh progress command"))
		return
	}
	progress, found := s.getProgress(cmd.ProgressID)
	if !found {
		result.Set(nil, cd.NewError(cd.Unexpected, "refresh progress not found"))
		return
	}
	result.Set(events.RefreshProgressResult{Progress: progress}, nil)
}

func (s *Account) handleExport(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.ExportCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid account export command"))
		return
	}
	items := s.store.Export(cmd.AccessTokens)
	if len(items) > 1000 {
		result.Set(nil, cd.NewError(cd.IllegalParam, "too many accounts to export"))
		return
	}
	result.Set(events.ExportResult{Items: items}, nil)
}

func (s *Account) handleExportByID(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.ExportByIDCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid account export-by-id command"))
		return
	}
	items := s.store.ExportByIDs(cmd.IDs)
	if len(items) > 1000 {
		result.Set(nil, cd.NewError(cd.IllegalParam, "too many accounts to export"))
		return
	}
	result.Set(events.ExportResult{Items: items}, nil)
}

func (s *Account) handleOAuthStart(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.OAuthStartCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid oauth start command"))
		return
	}
	out, err := s.bridge.start(cmd.EmailHint)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
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
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid oauth finish command"))
		return
	}
	code, verifier, sessionID, err := s.bridge.finish(cmd.SessionID, cmd.Callback)
	if err != nil {
		result.Set(nil, cd.NewError(cd.IllegalParam, err.Error()))
		return
	}
	tokens, err := s.oauth.ExchangeAuthorizationCode(context.Background(), oauth.AuthorizationCodeRequest{Code: code, CodeVerifier: verifier, RedirectURI: oauthRedirectURI})
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	item, added, err := s.store.AddOAuth(tokens.AccessToken, tokens.RefreshToken, tokens.IDToken)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	s.bridge.consume(sessionID)
	result.Set(events.OAuthFinishResult{Added: added, Item: item}, nil)
}

func (s *Account) Run(ctx context.Context) *cd.Error {
	if s.store != nil && s.refreshEvery > 0 {
		s.Timer(ctx, s.refreshEvery, 0, s.refreshAccounts)
	}
	return nil
}

func (s *Account) Teardown(context.Context) {
	// BackgroundRoutine does not wait for individual tasks before module
	// teardown. Keep the store alive until task closures release Account, and
	// make those tasks exit without further upstream or persistence work.
	s.stopping.Store(true)
	s.shutdown()
	for _, topic := range s.topics {
		s.UnsubscribeFunc(topic)
	}
	if s.store != nil {
		_ = s.store.Close()
	}
}

func (s *Account) handleList(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	if _, ok := ev.Data().(events.ListCommand); !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid list command"))
		return
	}
	result.Set(events.ListResult{Items: s.store.List()}, nil)
}

func (s *Account) handleAdd(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.AddCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid add command"))
		return
	}
	added, updated, skipped, err := s.store.Import(cmd.Tokens, cmd.Accounts, cmd.SourceType)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(events.AddResult{Added: added, Updated: updated, Skipped: skipped}, nil)
}

func (s *Account) handleDelete(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.DeleteCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid delete command"))
		return
	}
	deleted, err := s.store.Delete(cmd.Tokens)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(events.DeleteResult{Deleted: deleted}, nil)
}

func (s *Account) handleDeleteByID(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.DeleteByIDCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid account delete-by-id command"))
		return
	}
	deleted, err := s.store.DeleteByIDs(cmd.IDs)
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
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid update command"))
		return
	}
	item, found, err := s.store.Update(cmd.AccessToken, cmd.Type, cmd.Status, cmd.Quota, cmd.Proxy)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	if !found {
		result.Set(nil, cd.NewError(cd.Unexpected, "account not found"))
		return
	}
	result.Set(events.UpdateResult{Item: item}, nil)
}

func (s *Account) handleUpdateByID(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.UpdateByIDCommand)
	if !ok || cmd.ID == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid account update-by-id command"))
		return
	}
	item, found, err := s.store.UpdateByID(cmd.ID, cmd.Type, cmd.Status, cmd.Quota, cmd.Proxy)
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	if !found {
		result.Set(nil, cd.NewError(cd.Unexpected, "account not found"))
		return
	}
	result.Set(events.UpdateResult{Item: item}, nil)
}

func (s *Account) handleAcquireImage(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.AcquireImageTokenCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid acquire image command"))
		return
	}
	acc, found := s.store.AcquireImageToken(cmd.PlanType, cmd.SourceType, cmd.Exclude, cmd.Model, cmd.Capability)
	if !found {
		result.Set(nil, cd.NewError(cd.Unexpected, "no available image quota"))
		return
	}
	result.Set(events.AcquireImageTokenResult{AccessToken: acc.AccessToken, Account: acc}, nil)
}

func (s *Account) handleAcquireImageAccount(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.AcquireImageAccountCommand)
	if !ok || cmd.AccountID == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid acquire image account command"))
		return
	}
	acc, found := s.store.AcquireImageAccount(cmd.AccountID)
	if !found {
		result.Set(nil, cd.NewError(cd.Unexpected, "saved image account is unavailable"))
		return
	}
	result.Set(events.AcquireImageTokenResult{AccessToken: acc.AccessToken, Account: acc}, nil)
}

func (s *Account) handleReleaseSlot(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.ReleaseImageSlotCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid release slot command"))
		return
	}
	s.store.ReleaseImageSlot(cmd.AccessToken)
	result.Set(events.ReleaseImageSlotResult{OK: true}, nil)
}

func (s *Account) handleMarkImage(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.MarkImageResultCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid mark image command"))
		return
	}
	acc, found := s.store.MarkImageResult(cmd.AccessToken, cmd.Model, cmd.Success, cmd.ErrorClass)
	if !found {
		result.Set(nil, cd.NewError(cd.Unexpected, "account not found"))
		return
	}
	result.Set(events.MarkImageResultResult{Account: acc}, nil)
}

func (s *Account) handleAcquireTextAccount(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.AcquireTextAccountCommand)
	if !ok || cmd.AccountID == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid acquire text account command"))
		return
	}
	acc, found := s.store.AcquireTextAccount(cmd.AccountID, cmd.Model, cmd.Capability)
	if !found {
		result.Set(nil, cd.NewError(cd.Unexpected, "saved text account is unavailable"))
		return
	}
	result.Set(events.AcquireTextAccountResult{AccessToken: acc.AccessToken, Account: acc}, nil)
}

func (s *Account) handleRecordTextResult(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.RecordTextResultCommand)
	if !ok || cmd.AccountID == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid record text result command"))
		return
	}
	acc, found := s.store.RecordTextResult(cmd.AccountID, cmd.Model, cmd.Success, cmd.ErrorClass)
	if !found {
		result.Set(nil, cd.NewError(cd.Unexpected, "account not found"))
		return
	}
	result.Set(events.RecordTextResultResult{Account: acc}, nil)
}

func (s *Account) handleRefreshTextToken(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	if s.stopping.Load() {
		result.Set(nil, cd.NewError(cd.Unexpected, "account pool is shutting down"))
		return
	}
	cmd, ok := ev.Data().(events.RefreshTextTokenCommand)
	if !ok || cmd.AccessToken == "" {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid refresh text token command"))
		return
	}
	refreshed, err := s.refreshTextToken(cmd.AccessToken)
	if err != nil {
		// Refresh rejection and a transient OAuth outage are normal account
		// lifecycle outcomes, not EventHub transport failures. Returning a
		// bounded result lets callers retain accounts on transient failures while
		// still retiring a demonstrably revoked credential.
		current, _ := s.store.ViewForAccessToken(cmd.AccessToken)
		result.Set(events.RefreshTextTokenResult{
			AccessToken:      current.AccessToken,
			Account:          current,
			PermanentFailure: !oauth.IsRetryable(err),
			ErrorClass:       oauth.FailureClass(err),
		}, nil)
		return
	}
	result.Set(refreshed, nil)
}

func (s *Account) handleAcquireText(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.AcquireTextTokenCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid acquire text command"))
		return
	}
	acc, found := s.store.AcquireTextToken(cmd.Exclude, cmd.Model, cmd.Capability)
	if !found {
		result.Set(nil, cd.NewError(cd.Unexpected, "no available text account"))
		return
	}
	result.Set(events.AcquireTextTokenResult{AccessToken: acc.AccessToken, Account: acc}, nil)
}

func (s *Account) handleRemoveInvalid(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.RemoveInvalidCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid remove invalid command"))
		return
	}
	removed := s.store.RemoveInvalid(cmd.AccessToken)
	result.Set(events.RemoveInvalidResult{Removed: removed}, nil)
}

func (s *Account) handleHealth(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	if _, ok := ev.Data().(events.HealthCommand); !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid health command"))
		return
	}
	result.Set(s.store.Health(), nil)
}

func (s *Account) handleListDiscoveryCandidates(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	if _, ok := ev.Data().(events.ListDiscoveryCandidatesCommand); !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid list discovery candidates command"))
		return
	}
	out, err := s.store.ListDiscoveryCandidates()
	if err != nil {
		result.Set(nil, cd.NewError(cd.Unexpected, err.Error()))
		return
	}
	result.Set(out, nil)
}

func (s *Account) handlePutModelSnapshot(ev event.Event, result event.Result) {
	if result == nil {
		return
	}
	cmd, ok := ev.Data().(events.PutModelSnapshotCommand)
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid put model snapshot command"))
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
	if !ok {
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid record model discovery failure command"))
		return
	}
	retryAt, found, err := s.store.RecordModelDiscoveryFailure(cmd.AccountID, cmd.Error)
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
		result.Set(nil, cd.NewError(cd.IllegalParam, "invalid catalog snapshot command"))
		return
	}
	result.Set(s.store.CatalogSnapshot(), nil)
}
