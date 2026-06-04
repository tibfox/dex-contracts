package main

import (
	"encoding/binary"
	"math/big"
	"strconv"
	"strings"
	"time"

	ce "github.com/vsc-eco/dex-contracts/contracterrors"
	"github.com/vsc-eco/dex-contracts/contracts/asset"
	"github.com/vsc-eco/dex-contracts/contracts/types"
	"github.com/vsc-eco/dex-contracts/sdk"
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

func getBigInt(key string) *big.Int {
	v := sdk.StateGetObject(key)
	n := new(big.Int)
	if v == nil || *v == "" {
		return n
	}

	n.SetBytes([]byte(*v))
	return n
}

func setBigInt(key string, val *big.Int) {
	// review7 M6: big.Int.Bytes() encodes magnitude only, so a negative value
	// would round-trip through getBigInt as its absolute value — silent state
	// corruption (a negative reserve/fee/LP becomes positive). Every value this
	// pool stores is non-negative by invariant; assert it so a future negative
	// write aborts loudly instead of corrupting the pool.
	contractAssert(val.Sign() >= 0, "setBigInt: negative value for "+key)
	sdk.StateSetObject(key, string(val.Bytes()))
}

// Timestamp helpers — store as Unix epoch in big-endian int64 bytes (8 bytes).
func setTimestamp(key string, timestamp string) {
	// Strip Go's monotonic clock suffix (e.g. " m=+1.854221781")
	if idx := strings.Index(timestamp, " m="); idx >= 0 {
		timestamp = timestamp[:idx]
	}
	layouts := []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05.999999999 -0700 MST",
	}
	var t time.Time
	var err error
	for _, layout := range layouts {
		t, err = time.Parse(layout, timestamp)
		if err == nil {
			break
		}
	}
	if err != nil {
		ce.CustomAbort(ce.NewContractError(ce.ErrStateAccess, "invalid timestamp: "+timestamp))
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(t.Unix()))
	sdk.StateSetObject(key, string(buf[:]))
}

func getTimestamp(key string) int64 {
	v := sdk.StateGetObject(key)
	if v == nil || *v == "" {
		return 0
	}
	b := []byte(*v)
	if len(b) < 8 {
		// Pad left with zeros for values stored with fewer bytes
		var buf [8]byte
		copy(buf[8-len(b):], b)
		return int64(binary.BigEndian.Uint64(buf[:]))
	}
	return int64(binary.BigEndian.Uint64(b[:8]))
}

// Pool state helpers
func getAsset0() (asset.Asset, error) {
	return asset.NewAsset(getStr(types.KeyAsset0Name), getStr(types.KeyAsset0Mapping))
}

func getAsset1() (asset.Asset, error) {
	return asset.NewAsset(getStr(types.KeyAsset1Name), getStr(types.KeyAsset1Mapping))
}

func getReserve0() *big.Int {
	return getBigInt(types.KeyReserve0)
}

func getReserve1() *big.Int {
	return getBigInt(types.KeyReserve1)
}

func getFee() *big.Int {
	return getBigInt(types.KeyFee)
}

func getTotalLp() *big.Int {
	return getBigInt(types.KeyTotalLP)
}

func getLp(address string) *big.Int {
	return getBigInt(types.KeyLpPrefix + address)
}

func setReserve0(reserve *big.Int) {
	setBigInt(types.KeyReserve0, reserve)
}

func setReserve1(reserve *big.Int) {
	setBigInt(types.KeyReserve1, reserve)
}

func setFee(fee *big.Int) {
	setBigInt(types.KeyFee, fee)
}

func setTotalLp(totalLp *big.Int) {
	setBigInt(types.KeyTotalLP, totalLp)
}

func setLp(address string, amount *big.Int) {
	setBigInt(types.KeyLpPrefix+address, amount)
}

func contractAssert(cond bool, msgs ...string) {
	if !cond {
		msg := "assertion failed"
		if len(msgs) > 0 {
			msg = msgs[0]
		}
		ce.CustomAbort(ce.NewContractError(ce.ErrArithmetic, msg))
	}
}

func logFee(asset string, magiFee, lpFee *big.Int) string {
	totalFee := new(big.Int).Add(magiFee, lpFee)
	var b strings.Builder
	b.Grow(64)

	// 2. Header
	b.WriteString("fee")
	b.WriteString(types.LogDelimiter)

	b.WriteString("a")
	b.WriteString(types.LogKeyDelimiter)
	b.WriteString(asset)
	b.WriteString(types.LogDelimiter)

	b.WriteString("t")
	b.WriteString(types.LogKeyDelimiter)
	b.WriteString(totalFee.String())
	b.WriteString(types.LogDelimiter)

	b.WriteString("m")
	b.WriteString(types.LogKeyDelimiter)
	b.WriteString(magiFee.String())
	b.WriteString(types.LogDelimiter)

	b.WriteString("lp")
	b.WriteString(types.LogKeyDelimiter)
	b.WriteString(lpFee.String())

	return b.String()
}

