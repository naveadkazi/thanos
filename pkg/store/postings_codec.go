// Copyright (c) The Thanos Authors.
// Licensed under the Apache License 2.0.

package store

import (
	"bytes"
	"sync"

	"github.com/golang/snappy"
	"github.com/klauspost/compress/s2"
	"github.com/pkg/errors"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/encoding"
	"github.com/prometheus/prometheus/tsdb/index"
)

// This file implements encoding and decoding of postings using diff (or delta) + varint
// number encoding. On top of that, we apply Snappy compression.
//
// On its own, Snappy compressing raw postings doesn't really help, because there is no
// repetition in raw data. Using diff (delta) between postings entries makes values small,
// and Varint is very efficient at encoding small values (values < 128 are encoded as
// single byte, values < 16384 are encoded as two bytes). Diff + varint reduces postings size
// significantly (to about 20% of original), snappy then halves it to ~10% of the original.

const (
	codecHeaderSnappy = "dvs" // As in "diff+varint+snappy".
)

// isDiffVarintSnappyEncodedPostings returns true, if input looks like it has been encoded by diff+varint+snappy codec.
func isDiffVarintSnappyEncodedPostings(input []byte) bool {
	return bytes.HasPrefix(input, []byte(codecHeaderSnappy))
}

// diffVarintSnappyEncode encodes postings into diff+varint representation,
// and applies snappy compression on the result.
// Returned byte slice starts with codecHeaderSnappy header.
// Length argument is expected number of postings, used for preallocating buffer.
func diffVarintSnappyEncode(p index.Postings, length int) ([]byte, error) {
	buf, err := diffVarintEncodeNoHeader(p, length)
	if err != nil {
		return nil, err
	}

	// Make result buffer large enough to hold our header and compressed block.
	result := make([]byte, len(codecHeaderSnappy)+snappy.MaxEncodedLen(len(buf)))
	copy(result, codecHeaderSnappy)

	compressed := snappy.Encode(result[len(codecHeaderSnappy):], buf)

	// Slice result buffer based on compressed size.
	result = result[:len(codecHeaderSnappy)+len(compressed)]
	return result, nil
}

// diffVarintEncodeNoHeader encodes postings into diff+varint representation.
// It doesn't add any header to the output bytes.
// Length argument is expected number of postings, used for preallocating buffer.
func diffVarintEncodeNoHeader(p index.Postings, length int) ([]byte, error) {
	buf := encoding.Encbuf{}

	// This encoding uses around ~1 bytes per posting, but let's use
	// conservative 1.25 bytes per posting to avoid extra allocations.
	if length > 0 {
		buf.B = make([]byte, 0, 5*length/4)
	}

	prev := storage.SeriesRef(0)
	for p.Next() {
		v := p.At()
		if v < prev {
			return nil, errors.Errorf("postings entries must be in increasing order, current: %d, previous: %d", v, prev)
		}

		// This is the 'diff' part -- compute difference from previous value.
		buf.PutUvarint64(uint64(v - prev))
		prev = v
	}
	if p.Err() != nil {
		return nil, p.Err()
	}

	return buf.B, nil
}

var snappyDecodePool sync.Pool

type closeablePostings interface {
	index.Postings
	close()
}

// alias returns true if given slices have the same both backing array.
// See: https://groups.google.com/g/golang-nuts/c/C6ufGl73Uzk.
func alias(x, y []byte) bool {
	return cap(x) > 0 && cap(y) > 0 && &x[0:cap(x)][cap(x)-1] == &y[0:cap(y)][cap(y)-1]
}

func diffVarintSnappyDecode(input []byte) (closeablePostings, error) {
	if !isDiffVarintSnappyEncodedPostings(input) {
		return nil, errors.New("header not found")
	}

	toFree := make([][]byte, 0, 2)

	var dstBuf []byte
	decodeBuf := snappyDecodePool.Get()
	if decodeBuf != nil {
		dstBuf = *(decodeBuf.(*[]byte))
		toFree = append(toFree, dstBuf)
	}

	raw, err := s2.Decode(dstBuf, input[len(codecHeaderSnappy):])
	if err != nil {
		return nil, errors.Wrap(err, "snappy decode")
	}

	if !alias(raw, dstBuf) {
		toFree = append(toFree, raw)
	}

	return newDiffVarintPostings(raw, toFree), nil
}

func newDiffVarintPostings(input []byte, freeSlices [][]byte) *diffVarintPostings {
	return &diffVarintPostings{freeSlices: freeSlices, buf: &encoding.Decbuf{B: input}}
}

// diffVarintPostings is an implementation of index.Postings based on diff+varint encoded data.
type diffVarintPostings struct {
	buf        *encoding.Decbuf
	cur        storage.SeriesRef
	freeSlices [][]byte
}

func (it *diffVarintPostings) close() {
	for i := range it.freeSlices {
		snappyDecodePool.Put(&it.freeSlices[i])
	}
}

func (it *diffVarintPostings) At() storage.SeriesRef {
	return it.cur
}

func (it *diffVarintPostings) Next() bool {
	if it.buf.Err() != nil || it.buf.Len() == 0 {
		return false
	}

	val := it.buf.Uvarint64()
	if it.buf.Err() != nil {
		return false
	}

	it.cur = it.cur + storage.SeriesRef(val)
	return true
}

func (it *diffVarintPostings) Seek(x storage.SeriesRef) bool {
	if it.cur >= x {
		return true
	}

	// We cannot do any search due to how values are stored,
	// so we simply advance until we find the right value.
	for it.Next() {
		if it.At() >= x {
			return true
		}
	}

	return false
}

func (it *diffVarintPostings) Err() error {
	return it.buf.Err()
}
