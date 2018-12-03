// Copyright 2018 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package pebble

import (
	"fmt"
	"sort"

	"github.com/petermattis/pebble/db"
	"github.com/petermattis/pebble/internal/rangedel"
)

// compactionIter provides a forward-only iterator that encapsulates the logic
// for collapsing entries during compaction. It wraps an internal iterator and
// collapses entries that are no longer necessary because they are shadowed by
// newer entries. The simplest example of this is when the internal iterator
// contains two keys: a.PUT.2 and a.PUT.1. Instead of returning both entries,
// compactionIter collapses the second entry because it is no longer
// necessary. The high-level structure for compactionIter is to iterate over
// its internal iterator and output 1 entry for every user-key. There are four
// complications to this story.
//
// 1. Eliding Deletion Tombstones
//
// Consider the entries a.DEL.2 and a.PUT.1. These entries collapse to
// a.DEL.2. Do we have to output the entry a.DEL.2? Only if a.DEL.2 possibly
// shadows an entry at a lower level. If we're compacting to the base-level in
// the LSM tree then a.DEL.2 is definitely not shadowing an entry at a lower
// level and can be elided.
//
// We can do slightly better than only eliding deletion tombstones at the base
// level by observing that we can elide a deletion tombstone if there are no
// sstables that contain the entry's key. This check is performed by
// elideTombstone.
//
// 2. Merges
//
// The MERGE operation merges the value for an entry with the existing value
// for an entry. The logical value of an entry can be composed of a series of
// merge operations. When compactionIter sees a MERGE, it scans forward in its
// internal iterator collapsing MERGE operations for the same key until it
// encounters a SET or DELETE operation. For example, the keys a.MERGE.4,
// a.MERGE.3, a.MERGE.2 will be collapsed to a.MERGE.4 and the values will be
// merged using the specified db.Merger.
//
// An interesting case here occurs when MERGE is combined with SET. Consider
// the entries a.MERGE.3 and a.SET.2. The collapsed key will be a.SET.3. The
// reason that the kind is changed to SET is because the SET operation acts as
// a barrier preventing further merging. This can be seen better in the
// scenario a.MERGE.3, a.SET.2, a.MERGE.1. The entry a.MERGE.1 may be at lower
// (older) level and not involved in the compaction. If the compaction of
// a.MERGE.3 and a.SET.2 produced a.MERGE.3, a subsequent compaction with
// a.MERGE.1 would merge the values together incorrectly.
//
// 3. Snapshots
//
// Snapshots are lightweight point-in-time views of the DB state. At its core,
// a snapshot is a sequence number along with a guarantee from Pebble that it
// will maintain the view of the database at that sequence number. Part of this
// guarantee is relatively straightforward to achieve. When reading from the
// database Pebble will ignore sequence numbers that are larger than the
// snapshot sequence number. The primary complexity with snapshots occurs
// during compaction: the collapsing of entries that are shadowed by newer
// entries is at odds with the guarantee that Pebble will maintain the view of
// the database at the snapshot sequence number. Rather than collapsing entries
// up to the next user key, compactionIter can only collapse entries up to the
// next snapshot boundary. That is, every snapshot boundary potentially causes
// another entry for the same user-key to be emitted. Another way to view this
// is that snapshots define stripes and entries are collapsed within stripes,
// but not across stripes. Consider the following scenario:
//
//   a.PUT.9
//   a.DEL.8
//   a.PUT.7
//   a.DEL.6
//   a.PUT.5
//
// In the absence of snapshots these entries would be collapsed to
// a.PUT.9. What if there is a snapshot at sequence number 7? The entries can
// be divided into two stripes and collapsed within the stripes:
//
//   a.PUT.9        a.PUT.9
//   a.DEL.8  --->
//   a.PUT.7
//   --             --
//   a.DEL.6  --->  a.DEL.6
//   a.PUT.5
//
// All of the rules described earlier still apply, but they are confined to
// operate within a snapshot stripe. Snapshots only affect compaction when the
// snapshot sequence number lies within the range of sequence numbers being
// compacted. In the above example, a snapshot at sequence number 10 or at
// sequence number 5 would not have any effect.
//
// 4. Range Deletions
//
// Range deletions provide the ability to delete all of the keys (and values)
// in a contiguous range. Range deletions are stored indexed by their start
// key. The end key of the range is stored in the value. In order to support
// lookup of the range deletions which overlap with a particular key, the range
// deletion tombstones need to be fragmented whenever they overlap. This
// fragmentation is performed by rangedel.Fragmenter. The fragments are then
// subject to the rules for snapshots. For example, consider the two range
// tombstones [a,e)#1 and [c,g)#2:
//
//   2:     c-------g
//   1: a-------e
//
// These tombstones will be fragmented into:
//
//   2:     c---e---g
//   1: a---c---e
//
// Do we output the fragment [c,e)#1? Since it is covered by [c-e]#2 the answer
// depends on whether it is in a new snapshot stripe.
//
// In addition to the fragmentation of range tombstones, compaction also needs
// to take the range tombstones into consideration when outputting normal
// keys. Just as with point deletions, a range deletion covering an entry can
// cause the entry to be elided.
type compactionIter struct {
	cmp   db.Compare
	merge db.Merge
	iter  internalIterator
	err   error
	key   db.InternalKey
	value []byte
	// Temporary buffer used for storing the previous user key in order to
	// determine when iteration has advanced to a new user key and thus a new
	// snapshot stripe.
	keyBuf []byte
	// Temporary buffer used for aggregating merge operations.
	valueBuf []byte
	// Is the current entry valid?
	valid bool
	// Skip indicates whether the remaining entries in the current snapshot
	// stripe should be skipped or processed. Skipped is true at the start of a
	// stripe and set to false afterwards.
	skip bool
	// The index of the snapshot for the current key within the snapshots slice.
	curSnapshotIdx    int
	curSnapshotSeqNum uint64
	// The snapshot sequence numbers that need to be maintained. These sequence
	// numbers define the snapshot stripes (see the Snapshots description
	// above). The sequence numbers are in ascending order.
	snapshots []uint64
	// The range deletion tombstone fragmenter.
	rangeDelFrag rangedel.Fragmenter
	// The fragmented tombstones.
	tombstones []rangedel.Tombstone
	// Byte allocator for the tombstone keys.
	alloc          byteAllocator
	elideTombstone func(key []byte) bool
}

