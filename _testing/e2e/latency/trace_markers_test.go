package latency

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestTraceMarkerEvidenceRejectsMissingDuplicateTruncatedAndForged(t *testing.T) {
	suite := validSuite(t)
	markers := traceMarkersFromSuite(suite)
	if err := VerifyTraceMarkers(suite, markers); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		mutate func([]TraceMarker) []TraceMarker
		want   string
	}{
		{"missing", func(in []TraceMarker) []TraceMarker { return in[:len(in)-1] }, "JSON samples"},
		{"duplicate", func(in []TraceMarker) []TraceMarker { return append(in, in[0]) }, "duplicate marker"},
		{"reordered", func(in []TraceMarker) []TraceMarker { in[0], in[1] = in[1], in[0]; return in }, "order"},
		{"forged payload", func(in []TraceMarker) []TraceMarker { in[0].EndQPCTicks++; return in }, "payload"},
		{"unknown", func(in []TraceMarker) []TraceMarker { in[0].MarkerID = "unknown"; return in }, "unknown"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutated := test.mutate(append([]TraceMarker(nil), markers...))
			if err := VerifyTraceMarkers(suite, mutated); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("marker error=%v", err)
			}
		})
	}

	encoded, err := json.Marshal(markers)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ParseTraceMarkers(bytes.NewReader(encoded[:len(encoded)-1])); err == nil {
		t.Fatal("truncated marker JSON was accepted")
	}
	if _, err = ParseTraceMarkers(bytes.NewReader(append(encoded, []byte(` {}`)...))); err == nil ||
		!strings.Contains(err.Error(), "trailing JSON") {
		t.Fatalf("trailing marker JSON error=%v", err)
	}
}

func traceMarkersFromSuite(suite *SuiteReport) []TraceMarker {
	var markers []TraceMarker
	for _, controllerCase := range suite.Cases {
		for _, run := range controllerCase.Runs {
			for _, sample := range run.Samples {
				markers = append(markers, TraceMarker{
					MarkerID: sample.MarkerID, Controller: controllerCase.Workload.ControllerType,
					Transport: run.Transport, TransportBlock: run.TransportBlock,
					Sequence: sample.Sequence, Transition: string(sample.Transition),
					StartQPCTicks: sample.StartQPCTicks,
					EndQPCTicks:   sample.EndQPCTicks, MarkerQPCTicks: sample.MarkerQPCTicks,
					LatencyNS:           sample.LatencyNS,
					EventTimestampNS:    sample.EventTimestampNS,
					SDLFenceTimestampNS: sample.SDLFenceTimestampNS,
				})
			}
		}
	}
	return markers
}
