# NATS for PGO collection

PGO collection keeps its control-plane state in three NATS JetStream stores.
The gateway never creates them:
it opens them at startup, checks their configuration, and exits non-zero when one is missing or misconfigured.
An operator provisions them once, with the `nats` CLI against the account that will own them.

## Buckets

```text
nats kv add PROFGATE_CONFIG    --storage file --ttl 0 --max-bucket-size -1 --max-value-size -1 --history 1
nats kv add PROFGATE_JOBS      --storage file --ttl 0 --max-bucket-size -1 --max-value-size -1 --history 1
nats object add PROFGATE_ARTIFACTS --storage file --ttl 0 --max-bucket-size -1
```

File storage, no TTL, and the default `discard: new` are the contract the gateway checks.
The `-1` sizes mean unlimited, the recommended safe default;
preflight also accepts a bounded capacity of at least 64 MiB per KV bucket and 1 GiB for the Object Store,
and a bounded KV value size of at least 512 KiB.
A TTL would expire a Collection record while it still owns work;
`discard: old` would evict one silently instead of failing the write that no longer fits.

On a NATS deployment without authentication this is the whole provisioning:
set `nats.credsFile: ""`, keep the URL free of userinfo, and the gateway connects without credentials;
the user and the Secret below are for an authenticated NATS only.

## User

[`account.conf`](account.conf) is the permission set the gateway's user needs.
It grants the three buckets and the JetStream API subjects that reach them, and nothing else:
no stream creation, no stream deletion, and no bucket outside `PROFGATE_`.
A unit test pins it to `docs/specs/pgo.md`, so a widening shows up as a failing test rather than as a quiet grant.

How the permissions reach the user depends on where the accounts live,
and the two paths end in different chart values, so follow one, not both.
A server whose accounts live in its configuration file takes the block verbatim,
and the gateway authenticates with a username and password in the URL;
a server in operator mode carries the permissions in a user JWT and hands the gateway a credentials file.

### Server-configuration accounts: username and password

Paste the `permissions` block of `account.conf` into the gateway's user entry,
and the user carries exactly those lists:

```text
accounts {
  PROFGATE {
    jetstream: enabled
    users: [
      {
        user: profgate
        password: "..."
        # the permissions block of account.conf, verbatim
        permissions: {...}
      }
    ]
  }
}
```

This path produces no credentials file and needs no Secret mount:
set `nats.credsFile: ""` and carry the username and password in the URL,
`nats.url: "nats://profgate:<password>@nats.nats.svc:4222"`.

The chart renders `nats.url` verbatim into the ConfigMap and prints it in the install notes,
so a password in the structured value lands in both.
To keep it out of them, render a credential-free URL and override it at runtime:
`nats.url` names the server without userinfo,
and `extraEnv` sets `PROFGATE_NATS_URL` from a Secret,
an override the binary applies on top of the rendered configuration:

```yaml
nats:
  url: nats://nats.nats.svc:4222
  credsFile: ""
extraEnv:
  - name: PROFGATE_NATS_URL
    valueFrom:
      secretKeyRef:
        name: profgate-nats-url
        key: url
```

The Secret's `url` key holds the full URL, userinfo included.

### Operator mode: JWT credentials file

On a server in operator mode the permissions ride in the user JWT instead,
and a bare `nsc add user` would grant the whole account.
Pass the same two lists — one `--allow-pub` or `--allow-sub` per subject,
single-quoted so the shell keeps `$JS`, `$KV`, and `$O` literal — and export the credentials:

