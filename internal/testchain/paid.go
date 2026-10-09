package testchain

import (
	"encoding/json"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/lightwebinc/bcommon/devkit/paidhost"

	"github.com/lightwebinc/bbox/boxrec"
)

// Paid is a stand-in for a host's terms route (spec sections 7.3 and 7.4):
// devkit's paidhost answering from a Host.
type Paid = paidhost.Paid

// Payment is one accepted payment, in the host ledger's shape.
type Payment = paidhost.Payment

// NewPaid is a terms route for host, paid to payee: ls_bbox's classes, its
// questions parsed by boxrec and answered by the host.
func NewPaid(host *Host, payee *ec.PrivateKey, prices map[string]uint64) *Paid {
	classes := make([]string, len(boxrec.Classes))
	for i, c := range boxrec.Classes {
		classes[i] = c.Name
	}
	return paidhost.New(paidhost.Service{Name: boxrec.LookupService, Classes: classes, Headers: host.chain,
		Ask: func(raw json.RawMessage) (string, func() (any, error), error) {
			var q map[string]any
			if err := json.Unmarshal(raw, &q); err != nil {
				return "", nil, err
			}
			qq, err := boxrec.ParseQuery(q)
			if err != nil {
				return "", nil, err
			}
			return qq.Class.Name, func() (any, error) {
				host.mu.Lock()
				defer host.mu.Unlock()
				return host.Answer(qq), nil
			}, nil
		}}, payee, prices)
}
