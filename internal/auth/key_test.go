package auth

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"regexp"
	"strings"
	"testing"
)

func TestGenerateKey(t *testing.T) {
	shape := regexp.MustCompile(`^lum_(ingest|read)_[0-9a-f]{64}$`)
	seen := map[string]bool{}
	for _, scope := range []Scope{ScopeIngest, ScopeRead} {
		for range 50 {
			key, err := generateKey(scope)
			if err != nil {
				t.Fatal(err)
			}
			if !shape.MatchString(key) {
				t.Fatalf("key has the wrong shape (length %d)", len(key))
			}
			if seen[key] {
				t.Fatal("generated the same key twice")
			}
			seen[key] = true

			gotScope, prefix, err := parseKey(key)
			if err != nil || gotScope != scope {
				t.Fatalf("parseKey = %q, %v; want scope %q", gotScope, err, scope)
			}
			if want := strings.Split(key, "_")[2][:PrefixLen]; prefix != want {
				t.Fatalf("prefix = %q; want %q", prefix, want)
			}
		}
	}
}

func TestParseKeyRejects(t *testing.T) {
	secret := strings.Repeat("ab", 32)
	cases := map[string]string{
		"empty":             "",
		"no separators":     "lumingest" + secret,
		"wrong brand":       "xyz_ingest_" + secret,
		"unknown scope":     "lum_admin_" + secret,
		"empty scope":       "lum__" + secret,
		"short secret":      "lum_ingest_" + secret[:63],
		"long secret":       "lum_ingest_" + secret + "a",
		"uppercase hex":     "lum_ingest_" + strings.ToUpper(secret),
		"non-hex secret":    "lum_ingest_" + strings.Repeat("zz", 32),
		"extra part":        "lum_ingest_" + secret + "_x",
		"leading space":     " lum_ingest_" + secret,
		"trailing newline":  "lum_ingest_" + secret + "\n",
		"bearer left on":    "Bearer lum_ingest_" + secret,
		"multibyte in tail": "lum_ingest_" + secret[:62] + "é",
	}
	for name, key := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := parseKey(key); !errors.Is(err, ErrInvalidKey) {
				t.Fatalf("err = %v; want ErrInvalidKey", err)
			}
		})
	}
}

func TestHashKey(t *testing.T) {
	key := "lum_read_" + strings.Repeat("0", 64)
	want := sha256.Sum256([]byte(key))
	if got := hashKey(key); !bytes.Equal(got, want[:]) {
		t.Fatalf("hashKey is not SHA-256 of the key")
	}
}

func TestParseScope(t *testing.T) {
	for _, s := range []string{"ingest", "read"} {
		if got, err := ParseScope(s); err != nil || string(got) != s {
			t.Fatalf("ParseScope(%q) = %q, %v", s, got, err)
		}
	}
	for _, s := range []string{"", "admin", "Ingest", "read "} {
		if _, err := ParseScope(s); err == nil {
			t.Fatalf("ParseScope(%q) succeeded", s)
		}
	}
}

func TestOrgIDRoundTrip(t *testing.T) {
	const s = "0190a5b2-7c3e-4f1a-9b2d-0123456789ab"
	id, err := ParseOrgID(s)
	if err != nil {
		t.Fatal(err)
	}
	if id.String() != s {
		t.Fatalf("String = %q; want %q", id.String(), s)
	}
	for _, bad := range []string{"", "not-a-uuid", "0190a5b2"} {
		if _, err := ParseOrgID(bad); err == nil {
			t.Fatalf("ParseOrgID(%q) succeeded", bad)
		}
	}
}
