// Package virtualbus manages USB bus topology and auto-assigns device addresses.
package virtualbus

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/Alia5/VIIPER/device"
	"github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
)

const basepath = "/sys/devices/pci0000:00/0000:00:08.1/0000:00:04:00.3/usb"

var (
	allocatedBusIds = make(map[uint32]bool)
	globalMtx       sync.Mutex
)

// VirtualBus manages USB bus topology and auto-assigns device addresses.
type VirtualBus struct {
	mtx                   sync.Mutex
	busID                 uint32
	nextDevID             uint32
	nextRegistrationToken uint64
	allocatedDevIDs       map[uint32]bool
	devices               []busDevice
	emptyCtx              context.Context
	emptyCancel           context.CancelFunc
	closed                bool
	closeDone             chan struct{}
}

// DeviceMeta exposes a registered device and its metadata for external queries.
type DeviceMeta struct {
	Dev  usb.Device
	Meta usbip.ExportMeta
	// Bus, Context, and RegistrationToken form one opaque Add incarnation.
	// Callers must pass the complete snapshot back to VirtualBus for exact
	// authentication/removal rather than interpreting the token.
	Bus               *VirtualBus
	Context           context.Context
	RegistrationToken uint64
	lifecycle         *registrationLifecycle
}

// New creates a new VirtualBus instance with a unique auto-assigned bus number.
func New(busID uint32) *VirtualBus {
	globalMtx.Lock()
	defer globalMtx.Unlock()

	allocatedBusIds[busID] = true

	return &VirtualBus{
		busID:           busID,
		nextDevID:       0,
		allocatedDevIDs: make(map[uint32]bool),
		closeDone:       make(chan struct{}),
	}
}

// NewWithBusID creates a new VirtualBus instance starting at a specific bus number.
// Returns an error if the bus number is already allocated.
func NewWithBusID(busID uint32) (*VirtualBus, error) {
	globalMtx.Lock()
	defer globalMtx.Unlock()

	if allocatedBusIds[busID] {
		return nil, fmt.Errorf("bus number %d already allocated", busID)
	}
	allocatedBusIds[busID] = true

	return &VirtualBus{
		busID:           busID,
		nextDevID:       0,
		allocatedDevIDs: make(map[uint32]bool),
		closeDone:       make(chan struct{}),
	}, nil
}

// Add registers a device using a descriptor provider implemented by the device.
// This is a convenience wrapper so callers can simply do "bus.Add(dev)".
// The device must implement a method:
//
//	GetDeviceDescriptor() DeviceDescriptorStruct
//
// which returns a static descriptor that will be used for bus registration.
// Returns a context containing the device's lifecycle and metadata (use GetDeviceMeta to extract).
func (vb *VirtualBus) Add(dev usb.Device) (context.Context, error) {
	registration, err := vb.AddRegistration(dev)
	if err != nil {
		return nil, err
	}
	return registration.Context, nil
}

// AddRegistration atomically adds a device and returns the opaque exact Add
// incarnation created under the same bus critical section. Callers which own
// external lifecycle state must use this method rather than calling Add and
// then attempting to rediscover the registration across a remove/re-add gap.
func (vb *VirtualBus) AddRegistration(dev usb.Device) (DeviceMeta, error) {
	return vb.addRegistration(dev, true, ^uint32(0), "")
}

// AddProvisionalRegistrationThrough atomically allocates an exact Add
// incarnation under an inclusive device-address ceiling. It participates in
// duplicate/removal/close ownership but remains absent from address-based
// discovery until PublishRegistration authenticates and commits that same
// incarnation. Allocation and the ceiling check share one bus critical
// section, so a caller never has to add an unrepresentable address and then
// roll it back.
func (vb *VirtualBus) AddProvisionalRegistrationThrough(
	dev usb.Device,
	maxDeviceID uint32,
) (DeviceMeta, error) {
	return vb.addRegistration(dev, false, maxDeviceID, "")
}

