//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/pprof/profile"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
)

const (
	// pgoGatewayName is the Deployment, ServiceAccount, ConfigMap, and
	// ClusterRoleBinding of the pgo-gateway overlay.
	pgoGatewayName = "profgate-pgo"
	// pgoGatewaySelector selects the Pod of a scenario's own gateway.
	pgoGatewaySelector = testAppLabel + "=" + pgoGatewayName

	// barrierDeadline bounds the wait for a gateway's PGO routes to answer past
	// the replay barrier.
	barrierDeadline = 90 * time.Second
	// collectionDeadline bounds the wait for a Collection to reach a state.
	collectionDeadline = 3 * time.Minute
	// slotDeadline bounds the wait for a scheduled slot to fire: one minEvery
	// plus a scheduler tick, with room for a slot boundary just missed.
	slotDeadline = 3 * time.Minute
	// reclaimDeadline bounds the wait for a Collection whose owner was killed
	// to be reclaimed and finish on the surviving replica.
	reclaimDeadline = 4 * time.Minute
	// disabledWatch is how long the connection count is watched while the
	// gateway with PGO disabled runs.
	disabledWatch = 20 * time.Second

	// collectionIDLength is the length of a Collection identifier; the routes
	// match that grammar and nothing else.
	collectionIDLength = 20
)

// collectionRecord mirrors the fields of the stored record the PGO scenarios
// assert on.
type collectionRecord struct {
	ID              string          `json:"id"`
	Namespace       string          `json:"namespace"`
	Service         string          `json:"service"`
	Origin          string          `json:"origin"`
	Slot            string          `json:"slot"`
	State           string          `json:"state"`
	Attempt         int             `json:"attempt"`
	Reason          string          `json:"reason"`
	ResolvedVersion string          `json:"resolvedVersion"`
	Owner           *recordOwner    `json:"owner"`
	LeaseUntil      *time.Time      `json:"leaseUntil"`
	Progress        recordProgress  `json:"progress"`
	Manifest        *recordManifest `json:"manifest"`
	Artifact        *recordArtifact `json:"artifact"`
	CreatedAt       time.Time       `json:"createdAt"`
}

type recordOwner struct {
	Instance string `json:"instance"`
	Pod      string `json:"pod"`
}

type recordProgress struct {
	Round         int `json:"round"`
	Rounds        int `json:"rounds"`
	SamplesOK     int `json:"samplesOK"`
	SamplesFailed int `json:"samplesFailed"`
}

type recordManifest struct {
	ResolvedVersion string         `json:"resolvedVersion"`
	Truncated       bool           `json:"truncated"`
	Samples         []recordSample `json:"samples"`
}

type recordSample struct {
	Round  int    `json:"round"`
	Pod    string `json:"pod"`
	PodUID string `json:"podUID"`
	Node   string `json:"node"`
	Result string `json:"result"`
	Reason string `json:"reason"`
	Bytes  int64  `json:"bytes"`
}

type recordArtifact struct {
	Object string `json:"object"`
	Bytes  int64  `json:"bytes"`
}

// acceptedCollection mirrors the body of a created Collection.
type acceptedCollection struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

// pgoURL, collectionsURL, and collectionURL build the PGO routes;
// the harness clients reach the API listener for any host.
func pgoURL(ns, service string) string {
	return fmt.Sprintf("http://gateway/v1/namespaces/%s/services/%s/pgo", ns, service)
}

func collectionsURL(ns, service string) string {
	return fmt.Sprintf("http://gateway/v1/namespaces/%s/services/%s/collections", ns, service)
}

func collectionURL(id, tail string) string {
	return "http://gateway/v1/collections/" + id + tail
}

// do performs one request through c and returns the finished exchange.
func do(t *testing.T, c *http.Client, method, rawURL, body string, header http.Header) response {
	t.Helper()
	return doCtx(t.Context(), t, c, method, rawURL, body, header)
}

// doCtx is do against a context the caller owns.
// A cleanup runs after the test's own context is cancelled, so a request it
// makes needs one that is still live.
func doCtx(
	ctx context.Context, t *testing.T, c *http.Client, method, rawURL, body string, header http.Header,
) response {
	t.Helper()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	var req *http.Request
	var err error
	if reader != nil {
		req, err = http.NewRequestWithContext(ctx, method, rawURL, reader)
	} else {
		req, err = http.NewRequestWithContext(ctx, method, rawURL, nil)
	}
	if err != nil {
		t.Fatalf("build request %s %s: %v", method, rawURL, err)
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, rawURL, err)
	}
	payload, err := readBody(resp)
	if err != nil {
		t.Fatalf("read %s %s: %v", method, rawURL, err)
	}
	return response{Status: resp.StatusCode, Header: resp.Header, Body: payload}
}

