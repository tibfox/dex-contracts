package tests

// A swap paid from a Hive account with return_address {chain: "HIVE"} either
// completes or its input goes back to that account on Hive (a withdrawal from
// Magi), never staying in the Magi balance. THORChain's refundTx: a swap that
// does not complete goes back to its sender on its own chain.

import (
	"strconv"
	"strings"
	"testing"

	"github.com/CosmWasm/tinyjson"

	"github.com/stretchr/testify/require"
	dexcontracts "github.com/vsc-eco/dex-contracts"
	"github.com/vsc-eco/dex-contracts/contracts/types"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/db/vsc/contracts"
	ledger_db "vsc-node/modules/db/vsc/ledger"
	state_engine "vsc-node/modules/state-processing"
)

const (
	hrOwner  = "hive:milo-hpr"
	hrTrader = "hive:hr-trader"
	hrDex    = "vsc1Bjn53csDr6wUoYsjXiN9Nhadu458Tw9wvR"
	hrRouter = "vsc1Bpc3SgDqCRQxzeDrvV7T4XKV6BZuHmME5F"
)

// setupHiveReturn is a HIVE/HBD pool (100,000 each) behind the router, and a
// trader holding 10,000 HIVE plus HBD for RC.
func setupHiveReturn(t *testing.T) (*test_utils.ContractTest, *RouterInfo) {
	t.Helper()
	requireWasm(t, "dex-router-v2", dexcontracts.DexRouterV2Wasm)
	requireWasm(t, "dex", dexcontracts.DexWasm)
	ct := test_utils.NewContractTest()
	whitelistPendulum(&ct)
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(hrDex, hrOwner, dexcontracts.DexWasm)
	ct.RegisterContract(hrRouter, hrOwner, dexcontracts.DexRouterV2Wasm)
	router := &RouterInfo{ct: &ct, id: hrRouter}
	dex := &DexInfo{ct: &ct, id: hrDex}
	for _, name := range []string{"HIVE", "HBD"} {
		r := router.registerToken(t, hrOwner, types.RegisterTokenParams{Name: name, TokenInfo: types.TokenInfo{Chain: "HIVE"}})
		require.True(t, r.Success, "register %s: %s: %s", name, r.Err, r.ErrMsg)
	}
	r := router.registerPool(t, hrOwner, types.RegisterPoolParams{Asset0: "hive", Asset1: "hbd", DexContractId: hrDex})
	require.True(t, r.Success, "register pool: %s: %s", r.Err, r.ErrMsg)
	r = dex.initPool(t, hrOwner, &types.InitParams{Asset0: "hive", Asset1: "hbd", FeeBps: 30, RouterContract: hrRouter})
	require.True(t, r.Success, "init pool: %s: %s", r.Err, r.ErrMsg)
	r = dex.addLiquidity(t, hrOwner, 100000, 100000)
	require.True(t, r.Success, "add liquidity: %s: %s", r.Err, r.ErrMsg)
	ct.Deposit(hrTrader, 10000, ledger_db.Asset("hive"))
	ct.Deposit(hrTrader, rcHeadroomHBD, ledger_db.Asset("hbd"))
	return &ct, router
}

func hiveAllow(asset, limit string) []contracts.Intent {
	return []contracts.Intent{{Type: "transfer.allow", Args: map[string]string{"token": asset, "limit": limit}}}
}

