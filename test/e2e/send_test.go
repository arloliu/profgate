//go:build e2e

package e2e

import (
	"net"
	"net/http"
	"testing"
	"time"
)

// resettingListener answers every connection by closing it with a reset until outage has passed,
// and serves 200 after that.
// It is the state a port-forward is in while it reopens:
// the local port is bound, so a dial succeeds, and nothing behind it answers yet.
func resettingListener(t *testing.T, outage time.Duration) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	opened := time.Now()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			if time.Since(opened) < outage {
				// SetLinger(0) makes Close send a reset rather than a orderly shutdown,
				// which is what the client sees from a forward whose stream is not up.
				if tcp, ok := conn.(*net.TCPConn); ok {
					_ = tcp.SetLinger(0)
				}
				_ = conn.Close()

				continue
			}
			go func() {
				defer func() { _ = conn.Close() }()
				buf := make([]byte, 1024)
				if _, err := conn.Read(buf); err != nil {
					return
				}
				_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"))
			}()
		}
	}()

	return ln.Addr().String()
}

// TestSendOutlastsAPortThatResets is the budget the retry in send is sized by.
// A forward the Pod ended rebinds its local port before its stream to the Pod works,
// and client-go gives up on a stream it cannot create only after 30 seconds,
// so a request that gives up sooner fails on a forward that was about to heal.
// The outage here is longer than one connection attempt and shorter than the budget.
func TestSendOutlastsAPortThatResets(t *testing.T) {
	addr := resettingListener(t, 10*time.Second)
	got := send(t, &http.Client{}, http.MethodGet, "http://"+addr+"/", http.Header{}, nil)
	if got.Status != http.StatusOK {
		t.Fatalf("status %d, want 200", got.Status)
	}
}

// TestRepeatableOnlyWhereTheGatewayCannotTell records which requests send may repeat.
func TestRepeatableOnlyWhereTheGatewayCannotTell(t *testing.T) {
	keyed := http.Header{"Idempotency-Key": {"k"}}
	for _, c := range []struct {
		name    string
		method  string
		headers http.Header
		want    bool
	}{
		{"a GET changes nothing", http.MethodGet, http.Header{}, true},
		{"a HEAD changes nothing", http.MethodHead, http.Header{}, true},
		{"a POST with no key would be acted on twice", http.MethodPost, http.Header{}, false},
		{"a POST with a key is answered from its receipt", http.MethodPost, keyed, true},
		{"a PUT with no key would be written twice", http.MethodPut, http.Header{}, false},
		{"a DELETE with no key would be applied twice", http.MethodDelete, http.Header{}, false},
	} {
		if got := repeatable(c.method, c.headers); got != c.want {
			t.Errorf("%s: repeatable is %v, want %v", c.name, got, c.want)
		}
	}
}
