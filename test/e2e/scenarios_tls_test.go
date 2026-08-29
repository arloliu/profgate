//go:build e2e

package e2e

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// tlsGatewayName is the Deployment, ServiceAccount, ConfigMap, and
	// ClusterRoleBinding of the tls-gateway overlay.
	tlsGatewayName = "profgate-tls"
	// tlsHost is the name the client asks for and the certificate is issued
	// for, so the handshake verifies a name rather than skipping the check.
	tlsHost = "gateway"
	// rotationDeadline bounds the wait for a replaced Secret to reach the container.
	// The gateway re-reads its files every 30 seconds,
	// but what dominates this number is the kubelet's own Secret sync period,
	// a minute by default, plus the time the projected volume takes to swap.
	rotationDeadline = 4 * time.Minute
)

// authority is a certificate authority minted inside the test, and a leaf it
// signed for the names given. Two of them are what a rotation is: the gateway
// serves the first, the Secret is replaced with the second, and a client that
// trusts only one of them says which is being served.
type authority struct {
	pool    *x509.CertPool
	ca      *x509.Certificate
	caPEM   []byte
	certPEM []byte
	keyPEM  []byte
}

// newAuthority mints a CA and a leaf signed by it that certifies hosts:
// a host net.ParseIP accepts goes into the leaf's IP addresses, any other into
// its DNS names.
// The chain the gateway serves is leaf then CA, which is what an operator's
// tls.crt holds and what lets a client verify against the CA alone.
func newAuthority(t *testing.T, hosts ...string) authority {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "profgate-e2e-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA: %v", err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: hosts[0]},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(12 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			leafTmpl.IPAddresses = append(leafTmpl.IPAddresses, ip)
		} else {
			leafTmpl.DNSNames = append(leafTmpl.DNSNames, host)
		}
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf: %v", err)
	}
	leafKeyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatalf("marshal leaf key: %v", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(ca)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	certPEM := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), caPEM...)

	return authority{
		pool:    pool,
		ca:      ca,
		caPEM:   caPEM,
		certPEM: certPEM,
		keyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: leafKeyDER}),
	}
}

// tlsClient reaches the forwarded API listener over HTTPS, trusting only pool.
// Keep-alives are off so every request handshakes: a connection held open would
// keep serving the certificate it was opened with and hide a rotation.
func tlsClient(local string, pool *x509.CertPool) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DisableKeepAlives: true,
		TLSClientConfig:   &tls.Config{RootCAs: pool, ServerName: tlsHost, MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer

			return d.DialContext(ctx, network, local)
		},
	}}
}

// tlsTargetsURL is the targets endpoint of the TLS gateway, over HTTPS.
func tlsTargetsURL(ns, service string) string {
	return fmt.Sprintf("https://%s/v1/namespaces/%s/services/%s/targets", tlsHost, ns, service)
}

// tryTLS performs one GET and returns its status, or the transport error.
// Unlike get it never fails the test, because a handshake that is refused is
// what half of this scenario asserts.
func tryTLS(ctx context.Context, c *http.Client, rawURL string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	return resp.StatusCode, nil
}

// scenarioTLSRotation proves the API listener serves HTTPS from a mounted
// Secret, refuses a client that does not trust it, and follows a rotation of
// that Secret's contents without the Pod being replaced or restarted.
// It reaches no application Pod: the gateway's own listener is what is tested.
func scenarioTLSRotation(t *testing.T, h *Harness) {
	ns := h.Namespace(t)
	deployTestApp(t, h, ns)

	first := newAuthority(t, tlsHost)
	local, pod := deployTLSGateway(t, h, ns, first)

	// The certificate on disk is served, and it verifies against the authority
	// that signed it rather than against anything the client skipped checking.
	// The first request is polled, as the errors gateway polls its first one:
	// the port-forward is open before the connection through it is usable.
	client := tlsClient(local, first.pool)
	err := poll(t.Context(), settleDeadline, func(ctx context.Context) (bool, error) {
		status, err := tryTLS(ctx, client, tlsTargetsURL(ns, testAppName))

		return err == nil && status == http.StatusOK, nil
	})
	if err != nil {
		t.Fatalf("the gateway never answered targets over HTTPS: %v", err)
	}

	// A client trusting nothing is refused: the listener is not serving a
	// certificate anybody would accept, it is serving this one.
	empty := tlsClient(local, x509.NewCertPool())
	_, err = tryTLS(t.Context(), empty, tlsTargetsURL(ns, testAppName))
	var unknown x509.UnknownAuthorityError
	if !errors.As(err, &unknown) {
		t.Errorf("HTTPS GET with an empty trust store: error = %v, want an unknown-authority failure", err)
	}

	before := podState(t, h, ns, pod)

	// The rotation: the same Secret, a certificate from a second authority.
	second := newAuthority(t, tlsHost)
	start := time.Now()
	if err := h.applyTLSSecret(t.Context(), ns, second.certPEM, second.keyPEM); err != nil {
		t.Fatal(err)
	}

	rotated := tlsClient(local, second.pool)
	err = poll(t.Context(), rotationDeadline, func(ctx context.Context) (bool, error) {
		status, err := tryTLS(ctx, rotated, tlsTargetsURL(ns, testAppName))

		return err == nil && status == http.StatusOK, nil
	})
	if err != nil {
		t.Fatalf("the rotated certificate was never served within %v: %v", rotationDeadline, err)
	}
	t.Logf("the rotated certificate was served %v after the Secret was updated", time.Since(start).Round(time.Second))

	// The old authority no longer verifies, which separates "the new one works"
	// from "both do".
	if _, err := tryTLS(t.Context(), client, tlsTargetsURL(ns, testAppName)); err == nil {
		t.Error("a client trusting only the replaced authority still succeeded; the certificate was not swapped")
	}

	// The point of the whole scenario: the process that served the first
	// certificate is the process serving the second.
	after := podState(t, h, ns, pod)
	if after.uid != before.uid {
		t.Errorf("the Pod's UID changed from %s to %s; the certificate was rotated by replacing the Pod", before.uid, after.uid)
	}
	if after.restarts != before.restarts {
		t.Errorf("the container restart count went from %d to %d; the gateway restarted to pick the certificate up",
			before.restarts, after.restarts)
	}
}

