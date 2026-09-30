package boxrec

import (
	"encoding/binary"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

// DefaultMaxBEEF is the BEEF bound Admit applies when a host names none: the
// floor docs/spec.md section 12 sets for a host's own bound.
const DefaultMaxBEEF = 256 << 10

// beefCounts reads the BUMP and transaction counts a BEEF declares on the
// wire, before any parser merges proofs of one block or collapses a
// transaction listed twice: the beef rule counts what was sent. b has passed
// bcommon guard.CheckBEEF, so every length it declares is present; the walk
// still checks each one.
func beefCounts(b []byte) (bumps, txs uint64, ok bool) {
	c := &walk{b: b}
	if len(b) >= 4 && binary.LittleEndian.Uint32(b) == 0x01010101 {
		c.i = 4 + 32
	}
	c.i += 4 // the V1 or V2 version word
	if bumps, ok = c.varInt(); !ok {
		return 0, 0, false
	}
	for n := uint64(0); n < bumps; n++ {
		if _, ok = c.varInt(); !ok { // block height
			return 0, 0, false
		}
		levels, ok := c.byte()
		if !ok {
			return 0, 0, false
		}
		for l := 0; l < int(levels); l++ {
			leaves, ok := c.varInt()
			if !ok {
				return 0, 0, false
			}
			for k := uint64(0); k < leaves; k++ {
				if _, ok = c.varInt(); !ok { // offset
					return 0, 0, false
				}
				flags, ok := c.byte()
				if !ok {
					return 0, 0, false
				}
				if flags&1 == 0 { // not a duplicate: a hash follows
					c.i += 32
				}
			}
		}
	}
	txs, ok = c.varInt()
	return bumps, txs, ok && c.i <= len(b)
}

type walk struct {
	b []byte
	i int
}

func (w *walk) byte() (byte, bool) {
	if w.i >= len(w.b) {
		return 0, false
	}
	w.i++
	return w.b[w.i-1], true
}

func (w *walk) varInt() (uint64, bool) {
	h, ok := w.byte()
	if !ok {
		return 0, false
	}
	n := 0
	switch h {
	case 0xfd:
		n = 2
	case 0xfe:
		n = 4
	case 0xff:
		n = 8
	default:
		return uint64(h), true
	}
	if w.i+n > len(w.b) {
		return 0, false
	}
	var v uint64
	for k := n - 1; k >= 0; k-- {
		v = v<<8 | uint64(w.b[w.i+k])
	}
	w.i += n
	return v, true
}

// minimalPath reports whether mp proves txid with only the hashes that
// needs: at the lowest level the txid, flagged as one, and its sibling (a
// hash or a duplicate), and above it exactly the one sibling each level
// needs. A proof carrying anything else carries bytes a host would store
// and serve for nothing.
func minimalPath(mp *transaction.MerklePath, txid chainhash.Hash) bool {
	if mp == nil || len(mp.Path) == 0 || len(mp.Path[0]) != 2 {
		return false
	}
	var at *transaction.PathElement
	for _, e := range mp.Path[0] {
		if e.Hash != nil && *e.Hash == txid && e.Txid != nil && *e.Txid {
			at = e
		}
	}
	if at == nil {
		return false
	}
	offset := at.Offset
	for level, leaves := range mp.Path {
		want := offset>>uint(level) ^ 1
		n := 0
		for _, e := range leaves {
			if e == at {
				continue
			}
			if e.Offset != want || (e.Txid != nil && *e.Txid) {
				return false
			}
			n++
		}
		if n != 1 {
			return false
		}
	}
	return true
}
