package testchain

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"sync"

	"github.com/bsv-blockchain/go-sdk/auth"
	"github.com/bsv-blockchain/go-sdk/auth/authpayload"
	"github.com/bsv-blockchain/go-sdk/auth/brc104"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/spv"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bbox/internal/boxrec"
)

// Paid is a stand-in for a host's terms route (spec sections 7.3 and 7.4):
// the terms document, and lookups over BRC-104 in which a priced class is
// answered 402 (BRC-105) until a payment pays the price to the key BRC-29
// derives for the payee, the prefix it issued, the payer's suffix and the
// payer, and verifies against the chain. It answers from a Host. Requests
// are served one at a time.
type Paid struct {
	Host   *Host
	Prices map[string]uint64
	// Payments are the payments it accepted: txid, satoshis, the remittance
	// and the Atomic BEEF, as a host's payment ledger records them.
	Payments []Payment

	// Again answers 402 to every priced question, a payment or none: a
	// host that keeps asking. FailPaid records a payment and then answers
	// 500: a host that took the money and did not answer.
	Again    bool
	FailPaid bool
	// OnPayment, when set, runs when a request carrying a payment arrives,
	// before anything is done with it.
	OnPayment func()

	// limited are the authenticated requests, by their count, it refuses
	// 429 without a signature (Limit).
	limited []int
	asked   int

	mu     sync.Mutex
	wallet *wallet.ProtoWallet
	peer   *auth.Peer
	tr     *serverTransport
	issued map[string]bool
	used   map[string]bool
}

// Limit makes it refuse the nth authenticated requests from now, counted
// from 1, with 429 and no signature, as a host does for a session over its
// budget of signed responses.
func (p *Paid) Limit(nth ...int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.limited = nil
	for _, n := range nth {
		p.limited = append(p.limited, p.asked+n)
	}
}

// Payment is one accepted payment, in the host ledger's shape.
type Payment struct {
	Txid              string `json:"txid"`
	Beef              string `json:"beef"`
	OutputIndex       int    `json:"outputIndex"`
	Satoshis          uint64 `json:"satoshis"`
	DerivationPrefix  string `json:"derivationPrefix"`
	DerivationSuffix  string `json:"derivationSuffix"`
	SenderIdentityKey string `json:"senderIdentityKey"`
	Class             string `json:"class"`
}

// NewPaid is a terms route for host, paid to payee.
func NewPaid(host *Host, payee *ec.PrivateKey, prices map[string]uint64) *Paid {
	w, err := wallet.NewProtoWallet(wallet.ProtoWalletArgs{Type: wallet.ProtoWalletArgsTypePrivateKey, PrivateKey: payee})
	if err != nil {
		panic(err)
	}
	p := &Paid{Host: host, Prices: prices, wallet: w, tr: &serverTransport{}, issued: map[string]bool{}, used: map[string]bool{}}
	p.peer = auth.NewPeer(&auth.PeerOptions{Wallet: &peerWallet{w}, Transport: p.tr, SessionManager: auth.NewSessionManager()})
	return p
}

// peerWallet is the payee's ProtoWallet as the wallet.Interface a Peer
// takes; a Peer uses only the key and signature methods.
type peerWallet struct{ *wallet.ProtoWallet }

