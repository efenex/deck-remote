package main

import (
	"reflect"
	"testing"
)

// TestMapProfilesResponse asserts the pure CLI->PWA mapping: each profile keeps
// name/isDefault, and current/proxyProfile are surfaced as given.
func TestMapProfilesResponse(t *testing.T) {
	cases := []struct {
		name    string
		in      profilesResp
		current string
		proxy   string
		want    map[string]any
	}{
		{
			name: "two profiles",
			in: profilesResp{
				Profiles:       []profileEntry{{Name: "default", IsDefault: true}, {Name: "work", IsDefault: false}},
				DefaultProfile: "default",
			},
			current: "default",
			proxy:   "default",
			want: map[string]any{
				"profiles": []map[string]any{
					{"name": "default", "isDefault": true},
					{"name": "work", "isDefault": false},
				},
				"current":      "default",
				"proxyProfile": "default",
			},
		},
		{
			name:    "empty list still yields a non-nil slice",
			in:      profilesResp{},
			current: "default",
			proxy:   "other",
			want: map[string]any{
				"profiles":     []map[string]any{},
				"current":      "default",
				"proxyProfile": "other",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mapProfilesResponse(c.in, c.current, c.proxy)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("mapProfilesResponse(...) = %#v, want %#v", got, c.want)
			}
		})
	}
}
