package avito

import (
	"sync"
	"testing"
)

// A room's two loops both move the document version: readLoop when a peer
// sends an op, sendLoop when our own op is acked. They did it without a
// lock, which -race caught now and then in the striped tests
// (TestStripedStreamSurvivesDroppedSocket). Under -race this fails on the
// old code and passes now.
func TestVersionIsSafeAcrossBothLoops(t *testing.T) {
	w := &avitoWS{stats: &channelStats{}}
	w.setVersion(1)
	const rounds = 2000
	var wg sync.WaitGroup
	wg.Add(2)
	// readLoop: a peer's ops.
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			// With an op: without one handlePeerOp returns before it
			// ever looks at the version.
			w.handlePeerOp(map[string]interface{}{
				"op": []interface{}{"QUJD"}, "v": float64(i),
			})
		}
	}()
	// sendLoop: reading it for an op, then the ack.
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			_ = w.version()
			if i%2 == 0 {
				w.setVersion(i + 1)
			} else {
				w.bumpVersion()
			}
		}
	}()
	wg.Wait()
	if got := w.version(); got <= 0 {
		t.Fatalf("version %d", got)
	}
	// raiseVersion never moves it back.
	w.setVersion(100)
	w.raiseVersion(50)
	if got := w.version(); got != 100 {
		t.Errorf("raiseVersion(50) over 100 gave %d", got)
	}
	w.raiseVersion(101)
	if got := w.version(); got != 101 {
		t.Errorf("raiseVersion(101) gave %d", got)
	}
}
