package keeper

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/cosmos/cosmos-sdk/codec"
	cdctypes "github.com/cosmos/cosmos-sdk/codec/types"
	"github.com/cosmos/cosmos-sdk/store/prefix"
	storetypes "github.com/cosmos/cosmos-sdk/store/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/query"
	paramtypes "github.com/cosmos/cosmos-sdk/x/params/types"
	stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
	"github.com/ethereum/go-ethereum/common"
	tmbytes "github.com/tendermint/tendermint/libs/bytes"
	"github.com/tendermint/tendermint/libs/log"

	"github.com/peggyjv/gravity-bridge/module/v2/x/gravity/types"
)

// Keeper maintains the link to storage and exposes getter/setter methods for the various parts of the state machine
type Keeper struct {
	StakingKeeper          types.StakingKeeper
	storeKey               storetypes.StoreKey
	paramSpace             paramtypes.Subspace
	cdc                    codec.Codec
	accountKeeper          types.AccountKeeper
	bankKeeper             types.BankKeeper
	SlashingKeeper         types.SlashingKeeper
	DistributionKeeper     types.DistributionKeeper
	PowerReduction         sdk.Int
	hooks                  types.GravityHooks
	ReceiverModuleAccounts map[string]string
	SenderModuleAccounts   map[string]string
}

// NewKeeper returns a new instance of the gravity keeper
func NewKeeper(
	cdc codec.Codec,
	storeKey storetypes.StoreKey,
	paramSpace paramtypes.Subspace,
	accKeeper types.AccountKeeper,
	stakingKeeper types.StakingKeeper,
	bankKeeper types.BankKeeper,
	slashingKeeper types.SlashingKeeper,
	distributionKeeper types.DistributionKeeper,
	powerReduction sdk.Int,
	receiverModuleAccounts map[string]string,
	senderModuleAccounts map[string]string,
) Keeper {
	// set KeyTable if it has not already been set
	if !paramSpace.HasKeyTable() {
		paramSpace = paramSpace.WithKeyTable(types.ParamKeyTable())
	}

	k := Keeper{
		cdc:                    cdc,
		paramSpace:             paramSpace,
		storeKey:               storeKey,
		accountKeeper:          accKeeper,
		StakingKeeper:          stakingKeeper,
		bankKeeper:             bankKeeper,
		SlashingKeeper:         slashingKeeper,
		DistributionKeeper:     distributionKeeper,
		PowerReduction:         powerReduction,
		ReceiverModuleAccounts: receiverModuleAccounts,
		SenderModuleAccounts:   senderModuleAccounts,
	}

	return k
}

func (k Keeper) Logger(ctx sdk.Context) log.Logger {
	return ctx.Logger().With("module", "x/"+types.ModuleName)
}

/////////////////////////////
//     SignerSetTxNonce    //
/////////////////////////////

// incrementLatestSignerSetTxNonce sets the latest valset nonce
func (k Keeper) incrementLatestSignerSetTxNonce(ctx sdk.Context) uint64 {
	current := k.GetLatestSignerSetTxNonce(ctx)
	next := current + 1
	ctx.KVStore(k.storeKey).Set([]byte{types.LatestSignerSetTxNonceKey}, sdk.Uint64ToBigEndian(next))
	return next
}

// GetLatestSignerSetTxNonce returns the latest valset nonce
func (k Keeper) GetLatestSignerSetTxNonce(ctx sdk.Context) uint64 {
	if bz := ctx.KVStore(k.storeKey).Get([]byte{types.LatestSignerSetTxNonceKey}); bz != nil {
		return binary.BigEndian.Uint64(bz)
	}
	return 0
}

// GetLatestSignerSetTx returns the latest validator set in state
func (k Keeper) GetLatestSignerSetTx(ctx sdk.Context) *types.SignerSetTx {
	key := types.MakeSignerSetTxKey(k.GetLatestSignerSetTxNonce(ctx))
	otx := k.GetOutgoingTx(ctx, key)
	out, _ := otx.(*types.SignerSetTx)
	return out
}

//////////////////////////////
// LastUnbondingBlockHeight //
//////////////////////////////

