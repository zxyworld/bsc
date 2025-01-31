package eth

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/gopool"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
)

var (
	m1, _  = hex.DecodeString("7ff36ab5") //swap exact ETH for tokens
	m2, _  = hex.DecodeString("38ed1739") //swapExactTokensForTokens
	m3, _  = hex.DecodeString("8803dbee") //swapTokensForExactTokens
	m4, _  = hex.DecodeString("fb3bdb41") //swapETHForExactTokens
	m5, _  = hex.DecodeString("18cbafe5") //swapExactTokensForETH
	m6, _  = hex.DecodeString("b6f9de95") //swapExactETHForTokensSupportingFeeOnTransferTokens
	m7, _  = hex.DecodeString("791ac947") //swapExactTokensForETHSupportingFeeOnTransferTokens
	m8, _  = hex.DecodeString("5c11d795") //swapExactTokensForTokensSupportingFeeOnTransferTokens
	m9, _  = hex.DecodeString("5f575529") //metamask swap
	m10, _ = hex.DecodeString("f87dc1b7") //dodoex proxy dodoSwapV2TokenToToken
	m11, _ = hex.DecodeString("54bacd13") //dodoex externalSwap

	watchedMethods = [][]byte{
		m1,
		m2,
		m3,
		m4,
		m5,
		m6,
		m7,
		m8,
		m9,
		m10,
		m11,
	}
)

// txTraceContext is the contextual infos about a transaction before it gets run.
type txTraceContext struct {
	index int         // Index of the transaction within the block
	hash  common.Hash // Hash of the transaction
	block common.Hash // Hash of the block containing the transaction
}

type Simulator struct {
	mux sync.Mutex

	db      *state.StateDB
	backend *EthAPIBackend

	vm *vm.EVM
}

type PublicBotAPI struct {
	eth *Ethereum

	//channels for subscription stuff
	install   chan *subscription
	uninstall chan *subscription

	simResultCh chan *SimulateSingleTxResult
	newTxsCh    chan core.NewTxsEvent
}

type subscription struct {
	id        rpc.ID
	created   time.Time
	installed chan struct{} // closed when the filter is installed
	err       chan error    // closed when the filter is uninstalled

	//todo: testing by just sending a feed of ticker ticks as ints
	ticks      chan []int
	hashes     chan []common.Hash
	simResults chan *SimulateSingleTxResult
}

// Subscription is created when the client registers itself for a particular event.
type Subscription struct {
	ID        rpc.ID
	f         *subscription
	api       *PublicBotAPI
	unsubOnce sync.Once
}

type simulatorSubscriptions map[rpc.ID]*subscription

func NewPublicBotAPI(eth *Ethereum) *PublicBotAPI {
	api := &PublicBotAPI{
		eth:       eth,
		install:   make(chan *subscription),
		uninstall: make(chan *subscription),

		simResultCh: make(chan *SimulateSingleTxResult),
		newTxsCh:    make(chan core.NewTxsEvent),
	}

	api.Start()

	// go api.eventLoop()

	return api
}

func (api *PublicBotAPI) Start() {

	api.eth.TxPool().SubscribeNewTxsEvent(api.newTxsCh)
}

func NewSimulator(backend *EthAPIBackend) *Simulator {
	return &Simulator{
		backend: backend,
	}

}

// func (api *PublicBotAPI) eventLoop() {

// 	api.eth.txPool.SubscribeNewTxsEvent(api.newTxsCh)

// 	simSubs := make(simulatorSubscriptions)
// 	dumbTicker := time.NewTicker(1 * time.Second)
// 	for {
// 		select {

// 		case txs := <-api.newTxsCh:
// 			for _, tx := range txs.Txs {
// 				log.Info("newSimulatorResults", "tx-eloop", tx.Hash())
// 				for _, s := range simSubs {
// 					s.hashes <- []common.Hash{tx.Hash()}
// 				}
// 			}
// 			//api.handleNewTxs(txs.Txs)

// 		case r := <-api.simResultCh:
// 			for _, sub := range simSubs {
// 				sub.simResults <- r
// 			}

// 		case <-dumbTicker.C:
// 			//send event to subscribers if any
// 			for _, s := range simSubs {
// 				s.ticks <- []int{time.Now().Second()}
// 			}

// 		case s := <-api.install:
// 			simSubs[s.id] = s
// 			close(s.installed)

