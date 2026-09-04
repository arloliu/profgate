package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/arloliu/profgate/internal/client"
)

// The bounds of the two wait flags.
const (
	defaultPollInterval = 2 * time.Second
	minPollInterval     = time.Second
	maxPollInterval     = time.Minute
	defaultWaitTimeout  = 30 * time.Minute
	minWaitTimeout      = time.Minute
	maxWaitTimeout      = 24 * time.Hour
)

// collectVerb is POST .../collections: the body from the field flags or
// --body, one idempotency key per invocation, and the identifier and state
// the answer carried, then under --wait the poll of the record until it
// ends.
func collectVerb() verb {
	var f collectFlags
	return verb{
		name: "collect",
		leaves: []leaf{{
			grammar:     "collect <ns>/<svc> [--duration <d>] [--rounds <n>] [--round-interval <d>] [--replicas all|<n>] [--max-parallel <n>] [--target-version <v>] [--retention <d>] [--body <path>] [--wait] [--poll-interval <d>] [--wait-timeout <d>]",
			positionals: 1,
			flags: func(fs *flag.FlagSet) {
				fs.StringVar(&f.duration, "duration", "", "how long each sample runs")
				fs.StringVar(&f.rounds, "rounds", "", "how many rounds to sample")
				fs.StringVar(&f.roundInterval, "round-interval", "", "the pause between rounds")
				fs.StringVar(&f.replicas, "replicas", "", "how many Pods each round samples: all or a count")
				fs.StringVar(&f.maxParallel, "max-parallel", "", "how many Pods are sampled at once")
				fs.StringVar(&f.targetVersion, "target-version", "", "the binary version to profile")
				fs.StringVar(&f.retention, "retention", "", "how long the merged profile is kept")
				fs.StringVar(&f.body, "body", "", "send this JSON file as the request body instead of the field flags")
				fs.BoolVar(&f.wait, "wait", false, "wait for the Collection to finish")
				fs.DurationVar(&f.pollInterval, "poll-interval", defaultPollInterval, "how often --wait reads the record, 1s to 1m")
				fs.DurationVar(&f.waitTimeout, "wait-timeout", defaultWaitTimeout, "how long --wait keeps reading, 1m to 24h")
			},
		}},
		run: func(ctx context.Context, env *cmdEnv, in *invocation) int {
			output, err := env.collect(ctx, in, f)
			if err != nil {
				return fail(env, output, err)
			}
			return exitOK
		},
	}
}

// collectFlags is what the collect verb's own flags said, each field flag
// as typed so a refusal can quote it.
type collectFlags struct {
	duration, rounds, roundInterval, replicas, maxParallel, targetVersion, retention, body string
	wait                                                                                   bool
	pollInterval, waitTimeout                                                              time.Duration
}

// checkWaitFlags refuses a poll interval or a wait timeout outside its range.
func (f collectFlags) checkWaitFlags() error {
	if f.pollInterval < minPollInterval || f.pollInterval > maxPollInterval {
		return fmt.Errorf("%w: --poll-interval %s is outside %s to %s", client.ErrUsage, f.pollInterval, minPollInterval, maxPollInterval)
	}
	if f.waitTimeout < minWaitTimeout || f.waitTimeout > maxWaitTimeout {
		return fmt.Errorf("%w: --wait-timeout %s is outside %s to %s", client.ErrUsage, f.waitTimeout, minWaitTimeout, maxWaitTimeout)
	}
	return nil
}

// fieldFlagSet reports whether any field flag is set, which --body excludes.
func (f collectFlags) fieldFlagSet() bool {
	for _, v := range []string{f.duration, f.rounds, f.roundInterval, f.replicas, f.maxParallel, f.targetVersion, f.retention} {
		if v != "" {
			return true
		}
	}
	return false
}

// requestBody is the create's body:
// the file under --body, or the field flags as the override shape of the Collection routes, each flag validated locally and a flag left unset absent,
// so the effective policy decides it.
func (f collectFlags) requestBody() ([]byte, error) {
	if f.body != "" {
		if f.fieldFlagSet() {
			return nil, fmt.Errorf("%w: --body sends a file as the whole request; pass it without the field flags", client.ErrUsage)
		}
		data, err := os.ReadFile(f.body)
		if err != nil {
			return nil, fmt.Errorf("%w: --body: %w", client.ErrUsage, err)
		}
		if !json.Valid(data) {
			return nil, fmt.Errorf("%w: --body %s is not a JSON document", client.ErrUsage, f.body)
		}
		return data, nil
	}
	body, err := f.fieldBody()
	if err != nil {
		return nil, err
	}
	return json.Marshal(body)
}