// AddProvisionalRegistrationWithXboxOneBusIDThrough installs an already-issued
// public production alias in the immutable Add identity before publication.
// Generic registrations retain their historical numeric export name.
func (vb *VirtualBus) AddProvisionalRegistrationWithXboxOneBusIDThrough(
	dev usb.Device, maxDeviceID uint32, busID string,
) (DeviceMeta, error) {
	if !usbip.ValidProductionXboxOneBusID(busID) {
		return DeviceMeta{}, fmt.Errorf("invalid production Xbox USB/IP alias")
	}
	return vb.addRegistration(dev, false, maxDeviceID, busID)
}

// CanAddRepresentableRegistration reports whether a later bounded Add could
// currently allocate both a registration token and a nonzero device address.
// It is a non-reserving preflight; AddProvisionalRegistrationThrough repeats
// the same checks atomically with allocation.
func (vb *VirtualBus) CanAddRepresentableRegistration(
	maxDeviceID uint32,
) bool {
	if vb == nil || maxDeviceID == 0 {
		return false
	}
	vb.mtx.Lock()
	defer vb.mtx.Unlock()
	if vb.closed || vb.nextRegistrationToken == ^uint64(0) {
		return false
	}
	_, available := vb.nextAvailableDeviceIDLocked(maxDeviceID)
	return available
}

func (vb *VirtualBus) addRegistration(
	dev usb.Device,
	published bool,
	maxDeviceID uint32,
	exportBusID string,
) (DeviceMeta, error) {
	if vb == nil {
		return DeviceMeta{}, fmt.Errorf("bus is nil")
	}
	if err := validateDeviceIdentity(dev); err != nil {
		return DeviceMeta{}, err
	}
	if maxDeviceID == 0 {
		return DeviceMeta{}, fmt.Errorf("device address range is empty")
	}
	vb.mtx.Lock()
	defer vb.mtx.Unlock()
	if vb.closed {
		return DeviceMeta{}, fmt.Errorf("bus %d is closed", vb.busID)
	}
	if vb.nextRegistrationToken == ^uint64(0) {
		return DeviceMeta{}, fmt.Errorf(
			"bus %d registration token exhausted", vb.busID)
	}

	for _, d := range vb.devices {
		if d.dev == dev {
			return DeviceMeta{}, fmt.Errorf(
				"device already registered on this bus")
		}
		if exportBusID != "" {
			if existing, _ := usbip.ExportBusID(d.meta); existing == exportBusID {
				return DeviceMeta{}, fmt.Errorf("USB/IP export alias already registered")
			}
		}
	}
	busID := vb.busID
	devID, available := vb.nextAvailableDeviceIDLocked(maxDeviceID)
	if !available {
		return DeviceMeta{}, fmt.Errorf(
			"bus %d has no available device address through %d",
			vb.busID, maxDeviceID)
	}
	vb.allocatedDevIDs[devID] = true

	busDevID := fmt.Sprintf("%d-%d", busID, devID)
	if exportBusID != "" {
		busDevID = exportBusID
	}
	path := fmt.Sprintf("%s%d/%s", basepath, busID, busDevID)

	var meta usbip.ExportMeta
	copy(meta.Path[:], path)
	copy(meta.USBBusID[:], busDevID)
	meta.BusID = busID
	meta.DevID = devID
	connTimer := time.NewTimer(0)

	ctx, cancel := context.WithCancel(context.Background())
	ctx = context.WithValue(ctx, device.ExportMetaKey, &meta)
	ctx = context.WithValue(ctx, device.ConnTimerKey, connTimer)
	vb.nextRegistrationToken++
	// Cancel the exact empty-bus incarnation only after every fallible Add
	// precondition has passed. A rejected duplicate/nil Add must not disarm the
	// cleanup owner for a bus which remains empty.
	if vb.emptyCancel != nil {
		vb.emptyCancel()
		vb.emptyCancel = nil
		vb.emptyCtx = nil
	}

	lifecycle := &registrationLifecycle{
		removalDone: make(chan struct{}),
		bus:         vb,
		dev:         dev,
		meta:        meta,
		ctx:         ctx,
		token:       vb.nextRegistrationToken,
		active:      true,
	}
	registration := busDevice{
		dev: dev, meta: meta, ctx: ctx, cancel: cancel,
		registrationToken: vb.nextRegistrationToken,
		lifecycle:         lifecycle,
		published:         published,
	}
	vb.devices = append(vb.devices, registration)
	return vb.deviceMetaLocked(registration), nil
}

