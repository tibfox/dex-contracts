package main

import (
	"math/big"
	"strconv"
	"strings"

	tinyjson "github.com/CosmWasm/tinyjson"

	ce "github.com/vsc-eco/dex-contracts/contracterrors"
	"github.com/vsc-eco/dex-contracts/contracts/types"
	"github.com/vsc-eco/dex-contracts/sdk"
)

func main() {}

// Contract initialization
// Payload: version string (e.g. "1.0.0")
//
//go:wasmexport init
func Init(payload *string) *string {
	// Owner-only: prevents anyone but the deployer from invoking init as a
	// regular transaction. The runtime exposes `init` like any other export.
	env := sdk.GetEnv()
	ownerPtr := sdk.GetEnvKey("contract.owner")
	if ownerPtr == nil || env.Caller.String() != *ownerPtr {
		ce.CustomAbort(ce.NewContractError(ce.ErrNoPermission, "owner only"))
	}

	// Idempotency guard: keyVersion is always set on successful init, so a
	// non-empty value means the router is already initialized.
	if getStr(keyVersion) != "" {
		ce.CustomAbort(ce.NewContractError(ce.ErrInitialization, "router already initialized"))
	}

	if payload == nil || *payload == "" {
		setStr(keyVersion, "1.0.0")
	} else {
		setStr(keyVersion, *payload)
	}
	return nil
}

// Register a token in the token registry. MUST be called before any pool can use the token.
// Payload: JSON {"name": "HBD", "chain": "HIVE"} or {"name": "BTC", "chain": "BTC"}
// Chains: HIVE (for HIVE/HBD native), MAGI (for MAGI native tokens), BTC, ETH, etc. (for mapped assets)
//
//go:wasmexport register_token
func RegisterToken(payload *string) *string {
	env := sdk.GetEnv()
	ownerPtr := sdk.GetEnvKey("contract.owner")
	if ownerPtr == nil || env.Caller.String() != *ownerPtr {
		ce.CustomAbort(ce.NewContractError(ce.ErrNoPermission, "action must be performed by the contract owner"))
	}
	if payload == nil {
		ce.CustomAbort(ce.NewContractError(ce.ErrInput, "payload required"))
	}

	var params types.RegisterTokenParams
	if err := tinyjson.Unmarshal([]byte(*payload), &params); err != nil {
		ce.CustomAbort(ce.WrapContractError(ce.ErrJson, err, "invalid payload"))
	}

	name := params.Name
	if name == "" {
		ce.CustomAbort(ce.NewContractError(ce.ErrInput, "name required"))
	}
	if params.Chain == "" {
		ce.CustomAbort(ce.NewContractError(ce.ErrInput, "chain required"))
	}

	// Normalize chain to uppercase and name to lowercase for storage key consistency
	params.Chain = strings.ToUpper(params.Chain)
	name = strings.ToLower(name)

	// Validate chain - support HIVE, MAGI, and common mapped chains
	validChains := map[string]bool{
		chainHIVE: true, chainMAGI: true,
		"BTC": true, "ETH": true, "SOL": true, "SUI": true,
		"LTC": true, "DASH": true, "DOGE": true, "BCH": true,
	}
	if !validChains[params.Chain] {
		ce.CustomAbort(ce.NewContractError(ce.ErrInput, "unsupported chain: "+params.Chain))
	}

	if params.MappingContract != "" && sdk.VerifyAddress("contract:"+params.MappingContract) != "contract" {
		ce.CustomAbort(ce.NewContractError(ce.ErrInput, "mapping_contract must be a valid contract ID"))
	}

	if isAssetRegistered(name) {
		ce.CustomAbort(ce.NewContractError(ce.ErrInput, "asset already registered"))
	}

	// Resolve decimals: query mapping contract for mapped assets, use payload value for native
	if params.MappingContract != "" {
		infoResult := sdk.ContractCall(params.MappingContract, "getInfo", "", nil)
		if infoResult == nil {
			ce.CustomAbort(ce.NewContractError(ce.ErrTransaction, "failed to query getInfo on mapping contract"))
		}
		var contractInfo types.MappingContractInfoReturn
		if err := tinyjson.Unmarshal([]byte(*infoResult), &contractInfo); err != nil {
			ce.CustomAbort(
				ce.WrapContractError(ce.ErrJson, err, "failed to parse getInfo response from mapping contract"),
			)
		}
		if strings.ToLower(contractInfo.Symbol) != name {
			ce.CustomAbort(ce.NewContractError(ce.ErrInput,
				"asset name '"+params.Name+"' does not match mapping contract symbol '"+contractInfo.Symbol+"'"))
		}
		if contractInfo.Decimals == "" {
			ce.CustomAbort(ce.NewContractError(ce.ErrInput, "mapping contract getInfo did not return decimals"))
		}
		decimals, parseErr := strconv.Atoi(contractInfo.Decimals)
		if parseErr != nil {
			ce.CustomAbort(
				ce.NewContractError(ce.ErrInput, "invalid decimals from mapping contract: "+contractInfo.Decimals),
			)
		}
		params.Decimals = decimals
	} else {
		// Native assets (HIVE, HBD) always have 3 decimals
		params.Decimals = 3
	}

	info, err := tinyjson.Marshal(params.TokenInfo)
	if err != nil {
		ce.CustomAbort(ce.NewContractError(ce.ErrJson, "error marshaling token info"))
	}
	setStr(assetKey(name), string(info))
	updateChainsList(params.Chain)
	updateTokensList(name)
	return nil
}

