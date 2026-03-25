package main

import (
	"math/big"
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
	if env.Caller.String() != *sdk.GetEnvKey("contract.owner") {
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

	if isAssetRegistered(name) {
		ce.CustomAbort(ce.NewContractError(ce.ErrInput, "asset already registered"))
	}

	info, err := tinyjson.Marshal(params.TokenInfo)
	if err != nil {
		ce.CustomAbort(ce.NewContractError(ce.ErrJson, "error marshaling token info"))
	}
	setStr(assetKey(name), string(info))
	updateChainsList(params.Chain)
	return nil
}

// Register a DEX contract for a pool. Both assets MUST be registered via register_token first.
// Payload: JSON {"asset0": "HBD", "asset1": "HIVE", "dex_contract_id": "dex-hbd-hive-123"}
//
//go:wasmexport register_pool
func RegisterPool(payload *string) *string {
	env := sdk.GetEnv()
	if env.Caller.String() != *sdk.GetEnvKey("contract.owner") {
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
	callerIsContract := env.Caller.Domain() == sdk.AddressDomainContract
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

	// Validate required fields
	if instruction.Type == "" || instruction.Version == "" ||
		instruction.AssetIn == "" || instruction.AssetOut == "" ||
		instruction.Recipient == "" {
		ce.CustomAbort(ce.NewContractError(ce.ErrInput, "missing required fields"))
	}

	switch instruction.Type {
	case "swap":
		return executeSwap(instruction)
	case "deposit":
		return executeDeposit(instruction)
	case "withdrawal":
		return executeWithdrawal(instruction)
	default:
		ce.CustomAbort(ce.NewContractError(ce.ErrInput, "unknown instruction type"))
		return nil
	}
}

// Execute swap operation with 2-step routing
func executeSwap(instruction types.DexInstruction) *string {
	// Normalize once — all downstream functions expect lowercase
	instruction.AssetIn = strings.ToLower(instruction.AssetIn)
	instruction.AssetOut = strings.ToLower(instruction.AssetOut)

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

	// Pre-fund input asset into the pool (mapped via transferFrom, native via HiveDraw+HiveTransfer).
	err := preFundAsset(instruction.AssetIn, instruction.AmountIn, dexContractId, &env)
	if err != nil {
		ce.CustomAbort(ce.Prepend(err, "error pre-funding asset to dex"))
	}

	swapParams := types.SwapParams{
		AssetIn:      instruction.AssetIn,
		AmountIn:     instruction.AmountIn,
		AssetOut:     instruction.AssetOut,
		MinAmountOut: instruction.MinAmountOut,
		To:           instruction.Recipient,
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

	secondSwapParams := types.SwapParams{
		AssetIn:      sdk.AssetHbd.String(),
		AmountIn:     res1AmountOut.String(),
		AssetOut:     instruction.AssetOut,
		From:         contractAccId,
		To:           instruction.Recipient,
		MinAmountOut: instruction.MinAmountOut,
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

	return secondResult
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
		sdk.ContractCall(mappingContract, "transferFrom", string(payload), &sdk.ContractCallOptions{})
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
func executeDeposit(instruction types.DexInstruction) *string {
	// Find the pool
	poolId := findPool(instruction.AssetIn, instruction.AssetOut)
	if poolId == "" {
		ce.CustomAbort(ce.NewContractError(ce.ErrTransaction, "pool not found"))
	}

	// For deposit, we need amounts from metadata
	if instruction.Metadata == nil {
		ce.CustomAbort(ce.NewContractError(ce.ErrInput, "deposit amounts required in metadata"))
	}

	amt0, ok := instruction.Metadata["amount0"]
	if !ok {
		ce.CustomAbort(ce.NewContractError(ce.ErrInput, "amount0 required in metadata"))
	}
	amt1, ok := instruction.Metadata["amount1"]
	if !ok {
		ce.CustomAbort(ce.NewContractError(ce.ErrInput, "amount1 required in metadata"))
	}

	if _, ok2 := new(big.Int).SetString(amt0, 10); !ok2 {
		ce.CustomAbort(ce.NewContractError(ce.ErrInput, "amount0 must be a valid number"))
	}
	if _, ok2 := new(big.Int).SetString(amt1, 10); !ok2 {
		ce.CustomAbort(ce.NewContractError(ce.ErrInput, "amount1 must be a valid number"))
	}

	// Pre-fund both assets into the pool (mapped via transferFrom, native via HiveDraw+HiveTransfer).
	env := sdk.GetEnv()
	err := preFundAsset(instruction.AssetIn, amt0, poolId, &env)
	if err != nil {
		ce.CustomAbort(ce.Prepend(err, "error pre-funding asset to dex"))
	}
	err = preFundAsset(instruction.AssetOut, amt1, poolId, &env)
	if err != nil {
		ce.CustomAbort(ce.Prepend(err, "error pre-funding asset to dex"))
	}

	// All assets are pre-deposited — pool skips DrawAssetFrom for both.
	addLiqParams := types.AddLiquidityParams{
		Amount0:       amt0,
		Amount1:       amt1,
		Recipient:     instruction.Recipient,
		PreDeposited0: true,
		PreDeposited1: true,
	}
	addLiqPayload, err := tinyjson.Marshal(&addLiqParams)
	if err != nil {
		ce.CustomAbort(ce.NewContractError(ce.ErrJson, "failed to marshal add liquidity params"))
	}

	// Call DEX contract's add_liquidity method
	result := sdk.ContractCall(poolId, "add_liquidity", string(addLiqPayload), &sdk.ContractCallOptions{})
	if result == nil {
		ce.CustomAbort(ce.NewContractError(ce.ErrTransaction, "add liquidity returned no result"))
	}

	return result
}

// Execute withdrawal (remove liquidity)
func executeWithdrawal(instruction types.DexInstruction) *string {
	// Find the pool
	poolId := findPool(instruction.AssetIn, instruction.AssetOut)
	if poolId == "" {
		ce.CustomAbort(ce.NewContractError(ce.ErrInitialization, "pool not found"))
	}

	// For withdrawal, we need LP amount from metadata
	if instruction.Metadata == nil {
		ce.CustomAbort(ce.NewContractError(ce.ErrInput, "lp_amount required in metadata"))
	}

	lpAmount, ok := instruction.Metadata["lp_amount"]
	if !ok {
		ce.CustomAbort(ce.NewContractError(ce.ErrInput, "lp_amount required in metadata"))
	}

	// Prepare remove liquidity parameters
	removeLiqParams := types.RemoveLiquidityParams{
		LpAmount:  lpAmount,
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

// Query schema information - returns supported assets and chains
// Payload: empty or {"type": "schema"}
//
//go:wasmexport get_schema
func GetSchema(payload *string) *string {
	// Get all registered assets and their chains
	// For now, return a simplified schema
	// In production, this would query all registered assets

	chainsStr := getStr(keyChainsList)
	chains := []string{"BTC", "ETH", "SOL", chainHIVE, chainMAGI} // Default chains (HIVE, MAGI for native tokens)
	if chainsStr != "" {
		parts := splitChains(chainsStr)
		if len(parts) > 0 {
			chains = parts
		}
	}

	// Build schema response
	schema := types.SchemaReturn{
		SupportedChains:     chains,
		ReturnAddressChains: chains,
		Note:                "Schema is dynamically generated from registered pools. Use indexer API for complete schema.",
	}

	schemaBytes, err := tinyjson.Marshal(&schema)
	if err != nil {
		ce.CustomAbort(ce.NewContractError(ce.ErrJson, "failed to marshal schema"))
	}

	result := string(schemaBytes)
	return &result
}
