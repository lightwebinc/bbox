package chainview

import (
	"errors"
	"testing"
)

func TestRefusedAnswer(t *testing.T) {
	for _, c := range []struct {
		err  string
		want bool
	}{
		{"publish: arcade refused abc: REJECTED: bad-txns-inputs-missingorspent", true},
		{"publish: the network refused abc: DOUBLE_SPEND_ATTEMPTED", true},
		{"publish: arcade answered 465: fee too low", true},
		{"publish: arcade answered 422: unprocessable", true},
		{"publish: arcade answered 460: not extended format", true},
		{"publish: arcade answered 503: busy", false},
		{"publish: arcade answered 401: unauthorized", false},
		{"publish: sendrawtransaction: sendrawtransaction: rpc error -26: mandatory-script-verify-flag-failed", true},
		{"publish: sendrawtransaction: sendrawtransaction: rpc error -25: missing inputs", true},
		{"publish: sendrawtransaction: sendrawtransaction: rpc error -26: mempool full", false},
		{"publish: sendrawtransaction: sendrawtransaction: rpc error -26: too-long-mempool-chain", false},
		{"publish: sendrawtransaction: sendrawtransaction: rpc error -28: loading", false},
		{"publish: dial 192.0.2.1:8725: connection refused", false},
		{"context deadline exceeded", false},
	} {
		if _, got := RefusedAnswer(errors.New(c.err)); got != c.want {
			t.Errorf("%q: refused %v, want %v", c.err, got, c.want)
		}
	}
	if _, got := RefusedAnswer(nil); got {
		t.Error("nil is a refusal")
	}
}
