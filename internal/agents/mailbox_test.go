package agents

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestMailbox points the state dir at a temp directory and returns a created
// mailbox, so every test runs against its own on-disk store.
func newTestMailbox(t *testing.T, name string) *Mailbox {
	t.Helper()
	t.Setenv(envStateDir, t.TempDir())
	mb, err := OpenMailbox(name)
	if err != nil {
		t.Fatalf("OpenMailbox(%q): %v", name, err)
	}
	if err := mb.Create(); err != nil {
		t.Fatalf("Create: %v", err)
	}
	return mb
}

func appendN(t *testing.T, mb *Mailbox, n int) []Message {
	t.Helper()
	out := make([]Message, 0, n)
	for i := 0; i < n; i++ {
		msg, created, err := mb.Append(Message{ID: fmt.Sprintf("m-%06d", i), From: "tester", Text: fmt.Sprintf("message %d", i)})
		if err != nil || !created {
			t.Fatalf("Append %d: created=%v err=%v", i, created, err)
		}
		out = append(out, msg)
	}
	return out
}

func ids(msgs []Message) string {
	parts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		parts = append(parts, m.ID)
	}
	return strings.Join(parts, ",")
}

func TestValidateMailboxName(t *testing.T) {
	for _, ok := range []string{"codex", "codex-voice", "a", "x.y_z", "mailbox123"} {
		if err := ValidateMailboxName(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Codex", "-codex", "a b", "deadbeef", strings.Repeat("a", 33), "../etc"} {
		if err := ValidateMailboxName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// TestAppendAssignsSequence: sequence numbers are dense and start at 1, and the
// mailbox records who the message is to regardless of what the caller set.
func TestAppendAssignsSequence(t *testing.T) {
	mb := newTestMailbox(t, "codex")
	msgs := appendN(t, mb, 3)
	for i, msg := range msgs {
		if msg.Seq != int64(i+1) {
			t.Errorf("message %d has seq %d", i, msg.Seq)
		}
		if msg.To != "codex" || !msg.Untrusted || msg.At.IsZero() {
			t.Errorf("message %d not normalised: %+v", i, msg)
		}
	}
}

// TestAppendIdempotentByID: the same id twice is one message, and the second
// call returns the stored one so a retrying sender learns its sequence number.
func TestAppendIdempotentByID(t *testing.T) {
	mb := newTestMailbox(t, "codex")
	first, created, err := mb.Append(Message{ID: "m-dup", Text: "once"})
	if err != nil || !created {
		t.Fatalf("first append: created=%v err=%v", created, err)
	}
	again, created, err := mb.Append(Message{ID: "m-dup", Text: "twice"})
	if err != nil || created {
		t.Fatalf("second append: created=%v err=%v", created, err)
	}
	if again.Seq != first.Seq || again.Text != "once" {
		t.Fatalf("duplicate append changed the message: %+v vs %+v", again, first)
	}
	st, _ := mb.Status()
	if st.Total != 1 {
		t.Fatalf("mailbox holds %d messages, want 1", st.Total)
	}
}

func TestAppendRequiresMailboxAndID(t *testing.T) {
	t.Setenv(envStateDir, t.TempDir())
	mb, _ := OpenMailbox("nobody")
	if _, _, err := mb.Append(Message{ID: "m-1", Text: "x"}); err == nil {
		t.Fatal("append to a mailbox that does not exist succeeded")
	}
	if err := mb.Create(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mb.Append(Message{Text: "x"}); err == nil {
		t.Fatal("append without an id succeeded")
	}
}

// TestReadAfterIsPure: an explicit cursor replays and moves nothing.
func TestReadAfterIsPure(t *testing.T) {
	mb := newTestMailbox(t, "codex")
	appendN(t, mb, 5)
	res, err := mb.ReadAfter(2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(res.Messages); got != "m-000002,m-000003,m-000004" {
		t.Fatalf("ReadAfter(2) = %s", got)
	}
	if res.Cursor != "5" || res.Pending != 0 {
		t.Fatalf("cursor=%q pending=%d", res.Cursor, res.Pending)
	}
	again, _ := mb.ReadAfter(2, 0)
	if ids(again.Messages) != ids(res.Messages) {
		t.Fatal("a second ReadAfter with the same cursor returned different messages")
	}
	st, _ := mb.Status()
	if st.DeliveredSeq != 0 || st.ReadSeq != 0 {
		t.Fatalf("ReadAfter moved a watermark: %+v", st)
	}
}

// TestFetchAdvancesAndNeverRepeats: cursor-less reads hand each message out
// once, honour the limit, and report what is still pending.
func TestFetchAdvancesAndNeverRepeats(t *testing.T) {
	mb := newTestMailbox(t, "codex")
	appendN(t, mb, 5)
	first, err := mb.Fetch(2)
	if err != nil {
		t.Fatal(err)
	}
	if ids(first.Messages) != "m-000000,m-000001" || first.Cursor != "2" || first.Pending != 3 {
		t.Fatalf("first fetch: %s cursor=%s pending=%d", ids(first.Messages), first.Cursor, first.Pending)
	}
	second, _ := mb.Fetch(0)
	if ids(second.Messages) != "m-000002,m-000003,m-000004" || second.Pending != 0 {
		t.Fatalf("second fetch: %s pending=%d", ids(second.Messages), second.Pending)
	}
	third, _ := mb.Fetch(0)
	if len(third.Messages) != 0 || third.Cursor != "5" {
		t.Fatalf("third fetch returned %d messages, cursor=%s", len(third.Messages), third.Cursor)
	}
	st, _ := mb.Status()
	if st.Queued != 0 || st.Unread != 5 || st.DeliveredSeq != 5 {
		t.Fatalf("status after fetching everything: %+v", st)
	}
}

// TestAckMarksRead: acks move the read watermark forward only, clamp to the
// log, and imply delivery.
func TestAckMarksRead(t *testing.T) {
	mb := newTestMailbox(t, "codex")
	appendN(t, mb, 3)
	st, err := mb.Ack(2)
	if err != nil {
		t.Fatal(err)
	}
	if st.ReadSeq != 2 || st.DeliveredSeq != 2 || st.Unread != 1 || st.Queued != 1 {
		t.Fatalf("after Ack(2): %+v", st)
	}
	if _, status, _, _ := mb.Find("m-000001"); status != StatusRead {
		t.Fatalf("m-000001 is %s, want read", status)
	}
	if _, status, _, _ := mb.Find("m-000002"); status != StatusQueued {
		t.Fatalf("m-000002 is %s, want queued", status)
	}
	st, _ = mb.Ack(1) // backwards: no-op
	if st.ReadSeq != 2 {
		t.Fatalf("Ack moved backwards: %+v", st)
	}
	st, _ = mb.Ack(99) // past the end: clamped
	if st.ReadSeq != 3 || st.Unread != 0 {
		t.Fatalf("Ack past the end: %+v", st)
	}
}

func TestFindReportsStatus(t *testing.T) {
	mb := newTestMailbox(t, "codex")
	appendN(t, mb, 2)
	if _, _, found, _ := mb.Find("m-missing"); found {
		t.Fatal("found a message that was never stored")
	}
	if _, status, _, _ := mb.Find("m-000000"); status != StatusQueued {
		t.Fatalf("fresh message is %s", status)
	}
	if _, err := mb.Fetch(1); err != nil {
		t.Fatal(err)
	}
	if _, status, _, _ := mb.Find("m-000000"); status != StatusDelivered {
		t.Fatalf("fetched message is %s", status)
	}
}

// TestSurvivesReopen: the store is the disk, so a new handle (a restarted
// server process) sees the same log and watermarks.
func TestSurvivesReopen(t *testing.T) {
	mb := newTestMailbox(t, "codex")
	appendN(t, mb, 3)
	if _, err := mb.Fetch(2); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenMailbox("codex")
	if err != nil {
		t.Fatal(err)
	}
	res, err := reopened.Fetch(0)
	if err != nil {
		t.Fatal(err)
	}
	if ids(res.Messages) != "m-000002" {
		t.Fatalf("after reopen Fetch returned %s", ids(res.Messages))
	}
	msg, created, err := reopened.Append(Message{ID: "m-000000", Text: "retry"})
	if err != nil || created || msg.Seq != 1 {
		t.Fatalf("idempotency lost across reopen: created=%v seq=%d err=%v", created, msg.Seq, err)
	}
}

// TestAppendRepairsTruncatedTail: a writer that died mid-line must not take the
// next message down with it.
func TestAppendRepairsTruncatedTail(t *testing.T) {
	mb := newTestMailbox(t, "codex")
	appendN(t, mb, 1)
	f, err := os.OpenFile(mb.logPath(), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"seq":2,"id":"m-cut","te`); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	msg, created, err := mb.Append(Message{ID: "m-after", Text: "after the crash"})
	if err != nil || !created {
		t.Fatalf("append after a torn line: created=%v err=%v", created, err)
	}
	if msg.Seq != 2 {
		t.Fatalf("seq after a torn line = %d, want 2 (the torn record never existed)", msg.Seq)
	}
	all, err := mb.readAll()
	if err != nil || ids(all) != "m-000000,m-after" {
		t.Fatalf("log after repair: %s err=%v", ids(all), err)
	}
}

// TestWaitReturnsOnArrival: a waiter parked on an empty mailbox wakes when a
// message lands, well before its timeout.
func TestWaitReturnsOnArrival(t *testing.T) {
	mb := newTestMailbox(t, "codex")
	go func() {
		time.Sleep(150 * time.Millisecond)
		_, _, _ = mb.Append(Message{ID: "m-late", Text: "here"})
	}()
	start := time.Now()
	res, err := mb.Wait(context.Background(), WaitOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res.TimedOut || ids(res.Messages) != "m-late" {
		t.Fatalf("Wait returned timed_out=%v messages=%s", res.TimedOut, ids(res.Messages))
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("Wait took %s to notice an arrival", time.Since(start))
	}
}

// TestWaitTimesOutHonestly: a quiet mailbox yields an empty, timed-out result at
// about the requested time, not an error and not early.
func TestWaitTimesOutHonestly(t *testing.T) {
	mb := newTestMailbox(t, "codex")
	start := time.Now()
	res, err := mb.Wait(context.Background(), WaitOptions{Timeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || len(res.Messages) != 0 || res.Cursor != "0" {
		t.Fatalf("quiet wait: %+v", res)
	}
	if el := time.Since(start); el < 250*time.Millisecond || el > 2*time.Second {
		t.Fatalf("quiet wait took %s", el)
	}
}

func TestWaitHonoursContext(t *testing.T) {
	mb := newTestMailbox(t, "codex")
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	res, err := mb.Wait(ctx, WaitOptions{Timeout: 10 * time.Second})
	if err == nil || !res.TimedOut {
		t.Fatalf("cancelled wait: err=%v res=%+v", err, res)
	}
}

// TestWaitWithCursorReplays: an explicit cursor returns what is already there
// after it, immediately, and leaves the watermarks alone.
func TestWaitWithCursorReplays(t *testing.T) {
	mb := newTestMailbox(t, "codex")
	appendN(t, mb, 3)
	cursor := int64(1)
	res, err := mb.Wait(context.Background(), WaitOptions{Cursor: &cursor, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if ids(res.Messages) != "m-000001,m-000002" {
		t.Fatalf("Wait(cursor=1) = %s", ids(res.Messages))
	}
	st, _ := mb.Status()
	if st.DeliveredSeq != 0 {
		t.Fatalf("Wait with a cursor moved the delivered watermark: %+v", st)
	}
}

// TestParallelWaitersShareWithoutDuplicates is the guarantee the cursor-less
// path makes: several readers parked on one mailbox, several writers appending
// concurrently — every message is handed out exactly once.
func TestParallelWaitersShareWithoutDuplicates(t *testing.T) {
	mb := newTestMailbox(t, "codex")
	const readers, writers, perWriter = 4, 3, 10
	total := writers * perWriter

	var (
		mu   sync.Mutex
		seen = map[string]int{}
		wg   sync.WaitGroup
	)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				done := len(seen) >= total
				mu.Unlock()
				if done || ctx.Err() != nil {
					return
				}
				res, err := mb.Wait(ctx, WaitOptions{Limit: 4, Timeout: 500 * time.Millisecond})
				if err != nil {
					return
				}
				mu.Lock()
				for _, m := range res.Messages {
					seen[m.ID]++
				}
				mu.Unlock()
			}
		}()
	}
	var ww sync.WaitGroup
	for w := 0; w < writers; w++ {
		ww.Add(1)
		go func(w int) {
			defer ww.Done()
			for i := 0; i < perWriter; i++ {
				if _, _, err := mb.Append(Message{ID: fmt.Sprintf("m-%d-%d", w, i), Text: "x"}); err != nil {
					t.Errorf("append: %v", err)
				}
				time.Sleep(5 * time.Millisecond)
			}
		}(w)
	}
	ww.Wait()
	wg.Wait()

	if len(seen) != total {
		t.Fatalf("readers saw %d distinct messages, want %d", len(seen), total)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("message %s was handed out %d times", id, n)
		}
	}
	st, _ := mb.Status()
	if st.Total != total || st.Queued != 0 {
		t.Fatalf("final status: %+v", st)
	}
}

// TestConcurrentAppendsKeepSequenceDense: writers from different goroutines
// (standing in for different processes) never share a sequence number.
func TestConcurrentAppendsKeepSequenceDense(t *testing.T) {
	mb := newTestMailbox(t, "codex")
	const n = 40
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, _, err := mb.Append(Message{ID: fmt.Sprintf("m-%d", i), Text: "x"}); err != nil {
				t.Errorf("append %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	all, err := mb.readAll()
	if err != nil {
		t.Fatal(err)
	}
	seqs := map[int64]bool{}
	for _, m := range all {
		if seqs[m.Seq] {
			t.Fatalf("sequence %d assigned twice", m.Seq)
		}
		seqs[m.Seq] = true
	}
	if len(all) != n || !seqs[1] || !seqs[int64(n)] {
		t.Fatalf("got %d messages, seqs 1..%d not dense", len(all), n)
	}
}

func TestListMailboxesAndDefault(t *testing.T) {
	t.Setenv(envStateDir, t.TempDir())
	if list, err := ListMailboxes(); err != nil || len(list) != 0 {
		t.Fatalf("empty state dir: list=%v err=%v", list, err)
	}
	if DefaultClientMailbox() != "" {
		t.Fatal("a default client mailbox exists before any was registered")
	}
	for _, name := range []string{"codex", "voice"} {
		mb, _ := OpenMailbox(name)
		if err := mb.Create(); err != nil {
			t.Fatal(err)
		}
	}
	codex, _ := OpenMailbox("codex")
	appendN(t, codex, 2)
	// A stray directory that is not a valid mailbox name is ignored, not an error.
	root, _ := mailboxesDir()
	if err := os.Mkdir(filepath.Join(root, "Not A Mailbox"), 0o700); err != nil {
		t.Fatal(err)
	}
	list, err := ListMailboxes()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "codex" || list[0].Total != 2 || list[1].Name != "voice" {
		t.Fatalf("ListMailboxes = %+v", list)
	}
	names, _ := MailboxNames()
	if strings.Join(names, ",") != "codex,voice" {
		t.Fatalf("MailboxNames = %v", names)
	}
	if err := SetDefaultClientMailbox("codex"); err != nil {
		t.Fatal(err)
	}
	if DefaultClientMailbox() != "codex" {
		t.Fatalf("default client mailbox = %q", DefaultClientMailbox())
	}
	if err := SetDefaultClientMailbox("Bad Name"); err == nil {
		t.Fatal("an invalid default was accepted")
	}
}

func TestParseCursor(t *testing.T) {
	for in, want := range map[string]int64{"": 0, "0": 0, " 17 ": 17} {
		if got, err := ParseCursor(in); err != nil || got != want {
			t.Errorf("ParseCursor(%q) = %d, %v", in, got, err)
		}
	}
	for _, bad := range []string{"-1", "abc", "1.5"} {
		if _, err := ParseCursor(bad); err == nil {
			t.Errorf("ParseCursor(%q) accepted", bad)
		}
	}
}
