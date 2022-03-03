// Copyright (C) 2019-2021, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package proposervm

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/ava-labs/avalanchego/database"
	"github.com/ava-labs/avalanchego/database/manager"
	"github.com/ava-labs/avalanchego/database/prefixdb"
	"github.com/ava-labs/avalanchego/database/versiondb"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow"
	"github.com/ava-labs/avalanchego/snow/choices"
	"github.com/ava-labs/avalanchego/snow/consensus/snowman"
	"github.com/ava-labs/avalanchego/snow/engine/common"
	"github.com/ava-labs/avalanchego/snow/engine/snowman/block"
	"github.com/ava-labs/avalanchego/utils"
	"github.com/ava-labs/avalanchego/utils/math"
	"github.com/ava-labs/avalanchego/utils/timer/mockable"
	"github.com/ava-labs/avalanchego/vms/proposervm/indexer"
	"github.com/ava-labs/avalanchego/vms/proposervm/proposer"
	"github.com/ava-labs/avalanchego/vms/proposervm/scheduler"
	"github.com/ava-labs/avalanchego/vms/proposervm/state"
	"github.com/ava-labs/avalanchego/vms/proposervm/tree"
	"github.com/olekukonko/tablewriter"

	ecommon "github.com/ethereum/go-ethereum/common"

	statelessblock "github.com/ava-labs/avalanchego/vms/proposervm/block"
)

const (
	// minBlockDelay should be kept as whole seconds because block timestamps
	// are only specific to the second.
	minBlockDelay                = time.Second
	checkIndexedFrequency        = 10 * time.Second
	optimalHeightDelay    uint64 = 256
)

var (
	_ block.ChainVM              = &VM{}
	_ block.BatchedChainVM       = &VM{}
	_ block.HeightIndexedChainVM = &VM{}

	dbPrefix = []byte("proposervm")
)

type VM struct {
	block.ChainVM
	activationTime      time.Time
	minimumPChainHeight uint64

	state.State
	resetHeightIndexOngoing utils.AtomicBool
	hIndexer                indexer.HeightIndexer

	proposer.Windower
	tree.Tree
	scheduler.Scheduler
	mockable.Clock

	ctx         *snow.Context
	db          *versiondb.Database
	toScheduler chan<- common.Message

	// Block ID --> Block
	// Each element is a block that passed verification but
	// hasn't yet been accepted/rejected
	verifiedBlocks map[ids.ID]PostForkBlock
	preferred      ids.ID
	bootstrapped   bool
	context        context.Context
	onShutdown     func()

	// lastAcceptedOptionTime is set to the last accepted PostForkBlock's
	// timestamp if the last accepted block has been a PostForkOption block
	// since having initialized the VM.
	lastAcceptedTime time.Time
}

func New(
	vm block.ChainVM,
	activationTime time.Time,
	minimumPChainHeight uint64,
	resetHeightIndex bool,
) *VM {
	proVM := &VM{
		ChainVM:             vm,
		activationTime:      activationTime,
		minimumPChainHeight: minimumPChainHeight,
	}

	proVM.resetHeightIndexOngoing.SetValue(resetHeightIndex)
	return proVM
}

type counter uint64

func (c counter) String() string {
	return fmt.Sprintf("%d", c)
}

func (c counter) Percentage(current uint64) string {
	return fmt.Sprintf("%d", current*100/uint64(c))
}

// stat stores sizes and count for a parameter
type stat struct {
	size  ecommon.StorageSize
	count counter
	l     sync.RWMutex
}

// Add size to the stat and increase the counter by 1
func (s *stat) Add(size ecommon.StorageSize) {
	s.l.Lock()
	defer s.l.Unlock()
	s.size += size
	s.count++
}

func (s *stat) AddBytes(b []byte) {
	s.Add(ecommon.StorageSize(len(b)))
}

func (s *stat) Size() string {
	s.l.RLock()
	defer s.l.RUnlock()
	return s.size.String()
}

