package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoadAndTheIdentityIsKept(t *testing.T) {
	home := t.TempDir()
	s, err := Load(home, "02aa")
	if err != nil || s.Identity != "02aa" || len(s.Outbox) != 0 {
		t.Fatalf("a new state: %+v %v", s, err)
	}
	s.AddOffice("post_abcdefghij")
	s.AddOffice("post_abcdefghij")
	s.Outbox = append(s.Outbox, Pending{Kind: KindEnvelope, Txid: "t1", Beef: "00"})
	s.Remember(Received{Txid: "r1", Office: "post_abcdefghij", Paid: 5})
	s.Remember(Received{Txid: "r1", Office: "post_abcdefghij", Box: "inbox"})
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(home, File))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", fi.Mode(), err)
	}
	back, err := Load(home, "02aa")
	if err != nil || len(back.Offices) != 1 || len(back.Outbox) != 1 || len(back.Received) != 1 ||
		back.Received[0].Paid != 5 || back.Received[0].Box != "inbox" {
		t.Fatalf("%+v %v", back, err)
	}
	back.Done("t1")
	if len(back.Outbox) != 0 {
		t.Fatal("Done left the object in the outbox")
	}
	if _, err := Load(home, "03bb"); err == nil {
		t.Fatal("another identity's state loaded")
	}
}

func TestTheHomeLockIsExclusive(t *testing.T) {
	p := filepath.Join(t.TempDir(), "lock")
	unlock, err := Lock(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(p); !errors.Is(err, ErrLocked) {
		t.Fatalf("a second lock: %v", err)
	}
	unlock()
	again, err := Lock(p)
	if err != nil {
		t.Fatal(err)
	}
	again()
}

func TestInFlight(t *testing.T) {
	s := &State{Sweeps: []Sweep{{Txid: "a", Done: true}, {Txid: "f", Failed: "refused"}, {Txid: "b"}}}
	if sw := s.InFlight(); sw == nil || sw.Txid != "b" {
		t.Fatalf("%+v", sw)
	}
	s.Sweeps[2].Failed = "refused"
	if sw := s.InFlight(); sw != nil {
		t.Fatalf("a failed sweep is in flight: %+v", sw)
	}
}
