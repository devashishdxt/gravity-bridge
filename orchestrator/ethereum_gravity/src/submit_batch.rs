use crate::utils::{EthSignerMiddleware, GasCost, get_tx_batch_nonce, set_contract_call_gas_for_estimate};
use ethers::contract::builders::ContractCall;
use ethers::prelude::*;
use ethers::types::Address as EthAddress;
use gravity_abi::gravity::*;
use gravity_utils::error::GravityError;
use gravity_utils::message_signatures::encode_tx_batch_confirm_hashed;
use gravity_utils::types::*;
use web30::types::SendTxOption;
use std::ops::Add;
use std::{cmp::min, time::Duration};
use web30::{client::Web3, types::TransactionRequest};

/// this function generates an appropriate Ethereum transaction
/// to submit the provided transaction batch
#[allow(clippy::too_many_arguments)]
pub async fn send_eth_transaction_batch(
    current_valset: Valset,
    batch: TransactionBatch,
    confirms: &[BatchConfirmResponse],
    timeout: Duration,
    gravity_contract_address: EthAddress,
    gravity_id: String,
    options: Vec<SendTxOption>,
    eth_client: EthClient,
) -> Result<(), GravityError> {
    let new_batch_nonce = batch.nonce;
    let eth_address = eth_client.address();
    info!(
        "Ordering signatures and submitting TransactionBatch {}:{} to Ethereum",
        batch.token_contract, new_batch_nonce
    );
    trace!("Batch {:?}", batch);

    let before_nonce = get_tx_batch_nonce(
        gravity_contract_address,
        batch.token_contract,
        eth_address,
        eth_client,
    )
    .await?;

    let current_block_height = eth_client.get_block_number().await?;
    if before_nonce >= new_batch_nonce {
        info!(
            "Someone else updated the batch to {}, exiting early",
            before_nonce
        );
        return Ok(());
    } else if current_block_height > batch.batch_timeout.into() {
        info!(
            "This batch is timed out. timeout block: {} current block: {}, exiting early",
            current_block_height, batch.batch_timeout
        );
        return Ok(());
    }

    let contract_call = build_submit_batch_contract_call(
        current_valset, batch, confirms, gravity_contract_address, gravity_id, eth_client
    );
    // TODO(bolten): we need to implement the gas multiplier being passed as a TxOption
    let pending_tx = contract_call.send().await?;
    info!("Sent batch update with txid {:#066x}", tx);
    // TODO(bolten): ethers interval default is 7s, this mirrors what web30 was doing, should we adjust?
    // additionally we are mirroring only waiting for 1 confirmation by leaving that as default
    pending_tx.interval(Duration::from_secs(1));

    if let Err(tx_error) = tokio::time::timeout(timeout, async { pending_tx.await? }).await {
        return Err(tx_error);
    };

    let last_nonce = get_tx_batch_nonce(
        gravity_contract_address,
        batch.token_contract,
        eth_address,
        eth_client,
    )
    .await?;

    if last_nonce != new_batch_nonce {
        error!(
            "Current nonce is {} expected to update to nonce {}",
            last_nonce, new_batch_nonce
        );
    } else {
        info!("Successfully updated Batch with new Nonce {:?}", last_nonce);
    }

    Ok(())
}

/// Returns the cost in Eth of sending this batch
pub async fn estimate_tx_batch_cost(
    current_valset: Valset,
    batch: TransactionBatch,
    confirms: &[BatchConfirmResponse],
    gravity_contract_address: EthAddress,
    gravity_id: String,
    eth_client: EthClient,
) -> Result<GasCost, GravityError> {
    let contract_call = build_submit_batch_contract_call(
        current_valset, batch, confirms, gravity_contract_address, gravity_id, eth_client
    );
    let contract_call = set_contract_call_gas_for_estimate(contract_call, eth_client);

    Ok(GasCost {
        gas: contract_call.estimate_gas().await?,
        gas_price
    })
}

pub fn build_submit_batch_contract_call(
    current_valset: Valset,
    batch: TransactionBatch,
    confirms: &[BatchConfirmResponse],
    gravity_contract_address: EthAddress,
    gravity_id: String,
    eth_client: EthClient,
) -> Result<ContractCall<EthSignerMiddleware, ()>, GravityError> {
    let (current_addresses, current_powers) = current_valset.filter_empty_addresses();
    let current_valset_nonce = current_valset.nonce;
    let new_batch_nonce = batch.nonce;
    let hash = encode_tx_batch_confirm_hashed(gravity_id, batch.clone());
    let sig_data = current_valset.order_sigs(&hash, confirms)?;
    let sig_arrays = to_arrays(sig_data);
    let (amounts, destinations, fees) = batch.get_checkpoint_values();

    let contract = Gravity::new(gravity_contract_address, eth_client);
    Ok(contract.submit_batch(
        current_addresses, current_powers.into(), current_valset_nonce.into(),
        sig_arrays.v, sig_arrays.r, sig_arrays.s,
        amounts, destinations, fees,
        new_batch_nonce.into(), batch.token_contract, batch.batch_timeout.into()
        .from(eth_client.address())
        .value(0u8.into())))
}