// podIdentity is what proves a Pod was not replaced or restarted.
type podIdentity struct {
	uid      string
	restarts int32
}

// podState reads the Pod's UID and its gateway container's restart count.
func podState(t *testing.T, h *Harness, ns, name string) podIdentity {
	t.Helper()

	p, err := h.Client.CoreV1().Pods(ns).Get(t.Context(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod %s/%s: %v", ns, name, err)
	}
	id := podIdentity{uid: string(p.UID)}
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Name == gatewayContainer {
			id.restarts = cs.RestartCount
		}
	}

	return id
}

// deployTLSGateway creates the certificate Secret, applies the tls-gateway
// overlay into ns, waits for its Pod, and returns the local address a
// port-forward reaches its API listener at, with the Pod's name.
func deployTLSGateway(t *testing.T, h *Harness, ns string, ca authority) (string, string) {
	t.Helper()
	cfg := gatewayConfig(gatewayConfigOptions{TLSMount: tlsMountPath})

	return deployHTTPSGateway(t, h, ns, "tls-gateway", tlsGatewayName, ca, cfg)
}

// deployHTTPSGateway creates the certificate Secret, applies the named overlay
// into ns with cfg as its configuration, waits for its Pod, and returns the
// local address a port-forward reaches its API listener at, with the Pod's name.
// name is the resource name every object in the overlay shares.
// The Secret comes first on purpose: the overlay's volume is not optional, so a
// Pod applied before it exists would wait at mount time until the rollout gave
// up, which is a slower and less legible failure than a missing Secret.
// The patches are merged over the overlay beside the configuration one,
// which is how a gateway whose configuration names a credentials file gains the mount that carries it.
func deployHTTPSGateway(
	t *testing.T, h *Harness, ns, overlay, name string, ca authority, cfg string, patches ...patch,
) (string, string) {
	t.Helper()
	ctx := t.Context()
	selector := testAppLabel + "=" + name

	if err := h.applyTLSSecret(ctx, ns, ca.certPEM, ca.keyPEM); err != nil {
		t.Fatal(err)
	}
	h.Apply(t, ns, overlay, append([]patch{configPatch(name, cfg)}, patches...)...)
	t.Cleanup(func() {
		// The namespace deletion takes the rest; the ClusterRoleBinding is cluster-scoped.
		err := h.Client.RbacV1().ClusterRoleBindings().Delete(context.Background(), name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			t.Errorf("delete clusterrolebinding %s: %v", name, err)
		}
	})
	if err := h.kubectl(ctx, "rollout", "status", "deployment/"+name, "-n", ns,
		"--timeout="+rolloutTimeout.String()); err != nil {
		_ = h.kubectl(ctx, "describe", "pods", "-n", ns, "-l", selector)
		_ = h.kubectl(ctx, "logs", "-n", ns, "-l", selector, "--tail=50")
		t.Fatal(err)
	}
	pod, err := h.waitOnePod(ctx, ns, selector)
	if err != nil {
		t.Fatal(err)
	}
	ports, stop, err := h.forward(ctx, ns, pod, []string{"0:" + gatewayAPIPort})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)

	return net.JoinHostPort("127.0.0.1", strconv.Itoa(int(ports[0]))), pod
}
