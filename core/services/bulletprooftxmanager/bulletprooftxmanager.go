package bulletprooftxmanager

import (
	"bytes"
	"context"
	"database/sql"
	"math/big"
	"time"

	"github.com/smartcontractkit/chainlink/core/assets"
	"github.com/smartcontractkit/chainlink/core/logger"
	"github.com/smartcontractkit/chainlink/core/services/eth"
	strpkg "github.com/smartcontractkit/chainlink/core/store"
	"github.com/smartcontractkit/chainlink/core/store/models"
	"github.com/smartcontractkit/chainlink/core/store/orm"
	"github.com/smartcontractkit/chainlink/core/utils"

	gethAccounts "github.com/ethereum/go-ethereum/accounts"
	gethCommon "github.com/ethereum/go-ethereum/common"
	gethTypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/jinzhu/gorm"
	"github.com/pkg/errors"
)

// For more information about the BulletproofTxManager architecture, see the design doc:
// https://www.notion.so/chainlink/BulletproofTxManager-Architecture-Overview-9dc62450cd7a443ba9e7dceffa1a8d6b

const (
	// maxEthNodeRequestTime is the worst case time we will wait for a response
	// from the eth node before we consider it to be an error
	maxEthNodeRequestTime = 2 * time.Minute
)

// SendEther creates a transaction that transfers the given value of ether
func SendEther(s *strpkg.Store, from, to gethCommon.Address, value assets.Eth) (models.EthTx, error) {
	ethtx := models.EthTx{
		FromAddress:    from,
		ToAddress:      to,
		EncodedPayload: []byte{},
		Value:          value,
		GasLimit:       s.Config.EthGasLimitDefault(),
		State:          models.EthTxUnstarted,
	}
	err := s.GetRawDB().Create(&ethtx).Error
	return ethtx, err
}

func newAttempt(s *strpkg.Store, etx models.EthTx, gasPrice *big.Int) (models.EthTxAttempt, error) {
	attempt := models.EthTxAttempt{}
	account, err := s.KeyStore.GetAccountByAddress(etx.FromAddress)
	if err != nil {
		return attempt, errors.Wrapf(err, "error getting account %s for transaction %v", etx.FromAddress.String(), etx.ID)
	}

	transaction := gethTypes.NewTransaction(uint64(*etx.Nonce), etx.ToAddress, etx.Value.ToInt(), etx.GasLimit, gasPrice, etx.EncodedPayload)
	hash, signedTxBytes, err := signTx(s.KeyStore, account, transaction, s.Config.ChainID())
	if err != nil {
		return attempt, errors.Wrapf(err, "error using account %s to sign transaction %v", etx.FromAddress.String(), etx.ID)
	}

	attempt.State = models.EthTxAttemptInProgress
	attempt.SignedRawTx = signedTxBytes
	attempt.EthTxID = etx.ID
	attempt.GasPrice = *utils.NewBig(gasPrice)
	attempt.Hash = hash

	return attempt, nil
}

func signTx(keyStore strpkg.KeyStoreInterface, account gethAccounts.Account, tx *gethTypes.Transaction, chainID *big.Int) (gethCommon.Hash, []byte, error) {
	signedTx, err := keyStore.SignTx(account, tx, chainID)
	if err != nil {
		return gethCommon.Hash{}, nil, errors.Wrap(err, "signTx failed")
	}
	rlp := new(bytes.Buffer)
	if err := signedTx.EncodeRLP(rlp); err != nil {
		return gethCommon.Hash{}, nil, errors.Wrap(err, "signTx failed")
	}
	return signedTx.Hash(), rlp.Bytes(), nil

}

// send broadcasts the transaction to the ethereum network, writes any relevant
// data onto the attempt and returns an error (or nil) depending on the status
func sendTransaction(gethClientWrapper strpkg.GethClientWrapper, a models.EthTxAttempt) *sendError {
	signedTx, err := a.GetSignedTx()
	if err != nil {
		return FatalSendError(err)
	}
	err = gethClientWrapper.GethClient(func(gethClient eth.GethClient) error {
		ctx, cancel := context.WithTimeout(context.Background(), maxEthNodeRequestTime)
		defer cancel()
		return errors.WithStack(gethClient.SendTransaction(ctx, signedTx))
	})

	logger.Debugw("BulletproofTxManager: Broadcasting transaction", "ethTxAttemptID", a.ID, "txHash", signedTx.Hash(), "gasPriceWei", a.GasPrice.ToInt().Int64())
	sendErr := SendError(err)
	if sendErr.IsTransactionAlreadyInMempool() {
		logger.Debugw("transaction already in mempool", "txHash", signedTx.Hash(), "nodeErr", sendErr.Error())
		return nil
	}
	return SendError(err)
}

