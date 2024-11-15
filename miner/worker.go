// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package miner

import (
	"bytes"
	"github.com/anduschain/go-anduschain/accounts"
	"github.com/anduschain/go-anduschain/common"
	"github.com/anduschain/go-anduschain/consensus"
	"github.com/anduschain/go-anduschain/consensus/deb"
	"github.com/anduschain/go-anduschain/consensus/deb/client"
	"github.com/anduschain/go-anduschain/consensus/layer2"
	lclient "github.com/anduschain/go-anduschain/consensus/layer2/client"
	"github.com/anduschain/go-anduschain/consensus/misc"
	"github.com/anduschain/go-anduschain/core"
	"github.com/anduschain/go-anduschain/core/interfaces"
	"github.com/anduschain/go-anduschain/core/state"
	"github.com/anduschain/go-anduschain/core/types"
	"github.com/anduschain/go-anduschain/core/vm"
	"github.com/anduschain/go-anduschain/event"
	"github.com/anduschain/go-anduschain/fairnode/verify"
	"github.com/anduschain/go-anduschain/log"
	"github.com/anduschain/go-anduschain/p2p/discover"
	"github.com/anduschain/go-anduschain/params"
	"github.com/anduschain/go-anduschain/trie"
	"github.com/deckarep/golang-set"
	"math/big"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// resultQueueSize is the size of channel listening to sealing result.
	resultQueueSize = 10

	// txChanSize is the size of channel listening to NewTxsEvent.
	// The number is referenced from the size of tx pool.
	txChanSize = 4096 * 5

	// chainHeadChanSize is the size of channel listening to ChainHeadEvent.
	chainHeadChanSize = 10

	// chainSideChanSize is the size of channel listening to ChainSideEvent.
	chainSideChanSize = 10

	// resubmitAdjustChanSize is the size of resubmitting interval adjustment channel.
	resubmitAdjustChanSize = 10

	// miningLogAtDepth is the number of confirmations before logging successful mining.
	miningLogAtDepth = 1 // README(hakuna) : confirm just in time.

	// minRecommitInterval is the minimal time interval to recreate the mining block with
	// any newly arrived transactions.
	minRecommitInterval = 1 * time.Second

	// maxRecommitInterval is the maximum time interval to recreate the mining block with
	// any newly arrived transactions.
	maxRecommitInterval = 15 * time.Second

	// intervalAdjustRatio is the impact a single interval adjustment has on sealing work
	// resubmitting interval.
	intervalAdjustRatio = 0.1

	// intervalAdjustBias is applied during the new resubmit interval calculation in favor of
	// increasing upper limit or decreasing lower limit so that the limit can be reachable.
	intervalAdjustBias = 200 * 1000.0 * 1000.0

	// staleThreshold is the maximum depth of the acceptable stale block.
	staleThreshold = 1 // README(hakuna) : confirm just in time.
)

// environment is the worker's current environment and holds all of the current state information.
type environment struct {
	signer types.Signer

	state     *state.StateDB // apply state changes here
	ancestors mapset.Set     // ancestor set (used for checking uncle parent validity)
	family    mapset.Set     // family set (used for checking uncle invalidity)

	count   int           // tx count in cycle
	gasPool *core.GasPool // available gas used to pack transactions

	header   *types.Header
	txs      []*types.Transaction
	receipts []*types.Receipt

	// circuit capacity check related fields
	traceEnv *core.TraceEnv // env for tracing
}

// task contains all information for consensus engine sealing and result submitting.
type task struct {
	receipts []*types.Receipt

	state     *state.StateDB
	block     *types.Block
	createdAt time.Time
}

const (
	commitInterruptNone int32 = iota
	commitInterruptNewHead
	commitInterruptResubmit
)

// newWorkReq represents a request for new sealing work submitting with relative interrupt notifier.
type newWorkReq struct {
	interrupt *int32
	noempty   bool
	timestamp int64
}

// intervalAdjust represents a resubmitting interval adjustment.
type intervalAdjust struct {
	ratio float64
	inc   bool
}

type NewLeagueBlockData struct {
	Block *types.Block
	Addr  common.Address
	Sign  []byte
}

// worker is the main object which takes care of submitting new work to consensus engine
// and gathering the sealing result.
type worker struct {
	config *params.ChainConfig
	engine consensus.Engine
	eth    interfaces.Backend
	chain  *core.BlockChain

	gasFloor uint64
	gasCeil  uint64

	// Feeds
	pendingLogsFeed event.Feed

	// Subscriptions
	mux          *event.TypeMux
	txsCh        chan types.NewTxsEvent
	txsSub       event.Subscription
	chainHeadCh  chan types.ChainHeadEvent
	chainHeadSub event.Subscription
	chainSideCh  chan types.ChainSideEvent
	chainSideSub event.Subscription

	// Channels
	newWorkCh          chan *newWorkReq
	taskCh             chan *task
	resultCh           chan *types.Block
	startCh            chan struct{}
	exitCh             chan struct{}
	resubmitIntervalCh chan time.Duration
	resubmitAdjustCh   chan *intervalAdjust

	current *environment // An environment for current running cycle.

	possibleUncles map[common.Hash]*types.Block // A set of side blocks as the possible uncle blocks. TODO(hakuna) : will be removed
	unconfirmed    *unconfirmedBlocks           // A set of locally mined blocks pending canonicalness confirmations. TODO(hakuna) : will be removed

	mu       sync.RWMutex // The lock used to protect the coinbase and extra fields
	coinbase common.Address
	extra    []byte

	pendingMu    sync.RWMutex
	pendingTasks map[common.Hash]*task

	snapshotMu    sync.RWMutex // The lock used to protect the block snapshot and state snapshot
	snapshotBlock *types.Block
	snapshotState *state.StateDB

	// atomic status counters
	running int32 // The indicator whether the consensus engine is running or not.
	newTxs  int32 // New arrival transaction count since last sealing work submitting.

	// Test hooks
	newTaskHook  func(*task)                        // Method to call upon receiving a new sealing task.
	skipSealHook func(*task) bool                   // Method to decide whether skipping the sealing.
	fullTaskHook func()                             // Method to call before pushing the full sealing task.
	resubmitHook func(time.Duration, time.Duration) // Method to call upon updating resubmitting interval.

	// TODO(hakuna) : added new version
	scope              event.SubscriptionScope
	newLeagueBlockFeed event.Feed

	debClient        *client.DebClient
	fnStatusCh       chan types.FairnodeStatusEvent
	fnStatusdSub     event.Subscription
	fnClientCloseCh  chan types.ClientClose
	fnClientCLoseSub event.Subscription

	fnStatus types.FnStatus

	leagueBlockCh      chan *types.NewLeagueBlockEvent
	voteResultCh       chan types.Voters
	submitBlockCh      chan *types.Block
	fnSignCh           chan []byte
	leagueBroadCastCh  chan struct{}
	possibleWinning    *types.Block // A set of possible winning block
	possibleFinalBlock *types.Block // A set of possible winning block

	finalBlock *types.Block
	finalizeCh chan struct{}

	possibleWinningBlock *types.VoteBlock // A set of possible winning voteblock

	// TODO: CSW add for Layer2
	layer2Client     *lclient.Layer2Client
	l2ClientCloseCh  chan types.ClientClose
	l2ClientCLoseSub event.Subscription
}