func (s *stat) Count() string {
	s.l.RLock()
	defer s.l.RUnlock()
	return s.count.String()
}

func (a ADatabase) DBUsageLogger(ctx context.Context, f *os.File) {
	t := time.NewTicker(10 * time.Second)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Display the database statistic.
			stats := [][]string{
				{a.chainID, "GET", a.get.Size(), a.get.Count()},
				{a.chainID, "PUT", a.put.Size(), a.put.Count()},
			}
			table := tablewriter.NewWriter(f)
			table.SetHeader([]string{"Chain", "Op", "Size", "Items"})
			table.AppendBulk(stats)
			table.Render()
		}
	}
}

type ADatabase struct {
	database.Database

	chainID string
	get     *stat
	put     *stat
}

func (a ADatabase) Put(k []byte, v []byte) error {
	a.put.AddBytes(v)
	return a.Database.Put(k, v)
}

func (a ADatabase) Get(k []byte) ([]byte, error) {
	dat, err := a.Database.Get(k)
	a.get.AddBytes(dat)
	return dat, err
}

func (vm *VM) Initialize(
	ctx *snow.Context,
	dbManager manager.Manager,
	genesisBytes []byte,
	upgradeBytes []byte,
	configBytes []byte,
	toEngine chan<- common.Message,
	fxs []*common.Fx,
	appSender common.AppSender,
) error {
	vm.ctx = ctx
	rawDB := dbManager.Current().Database
	prefixDB := ADatabase{prefixdb.New(dbPrefix, rawDB), ctx.ChainID.String(), &stat{}, &stat{}}
	vm.db = versiondb.New(prefixDB)
	vm.State = state.New(vm.db)
	vm.Windower = proposer.New(ctx.ValidatorState, ctx.SubnetID, ctx.ChainID)
	vm.Tree = tree.New()

	indexerDB := versiondb.New(vm.db)
	indexerState := state.New(indexerDB)
	vm.hIndexer = indexer.NewHeightIndexer(vm, vm.ctx.Log, indexerState)

	scheduler, vmToEngine := scheduler.New(vm.ctx.Log, toEngine)
	vm.Scheduler = scheduler
	vm.toScheduler = vmToEngine

	go ctx.Log.RecoverAndPanic(func() {
		scheduler.Dispatch(time.Now())
	})

	vm.verifiedBlocks = make(map[ids.ID]PostForkBlock)
	context, cancel := context.WithCancel(context.Background())
	vm.context = context
	vm.onShutdown = cancel
	go prefixDB.DBUsageLogger(vm.context, os.Stderr)

	err := vm.ChainVM.Initialize(
		ctx,
		dbManager,
		genesisBytes,
		upgradeBytes,
		configBytes,
		vmToEngine,
		fxs,
		appSender,
	)
	if err != nil {
		return err
	}

	if err := vm.repairAcceptedChain(); err != nil {
		return err
	}

	if err := vm.setLastAcceptedOptionTime(); err != nil {
		return err
	}

	// check and possibly rebuild height index
	innerHVM, ok := vm.ChainVM.(block.HeightIndexedChainVM)
	if !ok {
		return nil // nothing else to do
	}

	// asynchronously rebuild height index, if needed
	go func() {
		// If index reset has been requested, carry it out first
		if vm.resetHeightIndexOngoing.GetValue() {
			if err := indexerState.ResetHeightIndex(); err != nil {
				vm.ctx.Log.Error("block height indexing reset failed with: %s", err)
				return
			}
			if err := indexerState.Commit(); err != nil {
				vm.ctx.Log.Error("block height indexing reset commit failed with: %s", err)
				return
			}
			if err := vm.Commit(); err != nil {
				vm.ctx.Log.Error("block height indexing reset atomic commit failed with: %s", err)
				return
			}

			vm.ctx.Log.Info("block height indexing reset finished")
			vm.resetHeightIndexOngoing.SetValue(false)
		}

		// Poll until the underlying chain's index is complete or shutdown is
		// called.
		ticker := time.NewTicker(checkIndexedFrequency)
		defer ticker.Stop()
		for {
			// The underlying VM expects the lock to be held here.
			vm.ctx.Lock.Lock()
			err := innerHVM.VerifyHeightIndex()
			vm.ctx.Lock.Unlock()

			if err == nil {
				// innerVM indexing complete. Let re-index this machine
				break
			}
			if err != block.ErrIndexIncomplete {
				vm.ctx.Log.Error("block height indexing failed with: %s", err)
				return
			}

			// innerVM index is incomplete. Wait for completion and retry
			select {
			case <-vm.context.Done():
				return
			case <-ticker.C:
			}
		}

		shouldRepair, err := vm.shouldHeightIndexBeRepaired()
		if err != nil {
			vm.ctx.Log.Error("could not verify the status of height indexing: %s", err)
			return
		}
		if !shouldRepair {
			vm.ctx.Log.Info("block height indexing is already complete")
			vm.hIndexer.MarkRepaired()
			return
		}

		err = vm.hIndexer.RepairHeightIndex(vm.context)
		if err == nil {
			vm.ctx.Log.Info("block height indexing finished")
			return
		}

		// Note that we don't check if `err` is `context.Canceled` here because
		// repairing the height index may have returned a non-standard error
		// due to the chain shutting down.
		if vm.context.Err() == nil {
			// The context wasn't closed, so the chain hasn't been shutdown.
			// This must have been an unexpected error.
			vm.ctx.Log.Error("block height indexing failed: %s", err)
		}
	}()

	return nil
}

