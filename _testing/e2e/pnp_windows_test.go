//go:build windows

package e2e_bench_test

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Alia5/VIIPER/_testing/e2e/latency"
	"golang.org/x/sys/windows"
)

var devPropKeyDeviceParent = windows.DEVPROPKEY{
	FmtID: windows.DEVPROPGUID(windows.GUID{
		Data1: 0x4340a6c5, Data2: 0x93fa, Data3: 0x4706,
		Data4: [8]byte{0x97, 0x2c, 0x7b, 0x64, 0x80, 0x08, 0xa5, 0xa7},
	}),
	PID: 8,
}

type presentDeviceNode struct {
	instanceID    string
	parentID      string
	containerID   string
	service       string
	hardwareIDs   []string
	locationInfo  string
	locationPaths []string
}

func pnpInstanceIDFromSDLPath(path string) (string, error) {
	trimmed := strings.TrimPrefix(path, `\\?\`)
	parts := strings.Split(trimmed, "#")
	if len(parts) < 4 || !strings.EqualFold(parts[0], "HID") ||
		parts[1] == "" || parts[2] == "" || !strings.HasPrefix(parts[len(parts)-1], "{") {
		return "", fmt.Errorf("SDL HID interface path has no exact PnP instance identity: %q", path)
	}
	return strings.ToUpper(strings.Join(parts[:3], `\`)), nil
}

func bindControllerPnP(proof *latency.ControllerProof, transport string, usbipPort int32) error {
	if proof == nil {
		return errors.New("nil controller proof")
	}
	instanceID, err := pnpInstanceIDFromSDLPath(proof.SDLPath)
	if err != nil {
		return err
	}
	deviceSet, err := windows.SetupDiGetClassDevsEx(nil, "", 0,
		windows.DIGCF_PRESENT|windows.DIGCF_ALLCLASSES, 0, "")
	if err != nil {
		return fmt.Errorf("enumerate present Windows PnP devices: %w", err)
	}
	defer deviceSet.Close()

	nodes := make(map[string]presentDeviceNode)
	for index := 0; ; index++ {
		info, enumErr := windows.SetupDiEnumDeviceInfo(deviceSet, index)
		if errors.Is(enumErr, windows.ERROR_NO_MORE_ITEMS) {
			break
		}
		if enumErr != nil {
			return fmt.Errorf("enumerate present PnP device %d: %w", index, enumErr)
		}
		id, idErr := windows.SetupDiGetDeviceInstanceId(deviceSet, info)
		if idErr != nil {
			return fmt.Errorf("read PnP instance ID %d: %w", index, idErr)
		}
		node := presentDeviceNode{instanceID: strings.ToUpper(id)}
		if value, propertyErr := windows.SetupDiGetDeviceProperty(deviceSet, info,
			&devPropKeyDeviceParent); propertyErr == nil {
			parentID, valid := value.(string)
			if !valid {
				return fmt.Errorf("PnP parent property for %q is not a string", node.instanceID)
			}
			node.parentID = strings.ToUpper(parentID)
		}
		if value, propertyErr := windows.SetupDiGetDeviceRegistryProperty(deviceSet, info, windows.SPDRP_SERVICE); propertyErr == nil {
			node.service, _ = value.(string)
		}
		if value, propertyErr := windows.SetupDiGetDeviceRegistryProperty(
			deviceSet, info, windows.SPDRP_BASE_CONTAINERID); propertyErr == nil {
			containerID, valid := value.(string)
			if !valid {
				return fmt.Errorf("PnP container property for %q is not a string", node.instanceID)
			}
			containerGUID, guidErr := windows.GUIDFromString(containerID)
			if guidErr != nil {
				return fmt.Errorf("PnP container property for %q is malformed: %w", node.instanceID, guidErr)
			}
			node.containerID = containerGUID.String()
		} else if !errors.Is(propertyErr, windows.ERROR_INVALID_DATA) &&
			!errors.Is(propertyErr, windows.ERROR_NOT_FOUND) {
			return fmt.Errorf("read PnP container property for %q: %w", node.instanceID, propertyErr)
		}
		if value, propertyErr := windows.SetupDiGetDeviceRegistryProperty(deviceSet, info, windows.SPDRP_HARDWAREID); propertyErr == nil {
			switch typed := value.(type) {
			case []string:
				node.hardwareIDs = append([]string(nil), typed...)
			case string:
				node.hardwareIDs = []string{typed}
			}
		}
		if value, propertyErr := windows.SetupDiGetDeviceRegistryProperty(deviceSet, info, windows.SPDRP_LOCATION_INFORMATION); propertyErr == nil {
			node.locationInfo, _ = value.(string)
		}
		if value, propertyErr := windows.SetupDiGetDeviceRegistryProperty(deviceSet, info, windows.SPDRP_LOCATION_PATHS); propertyErr == nil {
			switch typed := value.(type) {
			case []string:
				node.locationPaths = append([]string(nil), typed...)
			case string:
				node.locationPaths = []string{typed}
			}
		}
		nodes[node.instanceID] = node
	}
	controllerNode, present := nodes[instanceID]
	if !present {
		return fmt.Errorf("SDL interface instance %q is not a present Windows PnP devnode", instanceID)
	}
	if controllerNode.containerID == "" {
		return fmt.Errorf("SDL interface instance %q has no exact PnP container identity", instanceID)
	}

	proof.PNPInstanceID = instanceID
	proof.PNPContainerID = controllerNode.containerID
	if err := appendPnPAncestry(nodes, instanceID, transport, proof); err != nil {
		return err
	}
	if err := latency.ValidateTransportAncestry(transport, usbipPort, *proof); err != nil {
		return fmt.Errorf("bind SDL path %q to %s transport: %w", proof.SDLPath, transport, err)
	}
	return nil
}

func appendPnPAncestry(nodes map[string]presentDeviceNode, startID, transport string,
	proof *latency.ControllerProof,
) error {
	seen := make(map[string]struct{})
	current := strings.ToUpper(startID)
	for depth := 0; depth < 64; depth++ {
		if _, duplicate := seen[current]; duplicate {
			return errors.New("Windows PnP ancestry contains a cycle")
		}
		seen[current] = struct{}{}
		node, exists := nodes[current]
		if !exists {
			return fmt.Errorf("PnP ancestor %q is absent from the present-device snapshot", current)
		}
		proof.PNPAncestorIDs = append(proof.PNPAncestorIDs, node.instanceID)
		proof.PNPAncestorContainerIDs = append(proof.PNPAncestorContainerIDs, node.containerID)
		proof.PNPAncestorServices = append(proof.PNPAncestorServices, node.service)
		proof.PNPAncestorHardwareIDs = append(proof.PNPAncestorHardwareIDs,
			append([]string(nil), node.hardwareIDs...))
		proof.PNPAncestorLocationInfo = append(proof.PNPAncestorLocationInfo, node.locationInfo)
		proof.PNPAncestorLocationPaths = append(proof.PNPAncestorLocationPaths,
			append([]string(nil), node.locationPaths...))

		if transport == latency.TransportNativeUDE &&
			strings.EqualFold(node.service, "ViiperUde") &&
			containsFoldE2E(node.hardwareIDs, `ROOT\VIIPER\UDE`) {
			proof.TransportAnchorInstanceID = node.instanceID
			proof.TransportAnchorService = node.service
		}
		if transport == latency.TransportUSBIP &&
			strings.EqualFold(node.service, "usbip2_ude") &&
			containsFoldE2E(node.hardwareIDs, `ROOT\USBIP_WIN2\UDE`) {
			proof.TransportAnchorInstanceID = node.instanceID
			proof.TransportAnchorService = node.service
		}
		if node.parentID == "" {
			if !strings.EqualFold(node.instanceID, `HTREE\ROOT\0`) {
				return fmt.Errorf("PnP ancestry ended at %q instead of HTREE\\ROOT\\0", node.instanceID)
			}
			return nil
		}
		current = strings.ToUpper(node.parentID)
	}
	return errors.New("Windows PnP ancestry exceeded the 64-node safety bound")
}

func containsFoldE2E(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}