func validateDeviceIdentity(dev usb.Device) error {
	if dev == nil {
		return fmt.Errorf("device is nil")
	}
	value := reflect.ValueOf(dev)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return fmt.Errorf("device is nil")
		}
	}
	if !value.Comparable() {
		return fmt.Errorf("device identity is not comparable")
	}
	return nil
}

func (vb *VirtualBus) nextAvailableDeviceIDLocked(
	maxDeviceID uint32,
) (uint32, bool) {
	for deviceID := uint32(1); ; deviceID++ {
		if !vb.allocatedDevIDs[deviceID] {
			return deviceID, true
		}
		if deviceID == maxDeviceID {
			return 0, false
		}
	}
}

// GetAllDeviceMetas returns a copy of all registered devices with their descriptors and export metadata.
func (vb *VirtualBus) GetAllDeviceMetas() []DeviceMeta {
	vb.mtx.Lock()
	defer vb.mtx.Unlock()
	out := make([]DeviceMeta, 0, len(vb.devices))
	for _, d := range vb.devices {
		if !deviceDiscoverableLocked(d) {
			continue
		}
		out = append(out, vb.deviceMetaLocked(d))
	}
	return out
}

// GetDeviceByID performs a direct, allocation-free lookup for narrow status
// endpoints. The returned interface remains owned by the bus; callers must not
// retain it across device lifecycle operations.
func (vb *VirtualBus) GetDeviceByID(deviceID uint32) (usb.Device, bool) {
	vb.mtx.Lock()
	defer vb.mtx.Unlock()
	for i := range vb.devices {
		if deviceDiscoverableLocked(vb.devices[i]) &&
			vb.devices[i].meta.DevID == deviceID {
			return vb.devices[i].dev, true
		}
	}
	return nil, false
}

// GetDeviceImportSnapshot returns one device, its immutable-by-value export
// metadata, and its exact cancellation context under the same bus lock. It is
// the import-selection boundary: callers must not reconstruct the selected
// resource by searching for the same device pointer on another bus.
func (vb *VirtualBus) GetDeviceImportSnapshot(
	deviceID uint32,
) (DeviceMeta, context.Context, bool) {
	vb.mtx.Lock()
	defer vb.mtx.Unlock()
	for i := range vb.devices {
		if !deviceDiscoverableLocked(vb.devices[i]) ||
			vb.devices[i].meta.DevID != deviceID {
			continue
		}
		snapshot := vb.deviceMetaLocked(vb.devices[i])
		return snapshot, snapshot.Context, snapshot.Context != nil
	}
	return DeviceMeta{}, nil, false
}

// GetDeviceImportSnapshotByBusID selects only the exact published export
// string. Numeric lookup remains available for management/test callers, but
// USB/IP imports must use this boundary so a protected alias has no fallback.
func (vb *VirtualBus) GetDeviceImportSnapshotByBusID(busID string) (DeviceMeta, bool) {
	if vb == nil || len(busID) == 0 || len(busID) >= len(usbip.ExportMeta{}.USBBusID) {
		return DeviceMeta{}, false
	}
	var expected [32]byte
	copy(expected[:], busID)
	vb.mtx.Lock()
	defer vb.mtx.Unlock()
	for _, device := range vb.devices {
		if deviceDiscoverableLocked(device) && device.meta.USBBusID == expected {
			return vb.deviceMetaLocked(device), true
		}
	}
	return DeviceMeta{}, false
}

// HasUSBIPBusID includes provisional/removing reservations solely for the
// server's collision preflight. It never makes an unpublished device importable.
func (vb *VirtualBus) HasUSBIPBusID(busID string) bool {
	if vb == nil || len(busID) == 0 || len(busID) >= len(usbip.ExportMeta{}.USBBusID) {
		return false
	}
	var expected [32]byte
	copy(expected[:], busID)
	vb.mtx.Lock()
	defer vb.mtx.Unlock()
	for _, device := range vb.devices {
		if device.meta.USBBusID == expected {
			return true
		}
	}
	return false
}

