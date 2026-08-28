// The port control's rules, kept apart from the page so a test can run them.
// This module imports nothing and declares plain functions; a Go test
// evaluates it in an ECMAScript interpreter with the trailing export cut off.

// selectionsOf returns the allowedSelections list of a pprof block, or an
// empty list when the block or the list is absent.
function selectionsOf(pprof) {
  const list = pprof && pprof.allowedSelections;
  return Array.isArray(list) ? list : [];
}

// sameValue compares a selection's value with the default's within one kind.
function sameValue(a, b) {
  return a !== undefined && a !== null && b !== undefined && b !== null && String(a) === String(b);
}

// deriveControl turns the pprof block of /v1/limits into the control:
// the menu's options and whether each free-form field exists.
// The menu offers default, then every concrete entry in the configured order,
// each as {value, label}.
// An entry equal to the default for its kind is left out, since default already sends it.
// A wildcard is never an option; it is what puts the field of its kind beside the menu.
function deriveControl(pprof) {
  const def = (pprof && pprof.default) || {};
  const defLabel = def.portName ? `default (${def.portName})` : def.port ? `default (${def.port})` : "default";
  const options = [{ value: "default", label: defLabel }];
  let numberField = false;
  let nameField = false;
  for (const s of selectionsOf(pprof)) {
    if (!s) {
      continue;
    }
    if (s.port === "*") {
      numberField = true;
    } else if (s.port !== undefined && s.port !== null) {
      if (!sameValue(s.port, def.port)) {
        options.push({ value: `port:${s.port}`, label: String(s.port) });
      }
    } else if (s.portName === "*") {
      nameField = true;
    } else if (s.portName !== undefined && s.portName !== null && s.portName !== "") {
      if (!sameValue(s.portName, def.portName)) {
        options.push({ value: `name:${s.portName}`, label: String(s.portName) });
      }
    }
  }
  return { options: options, numberField: numberField, nameField: nameField };
}

// paramsOf is the query a state sends: a non-empty free-form field wins over
// the menu, each control sends the parameter of its own kind whatever the
// value holds, and default sends nothing.
function paramsOf(state) {
  if (state.portNumber !== "") {
    return { port: state.portNumber };
  }
  if (state.portName !== "") {
    return { portName: state.portName };
  }
  if (state.portChoice.startsWith("port:")) {
    return { port: state.portChoice.slice(5) };
  }
  if (state.portChoice.startsWith("name:")) {
    return { portName: state.portChoice.slice(5) };
  }
  return {};
}

// applyInput applies one edit — source is "menu", "number", or "name",
// or undefined for no edit — to the control's state and returns
// { state, params }: the next state, and the query it sends,
// which is {}, {port}, or {portName}, never both.
// An edit to one free-form field clears the other.
// The state handed in is not changed.
function applyInput(state, source, value) {
  const s = state || {};
  const next = {
    portChoice: typeof s.portChoice === "string" && s.portChoice !== "" ? s.portChoice : "default",
    portNumber: typeof s.portNumber === "string" ? s.portNumber : "",
    portName: typeof s.portName === "string" ? s.portName : "",
  };
  const v = value === undefined || value === null ? "" : String(value);
  if (source === "menu") {
    next.portChoice = v === "" ? "default" : v;
  } else if (source === "number") {
    next.portNumber = v;
    next.portName = "";
  } else if (source === "name") {
    next.portName = v;
    next.portNumber = "";
  }
  return { state: next, params: paramsOf(next) };
}

export { deriveControl, applyInput };
