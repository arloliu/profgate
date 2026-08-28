package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"

	"github.com/arloliu/profgate/internal/client"
)

// pgoVerb is the three policy subverbs over GET, PUT, and DELETE .../pgo:
// get reads, and set and delete each read first and then send one conditional write.
func pgoVerb() verb {
	var f policyFlags
	return verb{
		name:        "pgo",
		subverbs:    []string{"policy get", "policy set", "policy delete"},
		positionals: 1,
		grammar:     "pgo policy get|set|delete <ns>/<svc> [--file <path> | --enabled[=false] --every <d> --jitter <d> <field flags of collect>]",
		flags: func(fs *flag.FlagSet) {
			fs.StringVar(&f.file, "file", "", "send this JSON file as the whole override instead of the flags")
			fs.BoolVar(&f.enabled, "enabled", false, "whether the scheduler collects for the Service")
			fs.StringVar(&f.every, "every", "", "how often the scheduler collects")
			fs.StringVar(&f.jitter, "jitter", "", "how far each scheduled collection may drift")
			fs.StringVar(&f.fields.duration, "duration", "", "how long each sample runs")
			fs.StringVar(&f.fields.rounds, "rounds", "", "how many rounds to sample")
			fs.StringVar(&f.fields.roundInterval, "round-interval", "", "the pause between rounds")
			fs.StringVar(&f.fields.replicas, "replicas", "", "how many Pods each round samples: all or a count")
			fs.StringVar(&f.fields.maxParallel, "max-parallel", "", "how many Pods are sampled at once")
			fs.StringVar(&f.fields.targetVersion, "target-version", "", "the binary version to profile")
			fs.StringVar(&f.fields.retention, "retention", "", "how long the merged profile is kept")
		},
		run: func(ctx context.Context, env *cmdEnv, in *invocation) int {
			switch in.subverb {
			case "policy set":
				if err := env.setPolicy(ctx, in, f); err != nil {
					return fail(env, err)
				}
				return exitOK
			case "policy delete":
				if err := env.deletePolicy(ctx, in); err != nil {
					return fail(env, err)
				}
				return exitOK
			default:
				return env.read(ctx, in, reading{
					build: func(s client.Settings, in *invocation) (client.Request, error) {
						path, err := policyPath(in, s)
						if err != nil {
							return client.Request{}, err
						}
						return client.Request{Method: http.MethodGet, Path: path}, nil
					},
					render: renderPolicy,
				})
			}
		},
	}
}

// policyFlags is what pgo policy set said:
// the file, the two schedule fields, enabled, and the field flags it shares with collect.
// Whether --enabled was passed at all is read from the parsed flag set,
// because an absent field is left to the defaults and a false one is not.
type policyFlags struct {
	file, every, jitter string
	enabled             bool
	fields              collectFlags
}

// requestBody is the override document: the file under --file, or the
// flags as the override shape, with --file beside any flag a usage error
// and nothing at all one too, because an empty override would replace whatever is stored with nothing.
func (f policyFlags) requestBody(fs *flag.FlagSet) ([]byte, error) {
	set := map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { set[fl.Name] = true })
	flagged := f.fields.fieldFlagSet() || f.every != "" || f.jitter != "" || set["enabled"]
	if f.file != "" {
		if flagged {
			return nil, fmt.Errorf("%w: --file sends a document as the whole override; pass it without the other flags", client.ErrUsage)
		}
		data, err := os.ReadFile(f.file)
		if err != nil {
			return nil, fmt.Errorf("%w: --file: %w", client.ErrUsage, err)
		}
		if !json.Valid(data) {
			return nil, fmt.Errorf("%w: --file %s is not a JSON document", client.ErrUsage, f.file)
		}
		return data, nil
	}
	if !flagged {
		return nil, fmt.Errorf("%w: pgo policy set takes --file <path> or at least one field flag", client.ErrUsage)
	}
	body, err := f.fields.fieldBody()
	if err != nil {
		return nil, err
	}
	if set["enabled"] {
		body["enabled"] = f.enabled
	}
	schedule := map[string]any{}
	for _, d := range []struct{ flag, value string }{{"every", f.every}, {"jitter", f.jitter}} {
		if d.value == "" {
			continue
		}
		if err := checkDuration(d.flag, d.value); err != nil {
			return nil, err
		}
		schedule[d.flag] = d.value
	}
	if len(schedule) > 0 {
		body["schedule"] = schedule
	}
	return json.Marshal(body)
}

// policyPath is .../pgo for the one positional.
func policyPath(in *invocation, s client.Settings) (string, error) {
	ns, svc, err := address(in.positionals[0], s.Namespace)
	if err != nil {
		return "", err
	}
	return servicePath(ns, svc) + "/pgo", nil
}

