package tests

// SP-0: a pool whose asset lives on the EVM bridge (multi-asset: "eth" and
// registered token symbols). The bridge's transfer / transferFrom refuse a
// call without an "asset" (isAllowedAsset("") is false), so before the fix
// every pool draw and payout of USDC aborted "unknown asset". This runs the
// REAL bridge wasm (EVM_BRIDGE_WASM, default: the parity devnet build) next
// to the real pool wasm.

import (
	"encoding/json"
	"os"
	"strconv"
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/db/vsc/contracts"
	ledger_db "vsc-node/modules/db/vsc/ledger"
	state_engine "vsc-node/modules/state-processing"

	"github.com/CosmWasm/tinyjson"
	"github.com/stretchr/testify/require"

	dexcontracts "github.com/vsc-eco/dex-contracts"
	"github.com/vsc-eco/dex-contracts/contracts/types"
)

const (
	evmBridgeId  = "vsc1Bo8h3TMFAYX5rkgCeauXFywZGiMbUftQL2"
	usdcPoolId   = "vsc1Bnuikc8sJii5baG5gmxno4V2xTW7joi2vu"
	usdcLp       = "hive:usdclpxx"
	usdcTrader   = "hive:usdctrader"
	usdcPoolLiq  = 100_000_000_000 // 100k USDC (6 decimals)
	hbdPoolLiq   = 100_000_000     // 100k HBD (3 decimals)
	usdcRcLimit  = 20_000
	evmBridgeEnv = "EVM_BRIDGE_WASM"
	// evmBridgeSkip is the explicit opt-out: a missing artifact otherwise fails.
	evmBridgeSkip = "skip"
	usdcOwner     = "hive:milo-hpr"
	usdcRouter    = "vsc1Bpc3SgDqCRQxzeDrvV7T4XKV6BZuHmME5F"
)

// evmBridgeWasm loads the real EVM bridge wasm these tests run against.
//
// A MISSING ARTIFACT FAILS, it does not skip. The default path below is one
// developer's checkout, so on every other machine — and in CI — a skip here
// turned the whole file, and every later test that needs the bridge (the
// settlement-refund test in return_address_test.go among them), into a silent
// no-op that still reported ok. A change nobody tested read as a change that
// passed. Set EVM_BRIDGE_WASM to the bridge build, or EVM_BRIDGE_WASM=skip to
// skip these deliberately and knowingly.
func evmBridgeWasm(t *testing.T) []byte {
	t.Helper()
	path := os.Getenv(evmBridgeEnv)
	if path == evmBridgeSkip {
		t.Skipf("%s=%s: skipping the tests that need the real EVM bridge wasm", evmBridgeEnv, evmBridgeSkip)
	}
	if path == "" {
		path = os.ExpandEnv("$HOME/evm-fix/gvn-parity-devnet/.devnet/evm-artifacts-parity/evm-bridge.wasm")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("EVM bridge wasm not available (%s): %v\n"+
			"These tests run against the real bridge build. Point %s at it, "+
			"or set %s=%s to skip them on purpose.", path, err, evmBridgeEnv, evmBridgeEnv, evmBridgeSkip)
	}
	return b
}

func bridgeBalance(ct *test_utils.ContractTest, who, asset string) string {
	return ct.StateGet(evmBridgeId, "a-"+who+"-"+asset)
}

func bridgeCall(t *testing.T, ct *test_utils.ContractTest, caller, action string, payload any) test_utils.ContractTestCallResult {
	t.Helper()
	b, err := json.Marshal(payload)
	require.NoError(t, err)
	return ct.Call(state_engine.TxVscCallContract{
		Self: *basicSelf(t, caller), ContractId: evmBridgeId, Action: action, Payload: b,
		RcLimit: usdcRcLimit, Intents: []contracts.Intent{}, Caller: caller,
	})
}

func setupUsdcHbdPool(t *testing.T) test_utils.ContractTest {
	t.Helper()
	return setupUsdcHbdPoolWith(t, types.InitParams{
		Asset0: "usdc", Asset0MappingContract: evmBridgeId, Asset1: "hbd", FeeBps: 8, RouterContract: usdcRouter,
	}, func(ct *test_utils.ContractTest) { whitelistPendulum(ct, usdcPoolId) })
}