// setLastUnbondingBlockHeight sets the last unbonding block height
func (k Keeper) setLastUnbondingBlockHeight(ctx sdk.Context, unbondingBlockHeight uint64) {
	ctx.KVStore(k.storeKey).Set([]byte{types.LastUnBondingBlockHeightKey}, sdk.Uint64ToBigEndian(unbondingBlockHeight))
}

// GetLastUnbondingBlockHeight returns the last unbonding block height
func (k Keeper) GetLastUnbondingBlockHeight(ctx sdk.Context) uint64 {
	if bz := ctx.KVStore(k.storeKey).Get([]byte{types.LastUnBondingBlockHeightKey}); len(bz) == 0 {
		return 0
	} else {
		return binary.BigEndian.Uint64(bz)
	}
}

///////////////////////////////
//     ETHEREUM SIGNATURES   //
///////////////////////////////

// getEthereumSignature returns a valset confirmation by a nonce and validator address
func (k Keeper) getEthereumSignature(ctx sdk.Context, storeIndex []byte, validator sdk.ValAddress) []byte {
	return ctx.KVStore(k.storeKey).Get(types.MakeEthereumSignatureKey(storeIndex, validator))
}

// SetEthereumSignature sets a valset confirmation
func (k Keeper) SetEthereumSignature(ctx sdk.Context, sig types.EthereumTxConfirmation, val sdk.ValAddress) []byte {
	key := types.MakeEthereumSignatureKey(sig.GetStoreIndex(), val)
	ctx.KVStore(k.storeKey).Set(key, sig.GetSignature())
	return key
}

// GetEthereumSignatures returns all etherum signatures for a given outgoing tx by store index
func (k Keeper) GetEthereumSignatures(ctx sdk.Context, storeIndex []byte) map[string][]byte {
	var signatures = make(map[string][]byte)
	k.iterateEthereumSignatures(ctx, storeIndex, func(val sdk.ValAddress, h []byte) bool {
		signatures[val.String()] = h
		return false
	})
	return signatures
}

// iterateEthereumSignatures iterates through all valset confirms by nonce in ASC order
func (k Keeper) iterateEthereumSignatures(ctx sdk.Context, storeIndex []byte, cb func(sdk.ValAddress, []byte) bool) {
	prefixStore := prefix.NewStore(ctx.KVStore(k.storeKey), append([]byte{types.EthereumSignatureKey}, storeIndex...))
	iter := prefixStore.Iterator(nil, nil)
	defer iter.Close()

	for ; iter.Valid(); iter.Next() {
		// cb returns true to stop early
		if cb(iter.Key(), iter.Value()) {
			break
		}
	}
}

/////////////////////////
//  ORC -> VAL ADDRESS //
/////////////////////////

// SetOrchestratorValidatorAddress sets the Orchestrator key for a given validator.
func (k Keeper) SetOrchestratorValidatorAddress(ctx sdk.Context, val sdk.ValAddress, orchAddr sdk.AccAddress) {
	store := ctx.KVStore(k.storeKey)
	key := types.MakeOrchestratorValidatorAddressKey(orchAddr)

	store.Set(key, val.Bytes())
}

// GetOrchestratorValidatorAddress returns the validator key associated with an
// orchestrator key.
func (k Keeper) GetOrchestratorValidatorAddress(ctx sdk.Context, orchAddr sdk.AccAddress) sdk.ValAddress {
	store := ctx.KVStore(k.storeKey)
	key := types.MakeOrchestratorValidatorAddressKey(orchAddr)

	return store.Get(key)
}

////////////////////////
// VAL -> ETH ADDRESS //
////////////////////////

// setValidatorEthereumAddress sets the ethereum address for a given validator
func (k Keeper) setValidatorEthereumAddress(ctx sdk.Context, valAddr sdk.ValAddress, ethAddr common.Address) {
	store := ctx.KVStore(k.storeKey)
	key := types.MakeValidatorEthereumAddressKey(valAddr)

	store.Set(key, ethAddr.Bytes())
}