// readETag issues the read and returns the ETag it carried, which is empty
// when the Service runs on defaults alone.
func readETag(ctx context.Context, gw *client.Client, path string) (string, error) {
	_, header, err := gw.JSON(ctx, client.Request{Method: http.MethodGet, Path: path})
	if err != nil {
		return "", err
	}
	return header.Get("ETag"), nil
}

// conditional is the write's header: If-Match with the ETag the read
// carried, and no If-Match at all when it carried none, which is what creates the override rather than updating it.
// If-Match: * is never sent; the gateway refuses it.
func conditional(etag string) http.Header {
	h := http.Header{"Accept": {"application/json"}}
	if etag != "" {
		h.Set("If-Match", etag)
	}
	return h
}

// lostCondition wraps a 412 or a 428, the two ways the policy is no longer what this command read, naming the command to run again after looking at the current value;
// nothing retries,
// because a retry would overwrite what the other writer decided.
func lostCondition(err error, service string) error {
	var ae *client.APIError
	var se *client.StatusError
	var status int
	switch {
	case errors.As(err, &ae):
		status = ae.Status
	case errors.As(err, &se):
		status = se.Status
	}
	if status == http.StatusPreconditionFailed || status == http.StatusPreconditionRequired {
		return fmt.Errorf("%w; the policy changed since it was read: run profgate pgo policy get %s, then decide again", err, service)
	}
	return err
}

// setPolicy runs pgo policy set: the body from the flags, the read for
// its ETag, and the one PUT under its condition, then the answer.
func (env *cmdEnv) setPolicy(ctx context.Context, in *invocation, f policyFlags) error {
	body, err := f.requestBody(in.fs)
	if err != nil {
		return err
	}
	gw, s, err := env.gateway(ctx, in.globals)
	if err != nil {
		return err
	}
	path, err := policyPath(in, s)
	if err != nil {
		return err
	}
	etag, err := readETag(ctx, gw, path)
	if err != nil {
		return err
	}
	h := conditional(etag)
	h.Set("Content-Type", "application/json")
	answer, _, err := gw.JSON(ctx, client.Request{Method: http.MethodPut, Path: path, Body: body, Header: h})
	if err != nil {
		return lostCondition(err, in.positionals[0])
	}
	if s.Output == "json" {
		_, err := env.stdout.Write(answer)
		return err
	}
	return renderPolicy(env, answer)
}

// deletePolicy runs pgo policy delete: the read for its ETag and the one
// DELETE under its condition.
// A Service on defaults alone sends no If-Match and the gateway answers
// 404 pgo_override_not_found, which is reported as it came.
func (env *cmdEnv) deletePolicy(ctx context.Context, in *invocation) error {
	gw, s, err := env.gateway(ctx, in.globals)
	if err != nil {
		return err
	}
	path, err := policyPath(in, s)
	if err != nil {
		return err
	}
	etag, err := readETag(ctx, gw, path)
	if err != nil {
		return err
	}
	resp, err := gw.Do(ctx, client.Request{Method: http.MethodDelete, Path: path, Header: conditional(etag)})
	if err != nil {
		return lostCondition(err, in.positionals[0])
	}
	_ = resp.Body.Close()
	_, _ = fmt.Fprintf(env.stderr, "deleted the policy override of %s\n", in.positionals[0])
	return nil
}

// renderPolicy prints the source, the effective policy one field per row,
// the two update fields when the override is stored, and one row per violation.
func renderPolicy(env *cmdEnv, body []byte) error {
	p, err := client.Decode[client.PolicyResponse](body)
	if err != nil {
		return err
	}
	e := p.Effective
	rows := [][]string{
		{"source", p.Source},
		{"enabled", strconv.FormatBool(e.Enabled)},
		{"every", e.Schedule.Every},
		{"jitter", e.Schedule.Jitter},
		{"duration", e.Sampling.Duration},
		{"rounds", strconv.Itoa(e.Sampling.Rounds)},
		{"roundInterval", e.Sampling.RoundInterval},
		{"replicas", fmt.Sprint(e.Sampling.Replicas)},
		{"maxParallel", strconv.Itoa(e.Sampling.MaxParallel)},
		{"versionPolicy", e.Target.VersionPolicy},
		{"version", e.Target.Version},
		{"retention", e.Artifact.Retention},
	}
	if p.UpdatedBy != "" {
		rows = append(rows, []string{"updatedBy", p.UpdatedBy}, []string{"updatedAt", p.UpdatedAt})
	}
	for _, v := range p.Violations {
		rows = append(rows, []string{"violation", v.Field + ": " + v.Detail})
	}
	return writeTable(env.stdout, env.terminal, nil, rows)
}
