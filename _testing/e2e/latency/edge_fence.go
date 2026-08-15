package latency

import "errors"

// RejectPreWriteEdge accounts for an exact SDL button edge observed during
// dwell or final queue drain and always rejects it as non-causal to the next
// input write.
func RejectPreWriteEdge(lastTimestamp, eventTimestamp uint64, down bool, counters *Counters) (uint64, error) {
	if eventTimestamp == 0 || (lastTimestamp != 0 && eventTimestamp < lastTimestamp) {
		return lastTimestamp, errors.New("SDL pre-write event timestamp was absent or regressed")
	}
	if counters != nil {
		if down {
			counters.Press++
		} else {
			counters.Release++
		}
	}
	return eventTimestamp, errors.New("SDL button edge preceded the input write")
}

// ValidatePostWriteTimestamp proves an observed SDL event was generated no
// earlier than the SDL clock fence captured before WriteBinary.
func ValidatePostWriteTimestamp(lastTimestamp, fenceTimestamp, eventTimestamp uint64) error {
	if fenceTimestamp == 0 || eventTimestamp == 0 {
		return errors.New("SDL event or pre-write fence timestamp is absent")
	}
	if lastTimestamp != 0 && eventTimestamp < lastTimestamp {
		return errors.New("SDL event clock regressed")
	}
	if eventTimestamp <= fenceTimestamp {
		return errors.New("SDL event did not follow the input write fence")
	}
	return nil
}