// GetValidatorEthereumAddress returns the eth address for a given gravity validator.
func (k Keeper) GetValidatorEthereumAddress(ctx sdk.Context, valAddr sdk.ValAddress) common.Address {
	store := ctx.KVStore(k.storeKey)
	key := types.MakeValidatorEthereumAddressKey(valAddr)

	return common.BytesToAddress(store.Get(key))
}

func (k Keeper) getValidatorsByEthereumAddress(ctx sdk.Context, ethAddr common.Address) (vals []sdk.ValAddress) {
	iter := ctx.KVStore(k.storeKey).Iterator(nil, nil)

	for ; iter.Valid(); iter.Next() {
		if common.BytesToAddress(iter.Value()) == ethAddr {
			valBs := bytes.TrimPrefix(iter.Key(), []byte{types.ValidatorEthereumAddressKey})
			val := sdk.ValAddress(valBs)
			vals = append(vals, val)
		}
	}

	return
}

////////////////////////
// ETH -> ORC ADDRESS //
////////////////////////

// setEthereumOrchestratorAddress sets the eth orch addr mapping
func (k Keeper) setEthereumOrchestratorAddress(ctx sdk.Context, ethAddr common.Address, orch sdk.AccAddress) {
	store := ctx.KVStore(k.storeKey)
	key := types.MakeEthereumOrchestratorAddressKey(ethAddr)

	store.Set(key, orch.Bytes())
}

// GetEthereumOrchestratorAddress gets the orch address for a given eth address
func (k Keeper) GetEthereumOrchestratorAddress(ctx sdk.Context, ethAddr common.Address) sdk.AccAddress {
	store := ctx.KVStore(k.storeKey)
	key := types.MakeEthereumOrchestratorAddressKey(ethAddr)

	return store.Get(key)
}

func (k Keeper) getEthereumAddressesByOrchestrator(ctx sdk.Context, orch sdk.AccAddress) (ethAddrs []common.Address) {
	iter := ctx.KVStore(k.storeKey).Iterator(nil, nil)

	for ; iter.Valid(); iter.Next() {
		if sdk.AccAddress(iter.Value()).String() == orch.String() {
			ethBs := bytes.TrimPrefix(iter.Key(), []byte{types.EthereumOrchestratorAddressKey})
			ethAddr := common.BytesToAddress(ethBs)
			ethAddrs = append(ethAddrs, ethAddr)
		}
	}

	return
}

// CreateSignerSetTx gets the current signer set from the staking keeper, increments the nonce,
// creates the signer set tx object, emits an event and sets the signer set in state
func (k Keeper) CreateSignerSetTx(ctx sdk.Context) *types.SignerSetTx {
	nonce := k.incrementLatestSignerSetTxNonce(ctx)
	currSignerSet := k.CurrentSignerSet(ctx)
	newSignerSetTx := types.NewSignerSetTx(nonce, uint64(ctx.BlockHeight()), currSignerSet)

	ctx.EventManager().EmitEvent(
		sdk.NewEvent(
			types.EventTypeMultisigUpdateRequest,
			sdk.NewAttribute(sdk.AttributeKeyModule, types.ModuleName),
			sdk.NewAttribute(types.AttributeKeyContract, k.getBridgeContractAddress(ctx)),
			sdk.NewAttribute(types.AttributeKeyBridgeChainID, strconv.Itoa(int(k.getBridgeChainID(ctx)))),
			sdk.NewAttribute(types.AttributeKeySignerSetNonce, fmt.Sprint(nonce)),
		),
	)
	k.SetOutgoingTx(ctx, newSignerSetTx)
	k.Logger(ctx).Info(
		"SignerSetTx created",
		"nonce", newSignerSetTx.Nonce,
		"height", newSignerSetTx.Height,
		"signers", len(newSignerSetTx.Signers),
	)
	return newSignerSetTx
}

