package main

// RETURN-FLOOR-1 regression test (2026-11-23).
//
// A swap settled to Ethereum records return_min: the floor a held payout's swap
// back must fill before any relayer may run it. It used to be anchored on the
// trader's input alone, less returnSlipBps (5%), which charges the sender for
// the pool's own fee TWICE — the value has to cross the pool to get out and
// again to come back — and calls the shortfall a skim.
//
// On devnet SWAP-SAFETY S8 (2026-11-23) that made the floor unfillable the
// moment it was written: 2 HBD against 50 HBD of depth cost 7.67% to round-trip
// while the floor allowed 5.00%. Every keeper dry run came back "the swap back
// did not run: reverted: slippage tolerance exceeded", its backoff doubled out
// to 16 minutes, and the sender's refund sat in the bridge's hold — which holds
// the floor FLAT for a day (constants.PayoutFloorFor) before it decays. It
// cleared only when an unrelated swap moved the pool the favourable way.
//
// The fix measures what the pool took and allows for it on top of the 5%. The
// test's job is to keep the floor on the fillable side of a real round trip, so
// it computes that round trip independently (roundTripBack, from the
// constant-product curve and THORChain's slip fee) rather than trusting the
// same arithmetic twice.

import (
	"math/big"
	"testing"

	"github.com/vsc-eco/dex-contracts/contracts/types"
)

// The devnet S8 pool and swap: 50 HBD (3 decimals) against 0.5 ETH, a 2 HBD
// swap, 0.08% base fee. A swap worth 4% of the input-side depth.
const (
	rfHbdReserve = 50_000              // 50 HBD in milli-HBD
	rfEthReserve = 500_000_000_000_000 // 0.5 ETH, scaled down; only ratios matter
	rfAmountIn   = 2_000               // 2 HBD
	rfFeeBps     = 8
)

// The swap's own output, as settleToEVM sees it: a gross amount and the net
// after the bridge's proving fee (0.3% here), so net/amount is the rate.
const (
	rfGross = 1_000_000
	rfNet   = 997_000
)

func rfPool(hbdReserve int64) types.PoolInfo {
	return types.PoolInfo{
		Asset0:   "hbd",
		Asset1:   "eth",
		Reserve0: big.NewInt(hbdReserve).String(),
		Reserve1: big.NewInt(rfEthReserve).String(),
		Fee:      rfFeeBps,
	}
}

func rfMin(t *testing.T, floor returnFloor) int64 {
	t.Helper()
	m := returnMin(big.NewInt(rfAmountIn), big.NewInt(rfNet), big.NewInt(rfGross), floor)
	if !m.IsInt64() {
		t.Fatalf("floor %s does not fit an int64", m)
	}
	return m.Int64()
}

// roundTripBack is what the pool really gives back, worked out independently of
// the code under test: out through the pool and back again, each leg paying the
// constant-product price impact, THORChain's slip fee (SlipFee's grossOut*x/(x+X),
// which is what PendulumSlipParityHeight switches on) and the base fee. The
// second leg runs on the reserves the first leg left behind.
//
// This returned 1850 for the devnet's S8 pool; the devnet itself delivered 1849
// milli-HBD, so the model is right to within a unit.
func roundTripBack(t *testing.T) int64 {
	t.Helper()
	x, X, Y := big.NewInt(rfAmountIn), big.NewInt(rfHbdReserve), big.NewInt(rfEthReserve)

	leg := func(in, resIn, resOut *big.Int) *big.Int {
		gross := new(big.Int).Mul(in, resOut)
		gross.Div(gross, new(big.Int).Add(resIn, in))
		// slip = gross * in/(in+resIn): the input's share of the input-side
		// reserve after the swap IS the proportion the pool takes.
		slip := new(big.Int).Mul(gross, in)
		slip.Div(slip, new(big.Int).Add(in, resIn))
		out := new(big.Int).Sub(gross, slip)
		fee := new(big.Int).Mul(out, big.NewInt(rfFeeBps))
		return out.Sub(out, fee.Div(fee, big.NewInt(10_000)))
	}

	grossOut := new(big.Int).Mul(x, Y)
	grossOut.Div(grossOut, new(big.Int).Add(X, x))
	slipOut := new(big.Int).Mul(grossOut, x)
	slipOut.Div(slipOut, new(big.Int).Add(x, X))
	paidOut := new(big.Int).Sub(grossOut, slipOut) // what left the pool

	out := leg(x, X, Y)                                                    // HBD -> ETH
	back := leg(out, new(big.Int).Sub(Y, paidOut), new(big.Int).Add(X, x)) // ETH -> HBD
	if !back.IsInt64() {
		t.Fatalf("round trip %s does not fit an int64", back)
	}
	return back.Int64()
}

