# Profgate Console

**Status:** Accepted

This document designs a thin operator console for the gateway
([`gateway.md`](gateway.md)):
a static page served by the gateway itself,
plus four read-only listing endpoints the page needs and a command-line client can use as well.
Everything in the gateway design — permission boundary, discovery seam, request algorithm, realms,
non-disclosure, configuration, testing — is assumed and not restated;
the browser flow, session cookie, and `Sec-Fetch` rules of [`auth.md`](auth.md) are assumed the same way,
and the PGO routes of [`pgo.md`](pgo.md) are read by the console and never changed.
Sections of this document and of the other specs are cited by heading name.

---

## 1. Overview

The gateway's documented clients are `curl` and `go tool pprof`.
Both need the caller to already know a namespace and a Service name,
and neither tells the caller what their realm admits.
The console closes that gap and nothing more:
pick a namespace, then a Service, then a profile and, optionally, a Pod, a version, a port, and a duration;
download the profile, or copy the URL that `go tool pprof` fetches it from;
see who you are and what your realm admits;
and, when PGO collection is enabled, read the Collections of a Service and download a finished artifact.

The console is a page, not a product surface.
It renders no profile, draws no flamegraph, and stores nothing;
the profile bytes it hands out are the same bytes the profile endpoint streams to `curl`,
through the same request algorithm, and the browser saves them as a file.

### 1.1 Core decisions

1. **The console is a client of the API, not a second API.**
   It calls `/v1` with `fetch` the way a script would,
   is authenticated the way a browser already is under [`auth.md`](auth.md),
   and holds no credential of its own.
   Every fact it shows came from a `/v1` response the gateway served to that caller.
2. **Four listing endpoints, all read-only, each bounded by what it describes.**
   The four are:
   `GET /v1/namespaces`, `GET /v1/namespaces/{namespace}/services`, `GET /v1/whoami`, and `GET /v1/limits`.
   The two catalogs are realm-filtered.
   `whoami` is caller-specific: it describes the caller and that caller's realm.
   `limits` is global: it describes the gateway's own configuration, is identical for every caller,
   and is answered to every request the configured `auth.mode` admits —
   anonymous requests included, because `disabled` admits them.
   They exist so the console can offer choices instead of a blank text field.
   They read informer caches and configuration, never the API server,
   and they add no Kubernetes capability.
3. **Nothing outside the caller's realm is disclosed, existence included.**
   A namespace the realm denies is absent from the namespace list and answers `403 realm_denied` on its Service list,
   the same `403` whether or not the namespace holds a Service.
   A Service the realm denies is absent from every list.
   No listing response carries a Pod IP or a port number a Pod declares.
   Realms bound cluster state; the global port policy `/v1/limits` reports is operator configuration,
   and disclosing it is a decision this document argues for (*Limits*).
4. **No build step, no network at runtime.**
   Preact, htm, and Pico CSS are vendored as files under `internal/ui/static/`
   and compiled into the binary with `go:embed`;
   the page loads nothing from a CDN, and the deployment topology of the gateway does not change.
5. **Strict Content Security Policy, and no HTML built from a response.**
   `default-src 'none'` with each source the page needs listed by name,
   no inline script, no inline style, no `eval`,
   and every value a response carries reaches the DOM as text and never as markup
   (*Rendering response values*),
   so a bug in the page cannot become a way to run someone else's code against the caller's session.
6. **Off by default.**
   `ui.enabled` is `false`; a gateway that does not set it serves `/ui/` as `404 route_unknown`,
   exactly as it does today.
7. **The interactive request algorithm keeps its shape.**
   The profile endpoint is untouched, step for step and outcome for outcome.
   The targets endpoint keeps its own steps, their order, and its realm boundary,
   and gains the diagnostic parameters of gateway *List targets*,
   which the console sends and reads (*Targets, with reasons*);
   the listing endpoints run a prefix of that algorithm and stop at the cache.

### 1.2 Non-goals

- Rendering profiles: flamegraphs, call graphs, diffs, top tables.
  `go tool pprof -http` renders a downloaded profile;
  Grafana Pyroscope and Parca exist for continuous profiling and are not dependencies of this design.
- Continuous or scheduled profiling from the console.
  Scheduled CPU collection is PGO collection ([`pgo.md`](pgo.md)) and the console only reads its state.
- Writing PGO state: creating, cancelling, or configuring Collections.
  Those are `curl` operations with request bodies and preconditions;
  a later revision may add them once the read-only console has earned its place.
- Any change to the gateway's authentication.
  The console logs in through the browser flow of [`auth.md`](auth.md) under `oidc`,
  through the browser's native dialog under `basic`,
  and not at all under `disabled`.
  It never holds, stores, or forwards a bearer token.
- Cross-origin use.
  The page and the API are one origin;
  there is no CORS header, no API key, and no embedding in another site.
- Accessibility and localisation work beyond what Pico CSS and plain HTML give for free.
  Every control is a native `<select>`, `<input>`, `<button>`, or `<a>`.
- A browser-driven test of the page.
  The toolchain pins no JavaScript runtime and no browser, and this design adds neither;
  *What is not proven* says what that leaves unproven and why the trade is accepted.

---

## 2. Routes

The API listener gains a static route family and four `/v1` routes.
The ops listener is unchanged.

| Route | Methods | Authentication | Realm | Exists when |
|---|---|---|---|---|
| `/ui/` | `GET`, `HEAD` | none | none | `ui.enabled` |
| `/ui/static/{hash}/{file}` | `GET`, `HEAD` | none | none | `ui.enabled` |
| `/` | `GET`, `HEAD` | none | none | `ui.enabled`; `302` to `/ui/` |
| `/v1/whoami` | `GET` | yes | none | always |
| `/v1/limits` | `GET` | yes | none | always |
| `/v1/namespaces` | `GET` | yes | filter | always |
| `/v1/namespaces/{namespace}/services` | `GET` | yes | namespace check, then filter | always |

The static routes and `/` accept `HEAD` because they serve files,
and a `HEAD` is how a proxy or a monitor asks about a file;
`HEAD` answers the headers of the `GET`, the `302` and its `Location` included, with no body,
and any other method is `405 method_not_allowed` with `Allow: GET, HEAD`.
The four `/v1` routes accept `GET` only, like the other `GET`-only `/v1` routes,
and answer `405` with `Allow: GET` otherwise.

The four `/v1` routes exist whether or not `ui.enabled` is set:
they are useful to a script,
and a route that appears and disappears with a page would make the API's shape depend on a display option.

### 2.1 Why the page itself is not authenticated

The shell — `index.html`, `app.js`, `app.css`, and the vendored files — is the same bytes for every caller
and discloses no cluster data:
no namespace, Service, Pod, realm, or principal appears in it, and its source is in this repository.
Only the `/v1` endpoints the page calls are authenticated,
and every fact the page shows came from one of them.

Serving the shell without an authentication step keeps `basic` mode free of special cases:
the page loads, its first `fetch` answers `401` with `WWW-Authenticate: Basic`,
and the browser's native dialog does the rest (*Signing in and out*).
Under `oidc` the page learns it is signed out from the same `401` on `/v1/whoami`
and navigates to `/auth/login` itself (*Signing in and out*).
The return from login needs nothing from the shell either way:
the callback answers a landing page whose refresh starts a navigation from the gateway's own origin
([`auth.md`](auth.md) *The `/auth/` routes*),
so the request that brings the browser back to `/ui/` arrives `same-origin`
and would pass the session rule even if the shell checked it.

The cost is that an unauthenticated caller can download the console's source from a gateway that enables it.
That is the same source anyone can read here.

### 2.2 Request algorithm for the listing endpoints

The four `/v1` routes run the gateway *Request algorithm* as [`auth.md`](auth.md) *Request algorithm* composes it,
in the order `internal/httpapi` runs it today, and stop at the cache:

1. **Route.** The path is one of the four; `{namespace}` must be a DNS-1123 label → `404 route_unknown`.
2. **Method.** `GET` only → `405 method_not_allowed` with `Allow: GET`.
3. **Readiness.** The readiness `internal/httpapi` composes for every `/v1` route —
   discovery synced and, under `oidc`, the issuer discovered with its first keys fetched
   ([`auth.md`](auth.md) *Request algorithm*; `Deps.Ready` in `internal/httpapi`) —
   false → `503 not_ready`, for all four,
   so a client learns the gateway's readiness from every `/v1` route in the same way.
4. **Credential placement.**
   `access_token` as a query parameter → `400 invalid_parameter`.
5. **Authentication.** Per `auth.mode` → `401 unauthenticated`, `429 too_many_auth`, `503 auth_unavailable`,
   or `302` to login for a navigation under the browser flow.
6. **Realm.** Service list only: the realm's `namespaces` must admit `{namespace}` → `403 realm_denied`.
   The other three have no realm step to fail:
   `whoami` describes the caller,
   `limits` describes the gateway and is the same for every caller,
   and the namespace list is filtered rather than refused.
7. **Parameters.** Any query parameter → `400 invalid_parameter`.
   None of the four takes one.
8. **Read.** `whoami` and `limits` answer `200` from the configuration snapshot the request loaded at entry.
   The namespace and Service lists call `Catalog` on the seam (*Package layout*),
   apply the realm filter (*The realm filter*), and answer `200`;
   an error from `Catalog` → `503 discovery_unavailable`,
   the same mapping `internal/httpapi` gives every unclassified `Targets` error today,
   and never an empty `200`.

The realm check precedes the read for the same reason it precedes discovery on the interactive routes:
a caller denied a namespace learns nothing from a malformed query, and causes no read of the cache;
the denial happens before `Catalog` is called, which a unit test proves (*Unit*).
Nothing after the read exists:
no discovery call, no admission slot, no confirmation, no API server round trip.