func (peerWallet) CreateAction(context.Context, wallet.CreateActionArgs, string) (*wallet.CreateActionResult, error) {
	return nil, errors.New("not a spending wallet")
}
func (peerWallet) SignAction(context.Context, wallet.SignActionArgs, string) (*wallet.SignActionResult, error) {
	return nil, errors.New("not a spending wallet")
}
func (peerWallet) AbortAction(context.Context, wallet.AbortActionArgs, string) (*wallet.AbortActionResult, error) {
	return nil, errors.New("not a spending wallet")
}
func (peerWallet) ListActions(context.Context, wallet.ListActionsArgs, string) (*wallet.ListActionsResult, error) {
	return nil, errors.New("not a spending wallet")
}
func (peerWallet) InternalizeAction(context.Context, wallet.InternalizeActionArgs, string) (*wallet.InternalizeActionResult, error) {
	return nil, errors.New("not a spending wallet")
}
func (peerWallet) ListOutputs(context.Context, wallet.ListOutputsArgs, string) (*wallet.ListOutputsResult, error) {
	return nil, errors.New("not a spending wallet")
}
func (peerWallet) RelinquishOutput(context.Context, wallet.RelinquishOutputArgs, string) (*wallet.RelinquishOutputResult, error) {
	return nil, errors.New("not a spending wallet")
}
func (peerWallet) AcquireCertificate(context.Context, wallet.AcquireCertificateArgs, string) (*wallet.Certificate, error) {
	return nil, errors.New("no certificates")
}
func (peerWallet) ListCertificates(context.Context, wallet.ListCertificatesArgs, string) (*wallet.ListCertificatesResult, error) {
	return &wallet.ListCertificatesResult{}, nil
}
func (peerWallet) ProveCertificate(context.Context, wallet.ProveCertificateArgs, string) (*wallet.ProveCertificateResult, error) {
	return nil, errors.New("no certificates")
}
func (peerWallet) RelinquishCertificate(context.Context, wallet.RelinquishCertificateArgs, string) (*wallet.RelinquishCertificateResult, error) {
	return nil, errors.New("no certificates")
}
func (peerWallet) DiscoverByIdentityKey(context.Context, wallet.DiscoverByIdentityKeyArgs, string) (*wallet.DiscoverCertificatesResult, error) {
	return nil, errors.New("no certificates")
}
func (peerWallet) DiscoverByAttributes(context.Context, wallet.DiscoverByAttributesArgs, string) (*wallet.DiscoverCertificatesResult, error) {
	return nil, errors.New("no certificates")
}
func (peerWallet) IsAuthenticated(context.Context, any, string) (*wallet.AuthenticatedResult, error) {
	return &wallet.AuthenticatedResult{Authenticated: true}, nil
}
func (peerWallet) WaitForAuthentication(context.Context, any, string) (*wallet.AuthenticatedResult, error) {
	return &wallet.AuthenticatedResult{Authenticated: true}, nil
}
func (peerWallet) GetHeight(context.Context, any, string) (*wallet.GetHeightResult, error) {
	return nil, errors.New("no chain")
}
func (peerWallet) GetHeaderForHeight(context.Context, wallet.GetHeaderArgs, string) (*wallet.GetHeaderResult, error) {
	return nil, errors.New("no chain")
}
func (peerWallet) GetNetwork(context.Context, any, string) (*wallet.GetNetworkResult, error) {
	return &wallet.GetNetworkResult{Network: wallet.NetworkTestnet}, nil
}
func (peerWallet) GetVersion(context.Context, any, string) (*wallet.GetVersionResult, error) {
	return &wallet.GetVersionResult{Version: "testchain-1"}, nil
}

// serverTransport hands a Peer the messages a request carries and catches
// what the Peer sends back, one request at a time.
type serverTransport struct {
	onData func(context.Context, *auth.AuthMessage) error
	sent   *auth.AuthMessage
}

func (t *serverTransport) Send(_ context.Context, m *auth.AuthMessage) error {
	t.sent = m
	return nil
}

func (t *serverTransport) OnData(cb func(context.Context, *auth.AuthMessage) error) error {
	t.onData = cb
	return nil
}

func (t *serverTransport) GetRegisteredOnData() (func(context.Context, *auth.AuthMessage) error, error) {
	if t.onData == nil {
		return nil, errors.New("no handler")
	}
	return t.onData, nil
}

