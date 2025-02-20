// Copyright 2022 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package keyspan

import (
	"bytes"
	"fmt"
	"sort"

	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/invariants"
)

// TODO(jackson): Document memory safety requirements. Once we're reading from
// sstables, key spans may only be considered stable between the
// Table{Start,End} bounds.

// TODO(jackson): Consider implementing an optimization to seek lower levels
// past higher levels' RANGEKEYDELs. This would be analaogous to the
// optimization pebble.mergingIter performs for RANGEDELs during point key
// seeks. It may not be worth it, because range keys are rare and cascading
// seeks would require introducing key comparisons to switchTo{Min,Max}Heap
// where there currently are none.

// Transform defines a transform function to be applied to a Span. A Transform
// takes a Span as input and writes the transformed Span to the provided output
// *Span pointer. The output Span's Keys slice may be reused by Transform to
// reduce allocations.
type Transform func(cmp base.Compare, in Span, out *Span) error

func noopTransform(_ base.Compare, s Span, dst *Span) error {
	dst.Start, dst.End = s.Start, s.End
	dst.Keys = append(dst.Keys[:0], s.Keys...)
	return nil
}

// visibleTransform filters keys that are invisible at the provided snapshot
// sequence number.
func visibleTransform(snapshot uint64) Transform {
	return func(_ base.Compare, s Span, dst *Span) error {
		s = s.Visible(snapshot)
		dst.Start, dst.End = s.Start, s.End
		dst.Keys = append(dst.Keys[:0], s.Keys...)
		return nil
	}
}