### 2.3 The realm filter

A realm's `namespaces`, `services`, and `profiles` are each a list of exact strings or the single entry `"*"`
(gateway *Realm structure*; `config.Realm` in `internal/config`).
`internal/httpapi` evaluates each list with one predicate, `listAllows(list, value)` in `realm.go`:
the list admits a value when it contains `"*"` or contains the value itself;
there is no prefix, glob, or pattern matching.
The filter is that predicate applied to what the Service cache holds, list by list:

- A **Service** is listed when it has a non-empty `spec.selector`,
  `realm.namespaces` admits its namespace, and `realm.services` admits its name.
- A **namespace** is listed when `realm.namespaces` admits it
  and it holds at least one Service that is listed by the rule above —
  a namespace whose only selector-bearing Services are all outside `realm.services` is absent,
  because listing it would disclose that the namespace exists.
- A **profile** is offered when `realm.profiles` admits it;
  `["*"]` therefore expands to every name `/v1/limits` returns,
  and an explicit list is intersected with it in the order `/v1/limits` uses.

The two lists are independent, so the four combinations are all real configurations:

| `namespaces` | `services` | Namespace list | Service list for an admitted namespace |
|---|---|---|---|
| `["*"]` | `["*"]` | every namespace holding a Service with a selector | every Service with a selector |
| `["*"]` | names | every namespace holding a named Service with a selector | the named Services that exist there with a selector |
| names | `["*"]` | the named namespaces that hold a Service with a selector | every Service with a selector |
| names | names | the named namespaces that hold a named Service with a selector | the named Services that exist there with a selector |

A selectorless Service is omitted from every list.
The gateway's first eligibility rule refuses it with `422 service_selectorless` whatever else is true,
so it is the one kind of Service a profile request is guaranteed to fail against;
omitting it removes guaranteed failures and nothing else.
A listed Service is not a promise that a profile request succeeds:
it may have no Ready Pod, no Pod declaring the selected port, or a Pod that dies mid-profile,
and those answer through the interactive algorithm as they do for `curl`.
Selector presence is the only criterion.
`spec.type` is not read: `Targets` does not read it either,
and a Service of type `ExternalName` carries no selector in practice and falls under the same rule.

A realm that names a namespace the cache does not hold gets a namespace list without it,
and a Service list for it of `[]` with `200`.
There is no `namespace_not_found`:
the gateway observes no Namespace objects — its RBAC has no `namespaces` tuple and this design adds none —
so "the namespace exists but holds no Service" and "the namespace does not exist" are the same fact to it,
and a `200` with an empty list states exactly that fact.
The realm's own configured lists, typos included, are what `/v1/whoami` returns (*Who am I*),
so an operator can tell a misspelled realm entry from an empty namespace.

The filter reads the Service informer cache and nothing else.
It is as old as the cache,
which the gateway *Informers* section already states as the contract for everything selection reads.

---

## 3. Response shapes

Every successful listing response and every gateway error envelope is `application/json`,
carries `Cache-Control: no-store`, and sorts its lists by name;
arrays are `[]` and never `null` when empty.
The one response on these routes that is not JSON is inherited from [`auth.md`](auth.md) *What is redirected*:
under `oidc`, a browser navigation to one of the four routes without a session —
a URL typed or pasted into the address bar —
is a bodyless `302` to `/auth/login`,
as it is on every `/v1` route.
The console never receives it, because a `fetch` is not a navigation.
No response of the four names a Pod, a Pod IP, a node, or a port a Pod declares;
the targets response of the gateway (*Targets, with reasons*) names Pods and nodes as it always has,
and no response on any route the console reads names a Pod IP or a port a Pod declares.

### 3.1 Namespaces

```http
GET /v1/namespaces
```

```json
{"namespaces": ["orders", "payments"]}
```

### 3.2 Services

```http
GET /v1/namespaces/payments/services
```

```json
{"namespace": "payments", "services": ["checkout", "ledger"]}
```

### 3.3 Who am I

```http
GET /v1/whoami
```

```json
{
  "principal": "alice",
  "realm": {
    "name": "payments-dev",
    "namespaces": ["payments"],
    "services": ["*"],
    "profiles": ["cpu", "heap", "goroutine"],
    "pgo": {"read": true, "collect": false, "configure": false}
  },
  "auth": {"mode": "oidc", "logout": "/auth/logout"}
}
```

`realm` is the caller's realm exactly as configured — the wildcard as `"*"`, explicit names as written —
so the page can say what the caller may ask for before it asks.
It is the caller's own realm and discloses nothing about any other.
`auth.mode` is one of `disabled`, `basic`, `oidc`;
`auth.logout` is present only when the browser flow is configured and is always `/auth/logout`,
so the page shows a logout link exactly when one would do something.
Under `disabled` the principal is `anonymous`, as everywhere else.

### 3.4 Limits

```http
GET /v1/limits
```

```json
{
  "cpuSeconds": 60,
  "traceSeconds": 60,
  "profiles": ["cpu", "trace", "heap", "allocs", "goroutine", "mutex", "block", "threadcreate"],
  "pprof": {
    "default": {"port": 6060},
    "allowedSelections": [{"port": 6061}, {"portName": "pprof-alt"}]
  },
  "pgo": {"enabled": true}
}
```

`cpuSeconds` and `traceSeconds` are `limits.cpuSeconds` and `limits.traceSeconds`,
so the page can bound its duration input instead of learning the bound from `400 seconds_exceeds_limit`.
`profiles` is the eight profile names in the gateway's order, before the realm filters them;
the page applies `realm.profiles` from `/v1/whoami` to it as *The realm filter* says.
`pprof.default` is `{"port": N}` or `{"portName": "name"}`, whichever `discovery.pprof` sets.
`pprof.allowedSelections` is `discovery.pprof.allowedSelections` in its configured order,
each entry `{"port": N}`, `{"portName": "name"}`, `{"port": "*"}`, or `{"portName": "*"}`,
and `[]` when the list is empty, which leaves the page nothing to offer beyond the default (*Controls*).
`pgo.enabled` is `pgo.enabled`, and is how the page learns whether PGO collection is enabled;
`/v1/whoami` says nothing about it.
The page offers the Collections view (*Collections, read-only*) exactly when `pgo.enabled` is `true`
and `realm.pgo.read` from `/v1/whoami` is `true`.

**`/v1/limits` deliberately discloses these values to every caller the configured `auth.mode` admits.**
The response carries `allowedSelections` and the default themselves,
not a marker that a value exists, so the page can offer them as choices;
that is a decision to disclose, and the argument for it is this.
A caller could already learn, by sending a value and reading `400 port_not_allowed`,
whether that one value is admitted (gateway *Non-disclosure*, third observation);
what the caller could not learn by probing is the full list without guessing its members,
and which permitted value is the default when several are permitted.
`/v1/limits` gives both.
The exposure is acceptable because the values are global operator configuration and not cluster state:
they say which values any client may name, not which port any Pod exposes,
and knowing them grants nothing —
every profile request the caller can make with the list in hand is one the caller could make without it,
bounded by the same realm, the same `allowedSelections`, and the same NetworkPolicy.
What stays hidden stays hidden:
the number a `portName` resolves to on a particular Pod, and which Pods declare which names,
are Pod state and are not in this response.
Under `basic` and `oidc` the endpoint answers only a caller who authenticated,
so a probe without a credential learns nothing but `401`.
Under `disabled` the gateway authenticates nobody:
this response, like every other `/v1` response, is served to anonymous callers on `auth.anonymousRealm`,
which is what that mode means and not an exception this endpoint makes.
The gateway *Non-disclosure* section gains this as a fourth listed observation (*Changes to the accepted designs*).

### 3.5 Collections, read-only

The read-only Collections view is part of the first console,
shown only when `/v1/limits` reports `pgo.enabled` and `/v1/whoami` reports `realm.pgo.read`.
The console lists Collections through the existing `GET /v1/namespaces/{namespace}/services/{service}/collections`
and reads one through `GET /v1/collections/{id}`, both defined in [`pgo.md`](pgo.md) *HTTP API*,
and downloads a finished artifact by navigating to `GET /v1/collections/{id}/profile`.
It sends no `POST`, `PUT`, or `DELETE`.
`state` is a closed set and `origin` and `reason` are open sets, as that document says;
the page shows an unrecognized `origin` or `reason` verbatim, as text, rather than failing on it.
What the view shows and when the download link appears is in *Controls*.

### 3.6 Targets, with reasons

```http
GET /v1/namespaces/{namespace}/services/{service}/targets?explain=true
```

The targets endpoint is the gateway's rather than one of the four,
and the gateway *List targets* section defines every field of its response.
The console sends `explain=true` on every targets fetch, not only when it expects an empty list:
on a gateway that accepts `explain`, one request then answers both which Pods can be profiled and why none can,
and a request sent after seeing an empty list would read a cache that has moved.
It sends the port selection beside it (*Controls*) and never `version=` or `pod=`,
because those two controls are filled from this response
and sending them back would narrow the choices it offers.

Beyond `targets` the page reads `selectorMatched`, the Pods the Service selects before eligibility,
and `excluded`, the reasons with a non-zero count in the gateway's vocabulary order.
The response carries no other field the page reads.

`reason` is a closed set the gateway writes from its own vocabulary (gateway *Eligibility*),
never a value a caller sent, and `count` is a number,
so those two are the values the page can recognize in full rather than merely render.
The rest of the body is the ordinary target listing —
namespace, Service, Pod, node, and version strings —
which the page renders as text like every other response value (*Rendering response values*),
and a `reason` outside the vocabulary is rendered as text for exactly that reason.

