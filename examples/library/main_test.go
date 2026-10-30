package main

import (
	"io"
	"testing"
)

func TestReadinessAcrossWorkerWrites(t *testing.T) {
	writer := &readiness{Writer: io.Discard, ready: make(chan struct{})}
	for _, part := range []string{"build output\nlibrary-", "worker-", "ready"} {
		if _, err := writer.Write([]byte(part)); err != nil {
			t.Fatal(err)
		}
		select {
		case <-writer.ready:
			t.Fatal("readiness fired before complete marker")
		default:
		}
	}
	if _, err := writer.Write([]byte("\nmore output\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-writer.ready:
	default:
		t.Fatal("readiness did not fire")
	}
	// Subsequent identical output must not close the channel again.
	if _, err := writer.Write([]byte("library-worker-ready\n")); err != nil {
		t.Fatal(err)
	}
}
