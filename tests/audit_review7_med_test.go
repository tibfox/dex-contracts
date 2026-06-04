package tests

// review7 dex MED fixes — runtime tests against the rebuilt dex/router wasm.

import (
	"strings"
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/db/vsc/contracts"

	"github.com/stretchr/testify/assert"
	dexcontracts "github.com/vsc-eco/dex-contracts"
	"github.com/vsc-eco/dex-contracts/contracts/types"
)

// M2: a swap with a negative min_amount_out must be rejected. Pre-fix the
// negative value parsed fine and the slippage Cmp passed vacuously (amountOut >
// negative), so the swap executed with NO slippage protection.
func TestAuditReview7_M2_NegativeMinAmountOutRejected(t *testing.T) {
	const owner = "hive:milo-hpr"
	user := "hive:m2-user"
	dexId := "vsc1Bjn53csDr6wUoYsjXiN9Nhadu458Tw9wvR"
	routerId := "vsc1Bpc3SgDqCRQxzeDrvV7T4XKV6BZuHmME5F"

	ct := test_utils.NewContractTest()
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(dexId, owner, dexcontracts.DexWasm)
	ct.RegisterContract(routerId, owner, dexcontracts.DexRouterV2Wasm)

	router := &RouterInfo{ct: &ct, id: routerId}
	dex := &DexInfo{ct: &ct, id: dexId}

	if r := router.initRouterV2(t, owner); !r.Success {
		t.Fatalf("init router: %s", r.Ret)
	}
	if r := router.registerToken(t, owner, types.RegisterTokenParams{Name: "HIVE", TokenInfo: types.TokenInfo{Chain: "HIVE"}}); !r.Success {
		t.Fatalf("register HIVE: %s", r.Ret)
	}
	if r := router.registerToken(t, owner, types.RegisterTokenParams{Name: "HBD", TokenInfo: types.TokenInfo{Chain: "HIVE"}}); !r.Success {
		t.Fatalf("register HBD: %s", r.Ret)
	}
	if r := router.registerPool(t, owner, types.RegisterPoolParams{Asset0: "hive", Asset1: "hbd", DexContractId: dexId}); !r.Success {
		t.Fatalf("register pool: %s", r.Ret)
	}
	if r := dex.initPool(t, owner, &types.InitParams{Asset0: "hbd", Asset1: "hive", FeeBps: 100, RouterContract: routerId}); !r.Success {
		t.Fatalf("init pool: %s: %s", r.Err, r.ErrMsg)
	}
	if r := dex.addLiquidity(t, owner, 100000_000, 100000_000); !r.Success {
		t.Fatalf("add liquidity: %s: %s", r.Err, r.ErrMsg)
	}

	const swapAmount = "5000000"
	ct.Deposit(user, 20000000, "hbd")

	allow := []contracts.Intent{{Type: "transfer.allow", Args: map[string]string{"token": "hbd", "limit": swapAmount}}}

	// Negative min_amount_out → must be rejected (no funds drawn).
	negMin := "-1"
	r := router.execute(t, user, &types.DexInstruction{
		Type: "swap", Version: "1.0.0",
		AssetIn: "hbd", AssetOut: "hive", AmountIn: swapAmount,
		MinAmountOut: &negMin, Recipient: user,
	}, allow)
	assert.False(t, r.Success, "swap with negative min_amount_out must be rejected (M2)")
	assert.True(t, strings.Contains(r.ErrMsg+r.Ret, "minimum amount out must be non-negative"),
		"expected the M2 guard message, got err=%q ret=%q", r.ErrMsg, r.Ret)

	// Control: a valid (zero) min succeeds, proving the setup is sound and the
	// guard does not reject legitimate swaps.
	zeroMin := "0"
	r2 := router.execute(t, user, &types.DexInstruction{
		Type: "swap", Version: "1.0.0",
		AssetIn: "hbd", AssetOut: "hive", AmountIn: swapAmount,
		MinAmountOut: &zeroMin, Recipient: user,
	}, allow)
	assert.Truef(t, r2.Success, "swap with min_amount_out=0 should succeed: err=%q ret=%q", r2.ErrMsg, r2.Ret)
}
