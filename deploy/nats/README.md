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

File storage, no TTL, no size ceiling, and the default `discard: new` are the contract the gateway checks.
A TTL would expire a Collection record while it still owns work;
`discard: old` would evict one silently instead of failing the write that no longer fits.

## User

[`account.conf`](account.conf) is the permission set the gateway's user needs.
It grants the three buckets and the JetStream API subjects that reach them, and nothing else:
no stream creation, no stream deletion, and no bucket outside `PROFGATE_`.
A unit test pins it to `docs/specs/pgo.md`, so a widening shows up as a failing test rather than as a quiet grant.

Give the user those permissions and export its credentials:

```text
nsc add user --account PROFGATE profgate
nsc generate creds --account PROFGATE --name profgate > profgate.creds
```

## Credentials Secret

The gateway reads the credentials from the file named by `nats.credsFile`.
Create the Secret from the exported file:

```text
kubectl -n profgate create secret generic profgate-nats-creds --from-file=nats.creds=profgate.creds
```

The Deployment already mounts it read-only at `/etc/profgate/nats/`, as an optional volume,
so the base applies cleanly before the Secret exists and picks it up once it does.
[`../secret-nats-example.yaml`](../secret-nats-example.yaml) shows the same Secret as a manifest;
it is commented out in full, because the file it carries is a credential.