// shutdown ops then propagate shutdown to innerVM
func (vm *VM) Shutdown() error {
	vm.onShutdown()

	if err := vm.db.Commit(); err != nil {
		return err
	}
	return vm.ChainVM.Shutdown()
}

func (vm *VM) SetState(state snow.State) error {
	vm.bootstrapped = (state == snow.NormalOp)
	return vm.ChainVM.SetState(state)
}

func (vm *VM) BuildBlock() (snowman.Block, error) {
	preferredBlock, err := vm.getBlock(vm.preferred)
	if err != nil {
		return nil, err
	}

	return preferredBlock.buildChild()
}

func (vm *VM) ParseBlock(b []byte) (snowman.Block, error) {
	if blk, err := vm.parsePostForkBlock(b); err == nil {
		return blk, nil
	}
	return vm.parsePreForkBlock(b)
}

func (vm *VM) GetBlock(id ids.ID) (snowman.Block, error) {
	return vm.getBlock(id)
}

func (vm *VM) SetPreference(preferred ids.ID) error {
	if vm.preferred == preferred {
		return nil
	}
	vm.preferred = preferred

	blk, err := vm.getPostForkBlock(preferred)
	if err != nil {
		return vm.ChainVM.SetPreference(preferred)
	}

	if err := vm.ChainVM.SetPreference(blk.getInnerBlk().ID()); err != nil {
		return err
	}

	pChainHeight, err := blk.pChainHeight()
	if err != nil {
		return err
	}

	// reset scheduler
	minDelay, err := vm.Windower.Delay(blk.Height()+1, pChainHeight, vm.ctx.NodeID)
	if err != nil {
		vm.ctx.Log.Debug("failed to fetch the expected delay due to: %s", err)
		// A nil error is returned here because it is possible that
		// bootstrapping caused the last accepted block to move past the latest
		// P-chain height. This will cause building blocks to return an error
		// until the P-chain's height has advanced.
		return nil
	}
	if minDelay < minBlockDelay {
		minDelay = minBlockDelay
	}

	preferredTime := blk.Timestamp()
	nextStartTime := preferredTime.Add(minDelay)
	vm.Scheduler.SetBuildBlockTime(nextStartTime)

	vm.ctx.Log.Debug("set preference to %s with timestamp %v; build time scheduled at %v",
		blk.ID(), preferredTime, nextStartTime)
	return nil
}

func (vm *VM) LastAccepted() (ids.ID, error) {
	lastAccepted, err := vm.State.GetLastAccepted()
	if err == database.ErrNotFound {
		return vm.ChainVM.LastAccepted()
	}
	return lastAccepted, err
}

