// Package routeregistry 提供由 magicEngine 驱动的进程级 HTTP 路由与 listener 基础设施。
package routeregistry

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"aetherrelay/internal/initiators/routeregistry/pkg/common"
	"aetherrelay/internal/pkg/aetherrelaybootstrap"

	cd "github.com/muidea/magicCommon/def"
	"github.com/muidea/magicCommon/event"
	plugincommon "github.com/muidea/magicCommon/framework/plugin/common"
	"github.com/muidea/magicCommon/framework/plugin/initiator"
	"github.com/muidea/magicCommon/task"
	enginehttp "github.com/muidea/magicEngine/http"
)

func init() { initiator.MustRegister(New()) }

// routeRegistry 是进程级技术资源 owner。业务状态和业务路由策略不应放在这里。
type routeRegistry struct {
	routes   enginehttp.RouteRegistry
	handler  http.Handler
	server   *http.Server
	listener net.Listener
	done     chan error
	mu       sync.Mutex
	started  bool
	stopping bool
}

var routeRegistryListen = net.Listen

var (
	_ plugincommon.ShutdownStarter = (*routeRegistry)(nil)
	_ plugincommon.Quiescer        = (*routeRegistry)(nil)
)

func New() *routeRegistry { return &routeRegistry{} }

func (r *routeRegistry) ID() string { return common.RouteRegistryInitiator }

func (r *routeRegistry) Setup(_ context.Context, _ event.Hub, _ task.BackgroundRoutine) *cd.Error {
	bootstrap, ok := aetherrelaybootstrap.Current()
	if !ok {
		return cd.NewError(cd.IllegalParam, "AetherRelay bootstrap is not configured")
	}
	if bootstrap.Config.ListenAddr == "" {
		return cd.NewError(cd.IllegalParam, "http listen address is empty")
	}

	routes := enginehttp.NewRouteRegistry()
	server := enginehttp.NewHTTPServer()
	server.Bind(routes)
	handler, ok := server.(http.Handler)
	if !ok {
		return cd.NewError(cd.Unexpected, "magicEngine HTTP server does not implement http.Handler")
	}
	listener, err := routeRegistryListen("tcp", bootstrap.Config.ListenAddr)
	if err != nil {
		return cd.NewError(cd.Unexpected, fmt.Sprintf("listen %s: %v", bootstrap.Config.ListenAddr, err))
	}

	r.stopping = false
	r.routes = routes
	r.handler = handler
	r.server = &http.Server{
		Addr:              bootstrap.Config.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
	}
	r.listener = listener
	r.done = make(chan error, 1)
	return nil
}

// Run only validates the prepared gateway. The process service starts serving
// after every Module has registered its routes.
func (r *routeRegistry) Run(context.Context) *cd.Error {
	if r.server == nil || r.listener == nil || r.done == nil {
		return cd.NewError(cd.IllegalParam, "http route registry is not configured")
	}
	return nil
}

func (r *routeRegistry) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.server == nil || r.listener == nil || r.done == nil {
		return errors.New("http route registry is not configured")
	}
	if r.stopping {
		return errors.New("HTTP gateway is stopping")
	}
	if r.started {
		return nil
	}
	server, listener, done := r.server, r.listener, r.done
	r.started = true
	go func() {
		err := server.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
			err = nil
		}
		done <- err
	}()
	return nil
}

// Stop admission before any Module is drained or its database is released.
func (r *routeRegistry) BeginShutdown(context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopping = true
	if r.server != nil {
		r.server.SetKeepAlivesEnabled(false)
	}
	if r.listener != nil {
		if err := r.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			panic(cd.NewError(cd.Unexpected, "close HTTP listener: "+err.Error()))
		}
	}
}

func (r *routeRegistry) Quiesce(ctx context.Context) *cd.Error {
	if ctx == nil {
		ctx = context.Background()
	}
	r.mu.Lock()
	server := r.server
	r.mu.Unlock()
	if server != nil {
		if err := server.Shutdown(ctx); err != nil {
			return cd.NewError(cd.Timeout, "HTTP requests have not drained: "+err.Error())
		}
	}
	return nil
}

func (r *routeRegistry) Teardown(ctx context.Context) {
	r.BeginShutdown(ctx)
	if err := r.Quiesce(ctx); err != nil {
		panic(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routes = nil
	r.handler = nil
	r.server = nil
	r.listener = nil
	r.started = false
}

func (r *routeRegistry) GetRouteRegistry() enginehttp.RouteRegistry { return r.routes }

func (r *routeRegistry) Done() <-chan error { return r.done }
