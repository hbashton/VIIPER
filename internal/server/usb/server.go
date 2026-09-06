package usb

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Alia5/VIIPER/internal/inputpresentation"
	"github.com/Alia5/VIIPER/internal/log"
	"github.com/Alia5/VIIPER/internal/retainedusb"
	"github.com/Alia5/VIIPER/usb"
	"github.com/Alia5/VIIPER/usbip"
	"github.com/Alia5/VIIPER/virtualbus"
)

type batchingWriter struct {
	mu           sync.Mutex
	w            *bufio.Writer
	flushEvery   time.Duration
	flushAtBytes int
	stopCh       chan struct{}
	closeOnce    sync.Once
	err          error
}

const (
	retSubmitHeaderSize = 0x30

	// avoid windows socket overhead while keeping latency very low.
	writeBatcherBufferSize   = 256 * 1024
	writeBatcherFlushAtBytes = 64 * 1024
)

func newBatchingWriter(dst io.Writer, bufSize int, flushEvery time.Duration, flushAtBytes int) *batchingWriter {
	if bufSize <= 0 {
		bufSize = writeBatcherBufferSize
	}
	if flushAtBytes < 0 {
		flushAtBytes = 0
	}
	if flushAtBytes > bufSize {
		flushAtBytes = bufSize
	}
	bw := &batchingWriter{
		w:            bufio.NewWriterSize(dst, bufSize),
		flushEvery:   flushEvery,
		flushAtBytes: flushAtBytes,
		stopCh:       make(chan struct{}),
	}
	if flushEvery > 0 {
		go bw.flushLoop()
	}
	return bw
}

func (b *batchingWriter) flushLoop() {
	t := time.NewTicker(b.flushEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			_ = b.Flush()
		case <-b.stopCh:
			return
		}
	}
}

func (b *batchingWriter) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return 0, b.err
	}

	n, err := b.w.Write(p)
	if err != nil {
		b.err = err
		return n, err
	}
	if b.flushAtBytes > 0 && b.w.Buffered() >= b.flushAtBytes {
		if err := b.w.Flush(); err != nil {
			b.err = err
			return n, err
		}
	}
	return n, nil
}

func (b *batchingWriter) Flush() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return b.err
	}
	if err := b.w.Flush(); err != nil {
		b.err = err
		return err
	}
	return nil
}

func (b *batchingWriter) Close() error {
	b.closeOnce.Do(func() {
		close(b.stopCh)
	})
	return b.Flush()
}

const (
	// USB standard request codes
	usbReqGetStatus        = 0x00
	usbReqClearFeature     = 0x01
	usbReqSetFeature       = 0x03
	usbReqSetAddress       = 0x05
	usbReqGetDescriptor    = 0x06
	usbReqSetDescriptor    = 0x07
	usbReqGetConfiguration = 0x08
	usbReqSetConfiguration = 0x09
	usbReqGetInterface     = 0x0A
	usbReqSetInterface     = 0x0B

	// USB descriptor types
	usbDescTypeDevice        = 0x01
	usbDescTypeConfiguration = 0x02
	usbDescTypeString        = 0x03
	usbDescTypeHID           = 0x21
	usbDescTypeHIDReport     = 0x22

	// USB request types (bmRequestType)
	usbReqTypeStandardToDevice      = 0x00
	usbReqTypeStandardFromInterface = 0x01
	usbReqTypeStandardToEndpoint    = 0x02
	usbReqTypeStandardToInterface   = 0x81
	usbReqTypeStandardFromDevice    = 0x80
	usbReqTypeMask                  = 0x60
	usbReqTypeClass                 = 0x20

	// USB interface classes
	usbInterfaceClassHID = 0x03

	// HID class requests (bRequest)
	hidReqGetReport   = 0x01
	hidReqGetIdle     = 0x02
	hidReqGetProtocol = 0x03
	hidReqSetReport   = 0x09
	hidReqSetIdle     = 0x0A
	hidReqSetProtocol = 0x0B

	// HID class request types (bmRequestType)
	hidReqTypeIn  = 0xA1
	hidReqTypeOut = 0x21

	// wIndex low-byte interface selector mask.
	usbIfaceIndexMask = 0x00FF

	// USB configuration values
	usbConfigValueDefault   = 1
	usbConfigAttrBusPowered = 0x80
	usbConfigMaxPower100mA  = 50 // In units of 2mA

	// URB header field offsets
	urbHdrSize          = 0x30
	urbHdrOffsetCommand = 0x00
	urbHdrOffsetSeqnum  = 0x04
	urbHdrOffsetDevid   = 0x08
	urbHdrOffsetDir     = 0x0c
	urbHdrOffsetEp      = 0x10
	urbHdrOffsetUnlink  = 0x14
	urbHdrOffsetFlags   = 0x14
	urbHdrOffsetLength  = 0x18
	urbHdrOffsetPackets = 0x20
	urbHdrOffsetSetup   = 0x28
	maxIsoPackets       = 1024

	// Standard header peek size
	headerPeekSize = 8

	// BUSID buffer size for import
	busIDSize = 32

	// Error codes
	errConnReset = -104 // -ECONNRESET
	errPipe      = -32  // -EPIPE: endpoint zero STALL
)

type controlLifecycleKind uint8

const (
	controlLifecycleNone controlLifecycleKind = iota
	controlLifecycleClearEndpointHalt
	controlLifecycleSetInterface
	controlLifecycleSetConfiguration
)

// controlLifecycleSetup is the single parse/validation result shared by the
// endpoint-presentation scheduler and the emulated device. Scheduler retirement
// must happen before the corresponding device reset, but only an exact accepted
// standard request may create that boundary.
type controlLifecycleSetup struct {
	kind               controlLifecycleKind
	recognized         bool
	accepted           bool
	endpointAddress    uint8
	interfaceNumber    uint8
	alternateSetting   uint8
	configurationValue uint8
}

func (s *Server) parseControlLifecycleSetup(
	dev usb.Device,
	setup []byte,
) controlLifecycleSetup {
	if dev == nil {
		return controlLifecycleSetup{}
	}
	return s.parseControlLifecycleSetupDescriptor(dev, dev.GetDescriptor(), setup)
}

func (s *Server) parseControlLifecycleSetupDescriptor(
	dev usb.Device,
	desc *usb.Descriptor,
	setup []byte,
) controlLifecycleSetup {
	if len(setup) != 8 {
		return controlLifecycleSetup{}
	}
	bmRequestType := setup[0]
	bRequest := setup[1]
	wValue := binary.LittleEndian.Uint16(setup[2:4])
	wIndex := binary.LittleEndian.Uint16(setup[4:6])
	wLength := binary.LittleEndian.Uint16(setup[6:8])
	switch {
	case bmRequestType == usbReqTypeStandardToEndpoint &&
		bRequest == usbReqClearFeature:
		result := controlLifecycleSetup{
			kind: controlLifecycleClearEndpointHalt, recognized: true,
		}
		if wValue != 0 || wIndex>>8 != 0 || wLength != 0 ||
			!s.descriptorHasActiveHaltEndpoint(
				dev, desc, uint8(wIndex)) {
			return result
		}
		result.accepted = true
		result.endpointAddress = uint8(wIndex)
		return result

	case bmRequestType == usbReqTypeStandardFromInterface &&
		bRequest == usbReqSetInterface:
		result := controlLifecycleSetup{
			kind: controlLifecycleSetInterface, recognized: true,
		}
		if wValue>>8 != 0 || wIndex>>8 != 0 || wLength != 0 {
			return result
		}
		result.interfaceNumber = uint8(wIndex)
		result.alternateSetting = uint8(wValue)
		if s.getDeviceConfiguration(dev) == 0 ||
			!descriptorHasInterfaceAlt(
				desc, result.interfaceNumber, result.alternateSetting,
			) {
			return result
		}
		result.accepted = true
		return result

	case bmRequestType == usbReqTypeStandardToDevice &&
		bRequest == usbReqSetConfiguration:
		result := controlLifecycleSetup{
			kind: controlLifecycleSetConfiguration, recognized: true,
		}
		configurationValue := descriptorConfigurationValue(desc)
		if wIndex != 0 || wLength != 0 ||
			(wValue != 0 && wValue != uint16(configurationValue)) {
			return result
		}
		result.accepted = true
		result.configurationValue = uint8(wValue)
		return result
	}

	return controlLifecycleSetup{}
}

// parseControlLifecycleSubmission binds a syntactically recognized setup
// packet to its USB/IP transport envelope. The eight setup bytes in a
// non-control URB are reserved padding, not a control request. Likewise, a
// host-to-device lifecycle request cannot arrive in a USB/IP DirIn submit.
// These lifecycle requests all require a zero-length data stage; a mismatched
// transfer length is recognized-but-rejected so EP0 returns STALL without
// retiring presentation state.
func (s *Server) parseControlLifecycleSubmission(
	dev usb.Device,
	ep, dir, transferLength uint32,
	setup []byte,
) controlLifecycleSetup {
	if dev == nil {
		return controlLifecycleSetup{}
	}
	return s.parseControlLifecycleSubmissionDescriptor(
		dev, dev.GetDescriptor(), ep, dir, transferLength, setup)
}

func (s *Server) parseControlLifecycleSubmissionDescriptor(
	dev usb.Device,
	desc *usb.Descriptor,
	ep, dir, transferLength uint32,
	setup []byte,
) controlLifecycleSetup {
	if ep != 0 {
		return controlLifecycleSetup{}
	}
	lifecycle := s.parseControlLifecycleSetupDescriptor(dev, desc, setup)
	if lifecycle.recognized &&
		(dir != usbip.DirOut || transferLength != 0) {
		lifecycle.accepted = false
	}
	return lifecycle
}

func applyControlLifecycleToSchedulers(
	schedulers *endpointSchedulers,
	lifecycle controlLifecycleSetup,
) {
	if schedulers == nil || !lifecycle.accepted {
		return
	}
	switch lifecycle.kind {
	case controlLifecycleClearEndpointHalt:
		schedulers.resetEndpoint(lifecycle.endpointAddress)
	case controlLifecycleSetInterface:
		schedulers.resetInterface(lifecycle.interfaceNumber)
	case controlLifecycleSetConfiguration:
		schedulers.resetAll()
	}
}

type Server struct {
	failedImportObserverMu sync.RWMutex
	failedImportObserver   func(string)
	config                 *ServerConfig
	logger                 *slog.Logger
	rawLogger              log.RawLogger
	busses                 map[uint32]*virtualbus.VirtualBus
	busesMu                sync.Mutex
	// Server-lifetime, bounded GIP identity reservations, protected by busesMu.
	// USB/IP drain/removal is not proof Windows has removed its old PDO; never
	// recycle its Hello lookup key when a bus or registration is removed.
	usedPrimaryGIPDeviceIDs map[uint64]struct{}
	busRemovals             map[uint32]*serverBusRemoval
	// beforeBusRemovalComplete is nil in production and lets package tests
	// pause after device/admission drain but before duplicate RemoveBus callers
	// are released.
	beforeBusRemovalComplete func()
	// beforeRegisteredRetainedAdmission and afterRetainedRegistrationWatchForget
	// are nil in production and expose deterministic boundaries for teardown
	// race tests.
	beforeRegisteredRetainedAdmission    func()
	afterRetainedRegistrationWatchForget func()
	beforeRetainedRegistrationRollback   func()
	alts                                 map[usb.Device]map[uint8]uint8
	altsMu                               sync.Mutex
	configurations                       map[usb.Device]uint8
	configurationsMu                     sync.Mutex
	ready                                chan struct{}
	readyOnce                            sync.Once
	ln                                   net.Listener
	diagnosticsMu                        sync.Mutex
	diagnosticConnections                map[*endpointSchedulers]struct{}
	importsMu                            sync.Mutex
	activeImports                        map[usb.Device]uint64
	nextImportLease                      uint64
	retainedImports                      *retainedImportAuthority
	retainedFailureMu                    sync.Mutex
	retainedDeviceAdmissions             map[retainedDeviceAdmissionKey]*retainedDeviceAdmission
	retainedOwnerAdmissions              map[retainedImportOwnerReference]*retainedDeviceAdmission
	serverLifecycleMu                    sync.Mutex
	serverClosing                        bool
	serverClosingSignal                  chan struct{}
	serverCloseOnce                      sync.Once
	serverCloseErr                       error
	serverShutdownDeadline               time.Time
	nextRetainedStream                   uint64
	retainedStreams                      map[uint64]*retainedServerStream
	retainedRegistrationClosures         map[retainedDeviceAdmissionKey]*retainedRegistrationClosure
	retainedStreamsWG                    sync.WaitGroup
	retainedCallbacksWG                  sync.WaitGroup
	nextServerConnection                 uint64
	serverConnections                    map[uint64]*trackedServerConnection
	serverConnectionsWG                  sync.WaitGroup
	serveWG                              sync.WaitGroup
}

type retainedServerStream struct {
	cancel              context.CancelFunc
	terminalErr         error
	done                chan struct{}
	registrationClosure *retainedRegistrationClosure
}

