package http

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
)

type HTTPServer interface {
	Use(handler MiddleWareHandler)
	Bind(routeRegistry RouteRegistry)
	Run() error
	Shutdown(ctx context.Context) error
}

type HTTPServerOption func(*httpServer)

func WithPort(port string) HTTPServerOption {
	return func(s *httpServer) {
		s.listenAddr = fmt.Sprintf(":%s", port)
	}
}

func WithStatic(rootPath, prefixUri, excludeUri string) HTTPServerOption {
	return func(s *httpServer) {
		s.staticOptions = &StaticOptions{
			RootPath:   rootPath,
			PrefixUri:  prefixUri,
			ExcludeUri: excludeUri,
		}
	}
}

func WithStaticEnabled(enabled bool) HTTPServerOption {
	return func(s *httpServer) {
		s.enableStatic = enabled
	}
}

type httpServer struct {
	listenAddr       string
	routeRegistry    RouteRegistry
	middlewareChains MiddleWareChains
	staticOptions    *StaticOptions
	enableStatic     bool
	serverLock       sync.Mutex
	server           *http.Server
}

func NewHTTPServer(opts ...HTTPServerOption) HTTPServer {
	svr := &httpServer{
		listenAddr:       ":8080",
		middlewareChains: NewMiddleWareChains(),
		staticOptions:    &StaticOptions{RootPath: "./static", PrefixUri: "/static", ExcludeUri: "/api/"},
		enableStatic:     false,
	}

	for _, opt := range opts {
		opt(svr)
	}

	svr.Use(&logger{})
	svr.Use(&recovery{})

	if svr.enableStatic {
		svr.Use(&static{rootPath: Root})
	}

	return svr
}

func (s *httpServer) ServeHTTP(res http.ResponseWriter, req *http.Request) {
	httpContext := context.WithValue(req.Context(), StaticOptionsKey{}, s.staticOptions)
	ctx := NewRequestContext(s.middlewareChains.GetHandlers(), s.routeRegistry, httpContext, res, req)

	ctx.Run()
}

func (s *httpServer) Use(handler MiddleWareHandler) {
	s.middlewareChains.Append(handler)
}

func (s *httpServer) Bind(routeRegistry RouteRegistry) {
	s.routeRegistry = routeRegistry
}

func (s *httpServer) Run() error {
	server := &http.Server{Addr: s.listenAddr, Handler: s}
	s.serverLock.Lock()
	s.server = server
	s.serverLock.Unlock()
	defer func() {
		s.serverLock.Lock()
		if s.server == server {
			s.server = nil
		}
		s.serverLock.Unlock()
	}()

	slog.Info("server listening", "addr", s.listenAddr)
	err := server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *httpServer) Shutdown(ctx context.Context) error {
	s.serverLock.Lock()
	server := s.server
	s.serverLock.Unlock()
	if server == nil {
		return nil
	}
	return server.Shutdown(ctx)
}
