package module

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// The rows journal: the TypeScript module's bytes, line for line, and its
// group commit (BBOX_JOURNAL_SYNC_MS): lines in the file at once and in
// order, fsynced together off the caller's path, a process crash losing
// nothing, Close flushing, a failed fsync tried again.

func sLine(i int) Line {
	return Line{K: "s", Op: fmt.Sprintf("%064x.%d", i, i%3), S: fmt.Sprintf("%064x", i+1), Office: office, H: i64(int64(800 + i)), At: i64(int64(1_700_000_000 + i))}
}

// same compares two lines by their bytes (Line holds pointers).
func same(t *testing.T, a, b Line) bool {
	t.Helper()
	x, err := a.encode()
	if err != nil {
		t.Fatal(err)
	}
	y, err := b.encode()
	if err != nil {
		t.Fatal(err)
	}
	return string(x) == string(y)
}

// countingSyncs makes j count its fsyncs, each still a real one.
func countingSyncs(j *FileJournal) *atomic.Int64 {
	var n atomic.Int64
	j.fsync = func(f *os.File) error {
		n.Add(1)
		return f.Sync()
	}
	return &n
}

func loadAll(t *testing.T, d string) []Line {
	t.Helper()
	lines, err := (&FileJournal{Path: filepath.Join(d, JournalFile)}).Load()
	if err != nil {
		t.Fatal(err)
	}
	return lines
}

// TestJournalBytes holds each line kind to the bytes the TypeScript
// module's JSON.stringify writes (journal.ts), and reads them back.
func TestJournalBytes(t *testing.T) {
	c := strings.Repeat("c1", 32)
	txid := strings.Repeat("ab", 32)
	op := strings.Repeat("0f", 32) + ".3"
	to := "02" + strings.Repeat("7e", 32)
	cases := []struct {
		l    Line
		want string
	}{
		{Line{K: "c", Op: op, C: c, Txid: txid, Kind: "e", Office: office, To: to, At: i64(1800000000)},
			`{"k":"c","op":"` + op + `","c":"` + c + `","txid":"` + txid + `","kind":"e","office":"` + office + `","to":"` + to + `","at":1800000000}`},
		{Line{K: "c", Op: op, C: c, Txid: txid, Kind: "r", Office: office, By: to, Acks: []string{strings.Repeat("01", 32), strings.Repeat("02", 32)}, At: i64(7)},
			`{"k":"c","op":"` + op + `","c":"` + c + `","txid":"` + txid + `","kind":"r","office":"` + office + `","by":"` + to + `","acks":["` + strings.Repeat("01", 32) + `","` + strings.Repeat("02", 32) + `"],"at":7}`},
		{Line{K: "d", Op: op, C: c, W: boolp(true)}, `{"k":"d","op":"` + op + `","c":"` + c + `","w":true}`},
		{Line{K: "d", Op: op, C: c, W: boolp(false)}, `{"k":"d","op":"` + op + `","c":"` + c + `","w":false}`},
		{Line{K: "s", Op: op, S: txid, Office: office, H: i64(0), At: i64(9)}, `{"k":"s","op":"` + op + `","s":"` + txid + `","office":"` + office + `","h":0,"at":9}`},
	}
	d := t.TempDir()
	j, _ := NewFileJournal(d)
	for _, x := range cases {
		b, err := x.l.encode()
		if err != nil || string(b) != x.want+"\n" {
			t.Fatalf("writes\n%s\nwant\n%s", b, x.want)
		}
		if err := j.Append(x.l); err != nil {
			t.Fatal(err)
		}
	}
	_ = j.Close()
	got := loadAll(t, d)
	if len(got) != len(cases) {
		t.Fatal(len(got))
	}
	for i := range got {
		if !same(t, got[i], cases[i].l) {
			t.Fatalf("line %d reads back as %+v", i, got[i])
		}
	}
	if _, err := (&Line{K: "x"}).encode(); err == nil {
		t.Fatal("an unknown kind encodes")
	}
}

