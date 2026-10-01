package efraw

import (
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/guard"
)

// A devnet coinbase as a Teranode asset API serves it (Extended Format).
const coinbaseEF = "010000000000000000ef010000000000000000000000000000000000000000000000000000000000000000ffffffff1c02ef572f746572616e6f64652d75732f8bb8deca6e402a65d443f296ffffffff0000000000000000000144910c00000000001976a9141cbc037010c8a074292ffb4f8f40da06c28e9b8f88ac00000000"
const coinbaseTxid = "d436d0478e84e762539f483031083edb56d1e2d252b93aa445dae98043dc4296"

func TestRawFromEF(t *testing.T) {
	ef, _ := hex.DecodeString(coinbaseEF)
	if _, err := guard.ParseTransaction(ef, guard.DefaultBound); err == nil {
		t.Fatal("the guard is expected to refuse Extended Format")
	}
	raw, isEF, err := Raw(ef)
	if err != nil || !isEF {
		t.Fatalf("Raw: %v %v", isEF, err)
	}
	tx, err := guard.ParseTransaction(raw, guard.DefaultBound)
	if err != nil {
		t.Fatal(err)
	}
	if tx.TxID().String() != coinbaseTxid {
		t.Fatalf("txid %s", tx.TxID())
	}
	// A transaction with inputs: go-sdk's EF writer, read back.
	sdk, _ := transaction.NewTransactionFromBytes(raw)
	sdk.Inputs[0].SetSourceTxOutput(&transaction.TransactionOutput{Satoshis: 7, LockingScript: sdk.Outputs[0].LockingScript})
	ef2, err := sdk.EF()
	if err != nil {
		t.Fatal(err)
	}
	raw2, isEF, err := Raw(ef2)
	if err != nil || !isEF || hex.EncodeToString(raw2) != hex.EncodeToString(raw) {
		t.Fatalf("round trip: %v %v", isEF, err)
	}
	// Raw bytes pass through; truncations are refused.
	if same, isEF, err := Raw(raw); isEF || err != nil || len(same) != len(raw) {
		t.Fatal("raw bytes must pass through")
	}
	for n := 10; n < len(ef); n++ {
		if _, _, err := Raw(ef[:n]); err == nil {
			t.Fatalf("a truncation at %d parsed", n)
		}
	}
}

func TestTransportFeedsTxRaw(t *testing.T) {
	ef, _ := hex.DecodeString(coinbaseEF)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(ef)
	}))
	defer srv.Close()
	a := Asset(srv.URL)
	raw, err := a.TxRaw(t.Context(), coinbaseTxid)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := guard.ParseTransaction(raw, guard.DefaultBound)
	if err != nil || tx.TxID().String() != coinbaseTxid {
		t.Fatalf("%v %v", tx, err)
	}
	// Other routes are untouched.
	resp, err := (&http.Client{Transport: Transport{}}).Get(srv.URL + "/api/v1/tx/" + coinbaseTxid + "/json")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if hex.EncodeToString(b) != coinbaseEF {
		t.Fatal("a route other than the raw transaction was rewritten")
	}
}
