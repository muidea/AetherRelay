// Package modeldetection integrates manual model fingerprint assessments.
package modeldetection

import (
	"context"

	"aetherrelay/internal/modules/application/modeldetection/biz"
	"aetherrelay/internal/modules/application/modeldetection/pkg/common"

	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	plugincommon "github.com/muidea/magicCommon/framework/plugin/common"
	"github.com/muidea/magicCommon/framework/plugin/module"
	"github.com/muidea/magicCommon/task"
)

func init() { module.MustRegister(New()) }

type Module struct{ bizPtr *biz.Detection }

var (
	_ plugincommon.ShutdownStarter = (*Module)(nil)
	_ plugincommon.Quiescer        = (*Module)(nil)
)

func New() *Module            { return &Module{} }
func (m *Module) ID() string  { return common.UnitID }
func (m *Module) Weight() int { return 125 }
func (m *Module) Setup(_ context.Context, hub event.Hub, bg task.BackgroundRoutine) *cd.Error {
	b, err := biz.New(hub, bg)
	m.bizPtr = b
	return err
}
func (m *Module) Run(ctx context.Context) *cd.Error {
	if m.bizPtr == nil {
		return cd.NewError(cd.Unexpected, "detection not configured")
	}
	return m.bizPtr.Run(ctx)
}
func (m *Module) BeginShutdown(ctx context.Context) {
	if m.bizPtr != nil {
		m.bizPtr.BeginShutdown(ctx)
	}
}
func (m *Module) Quiesce(ctx context.Context) *cd.Error {
	if m.bizPtr != nil {
		return m.bizPtr.Quiesce(ctx)
	}
	return nil
}
func (m *Module) Teardown(ctx context.Context) {
	if m.bizPtr != nil {
		m.bizPtr.Teardown(ctx)
	}
	m.bizPtr = nil
}
