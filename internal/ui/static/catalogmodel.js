// The catalog's rules, kept apart from the page so a test can run them:
// what each menu offers from the one catalog answer,
// and what a filter field leaves standing in the menu beside it.
// This module imports nothing and declares plain functions;
// a Go test evaluates it in an ECMAScript interpreter with the trailing export cut off.

// entryNamespace is an entry's namespace when the entry carries one as a non-empty string,
// and the empty string for an entry of any other shape,
// so a body the gateway never sends narrows a menu rather than putting an unnamed option in it.
function entryNamespace(entry) {
  if (entry === null || typeof entry !== "object") {
    return "";
  }

  return typeof entry.namespace === "string" ? entry.namespace : "";
}

// namespacesOf is the distinct namespaces the catalog names, in the order the catalog arrived.
// That order is already namespace and then name, so equal namespaces stand together
// and compacting the run is what leaves each named once;
// the function sorts nothing and chooses no order of its own.
// It changes nothing it is handed.
function namespacesOf(catalog) {
  const entries = Array.isArray(catalog) ? catalog : [];
  const out = [];
  for (const entry of entries) {
    const ns = entryNamespace(entry);
    if (ns !== "" && ns !== out[out.length - 1]) {
      out.push(ns);
    }
  }

  return out;
}

// servicesOf is the names the catalog holds under ns, in the catalog's order,
// and an empty list for a namespace the catalog does not hold.
// It changes nothing it is handed.
function servicesOf(catalog, ns) {
  const entries = Array.isArray(catalog) ? catalog : [];
  const out = [];
  for (const entry of entries) {
    if (entryNamespace(entry) === ns && typeof entry.name === "string") {
      out.push(entry.name);
    }
  }

  return out;
}

// filterOptions narrows a menu's options to the entries the query matches,
// keeping keep, the value the menu is currently showing, whatever the query is.
// The query is trimmed and matched case-insensitively as a substring,
// so a query of whitespace alone leaves every entry standing.
// It adds no entry the list lacks, holds the list's order, and changes neither argument;
// the result is a new array in every case, so a caller that writes into it writes into nothing else.
function filterOptions(list, query, keep) {
  const entries = Array.isArray(list) ? list : [];
  const text = query === undefined || query === null ? "" : String(query);
  const q = text.trim().toLowerCase();
  if (q === "") {
    return entries.slice();
  }

  return entries.filter((entry) => String(entry).toLowerCase().includes(q) || entry === keep);
}

export { namespacesOf, servicesOf, filterOptions };
