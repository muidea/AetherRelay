package chatgptimagetask

import (
	"ai-proxy/internal/modules/application/chatgptimagetask/biz"
	"ai-proxy/internal/modules/application/chatgptimagetask/pkg/common"
	"context"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/framework/plugin/module"
	"github.com/muidea/magicCommon/task"
)

func init() {
	module.Register(New())
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
