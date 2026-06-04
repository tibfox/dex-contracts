package main

import (
	"math/big"
	"strconv"
	"strings"

	"github.com/vsc-eco/dex-contracts/contracts/asset"
	"github.com/vsc-eco/dex-contracts/contracts/types"
	"github.com/vsc-eco/dex-contracts/sdk"

	ce "github.com/vsc-eco/dex-contracts/contracterrors"

	tinyjson "github.com/CosmWasm/tinyjson"
)

func main() {}

// Contract initialization
// Payload: JSON with pool parameters
// {"asset0": "HBD", "asset1": "HIVE", "fee_bps": 8}
//
//go:wasmexport init
func Init(payload *string) *string {
	// Owner-only: prevents anyone but the deployer from invoking init as a
	// regular transaction. The runtime exposes `init` like any other export,
	// so without this check a fresh transaction can re-enter Init and zero
	// reserves.
	env := sdk.GetEnv()
	ownerPtr := sdk.GetEnvKey("contract.owner")
	if ownerPtr == nil || env.Caller.String() != *ownerPtr {
		ce.CustomAbort(ce.NewContractError(ce.ErrNoPermission, "owner only"))
	}

	// Idempotency guard: KeyMigrateVer is set at the end of a successful Init
	// (and by Migrate). A non-empty value means the pool is already
	// initialized, so re-running would clobber live reserves and LP supply.
	if getStr(types.KeyMigrateVer) != "" {
		ce.CustomAbort(ce.NewContractError(ce.ErrInitialization, "pool already initialized"))
	}

	if payload == nil {
		ce.CustomAbort(ce.NewContractError(ce.ErrInput, "payload required"))
	}

	var params types.InitParams
	if err := tinyjson.Unmarshal([]byte(*payload), &params); err != nil {
		ce.CustomAbort(ce.WrapContractError(ce.ErrJson, err))
	}

	// Normalize to lowercase
	params.Asset0 = strings.ToLower(params.Asset0)
	params.Asset1 = strings.ToLower(params.Asset1)

	// Validate assets are different
	if params.Asset0 == params.Asset1 {
		ce.CustomAbort(ce.NewContractError(ce.ErrInput, "assets must be different"))
	}

	// Enforce alphabetical ordering: asset0 < asset1
	if params.Asset0 > params.Asset1 {
		params.Asset0, params.Asset1 = params.Asset1, params.Asset0
		params.Asset0MappingContract, params.Asset1MappingContract = params.Asset1MappingContract, params.Asset0MappingContract
	}

	if params.Asset0MappingContract != "" && sdk.VerifyAddress("contract:"+params.Asset0MappingContract) != "contract" {
		ce.CustomAbort(ce.NewContractError(ce.ErrInput, "asset0_mapping_contract must be a valid contract ID"))
	}
	if params.Asset1MappingContract != "" && sdk.VerifyAddress("contract:"+params.Asset1MappingContract) != "contract" {
		ce.CustomAbort(ce.NewContractError(ce.ErrInput, "asset1_mapping_contract must be a valid contract ID"))
	}

	if _, err := asset.NewAsset(params.Asset0, params.Asset0MappingContract); err != nil {
		ce.CustomAbort(ce.WrapContractError(ce.ErrInput, err, "invalid asset0"))
	}
	if _, err := asset.NewAsset(params.Asset1, params.Asset1MappingContract); err != nil {
		ce.CustomAbort(ce.WrapContractError(ce.ErrInput, err, "invalid asset1"))
	}

	// Default fee if not specified
	if params.FeeBps == 0 {
		params.FeeBps = types.DefaultBaseFeeBps
	}
	if params.FeeBps > 10000 {
		ce.CustomAbort(
			ce.NewContractError(
				ce.ErrInput,
				"fee bps must not exceed 10000 got "+strconv.FormatUint(params.FeeBps, 10),
			),
		)
	}

	// Initialize pool state (already lowercased and alphabetically ordered above)
	setStr(types.KeyAsset0Name, params.Asset0)
	setStr(types.KeyAsset1Name, params.Asset1)
	setStr(types.KeyAsset0Mapping, params.Asset0MappingContract)
	setStr(types.KeyAsset1Mapping, params.Asset1MappingContract)
	setReserve0(big.NewInt(0))
	setReserve1(big.NewInt(0))
	setFee(big.NewInt(int64(params.FeeBps)))
	setTotalLp(big.NewInt(0))
	setBigInt(types.KeySystemFee0, big.NewInt(0))
	setBigInt(types.KeySystemFee1, big.NewInt(0))
	setTimestamp(types.KeyFeeLastClaim, sdk.GetEnv().Timestamp)
	if params.RouterContract == "" {
		ce.CustomAbort(ce.NewContractError(ce.ErrInput, "router_contract is required"))
	}
	if sdk.VerifyAddress("contract:"+params.RouterContract) != "contract" {
		ce.CustomAbort(ce.NewContractError(ce.ErrInput, "router_contract must be a valid contract ID"))
	}
	setStr(types.KeyRouter, params.RouterContract)
	setStr(types.KeyMigrateVer, "4")

	sdk.Log(logPoolInit(params.Asset0, params.Asset1, params.FeeBps))

	return nil
}

