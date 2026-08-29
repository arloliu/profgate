// The Collection controls' rules, kept apart from the page so a test can run them:
// whether each control exists, what each of the two requests carries,
// what an answer to either does to the page, and the armed state that holds one attempt.
// This module imports nothing and declares plain functions;
// a Go test evaluates it in an ECMAScript interpreter with the trailing export cut off.
// It spells no path: app.js hands in the route urls.js built.

// collectionIDRe is the Collection identifier grammar:
// 20 characters of the lowercase Crockford base32 alphabet.
// urls.js spells the same expression to build a path from an identifier;
// this module imports nothing, so it carries its own copy and a test holds the two together.
const collectionIDRe = /^[0-9a-hjkmnp-tv-z]{20}$/;

// digitsRe is a Retry-After value the page reads: a count of seconds and nothing else.
const digitsRe = /^[0-9]+$/;

// retryAfterMax is the longest wait a Retry-After can ask the control to sit out,
// and retryAfterDefault is what an absent or unreadable one reads as.
const retryAfterMax = 300;
const retryAfterDefault = 5;

// initializingRetryMs is how long after a 409 collection_initializing the one retry goes.
const initializingRetryMs = 1000;

// jsonMediaType is what both write routes require, with or without a body.
const jsonMediaType = "application/json";

// cancellableStates are the Collection states a cancel control is offered on.
// Every other value, initializing and the four terminal states included,
// gets no button: an unknown state reads as one that cannot be cancelled.
const cancellableStates = ["pending", "running"];

// attemptPhases are the phases that hold an attempt, which is its key and its route.
// idle and cooling hold neither.
const attemptPhases = ["armed", "inflight", "retained"];

// abandonMessage is what the page says when an attempt no answer ever classified is dropped.
// The POST may already have created a Collection,
// and the Collections table is where that Collection would show.
const abandonMessage = "A Collection may already exist for this Service; the Collections table shows it.";

// text returns value as a string, with null and undefined reading as empty.
function text(value) {
  return value === undefined || value === null ? "" : String(value);
}

// count returns value as a number, with anything unreadable reading as zero.
function count(value) {
  return Number(value) || 0;
}

// nullable returns value, with undefined reading as null,
// so a state field the page left out comes back as one the page can compare.
function nullable(value) {
  return value === undefined ? null : value;
}

// pgoRealm returns the pgo block of a /v1/whoami realm, or an empty one.
function pgoRealm(whoami) {
  return (whoami && whoami.realm && whoami.realm.pgo) || {};
}

// startOffered reports whether the start control exists.
// All four hold or it does not: PGO enabled in /v1/limits,
// both pgo realm flags in /v1/whoami, and a chosen Service.
// read is what shows the Collections table and collect is what adds this control to it,
// so a realm holding one without the other draws no control rather than a disabled one.
function startOffered(limits, whoami, service) {
  const pgo = (limits && limits.pgo) || {};
  const realm = pgoRealm(whoami);
  return pgo.enabled === true && realm.read === true && realm.collect === true && text(service) !== "";
}

// cancelOffered reports whether a Collection row carries a cancel control:
// its state is one a cancel can reach and the realm admits collecting.
function cancelOffered(state, whoami) {
  return pgoRealm(whoami).collect === true && cancellableStates.indexOf(text(state)) >= 0;
}

// uuidFromBytes formats sixteen bytes as a UUIDv4 string:
// the version nibble 4 in the seventh byte and the variant bits 10 in the ninth,
// whatever those bytes held there, and lowercase hex in the 8-4-4-4-12 grouping.
// The module holds no random source; app.js draws the bytes from crypto.getRandomValues
// when the browser defines no crypto.randomUUID.
function uuidFromBytes(bytes) {
  const octets = [];
  for (let i = 0; i < 16; i++) {
    octets.push((bytes ? bytes[i] : 0) & 0xff);
  }
  octets[6] = (octets[6] & 0x0f) | 0x40;
  octets[8] = (octets[8] & 0x3f) | 0x80;
  const hex = [];
  for (let i = 0; i < 16; i++) {
    hex.push(`0${octets[i].toString(16)}`.slice(-2));
  }
  const groups = [[0, 4], [4, 6], [6, 8], [8, 10], [10, 16]];
  const parts = [];
  for (const group of groups) {
    parts.push(hex.slice(group[0], group[1]).join(""));
  }
  return parts.join("-");
}