func newWorker(config *params.ChainConfig, engine consensus.Engine, eth interfaces.Backend, mux *event.TypeMux, recommit time.Duration, gasFloor, gasCeil uint64, loacalIps map[string]string, staticNodes []*discover.Node) *worker {
	worker := &worker{
		config:             config,
		engine:             engine,
		eth:                eth,
		mux:                mux,
		chain:              eth.BlockChain(),
		gasFloor:           gasFloor,
		gasCeil:            gasCeil,
		possibleUncles:     make(map[common.Hash]*types.Block),
		unconfirmed:        newUnconfirmedBlocks(eth.BlockChain(), miningLogAtDepth),
		pendingTasks:       make(map[common.Hash]*task),
		txsCh:              make(chan types.NewTxsEvent, txChanSize),
		chainHeadCh:        make(chan types.ChainHeadEvent, chainHeadChanSize),
		chainSideCh:        make(chan types.ChainSideEvent, chainSideChanSize),
		newWorkCh:          make(chan *newWorkReq),
		taskCh:             make(chan *task),
		resultCh:           make(chan *types.Block, resultQueueSize),
		exitCh:             make(chan struct{}),
		startCh:            make(chan struct{}, 1),
		resubmitIntervalCh: make(chan time.Duration),
		resubmitAdjustCh:   make(chan *intervalAdjust, resubmitAdjustChanSize),

		// for deb client
		fnStatusCh:      make(chan types.FairnodeStatusEvent), // non async channel
		fnClientCloseCh: make(chan types.ClientClose),

		leagueBlockCh:     make(chan *types.NewLeagueBlockEvent),
		leagueBroadCastCh: make(chan struct{}),
		voteResultCh:      make(chan types.Voters),
		submitBlockCh:     make(chan *types.Block),
		fnSignCh:          make(chan []byte),
		finalizeCh:        make(chan struct{}),

		// for layer2 client
		l2ClientCloseCh: make(chan types.ClientClose),
	}

	// Subscribe NewTxsEvent for tx pool
	worker.txsSub = eth.TxPool().SubscribeNewTxsEvent(worker.txsCh)
	// Subscribe events for blockchain
	worker.chainHeadSub = eth.BlockChain().SubscribeChainHeadEvent(worker.chainHeadCh)
	worker.chainSideSub = eth.BlockChain().SubscribeChainSideEvent(worker.chainSideCh)

	// Sanitize recommit interval if the user-specified one is too short.
	if recommit < minRecommitInterval {
		log.Warn("Sanitizing miner recommit interval", "provided", recommit, "updated", minRecommitInterval)
		recommit = minRecommitInterval
	}

	go worker.mainLoop()
	go worker.newWorkLoop(recommit)
	go worker.resultLoop()
	go worker.taskLoop()
	go worker.commitLoop()

	if worker.config.Deb != nil {
		worker.debClient = client.NewDebClient(config, worker.exitCh, loacalIps, staticNodes)
		// TODO(hakuna) : new version miner process, event receiver
		worker.fnStatusdSub = worker.debClient.SubscribeFairnodeStatusEvent(worker.fnStatusCh)
		worker.fnClientCLoseSub = worker.debClient.SubscribeClientCloseEvent(worker.fnClientCloseCh)

		go worker.clientStatusLoop() // client close check and mininig canceled
		go worker.leagueStatusLoop() // for league status message
	} else if worker.config.Layer2 != nil {
		worker.layer2Client = lclient.NewLayer2Client(config, worker.exitCh)
		worker.l2ClientCLoseSub = worker.layer2Client.SubscribeClientCloseEvent(worker.l2ClientCloseCh)

		go worker.l2clientStatusLoop()
	}

	// Submit first work to initialize pending state.
	worker.startCh <- struct{}{}

	return worker
}

func (w *worker) leagueStatusLoop() {
	defer log.Warn("leagueStatusLoop was dead")
	defer w.fnStatusdSub.Unsubscribe()

	for {
		select {
		case ev := <-w.fnStatusCh:
			if w.fnStatus != ev.Status {
				w.fnStatus = ev.Status
			}
			switch w.fnStatus {
			case types.MAKE_BLOCK:
				if otprn, ok := ev.Payload.(types.Otprn); ok {
					if engine, ok := w.engine.(*deb.Deb); ok {
						engine.SetCoinbase(w.coinbase) // deb consensus engine setting coinbase
						engine.SetOtprn(&otprn)        // deb consensus engine setting otprn
					}
					w.startCh <- struct{}{}
				} else {
				}
			case types.LEAGUE_BROADCASTING:
				// league broadcasting
				w.leagueBroadCastCh <- struct{}{}
			case types.VOTE_START:
				if voteCh, ok := ev.Payload.(chan types.NewLeagueBlockEvent); ok {
					if w.possibleWinning == nil {
						log.Error("leagueStatusLoop possible winning block was nil")
						continue
					}
					block := w.possibleWinning
					bHash := rlpHash(block) // 리그 브로드케스팅 블록 해시 (전체를 해시 한다)
					acc := accounts.Account{Address: w.coinbase}
					wallet, err := w.eth.AccountManager().Find(acc)
					if err != nil {
						log.Error("wallet not fount", "msg", err, "account", w.coinbase.String())
						continue
					}

					sign, err := wallet.SignHash(acc, bHash.Bytes())
					if err != nil {
						log.Error("Block Sign Hash", "msg", err)
						continue
					}
					otprn, _ := types.DecodeOtprn(block.Otprn())
					log.Info("Vote Block", "number", block.Number(), "otprn", otprn.HashOtprn(), "hash", block.Hash(),
						"difficulty", block.Difficulty())
					voteCh <- types.NewLeagueBlockEvent{Block: block, Address: w.coinbase, Sign: sign}
				}
			case types.VOTE_COMPLETE:
				if payload, ok := ev.Payload.([]interface{}); ok {
					if voters, ok := payload[0].(types.Voters); ok {
						w.voteResultCh <- voters
					}
					if submitBlockCh, ok := payload[1].(chan *types.Block); ok {
						w.submitBlockCh = submitBlockCh
					}
				}
			case types.REQ_FAIRNODE_SIGN:
				if fnSign, ok := ev.Payload.([]byte); ok {
					w.fnSignCh <- fnSign
				}
			case types.FINALIZE:
				if w.finalBlock == nil {
					continue
				}
				w.finalizeCh <- struct{}{}
			default:
				log.Info("leagueStatusLoop", "pos", "miner.worker", "status", ev.Status.String())
			}
		case <-w.fnStatusdSub.Err():
			return
		}
	}
}

func (w *worker) clientStatusLoop() {
	defer log.Warn("fair client was dead and worker exited")
	defer w.fnClientCLoseSub.Unsubscribe()

	for {
		select {
		case <-w.fnClientCloseCh:
			log.Warn("deb client was close")
			w.debClient.Stop()
			time.AfterFunc(10*time.Second, func() {
				if w.isRunning() {
					log.Info("Retry Mining")
					w.debStart()
				}
			})
		case <-w.fnClientCLoseSub.Err():
			return
		}
	}
}