// Register a DEX contract for a pool. Both assets MUST be registered via register_token first.
// Payload: JSON {"asset0": "HBD", "asset1": "HIVE", "dex_contract_id": "dex-hbd-hive-123"}
//
//go:wasmexport register_pool
func RegisterPool(payload *string) *string {
	env := sdk.GetEnv()
	ownerPtr := sdk.GetEnvKey("contract.owner")
	if ownerPtr == nil || env.Caller.String() != *ownerPtr {
		ce.CustomAbort(ce.NewContractError(ce.ErrNoPermission, "action must be performed by the contract owner"))
	}
	if payload == nil {
		ce.CustomAbort(ce.NewContractError(ce.ErrInput, "payload required"))
	}

	var params types.RegisterPoolParams
	if err := tinyjson.Unmarshal([]byte(*payload), &params); err != nil {
		ce.CustomAbort(ce.WrapContractError(ce.ErrJson, err, "invalid payload"))
	}

	// Normalize asset names to lowercase
	params.Asset0 = strings.ToLower(params.Asset0)
	params.Asset1 = strings.ToLower(params.Asset1)

	// Validate assets are different
	if params.Asset0 == params.Asset1 {
		ce.CustomAbort(ce.NewContractError(ce.ErrInput, "assets must be different"))
	}

	// Enforce: both assets must be registered before pool can use them
	if !isAssetRegistered(params.Asset0) {
		ce.CustomAbort(
			ce.NewContractError(
				ce.ErrInitialization,
				"asset "+params.Asset0+" not registered - call register_token first",
			),
		)
	}
	if !isAssetRegistered(params.Asset1) {
		ce.CustomAbort(
			ce.NewContractError(
				ce.ErrInitialization,
				"asset "+params.Asset1+" not registered - call register_token first",
			),
		)
	}

	// Normalize asset order (alphabetical)
	if params.Asset0 > params.Asset1 {
		params.Asset0, params.Asset1 = params.Asset1, params.Asset0
	}

	if sdk.VerifyAddress("contract:"+params.DexContractId) != "contract" {
		ce.CustomAbort(ce.NewContractError(ce.ErrInput, "dex_contract_id must be a valid contract ID"))
	}

	// Store pool mapping (reject if pool already exists)
	poolKey := poolKeyForAssets(params.Asset0, params.Asset1)
	if existing := getStr(poolKey); existing != "" {
		ce.CustomAbort(ce.NewContractError(ce.ErrInput, "pool already registered for "+params.Asset0+"-"+params.Asset1))
	}
	setStr(poolKey, params.DexContractId)

	sdk.Log("reg_pool|a0=" + params.Asset0 + "|a1=" + params.Asset1 + "|pool=" + params.DexContractId)

	return nil
}