**A replica that does not know the parameter.**
Mid-rollout a targets fetch can reach a gateway older than this design,
which answers `explain=true` with `400 invalid_parameter`,
the answer it gives any parameter it does not know.
The page retries that one fetch once, without `explain` and with the port selection unchanged,
and renders the plain body the retry returns.
The rule is narrow enough to state without exceptions:
once per fetch, on the targets route only, only for a request that carried `explain=true`,
and only on a `400` whose envelope `code` is `invalid_parameter`.
Any other status, any other code, and a second failure are ordinary errors and reach *Errors* unchanged.
A `400 invalid_parameter` a current gateway earned on the port selection is retried too
and fails the same way the second time,
which costs one request and keeps the rule from needing a second clause.

---

## 4. The page

### 4.1 Flow

```text
/ui/?ns=payments&svc=checkout
  |
  |-- GET /v1/whoami ------> principal, realm, auth mode; "not signed in" -> Signing in and out
  |-- GET /v1/limits ------> duration bounds, port choices, pgo.enabled
  |-- GET /v1/namespaces --> namespace <select>
  |         (ns chosen)
  |-- GET /v1/namespaces/{ns}/services ----------------> service <select>
  |         (svc chosen)
  |-- GET .../services/{svc}/targets?explain=true[&port=|&portName=] --> pod <select>, versions, empty state
  |-- GET .../collections   (only when pgo.enabled and realm.pgo.read)     --> collections table
  |
  |-- profile <select> from limits.profiles filtered by realm.profiles
  |-- seconds <input>, 1..limit, shown for cpu and trace only
  |-- pod <select>, version <select>, port <select> and/or <input>
  v
  URL = /v1/namespaces/{ns}/services/{svc}/profiles/{profile}?[seconds=&pod=&version=&port=|portName=]
  [Download]  = <a href=URL download>         a navigation; the browser saves the file
  [Copy URL]  = absolute URL on the clipboard  for `go tool pprof <url>`; the URL is also shown as text
```

The selection lives in the page's query string, `?ns=&svc=`, and nothing else is remembered:
the browser flow seals the return path as path plus query and drops the fragment
([`auth.md`](auth.md) *Wire values and bounds*),
so state kept in `#fragment` would not survive a login round trip and state kept in the query does.
The return path the page sends is `/ui/?ns=<label>&svc=<label>&returned=1`:
two DNS-1123 labels and a fixed marker keep it far under the 1024-byte bound that document checks before decoding,
and it holds no `.` or `..` segment, so it is sealed as sent and never replaced by `/`.
The `returned` marker is how the page tells a return from login from a plain load (*Signing in and out*);
it holds no credential and no state, and the page drops it from the address bar as soon as it has read it.
A reload, a bookmark, and a return from login all land on the same selection.

The download is an ordinary navigation to the profile endpoint.
It carries the session cookie with `Sec-Fetch-Site: same-origin` under `oidc`,
the browser's remembered credential under `basic`, and nothing under `disabled`,
and it runs the full interactive request algorithm, admission and confirmation included.
The response's `Content-Disposition`, which Go's pprof handler sets and the gateway passes through,
is what makes the browser save rather than display it.
The page cannot read `X-Pprof-Target-*` on a navigation,
so a user who wants to know which Pod served a profile picks the Pod explicitly from the targets list.

The copied URL is the profile URL made absolute with the page's own origin, and carries no credential.
Under `disabled` it works as is;
under `basic` the user adds `user:password@` or uses `curl -u`,
which [`auth.md`](auth.md) *Clients* documents with its caveats;
under `oidc` it works only with a bearer token that `go tool pprof` cannot send,
which is the asymmetry that document's first core decision records rather than works around.
The page says which of the three applies, from `auth.mode`, next to the button.
`navigator.clipboard` exists only in a secure context,
and a gateway under `disabled`, or under `basic` with plaintext explicitly permitted, can serve the page over HTTP;
the page therefore always shows the URL in a read-only `<input>` the user can select and copy by hand,
feature-detects `navigator.clipboard.writeText`, and renders the copy button only when it exists.

### 4.2 Controls

The controls, their defaults, and what each change does,
so the behavior is a contract and not an implementation guess:

| Control | Choices | Default | Sent as | On change |
|---|---|---|---|---|
| namespace | `/v1/namespaces` | the page's `ns`, when listed; else none | page query `ns` | fetch the Service list; clear Service, Pod, version, Collections |
| Service | `/v1/namespaces/{ns}/services` | the page's `svc`, when listed; else none | page query `svc` | fetch targets and, when offered, Collections; clear Pod and version |
| profile | `limits.profiles` filtered by `realm.profiles` | `cpu` when offered, else the first offered | path segment | show or hide the duration input |
| seconds | integer, `1` to the profile's limit | the gateway's upstream default (`30` for `cpu`, `1` for `trace`), or the limit when it is lower | `seconds=`, always sent for `cpu` and `trace` | none |
| port | see below | `default` | `port=` or `portName=`, nothing for `default` | refetch targets; clear Pod and version |
| Pod | `targets[].pod` | `any` | `pod=`, nothing for `any` | none |
| version | the distinct `targets[].version` values | `any` | `version=`, nothing for `any` | none |

`seconds` is always sent explicitly for `cpu` and `trace`,
so the request never depends on an upstream default that could exceed the configured limit;
the input's `min` is `1` and its `max` is the profile's limit,
and a value outside that range disables the download link and names the bound next to the input.

The port control follows `allowedSelections` (gateway *Port resolution*).
Its rules are two pure functions in `portmodel.js` —
one derives the control from `/v1/limits`, the other turns the control's state into what the request sends —
and they are the one part of the page a test executes (*What is not proven*):

- A `<select>` always exists.
  It offers `default`, which sends nothing and resolves under `pprof.default`,
  then every `{"port": N}` and `{"portName": "name"}` entry of `allowedSelections`, in the configured order.
  An entry equal to `pprof.default` is left out, because `default` already sends it
  and two options with one effect are a menu that invites a wrong guess about the difference.
  Equality is per kind and per value:
  a `{"portName": "6060"}` entry never equals a `{"port": 6060}` default, and is offered.
  `/v1/limits` still reports the suppressed entry, because that response is the gateway's configuration
  and not the menu the page built from it.
  A wildcard entry is not an option in the menu; it is what puts a field beside the menu.
- A free-form `<input>` exists beside the select only for the kind whose wildcard is present:
  a numeric field when `allowedSelections` holds `{"port": "*"}`,
  a name field when it holds `{"portName": "*"}`.
  Without the matching wildcard the menu is the whole of what the gateway accepts,
  so the page offers no field whose every typed value would earn a `400 port_not_allowed`.
  An empty `allowedSelections` therefore leaves `default` as the only choice and shows no field.
- One selection is sent, and the control it came from decides the parameter, never the value it holds.
  The numeric field sends `port=` and the name field sends `portName=`;
  a menu entry sends the parameter for its own key, `port=` for `{"port": N}` and `portName=` for `{"portName": "name"}`.
  So `123` typed in the name field is sent as `portName=123` and earns `400 invalid_parameter`,
  which is the answer the caller asked for,
  rather than turning into a numeric selection the caller did not ask for —
  a difference that matters most when both wildcards are present and both fields exist.
  A non-empty free-form field wins over the select;
  typing in one free-form field clears the other, so `port` and `portName` are never sent together.
  The gateway validates the grammar and answers `400 invalid_parameter` for a value outside it.

**The empty state.**
When `targets` is empty, the Pod and version controls have nothing to offer,
and the page puts the reasons the response counted in their place:
one row per `excluded` entry, in the order the gateway sent them, each row the count beside a fixed wording.

| `reason` | Wording |
|---|---|
| `pod_terminating` | Pods being deleted |
| `pod_not_running` | Pods not in phase Running |
| `pod_not_ready` | Pods whose Ready condition is not True |
| `endpoint_missing` | Pods with no trusted EndpointSlice entry naming the current Pod identity |
| `endpoint_not_ready` | Pods whose EndpointSlice entry is not ready |
| `endpoint_address_mismatch` | Pods whose EndpointSlice address is not one the Pod holds |
| `endpoint_address_conflict` | Pods whose EndpointSlice entries disagree on the address |
| `port_name_not_declared` | Pods declaring no TCP container port of the effective pprof port name |
| `version_mismatch` | Pods carrying another version |
| `pod_name_mismatch` | Pods with another name |

The wording is plural whatever the count is, so the page carries no grammar rule.
The last two rows are reached only by a request carrying `version=` or `pod=`,
which this page never sends on a targets fetch (*Targets, with reasons*);
they are in the table because the wording belongs to the vocabulary and not to who sent the request.
An empty `targets` with `selectorMatched` of `0` reads "the Service's selector matches no Pod" instead,
which is the one empty answer no reason describes;
an empty `targets` with a non-zero `selectorMatched` and an empty `excluded` cannot occur,
because the gateway's counts add up (gateway *List targets*).
A response with no `excluded` field at all leaves the plain empty state and no reasons:
that is the shape a fetch that fell back to no `explain` returns (*Targets, with reasons*),
and the shape to render if the field is ever absent for another reason.
An entry whose `reason` is not in the table is shown as that name, as text,
the way an unrecognized Collection `origin` is (*Collections, read-only*);
a rolling update is again the only way it arrives.