// MergingIter merges spans across levels of the LSM, exposing an iterator over
// spans that yields sets of spans fragmented at unique user key boundaries.
//
// A MergingIter is initialized with an arbitrary number of child iterators over
// fragmented spans. Each child iterator exposes fragmented key spans, such that
// overlapping keys are surfaced in a single Span. Key spans from one child
// iterator may overlap key spans from another child iterator arbitrarily.
//
// Algorithm
//
// The merging iterator wraps child iterators, merging and fragmenting spans
// across levels. The high-level algorithm is:
//
//     1. Initialize the heap with bound keys from child iterators' spans.
//     2. Find the next [or previous] two unique user keys' from bounds.
//     3. Consider the span formed between the two unique user keys a candidate
//        span.
//     4. Determine if any of the child iterators' spans overlap the candidate
//        span.
//         4a. If any of the child iterator's current bounds are end keys
//             (during forward iteration) or start keys (during reverse
//             iteration), then all the spans with that bound overlap the
//             candidate span.
//         4b. Apply the configured transform, which may remove spans.
//         4b. If after transformation no spans overlap, forget the smallest
//             (forward iteration) or largest (reverse iteration) unique user
//             key and advance the iterators to the next unique user key.
//             Start again from 3.
//
// Detailed algorithm
//
// Each level (i0, i1, ...) has a user-provided input FragmentIterator. The
// merging iterator wraps each of these input iterators with a
// fragmentBoundIterator that returns each individual boundary of the underlying
// fragments separately. If the underlying FragmentIterator has fragments
// [a,b){#2,#1} [b,c){#1} the fragmentBoundIterator returns:
//
//     (a, start), (b, end), (b, start), (c, end)
//
// Note that (a, start) and (b, end) are returned ONCE each, despite two keys
// sharing those bounds. Also note that (b, end) and (b, start) are two distinct
// iterator positions.
//
// The merging iterator maintains a heap (min during forward iteration, max
// during reverse iteration) containing the boundKeys returned by a
// fragmentBoundIterator. Each boundKey is a 3-tuple holding the bound user key,
// whether the bound is a start or end key and the set of keys from that level
// that have that bound. The heap orders based on the boundKey's user key only.
//
// The merging iterator is responsible for merging spans across levels to
// determine which span is next, but it's also responsible for fragmenting
// overlapping spans. Consider the example:
//
//            i0:     b---d e-----h
//            i1:   a---c         h-----k
//            i2:   a------------------------------p
//
//     fragments:   a-b-c-d-e-----h-----k----------p
//
// None of the individual child iterators contain a span with the exact bounds
// [c,d), but the merging iterator must produce a span [c,d). To accomplish
// this, the merging iterator visits every span between unique boundary user
// keys. In the above example, this is:
//
//     [a,b), [b,c), [c,d), [d,e), [e, h), [h, k), [k, p)
//
// The merging iterator first initializes the heap to prepare for iteration.
// The description below discusses the mechanics of forward iteration after a
// call to First, but the mechanics are similar for reverse iteration and
// other positioning methods.
//
// During a call to First, the heap is initialized by seeking every
// fragmentBoundIterator to the first bound of the first fragment. In the above
// example, this seeks the child iterators to:
//
//     i0: (b, boundKindFragmentStart, [ [b,d) ])
//     i1: (a, boundKindFragmentStart, [ [a,c) ])
//     i2: (a, boundKindFragmentStart, [ [a,p) ])
//
// After fixing up the heap, the root of the heap is a boundKey with the
// smallest user key ('a' in the example). Once the heap is setup for iteration
// in the appropriate direction and location, the merging iterator uses
// find{Next,Prev}FragmentSet to find the next/previous span bounds.
//
// During forward iteration, the root of the heap's user key is the start key
// key of next merged span. findNextFragmentSet sets m.start to this user
// key. The heap may contain other boundKeys with the same user key if another
// level has a fragment starting or ending at the same key, so the
// findNextFragmentSet method pulls from the heap until it finds the first key
// greater than m.start. This key is used as the end key.
//
// In the above example, this results in m.start = 'a', m.end = 'b' and child
// iterators in the following positions:
//
//     i0: (b, boundKindFragmentStart, [ [b,d) ])
//     i1: (c, boundKindFragmentEnd,   [ [a,c) ])
//     i2: (p, boundKindFragmentEnd,   [ [a,p) ])
//
// With the user key bounds of the next merged span established,
// findNextFragmentSet must determine which, if any, fragments overlap the span.
// During forward iteration any child iterator that is now positioned at an end
// boundary has an overlapping span. (Justification: The child iterator's end
// boundary is ≥ m.end. The corresponding start boundary must be ≤ m.start since
// there were no other user keys between m.start and m.end. So the fragments
// associated with the iterator's current end boundary have start and end bounds
// such that start ≤ m.start < m.end ≤ end).
//
// findNextFragmentSet iterates over the levels, collecting keys from any child
// iterators positioned at end boundaries. In the above example, i1 and i2 are
// positioned at end boundaries, so findNextFragmentSet collects the keys of
// [a,c) and [a,p). These spans contain the merging iterator's [m.start, m.end)
// span, but they may also extend beyond the m.start and m.end. The merging
// iterator returns the keys with the merging iter's m.start and m.end bounds,
// preserving the underlying keys' sequence numbers, key kinds and values.
//
// A MergingIter is configured with a Transform that's applied to the span
// before surfacing it to the iterator user. A Transform may remove spans
// arbitrarily, but it may not modify the values themselves.
//
// It may be the case that findNextFragmentSet finds no levels positioned at end
// boundaries, or that there are no spans remaining after applying a transform,
// in which case the span [m.start, m.end) overlaps with nothing. In this case
// findNextFragmentSet loops, repeating the above process again until it finds a
// span that does contain keys.
type MergingIter struct {
	levels []mergingIterLevel
	heap   mergingIterHeap

	// start and end hold the bounds for the span currently under the
	// iterator position.
	//
	// Invariant: None of the levels' iterators contain spans with a bound
	// between start and end. For all bounds b, b ≤ start || b ≥ end.
	start, end []byte
	// keys holds all of the keys across all levels that overlap the key span
	// [start, end), sorted by sequence number and kind descending. This slice
	// is reconstituted in synthesizeKeys from each mergingIterLevel's keys
	// every time the [start, end) bounds change.
	//
	// Each element points into a child iterator's memory, so the keys may not
	// be directly modified.
	keys keysBySeqNumKind
	// transform defines a function to be applied to a span before it's yielded
	// to the user. A transform may filter individual keys contained within the
	// span.
	transform Transform
	// span holds the iterator's current span. This span is used as the
	// destination for transforms. Every tranformed span overwrites the
	// previous.
	span Span

	err error
	dir int8
}

// MergingIter implements the FragmentIterator interface.
var _ FragmentIterator = (*MergingIter)(nil)