// setupUsdcHbdPoolWith: a USDC/HBD pool initialized with `init` on the real
// EVM bridge, its applier wired by `wire`, seeded with 100k/100k.
func setupUsdcHbdPoolWith(t *testing.T, init types.InitParams, wire func(*test_utils.ContractTest)) test_utils.ContractTest {
	t.Helper()
	requireWasm(t, "dex", dexcontracts.DexWasm)
	bridge := evmBridgeWasm(t)
	ct := test_utils.NewContractTest()
	wire(&ct)
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(evmBridgeId, usdcOwner, bridge)
	ct.RegisterContract(usdcPoolId, usdcOwner, dexcontracts.DexWasm)
	ct.RegisterContract(usdcRouter, usdcOwner, dexcontracts.DexRouterV2Wasm)

	// The bridge after registerToken("usdc") and two credited deposits.
	ct.StateSet(evmBridgeId, "as-usdc", "1")
	ct.StateSet(evmBridgeId, "a-"+usdcLp+"-usdc", strconv.Itoa(usdcPoolLiq))
	ct.StateSet(evmBridgeId, "a-"+usdcTrader+"-usdc", "10000000000") // 10k USDC
	// The bridge's books for those deposits (Active|User|Fee|BaseFee): a
	// withdrawal debits them.
	ct.StateSet(evmBridgeId, "s-usdc", strconv.Itoa(usdcPoolLiq+10_000_000_000)+"|"+strconv.Itoa(usdcPoolLiq+10_000_000_000)+"|0|0")

	payload, _ := tinyjson.Marshal(&init)
	r := ct.Call(state_engine.TxVscCallContract{
		Self: *basicSelf(t, usdcOwner), ContractId: usdcPoolId, Action: "init", Payload: payload,
		RcLimit: usdcRcLimit, Intents: []contracts.Intent{}, Caller: usdcOwner,
	})
	require.True(t, r.Success, "init: %s: %s", r.Err, r.ErrMsg)

	// LP approves the pool on the bridge, then adds liquidity (hbd is asset0).
	r = bridgeCall(t, &ct, usdcLp, "approve", map[string]string{"spender": "contract:" + usdcPoolId, "amount": strconv.Itoa(usdcPoolLiq), "asset": "usdc"})
	require.True(t, r.Success, "approve: %s: %s", r.Err, r.ErrMsg)
	ct.Deposit(usdcLp, hbdPoolLiq+rcHeadroomHBD, ledger_db.Asset("hbd"))
	lp, _ := tinyjson.Marshal(types.AddLiquidityParams{
		Amount0: strconv.Itoa(hbdPoolLiq), Amount1: strconv.Itoa(usdcPoolLiq), Recipient: usdcLp,
	})
	r = ct.Call(state_engine.TxVscCallContract{
		Self: *basicSelf(t, usdcLp), ContractId: usdcPoolId, Action: "add_liquidity", Payload: lp,
		RcLimit: usdcRcLimit, Caller: usdcLp,
		Intents: []contracts.Intent{{Type: "transfer.allow", Args: map[string]string{"token": "hbd", "limit": strconv.Itoa(hbdPoolLiq)}}},
	})
	require.True(t, r.Success, "SP-0: add_liquidity drawing USDC from the EVM bridge: %s: %s", r.Err, r.ErrMsg)
	require.Equal(t, strconv.Itoa(usdcPoolLiq), bridgeBalance(&ct, "contract:"+usdcPoolId, "usdc"), "pool's USDC on the bridge")
	require.Equal(t, "0", bridgeBalance(&ct, usdcLp, "usdc"))
	return ct
}

