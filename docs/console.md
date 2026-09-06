# Console

A browser page, served by the gateway itself, for pulling a profile without already knowing a namespace and a Service name by heart.
This guide covers it from a user's view: turning it on, what it shows, signing in,
downloading a profile or copying its URL,
and the Collections view, from which a Collection can also be started and cancelled.
The full design lives in [specs/ui.md](specs/ui.md);
the four routes it calls are documented in [api.md](api.md#listing-endpoints).

## Enabling it

The console is off by default.
`ui.enabled: true` (environment `PROFGATE_UI_ENABLED`) turns it on, restart-only —
[configuration.md](configuration.md#ui) covers the key,
and [deployment.md](deployment.md#the-console) covers the Ingress paths it needs once it is on.
Under `auth.mode: oidc` an enabled console requires `auth.oidc.browser`,
because a console that cannot log a browser in serves nobody;
the gateway refuses to start without it.
`basic` and `disabled` need nothing extra.

Once it is on, open `/ui/` in a browser — for example, through the same port-forward
[api.md](api.md#reaching-the-api) uses:

```sh
kubectl -n profgate port-forward svc/profgate 8080:8080
```

then visit `http://localhost:8080/ui/`.

## What it shows

The page has four parts, in the order they load: Identity, Profile, Collections (when offered),
and, on the way, a namespace and a Service picker.

- **Identity.** Who you are, your realm's namespaces, Services, and profiles exactly as configured
  (the wildcard `*` shown as written), the three PGO flags, and the authentication mode.
  A sign-out link appears when one would do something (see [Signing in and out](#signing-in-and-out)).
- **Namespace and Service.** Two `<select>` controls, populated from what your realm admits;
  picking one fetches the next.
  A namespace or Service named in the page's URL that is not in the fetched list —
  the realm changed, the Service went away, the label was typed by hand —
  shows "`<value>` is not listed" and keeps the value until you pick another, so reloading retries it.
- **Profile.** A profile `<select>` (filtered by your realm), a duration field for `cpu` and `trace` bounded by the operator's configured limit, a port control, and a Pod and version `<select>`
  once targets have loaded.
  The port control is a menu of the configured default and every entry `discovery.pprof.allowedSelections` lists,
  read from `/v1/limits`.
  A free-form field appears beside the menu only for the kind whose wildcard is configured:
  a port-number field under `{port: "*"}`, a port-name field under `{portName: "*"}`.
  Typing in one field clears the other, and a non-empty field wins over the menu.
  Building this fills in a read-only Profile URL field with a **Download** control
  and, where the browser allows it, a **Copy URL** button.
  The page always asks the targets endpoint for `explain=true`, the diagnostic behind this control.
  When a Service has no target, the Pod and version controls are replaced by the reasons the gateway counted,
  in its order, each in its own wording rather than the raw reason name.
  A Service whose selector matches no Pod reads as its own sentence instead of a reason list.
  A fetch a mid-rollout replica refuses for `explain=true` is retried once without it,
  keeping the port selection, and the retry's answer is the plain listing with no reasons.
  **Download** is disabled while the Service has no eligible Pod, with those reasons beside it,
  and a **Refresh** control fetches the targets again.
- **Collections.** Shown only when PGO collection is enabled and your realm may read it,
  with a **Start collection** control and a **Cancel** on a live row when your realm may collect as well;
  see [Collections](#collections).

The page keeps the chosen namespace and Service in its own URL (`/ui/?ns=&svc=`),
so a reload, a bookmark, or a return from signing in lands back on the same selection —
unless including the selection would make the encoded return path exceed the 1024 bytes the login accepts,
in which case signing in lands on the console with no selection.
Nothing else about your session is remembered.

## Signing in and out

The page never holds a credential of its own; it relies on whatever the browser already has.

- **`auth.mode: disabled`.**
  No sign-in exists.
  The Identity panel shows `anonymous` and the realm named by `auth.anonymousRealm`.
- **`auth.mode: basic`.**
  The page loads, and its first request gets a `401` that carries `WWW-Authenticate: Basic`;
  the browser's own dialog prompts for a name and password
  and the browser remembers them for the rest of the session.
  Cancelling the dialog shows "sign in required" with a **Retry** button that prompts again.
  There is no sign-out: how long a browser keeps a Basic credential is the browser's decision,
  and the Identity panel says so in place of a sign-out link.
- **`auth.mode: oidc`.**
  The page checks who you are first;
  a `401` sends the browser to the issuer's own login page,
  and signing in returns you to the same selection,
  or to the console with no selection when including it would push the encoded return path past that bound.
  If the page is still `401` after that return,
  it shows "sign in required" with a **Sign in again** button rather than sending you back to the issuer on its own.
  A **Sign out** link appears in the Identity panel whenever the browser flow is configured,
  and signing out lands back on a signed-out console.

## Downloading a profile and copying its URL

**Download** fetches the profile through the same request the gateway runs for `curl`
and saves the bytes under the name the response carries;
a response that declares no `Content-Encoding`, which is every response the pprof handler writes,
is saved as the body `curl` receives, byte for byte,
and a response an intermediary encoded is saved decoded.
An answer that is not a profile — `no_targets`, `service_not_found`, `realm_denied` — is shown in the panel with its hint,
and the control is disabled while a download is in flight.

**Copy URL** puts the profile's absolute URL on the clipboard, so it can be pasted into
`go tool pprof <url>` or a `curl` command elsewhere.
The URL carries no credential of its own, and which mode can use it as is differs:

| Mode | Copied URL |
|---|---|
| `disabled` | Works as is. |
| `basic` | Needs `user:password@` added to the URL, or `curl -u`. |
| `oidc` | Needs a bearer token, which `go tool pprof` cannot send; save the file from the console instead. |

The page always shows the URL in a plain, selectable text field.
**Copy URL** appears only when the browser exposes its clipboard to the page,
which it does in a secure context: an `https://` page, or `http://localhost` such as the port-forward above.
A plain `http://` host under `disabled`, or under `basic` with plaintext permitted,
shows the URL for copying by hand.

## Collections

The Collections panel appears once a Service is chosen, only when `pgo.enabled` is true on the gateway
and your realm's `pgo.read` flag is true.
It lists the Service's Collections newest first —
`id`, `origin`, `state`, `attempt`, `resolvedVersion`, `createdAt`, `finishedAt`, and `expiresAt` —
and picking a row shows the full record:
`state`, `reason` on a failed or cancelled one, `progress`, `createdBy`, the four timestamps, and the stored artifact's size.
The table is the newest hundred at most;
when older Collections exist a line under it says so and names `profgate collections <ns>/<svc>`, which lists them all.
A **Refresh** control fetches the list again,
and it is how a Collection started here is watched: the page polls nothing.
A **Download profile** link appears only once a record's `state` is `completed` and it carries an artifact;
every other state shows no link.
**Start collection** sits above the table, and **Cancel** on every row whose state is `pending` or `running`,
when your realm's `pgo.collect` flag is true as well;
a realm that may read and not collect sees the table alone.
Each control takes two presses: the first arms it and offers **Keep**,
the second — **Confirm start** or **Confirm cancel** — sends the request;
a second press inside half a second of the first is ignored,
so a double-click whose second click lands inside that window sends nothing.
An armed control that is neither confirmed nor kept disarms itself after ten seconds.
**Keep** disarms a control that has sent nothing;
after a start whose answer never arrived, **Keep** abandons the attempt and says a Collection may already exist.

A start whose answer never arrives keeps the control armed, and pressing it again is the same attempt:
the page sends the idempotency key the lost press sent,
so the gateway answers with the Collection that press created rather than starting a second one.
Changing the namespace or the Service abandons that attempt and says so,
because the answer that never came belongs to the Service it was sent for.
When the gateway is at its limit, the control comes back after the delay the answer names,
or after five seconds when it names none.

What a Collection is and how it runs is [pgo.md](pgo.md)'s subject.

## During a rolling update

Each request the page makes — the shell, the script, the stylesheet, every asset — can land on either build during a rollout.
Every asset has the same URL on both, so an asset that both builds carry is served by whichever replica answers.
Two things can still fail a load, and a reload once the rollout has converged recovers from both:
a release that adds a file or drops one has a path the other build answers `404` for,
and the `v0.4.0` to `v0.5.0` upgrade, which moved the console off its content-hashed asset URLs,
is the one rollout where neither build serves what the other's page asks for.
A load can also take its shell from one build and a module from the other,
which runs unless those two files changed incompatibly in that release.
The gateway pins no browser to one replica and shares no asset store between them.

## What the console never does

- **Render a profile.**
  It downloads the bytes `curl` would, decoded where an intermediary added a `Content-Encoding`;
  open them with `go tool pprof -http` or a tool of your choosing.
- **Store anything.**
  The page keeps no state beyond the namespace and Service in its own URL;
  it holds no database, no cache, and no file,
  and a download holds the profile in memory until the save has begun and drops its references to it then.
- **Edit a Service's PGO policy.**
  It starts and cancels Collections, and it writes nothing else:
  the stored override stays a `curl` operation with an `If-Match` precondition
  ([api.md](api.md#policy-override)).
