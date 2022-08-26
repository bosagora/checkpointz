package beacon

import (
	"context"
	"errors"
	"fmt"
	"time"

	v1 "github.com/attestantio/go-eth2-client/api/v1"
	"github.com/attestantio/go-eth2-client/spec"
	"github.com/attestantio/go-eth2-client/spec/phase0"
	"github.com/samcm/checkpointz/pkg/eth"
	"github.com/sirupsen/logrus"
)

func (d *Default) downloadServingCheckpoint(ctx context.Context, checkpoint *v1.Finality) error {
	upstream, err := d.nodes.
		Ready(ctx).
		DataProviders(ctx).
		PastFinalizedCheckpoint(ctx, checkpoint). // Ensure we attempt to fetch the bundle from a node that knows about the checkpoint.
		RandomNode(ctx)
	if err != nil {
		return err
	}

	block, err := d.fetchBundle(ctx, checkpoint.Finalized.Root, upstream)
	if err != nil {
		return err
	}

	// Validate that everything is ok to serve.
	// Lighthouse ref: https://lighthouse-book.sigmaprime.io/checkpoint-sync.html#alignment-requirements
	blockSlot, err := block.Slot()
	if err != nil {
		return fmt.Errorf("failed to get slot from block: %w", err)
	}

	// For simplicity we'll hardcode SLOTS_PER_EPOCH to 32.
	// TODO(sam.calder-mason): Fetch this from a beacon node and store it in the instance.
	const slotsPerEpoch = 32
	if blockSlot%slotsPerEpoch != 0 {
		return fmt.Errorf("block slot is not aligned from an epoch boundary: %d", blockSlot)
	}

	d.servingBundle = checkpoint
	d.metrics.ObserveServingEpoch(checkpoint.Finalized.Epoch)

	d.log.WithFields(
		logrus.Fields{
			"epoch": checkpoint.Finalized.Epoch,
			"root":  fmt.Sprintf("%#x", checkpoint.Finalized.Root),
		},
	).Info("Serving a new finalized checkpoint bundle")

	return nil
}

func (d *Default) checkGenesis(ctx context.Context) error {
	// No-Op if we already have the genesis block AND state stored.
	// Note: this check will constantly touch the genesis block and state in their
	// respective stores, ensuring that we never purge those items.
	block, err := d.blocks.GetBySlot(phase0.Slot(0))
	if err == nil && block != nil {
		stateRoot, errr := block.StateRoot()
		if errr == nil {
			if st, er := d.states.GetByStateRoot(stateRoot); er == nil && st != nil {
				return nil
			}
		}
	}

	d.log.Debug("Fetching genesis block and state")

	readyNodes := d.nodes.Ready(ctx)
	if len(readyNodes) == 0 {
		return errors.New("no nodes ready")
	}

	// Grab the genesis root
	randomNode, err := readyNodes.RandomNode(ctx)
	if err != nil {
		return err
	}

	genesisBlock, err := randomNode.Beacon.FetchBlock(ctx, "genesis")
	if err != nil {
		return err
	}

	genesisBlockRoot, err := genesisBlock.Root()
	if err != nil {
		return err
	}

	upstream, err := d.nodes.Ready(ctx).DataProviders(ctx).RandomNode(ctx)
	if err != nil {
		return err
	}

	// Fetch the bundle
	if _, err := d.fetchBundle(ctx, genesisBlockRoot, upstream); err != nil {
		return err
	}

	d.log.WithFields(logrus.Fields{
		"root": fmt.Sprintf("%#x", genesisBlockRoot),
	}).Info("Fetched genesis bundle")

	return nil
}