// 		case <-api.uninstall:
// 			//need to delete from simSubs array, copied code uses a map and deletes from map

// 		}
// 	}
// }

func (api *PublicBotAPI) isWatchedTx(tx *types.Transaction) bool {

	if len(tx.Data()) < 4 {
		return false
	}

	for _, sig := range watchedMethods {
		if bytes.Equal(sig, tx.Data()[:4]) {
			return true
		}
	}
	return false
}

func (api *PublicBotAPI) subscribeSimulatorResults(ticksCh chan []int) *Subscription {
	sub := &subscription{
		id:        rpc.NewID(),
		created:   time.Now(),
		ticks:     ticksCh,
		installed: make(chan struct{}),
	}
	//code i'm copying calls subcribe which installs the subscription into the event ssystem in the eventLoop
	return api.subscribe(sub)
}

func (sub *Subscription) Unsubscribe() {
}

// subscribe installs the subscription in the event broadcast loop.
func (api *PublicBotAPI) subscribe(sub *subscription) *Subscription {
	api.install <- sub
	<-sub.installed
	return &Subscription{ID: sub.id, f: sub, api: api}
}

type SimulateResult struct {
	Duration            *time.Duration `json:"duration"`
	Logs                []*types.Log   `json:"logs"`
	TargetTxResult      *SimulateSingleTxResult
	FinalTxResult       *SimulateSingleTxResult
	TxSimCount          int
	PostTargetProcessed int
}

func (api *PublicBotAPI) SimulateTxsSince(txCount int, sinceTime time.Time, gasPoolLimit int, finalTx *types.Transaction) (*SimulateResult, error) {

	s := NewSimulator(api.eth.APIBackend)

	block := api.eth.blockchain.CurrentBlock()
	log.Info("SimulateTxs", "currentBlock", block.NumberU64())
	s.Fork(block.NumberU64())

	pending, _ := api.eth.txPool.PendingEnteredAfter(sinceTime)
	signer := types.MakeSigner(api.eth.blockchain.Config(), block.Number())
	txs := types.NewTransactionsByPriceAndNonce(signer, pending)

	return s.executeSimulation(txs, common.HexToHash("0x0"), 0, txCount, gasPoolLimit, finalTx)
}

func (api *PublicBotAPI) SimulateTxsBefore(txCount int, beforeTime time.Time, gasPoolLimit int, finalTx *types.Transaction) (*SimulateResult, error) {

	s := NewSimulator(api.eth.APIBackend)

	block := api.eth.blockchain.CurrentBlock()
	log.Info("SimulateTxs", "currentBlock", block.NumberU64())
	s.Fork(block.NumberU64())

	pending, _ := api.eth.txPool.PendingEnteredBeforeMap(beforeTime)
	signer := types.MakeSigner(api.eth.blockchain.Config(), block.Number())
	txs := types.NewTransactionsByPriceAndNonce(signer, pending)

	return s.executeSimulation(txs, common.HexToHash("0x0"), 0, txCount, gasPoolLimit, finalTx)
}

//Returns pending txs that are older than cutoff time. Cutoff time is the time the txs entered the txpool originally
func (api *PublicBotAPI) PendingTxsBeforeCutoff(entryCutoff time.Time) ([]*types.Transaction, error) {
	return api.eth.txPool.PendingEnteredBeforeArray(entryCutoff)
}

func (api *PublicBotAPI) handleNewTxs(txs []*types.Transaction, notifier *rpc.Notifier, notifySubID rpc.ID) {

	for _, tx := range txs {

		//check if tx method sig is a match
		if api.isWatchedTx(tx) {

			//sim the tx against current state
			simResult, err := api.SimulateSingleTx(context.Background(), tx)

			if err != nil {
				//log here?
				continue
			}

			//send tx sim result to subscribers
			notifier.Notify(notifySubID, simResult)

		}
	}
}