```text
nsc add user --account PROFGATE profgate \
  --allow-pub '$JS.API.INFO' \
  --allow-pub '$JS.API.STREAM.INFO.KV_PROFGATE_CONFIG' \
  --allow-pub '$JS.API.STREAM.INFO.KV_PROFGATE_JOBS' \
  --allow-pub '$JS.API.STREAM.INFO.OBJ_PROFGATE_ARTIFACTS' \
  --allow-pub '$JS.API.CONSUMER.CREATE.KV_PROFGATE_CONFIG.>' \
  --allow-pub '$JS.API.CONSUMER.CREATE.KV_PROFGATE_JOBS.>' \
  --allow-pub '$JS.API.CONSUMER.CREATE.OBJ_PROFGATE_ARTIFACTS.>' \
  --allow-pub '$JS.API.CONSUMER.DELETE.KV_PROFGATE_CONFIG.>' \
  --allow-pub '$JS.API.CONSUMER.DELETE.KV_PROFGATE_JOBS.>' \
  --allow-pub '$JS.API.CONSUMER.DELETE.OBJ_PROFGATE_ARTIFACTS.>' \
  --allow-pub '$JS.API.CONSUMER.INFO.KV_PROFGATE_CONFIG.>' \
  --allow-pub '$JS.API.CONSUMER.INFO.KV_PROFGATE_JOBS.>' \
  --allow-pub '$JS.API.CONSUMER.INFO.OBJ_PROFGATE_ARTIFACTS.>' \
  --allow-pub '$JS.API.CONSUMER.MSG.NEXT.KV_PROFGATE_CONFIG.>' \
  --allow-pub '$JS.API.CONSUMER.MSG.NEXT.KV_PROFGATE_JOBS.>' \
  --allow-pub '$JS.API.CONSUMER.MSG.NEXT.OBJ_PROFGATE_ARTIFACTS.>' \
  --allow-pub '$JS.API.DIRECT.GET.KV_PROFGATE_CONFIG.>' \
  --allow-pub '$JS.API.DIRECT.GET.KV_PROFGATE_JOBS.>' \
  --allow-pub '$JS.API.DIRECT.GET.OBJ_PROFGATE_ARTIFACTS.>' \
  --allow-pub '$JS.API.STREAM.MSG.GET.KV_PROFGATE_CONFIG' \
  --allow-pub '$JS.API.STREAM.MSG.GET.KV_PROFGATE_JOBS' \
  --allow-pub '$JS.API.STREAM.MSG.GET.OBJ_PROFGATE_ARTIFACTS' \
  --allow-pub '$JS.API.STREAM.PURGE.OBJ_PROFGATE_ARTIFACTS' \
  --allow-pub '$KV.PROFGATE_CONFIG.>' \
  --allow-pub '$KV.PROFGATE_JOBS.>' \
  --allow-pub '$O.PROFGATE_ARTIFACTS.>' \
  --allow-sub '_INBOX.>' \
  --allow-sub '$KV.PROFGATE_CONFIG.>' \
  --allow-sub '$KV.PROFGATE_JOBS.>' \
  --allow-sub '$O.PROFGATE_ARTIFACTS.>'
nsc generate creds --account PROFGATE --name profgate > profgate.creds
```

`account.conf` is the pinned source; when it changes, these flags change with it.

### Verify the grant

Before distributing the credentials, on the operator path,
`nsc describe user --account PROFGATE profgate` prints the JWT's Pub Allow and Sub Allow lists,
which must match `account.conf` subject for subject.
Then probe the grant by listing streams, which is outside it:
with username and password, run `nats stream ls -s "nats://profgate:<password>@<host>:4222"`;
in operator mode, run `nats stream ls -s "nats://<host>:4222" --creds profgate.creds`.
Either probe must fail with a permissions violation, not an authentication error,
which proves the grant blocks stream listing.

## Credentials Secret

Only the operator-mode path produces a credentials file;
the username-and-password path mounts nothing and skips this section.
The gateway reads the credentials from the file named by `nats.credsFile`.
Create the Secret from the exported file:

```text
kubectl -n profgate create secret generic profgate-nats-creds --from-file=nats.creds=profgate.creds
```

The Deployment already mounts it read-only at `/etc/profgate/nats/`, as an optional volume,
so the base applies cleanly before the Secret exists and picks it up once it does.
[`../secret-nats-example.yaml`](../secret-nats-example.yaml) shows the same Secret as a manifest;
it is commented out in full, because the file it carries is a credential.
