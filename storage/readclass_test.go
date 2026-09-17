package storage

import (
	"testing"
	"time"

	"github.com/anacrolix/torrent"
)

// The whole point of ReadClass is that a demux read can pipeline (so a wanted window actually
// downloads) yet can never outbid a live player. Encode that as an invariant on the policy table.
func TestReadClassPolicyOrdering(t *testing.T) {
	pb := ClassPlayback.policy()
	ad := ClassAheadDemux.policy()

	if pb.curPrio != torrent.PiecePriorityNow {
		t.Errorf("playback curPrio = %v, want Now", pb.curPrio)
	}
	// AheadDemux must never OUTRANK playback (so it can't outbid a live player), yet must REQUEST its
	// window (> None) so a wanted window actually downloads — the old code left it at None (unfetched).
	if ad.aheadPrio > pb.aheadPrio {
		t.Errorf("ahead aheadPrio %v must not exceed playback aheadPrio %v", ad.aheadPrio, pb.aheadPrio)
	}
	if ad.aheadPrio <= torrent.PiecePriorityNone {
		t.Errorf("ahead aheadPrio = %v, want > None so the window is actually requested", ad.aheadPrio)
	}
	if ad.curPrio >= pb.curPrio {
		t.Errorf("ahead curPrio %v must rank below playback curPrio %v", ad.curPrio, pb.curPrio)
	}
	if ad.aheadPieces <= 0 {
		t.Errorf("ahead must have a bounded read-ahead > 0, got %d", ad.aheadPieces)
	}
	if got := ClassColdProbe.policy().aheadPieces; got != 0 {
		t.Errorf("cold probe read-ahead = %d, want 0", got)
	}
	if !ClassColdProbe.policy().headFootBoost {
		t.Errorf("cold probe must boost head/foot (it seeks to moov/Cues)")
	}
}

// R4: a reader parked in WaitRange must be woken by the write that satisfies its range. Under the old
// swap-notifyCh-then-recheck code a wakeup could be lost; WaitRange captures the channel atomically
// with the readiness check so it can't.
func TestMemPieceNoLostWakeup(t *testing.T) {
	mp := NewMemPiece(16384)
	woke := make(chan struct{})

	go func() {
		for {
			ready, ch := mp.WaitRange(0, 100)
			if ready {
				close(woke)
				return
			}
			<-ch
		}
	}()

	time.Sleep(20 * time.Millisecond) // let the waiter park on the channel
	if _, err := mp.WriteAt(make([]byte, 100), 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	select {
	case <-woke:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter never woke after WriteAt satisfied the range (lost wakeup)")
	}
}

// The eviction path must subtract from Cache.filled exactly what a piece contributed, so accounting
// can't drift (MarkNotComplete used to subtract nothing; eviction used len(data)=full piece size).
func TestMemPieceReleaseAccounting(t *testing.T) {
	mp := NewMemPiece(16384)
	if n, _ := mp.WriteAt(make([]byte, 500), 0); n != 500 {
		t.Fatalf("WriteAt n=%d, want 500", n)
	}
	if got := mp.ReleaseAndSize(); got != 500 {
		t.Errorf("ReleaseAndSize = %d, want 500 (bytes written)", got)
	}
	if got := mp.ReleaseAndSize(); got != 0 {
		t.Errorf("second ReleaseAndSize = %d, want 0 (already released)", got)
	}
}
