package auth

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// discoveryPath is where an OpenID Connect issuer publishes its document.
const discoveryPath = "/.well-known/openid-configuration"

// discoveryDocument is what the gateway keeps from the issuer's discovery
// document: the key set to trust and, with the browser flow, the endpoints
// a browser and a client secret are sent to.
type discoveryDocument struct {
	Issuer                string `json:"issuer"`
	JWKSURI               string `json:"jwks_uri"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	EndSessionEndpoint    string `json:"end_session_endpoint"`
}

// discover fetches <issuer>/.well-known/openid-configuration and validates
// it; browser says whether the two browser endpoints are required.
// The document's issuer must equal the configured one byte for byte, and
// every recorded endpoint must be an absolute https URL with no userinfo
// and no fragment, because the gateway later sends a browser to one and a
// client secret to another.
func discover(ctx context.Context, c *issuerClient, issuer string, browser bool) (discoveryDocument, error) {
	if err := requireHTTPS(issuer); err != nil {
		return discoveryDocument{}, fmt.Errorf("auth: issuer must be https: %w", err)
	}
	var doc discoveryDocument
	if err := c.getJSON(ctx, strings.TrimSuffix(issuer, "/")+discoveryPath, &doc); err != nil {
		return discoveryDocument{}, fmt.Errorf("auth: discovery: %w", err)
	}
	if doc.Issuer != issuer {
		return discoveryDocument{}, fmt.Errorf("auth: discovery: document issuer %q does not equal configured issuer %q",
			doc.Issuer, issuer)
	}
	required := []struct{ name, value string }{{"jwks_uri", doc.JWKSURI}}
	optional := []struct{ name, value string }{}
	if browser {
		required = append(required,
			struct{ name, value string }{"authorization_endpoint", doc.AuthorizationEndpoint},
			struct{ name, value string }{"token_endpoint", doc.TokenEndpoint},
		)
		optional = append(optional, struct{ name, value string }{"end_session_endpoint", doc.EndSessionEndpoint})
	} else {
		// Without the browser flow the other endpoints are never used, so
		// they are neither required nor kept.
		doc.AuthorizationEndpoint, doc.TokenEndpoint, doc.EndSessionEndpoint = "", "", ""
	}
	for _, e := range required {
		if e.value == "" {
			return discoveryDocument{}, fmt.Errorf("auth: discovery: document has no %s", e.name)
		}
		if err := validateEndpoint(e.name, e.value); err != nil {
			return discoveryDocument{}, err
		}
	}
	for _, e := range optional {
		if e.value == "" {
			continue
		}
		if err := validateEndpoint(e.name, e.value); err != nil {
			return discoveryDocument{}, err
		}
	}

	return doc, nil
}

// validateEndpoint holds a discovered endpoint to the same rule as a
// configured URL: absolute https, a host, no userinfo, no fragment.
func validateEndpoint(name, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("auth: discovery: %s %q: %w", name, raw, err)
	}
	switch {
	case u.Scheme != "https":
		return fmt.Errorf("auth: discovery: %s %q: must be an absolute https URL", name, raw)
	case u.Host == "":
		return fmt.Errorf("auth: discovery: %s %q: must name a host", name, raw)
	case u.User != nil:
		return fmt.Errorf("auth: discovery: %s %q: must carry no userinfo", name, raw)
	case u.Fragment != "" || u.RawFragment != "":
		return fmt.Errorf("auth: discovery: %s %q: must carry no fragment", name, raw)
	}

	return nil
}