type mergingIterLevel struct {
	iter fragmentBoundIterator

	// heapKey holds the current key at this level for use within the heap.
	heapKey boundKey
}

// Init initializes the merging iterator with the provided fragment iterators.
func (m *MergingIter) Init(cmp base.Compare, transform Transform, iters ...FragmentIterator) {
	levels, items := m.levels, m.heap.items

	*m = MergingIter{
		heap:      mergingIterHeap{cmp: cmp},
		transform: transform,
	}
	// Invariant: cap(levels) == cap(items)
	if cap(levels) < len(iters) {
		m.levels = make([]mergingIterLevel, len(iters))
		m.heap.items = make([]mergingIterItem, 0, len(iters))
	} else {
		m.levels = levels[:len(iters)]
		m.heap.items = items[:0]
	}
	for i := range m.levels {
		m.levels[i] = mergingIterLevel{}
		m.levels[i].iter.init(iters[i])
	}
}

// SeekGE moves the iterator to the first span with a start key greater than or
// equal to key.
func (m *MergingIter) SeekGE(key []byte) Span {
	m.invalidate() // clear state about current position
	for i := range m.levels {
		l := &m.levels[i]
		l.heapKey = l.iter.seekGE(m.cmp, key)
	}
	m.initMinHeap()
	return m.findNextFragmentSet()
}

// SeekLT moves the iterator to the last span with a start key less than key.
func (m *MergingIter) SeekLT(key []byte) Span {
	// TODO(jackson): Evaluate whether there's an implementation of SeekLT
	// independent of SeekGE that is more efficient. It's tricky, because the
	// span we should return might straddle `key` itself.
	//
	// Consider the scenario:
	//       a----------l      #2
	//         b-----------m   #1
	//
	// The merged, fully-fragmented spans that MergingIter exposes to the caller
	// have bounds:
	//        a-b              #2
	//          b--------l     #2
	//          b--------l     #1
	//                   l-m   #1
	//
	// A call SeekLT(c) must return the largest of the above spans with a
	// Start user key < key: [b,l)#1. This requires examining bounds both < 'c'
	// (the 'b' of [b,m)#1's start key) and bounds ≥ 'c' (the 'l' of ([a,l)#2's
	// end key).
	if s := m.SeekGE(key); !s.Valid() && m.err != nil {
		return Span{}
	}
	// Prev to the previous span.
	return m.Prev()
}

// First seeks the iterator to the first span.
func (m *MergingIter) First() Span {
	m.invalidate() // clear state about current position
	for i := range m.levels {
		l := &m.levels[i]
		l.heapKey = l.iter.first()
	}
	m.initMinHeap()
	return m.findNextFragmentSet()
}

// Last seeks the iterator to the last span.
func (m *MergingIter) Last() Span {
	m.invalidate() // clear state about current position
	for i := range m.levels {
		l := &m.levels[i]
		l.heapKey = l.iter.last()
	}
	m.initMaxHeap()
	return m.findPrevFragmentSet()
}

// Next advances the iterator to the next span.
func (m *MergingIter) Next() Span {
	if m.err != nil {
		return Span{}
	}
	if m.dir == +1 && (m.end == nil || m.start == nil) {
		return Span{}
	}
	if m.dir != +1 {
		m.switchToMinHeap()
	}
	return m.findNextFragmentSet()
}

// Prev advances the iterator to the previous span.
func (m *MergingIter) Prev() Span {
	if m.err != nil {
		return Span{}
	}
	if m.dir == -1 && (m.end == nil || m.start == nil) {
		return Span{}
	}
	if m.dir != -1 {
		m.switchToMaxHeap()
	}
	return m.findPrevFragmentSet()
}

// Error returns any accumulated error.
func (m *MergingIter) Error() error {
	if m.heap.len() == 0 || m.err != nil {
		return m.err
	}
	return m.levels[m.heap.items[0].index].iter.iter.Error()
}

// SetBounds sets the lower and upper bounds for the iterator. Note that the
// result of Next and Prev will be undefined until the iterator has been
// repositioned with SeekGE, SeekLT, First, or Last.
func (m *MergingIter) SetBounds(lower, upper []byte) {
	for i := range m.levels {
		m.levels[i].iter.iter.SetBounds(lower, upper)
	}
}

