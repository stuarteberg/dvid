/*
	This file contains code that manages labelblk mutations at a low-level, using sharding
	to specific goroutines depending on the block coordinate being mutated.
	TODO: Move ingest/mutate/delete block ops in write.go into the same system.  Currently,
	we assume that merge/split ops in a version do not overlap the raw block label mutations.
*/

package labels64

import (
	"encoding/binary"
	"fmt"
	"io"
	"sort"

	"github.com/janelia-flyem/dvid/datastore"
	"github.com/janelia-flyem/dvid/datatype/common/labels"
	"github.com/janelia-flyem/dvid/dvid"
	"github.com/janelia-flyem/dvid/storage"
)

type sizeChange struct {
	oldSize, newSize uint64
}

// MergeLabels handles merging of any number of labels throughout the various label data
// structures.  It assumes that the merges aren't cascading, e.g., there is no attempt
// to merge label 3 into 4 and also 4 into 5.  The caller should have flattened the merges.
// TODO: Provide some indication that subset of labels are under evolution, returning
//   an "unavailable" status or 203 for non-authoritative response.  This might not be
//   feasible for clustered DVID front-ends due to coordination issues.
//
// EVENTS
//
// labels.MergeStartEvent occurs at very start of merge and transmits labels.DeltaMergeStart struct.
//
// labels.MergeBlockEvent occurs for every block of a merged label and transmits labels.DeltaMerge struct.
//
// labels.MergeEndEvent occurs at end of merge and transmits labels.DeltaMergeEnd struct.
//
func (d *Data) MergeLabels(v dvid.VersionID, op labels.MergeOp) error {
	dvid.Debugf("Merging %s into label %d ...\n", op.Merged, op.Target)

	// Asynchronously perform merge and handle any concurrent requests using the cache map until
	// labels64 is updated and consistent.  Mark these labels as dirty until done.
	d.StartUpdate()
	iv := dvid.InstanceVersion{Data: d.DataUUID(), Version: v}
	if err := labels.MergeStart(iv, op); err != nil {
		return err
	}

	// Signal that we are starting a merge.
	evt := datastore.SyncEvent{d.DataUUID(), labels.MergeStartEvent}
	msg := datastore.SyncMessage{labels.MergeStartEvent, v, labels.DeltaMergeStart{op}}
	if err := datastore.NotifySubscribers(evt, msg); err != nil {
		d.StopUpdate()
		return err
	}

	ctx := datastore.NewVersionedCtx(d, v)
	go func() {
		defer d.StopUpdate()

		// Get all the affected blocks in the merge.
		targetMeta, err := d.GetLabelMeta(ctx, labels.NewSet(op.Target), dvid.Bounds{})
		if err != nil {
			dvid.Errorf("can't get block indices of to merge target label %d\n", op.Target)
			return
		}
		mergedMeta, err := d.GetLabelMeta(ctx, op.Merged, dvid.Bounds{})
		if err != nil {
			dvid.Errorf("can't get block indices of to merge labels %s\n", op.Merged)
			return
		}

		delta := labels.DeltaMerge{
			MergeOp:      op,
			Blocks:       targetMeta.Blocks.Merge(mergedMeta.Blocks),
			TargetVoxels: targetMeta.Voxels,
			MergedVoxels: mergedMeta.Voxels,
		}
		if err := d.processMerge(v, delta); err != nil {
			dvid.Criticalf("unable to process merge: %v\n", err)
		}

		// Remove dirty labels and updating flag when done.
		labels.MergeStop(iv, op)
	}()

	return nil
}