func TestSP0_PoolDrawsAndPaysUsdcOnTheEvmBridge(t *testing.T) {
	ct := setupUsdcHbdPool(t)

	// HBD -> USDC: the pool pays USDC out through the bridge's transfer.
	ct.Deposit(usdcTrader, 1_000_000+rcHeadroomHBD, ledger_db.Asset("hbd"))
	sw, _ := tinyjson.Marshal(types.SwapParams{AssetIn: "hbd", AmountIn: "1000000", AssetOut: "usdc", From: usdcTrader, To: usdcTrader})
	r := ct.Call(state_engine.TxVscCallContract{
		Self: *basicSelf(t, usdcTrader), ContractId: usdcPoolId, Action: "swap", Payload: sw,
		RcLimit: usdcRcLimit, Caller: usdcTrader,
		Intents: []contracts.Intent{{Type: "transfer.allow", Args: map[string]string{"token": "hbd", "limit": "1000000"}}},
	})
	require.True(t, r.Success, "SP-0: swap paying USDC: %s: %s", r.Err, r.ErrMsg)
	got, _ := strconv.ParseInt(bridgeBalance(&ct, usdcTrader, "usdc"), 10, 64)
	require.Greater(t, got, int64(10_000_000_000), "trader received USDC on the bridge")

	// USDC -> HBD: the pool draws USDC through transferFrom.
	r = bridgeCall(t, &ct, usdcTrader, "approve", map[string]string{"spender": "contract:" + usdcPoolId, "amount": "500000000", "asset": "usdc"})
	require.True(t, r.Success, "approve: %s: %s", r.Err, r.ErrMsg)
	hbdBefore := ct.GetBalance(usdcTrader, ledger_db.Asset("hbd"))
	sw, _ = tinyjson.Marshal(types.SwapParams{AssetIn: "usdc", AmountIn: "500000000", AssetOut: "hbd", From: usdcTrader, To: usdcTrader})
	r = ct.Call(state_engine.TxVscCallContract{
		Self: *basicSelf(t, usdcTrader), ContractId: usdcPoolId, Action: "swap", Payload: sw,
		RcLimit: usdcRcLimit, Caller: usdcTrader, Intents: []contracts.Intent{},
	})
	require.True(t, r.Success, "SP-0: swap drawing USDC: %s: %s", r.Err, r.ErrMsg)
	require.Greater(t, ct.GetBalance(usdcTrader, ledger_db.Asset("hbd")), hbdBefore, "trader received HBD")
}

// SP-0b + SP-0 on the router: register USDC from the EVM bridge (getInfo by
// symbol), register the pool, and route swaps both ways. The router pre-funds
// a mapped input with the bridge's transferFrom, so it needs the asset field
// too. This is also the path an EVM deposit-and-swap takes (the bridge calls
// router.execute after granting the router an allowance).
func TestSP0b_RouterRegistersAndRoutesUsdc(t *testing.T) {
	ct := setupUsdcHbdPool(t)
	router := &RouterInfo{ct: &ct, id: usdcRouter}
	r := router.initRouterV2(t, usdcOwner)
	require.True(t, r.Success, "init router: %s: %s", r.Err, r.ErrMsg)

	// The bridge's token registry after registerToken(usdc, 6 decimals).
	ref := "11155111-1c7d4b196cb0c7b01d743fbc6116a902379c7238"
	ct.StateSet(evmBridgeId, "ts-usdc", ref)
	ct.StateSet(evmBridgeId, "t-"+ref, "usdc|6|1")

	r = router.registerToken(t, usdcOwner, types.RegisterTokenParams{Name: "HBD", TokenInfo: types.TokenInfo{Chain: "HIVE"}})
	require.True(t, r.Success, "register hbd: %s: %s", r.Err, r.ErrMsg)
	// register_token reads getInfo from the bridge (a large wasm): over the
	// helper's 2000 RC cap.
	regPayload, _ := tinyjson.Marshal(types.RegisterTokenParams{Name: "usdc", TokenInfo: types.TokenInfo{Chain: "ETH", MappingContract: evmBridgeId}})
	reg := ct.Call(state_engine.TxVscCallContract{
		Self: *basicSelf(t, usdcOwner), ContractId: usdcRouter, Action: "register_token", Payload: regPayload,
		RcLimit: usdcRcLimit, Intents: []contracts.Intent{}, Caller: usdcOwner,
	})
	require.True(t, reg.Success, "SP-0b: register usdc from the EVM bridge: %s: %s", reg.Err, reg.ErrMsg)
	t.Logf("register_token usdc: %d RC", reg.RcUsed)
	r = router.registerPool(t, usdcOwner, types.RegisterPoolParams{Asset0: "hbd", Asset1: "usdc", DexContractId: usdcPoolId})
	require.True(t, r.Success, "register pool: %s: %s", r.Err, r.ErrMsg)

	// USDC -> HBD through the router: the router pulls USDC from the trader.
	r2 := bridgeCall(t, &ct, usdcTrader, "approve", map[string]string{"spender": "contract:" + usdcRouter, "amount": "300000000", "asset": "usdc"})
	require.True(t, r2.Success, "approve router: %s: %s", r2.Err, r2.ErrMsg)
	ct.Deposit(usdcTrader, rcHeadroomHBD, ledger_db.Asset("hbd"))
	hbdBefore := ct.GetBalance(usdcTrader, ledger_db.Asset("hbd"))
	r = router.execute(t, usdcTrader, &types.DexInstruction{
		Type: "swap", Version: "1.0.0", AssetIn: "usdc", AssetOut: "hbd", AmountIn: "300000000", Recipient: usdcTrader,
	}, []contracts.Intent{})
	require.True(t, r.Success, "SP-0: routed USDC->HBD: %s: %s", r.Err, r.ErrMsg)
	require.Greater(t, ct.GetBalance(usdcTrader, ledger_db.Asset("hbd")), hbdBefore)

	// HBD -> USDC through the router.
	ct.Deposit(usdcTrader, 200_000, ledger_db.Asset("hbd"))
	usdcBefore, _ := strconv.ParseInt(bridgeBalance(&ct, usdcTrader, "usdc"), 10, 64)
	r = router.execute(t, usdcTrader, &types.DexInstruction{
		Type: "swap", Version: "1.0.0", AssetIn: "hbd", AssetOut: "usdc", AmountIn: "200000", Recipient: usdcTrader,
	}, []contracts.Intent{{Type: "transfer.allow", Args: map[string]string{"token": "hbd", "limit": "200000"}}})
	require.True(t, r.Success, "routed HBD->USDC: %s: %s", r.Err, r.ErrMsg)
	usdcAfter, _ := strconv.ParseInt(bridgeBalance(&ct, usdcTrader, "usdc"), 10, 64)
	require.Greater(t, usdcAfter, usdcBefore)
}

