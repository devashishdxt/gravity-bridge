package keeper

import (
	"encoding/binary"
	"fmt"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	"strconv"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/ethereum/go-ethereum/common"

	"github.com/peggyjv/gravity-bridge/module/v2/x/gravity/types"
)

// BuildBatchTx starts the following process chain:
// - find bridged denominator for given voucher type
// - determine if a an unexecuted batch is already waiting for this token type, if so confirm the new batch would
//   have a higher total fees. If not exit withtout creating a batch
// - select available transactions from the outgoing transaction pool sorted by fee desc
// - persist an outgoing batch object with an incrementing ID = nonce
// - emit an event
func (k Keeper) BuildBatchTx(ctx sdk.Context, contractAddress common.Address, maxElements int) *types.BatchTx {
	if maxElements == 0 {
		return nil
	}
	// if there is a more profitable batch for this token type do not create a new batch
	if lastBatch := k.getLastOutgoingBatchByTokenType(ctx, contractAddress); lastBatch != nil {
		if lastBatch.GetFees().GTE(k.getBatchFeesByTokenType(ctx, contractAddress, maxElements)) {
			return nil
		}
	}

	var selectedStes []*types.SendToEthereum
	k.iterateUnbatchedSendToEthereumsByContract(ctx, contractAddress, func(ste *types.SendToEthereum) bool {
		selectedStes = append(selectedStes, ste)
		k.deleteUnbatchedSendToEthereum(ctx, ste.Id, ste.Erc20Fee)
		return len(selectedStes) == maxElements
	})

	// do not create batches that would contain no transactions, even if they are requested
	if len(selectedStes) == 0 {
		return nil
	}

	batch := &types.BatchTx{
		BatchNonce:    k.incrementLastOutgoingBatchNonce(ctx),
		Timeout:       k.getTimeoutHeight(ctx),
		Transactions:  selectedStes,
		TokenContract: contractAddress.Hex(),
		Height:        uint64(ctx.BlockHeight()),
	}
	k.SetOutgoingTx(ctx, batch)

	ctx.EventManager().EmitEvent(sdk.NewEvent(
		types.EventTypeOutgoingBatch,
		sdk.NewAttribute(sdk.AttributeKeyModule, types.ModuleName),
		sdk.NewAttribute(types.AttributeKeyContract, k.getBridgeContractAddress(ctx)),
		sdk.NewAttribute(types.AttributeKeyBridgeChainID, strconv.Itoa(int(k.getBridgeChainID(ctx)))),
		sdk.NewAttribute(types.AttributeKeyOutgoingBatchID, fmt.Sprint(batch.BatchNonce)),
		sdk.NewAttribute(types.AttributeKeyNonce, fmt.Sprint(batch.BatchNonce)),
	))

	return batch
}

// batchTxExecuted is run when the Cosmos chain detects that a batch has been executed on Ethereum
// It deletes all the transactions in the batch, then cancels all earlier batches
func (k Keeper) batchTxExecuted(ctx sdk.Context, tokenContract common.Address, nonce uint64) error {
	otx := k.GetOutgoingTx(ctx, types.MakeBatchTxKey(tokenContract, nonce))
	if otx == nil {
		k.Logger(ctx).Error("Failed to clean batches",
			"token contract", tokenContract.Hex(),
			"nonce", nonce)
		return nil
	}
	batchTx, _ := otx.(*types.BatchTx)
	k.IterateOutgoingTxsByType(ctx, types.BatchTxPrefixByte, func(key []byte, otx types.OutgoingTx) bool {
		// If the iterated batches nonce is lower than the one that was just executed, cancel it
		btx, _ := otx.(*types.BatchTx)
		if (btx.BatchNonce < batchTx.BatchNonce) && (btx.TokenContract == batchTx.TokenContract) {
			k.CancelBatchTx(ctx, btx)
		}
		return false
	})

	// burn the amount for non cosmos originated asset
	isCosmosOriginated, denom := k.ERC20ToDenomLookup(ctx, common.HexToAddress(batchTx.TokenContract))
	if !isCosmosOriginated {
		totalToBurn := sdk.NewInt(0)
		for _, tx := range batchTx.Transactions {
			// sanity check
			if tx.Erc20Token.Contract != batchTx.TokenContract || tx.Erc20Fee.Contract != batchTx.TokenContract {
				return sdkerrors.Wrapf(types.ErrInvalid, "detected invalid batch, contains tx with different contract address")
			}
			totalToBurn = totalToBurn.Add(tx.Erc20Token.Amount.Add(tx.Erc20Fee.Amount))
		}
		burnVouchers := sdk.NewCoins(sdk.NewCoin(denom, totalToBurn))
		if err := k.bankKeeper.BurnCoins(ctx, types.ModuleName, burnVouchers); err != nil {
			return sdkerrors.Wrapf(err, "burn vouchers coins: %s", burnVouchers)
		}
	}

	k.DeleteOutgoingTx(ctx, batchTx.GetStoreIndex())
	return nil
}