// CurrentSignerSet gets powers from the store and normalizes them
// into an integer percentage with a resolution of uint32 Max meaning
// a given validators 'gravity power' is computed as
// Cosmos power / total cosmos power = x / uint32 Max
// where x is the voting power on the gravity contract. This allows us
// to only use integer division which produces a known rounding error
// from truncation equal to the ratio of the validators
// Cosmos power / total cosmos power ratio, leaving us at uint32 Max - 1
// total voting power. This is an acceptable rounding error since floating
// point may cause consensus problems if different floating point unit
// implementations are involved.
func (k Keeper) CurrentSignerSet(ctx sdk.Context) types.EthereumSigners {
	validators := k.StakingKeeper.GetBondedValidatorsByPower(ctx)
	ethereumSigners := make([]*types.EthereumSigner, 0)
	var totalPower uint64
	for _, validator := range validators {
		val := validator.GetOperator()

		p := uint64(k.StakingKeeper.GetLastValidatorPower(ctx, val))

		if ethAddr := k.GetValidatorEthereumAddress(ctx, val); ethAddr.Hex() != "0x0000000000000000000000000000000000000000" {
			es := &types.EthereumSigner{Power: p, EthereumAddress: ethAddr.Hex()}
			ethereumSigners = append(ethereumSigners, es)
			totalPower += p
		}
	}
	// normalize power values
	for i := range ethereumSigners {
		ethereumSigners[i].Power = sdk.NewUint(ethereumSigners[i].Power).MulUint64(math.MaxUint32).QuoUint64(totalPower).Uint64()
	}

	return ethereumSigners
}

// GetSignerSetTxs returns all the signer set txs from the store
func (k Keeper) GetSignerSetTxs(ctx sdk.Context) (out []*types.SignerSetTx) {
	k.IterateOutgoingTxsByType(ctx, types.SignerSetTxPrefixByte, func(_ []byte, otx types.OutgoingTx) bool {
		sstx, _ := otx.(*types.SignerSetTx)
		out = append(out, sstx)
		return false
	})
	return
}

/////////////////////////////
//       PARAMETERS        //
/////////////////////////////

// GetParams returns the parameters from the store
func (k Keeper) GetParams(ctx sdk.Context) (params types.Params) {
	k.paramSpace.GetParamSet(ctx, &params)
	return
}

// SetParams sets the parameters in the store
func (k Keeper) SetParams(ctx sdk.Context, ps types.Params) {
	k.paramSpace.SetParamSet(ctx, &ps)
}

// getBridgeContractAddress returns the bridge contract address on ETH
func (k Keeper) getBridgeContractAddress(ctx sdk.Context) string {
	var a string
	k.paramSpace.Get(ctx, types.ParamsStoreKeyBridgeContractAddress, &a)
	return a
}

// getBridgeChainID returns the chain id of the ETH chain we are running against
func (k Keeper) getBridgeChainID(ctx sdk.Context) uint64 {
	var a uint64
	k.paramSpace.Get(ctx, types.ParamsStoreKeyBridgeContractChainID, &a)
	return a
}

// getGravityID returns the GravityID the GravityID is essentially a salt value
// for bridge signatures, provided each chain running Gravity has a unique ID
// it won't be possible to play back signatures from one bridge onto another
// even if they share a validator set.
//
// The lifecycle of the GravityID is that it is set in the Genesis file
// read from the live chain for the contract deployment, once a Gravity contract
// is deployed the GravityID CAN NOT BE CHANGED. Meaning that it can't just be the
// same as the chain id since the chain id may be changed many times with each
// successive chain in charge of the same bridge
func (k Keeper) getGravityID(ctx sdk.Context) string {
	var a string
	k.paramSpace.Get(ctx, types.ParamsStoreKeyGravityID, &a)
	return a
}

