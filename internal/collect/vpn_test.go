package collect

import (
	"testing"
	"time"

	"mikrodash/internal/routeros"
)

// ── PHASE 4.1: THE TUNNEL DERIVATION, WITHOUT A COLLECTOR ──────────────────
//
// `buildTunnels` was a method reading three fields and writing a fourth, so none
// of this was reachable without a `*VPN` and a driven tick.

// TestBuildTunnelsReadsEitherCounterSpelling.
//
// RouterOS reports these under two names depending on the build. A tunnel whose
// counters read zero because the other spelling was used looks exactly like an
// idle one — the failure is a busy peer displayed as quiet, on a page whose
// whole job is to say which peers are carrying traffic.
func TestBuildTunnelsReadsEitherCounterSpelling(t *testing.T) {
	for _, tc := range []struct{ name, rxKey, txKey string }{
		{"short form", "rx", "tx"},
		{"suffixed form", "rx-bytes", "tx-bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := routeros.Reply{"public-key": "k1", "name": "peer1"}
			row[tc.rxKey], row[tc.txKey] = "5000", "2500"
			got, _ := BuildTunnels([]routeros.Reply{row}, nil, time.Unix(100, 0))
			if len(got) != 1 {
				t.Fatalf("%d tunnels, want 1", len(got))
			}
			if got[0].RX != 5000 || got[0].TX != 2500 {
				t.Errorf("rx=%d tx=%d; the %s counters were not read, so a busy peer "+
					"renders as idle", got[0].RX, got[0].TX, tc.name)
			}
		})
	}
}

// TestBuildTunnelsDerivesRatesFromThePriorReading, and returns the next state
// rather than writing through its argument.
func TestBuildTunnelsDerivesRatesFromThePriorReading(t *testing.T) {
	t0 := time.Unix(100, 0)
	row := func(rx, tx string) []routeros.Reply {
		return []routeros.Reply{{"public-key": "k1", "name": "p", "rx": rx, "tx": tx}}
	}

	// A FIRST READING HAS NO PRIOR, so its rate is zero rather than a guess.
	first, prev := BuildTunnels(row("1000", "500"), nil, t0)
	if first[0].RXRate != 0 || first[0].TXRate != 0 {
		t.Errorf("first reading produced a rate of %v/%v with nothing to subtract from",
			first[0].RXRate, first[0].TXRate)
	}
	if len(prev) != 1 {
		t.Fatalf("next state holds %d entries, want 1", len(prev))
	}

	// Ten seconds later, 2000 more bytes in: 200 B/s.
	second, _ := BuildTunnels(row("3000", "500"), prev, t0.Add(10*time.Second))
	if second[0].RXRate != 200 {
		t.Errorf("rxRate = %v, want 200", second[0].RXRate)
	}

	// THE INPUT MAP MUST NOT HAVE BEEN WRITTEN. It was, before this extraction:
	// the method took the collector's map and wrote samples into it, so every
	// caller's map was an output parameter and a nil one panicked.
	if got := prev["k1"]; got.rx != 1000 {
		t.Errorf("the prior map was mutated — rx is now %d, want the 1000 it was "+
			"built with", got.rx)
	}
}

// TestBuildTunnelsForgetsAPeerThatHasGone. The dead-key sweep was a delete loop
// over the collector's map; it is now a consequence of building the next state
// from the rows that are actually present.
func TestBuildTunnelsForgetsAPeerThatHasGone(t *testing.T) {
	t0 := time.Unix(100, 0)
	two := []routeros.Reply{
		{"public-key": "k1", "name": "a", "rx": "1", "tx": "1"},
		{"public-key": "k2", "name": "b", "rx": "1", "tx": "1"},
	}
	_, prev := BuildTunnels(two, nil, t0)
	if len(prev) != 2 {
		t.Fatalf("%d tracked, want 2", len(prev))
	}

	_, prev = BuildTunnels(two[:1], prev, t0.Add(time.Second))
	if _, still := prev["k2"]; still {
		t.Error("a peer that is no longer in the table is still tracked; the counter " +
			"state grows without bound over the life of a session")
	}
	if len(prev) != 1 {
		t.Errorf("%d tracked, want 1", len(prev))
	}
}

// TestBuildTunnelsKeysByNameWhenThereIsNoPublicKey.
//
// ── ONE KEYLESS PEER PROVES NOTHING, WHICH A MUTATION SHOWED ───────────────
//
// Written with a single peer, this passed even with the `peerName` fallback
// removed: an empty-string key is still a STABLE key when there is only one of
// them, so the rate came out right for the wrong reason.
//
// Two keyless peers is the case that distinguishes them. Without the fallback
// they collide on "", the second overwrites the first, and both rates are
// derived from the wrong prior reading — a peer's throughput attributed to
// another peer, which is worse than a missing number.
func TestBuildTunnelsKeysByNameWhenThereIsNoPublicKey(t *testing.T) {
	t0 := time.Unix(100, 0)
	at := func(aRx, bRx string) []routeros.Reply {
		return []routeros.Reply{
			{"name": "alpha", "rx": aRx, "tx": "0"},
			{"name": "beta", "rx": bRx, "tx": "0"},
		}
	}

	_, prev := BuildTunnels(at("1000", "1000"), nil, t0)
	if len(prev) != 2 {
		t.Fatalf("%d tracked, want 2 — two peers with no public key collided on one "+
			"key, so one peer's counters stand in for the other's", len(prev))
	}

	// Ten seconds on: alpha moved 2000 bytes, beta moved none.
	got, _ := BuildTunnels(at("3000", "1000"), prev, t0.Add(10*time.Second))
	byName := map[string]float64{}
	for _, tn := range got {
		byName[tn.Name] = tn.RXRate
	}
	if byName["alpha"] != 200 {
		t.Errorf("alpha rxRate = %v, want 200", byName["alpha"])
	}
	if byName["beta"] != 0 {
		t.Errorf("beta rxRate = %v, want 0 — beta moved nothing, so a non-zero rate "+
			"means it was differenced against alpha's reading", byName["beta"])
	}
}