// Close closes the iterator, releasing all acquired resources.
func (m *MergingIter) Close() error {
	for i := range m.levels {
		if err := m.levels[i].iter.iter.Close(); err != nil && m.err == nil {
			m.err = err
		}
	}
	m.levels = nil
	m.heap.items = m.heap.items[:0]
	return m.err
}

// String implements fmt.Stringer.
func (m *MergingIter) String() string {
	return "merging-keyspan"
}

func (m *MergingIter) initMinHeap() {
	m.dir = +1
	m.heap.reverse = false
	m.initHeap()
}

func (m *MergingIter) initMaxHeap() {
	m.dir = -1
	m.heap.reverse = true
	m.initHeap()
}

func (m *MergingIter) initHeap() {
	m.heap.items = m.heap.items[:0]
	for i := range m.levels {
		if l := &m.levels[i]; l.heapKey.kind != boundKindInvalid {
			m.heap.items = append(m.heap.items, mergingIterItem{
				index:    i,
				boundKey: &l.heapKey,
			})
		} else {
			m.err = firstError(m.err, l.iter.iter.Error())
			if m.err != nil {
				return
			}
		}
	}
	m.heap.init()
}

func (m *MergingIter) switchToMinHeap() {
	// switchToMinHeap reorients the heap for forward iteration, without moving
	// the current MergingIter position.

	// The iterator is currently positioned at the span [m.start, m.end),
	// oriented in the reverse direction, so each level's iterator is positioned
	// to the largest key ≤ m.start. To reorient in the forward direction, we
	// must advance each level's iterator to the smallest key ≥ m.end. Consider
	// this three-level example.
	//
	//         i0:     b---d e-----h
	//         i1:   a---c         h-----k
	//         i2:   a------------------------------p
	//
	//     merged:   a-b-c-d-e-----h-----k----------p
	//
	// If currently positioned at the merged span [c,d), then the level
	// iterators' heap keys are:
	//
	//    i0: (b, [b, d))   i1: (c, [a,c))   i2: (a, [a,p))
	//
	// Reversing the heap should not move the merging iterator and should not
	// change the current [m.start, m.end) bounds. It should only prepare for
	// forward iteration by updating the child iterators' heap keys to:
	//
	//    i0: (d, [b, d))   i1: (h, [h,k))   i2: (p, [a,p))
	//
	// In every level the first key ≥ m.end is the next in the iterator.
	// Justification: Suppose not and a level iterator's next key was some key k
	// such that k < m.end. The max-heap invariant dictates that the current
	// iterator position is the largest entry with a user key ≥ m.start. This
	// means k > m.start. We started with the assumption that k < m.end, so
	// m.start < k < m.end. But then k is between our current span bounds,
	// and reverse iteration would have constructed the current interval to be
	// [k, m.end) not [m.start, m.end).

	if invariants.Enabled {
		for i := range m.levels {
			l := &m.levels[i]
			if l.heapKey.kind != boundKindInvalid && m.cmp(l.heapKey.key, m.start) > 0 {
				panic("pebble: invariant violation: max-heap key > m.start")
			}
		}
	}

	for i := range m.levels {
		l := &m.levels[i]
		l.heapKey = l.iter.next()
	}
	m.initMinHeap()
}