// handle block and label index mods for a merge.
func (d *Data) processMerge(v dvid.VersionID, delta labels.DeltaMerge) error {
	timedLog := dvid.NewTimeLog()

	evt := datastore.SyncEvent{d.DataUUID(), labels.MergeBlockEvent}
	msg := datastore.SyncMessage{labels.MergeBlockEvent, v, delta}
	if err := datastore.NotifySubscribers(evt, msg); err != nil {
		return fmt.Errorf("can't notify subscribers for event %v: %v\n", evt, err)
	}

	mutID := d.NewMutationID()
	for _, izyx := range delta.Blocks {
		n := izyx.Hash(numBlockHandlers)
		d.MutAdd(mutID)
		op := mergeOp{mutID: mutID, MergeOp: delta.MergeOp, block: izyx}
		d.mutateCh[n] <- procMsg{op: op, v: v}
	}

	// When we've processed all the delta blocks, we can remove this merge op
	// from the merge cache since all labels will have completed.
	d.MutWait(mutID)
	d.MutDelete(mutID)
	timedLog.Debugf("labels64 block-level merge (%d blocks) of %s -> %d", len(delta.Blocks), delta.MergeOp.Merged, delta.MergeOp.Target)

	store, err := d.GetOrderedKeyValueDB()
	if err != nil {
		return fmt.Errorf("Data %q merge had error initializing store: %v\n", d.DataName(), err)
	}
	batcher, ok := store.(storage.KeyValueBatcher)
	if !ok {
		return fmt.Errorf("Data %q merge requires batch-enabled store, which %q is not\n", d.DataName(), store)
	}

	// Merge the new blocks into the target label block index.
	ctx := datastore.NewVersionedCtx(d, v)
	batch := batcher.NewBatch(ctx)

	tk := NewLabelIndexTKey(delta.Target)
	meta := Meta{
		Voxels: delta.TargetVoxels + delta.MergedVoxels,
		Blocks: delta.Blocks,
	}
	data, err := meta.MarshalBinary()
	if err != nil {
		return fmt.Errorf("Unable to serialize label meta for merge on label %d, data %q: %v\n", delta.Target, d.DataName(), err)
	} else {
		batch.Put(tk, data)
	}

	// Delete all the merged label block index kv pairs.
	for merged := range delta.Merged {
		tk = NewLabelIndexTKey(merged)
		batch.Delete(tk)
	}
	if err := batch.Commit(); err != nil {
		return fmt.Errorf("Error on commiting block indices for label %d, data %q: %v\n", delta.Target, d.DataName(), err)
	}

	deltaRep := labels.DeltaReplaceSize{
		Label:   delta.Target,
		OldSize: delta.TargetVoxels,
		NewSize: delta.TargetVoxels + delta.MergedVoxels,
	}
	evt = datastore.SyncEvent{d.DataUUID(), labels.ChangeSizeEvent}
	msg = datastore.SyncMessage{labels.ChangeSizeEvent, v, deltaRep}
	if err := datastore.NotifySubscribers(evt, msg); err != nil {
		dvid.Errorf("can't notify subscribers for event %v: %v\n", evt, err)
	}

	evt = datastore.SyncEvent{d.DataUUID(), labels.MergeEndEvent}
	msg = datastore.SyncMessage{labels.MergeEndEvent, v, labels.DeltaMergeEnd{delta.MergeOp}}
	if err := datastore.NotifySubscribers(evt, msg); err != nil {
		dvid.Errorf("can't notify subscribers for event %v: %v\n", evt, err)
	}

	dvid.Infof("Merged %s -> %d, data %q, resulting in %d blocks\n", delta.Merged, delta.Target, d.DataName(), len(delta.Blocks))

	d.publishDownresCommit(v, mutID)
	return nil
}