// readBody reads and closes a response body.
func readBody(resp *http.Response) ([]byte, error) {
	defer func() { _ = resp.Body.Close() }()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// expectCode asserts a status and error code on a finished exchange.
func expectCode(t *testing.T, what string, resp response, status int, code string) {
	t.Helper()
	var e errorResponse
	if err := json.Unmarshal(resp.Body, &e); err != nil {
		t.Fatalf("%s: status %d, body %q is not an error envelope: %v", what, resp.Status, resp.Body, err)
	}
	if resp.Status != status || e.Code != code {
		t.Fatalf("%s: got %d %s, want %d %s", what, resp.Status, e.Code, status, code)
	}
}

// PurgeStores empties the three stores so the scenario starts from nothing.
func (h *Harness) PurgeStores(t *testing.T) {
	t.Helper()
	if err := h.NATS.purgeStores(t.Context()); err != nil {
		t.Fatalf("purge stores: %v", err)
	}
}

// waitPGOReady polls both shared gateways until their PGO routes answer past
// the replay barrier, which every one of them is behind for a moment after a
// gateway starts or its NATS connection returns.
func waitPGOReady(t *testing.T, h *Harness, ns, service string) {
	t.Helper()
	for i, c := range h.Gateways {
		var last response
		err := poll(t.Context(), barrierDeadline, func(_ context.Context) (bool, error) {
			last = do(t, c, http.MethodGet, pgoURL(ns, service), "", nil)
			return last.Status != http.StatusServiceUnavailable, nil
		})
		if err != nil {
			t.Fatalf("gateway %d never left the replay barrier: %v (last %d %s)", i, err, last.Status, last.Body)
		}
	}
}

// jsonHeaders is the media type a POST to a write route declares.
func jsonHeaders() http.Header { return http.Header{"Content-Type": {"application/json"}} }

// createCollection posts an on-demand Collection and returns its identifier.
func createCollection(t *testing.T, c *http.Client, ns, service, body string) string {
	t.Helper()
	resp := do(t, c, http.MethodPost, collectionsURL(ns, service), body, jsonHeaders())
	if resp.Status != http.StatusAccepted {
		t.Fatalf("POST collections %s/%s: status %d: %s", ns, service, resp.Status, resp.Body)
	}
	var accepted acceptedCollection
	if err := json.Unmarshal(resp.Body, &accepted); err != nil {
		t.Fatalf("POST collections %s/%s: body %q: %v", ns, service, resp.Body, err)
	}
	if len(accepted.ID) != collectionIDLength {
		t.Fatalf("collection id %q is %d characters, want %d", accepted.ID, len(accepted.ID), collectionIDLength)
	}
	if loc := resp.Header.Get("Location"); loc != "/v1/collections/"+accepted.ID {
		t.Fatalf("Location %q, want /v1/collections/%s", loc, accepted.ID)
	}
	return accepted.ID
}

// getCollection reads one record and fails unless the gateway answers 200.
func getCollection(t *testing.T, c *http.Client, id string) (collectionRecord, []byte) {
	t.Helper()
	resp := do(t, c, http.MethodGet, collectionURL(id, ""), "", nil)
	if resp.Status != http.StatusOK {
		t.Fatalf("GET collection %s: status %d: %s", id, resp.Status, resp.Body)
	}
	var rec collectionRecord
	if err := json.Unmarshal(resp.Body, &rec); err != nil {
		t.Fatalf("GET collection %s: body %q: %v", id, resp.Body, err)
	}
	return rec, resp.Body
}

// waitCollection polls until the record satisfies done, and fails with the last
// record it saw when the deadline passes.
func waitCollection(
	t *testing.T, c *http.Client, id string, within time.Duration, what string, done func(collectionRecord) bool,
) collectionRecord {
	t.Helper()
	var last collectionRecord
	err := poll(t.Context(), within, func(_ context.Context) (bool, error) {
		last, _ = getCollection(t, c, id)
		return done(last), nil
	})
	if err != nil {
		t.Fatalf("collection %s never %s: %v (state %q, attempt %d, reason %q, progress %+v)",
			id, what, err, last.State, last.Attempt, last.Reason, last.Progress)
	}
	return last
}

// terminal reports whether a state is one a Collection never leaves.
func terminal(state string) bool {
	switch state {
	case "completed", "failed", "cancelled", "expired":
		return true
	}
	return false
}

// waitCompleted waits for a Collection to complete and fails on any other
// terminal state, naming the reason the gateway stored.
func waitCompleted(t *testing.T, c *http.Client, id string) collectionRecord {
	t.Helper()
	rec := waitCollection(t, c, id, collectionDeadline, "reached a terminal state", func(r collectionRecord) bool {
		return terminal(r.State)
	})
	if rec.State != "completed" {
		t.Fatalf("collection %s ended %s (%s), want completed", id, rec.State, rec.Reason)
	}
	return rec
}

// download fetches the merged profile of a completed Collection.
func download(t *testing.T, c *http.Client, id string) []byte {
	t.Helper()
	resp := do(t, c, http.MethodGet, collectionURL(id, "/profile"), "", nil)
	if resp.Status != http.StatusOK {
		t.Fatalf("GET profile %s: status %d: %s", id, resp.Status, resp.Body)
	}
	if got := resp.Header.Get("X-Pprof-Collection"); got != id {
		t.Fatalf("X-Pprof-Collection %q, want %s", got, id)
	}
	want := fmt.Sprintf("attachment; filename=%q", id+".pprof")
	if got := resp.Header.Get("Content-Disposition"); got != want {
		t.Fatalf("Content-Disposition %q, want %q", got, want)
	}
	return resp.Body
}

// sampleResults counts the manifest's ok samples and the distinct Pod UIDs they
// name, which is how a scenario states "one sample per Pod per round".
func sampleResults(t *testing.T, rec collectionRecord) (ok int, uids []string) {
	t.Helper()
	if rec.Manifest == nil {
		t.Fatalf("collection %s completed with no manifest", rec.ID)
	}
	seen := map[string]bool{}
	for _, s := range rec.Manifest.Samples {
		if s.Result != "ok" {
			continue
		}
		ok++
		if !seen[s.PodUID] {
			seen[s.PodUID] = true
			uids = append(uids, s.PodUID)
		}
	}
	slices.Sort(uids)
	return ok, uids
}

// deployTestAppScaled applies the test app into ns at the replica count given,
// waits for its rollout, and waits until both gateways list every Pod.
func deployTestAppScaled(t *testing.T, h *Harness, ns string, replicas int32) []corev1.Pod {
	t.Helper()
	ctx := t.Context()
	if err := h.kubectl(ctx, "apply", "-n", ns, "-f", testAppManifest); err != nil {
		t.Fatal(err)
	}
	// A merge patch rather than a read and a write of the scale subresource:
	// the Deployment the apply just created is being written by its controllers,
	// and a write that carries the version this test read loses that race.
	body := fmt.Appendf(nil, `{"spec":{"replicas":%d}}`, replicas)
	_, err := h.Client.AppsV1().Deployments(ns).Patch(ctx, testAppName, types.MergePatchType, body, metav1.PatchOptions{})
	if err != nil {
		t.Fatalf("scale %s to %d: %v", testAppName, replicas, err)
	}
	if err := h.kubectl(ctx, "rollout", "status", "deployment/"+testAppName, "-n", ns, "--timeout="+podTimeout.String()); err != nil {
		t.Fatal(err)
	}
	pods := readyPods(t, h, ns, testAppLabel+"="+testAppName)
	if len(pods) != int(replicas) {
		t.Fatalf("%d ready test-app pods, want %d", len(pods), replicas)
	}
	waitTargets(t, h, ns, testAppName, podNames(pods))
	return pods
}

// scenarioPGOOnDemand proves an on-demand Collection runs end to end:
// it completes, the artifact parses, the manifest holds one ok sample per Pod
// per round over three distinct Pod UIDs, and both replicas answer with the
// same record and the same bytes.
func scenarioPGOOnDemand(t *testing.T, h *Harness) {
	ns := h.Namespace(t)
	h.PurgeStores(t)
	deployTestAppScaled(t, h, ns, 3)
	waitPGOReady(t, h, ns, testAppName)

	id := createCollection(t, h.Gateways[0], ns, testAppName,
		`{"sampling":{"duration":"2s","rounds":2,"roundInterval":"1s","replicas":"all"}}`)
	rec := waitCompleted(t, h.Gateways[0], id)

	ok, uids := sampleResults(t, rec)
	if ok != 6 {
		t.Fatalf("manifest holds %d ok samples, want 6 (three Pods over two rounds): %+v", ok, rec.Manifest.Samples)
	}
	if len(uids) != 3 {
		t.Fatalf("ok samples name %d distinct Pod UIDs, want 3: %v", len(uids), uids)
	}
	if rec.Artifact == nil || rec.Artifact.Bytes <= 0 {
		t.Fatalf("completed record names no artifact: %+v", rec.Artifact)
	}

	// Both replicas read the same durable record and stream the same object.
	_, first := getCollection(t, h.Gateways[0], id)
	_, second := getCollection(t, h.Gateways[1], id)
	if !bytes.Equal(first, second) {
		t.Fatalf("replicas disagree on the record:\n%s\n%s", first, second)
	}
	bodies := [gatewayReplicas][]byte{}
	for i, c := range h.Gateways {
		bodies[i] = download(t, c, id)
		if _, err := profile.ParseData(bodies[i]); err != nil {
			t.Fatalf("gateway %d: the merged profile does not parse: %v", i, err)
		}
	}
	if !bytes.Equal(bodies[0], bodies[1]) {
		t.Fatalf("replicas served %d and %d bytes for the same artifact", len(bodies[0]), len(bodies[1]))
	}
	if int64(len(bodies[0])) != rec.Artifact.Bytes {
		t.Fatalf("the artifact is %d bytes, the record says %d", len(bodies[0]), rec.Artifact.Bytes)
	}
}

// scenarioPGOScheduledSlot proves two gateways contending on one slot create
// exactly one Collection for it.
// The harness reads PROFGATE_JOBS itself, because the count of schedule keys is
// the fact the API cannot show.
func scenarioPGOScheduledSlot(t *testing.T, h *Harness) {
	ns := h.Namespace(t)
	h.PurgeStores(t)
	deployTestAppScaled(t, h, ns, 2)
	waitPGOReady(t, h, ns, testAppName)

	body := `{"enabled":true,"schedule":{"every":"1m","jitter":"0s"},` +
		`"sampling":{"duration":"1s","rounds":1,"roundInterval":"0s","replicas":1}}`
	resp := do(t, h.Gateways[0], http.MethodPut, pgoURL(ns, testAppName), body, nil)
	if resp.Status != http.StatusCreated {
		t.Fatalf("PUT pgo: status %d: %s", resp.Status, resp.Body)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" {
		t.Fatal("PUT pgo returned no ETag")
	}
	// The override outlives the namespace unless it is deleted: every minute it
	// would manufacture a Collection for a Service that no longer exists.
	t.Cleanup(func() {
		del := doCtx(context.Background(), t, h.Gateways[0], http.MethodDelete, pgoURL(ns, testAppName), "",
			http.Header{"If-Match": []string{etag}})
		if del.Status != http.StatusNoContent {
			t.Errorf("DELETE pgo: status %d: %s", del.Status, del.Body)
		}
	})

	// One schedule key for the slot, and one Collection under it.
	prefix := "schedule." + ns + "." + testAppName + "."
	var slots []string
	var mine []collectionRecord
	err := poll(t.Context(), slotDeadline, func(ctx context.Context) (bool, error) {
		keys, err := h.NATS.keys(ctx, jobsBucket)
		if err != nil {
			return false, err
		}
		slots = slots[:0]
		for _, k := range keys {
			if strings.HasPrefix(k, prefix) {
				slots = append(slots, k)
			}
		}
		mine, err = h.NATS.recordsOf(ctx, ns, testAppName)
		if err != nil {
			return false, err
		}
		return len(slots) > 0 && len(mine) > 0, nil
	})
	if err != nil {
		t.Fatalf("no slot fired for %s/%s within %s: %v", ns, testAppName, slotDeadline, err)
	}
	if len(slots) != 1 {
		t.Fatalf("%d schedule keys for one slot, want 1: %v", len(slots), slots)
	}
	if len(mine) != 1 {
		t.Fatalf("%d Collections for one slot, want 1: %+v", len(mine), mine)
	}
	rec := mine[0]
	if rec.Origin != "schedule" {
		t.Fatalf("origin %q, want schedule", rec.Origin)
	}
	if rec.Slot == "" {
		t.Fatal("a scheduled Collection carries no slot")
	}
	if got := strings.TrimPrefix(slots[0], prefix); got == "" {
		t.Fatalf("schedule key %q carries no slot", slots[0])
	}
	waitCompleted(t, h.Gateways[0], rec.ID)
}

// scenarioPGOCancel proves a Collection cancelled mid-flight ends cancelled and
// leaves no object behind.
func scenarioPGOCancel(t *testing.T, h *Harness) {
	ns := h.Namespace(t)
	h.PurgeStores(t)
	deployTestAppScaled(t, h, ns, 2)
	waitPGOReady(t, h, ns, testAppName)

	id := createCollection(t, h.Gateways[0], ns, testAppName,
		`{"sampling":{"duration":"2s","rounds":3,"roundInterval":"20s","replicas":"all"}}`)
	// The owner reports progress on each renewal, so a round that has ended is
	// visible before the next one starts.
	waitCollection(t, h.Gateways[0], id, collectionDeadline, "finished its first round", func(r collectionRecord) bool {
		return r.Progress.SamplesOK > 0
	})

	resp := do(t, h.Gateways[1], http.MethodPost, collectionURL(id, "/cancel"), "", jsonHeaders())
	if resp.Status != http.StatusOK {
		t.Fatalf("POST cancel: status %d: %s", resp.Status, resp.Body)
	}
	rec := waitCollection(t, h.Gateways[0], id, collectionDeadline, "became terminal", func(r collectionRecord) bool {
		return terminal(r.State)
	})
	if rec.State != "cancelled" {
		t.Fatalf("collection %s ended %s (%s), want cancelled", id, rec.State, rec.Reason)
	}
	if rec.Artifact != nil {
		t.Fatalf("a cancelled Collection names an artifact: %+v", rec.Artifact)
	}
	names, err := h.NATS.objects(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if strings.HasPrefix(name, id+"-") {
			t.Fatalf("a cancelled Collection left %s in %s", name, artifactsBucket)
		}
	}
	expectCode(t, "GET profile of a cancelled collection",
		do(t, h.Gateways[0], http.MethodGet, collectionURL(id, "/profile"), "", nil),
		http.StatusConflict, "collection_not_completed")
}

// scenarioPGOVersionConflict proves two versions behind one Service are refused
// until the request pins one, and that a pinned Collection samples only Pods of
// that version.
func scenarioPGOVersionConflict(t *testing.T, h *Harness) {
	ns := h.Namespace(t)
	h.PurgeStores(t)
	const app = "versioned"
	createDeployment(t, h, testAppDeployment("app-v1", ns, app, "1.0.0"))
	createDeployment(t, h, testAppDeployment("app-v2", ns, app, "2.0.0"))
	createService(t, h, testAppService(app, ns, app, false))
	v1 := waitDeploymentReady(t, h, ns, "app-v1")
	v2 := waitDeploymentReady(t, h, ns, "app-v2")
	waitTargets(t, h, ns, app, append(podNames(v1), podNames(v2)...))
	waitPGOReady(t, h, ns, app)

	expectCode(t, "POST collections over two versions",
		do(t, h.Gateways[0], http.MethodPost, collectionsURL(ns, app),
			`{"sampling":{"duration":"2s","rounds":1,"roundInterval":"0s","replicas":"all"}}`, jsonHeaders()),
		http.StatusConflict, "version_conflict")

	id := createCollection(t, h.Gateways[0], ns, app,
		`{"sampling":{"duration":"2s","rounds":1,"roundInterval":"0s","replicas":"all"},"target":{"version":"2.0.0"}}`)
	rec := waitCompleted(t, h.Gateways[0], id)
	if rec.ResolvedVersion != "2.0.0" {
		t.Fatalf("resolvedVersion %q, want 2.0.0", rec.ResolvedVersion)
	}
	ok, _ := sampleResults(t, rec)
	if ok != 1 {
		t.Fatalf("manifest holds %d ok samples, want 1 (the pinned Deployment's only Pod): %+v", ok, rec.Manifest.Samples)
	}
	wanted := podNames(v2)
	for _, s := range rec.Manifest.Samples {
		if !slices.Contains(wanted, s.Pod) {
			t.Fatalf("sample names %s, which is not a Pod of the pinned version %v", s.Pod, wanted)
		}
	}
}

// scenarioPGOReclaim proves a Collection whose owner disappears is reclaimed by
// the other replica and finishes.
func scenarioPGOReclaim(t *testing.T, h *Harness) {
	ns := h.Namespace(t)
	h.PurgeStores(t)
	deployTestAppScaled(t, h, ns, 2)
	waitPGOReady(t, h, ns, testAppName)

	id := createCollection(t, h.Gateways[0], ns, testAppName,
		`{"sampling":{"duration":"3s","rounds":3,"roundInterval":"15s","replicas":"all"}}`)
	claimed := waitCollection(t, h.Gateways[0], id, collectionDeadline, "was claimed", func(r collectionRecord) bool {
		return r.State == "running" && r.Owner != nil && r.LeaseUntil != nil
	})
	owner := claimed.Owner.Pod
	first := *claimed.LeaseUntil
	// The lease is renewed every third of its life; killing the owner before it
	// has renewed once would prove nothing about recovering a live lease.
	waitCollection(t, h.Gateways[0], id, collectionDeadline, "renewed its lease once", func(r collectionRecord) bool {
		return r.LeaseUntil != nil && r.LeaseUntil.After(first)
	})

	survivor := survivingGateway(t, h, owner)
	// The owner is meant to die between renewals, which is what leaves a live
	// lease for the other replica to time out.
	// Its container is stopped before its Pod is deleted, because a deletion on
	// its own only asks the gateway to drain, and a draining owner keeps
	// sampling: on a lane whose kubelet lets the drain run its course, the owner
	// reaches its next round with a Pod that no longer exists, every
	// confirmation fails, and it ends the Collection itself.
	h.CrashGateway(t, owner)
	if err := h.Client.CoreV1().Pods(gatewayNamespace).Delete(t.Context(), owner,
		metav1.DeleteOptions{GracePeriodSeconds: ptr(int64(0))}); err != nil {
		t.Fatalf("delete owner pod %s: %v", owner, err)
	}
	t.Cleanup(func() { h.RefreshGateways(t) })

	rec := waitCollection(t, survivor, id, reclaimDeadline, "was reclaimed and finished", func(r collectionRecord) bool {
		return terminal(r.State)
	})
	if rec.State != "completed" {
		t.Fatalf("reclaimed collection ended %s (%s) at attempt %d, want completed: %+v",
			rec.State, rec.Reason, rec.Attempt, rec.Manifest)
	}
	if rec.Attempt != 2 {
		t.Fatalf("attempt %d, want 2: the first claim died and the second finished", rec.Attempt)
	}
	if rec.Owner == nil || rec.Owner.Pod == owner {
		t.Fatalf("owner %+v, want a replica other than %s", rec.Owner, owner)
	}
}

// survivingGateway returns the client of the gateway that is not the named Pod.
// forwardGateways assigns the clients in the Pods' sorted order, so the index of
// the name in that order is the index of its client.
func survivingGateway(t *testing.T, h *Harness, pod string) *http.Client {
	t.Helper()
	pods, err := h.gatewayPods(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range pods {
		if p.Name != pod {
			return h.Gateways[i]
		}
	}
	t.Fatalf("no gateway other than %s", pod)
	return nil
}

// scenarioPGORealmFlags proves the realm's PGO flags gate the routes:
// a realm without configure cannot write policy, and one without read cannot
// see a record that exists.
func scenarioPGORealmFlags(t *testing.T, h *Harness) {
	ns := h.Namespace(t)
	h.PurgeStores(t)
	deployTestAppScaled(t, h, ns, 2)
	waitPGOReady(t, h, ns, testAppName)

	// A record the restricted realm may not see, made by a realm that may.
	id := createCollection(t, h.Gateways[0], ns, testAppName,
		`{"sampling":{"duration":"1s","rounds":1,"roundInterval":"0s","replicas":1}}`)
	waitCollection(t, h.Gateways[0], id, collectionDeadline, "became terminal", func(r collectionRecord) bool {
		return terminal(r.State)
	})

	// The same gateway with PGO on and a realm that carries no pgo block, which
	// leaves every flag false.
	c := deployOwnGateway(t, h, ns, gatewayConfig(gatewayConfigOptions{NATSURL: natsURL(gatewayNamespace)}))
	expectCode(t, "PUT pgo without configure",
		do(t, c, http.MethodPut, pgoURL(ns, testAppName), `{"enabled":true}`, nil),
		http.StatusForbidden, "realm_denied")
	expectCode(t, "GET an existing collection without read",
		do(t, c, http.MethodGet, collectionURL(id, ""), "", nil),
		http.StatusNotFound, "collection_not_found")
}

// scenarioPGODisabled proves a gateway with PGO off answers 501 on every PGO
// route and opens no NATS connection.
// The count is read against a baseline rather than against zero, because the
// suite's shared gateways stay connected throughout.
func scenarioPGODisabled(t *testing.T, h *Harness) {
	ns := h.Namespace(t)
	ctx := t.Context()
	baseline, err := h.NATS.connections(ctx)
	if err != nil {
		t.Fatalf("baseline connection count: %v", err)
	}
	t.Logf("NATS connections before the gateway with PGO disabled starts: %d", baseline)

	c := deployOwnGateway(t, h, ns, gatewayConfig(gatewayConfigOptions{}))

	id := strings.Repeat("0", collectionIDLength)
	routes := []struct{ method, url string }{
		{http.MethodGet, pgoURL(ns, testAppName)},
		{http.MethodPut, pgoURL(ns, testAppName)},
		{http.MethodDelete, pgoURL(ns, testAppName)},
		{http.MethodGet, collectionsURL(ns, testAppName)},
		{http.MethodPost, collectionsURL(ns, testAppName)},
		{http.MethodGet, collectionURL(id, "")},
		{http.MethodGet, collectionURL(id, "/profile")},
		{http.MethodPost, collectionURL(id, "/cancel")},
	}
	for _, r := range routes {
		// A POST declares the media type the two write routes require,
		// which is checked before this answer,
		// so every row here reaches the PGO step.
		var header http.Header
		if r.method == http.MethodPost {
			header = jsonHeaders()
		}
		expectCode(t, r.method+" "+r.url, do(t, c, r.method, r.url, "", header), http.StatusNotImplemented, "pgo_disabled")
	}

	// Every route has been answered; whatever the gateway would link, it would
	// have linked by now.
	deadline := time.Now().Add(disabledWatch)
	for time.Now().Before(deadline) {
		now, err := h.NATS.connections(ctx)
		if err != nil {
			t.Fatalf("connection count: %v", err)
		}
		if now > baseline {
			t.Fatalf("NATS connections rose to %d from a baseline of %d while a gateway with PGO disabled ran", now, baseline)
		}
		time.Sleep(pollInterval)
	}
}

// scenarioPGOClusterRole proves PGO needed no Kubernetes permission:
// the ClusterRole the running gateways are bound to is the shipped one, and a
// PGO-enabled gateway missing a verb still exits naming the denied tuple.
func scenarioPGOClusterRole(t *testing.T, h *Harness) {
	ctx := t.Context()
	live, err := h.Client.RbacV1().ClusterRoles().Get(ctx, gatewayDeployment, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read the gateway ClusterRole: %v", err)
	}
	shipped := shippedClusterRole(t, h)
	if fmt.Sprint(live.Rules) != fmt.Sprint(shipped.Rules) {
		t.Fatalf("the ClusterRole in the cluster differs from deploy/base/clusterrole.yaml:\n%v\n%v", live.Rules, shipped.Rules)
	}
	if _, err := h.gatewayPods(ctx); err != nil {
		t.Fatalf("gateways with PGO enabled do not run on the shipped ClusterRole: %v", err)
	}

	cfg := gatewayConfig(gatewayConfigOptions{NATSURL: natsURL(gatewayNamespace), RealmPGO: true})
	variants := []struct{ overlay, name, verb string }{
		{"reduced-no-watch", "profgate-no-watch", "watch"},
		{"reduced-no-get", "profgate-no-get", "get"},
	}
	for _, v := range variants {
		h.Apply(t, gatewayNamespace, v.overlay, configPatch(v.name, cfg), credsMountPatch(v.name))
		t.Cleanup(func() { deleteReducedGateway(t, h, v.name) })
	}
	for _, v := range variants {
		pod := waitCrashLoop(t, h, gatewayNamespace, testAppLabel+"="+v.name)
		logs := podLogs(t, h, gatewayNamespace, pod)
		verb := fmt.Sprintf(`"verb":%q`, v.verb)
		if !strings.Contains(logs, `"resource":"pods"`) || !strings.Contains(logs, verb) {
			t.Fatalf("%s logs lack the denied tuple with %s:\n%s", v.name, verb, logs)
		}
	}
}

// shippedClusterRole decodes deploy/base/clusterrole.yaml.
func shippedClusterRole(t *testing.T, h *Harness) *rbacv1.ClusterRole {
	t.Helper()
	path := filepath.Join(h.root, "deploy", "base", "clusterrole.yaml")
	f, err := os.Open(path) //nolint:gosec // the path is composed from the module root
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	var role rbacv1.ClusterRole
	if err := k8syaml.NewYAMLOrJSONDecoder(f, 4096).Decode(&role); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return &role
}

// scenarioPGOPreflightNegative proves NATS preflight refuses to start the
// gateway on a store the contract forbids or a user that cannot do what the
// gateway will need, and that it leaves no probe behind.
// It runs its own NATS server: it re-provisions a bucket, which no gateway
// watching one may see.
func scenarioPGOPreflightNegative(t *testing.T, h *Harness) {
	ns := h.Namespace(t)
	ctx := t.Context()
	srv, err := h.deployNATS(ctx, ns)
	if err != nil {
		t.Fatalf("deploy nats in %s: %v", ns, err)
	}
	t.Cleanup(srv.close)
	pub, sub, err := gatewayPermissions(h.root)
	if err != nil {
		t.Fatal(err)
	}

	// A bucket with a TTL: an expiring key would let a second Collection start
	// while the first still runs, so the gateway refuses the bucket outright.
	if err := srv.provisionStores(ctx, time.Minute); err != nil {
		t.Fatalf("provision with a TTL: %v", err)
	}
	full, err := srv.ID.user("profgate", pub, sub)
	if err != nil {
		t.Fatal(err)
	}
	logs := preflightFailure(t, h, ns, srv, full.Creds)
	if want := "bucket " + jobsBucket + ": field TTL"; !strings.Contains(logs, want) {
		t.Fatalf("the gateway did not name %q when %s carries a TTL:\n%s", want, jobsBucket, logs)
	}

	// The contract holds from here on; what is missing is permission.
	if err := srv.provisionStores(ctx, 0); err != nil {
		t.Fatalf("provision without a TTL: %v", err)
	}
	// Each user can open every bucket and is missing exactly one publish
	// permission, so the probe that needs it is the one that fails and the
	// message names the bucket, the probe, and the subject the server denied.
	denials := []struct {
		subject string
		want    []string
	}{
		{"$KV.PROFGATE_JOBS.>", []string{"bucket " + jobsBucket + ": probe create of probe.", "$KV.PROFGATE_JOBS."}},
		{"$O.PROFGATE_ARTIFACTS.>", []string{"bucket " + artifactsBucket + ": probe put of probe-", "$O.PROFGATE_ARTIFACTS."}},
		{
			"$JS.API.CONSUMER.CREATE.OBJ_PROFGATE_ARTIFACTS.>",
			[]string{"bucket " + artifactsBucket + ": probe ", "$JS.API.CONSUMER.CREATE.OBJ_PROFGATE_ARTIFACTS"},
		},
	}
	for _, d := range denials {
		t.Logf("preflight without publish on %s", d.subject)
		reduced, err := without(pub, d.subject)
		if err != nil {
			t.Fatal(err)
		}
		user, err := srv.ID.user("reduced", reduced, sub)
		if err != nil {
			t.Fatal(err)
		}
		logs := preflightFailure(t, h, ns, srv, user.Creds)
		for _, want := range d.want {
			if !strings.Contains(logs, want) {
				t.Fatalf("without publish on %s the gateway did not name %q:\n%s", d.subject, want, logs)
			}
		}
		assertNoProbes(t, srv, probeInstance(t, logs))
	}
}

// probeInstance returns the instance identifier the probe name in a failed
// preflight's log carries.
// Every run of the gateway names itself anew, so its probes are named anew too,
// and the store can only be judged against the attempt whose log was read:
// the crash-looping Deployment always has a later attempt in flight, whose
// probe is in the store on purpose while it works.
func probeInstance(t *testing.T, logs string) string {
	t.Helper()
	m := probeNameRe.FindStringSubmatch(logs)
	if m == nil {
		t.Fatalf("the preflight log names no probe:\n%s", logs)
	}
	return m[1]
}

// probeNameRe matches the probe key or object name in a preflight error;
// the name runs to the colon that starts the reason.
var probeNameRe = regexp.MustCompile(`probe[-.]([^\s":]+)`)

// preflightFailure starts a gateway against srv with the credentials given,
// waits for it to crash-loop, and returns the log of its last run.
func preflightFailure(t *testing.T, h *Harness, ns string, srv *natsServer, creds []byte) string {
	t.Helper()
	ctx := t.Context()
	deleteOwnGateway(t, h, ns)
	if err := h.applyCredsSecret(ctx, ns, creds); err != nil {
		t.Fatal(err)
	}
	h.Apply(t, ns, "pgo-gateway",
		configPatch(pgoGatewayName, gatewayConfig(gatewayConfigOptions{NATSURL: natsURL(srv.Namespace), RealmPGO: true})))
	t.Cleanup(func() { deleteOwnGatewayBinding(t, h) })
	pod := waitCrashLoop(t, h, ns, pgoGatewaySelector)
	return podLogs(t, h, ns, pod)
}

// assertNoProbes fails when the preflight of the named instance left a probe
// key or object behind in any of the three stores.
func assertNoProbes(t *testing.T, srv *natsServer, instance string) {
	t.Helper()
	ctx := t.Context()
	for _, bucket := range []string{configBucket, jobsBucket} {
		keys, err := srv.keys(ctx, bucket)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(keys, "probe."+instance) {
			t.Fatalf("a failed preflight left probe.%s in %s", instance, bucket)
		}
	}
	names, err := srv.objects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(names, "probe-"+instance) {
		t.Fatalf("a failed preflight left probe-%s in %s", instance, artifactsBucket)
	}
}

// deployOwnGateway applies the pgo-gateway overlay into ns with the given
// configuration, waits for its Pod, and returns a client that reaches its API
// listener.
// The credentials Secret is the shared server's, so a gateway that needs one
// finds it in its own namespace.
func deployOwnGateway(t *testing.T, h *Harness, ns, cfg string) *http.Client {
	t.Helper()
	ctx := t.Context()
	pub, sub, err := gatewayPermissions(h.root)
	if err != nil {
		t.Fatal(err)
	}
	user, err := h.NATS.ID.user("profgate", pub, sub)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.applyCredsSecret(ctx, ns, user.Creds); err != nil {
		t.Fatal(err)
	}
	h.Apply(t, ns, "pgo-gateway", configPatch(pgoGatewayName, cfg))
	t.Cleanup(func() { deleteOwnGatewayBinding(t, h) })
	if err := h.kubectl(ctx, "rollout", "status", "deployment/"+pgoGatewayName, "-n", ns,
		"--timeout="+rolloutTimeout.String()); err != nil {
		_ = h.kubectl(ctx, "logs", "-n", ns, "-l", pgoGatewaySelector, "--tail=50")
		t.Fatal(err)
	}
	pod, err := h.waitOnePod(ctx, ns, pgoGatewaySelector)
	if err != nil {
		t.Fatal(err)
	}
	ports, stop, err := h.forward(ctx, ns, pod, []string{"0:" + gatewayAPIPort})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	local := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(ports[0])))
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, local)
		},
	}}
}