// fieldBody is the field flags as the override shape,
// which the policy override shares with the Collection request:
// sampling, target, and artifact, each present only when a flag under it is set.
func (f collectFlags) fieldBody() (map[string]any, error) {
	sampling := map[string]any{}
	body := map[string]any{}
	for _, d := range []struct{ flag, key, value string }{
		{"duration", "duration", f.duration},
		{"round-interval", "roundInterval", f.roundInterval},
	} {
		if d.value == "" {
			continue
		}
		if err := checkDuration(d.flag, d.value); err != nil {
			return nil, err
		}
		sampling[d.key] = d.value
	}
	for _, n := range []struct{ flag, key, value string }{
		{"rounds", "rounds", f.rounds},
		{"max-parallel", "maxParallel", f.maxParallel},
	} {
		if n.value == "" {
			continue
		}
		v, err := positiveInt(n.flag, n.value)
		if err != nil {
			return nil, err
		}
		sampling[n.key] = v
	}
	if f.replicas != "" {
		if f.replicas == "all" {
			sampling["replicas"] = "all"
		} else {
			v, err := positiveInt("replicas", f.replicas)
			if err != nil {
				return nil, fmt.Errorf("%w: --replicas %q is neither all nor a positive integer", client.ErrUsage, f.replicas)
			}
			sampling["replicas"] = v
		}
	}
	if len(sampling) > 0 {
		body["sampling"] = sampling
	}
	if f.targetVersion != "" {
		body["target"] = map[string]any{"version": f.targetVersion}
	}
	if f.retention != "" {
		if err := checkDuration("retention", f.retention); err != nil {
			return nil, err
		}
		body["artifact"] = map[string]any{"retention": f.retention}
	}
	return body, nil
}

// checkDuration refuses a flag value that is not a positive duration.
func checkDuration(flag, value string) error {
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return fmt.Errorf("%w: --%s %q is not a positive duration", client.ErrUsage, flag, value)
	}
	return nil
}

// positiveInt refuses a flag value that is not a positive integer.
func positiveInt(flag, value string) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%w: --%s %q is not a positive integer", client.ErrUsage, flag, value)
	}
	return n, nil
}

// collect runs the verb: the body, the key, the create under it, and the
// identifier and state the answer carried; under --wait, the poll of that record
// until it ends, which begins only once an answer has carried an identifier.
// The client retries the create under the same key while its result is unknown.
// A result still unknown when that window closes is reported once,
// and the message says a Collection may already exist and how to find out.
// It returns the resolved --output beside its failure,
// which is "" while the settings have not been resolved.
func (env *cmdEnv) collect(ctx context.Context, in *invocation, f collectFlags) (string, error) {
	if err := f.checkWaitFlags(); err != nil {
		return "", err
	}
	body, err := f.requestBody()
	if err != nil {
		return "", err
	}
	gw, s, err := env.gateway(ctx, in.globals)
	if err != nil {
		return "", err
	}
	ns, svc, err := address(in.positionals[0], s.Namespace)
	if err != nil {
		return s.Output, err
	}
	key, err := client.NewKey(env.random)
	if err != nil {
		return s.Output, err
	}
	created, err := gw.Create(ctx, ns, svc, body, key)
	if err != nil {
		if client.CreateIndeterminate(err) {
			return s.Output, fmt.Errorf("%w; a Collection may already have been created: run profgate collections %s/%s to find out", err, ns, svc)
		}
		return s.Output, err
	}
	if !f.wait {
		if s.Output == "json" {
			_, err := env.stdout.Write(created.Body)
			return s.Output, err
		}
		return s.Output, writeTable(env.stdout, env.terminal, nil, [][]string{{"id", created.ID}, {"state", created.State}})
	}
	if s.Output != "json" {
		if err := writeTable(env.stdout, env.terminal, nil, [][]string{{"id", created.ID}, {"state", created.State}}); err != nil {
			return s.Output, err
		}
	}
	return s.Output, env.wait(ctx, gw, s, created.ID, f)
}

