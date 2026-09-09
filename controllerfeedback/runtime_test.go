package controllerfeedback

import (
	"sync"
	"testing"
)

func TestRuntimeTypedPublicationRejectsStaleAndPostStopResurrection(t *testing.T) {
	var runtime Runtime
	if runtime.Publish(Publication{}) {
		t.Fatal("invalid publication was accepted")
	}
	first := runtimePublication(t, PublicationOriginNativeGame, runtimeFrame(1))
	if !runtime.Publish(first) || runtime.Publish(first) {
		t.Fatal("publication ordering watermark failed")
	}
	differentSource := runtimeFrame(2)
	differentSource.Source = SourceXbox360VirtualDevice
	if runtime.Publish(runtimePublication(t, PublicationOriginNativeGame,
		differentSource)) {
		t.Fatal("source changed inside one ownership epoch")
	}
	stop := runtimeStop(runtimeFrame(2))
	if !runtime.Publish(runtimePublication(t, PublicationOriginNativeGame, stop)) {
		t.Fatal("stop publication failed")
	}
	if runtime.Publish(runtimePublication(t, PublicationOriginNativeGame,
		runtimeFrame(3))) {
		t.Fatal("stopped epoch was resurrected by sequence alone")
	}
	successor := runtimeFrame(1)
	successor.OwnershipEpoch = 2
	if !runtime.Publish(runtimePublication(t, PublicationOriginNativeGame,
		successor)) {
		t.Fatal("successor ownership epoch was rejected")
	}
}

func TestRuntimeArbitrationUsesFixedPriorityAndStopsBeforeFallback(t *testing.T) {
	var runtime Runtime
	publications := []struct {
		origin PublicationOrigin
		value  uint16
	}{
		{PublicationOriginProfileEffect, 100},
		{PublicationOriginAudioHaptics, 200},
		{PublicationOriginNativeGame, 300},
		{PublicationOriginTestPreview, 400},
	}
	for _, item := range publications {
		frame := runtimeFrame(1)
		frame.BodyLow = item.value
		if !runtime.Publish(runtimePublication(t, item.origin, frame)) {
			t.Fatalf("publish origin=%d failed", item.origin)
		}
	}
	writer, ok := runtime.AcquireWriter(1, 1)
	if !ok {
		t.Fatal("writer acquisition failed")
	}
	preview := runtimeDeliverNext(t, &runtime, writer, 1_000)
	if preview.Disposition != DeliveryFrame ||
		preview.Origin != PublicationOriginTestPreview ||
		preview.Frame.BodyLow != 400 {
		t.Fatalf("preview winner=%+v", preview)
	}

	stop := runtimeStop(runtimeFrame(2))
	stop.TimestampMicroseconds = 1_001
	if !runtime.Publish(runtimePublication(t, PublicationOriginTestPreview, stop)) {
		t.Fatal("preview stop publish failed")
	}
	firstStop, firstToken := runtimeClaimAndAdmit(t, &runtime, writer, 1_001)
	if firstStop.Disposition != DeliveryStop ||
		firstStop.DeliveryEpoch != preview.DeliveryEpoch {
		t.Fatalf("first stop=%+v preview=%+v", firstStop, preview)
	}
	if !runtime.Complete(writer, firstToken, false, 1_001) {
		t.Fatal("failed stop completion failed")
	}
	retryStop, retryToken := runtimeClaimAndAdmit(t, &runtime, writer, 1_001)
	if retryStop != firstStop || retryToken == firstToken {
		t.Fatalf("stop retry=%+v token=%d first=%+v token=%d",
			retryStop, retryToken, firstStop, firstToken)
	}
	if !runtime.Complete(writer, retryToken, true, 1_001) {
		t.Fatal("successful stop completion failed")
	}
	game := runtimeDeliverNext(t, &runtime, writer, 1_001)
	if game.Origin != PublicationOriginNativeGame || game.Frame.BodyLow != 300 ||
		game.DeliveryEpoch == preview.DeliveryEpoch {
		t.Fatalf("game fallback=%+v", game)
	}
}

