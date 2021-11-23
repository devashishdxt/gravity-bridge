use crate::{application::APP, prelude::*};
use abscissa_core::{Clap, Command, Runnable};
use ethereum_gravity::deploy_erc20::deploy_erc20;
use ethers::prelude::*;
use gravity_proto::gravity::{DenomToErc20ParamsRequest, DenomToErc20Request};
use gravity_utils::connection_prep::{check_for_eth, create_rpc_connections};
use std::convert::TryFrom;
use std::process::exit;
use std::{sync::Arc, time::Duration};
use tokio::time::sleep as delay_for;

/// Deploy Erc20
#[derive(Command, Debug, Clap)]
pub struct Erc20 {
    args: Vec<String>,

    #[clap(short, long)]
    ethereum_key: String,
}

impl Runnable for Erc20 {
    fn run(&self) {
        abscissa_tokio::run_with_actix(&APP, async {
            self.deploy().await;
        })
        .unwrap_or_else(|e| {
            status_err!("executor exited with error: {}", e);
            exit(1);
        });
    }
}

impl Erc20 {
    async fn deploy(&self) {
        let denom = self.args.get(0).expect("denom is required");

        let config = APP.config();

        let ethereum_wallet = config.load_ethers_wallet(self.ethereum_key.clone());
        let contract_address = config
            .gravity
            .contract
            .parse()
            .expect("Could not parse gravity contract address");

        let timeout = Duration::from_secs(500);
        let connections = create_rpc_connections(
            config.cosmos.prefix.clone(),
            Some(config.cosmos.grpc.clone()),
            Some(config.ethereum.rpc.clone()),
            timeout,
        )
        .await;

        let mut grpc = connections.grpc.clone().unwrap();
        let eth_client = SignerMiddleware::new(
            connections.eth_provider.clone().unwrap(),
            ethereum_wallet.clone(),
        );
        let eth_client = Arc::new(eth_client);

        check_for_eth(eth_client.address(), eth_client.clone()).await;

        let req = DenomToErc20ParamsRequest {
            denom: denom.clone(),
        };

        let res = grpc
            .denom_to_erc20_params(req)
            .await
            .expect("Couldn't get erc-20 params")
            .into_inner();

        println!("Starting deploy of ERC20");

        let res = deploy_erc20(
            res.base_denom,
            res.erc20_name,
            res.erc20_symbol,
            u8::try_from(res.erc20_decimals).unwrap(),
            contract_address,
            Some(timeout),
            eth_client.clone(),
        )
        .await
        .expect("Could not deploy ERC20");

        println!("We have deployed ERC20 contract {}, waiting to see if the Cosmos chain choses to adopt it", res);

        match tokio::time::timeout(Duration::from_secs(100), async {
            loop {
                let req = DenomToErc20Request {
                    denom: denom.clone(),
                };

                let res = grpc.denom_to_erc20(req).await;

                if let Ok(val) = res {
                    break val;
                }
                delay_for(Duration::from_secs(1)).await;
            }
        })
        .await
        {
            Ok(val) => {
                println!(
                    "Asset {} has accepted new ERC20 representation {}",
                    denom,
                    val.into_inner().erc20
                );
                exit(0);
            }
            Err(_) => {
                println!(
                    "Your ERC20 contract was not adopted, double check the metadata and try again"
                );
                exit(1);
            }
        }
    }
}
