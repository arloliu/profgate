//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// natsIdentity is the operator, the system account, and the PROFGATE account
// the harness mints for one NATS server.
// Users are signed by the account key, so a scenario can mint one with reduced
// permissions against a server that is already running:
// the memory resolver holds the account, and every user the account signs is
// accepted without touching the server's configuration.
type natsIdentity struct {
	operatorJWT string
	sysPub      string
	sysJWT      string
	accountPub  string
	accountJWT  string
	accountKey  nkeys.KeyPair
}

// newNATSIdentity mints one run's operator, system account, and PROFGATE
// account, with JetStream unlimited on the account that owns the stores.
func newNATSIdentity() (*natsIdentity, error) {
	operatorKey, err := nkeys.CreateOperator()
	if err != nil {
		return nil, fmt.Errorf("operator key: %w", err)
	}
	operatorPub, err := operatorKey.PublicKey()
	if err != nil {
		return nil, fmt.Errorf("operator key: %w", err)
	}
	sysKey, err := nkeys.CreateAccount()
	if err != nil {
		return nil, fmt.Errorf("system account key: %w", err)
	}
	sysPub, err := sysKey.PublicKey()
	if err != nil {
		return nil, fmt.Errorf("system account key: %w", err)
	}
	accountKey, err := nkeys.CreateAccount()
	if err != nil {
		return nil, fmt.Errorf("account key: %w", err)
	}
	accountPub, err := accountKey.PublicKey()
	if err != nil {
		return nil, fmt.Errorf("account key: %w", err)
	}

	sysClaims := jwt.NewAccountClaims(sysPub)
	sysClaims.Name = "SYS"
	sysJWT, err := sysClaims.Encode(operatorKey)
	if err != nil {
		return nil, fmt.Errorf("system account jwt: %w", err)
	}
	accountClaims := jwt.NewAccountClaims(accountPub)
	accountClaims.Name = "PROFGATE"
	// Without an explicit grant JetStream is off for the account and every
	// store operation fails as if the buckets did not exist.
	accountClaims.Limits.DiskStorage = -1
	accountClaims.Limits.MemoryStorage = -1
	accountClaims.Limits.Streams = -1
	accountJWT, err := accountClaims.Encode(operatorKey)
	if err != nil {
		return nil, fmt.Errorf("account jwt: %w", err)
	}
	operatorClaims := jwt.NewOperatorClaims(operatorPub)
	operatorClaims.Name = "profgate-e2e"
	operatorClaims.SystemAccount = sysPub
	operatorJWT, err := operatorClaims.Encode(operatorKey)
	if err != nil {
		return nil, fmt.Errorf("operator jwt: %w", err)
	}

	return &natsIdentity{
		operatorJWT: operatorJWT,
		sysPub:      sysPub,
		sysJWT:      sysJWT,
		accountPub:  accountPub,
		accountJWT:  accountJWT,
		accountKey:  accountKey,
	}, nil
}

// serverConf is the nats-server configuration: who signs users, which account
// is the system account, and the two account JWTs, resolved from memory.
// JetStream, its store directory, and the monitoring listener are arguments in
// nats.yaml, so this file carries authentication and nothing else.
func (id *natsIdentity) serverConf() string {
	return fmt.Sprintf(`operator: %q
system_account: %q
resolver: MEMORY
resolver_preload: {
  %s: %q
  %s: %q
}
`, id.operatorJWT, id.sysPub, id.sysPub, id.sysJWT, id.accountPub, id.accountJWT)
}

// natsUser is one minted user: the credentials file a gateway mounts and the
// JWT and seed the harness's own connections authenticate with directly.
type natsUser struct {
	Creds []byte
	jwt   string
	seed  string
}

// user mints a user of the PROFGATE account with exactly the permissions given.
func (id *natsIdentity) user(name string, pub, sub []string) (natsUser, error) {
	key, err := nkeys.CreateUser()
	if err != nil {
		return natsUser{}, fmt.Errorf("user key %s: %w", name, err)
	}
	userPub, err := key.PublicKey()
	if err != nil {
		return natsUser{}, fmt.Errorf("user key %s: %w", name, err)
	}
	seed, err := key.Seed()
	if err != nil {
		return natsUser{}, fmt.Errorf("user seed %s: %w", name, err)
	}
	claims := jwt.NewUserClaims(userPub)
	claims.Name = name
	claims.Pub.Allow = pub
	claims.Sub.Allow = sub
	userJWT, err := claims.Encode(id.accountKey)
	if err != nil {
		return natsUser{}, fmt.Errorf("user jwt %s: %w", name, err)
	}
	creds, err := jwt.FormatUserConfig(userJWT, seed)
	if err != nil {
		return natsUser{}, fmt.Errorf("user credentials %s: %w", name, err)
	}
	return natsUser{Creds: creds, jwt: userJWT, seed: string(seed)}, nil
}