// Execute swap through router (2-step routing)
// Payload: JSON instruction as defined in schema
// {"type": "swap", "version": "1.0.0", "asset_in": "BTC", "asset_out": "HIVE", "amount_in": 100000, "min_amount_out": 90000, "recipient": "user123"}
//
//go:wasmexport execute
func Execute(payload *string) *string {
	if payload == nil {
		ce.CustomAbort(ce.NewContractError(ce.ErrInput, "payload required"))
	}

	// Auth: either the sender signed with active auth (direct user call),
	// or the caller is a contract (swap-via-map from mapping contract).
	env := sdk.GetEnv()
	callerIsContract := sdk.VerifyAddress(env.Caller.String()) == "contract"
	senderHasAuth := false
	for _, auth := range env.Sender.RequiredAuths {
		if auth == env.Sender.Address {
			senderHasAuth = true
			break
		}
	}
	if !callerIsContract && !senderHasAuth {
		ce.CustomAbort(ce.NewContractError(ce.ErrNoPermission, "active auth required or must be called by a contract"))
	}

	var instruction types.DexInstruction
	if err := tinyjson.Unmarshal([]byte(*payload), &instruction); err != nil {
		ce.CustomAbort(ce.WrapContractError(ce.ErrJson, err, "invalid payload"))
	}

	// Common validation
	if instruction.Type == "" || instruction.Version == "" || instruction.Recipient == "" {
		ce.CustomAbort(ce.NewContractError(ce.ErrInput, "type, version, and recipient are required"))
	}

	// Normalize destination chain
	instruction.DestinationChain = strings.ToUpper(instruction.DestinationChain)

	// When settling on an external chain, Recipient is an address on that chain
	// (e.g. a Hive username or BTC address) and cannot be validated as a Magi address.
	// Only validate as Magi address when there is no cross-chain destination.
	if instruction.DestinationChain == "" || instruction.DestinationChain == chainMAGI {
		if sdk.VerifyAddress(instruction.Recipient) == "unknown" {
			ce.CustomAbort(ce.NewContractError(ce.ErrInput, "recipient address ["+instruction.Recipient+"] invalid"))
		}
		instruction.DestinationChain = "" // normalize MAGI to empty (default)
	}

	switch instruction.Type {
	case "swap":
		// Swap uses directional asset_in/asset_out
		if instruction.AssetIn == "" || instruction.AssetOut == "" {
			ce.CustomAbort(ce.NewContractError(ce.ErrInput, "swap requires asset_in and asset_out"))
		}
		return executeSwap(instruction)

	case "deposit":
		// Deposit uses pool-ordered asset0/asset1
		if instruction.Asset0 == "" || instruction.Asset1 == "" {
			ce.CustomAbort(ce.NewContractError(ce.ErrInput, "deposit requires asset0 and asset1"))
		}
		if instruction.Amount0 == "" || instruction.Amount1 == "" {
			ce.CustomAbort(ce.NewContractError(ce.ErrInput, "deposit requires amount0 and amount1"))
		}
		// Normalize to lowercase and enforce alphabetical ordering
		instruction.Asset0 = strings.ToLower(instruction.Asset0)
		instruction.Asset1 = strings.ToLower(instruction.Asset1)
		if instruction.Asset0 > instruction.Asset1 {
			instruction.Asset0, instruction.Asset1 = instruction.Asset1, instruction.Asset0
			instruction.Amount0, instruction.Amount1 = instruction.Amount1, instruction.Amount0
		}
		return executeDeposit(instruction)

	case "withdrawal":
		// Withdrawal uses pool-ordered asset0/asset1
		if instruction.Asset0 == "" || instruction.Asset1 == "" {
			ce.CustomAbort(ce.NewContractError(ce.ErrInput, "withdrawal requires asset0 and asset1"))
		}
		if instruction.LpAmount == "" {
			ce.CustomAbort(ce.NewContractError(ce.ErrInput, "withdrawal requires lp_amount"))
		}
		instruction.Asset0 = strings.ToLower(instruction.Asset0)
		instruction.Asset1 = strings.ToLower(instruction.Asset1)
		if instruction.Asset0 > instruction.Asset1 {
			instruction.Asset0, instruction.Asset1 = instruction.Asset1, instruction.Asset0
		}
		return executeWithdrawal(instruction)

	default:
		ce.CustomAbort(ce.NewContractError(ce.ErrInput, "unknown instruction type: "+instruction.Type))
		return nil
	}
}