func (m *MergingIter) switchToMaxHeap() {
	// switchToMaxHeap reorients the heap for reverse iteration, without moving
	// the current MergingIter position.

	// The iterator is currently positioned at the span [m.start, m.end),
	// oriented in the forward direction. Each level's iterator is positioned at
	// the smallest bound ≥ m.end. To reorient in the reverse direction, we must
	// move each level's iterator to the largest key ≤ m.start. Consider this
	// three-level example.
	//
	//         i0:     b---d e-----h
	//         i1:   a---c         h-----k
	//         i2:   a------------------------------p
	//
	//     merged:   a-b-c-d-e-----h-----k----------p
	//
	// If currently positioned at the merged span [c,d), then the level
	// iterators' heap keys are:
	//
	//    i0: (d, [b, d))   i1: (h, [h,k))   i2: (p, [a,p))
	//
	// Reversing the heap should not move the merging iterator and should not
	// change the current [m.start, m.end) bounds. It should only prepare for
	// reverse iteration by updating the child iterators' heap keys to:
	//
	//    i0: (b, [b, d))   i1: (c, [a,c))   i2: (a, [a,p))
	//
	// In every level the largest key ≤ m.start is the prev in the iterator.
	// Justification: Suppose not and a level iterator's prev key was some key k
	// such that k > m.start. The min-heap invariant dictates that the current
	// iterator position is the smallest entry with a user key ≥ m.end. This
	// means k < m.end, otherwise the iterator would be positioned at k. We
	// started with the assumption that k > m.start, so m.start < k < m.end. But
	// then k is between our current span bounds, and reverse iteration
	// would have constructed the current interval to be [m.start, k) not
	// [m.start, m.end).

	if invariants.Enabled {
		for i := range m.levels {
			l := &m.levels[i]
			if l.heapKey.kind != boundKindInvalid && m.cmp(l.heapKey.key, m.end) < 0 {
				panic("pebble: invariant violation: min-heap key < m.end")
			}
		}
	}

	for i := range m.levels {
		l := &m.levels[i]
		l.heapKey = l.iter.prev()
	}
	m.initMaxHeap()
}

func (m *MergingIter) cmp(a, b []byte) int {
	return m.heap.cmp(a, b)
}

func (m *MergingIter) findNextFragmentSet() Span {
	// Each iteration of this loop considers a new merged span between unique
	// user keys. An iteration may find that there exists no overlap for a given
	// span, (eg, if the spans [a,b), [d, e) exist within level iterators, the
	// below loop will still consider [b,d) before continuing to [d, e)). It
	// returns when it finds a span that is covered by at least one key.

	for m.heap.len() > 0 && m.err == nil {
		// Initialize the next span's start bound. SeekGE and First prepare the
		// heap without advancing. Next leaves the heap in a state such that the
		// root is the smallest bound key equal to the returned span's end key,
		// so the heap is already positioned at the next merged span's start key.

		// NB: m.heapRoot().key might be either an end boundary OR a start
		// boundary of a level's span. Both end and start boundaries may still
		// be a start key of a span in the set of fragmented spans returned by
		// MergingIter.
		// Consider the scenario:
		//       a----------l      #1
		//         b-----------m   #2
		//
		// The merged, fully-fragmented spans that MergingIter exposes to the caller
		// have bounds:
		//        a-b              #1
		//          b--------l     #1
		//          b--------l     #2
		//                   l-m   #2
		//
		// When advancing to l-m#2, we must set m.start to 'l', which originated
		// from [a,l)#1's end boundary.
		m.start = m.heapRoot().key

		// There may be many entries all with the same user key. Spans in other
		// levels may also start or end at this same user key. For eg:
		// L1:   [a, c) [c, d)
		// L2:          [c, e)
		// If we're positioned at L1's end(c) end boundary, we want to advance
		// to the first bound > c.
		m.nextEntry()
		for len(m.heap.items) > 0 && m.err == nil && m.cmp(m.heapRoot().key, m.start) == 0 {
			m.nextEntry()
		}
		if len(m.heap.items) == 0 || m.err != nil {
			break
		}

		// The current entry at the top of the heap is the first key > m.start.
		// It must become the end bound for the span we will return to the user.
		// In the above example, the root of the heap is L1's end(d).
		m.end = m.heapRoot().key

		// Each level within m.levels may have a span that overlaps the
		// fragmented key span [m.start, m.end). Update m.keys to point to them
		// and sort them by kind, sequence number. There may not be any keys
		// defined over [m.start, m.end) if we're between the end of one span
		// and the start of the next, OR if the configured transform filters any
		// keys out. We allow empty spans that were emitted by child iterators, but
		// we elide empty spans created by the mergingIter itself that don't overlap
		// with any child iterator returned spans (i.e. empty spans that bridge two
		// distinct child-iterator-defined spans).
		if found, s := m.synthesizeKeys(+1); found && s.Valid() {
			return s
		}
	}
	// Exhausted.
	m.clear()
	return Span{}
}