type trackedServerConnection struct {
	closeConn   func() error
	terminalErr error
}

type serverBusRemoval struct {
	done chan struct{}
	err  error
}

// serverOwnedConnection is the one idempotent Close authority shared by
// shutdown tracking, the handler defer, retained cancellation, and retained
// ingress. The first raw Close result is cached and replayed, so an arbitrary
// failure cannot be discarded by a later net.ErrClosed.
type serverOwnedConnection struct {
	net.Conn
	closeOnce sync.Once
	closeDone chan struct{}
	closeErr  error
}

func newServerOwnedConnection(conn net.Conn) *serverOwnedConnection {
	return &serverOwnedConnection{Conn: conn, closeDone: make(chan struct{})}
}

func (conn *serverOwnedConnection) Close() error {
	if conn == nil || conn.Conn == nil {
		return errRetainedImportInvalid
	}
	conn.closeOnce.Do(func() {
		conn.closeErr = conn.Conn.Close()
		close(conn.closeDone)
	})
	<-conn.closeDone
	return conn.closeErr
}

func New(config ServerConfig, logger *slog.Logger, rawLogger log.RawLogger) *Server {
	server := &Server{
		config:                &config,
		logger:                logger,
		rawLogger:             rawLogger,
		busses:                make(map[uint32]*virtualbus.VirtualBus),
		busRemovals:           make(map[uint32]*serverBusRemoval),
		alts:                  make(map[usb.Device]map[uint8]uint8),
		configurations:        make(map[usb.Device]uint8),
		ready:                 make(chan struct{}),
		diagnosticConnections: make(map[*endpointSchedulers]struct{}),
		activeImports:         make(map[usb.Device]uint64),
		retainedDeviceAdmissions: make(
			map[retainedDeviceAdmissionKey]*retainedDeviceAdmission),
		retainedOwnerAdmissions: make(
			map[retainedImportOwnerReference]*retainedDeviceAdmission),
		retainedStreams:              make(map[uint64]*retainedServerStream),
		retainedRegistrationClosures: make(map[retainedDeviceAdmissionKey]*retainedRegistrationClosure),
		serverConnections:            make(map[uint64]*trackedServerConnection),
		serverClosingSignal:          make(chan struct{}),
	}
	if config.RetainedImportAuthorityID != 0 {
		// The constructor can reject only a zero authority identity; the guard
		// above therefore keeps New's historical no-error API while still using
		// the canonical authority constructor.
		server.retainedImports, _ = newRetainedImportAuthority(
			config.RetainedImportAuthorityID, 0, 0)
	}
	return server
}

// RetainedImportAuthorityID returns the immutable, explicit retained-import
// opt-in configured for this server. The value is a local construction
// capability identifier, not an authentication secret; API authentication and
// the typed one-shot persona authorization remain separate boundaries.
func (s *Server) RetainedImportAuthorityID() (uint64, bool) {
	if s == nil || s.retainedImports == nil || s.retainedImports.identity == 0 {
		return 0, false
	}
	return s.retainedImports.identity, true
}

// AddBus registers a bus with the server. If the bus number is already present,
// an error is returned.
func (s *Server) AddBus(bus *virtualbus.VirtualBus) error {
	s.busesMu.Lock()
	defer s.busesMu.Unlock()
	s.serverLifecycleMu.Lock()
	defer s.serverLifecycleMu.Unlock()
	if s.serverClosing {
		return net.ErrClosed
	}
	if bus == nil {
		return fmt.Errorf("bus is nil")
	}
	if bus.IsClosed() {
		return fmt.Errorf("bus %d is closed", bus.BusID())
	}
	if _, ok := s.busses[bus.BusID()]; ok {
		return fmt.Errorf("bus %d already registered", bus.BusID())
	}
	s.busses[bus.BusID()] = bus
	// Production management may create a bus and then reject its device
	// factory. Positive configured cleanup budgets therefore also cover the
	// initial empty incarnation, without any client numeric-removal fallback.
	// Preserve historical zero-budget embedding behavior on initial Add.
	if s.config.BusCleanupTimeout > 0 {
		s.scheduleEmptyBusCleanup(bus.BusID(), bus)
	}
	return nil
}

// ValidateRetainedDeviceRegistrationTarget performs the non-consuming half of
// an explicit retained-device registration. It exists so a caller which owns a
// one-shot product authorization can reject ambient server/topology mistakes
// before consuming that authorization. AddRetainedDeviceRegistration repeats
// the same checks atomically with Add; this preflight is not a reservation.
func (s *Server) ValidateRetainedDeviceRegistrationTarget(
	busID uint32,
	authorityID uint64,
) error {
	if s == nil {
		return errRetainedImportInvalid
	}
	s.busesMu.Lock()
	s.serverLifecycleMu.Lock()
	bus, err := s.validateRetainedDeviceRegistrationTargetLocked(
		busID, authorityID)
	if err == nil && !bus.CanAddRepresentableRegistration(
		s.retainedImportDeviceAddressLimit()) {
		err = fmt.Errorf(
			"%w: retained USB/IP registration capacity is unavailable",
			errRetainedImportInvalid)
	}
	s.serverLifecycleMu.Unlock()
	s.busesMu.Unlock()
	return err
}

// AddRetainedDeviceRegistration authenticates the server's exact opt-in
// authority and current bus, adds one retained-only device provisionally,
// admits and seals its descriptor while it is undiscoverable, and atomically
// publishes only that exact incarnation. The USB/IP bus and device components
// must both be nonzero 16-bit values. Capability/owner callbacks remain import
// owned and cannot run before publication.
func (s *Server) AddRetainedDeviceRegistration(
	busID uint32,
	authorityID uint64,
	dev usb.Device,
) (virtualbus.DeviceMeta, error) {
	return s.addRetainedDeviceRegistration(busID, authorityID, dev, "")
}

// AddProductionXboxOneRetainedDeviceRegistration issues a fresh export alias
// before the canonical retained descriptor/owner registration is published.
func (s *Server) AddProductionXboxOneRetainedDeviceRegistration(
	busID uint32, authorityID uint64, dev usb.Device,
) (virtualbus.DeviceMeta, error) {
	alias, err := usbip.NewProductionXboxOneBusID()
	if err != nil {
		return virtualbus.DeviceMeta{}, err
	}
	return s.addRetainedDeviceRegistration(busID, authorityID, dev, alias)
}

func (s *Server) addRetainedDeviceRegistration(
	busID uint32, authorityID uint64, dev usb.Device, alias string,
) (virtualbus.DeviceMeta, error) {
	if s == nil || dev == nil {
		return virtualbus.DeviceMeta{}, errRetainedImportInvalid
	}
	if _, valid := exactRetainedImportOwnerReference(dev); !valid {
		return virtualbus.DeviceMeta{}, errRetainedImportInvalid
	}
	if _, retained := dev.(retainedusb.ImportDevice); !retained {
		return virtualbus.DeviceMeta{}, errRetainedImportInvalid
	}
	// Callback reads stay outside global locks, including for rejected inputs.
	gipID, err := retainedPrimaryGIPIdentity(dev, alias != "")
	if err != nil {
		return virtualbus.DeviceMeta{}, err
	}

	s.busesMu.Lock()
	s.serverLifecycleMu.Lock()
	bus, err := s.validateRetainedDeviceRegistrationTargetLocked(
		busID, authorityID)
	if err != nil {
		s.serverLifecycleMu.Unlock()
		s.busesMu.Unlock()
		return virtualbus.DeviceMeta{}, err
	}
	if gipID != 0 {
		if err := s.validateUnusedPrimaryGIPIdentityLocked(gipID); err != nil {
			s.serverLifecycleMu.Unlock()
			s.busesMu.Unlock()
			return virtualbus.DeviceMeta{}, err
		}
	}
	var registration virtualbus.DeviceMeta
	if alias == "" {
		registration, err = bus.AddProvisionalRegistrationThrough(dev, s.retainedImportDeviceAddressLimit())
	} else {
		// A collision must fail closed, even though independent 128-bit
		// cryptographic issuance makes one overwhelmingly unlikely.
		for _, candidate := range s.busses {
			if candidate.HasUSBIPBusID(alias) {
				s.serverLifecycleMu.Unlock()
				s.busesMu.Unlock()
				return virtualbus.DeviceMeta{}, fmt.Errorf("duplicate USB/IP export alias")
			}
		}
		registration, err = bus.AddProvisionalRegistrationWithXboxOneBusIDThrough(dev, s.retainedImportDeviceAddressLimit(), alias)
	}
	if err != nil {
		s.serverLifecycleMu.Unlock()
		s.busesMu.Unlock()
		return virtualbus.DeviceMeta{}, err
	}
	if gipID != 0 {
		if s.usedPrimaryGIPDeviceIDs == nil {
			s.usedPrimaryGIPDeviceIDs = make(map[uint64]struct{})
		}
		// Reserve before dropping the locks for descriptor admission, so even
		// another unpublished registration cannot race this identity. Retain
		// failed provisional IDs too: this is intentionally fail-closed.
		s.usedPrimaryGIPDeviceIDs[gipID] = struct{}{}
	}
	s.watchRetainedDeviceRegistration(registration)
	s.serverLifecycleMu.Unlock()
	s.busesMu.Unlock()

	if _, err := s.SnapshotDeviceDescriptor(registration); err != nil {
		if s.beforeRetainedRegistrationRollback != nil {
			s.beforeRetainedRegistrationRollback()
		}
		_, removeErr := s.RemoveDeviceRegistrationIfPresent(registration)
		return virtualbus.DeviceMeta{}, errors.Join(err, removeErr)
	}
	if err := s.publishRetainedDeviceRegistration(
		registration, authorityID); err != nil {
		_, removeErr := s.RemoveDeviceRegistrationIfPresent(registration)
		return virtualbus.DeviceMeta{}, errors.Join(err, removeErr)
	}
	return registration, nil
}

func (s *Server) publishRetainedDeviceRegistration(
	registration virtualbus.DeviceMeta,
	authorityID uint64,
) error {
	if s == nil || registration.Bus == nil {
		return errRetainedImportInvalid
	}
	s.busesMu.Lock()
	s.serverLifecycleMu.Lock()
	bus, err := s.validateRetainedDeviceRegistrationTargetLocked(
		registration.Meta.BusID, authorityID)
	if err == nil && bus != registration.Bus {
		err = errRetainedImportInvalid
	}
	if err == nil {
		var published bool
		published, err = bus.PublishRegistration(registration)
		if err == nil && !published {
			err = errRetainedImportInvalid
		}
	}
	s.serverLifecycleMu.Unlock()
	s.busesMu.Unlock()
	return err
}

func (s *Server) validateRetainedDeviceRegistrationTargetLocked(
	busID uint32,
	authorityID uint64,
) (*virtualbus.VirtualBus, error) {
	if s.serverClosing || s.retainedImports == nil || authorityID == 0 ||
		s.retainedImports.identity != authorityID {
		return nil, fmt.Errorf(
			"%w: retained USB/IP registration authority is unavailable",
			errRetainedImportInvalid)
	}
	if busID == 0 || busID > 0xffff {
		return nil, fmt.Errorf(
			"%w: retained USB/IP bus address is not representable",
			errRetainedImportInvalid)
	}
	bus := s.busses[busID]
	if bus == nil || bus.IsClosed() {
		return nil, fmt.Errorf(
			"%w: retained USB/IP target bus is unavailable",
			errRetainedImportInvalid)
	}
	return bus, nil
}

func (s *Server) retainedImportDeviceAddressLimit() uint32 {
	if s != nil && s.config != nil &&
		s.config.RetainedImportDeviceAddressLimit != 0 &&
		s.config.RetainedImportDeviceAddressLimit <= 0xffff {
		return s.config.RetainedImportDeviceAddressLimit
	}
	return 0xffff
}

// RemoveBus unregisters a bus from the server.
func (s *Server) RemoveBus(busID uint32) error {
	s.busesMu.Lock()
	if removal := s.busRemovals[busID]; removal != nil {
		s.busesMu.Unlock()
		<-removal.done
		return removal.err
	}
	bus, ok := s.busses[busID]
	if !ok {
		s.busesMu.Unlock()
		return fmt.Errorf("bus %d not found", busID)
	}
	removal := &serverBusRemoval{done: make(chan struct{})}
	s.busRemovals[busID] = removal
	s.busesMu.Unlock()
	// Keep registration ownership until the bus-level Add fence and exact drain
	// are established. A concurrent AddBus of this same pointer then observes
	// the still-registered bus or its permanently closed pointer. The server
	// lock is not held while exact device operations drain, so operation code
	// may safely reenter server status APIs.
	registrations, closeErr := bus.CloseAndTakeRegistrations()
	for _, registration := range registrations {
		s.forgetRetainedDeviceRegistration(registration)
	}
	// A caller may have closed the exported VirtualBus before asking the
	// server to remove its topology entry. In that case the joined bus Close
	// returns no snapshots, so retire every exact admission owned by this bus
	// before duplicate RemoveBus callers are released. Failed admissions remain
	// server-lifetime tombstones under forget/cleanup policy.
	s.forgetRetainedBusAdmissions(bus)
	if s.beforeBusRemovalComplete != nil {
		s.beforeBusRemovalComplete()
	}
	s.busesMu.Lock()
	if s.busses[busID] == bus {
		delete(s.busses, busID)
	}
	removal.err = closeErr
	close(removal.done)
	delete(s.busRemovals, busID)
	s.busesMu.Unlock()

	if len(registrations) > 0 {
		s.logger.Warn(fmt.Sprintf("Removing non-empty bus %d with %d device(s) attached; removing devices", busID, len(registrations)))
	}
	return closeErr
}