// Execute swap operation with 2-step routing
func executeSwap(instruction types.DexInstruction) *string {
	// Normalize once — all downstream functions expect lowercase
	instruction.AssetIn = strings.ToLower(instruction.AssetIn)
	instruction.AssetOut = strings.ToLower(instruction.AssetOut)

	// Reject same-asset swaps: without this guard they fall through to the
	// two-hop path and get charged double fees for a no-op swap.
	if instruction.AssetIn == instruction.AssetOut {
		ce.CustomAbort(
			ce.NewContractError(ce.ErrInput, "asset_in and asset_out must differ"),
		)
		return nil
	}

	// Try direct pool first
	directPoolId := findPool(instruction.AssetIn, instruction.AssetOut)
	if directPoolId != "" {
		return executeDirectSwap(directPoolId, instruction)
	}

	// Try two-hop swap via HBD
	hbd := sdk.AssetHbd.String()
	if instruction.AssetIn != hbd && instruction.AssetOut != hbd {
		return executeTwoHopSwap(instruction)
	}

	ce.CustomAbort(
		ce.NewContractError(ce.ErrTransaction, "no suitable pool found"),
	)
	return nil
}

// Find pool by assets
func findPool(assetA, assetB string) string {
	// Normalize asset order
	astA := strings.ToLower(assetA)
	astB := strings.ToLower(assetB)
	if astA > astB {
		astA, astB = astB, astA
	}
	poolKey := poolKeyForAssets(astA, astB)
	poolVal := getStr(poolKey)
	return poolVal
}

// Execute direct swap within a single DEX pool
func executeDirectSwap(dexContractId string, instruction types.DexInstruction) *string {
	env := sdk.GetEnv()
	userAddr := env.Caller.String()
	contractAccId := "contract:" + env.ContractId

	// Pre-fund input asset into the pool (mapped via transferFrom, native via HiveDraw+HiveTransfer).
	err := preFundAsset(instruction.AssetIn, instruction.AmountIn, dexContractId, &env)
	if err != nil {
		ce.CustomAbort(ce.Prepend(err, "error pre-funding asset to dex"))
	}

	// When settling on an external chain, output goes to the router first
	// so it can bridge the tokens out.
	swapRecipient := instruction.Recipient
	if instruction.DestinationChain != "" {
		swapRecipient = contractAccId
	}

	swapParams := types.SwapParams{
		AssetIn:      instruction.AssetIn,
		AmountIn:     instruction.AmountIn,
		AssetOut:     instruction.AssetOut,
		MinAmountOut: instruction.MinAmountOut,
		To:           swapRecipient,
		From:         userAddr,
		Beneficiary:  instruction.Beneficiary,
		RefBps:       instruction.RefBps,
		PreDeposited: true,
	}

	swapPayload, err := tinyjson.Marshal(&swapParams)
	if err != nil {
		ce.CustomAbort(
			ce.WrapContractError(ce.ErrJson, err, "failed to marshal swap params"),
		)
	}

	result := sdk.ContractCall(dexContractId, "swap", string(swapPayload), &sdk.ContractCallOptions{})
	if result == nil {
		ce.CustomAbort(
			ce.NewContractError(ce.ErrTransaction, "unknown swap failure"),
		)
	}

	// Bridge output to external chain if requested
	if instruction.DestinationChain != "" {
		var swapResult types.SwapResult
		if err := tinyjson.Unmarshal([]byte(*result), &swapResult); err != nil {
			ce.CustomAbort(ce.WrapContractError(ce.ErrJson, err, "error unmarshalling swap result"))
		}
		amountOut, ok := new(big.Int).SetString(swapResult.AmountOut, 10)
		if !ok || amountOut.Sign() <= 0 {
			ce.CustomAbort(ce.NewContractError(ce.ErrTransaction, "swap returned invalid amount out"))
		}
		settleToChain(instruction.AssetOut, amountOut, instruction.Recipient, instruction.DestinationChain, instruction.MaxFee)
	}

	return result
}