//subscribe to this feed with newSimulatorResults using the rpc client subscribe method and the bot namespace
func (api *PublicBotAPI) NewSimulatorResults(ctx context.Context) (*rpc.Subscription, error) {

	notifier, supported := rpc.NotifierFromContext(ctx)
	if !supported {
		return &rpc.Subscription{}, rpc.ErrNotificationsUnsupported
	}

	rpcSub := notifier.CreateSubscription()

	log.Info("newSimulatorResults", "ID", rpcSub.ID)

	gopool.Submit(func() {

		// resultCh := make(chan []int, 128)
		// resultSub := api.subscribeSimulatorResults(resultCh)

		// dumbTicker := time.NewTicker(1 * time.Second)

		for {
			select {

			case txs := <-api.newTxsCh:
				api.handleNewTxs(txs.Txs, notifier, rpcSub.ID)
				// for _, tx := range txs.Txs {
				// 	log.Info("newSimulatorResults", "tx", tx.Hash())
				// 	notifier.Notify(rpcSub.ID, []common.Hash{tx.Hash()})
				// }

				// case r := <-api.simResultCh:
				// 	log.Info("newSimulatorResults", "result", r.TxHash)
				// 	notifier.Notify(rpcSub.ID, r)

				// case <-dumbTicker.C:
				// 	notifier.Notify(rpcSub.ID, []int{time.Now().Second()})

				// case result := <-resultCh:
				// 	notifier.Notify(rpcSub.ID, result)
				// case <-rpcSub.Err():
				// 	resultSub.Unsubscribe()
				// 	return
				// case <-notifier.Closed():
				// 	resultSub.Unsubscribe()
				// 	return
			}
		}
	})

	return rpcSub, nil
}

func (s *Simulator) Fork(blockNumber uint64) {

	header := s.backend.CurrentHeader()
	block := s.backend.eth.blockchain.GetBlockByNumber(blockNumber)
	statedb, err := s.backend.eth.blockchain.StateAt(block.Root())
	if err != nil {
		log.Info("Fork Error", "stateAtError", err)
	}

	s.db = statedb

	blockCtx := core.NewEVMBlockContext(header, s.backend.eth.blockchain, nil)
	traceContext := vm.TxContext{}

	s.vm = vm.NewEVM(blockCtx, traceContext, statedb, s.backend.eth.blockchain.Config(), *s.backend.eth.blockchain.GetVMConfig())
}