// SplitLabels splits a portion of a label's voxels into a given split label or, if the given split
// label is 0, a new label, which is returned.  The input is a binary sparse volume and should
// preferably be the smaller portion of a labeled region.  In other words, the caller should chose
// to submit for relabeling the smaller portion of any split.  It is assumed that the given split
// voxels are within the fromLabel set of voxels and will generate unspecified behavior if this is
// not the case.
//
// EVENTS
//
// labels.SplitStartEvent occurs at very start of split and transmits labels.DeltaSplitStart struct.
//
// labels.SplitBlockEvent occurs for every block of a split label and transmits labels.DeltaSplit struct.
//
// labels.SplitEndEvent occurs at end of split and transmits labels.DeltaSplitEnd struct.
//
func (d *Data) SplitLabels(v dvid.VersionID, fromLabel, splitLabel uint64, r io.ReadCloser) (toLabel uint64, err error) {
	// Create a new label id for this version that will persist to store
	if splitLabel != 0 {
		toLabel = splitLabel
		dvid.Debugf("Splitting subset of label %d into given label %d ...\n", fromLabel, splitLabel)
	} else {
		toLabel, err = d.NewLabel(v)
		if err != nil {
			return
		}
		dvid.Debugf("Splitting subset of label %d into new label %d ...\n", fromLabel, toLabel)
	}

	evt := datastore.SyncEvent{d.DataUUID(), labels.SplitStartEvent}
	splitOpStart := labels.DeltaSplitStart{fromLabel, toLabel}
	splitOpEnd := labels.DeltaSplitEnd{fromLabel, toLabel}

	// Make sure we can split given current merges in progress
	iv := dvid.InstanceVersion{Data: d.DataUUID(), Version: v}
	if err = labels.SplitStart(iv, splitOpStart); err != nil {
		return
	}
	defer labels.SplitStop(iv, splitOpEnd)

	// Signal that we are starting a split.
	msg := datastore.SyncMessage{labels.SplitStartEvent, v, splitOpStart}
	if err = datastore.NotifySubscribers(evt, msg); err != nil {
		return
	}

	// Read the sparse volume from reader.
	var split dvid.RLEs
	split, err = dvid.ReadRLEs(r)
	if err != nil {
		return
	}
	toLabelSize, _ := split.Stats()

	// Partition the split spans into blocks.
	var splitmap dvid.BlockRLEs
	blockSize, ok := d.BlockSize().(dvid.Point3d)
	if !ok {
		err = fmt.Errorf("Can't do split because block size for instance %s is not 3d: %v", d.DataName(), d.BlockSize())
		return
	}
	splitmap, err = split.Partition(blockSize)
	if err != nil {
		return
	}

	// Get a sorted list of blocks that cover split.
	splitblks := splitmap.SortedKeys()

	// Do the split
	deltaSplit := labels.DeltaSplit{
		OldLabel:     fromLabel,
		NewLabel:     toLabel,
		Split:        splitmap,
		SortedBlocks: splitblks,
		SplitVoxels:  toLabelSize,
	}
	if err = d.processSplit(v, deltaSplit); err != nil {
		return
	}

	return toLabel, nil
}

// SplitCoarseLabels splits a portion of a label's voxels into a given split label or, if the given split
// label is 0, a new label, which is returned.  The input is a binary sparse volume defined by block
// coordinates and should be the smaller portion of a labeled region-to-be-split.
//
// EVENTS
//
// labels.SplitStartEvent occurs at very start of split and transmits labels.DeltaSplitStart struct.
//
// labels.SplitBlockEvent occurs for every block of a split label and transmits labels.DeltaSplit struct.
//
// labels.SplitEndEvent occurs at end of split and transmits labels.DeltaSplitEnd struct.
//
func (d *Data) SplitCoarseLabels(v dvid.VersionID, fromLabel, splitLabel uint64, r io.ReadCloser) (toLabel uint64, err error) {
	// Create a new label id for this version that will persist to store
	if splitLabel != 0 {
		toLabel = splitLabel
		dvid.Debugf("Splitting coarse subset of label %d into given label %d ...\n", fromLabel, splitLabel)
	} else {
		toLabel, err = d.NewLabel(v)
		if err != nil {
			return
		}
		dvid.Debugf("Splitting coarse subset of label %d into new label %d ...\n", fromLabel, toLabel)
	}

	evt := datastore.SyncEvent{d.DataUUID(), labels.SplitStartEvent}
	splitOpStart := labels.DeltaSplitStart{fromLabel, toLabel}
	splitOpEnd := labels.DeltaSplitEnd{fromLabel, toLabel}

	// Make sure we can split given current merges in progress
	iv := dvid.InstanceVersion{Data: d.DataUUID(), Version: v}
	if err := labels.SplitStart(iv, splitOpStart); err != nil {
		return toLabel, err
	}
	defer labels.SplitStop(iv, splitOpEnd)

	// Signal that we are starting a split.
	msg := datastore.SyncMessage{labels.SplitStartEvent, v, splitOpStart}
	if err := datastore.NotifySubscribers(evt, msg); err != nil {
		return 0, err
	}

	// Read the sparse volume from reader.
	var splits dvid.RLEs
	splits, err = dvid.ReadRLEs(r)
	if err != nil {
		return
	}
	numBlocks, _ := splits.Stats()

	// Order the split blocks
	splitblks := make(dvid.IZYXSlice, numBlocks)
	n := 0
	for _, rle := range splits {
		p := rle.StartPt()
		run := rle.Length()
		for i := int32(0); i < run; i++ {
			izyx := dvid.IndexZYX{p[0] + i, p[1], p[2]}
			splitblks[n] = izyx.ToIZYXString()
			n++
		}
	}
	sort.Sort(splitblks)

	// Publish split event
	deltaSplit := labels.DeltaSplit{
		OldLabel:     fromLabel,
		NewLabel:     toLabel,
		Split:        nil,
		SortedBlocks: splitblks,
	}
	evt = datastore.SyncEvent{d.DataUUID(), labels.SplitLabelEvent}
	msg = datastore.SyncMessage{labels.SplitLabelEvent, v, deltaSplit}
	if err := datastore.NotifySubscribers(evt, msg); err != nil {
		return 0, err
	}

	dvid.Infof("Coarsely split %d blocks from label %d to label %d\n", numBlocks, fromLabel, toLabel)
	return toLabel, nil
}