func (vm *VM) repairAcceptedChain() error {
	lastAcceptedID, err := vm.GetLastAccepted()
	if err == database.ErrNotFound {
		// If the last accepted block isn't indexed yet, then the underlying
		// chain is the only chain and there is nothing to repair.
		return nil
	}
	if err != nil {
		return err
	}

	// Revert accepted blocks that weren't committed to the database.
	for {
		lastAccepted, err := vm.getPostForkBlock(lastAcceptedID)
		if err == database.ErrNotFound {
			// If the post fork block can't be found, it's because we're
			// reverting past the fork boundary. If this is the case, then there
			// is only one database to keep consistent, so there is nothing to
			// repair anymore.
			if err := vm.State.DeleteLastAccepted(); err != nil {
				return err
			}
			if err := vm.State.DeleteCheckpoint(); err != nil {
				return err
			}
			return vm.db.Commit()
		}
		if err != nil {
			return err
		}

		shouldBeAccepted := lastAccepted.getInnerBlk()

		// If the inner block is accepted, then we don't need to revert any more
		// blocks.
		if shouldBeAccepted.Status() == choices.Accepted {
			return vm.db.Commit()
		}

		// Mark the last accepted block as processing - rather than accepted.
		lastAccepted.setStatus(choices.Processing)
		if err := vm.State.PutBlock(lastAccepted.getStatelessBlk(), choices.Processing); err != nil {
			return err
		}

		// Advance to the parent block
		previousLastAcceptedID := lastAcceptedID
		lastAcceptedID = lastAccepted.Parent()
		if err := vm.State.SetLastAccepted(lastAcceptedID); err != nil {
			return err
		}

		// If the indexer checkpoint was previously pointing to the last
		// accepted block, roll it back to the new last accepted block.
		checkpoint, err := vm.State.GetCheckpoint()
		if err == database.ErrNotFound {
			continue
		}
		if err != nil {
			return err
		}
		if previousLastAcceptedID != checkpoint {
			continue
		}
		if err := vm.State.SetCheckpoint(lastAcceptedID); err != nil {
			return err
		}
	}
}

func (vm *VM) setLastAcceptedOptionTime() error {
	lastAcceptedID, err := vm.GetLastAccepted()
	if err == database.ErrNotFound {
		// If the last accepted block wasn't a PostFork block, then we don't
		// initialize the time.
		return nil
	}
	if err != nil {
		return err
	}

	lastAccepted, _, err := vm.State.GetBlock(lastAcceptedID)
	if err != nil {
		return err
	}

	if _, ok := lastAccepted.(statelessblock.SignedBlock); ok {
		// If the last accepted block wasn't a PostForkOption, then we don't
		// initialize the time.
		return nil
	}

	acceptedParent, err := vm.getPostForkBlock(lastAccepted.ParentID())
	if err != nil {
		return err
	}
	vm.lastAcceptedTime = acceptedParent.Timestamp()
	return nil
}

func (vm *VM) parsePostForkBlock(b []byte) (PostForkBlock, error) {
	statelessBlock, err := statelessblock.Parse(b)
	if err != nil {
		return nil, err
	}

	// if the block already exists, then make sure the status is set correctly
	blkID := statelessBlock.ID()
	blk, err := vm.getPostForkBlock(blkID)
	if err == nil {
		return blk, nil
	}
	if err != database.ErrNotFound {
		return nil, err
	}

	innerBlkBytes := statelessBlock.Block()
	innerBlk, err := vm.ChainVM.ParseBlock(innerBlkBytes)
	if err != nil {
		return nil, err
	}

	if statelessSignedBlock, ok := statelessBlock.(statelessblock.SignedBlock); ok {
		blk = &postForkBlock{
			SignedBlock: statelessSignedBlock,
			postForkCommonComponents: postForkCommonComponents{
				vm:       vm,
				innerBlk: innerBlk,
				status:   choices.Processing,
			},
		}
	} else {
		blk = &postForkOption{
			Block: statelessBlock,
			postForkCommonComponents: postForkCommonComponents{
				vm:       vm,
				innerBlk: innerBlk,
				status:   choices.Processing,
			},
		}
	}
	return blk, nil
}