// Swap tokens in this pool
// Payload: JSON with swap parameters
// {"asset_in": "HBD", "amount_in": 1000000, "asset_out": "HIVE", "min_amount_out": 900000, "recipient": "user123"}
//
//go:wasmexport swap
func Swap(payload *string) *string {
	if payload == nil {
		ce.CustomAbort(
			ce.NewContractError(ce.ErrInput, "payload required"),
		)
	}

	var params types.SwapParams
	if err := tinyjson.Unmarshal([]byte(*payload), &params); err != nil {
		ce.CustomAbort(
			ce.WrapContractError(ce.ErrInput, err, "invalid payload"),
		)
	}

	// Validate required fields
	if params.AssetIn == "" || params.AssetOut == "" || params.To == "" {
		ce.CustomAbort(
			ce.NewContractError(ce.ErrInput, "asset_in, asset_out, and recipient are required"),
		)
	}
	if sdk.VerifyAddress(params.To) == "unknown" {
		ce.CustomAbort(
			ce.NewContractError(ce.ErrInput, "to address ["+params.To+"] invalid"),
		)
	}

	params.AssetIn = strings.ToLower(params.AssetIn)
	params.AssetOut = strings.ToLower(params.AssetOut)

	// Use cached asset names for direction check (avoids JSON unmarshal)
	asset0Name := getStr(types.KeyAsset0Name)
	asset1Name := getStr(types.KeyAsset1Name)

	feeBps := getFee()

	// Determine swap direction using cached names
	var magiFeeKey string
	var rInKey, rOutKey string
	var inputIsAsset0 bool

	if asset0Name == params.AssetIn && asset1Name == params.AssetOut {
		inputIsAsset0 = true
		magiFeeKey = types.KeySystemFee1
		rInKey = types.KeyReserve0
		rOutKey = types.KeyReserve1
	} else if asset1Name == params.AssetIn && asset0Name == params.AssetOut {
		inputIsAsset0 = false
		magiFeeKey = types.KeySystemFee0
		rInKey = types.KeyReserve1
		rOutKey = types.KeyReserve0
	} else {
		ce.CustomAbort(
			ce.NewContractError(ce.ErrInitialization, "invalid asset pair for pool want "+asset0Name+"-"+asset1Name+" got "+params.AssetIn+"-"+params.AssetOut),
		)
	}

	// Only parse full asset objects when needed for transfers
	asset0, err := getAsset0()
	if err != nil {
		ce.CustomAbort(ce.NewContractError(ce.ErrInitialization, "asset0 not found"))
	}
	asset1, err := getAsset1()
	if err != nil {
		ce.CustomAbort(ce.NewContractError(ce.ErrInitialization, "asset1 not found"))
	}

	var inputAsset, outputAsset asset.Asset
	if inputIsAsset0 {
		inputAsset = asset0
		outputAsset = asset1
	} else {
		inputAsset = asset1
		outputAsset = asset0
	}

	rIn := getBigInt(rInKey)
	rOut := getBigInt(rOutKey)

	if rIn.Sign() == 0 || rOut.Sign() == 0 {
		ce.CustomAbort(
			ce.NewContractError(ce.ErrTransaction, "pool has zero reserves"),
		)
	}
	maxSwap := new(big.Int).Div(rIn, big.NewInt(2))

	// baseFee = amountIn * feeBps / 10000
	amountIn, ok := new(big.Int).SetString(params.AmountIn, 10)
	if !ok || amountIn.Sign() <= 0 {
		ce.CustomAbort(
			ce.NewContractError(ce.ErrInput, "invalid input amount"),
		)
	}
	if amountIn.Cmp(maxSwap) > 0 {
		ce.CustomAbort(
			ce.NewContractError(ce.ErrTransaction, "amount exceeds input asset liquidity / 2", "swap too large"),
		)
	}

	// Fees are denominated in the OUTPUT asset: the full amountIn enters the
	// pool, the invariant produces a gross output, and fees are carved off it.

	// k = rIn * rOut  (big.Int to prevent overflow)
	k := new(big.Int).Mul(rIn, rOut)

	// newRIn = rIn + amountIn (entire input enters the pool)
	newRIn := new(big.Int).Add(rIn, amountIn)

	// grossOut = rOut - k / newRIn (pre-fee output from the invariant)
	grossOut := new(big.Int).Div(k, newRIn)
	grossOut.Sub(rOut, grossOut)

	// baseFee = grossOut * feeBps / 10000  (output units)
	baseFee := new(big.Int).Mul(grossOut, feeBps)
	baseFee.Div(baseFee, big.NewInt(10000))
	if baseFee.Sign() == 0 {
		baseFee.SetUint64(1)
	}

	// clpFee = (amountIn^2 * rOut) / (amountIn + rIn)^2  (output units)
	numerator := new(big.Int).Mul(amountIn, amountIn)
	numerator.Mul(numerator, rOut)
	denominator := new(big.Int).Add(amountIn, rIn)
	denominator.Mul(denominator, denominator)
	clpFee := new(big.Int).Div(numerator, denominator)
	if clpFee.Sign() == 0 {
		clpFee.SetUint64(1)
	}
	magiFee := new(big.Int).Add(baseFee, clpFee)
	lpFee := new(big.Int).Set(magiFee)
	magiFee.Div(magiFee, big.NewInt(4))
	if magiFee.Sign() == 0 {
		magiFee.SetUint64(1)
	}
	lpFee.Sub(lpFee, magiFee)

	// amountOut = grossOut - baseFee - clpFee
	amountOut := new(big.Int).Sub(grossOut, baseFee)
	amountOut.Sub(amountOut, clpFee)
	if amountOut.Sign() <= 0 {
		ce.CustomAbort(
			ce.NewContractError(ce.ErrTransaction, "insufficient amount to cover fees"),
		)
	}

	// newROut = rOut - grossOut; lpFee is added back below so it stays in the pool
	newROut := new(big.Int).Sub(rOut, grossOut)

	maybeEnv := types.MaybeEnv{}

	if sdk.VerifyAddress(params.From) == "unknown" {
		ce.CustomAbort(ce.NewContractError(ce.ErrInput, "from address ["+params.From+"] invalid"))
	}
	from := sdk.Address(params.From)

	// SECURITY: Only trust PreDeposited from the authorized Router contract.
	// A direct call with PreDeposited=true would skip the deposit and drain the pool.
	if params.PreDeposited {
		env := sdk.GetEnv()
		routerId := getStr(types.KeyRouter)
		if routerId == "" || env.Caller.String() != "contract:"+routerId {
			ce.CustomAbort(
				ce.NewContractError(ce.ErrNoPermission, "PreDeposited only allowed from the authorized Router"),
			)
		}
	}
	if !params.PreDeposited {
		if err := inputAsset.DrawAssetFrom(amountIn, from, maybeEnv); err != nil {
			ce.CustomAbort(ce.WrapContractError(ce.ErrTransaction, err, "failed to draw input asset"))
		}
	}

	// Handle referral fees (calculate before state updates)
	var refOut *big.Int
	if params.Beneficiary != nil && params.RefBps != nil {
		if sdk.VerifyAddress(*params.Beneficiary) == "unknown" {
			ce.CustomAbort(
				ce.NewContractError(ce.ErrInput, "beneficiary address ["+*params.Beneficiary+"] invalid"),
			)
		}
		if *params.RefBps > 10000 {
			ce.CustomAbort(
				ce.NewContractError(ce.ErrInput, "ref bps ["+strconv.FormatUint(*params.RefBps, 10)+"] > 10000"),
			)
		}
		refOut = new(big.Int).Set(amountOut)
		refOut.Mul(refOut, new(big.Int).SetUint64(*params.RefBps))
		refOut.Div(refOut, big.NewInt(10000))
		if refOut.Sign() == 1 {
			if refOut.Cmp(amountOut) > 0 {
				refOut.Set(amountOut)
			}
			amountOut.Sub(amountOut, refOut)
		} else {
			refOut = nil
		}
	}

	// Apply slippage protection AFTER referral deduction so min_amount_out
	// reflects what the user actually receives.
	if params.MinAmountOut != nil {
		minOut, ok := new(big.Int).SetString(*params.MinAmountOut, 10)
		if !ok {
			ce.CustomAbort(
				ce.NewContractError(ce.ErrInitialization, "invalid minimum amount out"),
			)
		}
		// review7 M2: SetString parses a negative value fine, and a negative
		// floor makes the Cmp check below vacuously pass — defeating slippage
		// protection entirely. Reject it.
		if minOut.Sign() < 0 {
			ce.CustomAbort(
				ce.NewContractError(ce.ErrInput, "minimum amount out must be non-negative"),
			)
		}
		if amountOut.Cmp(minOut) < 0 {
			ce.CustomAbort(
				ce.NewContractError(ce.ErrInitialization, "slippage tolerance exceeded"),
			)
		}
	}

	// --- EFFECTS: update reserves BEFORE external transfers (reentrancy protection) ---
	if magiFee.Sign() == 1 {
		currentFee := getBigInt(magiFeeKey)
		currentFee.Add(currentFee, magiFee)
		setBigInt(magiFeeKey, currentFee)
	}
	if lpFee.Sign() == 1 {
		newROut.Add(newROut, lpFee)
	}
	setBigInt(rInKey, newRIn)
	setBigInt(rOutKey, newROut)

	// --- INTERACTIONS: external transfers after state is finalized ---
	if refOut != nil && refOut.Sign() == 1 {
		err := outputAsset.TransferAsset(*params.Beneficiary, refOut)
		if err != nil {
			ce.CustomAbort(
				ce.WrapContractError(ce.ErrInitialization, err, "error transferring beneficiary asset out"),
			)
		}
	}
	if err := outputAsset.TransferAsset(params.To, amountOut); err != nil {
		ce.CustomAbort(
			ce.WrapContractError(ce.ErrTransaction, err, "error transferring output asset"),
		)
	}

	// log fee and amount swapped
	sdk.Log(logFee(params.AssetOut, magiFee, lpFee))
	sdk.Log(logSwap(params.AssetIn, params.AssetOut, amountIn, amountOut, params.To))

	// Return swap result — use cached names and local reserve values
	// to avoid redundant state reads.
	var resultR0, resultR1 string
	if inputIsAsset0 {
		resultR0 = newRIn.String()
		resultR1 = newROut.String()
	} else {
		resultR0 = newROut.String()
		resultR1 = newRIn.String()
	}
	result := types.SwapResult{
		AmountOut: amountOut.String(),
		PoolState: types.PoolInfo{
			Asset0:   asset0Name,
			Asset1:   asset1Name,
			Reserve0: resultR0,
			Reserve1: resultR1,
			Fee:      feeBps.Uint64(),
			TotalLp:  getTotalLp().String(),
		},
	}

	resultBytes, err := tinyjson.Marshal(&result)
	if err != nil {
		ce.CustomAbort(
			ce.WrapContractError(ce.ErrJson, err, "failed to serialize output"),
		)
	}

	resultStr := string(resultBytes)
	return &resultStr
}