func TestRuntimeExpiryProducesOneRetryableStopPerAdmittedEpoch(t *testing.T) {
	var runtime Runtime
	frame := runtimeFrame(1)
	frame.TimeToLiveMicroseconds = 50
	if !runtime.Publish(runtimePublication(t,
		PublicationOriginProfileEffect, frame)) {
		t.Fatal("publish failed")
	}
	writer, ok := runtime.AcquireWriter(1, 1)
	if !ok {
		t.Fatal("writer acquisition failed")
	}
	applied := runtimeDeliverNext(t, &runtime, writer, 1_049)
	stop, stopToken := runtimeClaimAndAdmit(t, &runtime, writer, 1_050)
	if stop.Disposition != DeliveryStop ||
		stop.DeliveryEpoch != applied.DeliveryEpoch {
		t.Fatalf("expiry stop=%+v applied=%+v", stop, applied)
	}
	if !runtime.Complete(writer, stopToken, false, 1_050) {
		t.Fatal("failed stop completion failed")
	}
	retry, retryToken := runtimeClaimAndAdmit(t, &runtime, writer, 1_050)
	if retry != stop {
		t.Fatalf("stop retry changed: %+v != %+v", retry, stop)
	}
	if !runtime.Complete(writer, retryToken, true, 1_050) {
		t.Fatal("stop retry completion failed")
	}
	if delivery, disposition, token := runtime.Claim(2_000, writer); disposition != DeliveryNone || delivery != (Delivery{}) || token != 0 {
		t.Fatal("completed stop was emitted again")
	}
}

func TestRuntimeSupersededUnadmittedFrameCannotReachFinalAdmission(t *testing.T) {
	var runtime Runtime
	profile := runtimeFrame(1)
	profile.BodyLow = 100
	if !runtime.Publish(runtimePublication(t,
		PublicationOriginProfileEffect, profile)) {
		t.Fatal("profile publish failed")
	}
	writer, ok := runtime.AcquireWriter(1, 1)
	if !ok {
		t.Fatal("writer acquisition failed")
	}

	claimed := make(chan struct{})
	published := make(chan struct{})
	var old Delivery
	var oldDisposition DeliveryDisposition
	var oldToken uint64
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		old, oldDisposition, oldToken = runtime.Claim(1_000, writer)
		close(claimed)
		<-published
	}()
	go func() {
		defer wait.Done()
		<-claimed
		preview := runtimeFrame(1)
		preview.BodyLow = 900
		if !runtime.Publish(runtimePublication(t,
			PublicationOriginTestPreview, preview)) {
			t.Error("preview publish failed")
		}
		close(published)
	}()
	wait.Wait()
	if oldDisposition != DeliveryFrame ||
		old.Origin != PublicationOriginProfileEffect || oldToken == 0 {
		t.Fatalf("old claim=%+v disposition=%d token=%d",
			old, oldDisposition, oldToken)
	}
	if runtime.Admit(writer, oldToken, 1_000) {
		t.Fatal("superseded frame reached final admission")
	}
	if !runtime.Complete(writer, oldToken, false, 1_000) {
		t.Fatal("superseded claim cancellation failed")
	}
	preview := runtimeDeliverNext(t, &runtime, writer, 1_000)
	if preview.Disposition != DeliveryFrame ||
		preview.Origin != PublicationOriginTestPreview ||
		preview.Frame.BodyLow != 900 {
		t.Fatalf("direct preview replacement=%+v", preview)
	}
}

