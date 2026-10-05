package send

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/nodeapi"

	"github.com/lightwebinc/bbox/internal/state"
)

// A coin a stopped run took goes back in the pool only when the node shows
// it unspent (status OK). One the node shows spent is reported, and one it
// does not say about (NOT_FOUND, IMMATURE, a SPENT naming no spender, a
// transaction it does not serve) is carried over to the next command.
func TestReconcileReturnsACoinOnlyOnTheNodesWordThatItIsUnspent(t *testing.T) {
	dir := t.TempDir()
	w, err := bwallet.Create(dir, Profile)
	if err != nil {
		t.Fatal(err)
	}
	st, err := state.Load(dir, w.Signer().IdentityHex())
	if err != nil {
		t.Fatal(err)
	}
	parent := strings.Repeat("11", 32)
	spender := strings.Repeat("ab", 32)
	node := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/utxos/"+parent+"/json" {
			http.NotFound(rw, r)
			return
		}
		fmt.Fprintf(rw, `[{"vout":0,"status":"OK"},{"vout":1,"status":"SPENT","spendingData":{"txId":"%s"}},{"vout":2,"status":"NOT_FOUND"},{"vout":3,"status":"IMMATURE"},{"vout":4,"status":"SPENT"}]`, spender)
	}))
	defer node.Close()
	coin := func(txid string, vout uint32) bwallet.Output {
		return bwallet.Output{TxID: txid, Vout: vout, Satoshis: 5}
	}
	st.Taking = []bwallet.Output{coin(parent, 0), coin(parent, 1), coin(parent, 2), coin(parent, 3), coin(parent, 4), coin(strings.Repeat("22", 32), 0)}
	var notes []string
	carried := Reconcile(context.Background(), st, w.Pool, &nodeapi.Asset{Base: node.URL}, func(f string, a ...any) { notes = append(notes, fmt.Sprintf(f, a...)) })
	held := map[string]bool{}
	for _, o := range w.Pool.Outputs() {
		held[o.Outpoint()] = true
	}
	if !held[coin(parent, 0).Outpoint()] || len(held) != 1 {
		t.Fatalf("the pool holds %v, want only the OK coin", held)
	}
	var got []string
	for _, o := range carried {
		got = append(got, o.Outpoint())
	}
	want := []string{coin(parent, 2).Outpoint(), coin(parent, 3).Outpoint(), coin(parent, 4).Outpoint(), coin(strings.Repeat("22", 32), 0).Outpoint()}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("carried %v, want %v", got, want)
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "is spent by") || strings.Count(joined, "could not say whether it is spent") != 4 || strings.Count(joined, "back in the pool") != 1 {
		t.Fatalf("notes:\n%s", joined)
	}
}
