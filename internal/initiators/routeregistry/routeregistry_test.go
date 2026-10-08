package routeregistry

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	configevents "aetherrelay/internal/modules/blocks/configruntime/pkg/events"
	"aetherrelay/internal/pkg/aetherrelaybootstrap"
	"aetherrelay/internal/pkg/aetherrelayconfig"
)

type testAddr string

func (a testAddr) Network() string { return "tcp" }
func (a testAddr) String() string  { return string(a) }

type testListener struct{ closed bool }

func (l *testListener) Accept() (net.Conn, error) { return nil, errors.New("listener closed") }
func (l *testListener) Close() error              { l.closed = true; return nil }
func (l *testListener) Addr() net.Addr            { return testAddr("127.0.0.1:0") }

func TestSetupFailsWhenListenerCannotBind(t *testing.T) {
	oldListen := routeRegistryListen
	t.Cleanup(func() { routeRegistryListen = oldListen })
	aetherrelaybootstrap.Configure(configevents.Bootstrap{Config: config.Config{ListenAddr: "127.0.0.1:0"}})
	routeRegistryListen = func(string, string) (net.Listener, error) { return nil, errors.New("listen failed") }

	if err := New().Setup(context.Background(), nil, nil); err == nil {
		t.Fatal("expected listener setup error")
	}
}

func TestTeardownClosesHTTPListener(t *testing.T) {
	oldListen := routeRegistryListen
	t.Cleanup(func() { routeRegistryListen = oldListen })
	aetherrelaybootstrap.Configure(configevents.Bootstrap{Config: config.Config{ListenAddr: "127.0.0.1:0"}})
	listener := &testListener{}
	routeRegistryListen = func(string, string) (net.Listener, error) { return listener, nil }

	router := New()
	if err := router.Setup(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if router.GetRouteRegistry() == nil {
		t.Fatal("route registry was not initialized")
	}
	router.Teardown(context.Background())
	if !listener.closed {
		t.Fatal("expected listener to be closed")
	}
}

func TestRunDefersServingUntilStart(t *testing.T) {
	oldListen := routeRegistryListen
	t.Cleanup(func() { routeRegistryListen = oldListen })
	aetherrelaybootstrap.Configure(configevents.Bootstrap{Config: config.Config{ListenAddr: "127.0.0.1:0"}})
	listener := &testListener{}
	routeRegistryListen = func(string, string) (net.Listener, error) { return listener, nil }

	router := New()
	if err := router.Setup(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := router.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-router.Done():
		t.Fatalf("Run unexpectedly started serving: %v", err)
	default:
	}
	if err := router.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-router.Done():
	case <-time.After(time.Second):
		t.Fatal("Start did not begin serving")
	}
}

func TestGatewayStopsAdmissionAndRetainsActiveRequests(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	router := New()
	router.listener = listener
	router.done = make(chan error, 1)
	router.server = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusOK)
	})}
	t.Cleanup(func() { router.server.Close() })
	if err := router.Start(); err != nil {
		t.Fatal(err)
	}
	responseDone := make(chan error, 1)
	go func() {
		response, err := http.Get("http://" + listener.Addr().String())
		if err == nil {
			response.Body.Close()
		}
		responseDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not start")
	}
	router.BeginShutdown(context.Background())
	// Verify admission at the listener: some test environments transparently
	// proxy TCP dials, so a completed client handshake is not an accept receipt.
	if _, err := listener.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("listener admission error=%v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := router.Quiesce(ctx); err == nil {
		t.Fatal("active request reported drained")
	}
	if router.server == nil {
		t.Fatal("failed drain released server")
	}
	close(release)
	released = true
	if err := <-responseDone; err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if err := router.Quiesce(ctx2); err != nil {
		t.Fatal(err)
	}
	// Clear only after the successful barrier; cleanup uses the saved server.
}
