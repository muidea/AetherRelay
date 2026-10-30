package aetherrelay

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	cd "github.com/muidea/magicCommon/def"
	frameworkservice "github.com/muidea/magicCommon/framework/service"
)

// Observe the standard lifecycle without changing plugin order or drain rules.
type shutdownService struct {
	frameworkservice.Service
	phase atomic.Value
}

func newShutdownService() *shutdownService {
	s := &shutdownService{Service: frameworkservice.DefaultService()}
	s.phase.Store("running")
	return s
}

func (s *shutdownService) Quiesce(ctx context.Context) *cd.Error {
	s.phase.Store("service.quiesce")
	started := time.Now()
	slog.Info("shutdown phase started", "stage", "service.quiesce")
	err := s.Service.(frameworkservice.Quiescer).Quiesce(ctx)
	slog.Info("shutdown phase finished", "stage", "service.quiesce", "elapsed", time.Since(started), "error", cd.ToStdError(err))
	if err == nil {
		s.phase.Store("runtime.drain")
	}
	return err
}

func (s *shutdownService) ShutdownChecked(ctx context.Context) *cd.Error {
	s.phase.Store("service.teardown")
	started := time.Now()
	slog.Info("shutdown phase started", "stage", "service.teardown")
	err := s.Service.(frameworkservice.CheckedShutdown).ShutdownChecked(ctx)
	slog.Info("shutdown phase finished", "stage", "service.teardown", "elapsed", time.Since(started), "error", cd.ToStdError(err))
	if err == nil {
		s.phase.Store("runtime.close")
	}
	return err
}

func (s *shutdownService) Shutdown(ctx context.Context) {
	if err := s.ShutdownChecked(ctx); err != nil {
		slog.Error("service shutdown incomplete", "error", err)
	}
}
