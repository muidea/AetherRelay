package chatgpttemporarychat

import (
	"aetherrelay/internal/modules/application/chatgpttemporarychat/biz"
	"aetherrelay/internal/modules/application/chatgpttemporarychat/pkg/common"
	"context"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/framework/plugin/module"
	"github.com/muidea/magicCommon/task"
)

func init() {
	module.Register(New())
}

type TemporaryChat struct {
	bizPtr *biz.TemporaryChat
}

func New() *TemporaryChat {
	return &TemporaryChat{}
}

func (s *TemporaryChat) ID() string  { return common.UnitID }
func (s *TemporaryChat) Weight() int { return 50 }

func (s *TemporaryChat) Setup(ctx context.Context, hub event.Hub, background task.BackgroundRoutine) *cd.Error {
	bizPtr, err := biz.New(ctx, hub, background)
	if err != nil {
		return err
	}
	s.bizPtr = bizPtr
	return nil
}

func (s *TemporaryChat) Run(ctx context.Context) *cd.Error {
	if s.bizPtr == nil {
		return cd.NewError(cd.Unexpected, "temporarychat biz is nil")
	}
	return s.bizPtr.Run(ctx)
}

func (s *TemporaryChat) Teardown(ctx context.Context) {
	if s.bizPtr != nil {
		s.bizPtr.Teardown(ctx)
	}
}
