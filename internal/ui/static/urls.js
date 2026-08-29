// URL builders for the console.
// This is the only module that spells a /v1, /ui, or /auth path: every URL
// the page fetches, navigates to, or offers as a link is built here with
// new URL(path, location.origin), each path segment passed through
// encodeURIComponent and every query built with URLSearchParams.
// Nothing here concatenates a response value into a path.

// collectionIDRe is the Collection identifier grammar: 20 characters of the
// lowercase Crockford base32 alphabet.
const collectionIDRe = /^[0-9a-hjkmnp-tv-z]{20}$/;

// build returns a same-origin URL whose path is the encoded segments under
// base and whose query holds every param with a defined, non-empty value.
function build(base, segments, params) {
  const url = new URL(base, location.origin);
  const encoded = segments.map((s) => encodeURIComponent(String(s)));
  url.pathname = [base, ...encoded].join("/");
  const query = new URLSearchParams();
  if (params) {
    for (const [key, value] of Object.entries(params)) {
      if (value !== undefined && value !== null && value !== "") {
        query.set(key, String(value));
      }
    }
  }
  url.search = query.toString();
  return url;
}

export function namespacesURL() {
  return build("/v1", ["namespaces"]);
}

export function servicesURL(ns) {
  return build("/v1", ["namespaces", ns, "services"]);
}

// targetsURL lists the Pods of a Service; query is what targetmodel.js built.
export function targetsURL(ns, svc, query) {
  return build("/v1", ["namespaces", ns, "services", svc, "targets"], query);
}

export function collectionsURL(ns, svc) {
  return build("/v1", ["namespaces", ns, "services", svc, "collections"]);
}

// isCollectionID reports whether id matches the identifier grammar; the page
// links a Collection only when it does.
export function isCollectionID(id) {
  return typeof id === "string" && collectionIDRe.test(id);
}

// collectionURL reads one Collection; it throws when id is outside the
// identifier grammar, so a record the gateway never wrote is never linked.
export function collectionURL(id) {
  if (!collectionIDRe.test(id)) {
    throw new Error("collection id outside the identifier grammar");
  }
  return build("/v1", ["collections", id]);
}

// collectionCancelURL cancels one Collection;
// it throws when id is outside the identifier grammar,
// so no request is ever posted to a path built from a record the gateway never wrote.
export function collectionCancelURL(id) {
  if (!collectionIDRe.test(id)) {
    throw new Error("collection id outside the identifier grammar");
  }
  return build("/v1", ["collections", id, "cancel"]);
}

export function collectionProfileURL(id) {
  if (!collectionIDRe.test(id)) {
    throw new Error("collection id outside the identifier grammar");
  }
  return build("/v1", ["collections", id, "profile"]);
}

// profileURL is the download URL; params holds any of seconds, pod, version,
// port, and portName, each sent only when set.
export function profileURL(ns, svc, profile, params) {
  const p = params || {};
  return build("/v1", ["namespaces", ns, "services", svc, "profiles", profile], {
    seconds: p.seconds,
    pod: p.pod,
    version: p.version,
    port: p.port,
    portName: p.portName,
  });
}

export function whoamiURL() {
  return build("/v1", ["whoami"]);
}

export function limitsURL() {
  return build("/v1", ["limits"]);
}

// loginURL starts the browser flow and asks it to return to the page's own
// path and query, with the returned marker added so the page can tell a fresh
// return from a plain load.
export function loginURL(ns, svc) {
  const page = build("/ui/", [], { ns: ns, svc: svc, returned: "1" });
  return build("/auth/login", [], { return: `${page.pathname}${page.search}` });
}

export function logoutURL() {
  return build("/auth/logout", []);
}

// pageURL is the console with its selection in the query and no marker; it is
// what the page writes with history.replaceState.
export function pageURL(ns, svc) {
  return build("/ui/", [], { ns: ns, svc: svc });
}