func (d *Data) processSplit(v dvid.VersionID, delta labels.DeltaSplit) error {
	timedLog := dvid.NewTimeLog()
	d.StartUpdate()

	mutID := d.NewMutationID()
	var doneCh chan struct{}
	var deleteBlks dvid.IZYXSlice
	if delta.Split == nil {
		// Coarse Split so block indexing simple because all split blocks are removed from old label.
		deleteBlks = delta.SortedBlocks
		for _, izyx := range delta.SortedBlocks {
			n := izyx.Hash(numBlockHandlers)
			d.MutAdd(mutID)
			op := splitOp{
				mutID:    mutID,
				oldLabel: delta.OldLabel,
				newLabel: delta.NewLabel,
				block:    izyx,
			}
			d.mutateCh[n] <- procMsg{op: op, v: v}
		}
		// However we must publish label size changes at the block level since we don't know
		// how much change occurs until we traverse voxels.
	} else {
		// Fine Split could partially split within a block so both old and new labels have same valid block.
		doneCh = make(chan struct{})
		deleteBlkCh := make(chan dvid.IZYXString) // blocks that should be fully deleted from old label.
		go func() {
			for {
				select {
				case blk := <-deleteBlkCh:
					deleteBlks = append(deleteBlks, blk)
				case <-doneCh:
					return
				}
			}
		}()

		for izyx, blockRLEs := range delta.Split {
			n := izyx.Hash(numBlockHandlers)
			d.MutAdd(mutID)
			op := splitOp{
				mutID:       mutID,
				oldLabel:    delta.OldLabel,
				newLabel:    delta.NewLabel,
				rles:        blockRLEs,
				block:       izyx,
				deleteBlkCh: deleteBlkCh,
			}
			d.mutateCh[n] <- procMsg{op: op, v: v}
		}

		// Publish change in label sizes.
		deltaNewSize := labels.DeltaNewSize{
			Label: delta.NewLabel,
			Size:  delta.SplitVoxels,
		}
		evt := datastore.SyncEvent{d.DataUUID(), labels.ChangeSizeEvent}
		msg := datastore.SyncMessage{labels.ChangeSizeEvent, v, deltaNewSize}
		if err := datastore.NotifySubscribers(evt, msg); err != nil {
			dvid.Errorf("can't notify subscribers for event %v: %v\n", evt, err)
		}

		deltaModSize := labels.DeltaModSize{
			Label:      delta.OldLabel,
			SizeChange: int64(-delta.SplitVoxels),
		}
		evt = datastore.SyncEvent{d.DataUUID(), labels.ChangeSizeEvent}
		msg = datastore.SyncMessage{labels.ChangeSizeEvent, v, deltaModSize}
		if err := datastore.NotifySubscribers(evt, msg); err != nil {
			dvid.Errorf("can't notify subscribers for event %v: %v\n", evt, err)
		}
	}

	// Wait for all blocks to be split then modify label indices and mark end of split op.
	d.MutWait(mutID)
	d.MutDelete(mutID)
	if doneCh != nil {
		close(doneCh)
	}
	if err := d.splitIndices(v, delta, deleteBlks); err != nil {
		return err
	}
	timedLog.Debugf("labels64 sync complete for split (%d blocks) of %d -> %d", len(delta.Split), delta.OldLabel, delta.NewLabel)
	d.StopUpdate()

	// Publish split event
	evt := datastore.SyncEvent{d.DataUUID(), labels.SplitLabelEvent}
	msg := datastore.SyncMessage{labels.SplitLabelEvent, v, delta}
	if err := datastore.NotifySubscribers(evt, msg); err != nil {
		dvid.Errorf("can't notify subscribers for event %v: %v\n", evt, err)
	}

	// Publish split end
	evt = datastore.SyncEvent{d.DataUUID(), labels.SplitEndEvent}
	msg = datastore.SyncMessage{labels.SplitEndEvent, v, labels.DeltaSplitEnd{delta.OldLabel, delta.NewLabel}}
	if err := datastore.NotifySubscribers(evt, msg); err != nil {
		return fmt.Errorf("Unable to notify subscribers to data %q for evt %v\n", d.DataName(), evt)
	}
	return nil
}

