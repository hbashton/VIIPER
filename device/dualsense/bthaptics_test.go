package dualsense

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestBuildBluetoothHapticsReportMatchesSAxenseLayout(t *testing.T) {
	sample := make([]byte, BluetoothHapticsSampleSize)
	for i := range sample {
		sample[i] = byte(i)
	}

	report, err := BuildBluetoothHapticsReport(0x0A, 0x37, sample)
	if err != nil {
		t.Fatalf("BuildBluetoothHapticsReport failed: %v", err)
	}

	if len(report) != BluetoothHapticsReportSize {
		t.Fatalf("unexpected report size: got %d want %d", len(report), BluetoothHapticsReportSize)
	}

	if report[0] != BluetoothHapticsReportID {
		t.Fatalf("unexpected report ID: got %#x want %#x", report[0], BluetoothHapticsReportID)
	}
	if report[1] != 0xA0 {
		t.Fatalf("unexpected tag/sequence byte: got %#x want 0xA0", report[1])
	}
	if report[2] != 0x91 || report[3] != 0x07 {
		t.Fatalf("unexpected packet 0x11 header: % x", report[2:4])
	}
	if !bytes.Equal(report[4:11], []byte{0xFE, 0x00, 0x00, 0x00, 0x00, 0xFF, 0x37}) {
		t.Fatalf("unexpected packet 0x11 body: % x", report[4:11])
	}
	if report[11] != 0x92 || report[12] != BluetoothHapticsSampleSize {
		t.Fatalf("unexpected packet 0x12 header: % x", report[11:13])
	}
	if !bytes.Equal(report[13:13+BluetoothHapticsSampleSize], sample) {
		t.Fatalf("sample payload was not copied into packet 0x12")
	}

	gotCRC := binary.LittleEndian.Uint32(report[BluetoothHapticsReportSize-4:])
	wantCRC := dualSenseBluetoothCRC32(report[:BluetoothHapticsReportSize-4])
	if gotCRC != wantCRC {
		t.Fatalf("unexpected crc: got %#x want %#x", gotCRC, wantCRC)
	}
}

func TestBuildBluetoothHapticsReportRejectsWrongSampleSize(t *testing.T) {
	if _, err := BuildBluetoothHapticsReport(0, 0, make([]byte, BluetoothHapticsSampleSize-1)); err == nil {
		t.Fatal("expected short sample to fail")
	}
}

func TestBuildBluetoothCombinedHapticsReportMatchesVDSLayout(t *testing.T) {
	sample := make([]byte, BluetoothHapticsSampleSize)
	for i := range sample {
		sample[i] = byte(i)
	}
	raw := make([]byte, OutputReportSize)
	raw[0] = ReportIDOutput
	raw[1] = 0xA1
	raw[37] = 0x4D

	report, err := BuildBluetoothCombinedHapticsReport(0x0A, 0x37, sample, raw)
	if err != nil {
		t.Fatalf("BuildBluetoothCombinedHapticsReport failed: %v", err)
	}
	if len(report) != BluetoothCombinedHapticsReportSize {
		t.Fatalf("unexpected report size: got %d want %d", len(report), BluetoothCombinedHapticsReportSize)
	}
	if report[0] != BluetoothCombinedHapticsReportID || report[1] != 0xA0 {
		t.Fatalf("unexpected 0x36 report header: % x", report[:2])
	}
	if !bytes.Equal(report[2:11], []byte{0x91, 0x07, 0xFE, 64, 64, 64, 64, 64, 0x37}) {
		t.Fatalf("unexpected packet 0x11 body: % x", report[2:11])
	}
	if report[11] != 0x90 || report[12] != BluetoothCombinedStateSize {
		t.Fatalf("unexpected state block header: % x", report[11:13])
	}
	if report[13] != raw[1] || report[13+36] != raw[37] {
		t.Fatalf("native USB output payload was not preserved in state block: % x", report[13:76])
	}
	if report[76] != 0x92 || report[77] != BluetoothHapticsSampleSize || !bytes.Equal(report[78:142], sample) {
		t.Fatalf("unexpected haptics block: % x", report[76:142])
	}
	if report[142] != 0x93 || report[143] != 0 {
		t.Fatalf("unexpected empty speaker block: % x", report[142:144])
	}

	gotCRC := binary.LittleEndian.Uint32(report[BluetoothCombinedHapticsReportSize-4:])
	wantCRC := dualSenseBluetoothCRC32(report[:BluetoothCombinedHapticsReportSize-4])
	if gotCRC != wantCRC {
		t.Fatalf("unexpected crc: got %#x want %#x", gotCRC, wantCRC)
	}
}

func TestBuildBluetoothOutputReportFromUSBOutputMatchesHIDMaestroMapping(t *testing.T) {
	usbReport := make([]byte, OutputReportSize)
	usbReport[0] = ReportIDOutput
	for i := 1; i < len(usbReport); i++ {
		usbReport[i] = byte(i)
	}

	report, err := BuildBluetoothOutputReportFromUSBOutput(0x07, usbReport)
	if err != nil {
		t.Fatalf("BuildBluetoothOutputReportFromUSBOutput failed: %v", err)
	}

	if len(report) != BluetoothOutputReportSize {
		t.Fatalf("unexpected report size: got %d want %d", len(report), BluetoothOutputReportSize)
	}
	if report[0] != BluetoothOutputReportID {
		t.Fatalf("unexpected report ID: got %#x want %#x", report[0], BluetoothOutputReportID)
	}
	if report[1] != 0x70 {
		t.Fatalf("unexpected BT tag: got %#x want 0x70", report[1])
	}
	if report[2] != 0x10 {
		t.Fatalf("unexpected BT flag: got %#x want 0x10", report[2])
	}
	if !bytes.Equal(report[3:50], usbReport[1:OutputReportSize]) {
		t.Fatalf("USB effect payload was not shifted to BT bytes 3-49:\n got: % x\nwant: % x",
			report[3:50], usbReport[1:OutputReportSize])
	}

	crcInput := append([]byte{0xA2, BluetoothOutputReportID}, report[1:74]...)
	gotCRC := binary.LittleEndian.Uint32(report[74:78])
	wantCRC := dualSenseBluetoothCRC32(crcInput)
	if gotCRC != wantCRC {
		t.Fatalf("unexpected crc: got %#x want %#x", gotCRC, wantCRC)
	}
}

func TestBuildBluetoothOutputReportFromUSBOutputRejectsInvalidReport(t *testing.T) {
	if _, err := BuildBluetoothOutputReportFromUSBOutput(0, make([]byte, OutputReportSize-1)); err == nil {
		t.Fatal("expected short USB output report to fail")
	}

	report := make([]byte, OutputReportSize)
	report[0] = 0x31
	if _, err := BuildBluetoothOutputReportFromUSBOutput(0, report); err == nil {
		t.Fatal("expected wrong report ID to fail")
	}
}