// Execute two-hop swap via HBD using ERC-20 allowances for mapped assets
// and protocol-level intents for native assets (HBD between hops).
func executeTwoHopSwap(instruction types.DexInstruction) *string {
	pool1Id := findPool(instruction.AssetIn, sdk.AssetHbd.String())
	if pool1Id == "" {
		ce.CustomAbort(ce.NewContractError(ce.ErrInitialization, "no pool found for first hop"))
	}

	pool2Id := findPool(sdk.AssetHbd.String(), instruction.AssetOut)
	if pool2Id == "" {
		ce.CustomAbort(ce.NewContractError(ce.ErrInitialization, "no pool found for second hop"))
	}

	var mEnv types.MaybeEnv
	env := mEnv.UseEnv()
	contractAccId := "contract:" + env.ContractId
	userAddr := env.Caller.String()

	// --- First hop: AssetIn → HBD ---
	// Pre-fund input asset into pool1 (mapped via transferFrom, native via HiveDraw+HiveTransfer).
	err := preFundAsset(instruction.AssetIn, instruction.AmountIn, pool1Id, env)
	if err != nil {
		ce.CustomAbort(ce.Prepend(err, "error pre-funding asset to dex"))
	}

	// First hop slippage: users can optionally set "min_intermediate" in metadata.
	// Even without it, the second hop's MinAmountOut provides atomic protection —
	// if first hop gets sandwiched, second hop produces less output and reverts.
	var firstHopMinOut *string
	if instruction.Metadata != nil {
		if v, ok := instruction.Metadata["min_intermediate"]; ok {
			firstHopMinOut = &v
		}
	}

	firstSwapParams := types.SwapParams{
		AssetIn:      instruction.AssetIn,
		AmountIn:     instruction.AmountIn,
		AssetOut:     sdk.AssetHbd.String(),
		From:         userAddr,
		To:           contractAccId, // Intermediate HBD goes to Router
		MinAmountOut: firstHopMinOut,
		PreDeposited: true,
	}

	firstSwapPayload, err := tinyjson.Marshal(&firstSwapParams)
	if err != nil {
		ce.CustomAbort(ce.WrapContractError(ce.ErrJson, err, "error marshalling first hop params"))
	}

	firstResult := sdk.ContractCall(pool1Id, "swap", string(firstSwapPayload), &sdk.ContractCallOptions{})
	if firstResult == nil {
		ce.CustomAbort(ce.NewContractError(ce.ErrTransaction, "first hop returned no result"))
	}

	var swapResult1 types.SwapResult
	if err := tinyjson.Unmarshal([]byte(*firstResult), &swapResult1); err != nil {
		ce.CustomAbort(ce.WrapContractError(ce.ErrJson, err, "error unmarshalling first hop result"))
	}
	res1AmountOut, ok := new(big.Int).SetString(swapResult1.AmountOut, 10)
	if !ok {
		ce.CustomAbort(ce.NewContractError(ce.ErrTransaction, "first hop returned non-integer amount out"))
	}
	if res1AmountOut.Sign() <= 0 {
		ce.CustomAbort(ce.NewContractError(ce.ErrTransaction, "first hop returned zero or negative amount"))
	}

	// --- Second hop: HBD → AssetOut ---
	// Router received intermediate HBD from first pool. Transfer it to pool2.
	sdk.HiveTransfer(sdk.Address("contract:"+pool2Id), res1AmountOut, sdk.AssetHbd)

	// When settling on an external chain, output goes to the router first
	// so it can bridge the tokens out.
	secondHopRecipient := instruction.Recipient
	if instruction.DestinationChain != "" {
		secondHopRecipient = contractAccId
	}

	secondSwapParams := types.SwapParams{
		AssetIn:      sdk.AssetHbd.String(),
		AmountIn:     res1AmountOut.String(),
		AssetOut:     instruction.AssetOut,
		From:         contractAccId,
		To:           secondHopRecipient,
		MinAmountOut: instruction.MinAmountOut,
		Beneficiary:  instruction.Beneficiary,
		RefBps:       instruction.RefBps,
		PreDeposited: true,
	}

	secondSwapPayload, err := tinyjson.Marshal(&secondSwapParams)
	if err != nil {
		ce.CustomAbort(ce.WrapContractError(ce.ErrJson, err, "error marshalling second hop params"))
	}

	secondResult := sdk.ContractCall(pool2Id, "swap", string(secondSwapPayload), &sdk.ContractCallOptions{})
	if secondResult == nil {
		ce.CustomAbort(ce.NewContractError(ce.ErrTransaction, "second hop returned no result"))
	}

	// Bridge output to external chain if requested
	if instruction.DestinationChain != "" {
		var swapResult2 types.SwapResult
		if err := tinyjson.Unmarshal([]byte(*secondResult), &swapResult2); err != nil {
			ce.CustomAbort(ce.WrapContractError(ce.ErrJson, err, "error unmarshalling second hop result"))
		}
		amountOut, ok := new(big.Int).SetString(swapResult2.AmountOut, 10)
		if !ok || amountOut.Sign() <= 0 {
			ce.CustomAbort(ce.NewContractError(ce.ErrTransaction, "second hop returned invalid amount out"))
		}
		settleToChain(instruction.AssetOut, amountOut, instruction.Recipient, instruction.DestinationChain, instruction.MaxFee)
	}

	return secondResult
}

