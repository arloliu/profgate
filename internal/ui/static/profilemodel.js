// The Profile panel's rules, kept apart from the page so a test can run them:
// which profiles the menu offers, what the duration's bound and starting value are,
// and whether a typed duration is one the gateway accepts.
// This module imports nothing and declares plain functions;
// a Go test evaluates it in an ECMAScript interpreter with the trailing export cut off.

// The upstream defaults for the two duration-bearing profiles.
// The page sends the lower of the default and the configured bound explicitly,
// so a request never rests on an upstream default a configured limit could sit below.
const upstreamSeconds = { cpu: 30, trace: 1 };

// listAllows mirrors the gateway's realm filter (internal/httpapi/realm.go): the wildcard or the name.
function listAllows(list, value) {
  return Array.isArray(list) && (list.includes("*") || list.includes(value));
}

// offeredProfiles is limits.profiles kept to what the realm's profiles admit, in the order /v1/limits gave them.
// It is the page's copy of the gateway's realm filter, run before the page can ask,
// because the menu is drawn before any request is sent.
// It takes the whole /v1/whoami answer and reaches the realm itself,
// because the menu is drawn before either answer has arrived;
// an absent answer, one carrying no realm, and a profiles of another shape each offer nothing.
function offeredProfiles(limits, whoami) {
  const realm = (whoami && whoami.realm) || {};
  const profiles = limits && limits.profiles;
  if (!Array.isArray(profiles)) {
    return [];
  }
  return profiles.filter((p) => listAllows(realm.profiles, p));
}

// secondsLimit is the profile's bound, or 0 for a profile with no duration,
// which is how the page knows to draw no duration input.
// A bound that reads as no number is 0.
function secondsLimit(limits, profile) {
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

// defaultSeconds is the upstream default, or the bound when the bound is lower, as the input's text,
// and "" for a profile with no bound, which leaves the input empty.
function defaultSeconds(limits, profile) {
  const limit = secondsLimit(limits, profile);
  if (!limit) {
    return "";
  }
  return String(Math.min(upstreamSeconds[profile], limit));
}

// secondsValid reports whether the duration is decimal digits spelling a number from 1 to the bound,
// and is true for a profile with no bound, which sends no duration to be wrong.
// It copies the gateway's grammar for seconds (parseSeconds in internal/httpapi/profile.go),
// digits first and range second,
// because the field sends the text that was typed and not the number it reads as:
// 1e1, 0x10, and +10 each read as a whole number, and the gateway refuses each of them.
function secondsValid(limits, profile, seconds) {
  const limit = secondsLimit(limits, profile);
  if (!limit) {
    return true;
  }
  const text = String(seconds);
  if (!/^[0-9]+$/.test(text)) {
    return false;
  }
  const n = Number(text);
  return n >= 1 && n <= limit;
}

export { offeredProfiles, secondsLimit, defaultSeconds, secondsValid };