// leauge new block subscribe
func (w *worker) SubscribeNewLeagueBlockEvent(ch chan<- types.NewLeagueBlockEvent) event.Subscription {
	return w.scope.Track(w.newLeagueBlockFeed.Subscribe(ch))
}

func (w *worker) LeagueBlockCh() chan *types.NewLeagueBlockEvent {
	return w.leagueBlockCh
}

// setEtherbase sets the etherbase used to initialize the block coinbase field.
func (w *worker) setEtherbase(addr common.Address) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.coinbase = addr
}

// setExtra sets the content used to initialize the block extra field.
func (w *worker) setExtra(extra []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.extra = extra
}

// setRecommitInterval updates the interval for miner sealing work recommitting.
func (w *worker) setRecommitInterval(interval time.Duration) {
	w.resubmitIntervalCh <- interval
}

// pending returns the pending state and corresponding block.
func (w *worker) pending() (*types.Block, *state.StateDB) {
	// return a snapshot to avoid contention on currentMu mutex
	w.snapshotMu.RLock()
	defer w.snapshotMu.RUnlock()
	if w.snapshotState == nil {
		return nil, nil
	}
	return w.snapshotBlock, w.snapshotState.Copy()
}

// pendingBlock returns pending block.
func (w *worker) pendingBlock() *types.Block {
	// return a snapshot to avoid contention on currentMu mutex
	w.snapshotMu.RLock()
	defer w.snapshotMu.RUnlock()
	return w.snapshotBlock
}

// pendingBlockAndReceipts returns pending block and corresponding receipts.
// TODO pendingReceipt not suppoert  CSW
func (w *worker) pendingBlockAndReceipts() (*types.Block, types.Receipts) {
	// return a snapshot to avoid contention on currentMu mutex
	w.snapshotMu.RLock()
	defer w.snapshotMu.RUnlock()
	return w.snapshotBlock, nil
}

func (w *worker) debStart() {
	if err := w.debClient.Start(w.eth); err == nil {
		w.startCh <- struct{}{}
	} else {
		log.Error("deb client start", "msg", err)
		return
	}
}

// start sets the running status as 1 and triggers new work submitting.
func (w *worker) start() {
	if w.isRunning() {
		return
	}
	atomic.StoreInt32(&w.running, 1)
	if _, ok := w.engine.(*deb.Deb); ok {
		w.debStart()
	} else if _, ok := w.engine.(*layer2.Layer2); ok {
		w.layer2Start()
	} else {
		w.startCh <- struct{}{}
	}
}

// stop sets the running status as 0.
func (w *worker) stop() {
	atomic.StoreInt32(&w.running, 0)
	if _, ok := w.engine.(*deb.Deb); ok {
		w.debClient.Stop()
	}
	if _, ok := w.engine.(*layer2.Layer2); ok {
		w.layer2Client.Stop()
	}
}

// isRunning returns an indicator whether worker is running or not.
func (w *worker) isRunning() bool {
	return atomic.LoadInt32(&w.running) == 1
}

// close terminates all background threads maintained by the worker.
// Note the worker does not support being closed multiple times.
func (w *worker) close() {
	close(w.exitCh)
}

// newWorkLoop is a standalone goroutine to submit new mining work upon received events.
func (w *worker) newWorkLoop(recommit time.Duration) {
	var (
		interrupt   *int32
		minRecommit = recommit // minimal resubmit interval specified by user.
		timestamp   int64      // timestamp for each round of mining.
	)

	timer := time.NewTimer(0)
	<-timer.C // discard the initial tick

	// commit aborts in-flight transaction execution with given signal and resubmits a new one.
	commit := func(noempty bool, s int32) {
		if interrupt != nil {
			atomic.StoreInt32(interrupt, s)
		}
		interrupt = new(int32)
		w.newWorkCh <- &newWorkReq{interrupt: interrupt, noempty: noempty, timestamp: timestamp}
		timer.Reset(recommit)
		atomic.StoreInt32(&w.newTxs, 0)
	}
	// recalcRecommit recalculates the resubmitting interval upon feedback.
	recalcRecommit := func(target float64, inc bool) {
		var (
			prev = float64(recommit.Nanoseconds())
			next float64
		)
		if inc {
			next = prev*(1-intervalAdjustRatio) + intervalAdjustRatio*(target+intervalAdjustBias)
			// Recap if interval is larger than the maximum time interval
			if next > float64(maxRecommitInterval.Nanoseconds()) {
				next = float64(maxRecommitInterval.Nanoseconds())
			}
		} else {
			next = prev*(1-intervalAdjustRatio) + intervalAdjustRatio*(target-intervalAdjustBias)
			// Recap if interval is less than the user specified minimum
			if next < float64(minRecommit.Nanoseconds()) {
				next = float64(minRecommit.Nanoseconds())
			}
		}
		recommit = time.Duration(int64(next))
	}
	// clearPending cleans the stale pending tasks.
	clearPending := func(number uint64) {
		w.pendingMu.Lock()
		for h, t := range w.pendingTasks {
			if t.block.NumberU64()+staleThreshold <= number {
				delete(w.pendingTasks, h)
			}
		}
		w.possibleFinalBlock = nil
		w.possibleWinning = nil
		w.finalBlock = nil
		w.pendingMu.Unlock()
	}

	for {
		select {
		case <-w.startCh:
			clearPending(w.chain.CurrentBlock().NumberU64())
			timestamp = time.Now().Unix()
			commit(false, commitInterruptNewHead)
		case head := <-w.chainHeadCh:
			clearPending(head.Block.NumberU64())
			timestamp = time.Now().Unix()
			if _, ok := w.engine.(*deb.Deb); !ok {
				commit(false, commitInterruptNewHead)
			}

		case <-timer.C:
			// If mining is running resubmit a new work cycle periodically to pull in
			// higher priced transactions. Disable this overhead for pending blocks.
			if w.isRunning() && w.config.Deb == nil {
				if w.config.Clique != nil && w.config.Clique.Period == 0 {
					// Short circuit if no new transaction arrives.
					if atomic.LoadInt32(&w.newTxs) == 0 {
						timer.Reset(recommit)
						continue
					}
				}
				commit(true, commitInterruptResubmit)
			}

		case interval := <-w.resubmitIntervalCh:
			// Adjust resubmit interval explicitly by user.
			if interval < minRecommitInterval {
				log.Warn("Sanitizing miner recommit interval", "provided", interval, "updated", minRecommitInterval)
				interval = minRecommitInterval
			}
			log.Info("Miner recommit interval update", "from", minRecommit, "to", interval)
			minRecommit, recommit = interval, interval

			if w.resubmitHook != nil {
				w.resubmitHook(minRecommit, recommit)
			}

		case adjust := <-w.resubmitAdjustCh:
			// Adjust resubmit interval by feedback.
			if adjust.inc {
				before := recommit
				recalcRecommit(float64(recommit.Nanoseconds())/adjust.ratio, true)
				log.Trace("Increase miner recommit interval", "from", before, "to", recommit)
			} else {
				before := recommit
				recalcRecommit(float64(minRecommit.Nanoseconds()), false)
				log.Trace("Decrease miner recommit interval", "from", before, "to", recommit)
			}

			if w.resubmitHook != nil {
				w.resubmitHook(minRecommit, recommit)
			}

		case <-w.exitCh:
			return
		}
	}
}