func (vm *VM) parsePreForkBlock(b []byte) (*preForkBlock, error) {
	blk, err := vm.ChainVM.ParseBlock(b)
	return &preForkBlock{
		Block: blk,
		vm:    vm,
	}, err
}

func (vm *VM) getBlock(id ids.ID) (Block, error) {
	if blk, err := vm.getPostForkBlock(id); err == nil {
		return blk, nil
	}
	return vm.getPreForkBlock(id)
}

func (vm *VM) getPostForkBlock(blkID ids.ID) (PostForkBlock, error) {
	block, exists := vm.verifiedBlocks[blkID]
	if exists {
		return block, nil
	}

	statelessBlock, status, err := vm.State.GetBlock(blkID)
	if err != nil {
		return nil, err
	}

	innerBlkBytes := statelessBlock.Block()
	innerBlk, err := vm.ChainVM.ParseBlock(innerBlkBytes)
	if err != nil {
		return nil, err
	}

	if statelessSignedBlock, ok := statelessBlock.(statelessblock.SignedBlock); ok {
		return &postForkBlock{
			SignedBlock: statelessSignedBlock,
			postForkCommonComponents: postForkCommonComponents{
				vm:       vm,
				innerBlk: innerBlk,
				status:   status,
			},
		}, nil
	}
	return &postForkOption{
		Block: statelessBlock,
		postForkCommonComponents: postForkCommonComponents{
			vm:       vm,
			innerBlk: innerBlk,
			status:   status,
		},
	}, nil
}

func (vm *VM) getPreForkBlock(blkID ids.ID) (*preForkBlock, error) {
	blk, err := vm.ChainVM.GetBlock(blkID)
	return &preForkBlock{
		Block: blk,
		vm:    vm,
	}, err
}

func (vm *VM) storePostForkBlock(blk PostForkBlock) error {
	if err := vm.State.PutBlock(blk.getStatelessBlk(), blk.Status()); err != nil {
		return err
	}

	height := blk.Height()
	blkID := blk.ID()
	if err := vm.updateHeightIndex(height, blkID); err != nil {
		return err
	}
	return vm.db.Commit()
}

func (vm *VM) verifyAndRecordInnerBlk(postFork PostForkBlock) error {
	// If inner block's Verify returned true, don't call it again.
	//
	// Note that if [innerBlk.Verify] returns nil, this method returns nil. This
	// must always remain the case to maintain the inner block's invariant that
	// if it's Verify() returns nil, it is eventually accepted or rejected.
	currentInnerBlk := postFork.getInnerBlk()
	if originalInnerBlk, contains := vm.Tree.Get(currentInnerBlk); !contains {
		if err := currentInnerBlk.Verify(); err != nil {
			return err
		}
		vm.Tree.Add(currentInnerBlk)
	} else {
		postFork.setInnerBlk(originalInnerBlk)
	}

	vm.verifiedBlocks[postFork.ID()] = postFork
	return nil
}

// notifyInnerBlockReady tells the scheduler that the inner VM is ready to build
// a new block
func (vm *VM) notifyInnerBlockReady() {
	select {
	case vm.toScheduler <- common.PendingTxs:
	default:
		vm.ctx.Log.Debug("dropping message to consensus engine")
	}
}

func (vm *VM) optimalPChainHeight(minPChainHeight uint64) (uint64, error) {
	currentPChainHeight, err := vm.ctx.ValidatorState.GetCurrentHeight()
	if err != nil {
		return 0, err
	}
	if currentPChainHeight < optimalHeightDelay {
		return minPChainHeight, nil
	}
	optimalHeight := currentPChainHeight - optimalHeightDelay
	return math.Max64(optimalHeight, minPChainHeight), nil
}