// wait polls the record until it ends and prints it, then decides the exit:
// completed is 0; failed and cancelled are 1 with the record's reason;
// expired is 1 with a fixed message, because an expired record carries no reason.
// An interrupt stops the watching and not the collecting: the message names
// the identifier and the command that reads the record again, and nothing is cancelled.
// Every other failure of the wait names the identifier beside the cause, so
// a denial of the record route still leaves the caller holding the
// Collection it started.
func (env *cmdEnv) wait(ctx context.Context, gw *client.Client, s client.Settings, id string, f collectFlags) error {
	rec, body, err := gw.Wait(ctx, id, f.pollInterval, f.waitTimeout, env.stderr)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("collection %s is still running; watch it again with profgate collection get %s", id, id)
		}
		return fmt.Errorf("collection %s: %w", id, err)
	}
	if s.Output == "json" {
		if _, err := env.stdout.Write(body); err != nil {
			return err
		}
	} else if err := renderCollection(env, body); err != nil {
		return err
	}
	switch rec.State {
	case client.StateFailed, client.StateCancelled:
		return fmt.Errorf("collection %s %s: %s", id, rec.State, rec.Reason)
	case client.StateExpired:
		return fmt.Errorf("collection %s expired: the artifact's retention elapsed before it was downloaded", id)
	default:
		return nil
	}
}

// collectionsVerb is GET .../collections:
// the Service's records, newest first, with no filter and no cursor because the gateway's listing accepts none.
// The plural takes a Service; an identifier in its place fails the address grammar before any request.
func collectionsVerb() verb {
	return verb{
		name: "collections", leaves: []leaf{{grammar: "collections <ns>/<svc>", positionals: 1}},
		run: func(ctx context.Context, env *cmdEnv, in *invocation) int {
			return env.read(ctx, in, reading{
				build: func(s client.Settings, in *invocation) (client.Request, error) {
					ns, svc, err := address(in.positionals[0], s.Namespace)
					if err != nil {
						return client.Request{}, err
					}
					return client.Request{Method: http.MethodGet, Path: servicePath(ns, svc) + "/collections"}, nil
				},
				render: func(env *cmdEnv, body []byte) error {
					r, err := client.Decode[client.CollectionsResponse](body)
					if err != nil {
						return err
					}
					rows := make([][]string, 0, len(r.Collections))
					for _, c := range r.Collections {
						rows = append(rows, []string{c.ID, c.State, c.Origin, c.CreatedAt})
					}
					return writeTable(env.stdout, env.terminal, []string{"ID", "STATE", "ORIGIN", "CREATED"}, rows)
				},
			})
		},
	}
}

// collectionVerb acts on one record: get is GET /v1/collections/{id} and
// cancel is POST /v1/collections/{id}/cancel, each printing the record.
// The singular takes an identifier; anything outside the identifier grammar,
// a Service address included, is a usage error before any request.
func collectionVerb() verb {
	return verb{
		name: "collection",
		leaves: []leaf{
			{words: "get", grammar: "collection get <id>", positionals: 1},
			{words: "cancel", grammar: "collection cancel <id>", positionals: 1},
		},
		run: func(ctx context.Context, env *cmdEnv, in *invocation) int {
			if in.subverb == "cancel" {
				output, err := env.cancel(ctx, in)
				if err != nil {
					return fail(env, output, err)
				}
				return exitOK
			}
			return env.read(ctx, in, reading{
				build: func(_ client.Settings, in *invocation) (client.Request, error) {
					id, err := collectionID(in)
					if err != nil {
						return client.Request{}, err
					}
					return client.Request{Method: http.MethodGet, Path: client.CollectionPath(id)}, nil
				},
				render: renderCollection,
			})
		},
	}
}

// collectionID is the one positional of the singular verb, refused before any request when it is not the identifier grammar.
func collectionID(in *invocation) (string, error) {
	id := in.positionals[0]
	if !client.IsCollectionID(id) {
		return "", fmt.Errorf("%w: %q is not a Collection identifier: 20 lowercase Crockford base32 characters", client.ErrUsage, id)
	}
	return id, nil
}