func (p *Paid) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tr.onData == nil {
		if err := p.peer.Start(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "body", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	if r.URL.Path == "/.well-known/auth" && r.Method == http.MethodPost {
		var m auth.AuthMessage
		if err := json.Unmarshal(body, &m); err != nil {
			http.Error(w, "not an auth message", http.StatusBadRequest)
			return
		}
		p.tr.sent = nil
		if err := p.tr.onData(ctx, &m); err != nil || p.tr.sent == nil {
			http.Error(w, "handshake failed", http.StatusUnauthorized)
			return
		}
		writeJSON(w, p.tr.sent)
		return
	}
	idHex := r.Header.Get(brc104.HeaderIdentityKey)
	if idHex == "" {
		p.reply(w, p.route(r, body, nil))
		return
	}
	reqID, err := base64.StdEncoding.DecodeString(r.Header.Get(brc104.HeaderRequestID))
	if err != nil || len(reqID) != brc104.RequestIDLength {
		http.Error(w, "request id", http.StatusUnauthorized)
		return
	}
	id, err := ec.PublicKeyFromString(idHex)
	if err != nil {
		http.Error(w, "identity", http.StatusUnauthorized)
		return
	}
	sig, _ := hex.DecodeString(r.Header.Get(brc104.HeaderSignature))
	r.Body = io.NopCloser(bytes.NewReader(body))
	payload, err := authpayload.FromHTTPRequest(reqID, r)
	if err != nil {
		http.Error(w, "payload", http.StatusBadRequest)
		return
	}
	m := &auth.AuthMessage{Version: r.Header.Get(brc104.HeaderVersion), MessageType: auth.MessageTypeGeneral, IdentityKey: id,
		Nonce: r.Header.Get(brc104.HeaderNonce), YourNonce: r.Header.Get(brc104.HeaderYourNonce), Signature: sig, Payload: payload}
	if err := p.tr.onData(ctx, m); err != nil {
		http.Error(w, "the request does not verify", http.StatusUnauthorized)
		return
	}
	p.asked++
	if slices.Contains(p.limited, p.asked) {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "too many requests on this session", http.StatusTooManyRequests)
		return
	}
	res := p.route(r, body, id)
	respPayload, err := authpayload.FromResponse(reqID, authpayload.SimplifiedHttpResponse{StatusCode: res.code, Header: res.header, Body: res.body})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	p.tr.sent = nil
	if err := p.peer.ToPeer(ctx, respPayload, id, 5000); err != nil || p.tr.sent == nil {
		http.Error(w, "signing the response failed", http.StatusInternalServerError)
		return
	}
	s := p.tr.sent
	res.header.Set(brc104.HeaderVersion, s.Version)
	res.header.Set(brc104.HeaderIdentityKey, s.IdentityKey.ToDERHex())
	res.header.Set(brc104.HeaderNonce, s.Nonce)
	res.header.Set(brc104.HeaderYourNonce, s.YourNonce)
	res.header.Set(brc104.HeaderSignature, hex.EncodeToString(s.Signature))
	res.header.Set(brc104.HeaderRequestID, base64.StdEncoding.EncodeToString(reqID))
	p.reply(w, res)
}

type response struct {
	code   int
	header http.Header
	body   []byte
}

func (p *Paid) reply(w http.ResponseWriter, r response) {
	for k, v := range r.header {
		w.Header()[k] = v
	}
	w.WriteHeader(r.code)
	_, _ = w.Write(r.body)
}

func jsonResponse(code int, v any) response {
	b, _ := json.Marshal(v)
	return response{code: code, header: http.Header{"Content-Type": {"application/json"}}, body: b}
}

