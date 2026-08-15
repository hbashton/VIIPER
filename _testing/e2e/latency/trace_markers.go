package latency

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

type TraceMarker struct {
	MarkerID            string `json:"trace_marker_id"`
	Controller          string `json:"controller"`
	Transport           string `json:"transport"`
	TransportBlock      int    `json:"transport_block"`
	Sequence            int    `json:"sequence"`
	Transition          string `json:"transition"`
	StartQPCTicks       int64  `json:"start_qpc_ticks"`
	EndQPCTicks         int64  `json:"end_qpc_ticks"`
	MarkerQPCTicks      int64  `json:"trace_marker_qpc_ticks"`
	LatencyNS           int64  `json:"latency_ns"`
	EventTimestampNS    uint64 `json:"sdl_event_timestamp_ns"`
	SDLFenceTimestampNS uint64 `json:"sdl_prewrite_fence_timestamp_ns"`
}

func ParseTraceMarkers(reader io.Reader) ([]TraceMarker, error) {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	var markers []TraceMarker
	if err := decoder.Decode(&markers); err != nil {
		return nil, fmt.Errorf("decode ETL marker evidence: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("ETL marker evidence contains trailing JSON")
		}
		return nil, fmt.Errorf("decode trailing ETL marker evidence: %w", err)
	}
	return markers, nil
}

// VerifyTraceMarkers requires an exact, chronological, one-to-one copy of every
// finalized JSON sample in the decoded sequential ETL marker stream. The
// decoder supplies events in oldest-first ETL order; accepting set equality
// would hide event reordering and weaken the scheduling evidence.
func VerifyTraceMarkers(suite *SuiteReport, observed []TraceMarker) error {
	if suite == nil {
		return errors.New("nil latency suite")
	}
	var expected []TraceMarker
	expectedByID := make(map[string]TraceMarker)
	for _, controllerCase := range suite.Cases {
		for _, run := range controllerCase.Runs {
			for _, sample := range run.Samples {
				marker := TraceMarker{
					MarkerID: sample.MarkerID, Controller: controllerCase.Workload.ControllerType,
					Transport: run.Transport, TransportBlock: run.TransportBlock,
					Sequence: sample.Sequence, Transition: string(sample.Transition),
					StartQPCTicks: sample.StartQPCTicks,
					EndQPCTicks:   sample.EndQPCTicks, MarkerQPCTicks: sample.MarkerQPCTicks,
					LatencyNS:           sample.LatencyNS,
					EventTimestampNS:    sample.EventTimestampNS,
					SDLFenceTimestampNS: sample.SDLFenceTimestampNS,
				}
				if marker.MarkerID == "" {
					return errors.New("latency JSON contains an absent trace marker identity")
				}
				if _, duplicate := expectedByID[marker.MarkerID]; duplicate {
					return fmt.Errorf("latency JSON contains duplicate marker %q", marker.MarkerID)
				}
				expectedByID[marker.MarkerID] = marker
				expected = append(expected, marker)
			}
		}
	}
	seen := make(map[string]struct{}, len(observed))
	for index, marker := range observed {
		_, exists := expectedByID[marker.MarkerID]
		if !exists {
			return fmt.Errorf("ETL marker %d has unknown or absent identity %q", index, marker.MarkerID)
		}
		if _, duplicate := seen[marker.MarkerID]; duplicate {
			return fmt.Errorf("ETL contains duplicate marker %q", marker.MarkerID)
		}
		seen[marker.MarkerID] = struct{}{}
	}
	if len(seen) != len(expected) {
		return fmt.Errorf("ETL contains %d exact markers for %d JSON samples", len(seen), len(expected))
	}
	for index, marker := range observed {
		want := expected[index]
		if marker.MarkerID != want.MarkerID {
			return fmt.Errorf("ETL marker order differs at index %d: got %q, want %q", index, marker.MarkerID, want.MarkerID)
		}
		if marker != want {
			return fmt.Errorf("ETL marker %q payload does not match its JSON sample", marker.MarkerID)
		}
	}
	return nil
}