func (s *Server) forgetRetainedBusAdmissions(bus *virtualbus.VirtualBus) {
	if s == nil || bus == nil {
		return
	}
	s.retainedFailureMu.Lock()
	references := make([]retainedDeviceAdmissionKey, 0)
	for reference := range s.retainedDeviceAdmissions {
		if reference.bus == bus {
			references = append(references, reference)
		}
	}
	s.retainedFailureMu.Unlock()
	for _, reference := range references {
		s.forgetRetainedDeviceAdmissionKey(reference)
	}
}

// RemoveDeviceByID removes a device by busId and cancels its connections.
func (s *Server) RemoveDeviceByID(busID uint32, deviceID string) error {
	s.busesMu.Lock()
	bus, ok := s.busses[busID]
	if !ok {
		s.busesMu.Unlock()
		return fmt.Errorf("bus %d not found", busID)
	}
	s.busesMu.Unlock()
	removedRegistration, err :=
		bus.RemoveDeviceRegistrationByIDSnapshot(deviceID)
	if err != nil {
		return err
	}
	s.forgetRetainedDeviceRegistration(removedRegistration)
	s.scheduleEmptyBusCleanup(busID, bus)
	return nil
}

// RemoveDeviceRegistrationIfPresent removes only registration's exact Add
// incarnation. A stale cleanup capability is a successful no-op and can never
// remove a successor which reused the same bus/device address.
func (s *Server) RemoveDeviceRegistrationIfPresent(
	registration virtualbus.DeviceMeta,
) (bool, error) {
	if s == nil || registration.Bus == nil || registration.Dev == nil ||
		registration.Context == nil || registration.RegistrationToken == 0 {
		return false, errRetainedImportInvalid
	}
	if !registration.Bus.RecognizesRegistration(registration) {
		return false, errRetainedImportInvalid
	}
	s.busesMu.Lock()
	if s.busses[registration.Meta.BusID] != registration.Bus {
		s.busesMu.Unlock()
		s.forgetRetainedDeviceRegistration(registration)
		return false, nil
	}
	s.busesMu.Unlock()
	removed, err := registration.Bus.RemoveRegistrationIfPresent(registration)
	if err == nil {
		s.forgetRetainedDeviceRegistration(registration)
	}
	if err != nil || !removed {
		return removed, err
	}
	s.scheduleEmptyBusCleanup(registration.Meta.BusID, registration.Bus)
	return true, nil
}

// watchRetainedDeviceRegistration links the exported VirtualBus teardown
// surface back to the server-owned admission tombstone. The caller holds
// serverLifecycleMu and has already proved serverClosing false before adding
// to retainedCallbacksWG, so Server.Close cannot begin its join in between.
func (s *Server) watchRetainedDeviceRegistration(
	registration virtualbus.DeviceMeta,
) {
	s.retainedCallbacksWG.Add(1)
	go func() {
		defer s.retainedCallbacksWG.Done()
		select {
		case <-registration.Context.Done():
			s.forgetRetainedDeviceRegistration(registration)
			if s.afterRetainedRegistrationWatchForget != nil {
				s.afterRetainedRegistrationWatchForget()
			}
		case <-s.serverClosingSignal:
		}
	}()
}

func (s *Server) scheduleEmptyBusCleanup(
	busID uint32,
	bus *virtualbus.VirtualBus,
) {
	if s == nil || bus == nil {
		return
	}
	emptyContext := bus.GetBusEmptyContext()
	if emptyContext == nil {
		return
	}
	delay := s.config.BusCleanupTimeout
	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-emptyContext.Done():
			return
		case <-timer.C:
		}
		removed, err := s.removeEmptyBusIncarnation(busID, bus, emptyContext)
		if err != nil {
			s.logger.Error("timeout: failed to remove empty bus",
				"busID", busID, "error", err)
		} else if removed {
			s.logger.Info("timeout: removed empty bus", "busID", busID)
		}
	}()
}

func (s *Server) removeEmptyBusIncarnation(
	busID uint32,
	bus *virtualbus.VirtualBus,
	emptyContext context.Context,
) (bool, error) {
	if s == nil || bus == nil || emptyContext == nil {
		return false, errRetainedImportInvalid
	}
	s.busesMu.Lock()
	defer s.busesMu.Unlock()
	if s.busses[busID] != bus {
		return false, nil
	}
	removed, err := bus.CloseIfEmptyContext(emptyContext)
	if removed {
		delete(s.busses, busID)
	}
	return removed, err
}

// ListBuses returns a snapshot of active bus numbers.
func (s *Server) ListBuses() []uint32 {
	s.busesMu.Lock()
	defer s.busesMu.Unlock()
	out := make([]uint32, 0, len(s.busses))
	for k := range s.busses {
		out = append(out, k)
	}
	return out
}

// GetBus returns a bus by ID or nil if not present.
func (s *Server) GetBus(busID uint32) *virtualbus.VirtualBus {
	s.busesMu.Lock()
	defer s.busesMu.Unlock()
	return s.busses[busID]
}

func (s *Server) NextFreeBusID() uint32 {
	s.busesMu.Lock()
	defer s.busesMu.Unlock()
	var id uint32 = 1
	for {
		if _, exists := s.busses[id]; !exists {
			return id
		}
		id++
	}
}

func (s *Server) Addr() string {
	if s == nil {
		return ""
	}
	s.serverLifecycleMu.Lock()
	ln := s.ln
	s.serverLifecycleMu.Unlock()
	if ln != nil {
		return ln.Addr().String()
	}
	if s.config != nil {
		return s.config.Addr
	}
	return ""
}

// ListenAndServe starts the USB-IP server and handles incoming connections.
func (s *Server) ListenAndServe() error {
	ln, err := net.Listen("tcp", s.config.Addr)
	if err != nil {
		return err
	}
	s.serverLifecycleMu.Lock()
	if s.serverClosing {
		s.serverLifecycleMu.Unlock()
		_ = ln.Close()
		return net.ErrClosed
	}
	s.ln = ln
	s.serveWG.Add(1)
	s.serverLifecycleMu.Unlock()
	defer s.serveWG.Done()
	s.config.Addr = ln.Addr().String()
	s.readyOnce.Do(func() { close(s.ready) })
	s.logger.Info("USBIP server listening", "addr", s.config.Addr)
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || strings.Contains(strings.ToLower(err.Error()), "use of closed network connection") {
				s.logger.Info("USBIP server stopped")
				return nil
			}
			s.logger.Error("Accept error", "error", err)
			continue
		}
		if tcpConn, ok := c.(*net.TCPConn); ok {
			if err := tcpConn.SetNoDelay(true); err != nil {
				s.logger.Warn("failed to set TCP_NODELAY", "error", err)
			}
		}
		s.logger.Info("Client connected", "remote", c.RemoteAddr())
		ownedConnection := newServerOwnedConnection(c)
		connectionID, registered := s.registerServerConnection(
			ownedConnection.Close)
		if !registered {
			_ = ownedConnection.Close()
			continue
		}
		go func() {
			err := s.handleRegisteredConn(ownedConnection, connectionID)
			s.finishServerConnection(connectionID, err)
			if err != nil {
				if retainedServerTerminalFailure(err) == nil {
					s.logger.Info("Client disconnected", "error", err)
				} else {
					s.logger.Error("Connection handler error", "error", err)
				}
			}
		}()
	}
}

// Ready returns a channel that is closed once the server has successfully bound
// to its listen address and is ready to accept connections.
func (s *Server) Ready() <-chan struct{} { return s.ready }

// Close stops listener admission and synchronously fences every active retained
// import. Legacy connection ownership remains unchanged; retained sessions are
// joined because their scheduler and device-local executor must drain before
// the authoritative disconnect/neutral boundary can release the import.
func (s *Server) Close() error {
	if s == nil {
		return nil
	}
	s.serverCloseOnce.Do(func() {
		s.serverCloseErr = s.closeServer()
	})
	return s.serverCloseErr
}

func (s *Server) closeServer() error {
	s.serverLifecycleMu.Lock()
	s.serverClosing = true
	close(s.serverClosingSignal)
	if s.serverShutdownDeadline.IsZero() {
		s.serverShutdownDeadline = time.Now().Add(
			3 * s.retainedLifecycleTimeout())
	}
	shutdownDeadline := s.serverShutdownDeadline
	ln := s.ln
	streams := make([]*retainedServerStream, 0, len(s.retainedStreams))
	for _, stream := range s.retainedStreams {
		streams = append(streams, stream)
	}
	connections := make([]*trackedServerConnection, 0,
		len(s.serverConnections))
	for _, connection := range s.serverConnections {
		connections = append(connections, connection)
	}
	s.serverLifecycleMu.Unlock()

	var closeErr error
	if ln != nil {
		closeErr = errors.Join(closeErr,
			ordinaryServerTerminalFailure(ln.Close()))
	}
	for _, stream := range streams {
		stream.cancel()
	}
	var closeOps sync.WaitGroup
	closeResults := make(chan error, len(connections))
	closeOps.Add(len(connections))
	for _, connection := range connections {
		connection := connection
		go func() {
			defer closeOps.Done()
			closeResults <- ordinaryServerTerminalFailure(
				connection.closeConn())
		}()
	}
	done := make(chan struct{})
	go func() {
		closeOps.Wait()
		s.serveWG.Wait()
		s.serverConnectionsWG.Wait()
		s.retainedStreamsWG.Wait()
		s.retainedCallbacksWG.Wait()
		close(done)
	}()
	joinRemaining := time.Until(
		shutdownDeadline.Add(s.retainedLifecycleTimeout()))
	if joinRemaining < 0 {
		joinRemaining = 0
	}
	timer := time.NewTimer(joinRemaining)
	defer timer.Stop()
	select {
	case <-done:
		for range connections {
			closeErr = errors.Join(closeErr, <-closeResults)
		}
		s.serverLifecycleMu.Lock()
		for _, connection := range connections {
			closeErr = errors.Join(closeErr,
				retainedServerTerminalFailure(connection.terminalErr))
		}
		s.serverLifecycleMu.Unlock()
		s.retainedFailureMu.Lock()
		s.retainedDeviceAdmissions = nil
		s.retainedOwnerAdmissions = nil
		s.retainedFailureMu.Unlock()
		return closeErr
	case <-timer.C:
		return errors.Join(closeErr, errRetainedImportCloseTimedOut)
	}
}

func retainedServerTerminalFailure(terminalErr error) error {
	if terminalErr == nil {
		return nil
	}
	var retainedTerminal *retainedImportTerminalError
	if errors.As(terminalErr, &retainedTerminal) {
		return errors.Join(
			ordinaryServerTerminalFailure(retainedTerminal.transport),
			retainedTerminal.teardown)
	}
	return ordinaryServerTerminalFailure(terminalErr)
}

func ordinaryServerTerminalFailure(terminalErr error) error {
	if terminalErr == nil {
		return nil
	}
	if !serverTerminalTreeHasFailure(terminalErr, 0) {
		return nil
	}
	return terminalErr
}

// serverTerminalTreeHasFailure classifies every terminal leaf independently.
// An EOF joined with an invariant or teardown failure is not an ordinary
// disconnect merely because errors.Is can find the EOF constituent.
func serverTerminalTreeHasFailure(terminalErr error, depth uint8) bool {
	if terminalErr == nil {
		return false
	}
	if depth == ^uint8(0) {
		return true
	}
	if joined, ok := terminalErr.(interface{ Unwrap() []error }); ok {
		for _, cause := range joined.Unwrap() {
			if serverTerminalTreeHasFailure(cause, depth+1) {
				return true
			}
		}
		return false
	}
	if wrapped, ok := terminalErr.(interface{ Unwrap() error }); ok {
		cause := wrapped.Unwrap()
		if cause != nil {
			return serverTerminalTreeHasFailure(cause, depth+1)
		}
	}
	return !(isClientDisconnect(terminalErr) ||
		errors.Is(terminalErr, net.ErrClosed) ||
		errors.Is(terminalErr, io.ErrClosedPipe) ||
		errors.Is(terminalErr, context.Canceled))
}

func (s *Server) registerRetainedStream(
	cancel context.CancelFunc,
) (uint64, bool) {
	if s == nil || cancel == nil {
		return 0, false
	}
	s.serverLifecycleMu.Lock()
	defer s.serverLifecycleMu.Unlock()
	return s.registerRetainedStreamLocked(cancel, nil)
}

