package handler

import (
	"encoding/binary"
	"fmt"
	"net"
	"time"

	"github.com/Alia5/VIIPER/device/xboxone"
	"github.com/Alia5/VIIPER/usbip"
)

// Wire operations follow the existing usb/retained_import_transport_test.go
// and retained_xbox_retirement_integration_test.go fixtures. Only the simulated
// host is new: the listener, import/START handling, input and feedback are real.
type xboxInteropRetainedHost struct {
	conn               net.Conn
	deviceID, sequence uint32
}

func newXboxInteropRetainedHost(addr, alias string, meta *usbip.ExportMeta) (_ *xboxInteropRetainedHost, err error) {
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = conn.Close()
		}
	}()
	if err = conn.SetDeadline(time.Now().Add(20 * time.Second)); err != nil {
		return nil, err
	}
	if err = (&usbip.MgmtHeader{Version: usbip.Version, Command: usbip.OpReqImport}).Write(conn); err != nil {
		return nil, err
	}
	var bus [32]byte
	copy(bus[:], alias)
	if _, err = conn.Write(bus[:]); err != nil {
		return nil, err
	}
	var reply [8 + 312]byte
	if err = usbip.ReadExactly(conn, reply[:]); err != nil {
		return nil, err
	}
	if binary.BigEndian.Uint16(reply[:2]) != usbip.Version ||
		binary.BigEndian.Uint16(reply[2:4]) != usbip.OpRepImport || binary.BigEndian.Uint32(reply[4:8]) != 0 {
		return nil, fmt.Errorf("isolated retained import was not acknowledged")
	}
	host := &xboxInteropRetainedHost{conn: conn, deviceID: meta.BusID<<16 | meta.DevID}
	if err = host.submit(usbip.DirOut, 0, 0, [8]byte{0, 9, 1}, nil); err != nil {
		return nil, err
	}
	if _, err = host.read(usbip.DirOut); err != nil {
		return nil, err
	}
	if err = host.submit(usbip.DirIn, 1, 64, [8]byte{}, nil); err != nil {
		return nil, err
	}
	hello, err := host.read(usbip.DirIn)
	if err != nil {
		return nil, err
	}
	if _, err = xboxone.DecodeHelloMessage(hello); err != nil {
		return nil, err
	}
	start := []byte{0x05, 0x20, 0x02, 0x01, byte(xboxone.SetDeviceStateStart)}
	if err = host.submit(usbip.DirOut, 1, uint32(len(start)), [8]byte{}, start); err != nil {
		return nil, err
	}
	if _, err = host.read(usbip.DirOut); err != nil {
		return nil, err
	}
	for range 2 {
		if err = host.submit(usbip.DirIn, 1, 64, [8]byte{}, nil); err != nil {
			return nil, err
		}
		if _, err = host.read(usbip.DirIn); err != nil {
			return nil, err
		}
	}
	// This request drives START's local permit and waits for C# semantic input.
	if err = host.submit(usbip.DirIn, 1, 64, [8]byte{}, nil); err != nil {
		return nil, err
	}
	return host, nil
}

func (host *xboxInteropRetainedHost) submit(direction, endpoint, length uint32, setup [8]byte, payload []byte) error {
	host.sequence++
	command := usbip.CmdSubmit{Basic: usbip.HeaderBasic{Command: usbip.CmdSubmitCode,
		Seqnum: host.sequence, Devid: host.deviceID, Dir: direction, Ep: endpoint},
		TransferBufferLen: length, NumberOfPackets: -1, Setup: setup}
	if err := command.Write(host.conn); err != nil {
		return err
	}
	if len(payload) != 0 {
		_, err := host.conn.Write(payload)
		return err
	}
	return nil
}

func (host *xboxInteropRetainedHost) read(direction uint32) ([]byte, error) {
	var header [48]byte
	if err := usbip.ReadExactly(host.conn, header[:]); err != nil {
		return nil, err
	}
	if binary.BigEndian.Uint32(header[:4]) != usbip.RetSubmitCode ||
		binary.BigEndian.Uint32(header[4:8]) != host.sequence || binary.BigEndian.Uint32(header[20:24]) != 0 {
		return nil, fmt.Errorf("isolated retained transfer rejected or miscorrelated: sequence=%d status=%d",
			binary.BigEndian.Uint32(header[4:8]), int32(binary.BigEndian.Uint32(header[20:24])))
	}
	length := binary.BigEndian.Uint32(header[24:28])
	if length > 64 {
		return nil, fmt.Errorf("unexpected isolated transfer length")
	}
	if direction == usbip.DirOut {
		return nil, nil
	}
	payload := make([]byte, length)
	return payload, usbip.ReadExactly(host.conn, payload)
}

func (host *xboxInteropRetainedHost) verifyInput(pressed bool) error {
	payload, err := host.read(usbip.DirIn)
	if err != nil {
		return err
	}
	_, report, err := xboxone.DecodeConsoleFunctionMapGamepadInputMessage(payload)
	if err != nil {
		return err
	}
	if pressed {
		if !report.State.A || report.State.LeftTrigger != 1023 || report.State.LeftStickX != 32767 {
			return fmt.Errorf("canonical C# press/trigger/stick did not reach GIP input")
		}
	} else if report.State != (xboxone.InputStateV1{}) {
		return fmt.Errorf("canonical C# neutral did not reach GIP input")
	}
	return nil
}

func (host *xboxInteropRetainedHost) sendFeedback() error {
	for index, body := range []xboxone.RumbleBodyV1{
		{Enabled: xboxone.MotorAll, LeftVibration: 20, Duration: 25},
		{Enabled: xboxone.MotorAll, RightVibration: 40, Duration: 25},
		{Enabled: xboxone.MotorAll, LeftImpulse: 60, Duration: 25},
		{Enabled: xboxone.MotorAll, RightImpulse: 80, Duration: 25},
	} {
		var packet [xboxone.DirectMotorMessageSize]byte
		if err := xboxone.EncodeDirectMotorMessageInto(packet[:], byte(index+1), body); err != nil {
			return err
		}
		if err := host.submit(usbip.DirOut, 1, uint32(len(packet)), [8]byte{}, packet[:]); err != nil {
			return err
		}
		if _, err := host.read(usbip.DirOut); err != nil {
			return err
		}
	}
	return nil
}