func hasLog(r *test_utils.ContractTestCallResult, contractId, prefix string) bool {
	for _, l := range r.Logs[contractId].Logs {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

func TestHiveReturn_FailedSwapGoesBackToHive(t *testing.T) {
	huge := "999999"
	cases := []struct {
		name  string
		instr types.DexInstruction
	}{
		{"slippage", types.DexInstruction{AssetIn: "hive", AssetOut: "hbd", AmountIn: "500", MinAmountOut: &huge, Recipient: hrTrader}},
		{"no pool", types.DexInstruction{AssetIn: "hive", AssetOut: "btc", AmountIn: "500", Recipient: hrTrader}},
		{"same asset", types.DexInstruction{AssetIn: "hive", AssetOut: "hive", AmountIn: "500", Recipient: hrTrader}},
		{"recipient is the router", types.DexInstruction{AssetIn: "hive", AssetOut: "hbd", AmountIn: "500", Recipient: "contract:" + hrRouter}},
		{"bad Magi recipient", types.DexInstruction{AssetIn: "hive", AssetOut: "hbd", AmountIn: "500", Recipient: "nobody!"}},
		{"payout chain does not match", types.DexInstruction{AssetIn: "hive", AssetOut: "hbd", AmountIn: "500", Recipient: "hr-dest", DestinationChain: "BTC"}},
		{"pool cap", types.DexInstruction{AssetIn: "hive", AssetOut: "hbd", AmountIn: "9000", Recipient: hrTrader}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ct, router := setupHiveReturn(t)
			if tc.name == "pool cap" {
				ct.Deposit(hrTrader, 50000, ledger_db.Asset("hive"))
				tc.instr.AmountIn = "60000" // above the pool's per-swap cap (reserve/2)
			}
			hiveBefore := ct.GetBalance(hrTrader, "hive")
			poolHive := ct.GetBalance("contract:"+hrDex, "hive")
			in := tc.instr
			in.Type, in.Version = "swap", "1.0.0"
			in.ReturnAddress = &types.ReturnAddress{Chain: "HIVE", Address: hrTrader}
			r := router.executeRc(t, hrTrader, &in, hiveAllow("hive", in.AmountIn), 10000)
			require.True(t, r.Success, "a Hive-return swap that fails must still succeed (refund): %s: %s", r.Err, r.ErrMsg)
			require.True(t, hasLog(r, hrRouter, "swaprefund|t="+hrTrader+"|a=hive|d="+in.AmountIn+"|why="), "refund log: %v", r.Logs[hrRouter].Logs)
			require.Contains(t, r.Ret, `"refunded":"`+in.AmountIn+`"`)
			// The router drew the input and holds none of it, the pool did not
			// get it and the trader's Magi balance is short by it: it left Magi
			// through the router's withdrawal to the Hive account the log names.
			require.Contains(t, r.Ret, `"to":"`+hrTrader+`"`)
			require.Equal(t, hiveBefore-atoi(t, in.AmountIn), ct.GetBalance(hrTrader, "hive"), "nothing of it stays in the Magi balance")
			require.Equal(t, int64(0), ct.GetBalance("contract:"+hrRouter, "hive"), "nothing stays on the router")
			require.Equal(t, poolHive, ct.GetBalance("contract:"+hrDex, "hive"), "the pool is untouched")
		})
	}
}

func TestHiveReturn_GoodSwapUnchanged(t *testing.T) {
	// Kept on Magi.
	ct, router := setupHiveReturn(t)
	hiveBefore := ct.GetBalance(hrTrader, "hive")
	hbdBefore := ct.GetBalance(hrTrader, "hbd")
	in := types.DexInstruction{Type: "swap", Version: "1.0.0", AssetIn: "hive", AssetOut: "hbd", AmountIn: "500", Recipient: hrTrader,
		ReturnAddress: &types.ReturnAddress{Chain: "HIVE", Address: hrTrader}}
	r := router.executeRc(t, hrTrader, &in, hiveAllow("hive", "500"), 10000)
	require.True(t, r.Success, "%s: %s", r.Err, r.ErrMsg)
	require.False(t, hasLog(r, hrRouter, "swaprefund"))
	require.Equal(t, hiveBefore-500, ct.GetBalance(hrTrader, "hive"))
	require.Greater(t, ct.GetBalance(hrTrader, "hbd"), hbdBefore, "the output reached the recipient")
	require.Equal(t, int64(0), ct.GetBalance("contract:"+hrRouter, "hive"))
	require.Equal(t, int64(0), ct.GetBalance("contract:"+hrRouter, "hbd"))
	require.Contains(t, r.Ret, `"amount_out"`)
	t.Logf("Hive-return swap kept on Magi: rc_used %d", r.RcUsed)

	// Paid out on Hive.
	ct2, router2 := setupHiveReturn(t)
	in2 := types.DexInstruction{Type: "swap", Version: "1.0.0", AssetIn: "hive", AssetOut: "hbd", AmountIn: "500", Recipient: "hr-dest",
		DestinationChain: "HIVE", ReturnAddress: &types.ReturnAddress{Chain: "HIVE", Address: "hr-trader"}}
	r = router2.executeRc(t, hrTrader, &in2, hiveAllow("hive", "500"), 10000)
	require.True(t, r.Success, "%s: %s", r.Err, r.ErrMsg)
	require.False(t, hasLog(r, hrRouter, "swaprefund"))
	require.Equal(t, int64(0), ct2.GetBalance("contract:"+hrRouter, "hbd"), "the HBD left through the withdrawal to the Hive recipient")
	require.Equal(t, int64(0), ct2.GetBalance("hive:hr-dest", "hbd"), "nothing landed in the recipient's Magi balance")
	require.Equal(t, int64(10000-500), ct2.GetBalance(hrTrader, "hive"))
	t.Logf("Hive-return swap paid on Hive: rc_used %d", r.RcUsed)

	// The same two without return_address, for the RC comparison.
	ct3, router3 := setupHiveReturn(t)
	in3 := in
	in3.ReturnAddress = nil
	r3 := router3.executeRc(t, hrTrader, &in3, hiveAllow("hive", "500"), 10000)
	require.True(t, r3.Success, "%s: %s", r3.Err, r3.ErrMsg)
	t.Logf("plain swap kept on Magi: rc_used %d", r3.RcUsed)
	_ = ct3
}

func TestHiveReturn_RefusedBeforeAnythingMoves(t *testing.T) {
	cases := []struct {
		name    string
		instr   types.DexInstruction
		intents []contracts.Intent
		want    string
	}{
		{"not a Hive account", types.DexInstruction{AssetIn: "hive", AssetOut: "hbd", AmountIn: "500", Recipient: hrTrader,
			ReturnAddress: &types.ReturnAddress{Chain: "HIVE", Address: "contract:" + hrDex}}, hiveAllow("hive", "500"), "is not a Hive account"},
		{"an EVM address", types.DexInstruction{AssetIn: "hive", AssetOut: "hbd", AmountIn: "500", Recipient: hrTrader,
			ReturnAddress: &types.ReturnAddress{Chain: "HIVE", Address: "did:pkh:eip155:1:0x00000000000000000000000000000000000000e2"}}, hiveAllow("hive", "500"), "is not a Hive account"},
		{"a mapped input", types.DexInstruction{AssetIn: "btc", AssetOut: "hbd", AmountIn: "500", Recipient: hrTrader,
			ReturnAddress: &types.ReturnAddress{Chain: "HIVE", Address: hrTrader}}, hiveAllow("hive", "500"), "needs a HIVE or HBD input"},
		{"zero amount", types.DexInstruction{AssetIn: "hive", AssetOut: "hbd", AmountIn: "0", Recipient: hrTrader,
			ReturnAddress: &types.ReturnAddress{Chain: "HIVE", Address: hrTrader}}, hiveAllow("hive", "500"), "amount_in must be positive"},
		// The draw itself fails: nothing was taken, so there is nothing to send back.
		{"intent below amount", types.DexInstruction{AssetIn: "hive", AssetOut: "hbd", AmountIn: "500", Recipient: hrTrader,
			ReturnAddress: &types.ReturnAddress{Chain: "HIVE", Address: hrTrader}}, hiveAllow("hive", "499"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ct, router := setupHiveReturn(t)
			hiveBefore := ct.GetBalance(hrTrader, "hive")
			in := tc.instr
			in.Type, in.Version = "swap", "1.0.0"
			r := router.executeRc(t, hrTrader, &in, tc.intents, 10000)
			require.False(t, r.Success, "must be refused")
			if tc.want != "" {
				require.Contains(t, r.ErrMsg, tc.want)
			}
			require.Equal(t, hiveBefore, ct.GetBalance(hrTrader, "hive"), "nothing moved")
		})
	}
}

func TestHiveReturn_SwapDrawnIsTheRoutersOwn(t *testing.T) {
	ct, _ := setupHiveReturn(t)
	in := types.DrawnSwap{Instruction: types.DexInstruction{Type: "swap", Version: "1.0.0", AssetIn: "hive", AssetOut: "hbd",
		AmountIn: "500", Recipient: hrTrader}, User: hrTrader, RefundTo: hrTrader}
	payload, err := jsonMarshal(&in)
	require.NoError(t, err)
	r := ct.Call(state_engine.TxVscCallContract{Self: *basicSelf(t, hrTrader), ContractId: hrRouter, Action: "swap_drawn",
		Payload: payload, RcLimit: 10000, Intents: []contracts.Intent{}, Caller: hrTrader})
	require.False(t, r.Success)
	require.Contains(t, r.ErrMsg, "the router's own call")
}

// Without return_address nothing changes: a failed swap aborts, and the HIVE
// stays where it was (the behaviour return_address on HIVE replaces).
func TestHiveReturn_AbsentKeepsTheOldBehaviour(t *testing.T) {
	ct, router := setupHiveReturn(t)
	hiveBefore := ct.GetBalance(hrTrader, "hive")
	huge := "999999"
	for _, ra := range []*types.ReturnAddress{nil, {Chain: "MAGI", Address: hrTrader}} {
		in := types.DexInstruction{Type: "swap", Version: "1.0.0", AssetIn: "hive", AssetOut: "hbd", AmountIn: "500",
			MinAmountOut: &huge, Recipient: hrTrader, ReturnAddress: ra}
		r := router.executeRc(t, hrTrader, &in, hiveAllow("hive", "500"), 10000)
		require.False(t, r.Success)
		require.Equal(t, hiveBefore, ct.GetBalance(hrTrader, "hive"))
	}
}

func atoi(t *testing.T, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	require.NoError(t, err)
	return n
}

func jsonMarshal(v *types.DrawnSwap) ([]byte, error) { return tinyjson.Marshal(v) }