func (s *Server) registerRetainedStreamLocked(
	cancel context.CancelFunc, registration *retainedRegistrationClosure,
) (uint64, bool) {
	if s.serverClosing || s.nextRetainedStream == ^uint64(0) {
		return 0, false
	}
	s.nextRetainedStream++
	streamID := s.nextRetainedStream
	s.retainedStreams[streamID] = &retainedServerStream{
		cancel: cancel, done: make(chan struct{}), registrationClosure: registration,
	}
	s.retainedStreamsWG.Add(1)
	return streamID, true
}

func (s *Server) finishRetainedStream(streamID uint64, terminalErr error) {
	if s == nil || streamID == 0 {
		return
	}
	s.serverLifecycleMu.Lock()
	if stream, exists := s.retainedStreams[streamID]; exists {
		stream.terminalErr = terminalErr
		if registration := stream.registrationClosure; registration != nil {
			registration.terminalErr = errors.Join(registration.terminalErr,
				retainedRegistrationCloseFailure(terminalErr))
		}
		delete(s.retainedStreams, streamID)
		if registration := stream.registrationClosure; registration != nil {
			s.forgetRetainedRegistrationClosureLocked(registration)
		}
		close(stream.done)
		s.retainedStreamsWG.Done()
	}
	s.serverLifecycleMu.Unlock()
}

func (s *Server) registerServerConnection(
	closeConn func() error,
) (uint64, bool) {
	if s == nil || closeConn == nil {
		return 0, false
	}
	s.serverLifecycleMu.Lock()
	defer s.serverLifecycleMu.Unlock()
	if s.serverClosing || s.nextServerConnection == ^uint64(0) {
		return 0, false
	}
	s.nextServerConnection++
	connectionID := s.nextServerConnection
	s.serverConnections[connectionID] = &trackedServerConnection{
		closeConn: closeConn,
	}
	s.serverConnectionsWG.Add(1)
	return connectionID, true
}

func (s *Server) finishServerConnection(
	connectionID uint64,
	terminalErr error,
) {
	if s == nil || connectionID == 0 {
		return
	}
	s.serverLifecycleMu.Lock()
	if connection, exists := s.serverConnections[connectionID]; exists {
		connection.terminalErr = terminalErr
		delete(s.serverConnections, connectionID)
		s.serverConnectionsWG.Done()
	}
	s.serverLifecycleMu.Unlock()
}

// releaseLegacyConnectionTracking atomically transfers a conclusively legacy
// or management connection back to the historical ownership model. If server
// shutdown already owns the preclassification fence, transfer loses and the
// handler must perform no later legacy/device callbacks.
func (s *Server) releaseLegacyConnectionTracking(connectionID uint64) bool {
	if s == nil || connectionID == 0 {
		return false
	}
	s.serverLifecycleMu.Lock()
	defer s.serverLifecycleMu.Unlock()
	if s.serverClosing {
		return false
	}
	if _, exists := s.serverConnections[connectionID]; !exists {
		return false
	}
	delete(s.serverConnections, connectionID)
	s.serverConnectionsWG.Done()
	return true
}

// GetListenPort extracts and returns the port number from the server's listen address.
func (s *Server) GetListenPort() uint16 {
	addr := s.Addr()
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil {
		return 0
	}
	return uint16(port)
}

// --

func (s *Server) handleConn(conn net.Conn) (resultErr error) {
	ownedConnection := newServerOwnedConnection(conn)
	connectionID, registered := s.registerServerConnection(
		ownedConnection.Close)
	if !registered {
		_ = ownedConnection.Close()
		return net.ErrClosed
	}
	defer func() { s.finishServerConnection(connectionID, resultErr) }()
	return s.handleRegisteredConn(ownedConnection, connectionID)
}

func (s *Server) handleRegisteredConn(
	conn net.Conn,
	connectionID uint64,
) (resultErr error) {
	defer func() {
		resultErr = errors.Join(resultErr,
			ordinaryServerTerminalFailure(conn.Close()))
	}()
	conn = &logConn{Conn: conn, s: s}
	if err := conn.SetDeadline(time.Now().Add(s.config.ConnectionTimeout)); err != nil {
		s.logger.Warn("Failed to set deadline", "error", err)
	}

	// Peek first 8 bytes to determine management op or URB stream.
	var hdrBuf [headerPeekSize]byte
	if err := usbip.ReadExactly(conn, hdrBuf[:]); err != nil {
		return fmt.Errorf("read header: %w", err)
	}

	ver := binary.BigEndian.Uint16(hdrBuf[0:2])
	code := binary.BigEndian.Uint16(hdrBuf[2:4])

	if ver == usbip.Version && (code == usbip.OpReqDevlist || code == usbip.OpReqImport) {
		switch code {
		case usbip.OpReqDevlist:
			s.logger.Info("OP_REQ_DEVLIST")
			return s.handleDevList(conn)
		case usbip.OpReqImport:
			s.logger.Info("OP_REQ_IMPORT")
			selection, err := s.readImportSelection(conn)
			if err != nil {
				return fmt.Errorf("handle import: %w", err)
			}
			defer selection.releaseRetainedCallbacks()
			if _, optedIn := selection.dev.(retainedusb.ImportDevice); optedIn {
				if s.retainedImports == nil {
					_ = writeImportFailure(conn)
					return fmt.Errorf(
						"retained import %s requires an explicit authority",
						selection.busID)
				}
				owner, deviceID, capabilityErr :=
					boundedRetainedImportCapability(
						selection.dev,
						time.Now().Add(s.retainedLifecycleTimeout()),
						selection.retainedCallbacks)
				if capabilityErr != nil {
					selection.rejectRetainedCallbacks(capabilityErr)
					_ = writeImportFailure(conn)
					return fmt.Errorf("retained import %s capability: %w",
						selection.busID, capabilityErr)
				}
				return s.handleRetainedImport(
					conn, selection, owner, deviceID)
			}
			if !s.releaseLegacyConnectionTracking(connectionID) {
				return net.ErrClosed
			}
			dev, releaseImport, err := s.completeLegacyImport(conn, selection)
			if err != nil {
				return fmt.Errorf("handle import: %w", err)
			}
			defer releaseImport()
			return s.handleUrbStream(conn, dev)
		}
	}

	return fmt.Errorf("protocol violation: client sent URB data without OP_REQ_IMPORT")
}

func (s *Server) handleDevList(conn net.Conn) error {
	_ = conn.SetDeadline(time.Time{})
	type deviceSnapshot struct {
		meta       virtualbus.DeviceMeta
		descriptor *usb.Descriptor
	}
	metas := s.getAllDeviceMetas()
	snapshots := make([]deviceSnapshot, 0, len(metas))
	for _, meta := range metas {
		var descriptor *usb.Descriptor
		if _, retained := meta.Dev.(retainedusb.ImportDevice); retained {
			if _, err := retainedImportWireDeviceID(meta.Meta); err != nil {
				s.logger.Error(
					"Omitting unrepresentable retained USB device from DEVLIST",
					"bus_id", meta.Meta.BusID, "device_id", meta.Meta.DevID,
					"error", err)
				continue
			}
			var err error
			descriptor, err = s.snapshotRegisteredRetainedDescriptor(meta)
			if err != nil {
				s.logger.Error("Omitting invalid retained USB device from DEVLIST",
					"bus_id", meta.Meta.BusID, "device_id", meta.Meta.DevID,
					"error", err)
				continue
			}
		} else {
			descriptor = meta.Dev.GetDescriptor()
		}
		if descriptor == nil {
			s.logger.Error("Omitting USB device without a descriptor from DEVLIST",
				"bus_id", meta.Meta.BusID, "device_id", meta.Meta.DevID)
			continue
		}
		snapshots = append(snapshots, deviceSnapshot{
			meta: meta, descriptor: descriptor,
		})
	}
	var buf bytes.Buffer
	rep := usbip.MgmtHeader{Version: usbip.Version, Command: usbip.OpRepDevlist, Status: 0}
	_ = rep.Write(&buf)
	n := uint32(len(snapshots))
	dlh := usbip.DevListReplyHeader{NDevices: n}
	_ = dlh.Write(&buf)
	for _, snapshot := range snapshots {
		desc := snapshot.descriptor
		meta := snapshot.meta.Meta

		exp := usbip.ExportedDevice{
			ExportMeta:          meta,
			Speed:               desc.Device.Speed,
			IDVendor:            desc.Device.IDVendor,
			IDProduct:           desc.Device.IDProduct,
			BcdDevice:           desc.Device.BcdDevice,
			BDeviceClass:        desc.Device.BDeviceClass,
			BDeviceSubClass:     desc.Device.BDeviceSubClass,
			BDeviceProtocol:     desc.Device.BDeviceProtocol,
			BConfigurationValue: descriptorConfigurationValue(desc),
			BNumConfigurations:  desc.Device.BNumConfigurations,
			BNumInterfaces:      desc.NumInterfaces(),
		}

		for _, iface := range descriptorListInterfaces(desc) {
			exp.Interfaces = append(exp.Interfaces, usbip.InterfaceDesc{
				Class:    iface.Descriptor.BInterfaceClass,
				SubClass: iface.Descriptor.BInterfaceSubClass,
				Protocol: iface.Descriptor.BInterfaceProtocol,
			})
		}
		_ = exp.WriteDevlist(&buf)
	}
	if _, err := conn.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("write devlist: %w", err)
	}
	return nil
}

func (s *Server) handleImport(conn net.Conn) (usb.Device, func(), error) {
	selection, err := s.readImportSelection(conn)
	if err != nil {
		return nil, nil, err
	}
	defer selection.releaseRetainedCallbacks()
	return s.completeLegacyImport(conn, selection)
}

type importSelection struct {
	busID             string
	dev               usb.Device
	meta              usbip.ExportMeta
	descriptor        *usb.Descriptor
	bus               *virtualbus.VirtualBus
	deviceContext     context.Context
	registration      virtualbus.DeviceMeta
	retainedCallbacks *retainedDeviceCallbackLease
	wireDeviceID      uint32
}

func (selection importSelection) releaseRetainedCallbacks() {
	if selection.retainedCallbacks != nil {
		selection.retainedCallbacks.release()
	}
}

func (selection importSelection) rejectRetainedCallbacks(failure error) {
	if selection.retainedCallbacks != nil {
		selection.retainedCallbacks.reject(failure)
	}
}

func (s *Server) readImportSelection(conn net.Conn) (importSelection, error) {
	var rest [busIDSize]byte
	if err := usbip.ReadExactly(conn, rest[:]); err != nil {
		return importSelection{}, fmt.Errorf("read import busid: %w", err)
	}
	reqBus, err := parseUSBIPBusID(rest[:])
	if err != nil {
		_ = writeImportFailure(conn)
		return importSelection{}, err
	}
	s.logger.Info("Import request", "busid", reqBus)
	chosen, err := s.lookupUSBIPImportRegistration(reqBus)
	if err != nil {
		s.observeFailedImport(reqBus)
		_ = writeImportFailure(conn)
		return importSelection{}, err
	}
	bus, deviceContext := chosen.Bus, chosen.Context
	var chosenDesc *usb.Descriptor
	var retainedCallbacks *retainedDeviceCallbackLease
	var wireDeviceID uint32
	if _, retained := chosen.Dev.(retainedusb.ImportDevice); retained {
		wireDeviceID, err = retainedImportWireDeviceID(chosen.Meta)
		if err != nil {
			_ = writeImportFailure(conn)
			return importSelection{}, fmt.Errorf(
				"retained device %s address: %w", reqBus, err)
		}
		retainedCallbacks, err =
			s.beginRetainedRegisteredDeviceCallbacks(chosen)
		if err != nil {
			_ = writeImportFailure(conn)
			return importSelection{}, fmt.Errorf(
				"admit retained device %s callbacks: %w", reqBus, err)
		}
		snapshot, snapshotErr := s.snapshotRetainedDescriptorWithLease(
			chosen.Dev, retainedCallbacks)
		if snapshotErr != nil {
			retainedCallbacks.release()
			_ = writeImportFailure(conn)
			return importSelection{}, fmt.Errorf(
				"snapshot retained device %s descriptor: %w",
				reqBus, snapshotErr)
		}
		chosenDesc = snapshot
	} else {
		// Preserve the historical legacy descriptor ownership/callback path.
		chosenDesc = chosen.Dev.GetDescriptor()
	}
	if chosenDesc == nil {
		_ = writeImportFailure(conn)
		return importSelection{}, fmt.Errorf("device %s has no descriptor", reqBus)
	}
	return importSelection{
		busID: reqBus, dev: chosen.Dev, meta: chosen.Meta,
		descriptor: chosenDesc, bus: bus, deviceContext: deviceContext,
		registration:      chosen,
		retainedCallbacks: retainedCallbacks,
		wireDeviceID:      wireDeviceID,
	}, nil
}

