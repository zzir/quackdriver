package quackdriver

import (
	"sync"
	"testing"
)

// TestConnClosedRace exercises the Conn.closed flag from concurrent readers
// and writers. Run with `go test -race ./...` to catch synchronization
// regressions. The flag used to be a plain bool guarded by sync.Mutex on
// write but read without locking — Go's race detector flagged the
// inconsistency.
func TestConnClosedRace(t *testing.T) {
	c := &Conn{}
	const N = 100
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < N; i++ {
			_ = c.IsValid()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < N; i++ {
			_ = c.ResetSession(nil)
		}
	}()
	go func() {
		defer wg.Done()
		c.closed.Store(true)
	}()
	wg.Wait()
	if !c.closed.Load() {
		t.Fatal("expected closed=true after Store")
	}
}
