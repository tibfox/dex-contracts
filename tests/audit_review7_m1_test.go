package tests

import (
	"testing"

	"vsc-node/modules/db/vsc/contracts"
	ledger_db "vsc-node/modules/db/vsc/ledger"
	state_engine "vsc-node/modules/state-processing"

	"github.com/CosmWasm/tinyjson"
	"github.com/stretchr/testify/assert"
	"github.com/vsc-eco/dex-contracts/contracts/types"
)

// review7 M1 / DX-H1 — systemFee insolvency (claim_fees vs reserves).
//
// In the deployed (audited) contract a swap credited the system fee (magiFee)
// to BOTH the reserve AND the f0/f1 fee accumulator, so claim_fees paid the fee
// out of pool holdings while the reserve still counted it — the last LP could
// not be fully paid (insolvency).
//
// The current source keeps magiFee OUT of the reserve (newROut = rOut - grossOut
// + lpFee; only lpFee is added back), so holdings == reserve + accrued fees and
// claim_fees draws only the accrued fees. This test stresses that invariant: an
// external user swaps to accrue fees, the owner claims them, and then the sole
// LP withdraws 100% of liquidity — which can only succeed if every reserve is
// still fully backed by pool holdings after the claim.
//
// It PASSES on the current (solvent) source; reintroduce the deployed bug
// (credit magiFee to the reserve) and the final remove_liquidity fails for lack
// of pool holdings.
func TestAuditReview7_M1_ClaimFeesSolvency(t *testing.T) {
	ct, _, dexId := setupNativeHiveHbdPool(t, 1000000, 1000000)
	owner := "hive:milo-hpr"
	user := "hive:m1-user"

	ct.Deposit(user, 200000, ledger_db.Asset("hive"))
	ct.Deposit(user, 200000, ledger_db.Asset("hbd"))

	swap := func(assetIn, assetOut string) {
		t.Helper()
		payload, _ := tinyjson.Marshal(types.SwapParams{
			AssetIn: assetIn, AmountIn: "20000", AssetOut: assetOut, From: user, To: user,
		})
		r := ct.Call(state_engine.TxVscCallContract{
			Self:       *basicSelf(t, user),
			ContractId: dexId,
			Action:     "swap",
			Payload:    payload,
			RcLimit:    1000,
			Intents: []contracts.Intent{
				{Type: "transfer.allow", Args: map[string]string{"token": assetIn, "limit": "20000"}},
			},
			Caller: user,
		})
		if !r.Success {
			t.Fatalf("swap %s->%s failed: %s: %s", assetIn, assetOut, r.Err, r.ErrMsg)
		}
	}
	// Swap both directions so fees accrue in BOTH f0 and f1.
	for i := 0; i < 4; i++ {
		swap("hive", "hbd")
		swap("hbd", "hive")
	}

	// Owner claims accrued system fees out of pool holdings.
	rc := ct.Call(state_engine.TxVscCallContract{
		Self: *basicSelf(t, owner), ContractId: dexId, Action: "claim_fees",
		Payload: []byte("{}"), RcLimit: 1000, Intents: []contracts.Intent{}, Caller: owner,
	})
	assert.Truef(t, rc.Success, "claim_fees should succeed: %s", rc.ErrMsg)

	// The sole LP (owner) removes 100% of liquidity. The first deposit of
	// (1_000_000, 1_000_000) minted sqrt(1e12) = 1_000_000 LP to the owner.
	// This only succeeds if both reserves remain fully backed after the claim.
	removePayload, _ := tinyjson.Marshal(types.RemoveLiquidityParams{
		LpAmount: "1000000", Recipient: owner,
	})
	rr := ct.Call(state_engine.TxVscCallContract{
		Self: *basicSelf(t, owner), ContractId: dexId, Action: "remove_liquidity",
		Payload: removePayload, RcLimit: 1000, Intents: []contracts.Intent{}, Caller: owner,
	})
	assert.Truef(t, rr.Success,
		"remove 100%% liquidity after claim_fees must succeed (pool solvent); insolvency => transfer underflow. err=%s ret=%s", rr.ErrMsg, rr.Ret)
}