func (m *MergingIter) findPrevFragmentSet() Span {
	// Each iteration of this loop considers a new merged span between unique
	// user keys. An iteration may find that there exists no overlap for a given
	// span, (eg, if the spans [a,b), [d, e) exist within level iterators, the
	// below loop will still consider [b,d) before continuing to [a, b)). It
	// returns when it finds a span that is covered by at least one key.

	for m.heap.len() > 0 && m.err == nil {
		// Initialize the next span's end bound. SeekLT and Last prepare the
		// heap without advancing. Prev leaves the heap in a state such that the
		// root is the largest bound key equal to the returned span's start key,
		// so the heap is already positioned at the next merged span's end key.

		// NB: m.heapRoot().key might be either an end boundary OR a start
		// boundary of a level's span. Both end and start boundaries may still
		// be a start key of a span returned by MergingIter. Consider the
		// scenario:
		//       a----------l      #2
		//         b-----------m   #1
		//
		// The merged, fully-fragmented spans that MergingIter exposes to the caller
		// have bounds:
		//        a-b              #2
		//          b--------l     #2
		//          b--------l     #1
		//                   l-m   #1
		//
		// When Preving to a-b#2, we must set m.end to 'b', which originated
		// from [b,m)#1's start boundary.
		m.end = m.heapRoot().key

		// There may be many entries all with the same user key. Spans in other
		// levels may also start or end at this same user key. For eg:
		// L1:   [a, c) [c, d)
		// L2:          [c, e)
		// If we're positioned at L1's start(c) start boundary, we want to prev
		// to move to the first bound < c.
		m.prevEntry()
		for len(m.heap.items) > 0 && m.err == nil && m.cmp(m.heapRoot().key, m.end) == 0 {
			m.prevEntry()
		}
		if len(m.heap.items) == 0 || m.err != nil {
			break
		}

		// The current entry at the top of the heap is the first key < m.end.
		// It must become the start bound for the span we will return to the
		// user. In the above example, the root of the heap is L1's start(a).
		m.start = m.heapRoot().key

		// Each level within m.levels may have a set of keys that overlap the
		// fragmented key span [m.start, m.end). Update m.keys to point to them
		// and sort them by kind, sequence number. There may not be any keys
		// spanning [m.start, m.end) if we're between the end of one span and
		// the start of the next, OR if the configured transform filters any
		// keys out.  We allow empty spans that were emitted by child iterators, but
		// we elide empty spans created by the mergingIter itself that don't overlap
		// with any child iterator returned spans (i.e. empty spans that bridge two
		// distinct child-iterator-defined spans).
		if found, s := m.synthesizeKeys(-1); found && s.Valid() {
			return s
		}
	}
	// Exhausted.
	m.clear()
	return Span{}
}

func (m *MergingIter) heapRoot() *boundKey {
	return m.heap.items[0].boundKey
}

// synthesizeKeys is called by find{Next,Prev}FragmentSet to populate and
// sort the set of keys overlapping [m.start, m.end).
//
// During forward iteration, if the current heap item is a fragment end,
// then the fragment's start must be ≤ m.start and the fragment overlaps the
// current iterator position of [m.start, m.end).
//
// During reverse iteration, if the current heap item is a fragment start,
// then the fragment's end must be ≥ m.end and the fragment overlaps the
// current iteration position of [m.start, m.end).
//
// The boolean return value, `found`, is true if the returned span overlaps
// with a span returned by a child iterator.
func (m *MergingIter) synthesizeKeys(dir int8) (bool, Span) {
	if invariants.Enabled {
		if m.cmp(m.start, m.end) >= 0 {
			panic(fmt.Sprintf("pebble: invariant violation: span start ≥ end: %s >= %s", m.start, m.end))
		}
	}

	m.keys = m.keys[:0]
	found := false
	for i := range m.levels {
		if dir == +1 && m.levels[i].heapKey.kind == boundKindFragmentEnd ||
			dir == -1 && m.levels[i].heapKey.kind == boundKindFragmentStart {
			m.keys = append(m.keys, m.levels[i].heapKey.span.Keys...)
			found = true
		}
	}
	sort.Sort(&m.keys)

	// Apply the configured transform. See VisibleTransform.
	s := Span{
		Start: m.start,
		End:   m.end,
		Keys:  m.keys,
	}
	if err := m.transform(m.cmp, s, &m.span); err != nil {
		m.err = err
		return false, Span{}
	}
	return found, m.span
}

func (m *MergingIter) invalidate() {
	m.err = nil
}