func TestRuntimeWriterLeaseIsSoleGenerationFencedAndCopySafe(t *testing.T) {
	var runtime Runtime
	if !runtime.Publish(runtimePublication(t,
		PublicationOriginNativeGame, runtimeFrame(1))) {
		t.Fatal("publish failed")
	}
	first, ok := runtime.AcquireWriter(1, 1)
	if !ok {
		t.Fatal("first writer acquisition failed")
	}
	if _, ok := runtime.AcquireWriter(1, 1); ok {
		t.Fatal("second writer acquired concurrently")
	}
	_, disposition, token := runtime.Claim(1_000, first)
	if disposition != DeliveryFrame || !runtime.Admit(first, token, 1_000) {
		t.Fatal("first writer admission failed")
	}
	if runtime.RetireWriter(first) {
		t.Fatal("admitted writer retired early")
	}
	copyOfFirst := *first
	if runtime.Complete(&copyOfFirst, token, true, 1_000) {
		t.Fatal("copied writer completed original claim")
	}
	if !runtime.Complete(first, token, true, 1_000) ||
		!runtime.RetireWriter(first) {
		t.Fatal("first writer completion/retirement failed")
	}

	successor, ok := runtime.AcquireWriter(1, 1)
	if !ok || successor.WriterGeneration <= first.WriterGeneration {
		t.Fatal("successor writer generation did not advance")
	}
	if _, disposition, _ := runtime.Claim(1_000, first); disposition != DeliveryNone || runtime.Admit(first, token, 1_000) ||
		runtime.Complete(first, token, false, 1_000) {
		t.Fatal("retired writer remained usable")
	}
	replay := runtimeDeliverNext(t, &runtime, successor, 1_000)
	if replay.Disposition != DeliveryFrame {
		t.Fatal("successor writer did not receive current state")
	}
}

func TestRuntimeNewTargetGenerationStopsOldAndCannotLeak(t *testing.T) {
	var runtime Runtime
	oldFrame := runtimeFrame(1)
	oldFrame.BodyLow = 100
	if !runtime.Publish(runtimePublication(t,
		PublicationOriginProfileEffect, oldFrame)) {
		t.Fatal("old publish failed")
	}
	oldWriter, ok := runtime.AcquireWriter(1, 1)
	if !ok {
		t.Fatal("old writer acquisition failed")
	}
	old := runtimeDeliverNext(t, &runtime, oldWriter, 1_000)
	newFrame := runtimeFrame(1)
	newFrame.DeviceGeneration = 2
	newFrame.BodyLow = 200
	if !runtime.Publish(runtimePublication(t,
		PublicationOriginProfileEffect, newFrame)) {
		t.Fatal("new generation publish failed")
	}
	stop := runtimeDeliverNext(t, &runtime, oldWriter, 1_000)
	if stop.Disposition != DeliveryStop || stop.DeviceGeneration != 1 ||
		stop.DeliveryEpoch != old.DeliveryEpoch {
		t.Fatalf("old generation stop=%+v", stop)
	}
	if _, disposition, _ := runtime.Claim(1_000, oldWriter); disposition != DeliveryNone {
		t.Fatal("old target writer claimed new target state")
	}
	if !runtime.RetireWriter(oldWriter) {
		t.Fatal("old writer retirement failed")
	}
	if _, ok := runtime.AcquireWriter(1, 1); ok {
		t.Fatal("writer acquired target generation different from pending event")
	}
	newWriter, ok := runtime.AcquireWriter(2, 1)
	if !ok {
		t.Fatal("new writer acquisition failed")
	}
	current := runtimeDeliverNext(t, &runtime, newWriter, 1_000)
	if current.Disposition != DeliveryFrame || current.DeviceGeneration != 2 ||
		current.Frame.BodyLow != 200 {
		t.Fatalf("new generation delivery=%+v", current)
	}
}