// sendEmptyTransaction sends a transaction with 0 Eth and an empty payload to the burn address
// May be useful for clearing stuck nonces
func sendEmptyTransaction(gethClientWrapper strpkg.GethClientWrapper, keyStore strpkg.KeyStoreInterface, nonce uint64, gasLimit uint64, gasPriceWei *big.Int, account gethAccounts.Account, chainID *big.Int) (*gethTypes.Transaction, error) {
	to := utils.ZeroAddress
	value := big.NewInt(0)
	payload := []byte{}
	tx := gethTypes.NewTransaction(nonce, to, value, gasLimit, gasPriceWei, payload)
	signedTx, err := keyStore.SignTx(account, tx, chainID)
	if err != nil {
		return signedTx, errors.Wrap(err, "sendEmptyTransaction failed")
	}
	err = gethClientWrapper.GethClient(func(gethClient eth.GethClient) error {
		ctx, cancel := context.WithTimeout(context.Background(), maxEthNodeRequestTime)
		defer cancel()
		return errors.Wrap(gethClient.SendTransaction(ctx, signedTx), "sendEmptyTransaction failed")
	})
	return signedTx, err
}

// BumpGas returns a new gas price increased by the largest of:
// - A configured percentage bump (ETH_GAS_BUMP_PERCENT)
// - A configured fixed amount of Wei (ETH_GAS_PRICE_WEI)
// - The configured default base gas price (ETH_GAS_PRICE_DEFAULT)
func BumpGas(config orm.ConfigReader, originalGasPrice *big.Int) *big.Int {
	// Currently this lives in store because TxManager also needs it.
	// It can move here permanently once the old TxManager has been deleted.
	return strpkg.BumpGas(config, originalGasPrice)
}

func withAdvisoryLock(s *strpkg.Store, classID int32, objectID int32, f func() error) error {
	ctx := context.Background()
	conn, err := s.GetRawDB().DB().Conn(ctx)
	if err != nil {
		return errors.Wrap(err, "withAdvisoryLock failed")
	}
	defer logger.ErrorIfCalling(conn.Close)
	if err := tryAdvisoryLock(ctx, conn, classID, objectID); err != nil {
		return errors.Wrap(err, "tryAdvisoryLock failed")
	}
	defer logger.ErrorIfCalling(func() error { return advisoryUnlock(ctx, conn, classID, objectID) })
	return f()
}

func tryAdvisoryLock(ctx context.Context, conn *sql.Conn, classID int32, objectID int32) error {
	gotLock := false
	rows, err := conn.QueryContext(ctx, "SELECT pg_try_advisory_lock($1, $2)", classID, objectID)
	if err != nil {
		return errors.Wrap(err, "tryAdvisoryLock failed")
	}
	defer logger.ErrorIfCalling(rows.Close)
	gotRow := rows.Next()
	if !gotRow {
		return errors.New("query unexpectedly returned 0 rows")
	}
	if err := rows.Scan(&gotLock); err != nil {
		return errors.Wrap(err, "tryAdvisoryLock failed")
	}
	if gotLock {
		return nil
	}
	return errors.Errorf("could not get advisory lock for classID, objectID %v, %v", classID, objectID)
}

func advisoryUnlock(ctx context.Context, conn *sql.Conn, classID int32, objectID int32) error {
	_, err := conn.ExecContext(ctx, "SELECT pg_advisory_unlock($1, $2)", classID, objectID)
	return errors.Wrap(err, "advisoryUnlock failed")
}

func saveReplacementInProgressAttempt(store *strpkg.Store, oldAttempt models.EthTxAttempt, replacementAttempt *models.EthTxAttempt) error {
	if oldAttempt.State != models.EthTxAttemptInProgress || replacementAttempt.State != models.EthTxAttemptInProgress {
		return errors.New("expected attempts to be in_progress")
	}
	if oldAttempt.ID == 0 {
		return errors.New("expected oldAttempt to have an ID")
	}
	return store.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`DELETE FROM eth_tx_attempts WHERE id = ? `, oldAttempt.ID).Error; err != nil {
			return errors.Wrap(err, "saveReplacementInProgressAttempt failed")
		}
		return errors.Wrap(tx.Create(replacementAttempt).Error, "saveReplacementInProgressAttempt failed")
	})
}
