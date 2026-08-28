// The targets fetch's rules, kept apart from the page so a test can run them:
// the query it sends, whether a refused fetch is repeated without explain,
// and the summary that becomes the Pod menu, the version menu, and the empty state.
// This module imports nothing and declares plain functions;
// a Go test evaluates it in an ECMAScript interpreter with the trailing export cut off.

// reasonWording is the fixed wording of each exclusion reason the gateway writes.
// The wording is plural whatever the count is, so this module carries no grammar rule.
const reasonWording = {
  "pod_terminating": "Pods being deleted",
  "pod_not_running": "Pods not in phase Running",
  "pod_not_ready": "Pods whose Ready condition is not True",
  "endpoint_missing": "Pods with no trusted EndpointSlice entry naming the current Pod identity",
  "endpoint_not_ready": "Pods whose EndpointSlice entry is not ready",
  "endpoint_address_mismatch": "Pods whose EndpointSlice address is not one the Pod holds",
  "endpoint_address_conflict": "Pods whose EndpointSlice entries disagree on the address",
  "port_name_not_declared": "Pods declaring no TCP container port of the effective pprof port name",
  "version_mismatch": "Pods carrying another version",
  "pod_name_mismatch": "Pods with another name",
};

// targetsQuery is the query a targets fetch sends:
// the port selection as portmodel.js produced it,
// plus explain "true" when withExplain is true and no explain key at all when it is false.
// Never version, never pod.
// The retry is the same call with false,
// which is how it drops only explain and keeps the port selection by construction.
function targetsQuery(portParams, withExplain) {
  const query = {};
  const params = portParams || {};
  for (const key of ["port", "portName"]) {
    if (params[key] !== undefined && params[key] !== null) {
      query[key] = String(params[key]);
    }
  }
  if (withExplain === true) {
    query.explain = "true";
  }
  return query;
}

// retryWithoutExplain decides whether a failed targets fetch is repeated without explain:
// only a fetch that carried explain=true, refused 400 with the envelope code invalid_parameter,
// the answer a gateway gives a parameter it does not know.
// The retry carries no explain, so a second failure is never retried.
function retryWithoutExplain(status, code, sentExplain) {
  return sentExplain === true && status === 400 && code === "invalid_parameter";
}

// targetSummary turns a targets response into { pods, versions, empty }:
// pods is each target's Pod in the order the response listed them,
// versions the distinct non-empty versions in that order,
// and empty is null when targets is non-empty, otherwise
// { kind: "noSelector" } for a selectorMatched of 0,
// { kind: "plain" } for a body with no excluded field or an empty one,
// or { kind: "reasons", rows } with one row per excluded entry in the order the gateway sent,
// an unrecognized reason carrying its own name as its text.
function targetSummary(body) {
  const b = body || {};
  const targets = Array.isArray(b.targets) ? b.targets : [];
  const pods = [];
  const versions = [];
  for (const t of targets) {
    if (!t) {
      continue;
    }
    pods.push(String(t.pod === undefined || t.pod === null ? "" : t.pod));
    const v = t.version === undefined || t.version === null ? "" : String(t.version);
    if (v !== "" && !versions.includes(v)) {
      versions.push(v);
    }
  }
  if (targets.length > 0) {
    return { pods: pods, versions: versions, empty: null };
  }
  if (b.selectorMatched === 0) {
    return { pods: pods, versions: versions, empty: { kind: "noSelector" } };
  }
  const excluded = Array.isArray(b.excluded) ? b.excluded : [];
  if (excluded.length === 0) {
    return { pods: pods, versions: versions, empty: { kind: "plain" } };
  }
  const rows = [];
  for (const e of excluded) {
    if (!e) {
      continue;
    }
    const reason = String(e.reason === undefined || e.reason === null ? "" : e.reason);
    const text = Object.prototype.hasOwnProperty.call(reasonWording, reason) ? reasonWording[reason] : reason;
    rows.push({ reason: reason, count: Number(e.count) || 0, text: text });
  }
  return { pods: pods, versions: versions, empty: { kind: "reasons", rows: rows } };
}

export { targetsQuery, retryWithoutExplain, targetSummary };
