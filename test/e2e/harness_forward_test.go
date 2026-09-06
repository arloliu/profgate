//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	// client-go's spdy.NewDialer returns this package's Dialer,
	// so the replacement cannot be used until client-go returns that one instead.
	"k8s.io/apimachinery/pkg/util/httpstream" //nolint:staticcheck // the type client-go hands back
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// forwardGateways opens one standing port-forward per gateway Pod and builds the clients that dial through them.
// Requests to port 9090 reach the ops listener; every other port reaches the API listener,
// so "http://gateway/..." and "http://gateway:9090/readyz" both work.
func (h *Harness) forwardGateways(ctx context.Context) (func(), error) {
	pods, err := h.gatewayPods(ctx)
	if err != nil {
		return nil, err
	}
	var stops []func()
	stopAll := func() {
		for _, s := range stops {
			s()
		}
	}
	for i, p := range pods {
		ports, stop, err := h.forward(ctx, p.Namespace, p.Name, []string{"0:" + gatewayAPIPort, "0:" + gatewayOpsPort})
		if err != nil {
			stopAll()
			return nil, fmt.Errorf("port-forward %s: %w", p.Name, err)
		}
		stops = append(stops, stop)
		h.log.Info("gateway port-forward", "pod", p.Name, "api", ports[0], "ops", ports[1])
		api, ops := ports[0], ports[1]
		h.Gateways[i] = &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				local := api
				if _, port, err := net.SplitHostPort(addr); err == nil && port == gatewayOpsPort {
					local = ops
				}
				var d net.Dialer
				return d.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", strconv.Itoa(int(local))))
			},
		}}
	}
	return stopAll, nil
}

// forward opens a port-forward to pod and returns the local ports in the order requested.
// The forward runs until stop is called; ctx only bounds opening it.
// One connection the Pod resets ends the whole session:
// client-go's handleConnection closes the stream connection on any message from the
// kubelet's error stream, ForwardPorts returns, and its deferred Close drops every local listener,
// so the next dial is refused.
// The session is reopened on the same local ports when that happens, so the
// address a caller holds stays valid for as long as the Pod lives.
func (h *Harness) forward(ctx context.Context, ns, pod string, ports []string) ([]uint16, func(), error) {
	req := h.Client.CoreV1().RESTClient().Post().Resource("pods").Namespace(ns).Name(pod).SubResource("portforward")
	transport, upgrader, err := spdy.RoundTripperFor(h.restConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("spdy transport: %w", err)
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, req.URL())
	stopCh := make(chan struct{})
	forwarded, done, err := openForward(ctx, dialer, ports, stopCh)
	if err != nil {
		close(stopCh)

		return nil, nil, fmt.Errorf("port-forward %s/%s: %w", ns, pod, err)
	}
	local := make([]uint16, len(forwarded))
	pinned := make([]string, len(forwarded))
	for i, fp := range forwarded {
		local[i] = fp.Local
		pinned[i] = fmt.Sprintf("%d:%d", fp.Local, fp.Remote)
	}
	// The reopen outlives the call that opened the forward, and stopCh is what ends it,
	// so a reopen carrying the caller's context would be cancelled while the scenario still needs the forward.
	go func() { //nolint:gosec // stopCh bounds this goroutine, not the caller's context
		for {
			select {
			case <-stopCh:
				return
			case err := <-done:
				h.log.Warn("port-forward ended; reopening", "namespace", ns, "pod", pod, "ports", pinned, "err", err)
			}
			for {
				_, next, err := openForward(context.Background(), dialer, pinned, stopCh)
				if err == nil {
					done = next

					break
				}
				h.log.Warn("port-forward reopen failed", "namespace", ns, "pod", pod, "ports", pinned, "err", err)
				select {
				case <-stopCh:
					return
				case <-time.After(pollInterval):
				}
			}
		}
	}()
	var once bool
	stop := func() {
		if !once {
			once = true
			close(stopCh)
		}
	}
	return local, stop, nil
}

// openForward starts one port-forward session over dialer and waits for its
// listeners; done carries ForwardPorts' result once the session ends.
// A session still opening when ctx ends or stopCh closes is left to the caller, whose close of stopCh ends it.
func openForward(ctx context.Context, dialer httpstream.Dialer, ports []string, stopCh chan struct{}) ([]portforward.ForwardedPort, <-chan error, error) {
	readyCh := make(chan struct{})
	var errOut bytes.Buffer
	pf, err := portforward.New(dialer, ports, stopCh, readyCh, io.Discard, &errOut)
	if err != nil {
		return nil, nil, err
	}
	done := make(chan error, 1)
	go func() { done <- pf.ForwardPorts() }()
	select {
	case <-readyCh:
	case err := <-done:
		return nil, nil, fmt.Errorf("%w (%s)", err, errOut.String())
	case <-stopCh:
		return nil, nil, errors.New("stopped")
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	forwarded, err := pf.GetPorts()
	if err != nil {
		return nil, nil, fmt.Errorf("forwarded ports: %w", err)
	}

	return forwarded, done, nil
}

// ForwardTestApp opens a port-forward to a test-app Pod's pprof port and returns the local base URL.
// Only a scenario that declares NeedsPodReach may call it,
// so a degraded lane skips exactly the scenarios that would fail there.
func (h *Harness) ForwardTestApp(t *testing.T, ns, pod string) string {
	t.Helper()
	if h.scenario == nil || !h.scenario.NeedsPodReach {
		t.Fatalf("ForwardTestApp called by a scenario that does not declare NeedsPodReach")
	}
	ports, stop, err := h.forward(t.Context(), ns, pod, []string{"0:" + testAppPort})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	return "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(int(ports[0])))
}

// RefreshGateways reopens the standing port-forwards against the gateway Pods
// running now.
// A scenario that deletes a gateway Pod leaves its forward pointing at a Pod
// that no longer exists, and every later scenario reads both replicas.
// The context is the process's, not the test's:
// the scenario that needs this calls it from a cleanup, which runs after the
// test's context is cancelled, and the forwards it opens serve every scenario
// after this one.
func (h *Harness) RefreshGateways(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if err := poll(ctx, rolloutTimeout, func(ctx context.Context) (bool, error) {
		_, err := h.gatewayPods(ctx)
		return err == nil, nil
	}); err != nil {
		t.Fatalf("gateways never returned to %d ready replicas: %v", gatewayReplicas, err)
	}
	h.stopGateways()
	stop, err := h.forwardGateways(ctx)
	if err != nil {
		t.Fatalf("reopen gateway port-forwards: %v", err)
	}
	h.stopGateways = stop
}