// SP-0c: a swap settled to Ethereum through the router. The EVM bridge has no
// "unmap"; the router now calls unmapERC20 (a token) / unmapETH, withdraws the
// output less the bridge's proof fee, and names the trader as refund_to, so a
// refund (here: the trader cancels the queued withdrawal) reaches the trader
// and not the router, which could never move it.
func TestSP0c_RouterSettlesUsdcToEthereum(t *testing.T) {
	ct := setupUsdcHbdPool(t)
	router := &RouterInfo{ct: &ct, id: usdcRouter}
	r := router.initRouterV2(t, usdcOwner)
	require.True(t, r.Success, "init router: %s: %s", r.Err, r.ErrMsg)
	ref := "11155111-1c7d4b196cb0c7b01d743fbc6116a902379c7238"
	ct.StateSet(evmBridgeId, "ts-usdc", ref)
	ct.StateSet(evmBridgeId, "t-"+ref, "usdc|6|1")
	ct.StateSet(evmBridgeId, "pf-usdc", "150000")   // 0.15 USDC proof fee
	ct.StateSet(evmBridgeId, "chainid", "11155111") // the bridge's own chain (Sepolia), as registered
	r = router.registerToken(t, usdcOwner, types.RegisterTokenParams{Name: "HBD", TokenInfo: types.TokenInfo{Chain: "HIVE"}})
	require.True(t, r.Success, "register hbd: %s: %s", r.Err, r.ErrMsg)
	regPayload, _ := tinyjson.Marshal(types.RegisterTokenParams{Name: "usdc", TokenInfo: types.TokenInfo{Chain: "ETH", MappingContract: evmBridgeId}})
	reg := ct.Call(state_engine.TxVscCallContract{
		Self: *basicSelf(t, usdcOwner), ContractId: usdcRouter, Action: "register_token", Payload: regPayload,
		RcLimit: usdcRcLimit, Intents: []contracts.Intent{}, Caller: usdcOwner,
	})
	require.True(t, reg.Success, "register usdc: %s: %s", reg.Err, reg.ErrMsg)
	r = router.registerPool(t, usdcOwner, types.RegisterPoolParams{Asset0: "hbd", Asset1: "usdc", DexContractId: usdcPoolId})
	require.True(t, r.Success, "register pool: %s: %s", r.Err, r.ErrMsg)

	// 1 HBD -> USDC, settled to an Ethereum address.
	const l1To = "0x00000000000000000000000000000000000000e2"
	ct.Deposit(usdcTrader, 1_000+rcHeadroomHBD, ledger_db.Asset("hbd"))
	usdcBefore := bridgeBalance(&ct, usdcTrader, "usdc")
	maxFee := int64(3_000_000_000_000_000) // the trader's cap on the reserve-paid gas (wei)
	r = router.execute(t, usdcTrader, &types.DexInstruction{
		Type: "swap", Version: "1.0.0", AssetIn: "hbd", AssetOut: "usdc", AmountIn: "1000",
		Recipient: l1To, DestinationChain: "ETH", MaxFee: &maxFee,
	}, []contracts.Intent{{Type: "transfer.allow", Args: map[string]string{"token": "hbd", "limit": "1000"}}})
	require.True(t, r.Success, "SP-0c: routed HBD->USDC settled to Ethereum: %s: %s", r.Err, r.ErrMsg)
	t.Logf("router swap + unmapERC20: %d RC", r.RcUsed)

	// The withdrawal is queued (no TSS key here) from the router, for the
	// trader, of the output less the proof fee; the router keeps nothing.
	tail, _ := strconv.Atoi(ct.StateGet(evmBridgeId, "wqt"))
	require.Equal(t, 1, tail, "one queued withdrawal")
	var q struct {
		From, To, Asset string
		Amount          json.Number
		RefundTo        string      `json:"refund_to"`
		TokenAddress    string      `json:"token_address"`
		MaxFee          json.Number `json:"max_fee"`
	}
	require.NoError(t, json.Unmarshal([]byte(ct.StateGet(evmBridgeId, "wq-0")), &q), ct.StateGet(evmBridgeId, "wq-0"))
	t.Logf("queued withdrawal: %+v", q)
	require.Equal(t, "contract:"+usdcRouter, q.From)
	require.Equal(t, usdcTrader, q.RefundTo)
	require.Equal(t, l1To, q.To)
	require.Equal(t, "usdc", q.Asset)
	require.Equal(t, "0x1c7d4b196cb0c7b01d743fbc6116a902379c7238", q.TokenAddress)
	require.Equal(t, "3000000000000000", string(q.MaxFee), "the trader's max_fee reaches the bridge (as its decimal string)")
	amt, _ := strconv.ParseInt(string(q.Amount), 10, 64)
	require.Greater(t, amt, int64(800_000), "~0.99 USDC less the 0.15 fee")
	require.Contains(t, []string{"", "0"}, bridgeBalance(&ct, "contract:"+usdcRouter, "usdc"), "the router holds nothing")
	require.Equal(t, "150000", bridgeBalance(&ct, "hive:altera.app", "usdc"), "the bridge charged its proof fee (ProofFeeRecipient)")

	// The trader cancels: the refund is the trader's, not the router's.
	cr := bridgeCall(t, &ct, usdcTrader, "cancelQueuedWithdrawal", map[string]any{"seq": 0})
	require.True(t, cr.Success, "trader cancels: %s: %s", cr.Err, cr.ErrMsg)
	before, _ := strconv.ParseInt(usdcBefore, 10, 64)
	after, _ := strconv.ParseInt(bridgeBalance(&ct, usdcTrader, "usdc"), 10, 64)
	require.Equal(t, before+amt, after, "the refund reached the trader")
	require.Contains(t, []string{"", "0"}, bridgeBalance(&ct, "contract:"+usdcRouter, "usdc"), "nothing stranded in the router")
}