The rows above, and the query the page sends to get them,
are three pure functions in `targetmodel.js`, built and tested the way `portmodel.js` is:
one turns the port control's state into the targets query, `explain=true` included;
one decides whether a failed targets fetch is repeated without `explain`;
and one turns a targets response into the Pod menu, the version menu, and the empty state's ordered rows.
None of the three touches the DOM or the network, and a test executes all three (*Unit*).
The mapping is short, and every branch of it is a decision a reader does not see going wrong:
a row dropped because its reason is unrecognized rather than shown as text,
rows re-sorted out of the gateway's vocabulary order,
a `selectorMatched` of `0` rendered as a list of nothing,
or a query that quietly stopped asking for `explain`.

A bookmarked `ns` or `svc` that is not in the fetched list — the realm changed, the Service went away,
the label was typed by hand — leaves the control with no selection and shows
"`<value>` is not listed" beside it, as text;
the page query keeps the value until the user picks another, so a reload retries it.
The other controls are per-load state and are never serialized.

The Collections view, when offered, is a table of the list entries [`pgo.md`](pgo.md) *List Collections* returns,
newest first as returned, with the columns
`id`, `origin`, `state`, `attempt`, `resolvedVersion`, `createdAt`, `finishedAt`, and `expiresAt`.
Selecting a row fetches `GET /v1/collections/{id}` and shows the record's
`state`, `reason`, `progress`, `createdBy`, `createdAt`, `startedAt`, `finishedAt`, `expiresAt`,
and `artifact.bytes`, as labelled text.
The download link, `GET /v1/collections/{id}/profile`, is rendered only from the detail record,
and only when its `state` is `completed` and its `artifact` is not `null`;
the list entry carries no `artifact` field, so a row alone never offers a download.
Every other state shows no link, and `reason` beside `failed` and `cancelled`.
An `id` is placed in a path only after it matches the identifier grammar of [`pgo.md`](pgo.md) *Identifier*;
a record whose `id` does not is shown and not linked.

### 4.3 Rendering response values

The page renders values it did not write — principal, realm entries, namespace and Service names,
Pod names, versions, error messages, Collection fields, `origin`, `reason` —
and an OIDC principal or a PGO `reason` can hold any character.
These rules are normative, and the source-scan unit test (*Unit*) enforces the ones a scan can:

1. **Every response-derived value is a Preact text child.**
   It is interpolated into an htm template as `${value}` in child position or as an attribute value,
   which Preact sets through `textContent` or the DOM property and never parses as markup.
2. **No HTML injection interface.**
   `innerHTML`, `outerHTML`, `dangerouslySetInnerHTML`, `insertAdjacentHTML`, `document.write`,
   and `DOMParser` are forbidden in `app.js`;
   the scan fails on any of those names.
   The vendored Preact module contains `innerHTML` in the `dangerouslySetInnerHTML` code path
   and is exempt from that one token by path, and only that one.
3. **URLs are built, not concatenated.**
   Every URL the page fetches, navigates to, or offers as a link is built with `new URL(path, location.origin)`,
   with each path segment passed through `encodeURIComponent`
   and every query built with `URLSearchParams`.
   The page never places a response string into a `href` or a `fetch` argument by string concatenation;
   the scan fails on a template literal or a `+` expression whose left operand is a string starting with `/v1`,
   `/ui`, or `/auth` outside the one URL-building module, `urls.js`.
4. **Every link stays on the page's origin.**
   The page has no link to another origin;
   `href` values are built by rule 3, and `Referrer-Policy: no-referrer` covers any link it gains.

Together with the policy of *Response headers and CSP*, the rules hold even if one is broken:
a value that reached the DOM as markup could load no script and no stylesheet,
and could start no request but an `img` or a form submission,
which `img-src data:` and `form-action 'none'` close.

### 4.4 Signing in and out

The page never holds a credential.
It calls `fetch` with `credentials: 'same-origin'` and lets the browser attach what it has:
the `__Host-profgate_session` cookie under `oidc`,
a remembered Basic credential under `basic`,
nothing under `disabled`.
Every `fetch` is initiated by the page,
so it carries `Sec-Fetch-Site: same-origin` and `Sec-Fetch-Mode: cors` or `same-origin`,
which is what [`auth.md`](auth.md) *What is redirected* calls a `fetch`-shaped request:
it is never redirected and answers `401` when it is not signed in.

Under **`oidc`**, the page calls `/v1/whoami` first on every load.
A `401` from it, or from any later `fetch`, makes the page navigate to
`/auth/login?return=<path and query of the page, plus returned=1>`;
the callback answers `200` with a landing page,
its `<meta http-equiv="refresh">` sends the browser back to the same selection,
and that navigation starts from the gateway's own document and arrives `same-origin`
([`auth.md`](auth.md) *The `/auth/` routes*).
The page navigates at most once per load, and never on a load whose query carries the `returned` marker:
that load is the return from a login, and a `401` on it — the session the callback set was not accepted,
or the login never completed — is shown as "sign in required" with a **Sign in again** button
that starts another login only when the user presses it,
because a loop between a page and a login that keeps failing is worse than a message the user can read.
The page drops the marker from the address bar with `history.replaceState` before its first request,
so a reload is a plain load that may navigate again,
and a bookmark taken after that first request carries no marker.
Only the exact value `returned=1` is the marker;
a URL that still carries it — bookmarked before the page stripped it, or typed by hand —
loads into the same "sign in required" state, which the **Sign in again** button leaves.
The marker is the only thing the page reads from its query besides the selection;
it uses no `sessionStorage` or `localStorage`, so "nothing else is remembered" (*Flow*) stays true.
The console requires the browser flow under `oidc`:
without it every browser request is `401` and `/auth/login` is `404 route_unknown`,
so a configuration that sets `ui.enabled` under `auth.mode: oidc` without an `auth.oidc.browser` block is rejected
(*Configuration*).
The logout link is `/auth/logout` and is shown exactly when `/v1/whoami` returned `auth.logout`;
the gateway answers it with `302` to the issuer's end-session endpoint or to `/`,
and `/` is `302` to `/ui/` while `ui.enabled` (*Routes*), so logout lands back on a signed-out console.

Under **`basic`**, the browser's own dialog is the login.
When a same-origin `fetch` receives `401` with `WWW-Authenticate: Basic realm="profgate"`,
the browser prompts for a name and password and retries the request with them,
and it keeps sending that credential on later requests to the same origin and protection space,
navigations included — the download link therefore works after the first prompt.
This is the Fetch standard's behavior for a same-origin request with `credentials: 'same-origin'`,
and the browsers named in [`auth.md`](auth.md) *Non-goals* implement it.
A user who cancels the dialog leaves the `fetch` with a `401`,
which the page shows as "sign in required" with a button that retries the request, prompting again.
There is no logout under `basic`, and the page says so where the logout link would be:
how long a browser keeps a Basic credential is the browser's decision —
some forget it when the last window closes, some sooner, some later —
and neither the gateway nor the page can end it, so neither promises to.

Under **`disabled`**, every request is `anonymous` in `auth.anonymousRealm`,
and the page shows that principal and realm with no sign-in or sign-out control.

### 4.5 Errors

The page shows every gateway error as its `code` and `error` from the envelope, as text,
plus a one-line hint for the codes a user can act on:

| `code` | Hint |
|---|---|
| `not_ready` | the gateway is still syncing; the page retries every 2 seconds |
| `realm_denied` | your realm does not admit this; the whoami panel shows what it does |
| `service_not_found` | the Service left the cache since the list was fetched; the page refreshes the Service list |
| `no_targets` | no Ready Pod declares the selected port |
| `port_not_allowed` | `allowedSelections` does not admit the value; the port control shows what it does admit |
| `seconds_exceeds_limit` | the limit the duration input was bounded by |
| `discovery_unavailable` | the gateway could not read its cache or confirm the Pod; retry |
| `pgo_disabled`, `pgo_unavailable` | the Collections view is hidden or shows the code |

Every other code is shown as is.
The page never rewrites a message the gateway generated, and never shows a message the gateway did not send.

A failed `fetch` does not always carry the envelope.
An Ingress can answer with its own HTML, a connection can drop mid-body, a proxy can strip the body,
and a gateway of another version can answer a shape this page does not know.
The page reads the body as the envelope only when the response's `Content-Type` is `application/json`
and the body decodes to an object with string `error` and `code` fields;
otherwise it shows `HTTP <status> <statusText>` and nothing from the body.
A rejected `fetch` — no response at all — shows "request failed" with a retry button.
In every case what reaches the DOM is text under the rules of *Rendering response values*;
a response body is never shown as HTML.

---

## 5. Static assets

### 5.1 Layout and embedding

```text
internal/ui/static/
  index.html                 the shell: <link> to the stylesheet, <script type="module"> to app.js, one <main>
  app.js                     the console; an ES module importing ./urls.js, ./portmodel.js, ./targetmodel.js,
                             ./vendor/preact/preact.module.js, and ./vendor/htm/htm.module.js
  urls.js                    the URL builders of Rendering response values; the only module that spells a /v1 path
  portmodel.js               the port control's two pure functions, importing nothing so a test can evaluate them
  targetmodel.js             the targets query, the retry rule, and the target summary, importing nothing either
  app.css                    the console's own rules, a few dozen lines on top of Pico
  vendor/
    MANIFEST                 one line per file: name, version, license, source URL, SHA-256
    preact/
      preact.module.js       Preact, the ES module build
      LICENSE                MIT
    htm/
      htm.module.js          htm, the standalone ES module build
      LICENSE                Apache-2.0; htm ships no NOTICE, and one is vendored here if a release adds it
    pico/
      pico.classless.min.css Pico CSS, the class-less build, under its published name
      LICENSE                MIT
```

`internal/ui` embeds the directory with `//go:embed static`.
Its constructor, not an `init` function (`200-coding-standards.md` forbids `init`),
walks the embedded tree once, hashes it, and renders the shell:

