package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bbox/internal/state"
)

// A run that stops is a home as its files stood at that moment. These
// tests copy the home at the moment a funding tree reaches the settlement
// leg, after the home recorded it and before anything else, and go on in
// the copy: the home the next command finds.

// stopAt copies the home name to the home as when the next transaction
// reaches the settlement leg, and returns that transaction once run has
// returned. land says whether the chain then takes the transaction; either
// way the run that sent it hears a refusal, and its own home is left
// behind.
func (h *harness) stopAt(name, as string, land bool, run func()) *transaction.Transaction {
	h.t.Helper()
	var sent *transaction.Transaction
	var failed error
	h.mu.Lock()
	h.refuse = func(tx *transaction.Transaction) bool {
		// Nothing the run sends after that moment reaches the chain.
		if sent != nil {
			return true
		}
		sent = tx
		failed = stoppedHome(filepath.Join(h.dir, name), filepath.Join(h.dir, as))
		if land && failed == nil {
			failed = h.chain.Send(tx)
		}
		return true
	}
	h.mu.Unlock()
	run()
	h.mu.Lock()
	h.refuse = nil
	h.mu.Unlock()
	if sent == nil || failed != nil {
		h.t.Fatalf("the run sent %v to the settlement leg: %v", sent, failed)
	}
	return sent
}

// stoppedHome copies a home a command is running in: the lock and the
// temporary files of a save under way are left out.
func stoppedHome(from, to string) error {
	return filepath.WalkDir(from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		rel, _ := filepath.Rel(from, p)
		dst := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o700)
		}
		if d.Name() == "lock" || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		b, err := os.ReadFile(p)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o600)
	})
}

// treeRecords is what a home's state says of its funding trees.
type treeRecords struct {
	Tree *struct {
		Txid string `json:"txid"`
	} `json:"tree"`
	Ahead []struct {
		Txid string `json:"txid"`
	} `json:"ahead"`
	PendingTrees []struct {
		Tree struct {
			Txid string `json:"txid"`
		} `json:"tree"`
		Coin struct {
			TxID string `json:"txid"`
			Vout uint32 `json:"vout"`
		} `json:"coin"`
		ReturnedOnce bool `json:"returned_once"`
	} `json:"pendingTrees"`
}

// coin is the outpoint of the first record's coin.
func (r treeRecords) coin() string {
	return fmt.Sprintf("%s.%d", r.PendingTrees[0].Coin.TxID, r.PendingTrees[0].Coin.Vout)
}

func (h *harness) trees(name string) treeRecords {
	h.t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.dir, name, state.File))
	if err != nil {
		h.t.Fatal(err)
	}
	var st treeRecords
	if err := json.Unmarshal(raw, &st); err != nil {
		h.t.Fatal(err)
	}
	return st
}

// poolCoins are the outpoints a home's pool holds, as txid.vout.
func (h *harness) poolCoins(name string) map[string]bool {
	h.t.Helper()
	g, _, _ := h.global(name)
	e, err := g.openWallet()
	if err != nil {
		h.t.Fatal(err)
	}
	out := map[string]bool{}
	for _, o := range e.Pool.Outputs() {
		out[o.Outpoint()] = true
	}
	return out
}

// start runs what every command that spends runs first, and nothing else:
// a command that mints nothing. It returns what it wrote to standard error.
func (h *harness) start(name string) string {
	h.t.Helper()
	g, _, errb := h.global(name)
	hm, err := g.openHome()
	if err != nil {
		h.t.Fatal(err)
	}
	defer hm.close()
	eng, err := g.engine(context.Background(), hm, 4)
	if err != nil {
		h.t.Fatalf("start: %v\n%s", err, errb.String())
	}
	if err := eng.Close(context.Background()); err != nil {
		h.t.Fatal(err)
	}
	return errb.String()
}