// handles modification of the old and new label's block indices on split.
func (d *Data) splitIndices(v dvid.VersionID, delta labels.DeltaSplit, deleteBlks dvid.IZYXSlice) error {
	store, err := d.GetOrderedKeyValueDB()
	if err != nil {
		return fmt.Errorf("Data %q merge had error initializing store: %v\n", d.DataName(), err)
	}
	batcher, ok := store.(storage.KeyValueBatcher)
	if !ok {
		return fmt.Errorf("Data %q merge requires batch-enabled store, which %q is not\n", d.DataName(), store)
	}

	ctx := datastore.NewVersionedCtx(d, v)
	batch := batcher.NewBatch(ctx)
	meta, err := d.GetLabelMeta(ctx, labels.NewSet(delta.OldLabel), dvid.Bounds{})
	if err != nil {
		return fmt.Errorf("unable to get block indices for label %d -- aborting label idx denorm: %v\n", delta.OldLabel, err)
	}

	blocks, err := meta.Blocks.Split(deleteBlks)
	if err != nil {
		return fmt.Errorf("unable to split deleted blocks for label %d to %d for data %q: %v\n", delta.OldLabel, delta.NewLabel, d.DataName(), err)
	}
	// store meta for old label
	meta.Voxels = meta.Voxels - delta.SplitVoxels
	meta.Blocks = blocks

	data, err := meta.MarshalBinary()
	if err != nil {
		return fmt.Errorf("unable to serialize block indices for split of label %d, data %q: %v\n", delta.OldLabel, d.DataName(), err)
	}
	tk := NewLabelIndexTKey(delta.OldLabel)
	batch.Put(tk, data)

	// store meta for new label
	meta.Voxels = delta.SplitVoxels
	meta.Blocks = delta.SortedBlocks
	tk = NewLabelIndexTKey(delta.NewLabel)
	batch.Put(tk, data)

	if err := batch.Commit(); err != nil {
		return fmt.Errorf("Error on commiting block indices for split of label %d, data %q: %v\n", delta.OldLabel, d.DataName(), err)
	}
	return nil
}

// Serializes block operations so despite having concurrent merge/split label requests,
// we make sure any particular block isn't concurrently GET/POSTED.  If more throughput is required
// and the backend is distributed, we can spawn many mutateBlock() goroutines as long as we uniquely
// shard blocks across them, so the same block will always be directed to the same goroutine.
func (d *Data) mutateBlock(ch <-chan procMsg) {
	for {
		msg, more := <-ch
		if !more {
			return
		}

		ctx := datastore.NewVersionedCtx(d, msg.v)
		switch op := msg.op.(type) {
		case mergeOp:
			d.mergeBlock(ctx, op)

		case splitOp:
			d.splitBlock(ctx, op)

		// TODO
		// case ingestOp:
		// 	d.ingestBlock(ctx, op)

		// case mutateOp:
		// 	d.mutateBlock(ctx, op)

		// case deleteOp:
		// 	d.deleteBlock(ctx, op)

		default:
			dvid.Criticalf("Received unknown processing msg in mutateBlock: %v\n", msg)
		}
	}
}

