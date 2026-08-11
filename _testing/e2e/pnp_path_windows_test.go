//go:build windows

package e2e_bench_test

import (
	"os"
	"strings"
	"testing"

	"github.com/Alia5/VIIPER/_testing/e2e/latency"
)

func TestPnPInstanceIDFromSDLPathFailsClosed(t *testing.T) {
	want := `HID\VID_045E&PID_028E&IG_00\7&ABC&0&0000`
	got, err := pnpInstanceIDFromSDLPath(
		`\\?\hid#vid_045e&pid_028e&ig_00#7&abc&0&0000#{4d1e55b2-f16f-11cf-88cb-001111000030}`)
	if err != nil || got != want {
		t.Fatalf("instance=%q error=%v, want %q", got, err, want)
	}
	for _, invalid := range []string{"", `HID\VID_045E`, `XInput#0`, `\\?\USB#VID_045E#1#{guid}`} {
		if got, err = pnpInstanceIDFromSDLPath(invalid); err == nil {
			t.Fatalf("invalid SDL path %q returned %q", invalid, got)
		}
	}
}

func TestPinnedSDLXboxPathRequiresRawInputForPnPIdentity(t *testing.T) {
	rawInputSource, err := os.ReadFile("deps/SDL/src/joystick/windows/SDL_rawinputjoystick.c")
	if err != nil {
		t.Fatal(err)
	}
	xinputSource, err := os.ReadFile("deps/SDL/src/joystick/windows/SDL_xinputjoystick.c")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rawInputSource),
		"SDL_GetHintBoolean(SDL_HINT_JOYSTICK_RAWINPUT, false)") {
		t.Fatal("pinned SDL RawInput default changed; re-audit the exact Xbox PnP binding")
	}
	if !strings.Contains(string(xinputSource), `"XInput#%u"`) {
		t.Fatal("pinned SDL XInput path contract changed; re-audit the exact Xbox PnP binding")
	}
}

func TestAppendPnPAncestryRequiresCompleteUnambiguousRootChain(t *testing.T) {
	const (
		hidID    = `HID\VID_045E&PID_028E\1`
		usbID    = `USB\VID_045E&PID_028E\1`
		anchorID = `ROOT\USB\0002`
		rootID   = `HTREE\ROOT\0`
	)
	valid := map[string]presentDeviceNode{
		hidID:    {instanceID: hidID, parentID: usbID, service: "HidUsb"},
		usbID:    {instanceID: usbID, parentID: anchorID, service: "usbccgp", locationPaths: []string{`USBROOT(0)#USB(7)`}},
		anchorID: {instanceID: anchorID, parentID: rootID, service: "usbip2_ude", hardwareIDs: []string{`ROOT\USBIP_WIN2\UDE`}},
		rootID:   {instanceID: rootID},
	}
	proof := latency.ControllerProof{PNPInstanceID: hidID}
	if err := appendPnPAncestry(valid, hidID, latency.TransportUSBIP, &proof); err != nil {
		t.Fatal(err)
	}
	if err := latency.ValidateTransportAncestry(latency.TransportUSBIP, 7, proof); err != nil {
		t.Fatalf("valid full USB/IP ancestry was rejected: %v", err)
	}
	if got := proof.PNPAncestorIDs[len(proof.PNPAncestorIDs)-1]; got != rootID {
		t.Fatalf("ancestry ended at %q, want %q", got, rootID)
	}

	t.Run("truncated", func(t *testing.T) {
		nodes := clonePresentNodes(valid)
		delete(nodes, rootID)
		if err := appendPnPAncestry(nodes, hidID, latency.TransportUSBIP,
			&latency.ControllerProof{PNPInstanceID: hidID}); err == nil {
			t.Fatal("truncated PnP ancestry was accepted")
		}
	})

	t.Run("cycle", func(t *testing.T) {
		nodes := clonePresentNodes(valid)
		root := nodes[rootID]
		root.parentID = usbID
		nodes[rootID] = root
		if err := appendPnPAncestry(nodes, hidID, latency.TransportUSBIP,
			&latency.ControllerProof{PNPInstanceID: hidID}); err == nil ||
			!strings.Contains(err.Error(), "cycle") {
			t.Fatalf("cyclic PnP ancestry error=%v", err)
		}
	})

	t.Run("nested spoof anchor", func(t *testing.T) {
		const spoofID = `ROOT\USB\0001`
		nodes := clonePresentNodes(valid)
		usb := nodes[usbID]
		usb.parentID = spoofID
		nodes[usbID] = usb
		nodes[spoofID] = presentDeviceNode{
			instanceID: spoofID, parentID: anchorID, service: "usbip2_ude",
			hardwareIDs: []string{`ROOT\USBIP_WIN2\UDE`},
		}
		candidate := latency.ControllerProof{PNPInstanceID: hidID}
		if err := appendPnPAncestry(nodes, hidID, latency.TransportUSBIP, &candidate); err != nil {
			t.Fatal(err)
		}
		if err := latency.ValidateTransportAncestry(latency.TransportUSBIP, 7, candidate); err == nil {
			t.Fatal("nested spoof transport anchor was accepted")
		}
	})
}

func clonePresentNodes(source map[string]presentDeviceNode) map[string]presentDeviceNode {
	result := make(map[string]presentDeviceNode, len(source))
	for key, node := range source {
		node.hardwareIDs = append([]string(nil), node.hardwareIDs...)
		node.locationPaths = append([]string(nil), node.locationPaths...)
		result[key] = node
	}
	return result
}
