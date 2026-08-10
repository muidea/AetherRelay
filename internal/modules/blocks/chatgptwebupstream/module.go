// Package chatgptwebupstream owns the authenticated ChatGPT Web transport.
package chatgptwebupstream

import (
	"context"

	"aetherrelay/internal/modules/blocks/chatgptwebupstream/biz"
	"aetherrelay/internal/modules/blocks/chatgptwebupstream/pkg/common"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/framework/plugin/module"
	"github.com/muidea/magicCommon/task"
)

func init() {
	module.Register(New())
}

type Upstream struct {
	bizPtr *biz.Upstream
}

func New() *Upstream {
	return &Upstream{}
}

func (s *Upstream) ID() string  { return common.UnitID }
func (s *Upstream) Weight() int { return 30 }

func (s *Upstream) Setup(ctx context.Context, hub event.Hub, background task.BackgroundRoutine) *cd.Error {
	bizPtr, err := biz.New(ctx, hub, background)
	if err != nil {
		return err
	}
	s.bizPtr = bizPtr
	return nil
}

func (s *Upstream) Run(ctx context.Context) *cd.Error {
	if s.bizPtr == nil {
		return cd.NewError(cd.Unexpected, "upstream biz is nil")
	}
	return s.bizPtr.Run(ctx)
}

func (s *Upstream) Teardown(ctx context.Context) {
	if s.bizPtr != nil {
		s.bizPtr.Teardown(ctx)
	}
	s.bizPtr = nil
}