// getBatchFeesByTokenType gets the fees the next batch of a given token type would
// have if created. This info is both presented to relayers for the purpose of determining
// when to request batches and also used by the batch creation process to decide not to create
// a new batch
func (k Keeper) getBatchFeesByTokenType(ctx sdk.Context, tokenContractAddr common.Address, maxElements int) sdk.Int {
	feeAmount := sdk.ZeroInt()
	i := 0
	k.iterateUnbatchedSendToEthereumsByContract(ctx, tokenContractAddr, func(tx *types.SendToEthereum) bool {
		feeAmount = feeAmount.Add(tx.Erc20Fee.Amount)
		i++
		return i == maxElements
	})

	return feeAmount
}

// GetBatchFeesByTokenType gets the fees the next batch of a given token type would
// have if created. This info is both presented to relayers for the purpose of determining
// when to request batches and also used by the batch creation process to decide not to create
// a new batch
func (k Keeper) GetBatchFeesByTokenType(ctx sdk.Context, tokenContractAddr common.Address, maxElements int) sdk.Int {
	feeAmount := sdk.ZeroInt()
	i := 0
	k.iterateUnbatchedSendToEthereumsByContract(ctx, tokenContractAddr, func(tx *types.SendToEthereum) bool {
		feeAmount = feeAmount.Add(tx.Erc20Fee.Amount)
		i++
		return i == maxElements
	})
	return feeAmount
}

// CancelBatchTx releases all TX in the batch and deletes the batch
func (k Keeper) CancelBatchTx(ctx sdk.Context, batch *types.BatchTx) {
	// free transactions from batch and reindex them
	for _, tx := range batch.Transactions {
		k.setUnbatchedSendToEthereum(ctx, tx)
	}

	// Delete batch since it is finished
	k.DeleteOutgoingTx(ctx, batch.GetStoreIndex())

	ctx.EventManager().EmitEvent(
		sdk.NewEvent(
			types.EventTypeOutgoingBatchCanceled,
			sdk.NewAttribute(sdk.AttributeKeyModule, types.ModuleName),
			sdk.NewAttribute(types.AttributeKeyContract, k.getBridgeContractAddress(ctx)),
			sdk.NewAttribute(types.AttributeKeyBridgeChainID, strconv.Itoa(int(k.getBridgeChainID(ctx)))),
			sdk.NewAttribute(types.AttributeKeyOutgoingBatchID, fmt.Sprint(batch.BatchNonce)),
			sdk.NewAttribute(types.AttributeKeyNonce, fmt.Sprint(batch.BatchNonce)),
		),
	)
}

// getLastOutgoingBatchByTokenType gets the latest outgoing tx batch by token type
func (k Keeper) getLastOutgoingBatchByTokenType(ctx sdk.Context, token common.Address) *types.BatchTx {
	var lastBatch *types.BatchTx = nil
	lastNonce := uint64(0)
	k.IterateOutgoingTxsByType(ctx, types.BatchTxPrefixByte, func(key []byte, otx types.OutgoingTx) bool {
		btx, _ := otx.(*types.BatchTx)
		if common.HexToAddress(btx.TokenContract) == token && btx.BatchNonce > lastNonce {
			lastBatch = btx
			lastNonce = btx.BatchNonce
		}
		return false
	})
	return lastBatch
}

// SetLastSlashedOutgoingTxBlockHeight sets the latest slashed Batch block height
func (k Keeper) SetLastSlashedOutgoingTxBlockHeight(ctx sdk.Context, blockHeight uint64) {
	ctx.KVStore(k.storeKey).Set([]byte{types.LastSlashedOutgoingTxBlockKey}, sdk.Uint64ToBigEndian(blockHeight))
}

// GetLastSlashedOutgoingTxBlockHeight returns the latest slashed Batch block
func (k Keeper) GetLastSlashedOutgoingTxBlockHeight(ctx sdk.Context) uint64 {
	if bz := ctx.KVStore(k.storeKey).Get([]byte{types.LastSlashedOutgoingTxBlockKey}); bz == nil {
		return 0
	} else {
		return binary.BigEndian.Uint64(bz)
	}
}

func (k Keeper) GetUnSlashedOutgoingTxs(ctx sdk.Context, maxHeight uint64) (out []types.OutgoingTx) {
	lastSlashed := k.GetLastSlashedOutgoingTxBlockHeight(ctx)
	k.iterateOutgoingTxs(ctx, func(key []byte, otx types.OutgoingTx) bool {
		if (otx.GetCosmosHeight() < maxHeight) && (otx.GetCosmosHeight() > lastSlashed) {
			out = append(out, otx)
		}
		return false
	})
	return
}

func (k Keeper) incrementLastOutgoingBatchNonce(ctx sdk.Context) uint64 {
	store := ctx.KVStore(k.storeKey)
	bz := store.Get([]byte{types.LastOutgoingBatchNonceKey})
	var id uint64 = 0
	if bz != nil {
		id = binary.BigEndian.Uint64(bz)
	}
	newId := id + 1
	bz = sdk.Uint64ToBigEndian(newId)
	store.Set([]byte{types.LastOutgoingBatchNonceKey}, bz)
	return newId
}
