// Package ns2pro provides a Nintendo Switch 2 Pro Controller compatible HID device.
package ns2pro

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Alia5/VIIPER/device"
	"github.com/Alia5/VIIPER/internal/inputpresentation"
	"github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
)

type NS2Pro struct {
	input                      *inputpresentation.FixedReportScheduler[InputState]
	inputSignal                chan struct{}
	inputReports               *ns2InputReportSequencer
	bulkCh                     chan struct{}
	stateMu                    sync.Mutex
	metaUpdateMu               sync.Mutex
	inputState                 InputState
	metaState                  *MetaState
	inputLifecycleMu           sync.Mutex
	producerMu                 sync.Mutex
	producerActive             bool
	compatProducerUsed         bool
	retiredCompatibilityLease  inputpresentation.FixedReportProducerLease
	resyncMu                   sync.Mutex
	pendingResync              InputState
	pendingResyncAt            time.Time
	hasPendingResync           bool
	activePresentationClaim    inputpresentation.Claim
	activePresentationReport   [InputReportSize]byte
	activePresentationState    InputState
	hasDeferredPresentation    bool
	inputReportSnapshot        [InputReportSize]byte
	inputReportSnapshotState   InputState
	inputReportSnapshotVersion uint64
	outputMu                   sync.RWMutex
	outputCallback             func(OutputState)
	outputVersion              uint64
	descriptor                 usb.Descriptor

	protoMu           sync.Mutex
	featureMask       uint8
	usbReportsEnabled bool
	bulkInQueue       [][]byte
}

func New(o *device.CreateOptions) (*NS2Pro, error) {
	metaState := defaultMetaState()
	if o != nil && o.DeviceSpecific != "" {
		var newMeta struct {
			SerialNumber  *string `json:"serial_number"`
			BatteryLevel  *uint8  `json:"battery_level"`
			Charging      *bool   `json:"charging"`
			ExternalPower *bool   `json:"external_power"`
			BatteryVolts  *uint16 `json:"battery_volts"`
		}
		if err := json.Unmarshal([]byte(o.DeviceSpecific), &newMeta); err != nil {
			return nil, fmt.Errorf("invalid device specific JSON: %w", err)
		}
		if newMeta.SerialNumber != nil && *newMeta.SerialNumber != "" {
			metaState.SerialNumber = *newMeta.SerialNumber
		}
		if newMeta.BatteryLevel != nil && *newMeta.BatteryLevel != 0 {
			metaState.BatteryLevel = *newMeta.BatteryLevel
		}
		if newMeta.Charging != nil {
			metaState.Charging = *newMeta.Charging
		}
		if newMeta.ExternalPower != nil {
			metaState.ExternalPower = *newMeta.ExternalPower
		}
		if newMeta.BatteryVolts != nil && *newMeta.BatteryVolts != 0 {
			metaState.BatteryVolts = *newMeta.BatteryVolts
		}
	}

	now := time.Now()
	neutral := *defaultInputState()
	inputSignal := make(chan struct{}, 1)
	inputSignal <- struct{}{}
	d := &NS2Pro{
		inputSignal:  inputSignal,
		bulkCh:       make(chan struct{}, 1),
		inputState:   neutral,
		metaState:    metaState,
		descriptor:   MakeDescriptor(),
		inputReports: newNS2InputReportSequencer(*metaState, now),
	}
	input, err := inputpresentation.NewFixedReportSchedulerWithOverflowFault(
		InputReportSize, neutral, d.inputReports.buildCurrentInto,
		ns2InputTransition, now)
	if err != nil {
		return nil, fmt.Errorf("create ns2pro input scheduler: %w", err)
	}
	d.input = input
	d.inputReports.buildControlInto(&neutral, 0, d.inputReportSnapshot[:])
	d.inputReportSnapshotState = neutral
	d.inputReportSnapshotVersion = 1
	serialEnding := DefaultSerialEnding
	if len(metaState.SerialNumber) >= 2 {
		serialEnding = metaState.SerialNumber[len(metaState.SerialNumber)-2:]
	}
	d.descriptor.Strings[3] = serialEnding

	if o != nil {
		if o.IDVendor != nil {
			d.descriptor.Device.IDVendor = *o.IDVendor
		}
		if o.IDProduct != nil {
			d.descriptor.Device.IDProduct = *o.IDProduct
		}
	}
	return d, nil
}