func (d *Default) fetchHistoricalCheckpoints(ctx context.Context, checkpoint *v1.Finality) error {
	if d.spec == nil {
		return errors.New("beacon spec unavailable")
	}

	if d.genesis == nil {
		return errors.New("genesis time unavailable")
	}

	// Download the previous n epochs worth of epoch boundaries if they don't already exist
	upstream, err := d.nodes.
		Ready(ctx).
		DataProviders(ctx).
		PastFinalizedCheckpoint(ctx, checkpoint).
		RandomNode(ctx)
	if err != nil {
		return errors.New("no data provider node available")
	}

	sp := d.spec

	slotsInScope := make(map[phase0.Slot]struct{})
	historicalFailureLimit := 5
	// Calculate the epoch boundaries we need to fetch
	// We'll derive the current finalized slot and then work back in intervals of SLOTS_PER_EPOCH.
	currentSlot := uint64(checkpoint.Finalized.Epoch) * uint64(sp.SlotsPerEpoch)
	for i := uint64(1); i < uint64(d.config.HistoricalEpochCount); i++ {
		if currentSlot-(i*uint64(sp.SlotsPerEpoch)) == 0 {
			continue
		}

		slot := phase0.Slot(currentSlot - i*uint64(sp.SlotsPerEpoch))
		slotsInScope[slot] = struct{}{}

		failureCount, exists := d.historicalSlotFailures[slot]
		if !exists {
			d.historicalSlotFailures[slot] = 0
		}

		if _, err := d.blocks.GetBySlot(slot); err == nil {
			continue
		}

		if failureCount >= historicalFailureLimit {
			continue
		}

		if _, err := d.downloadBlock(ctx, slot, upstream); err != nil {
			failureCount++

			d.log.WithError(err).
				WithField("slot", eth.SlotAsString(slot)).
				WithField("failure_count", failureCount).
				Error("Failed to download historical block")
		}

		if failureCount == historicalFailureLimit {
			d.log.WithField("slot", eth.SlotAsString(slot)).
				WithField("failure_count", failureCount).
				Error("No longer attempting to download historical block - too many failures")
		}

		d.historicalSlotFailures[slot] = failureCount
	}

	// Cleanup any banned slots that we don't care about anymore to prevent leaking memory.
	for slot := range d.historicalSlotFailures {
		if _, exists := slotsInScope[slot]; !exists {
			delete(d.historicalSlotFailures, slot)
		}
	}

	return nil
}

func (d *Default) downloadBlock(ctx context.Context, slot phase0.Slot, upstream *Node) (*spec.VersionedSignedBeaconBlock, error) {
	// If we don't know genesis time yet, don't bother fetching blocks as
	// we won't be able to calculate an expiry.
	if d.genesis == nil {
		return nil, errors.New("genesis time not known")
	}

	// Same thing with the chain spec.
	if d.spec == nil {
		return nil, errors.New("chain spec not known")
	}

	// Check if we already have the block.
	bl, err := d.blocks.GetBySlot(slot)
	if err == nil && bl != nil {
		return bl, nil
	}

	// Download the block from our upstream.
	block, err := upstream.Beacon.FetchBlock(ctx, eth.SlotAsString(slot))
	if err != nil {
		return nil, err
	}

	if block == nil {
		return nil, errors.New("invalid block")
	}

	stateRoot, err := block.StateRoot()
	if err != nil {
		return nil, err
	}

	root, err := block.Root()
	if err != nil {
		return nil, err
	}

	if err := d.storeBlock(ctx, block); err != nil {
		return nil, err
	}

	d.log.
		WithFields(logrus.Fields{
			"slot":       slot,
			"root":       eth.RootAsString(root),
			"state_root": eth.RootAsString(stateRoot),
		}).
		Infof("Downloaded and stored block for slot %d", slot)

	return block, nil
}

func (d *Default) fetchBundle(ctx context.Context, root phase0.Root, upstream *Node) (*spec.VersionedSignedBeaconBlock, error) {
	d.log.Infof("Fetching bundle from node %s with root %#x", upstream.Config.Name, root)

	block, err := d.blocks.GetByRoot(root)
	if err != nil || block == nil {
		// Download the block.
		block, err = upstream.Beacon.FetchBlock(ctx, fmt.Sprintf("%#x", root))
		if err != nil {
			return nil, err
		}

		if block == nil {
			return nil, errors.New("block is nil")
		}
	}

	stateRoot, err := block.StateRoot()
	if err != nil {
		return nil, err
	}

	blockRoot, err := block.Root()
	if err != nil {
		return nil, err
	}

	if blockRoot != root {
		return nil, errors.New("block root does not match")
	}

	slot, err := block.Slot()
	if err != nil {
		return nil, err
	}

	d.log.
		WithField("slot", slot).
		WithField("root", fmt.Sprintf("%#x", blockRoot)).
		WithField("state_root", fmt.Sprintf("%#x", stateRoot)).
		Info("Fetched beacon block")

	err = d.storeBlock(ctx, block)
	if err != nil {
		return nil, err
	}

	// If the state already exists, don't bother downloading it again.
	existingState, err := d.states.GetByStateRoot(stateRoot)
	if err == nil && existingState != nil {
		d.log.Infof("Successfully fetched bundle from %s", upstream.Config.Name)

		return block, nil
	}

	beaconState, err := upstream.Beacon.FetchRawBeaconState(ctx, fmt.Sprintf("%#x", stateRoot), "application/octet-stream")
	if err != nil {
		return nil, err
	}

	if beaconState == nil {
		return nil, errors.New("beacon state is nil")
	}

	expiresAt := time.Now().Add(3 * time.Hour)
	if slot == phase0.Slot(0) {
		expiresAt = time.Now().Add(999999 * time.Hour)
	}

	if err := d.states.Add(stateRoot, &beaconState, expiresAt); err != nil {
		return nil, err
	}

	d.log.Infof("Successfully fetched bundle from %s", upstream.Config.Name)

	return block, nil
}