// mainLoop is a standalone goroutine to regenerate the sealing task based on the received event.
func (w *worker) mainLoop() {
	defer w.txsSub.Unsubscribe()
	defer w.chainHeadSub.Unsubscribe()
	defer w.chainSideSub.Unsubscribe()

	for {
		select {
		case req := <-w.newWorkCh:
			if _, ok := w.engine.(*layer2.Layer2); ok {
				w.layser2SendTransaction(req.interrupt, req.noempty, req.timestamp)
			} else {
				w.commitNewWork(req.interrupt, req.noempty, req.timestamp)
			}

		case ev := <-w.chainSideCh:
			if _, exist := w.possibleUncles[ev.Block.Hash()]; exist {
				continue
			}
			// Add side block to possible uncle block set.
			w.possibleUncles[ev.Block.Hash()] = ev.Block

		case ev := <-w.txsCh:
			// Apply transactions to the pending state if we're not mining.
			//
			// Note all transactions received may not be continuous with transactions
			// already included in the current mining block. These transactions will
			// be automatically eliminated.
			if !w.isRunning() && w.current != nil {
				w.mu.RLock()
				coinbase := w.coinbase
				w.mu.RUnlock()

				txs := make(map[common.Address]types.Transactions)
				for _, tx := range ev.Txs {
					acc, _ := tx.Sender(w.current.signer)
					txs[acc] = append(txs[acc], tx)
				}
				txset := types.NewTransactionsByPriceAndNonce(w.current.signer, txs)
				w.commitTransactions(txset, coinbase, nil)
				w.updateSnapshot()
			} else {
				//If we're mining, but nothing is being processed, wake on new transactions
				if _, ok := w.engine.(*deb.Deb); ok {
					// otprn check
					otprn := w.engine.Otprn()

					if otprn != nil && otprn.FnAddr == params.TestFairnodeAddr && w.config.ChainID == params.DvlpNetId { // TEST
						w.commitNewWork(nil, false, time.Now().Unix())
					}
				}
			}
			atomic.AddInt32(&w.newTxs, int32(len(ev.Txs)))

		// System stopped
		case <-w.exitCh:
			return
		case <-w.txsSub.Err():
			return
		case <-w.chainHeadSub.Err():
			return
		case <-w.chainSideSub.Err():
			return
		}
	}
}

// taskLoop is a standalone goroutine to fetch sealing task from the generator and
// push them to consensus engine.
func (w *worker) taskLoop() {
	var (
		stopCh chan struct{}
		prev   common.Hash
	)

	// interrupt aborts the in-flight sealing task.
	interrupt := func() {
		if stopCh != nil {
			close(stopCh)
			stopCh = nil
		}
	}
	for {
		select {
		case task := <-w.taskCh:
			if w.newTaskHook != nil {
				w.newTaskHook(task)
			}
			// Reject duplicate sealing work due to resubmitting.
			sealHash := w.engine.SealHash(task.block.Header())
			if sealHash == prev {
				continue
			}
			// Interrupt previous sealing operation
			interrupt()
			stopCh, prev = make(chan struct{}), sealHash

			if w.skipSealHook != nil && w.skipSealHook(task) {
				continue
			}
			w.pendingMu.Lock()
			w.pendingTasks[w.engine.SealHash(task.block.Header())] = task
			w.pendingMu.Unlock()

			if err := w.engine.Seal(w.chain, task.block, w.resultCh, stopCh); err != nil {
				log.Warn("Block sealing failed", "err", err)
				w.pendingMu.Lock()
				delete(w.pendingTasks, sealHash)
				w.pendingMu.Unlock()
			}
		case <-w.exitCh:
			interrupt()
			return
		}
	}
}