// settleToChain bridges swap output to an external chain.
// For HIVE/HBD: uses sdk.HiveWithdraw to send to a Hive account.
// For mapped assets (BTC, ETH, etc.): calls "unmap" on the mapping contract.
func settleToChain(asset string, amount *big.Int, toAddress string, chain string, maxFee *int64) {
	assetLower := strings.ToLower(asset)

	// review7 M4: the destination chain was ignored — settlement routed purely
	// by asset, so destination_chain:"BTC" with a HIVE asset/recipient still
	// HiveWithdrew to Hive (and vice versa), sending funds somewhere the caller
	// did not intend. If a chain is specified it must match the asset's
	// registered chain.
	if chain != "" {
		assetChain := getAssetChain(assetLower)
		if assetChain != "" && !strings.EqualFold(chain, assetChain) {
			ce.CustomAbort(ce.NewContractError(ce.ErrInput,
				"destination chain "+chain+" does not match asset "+asset+" (registered chain "+assetChain+")"))
		}
	}

	if assetLower == "hive" || assetLower == "hbd" {
		sdk.HiveWithdraw(sdk.Address(toAddress), amount, sdk.Asset(assetLower))
		return
	}

	// Mapped asset: call "unmap" on the mapping contract to send to native chain.
	// The router owns the tokens (swap output was directed here), so no "from" needed.
	mappingContract := getMappingContract(assetLower)
	if mappingContract == "" {
		ce.CustomAbort(
			ce.NewContractError(ce.ErrTransaction, "no mapping contract found for asset: "+asset),
		)
	}

	params := types.UnmapParams{
		Amount:    amount.String(),
		To:        toAddress,
		DeductFee: true,
		MaxFee:    maxFee,
	}
	payload, err := tinyjson.Marshal(&params)
	if err != nil {
		ce.CustomAbort(ce.WrapContractError(ce.ErrJson, err, "failed to marshal unmap params"))
	}
	result := sdk.ContractCall(mappingContract, "unmap", string(payload), &sdk.ContractCallOptions{})
	if result == nil {
		ce.CustomAbort(
			ce.NewContractError(ce.ErrTransaction, "unmap failed for "+asset),
		)
	}
}

