package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/lightwebinc/bcommon/purse"
)

// TestPayWords: each refusal of the purse reaches the user naming the flag
// or the config key that decides it, and still is the purse's error.
func TestPayWords(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{purse.ErrOverMaxPay, "more than the 6 this command may pay (-max-sats)"},
		{purse.ErrNoNode, "(config key chain)"},
		{purse.ErrNoSettler, "(config key settle)"},
		{purse.ErrNotMined, "run the command again to take it into the pool once it is"},
	} {
		got := payWords(fmt.Errorf("wrapped: %w", c.err), 6)
		if !errors.Is(got, c.err) || !strings.Contains(got.Error(), c.want) {
			t.Errorf("%v: %q, want it to say %q", c.err, got, c.want)
		}
	}
	other := errors.New("another failure")
	if got := payWords(other, 6); got != other {
		t.Errorf("another error is changed: %v", got)
	}
}
