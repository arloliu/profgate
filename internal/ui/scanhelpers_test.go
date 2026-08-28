package ui

import (
	"regexp"
	"strings"
)

// The scan helpers below are shared by the vendored-file checks and the
// console source scan; each returns the findings so a caller can report
// them, and a fixture that must fail proves the helper matches something.

// staticImportRe matches an ES module static import's quoted specifier,
// covering "import ... from '<spec>'" and the side-effect form
// "import '<spec>'".
var staticImportRe = regexp.MustCompile(`import\s+(?:[\w*{}\s,]+\s+from\s+)?['"]([^'"]+)['"]`)

// dynamicImportRe matches a dynamic import(...) call's quoted argument.
var dynamicImportRe = regexp.MustCompile(`import\s*\(\s*['"]([^'"]+)['"]`)

// nonRelativeImports returns every import specifier in content, static or
// dynamic, that does not start with "./" or "../".
func nonRelativeImports(content string) []string {
	var found []string
	for _, m := range staticImportRe.FindAllStringSubmatch(content, -1) {
		if spec := m[1]; !strings.HasPrefix(spec, "./") && !strings.HasPrefix(spec, "../") {
			found = append(found, spec)
		}
	}
	for _, m := range dynamicImportRe.FindAllStringSubmatch(content, -1) {
		if spec := m[1]; !strings.HasPrefix(spec, "./") && !strings.HasPrefix(spec, "../") {
			found = append(found, spec)
		}
	}

	return found
}

// anyImports returns every import specifier in content, static or dynamic,
// relative or not.
func anyImports(content string) []string {
	var found []string
	for _, m := range staticImportRe.FindAllStringSubmatch(content, -1) {
		found = append(found, m[1])
	}
	for _, m := range dynamicImportRe.FindAllStringSubmatch(content, -1) {
		found = append(found, m[1])
	}

	return found
}

var (
	scriptTagRe   = regexp.MustCompile(`<script`)
	styleTagRe    = regexp.MustCompile(`<style`)
	styleAttrRe   = regexp.MustCompile(` style=`)
	onAttrRe      = regexp.MustCompile(`\bon[a-z]+=`)
	evalCallRe    = regexp.MustCompile(`eval\(`)
	newFunctionRe = regexp.MustCompile(`new Function\(`)
)

// inlineFormFindings returns every inline-form violation content holds: an
// inline <script> or <style>, a style= attribute, or an on*= handler
// attribute.
// When isJS is true it also looks for eval( and new Function(.
func inlineFormFindings(content string, isJS bool) []string {
	var found []string
	if scriptTagRe.MatchString(content) {
		found = append(found, "<script")
	}
	if styleTagRe.MatchString(content) {
		found = append(found, "<style")
	}
	if styleAttrRe.MatchString(content) {
		found = append(found, " style=")
	}
	if onAttrRe.MatchString(content) {
		found = append(found, "on*=")
	}
	if isJS {
		if evalCallRe.MatchString(content) {
			found = append(found, "eval(")
		}
		if newFunctionRe.MatchString(content) {
			found = append(found, "new Function(")
		}
	}

	return found
}