func (d *NS2Pro) SetOutputCallback(f func(OutputState)) func() {
	d.outputMu.Lock()
	d.outputVersion++
	version := d.outputVersion
	d.outputCallback = f
	d.outputMu.Unlock()

	return func() {
		d.outputMu.Lock()
		if d.outputVersion == version {
			d.outputCallback = nil
		}
		d.outputMu.Unlock()
	}
}

func (d *NS2Pro) UpdateInputState(state InputState) bool {
	d.producerMu.Lock()
	defer d.producerMu.Unlock()
	if d.producerActive {
		return false
	}
	d.compatProducerUsed = true
	_, disposition := d.publishInputStateWithLease(
		d.input.ProducerLease(), state, time.Now())
	return disposition.Accepted()
}

func (d *NS2Pro) SetMetaState(meta MetaState) {
	d.metaUpdateMu.Lock()
	defer d.metaUpdateMu.Unlock()
	d.stateMu.Lock()
	d.metaState = &meta
	if d.descriptor.Strings != nil {
		serialEnding := DefaultSerialEnding
		if len(meta.SerialNumber) >= 2 {
			serialEnding = meta.SerialNumber[len(meta.SerialNumber)-2:]
		}
		d.descriptor.Strings[3] = serialEnding
	}
	d.stateMu.Unlock()
	d.updateInputEncodingConfiguration(func() {
		d.inputReports.setMeta(meta)
	})
}

// UpdateNS2ProRuntimeStatusV1 applies a strict, complete out-of-band power
// snapshot. A status update never mutates serial identity, never enters the
// 24-byte producer stream, and never fences controls to a synthetic neutral.
// Any already selected immutable report may complete with the prior status;
// the next newly encoded report observes this snapshot.
func (d *NS2Pro) UpdateNS2ProRuntimeStatusV1(payload string) error {
	status, err := DecodeRuntimeStatusV1(payload)
	if err != nil {
		return err
	}
	return d.SetRuntimeStatusV1(status)
}

func (d *NS2Pro) SetRuntimeStatusV1(status RuntimeStatusV1) error {
	if err := status.Validate(); err != nil {
		return err
	}
	d.metaUpdateMu.Lock()
	defer d.metaUpdateMu.Unlock()

	d.stateMu.Lock()
	if d.metaState == nil {
		d.stateMu.Unlock()
		return errors.New("ns2pro metadata is unavailable")
	}
	meta := *d.metaState
	meta.BatteryLevel = status.BatteryLevel
	meta.Charging = status.Charging
	meta.ExternalPower = status.ExternalPower
	meta.BatteryVolts = status.BatteryVolts
	d.metaState = &meta
	d.stateMu.Unlock()

	// The sequencer lock linearizes this rare control-plane change against
	// report encoding. It does not touch the semantic input scheduler.
	d.inputReports.setMeta(meta)
	return nil
}

func (d *NS2Pro) HandleTransfer(ctx context.Context, ep uint32, dir uint32, out []byte) []byte {
	switch {
	case dir == usbip.DirIn && ep == 1:
		for {
			select {
			case <-ctx.Done():
				if errors.Is(ctx.Err(), context.DeadlineExceeded) && d.reportsEnabled() {
					return d.nextInputReport()
				}
				return nil
			case <-d.inputSignal:
				if d.reportsEnabled() {
					return d.nextInputReport()
				}
			}
		}
	case dir == usbip.DirIn && ep == 2:
		for {
			if resp := d.popBulkIn(); resp != nil {
				return resp
			}
			select {
			case <-ctx.Done():
				return nil
			case <-d.bulkCh:
			}
		}
	case dir == usbip.DirOut && ep == 1:
		d.handleOutputReport(out)
	case dir == usbip.DirOut && ep == 2:
		d.handleBulkOut(out)
	}
	return nil
}

