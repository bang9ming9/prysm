
set -o nounset

rm -rf ${DATA_DIR}
mkdir -p ${DATA_DIR}

go run .\
 --datadir "${DATA_DIR}"\
 --accept-terms-of-use\
 --interop-num-validators 64\
 --chain-config-file "${GETH_ENV}/consensus-layer/config/config.yml"\
 --suggested-fee-recipient "${FEE_RECEIPIENT}"\
 --interop-start-index 0\
 --force-clear-db