// Add liquidity to the pool
// Payload: JSON with deposit parameters
// {"amount0": 1000000, "amount1": 1000000, "recipient": "user123"}
//
//go:wasmexport add_liquidity
func AddLiquidity(payload *string) *string {
	if payload == nil {
		ce.CustomAbort(
			ce.NewContractError(ce.ErrInput, "payload required"),
		)
	}

	var params types.AddLiquidityParams
	if err := tinyjson.Unmarshal([]byte(*payload), &params); err != nil {
		ce.CustomAbort(
			ce.NewContractError(ce.ErrInput, "invalid payload"),
		)
	}

	amt0, ok := new(big.Int).SetString(params.Amount0, 10)
	if !ok {
		ce.CustomAbort(
			ce.NewContractError(ce.ErrInput, "invalid amount0"),
		)
	}
	amt1, ok := new(big.Int).SetString(params.Amount1, 10)
	if !ok {
		ce.CustomAbort(
			ce.NewContractError(ce.ErrInput, "invalid amount1"),
		)
	}

	if amt0.Sign() <= 0 || amt1.Sign() <= 0 || params.Recipient == "" {
		ce.CustomAbort(
			ce.NewContractError(ce.ErrInput, "positive amount0, amount1, and recipient are required"),
		)
	}
	if sdk.VerifyAddress(params.Recipient) == "unknown" {
		ce.CustomAbort(
			ce.NewContractError(ce.ErrInput, "recipient address ["+params.Recipient+"] invalid"),
		)
	}

	return executeAddLiquidity(amt0, amt1, params.Recipient, params)
}

