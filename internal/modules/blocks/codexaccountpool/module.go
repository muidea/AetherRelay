// Package codexaccountpool provides the Codex OAuth account-pool Block lifecycle.
package codexaccountpool

import (
	"context"

	"ai-proxy/internal/modules/blocks/codexaccountpool/biz"
	"ai-proxy/internal/modules/blocks/codexaccountpool/pkg/common"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/framework/plugin/module"
	"github.com/muidea/magicCommon/task"
)

func init() { module.Register(New()) }

type AccountPool struct{ bizPtr *biz.Account }

func New() *AccountPool            { return &AccountPool{} }
func (s *AccountPool) ID() string  { return common.UnitID }
func (s *AccountPool) Weight() int { return 42 }
func (s *AccountPool) Setup(ctx context.Context, hub event.Hub, background task.BackgroundRoutine) *cd.Error {
	account, err := biz.New(ctx, hub, background)
	if err != nil {
		return err
	}
	s.bizPtr = account
	return nil
}
func (s *AccountPool) Run(ctx context.Context) *cd.Error {
	if s.bizPtr == nil {
		return cd.NewError(cd.Unexpected, "Codex account biz is nil")
	}
	return s.bizPtr.Run(ctx)
}
func (s *AccountPool) Teardown(ctx context.Context) {
	if s.bizPtr != nil {
		s.bizPtr.Teardown(ctx)
	}
	s.bizPtr = nil
}