- The **tree hash** is the SHA-256 over every embedded file in path order,
  each framed as its length-prefixed path followed by its length-prefixed content,
  so no two trees share a hash by shifting bytes between a path and a file;
  the digest is truncated to its first 16 hex digits.
  `index.html` is hashed with the rest and has no hashed serving route:
  it is served only as the rendered shell at `/ui/`.
  Every asset is served under `/ui/static/<tree hash>/<path>`,
  so a change to any file changes every asset URL and a page never mixes files from two builds.
  Hashing the tree rather than each file keeps the relative `import` paths inside the modules valid as written;
  hashing per file would mean rewriting them.
- The **shell** is `index.html` with its two placeholders — the stylesheet path and the script path —
  replaced by the hashed paths.
  It is rendered once and held in memory;
  no template runs per request.
- Any path under `/ui/static/` whose hash segment is not this binary's, or whose file does not exist,
  is `404 route_unknown`.
  During a rolling update the replicas serve two hashes,
  and every request a page makes — the shell, the script, the stylesheet, each module — may land on either build;
  a page can therefore fail to load, and fail again on reload, until the rollout converges,
  which *Failure scenarios* records.

The shell references exactly two assets; every other file loads through a relative `import` from `app.js`
or a relative `url()` from the stylesheet.
There is no import map:
an import map is an inline script under CSP,
and the vendored modules are chosen so none uses a bare specifier (*Vendoring rule*).

### 5.2 Headers

| Path | `Cache-Control` | `Content-Type` |
|---|---|---|
| `/ui/` | `no-store` | `text/html; charset=utf-8` |
| `/ui/static/<hash>/*.js` | `public, max-age=31536000, immutable` | `text/javascript; charset=utf-8` |
| `/ui/static/<hash>/*.css` | `public, max-age=31536000, immutable` | `text/css; charset=utf-8` |
| `/ui/static/<hash>/*` (other) | `public, max-age=31536000, immutable` | by extension, `application/octet-stream` otherwise |

The shell is `no-store` because it is the one file whose content changes what the browser fetches next;
the hashed assets are immutable because their URL changes when they do.
`ETag` and `Last-Modified` are not set:
an immutable asset is never revalidated and the shell is never cached.
`Content-Length` is set from the embedded size, and `HEAD` is answered as `GET` without a body (*Routes*).
The `Content-Type` for a module script is `text/javascript`, which is what a browser requires for `type="module"`.

### 5.3 Vendoring rule

A vendored file is accepted only when:

1. it is the upstream's published ES module or CSS build, byte for byte, with its SHA-256 recorded in `MANIFEST`;
2. every `import` in it names a relative path, so it resolves under the hashed prefix without an import map;
3. its license text, and its `NOTICE` when the project ships one,
   sit in its directory under `vendor/` and its license is recorded in `MANIFEST`.

Rule 2 is why the vendored set is Preact's core module and htm's standalone module,
and not `preact/hooks` or `htm/preact`,
whose published builds import `preact` by bare specifier.
The console therefore uses Preact class components for state and binds htm to Preact's `h` itself;
a thin console needs nothing hooks give.
A unit test enforces rules 1 and 2 (*Unit*).
Updating a vendored file is a commit that changes the file, the manifest line,
and the license or `NOTICE` text when the release changed it.

---

## 6. Response headers and CSP

Every response under `/ui/` carries:

| Header | Value |
|---|---|
| `Content-Security-Policy` | `default-src 'none'; script-src 'self'; style-src 'self'; img-src data:; connect-src 'self'; font-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; object-src 'none'` |
| `X-Frame-Options` | `DENY` |
| `X-Content-Type-Options` | `nosniff` |
| `Referrer-Policy` | `no-referrer` |
| `Cross-Origin-Opener-Policy` | `same-origin` |
| `Cross-Origin-Resource-Policy` | `same-origin` |
| `Cache-Control` | *Headers* |

`default-src 'none'` with the sources the page needs listed by name,
so a directive nobody listed cannot fall back to something broader,
and each listed source is the narrowest the finished page needs:

- `img-src data:` and not `'self'`.
  The page has no `<img>`;
  the only images are the `data:` SVG backgrounds inside Pico's stylesheet, and `img-src` governs those.
  Leaving `'self'` out means a stray `<img src="/v1/...">` cannot issue an authenticated `GET`,
  which the session cookie and `Sec-Fetch-Site: same-origin` would otherwise let through.
- `form-action 'none'`.
  The page submits no form: the download is an `<a>`, the retry is a button with a listener.
  A stray `<form action=...>` can therefore submit nowhere.
- `connect-src 'self'` is what `fetch` needs and the only network the page has.

**No inline anything.**
The shell has no inline `<script>`, no `<style>` element, no `style=` attribute, and no `on*=` attribute;
`app.js` renders no `style` prop and attaches handlers through Preact's properties,
which are DOM event listeners and not attributes.
Pico CSS is a stylesheet and needs no inline style;
its theme switch is a `data-theme` attribute, which CSP does not govern.
htm is a tagged-template parser and uses no `eval` or `Function`,
and Preact needs neither,
so `script-src 'self'` without `'unsafe-inline'` or `'unsafe-eval'` is sufficient.
A unit test scans the shell and the vendored files for the four inline forms and for `eval(` and `new Function(`,
so an upgrade that introduced one fails before it ships.

`Referrer-Policy: no-referrer` matters because the page's URLs carry namespace and Service names,
and a link out of the page would otherwise send them to wherever it points;
the page has no links out, and the header makes that true of any it gains.
`X-Frame-Options` duplicates `frame-ancestors` for the browsers that read only one.
`Cross-Origin-Resource-Policy` on the assets keeps another origin from loading the console's script as its own resource.

The `/v1` responses are unchanged:
`Cache-Control: no-store` and `Content-Type: application/json`, as the gateway *HTTP API* section states.
They gain no CORS header; the console is same-origin by construction.

---

## 7. Errors

The listing endpoints reuse the gateway's codes with their meanings:

| Status | `code` | When |
|---|---|---|
| 400 | `invalid_parameter` | any query parameter, or `access_token` in the query |
| 401 | `unauthenticated` | [`auth.md`](auth.md) *Failure responses* |
| 403 | `realm_denied` | Service list for a namespace the realm denies; identical for a present and an absent namespace |
| 404 | `route_unknown` | a `{namespace}` that is not a DNS-1123 label; any `/ui/` path this build does not serve |
| 405 | `method_not_allowed` | `Allow: GET` on the four `/v1` routes; `Allow: GET, HEAD` under `/ui/` and on `/` |
| 429 | `too_many_auth` | [`auth.md`](auth.md) *`basic` mode* |
| 503 | `not_ready`, `auth_unavailable` | as on every `/v1` route |
| 503 | `discovery_unavailable` | `Catalog` returned an error on the namespace or Service list; never an empty `200` |

No new code is needed.
`404 route_unknown` covers every `/ui/` miss, a stale hash included,
because from the gateway's side a URL from another build is a route it does not have.
`/ui/` routes never generate the JSON envelope for a success and always do for a failure,
so a fetch of a missing asset reads the same envelope every gateway error carries.

---

## 8. Audit and metrics

**Audit.**
Each of the four listing routes writes the gateway *Logging* record on completion:
`principal`, `namespace` (the Service list only), empty `service`, `pod`, `profile`, `port`, zero `seconds`,
`status`, `code`, and `duration_ms`.
`auth_reason` appears on authentication failures as it does elsewhere.
Requests under `/ui/` and to `/` write no audit line:
they carry no principal, name nothing a realm bounds, and one page load is several of them;
they are counted, not narrated.

**Metrics.**
`profgate_requests_total{endpoint,profile,code}` gains `endpoint` values
`namespaces`, `services`, `whoami`, `limits`, and `ui`,
with `profile` fixed to `none` for all five.
`ui` covers `/ui/`, every path under it, and `/`;
its `code` is `ok` for a `200` or the `302`, `route_unknown`, `method_not_allowed`,
or `internal_error` for any status the console wrote outside `2xx`, `3xx`, `404`, and `405`,
derived by `internal/httpapi` from the status the console handler wrote (*Package layout*),
and its histogram bucket is the first one in practice.
The set is closed at those four values.
The tree hash is not a label and neither is a file name:
`code` and `endpoint` stay closed sets, and the label cardinality rule of the gateway *Metrics* section holds.

---

## 9. Failure scenarios

| Event | Behavior |
|---|---|
| `ui.enabled` false | `/ui/` and `/` are `404 route_unknown`; the four listing routes still answer |
| Gateway not yet synced | the shell loads; every `fetch` is `503 not_ready`; the page retries every 2 seconds until the first `200` |
| Session cookie expires while the page is open | the next `fetch` is `401`; the page navigates to `/auth/login?return=` with its current query, and the callback's landing page brings it back to the same selection |
| Login fails after the redirect | the callback answers `401` as a page; nothing on the console side loops back to login |
| Basic dialog cancelled | the `fetch` resolves `401`; the page shows "sign in required" and a retry button that prompts again |
| Issuer token endpoint down | login answers `503 auth_unavailable` on the callback; existing sessions keep working |
| Rolling update in progress | each request a page makes may reach either build; an asset the answering replica lacks is `404 route_unknown` and the page does not render; a reload can fail the same way; the console is unavailable for some loads until the rollout converges and every replica serves one hash, after which a reload recovers |
| Service deleted between listing and download | `404 service_not_found` from the profile endpoint; the page refreshes the Service list |
| Service with no eligible target | the Pod and version controls are replaced by the counted reasons of *Controls*, in the order the gateway sent them |
| Service whose selector matches no Pod | the same empty state says the selector matches no Pod, from `selectorMatched` of `0`, and lists no reason |
| Targets fetch refused `400 invalid_parameter` by a replica older than this design | retried once without `explain`, keeping the port selection; the plain body renders with no reasons and no error, and a second failure is an ordinary error |
| Namespace in the realm holds no Service | absent from the namespace list; its Service list is `200` with `[]`; `/v1/whoami` still names it |
| Namespace in the realm whose Services are all outside `realm.services` | absent from the namespace list; its Service list is `200` with `[]` |
| Namespace not in the realm | absent from the namespace list; its Service list is `403 realm_denied`, present or not, and `Catalog` is not called |
| Cache read fails on a listing route | `503 discovery_unavailable`; never an empty `200` |
| Kubernetes API unreachable while running | listings serve the cache unchanged; downloads fail at confirmation with `503 discovery_unavailable`, as the gateway *Failure Scenarios* section states |
| Page served over HTTP (`disabled`, or `basic` with plaintext permitted) | `navigator.clipboard` is absent in an insecure context; the copy button is not rendered and the URL is shown for manual copying |
| A `fetch` answers without the envelope: HTML from an Ingress, an empty body, truncated JSON | the page shows `HTTP <status> <statusText>` and nothing from the body |
| A `fetch` is rejected: network failure, connection reset | the page shows "request failed" with a retry button |
| A vendored file is edited or replaced | the manifest hash test fails; the tree hash changes and every asset URL with it |