// A run that stops after it signed a tree and before the settlement leg
// took it loses nothing: the next command finds the record, the node knows
// no such tree and shows the coin unspent, and the coin is back in the
// pool before anything is minted.
func TestARunStoppedBeforeTheTreeWasSent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.identity("alice")
	bob := h.identity("bob")
	h.newOffice("bob")
	before := h.poolCoins("alice")
	tree := h.stopAt("alice", "stopped", false, func() {
		if r := h.run("alice", "", "send", bob, "-m", "one", "-rate", "20", "-tree-count", "4"); r.code == 0 {
			t.Fatalf("a send whose tree the leg refused:\n%s\n%s", r.stdout, r.stderr)
		}
	})
	st := h.trees("stopped")
	if st.Tree != nil || len(st.PendingTrees) != 1 || st.PendingTrees[0].Tree.Txid != tree.TxID().String() {
		t.Fatalf("the home the run left: %+v", st)
	}
	coin := st.coin()
	if !before[coin] || h.poolCoins("stopped")[coin] {
		t.Fatalf("the coin %s: in the pool before %v, after the run stopped %v", coin, before[coin], h.poolCoins("stopped")[coin])
	}
	if r := h.must("stopped", "", "doctor"); !strings.Contains(r.stdout, "signed for coin "+coin+" and not recorded as minted") {
		t.Fatalf("doctor:\n%s", r.stdout)
	}
	r := h.must("stopped", "", "send", bob, "-m", "one", "-rate", "20", "-tree-count", "4")
	if !strings.Contains(r.stderr, "never reached the chain: its fee coin "+coin+" is unspent and back in the pool") {
		t.Fatalf("the next command:\n%s\n%s", r.stdout, r.stderr)
	}
	st = h.trees("stopped")
	if st.Tree == nil || !h.chain.Mined(st.Tree.Txid) {
		t.Fatalf("no funding tree after the recovery: %+v", st)
	}
	for _, p := range st.PendingTrees {
		if p.Tree.Txid == st.Tree.Txid {
			t.Fatalf("the adopted tree still has a record: %+v", st)
		}
	}
	if r := h.must("bob", "", "list"); strings.Count(r.stdout, "\n") != 1 {
		t.Fatalf("list:\n%s\n%s", r.stdout, r.stderr)
	}
}

// A tree the node does not know, with its coin unspent, keeps its record
// for one more command: the coin is back at once, and the record goes only
// when a second command hears the same answer.
func TestAReturnedCoinKeepsItsRecordForOneMoreCommand(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.identity("alice")
	bob := h.identity("bob")
	h.newOffice("bob")
	h.stopAt("alice", "stopped", false, func() {
		h.run("alice", "", "send", bob, "-m", "one", "-rate", "20", "-tree-count", "4")
	})
	coin := h.trees("stopped").coin()
	errs := h.start("stopped")
	st := h.trees("stopped")
	if len(st.PendingTrees) != 1 || !st.PendingTrees[0].ReturnedOnce || !h.poolCoins("stopped")[coin] ||
		!strings.Contains(errs, "its record is kept for one more command") {
		t.Fatalf("after the first command: %+v, coin in the pool %v\n%s", st, h.poolCoins("stopped")[coin], errs)
	}
	errs = h.start("stopped")
	st = h.trees("stopped")
	if len(st.PendingTrees) != 0 || !h.poolCoins("stopped")[coin] || !strings.Contains(errs, "its record is dropped") {
		t.Fatalf("after the second command: %+v, coin in the pool %v\n%s", st, h.poolCoins("stopped")[coin], errs)
	}
	if errs = h.start("stopped"); strings.Contains(errs, "funding tree") {
		t.Fatalf("a third command still speaks of the tree:\n%s", errs)
	}
}

