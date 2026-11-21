package tests

// SENDER-REFUND-1 end to end, on the real wasm of all three contracts: BTC
// deposit swaps paid out as USDC on Ethereum (BTC mapping contract -> router ->
// EVM bridge), and every way they can fail. Each case ends with the funds either
// delivered (a USDC payout to the recipient, a BTC payout to the SENDING
// address) or held for that sender, never in a Magi balance or on a contract.
//
// Needs BTC_SENDER_REFUND_WASM (the utxo-mapping testnet3 build with
// proveSender; the node names only Bitcoin testnet/mainnet addresses, not
// regtest ones) and EVM_BRIDGE_WASM. State keys are literals so this compiles
// against the pinned (older) BTC module.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/db/vsc/contracts"
	ledger_db "vsc-node/modules/db/vsc/ledger"
	tss_db "vsc-node/modules/db/vsc/tss"
	state_engine "vsc-node/modules/state-processing"

	"github.com/CosmWasm/tinyjson"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/stretchr/testify/require"
	dexcontracts "github.com/vsc-eco/dex-contracts"
	"github.com/vsc-eco/dex-contracts/contracts/types"

	"btc-mapping-contract/contract/mapping"
)

const (
	srBtcId     = "vsc1BpQYDaMwcfdsh9T7DSEHZvdma1XaSXMPPj"
	srBtcPoolId = "vsc1BquGPy8B766YpstdcL5cSF2GkWVVsVxJS3"
	srEthTo     = "0x00000000000000000000000000000000000000e2"
	srDeposit   = int64(200_000) // sats
	srTestnetID = "did:pkh:bip122:000000000933ea01ad0ee984209779ba:"
)

var srNet = &chaincfg.TestNet3Params

type srWorld struct {
	t         *testing.T
	ct        *test_utils.ContractTest
	sender    string
	senderDID string
}

func srTxPaying(t *testing.T, in *wire.OutPoint, amount int64, addr string) *wire.MsgTx {
	t.Helper()
	tx := wire.NewMsgTx(wire.TxVersion)
	op := wire.OutPoint{Index: 0xffffffff}
	if in != nil {
		op = *in
	}
	tx.AddTxIn(&wire.TxIn{PreviousOutPoint: op, SignatureScript: []byte{0x00}, Sequence: wire.MaxTxInSequenceNum})
	a, err := btcutil.DecodeAddress(addr, srNet)
	require.NoError(t, err)
	script, err := txscript.PayToAddrScript(a)
	require.NoError(t, err)
	tx.AddTxOut(&wire.TxOut{Value: amount, PkScript: script})
	return tx
}

func srB64(t *testing.T, tx *wire.MsgTx) string {
	var b bytes.Buffer
	require.NoError(t, tx.SerializeNoWitness(&b))
	return strconv.Quote(base64.StdEncoding.EncodeToString(b.Bytes()))
}

func (w *srWorld) block(tx *wire.MsgTx, height int) {
	var hb bytes.Buffer
	require.NoError(w.t, swapTestBuildBlockHeader(chainhash.Hash{}, tx.TxHash(), time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)).Serialize(&hb))
	w.ct.StateSet(srBtcId, "b-"+strconv.Itoa(height), hb.String())
}

// call runs a contract call and returns its logs, joined.
func (w *srWorld) call(caller, contract, action, payload string) (bool, string) {
	res := w.ct.Call(state_engine.TxVscCallContract{
		Self: *basicSelf(w.t, caller), ContractId: contract, Action: action, Payload: []byte(payload),
		RcLimit: 1_000_000, Intents: []contracts.Intent{}, Caller: caller,
	})
	var logs []string
	for _, out := range res.Logs {
		logs = append(logs, out.Logs...)
	}
	if !res.Success {
		logs = append(logs, "ERROR "+res.Err+" "+res.ErrMsg)
	}
	w.t.Logf("%s.%s by %s: ok=%v\n  %s", contract, action, caller, res.Success, strings.Join(logs, "\n  "))
	return res.Success, strings.Join(logs, "\n")
}

