// Package chatgptimagestore owns ChatGPT-generated local image persistence.
package chatgptimagestore

import (
	"context"

	"ai-proxy/internal/modules/blocks/chatgptimagestore/biz"
	"ai-proxy/internal/modules/blocks/chatgptimagestore/pkg/common"
	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	"github.com/muidea/magicCommon/framework/plugin/module"
	"github.com/muidea/magicCommon/task"
)

func init() {
	module.Register(New())
}

type ImageStore struct {
	bizPtr *biz.ImageStore
}

func New() *ImageStore {
	return &ImageStore{}
}

func (s *ImageStore) ID() string  { return common.UnitID }
func (s *ImageStore) Weight() int { return 35 }

func (s *ImageStore) Setup(ctx context.Context, hub event.Hub, background task.BackgroundRoutine) *cd.Error {
	bizPtr, err := biz.New(ctx, hub, background)
	if err != nil {
		return err
	}
	s.bizPtr = bizPtr
	return nil
}

func (s *ImageStore) Run(ctx context.Context) *cd.Error {
	if s.bizPtr == nil {
		return cd.NewError(cd.Unexpected, "imagestore biz is nil")
	}
	return s.bizPtr.Run(ctx)
}

func (s *ImageStore) Teardown(ctx context.Context) {
	if s.bizPtr != nil {
		s.bizPtr.Teardown(ctx)
	}
	s.bizPtr = nil
}
