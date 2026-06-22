package main

import (
	"context"
	"net/http/httptest"
	"reflect"
	"testing"
)

// TestProfileContext covers the withProfile/profileFrom round-trip, including
// the override/default fallback semantics.
func TestProfileContext(t *testing.T) {
	cases := []struct {
		name       string
		ctxProfile string
		def        string
		want       string
	}{
		{"override wins", "alpha", "default", "alpha"},
		{"empty override falls back to default", "", "default", "default"},
		{"empty default with empty override", "", "", ""},
		{"override with empty default", "beta", "", "beta"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := withProfile(context.Background(), c.ctxProfile)
			if got := profileFrom(ctx, c.def); got != c.want {
				t.Errorf("profileFrom(withProfile(%q), %q) = %q, want %q", c.ctxProfile, c.def, got, c.want)
			}
		})
	}
}

// TestAdeckArgs asserts the argv assembly prepends -p <profile> before the
// subcommand args (the load-bearing ordering for agent-deck's global flag).
func TestAdeckArgs(t *testing.T) {
	cases := []struct {
		name    string
		profile string
		args    []string
		want    []string
	}{
		{"list", "default", []string{"list", "--json"}, []string{"-p", "default", "list", "--json"}},
		{"override profile", "alpha", []string{"session", "output", "id"}, []string{"-p", "alpha", "session", "output", "id"}},
		{"no args", "x", nil, []string{"-p", "x"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := adeckArgs(c.profile, c.args...); !reflect.DeepEqual(got, c.want) {
				t.Errorf("adeckArgs(%q, %v) = %v, want %v", c.profile, c.args, got, c.want)
			}
		})
	}
}

// TestReqProfile covers extraction + trimming of the ?profile= override.
func TestReqProfile(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string
	}{
		{"present", "/api/rc/sessions?profile=foo", "foo"},
		{"missing", "/api/rc/sessions", ""},
		{"empty value", "/api/rc/sessions?profile=", ""},
		{"with spaces", "/api/rc/sessions?profile=%20bar%20", "bar"},
		{"alongside other params", "/api/rc/reply?id=1&profile=baz", "baz"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", c.url, nil)
			if got := reqProfile(r); got != c.want {
				t.Errorf("reqProfile(%q) = %q, want %q", c.url, got, c.want)
			}
		})
	}
}
