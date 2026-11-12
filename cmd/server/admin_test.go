package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/CheeseBurger7/llm-usage-meter/internal/auth"
)

const testOrg = "0190a5b2-7c3e-4f1a-9b2d-0123456789ab"

var testKey = "lum_ingest_" + strings.Repeat("ab", 32)

// fakeAdmin records calls and returns canned results.
type fakeAdmin struct {
	orgName   string
	keyOrg    auth.OrgID
	keyScope  auth.Scope
	prefix    string
	revoked   int64
	createErr error
	calls     int
}

func (f *fakeAdmin) CreateOrg(_ context.Context, name string) (auth.Org, error) {
	f.calls++
	f.orgName = name
	id, _ := auth.ParseOrgID(testOrg)
	return auth.Org{ID: id, Name: name}, nil
}

func (f *fakeAdmin) CreateKey(_ context.Context, org auth.OrgID, scope auth.Scope) (auth.NewKey, error) {
	f.calls++
	f.keyOrg, f.keyScope = org, scope
	if f.createErr != nil {
		return auth.NewKey{}, f.createErr
	}
	return auth.NewKey{Plaintext: testKey, Prefix: "abababab", OrgID: org, Scope: scope}, nil
}

func (f *fakeAdmin) RevokeByPrefix(_ context.Context, prefix string) (int64, error) {
	f.calls++
	f.prefix = prefix
	return f.revoked, nil
}

func run(f *fakeAdmin, args ...string) (stdout, stderr string, err error) {
	var out, errOut bytes.Buffer
	err = runAdmin(context.Background(), args, f, &out, &errOut)
	return out.String(), errOut.String(), err
}

func TestOrgsCreate(t *testing.T) {
	f := &fakeAdmin{}
	out, _, err := run(f, "orgs", "create", "--name", "Acme Inc")
	if err != nil {
		t.Fatal(err)
	}
	if f.orgName != "Acme Inc" {
		t.Fatalf("name = %q", f.orgName)
	}
	if out != testOrg+"\n" {
		t.Fatalf("stdout = %q; want only the org id", out)
	}
}

func TestKeysCreate(t *testing.T) {
	f := &fakeAdmin{}
	out, errOut, err := run(f, "keys", "create", "--org", testOrg, "--scope", "ingest")
	if err != nil {
		t.Fatal(err)
	}
	if f.keyOrg.String() != testOrg || f.keyScope != auth.ScopeIngest {
		t.Fatalf("CreateKey(%s, %s)", f.keyOrg, f.keyScope)
	}
	if out != testKey+"\n" {
		t.Fatal("stdout is not exactly the plaintext key")
	}
	if strings.Contains(errOut, testKey) {
		t.Fatal("the key was also written to stderr")
	}
	if !strings.Contains(errOut, "abababab") || !strings.Contains(errOut, "cannot be shown again") {
		t.Fatalf("stderr = %q", errOut)
	}
}

func TestKeysCreateErrors(t *testing.T) {
	for name, args := range map[string][]string{
		"bad org":     {"keys", "create", "--org", "nope", "--scope", "ingest"},
		"missing org": {"keys", "create", "--scope", "ingest"},
		"bad scope":   {"keys", "create", "--org", testOrg, "--scope", "admin"},
		"no scope":    {"keys", "create", "--org", testOrg},
		"extra arg":   {"keys", "create", "--org", testOrg, "--scope", "read", "oops"},
		"bad flag":    {"keys", "create", "--nope"},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeAdmin{}
			out, _, err := run(f, args...)
			if err == nil || f.calls != 0 || out != "" {
				t.Fatalf("err = %v, calls = %d, stdout = %q", err, f.calls, out)
			}
		})
	}

	f := &fakeAdmin{createErr: auth.ErrOrgNotFound}
	if _, _, err := run(f, "keys", "create", "--org", testOrg, "--scope", "read"); !errors.Is(err, auth.ErrOrgNotFound) {
		t.Fatalf("err = %v; want ErrOrgNotFound", err)
	}
}

func TestKeysRevoke(t *testing.T) {
	f := &fakeAdmin{revoked: 1}
	out, errOut, err := run(f, "keys", "revoke", "--prefix", "abababab")
	if err != nil {
		t.Fatal(err)
	}
	if f.prefix != "abababab" || out != "" || !strings.Contains(errOut, "revoked 1") {
		t.Fatalf("prefix = %q, stdout = %q, stderr = %q", f.prefix, out, errOut)
	}

	// Nothing matched: that is an error, not a silent success.
	f = &fakeAdmin{revoked: 0}
	if _, _, err := run(f, "keys", "revoke", "--prefix", "abababab"); err == nil {
		t.Fatal("revoking nothing returned no error")
	}
}

func TestAdminUsage(t *testing.T) {
	for _, args := range [][]string{{"keys"}, {"keys", "list"}, {"orgs", "delete"}, {}} {
		f := &fakeAdmin{}
		if _, _, err := run(f, args...); err == nil || !strings.Contains(err.Error(), "usage:") || f.calls != 0 {
			t.Fatalf("%v: err = %v, calls = %d", args, err, f.calls)
		}
	}
	if !isAdminCommand("orgs") || !isAdminCommand("keys") || isAdminCommand("backfill") {
		t.Fatal("isAdminCommand is wrong")
	}
}