// preFundAsset pre-funds an asset into the target pool before calling the pool contract.
// For mapped assets: calls transferFrom on the mapping contract (user → pool).
// For native assets: draws from user into router, then transfers to pool.
// Returns true (asset is pre-deposited in the pool, pool should skip DrawAssetFrom).
func preFundAsset(asset string, amount string, toPool string, env *sdk.Env) error {
	mappingContract := getMappingContract(strings.ToLower(asset))
	if mappingContract != "" {
		// Mapped asset: transfer via ERC-20 allowance (user → pool).
		input := types.MappingContractInput{
			Amount: amount,
			To:     "contract:" + toPool,
			From:   env.Caller.String(),
		}
		payload, err := tinyjson.Marshal(input)
		if err != nil {
			return ce.WrapContractError(ce.ErrJson, err, "failed to marshal transferFrom payload")
		}
		result := sdk.ContractCall(mappingContract, "transferFrom", string(payload), &sdk.ContractCallOptions{})
		if result == nil {
			return ce.NewContractError(ce.ErrTransaction, "transferFrom on mapping contract returned no result")
		}
		return nil
	}

	// Native asset: draw from user into router, then transfer to pool.
	// Router's Caller is the user, so HiveDraw pulls from the user's balance
	// up to the transfer.allow intent limit set in the original transaction.
	amt, ok := new(big.Int).SetString(amount, 10)
	if !ok || amt.Sign() <= 0 {
		return ce.NewContractError(ce.ErrInput, "amount must be positive")
	}
	sdk.HiveDraw(amt, sdk.Asset(strings.ToLower(asset)))
	sdk.HiveTransfer(sdk.Address("contract:"+toPool), amt, sdk.Asset(strings.ToLower(asset)))
	return nil
}

// Execute deposit (add liquidity)
// Asset0/Asset1 are already normalized to alphabetical order by Execute().
func executeDeposit(instruction types.DexInstruction) *string {
	poolId := findPool(instruction.Asset0, instruction.Asset1)
	if poolId == "" {
		ce.CustomAbort(ce.NewContractError(ce.ErrTransaction, "pool not found"))
	}

	amt0, ok := new(big.Int).SetString(instruction.Amount0, 10)
	if !ok || amt0.Sign() <= 0 {
		ce.CustomAbort(ce.NewContractError(ce.ErrInput, "amount0 must be a positive number"))
	}
	amt1, ok := new(big.Int).SetString(instruction.Amount1, 10)
	if !ok || amt1.Sign() <= 0 {
		ce.CustomAbort(ce.NewContractError(ce.ErrInput, "amount1 must be a positive number"))
	}

	// Pre-fund both assets into the pool.
	// Asset0/Asset1 match pool ordering, so Amount0 funds pool.asset0 and Amount1 funds pool.asset1.
	env := sdk.GetEnv()
	err := preFundAsset(instruction.Asset0, instruction.Amount0, poolId, &env)
	if err != nil {
		ce.CustomAbort(ce.Prepend(err, "error pre-funding asset0 to dex"))
	}
	err = preFundAsset(instruction.Asset1, instruction.Amount1, poolId, &env)
	if err != nil {
		ce.CustomAbort(ce.Prepend(err, "error pre-funding asset1 to dex"))
	}

	addLiqParams := types.AddLiquidityParams{
		Amount0:       instruction.Amount0,
		Amount1:       instruction.Amount1,
		Recipient:     instruction.Recipient,
		PreDeposited0: true,
		PreDeposited1: true,
	}
	addLiqPayload, err := tinyjson.Marshal(&addLiqParams)
	if err != nil {
		ce.CustomAbort(ce.NewContractError(ce.ErrJson, "failed to marshal add liquidity params"))
	}

	result := sdk.ContractCall(poolId, "add_liquidity", string(addLiqPayload), &sdk.ContractCallOptions{})
	if result == nil {
		ce.CustomAbort(ce.NewContractError(ce.ErrTransaction, "add liquidity returned no result"))
	}

	return result
}