// gatewayPermissions returns the publish and subscribe subjects
// deploy/nats/account.conf grants.
// The end-to-end gateway user is exactly the set the repository ships, so a
// subject the gateway needs and the fragment omits fails the suite instead of
// passing against a copy of the list that has drifted.
func gatewayPermissions(root string) (pub, sub []string, err error) {
	path := filepath.Join(root, "deploy", "nats", "account.conf")
	b, err := os.ReadFile(path) //nolint:gosec // the path is composed from the module root
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	var into *[]string
	for _, line := range strings.Split(string(b), "\n") {
		text := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(text, "publish:"):
			into = &pub
		case strings.HasPrefix(text, "subscribe:"):
			into = &sub
		case strings.HasPrefix(text, `"`) && strings.HasSuffix(text, `"`) && into != nil:
			*into = append(*into, strings.Trim(text, `"`))
		}
	}
	if len(pub) == 0 || len(sub) == 0 {
		return nil, nil, fmt.Errorf("%s granted %d publish and %d subscribe subjects", path, len(pub), len(sub))
	}
	return pub, sub, nil
}

// without returns subjects with one entry removed, and fails when the entry was
// not there: a reduced user must be reduced by something the full set granted.
func without(subjects []string, drop string) ([]string, error) {
	out := make([]string, 0, len(subjects))
	for _, s := range subjects {
		if s != drop {
			out = append(out, s)
		}
	}
	if len(out) == len(subjects) {
		return nil, fmt.Errorf("deploy/nats/account.conf does not grant %q, so removing it reduces nothing", drop)
	}
	return out, nil
}

// natsServer is a running NATS Deployment: the identity its users are minted
// from, an administrative connection through a standing port-forward, and the
// monitoring endpoint the connection count is read from.
type natsServer struct {
	Namespace string
	ID        *natsIdentity

	conn    *nats.Conn
	js      jetstream.JetStream
	monitor string // http://127.0.0.1:<local port>
	stop    func()
}

// natsURL is the in-cluster address of the server in ns.
func natsURL(ns string) string {
	return "nats://" + natsName + "." + ns + ".svc:" + natsClientPort
}

// deployNATS writes the server configuration, applies nats.yaml into ns, waits
// for the rollout, and opens the administrative connection through a
// port-forward.
// A Deployment that already existed is restarted: every run mints a new
// account, and a Pod still holding the previous one would reject every
// connection this run makes.
func (h *Harness) deployNATS(ctx context.Context, ns string) (*natsServer, error) {
	id, err := newNATSIdentity()
	if err != nil {
		return nil, err
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: natsConfigMap, Namespace: ns},
		Data:       map[string]string{"nats.conf": id.serverConf()},
	}
	if err := h.applyConfigMap(ctx, ns, cm); err != nil {
		return nil, err
	}
	_, err = h.Client.AppsV1().Deployments(ns).Get(ctx, natsName, metav1.GetOptions{})
	existed := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get deployment %s: %w", natsName, err)
	}
	if err := h.kubectl(ctx, "apply", "-n", ns, "-f", natsManifest); err != nil {
		return nil, err
	}
	if existed {
		if err := h.kubectl(ctx, "rollout", "restart", "deployment/"+natsName, "-n", ns); err != nil {
			return nil, err
		}
	}
	if err := h.kubectl(ctx, "rollout", "status", "deployment/"+natsName, "-n", ns,
		"--timeout="+rolloutTimeout.String()); err != nil {
		return nil, err
	}
	pod, err := h.waitOnePod(ctx, ns, "app.kubernetes.io/name="+natsName)
	if err != nil {
		return nil, err
	}
	ports, stop, err := h.forward(ctx, ns, pod, []string{"0:" + natsClientPort, "0:" + natsMonitorPort})
	if err != nil {
		return nil, fmt.Errorf("port-forward %s: %w", pod, err)
	}
	admin, err := id.user("harness", []string{">"}, []string{">"})
	if err != nil {
		stop()
		return nil, err
	}
	conn, err := nats.Connect("nats://"+net.JoinHostPort("127.0.0.1", strconv.Itoa(int(ports[0]))),
		nats.UserJWTAndSeed(admin.jwt, admin.seed), nats.Name("profgate-e2e-harness"))
	if err != nil {
		stop()
		return nil, fmt.Errorf("connect to nats in %s: %w", ns, err)
	}
	js, err := jetstream.New(conn)
	if err != nil {
		conn.Close()
		stop()
		return nil, fmt.Errorf("jetstream in %s: %w", ns, err)
	}
	h.log.Info("nats", "namespace", ns, "pod", pod, "client", ports[0], "monitor", ports[1])

	return &natsServer{
		Namespace: ns,
		ID:        id,
		conn:      conn,
		js:        js,
		monitor:   "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(int(ports[1]))),
		stop:      stop,
	}, nil
}

// close ends the administrative connection and its port-forward.
func (s *natsServer) close() {
	s.conn.Close()
	s.stop()
}