// getDelegateKeys iterates both the EthAddress and Orchestrator address indexes to produce
// a vector of MsgDelegateKeys entries containing all the delgate keys for state
// export / import. This may seem at first glance to be excessively complicated, why not combine
// the EthAddress and Orchestrator address indexes and simply iterate one thing? The answer is that
// even though we set the Eth and Orchestrator address in the same place we use them differently we
// always go from Orchestrator address to Validator address and from validator address to Ethereum address
// we want to keep looking up the validator address for various reasons, so a direct Orchestrator to Ethereum
// address mapping will mean having to keep two of the same data around just to provide lookups.
//
// For the time being this will serve
func (k Keeper) getDelegateKeys(ctx sdk.Context) (out []*types.MsgDelegateKeys) {
	store := ctx.KVStore(k.storeKey)
	iter := prefix.NewStore(store, []byte{types.ValidatorEthereumAddressKey}).Iterator(nil, nil)
	for ; iter.Valid(); iter.Next() {
		out = append(out, &types.MsgDelegateKeys{
			ValidatorAddress: sdk.ValAddress(iter.Key()).String(),
			EthereumAddress:  common.BytesToAddress(iter.Value()).Hex(),
		})
	}
	iter.Close()

	for _, msg := range out {
		msg.OrchestratorAddress = k.GetEthereumOrchestratorAddress(ctx, common.HexToAddress(msg.EthereumAddress)).String()
	}

	// we iterated over a map, so now we have to sort to ensure the
	// output here is deterministic, eth address chosen for no particular
	// reason
	sort.Slice(out[:], func(i, j int) bool {
		return out[i].EthereumAddress < out[j].EthereumAddress
	})

	return out
}

// GetUnbondingvalidators returns UnbondingValidators.
// Adding here in gravity keeper as cdc is available inside endblocker.
func (k Keeper) GetUnbondingvalidators(unbondingVals []byte) stakingtypes.ValAddresses {
	unbondingValidators := stakingtypes.ValAddresses{}
	k.cdc.MustUnmarshal(unbondingVals, &unbondingValidators)
	return unbondingValidators
}

// This gets the timeout height in Ethereum blocks for expiring old batches and contract calls.
func (k Keeper) getTimeoutHeight(ctx sdk.Context) uint64 {
	params := k.GetParams(ctx)
	currentCosmosHeight := ctx.BlockHeight()
	// we store the last observed Cosmos and Ethereum heights, we do not concern ourselves if these values are zero because
	// no batch can be produced if the last Ethereum block height is not first populated by a deposit event.
	heights := k.GetLastObservedEthereumBlockHeight(ctx)
	if heights.CosmosHeight == 0 || heights.EthereumHeight == 0 {
		return 0
	}
	// we project how long it has been in milliseconds since the last Ethereum block height was observed
	projectedMillis := (uint64(currentCosmosHeight) - heights.CosmosHeight) * params.AverageBlockTime
	// we convert that projection into the current Ethereum height using the average Ethereum block time in millis
	projectedCurrentEthereumHeight := (projectedMillis / params.AverageEthereumBlockTime) + heights.EthereumHeight
	// we convert our target time for block timeouts (lets say 12 hours) into a number of blocks to
	// place on top of our projection of the current Ethereum block height.
	blocksToAdd := params.TargetEthTxTimeout / params.AverageEthereumBlockTime
	return projectedCurrentEthereumHeight + blocksToAdd
}

/////////////////
// OUTGOING TX //
/////////////////

// GetOutgoingTx todo: outgoingTx prefix byte
func (k Keeper) GetOutgoingTx(ctx sdk.Context, storeIndex []byte) (out types.OutgoingTx) {
	if err := k.cdc.UnmarshalInterface(ctx.KVStore(k.storeKey).Get(types.MakeOutgoingTxKey(storeIndex)), &out); err != nil {
		panic(err)
	}
	return out
}

func (k Keeper) SetOutgoingTx(ctx sdk.Context, outgoing types.OutgoingTx) {
	any, err := types.PackOutgoingTx(outgoing)
	if err != nil {
		panic(err)
	}
	ctx.KVStore(k.storeKey).Set(
		types.MakeOutgoingTxKey(outgoing.GetStoreIndex()),
		k.cdc.MustMarshal(any),
	)
}

// DeleteOutgoingTx deletes a given outgoingtx
func (k Keeper) DeleteOutgoingTx(ctx sdk.Context, storeIndex []byte) {
	ctx.KVStore(k.storeKey).Delete(types.MakeOutgoingTxKey(storeIndex))
}

