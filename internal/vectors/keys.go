// Package vectors generates the golden vectors under testdata/vectors from
// the boxrec codec. Every input is fixed, so a run writes the same bytes
// every time; TestVectors regenerates them and compares byte for byte.
package vectors

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bbox/internal/boxrec"
)

// SenderKey and RecipientKey are the test keys: 32 bytes of 0x42 and of
// 0x43. Both are public and must never hold anything real.
var (
	SenderKey    = bytes.Repeat([]byte{0x42}, 32)
	RecipientKey = bytes.Repeat([]byte{0x43}, 32)
)

// Office is the vectors' office identifier: the readable part
// "example_office" and a fixed example suffix. A real office draws its
// suffix at random (boxrec.NewOffice); a vector fixes one so it is
// reproducible.
const Office = "example_office_qzxkvbmwtr"

// OtherOffice shares Office's readable part under another suffix: a
// different office, and a different topic.
const OtherOffice = "example_office_hdjvnwpxle"

// t0 is the vectors' base time, Unix seconds (2026-01-01T00:00:00Z).
const t0 = 1767225600

func h(b []byte) string { return hex.EncodeToString(b) }

func priv(k []byte) *ec.PrivateKey {
	p, _ := ec.PrivateKeyFromBytes(k)
	return p
}

// Sender and Recipient return the test identities' compressed keys.
func Sender() []byte    { return priv(SenderKey).PubKey().Compressed() }
func Recipient() []byte { return priv(RecipientKey).PubKey().Compressed() }

func protoWallet(k []byte) *wallet.ProtoWallet {
	w, err := wallet.NewProtoWallet(wallet.ProtoWalletArgs{Type: wallet.ProtoWalletArgsTypePrivateKey, PrivateKey: priv(k)})
	if err != nil {
		panic(err)
	}
	return w
}

// fixed32 is a fixed stand-in for 32 random bytes, named so a reader sees
// what it stands for.
func fixed32(label string) [32]byte {
	return sha256.Sum256([]byte("bbox/vector/" + label))
}

// commitment is a stand-in carrier txid (hash byte order) for envelope i.
// Carrier vectors come with the transaction codec; a receipt needs only the
// commitments.
func commitment(i int) [32]byte { return fixed32(fmt.Sprintf("commitment/%d", i)) }

// BRC78 encrypts plaintext from sender to recipient exactly as BRC-78 and
// go-sdk's message.Encrypt do, but with the key id and the IV given rather
// than drawn, so the vector is reproducible. The tests decrypt every result
// with go-sdk's message.Decrypt and through the recipient's wallet.
func BRC78(plaintext, sender, recipient []byte, keyID, iv [32]byte) ([]byte, error) {
	s := priv(sender)
	r := priv(recipient).PubKey()
	invoice := "2-message encryption-" + base64.StdEncoding.EncodeToString(keyID[:])
	childPriv, err := s.DeriveChild(r, invoice)
	if err != nil {
		return nil, err
	}
	childPub, err := r.DeriveChild(s, invoice)
	if err != nil {
		return nil, err
	}
	shared, err := childPriv.DeriveSharedSecret(childPub)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(shared.Compressed()[1:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, len(iv))
	if err != nil {
		return nil, err
	}
	out := append([]byte{}, boxrec.BRC78Version...)
	out = append(out, s.PubKey().Compressed()...)
	out = append(out, r.Compressed()...)
	out = append(out, keyID[:]...)
	out = append(out, iv[:]...)
	return gcm.Seal(out, iv[:], plaintext, nil), nil
}

func putJSON(files map[string][]byte, name string, v any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return err
	}
	files[name] = buf.Bytes()
	return nil
}

// Names returns the generated file names in order.
func Names(files map[string][]byte) []string {
	var n []string
	for k := range files {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
