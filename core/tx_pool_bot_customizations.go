package core

import (
	"context"
	"encoding/hex"
	"errors"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var (

	//Error returned when tx is not accepted by customized pool
	ErrNotToRouter = errors.New("tx to address not router or arb swap contract")

	ArbFlashSwapAddress = "0x3E8F576b1dF7A3D07E9E1872199819C0781996b8"

	//below router address must own pairs whose _uniswapV2LikeCall func is listed in
	//our arb contract
	routerAddressArray = []string{
		"0x10ED43C718714eb63d5aA57B78B54704E256024E",
		"0x05fF2B0DB69458A0750badebc4f9e13aDd608C7F",
		"0xcF0feBd3f17CEf5b47b0cD257aCf6025c5BFf3b7",
		"0x7DAe51BD3E3376B8c7c4900E9107f12Be3AF1bA8",
		"0xbd67d157502A23309Db761c41965600c2Ec788b2",
		"0x2AD2C5314028897AEcfCF37FD923c079BeEb2C56",
		"0xd954551853F55deb4Ae31407c423e67B1621424A",
	}

	//controls if bot txs are captured and logged to mongo for review
	enableTxDeliveryLoggingForBots  = false
	enableTxDeliveryLoggingForMyArb = true

	MongoUri                        = "mongodb://localhost:27017"
	DbName                          = "txdelivery"
	Collection_Tx_Delivery_Tracking = "txs"
)

//AMH type to capture tx receipts from nodes
type TxDeliveryTrackingInfo struct {
	MethodId string    `json"methodId" bson:"methodId"`
	Hash     string    `json:"hash" bson:"hash"`
	Peer     string    `json:"peer" bson:"peer"`
	Data     string    `json:"data" bson:"data"`
	From     string    `json:"from" bson:"from"`
	To       string    `json:"to" bson:"to"`
	Nonce    uint64    `json:"nonce" bson:"nonce"`
	Time     time.Time `json:"time" bson:"time"`
	GasPrice uint64    `json:"gasPrice" bson:"gasPrice"`
	Gas      uint      `json:"gas" bson:"gas"`
}

func (pool *TxPool) checkForArbBotAndLogIfSeen(tx *types.Transaction) {
	//check for arb bot competitors and allow through
	//1de9c881
	from, err := types.Sender(pool.signer, tx)
	if err != nil {
		log.Info("1de9c881", "sender", "invalid sender", "err", err)
		return
	}

	if tx.To() == nil {
		return
	}
	data := hex.EncodeToString(tx.Data())
	if len(data) < 10 {
		return
	}
	method := data[0:8]

	logMyTx := enableTxDeliveryLoggingForMyArb && method == "c4d44074"
	logBotTx := enableTxDeliveryLoggingForBots && (method == "1de9c881" ||
		method == "1171c9aa" ||
		method == "985ea703" ||
		method == "a53a688b" ||
		method == "bf3b9e38" ||
		method == "ecfa311d" ||
		method == "b92a8126" ||
		method == "0548f398" ||
		method == "36946015")

	if logMyTx || logBotTx {
		//log with peer info to mongo
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()

		collection := pool.mongoClient.Database(DbName).Collection(Collection_Tx_Delivery_Tracking)

		info := &TxDeliveryTrackingInfo{
			MethodId: method,
			Hash:     tx.Hash().String(),
			Peer:     tx.PeerID,
			Data:     data,
			From:     from.String(),
			To:       tx.To().String(),
			Nonce:    tx.Nonce(),
			Time:     tx.Time(),
			GasPrice: tx.GasPrice().Uint64(),
			Gas:      uint(tx.Gas()),
		}
		collection.InsertOne(ctx, info, &options.InsertOneOptions{})
	}

}
