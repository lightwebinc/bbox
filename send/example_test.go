package send_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/headers"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/publish"

	"github.com/lightwebinc/bbox/boxrec"
	"github.com/lightwebinc/bbox/send"
)

// A home is locked before its wallet is opened, one process at a time, and
// its state is started empty the first time.
func ExampleLockHome() {
	dir, err := os.MkdirTemp("", "bbox-home")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	unlock, err := send.LockHome(dir)
	if err != nil {
		panic(err)
	}
	defer unlock()
	_, err = send.LockHome(dir)
	fmt.Println(errors.Is(err, send.ErrLocked))

	st, err := send.LoadState(dir, "02aa")
	if err != nil {
		panic(err)
	}
	fmt.Println(st.Identity, len(st.Sent))
	// Output:
	// true
	// 02aa 0
}

// A direct message with an application's member, sent to the recipient's
// office and box over the plane: the home is locked, its wallet opened,
// the engine started (it finishes what an earlier run left), and the
// envelope sealed, persisted and published.
func ExampleEngine_Envelope() {
	ctx := context.Background()
	dir := "/path/to/home"
	var to []byte // the recipient's identity key

	unlock, err := send.LockHome(dir)
	if err != nil {
		return
	}
	defer unlock()
	w, err := bwallet.Open(dir, send.Profile)
	if err != nil {
		return
	}
	st, err := send.LoadState(dir, w.Signer().IdentityHex())
	if err != nil {
		return
	}
	// No node: WhatsOnChain headers and chain view, GorillaPool's arcade.
	hc := headers.New("woc:main")
	chain, err := nodeapi.ParseChain("woc:main", nodeapi.ChainOptions{Headers: hc})
	if err != nil {
		return
	}
	settler, arcade, err := publish.ParseSettler("arcade:main", publish.SettleOptions{Spends: chain})
	if err != nil {
		return
	}
	legs := send.Legs{
		Settler: settler,
		Arcade:  arcade,
		Chain:   chain,
		Headers: hc,
		Facade:  &publish.Facade{Base: "https://host.example"},
	}
	eng, err := send.New(st, w.Signer(), w.Pool, legs, send.Options{TreeCount: 32, TreeSats: 1, Ahead: 4,
		Fees: mint.DefaultFees, ObjectBound: 1 << 20, Poll: 2 * time.Second, Wait: 10 * time.Minute})
	if err != nil {
		return
	}
	defer func() { _ = eng.Close(context.WithoutCancel(ctx)) }()
	if err := eng.Start(ctx); err != nil {
		return
	}

	app := &boxrec.Object{}
	app.Set("v", boxrec.Int(1))
	doc := &boxrec.Object{}
	doc.Set(boxrec.PlainBody, "hello")
	doc.Set("app", app)
	plain, err := boxrec.EncodePlaintext(doc)
	if err != nil {
		return
	}
	now := uint64(time.Now().Unix()) //nolint:gosec // a clock after 1970
	sent, err := eng.Envelope(ctx, send.Letter{Office: "example_abcdefghij", To: to, Box: "inbox", Created: now, Plaintext: plain})
	if err != nil {
		return
	}
	fmt.Println("sent", sent.Txid)
}
