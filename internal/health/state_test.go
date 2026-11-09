package health

import "testing"

// Healthy backend is evicted after `Fall` failed probes in a row
func TestState_ProbeFailuresEvictHealthyBackend(t *testing.T) {
	var s State // zero value is Healthy
	const rise, fall = 2, 3

	for i, ok := range []bool{false, false, true, false, false} {
		s.ProbeResult(ok, rise, fall)
		if !s.Admits() {
			t.Fatalf("evicted after probe %d; want still healthy (never %d failures in a row)", i+1, fall)
		}
	}
	s.ProbeResult(false, rise, fall) // 3rd failure in a row
	if s.Phase() != Evicted {
		t.Fatalf("phase = %v after %d failures in a row, want evicted", s.Phase(), fall)
	}
}

// With fall = 0, probes never touch a Healthy backend (they only readmit)
func TestState_ProbeFallZeroNeverEvicts(t *testing.T) {
	var s State
	for i := 0; i < 10; i++ {
		s.ProbeResult(false, 2, 0)
	}
	if !s.Admits() {
		t.Fatalf("phase = %v, want healthy when active.fall is 0", s.Phase())
	}
}