func (s *Server) snapshotRegisteredRetainedDescriptor(
	registration virtualbus.DeviceMeta,
) (*usb.Descriptor, error) {
	lease, err := s.beginRetainedRegisteredDeviceCallbacks(registration)
	if err != nil {
		return nil, err
	}
	defer lease.release()
	return s.snapshotRetainedDescriptorWithLease(registration.Dev, lease)
}

// SnapshotDeviceDescriptor returns one descriptor through the ownership path
// appropriate for registration. Retained devices are admitted, bounded,
// sealed, and topology-latched; legacy devices preserve their historical
// direct descriptor callback after exact registration authentication.
func (s *Server) SnapshotDeviceDescriptor(
	registration virtualbus.DeviceMeta,
) (*usb.Descriptor, error) {
	if s == nil || registration.Bus == nil || registration.Dev == nil ||
		registration.Context == nil || registration.RegistrationToken == 0 {
		return nil, errRetainedImportInvalid
	}
	if _, retained := registration.Dev.(retainedusb.ImportDevice); retained {
		if _, err := retainedImportWireDeviceID(registration.Meta); err != nil {
			return nil, err
		}
		return s.snapshotRegisteredRetainedDescriptor(registration)
	}
	s.busesMu.Lock()
	authenticated := s.busses[registration.Meta.BusID] == registration.Bus &&
		registration.Bus.AuthenticatesRegistration(registration)
	s.busesMu.Unlock()
	if !authenticated {
		return nil, fmt.Errorf("device registration is no longer active")
	}
	descriptor := registration.Dev.GetDescriptor()
	if descriptor == nil {
		return nil, fmt.Errorf("device has no descriptor")
	}
	return descriptor, nil
}

func (s *Server) snapshotRetainedDescriptor(
	dev usb.Device,
) (*usb.Descriptor, error) {
	lease, err := s.beginRetainedDeviceCallbacks(dev)
	if err != nil {
		return nil, err
	}
	defer lease.release()
	return s.snapshotRetainedDescriptorWithLease(dev, lease)
}

