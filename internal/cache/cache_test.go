package cache

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestFetchCachesAndExpires(t *testing.T) {
	c := New(t.TempDir(), time.Minute, true)
	now := time.Now()
	c.now = func() time.Time { return now }
	calls := 0
	fetch := func(context.Context) ([]string, error) { calls++; return []string{"a", "b"}, nil }
	key := Key("gh", "repos")
	for i := 0; i < 3; i++ {
		v, err := Fetch(context.Background(), c, key, fetch)
		if err != nil || len(v) != 2 {
			t.Fatalf("Fetch = %v, %v", v, err)
		}
	}
	if calls != 1 {
		t.Fatalf("fetch called %d times, want 1", calls)
	}
	now = now.Add(2 * time.Minute)
	if _, err := Fetch(context.Background(), c, key, fetch); err != nil || calls != 2 {
		t.Fatalf("expired entry not refetched: calls=%d err=%v", calls, err)
	}
	if err := c.Invalidate(); err != nil {
		t.Fatal(err)
	}
	if c.Load(key, new([]string)) {
		t.Fatal("entry survived Invalidate")
	}
}

func TestDisabledCacheAlwaysFetches(t *testing.T) {
	c := New(t.TempDir(), time.Minute, false)
	calls := 0
	for i := 0; i < 2; i++ {
		_, _ = Fetch(context.Background(), c, "k", func(context.Context) (int, error) { calls++; return 1, nil })
	}
	if calls != 2 {
		t.Fatalf("calls = %d", calls)
	}
}

func TestErrorsNotCached(t *testing.T) {
	c := New(t.TempDir(), time.Minute, true)
	boom := errors.New("boom")
	if _, err := Fetch(context.Background(), c, "k", func(context.Context) (int, error) { return 0, boom }); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if c.Load("k", new(int)) {
		t.Fatal("error result was cached")
	}
}