func newCompactionIter(
	cmp db.Compare,
	merge db.Merge,
	iter internalIterator,
	snapshots []uint64,
	elideTombstones func(key []byte) bool,
) *compactionIter {
	i := &compactionIter{
		cmp:            cmp,
		merge:          merge,
		iter:           iter,
		snapshots:      snapshots,
		elideTombstone: elideTombstones,
	}
	i.rangeDelFrag.Cmp = cmp
	i.rangeDelFrag.Emit = i.emitRangeDelChunk
	return i
}

func (i *compactionIter) First() {
	if i.err != nil {
		return
	}
	i.iter.First()
	if i.iter.Valid() {
		i.curSnapshotIdx, i.curSnapshotSeqNum = snapshotIndex(i.iter.Key().SeqNum(), i.snapshots)
	}
	i.Next()
}

func (i *compactionIter) Next() bool {
	if i.err != nil {
		return false
	}

	if i.skip {
		i.skip = false
		i.skipStripe()
	}

	i.valid = false
	for i.iter.Valid() {
		i.key = i.iter.Key()
		switch i.key.Kind() {
		case db.InternalKeyKindDelete:
			// If we're at the last snapshot stripe and the tombstone can be elided
			// skip to the next stripe (which will be the next user key).
			if i.curSnapshotIdx == 0 && i.elideTombstone(i.key.UserKey) {
				i.saveKey()
				i.skipStripe()
				continue
			}

			i.saveKey()
			i.value = i.iter.Value()
			i.valid = true
			i.skip = true
			return true

		case db.InternalKeyKindRangeDelete:
			i.key = i.cloneKey(i.key)
			i.rangeDelFrag.Add(i.key, i.iter.Value())
			i.nextInStripe()
			continue

		case db.InternalKeyKindSet:
			if i.rangeDelFrag.Deleted(i.key, i.curSnapshotSeqNum) {
				i.saveKey()
				i.skipStripe()
				continue
			}

			i.saveKey()
			i.value = i.iter.Value()
			i.valid = true
			i.skip = true
			return true

		case db.InternalKeyKindMerge:
			if i.rangeDelFrag.Deleted(i.key, i.curSnapshotSeqNum) {
				i.saveKey()
				i.skipStripe()
				continue
			}

			return i.mergeNext()

		case db.InternalKeyKindInvalid:
			// NB: Invalid keys occur when there is some error parsing the key. Pass
			// them through unmodified.
			i.saveKey()
			i.saveValue()
			i.iter.Next()
			i.valid = true
			return true

		default:
			i.err = fmt.Errorf("invalid internal key kind: %d", i.key.Kind())
			return false
		}
	}

	return false
}

// snapshotIndex returns the index of the first sequence number in snapshots
// which is greater than or equal to seq.
func snapshotIndex(seq uint64, snapshots []uint64) (int, uint64) {
	index := sort.Search(len(snapshots), func(i int) bool {
		return snapshots[i] > seq
	})
	if index >= len(snapshots) {
		return index, db.InternalKeySeqNumMax
	}
	return index, snapshots[index]
}

func (i *compactionIter) skipStripe() {
	for i.nextInStripe() {
	}
}