---

## 10. Configuration

Added rows in the gateway *Configuration* table:

| Key | Env | Default | Reload | Validation |
|---|---|---|---|---|
| `ui.enabled` | `PROFGATE_UI_ENABLED` | `false` | restart | boolean; under `auth.mode: oidc` requires `auth.oidc.browser` |

`ui.enabled` is restart-only because it decides which routes the handler registers,
like `server.listen` and unlike a realm.
The `oidc` rule exists because a console that cannot log a browser in serves nobody (*Signing in and out*);
`basic` and `disabled` need nothing.
Nothing else is configurable:
the path is `/ui/`, the assets are the embedded ones, and the theme follows the browser.
An operator whose Ingress routes only `/v1` adds `/ui/`, `/auth/`, and `/` to it;
the Helm chart offers an Ingress template, off by default,
routing `/`, `/ui/`, `/auth/`, and `/v1/` to the API port,
and the NetworkPolicy, Service, and container security context do not change.

The Helm chart gains a top-level `ui.enabled` value, default `false`,
beside `tls.enabled`, `pgo.enabled`, and `auth.mode`,
and renders it into the ConfigMap so `checksum/config` moves when it changes and the Deployment rolls.
The raw `config:` block refuses `config.ui.enabled` at render time
and requires `config.ui`, when present, to be a mapping,
with the same guard the chart applies to `pgo.enabled` and the `tls` and `auth` file paths:
a value arriving through the raw block would bypass the structured value the chart's own text documents.

---

## 11. Testing

### 11.1 Unit

`internal/ui`, against the embedded tree:

- the tree hash is stable across two constructions and changes when one byte of one file changes;
- `/ui/` serves the shell with every header of *Response headers and CSP* and `Cache-Control: no-store`;
- each asset serves under the current hash with its `Content-Type`, `Content-Length`, and the immutable `Cache-Control`;
  the same path under another hash is `404 route_unknown`,
  and so is a path that traverses (`..`, an absolute path, a backslash);
  `HEAD` answers the headers without a body; `POST` is `405` with `Allow: GET, HEAD`;
- every vendored file's SHA-256 equals its `MANIFEST` line, and every file in `vendor/` has a line;
- no vendored module, `app.js`, or `urls.js` contains an `import` or dynamic `import(`
  whose specifier does not start with `./` or `../`,
  and `portmodel.js` and `targetmodel.js` contain neither at all;
- the shell and every `.js` file contain no `<script>` with a body, no `<style>`, no `style=`, no `on[a-z]+=`,
  no `eval(`, and no `new Function(`;
- the source scan of *Rendering response values*:
  `app.js`, `urls.js`, `portmodel.js`, and `targetmodel.js` contain none of `innerHTML`, `outerHTML`,
  `dangerouslySetInnerHTML`, `insertAdjacentHTML`, `document.write`, or `DOMParser`;
  `app.js` contains no string literal beginning with `/v1`, `/ui`, or `/auth`;
  `urls.js` contains `encodeURIComponent` and `URLSearchParams` and no `+` between a string literal and an expression;
  the test runs against fixture strings that must fail as well as against the real files,
  so a scan that matches nothing is itself caught;
- `targetmodel.js` contains every reason name of the gateway's exclusion vocabulary
  (gateway *Eligibility*) as a literal,
  the ten names written out in the Go test rather than derived from the page,
  so a reason added to the gateway without wording on the console turns the suite red.

`internal/ui`, against the port-control model:

- `portmodel.js` imports nothing, declares plain functions,
  and ends in a single `export { ... }` statement with no other export;
  a test asserts that shape,
  so an `import` added later turns the suite red instead of quietly making the model unloadable.
- a table-driven test drives both functions in a pure-Go ECMAScript interpreter (gateway *Dependencies*).
  The interpreter has no module loader and cannot parse `export`,
  so the test evaluates the source with that one trailing statement cut off
  and reads the functions as globals;
  the asserted shape above is what makes cutting it safe.
  Deriving the control, run against a numeric `pprof.default` and against a named one:
  an empty `allowedSelections` offers `default` alone and shows no field;
  concrete entries of each kind become one option each in the configured order, and show no field;
  `{"port": "*"}` alone shows the numeric field and no name field;
  `{"portName": "*"}` alone shows the name field and no numeric field;
  both wildcards show both fields;
  a wildcard is never itself an option;
  a concrete entry equal to the default is absent from the options
  while the `allowedSelections` handed in is unchanged, for a numeric default and for a named one;
  and a `{"portName": "6060"}` entry beside a `{"port": 6060}` default is offered, because kinds do not compare.
  Serializing the choice:
  `default` sends neither parameter;
  a `{"port": N}` option sends `port=` and a `{"portName": "name"}` option sends `portName=`;
  the numeric field sends `port=` and the name field sends `portName=`;
  `123` in the name field sends `portName=123` and never `port=123`;
  a non-empty field wins over the menu;
  and setting one field clears the other, so no input produces both parameters.

`internal/ui`, against the target-summary model:

- `targetmodel.js` satisfies the shape assertion `portmodel.js` does
  and is evaluated the same way, its one trailing `export` cut off and its functions read as globals.
- a table-driven test drives all three functions in the same interpreter.
  The query, over each state the port control can be in:
  `default` sends `explain=true` alone,
  a numeric selection sends `port=` beside it and a named one `portName=`,
  `explain=true` is present in every case,
  and neither `version=` nor `pod=` is ever sent.
  The retry rule: a `400` whose envelope `code` is `invalid_parameter`,
  on a fetch that carried `explain=true`, is retried once without it;
  a second failure is not retried;
  a `400` with another code, a `403`, a `404`, and a `503` are not retried;
  and a fetch that carried no `explain` is not retried.
  The summary, over a response with targets:
  the Pod menu holds each `targets[].pod` and the version menu the distinct `targets[].version` values,
  in the order the response listed them,
  and no empty state is produced even when `excluded` is non-empty.
  The summary, over a response with no targets:
  each `excluded` entry becomes one row of its count beside the wording of *Controls*;
  the ten reasons in the gateway's vocabulary order come back in that order,
  and the same ten shuffled come back in the order they arrived, never re-sorted;
  a `reason` the table does not hold becomes a row carrying that name as its own text, dropped by nothing;
  `selectorMatched` of `0` produces the selector message and no rows, whatever `excluded` holds;
  a body with no `excluded` field, and one whose `excluded` is `[]`,
  each produce the plain empty state and no rows;
  and a `count` of `1` reads the same plural wording as a count of `9`.

`internal/httpapi`, against the fake `Discovery` extended with a namespace and Service catalog:

- a table over the four routes and every step of *Request algorithm for the listing endpoints*:
  each method, `?access_token=`, readiness false, an unknown query parameter, a `{namespace}` that is not a label;
- realm filtering, over all four combinations of the *The realm filter* table and a fake that holds,
  in three namespaces, a Service the realm names and one it does not
  (the fake holds no selectorless Service: `ServiceRef` carries no selector,
  so what the HTTP layer receives is already selector-independent):
  each combination lists exactly the namespaces and Services the table says;
  a namespace whose only Services are outside `realm.services` is absent;
  an explicit realm omits a named namespace the cache lacks;
  the Service list of a denied namespace is `403` with an identical body whether the fake holds the namespace or not,
  and the fake records that `Catalog` was not called;
  the Service list of an admitted namespace the fake lacks is `200` with `[]`;
  a fake whose `Catalog` returns an error yields `503 discovery_unavailable` on both list routes;
  lists are sorted and empty ones encode as `[]`;
- `/v1/whoami` returns the configured lists verbatim, the wildcard included, the three `pgo` flags,
  `auth.mode`, and `auth.logout` only under the browser flow;
- `/v1/limits` reflects each of `cpuSeconds`, `traceSeconds`, a numeric default, a named default,
  and `allowedSelections` as an array of one-key objects — `[]` when the list is empty and never `null`,
  and carrying `{"port": "*"}` and `{"portName": "*"}` as configured;
- no Service or namespace listing exposes a Pod-discovered or selected backend port,
  and no listing response contains a string that matches an IP address or a `podIP` field,
  with the fake holding Pods that have both;
  `/v1/limits` returns `allowedSelections` and the default by design (*Limits*),
  and the two list responses are asserted to contain no `6060`;