func newSrWorld(t *testing.T) *srWorld {
	t.Helper()
	path := os.Getenv("BTC_SENDER_REFUND_WASM")
	if path == "" {
		t.Skip("BTC_SENDER_REFUND_WASM not set (the utxo-mapping testnet3 build with proveSender)")
	}
	btcWasm, err := os.ReadFile(path)
	require.NoError(t, err)
	ctv := setupUsdcHbdPool(t) // EVM bridge, USDC/HBD pool, router
	ct := &ctv
	w := &srWorld{t: t, ct: ct}
	router := &RouterInfo{ct: ct, id: usdcRouter}
	r := router.initRouterV2(t, usdcOwner)
	require.True(t, r.Success, "init router: %s: %s", r.Err, r.ErrMsg)
	ref := "11155111-1c7d4b196cb0c7b01d743fbc6116a902379c7238"
	ct.StateSet(evmBridgeId, "ts-usdc", ref)
	ct.StateSet(evmBridgeId, "t-"+ref, "usdc|6|1")
	ct.StateSet(evmBridgeId, "pf-usdc", "150000")
	ct.StateSet(evmBridgeId, "chainid", "11155111")
	ct.StateSet(evmBridgeId, "routerid", usdcRouter) // the bridge's swap-back path (registerRouter)

	ct.RegisterContract(srBtcId, usdcOwner, btcWasm)
	ct.RegisterContract(srBtcPoolId, usdcOwner, dexcontracts.DexWasm)
	ct.Deposit(usdcOwner, 50_000_000, ledger_db.Asset("hbd")) // RC for the setup calls
	for _, tk := range []types.RegisterTokenParams{
		{Name: "HBD", TokenInfo: types.TokenInfo{Chain: "HIVE"}},
		{Name: "usdc", TokenInfo: types.TokenInfo{Chain: "ETH", MappingContract: evmBridgeId}},
		{Name: "BTC", TokenInfo: types.TokenInfo{Chain: "BTC", MappingContract: srBtcId}},
	} {
		p, _ := tinyjson.Marshal(tk)
		ok, _ := w.call(usdcOwner, usdcRouter, "register_token", string(p))
		require.True(t, ok, "register %s", tk.Name)
	}
	for _, p := range []types.RegisterPoolParams{
		{Asset0: "hbd", Asset1: "usdc", DexContractId: usdcPoolId},
		{Asset0: "btc", Asset1: "hbd", DexContractId: srBtcPoolId},
	} {
		r = router.registerPool(t, usdcOwner, p)
		require.True(t, r.Success, "register pool: %s: %s", r.Err, r.ErrMsg)
	}
	whitelistPendulum(ct, srBtcPoolId)
	initP, _ := tinyjson.Marshal(&types.InitParams{Asset0: "btc", Asset1: "hbd", FeeBps: 100, Asset0MappingContract: srBtcId, RouterContract: usdcRouter})
	ok, _ := w.call(usdcOwner, srBtcPoolId, "init", string(initP))
	require.True(t, ok, "init btc pool")
	ct.StateSet(srBtcId, "a-"+usdcOwner, formatUintAsBytes(t, 2_00000000))
	ct.StateSet(srBtcId, "q|"+usdcOwner+"|contract:"+srBtcPoolId, formatUintAsBytes(t, 2_00000000))
	ct.Deposit(usdcOwner, 100000_000+rcHeadroomHBD, ledger_db.Asset("hbd"))
	lp, _ := tinyjson.Marshal(types.AddLiquidityParams{Amount0: "100000000", Amount1: "100000000", Recipient: usdcOwner}) // 1 BTC : 100k HBD
	res := ct.Call(state_engine.TxVscCallContract{
		Self: *basicSelf(t, usdcOwner), ContractId: srBtcPoolId, Action: "add_liquidity", Payload: lp, RcLimit: 1_000_000, Caller: usdcOwner,
		Intents: []contracts.Intent{{Type: "transfer.allow", Args: map[string]string{"token": "hbd", "limit": "100000000"}}},
	})
	require.True(t, res.Success, "btc liquidity: %s: %s", res.Err, res.ErrMsg)

	ct.StateSet(srBtcId, "s", string(mapping.MarshalSupply(&mapping.SystemSupply{ActiveSupply: 2_00000000, UserSupply: 2_00000000, BaseFeeRate: 1})))
	ct.StateSet(srBtcId, "h", "102")
	ct.StateSet(srBtcId, "pubkey", swapTestDecodeHex(t, swapTestPrimaryPubKeyHex))
	ct.StateSet(srBtcId, "backupkey", swapTestDecodeHex(t, swapTestBackupPubKeyHex))
	ct.StateSet(srBtcId, "routerid", usdcRouter)
	ct.Tss.Keys.SetKey(tss_db.TssKey{Id: srBtcId + "-main", Status: tss_db.TssKeyActive})

	// Another user's coin in the vault (a plain deposit at 99), so a Bitcoin
	// payout with its fee taken from the amount can select coins.
	seedInstr := "deposit_to=hive:someone-else"
	seedAddr, _, err := mapping.DepositAddress(swapTestPrimaryPubKeyHex, swapTestBackupPubKeyHex, seedInstr, srNet)
	require.NoError(t, err)
	seed := srTxPaying(t, nil, 300_000, seedAddr)
	w.block(seed, 99)
	ok, _ = w.call(usdcOwner, srBtcId, "map", `{"tx_data":{"block_height":99,"raw_tx_b64":`+srB64(t, seed)+`,"merkle_proof_hex":"","tx_index":0},"instructions":[`+strconv.Quote(seedInstr)+`]}`)
	require.True(t, ok, "seed coin")

	pub := append([]byte{0x02}, bytes.Repeat([]byte{0x11}, 32)...)
	a, err := btcutil.NewAddressWitnessPubKeyHash(btcutil.Hash160(pub), srNet)
	require.NoError(t, err)
	w.sender = a.EncodeAddress()
	w.senderDID = srTestnetID + w.sender
	return w
}