func TestRuntimeDeliveredTrueRequiresAdmissionAndFailureCancels(t *testing.T) {
	var runtime Runtime
	if !runtime.Publish(runtimePublication(t,
		PublicationOriginProfileEffect, runtimeFrame(1))) {
		t.Fatal("publish failed")
	}
	writer, ok := runtime.AcquireWriter(1, 1)
	if !ok {
		t.Fatal("writer acquisition failed")
	}
	_, disposition, token := runtime.Claim(1_000, writer)
	if disposition != DeliveryFrame {
		t.Fatal("claim failed")
	}
	if runtime.Complete(writer, token, true, 1_000) {
		t.Fatal("unadmitted claim completed as delivered")
	}
	if !runtime.Complete(writer, token, false, 1_000) {
		t.Fatal("unadmitted claim could not be cancelled")
	}
	_, disposition, retry := runtime.Claim(1_000, writer)
	if disposition != DeliveryFrame || retry == 0 || retry == token {
		t.Fatal("cancelled claim was not retryable")
	}
}

func TestRuntimeSteadyStateHotPathDoesNotAllocate(t *testing.T) {
	var runtime Runtime
	if !runtime.Publish(Publication{
		Origin: PublicationOriginNativeGame, Frame: runtimeFrame(1)}) {
		t.Fatal("publish failed")
	}
	writer, ok := runtime.AcquireWriter(1, 1)
	if !ok {
		t.Fatal("writer acquisition failed")
	}
	runtimeDeliverNext(t, &runtime, writer, 1_000)
	sequence := uint64(1)
	cycle := func() {
		sequence++
		frame := runtimeFrame(sequence)
		frame.TimestampMicroseconds = 1_000 + sequence
		if !runtime.Publish(Publication{
			Origin: PublicationOriginNativeGame, Frame: frame}) {
			panic("publish failed")
		}
		_, disposition, token := runtime.Claim(1_000+sequence, writer)
		if disposition != DeliveryFrame ||
			!runtime.Admit(writer, token, 1_000+sequence) ||
			!runtime.Complete(writer, token, true, 1_000+sequence) {
			panic("delivery cycle failed")
		}
	}
	for range 128 {
		cycle()
	}
	allocations := testing.AllocsPerRun(1_000, cycle)
	if allocations != 0 {
		t.Fatalf("runtime hot path allocated %.2f times per cycle", allocations)
	}
}

func runtimePublication(t *testing.T, origin PublicationOrigin,
	frame Frame) Publication {
	t.Helper()
	publication, ok := NewPublication(origin, frame)
	if !ok {
		t.Fatalf("invalid publication origin=%d frame=%+v", origin, frame)
	}
	return publication
}

func runtimeClaimAndAdmit(t *testing.T, runtime *Runtime,
	writer *WriterLease, nowMicroseconds uint64) (Delivery, uint64) {
	t.Helper()
	delivery, disposition, token := runtime.Claim(nowMicroseconds, writer)
	if disposition == DeliveryNone || disposition != delivery.Disposition ||
		token == 0 || !runtime.Admit(writer, token, nowMicroseconds) {
		t.Fatalf("claim/admit delivery=%+v disposition=%d token=%d",
			delivery, disposition, token)
	}
	return delivery, token
}

func runtimeDeliverNext(t *testing.T, runtime *Runtime,
	writer *WriterLease, nowMicroseconds uint64) Delivery {
	t.Helper()
	delivery, token := runtimeClaimAndAdmit(t, runtime, writer, nowMicroseconds)
	if !runtime.Complete(writer, token, true, nowMicroseconds) {
		t.Fatal("completion failed")
	}
	return delivery
}

func runtimeFrame(sequence uint64) Frame {
	return Frame{
		Version: Version1, Source: SourceXboxOneVirtualDevice,
		Command: CommandApply, Actuators: ActuatorAll, BodyLow: 11,
		Sequence: sequence, DeviceGeneration: 1, TransportGeneration: 1,
		OwnershipEpoch: 1, TimestampMicroseconds: 1_000,
		TimeToLiveMicroseconds: MaxTimeToLiveMicroseconds,
	}
}

func runtimeStop(frame Frame) Frame {
	frame.Command = CommandStop
	frame.BodyLow = 0
	frame.BodyHigh = 0
	frame.LeftTrigger = 0
	frame.RightTrigger = 0
	return frame
}
