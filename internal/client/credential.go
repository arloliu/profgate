package client

import (
	"fmt"
	"io"
	"os"
	"time"
)

// CredentialInput is everything the resolver reads: the two token flags, the
// environment seam, the store and issuer for the cached path, the context's
// snapshot, and the command-side prompt for basic.
type CredentialInput struct {
	TokenFile  string
	TokenStdin bool
	Stdin      io.Reader
	User       string // -u
	Getenv     func(string) (string, bool)
	Settings   Settings
	Store      *Store
	Issuer     *Issuer
	Now        func() time.Time
	Prompt     func(user string) (password string, err error) // nil when stdin is not a terminal
}

// ResolveCredential picks the credential for one command in the order the
// design fixes, or returns nil when the command sends none:
// --token-file or --token-stdin, then PROFGATE_TOKEN, then the cached token
// for the context, then, under basic, the user name and password, then nothing.
// A token from the first three is used for this one command and never
// written to the cache.
func ResolveCredential(in CredentialInput) (Credential, error) {
	if in.Getenv == nil {
		in.Getenv = os.LookupEnv
	}
	if in.Now == nil {
		in.Now = time.Now
	}
	if in.TokenFile != "" && in.TokenStdin {
		return nil, fmt.Errorf("%w: --token-file and --token-stdin name two sources for one token", ErrUsage)
	}
	if _, ok := in.Getenv("PROFGATE_PASSWORD"); in.TokenStdin && in.User != "" && !ok {
		return nil, fmt.Errorf("%w: --token-stdin and -u would both read stdin; set PROFGATE_PASSWORD or drop one of them", ErrUsage)
	}
	if in.TokenFile != "" {
		data, err := os.ReadFile(in.TokenFile) //nolint:gosec // the user names the file; reading it is the purpose
		if err != nil {
			return nil, fmt.Errorf("%w: --token-file %s: %w", ErrUsage, in.TokenFile, err)
		}
		return tokenFrom("--token-file "+in.TokenFile, data)
	}
	if in.TokenStdin {
		data, err := io.ReadAll(io.LimitReader(in.Stdin, maxTokenBytes+1))
		if err != nil {
			return nil, fmt.Errorf("%w: --token-stdin: %w", ErrUsage, err)
		}
		return tokenFrom("--token-stdin", data)
	}
	if v, ok := in.Getenv("PROFGATE_TOKEN"); ok {
		return tokenFrom("PROFGATE_TOKEN", []byte(v))
	}
	switch in.Settings.Context.Auth.Mode {
	case "disabled":
		return nil, nil
	case "oidc":
		return cachedCredentialFor(in, true)
	case "basic":
		return basicCredentialFor(in)
	default:
		// No snapshot: --server alone, or a context no login has recorded.
		// An entry the login wrote is used, a named user is a basic pair,
		// and otherwise nothing is sent.
		if cred, err := cachedCredentialFor(in, false); cred != nil || err != nil {
			return cred, err
		}
		if in.User == "" {
			if _, ok := in.Getenv("PROFGATE_USER"); !ok {
				return nil, nil
			}
		}
		return basicCredentialFor(in)
	}
}

// tokenFrom is TokenCredential over what one source held, naming the source
// when it held nothing.
func tokenFrom(source string, data []byte) (Credential, error) {
	if len(data) > maxTokenBytes {
		return nil, fmt.Errorf("%w: %s holds more than %d bytes", ErrUsage, source, maxTokenBytes)
	}
	cred, err := TokenCredential(string(data))
	if err != nil {
		return nil, fmt.Errorf("%w: %s holds no token", ErrUsage, source)
	}
	return cred, nil
}

// cachedCredentialFor is the cached token for the resolved gateway.
// With no entry it returns ErrLoginNeeded before any request when required,
// and nothing otherwise.
func cachedCredentialFor(in CredentialInput, required bool) (Credential, error) {
	_, ok, err := in.Store.Read(in.Settings.CacheName)
	if err != nil {
		return nil, err
	}
	if !ok {
		if !required {
			return nil, nil
		}
		return nil, (&cachedCredential{settings: in.Settings}).loginNeeded()
	}
	return CachedCredential(in.Store, in.Issuer, in.Settings, in.Now), nil
}

// basicCredentialFor is the user name from -u or PROFGATE_USER and the
// password from PROFGATE_PASSWORD or the prompt.
func basicCredentialFor(in CredentialInput) (Credential, error) {
	user := in.User
	if user == "" {
		user, _ = in.Getenv("PROFGATE_USER")
	}
	if user == "" {
		return nil, fmt.Errorf("%w: the gateway authenticates with a user name and password; pass -u <name> or set PROFGATE_USER", ErrUsage)
	}
	password, ok := in.Getenv("PROFGATE_PASSWORD")
	if !ok {
		if in.Prompt == nil {
			return nil, fmt.Errorf("%w: no password for %s: no prompt is available, and PROFGATE_PASSWORD is unset", ErrUsage, user)
		}
		p, err := in.Prompt(user)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrUsage, err)
		}
		password = p
	}
	return BasicCredential(user, password)
}
