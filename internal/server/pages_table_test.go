package server

import (
	"testing"

	"mikrodash/internal/pages"
)

// ── THE ROLES CATALOGUE MUST NAME THE PAGES THAT EXIST ─────────────────────
//
// `internal/server/pages_table.json` is what `GET /api/roles` sends as the list
// of grantable pages, and what the nav-prefs allow-list is built from. It was
// generated from the Node `src/pages.js` by a tool that now skips, so it is a
// frozen artefact nothing was comparing against anything.
//
// NOTE THAT IT IS A DIFFERENT FILE FROM `testdata/pages-table.json`, which
// `internal/verify/pagekeys_test.go` does check. Two files one hyphen apart,
// one guarded and one not — which is most of why this drifted unnoticed.
//
// It had drifted in BOTH directions after the 2026-09-01 rename, and each
// direction is its own bug:
//
//   - FIVE DEAD KEYS — `topology`, `wifi`, `wireless`, `rosusers`, `audit` —
//     offered as grantable. Granting one wrote a `role_pages` row naming a page
//     that does not exist, which `rbac.CanPage` then refuses. The operator sees
//     a permission they granted having no effect, with nothing to say why.
//   - FIVE LIVE PAGES MISSING — `network-topology`, `wifi-networks`,
//     `wifi-clients`, `users`, `audit-trail` — which therefore COULD NOT BE
//     GRANTED AT ALL from the roles editor. That half is the worse one: no
//     amount of clicking gets a role access to those pages.
//
// Both directions are asserted, because a catalogue that is merely a subset
// fails silently in exactly the way the missing half did.
//
// SETTINGS KEYS ARE A DIFFERENT NAMESPACE AND DID NOT MOVE. `pageWifi` is still
// `pageWifi` on the page whose key is now `wifi-networks`; only the page key was
// renamed. That is why this checks `key` and leaves `settingsKey` alone.
func TestTheRolesCatalogueNamesRealPages(t *testing.T) {
	live := map[string]bool{}
	for _, k := range pages.Keys() {
		live[k] = true
	}
	if len(live) < 20 {
		t.Fatalf("only %d live page keys — the source moved and this check "+
			"would pass anything", len(live))
	}
	if len(pageCatalogue) < 20 {
		t.Fatalf("the catalogue holds %d entries — it did not load, and an "+
			"empty one satisfies every assertion below", len(pageCatalogue))
	}

	seen := map[string]bool{}
	for _, p := range pageCatalogue {
		seen[p.Key] = true
		if live[p.Key] {
			continue
		}
		msg := ""
		if to, renamed := pages.Renamed[p.Key]; renamed {
			msg = " — renamed to " + to
		}
		t.Errorf("pages_table.json offers %q as a grantable page%s, but no such "+
			"page exists. A role granted it holds a permission that confers "+
			"nothing.", p.Key, msg)
	}

	for _, k := range pages.Keys() {
		if !seen[k] {
			t.Errorf("page %q is missing from pages_table.json, so no role can "+
				"be granted it from the roles editor at all", k)
		}
	}
}

// And the titles agree with the page list, because two sources of a name is two
// names. This is a weaker property than the keys above — a wrong title is
// cosmetic where a wrong key is a denied permission — but it is free to check
// and it is how the drift above would have been noticed sooner.
func TestTheRolesCatalogueTitlesMatchThePageList(t *testing.T) {
	title := map[string]string{}
	for _, p := range pages.All {
		title[p.Key] = p.Title
	}
	for _, p := range pageCatalogue {
		want, ok := title[p.Key]
		if !ok {
			continue // the key test above already reports this
		}
		if p.Title != want {
			t.Errorf("pages_table.json calls %q %q; internal/pages calls it %q",
				p.Key, p.Title, want)
		}
	}
}