- hostile names survive encoding:
  a principal, a namespace, and a Service name that carry HTML metacharacters (`<`, `>`, `&`, `"`, `'`)
  are JSON-escaped in the responses and decode to the configured strings unchanged,
  so the page's text rendering receives exactly the configured string;
- `ui.enabled` false: `/ui/`, `/ui/static/<hash>/app.js`, and `/` are `404 route_unknown`;
  true: `GET /` and `HEAD /` are `302` with `Location: /ui/`, the `HEAD` without a body;
- the audit line for each listing route carries the principal and, for the Service list, the namespace;
  `/ui/` writes none;
  the recorder sees `endpoint` `namespaces`, `services`, `whoami`, `limits`, or `ui` with `profile` `none`,
  and for `ui` the `code` `ok` on `200` and `302`, `route_unknown` on `404`, `method_not_allowed` on `405`,
  and `internal_error` on any other status the console wrote.

`internal/k8s`, against the fake clientset with real informers:

- `Catalog` reads only the Service lister;
  the recording transport of the gateway *Layers* section sees no request beyond the seven tuples when it runs;
- a Service without a selector is not listed, whatever its `spec.type`,
  and when it is the namespace's only Service that namespace yields no entry;
  selector presence is decided here and nowhere else;
  a Service that appears or disappears in the fake is reflected once the informer delivers it;
  `Catalog` with a namespace returns that namespace's entries and `[]` for one the cache lacks.

`internal/config`: `ui.enabled` from file and from `PROFGATE_UI_ENABLED`; a non-boolean value rejected;
`ui.enabled` with `auth.mode: oidc` and no `browser` block rejected, and accepted with one.

`deploy/chart`, in the chart's render tests: `ui.enabled` renders `ui.enabled: true` into the ConfigMap;
`config.ui.enabled` and a scalar `config.ui` fail rendering.

### 11.2 What is not proven

No test runs `app.js`, and `portmodel.js` and `targetmodel.js` are the exceptions that mark where the line falls.
The repository's toolchain (`mise.toml`) pins Go, `golangci-lint`, `helm`, `kind`, `ko`, and `kubectl`;
it pins no Node.js and no browser, and the end-to-end harness drives the gateway with Go's HTTP client.
Running `app.js` itself would mean one of two additions:
a Go-driven headless Chromium (`chromedp` or similar),
which is a new Go module plus a Chromium binary on every machine and CI runner that runs the suite;
or a Node.js test runner with a DOM, which is a second toolchain for a page of a few hundred lines.
This design adds neither.
The console is small enough to read, its rendering rules are four, and a scan can enforce most of them;
a second toolchain to prove the rest would cost more than the page it guards.

The port control and the target summary are the parts that argument does not cover,
which is why each is a module of its own.
Each is a decision table rather than a rendering:
pure, DOM-free, and wrong in ways a reader does not see —
an option that duplicates the default, a value serialized under the kind it was not typed as,
a counted reason dropped instead of shown, rows re-sorted out of the gateway's order,
a query that stopped asking for `explain` —
where being wrong sends the caller's request under a parameter the caller did not choose,
or tells an operator a Service has no Pods when the gateway said why it has none.
Executing them costs a third thing the two rejected options did not offer:
a pure-Go ECMAScript interpreter, used by tests only,
which runs wherever `go test` runs and puts no binary and no second toolchain on any machine or runner.
That is what this design takes, and gateway *Dependencies* lists the module
because decision 10 of that document makes adding one a design change.
`app.js` still renders the control and hands it what the caller typed;
that wiring is reviewed, not executed.

What that leaves unproven, stated plainly:
that `app.js` as executed renders a hostile principal, error message, version, `origin`, or `reason` as text;
that the URLs it builds at runtime are the ones *Rendering response values* describes;
that the non-envelope fallback of *Errors* behaves as written on malformed JSON, an empty body, or a rejected `fetch`;
and that `app.js` hands `portmodel.js` what the caller typed in the fields it renders,
hands `targetmodel.js` the response it fetched, and repeats a fetch when that model says to,
both models being proven apart from the widget and apart from the network.
The source scan proves the page contains no interface that could render markup and no hand-built `/v1` path;
the `internal/httpapi` tests prove the JSON the page receives carries hostile strings intact;
the end-to-end scenarios prove the shell, the headers, and the listing responses over the wire.
The gap between "contains no way to do it wrong" and "was seen doing it right" is closed by review,
on every change to `app.js`, `urls.js`, `portmodel.js`, and `targetmodel.js`,
which is why all four stay small and why the three besides `app.js` are separate files.
If the page grows past what a reviewer can hold, adding the browser-driven test becomes the right call,
and this section is where that decision is revised.

### 11.3 End to end

Two proofs, each run inside the existing authentication scenario of its lane
([`auth.md`](auth.md) *Testing*), both against a gateway with `ui.enabled`:

- **Browser flow under Dex.**
  With no cookie, `GET /ui/` is `200` with the shell and the *Response headers and CSP* headers,
  and `HEAD /ui/` is `200` with the same headers and no body;
  `GET /v1/whoami` with `Sec-Fetch-Mode: cors` and no cookie is `401`, not `302`;
  the login walk of that document's scenario, started from `GET /auth/login?return=/ui/?ns=x`,
  ends in a `200` landing page whose refresh and `Continue` link both name `/ui/?ns=x`;
  `GET /ui/?ns=x` with the cookie is `200` whatever `Sec-Fetch-Site` says,
  because the shell has no authentication step (*Why the page itself is not authenticated*);
  then, with the cookie and `Sec-Fetch-Site: same-origin`,
  `GET /v1/whoami`, `/v1/limits`, `/v1/namespaces`, and `/v1/namespaces/<test ns>/services` are each `200`,
  `whoami` names the Dex user and its mapped realm,
  and the Service list holds the test app's Service;
  `GET /auth/logout` with the cookie is `302` to `/` and `GET /` is `302` to `/ui/`.
- **`basic` over TLS.**
  `GET /ui/` without a credential is `200`;
  `GET /v1/namespaces` without one is `401` with `WWW-Authenticate: Basic realm="profgate"`
  and with `-u` is `200` and lists the test namespace;
  `GET /v1/limits` with `-u` reports the lane's configured limits.
  The browser's native dialog cannot be driven without a browser and is not proven here;
  what is proven is the pair of responses it reacts to.

A scenario that reaches no application Pod declares nothing;
the Service list reads the cache, so neither proof adds `needsPodReach` to its scenario.

---

## 12. Dependencies

No Go module is added to the gateway binary.
`embed`, `crypto/sha256`, `mime`, and `net/http` are the standard library.
One module is added to the tests:
the pure-Go ECMAScript interpreter that evaluates `portmodel.js` and `targetmodel.js`,
listed in gateway *Dependencies* and argued for in *What is not proven*.

Vendored browser code, pinned in `internal/ui/static/vendor/MANIFEST`:

| File | Project | Version | License | Size |
|---|---|---|---|---|
| `preact.module.js` | Preact | 10.29.8 | MIT | about 11 KiB |
| `htm.module.js` | htm | 3.1.1 | Apache-2.0 | about 1 KiB |
| `pico.classless.min.css` | Pico CSS | 2.1.1 | MIT | about 69 KiB |

Versions are those published on the npm registry as `latest` on the day this document was drafted;
the implementation pins what it reviews and updates this table.
Preact is MIT, Pico CSS is MIT, and htm is Apache-2.0.
Apache-2.0 asks that the license text and any `NOTICE` travel with the file:
htm's `LICENSE` is vendored beside it under `internal/ui/static/vendor/htm/`,
htm ships no `NOTICE`, and one is vendored there if a later release adds it.
The two MIT license texts are vendored the same way under `vendor/preact/` and `vendor/pico/`.

Every vendored file is the upstream's published build, unmodified, which is what makes the manifest hash meaningful.

---

## 13. Package layout

```text
internal/ui/           the console: embedded static tree, tree hash, rendered shell, asset handler, headers
internal/ui/static/    index.html, app.js, urls.js, portmodel.js, targetmodel.js, app.css, vendor/
internal/httpapi/      gains the four listing routes, the /ui/ and / dispatch, and the endpoint labels
internal/k8s/          gains Catalog on the Discovery interface, reading the Service lister
internal/config/       gains the ui block
internal/metrics/      gains the five Endpoint values
cmd/profgate/          constructs internal/ui when ui.enabled and passes it to httpapi
```

What the console adds to the seam is one method and one type.
The snippet below is that addition and not the whole interface,
which gateway *The seam* holds in full — `Explain`, `Explanation`, and `Exclusion` beside these:

```go
// ServiceRef names one Service in the cache.
type ServiceRef struct {
    Namespace, Name string
}

// The console's delta; gateway The seam has the complete interface.
type Discovery interface {
    // ... Targets, HasSynced, Confirm, and Explain as that document defines them
    // Catalog lists the Services with a non-empty selector from the cache,
    // sorted by namespace then name.
    // An empty namespace means every namespace; a namespace the cache lacks is an empty list, not an error.
    // It issues no request; an error means the lister could not be read.
    Catalog(ctx context.Context, namespace string) ([]ServiceRef, error)
}
```

`Catalog` returns Services, not namespaces, because the namespace list depends on `realm.services`
(*The realm filter*) and the seam does not know realms;
`internal/httpapi` derives the namespace list from the filtered Services and never asks the seam for one.
`Catalog` reads the Service informer cache and issues no request,
so the set of things Profgate can do to Kubernetes — the point of keeping the interface small
([`800-security-invariant.md`](../../.agents/rules/800-security-invariant.md)) — is unchanged:
the seven RBAC tuples stay seven, the golden ClusterRole test stays green, and the recording transport sees nothing new.
The interface is one method longer, which is the visible cost that rule asks for.

