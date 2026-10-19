package biz

import (
	"context"
	"net"
	"testing"

	upclient "aetherrelay/internal/modules/blocks/chatgptwebupstream/internal/client"
	"aetherrelay/internal/modules/blocks/chatgptwebupstream/pkg/events"
	transport "aetherrelay/internal/pkg/aetherrelaytransport"
)

func TestModelDiscoveryDiagnosticsPreserveStatusAndTransport(t *testing.T) {
	for _, tc := range []struct {
		err    *upclient.Error
		status int
		reason transport.Reason
		class  events.ErrorClass
	}{
		{&upclient.Error{Class: upclient.Upstream, StatusCode: 403}, 403, "", events.ErrClassUpstream},
		{&upclient.Error{Class: upclient.TLS, Cause: &net.DNSError{Name: "original-host", Err: "original-error"}}, 0, transport.DNS, events.ErrClassTLS},
		{&upclient.Error{Class: upclient.Timeout, Cause: context.DeadlineExceeded}, 0, transport.Timeout, events.ErrClassTimeout},
	} {
		got := modelDiscoveryFailure(tc.err)
		if got.HTTPStatus != tc.status || got.TransportReason != tc.reason || got.ErrorClass != tc.class {
			t.Fatalf("got=%+v", got)
		}
	}
}