// send: the sender's wallet is funded at 90 and deposits to instr's address at
// 100. It returns the deposit and the sender-proof payload.
func (w *srWorld) send(instr string) (*wire.MsgTx, string) {
	addr, _, err := mapping.DepositAddress(swapTestPrimaryPubKeyHex, swapTestBackupPubKeyHex, instr, srNet)
	require.NoError(w.t, err)
	prev := srTxPaying(w.t, nil, srDeposit+10_000, w.sender)
	dep := srTxPaying(w.t, &wire.OutPoint{Hash: prev.TxHash(), Index: 0}, srDeposit, addr)
	w.block(prev, 90)
	w.block(dep, 100)
	return dep, `{"deposit_tx_b64":` + srB64(w.t, dep) + `,"sender":{"block_height":90,"raw_tx_b64":` + srB64(w.t, prev) + `,"merkle_proof_hex":"","tx_index":0}}`
}

func (w *srWorld) mapDeposit(dep *wire.MsgTx, instr string) string {
	ok, logs := w.call(usdcOwner, srBtcId, "map", `{"tx_data":{"block_height":100,"raw_tx_b64":`+srB64(w.t, dep)+`,"merkle_proof_hex":"","tx_index":0},"instructions":[`+strconv.Quote(instr)+`]}`)
	require.True(w.t, ok, "map")
	return logs
}

func srInstr(min string) string {
	return "swap_to=did:pkh:eip155:1:" + srEthTo + "&swap_asset_out=usdc&destination_chain=ETH&min_amount_out=" + min
}