// GetDeviceRegistration returns the exact Add incarnation matching both the
// device object and the context returned by Add. It lets callers convert the
// historical Add result into an opaque registration without an address lookup
// that could select a successor.
func (vb *VirtualBus) GetDeviceRegistration(
	dev usb.Device,
	deviceContext context.Context,
) (DeviceMeta, bool) {
	if vb == nil || deviceContext == nil {
		return DeviceMeta{}, false
	}
	if err := validateDeviceIdentity(dev); err != nil {
		return DeviceMeta{}, false
	}
	vb.mtx.Lock()
	defer vb.mtx.Unlock()
	for i := range vb.devices {
		if registrationActiveLocked(vb.devices[i]) &&
			vb.devices[i].dev == dev && vb.devices[i].ctx == deviceContext {
			return vb.deviceMetaLocked(vb.devices[i]), true
		}
	}
	return DeviceMeta{}, false
}

func (vb *VirtualBus) deviceMetaLocked(device busDevice) DeviceMeta {
	return DeviceMeta{
		Dev: device.dev, Meta: device.meta, Bus: vb, Context: device.ctx,
		RegistrationToken: device.registrationToken,
		lifecycle:         device.lifecycle,
	}
}

// AuthenticatesRegistration atomically verifies one exact Add incarnation.
func (vb *VirtualBus) AuthenticatesRegistration(snapshot DeviceMeta) bool {
	if vb == nil || snapshot.Bus != vb || snapshot.Dev == nil ||
		snapshot.Context == nil || snapshot.RegistrationToken == 0 {
		return false
	}
	if !registrationSnapshotDeviceIdentitySafe(snapshot) {
		return false
	}
	vb.mtx.Lock()
	defer vb.mtx.Unlock()
	if vb.closed {
		return false
	}
	return vb.authenticatesRegistrationLocked(snapshot)
}

// RecognizesRegistration verifies that snapshot is an unmodified exact
// incarnation capability minted by this bus, even after that incarnation has
// entered or completed removal. It does not report liveness. Server-owned
// lifecycle cleanup uses it to retire exact cold-path state after a direct bus
// teardown without accepting caller-mutated exported metadata.
func (vb *VirtualBus) RecognizesRegistration(snapshot DeviceMeta) bool {
	if vb == nil || snapshot.Bus != vb || snapshot.Dev == nil ||
		snapshot.Context == nil || snapshot.RegistrationToken == 0 ||
		!registrationSnapshotDeviceIdentitySafe(snapshot) {
		return false
	}
	vb.mtx.Lock()
	defer vb.mtx.Unlock()
	return vb.registrationIdentityMatchesLocked(snapshot)
}

// PublishRegistration commits one exact provisional Add incarnation to
// address-based discovery. An already-published exact incarnation is an
// idempotent success. A stale snapshot can never publish a successor.
func (vb *VirtualBus) PublishRegistration(snapshot DeviceMeta) (bool, error) {
	if vb == nil || snapshot.Bus != vb || snapshot.Dev == nil ||
		snapshot.Context == nil || snapshot.RegistrationToken == 0 {
		return false, fmt.Errorf("invalid device registration")
	}
	if !registrationSnapshotDeviceIdentitySafe(snapshot) {
		return false, fmt.Errorf("invalid device registration identity")
	}
	vb.mtx.Lock()
	defer vb.mtx.Unlock()
	if vb.closed {
		return false, nil
	}
	for i := range vb.devices {
		device := &vb.devices[i]
		if device.lifecycle != snapshot.lifecycle ||
			!vb.authenticatesRegistrationLocked(snapshot) {
			continue
		}
		device.published = true
		return true, nil
	}
	return false, nil
}

// RegistrationOperationLease fences removal/re-add while one exact
// registration-owned operation is in progress. It is a single-owner, move-only
// value: do not copy or share it after acquisition, and call Release exactly
// once from the owning goroutine. A zero lease may be released as a no-op. The
// lease contains no device pointer and cannot authenticate another
// incarnation.
type RegistrationOperationLease struct {
	noCopy    noCopy
	lifecycle *registrationLifecycle
}