func (m *MergingIter) clear() {
	for fi := range m.keys {
		m.keys[fi] = Key{}
	}
	m.keys = m.keys[:0]
}

// nextEntry steps to the next entry.
func (m *MergingIter) nextEntry() {
	l := &m.levels[m.heap.items[0].index]
	l.heapKey = l.iter.next()
	if !l.heapKey.valid() {
		// l.iter is exhausted.
		m.err = l.iter.iter.Error()
		if m.err == nil {
			m.heap.pop()
		}
		return
	}

	if m.heap.len() > 1 {
		m.heap.fix(0)
	}
}

// prevEntry steps to the previous entry.
func (m *MergingIter) prevEntry() {
	l := &m.levels[m.heap.items[0].index]
	l.heapKey = l.iter.prev()
	if !l.heapKey.valid() {
		// l.iter is exhausted.
		m.err = l.iter.iter.Error()
		if m.err == nil {
			m.heap.pop()
		}
		return
	}

	if m.heap.len() > 1 {
		m.heap.fix(0)
	}
}

// Clone clones the merging iterator and its underlying iterators.
func (m *MergingIter) Clone() FragmentIterator {
	// TODO(jackson): Remove when range-key state is included in readState.
	var iters []FragmentIterator
	for l := range m.levels {
		iters = append(iters, m.levels[l].iter.iter.Clone())
	}
	dup := &MergingIter{}
	dup.Init(m.cmp, m.transform, iters...)
	return dup
}

// DebugString returns a string representing the current internal state of the
// merging iterator and its heap for debugging purposes.
func (m *MergingIter) DebugString() string {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "Current bounds: [%q, %q)\n", m.start, m.end)
	for i := range m.levels {
		fmt.Fprintf(&buf, "%d: heap key %s\n", i, m.levels[i].heapKey)
	}
	return buf.String()
}

type mergingIterItem struct {
	// boundKey points to the corresponding mergingIterLevel's `iterKey`.
	*boundKey
	// index is the index of this level within the MergingIter's levels field.
	index int
}

// mergingIterHeap is copied from mergingIterHeap defined in the root pebble
// package for use with point keys.

type mergingIterHeap struct {
	cmp     base.Compare
	reverse bool
	items   []mergingIterItem
}

func (h *mergingIterHeap) len() int {
	return len(h.items)
}

func (h *mergingIterHeap) less(i, j int) bool {
	// This key comparison only uses the user key and not the boundKind. Bound
	// kind doesn't matter because when stepping over a user key,
	// findNextFragmentSet and findPrevFragmentSet skip past all heap items with
	// that user key, and makes no assumptions on ordering. All other heap
	// examinations only consider the user key.
	ik, jk := h.items[i].key, h.items[j].key
	c := h.cmp(ik, jk)
	if h.reverse {
		return c > 0
	}
	return c < 0
}

func (h *mergingIterHeap) swap(i, j int) {
	h.items[i], h.items[j] = h.items[j], h.items[i]
}

// init, fix, up and down are copied from the go stdlib.
func (h *mergingIterHeap) init() {
	// heapify
	n := h.len()
	for i := n/2 - 1; i >= 0; i-- {
		h.down(i, n)
	}
}

func (h *mergingIterHeap) fix(i int) {
	if !h.down(i, h.len()) {
		h.up(i)
	}
}

func (h *mergingIterHeap) pop() *mergingIterItem {
	n := h.len() - 1
	h.swap(0, n)
	h.down(0, n)
	item := &h.items[n]
	h.items = h.items[:n]
	return item
}

func (h *mergingIterHeap) up(j int) {
	for {
		i := (j - 1) / 2 // parent
		if i == j || !h.less(j, i) {
			break
		}
		h.swap(i, j)
		j = i
	}
}

func (h *mergingIterHeap) down(i0, n int) bool {
	i := i0
	for {
		j1 := 2*i + 1
		if j1 >= n || j1 < 0 { // j1 < 0 after int overflow
			break
		}
		j := j1 // left child
		if j2 := j1 + 1; j2 < n && h.less(j2, j1) {
			j = j2 // = 2*i + 2  // right child
		}
		if !h.less(j, i) {
			break
		}
		h.swap(i, j)
		i = j
	}
	return i > i0
}