func logSwap(assetIn, assetOut string, amountIn, amountOut *big.Int, to string) string {
	var b strings.Builder
	b.Grow(64)

	b.WriteString("swap")
	b.WriteString(types.LogDelimiter)

	b.WriteString("in")
	b.WriteString(types.LogKeyDelimiter)
	b.WriteString(assetIn)
	b.WriteString(types.LogDelimiter)

	b.WriteString("out")
	b.WriteString(types.LogKeyDelimiter)
	b.WriteString(assetOut)
	b.WriteString(types.LogDelimiter)

	b.WriteString("ai")
	b.WriteString(types.LogKeyDelimiter)
	b.WriteString(amountIn.String())
	b.WriteString(types.LogDelimiter)

	b.WriteString("ao")
	b.WriteString(types.LogKeyDelimiter)
	b.WriteString(amountOut.String())
	b.WriteString(types.LogDelimiter)

	b.WriteString("to")
	b.WriteString(types.LogKeyDelimiter)
	b.WriteString(to)

	return b.String()
}

func logAddLiquidity(provider string, amt0, amt1, minted *big.Int) string {
	var b strings.Builder
	b.Grow(64)

	b.WriteString("add_liq")
	b.WriteString(types.LogDelimiter)

	b.WriteString("p")
	b.WriteString(types.LogKeyDelimiter)
	b.WriteString(provider)
	b.WriteString(types.LogDelimiter)

	b.WriteString("a0")
	b.WriteString(types.LogKeyDelimiter)
	b.WriteString(amt0.String())
	b.WriteString(types.LogDelimiter)

	b.WriteString("a1")
	b.WriteString(types.LogKeyDelimiter)
	b.WriteString(amt1.String())
	b.WriteString(types.LogDelimiter)

	b.WriteString("lp")
	b.WriteString(types.LogKeyDelimiter)
	b.WriteString(minted.String())

	return b.String()
}

func logRemoveLiquidity(provider string, amt0, amt1, lpAmount *big.Int) string {
	var b strings.Builder
	b.Grow(64)

	b.WriteString("rem_liq")
	b.WriteString(types.LogDelimiter)

	b.WriteString("p")
	b.WriteString(types.LogKeyDelimiter)
	b.WriteString(provider)
	b.WriteString(types.LogDelimiter)

	b.WriteString("a0")
	b.WriteString(types.LogKeyDelimiter)
	b.WriteString(amt0.String())
	b.WriteString(types.LogDelimiter)

	b.WriteString("a1")
	b.WriteString(types.LogKeyDelimiter)
	b.WriteString(amt1.String())
	b.WriteString(types.LogDelimiter)

	b.WriteString("lp")
	b.WriteString(types.LogKeyDelimiter)
	b.WriteString(lpAmount.String())

	return b.String()
}

func logPoolInit(asset0, asset1 string, feeBps uint64) string {
	var b strings.Builder
	b.Grow(64)

	b.WriteString("pool_init")
	b.WriteString(types.LogDelimiter)

	b.WriteString("a0")
	b.WriteString(types.LogKeyDelimiter)
	b.WriteString(asset0)
	b.WriteString(types.LogDelimiter)

	b.WriteString("a1")
	b.WriteString(types.LogKeyDelimiter)
	b.WriteString(asset1)
	b.WriteString(types.LogDelimiter)

	b.WriteString("fee")
	b.WriteString(types.LogKeyDelimiter)
	b.WriteString(strconv.FormatUint(feeBps, 10))

	return b.String()
}

func logMigrate(version, asset0, asset1 string) string {
	var b strings.Builder
	b.Grow(64)

	b.WriteString("migrate")
	b.WriteString(types.LogDelimiter)

	b.WriteString("v")
	b.WriteString(types.LogKeyDelimiter)
	b.WriteString(version)
	b.WriteString(types.LogDelimiter)

	b.WriteString("a0")
	b.WriteString(types.LogKeyDelimiter)
	b.WriteString(asset0)
	b.WriteString(types.LogDelimiter)

	b.WriteString("a1")
	b.WriteString(types.LogKeyDelimiter)
	b.WriteString(asset1)

	return b.String()
}