// A tree that reaches the node after a command put its coin back is found
// by the next command, which adopts it and takes the coin, now spent, out
// of the pool: nothing is built on a spent coin.
func TestATreeThatLandsAfterItsCoinWentBack(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.identity("alice")
	bob := h.identity("bob")
	h.newOffice("bob")
	tree := h.stopAt("alice", "stopped", false, func() {
		h.run("alice", "", "send", bob, "-m", "one", "-rate", "20", "-tree-count", "4")
	})
	coin := h.trees("stopped").coin()
	h.start("stopped")
	if !h.poolCoins("stopped")[coin] {
		t.Fatal("the coin did not go back")
	}
	// The tree lands after all.
	if err := h.chain.Send(tree); err != nil {
		t.Fatal(err)
	}
	errs := h.start("stopped")
	st := h.trees("stopped")
	if st.Tree == nil || st.Tree.Txid != tree.TxID().String() || len(st.PendingTrees) != 0 {
		t.Fatalf("the tree that landed late: %+v\n%s", st, errs)
	}
	pool := h.poolCoins("stopped")
	if pool[coin] || !strings.Contains(errs, "its fee coin "+coin+" is spent and is taken out of the pool") {
		t.Fatalf("the spent coin is still in the pool (%v):\n%s", pool[coin], errs)
	}
	change := false
	for op := range pool {
		change = change || strings.HasPrefix(op, tree.TxID().String()+".")
	}
	if !change {
		t.Fatalf("the tree's change is not in the pool: %v", pool)
	}
	h.send("stopped", bob, "one")
	if got := h.trees("stopped"); got.Tree.Txid != tree.TxID().String() {
		t.Fatalf("the send minted another tree: %+v", got)
	}
}

// A run that stops after the settlement leg took a tree and before the
// home adopted it loses neither the tree nor its change: the next command
// finds the tree on the chain and adopts it.
func TestARunStoppedAfterTheTreeWasSent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.identity("alice")
	bob := h.identity("bob")
	h.newOffice("bob")
	coins := len(h.poolCoins("alice"))
	tree := h.stopAt("alice", "stopped", true, func() {
		h.run("alice", "", "send", bob, "-m", "one", "-rate", "20", "-tree-count", "4")
	})
	id := tree.TxID().String()
	if st := h.trees("stopped"); st.Tree != nil || len(st.PendingTrees) != 1 || len(h.poolCoins("stopped")) != coins-1 {
		t.Fatalf("the home the run left: %+v, %d coin(s) of %d", st, len(h.poolCoins("stopped")), coins)
	}
	r := h.must("stopped", "", "send", bob, "-m", "one", "-rate", "20", "-tree-count", "4")
	if !strings.Contains(r.stderr, "funding tree "+id+" is recovered") {
		t.Fatalf("the next command:\n%s\n%s", r.stdout, r.stderr)
	}
	st := h.trees("stopped")
	if st.Tree == nil || st.Tree.Txid != id || len(st.PendingTrees) != 0 {
		t.Fatalf("after the recovery: %+v", st)
	}
	// The tree's change replaces the coin it spent.
	pool := h.poolCoins("stopped")
	if len(pool) != coins || !pool[id+".4"] {
		t.Fatalf("the pool after the recovery: %d coin(s) of %d, the tree's change %v", len(pool), coins, pool[id+".4"])
	}
	if r := h.must("bob", "", "list"); strings.Count(r.stdout, "\n") != 1 {
		t.Fatalf("list:\n%s\n%s", r.stdout, r.stderr)
	}
}