// handles relabeling of blocks during a merge operation.
func (d *Data) mergeBlock(ctx *datastore.VersionedCtx, op mergeOp) {
	defer d.MutDone(op.mutID)

	store, err := d.GetKeyValueDB()
	if err != nil {
		dvid.Errorf("Data type labelblk had error initializing store: %v\n", err)
		return
	}

	tk := NewBlockTKeyByCoord(op.block)
	data, err := store.Get(ctx, tk)
	if err != nil {
		dvid.Errorf("Error on GET of labelblk with coord string %q\n", op.block)
		return
	}
	if data == nil {
		dvid.Errorf("nil label block where merge was done!\n")
		return
	}

	compressed, _, err := dvid.DeserializeData(data, true)
	if err != nil {
		dvid.Criticalf("unable to deserialize label block in '%s': %v\n", d.DataName(), err)
		return
	}
	blockData, err := labels.Decompress(compressed, d.BlockSize())
	if err != nil {
		dvid.Errorf("Unable to decompress google compression in %q: %v\n", d.DataName(), err)
		return
	}
	blockBytes := int(d.BlockSize().Prod() * 8)
	if len(blockData) != blockBytes {
		dvid.Criticalf("After labelblk deserialization got back %d bytes, expected %d bytes\n", len(blockData), blockBytes)
		return
	}

	// Iterate through this block of labels and relabel if label in merge.
	for i := 0; i < blockBytes; i += 8 {
		label := binary.LittleEndian.Uint64(blockData[i : i+8])
		if _, merged := op.Merged[label]; merged {
			binary.LittleEndian.PutUint64(blockData[i:i+8], op.Target)
		}
	}

	// Store this block.
	serialization, err := dvid.SerializeData(blockData, d.Compression(), d.Checksum())
	if err != nil {
		dvid.Criticalf("Unable to serialize block in %q: %v\n", d.DataName(), err)
		return
	}
	if err := store.Put(ctx, tk, serialization); err != nil {
		dvid.Errorf("Error in putting key %v: %v\n", tk, err)
	}

	// Notify any downstream downres instance.
	d.publishBlockChange(ctx.VersionID(), op.mutID, op.block, blockData)
}

