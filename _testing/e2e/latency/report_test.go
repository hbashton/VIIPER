package latency

import (
	"math"
	"testing"
)

func TestCalculateUsesNearestRankAndPopulationJitter(t *testing.T) {
	values := []int64{1, 2, 3, 4, 100}
	distribution, err := Calculate(values)
	if err != nil {
		t.Fatal(err)
	}
	if distribution.Count != 5 || distribution.P50NS != 3 || distribution.P90NS != 100 ||
		distribution.P95NS != 100 || distribution.P99NS != 100 || distribution.MaxNS != 100 {
		t.Fatalf("distribution=%+v", distribution)
	}
	if math.Abs(distribution.MeanNS-22) > 0.0001 || distribution.JitterNS <= 0 {
		t.Fatalf("mean/jitter=%f/%f", distribution.MeanNS, distribution.JitterNS)
	}
	if !reflectInt64(values, []int64{1, 2, 3, 4, 100}) {
		t.Fatalf("Calculate reordered source values: %v", values)
	}
}

func TestCalculateRejectsMissingOrNonPositiveSamples(t *testing.T) {
	if _, err := Calculate(nil); err == nil {
		t.Fatal("empty distribution was accepted")
	}
	if _, err := Calculate([]int64{1, 0}); err == nil {
		t.Fatal("zero latency was accepted")
	}
}

func reflectInt64(left, right []int64) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