// A run that stops while a tree minted ahead is on its way to the chain
// leaves a tree no list of the home names. The next command finds it, holds
// it as the tree minted ahead, mints no other, and the switch adopts it and
// drops its record.
func TestARunStoppedWhileATreeWasMintedAhead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.identity("alice")
	bob := h.identity("bob")
	h.newOffice("bob")
	h.send("alice", bob, "one")
	// The envelope of the run that stops reaches no host: the home that
	// goes on is the copy, and it spends the same funding outputs.
	h.a.SetDown(true)
	h.b.SetDown(true)
	tree := h.stopAt("alice", "stopped", true, func() {
		h.run("alice", "", "send", bob, "-m", "two", "-rate", "20", "-tree-count", "4")
	})
	h.a.SetDown(false)
	h.b.SetDown(false)
	id := tree.TxID().String()
	st := h.trees("stopped")
	if st.Tree == nil || st.Tree.Txid == id || len(st.Ahead) != 0 || len(st.PendingTrees) != 1 || st.PendingTrees[0].Tree.Txid != id {
		t.Fatalf("the home the run left: %+v", st)
	}
	first := st.Tree.Txid
	r := h.must("stopped", "", "send", bob, "-m", "next", "-rate", "20", "-tree-count", "4")
	if !strings.Contains(r.stderr, "funding tree "+id+" is recovered and waits for the switch") || strings.Contains(r.stderr, "so the next is minted ahead") {
		t.Fatalf("the next command:\n%s\n%s", r.stdout, r.stderr)
	}
	st = h.trees("stopped")
	if st.Tree.Txid != first || len(st.Ahead) != 1 || st.Ahead[0].Txid != id || len(st.PendingTrees) != 1 {
		t.Fatalf("after the recovery: %+v", st)
	}
	if !h.poolCoins("stopped")[id+".4"] {
		t.Fatal("the change of the tree minted ahead is not in the pool")
	}
	// The record stays until the switch, and no command mints another tree
	// or speaks of the recovery again.
	switched := false
	for i := 0; i < 4 && !switched; i++ {
		r = h.must("stopped", "", "send", bob, "-m", "more", "-rate", "20", "-tree-count", "4")
		if strings.Contains(r.stderr, "so the next is minted ahead") || strings.Contains(r.stderr, "is recovered") {
			t.Fatalf("send %d:\n%s", i, r.stderr)
		}
		switched = strings.Contains(r.stderr, "switching to funding tree "+id)
		if st = h.trees("stopped"); !switched && (st.Tree.Txid != first || len(st.PendingTrees) != 1) {
			t.Fatalf("before the switch: %+v", st)
		}
	}
	if st = h.trees("stopped"); !switched || st.Tree.Txid != id || len(st.Ahead) != 0 || len(st.PendingTrees) != 0 {
		t.Fatalf("after the switch (%v): %+v", switched, st)
	}
}

// A node that cannot answer decides nothing: the record stays, the command
// says so, and a command that needs no tree goes on.
func TestARecoveryTheNodeCannotAnswerKeepsTheRecord(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.identity("alice")
	bob := h.identity("bob")
	h.newOffice("bob")
	h.send("alice", bob, "one")
	// A record whose bytes are not the tree it names: nothing the node
	// says can settle it.
	h.editState("alice", func(st map[string]any) {
		tree := st["tree"].(map[string]any)
		st["pendingTrees"] = []any{map[string]any{
			"tree": map[string]any{"identityKey": tree["identityKey"], "txid": strings.Repeat("ab", 32), "rawHex": tree["rawHex"], "count": 4, "sats": 1},
			"coin": map[string]any{"txid": strings.Repeat("cd", 32), "vout": 0, "satoshis": 5},
		}}
	})
	r := h.must("alice", "", "send", bob, "-m", "two", "-rate", "20", "-tree-count", "4")
	if !strings.Contains(r.stderr, "could not be settled now") || !strings.Contains(r.stderr, "its record is kept and the next command asks again") {
		t.Fatalf("the command:\n%s\n%s", r.stdout, r.stderr)
	}
	if st := h.trees("alice"); len(st.PendingTrees) == 0 || st.PendingTrees[0].Tree.Txid != strings.Repeat("ab", 32) {
		t.Fatalf("the record: %+v", st)
	}
	// A reader asks no node about it.
	if r := h.must("bob", "", "list"); strings.Count(r.stdout, "\n") != 2 || strings.Contains(r.stderr, "funding tree") {
		t.Fatalf("list:\n%s\n%s", r.stdout, r.stderr)
	}
}