func (k Keeper) PaginateOutgoingTxsByType(ctx sdk.Context, pageReq *query.PageRequest, prefixByte byte, cb func(key []byte, outgoing types.OutgoingTx) bool) (*query.PageResponse, error) {
	prefixStore := prefix.NewStore(ctx.KVStore(k.storeKey), types.MakeOutgoingTxKey([]byte{prefixByte}))

	return query.FilteredPaginate(prefixStore, pageReq, func(key []byte, value []byte, accumulate bool) (bool, error) {
		if !accumulate {
			return false, nil
		}

		var any cdctypes.Any
		k.cdc.MustUnmarshal(value, &any)
		var otx types.OutgoingTx
		if err := k.cdc.UnpackAny(&any, &otx); err != nil {
			panic(err)
		}
		if accumulate {
			return cb(key, otx), nil
		}

		return false, nil
	})
}

// IterateOutgoingTxsByType iterates over a specific type of outgoing transaction denoted by the chosen prefix byte
func (k Keeper) IterateOutgoingTxsByType(ctx sdk.Context, prefixByte byte, cb func(key []byte, outgoing types.OutgoingTx) (stop bool)) {
	prefixStore := prefix.NewStore(ctx.KVStore(k.storeKey), types.MakeOutgoingTxKey([]byte{prefixByte}))
	iter := prefixStore.ReverseIterator(nil, nil)
	defer iter.Close()
	for ; iter.Valid(); iter.Next() {
		var any cdctypes.Any
		k.cdc.MustUnmarshal(iter.Value(), &any)
		var otx types.OutgoingTx
		if err := k.cdc.UnpackAny(&any, &otx); err != nil {
			panic(err)
		}
		if cb(iter.Key(), otx) {
			break
		}
	}
}

// iterateOutgoingTxs iterates over a specific type of outgoing transaction denoted by the chosen prefix byte
func (k Keeper) iterateOutgoingTxs(ctx sdk.Context, cb func(key []byte, outgoing types.OutgoingTx) bool) {
	prefixStore := prefix.NewStore(ctx.KVStore(k.storeKey), []byte{types.OutgoingTxKey})
	iter := prefixStore.ReverseIterator(nil, nil)
	defer iter.Close()
	for ; iter.Valid(); iter.Next() {
		var any cdctypes.Any
		k.cdc.MustUnmarshal(iter.Value(), &any)
		var otx types.OutgoingTx
		if err := k.cdc.UnpackAny(&any, &otx); err != nil {
			panic(err)
		}
		if cb(iter.Key(), otx) {
			break
		}
	}
}

// GetLastObservedSignerSetTx retrieves the last observed validator set from the store
func (k Keeper) GetLastObservedSignerSetTx(ctx sdk.Context) *types.SignerSetTx {
	key := []byte{types.LastObservedSignerSetKey}
	if val := ctx.KVStore(k.storeKey).Get(key); val != nil {
		var out types.SignerSetTx
		k.cdc.MustUnmarshal(val, &out)
		return &out
	}
	return nil
}

// setLastObservedSignerSetTx updates the last observed validator set in the stor e
func (k Keeper) setLastObservedSignerSetTx(ctx sdk.Context, signerSet types.SignerSetTx) {
	key := []byte{types.LastObservedSignerSetKey}
	ctx.KVStore(k.storeKey).Set(key, k.cdc.MustMarshal(&signerSet))
}