// AcquireRegistrationOperation authenticates an opaque per-incarnation
// capability in O(1), then takes its read fence without holding the bus lock.
// It revalidates after acquiring the fence. Removal first marks the exact
// incarnation under the bus lock and waits for the write fence without that
// lock, so operation code may safely reenter read-only bus APIs and can never
// resume in a same-pointer successor incarnation.
func (vb *VirtualBus) AcquireRegistrationOperation(
	snapshot DeviceMeta,
) (RegistrationOperationLease, bool) {
	if vb == nil || snapshot.Bus != vb || snapshot.Dev == nil ||
		snapshot.Context == nil || snapshot.RegistrationToken == 0 ||
		snapshot.lifecycle == nil {
		return RegistrationOperationLease{}, false
	}
	if !registrationSnapshotDeviceIdentitySafe(snapshot) {
		return RegistrationOperationLease{}, false
	}
	vb.mtx.Lock()
	if vb.closed || !vb.authenticatesRegistrationLocked(snapshot) {
		vb.mtx.Unlock()
		return RegistrationOperationLease{}, false
	}
	lifecycle := snapshot.lifecycle
	vb.mtx.Unlock()

	lifecycle.mu.RLock()
	vb.mtx.Lock()
	active := !vb.closed && vb.authenticatesRegistrationLocked(snapshot)
	vb.mtx.Unlock()
	if !active {
		lifecycle.mu.RUnlock()
		return RegistrationOperationLease{}, false
	}
	return RegistrationOperationLease{lifecycle: lifecycle}, true
}

// Release ends a single-owner exact registration operation. It must not be
// called concurrently or on copied nonzero lease values.
func (lease *RegistrationOperationLease) Release() {
	if lease == nil || lease.lifecycle == nil {
		return
	}
	lifecycle := lease.lifecycle
	lease.lifecycle = nil
	lifecycle.mu.RUnlock()
}

// noCopy lets go vet diagnose copies of synchronization ownership values.
// See the convention used by the standard library's sync package.
type noCopy struct{}

func (*noCopy) Lock()   {}
func (*noCopy) Unlock() {}

// BusID returns the bus number for this VirtualBus.
func (vb *VirtualBus) BusID() uint32 {
	vb.mtx.Lock()
	defer vb.mtx.Unlock()
	return vb.busID
}

// IsClosed reports whether device admission has been permanently fenced.
func (vb *VirtualBus) IsClosed() bool {
	if vb == nil {
		return true
	}
	vb.mtx.Lock()
	defer vb.mtx.Unlock()
	return vb.closed
}

// Devices returns all devices currently attached to this bus.
func (vb *VirtualBus) Devices() []usb.Device {
	vb.mtx.Lock()
	defer vb.mtx.Unlock()
	out := make([]usb.Device, 0, len(vb.devices))
	for _, d := range vb.devices {
		if !deviceDiscoverableLocked(d) {
			continue
		}
		out = append(out, d.dev)
	}
	return out
}

func (vb *VirtualBus) GetBusEmptyContext() context.Context {
	vb.mtx.Lock()
	defer vb.mtx.Unlock()
	if vb.closed {
		return nil
	}
	if len(vb.devices) > 0 {
		return nil
	}
	if vb.emptyCtx == nil {
		vb.emptyCtx, vb.emptyCancel = context.WithCancel(context.Background())
	}
	return vb.emptyCtx
}

// CloseIfEmptyContext permanently closes this bus only when expected is the
// still-current empty incarnation created by GetBusEmptyContext. A device Add,
// even if followed by another empty period, cancels/replaces that incarnation
// and makes a stale cleanup callback a no-op.
func (vb *VirtualBus) CloseIfEmptyContext(
	expected context.Context,
) (bool, error) {
	if vb == nil || expected == nil {
		return false, fmt.Errorf("invalid empty-bus context")
	}
	vb.mtx.Lock()
	if vb.closed || len(vb.devices) != 0 || vb.emptyCtx != expected ||
		expected.Err() != nil {
		vb.mtx.Unlock()
		return false, nil
	}
	vb.closed = true
	if vb.emptyCancel != nil {
		vb.emptyCancel()
		vb.emptyCancel = nil
		vb.emptyCtx = nil
	}
	busID := vb.busID
	vb.mtx.Unlock()

	globalMtx.Lock()
	delete(allocatedBusIds, busID)
	globalMtx.Unlock()
	close(vb.closeDone)
	return true, nil
}

