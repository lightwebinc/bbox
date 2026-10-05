package state

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// A state file written before the payee record moved into payee.Book loads
// and saves byte for byte: "settled" and "unsettleable" keep their place.
func TestPayeeBookKeepsStateBytes(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("testdata", "state-payee.json"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, File), want, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Settled) != 2 || !s.IsUnsettleable("46608f785a1f65b0ce37238455737df12752752e1491bf04e00da097cdeb071c") {
		t.Fatalf("record not read: %+v %+v", s.Settled, s.Unsettleable)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, File))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("saved state differs:\n%s\nwant:\n%s", got, want)
	}
}