// resultLoop is a standalone goroutine to handle sealing result submitting
// and flush relative data to the database.
func (w *worker) resultLoop() {
	for {
		select {
		case block := <-w.resultCh:
			if w.config.Deb != nil && w.fnStatus != types.MAKE_BLOCK {
				log.Error("Got resultCh But fnStatus is not MAKE_BLOCK", "fnStatus", w.fnStatus.String())
				continue
			}

			// Short circuit when receiving empty result.
			if block == nil {
				continue
			}
			// Short circuit when receiving duplicate result caused by resubmitting.
			if w.chain.HasBlock(block.Hash(), block.NumberU64()) {
				continue
			}
			var (
				sealhash = w.engine.SealHash(block.Header())
				hash     = block.Hash()
			)

			w.pendingMu.RLock()
			task, exist := w.pendingTasks[sealhash]
			w.pendingMu.RUnlock()

			if !exist {
				log.Error("Block found but no relative pending task", "number", block.Number(), "sealhash", sealhash, "hash", hash)
				continue
			}
			if _, ok := w.engine.(*deb.Deb); ok {
				w.pendingMu.Lock()
				w.possibleWinning = block // made for me, saving possible block
				w.pendingMu.Unlock()

				log.Info("Save possible block for league broadcasting", "hash", w.possibleWinning.Hash())
			} else {
				// Different block could share same sealhash, deep copy here to prevent write-write conflict.
				var (
					receipts = make([]*types.Receipt, len(task.receipts))
					logs     []*types.Log
				)
				for i, taskReceipt := range task.receipts {
					receipt := new(types.Receipt)
					receipts[i] = receipt
					*receipt = *taskReceipt

					// add block location fields
					receipt.BlockHash = hash
					receipt.BlockNumber = block.Number()
					receipt.TransactionIndex = uint(i)

					// Update the block hash in all logs since it is now available and not when the
					// receipt/log of individual transactions were created.
					receipt.Logs = make([]*types.Log, len(taskReceipt.Logs))
					for i, taskLog := range taskReceipt.Logs {
						log := new(types.Log)
						receipt.Logs[i] = log
						*log = *taskLog
						log.BlockHash = hash
					}
					logs = append(logs, receipt.Logs...)
				}
				// Commit block and state to database.
				_, err := w.chain.WriteBlockWithState(block, receipts, task.state)
				if err != nil {
					log.Error("Failed writing block to chain", "err", err)
					continue
				}
				log.Info("Successfully sealed new block", "number", block.Number(), "sealhash", sealhash, "hash", hash,
					"elapsed", common.PrettyDuration(time.Since(task.createdAt)))

				// Broadcast the block and announce chain insertion event
				w.mux.Post(types.NewMinedBlockEvent{Block: block})

				// Insert the block into the set of pending ones to resultLoop for confirmations
				w.unconfirmed.Insert(block.NumberU64(), block.Hash())
			}
		case <-w.leagueBroadCastCh:
			if w.config.Deb != nil && w.fnStatus != types.LEAGUE_BROADCASTING {
				log.Error("Got leagueBroadCastCh But fnStatus is not LEAGUE_BROADCASTING", "fnStatus", w.fnStatus.String())
				continue
			}

			if w.possibleWinning == nil {
				continue
			}

			block := w.possibleWinning

			bHash := rlpHash(block) // 리그 브로드케스팅 블록 해시 (전체를 해시 한다)
			acc := accounts.Account{Address: w.coinbase}
			wallet, err := w.eth.AccountManager().Find(acc)
			if err != nil {
				log.Error("wallet not fount", "msg", err, "account", w.coinbase.String())
				continue
			}

			sign, err := wallet.SignHash(acc, bHash.Bytes())
			if err != nil {
				log.Error("Block Sign Hash", "msg", err)
				continue
			}

			w.newLeagueBlockFeed.Send(types.NewLeagueBlockEvent{Block: block, Address: w.coinbase, Sign: sign}) // league block for broadcasting
			otprn, _ := types.DecodeOtprn(block.Otprn())
			log.Info("Possible winning block and league broadcasting2", "number", block.Number(), "otprn", otprn.HashOtprn(), "hash", block.Hash(),
				"difficulty", block.Difficulty())
		case ev := <-w.leagueBlockCh:
			bypass := true
			switch w.fnStatus {
			case types.MAKE_BLOCK, types.LEAGUE_BROADCASTING:
				bypass = false
			}

			if bypass {
				continue
			}

			bHash := rlpHash(ev.Block)
			if err := verify.ValidationSignHash(ev.Sign, bHash, ev.Address); err != nil {
				log.Error("VerifySignature", "msg", err)
				continue
			}

			rblock := ev.Block // received block

			if err := w.engine.VerifyHeader(w.chain, rblock.Header(), false); err != nil {
				log.Error("Received league block verifyHeader", "msg", err, "hash", rblock.Header().Hash())
				continue
			}

			var wBlock *types.Block

			if engine, ok := w.engine.(*deb.Deb); ok {
				if err := engine.ValidationLeagueBlock(w.chain, rblock); err != nil {
					otprn, _ := types.DecodeOtprn(rblock.Otprn())

					log.Error("Received league block Validation League Block", "msg", err, "chainOtprn", w.chain.Engine().Otprn().HashOtprn(), "otprn", otprn.HashOtprn())
					continue
				}

				if wBlock = engine.SelectWinningBlock(w.possibleWinning, rblock); wBlock == nil {
					log.Error("SelectWinningBlock", "msg", "selected block was nil")
					continue
				}
			}

			if w.possibleWinning != nil && wBlock.Hash() == w.possibleWinning.Hash() {
				continue
			}

			w.pendingMu.Lock()
			w.possibleWinning = wBlock
			w.pendingMu.Unlock()

			bHash = rlpHash(w.possibleWinning) // 리그 브로드케스팅 블록 해시 (전체를 해시 한다)
			acc := accounts.Account{Address: w.coinbase}
			wallet, err := w.eth.AccountManager().Find(acc)
			if err != nil {
				log.Error("wallet not fount", "msg", err, "account", w.coinbase.String())
				continue
			}

			sign, err := wallet.SignHash(acc, bHash.Bytes())
			if err != nil {
				log.Error("Block Sign Hash", "msg", err)
				continue
			}

			w.newLeagueBlockFeed.Send(types.NewLeagueBlockEvent{Block: w.possibleWinning, Address: w.coinbase, Sign: sign}) // league block for broadcasting
			otprn, _ := types.DecodeOtprn(w.possibleWinning.Otprn())
			log.Info("Possible winning block and league broadcasting", "number", w.possibleWinning.Number(),
				"otprn", otprn.HashOtprn(), "hash", w.possibleWinning.Hash(),
				"difficulty", w.possibleWinning.Difficulty())

		case voters := <-w.voteResultCh:
			if w.fnStatus != types.VOTE_COMPLETE {
				log.Error("Got voteResultCh But fnStatus is not VOTE_COMPLETE", "fnStatus", w.fnStatus.String())
				continue
			}

			pBlock := w.possibleWinning
			if pBlock == nil {
				continue
			}

			fbHash := verify.ValidationFinalBlockHash(voters)
			if fbHash != pBlock.Hash() {
				log.Error("Vote result final block hash difference", "fbHash", fbHash, "pHash", pBlock.Hash())
				continue
			}

			w.pendingMu.Lock()
			w.possibleFinalBlock = pBlock.WithVoter(voters, trie.NewStackTrie(nil)) // add voters in block
			w.submitBlockCh <- w.possibleFinalBlock                                 // pass votershash
			w.possibleWinning = nil
			w.pendingMu.Unlock()

		case fnSign := <-w.fnSignCh:
			if w.fnStatus != types.REQ_FAIRNODE_SIGN {
				log.Error("Got FairnodeSign But fnStatus is not REQ_FAIRNODE_SIGN", "fnStatus", w.fnStatus.String())
				continue
			}

			if w.possibleFinalBlock == nil {
				log.Error("Possible final block is nil")
				continue
			}

			if bytes.Compare(fnSign, []byte{}) == 0 {
				log.Error("Empty fairnode signature")
				continue
			}

			block := w.possibleFinalBlock.WithFairnodeSign(fnSign) // fairnode sign add

			if engine, ok := w.engine.(*deb.Deb); ok {
				err := engine.VerifyFairnodeSign(block.Header())
				if err != nil {
					log.Error("Verify fairnode signature", "msg", err)
					continue
				}
			}

			w.finalBlock = block
			log.Info("Make Final Block for Finalize", "hash", w.finalBlock.Hash())
		case <-w.finalizeCh:
			if w.config.Deb != nil && w.fnStatus != types.FINALIZE {
				log.Error("Got Finalize But fnStatus is not FINALIZE", "fnStatus", w.fnStatus.String())
				continue
			}

			if w.finalBlock == nil {
				log.Error("Final Block is nil")
				continue
			}

			block := w.finalBlock

			var (
				sealhash = w.engine.SealHash(block.Header())
				hash     = block.Hash()
			)

			if w.current == nil || w.current.header.Number.Cmp(block.Number()) != 0 {
				if w.current == nil {
					log.Error("Block found but current is nil", "bNumber", block.Number(), "sealhash", sealhash, "hash", hash)
				} else {
					log.Error("Block found but not match block Number", "cNumber", w.current.header.Number, "bNumber", block.Number(), "sealhash", sealhash, "hash", hash)
				}

				continue
			}

			bstart := time.Now()

			// finalblock 검증 및 commit block
			var parent *types.Block
			parent = w.chain.GetBlock(block.ParentHash(), block.NumberU64()-1)

			state, err := w.chain.StateAt(parent.Root())
			if err != nil {
				log.Error("Worker result StateNew Error", "err", err)
			}

			// Process block using the parent state as reference point.
			receipts, logs, usedGas, err := w.chain.Processor().Process(block, state, w.chain.GetVmConifg())
			if err != nil {
				log.Error("Worker result Processor Error", "err", err)
			}

			// Validate the state using the default validator
			err = w.chain.Validator().ValidateState(block, parent, state, receipts, usedGas)
			if err != nil {
				log.Error("Worker result ValidateState Error", "err", err)
			}

			//Commit block and state to database.
			stat, err := w.chain.WriteBlockWithState(block, receipts, state)
			if err != nil {
				log.Error("Failed writing block to chain", "err", err)
				continue
			}

			log.Info("Successfully sealed new block", "number", block.Number(), "sealhash", sealhash, "hash", hash,
				"elapsed", common.PrettyDuration(time.Since(bstart)))

			// Broadcast the block and announce chain insertion event
			w.mux.Post(types.NewMinedBlockEvent{Block: block})

			var CanonStatTy, SideStatTy bool
			var events []interface{}
			switch stat {
			case core.CanonStatTy:
				CanonStatTy = true
				events = append(events, types.ChainEvent{Block: block, Hash: block.Hash(), Logs: logs})
				events = append(events, types.ChainHeadEvent{Block: block})
			case core.SideStatTy:
				SideStatTy = true
				events = append(events, types.ChainSideEvent{Block: block})
			}

			log.Trace("WriteBlockWithState", "current", w.current.header.Number.String(), "CanonStatTy", CanonStatTy, "SideStatTy", SideStatTy)
			w.chain.PostChainEvents(events, logs)

			// Insert the block into the set of pending ones to resultLoop for confirmations
			w.unconfirmed.Insert(block.NumberU64(), block.Hash())

			w.pendingMu.Lock()
			w.possibleFinalBlock = nil
			w.finalBlock = nil
			w.pendingMu.Unlock()
		case <-w.exitCh:
			return
		}
	}
}

