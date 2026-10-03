package buildah

import "testing"

func TestGraphParallelismZeroUsesAllStages(t *testing.T) {
	if got := graphParallelism(0, 7); got != 7 {
		t.Fatalf("parallelism = %d, want 7", got)
	}
	if got := graphParallelism(3, 7); got != 3 {
		t.Fatalf("bounded parallelism = %d, want 3", got)
	}
}
