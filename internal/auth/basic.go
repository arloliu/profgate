package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/arloliu/profgate/internal/config"
	"github.com/arloliu/profgate/internal/metrics"
)

const (
	// maxBasicHeaderBytes bounds the Authorization value before decoding.
	maxBasicHeaderBytes = 1024
	// maxPasswordBytes is where bcrypt stops reading; a longer password is
	// refused rather than silently truncated to one that equals another.
	maxPasswordBytes = 72
	// basicScheme is the RFC 7617 scheme token, compared case-insensitively.
	basicScheme = "Basic"
)

// passwordComparer checks a password against a bcrypt hash; production is
// bcrypt.CompareHashAndPassword and a test counts or blocks the calls.
type passwordComparer interface {
	compare(hash, password []byte) error
}

type bcryptComparer struct{}

func (bcryptComparer) compare(hash, password []byte) error {
	return bcrypt.CompareHashAndPassword(hash, password)
}

// BasicOptions is what Basic needs from the outside.
type BasicOptions struct {
	Logger   *slog.Logger
	Recorder metrics.Recorder
	// PollInterval replaces the 30-second users-file poll; zero keeps it.
	// Mirrors tlscert.Options.Interval.
	PollInterval time.Duration
}

// Basic authenticates Authorization: Basic against the configured user set.
// Inline users come from the configuration snapshot each request carries;
// file users and the dummy hash come from the set the poller last accepted.
type Basic struct {
	// inline and realms are the startup snapshot the poller validates each
	// file read against.
	inline []config.BasicUser
	realms map[string]config.Realm
	// set is the file users plus the dummy hash, swapped whole by the poller.
	set atomic.Pointer[userSet]
	// gate holds one token per comparison allowed to run at once.
	gate    chan struct{}
	compare passwordComparer
	poller  *filePoller // nil when no users file is configured
	log     *slog.Logger
}

// NewBasic builds the authenticator from the startup snapshot: the gate, the
// dummy hash at the cost the user set shares, and the users-file poller when
// auth.basic.usersFile is set.
// The configuration has already been validated, so the file is expected to
// load here; a file that does not is a startup failure.
// The poller reads it, so the bytes in effect at startup are the ones its
// first poll compares against and an unchanged file is not applied twice.
func NewBasic(cfg *config.Config, opts BasicOptions) (*Basic, error) {
	bc := cfg.Auth.Basic
	if bc == nil {
		return nil, errors.New("auth: basic mode needs auth.basic")
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Recorder == nil {
		opts.Recorder = metrics.Noop{}
	}
	b := &Basic{
		inline:  bc.Users,
		realms:  cfg.Realms,
		gate:    make(chan struct{}, bc.MaxConcurrent),
		compare: bcryptComparer{},
		log:     opts.Logger,
	}
	if bc.UsersFile != "" {
		b.poller = newFilePoller(bc.UsersFile, b.applyUsersFile, "users", opts.PollInterval, opts.Recorder, opts.Logger)
		if err := b.poller.load(); err != nil {
			return nil, fmt.Errorf("auth: usersFile: %w", err)
		}

		return b, nil
	}
	set, err := newUserSet(b.inline, nil, b.realms)
	if err != nil {
		return nil, err
	}
	b.set.Store(set)

	return b, nil
}

// Run polls the users file every PollInterval until ctx ends;
// it returns at once when no file is configured.
func (b *Basic) Run(ctx context.Context) {
	if b.poller == nil {
		return
	}
	b.poller.Run(ctx)
}

// Authenticate implements Authenticator.
// The checks run in the order that reads the least: the header's presence,
// its scheme, its size, its encoding, the password's length, and only then
// the gate and the one bcrypt comparison.
// Every rejection after decoding is bad_credential, whether the name is
// unknown or the password wrong, and both paths compare exactly once.
func (b *Basic) Authenticate(_ context.Context, r *http.Request, cfg *config.Config) (Principal, error) {
	if cfg.Auth.Basic == nil {
		return Principal{}, errors.New("auth: basic mode needs auth.basic")
	}
	name, password, err := parseBasic(r.Header.Get("Authorization"))
	if err != nil {
		return Principal{}, err
	}

	select {
	case b.gate <- struct{}{}:
	default:
		return Principal{}, &Failure{Status: http.StatusTooManyRequests, Reason: ReasonThrottled}
	}
	defer func() { <-b.gate }()

	set := b.set.Load()
	user, hash := lookup(name, cfg.Auth.Basic.Users, set)
	if err := b.compare.compare(hash, password); err != nil || user == nil {
		return Principal{}, &Failure{Status: http.StatusUnauthorized, Reason: ReasonBadCredential}
	}

	return Principal{Name: user.Name, Realm: user.Realm}, nil
}

// lookup finds name in the snapshot's inline users, then the file set, and
// returns the hash to compare against: the user's own, or the dummy hash when
// the name is unknown.
func lookup(name string, inline []config.BasicUser, set *userSet) (*config.BasicUser, []byte) {
	for i := range inline {
		if inline[i].Name == name {
			return &inline[i], []byte(inline[i].PasswordHash)
		}
	}
	if u, ok := set.users[name]; ok {
		return &u, []byte(u.PasswordHash)
	}

	return nil, set.dummy
}

// parseBasic reads name and password out of an Authorization header value.
// Every rejection is a Failure that names what was wrong with the shape,
// never what the value was.
func parseBasic(header string) (string, []byte, error) {
	if header == "" {
		return "", nil, &Failure{Status: http.StatusUnauthorized, Reason: ReasonMissing}
	}
	scheme, rest, _ := strings.Cut(header, " ")
	if !strings.EqualFold(scheme, basicScheme) {
		return "", nil, &Failure{Status: http.StatusUnauthorized, Reason: ReasonScheme}
	}
	malformed := &Failure{Status: http.StatusUnauthorized, Reason: ReasonMalformed}
	if len(header) > maxBasicHeaderBytes {
		return "", nil, malformed
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(rest))
	if err != nil {
		return "", nil, malformed
	}
	name, password, ok := bytes.Cut(decoded, []byte{':'})
	if !ok {
		return "", nil, malformed
	}
	if len(password) > maxPasswordBytes {
		return "", nil, malformed
	}

	return string(name), password, nil
}
