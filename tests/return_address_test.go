package tests

// A swap settled to Ethereum names who gets it back if the L1 withdrawal fails.
// By default that is the router's caller, right for a user calling directly. A
// contract calling for someone else (a mapping contract running a deposit swap)
// names the sender in return_address {chain: "MAGI", address}; otherwise the
// refund would land on the calling contract, where nobody can move it.

import (
	"encoding/json"
	"strconv"
	"testing"

	"vsc-node/modules/db/vsc/contracts"
	ledger_db "vsc-node/modules/db/vsc/ledger"
	state_engine "vsc-node/modules/state-processing"

	"github.com/CosmWasm/tinyjson"
	"github.com/stretchr/testify/require"

	"github.com/vsc-eco/dex-contracts/contracts/types"
)

func TestReturnAddress_NamesTheSettlementRefund(t *testing.T) {
	const l1To = "0x00000000000000000000000000000000000000e2"
	const sender = "hive:the-real-sender"
	cases := []struct {
		name     string
		ra       *types.ReturnAddress
		ok       bool
		refundTo string
	}{
		{"absent: the caller", nil, true, usdcTrader},
		{"MAGI: the named sender", &types.ReturnAddress{Chain: "MAGI", Address: sender}, true, sender},
		{"no chain: the named sender", &types.ReturnAddress{Address: sender}, true, sender},
		{"another chain: the caller", &types.ReturnAddress{Chain: "ETH", Address: "0x00000000000000000000000000000000000000e3"}, true, usdcTrader},
		{"not a user account: refused", &types.ReturnAddress{Chain: "MAGI", Address: "contract:" + usdcRouter}, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ct := setupUsdcHbdPool(t)
			router := &RouterInfo{ct: &ct, id: usdcRouter}
			r := router.initRouterV2(t, usdcOwner)
			require.True(t, r.Success, "init router: %s: %s", r.Err, r.ErrMsg)
			ref := "11155111-1c7d4b196cb0c7b01d743fbc6116a902379c7238"
			ct.StateSet(evmBridgeId, "ts-usdc", ref)
			ct.StateSet(evmBridgeId, "t-"+ref, "usdc|6|1")
			ct.StateSet(evmBridgeId, "pf-usdc", "150000")
			ct.StateSet(evmBridgeId, "chainid", "11155111")
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

			ct.Deposit(usdcTrader, 1_000+rcHeadroomHBD, ledger_db.Asset("hbd"))
			hbdBefore := ct.GetBalance(usdcTrader, ledger_db.Asset("hbd"))
			maxFee := int64(3_000_000_000_000_000)
			r = router.execute(t, usdcTrader, &types.DexInstruction{
				Type: "swap", Version: "1.0.0", AssetIn: "hbd", AssetOut: "usdc", AmountIn: "1000",
				Recipient: l1To, DestinationChain: "ETH", MaxFee: &maxFee, ReturnAddress: tc.ra,
			}, []contracts.Intent{{Type: "transfer.allow", Args: map[string]string{"token": "hbd", "limit": "1000"}}})
			if !tc.ok {
				require.False(t, r.Success, "a return_address that is not a user account must be refused")
				require.Equal(t, hbdBefore, ct.GetBalance(usdcTrader, ledger_db.Asset("hbd")), "a refused swap moves nothing")
				return
			}
			require.True(t, r.Success, "swap: %s: %s", r.Err, r.ErrMsg)
			var q struct {
				From     string
				Amount   json.Number
				RefundTo string `json:"refund_to"`
			}
			require.NoError(t, json.Unmarshal([]byte(ct.StateGet(evmBridgeId, "wq-0")), &q), ct.StateGet(evmBridgeId, "wq-0"))
			require.Equal(t, "contract:"+usdcRouter, q.From)
			require.Equal(t, tc.refundTo, q.RefundTo)
			amt, _ := strconv.ParseInt(string(q.Amount), 10, 64)
			require.Greater(t, amt, int64(0))
			require.Contains(t, []string{"", "0"}, bridgeBalance(&ct, "contract:"+usdcRouter, "usdc"), "the router holds nothing")
		})
	}
}