// Remove liquidity from the pool
// Payload: JSON with withdrawal parameters
// {"lp_amount": 500000, "recipient": "user123"}
//
//go:wasmexport remove_liquidity
func RemoveLiquidity(payload *string) *string {
	if payload == nil {
		ce.CustomAbort(
			ce.NewContractError(ce.ErrInput, "payload required"),
		)
	}

	var params types.RemoveLiquidityParams
	if err := tinyjson.Unmarshal([]byte(*payload), &params); err != nil {
		ce.CustomAbort(
			ce.NewContractError(ce.ErrInput, "invalid payload"),
		)
	}

	lpAmt, ok := new(big.Int).SetString(params.LpAmount, 10)
	if !ok {
		ce.CustomAbort(
			ce.NewContractError(ce.ErrInput, "invalid lp amount"),
		)
	}

	if lpAmt.Sign() <= 0 || params.Recipient == "" {
		ce.CustomAbort(
			ce.NewContractError(ce.ErrInput, "positive lp_amount and recipient are required"),
		)
	}
	if sdk.VerifyAddress(params.Recipient) == "unknown" {
		ce.CustomAbort(
			ce.NewContractError(ce.ErrInput, "recipient address ["+params.Recipient+"] invalid"),
		)
	}

	return executeRemoveLiquidity(lpAmt, params.Recipient)
}