func (i *compactionIter) nextInStripe() bool {
	i.iter.Next()
	if !i.iter.Valid() {
		return false
	}
	key := i.iter.Key()
	if i.cmp(i.key.UserKey, key.UserKey) != 0 {
		i.curSnapshotIdx, i.curSnapshotSeqNum = snapshotIndex(key.SeqNum(), i.snapshots)
		return false
	}
	switch key.Kind() {
	case db.InternalKeyKindRangeDelete:
		// Range tombstones are always added to the fragmenter. They are processed
		// into stripes after fragmentation.
		i.rangeDelFrag.Add(i.cloneKey(key), i.iter.Value())
		return true
	case db.InternalKeyKindInvalid:
		i.curSnapshotIdx, i.curSnapshotSeqNum = snapshotIndex(key.SeqNum(), i.snapshots)
		return false
	}
	if len(i.snapshots) == 0 {
		return true
	}
	idx, seqNum := snapshotIndex(key.SeqNum(), i.snapshots)
	if i.curSnapshotIdx == idx {
		return true
	}
	i.curSnapshotIdx = idx
	i.curSnapshotSeqNum = seqNum
	return false
}

func (i *compactionIter) mergeNext() bool {
	// Save the current key and value.
	i.saveKey()
	i.saveValue()
	i.valid = true

	// Loop looking for older values in the current snapshot stripe and merging
	// them.
	for {
		if !i.nextInStripe() {
			i.skip = false
			return true
		}
		key := i.iter.Key()
		switch key.Kind() {
		case db.InternalKeyKindDelete:
			// We've hit a deletion tombstone. Return everything up to this point and
			// then skip entries until the next snapshot stripe.
			i.valueBuf = i.value[:0]
			i.skip = true
			return true

		case db.InternalKeyKindRangeDelete:
			// We've hit a range deletion tombstone. Return everything up to this
			// point and then skip entries until the next snapshot stripe.
			i.skip = true
			return true

		case db.InternalKeyKindSet:
			if i.rangeDelFrag.Deleted(key, i.curSnapshotSeqNum) {
				i.skip = true
				return true
			}

			// We've hit a Set value. Merge with the existing value and return. We
			// change the kind of the resulting key to a Set so that it shadows keys
			// in lower levels. That is, MERGE+MERGE+SET -> SET.
			i.value = i.merge(i.key.UserKey, i.value, i.iter.Value(), nil)
			i.valueBuf = i.value[:0]
			i.key.SetKind(db.InternalKeyKindSet)
			i.skip = true
			return true

		case db.InternalKeyKindMerge:
			if i.rangeDelFrag.Deleted(key, i.curSnapshotSeqNum) {
				i.skip = true
				return true
			}

			// We've hit another Merge value. Merge with the existing value and
			// continue looping.
			i.value = i.merge(i.key.UserKey, i.value, i.iter.Value(), nil)
			i.valueBuf = i.value[:0]

		default:
			i.err = fmt.Errorf("invalid internal key kind: %d", i.iter.Key().Kind())
			return false
		}
	}
}

func (i *compactionIter) saveKey() {
	i.keyBuf = append(i.keyBuf[:0], i.iter.Key().UserKey...)
	i.key.UserKey = i.keyBuf
}

func (i *compactionIter) saveValue() {
	i.valueBuf = append(i.valueBuf[:0], i.iter.Value()...)
	i.value = i.valueBuf
}

func (i *compactionIter) cloneKey(key db.InternalKey) db.InternalKey {
	i.alloc, key.UserKey = i.alloc.Copy(key.UserKey)
	return key
}

func (i *compactionIter) Key() db.InternalKey {
	return i.key
}

func (i *compactionIter) Value() []byte {
	return i.value
}

func (i *compactionIter) Valid() bool {
	return i.valid
}

func (i *compactionIter) Error() error {
	return i.err
}

func (i *compactionIter) Close() error {
	err := i.iter.Close()
	if i.err == nil {
		i.err = err
	}
	return i.err
}

func (i *compactionIter) Tombstones(key []byte) []rangedel.Tombstone {
	if key == nil {
		i.rangeDelFrag.Finish()
	} else {
		i.rangeDelFrag.FlushTo(key)
	}
	tombstones := i.tombstones
	i.tombstones = nil
	return tombstones
}

func (i *compactionIter) emitRangeDelChunk(fragmented []rangedel.Tombstone) {
	// Apply the snapshot stripe rules, keeping only the latest tombstone for
	// each snapshot stripe.
	currentIdx := -1
	for _, v := range fragmented {
		idx, _ := snapshotIndex(v.Start.SeqNum(), i.snapshots)
		if currentIdx == idx {
			continue
		}
		i.tombstones = append(i.tombstones, v)
		currentIdx = idx
		if currentIdx == 0 {
			// This is the last snapshot stripe.
			//
			// TODO(peter,rangedel): Check to see whether the range tombstone can be
			// elided. Need to add an elideRangeTombstone callback.
			break
		}
	}
}
