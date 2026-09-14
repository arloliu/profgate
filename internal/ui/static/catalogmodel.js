// The catalog's rules, kept apart from the page so a test can run them:
// what a filter field leaves standing in the menu beside it.
// This module imports nothing and declares plain functions;
// a Go test evaluates it in an ECMAScript interpreter with the trailing export cut off.

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

export { filterOptions };