// Execute add liquidity operation
func executeAddLiquidity(amt0, amt1 *big.Int, provider string, params types.AddLiquidityParams) *string {
	asset0, err := getAsset0()
	if err != nil {
		ce.CustomAbort(ce.WrapContractError(ce.ErrInitialization, err, "pool not initialized"))
	}
	asset1, err := getAsset1()
	if err != nil {
		ce.CustomAbort(ce.WrapContractError(ce.ErrInitialization, err, "pool not initialized"))
	}

	maybeEnv := types.MaybeEnv{}

	// SECURITY: Only trust PreDeposited flags from the authorized Router.
	if params.PreDeposited0 || params.PreDeposited1 {
		env := maybeEnv.UseEnv()
		routerId := getStr(types.KeyRouter)
		if routerId == "" || env.Caller.String() != "contract:"+routerId {
			ce.CustomAbort(
				ce.NewContractError(ce.ErrNoPermission, "PreDeposited only allowed from the authorized Router"),
			)
		}
	}
	// Draw each asset independently — skip if pre-deposited by Router.
	if amt0.Sign() == 1 && !params.PreDeposited0 {
		if err := asset0.DrawAssetFrom(amt0, maybeEnv.UseEnv().Caller, maybeEnv); err != nil {
			ce.CustomAbort(ce.WrapContractError(ce.ErrTransaction, err, "failed to draw asset0"))
		}
	}
	if amt1.Sign() == 1 && !params.PreDeposited1 {
		if err := asset1.DrawAssetFrom(amt1, maybeEnv.UseEnv().Caller, maybeEnv); err != nil {
			ce.CustomAbort(ce.WrapContractError(ce.ErrTransaction, err, "failed to draw asset1"))
		}
	}

	// Update reserves and mint LP
	r0 := getReserve0()
	r1 := getReserve1()
	totalLP := getTotalLp()

	minted := new(big.Int)
	if totalLP.Sign() == 0 {
		// Geometric mean: sqrt(amt0 * amt1) using big.Int
		product := new(big.Int).Mul(amt0, amt1)
		minted = new(big.Int).Sqrt(product)
	} else {
		// Proportional minting: min(amt0 * totalLP / r0, amt1 * totalLP / r1)
		m0 := new(big.Int).Mul(amt0, totalLP)
		m0.Div(m0, r0)

		m1 := new(big.Int).Mul(amt1, totalLP)
		m1.Div(m1, r1)

		minted = m0
		if m1.Cmp(m0) < 0 {
			minted = m1
		}
	}
	contractAssert(minted.Sign() == 1)

	// Slippage protection: reject if the LP minted is below the caller's
	// minimum. Empty/absent MinLpOut keeps the legacy (no-check) behavior.
	if params.MinLpOut != "" {
		minLpOut, ok := new(big.Int).SetString(params.MinLpOut, 10)
		if !ok {
			ce.CustomAbort(
				ce.NewContractError(ce.ErrInitialization, "invalid min_lp_out"),
			)
		}
		if minted.Cmp(minLpOut) < 0 {
			ce.CustomAbort(
				ce.NewContractError(ce.ErrInitialization, "insufficient LP minted: slippage"),
			)
		}
	}

	// Reuse big.Int objects to avoid extra allocations
	r0.Add(r0, amt0)
	r1.Add(r1, amt1)
	totalLP.Add(totalLP, minted)
	setReserve0(r0)
	setReserve1(r1)
	setTotalLp(totalLP)

	// Mint LP tokens to provider
	providerAddr := sdk.Address(provider)
	currentLP := getLp(providerAddr.String())
	newLP := new(big.Int).Add(currentLP, minted)
	setLp(providerAddr.String(), newLP)

	sdk.Log(logAddLiquidity(providerAddr.String(), amt0, amt1, minted))

	return nil
}

