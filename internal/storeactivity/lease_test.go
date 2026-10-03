package storeactivity

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestExclusiveLeaseWaitsForActiveSharedLease(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	shared, err := AcquireShared(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}

	deadline, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	if _, err := AcquireExclusive(deadline, root); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("exclusive lease while store active = %v, want deadline", err)
	}
	if err := shared.Close(); err != nil {
		t.Fatal(err)
	}
	exclusive, err := AcquireExclusive(context.Background(), root)
	if err != nil {
		t.Fatalf("exclusive lease after activity ended: %v", err)
	}
	if err := exclusive.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLeaseCanonicalizesDeduplicatesAndOrdersRoots(t *testing.T) {
	base := t.TempDir()
	first := filepath.Join(base, "a")
	second := filepath.Join(base, "b")
	lease, err := AcquireShared(context.Background(), second, first, first)
	if err != nil {
		t.Fatal(err)
	}
	if len(lease.locks) != 2 {
		t.Fatalf("lease locks = %d, want 2", len(lease.locks))
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestContextWithExclusiveLeaseReusesNestedAcquisitions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	exclusive, err := AcquireExclusive(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := exclusive.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	ctx := ContextWithLease(context.Background(), exclusive)
	for _, acquire := range []func(context.Context, ...string) (*Lease, error){AcquireShared, AcquireExclusive} {
		nested, err := acquire(ctx, root)
		if err != nil {
			t.Fatalf("reuse nested acquisition: %v", err)
		}
		if err := nested.Close(); err != nil {
			t.Fatal(err)
		}
	}
	deadline, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	if _, err := AcquireShared(deadline, root); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("independent lease while reused lease remains active = %v, want deadline", err)
	}
}

func TestContextWithSharedLeaseDoesNotPermitUpgrade(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	shared, err := AcquireShared(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := shared.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	ctx, cancel := context.WithTimeout(ContextWithLease(context.Background(), shared), 75*time.Millisecond)
	defer cancel()
	if _, err := AcquireExclusive(ctx, root); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("exclusive upgrade through shared lease = %v, want deadline", err)
	}
}
