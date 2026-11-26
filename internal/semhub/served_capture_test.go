package semhub

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	semreg "github.com/Project-Helianthus/helianthus-semreg/semreg/v1"
)

func TestCaptureServedKeepsCommitOutsideTheWholeRead(t *testing.T) {
	h := newTestHub(t)
	const first, second = semreg.AssetID("asset:first"), semreg.AssetID("asset:second")
	// Declare in reverse read order. Give each asset its own owner.
	mustDeclare(t, h, second, "source:second", "served-second", nil)
	mustDeclare(t, h, first, "source:first", "served-first", nil)
	a := mustAdmit(t, h, "source:first", "served-first")
	b := mustAdmit(t, h, "source:second", "served-second")
	mustPublish(t, h, a, first, "served_first", 1, 1)
	mustPublish(t, h, b, second, "served_second", 2, 1)
	before := m11Capture(t, h, first, second)
	tx, err := h.Begin(b, second)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Abort()
	if _, _, err = tx.Apply(testBatch(t, tx, "served_second", 3, 2), testMono(2)); err != nil {
		t.Fatal(err)
	}

	entered := make(chan semreg.AssetID, 1)
	release := make(chan struct{})
	paused := false
	h.faults = func(stage string, id semreg.AssetID) {
		if stage == "capture-read" && !paused {
			paused = true
			entered <- id
			<-release
		}
	}
	type result struct {
		capture CoherentCapture
		err     error
	}
	read := make(chan result, 1)
	clock := func() time.Time { return time.Unix(1800000004, 0) }
	go func() {
		capture, err := h.CaptureServed(context.Background(), clock)
		read <- result{capture, err}
	}()
	var firstRead semreg.AssetID
	select {
	case firstRead = <-entered:
	case r := <-read:
		t.Fatalf("capture returned before reading any asset: %+v", r)
	}
	written := make(chan error, 1)
	go func() {
		outcome, err := tx.Commit(nil)
		if err == nil && outcome != Accepted {
			err = errors.New("publication was not accepted")
		}
		written <- err
	}()
	// Wait for the writer to queue while the reader is paused between assets.
	// TryRLock fails when a writer is waiting; a per-asset unlock would then
	// let the writer publish before the second asset's read lock is taken.
	deadline := time.Now().Add(5 * time.Second)
	queued := false
	for time.Now().Before(deadline) {
		if !h.mu.TryRLock() {
			queued = true
			break
		}
		h.mu.RUnlock()
		runtime.Gosched()
	}
	close(release)
	r := <-read
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	h.faults = nil
	if !queued || firstRead != first || r.err != nil || len(r.capture.Assets) != 2 {
		t.Fatalf("queued=%v first=%s count=%d err=%v", queued, firstRead, len(r.capture.Assets), r.err)
	}
	for i, item := range r.capture.Assets {
		want := before.Assets[i]
		if item.AssetID != want.AssetID || item.Snapshot == nil || item.Evaluation == nil || item.Snapshot.SnapshotID != want.Snapshot.SnapshotID {
			t.Fatalf("asset %s did not retain its pre-commit snapshot and evaluation", want.AssetID)
		}
	}
	next, err := h.CaptureServed(context.Background(), clock)
	if err != nil || len(next.Assets) != 2 || next.Assets[1].Snapshot == nil || next.Assets[1].Snapshot.SnapshotID == before.Assets[1].Snapshot.SnapshotID {
		t.Fatalf("read after commit missed publication: %+v, %v", next, err)
	}
}

func TestCaptureServedReturnsDeclaredOwnerEvenWhenAnotherSourcePublishes(t *testing.T) {
	h := newTestHub(t)
	mustDeclareShared(t, h)
	peer := mustAdmit(t, h, sharedPeer, peerDriver)
	mustPublish(t, h, peer, sharedAsset, "served_peer", 11, 1)
	const unpublished = semreg.AssetID("asset:unpublished")
	const unpublishedOwner = semreg.SourceID("source:unpublished")
	mustDeclare(t, h, unpublished, unpublishedOwner, "served-unpublished", nil)
	capture, err := h.CaptureServed(context.Background(), func() time.Time { return time.Unix(1800000004, 0) })
	if err != nil || len(capture.Assets) != 2 {
		t.Fatalf("capture=%+v, err=%v", capture, err)
	}
	if asset := capture.Assets[0]; asset.AssetID != sharedAsset || asset.Owner != sharedOwner || !asset.PublicAdmitted || asset.Snapshot == nil || asset.Evaluation == nil {
		t.Fatalf("peer publication must retain its declared owner: %+v", asset)
	}
	if asset := capture.Assets[1]; asset.AssetID != unpublished || asset.Owner != unpublishedOwner || asset.PublicAdmitted || asset.Snapshot != nil || asset.Evaluation != nil || asset.UnavailableReason != "semantic.read.reason.source_unavailable" {
		t.Fatalf("unavailable declaration must keep its owner but no publication: %+v", asset)
	}
}
