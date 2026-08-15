package latency

import "testing"

func TestCausalEdgeFenceRejectsDwellDrainAndPreFenceEvents(t *testing.T) {
	counters := Counters{}
	last, err := RejectPreWriteEdge(100, 101, true, &counters)
	if err == nil || last != 101 || counters.Press != 1 {
		t.Fatalf("queued press was not rejected/accounted: last=%d counters=%+v error=%v", last, counters, err)
	}
	last, err = RejectPreWriteEdge(last, 102, false, &counters)
	if err == nil || last != 102 || counters.Release != 1 {
		t.Fatalf("dwell release was not rejected/accounted: last=%d counters=%+v error=%v", last, counters, err)
	}
	if err = ValidatePostWriteTimestamp(last, 200, 199); err == nil {
		t.Fatal("an event older than the SDL pre-write fence was accepted")
	}
	if err = ValidatePostWriteTimestamp(last, 200, 200); err == nil {
		t.Fatal("an event sharing the pre-write fence tick was accepted")
	}
	if err = ValidatePostWriteTimestamp(last, 200, 201); err != nil {
		t.Fatalf("an event after the causal admission fence was rejected: %v", err)
	}
}

func TestCausalEdgeFenceDoesNotCountInvalidTimestamp(t *testing.T) {
	counters := Counters{}
	last, err := RejectPreWriteEdge(100, 99, true, &counters)
	if err == nil || last != 100 || counters.Total() != 0 {
		t.Fatalf("regressed event corrupted counters: last=%d counters=%+v error=%v", last, counters, err)
	}
}