// CreateContractCallTx xxx
func (k Keeper) CreateContractCallTx(ctx sdk.Context, invalidationNonce uint64, invalidationScope tmbytes.HexBytes,
	address common.Address, payload []byte, tokens []types.ERC20Token, fees []types.ERC20Token) *types.ContractCallTx {
	params := k.GetParams(ctx)

	newContractCallTx := &types.ContractCallTx{
		InvalidationNonce: invalidationNonce,
		InvalidationScope: invalidationScope,
		Address:           address.String(),
		Payload:           payload,
		Timeout:           k.getTimeoutHeight(ctx),
		Tokens:            tokens,
		Fees:              fees,
		Height:            uint64(ctx.BlockHeight()),
	}

	var tokenString []string
	for _, token := range tokens {
		tokenString = append(tokenString, token.String())
	}

	var feeString []string
	for _, fee := range fees {
		feeString = append(feeString, fee.String())
	}

	ctx.EventManager().EmitEvent(
		sdk.NewEvent(
			types.EventTypeMultisigUpdateRequest,
			sdk.NewAttribute(sdk.AttributeKeyModule, types.ModuleName),
			sdk.NewAttribute(types.AttributeKeyContract, k.getBridgeContractAddress(ctx)),
			sdk.NewAttribute(types.AttributeKeyBridgeChainID, strconv.Itoa(int(k.getBridgeChainID(ctx)))),
			sdk.NewAttribute(types.AttributeKeyContractCallInvalidationNonce, fmt.Sprint(invalidationNonce)),
			sdk.NewAttribute(types.AttributeKeyContractCallInvalidationScope, fmt.Sprint(invalidationScope)),
			sdk.NewAttribute(types.AttributeKeyContractCallAddress, fmt.Sprint(address.String())),
			sdk.NewAttribute(types.AttributeKeyContractCallPayload, string(payload)),
			sdk.NewAttribute(types.AttributeKeyContractCallTokens, strings.Join(tokenString, "|")),
			sdk.NewAttribute(types.AttributeKeyContractCallFees, strings.Join(feeString, "|")),
			sdk.NewAttribute(types.AttributeKeyEthTxTimeout, strconv.FormatUint(params.TargetEthTxTimeout, 10)),
		),
	)
	k.SetOutgoingTx(ctx, newContractCallTx)
	k.Logger(ctx).Info(
		"ContractCallTx created",
		"bridge_contract", k.getBridgeContractAddress(ctx),
		"bridge_chain_id", strconv.Itoa(int(k.getBridgeChainID(ctx))),
		"invalidation_nonce", newContractCallTx.InvalidationNonce,
		"invalidation_scope", newContractCallTx.InvalidationScope,
		"address", address.String(),
		"payload", string(payload),
		"tokens", strings.Join(tokenString, "|"),
		"fees", strings.Join(feeString, "|"),
		"eth_tx_timeout", strconv.FormatUint(params.TargetEthTxTimeout, 10),
	)
	return newContractCallTx
}

//////////////////////////////////////
// Observed Ethereum/Cosmos heights //
//////////////////////////////////////

// GetEthereumHeightVoteRecord gets the latest observed heights per validator
func (k Keeper) GetEthereumHeightVote(ctx sdk.Context, valAddress sdk.ValAddress) types.LatestEthereumBlockHeight {
	store := ctx.KVStore(k.storeKey)
	key := types.MakeEthereumHeightVoteKey(valAddress)
	bytes := store.Get(key)

	if len(bytes) == 0 {
		return types.LatestEthereumBlockHeight{
			CosmosHeight:   0,
			EthereumHeight: 0,
		}
	}

	height := types.LatestEthereumBlockHeight{}
	k.cdc.MustUnmarshal(bytes, &height)
	return height
}

// SetEthereumHeightVoteRecord sets the latest observed heights per validator
func (k Keeper) SetEthereumHeightVote(ctx sdk.Context, valAddress sdk.ValAddress, ethereumHeight uint64) {
	store := ctx.KVStore(k.storeKey)
	height := types.LatestEthereumBlockHeight{
		EthereumHeight: ethereumHeight,
		CosmosHeight:   uint64(ctx.BlockHeight()),
	}
	key := types.MakeEthereumHeightVoteKey(valAddress)
	store.Set(key, k.cdc.MustMarshal(&height))
}

func (k Keeper) IterateEthereumHeightVotes(ctx sdk.Context, cb func(val sdk.ValAddress, height types.LatestEthereumBlockHeight) (stop bool)) {
	store := ctx.KVStore(k.storeKey)
	iter := sdk.KVStorePrefixIterator(store, []byte{types.EthereumHeightVoteKey})
	defer iter.Close()

	for ; iter.Valid(); iter.Next() {
		var height types.LatestEthereumBlockHeight
		key := bytes.NewBuffer(bytes.TrimPrefix(iter.Key(), []byte{types.EthereumHeightVoteKey}))
		val := sdk.ValAddress(key.Next(20))

		k.cdc.MustUnmarshal(iter.Value(), &height)
		if cb(val, height) {
			break
		}
	}
}