// RemoveDeviceByID removes a device by its  ID (e.g., "1").
// Returns error if not found.
func (vb *VirtualBus) RemoveDeviceByID(deviceID string) error {
	_, err := vb.RemoveDeviceByIDSnapshot(deviceID)
	return err
}

// RemoveDeviceByIDSnapshot removes and returns the exact device selected under
// the bus lock. Callers which own external per-device state must use this
// atomic result rather than searching before a separately locked removal.
func (vb *VirtualBus) RemoveDeviceByIDSnapshot(
	deviceID string,
) (usb.Device, error) {
	snapshot, err := vb.RemoveDeviceRegistrationByIDSnapshot(deviceID)
	return snapshot.Dev, err
}

// RemoveDeviceRegistrationByIDSnapshot removes and returns the exact Add
// incarnation selected under the bus lock.
func (vb *VirtualBus) RemoveDeviceRegistrationByIDSnapshot(
	deviceID string,
) (DeviceMeta, error) {
	vb.mtx.Lock()
	for i, d := range vb.devices {
		if !d.published || fmt.Sprintf("%d", d.meta.DevID) != deviceID {
			continue
		}
		if d.lifecycle != nil && d.lifecycle.removing {
			done := d.lifecycle.removalDone
			vb.mtx.Unlock()
			<-done
			return vb.deviceMetaLocked(d), nil
		}
		if registrationActiveLocked(d) {
			marked := vb.markRegistrationRemovalLocked(i)
			vb.mtx.Unlock()
			vb.finishMarkedRegistrationRemoval(marked)
			return marked.snapshot, nil
		}
	}
	busID := vb.busID
	vb.mtx.Unlock()
	return DeviceMeta{}, fmt.Errorf(
		"device with id %s not found on bus %d", deviceID, busID)
}

// Remove unregisters a device from the bus.
// This removes the device from the internal list; it does not currently free
// the global bus number. Removal should be used for dynamic device teardown
// during runtime.
func (vb *VirtualBus) Remove(dev usb.Device) error {
	removed, err := vb.RemoveIfPresent(dev)
	if err != nil {
		return err
	}
	if removed {
		return nil
	}
	return fmt.Errorf("device not found")
}

// RemoveIfPresent atomically removes only the exact device object when it is
// still registered. Absence is a successful no-op; in particular, a different
// device which reused the old USB address is never selected by address.
func (vb *VirtualBus) RemoveIfPresent(dev usb.Device) (bool, error) {
	if vb == nil {
		return false, fmt.Errorf("bus is nil")
	}
	if err := validateDeviceIdentity(dev); err != nil {
		return false, err
	}
	vb.mtx.Lock()
	for i, d := range vb.devices {
		if d.dev != dev {
			continue
		}
		if d.lifecycle != nil && d.lifecycle.removing {
			done := d.lifecycle.removalDone
			vb.mtx.Unlock()
			<-done
			return false, nil
		}
		if registrationActiveLocked(d) {
			marked := vb.markRegistrationRemovalLocked(i)
			vb.mtx.Unlock()
			vb.finishMarkedRegistrationRemoval(marked)
			return true, nil
		}
	}
	vb.mtx.Unlock()
	return false, nil
}

