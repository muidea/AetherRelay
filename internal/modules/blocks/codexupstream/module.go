// Package codexupstream provides the native Codex Responses upstream Block lifecycle.
package codexupstream

import (
	"context"

	"ai-proxy/internal/modules/blocks/codexupstream/biz"
	"ai-proxy/internal/modules/blocks/codexupstream/pkg/common"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/framework/plugin/module"
	"github.com/muidea/magicCommon/task"
)

func init() { module.Register(New()) }

type Block struct{ bizPtr *biz.Upstream }

func New() *Block            { return &Block{} }
func (s *Block) ID() string  { return common.UnitID }
func (s *Block) Weight() int { return 44 }
func (s *Block) Setup(_ context.Context, hub event.Hub, background task.BackgroundRoutine) *cd.Error {
	s.bizPtr = biz.New(hub, background)
	return nil
}
func (s *Block) Run(ctx context.Context) *cd.Error {
	if s.bizPtr == nil {
		return cd.NewError(cd.Unexpected, "Codex upstream biz is nil")
	}
	return s.bizPtr.Run(ctx)
}
func (s *Block) Teardown(ctx context.Context) {
	if s.bizPtr != nil {
		s.bizPtr.Teardown(ctx)
	}
	s.bizPtr = nil
}
