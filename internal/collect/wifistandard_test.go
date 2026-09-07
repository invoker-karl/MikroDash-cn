package collect

import "testing"

// WifiStandard against the documented band vocabulary, not against one router.
//
// Every modern value here is from MikroTik's own enum for
// `/interface/wifi/channel` band, read on 2026-09-07:
//
//	2ghz-g 2ghz-n 2ghz-ax 2ghz-be 5ghz-a 5ghz-ac 5ghz-an 5ghz-ax 5ghz-be
//	6ghz-ax 6ghz-be
//
// The registration table's own `band` is documented only as "string" — "band on
// which particular router is communicating with the AP" — so the enum above is
// the vocabulary it draws from, and a measured sample confirmed the shape:
// `band=5ghz-ac` off a live hAP AX3 that same day.
//
// THE FIXTURE COULD NOT HAVE TOLD US THIS. Every radio in this fleet answers
// with an `ax`-capable modern stack, so `5ghz-an`, the slash lists and every
// `be` value are unreachable from any capture here. They are the cases the
// mapping is most likely to get wrong and the ones no recording can supply.
func TestWifiStandard(t *testing.T) {
	cases := []struct {
		raw, want, why string
	}{
		// ── the modern enum, verbatim ────────────────────────────────────
		{"2ghz-g", "Legacy", "802.11g predates the generation numbering"},
		{"2ghz-n", "Wi-Fi 4", ""},
		{"2ghz-ax", "Wi-Fi 6", ""},
		{"2ghz-be", "Wi-Fi 7", ""},
		{"5ghz-a", "Legacy", "802.11a predates the numbering"},
		{"5ghz-ac", "Wi-Fi 5", "measured on a live hAP AX3"},
		{"5ghz-an", "Wi-Fi 4", "ONE token meaning a/n, not a suffix 'n'"},
		{"5ghz-ax", "Wi-Fi 6", ""},
		{"5ghz-be", "Wi-Fi 7", ""},
		{"6ghz-ax", "Wi-Fi 6E", "6E IS ax on 6 GHz — the band is half the answer"},
		{"6ghz-be", "Wi-Fi 7", "there is no 7E; 6 GHz is ordinary for Wi-Fi 7"},

		// ── the legacy stack's slash lists ───────────────────────────────
		//
		// THE HIGHEST TOKEN WINS. Reporting the lowest would label a Wi-Fi 5
		// client as Wi-Fi 4 on every legacy AP, which is the failure that makes
		// the column worse than absent.
		{"2ghz-b/g/n", "Wi-Fi 4", "highest of b, g, n"},
		{"5ghz-a/n/ac", "Wi-Fi 5", "highest of a, n, ac"},
		{"2ghz-b/g", "Legacy", "nothing here is a numbered generation"},
		{"2ghz-onlyn", "Wi-Fi 4", "the only- prefix is stripped"},
		{"5ghz-onlyac", "Wi-Fi 5", ""},

		// ── and the cases that must produce nothing ──────────────────────
		//
		// A CAPsMAN row carries no band at all, which is why wlBandOf falls back
		// to the interface name. The pill has to render that as absence rather
		// than as a guess: this column is read as a fact about the client.
		{"", "", "a CAPsMAN row has no band"},
		{"5ghz", "", "a band with no generation part says nothing"},
		{"5ghz-zz", "", "an unknown token is not a generation"},
		{"nonsense", "", ""},
	}
	for _, c := range cases {
		if got := WifiStandard(c.raw); got != c.want {
			msg := "WifiStandard(%q) = %q, want %q"
			if c.why != "" {
				msg += " — " + c.why
			}
			t.Errorf(msg, c.raw, got, c.want)
		}
	}
}

// Case and whitespace are not the router's promise to keep.
func TestWifiStandardIsCaseInsensitive(t *testing.T) {
	for _, raw := range []string{"5GHZ-AC", " 5ghz-ac ", "5Ghz-Ac"} {
		if got := WifiStandard(raw); got != "Wi-Fi 5" {
			t.Errorf("WifiStandard(%q) = %q, want \"Wi-Fi 5\"", raw, got)
		}
	}
}
