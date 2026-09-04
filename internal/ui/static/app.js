// The console: one Preact class component holding the page's state,
// rendered through htm bound to Preact's h.
// Every response-derived value reaches the template as ${value} in child or attribute position,
// so it is set as text or as a DOM property and never parsed as markup.
// Every URL comes from urls.js; this module spells no path.
import { h, Component, render } from "./vendor/preact/preact.module.js";
import htm from "./vendor/htm/htm.module.js";
import {
  namespacesURL,
  servicesURL,
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
import { targetsQuery, retryWithoutExplain, targetSummary } from "./targetmodel.js";
import {
  startOffered,
  cancelOffered,
  uuidFromBytes,
  startRequest,
  cancelRequest,
  startOutcome,
  cancelOutcome,
  startNext,
  progressText,
} from "./collectionmodel.js";

const html = htm.bind(h);

// The upstream defaults for the two duration-bearing profiles; the page sends
// min(default, limit) explicitly so the request never relies on the upstream.
const upstreamSeconds = { cpu: 30, trace: 1 };

// notReadyDelay is how often a not_ready answer is retried.
const notReadyDelay = 2000;

// armDelay is how long a control that has armed itself waits for the second press that sends the request.
// It runs before the first request only:
// submission cancels it,
// and nothing restarts it while a request is in flight or after an outcome the page could not classify,
// where disarming would drop the key that a repeat of the same attempt needs.
const armDelay = 10000;

// hints are the one-line hints for the codes a user can act on; every other
// code is shown as is.
const hints = {
  not_ready: "the gateway is still syncing; the page retries every 2 seconds",
  realm_denied: "your realm does not admit this; the identity panel shows what it does",
  service_not_found: "the Service left the cache since the list was fetched; the page refreshes the Service list",
  no_targets: "no Ready Pod declares the selected port",
  port_not_allowed: "the value is outside the allowlist shown in the port control",
  seconds_exceeds_limit: "the limit the duration input was bounded by",
  discovery_unavailable: "the gateway could not read its cache or confirm the Pod; retry",
  pgo_disabled: "PGO collection is off on this gateway; the Collections view goes once the limits have been refetched",
  pgo_unavailable: "the gateway could not reach its store, so a start may or may not have taken; the same press can be repeated",
  collector_unavailable:
    "nothing is running Collections at the moment, and nothing was started; the press can be repeated once something is",
  collection_in_progress: "a Collection is already running for this Service; the list shows it",
  rate_limited: "the gateway is at its limit for now; the control returns after the delay",
  capacity_exhausted: "the gateway is at its limit for now; the control returns after the delay",
  collection_terminal: "the Collection ended before the cancel arrived; the list shows its final state",
  collection_initializing: "the Collection is still being created; the cancel is retried once",
  limit_exceeded: "the Collection's policy exceeds a configured ceiling; the message names the fields",
};

// fetchJSON runs one same-origin request and resolves to
// {status, headers, body, bodyText, rejected, code, message, error}.
// req, when given, is the request description a write control built:
// its method, its headers, and its body, which is null for a request that carries none.
// Without one the request is a GET, which is what every listing fetch is.
// body is the decoded JSON when the Content-Type says JSON, and bodyText is the body as it arrived;
// rejected says fetch itself never produced a response;
// code and message are the error envelope's two fields when the body is one, and empty strings otherwise.
// error composes those parts for the page's error surface:
// the envelope, or the string "HTTP <status> <statusText>", or "request failed" with status 0.
// The parts stay beside it because collectionmodel.js branches on the status and the code,
// and a composed string cannot be branched on.
// It decides nothing about 401: the caller does, because what a 401 means depends on the mode.
async function fetchJSON(url, req) {
  const out = {
    status: 0,
    headers: new Headers(),
    body: null,
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
    out.bodyText = await res.text();
    const ctype = res.headers.get("content-type") || "";
    if (ctype.startsWith("application/json")) {
      try {
        out.body = JSON.parse(out.bodyText);
      } catch {
        out.body = null;
      }
    }
    if (res.ok && out.body !== null) {
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

// listAllows mirrors the gateway's realm filter: the wildcard or the name.
function listAllows(list, value) {
  return Array.isArray(list) && (list.includes("*") || list.includes(value));
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
    // seq stamps each targets or Collections request so a stale answer is
    // dropped when a later selection has already replaced it.
    this.seq = 0;
    // selection counts namespace and Service changes, and collectionsFor is
    // the selection the Collections list was last requested for, so the list
    // is fetched once whether limits or the Service list answers last.
    this.selection = 0;
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
    this.state = {
      phase: "booting",
      bootError: null,
      whoami: null,
      limits: null,
      ns: q.ns,
      svc: q.svc,
      namespaces: [],
      services: [],
      targets: [],
      targetSummary: null,
      collections: [],
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
      // start is the start control's whole state, as startNext keeps it,
      // and startMessage is what that model last asked the page to say.
      start: { phase: "idle", key: null, route: null, token: 0, until: 0 },
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
    this.loadNamespaces();
  }

  // request runs one request after boot and applies the mode the page now
  // knows to a 401: oidc navigates once, basic asks to sign in with a retry,
  // disabled shows it as an error. not_ready is retried every 2 seconds.
  // It resolves to the body on 200 and null otherwise, after recording the
  // error under key.
  // retryOnce, when given, is consulted once when the first attempt is not a 200:
  // it takes the status and the envelope code and returns a URL to fetch instead, or null.
  // The second answer is then handled exactly as a first one would be, and never consulted again.
  async request(key, url, retry, retryOnce) {
    let res = await fetchJSON(url);
    if (retryOnce && res.status !== 200) {
      const code = typeof res.error === "object" && res.error !== null ? res.error.code : undefined;
      const again = retryOnce(res.status, code);
      if (again) {
        res = await fetchJSON(again);
      }
    }
    if (res.status === 200 && res.body !== null) {
      this.clearError(key);
      return res.body;
    }
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
    return null;
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
      const profile = this.offeredProfiles().includes("cpu") ? "cpu" : this.offeredProfiles()[0] || "";
      this.setState({ profile: profile, seconds: this.defaultSeconds(profile) });
      this.maybeLoadCollections();
    });
  };

  loadNamespaces = async () => {
    const body = await this.request("namespaces", namespacesURL(), this.loadNamespaces);
    if (!body) {
      return;
    }
    const namespaces = asList(body.namespaces);
    this.setState({ namespaces: namespaces }, () => {
      if (this.state.ns && namespaces.includes(this.state.ns)) {
        this.loadServices();
      }
    });
  };

  loadServices = async () => {
    const ns = this.state.ns;
    const body = await this.request("services", servicesURL(ns), this.loadServices);
    if (!body || this.state.ns !== ns) {
      return;
    }
    const services = asList(body.services);
    this.setState({ services: services }, () => {
      if (this.state.svc && services.includes(this.state.svc)) {
        this.loadTargets();
        this.maybeLoadCollections();
      }
    });
  };

  // loadTargets asks for the listing with explain=true and the port selection,
  // and repeats the one fetch without explain when a gateway refuses the parameter.
  loadTargets = async () => {
    const { ns, svc } = this.state;
    const seq = ++this.seq;
    const port = this.portChoice();
    const retryOnce = (status, code) =>
      retryWithoutExplain(status, code, true) ? targetsURL(ns, svc, targetsQuery(port, false)) : null;
    const body = await this.request(
      "targets",
      targetsURL(ns, svc, targetsQuery(port, true)),
      this.loadTargets,
      retryOnce,
    );
    if (seq !== this.seq) {
      return;
    }
    if (!body) {
      this.setState({ targets: [], targetSummary: null, pod: "", version: "" });
      this.afterServiceError("targets");
      return;
    }
    this.setState({ targets: asList(body.targets), targetSummary: targetSummary(body) });
  };

  // maybeLoadCollections fetches the Collections list once per selection,
  // when the view is offered and the selection is listed; limits and the
  // Service list answer in either order, and whichever comes last starts it.
  maybeLoadCollections() {
    if (!this.collectionsOffered() || !this.selectionListed() || this.collectionsFor === this.selection) {
      return;
    }
    this.collectionsFor = this.selection;
    this.loadCollections();
  }

  loadCollections = async () => {
    if (!this.collectionsOffered()) {
      return;
    }
    const { ns, svc } = this.state;
    const seq = this.seq;
    const body = await this.request("collections", collectionsURL(ns, svc), this.loadCollections);
    if (seq !== this.seq) {
      return;
    }
    if (!body) {
      this.setState({ collections: [] });
      this.afterServiceError("collections");
      return;
    }
    this.setState({ collections: asList(body.collections) });
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

  // refetch runs the fetches an outcome asked for, under the names the model uses.
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
  onStart = () => {
    const { ns, svc } = this.state;
    if (this.state.start.phase === "idle") {
      this.startEvent({ kind: "arm", key: newKey(), route: collectionsURL(ns, svc).href });

      return;
    }
    const step = this.startEvent({ kind: "submit" });
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
  onCancel = (id) => {
    clearTimeout(this.cancelTimer);
    if (this.state.cancelArmed !== id) {
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

  // afterServiceError refetches the Service list when a targets or
  // Collections fetch answered service_not_found.
  afterServiceError(key) {
    const rec = this.state.errors[key];
    if (rec && typeof rec.error === "object" && rec.error !== null && rec.error.code === "service_not_found") {
      this.loadServices();
    }
  }

  // writeQuery serializes the selection as ?ns=&svc= without a navigation.
  writeQuery(ns, svc) {
    history.replaceState(null, "", pageURL(ns, svc).href);
  }

  onNamespace = (e) => {
    const ns = e.target.value;
    this.seq++;
    this.selection++;
    this.writeQuery(ns, "");
    this.setState(
      {
        ns: ns,
        svc: "",
        services: [],
        targets: [],
        targetSummary: null,
        collections: [],
        collection: null,
        pod: "",
        version: "",
      },
      () => {
        this.clearError("services");
        this.clearError("targets");
        this.clearError("collections");
        this.clearWriteControls();
        if (ns) {
          this.loadServices();
        }
      },
    );
  };

  onService = (e) => {
    const svc = e.target.value;
    this.seq++;
    this.selection++;
    this.writeQuery(this.state.ns, svc);
    this.setState(
      { svc: svc, targets: [], targetSummary: null, collections: [], collection: null, pod: "", version: "" },
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
  };

  onProfile = (e) => {
    const profile = e.target.value;
    this.setState({ profile: profile, seconds: this.defaultSeconds(profile), copied: false });
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

  onSelectCollection = (id) => {
    if (!isCollectionID(id)) {
      this.setState({ collection: null });
      return;
    }
    this.loadCollection(id);
  };

  // offeredProfiles is limits.profiles filtered by realm.profiles.
  offeredProfiles() {
    const { limits, whoami } = this.state;
    if (!limits || !whoami) {
      return [];
    }
    return asList(limits.profiles).filter((p) => listAllows(whoami.realm.profiles, p));
  }

  // secondsLimit is the profile's bound, or 0 for a profile with no duration.
  secondsLimit(profile) {
    const limits = this.state.limits;
    if (!limits) {
      return 0;
    }
    if (profile === "cpu") {
      return Number(limits.cpuSeconds) || 0;
    }
    if (profile === "trace") {
      return Number(limits.traceSeconds) || 0;
    }
    return 0;
  }

  // defaultSeconds is the upstream default or the limit when it is lower.
  defaultSeconds(profile) {
    const limit = this.secondsLimit(profile);
    if (!limit) {
      return "";
    }
    return String(Math.min(upstreamSeconds[profile], limit));
  }

  // secondsValid reports whether the duration is an integer in 1..limit.
  secondsValid() {
    const limit = this.secondsLimit(this.state.profile);
    if (!limit) {
      return true;
    }
    const n = Number(this.state.seconds);
    return Number.isInteger(n) && n >= 1 && n <= limit;
  }

  // portChoice is what the port control sends: {port}, {portName}, or {}
  // for default, as applyInput reads the current state with no edit.
  portChoice() {
    return applyInput(this.state).params;
  }

  // selectionListed reports whether the page's namespace and Service are both
  // in the fetched lists; an unlisted bookmark leaves the control unselected
  // and builds no URL.
  selectionListed() {
    const { ns, svc, namespaces, services } = this.state;
    return Boolean(ns && svc && namespaces.includes(ns) && services.includes(svc));
  }

  collectionsOffered() {
    const { limits, whoami } = this.state;
    return Boolean(limits && limits.pgo && limits.pgo.enabled && whoami && whoami.realm.pgo && whoami.realm.pgo.read);
  }

  // currentProfileURL is the download URL for the selection, or null when the
  // selection is incomplete or unlisted or the duration is out of range.
  currentProfileURL() {
    const { ns, svc, profile, pod, version } = this.state;
    if (!this.selectionListed() || !profile || !this.secondsValid()) {
      return null;
    }
    const port = this.portChoice();
    const params = { pod: pod, version: version, port: port.port, portName: port.portName };
    if (this.secondsLimit(profile)) {
      params.seconds = this.state.seconds;
    }
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
      <div class="panels">
        ${this.renderIdentity()}
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
      <article>
        <header><strong>Identity</strong></header>
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
        ${this.panelError("whoami")}
        <footer>
          ${w.auth.logout ? html`<a href=${logoutURL().href}>Sign out</a>` : null}
          ${mode === "basic"
            ? html`<small>A Basic credential is the browser's to keep; there is no sign-out here.</small>`
            : null}
        </footer>
      </article>
    `;
  }

  renderSelection() {
    const { ns, svc, namespaces, services } = this.state;
    const nsListed = !ns || namespaces.includes(ns);
    const svcListed = !svc || services.includes(svc);
    return html`
      <article>
        <header><strong>Service</strong></header>
        <label>
          Namespace
          <select value=${nsListed ? ns : ""} onChange=${this.onNamespace}>
            <option value="">${namespaces.length ? "choose a namespace" : "no namespace listed"}</option>
            ${namespaces.map((n) => html`<option key=${n} value=${n}>${n}</option>`)}
          </select>
        </label>
        ${nsListed ? null : html`<p><small>${ns} is not listed</small></p>`}
        ${this.panelError("namespaces")}
        <label>
          Service
          <select value=${svcListed ? svc : ""} onChange=${this.onService} disabled=${!ns || !nsListed}>
            <option value="">${services.length ? "choose a Service" : "no Service listed"}</option>
            ${services.map((s) => html`<option key=${s} value=${s}>${s}</option>`)}
          </select>
        </label>
        ${svcListed ? null : html`<p><small>${svc} is not listed</small></p>`}
        ${this.panelError("services")}
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
    const { ns, svc, profile, seconds, pod, version, targets, targetSummary: summary, limits, whoami, copied } = this.state;
    const profiles = this.offeredProfiles();
    const svcListed = this.selectionListed();
    const limit = this.secondsLimit(profile);
    const valid = this.secondsValid();
    const url = this.currentProfileURL();
    const pods = summary ? summary.pods : [];
    const versions = summary ? summary.versions : [];
    const empty = summary ? summary.empty : null;
    const canCopy = Boolean(navigator.clipboard && typeof navigator.clipboard.writeText === "function");
    const mode = whoami.auth.mode;
    const copyNote =
      mode === "disabled"
        ? "The URL works as is with go tool pprof."
        : mode === "basic"
          ? "Add user:password@ to the URL or use curl -u; the copied URL carries no credential."
          : "Under oidc the URL needs a bearer token that go tool pprof cannot send.";
    return html`
      <article>
        <header><strong>Profile</strong></header>
        ${this.panelError("limits")}
        ${limits
          ? html`
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
                ? this.renderEmptyTargets(empty)
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
              ${this.panelError("targets")}
              ${svcListed && !empty && !this.state.errors.targets && !this.state.signIn.targets && targets.length === 0
                ? html`<p><small>no target listed yet</small></p>`
                : null}
              <label>
                Profile URL
                <input type="text" class="url" readOnly value=${url ? url.href : ""} />
              </label>
              <div class="actions">
                ${url ? html`<a href=${url.href} download role="button">Download</a>` : null}
                ${url && canCopy ? html`<button type="button" onClick=${this.onCopy}>Copy URL</button>` : null}
                ${copied ? html`<small>copied</small>` : null}
              </div>
              <p><small>${copyNote}</small></p>
              ${!ns || !svc ? html`<p><small>choose a namespace and a Service to build the URL</small></p>` : null}
              ${ns && svc && !svcListed ? html`<p><small>the selection is not listed, so no URL is built</small></p>` : null}
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
    const { svc, collections, collection, startMessage } = this.state;
    return html`
      <article>
        <header><strong>Collections</strong></header>
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
                          <td>${text(c.createdAt)}</td>
                          <td>${text(c.finishedAt)}</td>
                          <td>${text(c.expiresAt)}</td>
                          <td class="row-actions">${this.renderCancel(c)}</td>
                        </tr>
                      `,
                      )}
                  </tbody>
                </table>
              </div>
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