// startRequest is what the start POST carries: the route it was handed,
// the JSON media type, exactly one Idempotency-Key, and an empty JSON object,
// which means every sampling field comes from the Service's effective policy.
function startRequest(route, key) {
  return {
    method: "POST",
    url: route,
    headers: { "Content-Type": jsonMediaType, "Idempotency-Key": key },
    body: "{}",
  };
}

// cancelRequest is what the cancel POST carries: no body and no idempotency key,
// since a repeat either cancels the same Collection or is answered 409 collection_terminal.
// The media type is declared anyway, because the gateway refuses a write route without it
// and that check reads the header and never the body.
function cancelRequest(route) {
  return { method: "POST", url: route, headers: { "Content-Type": jsonMediaType }, body: null };
}

// retryAfterSeconds reads a Retry-After as a count of seconds and as nothing else.
// A value above the ceiling clamps to it;
// absent, empty, negative, fractional, non-numeric, and an HTTP date each read as the default,
// long enough not to be a retry loop and short enough to be worth waiting through.
function retryAfterSeconds(header) {
  const raw = text(header).trim();
  if (!digitsRe.test(raw)) {
    return retryAfterDefault;
  }
  const seconds = Number(raw);
  return seconds > retryAfterMax ? retryAfterMax : seconds;
}

// errorText is what the page shows for an answer:
// the envelope's message when the body parsed as one, its code when the envelope carried none,
// a plain failure for a fetch that never produced a response, and the status otherwise.
function errorText(answer) {
  const message = text(answer.message);
  if (message !== "") {
    return message;
  }
  const code = text(answer.code);
  if (code !== "") {
    return code;
  }
  if (answer.rejected === true) {
    return "request failed";
  }
  return `HTTP ${count(answer.status)}`;
}

// startAnswer is one arm of startOutcome.
// The control returns to its armed state exactly when the key is kept:
// the key is what an armed control holds,
// and it is kept only for an outcome the page could not classify,
// where the create may already have committed.
function startAnswer(keep, select, refetch, error, disableSeconds) {
  return { keep: keep, armed: keep, select: select, refetch: refetch, error: error, disableSeconds: disableSeconds };
}

// startOutcome maps an answer to a start request onto what the page does next.
// answer is {rejected, status, code, message, id, body, retryAfter}:
// rejected says the fetch never produced a response,
// code and message are the envelope's when the body parsed as one,
// id is the identifier a success carried,
// body is the response text as it arrived, which only the unusable success reads,
// and retryAfter is the header verbatim.
// Three outcomes keep the key for the next press, which is the retry of the same attempt:
// a rejected fetch, a 503 pgo_unavailable, and any other 5xx besides 503 collector_unavailable.
// The whole 2xx range selects, not 202 and 200 alone,
// so a later release answering a replay with another success status still selects the record.
function startOutcome(answer) {
  const a = answer || {};
  const status = count(a.status);
  const code = text(a.code);
  if (a.rejected !== true) {
    if (status >= 200 && status <= 299) {
      const id = text(a.id);
      if (collectionIDRe.test(id)) {
        return startAnswer(false, id, ["collections"], null, 0);
      }
      return startAnswer(false, null, [], text(a.body), 0);
    }
    if (status === 429 && code === "collection_in_progress") {
      return startAnswer(false, null, ["collections"], null, 0);
    }
    if (status === 429 && (code === "rate_limited" || code === "capacity_exhausted")) {
      const seconds = retryAfterSeconds(a.retryAfter);
      return startAnswer(false, null, [], `${errorText(a)}; the control is disabled for ${seconds} seconds`, seconds);
    }
    if (status === 409 && code === "idempotency_mismatch") {
      return startAnswer(false, null, ["collections"], errorText(a), 0);
    }
    if (status === 403 && code === "realm_denied") {
      return startAnswer(false, null, ["whoami"], errorText(a), 0);
    }
    if (status === 501 && code === "pgo_disabled") {
      return startAnswer(false, null, ["limits"], errorText(a), 0);
    }
    if (status === 503 && code === "collector_unavailable") {
      return startAnswer(false, null, [], errorText(a), 0);
    }
  }
  return startAnswer(a.rejected === true || status >= 500, null, [], errorText(a), 0);
}