// Execute remove liquidity operation
func executeRemoveLiquidity(lpAmount *big.Int, provider string) *string {
	providerAddr := sdk.Address(provider)

	// Only the LP owner, the authorized router, or system can remove liquidity.
	env := sdk.GetEnv()
	routerId := getStr(types.KeyRouter)
	isRouter := routerId != "" && env.Caller.String() == "contract:"+routerId
	if env.Caller.String() != providerAddr.String() && !isRouter {
		ce.CustomAbort(ce.NewContractError(ce.ErrNoPermission, "caller is not the LP owner"))
	}

	userLP := getLp(providerAddr.String())
	totalLP := getTotalLp()

	contractAssert(lpAmount.Sign() == 1 && lpAmount.Cmp(userLP) <= 0 && lpAmount.Cmp(totalLP) <= 0)

	r0 := getReserve0()
	r1 := getReserve1()

	// amt = reserve * lpAmount / totalLP  (big.Int to avoid overflow)
	amt0 := new(big.Int).Mul(r0, lpAmount)
	amt0.Div(amt0, totalLP)

	amt1 := new(big.Int).Mul(r1, lpAmount)
	amt1.Div(amt1, totalLP)

	// newR0 = r0 - amt0
	newR0 := new(big.Int).Sub(r0, amt0)
	newR1 := new(big.Int).Sub(r1, amt1)
	newUserLP := new(big.Int).Sub(userLP, lpAmount)
	newTotalLP := new(big.Int).Sub(totalLP, lpAmount)

	if newR0.Sign() < 0 || newR1.Sign() < 0 || newUserLP.Sign() < 0 || newTotalLP.Sign() < 0 {
		ce.CustomAbort(
			ce.NewContractError(ce.ErrArithmetic, "underflow in remove liquidity"),
		)
	}

	setLp(providerAddr.String(), newUserLP)
	setTotalLp(newTotalLP)
	setReserve0(newR0)
	setReserve1(newR1)

	// Transfer assets out
	asset0, err := getAsset0()
	if err != nil {
		ce.CustomAbort(
			ce.WrapContractError(ce.ErrStateAccess, err, "error retrieving asset0"),
		)
	}
	asset1, err := getAsset1()
	if err != nil {
		ce.CustomAbort(
			ce.WrapContractError(ce.ErrStateAccess, err, "error retrieving asset1"),
		)
	}
	if amt0.Sign() == 1 {
		if err := asset0.TransferAsset(provider, amt0); err != nil {
			ce.CustomAbort(ce.WrapContractError(ce.ErrTransaction, err, "error transferring asset0 to provider"))
		}
	}
	if amt1.Sign() == 1 {
		if err := asset1.TransferAsset(provider, amt1); err != nil {
			ce.CustomAbort(ce.WrapContractError(ce.ErrTransaction, err, "error transferring asset1 to provider"))
		}
	}

	sdk.Log(logRemoveLiquidity(providerAddr.String(), amt0, amt1, lpAmount))

	return nil
}

