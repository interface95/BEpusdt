package conf

import (
	"fmt"
	"sync"
	"testing"
)

func TestStatsIncludeLatestSample(t *testing.T) {
	net := t.Name()
	t.Cleanup(func() { data.Delete(net) })
	if got := GetSuccessRate(net); got != "" {
		t.Fatalf("empty samples must be unknown, got %q", got)
	}
	RecordFailure(net)
	if got := GetStats()[net]; got.Succ != "0.00%" || got.Time != 0 || got.Block != "" {
		t.Fatalf("first failure: %+v", got)
	}
	RecordSuccess(net, "123")
	previous := GetStats()[net]
	if previous.Succ != "50.00%" || previous.Block != "123" || previous.Time == 0 {
		t.Fatalf("latest success missing: %+v", previous)
	}
	RecordFailure(net)
	if got := GetStats()[net]; got.Succ != "33.33%" || got.Time != previous.Time || got.Block != previous.Block {
		t.Fatalf("failure must refresh rate without advancing success time: %+v", got)
	}
}

func TestStatsRollingWindow(t *testing.T) {
	net := t.Name()
	t.Cleanup(func() { data.Delete(net) })
	for i := 0; i < maxRecords; i++ {
		RecordSuccess(net, fmt.Sprint(i))
	}
	for i := 0; i < maxRecords; i++ {
		RecordFailure(net)
	}
	if got := GetStats()[net].Succ; got != "0.00%" {
		t.Fatalf("overwritten successes still counted: %s", got)
	}
	RecordSuccess(net, "2000")
	if got := GetStats()[net].Succ; got != "0.10%" {
		t.Fatalf("latest rolling sample missing: %s", got)
	}
}

func TestStatsConcurrentRecording(t *testing.T) {
	net := t.Name()
	t.Cleanup(func() { data.Delete(net) })
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			RecordSuccess(net, "42")
			RecordFailure(net)
			GetStats()
		}()
	}
	wg.Wait()
	if got := GetStats()[net].Succ; got != "50.00%" {
		t.Fatalf("concurrent sample count: %s", got)
	}
}