// cancelOutcome maps an answer to a cancel request onto what the page does next.
// answer carries record, the Collection the gateway returned, where startOutcome reads id and body.
// attempt is which press of this cancel produced the answer, 1 or 2;
// it is named for the retry rather than for the Collection record's own attempt field.
function cancelOutcome(answer, attempt) {
  const a = answer || {};
  const status = count(a.status);
  const code = text(a.code);
  const result = { replace: null, refetch: [], error: null, retryAfterMs: 0 };
  if (a.rejected !== true) {
    if (status === 200) {
      result.replace = nullable(a.record);
      result.refetch = ["collections"];
      return result;
    }
    if (status === 409 && code === "collection_terminal") {
      result.refetch = ["collections"];
      return result;
    }
    if (status === 409 && code === "collection_initializing") {
      if (count(attempt) === 1) {
        result.retryAfterMs = initializingRetryMs;
        return result;
      }
      result.error = code;
      return result;
    }
    if (status === 404 && code === "collection_not_found") {
      // whoami as well: a realm that stopped admitting the record takes the controls with it.
      result.refetch = ["collections", "whoami"];
      return result;
    }
  }
  result.error = errorText(a);
  return result;
}

// nextState builds the start control's state.
// A phase that holds no attempt holds neither key nor route,
// so nothing downstream reads a route as something still pending.
function nextState(phase, key, route, token, until) {
  const holds = attemptPhases.indexOf(phase) >= 0;
  return {
    phase: phase,
    key: holds ? nullable(key) : null,
    route: holds ? nullable(route) : null,
    token: token,
    until: until,
  };
}

// nextStep is what startNext returns: the state and the text the page shows, or null.
function nextStep(state, message) {
  return { state: state, message: message === undefined ? null : message };
}

// startNext is the start control's armed state, and where the attempt's identity lives.
// state is {phase, key, route, token, until},
// with phase one of idle, armed, inflight, retained, and cooling.
// token is the attempt's own number, raised on every arm;
// app.js sends it with the request and hands it back on the outcome.
// An answer to an attempt the page has left is discarded here,
// never selecting a record of a Service nobody is looking at.
// until is when a cooling phase ends, on the millisecond clock the timer event reports.
// event is {kind, ...} with kind one of arm, submit, outcome, timer, keep, and selection:
// arm carries {key, route}, outcome carries {token, keep, disableSeconds, now},
// and timer carries {now}.
// The function is total: a pair with no rule of its own leaves the state alone and says nothing.
// An arm always leaves a cooling control cooling, because this module holds no clock
// and an arm carries no time to compare with until.
function startNext(state, event) {
  const s = state || {};
  const e = event || {};
  const phase = text(s.phase);
  const kind = text(e.kind);
  const token = count(s.token);
  const until = count(s.until);
  const unchanged = nextStep(nextState(phase, s.key, s.route, token, until), null);
  const idle = nextStep(nextState("idle", null, null, token, 0), null);
  // An inflight or retained control has sent a POST that commits before it answers,
  // so abandoning it silently is the loss the key exists to prevent.
  const abandoned = nextStep(nextState("idle", null, null, token, 0), abandonMessage);

  if (phase === "idle") {
    if (kind === "arm") {
      return nextStep(nextState("armed", e.key, e.route, token + 1, 0), null);
    }
    if (kind === "selection") {
      return idle;
    }
    return unchanged;
  }
  if (phase === "armed") {
    if (kind === "submit") {
      return nextStep(nextState("inflight", s.key, s.route, token, 0), null);
    }
    // The ten-second timer disarms a control that was never submitted;
    // Keep before the first request has nothing to warn about, since nothing was sent.
    if (kind === "timer" || kind === "keep" || kind === "selection") {
      return idle;
    }
    return unchanged;
  }
  if (phase === "inflight") {
    if (kind === "outcome") {
      if (count(e.token) !== token) {
        return unchanged;
      }
      if (e.keep === true) {
        return nextStep(nextState("retained", s.key, s.route, token, 0), null);
      }
      const seconds = count(e.disableSeconds);
      if (seconds > 0) {
        return nextStep(nextState("cooling", null, null, token, count(e.now) + seconds * 1000), null);
      }
      return idle;
    }
    if (kind === "selection") {
      return abandoned;
    }
    return unchanged;
  }
  if (phase === "retained") {
    if (kind === "submit") {
      return nextStep(nextState("inflight", s.key, s.route, token, 0), null);
    }
    if (kind === "keep" || kind === "selection") {
      return abandoned;
    }
    return unchanged;
  }
  if (phase === "cooling") {
    if (kind === "timer" && count(e.now) >= until) {
      return idle;
    }
    if (kind === "selection") {
      return idle;
    }
    return unchanged;
  }
  return unchanged;
}

export { startOffered, cancelOffered, uuidFromBytes, startRequest, cancelRequest, startOutcome, cancelOutcome, retryAfterSeconds, startNext };