func (d *NS2Pro) HandleControl(bmRequestType, bRequest uint8, wValue, wIndex uint16, wLength uint16, data []byte) ([]byte, bool) {
	reportType := uint8(wValue >> 8)
	reportID := uint8(wValue)

	if bmRequestType == hidClassRequestIn && bRequest == hidGetReport && reportType == reportTypeInput {
		switch reportID {
		case ReportIDCommon, ReportIDPro, 0:
			return d.inputReportForID(reportID), true
		}
	}

	if bmRequestType == hidClassRequestOut && bRequest == hidSetReport && reportType == reportTypeOutput && reportID == ReportIDOutput {
		d.handleOutputReport(data)
		return nil, true
	}

	if isAudioClassRequest(bmRequestType) {
		switch bRequest {
		case audioSetCur:
			return nil, true
		case audioGetCur, audioGetMin, audioGetMax, audioGetRes:
			return make([]byte, wLength), true
		}
	}

	return nil, false
}

func isAudioClassRequest(bmRequestType uint8) bool {
	return bmRequestType&requestTypeMask == requestClass &&
		(bmRequestType&recipientMask == recipientIface || bmRequestType&recipientMask == recipientEndpoint)
}

func (d *NS2Pro) GetDescriptor() *usb.Descriptor {
	return &d.descriptor
}

func (d *NS2Pro) GetDeviceSpecificArgs() map[string]any {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	if d.metaState == nil {
		return map[string]any{}
	}
	b, err := json.Marshal(d.metaState)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return map[string]any{}
	}
	return out
}

func (d *NS2Pro) reportsEnabled() bool {
	d.protoMu.Lock()
	defer d.protoMu.Unlock()
	return d.usbReportsEnabled
}

func (d *NS2Pro) nextInputReport() []byte {
	report := make([]byte, InputReportSize)
	if d.BuildInputReportInto(report) != InputReportSize {
		return nil
	}
	return report
}

func (d *NS2Pro) inputReportForID(reportID uint8) []byte {
	report := make([]byte, InputReportSize)
	if n, _ := d.snapshotInputReportForIDInto(reportID, report); n !=
		InputReportSize {
		return nil
	}
	return report
}

func (d *NS2Pro) serialNumber() string {
	d.stateMu.Lock()
	defer d.stateMu.Unlock()
	if d.metaState == nil {
		return ""
	}
	return d.metaState.SerialNumber
}

func (d *NS2Pro) handleOutputReport(out []byte) {
	if len(out) == 0 {
		return
	}

	payload := out
	if out[0] == ReportIDOutput {
		payload = out[1:]
	} else if len(out) != OutputRumbleSize {
		return
	}
	if len(payload) < OutputRumbleSize {
		return
	}

	feedback := OutputState{}
	copy(feedback.LeftRumble[:], payload[0:16])
	copy(feedback.RightRumble[:], payload[16:32])
	feedback.Flags = OutputFlagRumble
	d.emitOutput(feedback)
}

func (d *NS2Pro) emitOutput(feedback OutputState) {
	d.outputMu.RLock()
	callback := d.outputCallback
	d.outputMu.RUnlock()
	if callback != nil {
		callback(feedback)
	}
}

func (d *NS2Pro) enqueueResponse(resp []byte) {
	d.protoMu.Lock()
	d.bulkInQueue = append(d.bulkInQueue, append([]byte(nil), resp...))
	d.protoMu.Unlock()
	select {
	case d.bulkCh <- struct{}{}:
	default:
	}
}

func (d *NS2Pro) popBulkIn() []byte {
	d.protoMu.Lock()
	defer d.protoMu.Unlock()
	if len(d.bulkInQueue) == 0 {
		return nil
	}
	chunk := d.bulkInQueue[0]
	d.bulkInQueue = d.bulkInQueue[1:]
	return append([]byte(nil), chunk...)
}

func commandHeader(cmd, seq, sub uint8) []byte {
	return []byte{cmd, 0x01, seq, sub, 0x10, 0x78, 0x00, 0x00}
}
