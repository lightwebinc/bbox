// Package efraw turns a node's Extended Format answers back into plain raw
// transactions.
//
// A Teranode asset API answers GET /api/v1/tx/<txid> in Extended Format
// (BRC-30: the marker 00 00 00 00 00 ef after the version, and each
// input's previous satoshis and locking script), coinbases included.
// bcommon's nodeapi.Asset.TxRaw hands those bytes to guard.ParseTransaction,
// which reads the marker as a transaction of no inputs and refuses it, so a
// coin whose parent must be fetched (a mined coinbase, before its first
// spend) cannot be spent. Transport rewrites such an answer to the raw
// transaction it extends, which has the same txid.
package efraw

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"github.com/lightwebinc/bcommon/nodeapi"
)

// marker follows the version in an Extended Format transaction.
var marker = []byte{0, 0, 0, 0, 0, 0xef}

var errShort = errors.New("efraw: truncated")

// Raw returns the raw transaction an Extended Format one extends, and
// false with b unchanged when b is not Extended Format.
func Raw(b []byte) ([]byte, bool, error) {
	if len(b) < 10 || !bytes.Equal(b[4:10], marker) {
		return b, false, nil
	}
	r := &reader{b: b, i: 10}
	out := append(make([]byte, 0, len(b)), b[:4]...)
	n, err := r.varInt()
	if err != nil {
		return nil, true, err
	}
	out = appendVarInt(out, n)
	for k := uint64(0); k < n; k++ {
		s := r.i
		if err := r.skip(36); err != nil { // outpoint
			return nil, true, err
		}
		if err := r.script(); err != nil { // unlocking script
			return nil, true, err
		}
		if err := r.skip(4); err != nil { // sequence
			return nil, true, err
		}
		out = append(out, b[s:r.i]...)
		if err := r.skip(8); err != nil { // previous satoshis
			return nil, true, err
		}
		if err := r.script(); err != nil { // previous locking script
			return nil, true, err
		}
	}
	// Outputs and the lock time are as in a raw transaction.
	s := r.i
	m, err := r.varInt()
	if err != nil {
		return nil, true, err
	}
	for k := uint64(0); k < m; k++ {
		if err := r.skip(8); err != nil {
			return nil, true, err
		}
		if err := r.script(); err != nil {
			return nil, true, err
		}
	}
	if err := r.skip(4); err != nil {
		return nil, true, err
	}
	if r.i != len(b) {
		return nil, true, errors.New("efraw: trailing bytes")
	}
	return append(out, b[s:]...), true, nil
}

type reader struct {
	b []byte
	i int
}

func (r *reader) skip(n uint64) error {
	if n > uint64(len(r.b)-r.i) {
		return errShort
	}
	r.i += int(n)
	return nil
}

func (r *reader) varInt() (uint64, error) {
	if r.i >= len(r.b) {
		return 0, errShort
	}
	h := r.b[r.i]
	r.i++
	w := map[byte]int{0xfd: 2, 0xfe: 4, 0xff: 8}[h]
	if w == 0 {
		return uint64(h), nil
	}
	if w > len(r.b)-r.i {
		return 0, errShort
	}
	var v uint64
	for k := w - 1; k >= 0; k-- {
		v = v<<8 | uint64(r.b[r.i+k])
	}
	r.i += w
	return v, nil
}

func (r *reader) script() error {
	n, err := r.varInt()
	if err != nil {
		return err
	}
	return r.skip(n)
}

func appendVarInt(b []byte, v uint64) []byte {
	switch {
	case v < 0xfd:
		return append(b, byte(v))
	case v <= 0xffff:
		return append(b, 0xfd, byte(v), byte(v>>8))
	case v <= 0xffffffff:
		return append(b, 0xfe, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
	}
	b = append(b, 0xff)
	for k := 0; k < 8; k++ {
		b = append(b, byte(v>>(8*k)))
	}
	return b
}

// txPath is the asset API's raw transaction route.
var txPath = regexp.MustCompile(`/api/v1/tx/[0-9a-f]{64}$`)

// Transport rewrites a 200 answer to GET /api/v1/tx/<txid> from Extended
// Format to raw; every other request and answer passes through. A body
// that claims Extended Format and does not parse is passed through as it
// came, for the caller's own checks to refuse.
type Transport struct {
	// Base is the transport used; nil is http.DefaultTransport.
	Base http.RoundTripper
	// Max bounds the body read; zero is 64 MiB.
	Max int64
}

// RoundTrip implements http.RoundTripper.
func (t Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil || req.Method != http.MethodGet || resp.StatusCode != http.StatusOK || !txPath.MatchString(req.URL.Path) {
		return resp, err
	}
	limit := t.Max
	if limit == 0 {
		limit = 64 << 20
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	if raw, ef, err := Raw(body); ef && err == nil {
		body = raw
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	return resp, nil
}

// Asset is the node's asset API at base, its raw transactions read through
// Transport.
func Asset(base string) *nodeapi.Asset {
	return &nodeapi.Asset{Base: base, Client: &http.Client{Timeout: 30 * time.Second, Transport: Transport{}}}
}