// provisionStores creates the three stores with the configuration of the bucket
// contract: file storage, no TTL, and no size ceiling.
// jobsTTL is zero for every caller but the preflight scenario, which provisions
// a bucket the contract forbids on purpose.
func (s *natsServer) provisionStores(ctx context.Context, jobsTTL time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, natsDeadline)
	defer cancel()
	buckets := []struct {
		name string
		ttl  time.Duration
	}{{configBucket, 0}, {jobsBucket, jobsTTL}}
	for _, b := range buckets {
		_, err := s.js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
			Bucket:       b.name,
			Storage:      jetstream.FileStorage,
			History:      1,
			TTL:          b.ttl,
			MaxBytes:     -1,
			MaxValueSize: -1,
		})
		if err != nil {
			return fmt.Errorf("provision %s: %w", b.name, err)
		}
	}
	_, err := s.js.CreateOrUpdateObjectStore(ctx, jetstream.ObjectStoreConfig{
		Bucket:   artifactsBucket,
		Storage:  jetstream.FileStorage,
		TTL:      0,
		MaxBytes: -1,
	})
	if err != nil {
		return fmt.Errorf("provision %s: %w", artifactsBucket, err)
	}
	return nil
}

// purgeStores removes every key and object so the next scenario starts empty.
// The buckets themselves are left alone: the gateways' watches and consumers
// belong to the streams that exist now, and a recreated stream would leave them
// watching nothing.
func (s *natsServer) purgeStores(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, natsDeadline)
	defer cancel()
	for _, bucket := range []string{configBucket, jobsBucket} {
		kv, err := s.js.KeyValue(ctx, bucket)
		if err != nil {
			return fmt.Errorf("open %s: %w", bucket, err)
		}
		keys, err := kv.Keys(ctx)
		if err != nil && !errors.Is(err, jetstream.ErrNoKeysFound) {
			return fmt.Errorf("list %s: %w", bucket, err)
		}
		for _, key := range keys {
			if err := kv.Purge(ctx, key); err != nil {
				return fmt.Errorf("purge %s %s: %w", bucket, key, err)
			}
		}
	}
	obs, err := s.js.ObjectStore(ctx, artifactsBucket)
	if err != nil {
		return fmt.Errorf("open %s: %w", artifactsBucket, err)
	}
	infos, err := obs.List(ctx)
	if err != nil && !errors.Is(err, jetstream.ErrNoObjectsFound) {
		return fmt.Errorf("list %s: %w", artifactsBucket, err)
	}
	for _, info := range infos {
		if err := obs.Delete(ctx, info.Name); err != nil {
			return fmt.Errorf("delete %s %s: %w", artifactsBucket, info.Name, err)
		}
	}
	return nil
}

// keys lists the keys of one KV bucket, empty when it holds none.
func (s *natsServer) keys(ctx context.Context, bucket string) ([]string, error) {
	kv, err := s.js.KeyValue(ctx, bucket)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", bucket, err)
	}
	keys, err := kv.Keys(ctx)
	if err != nil && !errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil, fmt.Errorf("list %s: %w", bucket, err)
	}
	return keys, nil
}

// objects lists the names in the artifact store, empty when it holds none.
func (s *natsServer) objects(ctx context.Context) ([]string, error) {
	obs, err := s.js.ObjectStore(ctx, artifactsBucket)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", artifactsBucket, err)
	}
	infos, err := obs.List(ctx)
	if err != nil && !errors.Is(err, jetstream.ErrNoObjectsFound) {
		return nil, fmt.Errorf("list %s: %w", artifactsBucket, err)
	}
	names := make([]string, 0, len(infos))
	for _, info := range infos {
		names = append(names, info.Name)
	}
	sort.Strings(names)
	return names, nil
}

// recordsOf returns the Collection records of one Service, read from
// PROFGATE_JOBS directly.
// The API answers from a watched cache; a scenario that counts what the
// schedulers wrote reads the durable keys instead.
func (s *natsServer) recordsOf(ctx context.Context, ns, service string) ([]collectionRecord, error) {
	kv, err := s.js.KeyValue(ctx, jobsBucket)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", jobsBucket, err)
	}
	keys, err := kv.Keys(ctx)
	if err != nil && !errors.Is(err, jetstream.ErrNoKeysFound) {
		return nil, fmt.Errorf("list %s: %w", jobsBucket, err)
	}
	var out []collectionRecord
	for _, key := range keys {
		if !strings.HasPrefix(key, "job.") {
			continue
		}
		entry, err := kv.Get(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("read %s %s: %w", jobsBucket, key, err)
		}
		var rec collectionRecord
		if err := json.Unmarshal(entry.Value(), &rec); err != nil {
			return nil, fmt.Errorf("decode %s %s: %w", jobsBucket, key, err)
		}
		if rec.Namespace == ns && rec.Service == service {
			out = append(out, rec)
		}
	}
	return out, nil
}

// connections is the server's current client connection count, from the
// monitoring endpoint.
func (s *natsServer) connections(ctx context.Context) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.monitor+"/varz", nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("varz: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var varz struct {
		Connections int `json:"connections"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&varz); err != nil {
		return 0, fmt.Errorf("varz: %w", err)
	}
	return varz.Connections, nil
}