func (s *Server) snapshotRetainedDescriptorWithLease(
	dev usb.Device,
	lease *retainedDeviceCallbackLease,
) (*usb.Descriptor, error) {
	if !lease.authenticates(s, dev) {
		return nil, errRetainedImportInvalid
	}
	snapshot, err := invokeRetainedImportStep(
		"retained import descriptor snapshot",
		time.Now().Add(s.retainedLifecycleTimeout()),
		func() (usb.Descriptor, error) {
			descriptor := dev.GetDescriptor()
			return sealRetainedImportDescriptor(descriptor)
		})
	if err != nil {
		lease.reject(err)
		return nil, err
	}
	if !lease.authenticates(s, dev) {
		return nil, errRetainedImportQuarantined
	}
	if err := lease.bindDescriptor(snapshot); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

type retainedDeviceAdmission struct {
	mu       sync.Mutex
	inFlight bool
	failure  error
	// reference is deliberately strong. The map's type/pointer key alone would
	// permit allocator address reuse to inherit stale quarantine state.
	reference      any
	removed        bool
	ownerReference retainedImportOwnerReference
	owner          any
	deviceID       uint64
	ownerIdentity  uint64
	registration   virtualbus.DeviceMeta
	descriptor     *usb.Descriptor
	limits         retainedusb.Limits
	limitsSet      bool
}

type retainedDeviceAdmissionKey struct {
	device            retainedImportOwnerReference
	bus               *virtualbus.VirtualBus
	registrationToken uint64
}

type retainedDeviceCallbackLease struct {
	server               *Server
	reference            retainedDeviceAdmissionKey
	admission            *retainedDeviceAdmission
	ownerReference       retainedImportOwnerReference
	ownerAdmission       *retainedDeviceAdmission
	serverLifecycleOwned bool
	releaseOnce          sync.Once
}

func (s *Server) beginRetainedDeviceCallbacks(
	dev usb.Device,
) (*retainedDeviceCallbackLease, error) {
	if s == nil || dev == nil {
		return nil, errRetainedImportInvalid
	}
	deviceReference, valid := exactRetainedImportOwnerReference(dev)
	if !valid {
		return nil, errRetainedImportInvalid
	}
	return s.beginRetainedDeviceCallbacksForKey(
		retainedDeviceAdmissionKey{device: deviceReference}, dev,
		virtualbus.DeviceMeta{})
}

func (s *Server) beginRetainedRegisteredDeviceCallbacks(
	registration virtualbus.DeviceMeta,
) (*retainedDeviceCallbackLease, error) {
	if s == nil || registration.Bus == nil || registration.Dev == nil ||
		registration.Context == nil || registration.RegistrationToken == 0 {
		return nil, errRetainedImportInvalid
	}
	deviceReference, valid := exactRetainedImportOwnerReference(registration.Dev)
	if !valid {
		return nil, errRetainedImportInvalid
	}
	key := retainedDeviceAdmissionKey{
		device: deviceReference, bus: registration.Bus,
		registrationToken: registration.RegistrationToken,
	}
	// Server-owned registration and removal paths use the same lock order:
	// server topology -> bus registration -> retained admission. Therefore a
	// callback admission linearizes either before exact removal or after it.
	s.busesMu.Lock()
	if s.busses[registration.Meta.BusID] != registration.Bus ||
		!registration.Bus.AuthenticatesRegistration(registration) {
		s.busesMu.Unlock()
		return nil, errRetainedImportInvalid
	}
	if s.beforeRegisteredRetainedAdmission != nil {
		s.beforeRegisteredRetainedAdmission()
	}
	lease, err := s.beginRetainedDeviceCallbacksForKey(
		key, registration.Dev, registration)
	s.busesMu.Unlock()
	return lease, err
}

func (s *Server) beginRetainedDeviceCallbacksForKey(
	reference retainedDeviceAdmissionKey,
	dev usb.Device,
	registration virtualbus.DeviceMeta,
) (lease *retainedDeviceCallbackLease, resultErr error) {
	if !s.registerRetainedCallback() {
		return nil, net.ErrClosed
	}
	registered := true
	defer func() {
		if registered {
			s.retainedCallbacksWG.Done()
		}
	}()
	s.retainedFailureMu.Lock()
	if s.retainedDeviceAdmissions == nil {
		s.retainedDeviceAdmissions = make(
			map[retainedDeviceAdmissionKey]*retainedDeviceAdmission)
	}
	for candidateKey, candidate := range s.retainedDeviceAdmissions {
		if candidateKey == reference || candidateKey.device != reference.device {
			continue
		}
		candidate.mu.Lock()
		if !candidate.removed && candidate.registration.Bus != nil &&
			candidate.registration.Context != nil &&
			candidate.registration.RegistrationToken != 0 &&
			candidate.registration.Context.Err() != nil {
			// A caller may use the exported VirtualBus exact-removal/Close
			// surface instead of the Server wrapper. Context retirement is
			// also watched asynchronously, but reconcile the exact stale
			// incarnation here only after its operation fence has drained and
			// cancellation is visible. This lets an immediate same-pointer
			// re-add avoid a scheduler-dependent Busy without overlapping an
			// old operation which has merely been marked for removal.
			candidate.removed = true
		}
		failure := candidate.failure
		conflicts := candidate.inFlight || !candidate.removed
		cleanup := candidate.removed && !candidate.inFlight &&
			candidate.failure == nil
		candidate.mu.Unlock()
		if failure != nil {
			s.retainedFailureMu.Unlock()
			return nil, fmt.Errorf(
				"%w: prior retained device callback failure: %w",
				errRetainedImportQuarantined, failure)
		}
		if conflicts {
			s.retainedFailureMu.Unlock()
			return nil, errRetainedImportBusy
		}
		if cleanup {
			delete(s.retainedDeviceAdmissions, candidateKey)
		}
	}
	admission := s.retainedDeviceAdmissions[reference]
	if admission == nil {
		admission = &retainedDeviceAdmission{
			reference: dev, registration: registration,
		}
		s.retainedDeviceAdmissions[reference] = admission
	}
	admission.mu.Lock()
	if admission.failure != nil {
		failure := admission.failure
		admission.mu.Unlock()
		s.retainedFailureMu.Unlock()
		return nil, fmt.Errorf("%w: prior retained callback failure: %w",
			errRetainedImportQuarantined, failure)
	}
	if admission.removed {
		admission.mu.Unlock()
		s.retainedFailureMu.Unlock()
		return nil, errRetainedImportInvalid
	}
	if admission.inFlight {
		admission.mu.Unlock()
		s.retainedFailureMu.Unlock()
		return nil, errRetainedImportBusy
	}
	admission.inFlight = true
	lease = &retainedDeviceCallbackLease{
		server: s, reference: reference, admission: admission,
		serverLifecycleOwned: true,
	}
	admission.mu.Unlock()
	s.retainedFailureMu.Unlock()
	registered = false
	return lease, nil
}

func (s *Server) registerRetainedCallback() bool {
	if s == nil {
		return false
	}
	s.serverLifecycleMu.Lock()
	defer s.serverLifecycleMu.Unlock()
	if s.serverClosing {
		return false
	}
	s.retainedCallbacksWG.Add(1)
	return true
}

func (lease *retainedDeviceCallbackLease) authenticates(
	server *Server,
	dev usb.Device,
) bool {
	if lease == nil || lease.server != server || lease.admission == nil {
		return false
	}
	deviceReference, valid := exactRetainedImportOwnerReference(dev)
	if !valid || deviceReference != lease.reference.device {
		return false
	}
	if lease.reference.bus != nil {
		lease.admission.mu.Lock()
		registration := lease.admission.registration
		lease.admission.mu.Unlock()
		server.busesMu.Lock()
		registered := server.busses[registration.Meta.BusID] == registration.Bus &&
			registration.Bus.AuthenticatesRegistration(registration)
		server.busesMu.Unlock()
		if !registered {
			return false
		}
	}
	lease.admission.mu.Lock()
	active := lease.admission.inFlight && lease.admission.failure == nil &&
		!lease.admission.removed
	lease.admission.mu.Unlock()
	if !active {
		return false
	}
	if lease.ownerAdmission != nil {
		lease.ownerAdmission.mu.Lock()
		active = lease.ownerAdmission.inFlight &&
			lease.ownerAdmission.failure == nil
		lease.ownerAdmission.mu.Unlock()
	}
	return active
}

func (lease *retainedDeviceCallbackLease) bindCapability(
	owner any,
	deviceID uint64,
) error {
	if lease == nil || lease.server == nil || lease.admission == nil ||
		interfaceIsNil(owner) || deviceID == 0 || lease.ownerAdmission != nil {
		return errRetainedImportInvalid
	}
	reference, valid := exactRetainedImportOwnerReference(owner)
	if !valid {
		return errRetainedImportInvalid
	}
	server := lease.server
	server.retainedFailureMu.Lock()
	lease.admission.mu.Lock()
	if lease.admission.ownerReference != (retainedImportOwnerReference{}) &&
		lease.admission.ownerReference != reference ||
		lease.admission.deviceID != 0 &&
			lease.admission.deviceID != deviceID {
		if lease.admission.failure == nil {
			lease.admission.failure = errRetainedImportInvalid
		}
		lease.admission.mu.Unlock()
		server.retainedFailureMu.Unlock()
		return fmt.Errorf("%w: retained device capability changed",
			errRetainedImportQuarantined)
	}
	// The first structurally valid capability sample is registration-lifetime
	// identity even when its owner is currently Busy. A retry cannot exploit a
	// failed owner claim to substitute a different owner or resource DeviceID.
	if lease.admission.ownerReference == (retainedImportOwnerReference{}) {
		lease.admission.ownerReference = reference
		lease.admission.owner = owner
		lease.admission.deviceID = deviceID
	}
	lease.admission.mu.Unlock()
	if server.retainedOwnerAdmissions == nil {
		server.retainedOwnerAdmissions = make(
			map[retainedImportOwnerReference]*retainedDeviceAdmission)
	}
	admission := server.retainedOwnerAdmissions[reference]
	created := false
	if admission == nil {
		admission = &retainedDeviceAdmission{reference: owner}
		server.retainedOwnerAdmissions[reference] = admission
		created = true
	}
	admission.mu.Lock()
	if admission.failure != nil {
		failure := admission.failure
		admission.mu.Unlock()
		server.retainedFailureMu.Unlock()
		return fmt.Errorf("%w: prior retained owner callback failure: %w",
			errRetainedImportQuarantined, failure)
	}
	if admission.inFlight {
		admission.mu.Unlock()
		server.retainedFailureMu.Unlock()
		return errRetainedImportBusy
	}
	lease.admission.mu.Lock()
	if !lease.admission.inFlight || lease.admission.failure != nil ||
		lease.admission.removed {
		lease.admission.mu.Unlock()
		if created {
			delete(server.retainedOwnerAdmissions, reference)
		}
		admission.mu.Unlock()
		server.retainedFailureMu.Unlock()
		return errRetainedImportQuarantined
	}
	// Publish the owner claim and its device association while the map lock
	// prevents cleanup from scanning or deleting either admission. There is no
	// interval in which a claimed owner is absent from all device records.
	admission.inFlight = true
	lease.admission.mu.Unlock()
	lease.ownerReference = reference
	lease.ownerAdmission = admission
	admission.mu.Unlock()
	server.retainedFailureMu.Unlock()
	return nil
}

func (lease *retainedDeviceCallbackLease) bindOwnerIdentity(
	identity uint64,
) error {
	if lease == nil || lease.server == nil || lease.admission == nil ||
		lease.ownerAdmission == nil || identity == 0 {
		return errRetainedImportInvalid
	}
	server := lease.server
	server.retainedFailureMu.Lock()
	ownerAdmission := server.retainedOwnerAdmissions[lease.ownerReference]
	if ownerAdmission != lease.ownerAdmission {
		server.retainedFailureMu.Unlock()
		return errRetainedImportQuarantined
	}
	ownerAdmission.mu.Lock()
	lease.admission.mu.Lock()
	if ownerAdmission.ownerIdentity != 0 &&
		ownerAdmission.ownerIdentity != identity {
		if ownerAdmission.failure == nil {
			ownerAdmission.failure = errRetainedImportInvalid
		}
		if lease.admission.failure == nil {
			lease.admission.failure = errRetainedImportInvalid
		}
		lease.admission.mu.Unlock()
		ownerAdmission.mu.Unlock()
		server.retainedFailureMu.Unlock()
		return fmt.Errorf("%w: retained owner identity changed",
			errRetainedImportQuarantined)
	}
	ownerAdmission.ownerIdentity = identity
	lease.admission.ownerIdentity = identity
	lease.admission.mu.Unlock()
	ownerAdmission.mu.Unlock()
	server.retainedFailureMu.Unlock()
	return nil
}

func (lease *retainedDeviceCallbackLease) bindDescriptor(
	descriptor usb.Descriptor,
) error {
	if lease == nil || lease.admission == nil {
		return errRetainedImportInvalid
	}
	lease.admission.mu.Lock()
	defer lease.admission.mu.Unlock()
	if lease.admission.failure != nil || lease.admission.removed {
		return errRetainedImportQuarantined
	}
	if lease.admission.descriptor == nil {
		latched := descriptor
		lease.admission.descriptor = &latched
		return nil
	}
	if equalSealedRetainedImportDescriptor(
		*lease.admission.descriptor, descriptor) {
		return nil
	}
	if lease.admission.failure == nil {
		lease.admission.failure = errRetainedImportInvalid
	}
	return fmt.Errorf("%w: retained device descriptor changed",
		errRetainedImportQuarantined)
}

func (lease *retainedDeviceCallbackLease) bindLimits(
	limits retainedusb.Limits,
) error {
	if lease == nil || lease.server == nil || lease.admission == nil ||
		lease.ownerAdmission == nil {
		return errRetainedImportInvalid
	}
	server := lease.server
	server.retainedFailureMu.Lock()
	defer server.retainedFailureMu.Unlock()
	if server.retainedOwnerAdmissions[lease.ownerReference] !=
		lease.ownerAdmission {
		return errRetainedImportQuarantined
	}
	lease.ownerAdmission.mu.Lock()
	defer lease.ownerAdmission.mu.Unlock()
	lease.admission.mu.Lock()
	defer lease.admission.mu.Unlock()
	if lease.ownerAdmission.failure != nil || lease.admission.failure != nil ||
		lease.admission.removed {
		return errRetainedImportQuarantined
	}
	if lease.ownerAdmission.limitsSet &&
		lease.ownerAdmission.limits != limits {
		lease.ownerAdmission.failure = errRetainedImportInvalid
		lease.admission.failure = errRetainedImportInvalid
		return fmt.Errorf("%w: retained owner limits changed",
			errRetainedImportQuarantined)
	}
	lease.ownerAdmission.limits = limits
	lease.ownerAdmission.limitsSet = true
	lease.admission.limits = limits
	lease.admission.limitsSet = true
	return nil
}

func equalSealedRetainedImportDescriptor(
	left usb.Descriptor,
	right usb.Descriptor,
) bool {
	if left.Device != right.Device || left.Configuration != right.Configuration ||
		len(left.Interfaces) != 1 || len(right.Interfaces) != 1 {
		return false
	}
	leftInterface := left.Interfaces[0]
	rightInterface := right.Interfaces[0]
	if leftInterface.Descriptor != rightInterface.Descriptor ||
		len(leftInterface.Endpoints) != len(rightInterface.Endpoints) {
		return false
	}
	for index := range leftInterface.Endpoints {
		leftEndpoint := leftInterface.Endpoints[index]
		rightEndpoint := rightInterface.Endpoints[index]
		if leftEndpoint.BEndpointAddress != rightEndpoint.BEndpointAddress ||
			leftEndpoint.BMAttributes != rightEndpoint.BMAttributes ||
			leftEndpoint.WMaxPacketSize != rightEndpoint.WMaxPacketSize ||
			leftEndpoint.BInterval != rightEndpoint.BInterval {
			return false
		}
	}
	return true
}

func (lease *retainedDeviceCallbackLease) reject(failure error) {
	if lease == nil || lease.admission == nil || failure == nil {
		return
	}
	lease.admission.mu.Lock()
	if lease.admission.failure == nil {
		lease.admission.failure = failure
	}
	lease.admission.mu.Unlock()
	if lease.ownerAdmission != nil {
		lease.ownerAdmission.mu.Lock()
		if lease.ownerAdmission.failure == nil {
			lease.ownerAdmission.failure = failure
		}
		lease.ownerAdmission.mu.Unlock()
	}
}

func (lease *retainedDeviceCallbackLease) release() {
	if lease == nil || lease.admission == nil {
		return
	}
	lease.releaseOnce.Do(func() {
		if lease.ownerAdmission != nil {
			lease.ownerAdmission.mu.Lock()
			lease.ownerAdmission.inFlight = false
			lease.ownerAdmission.mu.Unlock()
		}
		lease.admission.mu.Lock()
		lease.admission.inFlight = false
		removed := lease.admission.removed
		lease.admission.mu.Unlock()
		if removed {
			lease.server.cleanupRetainedDeviceAdmission(lease.reference)
		}
		if lease.serverLifecycleOwned {
			lease.serverLifecycleOwned = false
			lease.server.retainedCallbacksWG.Done()
		}
	})
}

func (s *Server) forgetRetainedDeviceAdmission(dev usb.Device) {
	if s == nil || dev == nil {
		return
	}
	deviceReference, valid := exactRetainedImportOwnerReference(dev)
	if !valid {
		return
	}
	s.retainedFailureMu.Lock()
	references := make([]retainedDeviceAdmissionKey, 0, 1)
	for reference := range s.retainedDeviceAdmissions {
		if reference.device == deviceReference {
			references = append(references, reference)
		}
	}
	s.retainedFailureMu.Unlock()
	for _, reference := range references {
		s.forgetRetainedDeviceAdmissionKey(reference)
	}
}

func (s *Server) forgetRetainedDeviceRegistration(
	registration virtualbus.DeviceMeta,
) {
	deviceReference, valid := exactRetainedImportOwnerReference(registration.Dev)
	if s == nil || !valid || registration.Bus == nil ||
		registration.RegistrationToken == 0 {
		return
	}
	s.forgetRetainedDeviceAdmissionKey(retainedDeviceAdmissionKey{
		device: deviceReference, bus: registration.Bus,
		registrationToken: registration.RegistrationToken,
	})
	s.forgetRetainedRegistrationClosure(registration)
}

func (s *Server) forgetRetainedDeviceAdmissionKey(
	reference retainedDeviceAdmissionKey,
) {
	s.retainedFailureMu.Lock()
	admission := s.retainedDeviceAdmissions[reference]
	if admission == nil {
		s.retainedFailureMu.Unlock()
		return
	}
	admission.mu.Lock()
	admission.removed = true
	inFlight := admission.inFlight
	admission.mu.Unlock()
	s.retainedFailureMu.Unlock()
	if !inFlight {
		s.cleanupRetainedDeviceAdmission(reference)
	}
}

func (s *Server) cleanupRetainedDeviceAdmission(
	reference retainedDeviceAdmissionKey,
) {
	if s == nil {
		return
	}
	s.retainedFailureMu.Lock()
	defer s.retainedFailureMu.Unlock()
	admission := s.retainedDeviceAdmissions[reference]
	if admission == nil {
		return
	}
	admission.mu.Lock()
	if admission.inFlight || !admission.removed {
		admission.mu.Unlock()
		return
	}
	if admission.failure != nil {
		// Preserve a failed exact device object as a server-lifetime tombstone.
		// A new registration token must not authorize another invocation of its
		// broken pre-owner descriptor/capability callback.
		admission.mu.Unlock()
		return
	}
	delete(s.retainedDeviceAdmissions, reference)
	admission.mu.Unlock()
	// Owner discovery is lazy: another already-registered wrapper can share
	// this owner while its device admission still has no ownerReference. Keep
	// sampled owner identity, limits, and quarantine proof until Server.Close;
	// deleting it here would let that wrapper recreate a clean admission and
	// bypass the predecessor's failure.
}

// rejectRetainedDevice bounds a broken pre-session callback to one invocation
// for a concrete device object. Go cannot cancel arbitrary user code after a
// callback deadline, so a timeout/panic permanently removes that object from
// subsequent DEVLIST/import callback paths on this server.
func (s *Server) rejectRetainedDevice(dev usb.Device, failure error) {
	if s == nil || failure == nil {
		return
	}
	deviceReference, valid := exactRetainedImportOwnerReference(dev)
	if !valid {
		return
	}
	s.retainedFailureMu.Lock()
	if s.retainedDeviceAdmissions == nil {
		s.retainedDeviceAdmissions = make(
			map[retainedDeviceAdmissionKey]*retainedDeviceAdmission)
	}
	reference := retainedDeviceAdmissionKey{device: deviceReference}
	admission := s.retainedDeviceAdmissions[reference]
	if admission == nil {
		admission = &retainedDeviceAdmission{reference: dev}
		s.retainedDeviceAdmissions[reference] = admission
	}
	s.retainedFailureMu.Unlock()
	admission.mu.Lock()
	if admission.failure == nil {
		admission.failure = failure
	}
	admission.mu.Unlock()
}

func (s *Server) completeLegacyImport(
	conn net.Conn,
	selection importSelection,
) (usb.Device, func(), error) {
	chosen := selection.dev
	if _, retained := chosen.(retainedusb.ImportDevice); retained {
		_ = writeImportFailure(conn)
		return nil, nil, fmt.Errorf(
			"retained device %s cannot use legacy import", selection.busID)
	}
	releaseImport, claimed := s.claimDeviceImport(chosen)
	if !claimed {
		_ = writeImportFailure(conn)
		return nil, nil, fmt.Errorf(
			"device %s is already imported", selection.busID)
	}
	packet := buildImportSuccessPacket(selection)
	if _, err := conn.Write(packet); err != nil {
		releaseImport()
		return nil, nil, fmt.Errorf("write import reply failed: %w", err)
	}
	return chosen, releaseImport, nil
}

func buildImportSuccessPacket(selection importSelection) []byte {
	var buf bytes.Buffer
	rep := usbip.MgmtHeader{
		Version: usbip.Version, Command: usbip.OpRepImport, Status: 0,
	}
	_ = rep.Write(&buf)
	descriptor := selection.descriptor
	exp := usbip.ExportedDevice{
		ExportMeta:          selection.meta,
		Speed:               descriptor.Device.Speed,
		IDVendor:            descriptor.Device.IDVendor,
		IDProduct:           descriptor.Device.IDProduct,
		BcdDevice:           descriptor.Device.BcdDevice,
		BDeviceClass:        descriptor.Device.BDeviceClass,
		BDeviceSubClass:     descriptor.Device.BDeviceSubClass,
		BDeviceProtocol:     descriptor.Device.BDeviceProtocol,
		BConfigurationValue: descriptorConfigurationValue(descriptor),
		BNumConfigurations:  descriptor.Device.BNumConfigurations,
		BNumInterfaces:      descriptor.NumInterfaces(),
	}
	for _, iface := range descriptorListInterfaces(descriptor) {
		exp.Interfaces = append(exp.Interfaces, usbip.InterfaceDesc{
			Class:    iface.Descriptor.BInterfaceClass,
			SubClass: iface.Descriptor.BInterfaceSubClass,
			Protocol: iface.Descriptor.BInterfaceProtocol,
		})
	}
	_ = exp.WriteImport(&buf)
	return buf.Bytes()
}

func retainedImportCapability(
	dev usb.Device,
) (
	owner retainedusb.ImportOwner,
	deviceID uint64,
	retained bool,
) {
	defer func() {
		if recover() != nil {
			owner, deviceID, retained = nil, 0, false
		}
	}()
	candidate, ok := dev.(retainedusb.ImportDevice)
	if !ok || candidate == nil || retainedImportHasAmbiguousContracts(dev) {
		return nil, 0, false
	}
	owner = candidate.RetainedUSBImportOwner()
	deviceID = candidate.RetainedUSBImportDeviceID()
	if interfaceIsNil(owner) || deviceID == 0 {
		return nil, 0, false
	}
	return owner, deviceID, true
}

type retainedImportCapabilityResult struct {
	owner    retainedusb.ImportOwner
	deviceID uint64
}

func boundedRetainedImportCapability(
	dev usb.Device,
	deadline time.Time,
	lease *retainedDeviceCallbackLease,
) (retainedusb.ImportOwner, uint64, error) {
	if lease == nil || !lease.authenticates(lease.server, dev) {
		return nil, 0, errRetainedImportInvalid
	}
	result, err := invokeRetainedImportStep(
		"retained import capability", deadline,
		func() (retainedImportCapabilityResult, error) {
			owner, deviceID, retained := retainedImportCapability(dev)
			if !retained {
				return retainedImportCapabilityResult{},
					errRetainedImportInvalid
			}
			return retainedImportCapabilityResult{
				owner: owner, deviceID: deviceID,
			}, nil
		})
	if err != nil {
		lease.reject(err)
		return nil, 0, err
	}
	if err := lease.bindCapability(result.owner, result.deviceID); err != nil {
		if errors.Is(err, errRetainedImportQuarantined) {
			lease.reject(err)
		}
		return nil, 0, err
	}
	if !lease.authenticates(lease.server, dev) {
		return nil, 0, errRetainedImportQuarantined
	}
	return result.owner, result.deviceID, nil
}

// sealRetainedImportDescriptor validates the retained path's fixed descriptor
// shape before allocating. It copies only the scalar topology consumed by
// USB/IP discovery, import framing, and lifecycle routing. Retained EP0 belongs
// exclusively to ImportOwner, so legacy string, Microsoft OS, HID, IAD, and
// class-specific descriptor collections are neither retained nor copied.
func sealRetainedImportDescriptor(
	source *usb.Descriptor,
) (usb.Descriptor, error) {
	if source == nil || source.Device.BNumConfigurations != 1 ||
		!validRetainedControlEndpointForSpeed(source.Device) ||
		source.Configuration.BConfigurationValue == 0 ||
		len(source.Associations) != 0 || len(source.Interfaces) != 1 {
		return usb.Descriptor{}, errRetainedImportInvalid
	}
	retainedInterface := &source.Interfaces[0]
	if retainedInterface.Descriptor.BInterfaceNumber != 0 ||
		retainedInterface.Descriptor.BAlternateSetting != 0 ||
		retainedInterface.Descriptor.BNumEndpoints != 2 ||
		len(retainedInterface.Endpoints) != 2 ||
		retainedInterface.HID != nil ||
		len(retainedInterface.ClassDescriptors) != 0 {
		return usb.Descriptor{}, errRetainedImportInvalid
	}
	var directionMask uint8
	sealedEndpoints := make([]usb.EndpointDescriptor, 2)
	for index := range retainedInterface.Endpoints {
		endpoint := retainedInterface.Endpoints[index]
		if endpoint.BEndpointAddress&0x0f == 0 ||
			endpoint.BEndpointAddress&0x70 != 0 ||
			endpoint.BMAttributes != 0x03 ||
			!validRetainedInterruptEndpointForSpeed(
				source.Device, endpoint) ||
			len(endpoint.Trailing) != 0 ||
			len(endpoint.ClassDescriptors) != 0 {
			return usb.Descriptor{}, errRetainedImportInvalid
		}
		directionBit := uint8(1)
		if endpoint.BEndpointAddress&0x80 != 0 {
			directionBit = 2
		}
		if directionMask&directionBit != 0 {
			return usb.Descriptor{}, errRetainedImportInvalid
		}
		directionMask |= directionBit
		sealedEndpoints[index] = usb.EndpointDescriptor{
			BEndpointAddress: endpoint.BEndpointAddress,
			BMAttributes:     endpoint.BMAttributes,
			WMaxPacketSize:   endpoint.WMaxPacketSize,
			BInterval:        endpoint.BInterval,
		}
	}
	if directionMask != 3 {
		return usb.Descriptor{}, errRetainedImportInvalid
	}
	return usb.Descriptor{
		Device:        source.Device,
		Configuration: source.Configuration,
		Interfaces: []usb.InterfaceConfig{{
			Descriptor: retainedInterface.Descriptor,
			Endpoints:  sealedEndpoints,
		}},
	}, nil
}

func validRetainedControlEndpointForSpeed(device usb.DeviceDescriptor) bool {
	switch device.Speed {
	case 1:
		return device.BMaxPacketSize0 == 8
	case 2:
		switch device.BMaxPacketSize0 {
		case 8, 16, 32, 64:
			return true
		default:
			return false
		}
	case 3:
		return device.BMaxPacketSize0 == 64
	default:
		// The retained fixed topology has no SuperSpeed endpoint companion
		// descriptors and therefore cannot lawfully advertise speed 4+.
		return false
	}
}

func validRetainedInterruptEndpointForSpeed(
	device usb.DeviceDescriptor,
	endpoint usb.EndpointDescriptor,
) bool {
	if endpoint.WMaxPacketSize == 0 || endpoint.WMaxPacketSize&0xf800 != 0 ||
		endpoint.BInterval == 0 {
		return false
	}
	switch device.Speed {
	case 1:
		return endpoint.WMaxPacketSize <= 8
	case 2:
		return endpoint.WMaxPacketSize <= 64
	case 3:
		return endpoint.WMaxPacketSize <= 1024 && endpoint.BInterval <= 16
	default:
		return false
	}
}

func retainedImportHasAmbiguousContracts(dev usb.Device) bool {
	if dev == nil {
		return true
	}
	if _, overlap := dev.(usb.TransactionalControlDevice); overlap {
		return true
	}
	if _, overlap := dev.(usb.TransactionalInterruptOutDevice); overlap {
		return true
	}
	if _, overlap := dev.(inputpresentation.PreparedSource); overlap {
		return true
	}
	return false
}

func parseUSBIPBusID(field []byte) (string, error) {
	if len(field) != busIDSize {
		return "", fmt.Errorf("USB/IP busid field length %d, want %d",
			len(field), busIDSize)
	}
	end := bytes.IndexByte(field, 0)
	if end < 0 {
		return "", fmt.Errorf("USB/IP busid is not NUL-terminated")
	}
	if end == 0 {
		return "", fmt.Errorf("USB/IP busid is empty")
	}
	return string(field[:end]), nil
}

func parseUSBIPBusAddress(value string) (uint32, uint32, error) {
	busText, deviceText, found := strings.Cut(value, "-")
	if !found || busText == "" || deviceText == "" ||
		strings.Contains(deviceText, "-") {
		return 0, 0, fmt.Errorf("invalid USB/IP busid %q", value)
	}
	bus, err := strconv.ParseUint(busText, 10, 32)
	if err != nil || bus == 0 {
		return 0, 0, fmt.Errorf("invalid USB/IP bus number %q", busText)
	}
	device, err := strconv.ParseUint(deviceText, 10, 32)
	if err != nil || device == 0 {
		return 0, 0, fmt.Errorf("invalid USB/IP device number %q", deviceText)
	}
	if canonical := fmt.Sprintf("%d-%d", bus, device); canonical != value {
		return 0, 0, fmt.Errorf("non-canonical USB/IP busid %q", value)
	}
	return uint32(bus), uint32(device), nil
}

func writeImportFailure(conn net.Conn) error {
	reply := usbip.MgmtHeader{
		Version: usbip.Version, Command: usbip.OpRepImport, Status: 1,
	}
	return reply.Write(conn)
}

// claimDeviceImport gives one USB/IP stream exclusive ownership of a device.
// The tokenized idempotent release prevents a late/double cleanup from
// deleting a successor's lease.
func (s *Server) claimDeviceImport(dev usb.Device) (func(), bool) {
	if s == nil || dev == nil {
		return func() {}, false
	}
	s.importsMu.Lock()
	if s.activeImports == nil {
		s.activeImports = make(map[usb.Device]uint64)
	}
	if _, busy := s.activeImports[dev]; busy {
		s.importsMu.Unlock()
		return func() {}, false
	}
	s.nextImportLease++
	if s.nextImportLease == 0 {
		s.nextImportLease = 1
	}
	lease := s.nextImportLease
	s.activeImports[dev] = lease
	s.importsMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			s.importsMu.Lock()
			if s.activeImports[dev] == lease {
				delete(s.activeImports, dev)
			}
			s.importsMu.Unlock()
		})
	}, true
}

