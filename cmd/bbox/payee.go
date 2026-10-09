package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/lightwebinc/bcommon/payee"
	"github.com/lightwebinc/bcommon/payeecmd"
	"github.com/lightwebinc/bcommon/purse"
)

// cmdPayee is bcommon's payee verbs (package payeecmd) over this home: its
// identity is the payee key, and its wallet the payee's.
func cmdPayee(ctx context.Context, g *global, args []string) error {
	c := g.payeeCommand()
	fs := g.flagSet("payee", c.Help())
	f := c.Bind(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	return c.Run(ctx, f, pos)
}

func (g *global) payeeCommand() *payeecmd.Command {
	return &payeecmd.Command{App: "bbox", KeyDir: g.cfg.Home, Stdout: g.stdout, Stderr: g.stderr, Note: g.say,
		Usage: usage, Refused: refused, Open: g.openPayee}
}

// openPayee opens the home for payee settle: the purse it settles through,
// the book in its state, and its pool.
func (g *global) openPayee(ctx context.Context) (*payeecmd.Session, error) {
	h, err := g.openHome()
	if err != nil {
		return nil, err
	}
	hc, err := g.headerClient()
	if err != nil {
		h.close()
		return nil, err
	}
	p, err := g.purse(ctx, h, hc, 0)
	if err != nil {
		h.close()
		return nil, err
	}
	return &payeecmd.Session{Payer: p, Record: payee.Saved(&h.st.Book, h.st.Save), Pool: h.e.Pool, Close: h.close}, nil
}

// payWords puts a refusal of the purse in this command's words. The library
// names no flag and no config key; the command that set them does.
func payWords(err error, maxSats uint64) error {
	switch {
	case errors.Is(err, purse.ErrOverMaxPay):
		return fmt.Errorf("%w: more than the %d this command may pay (-max-sats)", err, maxSats)
	case errors.Is(err, purse.ErrNoNode):
		return fmt.Errorf("%w (config key chain)", err)
	case errors.Is(err, purse.ErrNoSettler):
		return fmt.Errorf("%w (config key settle)", err)
	case errors.Is(err, purse.ErrNotMined):
		return fmt.Errorf("%w; run the command again to take it into the pool once it is", err)
	}
	return err
}
