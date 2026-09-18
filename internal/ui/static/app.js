// The console: one Preact class component holding the page's state,
// rendered through htm bound to Preact's h.
// Every response-derived value reaches the template as ${value} in child or attribute position,
// so it is set as text or as a DOM property and never parsed as markup.
// Every URL comes from urls.js; this module spells no path.
import { h, Component, render, createRef } from "./vendor/preact/preact.module.js";
import htm from "./vendor/htm/htm.module.js";
import {
  catalogURL,
  targetsURL,
  collectionsURL,
  collectionURL,
  collectionCancelURL,
  collectionProfileURL,
  isCollectionID,
  profileURL,
  whoamiURL,
  limitsURL,
  loginURL,
  logoutURL,
  pageURL,
} from "./urls.js";
import { deriveControl, applyInput } from "./portmodel.js";
import { targetsQuery, retryWithoutExplain, targetSummary, downloadNote } from "./targetmodel.js";
import {
  tableOffered,
  startOffered,
  cancelOffered,
  uuidFromBytes,
  startRequest,
  cancelRequest,
  startOutcome,
  cancelOutcome,
  startNext,
  confirmAccepted,
  progressText,
  olderCollectionsNote,
  shortTime,
} from "./collectionmodel.js";
import { namespacesOf, servicesOf, filterOptions, searchCatalog } from "./catalogmodel.js";
import { offeredProfiles, secondsLimit, defaultSeconds, secondsValid, profileRequest } from "./profilemodel.js";

const html = htm.bind(h);

// notReadyDelay is how often a not_ready answer is retried.
const notReadyDelay = 2000;

// armDelay is how long a control that has armed itself waits for the second press that sends the request.
// It runs before the first request only:
// submission cancels it,
// and nothing restarts it while a request is in flight or after an outcome the page could not classify,
// where disarming would drop the key that a repeat of the same attempt needs.
const armDelay = 10000;

// searchLimit is how many result rows the search draws at most.
// It bounds what the page draws and not what the page knows:
// the page holds the whole catalog,
// and it says how many matched whenever more matched than it drew.
const searchLimit = 50;

// hints are the one-line hints for the codes a user can act on; every other
// code is shown as is.
const hints = {
  not_ready: "the gateway is still syncing; the page retries every 2 seconds",
  too_many_auth: "the gateway is checking too many passwords at once; retry in a moment",
  auth_unavailable: "the gateway cannot decide who you are right now; retry",
  realm_denied: "your realm does not admit this; the identity shows what it does",
  service_not_found: "the Service left the cache since the catalog was fetched; the page refetches the catalog",
  no_targets:
    "no Pod is eligible for the selection; Refresh on the targets list updates the available Pods and the empty state",
  port_not_allowed: "allowedSelections does not admit the value; the port control shows what it does admit",
  seconds_exceeds_limit: "the limit the duration input was bounded by",
  discovery_unavailable: "the gateway could not read its cache or confirm the Pod; retry",
  pgo_disabled: "PGO collection is off on this gateway; the Collections view goes once the limits have been refetched",
  pgo_unavailable: "the gateway could not reach its store, so a start may or may not have taken; the same press can be repeated",
  collection_in_progress: "a Collection is already running for this Service; the list shows it",
  rate_limited: "the gateway is at its limit for now; the control returns after the delay",
  capacity_exhausted: "the gateway is at its limit for now; the control returns after the delay",
  collection_terminal: "the Collection ended before the cancel arrived; the list shows its final state",
  collection_initializing: "the Collection is still being created; the cancel is retried once",
  limit_exceeded: "the Collection's policy exceeds a configured ceiling; the message names the fields",
  version_conflict: "the Service's Pods carry more than one version, or none",
  version_missing: "the Service's Pods carry more than one version, or none",
};

// fetchJSON runs one same-origin request and resolves to
// {status, headers, body, blob, bodyText, rejected, code, message, error}.
// req, when given, is the request description a write control built:
// its method, its headers, and its body, which is null for a request that carries none.
// Without one the request is a GET, which is what every listing fetch is.
// asBlob, when true, reads a 200 as a Blob into blob and leaves body null,
// which is what a download wants; every other status is an error under the envelope rule below,
// a JSON body or not, because a download has nothing to do with a 201 or a 204 and no Blob to save from one.
// body is the decoded JSON when the Content-Type says JSON, and bodyText is the body as it arrived;
// rejected says fetch itself never produced a response;
// code and message are the error envelope's two fields when the body is one, and empty strings otherwise.
// error composes those parts for the page's error surface:
// the envelope, or the string "HTTP <status> <statusText>", or "request failed" with status 0.
// The parts stay beside it because collectionmodel.js branches on the status and the code,
// and a composed string cannot be branched on.
// It decides nothing about 401: the caller does, because what a 401 means depends on the mode.
async function fetchJSON(url, req, asBlob) {
  const out = {
    status: 0,
    headers: new Headers(),
    body: null,
    blob: null,
    bodyText: "",
    rejected: false,
    code: "",
    message: "",
    error: null,
  };
  try {
    const init = { credentials: "same-origin" };
    if (req) {
      init.method = req.method;
      init.headers = req.headers;
      if (req.body !== null) {
        init.body = req.body;
      }
    }
    const res = await fetch(url, init);
    out.status = res.status;
    out.statusText = res.statusText;
    out.headers = res.headers;
    if (asBlob === true && res.status === 200) {
      out.blob = await res.blob();
      return out;
    }
    out.bodyText = await res.text();
    const ctype = res.headers.get("content-type") || "";
    if (ctype.startsWith("application/json")) {
      try {
        out.body = JSON.parse(out.bodyText);
      } catch {
        out.body = null;
      }
    }
    if (asBlob !== true && res.ok && out.body !== null) {
      return out;
    }
    if (isEnvelope(out.body)) {
      out.code = out.body.code;
      out.message = out.body.error;
      out.error = { error: out.message, code: out.code };
    } else {
      out.error = `HTTP ${res.status} ${res.statusText}`;
    }
    return out;
  } catch {
    out.rejected = true;
    out.error = "request failed";
    return out;
  }
}

// answerOf is what collectionmodel.js reads from one transport result.
// Every field stays apart: the model decides on the status and the code,
// and reads the body as text only for a success whose id it cannot use.
// id and record are the same response read two ways —
// startOutcome selects by identifier, cancelOutcome replaces a row with the record.
function answerOf(res) {
  return {
    rejected: res.rejected,
    status: res.status,
    code: res.code,
    message: res.message,
    id: res.body ? res.body.id : null,
    record: res.body,
    body: res.bodyText,
    retryAfter: res.headers.get("retry-after"),
  };
}

// newKey is one idempotency key per attempt series.
// crypto.randomUUID is defined only in a secure context and this page can be served over plain HTTP,
// so where the browser does not define it the key is sixteen random bytes formatted as a UUIDv4.
function newKey() {
  if (typeof crypto.randomUUID === "function") {
    return crypto.randomUUID();
  }

  return uuidFromBytes(crypto.getRandomValues(new Uint8Array(16)));
}

// isEnvelope reports whether body is the gateway's error envelope.
function isEnvelope(body) {
  return (
    body !== null &&
    typeof body === "object" &&
    !Array.isArray(body) &&
    typeof body.error === "string" &&
    typeof body.code === "string"
  );
}

// filenameOf is the filename parameter of a Content-Disposition header, quoted or bare,
// and "profile" when the header is absent, names none, or names one that is empty.
// The header is split at the semicolons that stand outside quoted strings,
// and a quoted pair inside one is the character it escapes,
// so filename="a\"b" is a"b and a filename= inside another parameter's quotes is not the parameter.
// filename* is a different parameter and is not read.
function filenameOf(header) {
  const params = [];
  let cur = "";
  let quoted = false;
  const s = header || "";
  for (let i = 0; i < s.length; i++) {
    const ch = s[i];
    if (quoted) {
      if (ch === "\\" && i + 1 < s.length) {
        cur += s[++i];
      } else if (ch === '"') {
        quoted = false;
      } else {
        cur += ch;
      }
    } else if (ch === '"') {
      quoted = true;
    } else if (ch === ";") {
      params.push(cur);
      cur = "";
    } else {
      cur += ch;
    }
  }
  params.push(cur);
  for (const p of params) {
    const eq = p.indexOf("=");
    if (eq >= 0 && p.slice(0, eq).trim().toLowerCase() === "filename") {
      const name = p.slice(eq + 1).trim();
      return name === "" ? "profile" : name;
    }
  }
  return "profile";
}