// cancel runs collection cancel:
// the one cancel, its retry of collection_initializing inside the client, and the updated record.
// It returns the resolved --output beside its failure,
// which is "" while the settings have not been resolved.
func (env *cmdEnv) cancel(ctx context.Context, in *invocation) (string, error) {
	id, err := collectionID(in)
	if err != nil {
		return "", err
	}
	gw, s, err := env.gateway(ctx, in.globals)
	if err != nil {
		return "", err
	}
	body, err := gw.Cancel(ctx, id)
	if err != nil {
		return s.Output, err
	}
	if s.Output == "json" {
		_, err := env.stdout.Write(body)
		return s.Output, err
	}
	return s.Output, renderCollection(env, body)
}

// renderCollection prints the record's identifier, state, and origin, then
// its progress once a round has been claimed and its reason when it has one,
// which only a failed or cancelled record does.
func renderCollection(env *cmdEnv, body []byte) error {
	rec, err := client.Decode[client.CollectionRecord](body)
	if err != nil {
		return err
	}
	rows := [][]string{{"id", rec.ID}, {"state", rec.State}, {"origin", rec.Origin}}
	if p := rec.Progress; p.Rounds > 0 {
		rows = append(rows, []string{"progress", fmt.Sprintf("round %d of %d, %d ok, %d failed", p.Round, p.Rounds, p.SamplesOK, p.SamplesFailed)})
	}
	if rec.Reason != "" {
		rows = append(rows, []string{"reason", rec.Reason})
	}
	return writeTable(env.stdout, env.terminal, nil, rows)
}

// downloadVerb is GET /v1/collections/{id}/profile: the artifact streamed to
// -o <path>, to <id>.pprof in the working directory, or to stdout under
// -o -, with the same file handling as profile.
func downloadVerb() verb {
	var output string
	return verb{
		name: "download",
		leaves: []leaf{{
			grammar: "download <id> [-o <path>]", positionals: 1,
			flags: func(fs *flag.FlagSet) {
				fs.StringVar(&output, "o", "", "write the artifact here; - writes it to stdout")
			},
		}},
		run: func(ctx context.Context, env *cmdEnv, in *invocation) int {
			mode, err := env.download(ctx, in, output)
			if err != nil {
				return fail(env, mode, err)
			}
			return exitOK
		},
	}
}

// artifactTarget is the two headers the gateway adds to an artifact, saying
// which Collection it is and which version it profiled.
type artifactTarget struct {
	Collection string `json:"collection"`
	Version    string `json:"version"`
}

// download runs the verb:
// the identifier grammar, the destination opened before the request, the fetch, and the metadata on stderr.
// 410 artifact_gone and 409 collection_not_completed come back as the envelope, printed by the caller, with no file left behind.
// It returns the resolved --output beside its failure,
// which is "" while the settings have not been resolved.
func (env *cmdEnv) download(ctx context.Context, in *invocation, output string) (string, error) {
	id, err := collectionID(in)
	if err != nil {
		return "", err
	}
	gw, s, err := env.gateway(ctx, in.globals)
	if err != nil {
		return "", err
	}
	dest, err := env.openDestination(output, id+".pprof", false)
	if err != nil {
		return s.Output, err
	}
	defer dest.cleanup()
	resp, err := gw.Do(ctx, client.Request{Method: http.MethodGet, Path: client.CollectionPath(id) + "/profile"})
	if err != nil {
		dest.discard()
		return s.Output, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := dest.write(resp.Body); err != nil {
		dest.discard()
		return s.Output, fmt.Errorf("%s: %w", s.Origin, err)
	}
	t := artifactTarget{Collection: resp.Header.Get("X-Pprof-Collection"), Version: resp.Header.Get("X-Pprof-Target-Version")}
	if s.Output == "json" {
		if err := json.NewEncoder(env.stderr).Encode(t); err != nil {
			return s.Output, err
		}
	} else if _, err := fmt.Fprintf(env.stderr, "collection: %s\nversion: %s\n", t.Collection, t.Version); err != nil {
		return s.Output, err
	}
	if dest.path != "" {
		_, _ = fmt.Fprintf(env.stderr, "wrote %s\n", dest.path)
	}
	return s.Output, nil
}