The console reaches `internal/httpapi` through one field:

```go
type Deps struct {
    // ...
    // Console serves /ui/ and /; nil means ui.enabled is false and both are 404 route_unknown.
    Console http.Handler
}
```

Ownership is split as follows, so each concern lives in exactly one package:

- `internal/httpapi` owns **dispatch**: a path under `/ui/` or exactly `/` goes to `Console` when it is non-nil,
  before the `/v1` route regular expressions and the way `/auth/` is dispatched today,
  and is `404 route_unknown` through the ordinary envelope when `Console` is nil.
  It owns the **metrics row** for those requests:
  it wraps the `ResponseWriter`, reads the status `Console` wrote, and maps it to the `ui` codes of *Audit and metrics*.
  It writes no audit line for them.
- `internal/ui` owns everything inside that dispatch:
  **path matching** under `/ui/static/<hash>/` and the shell at `/ui/`,
  the **method check** (`GET` and `HEAD`; `405` with `Allow: GET, HEAD` otherwise),
  the **`302`** from `/` to `/ui/`,
  every **security header** of *Response headers and CSP*, and the `Cache-Control` and `Content-Type` of *Headers*.
- The **error envelope** is written once, by `internal/httpapi`,
  which exports its writer as `httpapi.WriteError` with the signature its unexported writer has today;
  `internal/ui` calls it for its `404` and `405`,
  so there is no shared envelope package and no second copy of the writer.

`internal/ui` imports `internal/httpapi` for the writer;
`internal/httpapi` takes an `http.Handler` and imports nothing of `internal/ui`,
so the import runs one way, and `cmd/profgate` constructs both.

---

## 14. Changes to the accepted designs

The following text is amended to match this document.
Each row names the heading it edits.

| File | Section | Change |
|---|---|---|
| `docs/specs/gateway.md` | *HTTP API* | "except the three `/auth/` routes that [`auth.md`](auth.md) adds when its browser flow is configured" gains "and the `/ui/` and `/` routes of [`ui.md`](ui.md) when `ui.enabled`"; the four listing routes are named as `/v1` routes defined in [`ui.md`](ui.md) |
| `docs/specs/gateway.md` | *Request algorithm* | the sentence introducing the endpoint-specific tail gains the four listing routes of [`ui.md`](ui.md): they run route, method, readiness, credential placement, authentication, and parameter checks as the interactive routes do; the realm check refuses only the Service list (a namespace the realm does not admit), while the namespace list is filtered and `whoami` and `limits` describe the caller; after that they read the cache, with no discovery, admission, confirmation, or proxy step; the method rule that the interactive routes accept `GET` only stays true of them |
| `docs/specs/gateway.md` | *List targets* | a following subsection, *Listing endpoints*, pointing to [`ui.md`](ui.md) *Response shapes* for the four response shapes |
| `docs/specs/gateway.md` | *Errors* | `503 discovery_unavailable` also covers a cache read that fails on a listing route; `405` under `/ui/` and on `/` carries `Allow: GET, HEAD` |
| `docs/specs/gateway.md` | *The seam* | `ServiceRef` and `Catalog`, reading the Service cache and issuing no request |
| `docs/specs/gateway.md` | *Non-disclosure* | a fourth listed observation: `/v1/limits` returns `allowedSelections` and the default to every request the configured `auth.mode` admits, anonymous requests under `disabled` included, with the argument of [`ui.md`](ui.md) *Limits* |
| `docs/specs/gateway.md` | *Logging* | the listing routes write the record with `namespace` on the Service list only and the other target fields empty; requests under `/ui/` and to `/` write no record |
| `docs/specs/gateway.md` | *Metrics* | `endpoint` gains `namespaces`, `services`, `whoami`, `limits`, `ui`; the `ui` codes |
| `docs/specs/gateway.md` | *Layers* | unit rows for `internal/ui`, the listing routes, `Catalog`, and the `ui.enabled` validation, per [`ui.md`](ui.md) *Unit* |
| `docs/specs/gateway.md` | *What end-to-end proves* | the two scenarios of [`ui.md`](ui.md) *End to end* |
| `docs/specs/gateway.md` | *Configuration* | the `ui.enabled` row |
| `docs/specs/gateway.md` | *Build and Deployment* | the vendored browser files under `internal/ui/static/vendor/` are embedded by `go:embed` and pinned by `MANIFEST`; the chart's `ui.enabled` value and raw-block guard |
| `docs/specs/gateway.md` | *Dependencies* | a closing sentence: no Go module for the console; the vendored browser code is listed in [`ui.md`](ui.md) *Dependencies* |
| `docs/specs/gateway.md` | *Package Layout* | `internal/ui/` |
| `docs/specs/gateway.md` | *Failure Scenarios* | rows for a cache read failing on a listing route, a rolling update with two asset hashes, and `ui.enabled` false |
| `docs/specs/auth.md` | *Non-goals* | "A UI, and the listing endpoints it would need, is a later document" becomes a pointer to this one |
| `docs/specs/auth.md` | *The `/auth/` routes* | logout's fallback `302` to `/` lands on `/ui/` when `ui.enabled` |
| `docs/specs/auth.md` | *What is redirected* | "a future UI's JSON requests" names this document |
| `docs/specs/auth.md` | *Testing* | the two lanes gain the scenarios of [`ui.md`](ui.md) *End to end* |
| `.agents/rules/100-project-map.md` | *Planned Structure* | `internal/ui/` |
| `.agents/rules/100-project-map.md` | *External HTTP API* | the four listing routes; `/ui/`, `/ui/static/{hash}/{file}`, and `/`, present only when `ui.enabled` |
| `AGENTS.md` | *Three Specs, All Accepted* | four, adding this document |
| `docs/README.md` | *Where Contributors Start* | [`specs/ui.md`](ui.md) beside the PGO and authentication specs |

The stale `302` this document once assumed from the callback is gone:
[`auth.md`](auth.md) *Amendments* records the landing page, and *Signing in and out* here describes that flow.

Updated with the implementation: `docs/api.md` (the listing endpoints), `docs/configuration.md` (`ui.enabled`),
`docs/deployment.md` (Ingress paths),
`deploy/chart/profgate/values.yaml` and `deploy/chart/profgate/README.md`
(the `ui.enabled` value and the raw-block guard).

---

## 15. Amendments

Edits made to this document after it was accepted, each in the change that made it.

| Section | Change |
|---|---|
| *Layout and embedding*, *Dependencies* | the Pico file is `pico.classless.min.css`, the class-less build's published name, and its size is about 69 KiB |
| *Layout and embedding* | the tree hash frames each file as its length-prefixed path and length-prefixed content; `index.html` is hashed but has no hashed serving route |
| *Request algorithm for the listing endpoints* | the readiness step is the readiness `internal/httpapi` composes for every `/v1` route — discovery synced and, under `oidc`, the issuer discovered — not `HasSynced()` alone |
| *Unit* | the `internal/httpapi` fake holds no selectorless Service, because `ServiceRef` carries no selector; the selectorless cases live in the `internal/k8s` bullet, where selector presence is decided; the non-disclosure assertion says no list exposes a Pod-discovered or selected backend port and no listing response carries an IP address or `podIP`, since `/v1/limits` returns `allowedSelections` and the default by design |
| *Audit and metrics*, *Unit* | the `ui` code set gains `internal_error` for any status the console wrote outside `2xx`, `3xx`, `404`, and `405`; the set stays closed |
| *End to end* | the two proofs run inside the existing authentication scenarios rather than as scenarios of their own |
| *Changes to the accepted designs* | the table describes edits already made, not edits acceptance would make |
| *Flow*, *Signing in and out* | the return path carries a `returned=1` marker; a load that starts with it never navigates to login on its own and shows a **Sign in again** button on `401` instead, which is what bounds the once-per-load rule across the round trip |
| *Core decisions*, *Request algorithm for the listing endpoints*, *Limits* | the namespace and Service catalogs are realm-filtered, `whoami` is caller-specific, and `/v1/limits` is global operator configuration answered to every request the configured `auth.mode` admits, anonymous requests under `disabled` included |
| *Controls* | the control a value came from decides whether it is sent as `port=` or `portName=`, never the value itself; a menu entry equal to `pprof.default` is left out while `/v1/limits` still reports it |
| *Controls*, *Layout and embedding*, *Unit*, *What is not proven*, *Dependencies*, *Package layout* | the port control's rules are two pure functions in `portmodel.js`, driven by a table-driven test in a pure-Go ECMAScript interpreter; that interpreter is the one test-only module the toolchain argument now admits |
| *Response shapes*, *Flow*, *Targets, with reasons* | the console sends `explain=true` on every targets fetch and never `version=` or `pod=`; the preamble's non-disclosure sentence is scoped to the four listing routes, the targets response having always named Pods and nodes |
| *Core decisions*, *Targets, with reasons* | the profile endpoint is unchanged while the targets endpoint keeps its steps, ordering, and realm boundary and gains diagnostics; a targets fetch a replica older than this design refuses `400 invalid_parameter` is retried once without `explain` |
| *Controls*, *Failure scenarios* | an empty target list is replaced by the gateway's counted reasons in a fixed wording, `selectorMatched` of `0` reading as a selector that matches no Pod, and a body without `excluded` leaving the plain empty state |
| *Controls*, *Layout and embedding*, *Unit*, *What is not proven*, *Dependencies*, *Package layout* | the targets query, the retry rule, and the target summary are three pure functions in `targetmodel.js`, executed by the interpreter that already runs `portmodel.js`; the scan pins the ten reason names there |
| *Package layout* | the `Discovery` snippet is labelled as the console's delta rather than the whole interface, which gateway *The seam* holds |