func (w *srWorld) btcBal(acct string) int64 {
	raw := w.ct.StateGet(srBtcId, "a-"+acct)
	if raw == "" {
		return 0
	}
	n := int64(0)
	for _, c := range []byte(raw) {
		n = n<<8 | int64(c)
	}
	return n
}

// safe asserts nothing sits on a Magi balance of the sender or on a contract.
func (w *srWorld) safe() {
	w.t.Helper()
	require.Zero(w.t, w.btcBal(w.senderDID), "the sender's BTC must not be on its Magi balance")
	require.Contains(w.t, []string{"", "0"}, bridgeBalance(w.ct, w.senderDID, "usdc"), "no USDC on the sender's Magi balance")
	require.Zero(w.t, w.btcBal("contract:"+srBtcId), "the BTC contract keeps nothing")
	require.Zero(w.t, w.btcBal("contract:"+usdcRouter), "the router keeps no BTC")
	require.Contains(w.t, []string{"", "0"}, bridgeBalance(w.ct, "contract:"+usdcRouter, "usdc"), "the router keeps no USDC")
	require.Zero(w.t, w.ct.GetBalance("contract:"+usdcRouter, ledger_db.AssetHbd), "the router keeps no HBD")
}

type srQueued struct {
	From, To, Asset string
	RefundTo        string      `json:"refund_to"`
	ReturnAsset     string      `json:"return_asset"`
	ReturnMin       json.Number `json:"return_min"`
}

func (w *srWorld) queued() srQueued {
	var q srQueued
	require.NoError(w.t, json.Unmarshal([]byte(w.ct.StateGet(evmBridgeId, "wq-0")), &q), "no queued payout: %q", w.ct.StateGet(evmBridgeId, "wq-0"))
	return q
}

// heldForSender asserts the BTC is held for the sender's address (refund hold +
// records), and that sendHome pays it to that address on Bitcoin.
func (w *srWorld) heldForSenderAndSentHome(amount int64) {
	w.t.Helper()
	require.Equal(w.t, strconv.FormatInt(amount, 10), w.ct.StateGet(srBtcId, "rfh-"+w.sender), "held for the sending address")
	require.Equal(w.t, amount, w.btcBal("system:btc-refund"))
	w.safe()
	before := w.ct.StateGet(srBtcId, "p")
	w.ct.Deposit("hive:keeper", 1_000_000, ledger_db.Asset("hbd")) // the keeper bot holds RC
	ok, logs := w.call("hive:keeper", srBtcId, "sendHome", strconv.Quote(w.sender))
	require.True(w.t, ok, "sendHome")
	require.Contains(w.t, logs, "sendhome|t="+w.sender)
	require.NotEqual(w.t, before, w.ct.StateGet(srBtcId, "p"), "a Bitcoin payout to the sender is pending")
	w.safe()
}