// saveBlob hands blob to the browser's download manager under name:
// an object URL, an anchor with the download attribute clicked once, and the URL revoked when the click has returned.
// The anchor is created here and never stands in the template, because the control that can be disabled is a button.
function saveBlob(blob, name) {
  const href = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = href;
  a.download = name;
  document.body.appendChild(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(href);
}

// asList returns value when it is an array and [] otherwise, so a body of
// another shape renders nothing rather than throwing.
function asList(value) {
  return Array.isArray(value) ? value : [];
}

// text renders any value as text: strings as they are, null as a dash,
// everything else through String.
function text(value) {
  if (value === null || value === undefined || value === "") {
    return "-";
  }
  return String(value);
}

// FilterNote is the line beside a menu saying how much of its list the filter's query found.
// It counts matches and never drawn rows:
// a menu always offers the value it is showing, so a query that missed that value still draws it,
// and a line counting rows would report one more match than the query made.
// When that is the case the line says so, rather than leaving the extra row unexplained.
// It stands only while the filter's query is not empty,
// because a query of whitespace alone leaves every option standing
// and a line saying so is a line that never changes.
function FilterNote(props) {
  if (props.query.trim() === "") {
    return null;
  }
  const kept = props.shown > props.matched;

  return html`<small class="shown"
    >${props.matched} of ${props.total} matched${kept ? "; the chosen value is shown too" : ""}</small
  >`;
}

// TimeCell is one of the table's three timestamp columns:
// the day over the second, each on its own line and neither broken within itself,
// with the value the gateway sent carried whole for whoever hovers it.
function TimeCell(props) {
  const parts = shortTime(props.value);
  return html`
    <td class="time" title=${text(props.value)}>
      <span>${parts.date}</span>${parts.clock ? html`<span>${parts.clock}</span>` : null}
    </td>
  `;
}

// SearchNote is the line under the search results saying why there are no more of them.
// Before the catalog answer arrives there is nothing to search,
// which is why the line names that rather than an empty result:
// a page that said nothing matched would be answering a question it cannot yet read.
// It separates an answer that has not come yet from one that came to nothing,
// because a request that failed is not a request still running,
// and the error standing above the menus carries the reason and the way to ask again.
// Past that the line stands for a query that matched nothing
// and for a query that matched more rows than searchLimit draws,
// and it stands not at all when every match is on the page.
function SearchNote(props) {
  if (!props.loaded) {
    if (props.failed) {
      return html`<small class="note">the catalog could not be read</small>`;
    }
    return html`<small class="note">the catalog is still loading</small>`;
  }
  if (props.total === 0) {
    return html`<small class="note">nothing matched</small>`;
  }
  if (props.total > props.shown) {
    return html`<small class="note">${props.total} matched; the first ${props.shown} are shown</small>`;
  }

  return null;
}

// errorParts splits an error of fetchJSON into what the page shows.
function errorParts(err) {
  if (typeof err === "string") {
    return { code: "", message: err, hint: "" };
  }
  return { code: err.code, message: err.error, hint: hints[err.code] || "" };
}

// ErrorBox shows one error as text: code, message, hint, and a retry button
// when the caller offers one.
function ErrorBox(props) {
  const parts = errorParts(props.error);
  return html`
    <div class="error" role="alert">
      ${parts.code ? html`<strong>${parts.code}</strong>` : null}
      ${parts.code ? " " : null}
      <span>${parts.message}</span>
      ${parts.hint ? html`<p><small>${parts.hint}</small></p>` : null}
      ${props.onRetry ? html`<button type="button" onClick=${props.onRetry}>Retry</button>` : null}
    </div>
  `;
}

// SignInRequired is what a 401 shows when the page does not navigate: under
// basic the browser's dialog was cancelled and Retry prompts again; under oidc
// the page just returned from a login that did not sign it in, and the button
// starts another login only when the user asks.
function SignInRequired(props) {
  return html`
    <div class="error" role="alert">
      <span>sign in required</span>
      ${props.onRetry ? html`<button type="button" onClick=${props.onRetry}>${props.label || "Retry"}</button>` : null}
    </div>
  `;
}

// initialQuery reads the selection the page was opened with and whether the
// load is a return from login, the marker loginURL adds to the return path.
function initialQuery() {
  const q = new URLSearchParams(location.search);
  return { ns: q.get("ns") || "", svc: q.get("svc") || "", returned: q.get("returned") === "1" };
}

class App extends Component {
  constructor(props) {
    super(props);
    const q = initialQuery();
    // navigatedToLogin is the once-per-load rule of Signing in and out: the
    // first 401 under oidc navigates, every later one is shown as an error.
    // returnedFromLogin is the marker the login round trip carries back: a
    // load that starts with it never navigates on its own, so a login that
    // keeps answering 401 ends in a message rather than a loop.
    this.navigatedToLogin = false;
    this.returnedFromLogin = q.returned;
    // targetsSeq stamps each targets request and collectionsSeq each Collections request,
    // so a stale answer is dropped when a later selection or a later refresh has already replaced it.
    // The two lists count apart, because a refresh of one must never discard another's answer.
    this.targetsSeq = 0;
    this.collectionsSeq = 0;
    // catalogSeq stamps each catalog request,
    // so the attempt a not_ready scheduled does nothing once a later attempt has superseded it.
    // catalogPending is the claim on the one request in flight, taken and released synchronously,
    // so two presses landing in one tick cannot both pass it;
    // catalogAgain is the one further request an answer records while that claim is held.
    this.catalogSeq = 0;
    this.catalogPending = false;
    this.catalogAgain = false;
    // identity is the disclosure the realm_denied hint names.
    // Its open is the element's own state, set once per qualifying answer rather than bound in the template,
    // because a native close changes open without the template knowing,
    // and a template that re-asserted the value it asserted last render would not reopen what a person closed.
    this.identity = createRef();
    // selection counts namespace and Service changes,
    // and targetsFor and collectionsFor are the selections those two lists were last requested for.
    // A catalog answer tests each on its own, so it starts the fetch its selection is still owed
    // and repeats neither that selection has already had.
    // The Collections list waits on the limits beside that, and whichever of the two answers last starts it.
    this.selection = 0;
    this.targetsFor = -1;
    this.collectionsFor = -1;
    this.timers = [];
    // startTimer, cancelTimer, and cancelRetryTimer hold the transitions the two controls schedule:
    // the start control's ten-second disarm and the end of its cooling,
    // the cancel control's ten-second disarm,
    // and the second try of a cancel whose first was answered with a retry.
    // Leaving the phase that scheduled a timer clears that timer,
    // so a transition scheduled by a phase the control has left cannot act on the phase it is in now.
    this.startTimer = 0;
    this.cancelTimer = 0;
    this.cancelRetryTimer = 0;
    // cancelArmedAt is when the armed cancel row was armed, on the millisecond clock,
    // which is what a second press on that row is measured against.
    this.cancelArmedAt = 0;
    this.state = {
      phase: "booting",
      bootError: null,
      whoami: null,
      limits: null,
      ns: q.ns,
      svc: q.svc,
      // catalog is the one answer behind both menus: the namespace and Service pairs the realm admits.
      // Neither menu's options are stored beside it, because both are derived from it at render,
      // so no two answers about what exists can disagree.
      // catalogLoaded records that an answer has been applied,
      // and catalogLoading is true while a request is in flight, which is what disables the Refresh control.
      catalog: [],
      catalogLoaded: false,
      catalogLoading: false,
      // nsFilter and svcFilter are what has been typed into the field beside each menu.
      // They narrow what that menu draws and are sent nowhere,
      // so neither reaches the page query and neither is restored from it.
      nsFilter: "",
      svcFilter: "",
      // search is what has been typed into the search field.
      // It draws result rows from the whole catalog rather than narrowing either menu,
      // and it is sent nowhere,
      // so it reaches neither the page query nor a request and is not restored from the query.
      search: "",
      targets: [],
      targetSummary: null,
      // targetsLoading and collectionsLoading are true from a list's fetch until its latest request has settled;
      // each disables its Refresh control, so a second press sends nothing while the first is in flight.
      // An answer a newer request has superseded clears neither, because the newer request is still in flight.
      targetsLoading: false,
      collections: [],
      // collectionsNote says older Collections exist beyond the page it drew;
      // it is stored and cleared with the rows, so a table and the line under it are always one answer.
      collectionsNote: "",
      collectionsLoading: false,
      collection: null,
      profile: "",
      seconds: "",
      portChoice: "default",
      portNumber: "",
      portName: "",
      pod: "",
      version: "",
      errors: {},
      signIn: {},
      copied: false,
      // downloading is true from the press on Download until the body has been read whole and the save has begun,
      // or the fetch has settled on anything else.
      downloading: false,
      // start is the start control's whole state, as startNext keeps it,
      // and startMessage is what that model last asked the page to say.
      start: { phase: "idle", key: null, route: null, token: 0, until: 0, armedAt: 0 },
      startMessage: null,
      // cancelArmed is the identifier of the row whose cancel is armed,
      // and the empty string when none is: there is one place to press.
      cancelArmed: "",
    };
  }

  componentDidMount() {
    if (this.returnedFromLogin) {
      // Drop the marker from the address bar so a reload is a plain load
      // that may navigate to login again.
      this.writeQuery(this.state.ns, this.state.svc);
    }
    this.boot();
  }

  componentWillUnmount() {
    for (const t of this.timers) {
      clearTimeout(t);
    }
    this.timers = [];
  }

  // later schedules fn and remembers the timer so unmount can clear it.
  // It returns the handle, for a caller that must cancel the timer earlier than that.
  later(fn, ms) {
    const id = setTimeout(fn, ms);
    this.timers.push(id);

    return id;
  }

  // signIn navigates to the browser login with the page's selection as the
  // return path; it is the user's action after a return that stayed 401.
  signIn = () => {
    this.navigatedToLogin = true;
    this.setState({ phase: "navigating" });
    location.assign(loginURL(this.state.ns, this.state.svc).href);
  };

  // mayNavigateToLogin reports whether a 401 under oidc navigates: once per
  // load, and never on the load that just returned from login.
  mayNavigateToLogin() {
    return !this.navigatedToLogin && !this.returnedFromLogin;
  }

  // boot runs /v1/whoami and nothing else until it answers 200.
  boot = async () => {
    this.setState({ phase: "booting", bootError: null });
    const res = await fetchJSON(whoamiURL());
    if (res.status === 200 && res.body && res.body.auth && res.body.realm) {
      this.setState({ phase: "ready", whoami: res.body }, () => this.loadAfterBoot());
      return;
    }
    if (res.status === 401) {
      const scheme = (res.headers.get("www-authenticate") || "").trim().toLowerCase();
      if (scheme.startsWith("bearer")) {
        if (this.mayNavigateToLogin()) {
          this.signIn();
          return;
        }
        this.setState({ phase: "signInRequired" });
        return;
      }
      if (scheme.startsWith("basic")) {
        this.setState({ phase: "signInRequired" });
        return;
      }
    }
    if (typeof res.error === "object" && res.error !== null && res.error.code === "not_ready") {
      this.later(this.boot, notReadyDelay);
    }
    this.setState({ phase: "error", bootError: res.error || `HTTP ${res.status}` });
  };

  loadAfterBoot() {
    this.loadLimits();
    this.loadCatalog();
  }

  // request runs one request after boot and applies the mode the page now
  // knows to a 401: oidc navigates once, basic asks to sign in with a retry,
  // disabled shows it as an error. not_ready is retried every 2 seconds.
  // It resolves to the body on 200 and null otherwise, after recording the
  // error under key.
  // retryOnce, when given, is consulted once when the first attempt is not a 200:
  // it takes the status and the envelope code and returns a URL to fetch instead, or null.
  // The second answer is then handled exactly as a first one would be, and never consulted again.
  // stale, when given, is asked once the answer has arrived;
  // when it returns true the answer is dropped whole and request resolves to null,
  // recording no error, scheduling no not_ready retry, and navigating nowhere,
  // because the error under key belongs to the latest request and a superseded answer arriving late would overwrite it.
  async request(key, url, retry, retryOnce, stale) {
    let res = await fetchJSON(url);
    if (retryOnce && res.status !== 200) {
      const code = typeof res.error === "object" && res.error !== null ? res.error.code : undefined;
      const again = retryOnce(res.status, code);
      if (again) {
        res = await fetchJSON(again);
      }
    }
    if (stale && stale()) {
      return null;
    }
    if (res.status === 200 && res.body !== null) {
      this.clearError(key);
      return res.body;
    }
    this.settle(key, res, retry);
    return null;
  }

  // settle records what a request that did not answer 200 leaves behind:
  // the 401 rule of Signing in and out, the not_ready retry, and the error under key.
  // It returns the error it recorded, or null when the 401 rule took the answer,
  // so a caller can act on the code without reading state a setState has not applied yet.
  // A realm_denied refetches the identity its hint names,
  // because the realm the identity shows is the one that no longer admits the request;
  // a refusal of the whoami fetch itself asks for no second one.
  // A 403 carrying that code opens the disclosure too, before the refetch is sent,
  // so what the hint names is on screen before the realm that refused it is read again.
  // The status is read beside the code: an answer of another status carrying it refetches and opens nothing.
  settle(key, res, retry) {
    if (res.status === 401) {
      const mode = this.state.whoami.auth.mode;
      if (mode === "oidc" && this.mayNavigateToLogin()) {
        this.signIn();
        return null;
      }
      if (mode === "oidc" || mode === "basic") {
        this.setState((s) => ({
          signIn: { ...s.signIn, [key]: retry },
          errors: { ...s.errors, [key]: undefined },
        }));
        return null;
      }
    }
    if (typeof res.error === "object" && res.error !== null && res.error.code === "not_ready" && retry) {
      this.later(retry, notReadyDelay);
    }
    this.setState((s) => ({
      errors: { ...s.errors, [key]: { error: res.error, retry: retry } },
      signIn: { ...s.signIn, [key]: undefined },
    }));
    if (isEnvelope(res.error) && res.error.code === "realm_denied" && key !== "whoami") {
      if (res.status === 403) {
        this.openIdentity();
      }
      this.reloadWhoami();
    }
    return res.error;
  }

  clearError(key) {
    this.setState((s) => ({
      errors: { ...s.errors, [key]: undefined },
      signIn: { ...s.signIn, [key]: undefined },
    }));
  }

  loadLimits = async () => {
    const body = await this.request("limits", limitsURL(), this.loadLimits);
    if (!body) {
      return;
    }
    this.setState({ limits: body }, () => {
      const { limits, whoami } = this.state;
      const offered = offeredProfiles(limits, whoami);
      const profile = offered.includes("cpu") ? "cpu" : offered[0] || "";
      this.setState({ profile: profile, seconds: defaultSeconds(limits, profile) });
      this.maybeLoadCollections();
    });
  };

  // loadCatalog is the whole lifecycle of the one source behind both menus:
  // the load's first fetch, each press of the Service panel's Refresh,
  // the attempt a not_ready schedules, and the refetch a service_not_found owes.
  // The claim is an instance field and not state,
  // because a setState is applied later than the line after it
  // and two presses in one tick would both read the value it had before either of them.
  // Every settled outcome releases the claim, not_ready included:
  // holding it across the two-second wait would hang the page,
  // because the scheduled attempt would meet the claim it is waiting on and return.
  // An answer that failed replaces no catalog,
  // so the menus the page already had stand with the error beside them.
  loadCatalog = async () => {
    if (this.catalogPending) {
      return;
    }
    this.catalogPending = true;
    const seq = ++this.catalogSeq;
    this.setState({ catalogLoading: true });
    // One wrapper is both the timer's callback and the error's Retry control, so those two cannot diverge,
    // and a later attempt makes an earlier timer inert by raising the generation.
    const retry = () => {
      if (seq === this.catalogSeq) {
        this.loadCatalog();
      }
    };
    const body = await this.request("catalog", catalogURL(), retry);
    this.catalogPending = false;
    const again = this.catalogAgain;
    this.catalogAgain = false;
    if (body) {
      this.setState({ catalog: asList(body.catalog), catalogLoaded: true, catalogLoading: again }, this.activate);
    } else {
      this.setState({ catalogLoading: again });
    }
    if (again) {
      this.loadCatalog();
    }
  };

  // activate starts the fetches a catalog answer owes the selection the page is on,
  // which is how a bookmarked selection reaches its targets with nobody touching a menu.
  // The two are tested apart:
  // a port change reaches loadTargets before the catalog has answered and carries no membership check of its own,
  // and one condition over both would leave unstarted the Collections fetch the limits could not start.
  activate = () => {
    if (!this.selectionListed()) {
      return;
    }
    if (this.targetsFor !== this.selection) {
      this.loadTargets();
    }
    this.maybeLoadCollections();
  };

  // invalidateCatalog refetches the catalog an answer says has moved,
  // and records the one further request instead while a request is in flight,
  // so an answer computed before the Service left the cache does not stand as the catalog.
  // However many answers record it, the settling request starts exactly one more.
  invalidateCatalog() {
    if (this.catalogPending) {
      this.catalogAgain = true;

      return;
    }
    this.loadCatalog();
  }

  // loadTargets asks for the listing with explain=true and the port selection,
  // and repeats the one fetch without explain when a gateway refuses the parameter.
  // It is the one fetch Refresh on the targets list repeats, and the not_ready retry is this function too,
  // so each raises the generation and the latest answer is the one applied.
  // A Pod or version choice survives the answer while the new summary still lists it and returns to any otherwise,
  // so no URL is built for a Pod the page no longer lists.
  loadTargets = async () => {
    const { ns, svc } = this.state;
    const seq = ++this.targetsSeq;
    // The selection this fetch ran for, so a catalog answer does not repeat one that selection has had.
    this.targetsFor = this.selection;
    this.setState({ targetsLoading: true });
    const port = this.portChoice();
    const retryOnce = (status, code) =>
      retryWithoutExplain(status, code, true) ? targetsURL(ns, svc, targetsQuery(port, false)) : null;
    const body = await this.request(
      "targets",
      targetsURL(ns, svc, targetsQuery(port, true)),
      this.loadTargets,
      retryOnce,
      () => seq !== this.targetsSeq,
    );
    if (seq !== this.targetsSeq) {
      return;
    }
    if (!body) {
      this.setState({ targets: [], targetSummary: null, pod: "", version: "", targetsLoading: false });
      this.afterServiceError("targets");
      return;
    }
    const summary = targetSummary(body);
    this.setState((s) => ({
      targets: asList(body.targets),
      targetSummary: summary,
      targetsLoading: false,
      pod: summary.pods.includes(s.pod) ? s.pod : "",
      version: summary.versions.includes(s.version) ? s.version : "",
    }));
  };

  // maybeLoadCollections fetches the Collections list once per selection,
  // when the view is offered and the selection is listed;
  // the limits and the catalog answer in either order, and whichever comes last starts it.
  maybeLoadCollections() {
    if (!this.collectionsOffered() || !this.selectionListed() || this.collectionsFor === this.selection) {
      return;
    }
    this.collectionsFor = this.selection;
    this.loadCollections();
  }

  // loadCollections is the one fetch Refresh on the Collections table repeats,
  // over a generation of its own, so a targets fetch never discards its answer.
  loadCollections = async () => {
    if (!this.collectionsOffered()) {
      return;
    }
    const { ns, svc } = this.state;
    const seq = ++this.collectionsSeq;
    this.setState({ collectionsLoading: true });
    const body = await this.request(
      "collections",
      collectionsURL(ns, svc),
      this.loadCollections,
      undefined,
      () => seq !== this.collectionsSeq,
    );
    if (seq !== this.collectionsSeq) {
      return;
    }
    if (!body) {
      this.setState({ collections: [], collectionsNote: "", collectionsLoading: false });
      this.afterServiceError("collections");
      return;
    }
    this.setState({
      collections: asList(body.collections),
      collectionsNote: olderCollectionsNote(body, ns, svc),
      collectionsLoading: false,
    });
  };

  loadCollection = async (id) => {
    if (!isCollectionID(id)) {
      return;
    }
    const retry = () => this.loadCollection(id);
    const body = await this.request("collection", collectionURL(id), retry);
    if (!body) {
      this.setState({ collection: null });
      return;
    }
    this.setState({ collection: body });
  };

  // clearWriteControls tells the start control that the selection moved,
  // drops an armed cancel, and drops the second try of a cancel whose first was answered with a retry,
  // so a page left open holds no loaded button,
  // sends nothing more about the Service it has left, and shows no error about one.
  // An attempt no answer classified says as it goes that a Collection may already exist,
  // which is startNext's message: what it names is the Collections table of the Service being left.
  clearWriteControls() {
    this.startEvent({ kind: "selection" });
    this.onCancelKeep();
    clearTimeout(this.cancelRetryTimer);
    this.clearError("start");
    this.clearError("cancel");
  }

  // reloadWhoami refetches the identity an answer says may have moved.
  // It replaces the realm the controls are drawn from without running boot again,
  // which would blank the page back to its loading state.
  reloadWhoami = async () => {
    const body = await this.request("whoami", whoamiURL(), this.reloadWhoami);
    if (!body || !body.auth || !body.realm) {
      return;
    }
    this.setState({ whoami: body });
  };

  // openIdentity opens the disclosure the realm_denied hint names.
  // The open is set on the element and never bound in the template,
  // because a native close changes it without the template knowing.
  // An answer classified before the identity is on the page has no element to open:
  // render answers booting, navigating, signInRequired, and error above it.
  openIdentity() {
    const node = this.identity.current;
    if (node) {
      node.open = true;
    }
  }

  // refetch runs the fetches an outcome asked for, under the names the model uses.
  // It is not where the disclosure opens:
  // it serves a start's 403 realm_denied, which opens it,
  // and a cancel's 404 collection_not_found, which names no realm that refused and opens nothing.
  refetch(what) {
    if (what === "collections") {
      this.loadCollections();

      return;
    }
    if (what === "whoami") {
      this.reloadWhoami();

      return;
    }
    if (what === "limits") {
      this.loadLimits();
    }
  }

  // showError records under key what a write control's outcome asks the page to show,
  // as the envelope the hint table is keyed by when the answer carried a code,
  // and clears the record when the outcome has nothing to say.
  // The record offers no retry: a repeat of a write is a press, not a button the page adds.
  showError(key, message, code) {
    const shown = code ? { error: message, code: code } : message;
    const record = message === null ? undefined : { error: shown, retry: null };
    this.setState((s) => ({
      errors: { ...s.errors, [key]: record },
      signIn: { ...s.signIn, [key]: undefined },
    }));
  }

  // startEvent hands one event to startNext, stores the state and message it decided,
  // and owns the timers the new phase needs:
  // the ten-second disarm belongs to the armed phase alone,
  // and a cooling phase ends with the timer event that leaves it.
  // The control is in one phase at a time, so one handle holds whichever of the two is pending,
  // and the phase that scheduled it takes it away on the way out.
  // It reports whether the phase moved,
  // which is how the page tells an applied outcome from one the model discarded because the operator had moved on.
  startEvent(event) {
    const was = this.state.start.phase;
    const step = startNext(this.state.start, event);
    const phase = step.state.phase;
    const moved = phase !== was;
    this.setState({ start: step.state, startMessage: step.message });
    if (moved) {
      clearTimeout(this.startTimer);
      if (phase === "armed") {
        this.startTimer = this.later(() => this.startEvent({ kind: "timer", now: Date.now() }), armDelay);
      }
      if (phase === "cooling") {
        const wait = Math.max(0, step.state.until - Date.now());
        this.startTimer = this.later(() => this.startEvent({ kind: "timer", now: Date.now() }), wait);
      }
    }

    return { state: step.state, moved: moved };
  }

  // onStart is the start control's only press.
  // Which of arming and submitting it is comes from the phase startNext holds,
  // so a press while the control is cooling or a request is in flight sends nothing.
  // Both events carry the clock, because the model counts a second press only past half a second from the arm:
  // the second click of a double-click leaves the control armed, and the page sends nothing for it.
  onStart = () => {
    const { ns, svc } = this.state;
    if (this.state.start.phase === "idle") {
      this.startEvent({ kind: "arm", key: newKey(), route: collectionsURL(ns, svc).href, now: Date.now() });

      return;
    }
    const step = this.startEvent({ kind: "submit", now: Date.now() });
    if (step.state.phase === "inflight" && step.moved) {
      this.sendStart(step.state);
    }
  };

  // onStartKeep abandons the attempt: before the first request it just disarms,
  // and after one whose outcome went unclassified it drops the route and the key
  // and says a Collection may already exist, which is startNext's message and not the page's.
  onStartKeep = () => {
    this.startEvent({ kind: "keep" });
  };

  // sendStart posts the start request and hands the answer to startOutcome.
  // The attempt's token goes out with the request and comes back with the answer,
  // so an answer to an attempt the operator has left moves no phase and acts on nothing.
  async sendStart(attempt) {
    const req = startRequest(attempt.route, attempt.key);
    const res = await fetchJSON(req.url, req);
    const answer = answerOf(res);
    const out = startOutcome(answer);
    const step = this.startEvent({
      kind: "outcome",
      token: attempt.token,
      keep: out.keep,
      disableSeconds: out.disableSeconds,
      now: Date.now(),
    });
    if (!step.moved) {
      return;
    }
    // The answer is the current attempt's, because the step moved,
    // so a 403 naming a realm that refused opens the disclosure, before the refetch loop below.
    if (answer.status === 403 && answer.code === "realm_denied") {
      this.openIdentity();
    }
    for (const what of out.refetch) {
      this.refetch(what);
    }
    if (out.select) {
      this.onSelectCollection(out.select);
    }
    this.showError("start", out.error, answer.code);
  }

  // onCancel is a row's cancel press: the first arms that row, the second sends.
  // Arming a second row disarms the first, so one row is armed at a time.
  // The second press counts only past half a second from the arm;
  // one inside that window leaves the row armed and its ten-second disarm running,
  // so the second click of a double-click sends nothing.
  onCancel = (id) => {
    if (this.state.cancelArmed === id && !confirmAccepted(this.cancelArmedAt, Date.now())) {
      return;
    }
    clearTimeout(this.cancelTimer);
    if (this.state.cancelArmed !== id) {
      this.cancelArmedAt = Date.now();
      this.setState({ cancelArmed: id });
      this.cancelTimer = this.later(() => this.setState({ cancelArmed: "" }), armDelay);

      return;
    }
    this.setState({ cancelArmed: "" });
    this.sendCancel(id, 1);
  };

  onCancelKeep = () => {
    clearTimeout(this.cancelTimer);
    this.setState({ cancelArmed: "" });
  };

  // sendCancel posts one cancel and hands the answer to cancelOutcome.
  // tryNumber is which press of this cancel the answer belongs to:
  // a Collection still being published answers the first with a retry a second later,
  // and the second answer is shown.
  async sendCancel(id, tryNumber) {
    const req = cancelRequest(collectionCancelURL(id).href);
    const res = await fetchJSON(req.url, req);
    const answer = answerOf(res);
    const out = cancelOutcome(answer, tryNumber);
    if (out.retryAfterMs > 0) {
      this.cancelRetryTimer = this.later(() => this.sendCancel(id, tryNumber + 1), out.retryAfterMs);

      return;
    }
    if (out.replace) {
      this.replaceCollection(id, out.replace);
    }
    for (const what of out.refetch) {
      this.refetch(what);
    }
    this.showError("cancel", out.error, answer.code);
  }

  // replaceCollection puts the record a cancel returned in the row it was pressed on.
  replaceCollection(id, record) {
    this.setState((s) => ({
      collections: s.collections.map((c) => (c && c.id === id ? record : c)),
    }));
  }

  // afterServiceError refetches the catalog when a targets or
  // Collections fetch answered service_not_found.
  afterServiceError(key) {
    const rec = this.state.errors[key];
    if (rec && typeof rec.error === "object" && rec.error !== null && rec.error.code === "service_not_found") {
      this.invalidateCatalog();
    }
  }

  // writeQuery serializes the selection as ?ns=&svc= without a navigation.
  writeQuery(ns, svc) {
    history.replaceState(null, "", pageURL(ns, svc).href);
  }

  // selectPair is the only writer of the selection after the constructor,
  // whichever control passed the pair.
  // It has one case and not three:
  // identical arguments produce identical behavior,
  // and with both menus derived from the catalog there is nothing to clear and nothing to fetch
  // when only the namespace moves.
  // Clearing either menu is the same call, and the condition on svc is what makes both of them start nothing.
  // It tests no equality either,
  // so the same pair chosen again runs the same transition and refetches its targets,
  // which is what Refresh on that list does.
  // Emptying a filter field belongs to the caller:
  // it is the one thing that differs between a menu pick and a search result naming the same pair.
  selectPair(ns, svc) {
    this.targetsSeq++;
    this.collectionsSeq++;
    this.selection++;
    // The page query is written before the new selection is applied,
    // so the query and the menus never disagree about which pair is chosen.
    this.writeQuery(ns, svc);
    this.setState(
      {
        ns: ns,
        svc: svc,
        targets: [],
        targetSummary: null,
        targetsLoading: false,
        collections: [],
        collectionsNote: "",
        collectionsLoading: false,
        collection: null,
        pod: "",
        version: "",
      },
      () => {
        this.clearError("targets");
        this.clearError("collections");
        this.clearWriteControls();
        if (svc) {
          this.loadTargets();
          this.maybeLoadCollections();
        }
      },
    );
  }

  onNamespace = (e) => {
    // The Service menu is about to offer another namespace's names,
    // and a query typed against the names it was offering says nothing about those.
    this.setState({ svcFilter: "" });
    this.selectPair(e.target.value, "");
  };

  // The two filter fields change what their menu draws and nothing else:
  // no fetch, no page query, and no selection.
  onNsFilter = (e) => {
    this.setState({ nsFilter: e.target.value });
  };

  onSvcFilter = (e) => {
    this.setState({ svcFilter: e.target.value });
  };

  // The search field draws its result rows from the whole catalog and does nothing else:
  // no fetch, no page query, and no selection.
  onSearch = (e) => {
    this.setState({ search: e.target.value });
  };

  // A result row chooses both halves of a pair at once,
  // so the Service menu is about to offer another namespace's names
  // and a query typed against the names it was offering says nothing about those.
  // The filter is cleared before selectPair,
  // which is safe because a pending state change is merged rather than replaced.
  onSearchResult = (ns, name) => {
    this.setState({ svcFilter: "" });
    this.selectPair(ns, name);
  };

  onService = (e) => {
    this.selectPair(this.state.ns, e.target.value);
  };

  onProfile = (e) => {
    const profile = e.target.value;
    this.setState({ profile: profile, seconds: defaultSeconds(this.state.limits, profile), copied: false });
  };

  onSeconds = (e) => {
    this.setState({ seconds: e.target.value, copied: false });
  };

  // The three port handlers go through applyInput, which holds the rules:
  // each stores the returned state, so an edit to one free-form field clears
  // the other in the model, not here.
  onPortChoice = (e) => {
    this.setState({ ...applyInput(this.state, "menu", e.target.value).state, copied: false }, this.refetchTargets);
  };

  onPortNumber = (e) => {
    this.setState({ ...applyInput(this.state, "number", e.target.value).state, copied: false });
  };

  onPortName = (e) => {
    this.setState({ ...applyInput(this.state, "name", e.target.value).state, copied: false });
  };

  refetchTargets = () => {
    if (this.state.svc) {
      this.setState({ pod: "", version: "", targets: [], targetSummary: null }, this.loadTargets);
    }
  };

  // onRefreshTargets repeats the targets fetch and nothing else.
  // It touches neither the start state, the armed cancel, nor the three write-control timers,
  // and it leaves the Collections list alone.
  onRefreshTargets = () => {
    if (this.state.svc) {
      this.loadTargets();
    }
  };

  // onRefreshCatalog repeats the catalog fetch and redraws both menus from the answer.
  // It repeats no fetch downstream of the selection that the selection has already had,
  // and it starts nothing while a catalog request is in flight.
  onRefreshCatalog = () => {
    this.loadCatalog();
  };

  // onRefreshCollections repeats the Collections fetch and nothing else.
  // It touches neither the start state, the armed cancel, nor the three write-control timers,
  // and it leaves the targets list alone.
  onRefreshCollections = () => {
    this.loadCollections();
  };

  onPod = (e) => {
    this.setState({ pod: e.target.value, copied: false });
  };

  onVersion = (e) => {
    this.setState({ version: e.target.value, copied: false });
  };

  onCopy = () => {
    const url = this.currentProfileURL();
    if (!url) {
      return;
    }
    navigator.clipboard.writeText(url.href).then(
      () => this.setState({ copied: true }),
      () => this.setState({ copied: false }),
    );
  };

  // downloadAllowed is whether a press on Download sends a request:
  // a URL is built, the targets answer lists a Pod, and no download is in flight.
  // It is the one rule the control's disabled state, its press, and every retry of it read,
  // so a Retry on an earlier error, or a scheduled not_ready retry,
  // sends nothing after a Refresh whose answer disabled the control.
  downloadAllowed() {
    return Boolean(this.currentProfileURL()) && downloadNote(this.state.targetSummary) === "" && !this.state.downloading;
  }

  // onDownload fetches the profile and saves a 200 through an object URL;
  // any other answer is shown in the Profile panel under the rule every listing follows,
  // and a service_not_found refetches the catalog as the two listings do.
  // The control is disabled from the press until the body has been read whole and the save has begun.
  // The refetch reads the value settle returned and not the recorded error,
  // because a setState is applied later than the line after it.
  onDownload = async () => {
    if (!this.downloadAllowed()) {
      return;
    }
    const url = this.currentProfileURL();
    this.setState({ downloading: true });
    const res = await fetchJSON(url.href, undefined, true);
    if (res.blob !== null) {
      this.clearError("download");
      saveBlob(res.blob, filenameOf(res.headers.get("content-disposition")));
    } else {
      const err = this.settle("download", res, this.onDownload);
      if (isEnvelope(err) && err.code === "service_not_found") {
        this.invalidateCatalog();
      }
    }
    this.setState({ downloading: false });
  };

  onSelectCollection = (id) => {
    if (!isCollectionID(id)) {
      this.setState({ collection: null });
      return;
    }
    this.loadCollection(id);
  };

  // portChoice is what the port control sends: {port}, {portName}, or {}
  // for default, as applyInput reads the current state with no edit.
  portChoice() {
    return applyInput(this.state).params;
  }

  // selectionListed reports whether the catalog holds the page's namespace and Service as one pair;
  // an unlisted bookmark leaves the control unselected and builds no URL.
  // It reads the whole catalog and never a filtered menu,
  // because it gates the Collections view, the profile URL, and the start control,
  // none of which may turn on what someone typed into a filter field.
  selectionListed() {
    const { ns, svc, catalog } = this.state;

    return Boolean(ns && svc && servicesOf(catalog, ns).includes(svc));
  }

  collectionsOffered() {
    const { limits, whoami } = this.state;
    return tableOffered(limits, whoami);
  }

  // currentProfileURL is the download URL for the selection, or null when the
  // selection is incomplete or unlisted or the duration is out of range.
  currentProfileURL() {
    const params = profileRequest(this.state, this.portChoice(), this.selectionListed());
    if (!params) {
      return null;
    }
    const { ns, svc, profile } = this.state;
    return profileURL(ns, svc, profile, params);
  }

  render() {
    const { phase } = this.state;
    if (phase === "booting" || phase === "navigating") {
      return html`<article><p aria-busy="true">${phase === "navigating" ? "Signing in" : "Loading"}</p></article>`;
    }
    if (phase === "signInRequired") {
      const oidc = this.returnedFromLogin || this.navigatedToLogin;
      return html`
        <article>
          <${SignInRequired} onRetry=${oidc ? this.signIn : this.boot} label=${oidc ? "Sign in again" : "Retry"} />
        </article>
      `;
    }
    if (phase === "error") {
      return html`<article><${ErrorBox} error=${this.state.bootError} onRetry=${this.boot} /></article>`;
    }
    return html`
      ${this.renderIdentity()}
      ${this.panelError("whoami")}
      <div class="panels">
        ${this.renderSelection()}
        ${this.renderRequest()}
        ${this.collectionsOffered() ? this.renderCollections() : null}
      </div>
    `;
  }

  // panelError renders the error or sign-in notice recorded under key; under
  // oidc the sign-in notice offers another login rather than a retry.
  panelError(key) {
    const retry = this.state.signIn[key];
    if (retry) {
      const oidc = this.state.whoami.auth.mode === "oidc";
      return html`<${SignInRequired} onRetry=${oidc ? this.signIn : retry} label=${oidc ? "Sign in again" : "Retry"} />`;
    }
    const rec = this.state.errors[key];
    if (!rec) {
      return null;
    }
    return html`<${ErrorBox} error=${rec.error} onRetry=${rec.retry} />`;
  }

  renderIdentity() {
    const w = this.state.whoami;
    const realm = w.realm;
    const mode = w.auth.mode;
    return html`
      <details class="identity" ref=${this.identity}>
        <summary>
          <strong>Identity</strong>
          <span class="who">${text(w.principal)} in ${text(realm.name)}</span>
        </summary>
        <dl>
          <dt>Principal</dt>
          <dd>${text(w.principal)}</dd>
          <dt>Realm</dt>
          <dd>${text(realm.name)}</dd>
          <dt>Namespaces</dt>
          <dd>${asList(realm.namespaces).join(", ") || "-"}</dd>
          <dt>Services</dt>
          <dd>${asList(realm.services).join(", ") || "-"}</dd>
          <dt>Profiles</dt>
          <dd>${asList(realm.profiles).join(", ") || "-"}</dd>
          <dt>PGO</dt>
          <dd>
            ${realm.pgo && realm.pgo.read ? "read " : ""}
            ${realm.pgo && realm.pgo.collect ? "collect " : ""}
            ${realm.pgo && realm.pgo.configure ? "configure" : ""}
            ${realm.pgo && !realm.pgo.read && !realm.pgo.collect && !realm.pgo.configure ? "none" : ""}
          </dd>
          <dt>Authentication</dt>
          <dd>${text(mode)}</dd>
        </dl>
        <footer>
          ${w.auth.logout ? html`<a href=${logoutURL().href}>Sign out</a>` : null}
          ${mode === "basic"
            ? html`<small>A Basic credential is the browser's to keep; there is no sign-out here.</small>`
            : null}
        </footer>
      </details>
    `;
  }

  renderSelection() {
    const { ns, svc, catalog, nsFilter, svcFilter, search, catalogLoaded, catalogLoading } = this.state;
    // Both menus are derived from the one catalog answer here, at render,
    // and neither list is stored beside it.
    const namespaces = namespacesOf(catalog);
    const services = servicesOf(catalog, ns);
    const nsListed = !ns || namespaces.includes(ns);
    const svcListed = !svc || services.includes(svc);
    // A menu offers what its own filter admits, plus the value it is showing,
    // which is what keeps a query from removing the current selection.
    // Whether a value is listed, and the placeholder each menu carries,
    // are read from the whole catalog instead,
    // so a query that matches nothing narrows a menu without unlisting anything.
    const nsFiltered = filterOptions(namespaces, nsFilter, ns);
    const svcFiltered = filterOptions(services, svcFilter, svc);
    const nsOptions = nsFiltered.options;
    const svcOptions = svcFiltered.options;
    // The search reads the whole catalog rather than either menu,
    // so a row it draws can name a namespace the namespace filter is hiding.
    // Its rows stand only once someone has typed,
    // because an empty query matches nothing and a page of every Service is the menus' job.
    const found = searchCatalog(catalog, search, searchLimit);
    const searching = search.trim() !== "";
    // A catalog that has not arrived is either still being read or was asked for and refused.
    // The refusal stands above the menus with the reason and the control that asks again,
    // so the line under the search says which of the two it is waiting on rather than claiming the first.
    const catalogFailed = Boolean(this.state.errors.catalog || this.state.signIn.catalog);
    // Each menu and the field that narrows it sit in one cell as two sibling labels.
    // The field is never nested inside the menu's label,
    // because a label naming a menu is the name of that one control.
    return html`
      <article class="selection">
        <header><strong>Service</strong></header>
        ${this.panelError("catalog")}
        <div class="fields">
          <div class="menu">
            <label>
              Namespace
              <select value=${nsListed ? ns : ""} onChange=${this.onNamespace}>
                <option value="">${namespaces.length ? "choose a namespace" : "no namespace listed"}</option>
                ${nsOptions.map((n) => html`<option key=${n} value=${n}>${n}</option>`)}
              </select>
            </label>
            <label>
              Namespace filter
              <input type="search" value=${nsFilter} onInput=${this.onNsFilter} />
            </label>
            <${FilterNote}
              query=${nsFilter}
              shown=${nsOptions.length}
              matched=${nsFiltered.matched}
              total=${namespaces.length}
            />
          </div>
          <div class="menu">
            <label>
              Service
              <select value=${svcListed ? svc : ""} onChange=${this.onService} disabled=${!ns || !nsListed}>
                <option value="">${services.length ? "choose a Service" : "no Service listed"}</option>
                ${svcOptions.map((s) => html`<option key=${s} value=${s}>${s}</option>`)}
              </select>
            </label>
            <label>
              Service filter
              <input type="search" value=${svcFilter} onInput=${this.onSvcFilter} />
            </label>
            <${FilterNote}
              query=${svcFilter}
              shown=${svcOptions.length}
              matched=${svcFiltered.matched}
              total=${services.length}
            />
          </div>
          <div class="actions">
            <button type="button" class="secondary" disabled=${catalogLoading} onClick=${this.onRefreshCatalog}>
              Refresh
            </button>
          </div>
        </div>
        ${nsListed ? null : html`<p><small>${ns} is not listed</small></p>`}
        ${svcListed ? null : html`<p><small>${svc} is not listed</small></p>`}
        <div class="search">
          <label>
            Find a Service in any namespace
            <input
              type="search"
              placeholder="namespace/service"
              value=${search}
              onInput=${this.onSearch}
            />
          </label>
          ${searching
            ? html`
                <div class="results">
                  ${found.matches.map(
                    (entry) => html`
                      <button
                        type="button"
                        key=${entry.namespace + "/" + entry.name}
                        onClick=${() => this.onSearchResult(entry.namespace, entry.name)}
                      >
                        ${entry.namespace}/${entry.name}
                      </button>
                    `,
                  )}
                </div>
                <${SearchNote}
                  loaded=${catalogLoaded}
                  failed=${catalogFailed}
                  shown=${found.matches.length}
                  total=${found.total}
                />
              `
            : null}
        </div>
      </article>
    `;
  }

  renderPortControl() {
    const limits = this.state.limits;
    // The menu's options and which free-form fields exist come from the model:
    // one option per allowedSelections entry, a wildcard entry opening a field
    // for its kind instead, and an empty list offering the default alone.
    const control = deriveControl(limits && limits.pprof);
    return html`
      <fieldset>
        <legend>Port</legend>
        <select value=${this.state.portChoice} onChange=${this.onPortChoice}>
          ${control.options.map((o) => html`<option key=${o.value} value=${o.value}>${o.label}</option>`)}
        </select>
        ${control.numberField
          ? html`
              <label>
                Port number
                <input
                  type="number"
                  inputmode="numeric"
                  min="1"
                  max="65535"
                  placeholder="any port number"
                  value=${this.state.portNumber}
                  onInput=${this.onPortNumber}
                  onChange=${this.refetchTargets}
                />
              </label>
            `
          : null}
        ${control.nameField
          ? html`
              <label>
                Port name
                <input
                  type="text"
                  placeholder="any port name"
                  value=${this.state.portName}
                  onInput=${this.onPortName}
                  onChange=${this.refetchTargets}
                />
              </label>
            `
          : null}
      </fieldset>
    `;
  }

  renderRequest() {
    const {
      ns,
      svc,
      profile,
      seconds,
      pod,
      version,
      targets,
      targetSummary: summary,
      targetsLoading,
      limits,
      whoami,
      copied,
      downloading,
    } = this.state;
    const profiles = offeredProfiles(limits, whoami);
    const svcListed = this.selectionListed();
    const limit = secondsLimit(limits, profile);
    const valid = secondsValid(limits, profile, seconds);
    const url = this.currentProfileURL();
    const pods = summary ? summary.pods : [];
    const versions = summary ? summary.versions : [];
    const empty = summary ? summary.empty : null;
    // note is the line beside Download while the targets response lists no Pod; the control is disabled with it.
    // While no response has arrived for the selection, and after one that failed, the summary is null and the note empty,
    // because a request the page cannot foresee failing is one the user may send and read the answer to.
    const note = downloadNote(summary);
    const canCopy = Boolean(navigator.clipboard && typeof navigator.clipboard.writeText === "function");
    const mode = whoami.auth.mode;
    const copyNote =
      mode === "disabled"
        ? "The URL works as is with go tool pprof."
        : mode === "basic"
          ? "Add user:password@ to the URL or use curl -u; the copied URL carries no credential."
          : "Under oidc the URL needs a bearer token that go tool pprof cannot send.";
    return html`
      <article class="request">
        <header><strong>Profile</strong></header>
        ${this.panelError("limits")}
        ${limits
          ? html`
              <div class="fields">
              <label>
                Profile
                <select value=${profile} onChange=${this.onProfile}>
                  ${profiles.map((p) => html`<option key=${p} value=${p}>${p}</option>`)}
                </select>
              </label>
              ${limit
                ? html`
                    <label>
                      Seconds
                      <input
                        type="number"
                        min="1"
                        max=${limit}
                        step="1"
                        value=${seconds}
                        aria-invalid=${valid ? undefined : "true"}
                        onInput=${this.onSeconds}
                      />
                      ${valid ? null : html`<small>seconds must be an integer from 1 to ${limit}</small>`}
                    </label>
                  `
                : null}
              ${this.renderPortControl()}
              ${empty
                ? html`<div class="empty">${this.renderEmptyTargets(empty)}</div>`
                : html`
                    <label>
                      Pod
                      <select value=${pod} onChange=${this.onPod} disabled=${!svcListed}>
                        <option value="">any</option>
                        ${pods.map((p) => html`<option key=${p} value=${p}>${p}</option>`)}
                      </select>
                    </label>
                    <label>
                      Version
                      <select value=${version} onChange=${this.onVersion} disabled=${!svcListed}>
                        <option value="">any</option>
                        ${versions.map((v) => html`<option key=${v} value=${v}>${v}</option>`)}
                      </select>
                    </label>
                  `}
                <div class="actions">
                  <button
                    type="button"
                    class="secondary"
                    disabled=${!svcListed || targetsLoading}
                    onClick=${this.onRefreshTargets}
                  >
                    Refresh
                  </button>
                </div>
              </div>
              ${this.panelError("targets")}
              ${svcListed && !empty && !this.state.errors.targets && !this.state.signIn.targets && targets.length === 0
                ? html`<p><small>no target listed yet</small></p>`
                : null}
              <label>
                Profile URL
                <input type="text" class="url" readOnly value=${url ? url.href : ""} />
              </label>
              <div class="actions">
                <button type="button" disabled=${!this.downloadAllowed()} onClick=${this.onDownload}>
                  ${downloading ? "Downloading" : "Download"}
                </button>
                ${note ? html`<small>${note}</small>` : null}
                ${url && canCopy ? html`<button type="button" onClick=${this.onCopy}>Copy URL</button>` : null}
                ${copied ? html`<small>copied</small>` : null}
              </div>
              ${this.panelError("download")}
              <div class="notes">
                <p><small>${copyNote}</small></p>
                ${url
                  ? html`<p><small>From a terminal: <code>profgate profile ${ns}/${svc} ${profile}</code></small></p>`
                  : null}
                ${!ns || !svc ? html`<p><small>choose a namespace and a Service to build the URL</small></p>` : null}
                ${ns && svc && !svcListed ? html`<p><small>the selection is not listed, so no URL is built</small></p>` : null}
              </div>
            `
          : null}
      </article>
    `;
  }

  // renderEmptyTargets stands where the Pod and version controls were when the listing is empty:
  // the counted reasons in the gateway's order, the sentence for a selector matching no Pod,
  // or the plain empty state.
  // Every reason text and count reaches the template as a text node.
  renderEmptyTargets(empty) {
    if (empty.kind === "reasons") {
      return html`
        <p><small>no target: every selected Pod was excluded</small></p>
        <table class="reasons">
          <tbody>
            ${empty.rows.map(
              (r) => html`
                <tr key=${r.reason}>
                  <td>${r.count}</td>
                  <td>${r.text}</td>
                </tr>
              `,
            )}
          </tbody>
        </table>
      `;
    }
    if (empty.kind === "noSelector") {
      return html`<p><small>the Service's selector matches no Pod</small></p>`;
    }
    return html`<p><small>no target listed yet</small></p>`;
  }

  // renderStart is the Start collection control.
  // It exists only when startOffered says all four of its conditions hold,
  // because a control whose every answer would be a refusal is a question the page does not ask.
  // It confirms in place: the first press arms it and the second sends the request.
  // What an answer said sits outside it, beside the cancel's:
  // a realm_denied refetches the identity that takes the control away,
  // and an error drawn inside the control would go with it.
  renderStart() {
    const { start, limits, whoami, svc } = this.state;
    if (!startOffered(limits, whoami, this.selectionListed() ? svc : "")) {
      return null;
    }
    const phase = start.phase;
    const armed = phase === "armed" || phase === "retained";
    const waiting = phase === "cooling" ? Math.ceil(Math.max(0, start.until - Date.now()) / 1000) : 0;
    return html`
      <div class="actions">
        ${armed
          ? html`
              <button type="button" class="armed" onClick=${this.onStart}>Confirm start</button>
              <button type="button" class="secondary" onClick=${this.onStartKeep}>Keep</button>
            `
          : html`
              <button type="button" onClick=${this.onStart} disabled=${phase !== "idle"}>
                ${phase === "cooling" ? `Waiting ${waiting} seconds` : phase === "inflight" ? "Starting" : "Start collection"}
              </button>
            `}
      </div>
    `;
  }

  renderCollections() {
    const { svc, collections, collectionsNote, collectionsLoading, collection, startMessage } = this.state;
    return html`
      <article class="collections">
        <header>
          <strong>Collections</strong>
          <button
            type="button"
            class="secondary"
            disabled=${!svc || collectionsLoading}
            onClick=${this.onRefreshCollections}
          >
            Refresh
          </button>
        </header>
        ${this.panelError("collections")}
        ${this.renderStart()}
        ${startMessage ? html`<p><small>${startMessage}</small></p>` : null}
        ${this.panelError("start")}
        ${this.panelError("cancel")}
        ${svc
          ? html`
              <div class="table">
                <table>
                  <thead>
                    <tr>
                      <th>id</th>
                      <th>origin</th>
                      <th>state</th>
                      <th>attempt</th>
                      <th>resolvedVersion</th>
                      <th>createdAt</th>
                      <th>finishedAt</th>
                      <th>expiresAt</th>
                      <th>cancel</th>
                    </tr>
                  </thead>
                  <tbody>
                    ${collections
                      .filter((c) => c !== null && typeof c === "object")
                      .map(
                        (c) => html`
                        <tr key=${text(c.id)}>
                          <td>
                            ${isCollectionID(c.id)
                              ? html`
                                  <button type="button" class="link" onClick=${() => this.onSelectCollection(c.id)}>
                                    ${c.id}
                                  </button>
                                `
                              : text(c.id)}
                          </td>
                          <td>${text(c.origin)}</td>
                          <td>${text(c.state)}</td>
                          <td>${text(c.attempt)}</td>
                          <td>${text(c.resolvedVersion)}</td>
                          <${TimeCell} value=${c.createdAt} />
                          <${TimeCell} value=${c.finishedAt} />
                          <${TimeCell} value=${c.expiresAt} />
                          <td class="row-actions">${this.renderCancel(c)}</td>
                        </tr>
                      `,
                      )}
                  </tbody>
                </table>
              </div>
              ${collectionsNote ? html`<p><small>${collectionsNote}</small></p>` : null}
              ${collections.length ? null : html`<p><small>no Collection listed</small></p>`}
            `
          : html`<p><small>choose a Service to list its Collections</small></p>`}
        ${this.panelError("collection")}
        ${collection ? this.renderCollectionDetail(collection) : null}
      </article>
    `;
  }

  // renderCancel is a row's cancel control, offered on the states cancelOffered admits
  // and only for an identifier a path can be built from,
  // so no press turns into a request to a path built from a record the gateway never wrote.
  // It confirms in place, the way the start control does.
  renderCancel(c) {
    if (!cancelOffered(c.state, this.state.whoami) || !isCollectionID(c.id)) {
      return null;
    }
    if (this.state.cancelArmed !== c.id) {
      return html`<button type="button" onClick=${() => this.onCancel(c.id)}>Cancel</button>`;
    }
    return html`
      <button type="button" class="armed" onClick=${() => this.onCancel(c.id)}>Confirm cancel</button>
      <button type="button" class="secondary" onClick=${this.onCancelKeep}>Keep</button>
    `;
  }

  renderCollectionDetail(c) {
    const artifact = c.artifact && typeof c.artifact === "object" ? c.artifact : null;
    const progress = c.progress && typeof c.progress === "object" ? c.progress : {};
    const showReason = c.state === "failed" || c.state === "cancelled";
    return html`
      <details open>
        <summary>Collection ${text(c.id)}</summary>
        <dl>
          <dt>state</dt>
          <dd>${text(c.state)}</dd>
          ${showReason ? html`<dt>reason</dt><dd>${text(c.reason)}</dd>` : null}
          <dt>progress</dt>
          <dd>${progressText(progress) || "-"}</dd>
          <dt>createdBy</dt>
          <dd>${text(c.createdBy)}</dd>
          <dt>createdAt</dt>
          <dd>${text(c.createdAt)}</dd>
          <dt>startedAt</dt>
          <dd>${text(c.startedAt)}</dd>
          <dt>finishedAt</dt>
          <dd>${text(c.finishedAt)}</dd>
          <dt>expiresAt</dt>
          <dd>${text(c.expiresAt)}</dd>
          <dt>artifact.bytes</dt>
          <dd>${artifact ? text(artifact.bytes) : "-"}</dd>
        </dl>
        ${this.renderCollectionDownload(c, artifact)}
      </details>
    `;
  }

  // renderCollectionDownload links the artifact only from the detail record,
  // only when it is completed with an artifact, and only for an id the
  // identifier grammar accepts.
  renderCollectionDownload(c, artifact) {
    if (c.state !== "completed" || artifact === null) {
      return null;
    }
    try {
      const url = collectionProfileURL(c.id);
      return html`<a href=${url.href} download role="button">Download profile</a>`;
    } catch {
      return html`<p><small>this record's id is not linkable</small></p>`;
    }
  }
}

render(html`<${App} />`, document.getElementById("app"));