// makeCurrent creates a new environment for the current cycle.
func (w *worker) makeCurrent(parent *types.Block, header *types.Header) error {
	state, err := w.chain.StateAt(parent.Root())
	if err != nil {
		return err
	}

	// don't commit the state during tracing for circuit capacity checker, otherwise we cannot revert.
	// and even if we don't commit the state, the `refund` value will still be correct, as explained in `CommitTransaction`
	commitStateAfterApply := false
	traceEnv, err := core.CreateTraceEnv(w.chain.Config(), w.chain, w.engine, w.chain.DB(), state, parent,
		// new block with a placeholder tx, for traceEnv's ExecutionResults length & TxStorageTraces length
		types.NewBlockWithHeader(header).WithBody([]*types.Transaction{types.NewTx(&types.LegacyTx{})}, nil),
		commitStateAfterApply)
	if err != nil {
		return err
	}

	state.StartPrefetcher("miner")

	env := &environment{
		signer:    types.NewEIP155Signer(w.config.ChainID),
		state:     state,
		ancestors: mapset.NewSet(),
		family:    mapset.NewSet(),
		header:    header,
		traceEnv:  traceEnv,
	}

	// when 08 is processed ancestors contain 07 (quick block)
	for _, ancestor := range w.chain.GetBlocksFromHash(parent.Hash(), 7) {
		env.family.Add(ancestor.Hash())
		env.ancestors.Add(ancestor.Hash())
	}

	// Keep track of transactions which return debErrors so they can be removed
	env.count = 0
	w.current = env
	return nil
}

// updateSnapshot updates pending snapshot block and state.
// Note this function assumes the current variable is thread safe.
func (w *worker) updateSnapshot() {
	w.snapshotMu.Lock()
	defer w.snapshotMu.Unlock()

	w.snapshotBlock = types.NewBlock(
		w.current.header,
		w.current.txs,
		w.current.receipts,
		[]*types.Voter{},
		trie.NewStackTrie(nil),
	)

	w.snapshotState = w.current.state.Copy()
}

func (w *worker) commitTransaction(tx *types.Transaction, coinbase common.Address) ([]*types.Log, *types.BlockTrace, error) {
	var traces *types.BlockTrace
	var err error

	snap := w.current.state.Snapshot()
	// ToDo - CSW
	// 1. check circuit capacity before 'core.ApplyTransaction'
	// 2. Get BlockTrace
	traces, err = w.current.traceEnv.GetBlockTrace(
		types.NewBlockWithHeader(w.current.header).WithBody([]*types.Transaction{tx}, nil),
	)
	w.current.state.RevertToSnapshot(snap)
	if err != nil {
		return nil, nil, err
	}

	receipt, _, err := core.ApplyTransaction(w.config, w.chain, &coinbase, w.current.gasPool, w.current.state, w.current.header, tx, &w.current.header.GasUsed, vm.Config{})
	if err != nil {
		w.current.state.RevertToSnapshot(snap)
		return nil, traces, err
	}

	//withTimer(l2CommitTxCCCTimer, func() {
	//	accRows, err = w.circuitCapacityChecker.ApplyTransaction(traces)
	//})
	//if err != nil {
	//	return nil, traces, err
	//}
	// w.circuitCapacityChecker.ApplyTransaction(traces)
	w.current.txs = append(w.current.txs, tx)
	w.current.receipts = append(w.current.receipts, receipt)

	return receipt.Logs, traces, nil
}

func (w *worker) commitTransactions(txs *types.TransactionsByPriceAndNonce, coinbase common.Address, interrupt *int32) bool {
	// Short circuit if current is nil
	if w.current == nil {
		return true
	}
	if w.current.gasPool == nil {
		w.current.gasPool = new(core.GasPool).AddGas(w.current.header.GasLimit)
	}

	var coalescedLogs []*types.Log

	for {
		// In the following three cases, we will interrupt the execution of the transaction.
		// (1) new head block event arrival, the interrupt signal is 1
		// (2) worker start or restart, the interrupt signal is 1
		// (3) worker recreate the mining block with any newly arrived transactions, the interrupt signal is 2.
		// For the first two cases, the semi-finished work will be discarded.
		// For the third case, the semi-finished work will be submitted to the consensus engine.
		if interrupt != nil && atomic.LoadInt32(interrupt) != commitInterruptNone {
			// Notify resubmit loop to increase resubmitting interval due to too frequent commits.
			if atomic.LoadInt32(interrupt) == commitInterruptResubmit {
				ratio := float64(w.current.header.GasLimit-w.current.gasPool.Gas()) / float64(w.current.header.GasLimit)
				if ratio < 0.1 {
					ratio = 0.1
				}
				w.resubmitAdjustCh <- &intervalAdjust{
					ratio: ratio,
					inc:   true,
				}
			}
			return atomic.LoadInt32(interrupt) == commitInterruptNewHead
		}
		// If we don't have enough gas for any further transactions then we're done
		if w.current.gasPool.Gas() < params.TxGas {
			log.Info("Not enough gas for further transactions", "have", w.current.gasPool, "want", params.TxGas)
			break
		}
		// Retrieve the next transaction and abort if all done
		tx := txs.Peek()
		if tx == nil {
			break
		}
		// Error may be ignored here. The error has already been checked
		// during transaction acceptance is the transaction pool.
		//
		// We use the eip155 signer regardless of the current hf.
		from, _ := tx.Sender(w.current.signer)
		// Check whether the tx is replay protected. If we're not in the EIP155 hf
		// phase, start ignoring the sender until we do.
		if tx.Protected() && !w.config.IsEIP155(w.current.header.Number) {
			log.Info("Ignoring reply protected transaction", "hash", tx.Hash(), "eip155", w.config.EIP155Block)
			txs.Pop()
			continue
		}
		// Start executing the transaction
		w.current.state.Prepare(tx.Hash(), common.Hash{}, w.current.count)

		// ToDo - CSW
		// traces 처리
		logs, _, err := w.commitTransaction(tx, coinbase)
		switch err {
		case core.ErrGasLimitReached:
			// Pop the current out-of-gas transaction without shifting in the next from the account
			log.Trace("Gas limit exceeded for current block", "sender", from)
			txs.Pop()

		case core.ErrNonceTooLow:
			// New head notification data race between the transaction pool and miner, shift
			log.Trace("Skipping transaction with low nonce", "sender", from, "nonce", tx.Nonce())
			txs.Shift()

		case core.ErrNonceTooHigh:
			// Reorg notification data race between the transaction pool and miner, skip account =
			log.Trace("Skipping account with hight nonce", "sender", from, "nonce", tx.Nonce())
			txs.Pop()

		case nil:
			// Everything ok, collect the logs and shift in the next transaction from the same account
			coalescedLogs = append(coalescedLogs, logs...)
			w.current.count++
			txs.Shift()

		default:
			// Strange error, discard the transaction and get the next in line (note, the
			// nonce-too-high clause will prevent us from executing in vain).
			log.Debug("Transaction failed, account skipped", "hash", tx.Hash(), "err", err)
			txs.Shift()
		}
	}
	if !w.isRunning() && len(coalescedLogs) > 0 {
		// We don't push the pendingLogsEvent while we are mining. The reason is that
		// when we are mining, the worker will regenerate a mining block every 3 seconds.
		// In order to avoid pushing the repeated pendingLog, we disable the pending log pushing.

		// make a copy, the state caches the logs and these logs get "upgraded" from pending to mined
		// logs by filling in the block hash when the block was mined by the local miner. This can
		// cause a race condition if a log was "upgraded" before the PendingLogsEvent is processed.
		cpy := make([]*types.Log, len(coalescedLogs))
		for i, l := range coalescedLogs {
			cpy[i] = new(types.Log)
			*cpy[i] = *l
		}
		// go w.mux.Post(types.PendingLogsEvent{Logs: cpy})
		w.pendingLogsFeed.Send(cpy)
	}

	// Notify resubmit loop to decrease resubmitting interval if current interval is larger
	// than the user-specified one.
	if interrupt != nil {
		w.resubmitAdjustCh <- &intervalAdjust{inc: false}
	}

	return false
}

