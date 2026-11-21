package taxapi

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRollupWindows(t *testing.T) {
	d := func(day, h int) time.Time { return time.Date(2026, 1, day, h, 0, 0, 0, time.UTC) }
	cases := []struct {
		start, end, from, to time.Time
		partial              int
	}{
		{d(1, 0), d(3, 0), d(1, 0), d(3, 0), 0},
		{time.Time{}, d(3, 0), time.Time{}, d(3, 0), 0},
		{d(1, 6), d(3, 0), d(2, 0), d(3, 0), 1},
		{d(1, 0), d(3, 6), d(1, 0), d(3, 0), 1},
		{d(1, 6), d(3, 6), d(2, 0), d(3, 0), 2},
		{d(1, 6), d(1, 9), d(2, 0), d(2, 0), 1},
		{d(1, 6), d(2, 9), d(2, 0), d(2, 0), 2},
		{d(2, 0), d(2, 0), d(2, 0), d(2, 0), 0},
	}
	for _, c := range cases {
		from, to, partial := rollupWindows(c.start, c.end)
		if !from.Equal(c.from) || !to.Equal(c.to) || len(partial) != c.partial {
			t.Fatalf("%s..%s: got %s..%s with %d partial, want %s..%s with %d", c.start, c.end, from, to, len(partial), c.from, c.to, c.partial)
		}
		var covered time.Duration
		if to.After(from) {
			covered += to.Sub(from)
		}
		for _, w := range partial {
			covered += w[1].Sub(w[0])
			if !utcFloorDay(w[0]).Equal(utcFloorDay(w[1].Add(-time.Nanosecond))) {
				t.Fatalf("partial window %v spans two days", w)
			}
		}
		if c.start.IsZero() {
			continue
		}
		if covered != c.end.Sub(c.start) {
			t.Fatalf("%s..%s: windows cover %s", c.start, c.end, covered)
		}
	}
}

func TestStatementScanLine(t *testing.T) {
	st := &CoverageStatement{}
	if got := statementScanLine(st); got != "Transactions scanned: not counted when this was generated." {
		t.Fatal(got)
	}
	tx, msgs := int64(95123456), int64(137000001)
	st.TransactionsScanned, st.MessagesScanned = &tx, &msgs
	if got := statementScanLine(st); got != "Transactions scanned: 95,123,456 cosmoshub-4 transactions (137,000,001 messages) indexed in the period." {
		t.Fatal(got)
	}
	for n, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", -1234567: "-1,234,567"} {
		if got := groupDigits(n); got != want {
			t.Fatalf("%d: %s", n, got)
		}
	}
	s := &Server{}
	p := s.periodCoverage(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))
	if !strings.Contains(p.Statement.Text, "Transactions scanned: not counted") || p.Statement.TransactionsScanned != nil {
		t.Fatalf("statement without a rollup:\n%s", p.Statement.Text)
	}
}

func TestFlightGroupSharesOneCall(t *testing.T) {
	var g flightGroup
	var calls atomic.Int32
	release := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := g.do(context.Background(), "k", func() (any, error) {
				calls.Add(1)
				<-release
				return 7, nil
			})
			if err != nil || v.(int) != 7 {
				t.Errorf("got %v %v", v, err)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("%d calls", calls.Load())
	}
	// A waiter that leaves does not cancel the call.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_, err := g.do(ctx, "k2", func() (any, error) { <-done; return 1, nil })
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err %v", err)
		}
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	go func() { time.Sleep(20 * time.Millisecond); close(done) }()
	v, err := g.do(context.Background(), "k2", func() (any, error) { return 2, nil })
	if err != nil || (v.(int) != 1 && v.(int) != 2) {
		t.Fatalf("got %v %v", v, err)
	}
}