func (p *Paid) route(r *http.Request, body []byte, asker *ec.PublicKey) response {
	if r.URL.Path == "/ls_bbox/terms" && r.Method == http.MethodGet {
		if len(p.Prices) == 0 {
			return jsonResponse(http.StatusNotFound, map[string]string{"status": "error", "code": "ERR_NO_TERMS"})
		}
		var classes []map[string]any
		for _, c := range boxrec.Classes {
			if s, ok := p.Prices[c.Name]; ok {
				classes = append(classes, map[string]any{"class": c.Name, "satoshis": s})
			}
		}
		return jsonResponse(http.StatusOK, map[string]any{"service": "ls_bbox", "terms": 1, "classes": classes})
	}
	if r.URL.Path != "/lookup" || r.Method != http.MethodPost {
		return jsonResponse(http.StatusNotFound, map[string]string{"error": "no such route"})
	}
	var q struct {
		Service string         `json:"service"`
		Query   map[string]any `json:"query"`
	}
	if err := json.Unmarshal(body, &q); err != nil || q.Service != boxrec.LookupService {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": "not an ls_bbox question"})
	}
	qq, err := boxrec.ParseQuery(q.Query)
	if err != nil {
		return jsonResponse(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	extra := http.Header{}
	if price := p.Prices[qq.Class.Name]; price > 0 {
		if asker == nil {
			return jsonResponse(http.StatusUnauthorized, map[string]string{"code": "ERR_AUTH_REQUIRED"})
		}
		raw := r.Header.Get("x-bsv-payment")
		if raw != "" && p.OnPayment != nil {
			p.OnPayment()
		}
		if raw == "" || p.Again {
			var n [32]byte
			copy(n[:], []byte(strconv.Itoa(len(p.issued)+1)))
			prefix := base64.StdEncoding.EncodeToString(n[:])
			p.issued[prefix] = true
			res := jsonResponse(http.StatusPaymentRequired, map[string]any{"code": "ERR_PAYMENT_REQUIRED", "satoshisRequired": price})
			res.header.Set("x-bsv-payment-version", "1.0")
			res.header.Set("x-bsv-payment-satoshis-required", strconv.FormatUint(price, 10))
			res.header.Set("x-bsv-payment-derivation-prefix", prefix)
			return res
		}
		paid, code := p.pay(r.Context(), raw, asker, price, qq.Class.Name)
		if code != "" {
			return jsonResponse(http.StatusBadRequest, map[string]string{"code": code})
		}
		if p.FailPaid {
			return jsonResponse(http.StatusInternalServerError, map[string]string{"error": "the index is not restored yet"})
		}
		extra.Set("x-bsv-payment-satoshis-paid", strconv.FormatUint(paid, 10))
	}
	p.Host.mu.Lock()
	outs := p.Host.Answer(qq)
	p.Host.mu.Unlock()
	res := jsonResponse(http.StatusOK, map[string]any{"type": "output-list", "outputs": outs})
	for k, v := range extra {
		res.header[k] = v
	}
	return res
}

// pay checks a BRC-105 payment and records it; a non-empty code refuses it.
func (p *Paid) pay(ctx context.Context, raw string, asker *ec.PublicKey, price uint64, class string) (uint64, string) {
	var h struct {
		Prefix string `json:"derivationPrefix"`
		Suffix string `json:"derivationSuffix"`
		Tx     string `json:"transaction"`
	}
	if err := json.Unmarshal([]byte(raw), &h); err != nil || h.Prefix == "" || h.Suffix == "" {
		return 0, "ERR_MALFORMED_PAYMENT"
	}
	if !p.issued[h.Prefix] {
		return 0, "ERR_INVALID_DERIVATION_PREFIX"
	}
	beef, err := base64.StdEncoding.DecodeString(h.Tx)
	if err != nil {
		return 0, "ERR_MALFORMED_PAYMENT"
	}
	tx, err := transaction.NewTransactionFromBEEF(beef)
	if err != nil || len(tx.Outputs) == 0 || tx.Outputs[0].Satoshis < price {
		return 0, "ERR_INVALID_PAYMENT"
	}
	forSelf := true
	k, err := p.wallet.GetPublicKey(ctx, wallet.GetPublicKeyArgs{EncryptionArgs: wallet.EncryptionArgs{
		ProtocolID: boxrec.PaymentProtocol, KeyID: h.Prefix + " " + h.Suffix,
		Counterparty: wallet.Counterparty{Type: wallet.CounterpartyTypeOther, Counterparty: asker}}, ForSelf: &forSelf}, "")
	if err != nil {
		return 0, "ERR_INVALID_PAYMENT"
	}
	addr, _ := script.NewAddressFromPublicKey(k.PublicKey, false)
	want, _ := p2pkh.Lock(addr)
	if !bytes.Equal(*tx.Outputs[0].LockingScript, *want) {
		return 0, "ERR_INVALID_PAYMENT"
	}
	if ok, err := spv.Verify(ctx, tx, p.Host.chain, nil); err != nil || !ok {
		return 0, "ERR_PAYMENT_SPV"
	}
	id := tx.TxID().String()
	if p.used[id] {
		return 0, "ERR_PAYMENT_REPLAYED"
	}
	p.used[id] = true
	p.Payments = append(p.Payments, Payment{Txid: id, Beef: base64.StdEncoding.EncodeToString(beef), Satoshis: tx.Outputs[0].Satoshis,
		DerivationPrefix: h.Prefix, DerivationSuffix: h.Suffix, SenderIdentityKey: asker.ToDERHex(), Class: class})
	return tx.Outputs[0].Satoshis, ""
}

// Ledger writes the accepted payments as the host module's payments.jsonl.
func (p *Paid) Ledger() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	var b bytes.Buffer
	for _, x := range p.Payments {
		line, _ := json.Marshal(x)
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.Bytes()
}