// commitNewWork generates several new sealing tasks based on the parent block.
func (w *worker) commitNewWork(interrupt *int32, noempty bool, timestamp int64) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	tstart := time.Now()
	parent := w.chain.CurrentBlock()

	if parent.Time().Cmp(new(big.Int).SetInt64(timestamp)) >= 0 {
		timestamp = parent.Time().Int64() + 1
	}

	// this will ensure we're not going off too far in the future
	if now := time.Now().Unix(); timestamp > now+1 {
		wait := time.Duration(timestamp-now) * time.Second
		log.Info("Mining too far in the future", "wait", common.PrettyDuration(wait))
		time.Sleep(wait)
	}

	num := parent.Number()
	var header *types.Header
	if _, ok := w.engine.(*deb.Deb); ok {
		header = &types.Header{
			ParentHash: parent.Hash(),
			Number:     num.Add(num, common.Big1),
			// Set GasLimit in Prepare() using otprn
			//GasLimit:   core.CalcGasLimit(parent, w.gasFloor, w.gasCeil),
			Extra: w.extra,
			Time:  big.NewInt(timestamp),
		}
	} else {
		header = &types.Header{
			ParentHash: parent.Hash(),
			Number:     num.Add(num, common.Big1),
			GasLimit:   core.CalcGasLimitEth(parent.GasLimit(), w.gasCeil),
			Extra:      w.extra,
			Time:       big.NewInt(timestamp),
		}
	}

	// Only set the coinbase if our consensus engine is running (avoid spurious block rewards)
	if w.isRunning() {
		if w.coinbase == (common.Address{}) {
			log.Error("Refusing to mine without coinbase")
			return
		}

		header.Coinbase = w.coinbase
		if err := w.engine.Prepare(w.chain, header); err != nil {
			log.Error("Failed to prepare header for mining", "err", err)
			return
		}

		// If we are care about TheDAO hard-fork check whether to override the extra-data or not
		if daoBlock := w.config.DAOForkBlock; daoBlock != nil {
			// Check whether the block is among the fork extra-override range
			limit := new(big.Int).Add(daoBlock, params.DAOForkExtraRange)
			if header.Number.Cmp(daoBlock) >= 0 && header.Number.Cmp(limit) < 0 {
				// Depending whether we support or oppose the fork, override differently
				if w.config.DAOForkSupport {
					header.Extra = common.CopyBytes(params.DAOForkBlockExtra)
				} else if bytes.Equal(header.Extra, params.DAOForkBlockExtra) {
					header.Extra = []byte{} // If miner opposes, don't let it use the reserved extra-data
				}
			}
		}

		// Could potentially happen if starting to mine in an odd state.
		err := w.makeCurrent(parent, header)
		if err != nil {
			log.Error("Failed to create mining context", "err", err)
			return
		}

		// Create the current work task and check any fork transitions needed
		env := w.current
		if w.config.DAOForkSupport && w.config.DAOForkBlock != nil && w.config.DAOForkBlock.Cmp(header.Number) == 0 {
			misc.ApplyDAOHardFork(env.state)
		}

		// Accumulate the uncles for the current block
		for hash, uncle := range w.possibleUncles {
			if uncle.NumberU64()+staleThreshold <= header.Number.Uint64() {
				delete(w.possibleUncles, hash)
			}
		}

		// Fill the block with all available pending transactions.
		pending, pendingJoinTx, err := w.eth.TxPool().Pending()
		if err != nil {
			log.Error("Failed to fetch pending transactions", "err", err)
			return
		}

		// Short circuit if there is no available pending transactions
		// otprn check
		if _, ok := w.engine.(*deb.Deb); ok {
			otprn, err := types.DecodeOtprn(header.Otprn)
			if err != nil {
				return
			}

			if otprn.FnAddr == params.TestFairnodeAddr { // TEST
				if len(pending) == 0 || len(pendingJoinTx) == 0 {
					w.updateSnapshot()
					return
				}
			} else {

				if len(pending) == 0 || len(pendingJoinTx) == 0 {
					w.updateSnapshot()
					return
				}

				// miner's join transaction is empty
				if txs, exist := pendingJoinTx[w.coinbase]; !exist || txs.Len() == 0 {
					log.Error("miner's join transaction is empty")
					w.updateSnapshot()
					return
				}
			}
		}

		// Split the pending transactions into locals and remotes
		localTxs, remoteTxs := make(map[common.Address]types.Transactions), pending
		for _, account := range w.eth.TxPool().Locals() {
			if txs := remoteTxs[account]; len(txs) > 0 {
				delete(remoteTxs, account)
				localTxs[account] = txs
			}
		}

		if len(localTxs) > 0 {
			txs := types.NewTransactionsByPriceAndNonce(w.current.signer, localTxs)
			if w.commitTransactions(txs, w.coinbase, interrupt) {
				return
			}
		}
		if len(remoteTxs) > 0 {
			txs := types.NewTransactionsByPriceAndNonce(w.current.signer, remoteTxs)
			if w.commitTransactions(txs, w.coinbase, interrupt) {
				return
			}
		}

		if _, ok := w.engine.(*layer2.Layer2); ok { // Layer2 의 경우는 블록을 만들지 않고 tx만 orderer에게 전달...
			// orderer에 Transactions 전송
			w.layer2Client.Transaction(w.config.ChainID.Uint64(), w.current.header.Number.Uint64(), w.current.txs)
			return
		}

		if err := w.commit(w.fullTaskHook, true, tstart); err != nil {
			log.Error("Failed commit for mining", "err", err, "update", true)
			return
		}
	}
}