func TestJournalEveryLine(t *testing.T) {
	d := t.TempDir()
	j, _ := NewFileJournal(d)
	syncs := countingSyncs(j)
	for i := range 5 {
		if err := j.Append(sLine(i)); err != nil {
			t.Fatal(err)
		}
		if got := syncs.Load(); got != int64(i+1) {
			t.Fatalf("line %d: %d fsyncs, want one a line before Append returns", i, got)
		}
	}
	if j.armed || j.pending != 0 {
		t.Fatal("a window armed with SyncEvery zero")
	}
	_ = j.Close()
	if got := loadAll(t, d); len(got) != 5 {
		t.Fatalf("%d lines", len(got))
	}
}

func TestJournalGroupCommitOrder(t *testing.T) {
	d := t.TempDir()
	j, _ := NewFileJournal(d)
	j.SyncEvery = 2 * time.Millisecond
	syncs := countingSyncs(j)
	var covered atomic.Int64
	j.OnSync = func(lines int, _ time.Duration, err error) {
		if err != nil {
			t.Error(err)
		}
		covered.Add(int64(lines))
	}
	const n = 2000
	for i := range n {
		if err := j.Append(sLine(i)); err != nil {
			t.Fatal(err)
		}
		if i%250 == 0 {
			time.Sleep(3 * time.Millisecond)
		}
	}
	got := loadAll(t, d)
	if len(got) != n {
		t.Fatalf("%d lines in the file, want %d", len(got), n)
	}
	for i, l := range got {
		if !same(t, l, sLine(i)) {
			t.Fatalf("line %d is %+v", i, l)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for covered.Load() < n && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if covered.Load() != n {
		t.Fatalf("timed fsyncs covered %d of %d lines", covered.Load(), n)
	}
	if s := syncs.Load(); s < 1 || s >= n/4 {
		t.Fatalf("%d fsyncs for %d lines: not grouped", s, n)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
}

// The append path never waits for a timed fsync.
func TestJournalAppendDoesNotWaitForSync(t *testing.T) {
	d := t.TempDir()
	j, _ := NewFileJournal(d)
	j.SyncEvery = time.Millisecond
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	j.fsync = func(f *os.File) error {
		once.Do(func() { close(entered) })
		<-release
		return f.Sync()
	}
	if err := j.Append(sLine(0)); err != nil {
		t.Fatal(err)
	}
	<-entered
	done := make(chan error, 1)
	go func() { done <- j.Append(sLine(1)) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Append waited for the fsync in flight")
	}
	close(release)
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	if got := loadAll(t, d); len(got) != 2 || !same(t, got[1], sLine(1)) {
		t.Fatalf("%+v", got)
	}
}

// A process that dies with lines still waiting for their fsync loses none
// of them. The child is this test binary, killed by SIGKILL mid-window.
func TestJournalCrashKeepsWrittenLines(t *testing.T) {
	if d := os.Getenv("BBOX_JOURNAL_CRASH_DIR"); d != "" {
		j, _ := NewFileJournal(d)
		j.SyncEvery = time.Hour
		j.fsync = func(*os.File) error { return errors.New("no fsync in the crash child") }
		for i := range 300 {
			if err := j.Append(sLine(i)); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(3)
			}
		}
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		time.Sleep(time.Minute)
		os.Exit(4)
	}
	d := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestJournalCrashKeepsWrittenLines$", "-test.count=1")
	cmd.Env = append(os.Environ(), "BBOX_JOURNAL_CRASH_DIR="+d)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("the child was not killed: %v %s", err, out)
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("the child ended %v: %s", err, out)
	}
	got := loadAll(t, d)
	if len(got) != 300 {
		t.Fatalf("%d lines survived the crash, want 300", len(got))
	}
	for i, l := range got {
		if !same(t, l, sLine(i)) {
			t.Fatalf("line %d is %+v", i, l)
		}
	}
}

func TestJournalCloseFlushes(t *testing.T) {
	d := t.TempDir()
	j, _ := NewFileJournal(d)
	j.SyncEvery = time.Hour
	syncs := countingSyncs(j)
	var timed atomic.Int64
	j.OnSync = func(int, time.Duration, error) { timed.Add(1) }
	for i := range 4 {
		if err := j.Append(sLine(i)); err != nil {
			t.Fatal(err)
		}
	}
	if syncs.Load() != 0 || !j.armed || j.pending != 4 {
		t.Fatalf("fsyncs %d armed %v pending %d", syncs.Load(), j.armed, j.pending)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	if syncs.Load() != 1 || j.pending != 0 || j.armed || j.timer != nil || j.f != nil {
		t.Fatalf("fsyncs %d pending %d armed %v", syncs.Load(), j.pending, j.armed)
	}
	j.timedSync()
	if timed.Load() != 0 {
		t.Fatal("a timed fsync after Close")
	}
	if err := j.Append(sLine(4)); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil || syncs.Load() != 2 {
		t.Fatal(err, syncs.Load())
	}
	if got := loadAll(t, d); len(got) != 5 {
		t.Fatalf("%d lines", len(got))
	}
}

func TestJournalFailedSyncTriedAgain(t *testing.T) {
	d := t.TempDir()
	j, _ := NewFileJournal(d)
	j.SyncEvery = time.Hour
	boom := errors.New("disk full")
	var fail atomic.Bool
	fail.Store(true)
	j.fsync = func(f *os.File) error {
		if fail.Load() {
			return boom
		}
		return f.Sync()
	}
	var errs, oks atomic.Int64
	j.OnSync = func(_ int, _ time.Duration, err error) {
		if err != nil {
			errs.Add(1)
		} else {
			oks.Add(1)
		}
	}
	_ = j.Append(sLine(0))
	_ = j.Append(sLine(1))
	j.timedSync()
	if errs.Load() != 1 || j.pending != 2 || !j.armed {
		t.Fatalf("errors %d pending %d armed %v: the lines are not left for the next window", errs.Load(), j.pending, j.armed)
	}
	if err := j.Append(sLine(2)); err == nil || !errors.Is(err, boom) {
		t.Fatalf("%v", err)
	}
	if err := j.Append(sLine(3)); err != nil {
		t.Fatalf("reported twice: %v", err)
	}
	fail.Store(false)
	j.timedSync()
	if oks.Load() != 1 || j.pending != 0 {
		t.Fatalf("oks %d pending %d", oks.Load(), j.pending)
	}
	_ = j.Close()
	if got := loadAll(t, d); len(got) != 4 {
		t.Fatalf("%d lines", len(got))
	}
}

// A mounted module's journal takes the configured window and counts its
// lines and syncs.
func TestMountJournal(t *testing.T) {
	d := t.TempDir()
	c := baseConfig(office)
	c.StateDir, c.JournalSync = d, 5*time.Millisecond
	host := newFakeHost()
	m, err := Mount(host, c, []string{topic}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	fj, ok := m.journal.(*FileJournal)
	if !ok || fj.SyncEvery != 5*time.Millisecond || fj.Path != filepath.Join(d, JournalFile) {
		t.Fatalf("%T %+v", m.journal, m.journal)
	}
	chain := newTestChain(15000)
	tree := fundingTree(t, alice, 1, chain)
	e := mintCarrier(t, tree, 0, alice, envelopeRecord(t, letter{office: office, from: alice, to: bob, box: "inbox", created: u64(now() - 60)}), lockTime(0))
	tell(t, m.LS, atomicOf(t, e), 0)
	deadline := time.Now().Add(2 * time.Second)
	for host.count("bbox_journal_syncs_total", map[string]string{"result": "ok"}) < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if host.count("bbox_journal_lines_total", nil) != 1 || host.count("bbox_journal_syncs_total", map[string]string{"result": "ok"}) != 1 {
		t.Fatal(host.counters)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if got := loadAll(t, d); len(got) != 1 || got[0].K != "c" || got[0].C != cOf(e) {
		t.Fatalf("%+v", got)
	}
}
