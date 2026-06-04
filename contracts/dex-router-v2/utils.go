package main

import (
	"slices"
	"strings"

	tinyjson "github.com/CosmWasm/tinyjson"
	ce "github.com/vsc-eco/dex-contracts/contracterrors"
	"github.com/vsc-eco/dex-contracts/contracts/types"
	"github.com/vsc-eco/dex-contracts/sdk"
)

// Keys for state storage
const (
	keyVersion     = "version"
	keyPoolPrefix  = "pool" + types.DirPathDelimiter  // pool-{asset0}-{asset1}
	keyStatePrefix = "state" + types.DirPathDelimiter // state-{pool_id}
	keyAssetPrefix = "asset" + types.DirPathDelimiter // asset-{symbol} -> AssetInfo
	keyChainsList  = "chains"                         // comma-separated list of supported chains
	keyTokensList  = "tokens"                         // comma-separated list of registered token names
)

// Supported chains for native tokens (HIVE for HIVE/HBD, MAGI for future MAGI native tokens)
const (
	chainHIVE = "HIVE"
	chainMAGI = "MAGI"
)

// State helpers
func getStr(key string) string {
	v := sdk.StateGetObject(key)
	if v == nil {
		return ""
	}
	return *v
}

func setStr(key string, val string) {
	sdk.StateSetObject(key, val)
}

// Pool key helpers
// poolKeyForAssets expects already-lowercased asset names.
func poolKeyForAssets(asset0, asset1 string) string {
	return keyPoolPrefix + asset0 + types.DirPathDelimiter + asset1
}

func poolStateKey(poolId string) string {
	return keyStatePrefix + poolId
}

// // Pool state cache helpers
// func getPoolState(poolId string) types.PoolInfo {
// 	stateKey := poolStateKey(poolId)
// 	stateStr := getStr(stateKey)
// 	if stateStr == "" {
// 		return types.PoolInfo{} // Empty state
// 	}

// 	// Parse pool state from stored string
// 	// In a real implementation, we'd use tinyjson to unmarshal
// 	// For now, return empty - will be populated from swap results
// 	return types.PoolInfo{}
// }

// func setPoolState(poolId string, state types.PoolInfo) {
// 	stateKey := poolStateKey(poolId)
// 	// In a real implementation, we'd marshal state to JSON
// 	// For now, store as placeholder
// 	setStr(stateKey, "cached")
// }

// Asset registry helpers - tokens MUST be registered before pools can use them
func assetKey(asset string) string {
	return keyAssetPrefix + strings.ToLower(asset)
}

// isAssetRegistered returns true if the asset has been registered in the token registry
func isAssetRegistered(asset string) bool {
	return getStr(assetKey(asset)) != ""
}

// updateChainsList adds a chain to the supported chains list if not already present
func updateChainsList(chain string) {
	chainsStr := getStr(keyChainsList)

	// Check if chain already in list
	// Simple check - in production, use proper parsing
	if len(chainsStr) > 0 {
		chains := strings.Split(chainsStr, ",")

		if !slices.Contains(chains, chain) {
			newChains := chainsStr + "," + chain
			setStr(keyChainsList, newChains)
		}
	} else {
		setStr(keyChainsList, chain)
	}
}

// updateTokensList adds a token name to the registered tokens list if not already present
func updateTokensList(name string) {
	tokensStr := getStr(keyTokensList)
	if len(tokensStr) > 0 {
		tokens := strings.Split(tokensStr, ",")
		if !slices.Contains(tokens, name) {
			setStr(keyTokensList, tokensStr+","+name)
		}
	} else {
		setStr(keyTokensList, name)
	}
}

// getAssetChain returns the blockchain chain for a given asset symbol.
// Returns empty string if asset is not registered - tokens MUST be registered
// via register_token before pools can use them.
func getAssetChain(asset string) string {
	infoStr := getStr(assetKey(asset))
	if infoStr == "" {
		return ""
	}
	var tokenInfo types.TokenInfo
	err := tinyjson.Unmarshal([]byte(infoStr), &tokenInfo)
	if err != nil {
		return ""
	}
	return tokenInfo.Chain
}

// splitChains parses comma-separated chains list, deduplicates and returns
func splitChains(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	seen := make(map[string]bool)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// getMappingContract returns the mapping contract ID for a mapped asset,
// or empty string for native assets. Looks up the token registry.
func getMappingContract(asset string) string {
	infoStr := getStr(assetKey(asset))
	if infoStr == "" {
		return ""
	}
	var tokenInfo types.TokenInfo
	err := tinyjson.Unmarshal([]byte(infoStr), &tokenInfo)
	if err != nil {
		// review7 M8: a registry entry EXISTS but is corrupted. Returning ""
		// here would make a mapped asset look native, so settleToChain would
		// HiveWithdraw it instead of unmapping — misrouting funds silently.
		// Abort instead so the failure is loud and no funds move.
		ce.CustomAbort(ce.WrapContractError(ce.ErrJson, err, "corrupted token registry entry for asset "+asset))
	}
	return tokenInfo.MappingContract
}

func strPrt(s string) *string {
	return &s
}
