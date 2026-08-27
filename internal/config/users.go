package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// BasicUser is one entry of the basic-mode user set.
// The same shape appears inline under auth.basic.users and in the file
// auth.basic.usersFile names, so hashes can live in a Secret volume while the
// rest of the configuration stays in a ConfigMap.
type BasicUser struct {
	Name         string `yaml:"name"`
	PasswordHash string `yaml:"passwordHash"`
	Realm        string `yaml:"realm"`
}

// bcryptHash is the hash grammar internal/config checks.
// This package cannot import x/crypto, which is the sole importer rule the
// repository checks, so the grammar stands in for bcrypt.Cost: it matches
// exactly the strings that function accepts, with the two cost digits
// captured. Refusing everything else is what turns a plaintext password
// written into configuration into a startup failure.
var bcryptHash = regexp.MustCompile(`^\$2[aby]\$(\d\d)\$[./A-Za-z0-9]{53}$`)

// The bounds on one user entry.
const (
	// minBcryptCost and maxBcryptCost bound the CPU an operator can make one
	// password comparison cost.
	minBcryptCost = 10
	maxBcryptCost = 14
	// maxUserNameBytes bounds the name a request may send in the Basic header.
	maxUserNameBytes = 256
)

// ParseUsers decodes YAML holding a single users list, rejecting unknown keys.
// The users-file poller in internal/auth calls it on the bytes it read, so a
// file that arrives as bytes and one that arrives as a path are judged alike.
// An empty document is a file with no users rather than a parse failure.
// The file holds one document: a second one after a `---` separator is an
// error, because users written there would otherwise be silently dropped.
func ParseUsers(b []byte) ([]BasicUser, error) {
	var doc struct {
		Users []BasicUser `yaml:"users"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}

		return nil, err
	}
	var extra yaml.Node
	switch err := dec.Decode(&extra); {
	case err == nil:
		return nil, errors.New("users file holds more than one YAML document; only the first would be read")
	case !errors.Is(err, io.EOF):
		return nil, err
	}

	return doc.Users, nil
}

// LoadUsersFile reads path and hands the bytes to ParseUsers.
func LoadUsersFile(path string) ([]BasicUser, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the operator names the file; reading it is the purpose
	if err != nil {
		return nil, err
	}

	return ParseUsers(b)
}

// ValidateBasicUsers checks the inline and file users as one set and returns
// the bcrypt cost they share.
// One cost across the whole set is what lets an unknown name be compared
// against a dummy hash that costs exactly what a real comparison costs, so the
// unknown-user path and the wrong-password path do the same work.
// Names are unique across both halves, and every realm named must exist.
func ValidateBasicUsers(inline, file []BasicUser, realms map[string]Realm) (int, error) {
	if len(inline)+len(file) == 0 {
		return 0, errors.New("auth.basic: at least one user is required in basic mode")
	}

	var cost int
	var costKey, costName string
	seen := make(map[string]struct{}, len(inline)+len(file))
	for _, half := range []struct {
		key   string
		users []BasicUser
	}{
		{"auth.basic.users", inline},
		{"auth.basic.usersFile users", file},
	} {
		for i, user := range half.users {
			key := fmt.Sprintf("%s[%d]", half.key, i)
			if err := validateUserName(key, user.Name, seen); err != nil {
				return 0, err
			}
			seen[user.Name] = struct{}{}

			userCost, err := hashCost(user.PasswordHash)
			if err != nil {
				return 0, fmt.Errorf("%s.passwordHash: user %q: %w", key, user.Name, err)
			}
			if cost == 0 {
				cost, costKey, costName = userCost, key, user.Name
			} else if userCost != cost {
				return 0, fmt.Errorf(
					"%s.passwordHash: user %q at cost %d differs from user %q at cost %d in %s.passwordHash; every bcrypt hash in the user set must share one cost",
					key, user.Name, userCost, costName, cost, costKey)
			}

			if user.Realm == "" {
				return 0, fmt.Errorf("%s.realm is required", key)
			}
			if _, ok := realms[user.Realm]; !ok {
				return 0, fmt.Errorf("%s.realm %q is not a realm", key, user.Realm)
			}
		}
	}

	return cost, nil
}

// validateUserName holds a name to what RFC 7617 can carry: a colon separates
// the name from the password in the header, so a name cannot contain one.
func validateUserName(key, name string, seen map[string]struct{}) error {
	if n := len(name); n < 1 || n > maxUserNameBytes {
		return fmt.Errorf("%s.name: 1 to %d bytes, found %d", key, maxUserNameBytes, n)
	}
	if strings.Contains(name, ":") {
		return fmt.Errorf("%s.name %q: must contain no colon", key, name)
	}
	if _, dup := seen[name]; dup {
		return fmt.Errorf("%s.name: duplicate user %q", key, name)
	}

	return nil
}

// hashCost returns the cost of a bcrypt hash and rejects anything else.
// The error never carries the value, because a value that is not a hash is
// most often the password itself.
func hashCost(hash string) (int, error) {
	match := bcryptHash.FindStringSubmatch(hash)
	if match == nil {
		return 0, errors.New("not a bcrypt hash; profgate auth hash prints one")
	}
	// The grammar already matched two digits, so this cannot fail.
	cost, _ := strconv.Atoi(match[1])
	if cost < minBcryptCost || cost > maxBcryptCost {
		return 0, fmt.Errorf("cost %d is not between %d and %d", cost, minBcryptCost, maxBcryptCost)
	}

	return cost, nil
}