// DeleteEthereumSignatures deletes the ethereum signatures for a specific outgoing tx
func (k Keeper) DeleteEthereumSignatures(ctx sdk.Context, otx types.OutgoingTx) {
	prefixStoreSig := prefix.NewStore(ctx.KVStore(k.storeKey), append([]byte{types.EthereumSignatureKey}, otx.GetStoreIndex()...))
	iterSig := prefixStoreSig.Iterator(nil, nil)
	defer iterSig.Close()

	for ; iterSig.Valid(); iterSig.Next() {
		prefixStoreSig.Delete(iterSig.Key())
	}
}

/////////////////
// MIGRATE     //
/////////////////

// Clean up all state associated a previous gravity contract and set a new contract. This is intended to run in the upgrade handler.
// This implementation is partial at best. It doees not contain necessary functionality to freeze the bridge.
// We will have yet to implement functionality to Migrate the Cosmos ERC20 tokens or any other ERC20 tokens bridged to the gravity contracts.
// This just does keeper state cleanup if a new gravity contract has been deployed
func (k Keeper) MigrateGravityContract(ctx sdk.Context, newBridgeAddress string, bridgeDeploymentHeight uint64) {
	// Delete Any Outgoing TXs.

	prefixStoreOtx := prefix.NewStore(ctx.KVStore(k.storeKey), []byte{types.OutgoingTxKey})
	iterOtx := prefixStoreOtx.ReverseIterator(nil, nil)
	defer iterOtx.Close()
	for ; iterOtx.Valid(); iterOtx.Next() {

		var any cdctypes.Any
		k.cdc.MustUnmarshal(iterOtx.Value(), &any)
		var otx types.OutgoingTx
		if err := k.cdc.UnpackAny(&any, &otx); err != nil {
			panic(err)
		}
		// Delete any partial Eth Signatures handging around
		k.DeleteEthereumSignatures(ctx, otx)

		prefixStoreOtx.Delete(iterOtx.Key())
	}

	// Reset the last observed signer set nonce
	store := ctx.KVStore(k.storeKey)
	store.Set([]byte{types.LatestSignerSetTxNonceKey}, sdk.Uint64ToBigEndian(0))

	// Reset all ethereum event nonces to zero
	k.setLastObservedEventNonce(ctx, 0)
	k.iterateEthereumEventVoteRecords(ctx, func(_ []byte, voteRecord *types.EthereumEventVoteRecord) bool {
		for _, vote := range voteRecord.Votes {
			val, err := sdk.ValAddressFromBech32(vote)

			if err != nil {
				panic(err)
			}

			k.setLastEventNonceByValidator(ctx, val, 0)
		}

		return false
	})

	// Delete all Ethereum Events
	prefixStoreEthereumEvent := prefix.NewStore(ctx.KVStore(k.storeKey), []byte{types.EthereumEventVoteRecordKey})
	iterEvent := prefixStoreEthereumEvent.Iterator(nil, nil)
	defer iterEvent.Close()
	for ; iterEvent.Valid(); iterEvent.Next() {
		prefixStoreEthereumEvent.Delete(iterEvent.Key())
	}

	// Set the Last oberved Ethereum Blockheight to zero
	height := types.LatestEthereumBlockHeight{
		EthereumHeight: (bridgeDeploymentHeight - 1),
		CosmosHeight:   uint64(ctx.BlockHeight()),
	}

	store.Set([]byte{types.LastEthereumBlockHeightKey}, k.cdc.MustMarshal(&height))

	k.setLastObservedSignerSetTx(ctx, types.SignerSetTx{
		Nonce:   0,
		Height:  0,
		Signers: nil,
	})

	// Set the batch Nonce to zero
	store.Set([]byte{types.LastOutgoingBatchNonceKey}, sdk.Uint64ToBigEndian(0))

	// Update the bridge contract address
	params := k.GetParams(ctx)
	params.BridgeEthereumAddress = newBridgeAddress
	k.SetParams(ctx, params)
}

// DisableBridge disable the bridge processing all outgoing and ingoing transactions
func (k Keeper) DisableBridge(ctx sdk.Context) {
	gravityParam := k.GetParams(ctx)
	gravityParam.BridgeActive = false
	k.SetParams(ctx, gravityParam)

	k.Logger(ctx).Info("BridgeActivate is set to false")
}