// TestReturnFloor_IsFillableByTheRoundTripItPaysFor is the regression: the old
// floor sat ABOVE what the pool can return, the new one sits below it.
func TestReturnFloor_IsFillableByTheRoundTripItPaysFor(t *testing.T) {
	pool := rfPool(rfHbdReserve)
	gives := roundTripBack(t)

	// What the code did before the fix is still reachable: a zero returnFloor
	// (the value newReturnFloor gives when the pool state does not name the
	// input asset) allows nothing for the round trip.
	before := rfMin(t, returnFloor{})
	if before <= gives {
		t.Fatalf("the pre-fix floor %d is below the round trip %d: this test no longer reproduces S8", before, gives)
	}

	after := rfMin(t, newReturnFloor(pool, "hbd", big.NewInt(rfAmountIn), 2))
	if after > gives {
		t.Fatalf("floor %d is above what the pool returns (%d): the refund would sit in the hold for a day, as it did on devnet S8", after, gives)
	}

	// Fillable is not enough: the floor still has to stop a relayer swapping the
	// return back at any price it likes. Keep it within 15% of the real round
	// trip (it is ~5.7% below at this depth, the returnSlipBps allowance).
	if floorPct := after * 100 / gives; floorPct < 85 {
		t.Fatalf("floor %d is only %d%% of the round trip %d: too low to stop a skim", after, floorPct, gives)
	}
	t.Logf("pre-fix %d (unfillable) -> %d, pool returns %d", before, after, gives)
}

// TestReturnFloor_ExactValues pins the arithmetic, so a change to it has to be
// deliberate. Worked out by hand for the constants above.
func TestReturnFloor_ExactValues(t *testing.T) {
	cases := []struct {
		name  string
		floor returnFloor
		want  int64
	}{
		{"no allowance (the pre-fix floor)", returnFloor{}, 1894},
		{"one pool, two legs", newReturnFloor(rfPool(rfHbdReserve), "hbd", big.NewInt(rfAmountIn), 2), 1745},
		{"two pools, four legs", newReturnFloor(rfPool(rfHbdReserve), "hbd", big.NewInt(rfAmountIn), 4), 1608},
		// 1000x the depth: the same swap barely moves the price, so the floor
		// comes back to the pre-fix value. The allowance is the pool's cost, not
		// a flat discount.
		{"a deep pool allows almost nothing", newReturnFloor(rfPool(50_000_000), "hbd", big.NewInt(rfAmountIn), 2), 1894},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rfMin(t, tc.floor); got != tc.want {
				t.Fatalf("floor %d, want %d", got, tc.want)
			}
		})
	}
}

// TestReturnFloor_FallsWithTheSwapsShareOfDepth is the property the fix added,
// and the one the old code could not have: the floor per unit of input has to
// fall as the swap grows against the pool, because a bigger swap pays a bigger
// slip on the way out AND on the way back. The old floor was independent of
// depth (it even rose slightly with size, as a fixed proving fee dilutes), so
// this fails on it.
func TestReturnFloor_FallsWithTheSwapsShareOfDepth(t *testing.T) {
	pool := rfPool(rfHbdReserve)
	prev := int64(-1)
	for _, in := range []int64{100, 500, 2_000, 5_000} { // 0.2% .. 10% of depth
		m := returnMin(big.NewInt(in), big.NewInt(rfNet), big.NewInt(rfGross),
			newReturnFloor(pool, "hbd", big.NewInt(in), 2))
		// per 100,000 units of input, so sizes compare
		rate := new(big.Int).Mul(m, big.NewInt(100_000))
		rate.Div(rate, big.NewInt(in))
		if prev >= 0 && rate.Int64() >= prev {
			t.Fatalf("input %d: floor rate %d did not fall below %d", in, rate.Int64(), prev)
		}
		t.Logf("input %d (%d%% of depth): floor %s, rate %s", in, in*100/rfHbdReserve, m, rate)
		prev = rate.Int64()
	}
}

// TestReturnFloor_UnknownPoolKeepsTheOldFloor: newReturnFloor allows nothing
// when it cannot find the input asset in the pool state, when the reserve is
// missing or absurd, or when the input is not positive. Allowing nothing is the
// pre-fix behaviour — a floor that may be too high, never one too low.
func TestReturnFloor_UnknownPoolKeepsTheOldFloor(t *testing.T) {
	pool := rfPool(rfHbdReserve)
	cases := []struct {
		name  string
		floor returnFloor
	}{
		{"the input asset is not in the pool", newReturnFloor(pool, "btc", big.NewInt(rfAmountIn), 2)},
		{"no reserves at all", newReturnFloor(types.PoolInfo{Asset0: "hbd", Asset1: "eth"}, "hbd", big.NewInt(rfAmountIn), 2)},
		{"the input is bigger than the reserve", newReturnFloor(rfPool(1_000), "hbd", big.NewInt(rfAmountIn), 2)},
		{"no amount in", newReturnFloor(pool, "hbd", nil, 2)},
		{"zero amount in", newReturnFloor(pool, "hbd", big.NewInt(0), 2)},
		{"no legs", newReturnFloor(pool, "hbd", big.NewInt(rfAmountIn), 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rfMin(t, tc.floor); got != 1894 {
				t.Fatalf("floor %d, want the pre-fix 1894", got)
			}
		})
	}
}

// TestReturnFloor_IsNeverZero: a floor of 0 would let any relayer swap a held
// return back at any price. Even a swap that is almost the whole reserve, over
// four legs, keeps a floor of at least 1.
func TestReturnFloor_IsNeverZero(t *testing.T) {
	m := returnMin(big.NewInt(1), big.NewInt(1), big.NewInt(1_000_000),
		newReturnFloor(rfPool(rfHbdReserve), "hbd", big.NewInt(rfHbdReserve-1), 4))
	if m.Sign() <= 0 {
		t.Fatalf("floor %s must never be 0", m)
	}
}