// RemoveRegistrationIfPresent removes only the exact Add incarnation. An
// absent registration is a successful no-op; a re-add of the same device
// pointer has a different token and is preserved.
func (vb *VirtualBus) RemoveRegistrationIfPresent(
	snapshot DeviceMeta,
) (bool, error) {
	if vb == nil || snapshot.Bus != vb || snapshot.Dev == nil ||
		snapshot.RegistrationToken == 0 || snapshot.lifecycle == nil {
		return false, fmt.Errorf("invalid device registration")
	}
	if !registrationSnapshotDeviceIdentitySafe(snapshot) {
		return false, fmt.Errorf("invalid device registration identity")
	}
	vb.mtx.Lock()
	if !vb.registrationIdentityMatchesLocked(snapshot) {
		vb.mtx.Unlock()
		return false, nil
	}
	if snapshot.lifecycle.removing || !snapshot.lifecycle.active {
		done := snapshot.lifecycle.removalDone
		vb.mtx.Unlock()
		if done != nil {
			<-done
		}
		return false, nil
	}
	for i, device := range vb.devices {
		if device.lifecycle != snapshot.lifecycle ||
			!vb.authenticatesRegistrationLocked(snapshot) {
			continue
		}
		marked := vb.markRegistrationRemovalLocked(i)
		vb.mtx.Unlock()
		vb.finishMarkedRegistrationRemoval(marked)
		return true, nil
	}
	vb.mtx.Unlock()
	return false, nil
}

// CloseAndTakeDevices atomically closes device admission, cancels and removes
// every registered device, and returns the exact removed device objects. The
// closed flag and drain share the Add lock, so an Add either precedes the drain
// and is returned here or observes the closed bus and fails.
func (vb *VirtualBus) CloseAndTakeDevices() ([]usb.Device, error) {
	registrations, err := vb.CloseAndTakeRegistrations()
	if err != nil {
		return nil, err
	}
	removed := make([]usb.Device, 0, len(registrations))
	for _, registration := range registrations {
		removed = append(removed, registration.Dev)
	}
	return removed, nil
}

// CloseAndTakeRegistrations atomically closes Add admission, cancels and
// removes every registration, and returns their exact incarnation snapshots.
func (vb *VirtualBus) CloseAndTakeRegistrations() ([]DeviceMeta, error) {
	if vb == nil {
		return nil, fmt.Errorf("bus is nil")
	}
	vb.mtx.Lock()
	if vb.closed {
		done := vb.closeDone
		vb.mtx.Unlock()
		if done != nil {
			<-done
		}
		return nil, nil
	}
	vb.closed = true

	marked := make([]markedRegistrationRemoval, 0, len(vb.devices))
	waiting := make([]<-chan struct{}, 0)
	for i := range vb.devices {
		device := &vb.devices[i]
		if device.lifecycle == nil || device.lifecycle.removing {
			if device.lifecycle != nil && device.lifecycle.removalDone != nil {
				waiting = append(waiting, device.lifecycle.removalDone)
			}
			continue
		}
		marked = append(marked, vb.markRegistrationRemovalLocked(i))
	}
	if vb.emptyCancel != nil {
		vb.emptyCancel()
		vb.emptyCancel = nil
		vb.emptyCtx = nil
	}
	busID := vb.busID
	vb.mtx.Unlock()

	removed := make([]DeviceMeta, 0, len(marked))
	for _, registration := range marked {
		vb.finishMarkedRegistrationRemoval(registration)
		removed = append(removed, registration.snapshot)
	}
	for _, done := range waiting {
		<-done
	}
	vb.mtx.Lock()
	clear(vb.allocatedDevIDs)
	vb.mtx.Unlock()

	globalMtx.Lock()
	delete(allocatedBusIds, busID)
	globalMtx.Unlock()
	close(vb.closeDone)
	return removed, nil
}

// Close frees the bus number allocated to this VirtualBus, allowing it to be
// reused. Close is idempotent and permanently closes device admission.
func (vb *VirtualBus) Close() error {
	_, err := vb.CloseAndTakeDevices()
	return err
}

// Note: Contexts are owned by the bus and created at Add(). They are cancelled
// when a device is removed or the bus is closed.

// GetDeviceContext returns the context for a specific device.
// Returns nil if the device is not found or has no active context.
func (vb *VirtualBus) GetDeviceContext(dev usb.Device) context.Context {
	if vb == nil {
		return nil
	}
	if err := validateDeviceIdentity(dev); err != nil {
		return nil
	}
	vb.mtx.Lock()
	defer vb.mtx.Unlock()
	for i := range vb.devices {
		if deviceDiscoverableLocked(vb.devices[i]) && vb.devices[i].dev == dev {
			return vb.devices[i].ctx
		}
	}
	return nil
}

