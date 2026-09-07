package server

// The Devices page is blank for two unrelated reasons and they look identical.
//
// `BuildStats` drops every row when the RBAC set is empty-but-not-nil
// (internal/routers/assemble.go: NIL means unrestricted, EMPTY means this
// principal may read nothing), so a viewer with a page grant and no
// router-scoped grant sees exactly what an install with no routers shows. Issue
// #129 arrived as "the page is empty" with no way to tell which.
//
// The log line that distinguishes them is only useful if it stays quiet on a
// healthy install, so the DECISION is pinned here rather than the logging. A
// condition written the wrong way round would put a line in every operator's log
// on every open of a working Devices page, which is worse than the silence it
// replaced.

import "testing"

func TestScopeHidesWholeFleet(t *testing.T) {
	two := map[string]bool{"r1": true, "r2": true}
	none := map[string]bool{}

	cases := []struct {
		name    string
		fleet   int
		visible map[string]bool
		want    bool
		why     string
	}{
		{"unrestricted principal", 4, nil, false,
			"nil means NO RESTRICTION; reporting it would log on every healthy open"},
		{"ordinary principal", 4, two, false,
			"they can read some of the fleet, so the page will not be blank"},
		{"scoped out of everything", 4, none, true,
			"THE CASE THIS EXISTS FOR: routers are configured and this viewer may read none"},
		{"genuinely no routers", 0, none, false,
			"the page's own \"No routers configured\" is already true; nothing to explain"},
		{"no routers, unrestricted", 0, nil, false,
			"a fresh install before the first router is added"},
		{"one router, scoped out", 1, none, true,
			"the smallest fleet that can still be hidden"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scopeHidesWholeFleet(tc.fleet, tc.visible); got != tc.want {
				t.Errorf("got %t, want %t — %s", got, tc.want, tc.why)
			}
		})
	}
}