// getAllDeviceMetas aggregates device metas from all registered busses.
func (s *Server) getAllDeviceMetas() []virtualbus.DeviceMeta {
	s.busesMu.Lock()
	defer s.busesMu.Unlock()
	out := []virtualbus.DeviceMeta{}
	for _, b := range s.busses {
		out = append(out, b.GetAllDeviceMetas()...)
	}
	return out
}

type logConn struct {
	net.Conn
	s *Server
}

func (lc *logConn) Read(p []byte) (int, error) {
	n, err := lc.Conn.Read(p)
	if n > 0 && lc.s.rawLogger != nil {
		lc.s.rawLogger.Log(true, p[:n])
	}
	return n, err
}

func (lc *logConn) Write(p []byte) (int, error) {
	n, err := lc.Conn.Write(p)
	if n > 0 && lc.s.rawLogger != nil {
		lc.s.rawLogger.Log(false, p[:n])
	}
	return n, err
}

func endpointIsIsochronous(desc *usb.Descriptor, ep, dir uint32) bool {
	if desc == nil || ep == 0 {
		return false
	}

	epAddr := uint8(ep) & 0x0f
	if dir == usbip.DirIn {
		epAddr |= 0x80
	}
	for i := range desc.Interfaces {
		for _, endpoint := range desc.Interfaces[i].Endpoints {
			if endpoint.BEndpointAddress == epAddr && endpoint.BMAttributes&0x03 == 0x01 {
				return true
			}
		}
	}
	return false
}

func usbServiceInterval(speed uint32, bInterval uint8) time.Duration {
	if bInterval == 0 {
		return 0
	}
	if speed >= 3 {
		return time.Duration(1<<(bInterval-1)) * 125 * time.Microsecond
	}
	return time.Duration(bInterval) * time.Millisecond
}

func isClientDisconnect(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		switch t := opErr.Err.(type) {
		case syscall.Errno:
			if t == syscall.ECONNRESET || t == syscall.EPIPE {
				return true
			}
		}
	}
	e := strings.ToLower(err.Error())
	if strings.Contains(e, "connection reset by peer") || strings.Contains(e, "forcibly closed") || strings.Contains(e, "an existing connection was forcibly closed") || strings.Contains(e, "aborted") {
		return true
	}
	return false
}

func (s *Server) processSubmit(ctx context.Context, dev usb.Device, ep uint32, dir uint32, setup []byte, out []byte) []byte {
	if ep != 0 {
		return dev.HandleTransfer(ctx, ep, dir, out)
	}
	lifecycle := s.parseControlLifecycleSubmission(
		dev, ep, dir, uint32(len(out)), setup,
	)
	return s.processSubmitWithLifecycle(ctx, dev, ep, dir, setup, out, lifecycle)
}