// Execute withdrawal (remove liquidity)
func executeWithdrawal(instruction types.DexInstruction) *string {
	// Only the LP owner can withdraw their own liquidity via the router.
	env := sdk.GetEnv()
	if instruction.Recipient != env.Caller.String() {
		ce.CustomAbort(ce.NewContractError(ce.ErrNoPermission, "can only withdraw your own liquidity"))
	}

	// Asset0/Asset1 already normalized to alphabetical order by Execute().
	poolId := findPool(instruction.Asset0, instruction.Asset1)
	if poolId == "" {
		ce.CustomAbort(ce.NewContractError(ce.ErrInitialization, "pool not found"))
	}

	removeLiqParams := types.RemoveLiquidityParams{
		LpAmount:  instruction.LpAmount,
		Recipient: instruction.Recipient,
	}

	removeLiqPayload, err := tinyjson.Marshal(&removeLiqParams)
	if err != nil {
		ce.CustomAbort(ce.NewContractError(ce.ErrJson, "failed to marshal remove liquidity params"))
	}

	// Call DEX contract's remove_liquidity method (clone intents for dex to spend)
	result := sdk.ContractCall(poolId, "remove_liquidity", string(removeLiqPayload), &sdk.ContractCallOptions{})
	if result == nil {
		ce.CustomAbort(ce.NewContractError(ce.ErrTransaction, "remove liquidity failed"))
	}

	return result
}

// Query pool information via router
// Payload: JSON with assets {"asset0": "HBD", "asset1": "HIVE"}
//
//go:wasmexport get_pool
func GetPool(payload *string) *string {
	if payload == nil {
		ce.CustomAbort(ce.NewContractError(ce.ErrInput, "payload required"))
	}

	var params types.GetPoolParams
	if err := tinyjson.Unmarshal([]byte(*payload), &params); err != nil {
		ce.CustomAbort(ce.NewContractError(ce.ErrJson, "invalid payload"))
	}

	// Find pool
	poolId := findPool(params.Asset0, params.Asset1)
	if poolId == "" {
		ce.CustomAbort(ce.NewContractError(ce.ErrTransaction, "pool not found"))
	}

	// Query DEX contract for pool info
	result := sdk.ContractCall(poolId, "get_pool", "", nil)
	if result == nil {
		ce.CustomAbort(ce.NewContractError(ce.ErrTransaction, "failed to query pool"))
	}

	return result
}

// Query token information from the registry
// Payload: token name string (e.g. "BTC", "HBD")
//
//go:wasmexport get_token
func GetToken(payload *string) *string {
	if payload == nil {
		ce.CustomAbort(ce.NewContractError(ce.ErrInput, "token name required"))
	}
	name := strings.ToLower(strings.Trim(*payload, "\" "))
	if name == "" {
		ce.CustomAbort(ce.NewContractError(ce.ErrInput, "token name required"))
	}
	infoStr := getStr(assetKey(name))
	if infoStr == "" {
		ce.CustomAbort(ce.NewContractError(ce.ErrInput, "token not registered: "+name))
	}
	return &infoStr
}

// Query schema information - returns supported chains and all registered tokens
// Payload: empty
//
//go:wasmexport get_schema
func GetSchema(payload *string) *string {
	chainsStr := getStr(keyChainsList)
	var chains []string
	if chainsStr != "" {
		chains = splitChains(chainsStr)
	}

	// Load all registered tokens
	var tokens []types.RegisterTokenParams
	tokensStr := getStr(keyTokensList)
	if tokensStr != "" {
		for name := range strings.SplitSeq(tokensStr, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			infoStr := getStr(assetKey(name))
			if infoStr == "" {
				continue
			}
			var info types.TokenInfo
			if err := tinyjson.Unmarshal([]byte(infoStr), &info); err != nil {
				continue
			}
			tokens = append(tokens, types.RegisterTokenParams{Name: name, TokenInfo: info})
		}
	}

	schema := types.SchemaReturn{
		SupportedChains:     chains,
		ReturnAddressChains: chains,
		Tokens:              tokens,
		Note:                "Schema dynamically generated from registered tokens and pools.",
	}

	schemaBytes, err := tinyjson.Marshal(&schema)
	if err != nil {
		ce.CustomAbort(ce.NewContractError(ce.ErrJson, "failed to marshal schema"))
	}

	result := string(schemaBytes)
	return &result
}