//Takes a list of transactions and simulates them sequentially. Returns logs output from simulation
func (s *Simulator) executeSimulation(txs *types.TransactionsByPriceAndNonce, targetHash common.Hash, postTargetCount int, maxTxCount int, gasPoolLimit int, finalTx *types.Transaction) (*SimulateResult, error) {
	startTs := time.Now()
	logs := make([]*types.Log, 0)

	gasPool := new(core.GasPool).AddGas(uint64(gasPoolLimit)) //s.backend.CurrentHeader().GasLimit)
	gasPool.SubGas(params.SystemTxsGas)

	txSimIndex := 0
	postTargetTxsProcessed := 0
	targetTxProcessed := false
	minGasPrice := big.NewInt(5000000000)

	var targetResult *SimulateSingleTxResult

	//loop through pendings and apply to evm simulation
	for {
		if txSimIndex >= maxTxCount || postTargetTxsProcessed > postTargetCount {
			log.Info("simulatetxs", "txSimIndex", txSimIndex, "postTargetTxsProcessed", postTargetTxsProcessed)
			//interupt simulation at maximum number of evaluated txs
			//Note: reverted or other failed txs still count against this number
			break
		}

		if targetTxProcessed {
			postTargetTxsProcessed++
		}

		// Retrieve the next transaction and abort if all done
		tx := txs.Peek()
		if tx == nil {
			break
		}

		//if tx gas is too low then pop the tx but don't shift to the next for the account
		if tx.GasPrice().Cmp(minGasPrice) == -1 {
			txs.Pop()
			continue
		}

		//filter out other txs we aren't likely to care about for our trace 9ie. transfers, etc..)
		if len(tx.Data()) < 20 {
			txs.Pop()
			continue
		}

		// if tx.Protected() && !w.chainConfig.IsEIP155(w.current.header.Number) {
		// 	//log.Trace("Ignoring reply protected transaction", "hash", tx.Hash(), "eip155", w.chainConfig.EIP155Block)
		// 	txs.Pop()
		// 	continue
		// }
		// Start executing the transaction
		s.db.Prepare(tx.Hash(), common.Hash{}, txSimIndex)

		snap := s.db.Snapshot()
		//log.Info("SimulateTxs", "apply", tx.Hash().String())
		// logs, err := w.commitTransaction(tx, coinbase)

		receipt, err := core.ApplyTransaction(s.backend.eth.blockchain.Config(),
			s.backend.eth.BlockChain(),
			nil,
			gasPool,
			s.db,
			s.backend.CurrentHeader(),
			tx,
			&s.backend.CurrentHeader().GasUsed,
			*s.backend.eth.blockchain.GetVMConfig())

		switch {
		case errors.Is(err, core.ErrGasLimitReached):
			// Pop the current out-of-gas transaction without shifting in the next from the account
			//log.Trace("Gas limit exceeded for current block", "sender", from)
			txs.Pop()
			log.Info("SimulateTxs", "reverting", err)
			s.db.RevertToSnapshot(snap)

		case errors.Is(err, core.ErrNonceTooLow):
			// New head notification data race between the transaction pool and miner, shift
			//log.Trace("Skipping transaction with low nonce", "sender", from, "nonce", tx.Nonce())
			txs.Shift()
			log.Info("SimulateTxs", "reverting", err)
			s.db.RevertToSnapshot(snap)

		case errors.Is(err, core.ErrNonceTooHigh):
			// Reorg notification data race between the transaction pool and miner, skip account =
			//log.Trace("Skipping account with hight nonce", "sender", from, "nonce", tx.Nonce())
			txs.Pop()
			log.Info("SimulateTxs", "reverting", err)
			s.db.RevertToSnapshot(snap)

		case errors.Is(err, nil):
			// Everything ok, collect the logs and shift in the next transaction from the same account
			logs = append(logs, receipt.Logs...)
			// w.current.tcount++
			txs.Shift()

		case errors.Is(err, core.ErrTxTypeNotSupported):
			// Pop the unsupported transaction without shifting in the next from the account
			//log.Trace("Skipping unsupported transaction type", "sender", from, "type", tx.Type())
			txs.Pop()
			log.Info("SimulateTxs", "reverting", err)
			s.db.RevertToSnapshot(snap)

		default:
			// Strange error, discard the transaction and get the next in line (note, the
			// nonce-too-high clause will prevent us from executing in vain).
			//log.Debug("Transaction failed, account skipped", "hash", tx.Hash(), "err", err)
			txs.Shift()
			log.Info("SimulateTxs", "reverting", err)
			s.db.RevertToSnapshot(snap)
		}

		txSimIndex++

		if tx.Hash() == targetHash && receipt != nil {
			log.Info("SimulateSingleTx", "logs", len(receipt.Logs), "status", receipt.Status, "gasused", receipt.GasUsed)
			targetResult = &SimulateSingleTxResult{
				TxHash:          receipt.TxHash,
				ContractAddress: receipt.ContractAddress,
				GasUsed:         receipt.GasUsed,
				Status:          receipt.Status,
				Duration:        time.Since(startTs),
				ForkBlock:       s.backend.CurrentHeader().Number.Uint64(),
				Logs:            receipt.Logs,
			}
		} else {
			targetResult = nil
		}

		if tx.Hash() == targetHash {
			targetTxProcessed = true
		}
	}

	//simualte final tx if non null
	var finalResult *SimulateSingleTxResult
	if finalTx != nil {
		finalReceipt, _ := core.ApplyTransaction(s.backend.eth.blockchain.Config(),
			s.backend.eth.BlockChain(),
			nil,
			gasPool,
			s.db,
			s.backend.CurrentHeader(),
			finalTx,
			&s.backend.CurrentHeader().GasUsed,
			*s.backend.eth.blockchain.GetVMConfig())

		if finalReceipt != nil {
			log.Info("SimulateSingleTx", "final-tx-logs", len(finalReceipt.Logs), "status", finalReceipt.Status, "gasused", finalReceipt.GasUsed)
			finalResult = &SimulateSingleTxResult{
				TxHash:          finalReceipt.TxHash,
				ContractAddress: finalReceipt.ContractAddress,
				GasUsed:         finalReceipt.GasUsed,
				Status:          finalReceipt.Status,
				Duration:        time.Since(startTs),
				ForkBlock:       s.backend.CurrentHeader().Number.Uint64(),
				Logs:            finalReceipt.Logs,
			}
		} else {
			finalResult = nil
		}
	}

	duration := time.Since(startTs)

	//could filter out to only send back syncs and dodoswaps new pools etc?

	result := &SimulateResult{
		Logs:                logs,
		Duration:            &duration,
		TargetTxResult:      targetResult,
		FinalTxResult:       finalResult,
		TxSimCount:          txSimIndex,
		PostTargetProcessed: postTargetTxsProcessed,
	}
	return result, nil
}

