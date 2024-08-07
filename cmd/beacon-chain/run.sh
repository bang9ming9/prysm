
set -o nounset

rm -rf ${DATA_DIR}
mkdir -p ${DATA_DIR}

go run .\
 --datadir "${DATA_DIR}"\
 --min-sync-peers 0\
 --genesis-state "${GETH_ENV}/consensus-layer/config/genesis.ssz"\
 --bootstrap-node= \
 --interop-eth1data-votes\
 --chain-config-file "${GETH_ENV}/consensus-layer/config/config.yml"\
 --contract-deployment-block 0\
 --chain-id 6939\
 --accept-terms-of-use\
 --jwt-secret "${GETH_ENV}/execution-layer/config/jwtsecret"\
 --suggested-fee-recipient "${FEE_RECEIPIENT}"\
 --minimum-peers-per-subnet 0\
 --enable-debug-rpc-endpoints\
 --execution-endpoint "http://127.0.0.1:8551"\
 --rpc-host "0.0.0.0"\
 --grpc-gateway-host "0.0.0.0"\
 --force-clear-db
#  --execution-endpoint "http://127.0.0.1:8551"\
#  --execution-endpoint "${ROOT_DIR}/.go-ethereum/geth.ipc"\