// Query pool information
//
//go:wasmexport get_pool
func GetPool(_ *string) *string {
	// Use cached names — avoids JSON unmarshal of full asset objects
	poolInfo := types.PoolInfo{
		Asset0:   getStr(types.KeyAsset0Name),
		Asset1:   getStr(types.KeyAsset1Name),
		Reserve0: getReserve0().String(),
		Reserve1: getReserve1().String(),
		Fee:      getFee().Uint64(),
		TotalLp:  getTotalLp().String(),
	}

	resultBytes, err := tinyjson.Marshal(&poolInfo)
	if err != nil {
		ce.CustomAbort(
			ce.WrapContractError(ce.ErrJson, err, "failed to serialize output"),
		)
	}

	result := string(resultBytes)
	return &result
}

// // Update the authorized router contract (owner only)
// // Payload: router contract ID string (e.g. "vsc1BoZJMQqpmdLxUfyRt5Tz82YM7Z57r7Dos7")
// //
// //go:wasmexport update_router
// func UpdateRouter(payload *string) *string {
// 	env := sdk.GetEnv()
// 	ownerPtr := sdk.GetEnvKey("contract.owner")
// 	if ownerPtr == nil || env.Caller.String() != *ownerPtr {
// 		ce.CustomAbort(
// 			ce.NewContractError(ce.ErrNoPermission, "action must be performed by the contract owner"),
// 		)
// 	}
// 	if payload == nil || *payload == "" {
// 		ce.CustomAbort(
// 			ce.NewContractError(ce.ErrInput, "router contract ID required"),
// 		)
// 	}
// 	setStr(types.KeyRouter, *payload)
// 	return nil
// }

// Claim fees (system only)
//
//go:wasmexport claim_fees
func ClaimFees(payload *string) *string {
	// Fee destination: contract owner receives fees.
	// HiveWithdraw requires a valid hive: address and only supports HBD.
	// Mapped assets are transferred via the mapping contract.
	ownerPtr := sdk.GetEnvKey("contract.owner")
	if ownerPtr == nil || *ownerPtr == "" {
		ce.CustomAbort(ce.NewContractError(ce.ErrStateAccess, "contract owner not set"))
	}
	owner := *ownerPtr

	env := sdk.GetEnv()
	if env.Caller.String() != owner {
		ce.CustomAbort(ce.NewContractError(ce.ErrNoPermission, "only the contract owner can claim fees"))
	}

	f0 := getBigInt(types.KeySystemFee0)
	f1 := getBigInt(types.KeySystemFee1)

	if f0.Sign() == 1 {
		setBigInt(types.KeySystemFee0, big.NewInt(0))
		asset0, err := getAsset0()
		if err != nil {
			ce.CustomAbort(ce.WrapContractError(ce.ErrStateAccess, err, "claim_fees: cannot read asset0"))
		}
		if asset0.MappingContract() == "" {
			sdk.HiveWithdraw(sdk.Address(owner), f0, sdk.Asset(asset0.Name()))
		} else {
			if err := asset0.TransferAsset(owner, f0); err != nil {
				ce.CustomAbort(ce.WrapContractError(ce.ErrTransaction, err, "error transferring fee asset0"))
			}
		}
	}
	if f1.Sign() == 1 {
		setBigInt(types.KeySystemFee1, big.NewInt(0))
		asset1, err := getAsset1()
		if err != nil {
			ce.CustomAbort(ce.WrapContractError(ce.ErrStateAccess, err, "claim_fees: cannot read asset1"))
		}
		if asset1.MappingContract() == "" {
			sdk.HiveWithdraw(sdk.Address(owner), f1, sdk.Asset(asset1.Name()))
		} else {
			if err := asset1.TransferAsset(owner, f1); err != nil {
				ce.CustomAbort(ce.WrapContractError(ce.ErrTransaction, err, "error transferring fee asset1"))
			}
		}
	}

	setTimestamp(types.KeyFeeLastClaim, sdk.GetEnv().Timestamp)
	return nil
}