// deleteOwnGateway removes a scenario's own gateway Deployment and waits for its
// Pods, so the next variant starts from nothing rather than reading the logs of
// the one before.
func deleteOwnGateway(t *testing.T, h *Harness, ns string) {
	t.Helper()
	ctx := t.Context()
	opts := metav1.DeleteOptions{PropagationPolicy: ptr(metav1.DeletePropagationBackground)}
	err := h.Client.AppsV1().Deployments(ns).Delete(ctx, pgoGatewayName, opts)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("delete deployment %s: %v", pgoGatewayName, err)
	}
	err = poll(ctx, podTimeout, func(ctx context.Context) (bool, error) {
		list, err := h.Client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: pgoGatewaySelector})
		if err != nil {
			return false, err
		}
		return len(list.Items) == 0, nil
	})
	if err != nil {
		t.Fatalf("pods of %s are still present: %v", pgoGatewayName, err)
	}
}

// deleteOwnGatewayBinding removes the one cluster-scoped object the pgo-gateway
// overlay creates; the namespace takes the rest.
func deleteOwnGatewayBinding(t *testing.T, h *Harness) {
	t.Helper()
	err := h.Client.RbacV1().ClusterRoleBindings().Delete(context.Background(), pgoGatewayName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Errorf("delete clusterrolebinding %s: %v", pgoGatewayName, err)
	}
}