func TestBtcSenderRefund_EndToEnd(t *testing.T) {
	t.Run("paid out in USDC on Ethereum, owed back to the sender as BTC", func(t *testing.T) {
		w := newSrWorld(t)
		instr := srInstr("1")
		dep, proof := w.send(instr)
		ok, _ := w.call(usdcOwner, srBtcId, "proveSender", proof)
		require.True(t, ok)
		require.Equal(t, w.sender, w.ct.StateGet(srBtcId, "snd-"+dep.TxID()))
		w.mapDeposit(dep, instr)
		q := w.queued()
		require.Equal(t, "contract:"+usdcRouter, q.From)
		require.Equal(t, srEthTo, q.To, "paid at the Ethereum identity's address")
		require.Equal(t, w.senderDID, q.RefundTo, "owed back to the BTC sender")
		require.Equal(t, "btc", q.ReturnAsset)
		if m, _ := strconv.ParseInt(string(q.ReturnMin), 10, 64); m <= 0 {
			t.Fatalf("no floor for the swap back: %q", q.ReturnMin)
		}
		w.safe()

		// The payout comes back (the bridge's owner cancels it here; on chain a
		// revert or a drop takes the same path, run by the bot's confirmSpend):
		// swapped back to BTC and paid on Bitcoin to the sending address. The
		// sender itself has no RC (a Bitcoin identity), so it is never asked to act.
		before := w.ct.StateGet(srBtcId, "p")
		ok, logs := w.call(usdcOwner, evmBridgeId, "cancelQueuedWithdrawal", `{"seq":0}`)
		require.True(t, ok)
		// Held (nothing swaps inside a call anyone could wrap); the bridge's bot
		// swaps it back to BTC and the router pays the sender on Bitcoin.
		require.Contains(t, logs, "payoutreturnheld|id=0|")
		require.NotEqual(t, "", w.ct.StateGet(evmBridgeId, "phr-0"))
		w.safe()
		ok, logs = w.botRetry(0)
		require.True(t, ok, "the bot's swap back")
		require.Contains(t, logs, "payoutreturn|id=0|")
		require.NotEqual(t, before, w.ct.StateGet(srBtcId, "p"), "a Bitcoin payout to the sender is pending")
		require.Equal(t, "", w.ct.StateGet(evmBridgeId, "phr-0"))
		w.safe()
	})

	t.Run("swap back sandwiched: held for the sender, sent once the price is back", func(t *testing.T) {
		w := newSrWorld(t)
		instr := srInstr("1")
		dep, proof := w.send(instr)
		ok, _ := w.call(usdcOwner, srBtcId, "proveSender", proof)
		require.True(t, ok)
		w.mapDeposit(dep, instr)
		// Someone pushes BTC up before the payout comes back.
		w.ct.Deposit(usdcTrader, 30000_000+rcHeadroomHBD, ledger_db.Asset("hbd"))
		push, _ := tinyjson.Marshal(&types.DexInstruction{Type: "swap", Version: "1.0.0", AssetIn: "hbd", AssetOut: "btc", AmountIn: "30000000", Recipient: usdcTrader, MinAmountOut: strPtr("1")})
		res := w.ct.Call(state_engine.TxVscCallContract{
			Self: *basicSelf(t, usdcTrader), ContractId: usdcRouter, Action: "execute", Payload: push, RcLimit: 1_000_000, Caller: usdcTrader,
			Intents: []contracts.Intent{{Type: "transfer.allow", Args: map[string]string{"token": "hbd", "limit": "30000.000"}}},
		})
		require.True(t, res.Success, "push: %s %s", res.Err, res.ErrMsg)
		ok, logs := w.call(usdcOwner, evmBridgeId, "cancelQueuedWithdrawal", `{"seq":0}`)
		require.True(t, ok)
		require.Contains(t, logs, "payoutreturnheld|id=0|")
		require.NotEqual(t, "", w.ct.StateGet(evmBridgeId, "phr-0"))
		w.safe()
		ok, _ = w.call(usdcTrader, evmBridgeId, "releasePayoutReturn", `{"id":0,"min_out":"1"}`)
		require.False(t, ok, "someone else released the sender's return")
		ok, _ = w.call(usdcTrader, evmBridgeId, "retryPayoutReturn", `{"id":0,"min_out":"1"}`)
		require.False(t, ok, "someone other than the bridge's bot ran the swap back")
		ok, _ = w.botRetry(0)
		require.False(t, ok, "the bot's swap back ran below the floor")
		// The sender cannot act (a Bitcoin identity has no RC): only the keeper's
		// retry with the floor brings it home.
		ok, _ = w.call(w.senderDID, evmBridgeId, "releasePayoutReturn", `{"id":0,"min_out":"1"}`)
		require.False(t, ok, "a Bitcoin identity has no RC to sign")
		// The price comes back (the pusher sells): the keeper's retry sends it.
		btcOut := w.btcBal(usdcTrader)
		w.ct.StateSet(srBtcId, "q|"+usdcTrader+"|contract:"+usdcRouter, formatUintAsBytes(t, uint64(btcOut)))
		back, _ := tinyjson.Marshal(&types.DexInstruction{Type: "swap", Version: "1.0.0", AssetIn: "btc", AssetOut: "hbd", AmountIn: strconv.FormatInt(btcOut, 10), Recipient: usdcTrader, MinAmountOut: strPtr("1")})
		res = w.ct.Call(state_engine.TxVscCallContract{
			Self: *basicSelf(t, usdcTrader), ContractId: usdcRouter, Action: "execute", Payload: back, RcLimit: 1_000_000, Caller: usdcTrader,
			Intents: []contracts.Intent{},
		})
		require.True(t, res.Success, "sell back: %s %s", res.Err, res.ErrMsg)
		before := w.ct.StateGet(srBtcId, "p")
		ok, logs = w.botRetry(0)
		require.True(t, ok, "the bot's swap back once the price is back")
		require.Contains(t, logs, "payoutreturn|id=0|")
		require.NotEqual(t, before, w.ct.StateGet(srBtcId, "p"), "paid to the sender on Bitcoin")
		require.Equal(t, "", w.ct.StateGet(evmBridgeId, "phr-0"))
		w.safe()
	})

	t.Run("the swap fails: the BTC goes back to the sender", func(t *testing.T) {
		w := newSrWorld(t)
		instr := srInstr("999999999999999")
		dep, proof := w.send(instr)
		ok, _ := w.call(usdcOwner, srBtcId, "proveSender", proof)
		require.True(t, ok)
		logs := w.mapDeposit(dep, instr)
		require.Contains(t, logs, "deposit-swap failed")
		require.Equal(t, "", w.ct.StateGet(evmBridgeId, "wq-0"), "no payout")
		w.heldForSenderAndSentHome(srDeposit)
	})

	t.Run("mapped without the sender proof: held, then sent home once proven", func(t *testing.T) {
		w := newSrWorld(t)
		instr := srInstr("1")
		dep, proof := w.send(instr)
		logs := w.mapDeposit(dep, instr)
		require.Contains(t, logs, "needs the sender's Bitcoin address")
		require.Equal(t, "", w.ct.StateGet(evmBridgeId, "wq-0"), "no payout without a known sender")
		require.Equal(t, srDeposit, w.btcBal("system:refund-hold"))
		w.safe()
		ok, logs := w.call(usdcOwner, srBtcId, "proveSender", proof)
		require.True(t, ok)
		require.Contains(t, logs, "senderrefund|t="+w.sender)
		require.Zero(t, w.btcBal("system:refund-hold"))
		w.heldForSenderAndSentHome(srDeposit)
	})

	t.Run("the EVM bridge is paused: the BTC goes back to the sender", func(t *testing.T) {
		w := newSrWorld(t)
		instr := srInstr("1")
		dep, proof := w.send(instr)
		ok, _ := w.call(usdcOwner, srBtcId, "proveSender", proof)
		require.True(t, ok)
		w.ct.StateSet(evmBridgeId, "paused", "1")
		logs := w.mapDeposit(dep, instr)
		require.Contains(t, logs, "deposit-swap failed")
		require.Equal(t, "", w.ct.StateGet(evmBridgeId, "wq-0"))
		w.ct.StateSet(evmBridgeId, "paused", "")
		w.heldForSenderAndSentHome(srDeposit)
	})
}

// botRetry runs retryPayoutReturn as the bridge's bot: hive:keeper, registered
// as a relayer (deploy: registerRelayer), holding RC, at min_out 1 (the bridge
// keeps the swap's floor for the first day).
func (w *srWorld) botRetry(id int) (bool, string) {
	w.ct.StateSet(evmBridgeId, "rl-hive:keeper", "1")
	w.ct.Deposit("hive:keeper", 1_000_000, ledger_db.Asset("hbd"))
	return w.call("hive:keeper", evmBridgeId, "retryPayoutReturn", `{"id":`+strconv.Itoa(id)+`,"min_out":"1"}`)
}