// Goroutine that handles splits across a lot of blocks for one label.
func (d *Data) splitBlock(ctx *datastore.VersionedCtx, op splitOp) {
	defer d.MutDone(op.mutID)

	store, err := d.GetOrderedKeyValueDB()
	if err != nil {
		dvid.Errorf("Data type labelblk had error initializing store: %v\n", err)
		return
	}

	// Read the block.
	tk := NewBlockTKeyByCoord(op.block)
	data, err := store.Get(ctx, tk)
	if err != nil {
		dvid.Errorf("Error on GET of labelblk with coord string %v\n", []byte(op.block))
		return
	}
	if data == nil {
		dvid.Errorf("nil label block where split was done, coord %v\n", []byte(op.block))
		return
	}
	compressed, _, err := dvid.DeserializeData(data, true)
	if err != nil {
		dvid.Criticalf("unable to deserialize label block in '%s' key %v: %v\n", d.DataName(), []byte(op.block), err)
		return
	}
	blockData, err := labels.Decompress(compressed, d.BlockSize())
	if err != nil {
		dvid.Errorf("Unable to decompress google compression in %q: %v\n", d.DataName(), err)
		return
	}
	blockBytes := int(d.BlockSize().Prod() * 8)
	if len(blockData) != blockBytes {
		dvid.Criticalf("splitBlock: coord %v got back %d bytes, expected %d bytes\n", []byte(op.block), len(blockData), blockBytes)
		return
	}

	// Modify the block using either voxel-level changes or coarser block-level mods.
	// If we are doing coarse block split, we can only get change in # voxels after going through
	// block-level splits, unlike when provided the RLEs for split itself.  Also, we don't know
	// whether block indices can be maintained for fine split until we do split and see if any
	// old label remains.
	var toLabelSize uint64
	if op.rles != nil {
		var oldRemains bool
		toLabelSize, oldRemains, err = d.splitLabel(blockData, op)
		if err != nil {
			dvid.Errorf("can't store label %d RLEs into block %s: %v\n", op.newLabel, op.block, err)
			return
		}
		if !oldRemains {
			op.deleteBlkCh <- op.block
		}
	} else {
		// We are doing coarse split and will replace all
		toLabelSize, err = d.replaceLabel(blockData, op.oldLabel, op.newLabel)
		if err != nil {
			dvid.Errorf("can't replace label %d with %d in block %s: %v\n", op.oldLabel, op.newLabel, op.block, err)
			return
		}
		delta := labels.DeltaNewSize{
			Label: op.newLabel,
			Size:  toLabelSize,
		}
		evt := datastore.SyncEvent{d.DataUUID(), labels.ChangeSizeEvent}
		msg := datastore.SyncMessage{labels.ChangeSizeEvent, ctx.VersionID(), delta}
		if err := datastore.NotifySubscribers(evt, msg); err != nil {
			dvid.Criticalf("Unable to notify subscribers to data %q for evt %v\n", d.DataName(), evt)
		}

		delta2 := labels.DeltaModSize{
			Label:      op.oldLabel,
			SizeChange: int64(-toLabelSize),
		}
		evt = datastore.SyncEvent{d.DataUUID(), labels.ChangeSizeEvent}
		msg = datastore.SyncMessage{labels.ChangeSizeEvent, ctx.VersionID(), delta2}
		if err := datastore.NotifySubscribers(evt, msg); err != nil {
			dvid.Criticalf("Unable to notify subscribers to data %q for evt %v\n", d.DataName(), evt)
		}

	}

	// Write the modified block.
	serialization, err := dvid.SerializeData(blockData, d.Compression(), d.Checksum())
	if err != nil {
		dvid.Criticalf("Unable to serialize block %s in %q: %v\n", op.block, d.DataName(), err)
		return
	}
	if err := store.Put(ctx, tk, serialization); err != nil {
		dvid.Errorf("Error in putting key %v: %v\n", tk, err)
	}

	// Notify any downstream downres instance.
	d.publishBlockChange(ctx.VersionID(), op.mutID, op.block, blockData)
}

// Replace a label in a block.
func (d *Data) replaceLabel(data []byte, fromLabel, toLabel uint64) (splitVoxels uint64, err error) {
	n := len(data)
	if n%8 != 0 {
		err = fmt.Errorf("label data in block not aligned to uint64: %d bytes", n)
		return
	}
	for i := 0; i < n; i += 8 {
		label := binary.LittleEndian.Uint64(data[i : i+8])
		if label == fromLabel {
			splitVoxels++
			binary.LittleEndian.PutUint64(data[i:i+8], toLabel)
		}
	}
	return
}

// Split a label defined by RLEs into a block.  If there is old label still in the block after a split,
// we return true for oldRemains.
func (d *Data) splitLabel(data []byte, op splitOp) (splitVoxels uint64, oldRemains bool, err error) {
	var bcoord dvid.ChunkPoint3d
	bcoord, err = op.block.ToChunkPoint3d()
	if err != nil {
		return
	}

	blockSize := d.BlockSize()
	offset := bcoord.MinPoint(blockSize)

	nx := blockSize.Value(0) * 8
	nxy := nx * blockSize.Value(1)
	for _, rle := range op.rles {
		p := rle.StartPt().Sub(offset)
		i := p.Value(2)*nxy + p.Value(1)*nx + p.Value(0)*8
		for n := int32(0); n < rle.Length(); n++ {
			binary.LittleEndian.PutUint64(data[i:i+8], op.newLabel)
			splitVoxels++
			i += 8
		}
	}

	for i := 0; i < len(data); i += 8 {
		if binary.LittleEndian.Uint64(data[i:i+8]) == op.oldLabel {
			oldRemains = true
			return
		}
	}
	return
}