// Migrate contract state to latest version.
// Owner-only. Each version runs once; re-calling is a no-op.
// Payload: JSON {"router_contract": "vsc1..."} (optional, for v1)
//
//go:wasmexport migrate
func Migrate(payload *string) *string {
	env := sdk.GetEnv()
	ownerPtr := sdk.GetEnvKey("contract.owner")
	if ownerPtr == nil || env.Caller.String() != *ownerPtr {
		ce.CustomAbort(ce.NewContractError(ce.ErrAuth, "owner only"))
	}

	version := getStr(types.KeyMigrateVer)

	// --- v1: populate cached asset name keys (a1n, a2n) and router (rtr) ---
	if version < "1" {
		// Read from old JSON keys directly (getAsset0/getAsset1 use new keys that don't exist yet)
		a0, err := asset.AssetFromJson(getStr("a1"))
		if err != nil {
			ce.CustomAbort(ce.WrapContractError(ce.ErrStateAccess, err, "migrate v1: cannot read asset0"))
		}
		a1, err := asset.AssetFromJson(getStr("a2"))
		if err != nil {
			ce.CustomAbort(ce.WrapContractError(ce.ErrStateAccess, err, "migrate v1: cannot read asset1"))
		}
		setStr("a1n", a0.Name())
		setStr("a2n", a1.Name())

		if payload != nil && *payload != "" {
			// Simple extraction: {"router_contract": "vsc1..."}
			p := *payload
			if idx := strings.Index(p, "router_contract"); idx >= 0 {
				// Find the value after the key
				rest := p[idx+len("router_contract"):]
				// Skip past ": " or ":" and opening quote
				start := strings.IndexByte(rest, '"')
				if start >= 0 {
					rest = rest[start+1:]
					end := strings.IndexByte(rest, '"')
					if end > 0 {
						setStr(types.KeyRouter, rest[:end])
					}
				}
			}
		}

		setStr(types.KeyMigrateVer, "1")
		sdk.Log(logMigrate("1", a0.Name(), a1.Name()))
	}

	// --- v2: decompose JSON asset blobs into individual keys, rename to 0-indexed ---
	if version < "2" {
		// Read old 1-indexed names (set by v1)
		oldAsset0Name := getStr("a1n")
		oldAsset1Name := getStr("a2n")

		// Parse old JSON blobs to extract mapping contract IDs
		var asset0Mapping, asset1Mapping string
		if a0, err := asset.AssetFromJson(getStr("a1")); err == nil {
			asset0Mapping = a0.MappingContract()
		}
		if a1, err := asset.AssetFromJson(getStr("a2")); err == nil {
			asset1Mapping = a1.MappingContract()
		}

		// Write to new 0-indexed individual keys
		setStr(types.KeyAsset0Name, oldAsset0Name)
		setStr(types.KeyAsset1Name, oldAsset1Name)
		setStr(types.KeyAsset0Mapping, asset0Mapping)
		setStr(types.KeyAsset1Mapping, asset1Mapping)

		setStr(types.KeyMigrateVer, "2")
		version = "2"
	}

	// --- v3: convert fee-last-claim timestamp from ISO string to 8-byte Unix epoch ---
	if version < "3" {
		oldTs := getStr(types.KeyFeeLastClaim)
		if oldTs != "" {
			// Old format is an ISO string; setTimestamp parses and re-stores as binary.
			setTimestamp(types.KeyFeeLastClaim, oldTs)
		}
		setStr(types.KeyMigrateVer, "3")
		version = "3"
	}

	// --- v4: enforce lexicographic asset ordering (asset0 < asset1) ---
	if version < "4" {
		a0n := getStr(types.KeyAsset0Name)
		a1n := getStr(types.KeyAsset1Name)
		if a0n > a1n {
			// Swap asset names
			setStr(types.KeyAsset0Name, a1n)
			setStr(types.KeyAsset1Name, a0n)

			// Swap mapping contracts
			m0 := getStr(types.KeyAsset0Mapping)
			m1 := getStr(types.KeyAsset1Mapping)
			setStr(types.KeyAsset0Mapping, m1)
			setStr(types.KeyAsset1Mapping, m0)

			// Swap reserves
			r0 := getReserve0()
			r1 := getReserve1()
			setReserve0(r1)
			setReserve1(r0)

			// Swap accumulated system fees
			f0 := getBigInt(types.KeySystemFee0)
			f1 := getBigInt(types.KeySystemFee1)
			setBigInt(types.KeySystemFee0, f1)
			setBigInt(types.KeySystemFee1, f0)
		}
		setStr(types.KeyMigrateVer, "4")
		version = "4"
	}

	// --- future migrations go here ---
	// Make sure to set the latest migration version in init as well.

	resultStr := getStr(types.KeyMigrateVer)
	return &resultStr
}