// SP-0c for ETH: unmapETH with the L1 fee taken from the amount (deduct_fee)
// and the proof fee netted out by the router, on a 100 ETH pool (SP-0d: past
// int64 in wei, so this also needs the 0.13.0 big swap amounts, and proves
// they reach a pool swapping inside the router's call).
func TestSP0c_RouterSettlesEthToEthereum(t *testing.T) {
	requireWasm(t, "dex", dexcontracts.DexWasm)
	bridge := evmBridgeWasm(t)
	const ethPool = "vsc1BquGPy8B766YpstdcL5cSF2GkWVVsVxJS3"
	ct := test_utils.NewContractTest()
	whitelistPendulum(&ct, ethPool)
	// 0.13.0 (SP-0d): 100 ETH is 1e20 wei, past int64; before 0.13 every swap
	// of this pool is refused "invalid argument".
	ct.BigSwapAmountsActive = true
	t.Cleanup(func() { ct.DataLayer.Stop() })
	ct.RegisterContract(evmBridgeId, usdcOwner, bridge)
	ct.RegisterContract(ethPool, usdcOwner, dexcontracts.DexWasm)
	ct.RegisterContract(usdcRouter, usdcOwner, dexcontracts.DexRouterV2Wasm)
	const lpEth = "100000000000000000000" // 100 ETH
	ct.StateSet(evmBridgeId, "a-"+usdcLp+"-eth", lpEth)
	ct.StateSet(evmBridgeId, "s-eth", lpEth+"|"+lpEth+"|0|0")
	ct.StateSet(evmBridgeId, "pf-eth", "50000000000000") // 0.00005 ETH proof fee

	payload, _ := tinyjson.Marshal(&types.InitParams{Asset0: "eth", Asset0MappingContract: evmBridgeId, Asset1: "hbd", FeeBps: 8, RouterContract: usdcRouter})
	r := ct.Call(state_engine.TxVscCallContract{Self: *basicSelf(t, usdcOwner), ContractId: ethPool, Action: "init", Payload: payload, RcLimit: usdcRcLimit, Intents: []contracts.Intent{}, Caller: usdcOwner})
	require.True(t, r.Success, "init: %s: %s", r.Err, r.ErrMsg)
	r = bridgeCall(t, &ct, usdcLp, "approve", map[string]string{"spender": "contract:" + ethPool, "amount": lpEth, "asset": "eth"})
	require.True(t, r.Success, "approve: %s: %s", r.Err, r.ErrMsg)
	ct.Deposit(usdcLp, hbdPoolLiq+rcHeadroomHBD, ledger_db.Asset("hbd"))
	lp, _ := tinyjson.Marshal(types.AddLiquidityParams{Amount0: lpEth, Amount1: strconv.Itoa(hbdPoolLiq), Recipient: usdcLp})
	r = ct.Call(state_engine.TxVscCallContract{Self: *basicSelf(t, usdcLp), ContractId: ethPool, Action: "add_liquidity", Payload: lp, RcLimit: usdcRcLimit, Caller: usdcLp,
		Intents: []contracts.Intent{{Type: "transfer.allow", Args: map[string]string{"token": "hbd", "limit": strconv.Itoa(hbdPoolLiq)}}}})
	require.True(t, r.Success, "add_liquidity ETH: %s: %s", r.Err, r.ErrMsg)

	router := &RouterInfo{ct: &ct, id: usdcRouter}
	require.True(t, router.initRouterV2(t, usdcOwner).Success)
	require.True(t, router.registerToken(t, usdcOwner, types.RegisterTokenParams{Name: "HBD", TokenInfo: types.TokenInfo{Chain: "HIVE"}}).Success)
	regPayload, _ := tinyjson.Marshal(types.RegisterTokenParams{Name: "eth", TokenInfo: types.TokenInfo{Chain: "ETH", MappingContract: evmBridgeId}})
	reg := ct.Call(state_engine.TxVscCallContract{Self: *basicSelf(t, usdcOwner), ContractId: usdcRouter, Action: "register_token", Payload: regPayload, RcLimit: usdcRcLimit, Intents: []contracts.Intent{}, Caller: usdcOwner})
	require.True(t, reg.Success, "register eth: %s: %s", reg.Err, reg.ErrMsg)
	require.True(t, router.registerPool(t, usdcOwner, types.RegisterPoolParams{Asset0: "eth", Asset1: "hbd", DexContractId: ethPool}).Success)

	// 1,000 HBD (1% of the pool) -> ~0.99 ETH, settled to an Ethereum address.
	const l1To = "0x00000000000000000000000000000000000000e3"
	ct.Deposit(usdcTrader, 1_000_000+rcHeadroomHBD, ledger_db.Asset("hbd"))
	rx := router.execute(t, usdcTrader, &types.DexInstruction{
		Type: "swap", Version: "1.0.0", AssetIn: "hbd", AssetOut: "eth", AmountIn: "1000000",
		Recipient: l1To, DestinationChain: "ETH",
	}, []contracts.Intent{{Type: "transfer.allow", Args: map[string]string{"token": "hbd", "limit": "1000000"}}})
	require.True(t, rx.Success, "SP-0c: routed HBD->ETH settled to Ethereum: %s: %s", rx.Err, rx.ErrMsg)
	t.Logf("router swap + unmapETH: %d RC", rx.RcUsed)
	var q struct {
		From, To, Asset string
		Amount          json.Number
		Debit           json.Number
		RefundTo        string `json:"refund_to"`
		DeductFee       bool   `json:"deduct_fee"`
	}
	require.NoError(t, json.Unmarshal([]byte(ct.StateGet(evmBridgeId, "wq-0")), &q), ct.StateGet(evmBridgeId, "wq-0"))
	t.Logf("queued withdrawal: %+v", q)
	require.Equal(t, "contract:"+usdcRouter, q.From)
	require.Equal(t, usdcTrader, q.RefundTo)
	require.Equal(t, l1To, q.To)
	require.True(t, q.DeductFee, "the L1 fee comes out of the amount")
	require.Contains(t, []string{"", "0"}, bridgeBalance(&ct, "contract:"+usdcRouter, "eth"), "the router holds nothing")
	require.Equal(t, "50000000000000", bridgeBalance(&ct, "hive:altera.app", "eth"), "proof fee charged")
	cr := bridgeCall(t, &ct, usdcTrader, "cancelQueuedWithdrawal", map[string]any{"seq": 0})
	require.True(t, cr.Success, "trader cancels: %s: %s", cr.Err, cr.ErrMsg)
	require.Equal(t, string(q.Debit), bridgeBalance(&ct, usdcTrader, "eth"), "the refund reached the trader")
}

