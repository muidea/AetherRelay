// Package aetherrelay 提供主代理进程的 magicCommon process service。
package aetherrelay

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"time"

	registrycommon "aetherrelay/internal/initiators/routeregistry/pkg/common"
	configevents "aetherrelay/internal/modules/blocks/configruntime/pkg/events"
	"aetherrelay/internal/pkg/aetherrelaybootstrap"

	frameworkapplication "github.com/muidea/magicCommon/framework/application"
	"github.com/muidea/magicCommon/framework/plugin/initiator"
)

// Runtime 是进程级 service，不是业务 module。它选择并驱动 framework lifecycle；
// HTTP gateway Initiator、运行时 Block 和启动快照均由入口显式加载的组件提供。
type Runtime struct {
	application frameworkapplication.Application
	bootstrap   configevents.Bootstrap
	service     *shutdownService
}

func NewRuntime(cfg configevents.Bootstrap) *Runtime {
	configDir := "."
	if cfg.ConfigPath != "" {
		configDir = filepath.Dir(cfg.ConfigPath)
	}
	return &Runtime{
		bootstrap: cfg,
		application: frameworkapplication.NewApplication(frameworkapplication.Options{
			ConfigDir: configDir, ServiceName: "AetherRelay",
		}),
	}
}

func (r *Runtime) Startup(ctx context.Context) error {
	if r == nil || r.application == nil {
		return fmt.Errorf("AetherRelay runtime is not initialized")
	}
	aetherrelaybootstrap.Configure(r.bootstrap)
	r.service = newShutdownService()
	if err := r.application.Startup(ctx, r.service); err != nil {
		return err
	}
	return nil
}

func (r *Runtime) Run(ctx context.Context) error {
	if r == nil || r.application == nil {
		return fmt.Errorf("AetherRelay runtime is not initialized")
	}
	if err := r.application.Run(ctx); err != nil {
		return err
	}
	if err := startGateway(); err != nil {
		return err
	}
	return waitGateway(ctx)
}

func startGateway() error {
	gateway, getErr := initiator.GetEntity(registrycommon.RouteRegistryInitiator, registrycommon.GatewayRuntimeHelper(nil))
	if getErr != nil {
		return fmt.Errorf("get HTTP route registry initiator: %s", getErr.Message)
	}
	if err := gateway.Start(); err != nil {
		return fmt.Errorf("start HTTP route registry: %w", err)
	}
	return nil
}

func (r *Runtime) Shutdown(ctx context.Context) error {
	if r != nil && r.application != nil {
		started := time.Now()
		slog.Info("application shutdown started")
		watchdog := time.AfterFunc(3*time.Second, func() {
			stage := "application.shutdown"
			if r.service != nil {
				stage = r.service.phase.Load().(string)
			}
			slog.Warn("application shutdown still waiting", "stage", stage, "elapsed", time.Since(started))
		})
		defer watchdog.Stop()
		if err := r.application.ShutdownChecked(ctx); err != nil {
			return err
		}
		slog.Info("application shutdown finished", "elapsed", time.Since(started))
	}
	return nil
}

func waitGateway(ctx context.Context) error {
	gateway, getErr := initiator.GetEntity(registrycommon.RouteRegistryInitiator, registrycommon.GatewayRuntimeHelper(nil))
	if getErr != nil {
		return fmt.Errorf("get HTTP route registry initiator: %s", getErr.Message)
	}
	if gateway.Done() == nil {
		return fmt.Errorf("HTTP route registry completion channel is unavailable")
	}
	select {
	case err := <-gateway.Done():
		if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		return nil
	}
}

func MetricsAccessLabel(remote bool) string {
	if remote {
		return "remote-access"
	}
	return "loopback-only"
}
