package foc

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	pdpclient "github.com/strahe/synapse-go/pdp"
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