func (s *Server) processSubmitWithLifecycle(
	ctx context.Context,
	dev usb.Device,
	ep uint32,
	dir uint32,
	setup []byte,
	out []byte,
	lifecycle controlLifecycleSetup,
) []byte {
	if ep != 0 {
		return dev.HandleTransfer(ctx, ep, dir, out)
	}
	if len(setup) != 8 {
		s.logger.Debug("EP0 submit with invalid setup size", "setupLen", len(setup), "setup", setup)
		return nil
	}
	bm := setup[0]
	breq := setup[1]
	wValue := binary.LittleEndian.Uint16(setup[2:4])
	wIndex := binary.LittleEndian.Uint16(setup[4:6])
	wLength := binary.LittleEndian.Uint16(setup[6:8])
	desc := dev.GetDescriptor()

	if breq == usbReqGetStatus {
		return []byte{0x00, 0x00}
	}
	if breq == usbReqSetAddress && bm == usbReqTypeStandardToDevice {
		return nil
	}
	if lifecycle.recognized {
		if lifecycle.accepted {
			switch lifecycle.kind {
			case controlLifecycleClearEndpointHalt:
				if resetter, ok := dev.(usb.EndpointResetDevice); ok {
					resetter.ResetEndpoint(lifecycle.endpointAddress)
				}
			case controlLifecycleSetInterface:
				s.setInterfaceAlt(dev, lifecycle.interfaceNumber,
					lifecycle.alternateSetting)
				s.notifyInterfaceAlt(dev, lifecycle.interfaceNumber,
					lifecycle.alternateSetting)
			case controlLifecycleSetConfiguration:
				s.setDeviceConfiguration(dev,
					lifecycle.configurationValue)
				s.resetInterfaceAlts(dev)
			}
		}
		return nil
	}
	if breq == usbReqGetConfiguration && bm == usbReqTypeStandardFromDevice {
		return []byte{s.getDeviceConfiguration(dev)}
	}
	if breq == usbReqGetInterface && bm == usbReqTypeStandardToInterface {
		return []byte{s.getInterfaceAlt(dev, uint8(wIndex&usbIfaceIndexMask))}
	}

	if breq == usbReqGetDescriptor && bm == usbReqTypeStandardFromDevice {
		dtype := uint8(wValue >> 8)
		dindex := uint8(wValue & 0xff)
		var data []byte
		switch dtype {
		case usbDescTypeDevice:
			data = desc.Bytes()
		case usbDescTypeConfiguration:
			data = s.buildConfigDescriptor(desc)
		case usbDescTypeString:
			if dindex == 0xEE && desc.MicrosoftOS10 != nil {
				data = desc.MicrosoftOS10.StringDescriptor()
			} else if s, ok := desc.Strings[dindex]; ok {
				data = usb.EncodeStringDescriptor(s)
			}
		}
		if len(data) == 0 {
			return nil
		}
		if int(wLength) < len(data) {
			return data[:wLength]
		}
		return data
	}

	if desc.MicrosoftOS10 != nil &&
		(bm == 0xC0 || bm == 0xC1) &&
		(breq == desc.MicrosoftOS10.EffectiveVendorCode() ||
			wIndex == 0x0004 || wIndex == 0x0005) {
		if data, ok := desc.MicrosoftOS10.ControlResponse(wValue, wIndex); ok {
			if int(wLength) < len(data) {
				return data[:wLength]
			}
			return data
		}
	}

	if breq == usbReqGetDescriptor && bm == usbReqTypeStandardToInterface {
		dtype := uint8(wValue >> 8)
		iface := uint8(wIndex & 0xff)
		var data []byte
		if ifaceConf, ok := desc.Interface(iface); ok {
			if ifaceConf.HID != nil {
				switch dtype {
				case usbDescTypeHID:
					d, err := ifaceConf.HID.DescriptorBytes()
					if err != nil {
						s.logger.Error("failed to build HID descriptor", "iface", iface, "error", err)
						return nil
					}
					data = []byte(d)
				case usbDescTypeHIDReport:
					d, err := ifaceConf.HID.ReportBytes()
					if err != nil {
						s.logger.Error("failed to build HID report descriptor", "iface", iface, "error", err)
						return nil
					}
					data = []byte(d)
				}
			}
			if len(data) == 0 {
				for _, cd := range ifaceConf.ClassDescriptors {
					if cd.DescriptorType == dtype {
						data = []byte(cd.Bytes())
						break
					}
				}
			}
		}
		if len(data) == 0 {
			return nil
		}
		if int(wLength) < len(data) {
			return data[:wLength]
		}
		return data
	}

	// Handle common HID state requests before controller-specific dispatch. The
	// descriptor slice contains alternate-setting entries, so interface numbers
	// must be resolved logically rather than used as slice indexes.
	if ifaceConfig, ok := desc.Interface(uint8(wIndex & usbIfaceIndexMask)); ok &&
		ifaceConfig.Descriptor.BInterfaceClass == usbInterfaceClassHID {
		switch {
		case bm == hidReqTypeIn && breq == hidReqGetIdle:
			return []byte{0x00}
		case bm == hidReqTypeOut && breq == hidReqSetIdle:
			return nil
		case bm == hidReqTypeIn && breq == hidReqGetProtocol:
			return []byte{0x01}
		case bm == hidReqTypeOut && breq == hidReqSetProtocol:
			return nil
		}
	}

	if cd, ok := dev.(usb.ControlDevice); ok {
		if resp, handled := cd.HandleControl(bm, breq, wValue, wIndex, wLength, out); handled {
			if resp == nil {
				return nil
			}
			if int(wLength) < len(resp) {
				return resp[:wLength]
			}
			return resp
		}
	}

	if ifaceConfig, ok := desc.Interface(uint8(wIndex & usbIfaceIndexMask)); ok {
		if ifaceConfig.Descriptor.BInterfaceClass == usbInterfaceClassHID {
			switch {
			case (bm == hidReqTypeIn || bm == hidReqTypeOut) && (breq == hidReqGetReport || breq == hidReqSetReport):
				return nil
			}
		}
	}

	if (bm & usbReqTypeMask) != usbReqTypeClass {
		s.logger.Debug("EP0 control unhandled", "bmRequestType", bm, "bRequest", breq, "wValue", wValue, "wIndex", wIndex, "wLength", wLength)
	}

	return nil
}

func (s *Server) buildConfigDescriptor(desc *usb.Descriptor) []byte {
	var b bytes.Buffer
	configValue := descriptorConfigurationValue(desc)
	attrs := desc.Configuration.BMAttributes
	if attrs == 0 {
		attrs = usbConfigAttrBusPowered
	}
	maxPower := desc.Configuration.BMaxPower
	if maxPower == 0 {
		maxPower = usbConfigMaxPower100mA
	}
	h := usb.ConfigHeader{
		WTotalLength:        0, // to be patched
		BNumInterfaces:      desc.NumInterfaces(),
		BConfigurationValue: configValue,
		IConfiguration:      desc.Configuration.IConfiguration,
		BMAttributes:        attrs,
		BMaxPower:           maxPower,
	}
	h.Write(&b)
	for _, iface := range desc.Interfaces {
		for _, iad := range desc.Associations {
			if iad.BFirstInterface == iface.Descriptor.BInterfaceNumber && iface.Descriptor.BAlternateSetting == 0 {
				iad.Write(&b)
			}
		}
		iface.Descriptor.Write(&b)
		if iface.HID != nil {
			hd, err := iface.HID.DescriptorBytes()
			if err != nil {
				s.logger.Error("failed to build HID descriptor", "iface", iface.Descriptor.BInterfaceNumber, "error", err)
				// Stall/return minimal config descriptor.
				return nil
			}
			b.Write([]byte(hd))
		}
		for _, cd := range iface.ClassDescriptors {
			b.Write([]byte(cd.Bytes()))
		}
		for _, ep := range iface.Endpoints {
			ep.Write(&b)
			for _, cd := range ep.ClassDescriptors {
				b.Write([]byte(cd.Bytes()))
			}
		}
	}

	data := b.Bytes()
	binary.LittleEndian.PutUint16(data[2:4], uint16(len(data)))
	return data
}

func descriptorListInterfaces(desc *usb.Descriptor) []usb.InterfaceConfig {
	out := make([]usb.InterfaceConfig, 0, desc.NumInterfaces())
	seen := map[uint8]struct{}{}
	for _, iface := range desc.Interfaces {
		n := iface.Descriptor.BInterfaceNumber
		if _, ok := seen[n]; ok || iface.Descriptor.BAlternateSetting != 0 {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, iface)
	}
	for _, iface := range desc.Interfaces {
		n := iface.Descriptor.BInterfaceNumber
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, iface)
	}
	return out
}

func descriptorHasInterfaceAlt(desc *usb.Descriptor, ifaceNumber, altSetting uint8) bool {
	if desc == nil {
		return false
	}
	for _, iface := range desc.Interfaces {
		if iface.Descriptor.BInterfaceNumber == ifaceNumber &&
			iface.Descriptor.BAlternateSetting == altSetting {
			return true
		}
	}
	return false
}

func descriptorConfigurationValue(desc *usb.Descriptor) uint8 {
	if desc == nil || desc.Configuration.BConfigurationValue == 0 {
		return usbConfigValueDefault
	}
	return desc.Configuration.BConfigurationValue
}

func (s *Server) descriptorHasActiveHaltEndpoint(
	dev usb.Device,
	desc *usb.Descriptor,
	endpointAddress uint8,
) bool {
	if endpointAddress&0x70 != 0 {
		return false
	}
	dir := uint32(usbip.DirOut)
	if endpointAddress&0x80 != 0 {
		dir = usbip.DirIn
	}
	binding, found := s.activeEndpointBindingDescriptor(dev, desc,
		uint32(endpointAddress&0x0f), dir)
	return found && binding.descriptor.BEndpointAddress == endpointAddress &&
		binding.descriptor.BMAttributes&0x03 != 0x01
}

// activeEndpointBinding resolves one endpoint against the connection-owned
// configuration and interface alternate settings. More than one active match
// is a malformed descriptor topology and fails closed.
func (s *Server) activeEndpointBinding(
	dev usb.Device,
	ep, dir uint32,
) (endpointDescriptorBinding, bool) {
	if dev == nil {
		return endpointDescriptorBinding{}, false
	}
	return s.activeEndpointBindingDescriptor(dev, dev.GetDescriptor(), ep, dir)
}

func (s *Server) activeEndpointBindingDescriptor(
	dev usb.Device,
	desc *usb.Descriptor,
	ep, dir uint32,
) (endpointDescriptorBinding, bool) {
	if dev == nil || ep == 0 || s.getDeviceConfiguration(dev) == 0 {
		return endpointDescriptorBinding{}, false
	}
	if desc == nil {
		return endpointDescriptorBinding{}, false
	}
	address := uint8(ep & 0x0f)
	if dir == usbip.DirIn {
		address |= 0x80
	}
	var match endpointDescriptorBinding
	found := false
	for ifaceIndex := range desc.Interfaces {
		iface := &desc.Interfaces[ifaceIndex]
		if iface.Descriptor.BAlternateSetting != s.getInterfaceAlt(
			dev, iface.Descriptor.BInterfaceNumber,
		) {
			continue
		}
		for endpointIndex := range iface.Endpoints {
			endpoint := &iface.Endpoints[endpointIndex]
			if endpoint.BEndpointAddress != address {
				continue
			}
			if found {
				return endpointDescriptorBinding{}, false
			}
			match = endpointDescriptorBinding{
				interfaceNumber:  iface.Descriptor.BInterfaceNumber,
				alternateSetting: iface.Descriptor.BAlternateSetting,
				descriptor:       endpoint,
			}
			found = true
		}
	}
	return match, found
}

// getDeviceConfiguration returns the connection's selected configuration.
// Before an explicit SET_CONFIGURATION, the USB/IP exported-device contract
// advertises the descriptor's active configuration value. An explicit zero is
// retained and must not be confused with an absent map entry.
func (s *Server) getDeviceConfiguration(dev usb.Device) uint8 {
	if dev == nil {
		return 0
	}
	s.configurationsMu.Lock()
	defer s.configurationsMu.Unlock()
	if value, ok := s.configurations[dev]; ok {
		return value
	}
	return descriptorConfigurationValue(dev.GetDescriptor())
}

func (s *Server) setDeviceConfiguration(dev usb.Device, value uint8) {
	if dev == nil {
		return
	}
	s.configurationsMu.Lock()
	defer s.configurationsMu.Unlock()
	if s.configurations == nil {
		s.configurations = make(map[usb.Device]uint8)
	}
	s.configurations[dev] = value
}

func (s *Server) clearDeviceConfiguration(dev usb.Device) {
	s.configurationsMu.Lock()
	defer s.configurationsMu.Unlock()
	delete(s.configurations, dev)
}

func descriptorInterfaceNumbers(desc *usb.Descriptor) []uint8 {
	out := make([]uint8, 0, desc.NumInterfaces())
	seen := map[uint8]struct{}{}
	for _, iface := range desc.Interfaces {
		n := iface.Descriptor.BInterfaceNumber
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	return out
}

func (s *Server) notifyInterfaceAlt(dev usb.Device, iface, alt uint8) {
	if notifier, ok := dev.(usb.InterfaceAltSettingDevice); ok {
		notifier.SetInterfaceAltSetting(iface, alt)
	}
}

func (s *Server) notifyInterfaceAltsCleared(dev usb.Device) {
	notifier, ok := dev.(usb.InterfaceAltSettingDevice)
	if !ok {
		return
	}

	for _, iface := range descriptorInterfaceNumbers(dev.GetDescriptor()) {
		notifier.SetInterfaceAltSetting(iface, 0)
	}
}

func (s *Server) getInterfaceAlt(dev usb.Device, iface uint8) uint8 {
	s.altsMu.Lock()
	defer s.altsMu.Unlock()
	if s.alts == nil {
		return 0
	}
	if devAlts, ok := s.alts[dev]; ok {
		return devAlts[iface]
	}
	return 0
}

func (s *Server) setInterfaceAlt(dev usb.Device, iface, alt uint8) {
	s.altsMu.Lock()
	defer s.altsMu.Unlock()
	if s.alts == nil {
		s.alts = make(map[usb.Device]map[uint8]uint8)
	}
	devAlts := s.alts[dev]
	if devAlts == nil {
		devAlts = make(map[uint8]uint8)
		s.alts[dev] = devAlts
	}
	devAlts[iface] = alt
}

func (s *Server) clearInterfaceAlt(dev usb.Device) {
	s.altsMu.Lock()
	defer s.altsMu.Unlock()
	delete(s.alts, dev)
}

func (s *Server) resetInterfaceAlts(dev usb.Device) {
	s.clearInterfaceAlt(dev)
	s.notifyInterfaceAltsCleared(dev)
}