func (w *worker) layser2SendTransaction(interrupt *int32, noempty bool, timestamp int64) {
	pending, _, err := w.eth.TxPool().Pending()
	if err != nil {
		log.Error("Failed to fetch pending transactions", "err", err)
		return
	}

	// Layer2 의 경우는 블록을 만들지 않고 tx만 orderer에게 전달...
	// orderer에 Transactions 전송
	var txs []*types.Transaction
	for _, tx := range pending {
		for _, item := range tx {
			txs = append(txs, item)
		}
	}
	// Check Layer2 Client running
	if w.layer2Client.Miner() != nil {
		w.layer2Client.Transaction(w.config.ChainID.Uint64(), uint64(len(txs)), txs)
	}
}

func (w *worker) layer2CommitNewWork(pending map[common.Address]types.Transactions, timestamp int64) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	tstart := time.Now()
	parent := w.chain.CurrentBlock()

	if parent.Time().Cmp(new(big.Int).SetInt64(timestamp)) >= 0 {
		timestamp = parent.Time().Int64() + 1
	}

	// this will ensure we're not going off too far in the future
	if now := time.Now().Unix(); timestamp > now+1 {
		wait := time.Duration(timestamp-now) * time.Second
		log.Info("Mining too far in the future", "wait", common.PrettyDuration(wait))
		time.Sleep(wait)
	}

	num := parent.Number()
	var header *types.Header

	header = &types.Header{
		ParentHash: parent.Hash(),
		Number:     num.Add(num, common.Big1),
		GasLimit:   core.CalcGasLimitEth(parent.GasLimit(), w.gasCeil),
		Extra:      w.extra,
		Time:       big.NewInt(timestamp),
	}

	// Only set the coinbase if our consensus engine is running (avoid spurious block rewards)
	if w.isRunning() {
		if w.coinbase == (common.Address{}) {
			log.Error("Refusing to mine without coinbase")
			return
		}

		header.Coinbase = w.coinbase
		if err := w.engine.Prepare(w.chain, header); err != nil {
			log.Error("Failed to prepare header for mining", "err", err)
			return
		}

		// If we are care about TheDAO hard-fork check whether to override the extra-data or not
		if daoBlock := w.config.DAOForkBlock; daoBlock != nil {
			// Check whether the block is among the fork extra-override range
			limit := new(big.Int).Add(daoBlock, params.DAOForkExtraRange)
			if header.Number.Cmp(daoBlock) >= 0 && header.Number.Cmp(limit) < 0 {
				// Depending whether we support or oppose the fork, override differently
				if w.config.DAOForkSupport {
					header.Extra = common.CopyBytes(params.DAOForkBlockExtra)
				} else if bytes.Equal(header.Extra, params.DAOForkBlockExtra) {
					header.Extra = []byte{} // If miner opposes, don't let it use the reserved extra-data
				}
			}
		}

		// Could potentially happen if starting to mine in an odd state.
		err := w.makeCurrent(parent, header)
		if err != nil {
			log.Error("Failed to create mining context", "err", err)
			return
		}

		// Create the current work task and check any fork transitions needed
		env := w.current
		if w.config.DAOForkSupport && w.config.DAOForkBlock != nil && w.config.DAOForkBlock.Cmp(header.Number) == 0 {
			misc.ApplyDAOHardFork(env.state)
		}

		// Accumulate the uncles for the current block
		for hash, uncle := range w.possibleUncles {
			if uncle.NumberU64()+staleThreshold <= header.Number.Uint64() {
				delete(w.possibleUncles, hash)
			}
		}

		if len(pending) > 0 {
			txs := types.NewTransactionsByPriceAndNonce(w.current.signer, pending)
			if w.commitTransactions(txs, w.coinbase, nil) {
				return
			}
		}

		if err := w.commit(w.fullTaskHook, true, tstart); err != nil {
			log.Error("Failed commit for mining", "err", err, "update", true)
			return
		}
	}
}

// Layer2의 경우는 서버에서 채굴 리스트를 받아서 채굴을 진행
func (w *worker) commitLoop() {
	for {
		select {
		case txlist, ok := <-w.layer2Client.TxListCh:
			if ok {
				w.layer2CommitNewWork(txlist, time.Now().Unix())
			}
		case <-w.exitCh:
			return
		}
	}
}

// commit runs any post-transaction state modifications, assembles the final block
// and commits new work if consensus engine is running.
func (w *worker) commit(interval func(), update bool, start time.Time) error {
	// Deep copy receipts here to avoid interaction between different tasks.
	receipts := make([]*types.Receipt, len(w.current.receipts))
	for i, l := range w.current.receipts {
		receipts[i] = new(types.Receipt)
		*receipts[i] = *l
	}

	s := w.current.state.Copy()
	block, err := w.engine.Finalize(w.chain, w.current.header, s, w.current.txs, w.current.receipts, []*types.Voter{})
	if err != nil {
		return err
	}

	if engine, ok := w.engine.(*deb.Deb); ok {
		// otprn check
		otprn, err := types.DecodeOtprn(block.Otprn())
		if err != nil {
			return err
		}
		if err := otprn.ValidateSignature(); err != nil {
			return err
		}
		if otprn.FnAddr != params.TestFairnodeAddr { // TEST CHECK
			if err := engine.ValidationLeagueBlock(w.chain, block); err != nil {
				return err
			}
		}
	}

	if w.isRunning() {
		if interval != nil {
			interval()
		}

		select {
		case w.taskCh <- &task{receipts: receipts, state: s, block: block, createdAt: time.Now()}:
			w.unconfirmed.Shift(block.NumberU64() - 1)

			feesWei := new(big.Int)
			// general transaction fee
			for i, tx := range block.Transactions() {
				feesWei.Add(feesWei, new(big.Int).Mul(new(big.Int).SetUint64(receipts[i].GasUsed), tx.GasPrice()))
			}
			feesEth := new(big.Float).Quo(new(big.Float).SetInt(feesWei), new(big.Float).SetInt(big.NewInt(params.Daon)))
			log.Info("Commit new mining work", "number", block.Number(), "sealhash", w.engine.SealHash(block.Header()), "txs", w.current.count, "gas", block.GasUsed(), "fees", feesEth, "elapsed", common.PrettyDuration(time.Since(start)))

		case <-w.exitCh:
			log.Info("Worker has exited")
		}
	}

	if update {
		w.updateSnapshot()
	}
	return nil
}

func (w *worker) layer2Start() {
	if err := w.layer2Client.Start(w.eth); err == nil {
		w.startCh <- struct{}{}
	} else {
		log.Error("layer2 client start", "msg", err)
		return
	}
}

func (w *worker) l2clientStatusLoop() {
	defer log.Warn("layer2 client was dead and worker exited")
	defer w.l2ClientCLoseSub.Unsubscribe()

	for {
		select {
		case <-w.l2ClientCloseCh:
			log.Warn("layser2 client was close")
			w.layer2Client.Stop()
			time.AfterFunc(10*time.Second, func() {
				if w.isRunning() {
					log.Info("Retry Mining")
					w.layer2Start()
				}
			})
		case <-w.l2ClientCLoseSub.Err():
			return
		}
	}
}
