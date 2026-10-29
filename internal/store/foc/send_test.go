package foc

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	pdpclient "github.com/strahe/synapse-go/pdp"

	"github.com/jbenet/ipfs2fil/internal/store"
)

func quickSend(t *testing.T) {
	t.Helper()
	oldEvery, oldBusy, oldRetries := awaitEvery, busyWait, sessionRetries
	awaitEvery, busyWait, sessionRetries = time.Millisecond, 50*time.Millisecond, []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { awaitEvery, busyWait, sessionRetries = oldEvery, oldBusy, oldRetries })
}

var conflict409 = &pdpclient.HTTPError{Method: "PUT", URL: "https://sp/pdp/piece/uploads", StatusCode: 409,
	Body: "This piece is already being uploaded or conflicts with an existing piece"}

func TestSendPieceWaitsOutAConflict(t *testing.T) {
	// The provider is still taking the piece in from a pull: once it has
	// it parked, the upload is done.
	quickSend(t)
	asks := 0
	err := sendPiece(context.Background(), func() error { return conflict409 }, func(context.Context) (bool, error) {
		asks++
		return asks == 3, nil
	})
	if err != nil || asks != 3 {
		t.Fatalf("err = %v after %d asks", err, asks)
	}
}

func TestSendPieceGivesUpOnAConflict(t *testing.T) {
	quickSend(t)
	err := sendPiece(context.Background(), func() error { return conflict409 }, func(context.Context) (bool, error) { return false, nil })
	if err == nil || !strings.Contains(err.Error(), "already taking this piece in") || !strings.Contains(err.Error(), "run push again later") {
		t.Fatalf("err = %v", err)
	}
}

func TestSendPieceRetriesASession(t *testing.T) {
	// Opening the upload session timed out, so nothing was sent: try again.
	quickSend(t)
	timeout := &url.Error{Op: "Post", URL: "https://sp/pdp/piece/uploads", Err: context.DeadlineExceeded}
	sends := 0
	err := sendPiece(context.Background(), func() error {
		if sends++; sends < 3 {
			return timeout
		}
		return nil
	}, nil)
	if err != nil || sends != 3 {
		t.Fatalf("err = %v after %d sends", err, sends)
	}

	sends = 0
	err = sendPiece(context.Background(), func() error { sends++; return timeout }, nil)
	if !errors.Is(err, context.DeadlineExceeded) || sends != 1+len(sessionRetries) {
		t.Fatalf("err = %v after %d sends", err, sends)
	}

	// A failed PUT read the piece, so it is not tried again here.
	sends = 0
	put := &pdpclient.HTTPError{Method: "PUT", URL: "https://sp/pdp/piece/uploads", StatusCode: 500}
	err = sendPiece(context.Background(), func() error { sends++; return put }, nil)
	if err == nil || sends != 1 {
		t.Fatalf("err = %v after %d sends", err, sends)
	}
}

func quickWatch(t *testing.T) {
	t.Helper()
	oldStall, oldPark, oldEvery := stallAfter, parkAfter, watchEvery
	stallAfter, parkAfter, watchEvery = 30*time.Millisecond, 60*time.Millisecond, time.Millisecond
	t.Cleanup(func() { stallAfter, parkAfter, watchEvery = oldStall, oldPark, oldEvery })
}

func TestWatchedEndsAStalledUpload(t *testing.T) {
	// The provider stops taking bytes and the upload never returns on its
	// own: it ends as stalled.
	quickWatch(t)
	err := watched(context.Background(), strings.NewReader("abcdef"), func(ctx context.Context, r io.Reader) error {
		r.Read(make([]byte, 2))
		<-ctx.Done()
		return ctx.Err()
	})
	var stall *store.StalledError
	if !errors.As(err, &stall) || stall.All || stall.Sent != 2 {
		t.Fatalf("err = %v (%+v)", err, stall)
	}

	// Every byte is sent but the provider never finishes taking it in.
	err = watched(context.Background(), strings.NewReader("abcdef"), func(ctx context.Context, r io.Reader) error {
		io.ReadAll(r)
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.As(err, &stall) || !stall.All {
		t.Fatalf("err = %v", err)
	}

	// An upload that keeps moving, however slowly, is left alone.
	err = watched(context.Background(), strings.NewReader("abcdef"), func(ctx context.Context, r io.Reader) error {
		b := make([]byte, 1)
		for {
			time.Sleep(10 * time.Millisecond)
			if _, err := r.Read(b); err == io.EOF {
				return nil
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
}