// SP-0c on the two-hop route: HIVE -> HBD -> USDC settled to an Ethereum
// address (no direct HIVE/USDC pool, so the router swaps through HBD and
// settles the second hop's output). The queued withdrawal is from the router
// with refund_to = the trader, as on the one-hop route; the router keeps
// nothing, and the trader's cancel refunds the trader.
func TestSP0c_TwoHopSettlesUsdcToEthereum(t *testing.T) {
	const hiveHbdPoolId = "vsc1Bjn53csDr6wUoYsjXiN9Nhadu458Tw9wvR"
	ct := setupUsdcHbdPoolWith(t, types.InitParams{
		Asset0: "usdc", Asset0MappingContract: evmBridgeId, Asset1: "hbd", FeeBps: 8, RouterContract: usdcRouter,
	}, func(ct *test_utils.ContractTest) { whitelistPendulum(ct, usdcPoolId, hiveHbdPoolId) })
	ct.RegisterContract(hiveHbdPoolId, usdcOwner, dexcontracts.DexWasm)
	hivePool := &DexInfo{ct: &ct, id: hiveHbdPoolId}
	r := hivePool.initPool(t, usdcOwner, &types.InitParams{Asset0: "hbd", Asset1: "hive", FeeBps: 8, RouterContract: usdcRouter})
	require.True(t, r.Success, "init hive/hbd pool: %s: %s", r.Err, r.ErrMsg)
	r = hivePool.addLiquidity(t, usdcOwner, 100_000_000, 100_000_000)
	require.True(t, r.Success, "hive/hbd liquidity: %s: %s", r.Err, r.ErrMsg)

	router := &RouterInfo{ct: &ct, id: usdcRouter}
	r = router.initRouterV2(t, usdcOwner)
	require.True(t, r.Success, "init router: %s: %s", r.Err, r.ErrMsg)
	ref := "11155111-1c7d4b196cb0c7b01d743fbc6116a902379c7238"
	ct.StateSet(evmBridgeId, "ts-usdc", ref)
	ct.StateSet(evmBridgeId, "t-"+ref, "usdc|6|1")
	ct.StateSet(evmBridgeId, "pf-usdc", "150000")
	ct.StateSet(evmBridgeId, "chainid", "11155111")
	for _, name := range []string{"HBD", "HIVE"} {
		r = router.registerToken(t, usdcOwner, types.RegisterTokenParams{Name: name, TokenInfo: types.TokenInfo{Chain: "HIVE"}})
		require.True(t, r.Success, "register %s: %s: %s", name, r.Err, r.ErrMsg)
	}
	regPayload, _ := tinyjson.Marshal(types.RegisterTokenParams{Name: "usdc", TokenInfo: types.TokenInfo{Chain: "ETH", MappingContract: evmBridgeId}})
	reg := ct.Call(state_engine.TxVscCallContract{
		Self: *basicSelf(t, usdcOwner), ContractId: usdcRouter, Action: "register_token", Payload: regPayload,
		RcLimit: usdcRcLimit, Intents: []contracts.Intent{}, Caller: usdcOwner,
	})
	require.True(t, reg.Success, "register usdc: %s: %s", reg.Err, reg.ErrMsg)
	r = router.registerPool(t, usdcOwner, types.RegisterPoolParams{Asset0: "hbd", Asset1: "usdc", DexContractId: usdcPoolId})
	require.True(t, r.Success, "register hbd/usdc: %s: %s", r.Err, r.ErrMsg)
	r = router.registerPool(t, usdcOwner, types.RegisterPoolParams{Asset0: "hive", Asset1: "hbd", DexContractId: hiveHbdPoolId})
	require.True(t, r.Success, "register hive/hbd: %s: %s", r.Err, r.ErrMsg)

	const l1To = "0x00000000000000000000000000000000000000e3"
	ct.Deposit(usdcTrader, 1_000, ledger_db.Asset("hive"))
	ct.Deposit(usdcTrader, rcHeadroomHBD, ledger_db.Asset("hbd"))
	usdcBefore := bridgeBalance(&ct, usdcTrader, "usdc")
	maxFee := int64(3_000_000_000_000_000)
	r = router.execute(t, usdcTrader, &types.DexInstruction{
		Type: "swap", Version: "1.0.0", AssetIn: "hive", AssetOut: "usdc", AmountIn: "1000",
		Recipient: l1To, DestinationChain: "ETH", MaxFee: &maxFee,
	}, []contracts.Intent{{Type: "transfer.allow", Args: map[string]string{"token": "hive", "limit": "1000"}}})
	require.True(t, r.Success, "two-hop HIVE->HBD->USDC settled to Ethereum: %s: %s", r.Err, r.ErrMsg)
	t.Logf("two-hop router swap + unmapERC20: %d RC", r.RcUsed)

	tail, _ := strconv.Atoi(ct.StateGet(evmBridgeId, "wqt"))
	require.Equal(t, 1, tail, "one queued withdrawal")
	var q struct {
		From, To, Asset string
		Amount          json.Number
		RefundTo        string `json:"refund_to"`
	}
	require.NoError(t, json.Unmarshal([]byte(ct.StateGet(evmBridgeId, "wq-0")), &q), ct.StateGet(evmBridgeId, "wq-0"))
	t.Logf("queued withdrawal: %+v", q)
	require.Equal(t, "contract:"+usdcRouter, q.From)
	require.Equal(t, usdcTrader, q.RefundTo, "the two-hop route names the trader as refund_to too")
	require.Equal(t, l1To, q.To)
	require.Equal(t, "usdc", q.Asset)
	amt, _ := strconv.ParseInt(string(q.Amount), 10, 64)
	require.Greater(t, amt, int64(700_000), "~0.98 USDC after two pool fees, less the 0.15 proof fee")
	require.Contains(t, []string{"", "0"}, bridgeBalance(&ct, "contract:"+usdcRouter, "usdc"), "the router holds no USDC")
	require.Equal(t, int64(0), ct.GetBalance("contract:"+usdcRouter, ledger_db.Asset("hbd")), "the router keeps no intermediate HBD")

	cr := bridgeCall(t, &ct, usdcTrader, "cancelQueuedWithdrawal", map[string]any{"seq": 0})
	require.True(t, cr.Success, "trader cancels: %s: %s", cr.Err, cr.ErrMsg)
	before, _ := strconv.ParseInt(usdcBefore, 10, 64)
	after, _ := strconv.ParseInt(bridgeBalance(&ct, usdcTrader, "usdc"), 10, 64)
	require.Equal(t, before+amt, after, "the refund reached the trader")
	require.Contains(t, []string{"", "0"}, bridgeBalance(&ct, "contract:"+usdcRouter, "usdc"), "nothing stranded in the router")
}