type SimulateSingleTxResult struct {
	TxHash          common.Hash        `json:"txHash"`
	FullTx          *types.Transaction `json:"fullTx"`
	ContractAddress common.Address     `json:"contractAddress"`
	GasUsed         uint64             `json:"gasUsed"`
	Status          uint64             `json:"status"`
	Duration        time.Duration      `json:"duration"`
	ForkBlock       uint64             `json:"forkBlock"`
	Logs            []*types.Log       `json:"logs"`
}

func (api *PublicBotAPI) SendArbTxs(ctx context.Context, txs types.Transactions) {

	peers := api.eth.handler.peers.peers

	for _, p := range peers {
		go func(peer *ethPeer) {
			err := peer.SendMyTransactions(txs)
			log.Info("SentArbToPeer", "peer", peer.Peer.Info().Enode, "err", err)
		}(p)
	}
}

func (api *PublicBotAPI) SimulateSingleTx(ctx context.Context, tx *types.Transaction) (*SimulateSingleTxResult, error) {

	s := NewSimulator(api.eth.APIBackend)

	block := api.eth.blockchain.CurrentBlock()
	// log.Info("SimulateSingleTx", "currentBlock", block.NumberU64())
	s.Fork(block.NumberU64())

	startTs := time.Now()

	gasPool := new(core.GasPool).AddGas(s.backend.CurrentHeader().GasLimit)
	gasPool.SubGas(params.SystemTxsGas)

	s.db.Prepare(tx.Hash(), common.Hash{}, 0)

	// snap := s.db.Snapshot()
	// log.Info("SimulateSingleTx", "apply", tx.Hash().String())
	// logs, err := w.commitTransaction(tx, coinbase)

	receipt, err := core.ApplyTransaction(s.backend.eth.blockchain.Config(),
		s.backend.eth.BlockChain(), nil, gasPool,
		s.db,
		s.backend.CurrentHeader(), tx,
		&s.backend.CurrentHeader().GasUsed,
		*s.backend.eth.blockchain.GetVMConfig())

	// log.Info("SimulateSingleTx", "duration", time.Since(startTs))
	// log.Info("SimulateSingleTx", "err", err)

	var result *SimulateSingleTxResult
	if receipt != nil {
		// log.Info("SimulateSingleTx", "logs", len(receipt.Logs), "status", receipt.Status, "gasused", receipt.GasUsed)
		result = &SimulateSingleTxResult{
			TxHash:          receipt.TxHash,
			FullTx:          tx,
			ContractAddress: receipt.ContractAddress,
			GasUsed:         receipt.GasUsed,
			Status:          receipt.Status,
			Duration:        time.Since(startTs),
			ForkBlock:       block.Number().Uint64(),
			Logs:            receipt.Logs,
		}
	} else {
		// log.Info("SimulateSingleTx", "receipt-nil", tx.Hash())
		result = nil
	}

	return result, err
}

func (api *PublicBotAPI) SimulateAllTxsUpToTargetTx(ctx context.Context, targetHash common.Hash, postTargetCount int, maxTxCount int, gasPoolLimit int, finalTx *types.Transaction) (*SimulateResult, error) {

	s := NewSimulator(api.eth.APIBackend)

	block := api.eth.blockchain.CurrentBlock()
	// log.Info("SimulateTxs", "currentBlock", block.NumberU64())
	s.Fork(block.NumberU64())

	pending, _ := api.eth.txPool.Pending()
	// log.Info("SimualateTxs", "pending-addresses", len(pending))
	signer := types.MakeSigner(api.eth.blockchain.Config(), block.Number())
	txs := types.NewTransactionsByPriceAndNonce(signer, pending)

	return s.executeSimulation(txs, targetHash, postTargetCount, maxTxCount, gasPoolLimit, finalTx)

}

// type chainContext struct {
// 	api *EthAPIBackend
// 	ctx context.Context
// }

// func (context *chainContext) Engine() consensus.Engine {
// 	return context.api.Engine()
// }

// func (context *chainContext) GetHeader(hash common.Hash, number uint64) *types.Header {
// 	header, err := context.api.HeaderByNumber(context.ctx, rpc.BlockNumber(number))
// 	if err != nil {
// 		return nil
// 	}
// 	if header.Hash() == hash {
// 		return header
// 	}
// 	header, err = context.api.HeaderByHash(context.ctx, hash)
// 	if err != nil {
// 		return nil
// 	}
// 	return header
// }