type busDevice struct {
	dev               usb.Device
	meta              usbip.ExportMeta
	ctx               context.Context
	cancel            context.CancelFunc
	registrationToken uint64
	lifecycle         *registrationLifecycle
	published         bool
}

type registrationLifecycle struct {
	mu          sync.RWMutex
	removalDone chan struct{}
	bus         *VirtualBus
	dev         usb.Device
	meta        usbip.ExportMeta
	ctx         context.Context
	token       uint64
	// active and removing are protected by the owning VirtualBus mutex.
	active   bool
	removing bool
}

type markedRegistrationRemoval struct {
	snapshot  DeviceMeta
	lifecycle *registrationLifecycle
}

func registrationActiveLocked(device busDevice) bool {
	return device.lifecycle != nil && device.lifecycle.active &&
		!device.lifecycle.removing
}

func deviceDiscoverableLocked(device busDevice) bool {
	return device.published && registrationActiveLocked(device)
}

func (vb *VirtualBus) authenticatesRegistrationLocked(
	snapshot DeviceMeta,
) bool {
	lifecycle := snapshot.lifecycle
	return vb.registrationIdentityMatchesLocked(snapshot) && lifecycle.active &&
		!lifecycle.removing
}

func (vb *VirtualBus) registrationIdentityMatchesLocked(
	snapshot DeviceMeta,
) bool {
	lifecycle := snapshot.lifecycle
	return lifecycle != nil && lifecycle.bus == vb &&
		lifecycle.dev == snapshot.Dev && lifecycle.meta == snapshot.Meta &&
		lifecycle.ctx == snapshot.Context &&
		lifecycle.token == snapshot.RegistrationToken
}

// registrationSnapshotDeviceIdentitySafe cheaply proves the common pointer
// case before any interface equality. Non-pointer values take the reflective
// recursive comparability check so a forged interface-containing value cannot
// panic exact-registration APIs. The private lifecycle capability supplies
// the expected dynamic type.
func registrationSnapshotDeviceIdentitySafe(snapshot DeviceMeta) bool {
	lifecycle := snapshot.lifecycle
	if lifecycle == nil || snapshot.Dev == nil || lifecycle.dev == nil {
		return false
	}
	candidateType := reflect.TypeOf(snapshot.Dev)
	if candidateType != reflect.TypeOf(lifecycle.dev) {
		return false
	}
	candidate := reflect.ValueOf(snapshot.Dev)
	if candidateType.Kind() == reflect.Pointer {
		return !candidate.IsNil()
	}
	return candidate.Comparable()
}

func (vb *VirtualBus) markRegistrationRemovalLocked(
	index int,
) markedRegistrationRemoval {
	device := &vb.devices[index]
	device.lifecycle.removing = true
	return markedRegistrationRemoval{
		snapshot: vb.deviceMetaLocked(*device), lifecycle: device.lifecycle,
	}
}

// finishMarkedRegistrationRemoval waits for exact-incarnation operations
// without holding the bus lock, then cancels and removes only the marked
// lifecycle. Marking prevents any later operation or remover from selecting
// it, while the private lifecycle pointer prevents same-address or
// same-device-pointer successors from being selected.
func (vb *VirtualBus) finishMarkedRegistrationRemoval(
	marked markedRegistrationRemoval,
) {
	lifecycle := marked.lifecycle
	if lifecycle == nil {
		return
	}
	lifecycle.mu.Lock()
	vb.mtx.Lock()
	for i := range vb.devices {
		device := &vb.devices[i]
		if device.lifecycle != lifecycle {
			continue
		}
		if device.cancel != nil {
			device.cancel()
		}
		delete(vb.allocatedDevIDs, device.meta.DevID)
		vb.devices = append(vb.devices[:i], vb.devices[i+1:]...)
		lifecycle.active = false
		break
	}
	vb.mtx.Unlock()
	lifecycle.mu.Unlock()
	close(lifecycle.removalDone)
}
