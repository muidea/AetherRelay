package chatgptimagetask

import (
	"aetherrelay/internal/modules/application/chatgptimagetask/biz"
	"aetherrelay/internal/modules/application/chatgptimagetask/pkg/common"
	"context"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	plugincommon "github.com/muidea/magicCommon/framework/plugin/common"
	"github.com/muidea/magicCommon/framework/plugin/module"
	"github.com/muidea/magicCommon/task"
)

func init() {
	module.MustRegister(New())
}

type ImageTask struct {
	bizPtr *biz.ImageTask
}

func New() *ImageTask {
	return &ImageTask{}
}

func (s *ImageTask) ID() string  { return common.UnitID }
func (s *ImageTask) Weight() int { return 50 }

func (s *ImageTask) Setup(ctx context.Context, hub event.Hub, background task.BackgroundRoutine) *cd.Error {
	bizPtr, err := biz.New(ctx, hub, background)
	if err != nil {
		return err
	}
	s.bizPtr = bizPtr
	return nil
}

func (s *ImageTask) Run(ctx context.Context) *cd.Error {
	if s.bizPtr == nil {
		return cd.NewError(cd.Unexpected, "imagetask biz is nil")
	}
	return s.bizPtr.Run(ctx)
}

func (s *ImageTask) Teardown(ctx context.Context) {
	if s.bizPtr != nil {
		s.bizPtr.Teardown(ctx)
	}
}

var _ plugincommon.ShutdownStarter = (*ImageTask)(nil)

func (s *ImageTask) BeginShutdown(ctx context.Context) {
	if s.bizPtr != nil {
		s.bizPtr.BeginShutdown(ctx)
	}
}
