// The catalog's rules, kept apart from the page so a test can run them:
// what each menu offers from the one catalog answer,
// what a filter field leaves standing in the menu beside it,
// and what the search field finds across the whole catalog.
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

// filterOptions is {options, matched}: what a menu draws under its filter's query,
// and how many entries of the list that query found.
// options narrows to the entries the query matches, keeping keep, the value the menu is showing,
// whatever the query is; matched counts only entries the query actually found,
// so a kept value the query missed is drawn without being counted
// and the line over the two numbers never reports a match nobody made.
// The query is trimmed and matched case-insensitively as a substring,
// so a query of whitespace alone leaves every entry standing.
// It adds no entry the list lacks, holds the list's order, and changes neither argument;
// options is a new array in every case, so a caller that writes into it writes into nothing else.
function filterOptions(list, query, keep) {
  const entries = Array.isArray(list) ? list : [];
  const q = normalizeQuery(query);
  if (q === "") {
    return { options: entries.slice(), matched: entries.length };
  }
  const matches = entries.filter((entry) => String(entry).toLowerCase().includes(q));
  // The value the menu is showing is offered whether or not it matched,
  // because a menu that dropped its own value would show a selection its options do not hold.
  // It is counted as a match only when it is one,
  // so the count says how many entries the query found and never how many rows were drawn.
  const keptOnly = keep !== undefined && keep !== null && keep !== "" && entries.includes(keep) && !matches.includes(keep);

  return {
    options: keptOnly ? entries.filter((entry) => matches.includes(entry) || entry === keep) : matches,
    matched: matches.length,
  };
}

// normalizeQuery is a query as both matching functions test for it:
// trimmed of the space around it and lowercased,
// so a match ignores case and a query of space alone is the empty query.
// The two read a query through this one function,
// because a person who learned one field would otherwise guess wrong at the other.
// What an empty query then means is each function's own.
function normalizeQuery(query) {
  const text = query === undefined || query === null ? "" : String(query);

  return text.trim().toLowerCase();
}

// searchCatalog is the first limit entries whose namespace and name, joined by a slash,
// hold the query somewhere within them, in the catalog's order,
// beside the number of entries that matched before that cap was applied.
// The cap bounds the rows the page draws and not what the page knows,
// so it can say how many more matched than it shows.
// The two halves are matched as the one joined string,
// which is what lets a single query narrow by both, as "payments/check" does.
// Each match is the catalog's entry whole,
// because the row the page draws names the two halves separately.
// An empty query matches nothing, so results appear only once someone has typed;
// this is the one place the two matching functions part,
// as a filter leaves every option standing for that same query.
// An entry naming no namespace or no name is passed over rather than matched,
// and a limit of zero or less draws no row while the total still counts what matched,
// each narrowing the answer the way this module narrows every input it cannot read.
// It changes nothing it is handed.
function searchCatalog(catalog, query, limit) {
  const entries = Array.isArray(catalog) ? catalog : [];
  const q = normalizeQuery(query);
  if (q === "") {
    return { matches: [], total: 0 };
  }

  const matches = [];
  let total = 0;
  for (const entry of entries) {
    const ns = entryNamespace(entry);
    if (ns === "" || typeof entry.name !== "string" || entry.name === "") {
      continue;
    }
    if (!(ns + "/" + entry.name).toLowerCase().includes(q)) {
      continue;
    }

    total += 1;
    if (matches.length < limit) {
      matches.push(entry);
    }
  }

  return { matches, total };
}

export { namespacesOf, servicesOf, filterOptions, searchCatalog };
