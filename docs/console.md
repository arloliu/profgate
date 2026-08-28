# Console

A browser page, served by the gateway itself, for pulling a profile without already knowing a namespace and a Service name by heart.
This guide covers it from a user's view: turning it on, what it shows, signing in,
downloading a profile or copying its URL, and the read-only Collections view.
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
  Building this fills in a read-only Profile URL field with a **Download** link
  and, where the browser allows it, a **Copy URL** button.
- **Collections.** Shown only when PGO collection is enabled and your realm may read it;
  see [Collections, read-only](#collections-read-only).

The page keeps the chosen namespace and Service in its own URL (`/ui/?ns=&svc=`),
so a reload, a bookmark, or a return from signing in lands back on the same selection.
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
  a `401` sends the browser to the issuer's own login page, and signing in returns you to the same selection.
  If the page is still `401` after that return,
  it shows "sign in required" with a **Sign in again** button rather than sending you back to the issuer on its own.
  A **Sign out** link appears in the Identity panel whenever the browser flow is configured,
  and signing out lands back on a signed-out console.

## Downloading a profile and copying its URL

**Download** is an ordinary link to the profile endpoint;
the browser saves the bytes the way it would for any file, through the same request the gateway runs for `curl`.

**Copy URL** puts the profile's absolute URL on the clipboard, so it can be pasted into
`go tool pprof <url>` or a `curl` command elsewhere.
The URL carries no credential of its own, and which mode can use it as is differs:

| Mode | Copied URL |
|---|---|
| `disabled` | Works as is. |
| `basic` | Needs `user:password@` added to the URL, or `curl -u`. |
| `oidc` | Needs a bearer token, which `go tool pprof` cannot send; save the file from the console instead. |

The page always shows the URL in a plain, selectable text field —
**Copy URL** only appears when the browser exposes a clipboard API to a secure context,
which an HTTP page under `disabled`, or under `basic` with plaintext explicitly permitted, does not.

## Collections, read-only

The Collections panel appears once a Service is chosen, only when `pgo.enabled` is true on the gateway
and your realm's `pgo.read` flag is true.
It lists the Service's Collections newest first —
`id`, `origin`, `state`, `attempt`, `resolvedVersion`, `createdAt`, `finishedAt`, and `expiresAt` —
and picking a row shows the full record:
`state`, `reason` on a failed or cancelled one, `progress`, `createdBy`, the four timestamps, and the stored artifact's size.
A **Download profile** link appears only once a record's `state` is `completed` and it carries an artifact;
every other state shows no link.
The console only reads Collections here — it sends no request that starts, cancels, or configures one.
What a Collection is and how it runs is [pgo.md](pgo.md)'s subject.

## During a rolling update

Each request the page makes — the shell, the script, the stylesheet, every asset — can land on either build during a rollout.
A page can therefore fail to load, or fail again on reload, until every replica has rolled to the new version;
a reload afterward recovers.
This is a property of serving static files from more than one replica, not a bug the console works around.

## What the console never does

- **Render a profile.**
  It downloads the same bytes `curl` would;
  open them with `go tool pprof -http` or a tool of your choosing.
- **Store anything.**
  The page keeps no state beyond the namespace and Service in its own URL;
  it holds no database, no cache, and no file.
- **Write PGO state.**
  It never starts, cancels, or configures a Collection —
  those stay `curl` operations with request bodies and preconditions ([api.md](api.md#pgo-collection)).
