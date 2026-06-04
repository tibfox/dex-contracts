package sdk

import (
	"encoding/hex"
	"math/big"
	"strconv"

	tinyjson "github.com/CosmWasm/tinyjson"
)

// Aborts the contract execution
func Abort(msg string) {
	ln := int32(0)
	abort(&msg, nil, &ln, &ln)
	panic(msg)
}

// Reverts the transaction and abort execution in the same way as Abort().
func Revert(msg string, symbol string) {
	revert(&msg, &symbol)
}

// Set a value by key in the contract state
func StateSetObject(key string, value string) {
	stateSetObject(&key, &value)
}

// Get a value by key from the contract state
func StateGetObject(key string) *string {
	return stateGetObject(&key)
}

// Delete or unset a value by key in the contract state
func StateDeleteObject(key string) {
	stateDeleteObject(&key)
}

// Set a value by key in the ephemeral contract state
func EphemStateSetObject(key string, value string) {
	ephemStateSetObject(&key, &value)
}

// Get a value by key from the ephemeral contract state
func EphemStateGetObject(contractId string, key string) *string {
	return ephemStateGetObject(&contractId, &key)
}

// Delete or unset a value by key in the ephemeral contract state
func EphemStateDeleteObject(key string) {
	ephemStateDeleteObject(&key)
}

// Get current execution environment variables
// envAddrList converts an untyped env value into []Address, tolerating a
// missing key or a wrong element type instead of panicking (review7 M18).
func envAddrList(v interface{}) []Address {
	out := make([]Address, 0)
	list, ok := v.([]interface{})
	if !ok {
		return out
	}
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, Address(s))
		}
	}
	return out
}

func GetEnv() Env {
	envStr := *getEnv(nil)
	env := Env{}
	// envMap := map[string]interface{}{}
	tinyjson.Unmarshal([]byte(envStr), &env)
	envMap := EnvMap{}
	tinyjson.Unmarshal([]byte(envStr), &envMap)

	// review7 M18: bare type assertions here panicked (trapping the whole
	// contract — a DoS) if any env field was absent or the wrong type. Use
	// comma-ok and fail CLOSED: a malformed env yields empty auths / empty
	// sender, so auth-gated entrypoints deny rather than crash, while read-only
	// entrypoints keep working.
	requiredAuths := envAddrList(envMap["msg.required_auths"])
	requiredPostingAuths := envAddrList(envMap["msg.required_posting_auths"])

	sender, _ := envMap["msg.sender"].(string)
	env.Sender = Sender{
		Address:              Address(sender),
		RequiredAuths:        requiredAuths,
		RequiredPostingAuths: requiredPostingAuths,
	}

	// env.ContractId = envMap["contract.id"].(string)
	// env.Index = envMap["tx.index"].(int64)
	// env.OpIndex = envMap["tx.op_index"].(int64)

	// for _, v := range envMap {
	// 	switch v {
	// 	case "contract.id":
	// 		env.CONTRACT_ID = *_GET_ENV(&v)
	// 	case "tx.origin":
	// 		env.TX_ORIGIN = *_GET_ENV(&v)
	// 	case "tx.id":
	// 		env.TX_ID = *_GET_ENV(&v)
	// 	case "tx.index":
	// 		indexStr := *_GET_ENV(&v)
	// 		index, err := strconv.Atoi(indexStr)
	// 		if err != nil {
	// 			Log("Das broken: " + err.Error())
	// 			panic(fmt.Sprintf("Failed to parse index: %s", err))
	// 		}
	// 		env.INDEX = index
	// 	case "tx.op_index":
	// 		opIndexStr := *_GET_ENV(&v)
	// 		opIndex, err := strconv.Atoi(opIndexStr)
	// 		if err != nil {
	// 			panic(fmt.Sprintf("Failed to parse op_index: %s", err))
	// 		}
	// 		env.OP_INDEX = opIndex
	// 	case "block.id":
	// 		env.BLOCK_ID = *_GET_ENV(&v)
	// 	case "block.height":
	// 		heightStr := *_GET_ENV(&v)
	// 		height, err := strconv.ParseUint(heightStr, 10, 64)
	// 		if err != nil {
	// 			panic(fmt.Sprintf("Failed to parse block height: %s", err))
	// 		}
	// 		env.BLOCK_HEIGHT = height
	// 	case "block.timestamp":
	// 		env.TIMESTAMP = *_GET_ENV(&v)
	// 	default:
	// 		panic(fmt.Sprintf("Unknown environment variable: %s", v[0]))
	// 	}
	// }
	return env
}

// Get current execution environment variables as json string
func GetEnvStr() string {
	return *getEnv(nil)
}

// Get current execution environment variable by a key
func GetEnvKey(key string) *string {
	return getEnvKey(&key)
}

// VerifyAddress asks the runtime to validate an address and returns its type.
// Returns one of: "user:hive", "user:evm", "key", "contract", "system", "unknown".
func VerifyAddress(addr string) string {
	return *verifyAddress(&addr)
}

// Get balance of an account
func GetBalance(address Address, asset Asset) int64 {
	addr := address.String()
	as := asset.String()
	balStr := *getBalance(&addr, &as)
	bal, err := strconv.ParseInt(balStr, 10, 64)
	if err != nil {
		panic(err)
	}
	return bal
}

// Transfer assets from caller account to the contract up to the limit specified in `intents`. The transaction must be signed using active authority for Hive accounts.
func HiveDraw(amount *big.Int, asset Asset) {
	amt := amount.String()
	as := asset.String()
	hiveDraw(&amt, &as)
}

// Transfer assets from the contract to another account.
func HiveTransfer(to Address, amount *big.Int, asset Asset) {
	toaddr := to.String()
	amt := amount.String()
	as := asset.String()
	hiveTransfer(&toaddr, &amt, &as)
}

// Unmap assets from the contract to a specified Hive account.
func HiveWithdraw(to Address, amount *big.Int, asset Asset) {
	toaddr := to.String()
	amt := amount.String()
	as := asset.String()
	hiveWithdraw(&toaddr, &amt, &as)
}

// Get a value by key from the contract state of another contract
func ContractStateGet(contractId string, key string) *string {
	return contractRead(&contractId, &key)
}

// Call another contract
func ContractCall(contractId string, method string, payload string, options *ContractCallOptions) *string {
	optStr := ""
	if options != nil {
		optByte, err := tinyjson.Marshal(options)
		if err != nil {
			Revert("could not serialize options", "sdk_error")
		}
		optStr = string(optByte)
	}
	return contractCall(&contractId, &method, &payload, &optStr)
}

func TssCreateKey(keyId string, algo string) string {
	if algo != "ecdsa" && algo != "eddsa" {
		Abort("algo must be ecdsa or eddsa")
	}

	return *tssCreateKey(&keyId, &algo)
}

func TssGetKey(keyId string) string {
	return *tssGetKey(&keyId)
}

func TssSignKey(keyId string, bytes []byte) {
	byteStr := hex.EncodeToString(bytes)

	tssSignKey(&keyId, &byteStr)
}
