//go:build windows

package cmd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/Alia5/VIIPER/internal/server/api"
	"github.com/Alia5/VIIPER/internal/server/api/auth"
	"github.com/Alia5/VIIPER/internal/transport/udecx"
	"github.com/Alia5/VIIPER/viiperclient"
	"github.com/Alia5/VIIPER/viipertypes"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	nativeBrokerDisplayName          = "VIIPER Native UDE Broker"
	nativeBrokerDescription          = "Provides authenticated native virtual-controller transport for DS4Windows."
	nativeBrokerLogName              = "viiper-native-broker.log"
	nativeServiceAccount             = "LocalSystem"
	nativeServiceRecoveryResetSecond = 15 * 60
	nativeServiceInstallTimeout      = 45 * time.Second
	nativeServiceStatePoll           = 100 * time.Millisecond
	nativeInstallMutexName           = `Global\VIIPER.NativeBroker.Install.v1`
	nativeBrokerServiceSDDL          = "O:BAD:P(A;;GA;;;SY)(A;;GA;;;BA)"
	nativeBrokerDirectorySDDL        = "O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;GRGX;;;BU)"
	nativeBrokerExecutableSDDL       = "O:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;GRGX;;;BU)"
)

var nativeServiceRecoveryActions = []mgr.RecoveryAction{
	{Type: mgr.ServiceRestart, Delay: 2 * time.Second},
	{Type: mgr.ServiceRestart, Delay: 15 * time.Second},
	// SCM repeats the final action for later failures. Ending with NoAction is
	// what makes this recovery policy bounded instead of a permanent restart loop.
	{Type: mgr.NoAction},
}

var expandEnvironmentStringsForUserW = windows.NewLazySystemDLL("userenv.dll").NewProc("ExpandEnvironmentStringsForUserW")

type nativeSCM interface {
	OpenService(string) (nativeManagedService, error)
	CreateService(string, string, mgr.Config, ...string) (nativeManagedService, error)
	Close() error
}

type nativeManagedService interface {
	Config() (mgr.Config, error)
	UpdateConfig(mgr.Config) error
	SecurityDescriptor() (string, error)
	SetSecurityDescriptor(string) error
	Query() (svc.Status, error)
	ProcessID() (uint32, error)
	Start(...string) error
	Control(svc.Cmd) (svc.Status, error)
	Delete() error
	SetRecoveryActions([]mgr.RecoveryAction, uint32) error
	SetRecoveryActionsExact([]mgr.RecoveryAction, uint32) error
	RecoveryActions() ([]mgr.RecoveryAction, error)
	ResetPeriod() (uint32, error)
	SetRecoveryActionsOnNonCrashFailures(bool) error
	RecoveryActionsOnNonCrashFailures() (bool, error)
	Close() error
}

type windowsNativeSCM struct{ manager *mgr.Mgr }

func (m *windowsNativeSCM) OpenService(name string) (nativeManagedService, error) {
	service, err := m.manager.OpenService(name)
	if err != nil {
		return nil, err
	}
	return &windowsNativeService{service: service}, nil
}

func (m *windowsNativeSCM) CreateService(
	name, executable string,
	config mgr.Config,
	args ...string,
) (nativeManagedService, error) {
	service, err := m.manager.CreateService(name, executable, config, args...)
	if err != nil {
		return nil, err
	}
	return &windowsNativeService{service: service}, nil
}

func (m *windowsNativeSCM) Close() error { return m.manager.Disconnect() }

type windowsNativeService struct{ service *mgr.Service }

func (s *windowsNativeService) Config() (mgr.Config, error) { return s.service.Config() }
func (s *windowsNativeService) UpdateConfig(config mgr.Config) error {
	return updateNativeServiceConfigExact(s.service.Handle, config)
}
func (s *windowsNativeService) SecurityDescriptor() (string, error) {
	return nativeObjectSecurityDescriptor(s.service.Handle, windows.SE_SERVICE)
}
func (s *windowsNativeService) SetSecurityDescriptor(sddl string) error {
	return setNativeObjectSecurityDescriptor(s.service.Handle, windows.SE_SERVICE, sddl)
}
func (s *windowsNativeService) Query() (svc.Status, error) { return s.service.Query() }
func (s *windowsNativeService) ProcessID() (uint32, error) {
	status := windows.SERVICE_STATUS_PROCESS{}
	var needed uint32
	if err := windows.QueryServiceStatusEx(
		s.service.Handle,
		windows.SC_STATUS_PROCESS_INFO,
		(*byte)(unsafe.Pointer(&status)),
		uint32(unsafe.Sizeof(status)),
		&needed,
	); err != nil {
		return 0, err
	}
	return status.ProcessId, nil
}
func (s *windowsNativeService) Start(args ...string) error { return s.service.Start(args...) }
func (s *windowsNativeService) Control(command svc.Cmd) (svc.Status, error) {
	return s.service.Control(command)
}
func (s *windowsNativeService) Delete() error { return s.service.Delete() }
func (s *windowsNativeService) SetRecoveryActions(actions []mgr.RecoveryAction, reset uint32) error {
	return s.service.SetRecoveryActions(actions, reset)
}
func (s *windowsNativeService) SetRecoveryActionsExact(actions []mgr.RecoveryAction, reset uint32) error {
	if len(actions) != 0 {
		return s.service.SetRecoveryActions(actions, reset)
	}
	// SERVICE_FAILURE_ACTIONS does not permit an empty action array with a
	// nonzero reset period: a NULL Actions pointer leaves both values unchanged,
	// while a non-NULL pointer with ActionsCount == 0 deletes both. Reject the
	// unrepresentable state before mutation in openAndSnapshotNativeService.
	if reset != 0 {
		return errors.New("Windows SCM cannot persist empty recovery actions with a nonzero reset period")
	}
	dummyAction := windows.SC_ACTION{}
	failureActions := windows.SERVICE_FAILURE_ACTIONS{Actions: &dummyAction}
	return windows.ChangeServiceConfig2(
		s.service.Handle,
		windows.SERVICE_CONFIG_FAILURE_ACTIONS,
		(*byte)(unsafe.Pointer(&failureActions)),
	)
}
func (s *windowsNativeService) RecoveryActions() ([]mgr.RecoveryAction, error) {
	return s.service.RecoveryActions()
}
func (s *windowsNativeService) ResetPeriod() (uint32, error) { return s.service.ResetPeriod() }
func (s *windowsNativeService) SetRecoveryActionsOnNonCrashFailures(value bool) error {
	return s.service.SetRecoveryActionsOnNonCrashFailures(value)
}
func (s *windowsNativeService) RecoveryActionsOnNonCrashFailures() (bool, error) {
	return s.service.RecoveryActionsOnNonCrashFailures()
}
func (s *windowsNativeService) Close() error { return s.service.Close() }

func updateNativeServiceConfigExact(handle windows.Handle, config mgr.Config) error {
	if strings.TrimSpace(config.BinaryPathName) == "" || strings.IndexByte(config.BinaryPathName, 0) >= 0 {
		return errors.New("service binary path must be nonempty and contain no NUL")
	}
	binaryPath, err := windows.UTF16PtrFromString(config.BinaryPathName)
	if err != nil {
		return err
	}
	loadOrderGroup, err := windows.UTF16PtrFromString(config.LoadOrderGroup)
	if err != nil {
		return err
	}
	dependencies, err := nativeServiceDependenciesBlock(config.Dependencies)
	if err != nil {
		return err
	}
	serviceAccount := config.ServiceStartName
	if isLocalSystemServiceAccount(serviceAccount) {
		serviceAccount = nativeServiceAccount
	}
	account, err := windows.UTF16PtrFromString(serviceAccount)
	if err != nil {
		return err
	}
	// The LocalSystem password is explicitly empty. NULL would mean "leave the
	// old password unchanged", which is not an exact configuration operation.
	emptyPassword, err := windows.UTF16PtrFromString("")
	if err != nil {
		return err
	}
	displayName, err := windows.UTF16PtrFromString(config.DisplayName)
	if err != nil {
		return err
	}
	if err := windows.ChangeServiceConfig(
		handle,
		config.ServiceType,
		config.StartType,
		config.ErrorControl,
		binaryPath,
		loadOrderGroup,
		nil,
		&dependencies[0],
		account,
		emptyPassword,
		displayName,
	); err != nil {
		return err
	}
	if err := windows.ChangeServiceConfig2(
		handle,
		windows.SERVICE_CONFIG_SERVICE_SID_INFO,
		(*byte)(unsafe.Pointer(&config.SidType)),
	); err != nil {
		return err
	}
	delayed := windows.SERVICE_DELAYED_AUTO_START_INFO{}
	if config.DelayedAutoStart {
		delayed.IsDelayedAutoStartUp = 1
	}
	if err := windows.ChangeServiceConfig2(
		handle,
		windows.SERVICE_CONFIG_DELAYED_AUTO_START_INFO,
		(*byte)(unsafe.Pointer(&delayed)),
	); err != nil {
		return err
	}
	descriptionValue, err := windows.UTF16PtrFromString(config.Description)
	if err != nil {
		return err
	}
	description := windows.SERVICE_DESCRIPTION{Description: descriptionValue}
	return windows.ChangeServiceConfig2(
		handle,
		windows.SERVICE_CONFIG_DESCRIPTION,
		(*byte)(unsafe.Pointer(&description)),
	)
}

func nativeServiceDependenciesBlock(dependencies []string) ([]uint16, error) {
	block := make([]uint16, 0, 2)
	for _, dependency := range dependencies {
		if dependency == "" || strings.IndexByte(dependency, 0) >= 0 {
			return nil, errors.New("service dependency must be nonempty and contain no NUL")
		}
		value, err := windows.UTF16FromString(dependency)
		if err != nil {
			return nil, err
		}
		block = append(block, value...)
	}
	// ChangeServiceConfig requires a non-NULL empty string to clear existing
	// dependencies. Every nonempty block also needs the second terminating NUL.
	block = append(block, 0)
	if len(block) == 1 {
		block = append(block, 0)
	}
	return block, nil
}

type nativeServiceSnapshot struct {
	exists               bool
	config               mgr.Config
	status               svc.Status
	securityDescriptor   string
	recoveryActions      []mgr.RecoveryAction
	recoveryResetSeconds uint32
	recoverNonCrash      bool
	releaseExecutable    func()
}

type nativeCredential struct {
	path       string
	password   string
	userSID    string
	created    bool
	replaced   bool
	priorBytes []byte
}

type nativeLegacyCommand struct {
	executable       string
	arguments        []string
	workingDirectory string
	source           nativeLegacyCommandSource
	running          bool
}

type nativeLegacyCommandSource uint8

const (
	legacyCommandRun nativeLegacyCommandSource = iota + 1
)

type nativeLegacyState struct {
	userSID             string
	userHive            registry.Key
	runKey              registry.Key
	runKeyExisted       bool
	runValue            *nativeRunRegistration
	scheduledAction     *nativeLegacyCommand
	scheduledXML        *string
	scheduledCurrentXML *string
	scheduledActive     bool
	scheduledEnabled    bool
	scheduledDisabled   bool
	scheduledStopped    bool
	verifyTaskAction    func() error
	release             func()
	commands            []nativeLegacyCommand
}

type nativeRunRegistration struct {
	value     string
	valueType uint32
}

type nativeScheduledStopResult struct {
	stopped    bool
	disabled   bool
	currentXML string
}

type nativeInstallDependencies struct {
	connectSCM          func() (nativeSCM, error)
	lockExecutable      func(string) (func(), error)
	lockPriorExecutable func(string) (func(), error)
	provisionCredential func() (nativeCredential, error)
	rollbackCredential  func(nativeCredential) error
	preflightDriver     func() error
	snapshotLegacy      func(context.Context) (nativeLegacyState, error)
	stopLegacy          func(context.Context, *nativeLegacyState, *slog.Logger) error
	removeLegacy        func(context.Context, nativeLegacyState) error
	restoreLegacy       func(context.Context, nativeLegacyState) error
	restartLegacy       func(context.Context, nativeLegacyState) error
	verifyBroker        func(context.Context, string) error
	wait                func(context.Context, time.Duration) error
}

func productionNativeInstallDependencies(userSID string) nativeInstallDependencies {
	return nativeInstallDependencies{
		connectSCM: func() (nativeSCM, error) {
			manager, err := mgr.Connect()
			if err != nil {
				return nil, err
			}
			return &windowsNativeSCM{manager: manager}, nil
		},
		lockExecutable:      lockNativeServiceExecutable,
		lockPriorExecutable: lockNativePriorServiceExecutable,
		provisionCredential: func() (nativeCredential, error) {
			return provisionNativeServiceCredential(userSID)
		},
		rollbackCredential: rollbackNativeServiceCredential,
		preflightDriver:    requireNativeUDEBroker,
		snapshotLegacy: func(ctx context.Context) (nativeLegacyState, error) {
			return snapshotNativeLegacyStartup(ctx, userSID)
		},
		stopLegacy:   stopNativeLegacyStartup,
		removeLegacy: removeNativeLegacyRegistrations,
		restoreLegacy: func(ctx context.Context, state nativeLegacyState) error {
			return restoreNativeLegacyRegistrationsAfterRemoval(ctx, state, nil, true)
		},
		restartLegacy: restartNativeLegacyStartup,
		verifyBroker:  verifyNativeBroker,
		wait: func(ctx context.Context, delay time.Duration) error {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
	}
}

func installNativeBroker(logger *slog.Logger, explicitUserSID string) error {
	release, err := acquireNativeInstallMutex(nativeServiceInstallTimeout)
	if err != nil {
		return err
	}
	defer release()
	userSID, err := resolveNativeInstallingUserSID(explicitUserSID)
	if err != nil {
		return err
	}
	executable, err := currentExecutable()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), nativeServiceInstallTimeout)
	defer cancel()
	return installNativeBrokerTransaction(ctx, logger, executable, productionNativeInstallDependencies(userSID))
}

// installNativeBrokerUntil is reserved for the nested native-package commit.
// It shares the outer transaction's absolute deadline rather than granting a
// fresh service-install budget after the driver has already been mutated.
func installNativeBrokerUntil(
	logger *slog.Logger, explicitUserSID string, deadline time.Time,
) error {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return context.DeadlineExceeded
	}
	if remaining > nativeServiceInstallTimeout {
		remaining = nativeServiceInstallTimeout
	}
	release, err := acquireNativeInstallMutex(remaining)
	if err != nil {
		return err
	}
	defer release()
	userSID, err := resolveNativeInstallingUserSID(explicitUserSID)
	if err != nil {
		return err
	}
	executable, err := currentExecutable()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	return installNativeBrokerTransaction(ctx, logger, executable, productionNativeInstallDependencies(userSID))
}

func uninstallNativeBroker(logger *slog.Logger, explicitUserSID string) error {
	release, err := acquireNativeInstallMutex(nativeServiceInstallTimeout)
	if err != nil {
		return err
	}
	defer release()
	userSID, err := resolveNativeInstallingUserSID(explicitUserSID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), nativeServiceInstallTimeout)
	defer cancel()
	dependencies := productionNativeInstallDependencies(userSID)
	manager, err := dependencies.connectSCM()
	if err != nil {
		return fmt.Errorf("connect to Windows Service Control Manager: %w", err)
	}
	defer manager.Close() //nolint:errcheck
	return uninstallNativeBrokerTransaction(ctx, logger, manager, dependencies)
}

func uninstallNativeBrokerTransaction(
	ctx context.Context,
	logger *slog.Logger,
	manager nativeSCM,
	dependencies nativeInstallDependencies,
) (resultErr error) {
	service, before, err := openAndSnapshotNativeService(
		ctx, manager, dependencies.wait, dependencies.lockPriorExecutable,
	)
	if err != nil {
		return err
	}
	if before.releaseExecutable != nil {
		defer before.releaseExecutable()
	}
	if before.exists && !isLocalSystemServiceAccount(before.config.ServiceStartName) {
		if service != nil {
			service.Close() //nolint:errcheck
		}
		return fmt.Errorf(
			"refusing to remove %s because it runs as non-LocalSystem account %q and cannot be transactionally restored",
			NativeBrokerServiceName, before.config.ServiceStartName,
		)
	}
	if service != nil {
		defer service.Close() //nolint:errcheck
	}
	legacy, err := dependencies.snapshotLegacy(ctx)
	if err != nil {
		return fmt.Errorf("snapshot legacy VIIPER startup before uninstall: %w", err)
	}
	if legacy.release != nil {
		defer legacy.release()
	}

	serviceChanged := false
	legacyStopped := false
	registrationsMayHaveChanged := false
	defer func() {
		if resultErr == nil {
			return
		}
		rollbackCtx, cancelRollback := context.WithTimeout(context.Background(), nativeServiceInstallTimeout)
		defer cancelRollback()
		var rollbackErrors []error
		safeToRestartLegacy := true
		if serviceChanged {
			var rollbackErr error
			safeToRestartLegacy, rollbackErr = rollbackNativeService(
				rollbackCtx, manager, service, before, dependencies.wait, nil,
			)
			if rollbackErr != nil {
				rollbackErrors = append(rollbackErrors, rollbackErr)
			}
		}
		// A restored scheduled task can start immediately through a registration
		// trigger or StartWhenAvailable. Do not make any legacy registration live
		// until the rejected service has been stopped/deleted or the prior service
		// has been restored completely.
		if registrationsMayHaveChanged && safeToRestartLegacy {
			if rollbackErr := dependencies.restoreLegacy(rollbackCtx, legacy); rollbackErr != nil {
				safeToRestartLegacy = false
				rollbackErrors = append(rollbackErrors, rollbackErr)
			}
		}
		if legacyStopped && safeToRestartLegacy {
			if rollbackErr := dependencies.restartLegacy(rollbackCtx, legacy); rollbackErr != nil {
				rollbackErrors = append(rollbackErrors, fmt.Errorf("restart legacy VIIPER after uninstall rollback: %w", rollbackErr))
			}
		}
		if len(rollbackErrors) != 0 {
			resultErr = errors.Join(resultErr, errors.Join(rollbackErrors...))
		}
	}()

	if before.exists && before.status.State == svc.Running {
		serviceChanged = true
		if err := stopNativeService(ctx, service, dependencies.wait); err != nil {
			return fmt.Errorf("stop %s before uninstall: %w", NativeBrokerServiceName, err)
		}
	}
	legacyStopped = true
	stopLegacyErr := dependencies.stopLegacy(ctx, &legacy, logger)
	registrationsMayHaveChanged = legacy.scheduledDisabled
	if stopLegacyErr != nil {
		return fmt.Errorf("stop legacy VIIPER before uninstall: %w", stopLegacyErr)
	}
	legacyStopped = hasRunningLegacyCommand(legacy)
	registrationsMayHaveChanged = true
	if err := dependencies.removeLegacy(ctx, legacy); err != nil {
		return fmt.Errorf("remove legacy VIIPER startup during uninstall: %w", err)
	}
	if before.exists {
		serviceChanged = true
		if err := service.Delete(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
			return fmt.Errorf("delete %s during uninstall: %w", NativeBrokerServiceName, err)
		}
	}
	logger.Info("VIIPER native broker service and legacy startup ownership removed",
		"service", NativeBrokerServiceName)
	return nil
}

func installNativeBrokerTransaction(
	ctx context.Context,
	logger *slog.Logger,
	executable string,
	dependencies nativeInstallDependencies,
) (resultErr error) {
	if !filepath.IsAbs(executable) {
		return fmt.Errorf("native broker executable must be an absolute path: %s", executable)
	}
	if strings.IndexByte(executable, 0) >= 0 {
		return errors.New("native broker executable contains NUL")
	}
	releaseExecutable, err := dependencies.lockExecutable(executable)
	if err != nil {
		return fmt.Errorf("validate protected native broker executable: %w", err)
	}
	if releaseExecutable == nil {
		return errors.New("protected native broker executable lock returned no release function")
	}
	defer releaseExecutable()

	var credential nativeCredential
	credentialProvisioned := false
	credentialFinalized := false
	rollbackCredential := func() error {
		if !credentialProvisioned || credentialFinalized {
			return nil
		}
		if err := dependencies.rollbackCredential(credential); err != nil {
			return err
		}
		credentialFinalized = true
		return nil
	}
	defer func() {
		if !credentialProvisioned || credentialFinalized {
			return
		}
		if rollbackErr := rollbackCredential(); rollbackErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("roll back native broker credential: %w", rollbackErr))
		}
	}()

	manager, err := dependencies.connectSCM()
	if err != nil {
		return fmt.Errorf("connect to Windows Service Control Manager: %w", err)
	}
	defer manager.Close() //nolint:errcheck -- closing a handle cannot invalidate a committed transaction

	service, before, err := openAndSnapshotNativeService(
		ctx, manager, dependencies.wait, dependencies.lockPriorExecutable,
	)
	if err != nil {
		return err
	}
	if before.releaseExecutable != nil {
		defer before.releaseExecutable()
	}
	if before.exists && !isLocalSystemServiceAccount(before.config.ServiceStartName) {
		return fmt.Errorf(
			"refusing to replace %s because it runs as non-LocalSystem account %q and its password cannot be transactionally restored",
			NativeBrokerServiceName, before.config.ServiceStartName,
		)
	}
	defer func() {
		if service != nil {
			service.Close() //nolint:errcheck -- the SCM handle owns no transactional state
		}
	}()

	legacy, err := dependencies.snapshotLegacy(ctx)
	if err != nil {
		return fmt.Errorf("snapshot legacy VIIPER startup: %w", err)
	}
	if legacy.release != nil {
		defer legacy.release()
	}

	serviceChanged := false
	legacyStopped := false
	registrationsMayHaveChanged := false
	defer func() {
		if resultErr == nil {
			return
		}
		// The forward operation commonly fails because its deadline elapsed.
		// Rollback must have an independent budget or a stopped prior service can
		// never be restored once the installation context is canceled.
		rollbackCtx, cancelRollback := context.WithTimeout(context.Background(), nativeServiceInstallTimeout)
		defer cancelRollback()
		var rollbackErrors []error
		safeToRestartLegacy := true
		if serviceChanged {
			var rollbackErr error
			safeToRestartLegacy, rollbackErr = rollbackNativeService(
				rollbackCtx, manager, service, before, dependencies.wait, rollbackCredential,
			)
			if rollbackErr != nil {
				rollbackErrors = append(rollbackErrors, rollbackErr)
			}
			if !safeToRestartLegacy && credentialProvisioned && !credentialFinalized {
				// The replacement could still own the key path. Retain the new
				// credential rather than invalidating a service we failed to stop
				// or prove restored. This is fail-closed and is reported alongside
				// the rollback failure.
				credentialFinalized = true
				rollbackErrors = append(rollbackErrors,
					errors.New("retained native credential because service ownership could not be rolled back safely"))
			}
		} else if rollbackErr := rollbackCredential(); rollbackErr != nil {
			safeToRestartLegacy = false
			rollbackErrors = append(rollbackErrors,
				fmt.Errorf("restore native broker credential before legacy restart: %w", rollbackErr))
		}
		// Restoring task XML can itself launch the legacy process. Keep legacy
		// ownership absent until the service and credential rollback has made it
		// safe for that process to exist again.
		if registrationsMayHaveChanged && safeToRestartLegacy {
			if rollbackErr := dependencies.restoreLegacy(rollbackCtx, legacy); rollbackErr != nil {
				safeToRestartLegacy = false
				rollbackErrors = append(rollbackErrors, rollbackErr)
			}
		}
		if legacyStopped && safeToRestartLegacy {
			if rollbackErr := dependencies.restartLegacy(rollbackCtx, legacy); rollbackErr != nil {
				rollbackErrors = append(rollbackErrors, fmt.Errorf("restart prior legacy VIIPER process: %w", rollbackErr))
			}
		}
		if len(rollbackErrors) != 0 {
			resultErr = errors.Join(resultErr, errors.Join(rollbackErrors...))
		}
	}()

	if before.exists && before.status.State != svc.Stopped {
		// Control(STOP) is itself a mutation. Even if the subsequent wait or
		// status query fails, rollback must reconcile the snapshotted state.
		serviceChanged = true
		if err := stopNativeService(ctx, service, dependencies.wait); err != nil {
			return fmt.Errorf("stop previous %s service: %w", NativeBrokerServiceName, err)
		}
	}
	legacyStopped = true
	stopLegacyErr := dependencies.stopLegacy(ctx, &legacy, logger)
	registrationsMayHaveChanged = legacy.scheduledDisabled
	if stopLegacyErr != nil {
		return fmt.Errorf("stop legacy VIIPER process: %w", stopLegacyErr)
	}
	legacyStopped = hasRunningLegacyCommand(legacy)

	if err := dependencies.preflightDriver(); err != nil {
		return err
	}
	// Rotate the machine credential only after every prior owner is stopped.
	// Existing bytes are retained solely for rollback; they are never trusted as
	// the new service secret because an unprivileged user may have pre-seeded the
	// ProgramData path before its ACL was hardened.
	credential, err = dependencies.provisionCredential()
	if err != nil {
		return fmt.Errorf("provision native broker credential: %w", err)
	}
	credentialProvisioned = true
	if !filepath.IsAbs(credential.path) || strings.TrimSpace(credential.password) == "" {
		return errors.New("provisioned native broker credential must have an absolute path and nonempty value")
	}

	config, arguments, err := nativeBrokerServiceConfiguration(executable, credential.path)
	if err != nil {
		return err
	}
	if before.exists {
		// ChangeServiceConfig is followed by ChangeServiceConfig2 calls inside
		// x/sys. Mark the service dirty before the call because a later optional
		// configuration failure can occur after the base configuration changed.
		serviceChanged = true
		if err := service.UpdateConfig(config); err != nil {
			return fmt.Errorf("update %s service: %w", NativeBrokerServiceName, err)
		}
	} else {
		// x/sys CreateService applies optional fields after the SCM create call
		// and ignores a failed cleanup DeleteService. Create only the atomic base
		// record first, then mark it owned and apply all optional settings through
		// UpdateConfig so every later partial failure is covered by rollback.
		baseConfig := config
		baseConfig.Description = ""
		baseConfig.SidType = windows.SERVICE_SID_TYPE_NONE
		baseConfig.DelayedAutoStart = false
		service, err = manager.CreateService(NativeBrokerServiceName, executable, baseConfig, arguments...)
		if err != nil {
			return fmt.Errorf("create %s service: %w", NativeBrokerServiceName, err)
		}
		serviceChanged = true
		if err := protectNativeServiceObject(service); err != nil {
			return err
		}
		if err := service.UpdateConfig(config); err != nil {
			return fmt.Errorf("complete %s service configuration: %w", NativeBrokerServiceName, err)
		}
	}
	if err := configureNativeServiceRecovery(service); err != nil {
		return err
	}
	if err := verifyConfiguredNativeService(service, config); err != nil {
		return err
	}
	if err := service.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
		return fmt.Errorf("start %s service: %w", NativeBrokerServiceName, err)
	}
	if err := waitForNativeServiceState(ctx, service, svc.Running, dependencies.wait); err != nil {
		return fmt.Errorf("wait for %s service readiness: %w", NativeBrokerServiceName, err)
	}
	servicePID, err := requireNativeServiceProcess(service, 0)
	if err != nil {
		return err
	}
	if err := dependencies.verifyBroker(ctx, credential.password); err != nil {
		return fmt.Errorf("authenticate and verify %s: %w", NativeBrokerServiceName, err)
	}
	if _, err := requireNativeServiceProcess(service, servicePID); err != nil {
		return fmt.Errorf("revalidate %s after authenticated ping: %w", NativeBrokerServiceName, err)
	}

	// Legacy registrations remain intact through authenticated readiness. They
	// are removed last so a failed native migration can still restart the exact
	// legacy command without reconstructing startup ownership.
	registrationsMayHaveChanged = true
	if err := dependencies.removeLegacy(ctx, legacy); err != nil {
		return fmt.Errorf("remove legacy VIIPER startup after native verification: %w", err)
	}
	// Re-authenticate after removing the legacy owner. A task trigger or restart
	// policy can race the earlier stop; the migration is committed only while the
	// verified native service still owns the exact endpoint contract.
	if err := dependencies.verifyBroker(ctx, credential.password); err != nil {
		return fmt.Errorf("reverify %s after legacy removal: %w", NativeBrokerServiceName, err)
	}
	if _, err := requireNativeServiceProcess(service, servicePID); err != nil {
		return fmt.Errorf("revalidate %s after legacy removal: %w", NativeBrokerServiceName, err)
	}
	credentialFinalized = true
	logger.Info("VIIPER native broker service installed and authenticated",
		"service", NativeBrokerServiceName, "exe", executable, "credential", credential.path)
	return nil
}

func requireNativeServiceProcess(service nativeManagedService, expectedPID uint32) (uint32, error) {
	status, err := service.Query()
	if err != nil {
		return 0, fmt.Errorf("query %s state: %w", NativeBrokerServiceName, err)
	}
	if status.State != svc.Running {
		return 0, fmt.Errorf("%s left Running state after verification (state=%d)", NativeBrokerServiceName, status.State)
	}
	pid, err := service.ProcessID()
	if err != nil {
		return 0, fmt.Errorf("query %s process identity: %w", NativeBrokerServiceName, err)
	}
	if pid == 0 {
		return 0, fmt.Errorf("%s reports no running process", NativeBrokerServiceName)
	}
	if expectedPID != 0 && pid != expectedPID {
		return 0, fmt.Errorf("%s process changed during verification (before=%d after=%d)",
			NativeBrokerServiceName, expectedPID, pid)
	}
	return pid, nil
}

func openAndSnapshotNativeService(
	ctx context.Context,
	manager nativeSCM,
	wait func(context.Context, time.Duration) error,
	lockExecutable func(string) (func(), error),
) (nativeManagedService, nativeServiceSnapshot, error) {
	var service nativeManagedService
	for {
		var err error
		service, err = manager.OpenService(NativeBrokerServiceName)
		if err == nil {
			break
		}
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return nil, nativeServiceSnapshot{}, nil
		}
		if !errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
			return nil, nativeServiceSnapshot{}, fmt.Errorf("open %s service: %w", NativeBrokerServiceName, err)
		}
		if err := wait(ctx, nativeServiceStatePoll); err != nil {
			return nil, nativeServiceSnapshot{}, fmt.Errorf(
				"wait for prior %s deletion to finish: %w", NativeBrokerServiceName, err,
			)
		}
	}
	config, err := service.Config()
	if err != nil {
		service.Close() //nolint:errcheck
		return nil, nativeServiceSnapshot{}, fmt.Errorf("query %s configuration: %w", NativeBrokerServiceName, err)
	}
	if lockExecutable == nil {
		service.Close() //nolint:errcheck
		return nil, nativeServiceSnapshot{}, errors.New("prior service executable lock is unavailable")
	}
	priorExecutable, err := nativeServiceExecutableFromCommandLine(config.BinaryPathName)
	if err != nil {
		service.Close() //nolint:errcheck
		return nil, nativeServiceSnapshot{}, fmt.Errorf("parse prior %s executable: %w", NativeBrokerServiceName, err)
	}
	releasePriorExecutable, err := lockExecutable(priorExecutable)
	if err != nil {
		service.Close() //nolint:errcheck
		return nil, nativeServiceSnapshot{}, fmt.Errorf("lock prior %s executable: %w", NativeBrokerServiceName, err)
	}
	if releasePriorExecutable == nil {
		service.Close() //nolint:errcheck
		return nil, nativeServiceSnapshot{}, errors.New("prior service executable lock returned no release function")
	}
	fail := func(err error) (nativeManagedService, nativeServiceSnapshot, error) {
		releasePriorExecutable()
		service.Close() //nolint:errcheck
		return nil, nativeServiceSnapshot{}, err
	}
	securityDescriptor, err := service.SecurityDescriptor()
	if err != nil {
		return fail(fmt.Errorf("query %s security descriptor: %w", NativeBrokerServiceName, err))
	}
	if _, err := windows.SecurityDescriptorFromString(securityDescriptor); err != nil {
		return fail(fmt.Errorf("parse %s security descriptor: %w", NativeBrokerServiceName, err))
	}
	// Replacing a permissive DACL does not revoke dangerous service handles
	// that another process opened while the old ACL was live. Reuse only an
	// already-protected SCM object; an untrusted prior service must be repaired
	// by an explicit delete-and-recreate flow, never silently adopted as
	// LocalSystem code by this rollback-capable update transaction.
	if err := compareNativeSecurityDescriptorStrings(securityDescriptor, nativeBrokerServiceSDDL); err != nil {
		return fail(fmt.Errorf("%s has an untrusted service security descriptor: %w", NativeBrokerServiceName, err))
	}
	// ChangeServiceConfig can request a load-order tag but cannot restore an
	// exact previously assigned TagId. VIIPER does not need a load-order group,
	// so reject that unrepresentable preexisting state before any mutation.
	if config.LoadOrderGroup != "" || config.TagId != 0 {
		return fail(fmt.Errorf(
			"%s uses unrepresentable load-order state group=%q tag=%d",
			NativeBrokerServiceName, config.LoadOrderGroup, config.TagId,
		))
	}
	status, err := service.Query()
	if err != nil {
		return fail(fmt.Errorf("query %s state: %w", NativeBrokerServiceName, err))
	}
	status, err = settleNativeServiceSnapshot(ctx, service, status, wait)
	if err != nil {
		return fail(err)
	}
	actions, err := service.RecoveryActions()
	if err != nil {
		return fail(fmt.Errorf("query %s recovery actions: %w", NativeBrokerServiceName, err))
	}
	reset, err := service.ResetPeriod()
	if err != nil {
		return fail(fmt.Errorf("query %s recovery reset period: %w", NativeBrokerServiceName, err))
	}
	// Per the SERVICE_FAILURE_ACTIONS contract, an empty action array can only
	// be restored with a zero reset period. A malformed/noncanonical preexisting
	// state must be rejected before we stop or reconfigure the service because
	// exact transactional rollback would otherwise be impossible.
	if len(actions) == 0 && reset != 0 {
		return fail(fmt.Errorf(
			"%s has an unrepresentable recovery policy (no actions, reset=%d); refusing transactional replacement",
			NativeBrokerServiceName, reset,
		))
	}
	nonCrash, err := service.RecoveryActionsOnNonCrashFailures()
	if err != nil {
		return fail(fmt.Errorf("query %s recovery flag: %w", NativeBrokerServiceName, err))
	}
	return service, nativeServiceSnapshot{
		exists: true, config: config, status: status,
		securityDescriptor: securityDescriptor,
		recoveryActions:    actions, recoveryResetSeconds: reset, recoverNonCrash: nonCrash,
		releaseExecutable: releasePriorExecutable,
	}, nil
}

func nativeServiceExecutableFromCommandLine(commandLine string) (string, error) {
	if commandLine == "" || strings.IndexByte(commandLine, 0) >= 0 {
		return "", errors.New("service command line is empty or contains NUL")
	}
	arguments, err := windows.DecomposeCommandLine(commandLine)
	if err != nil {
		return "", err
	}
	if len(arguments) == 0 || !filepath.IsAbs(arguments[0]) {
		return "", errors.New("service command line does not name an absolute executable")
	}
	return filepath.Clean(arguments[0]), nil
}

func nativeBrokerServiceConfiguration(executable, keyPath string) (mgr.Config, []string, error) {
	if !filepath.IsAbs(executable) || !filepath.IsAbs(keyPath) {
		return mgr.Config{}, nil, errors.New("native broker executable and credential paths must be absolute")
	}
	logPath := filepath.Join(filepath.Dir(keyPath), nativeBrokerLogName)
	arguments := []string{
		"service", "--transport", "native-ude", "--key-file", keyPath,
		"--log.file", logPath,
	}
	binaryPath, err := windowsCommandLine(executable, arguments...)
	if err != nil {
		return mgr.Config{}, nil, err
	}
	return mgr.Config{
		ServiceType:      windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:        mgr.StartAutomatic,
		ErrorControl:     mgr.ErrorNormal,
		BinaryPathName:   binaryPath,
		ServiceStartName: nativeServiceAccount,
		DisplayName:      nativeBrokerDisplayName,
		Description:      nativeBrokerDescription,
		SidType:          windows.SERVICE_SID_TYPE_UNRESTRICTED,
		DelayedAutoStart: false,
	}, arguments, nil
}

func windowsCommandLine(executable string, arguments ...string) (string, error) {
	parts := append([]string{executable}, arguments...)
	for _, part := range parts {
		if strings.IndexByte(part, 0) >= 0 {
			return "", errors.New("Windows command-line argument contains NUL")
		}
	}
	commandLine := syscall.EscapeArg(executable)
	for _, argument := range arguments {
		commandLine += " " + syscall.EscapeArg(argument)
	}
	return commandLine, nil
}

func configureNativeServiceRecovery(service nativeManagedService) error {
	if err := service.SetRecoveryActions(nativeServiceRecoveryActions, nativeServiceRecoveryResetSecond); err != nil {
		return fmt.Errorf("configure %s bounded recovery actions: %w", NativeBrokerServiceName, err)
	}
	if err := service.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		return fmt.Errorf("configure %s non-crash recovery: %w", NativeBrokerServiceName, err)
	}
	return nil
}

func verifyConfiguredNativeService(service nativeManagedService, expected mgr.Config) error {
	current, err := service.Config()
	if err != nil {
		return fmt.Errorf("verify %s configuration: %w", NativeBrokerServiceName, err)
	}
	if !nativeServiceConfigsEqual(current, expected) {
		return fmt.Errorf("%s configuration did not match after update", NativeBrokerServiceName)
	}
	securityDescriptor, err := service.SecurityDescriptor()
	if err != nil {
		return fmt.Errorf("verify %s service security: %w", NativeBrokerServiceName, err)
	}
	if err := compareNativeSecurityDescriptorStrings(securityDescriptor, nativeBrokerServiceSDDL); err != nil {
		return fmt.Errorf("%s service security did not match after update: %w", NativeBrokerServiceName, err)
	}
	return verifyNativeServiceRecovery(service, nativeServiceSnapshot{
		recoveryActions:      nativeServiceRecoveryActions,
		recoveryResetSeconds: nativeServiceRecoveryResetSecond,
		recoverNonCrash:      true,
	})
}

func rollbackNativeService(
	ctx context.Context,
	manager nativeSCM,
	service nativeManagedService,
	before nativeServiceSnapshot,
	wait func(context.Context, time.Duration) error,
	beforeResume func() error,
) (bool, error) {
	var rollbackErrors []error
	if service != nil {
		if err := stopNativeService(ctx, service, wait); err != nil {
			return false, fmt.Errorf("stop replacement native service before rollback: %w", err)
		}
	}
	if !before.exists {
		if service != nil {
			if err := service.Delete(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
				return false, fmt.Errorf("delete replacement native service: %w", err)
			}
		}
		if beforeResume != nil {
			if err := beforeResume(); err != nil {
				return false, fmt.Errorf("restore native credential after deleting replacement service: %w", err)
			}
		}
		return true, nil
	}
	if service == nil {
		var err error
		service, err = manager.OpenService(NativeBrokerServiceName)
		if err != nil {
			return false, errors.Join(append(rollbackErrors, fmt.Errorf("reopen prior native service: %w", err))...)
		}
		defer service.Close() //nolint:errcheck
	}
	if err := service.UpdateConfig(before.config); err != nil {
		return false, fmt.Errorf("restore prior native service configuration: %w", err)
	} else if current, err := service.Config(); err != nil {
		return false, fmt.Errorf("verify prior native service configuration: %w", err)
	} else if !nativeServiceConfigsEqual(current, before.config) {
		return false, errors.New("prior native service configuration did not verify after rollback")
	}
	if err := service.SetRecoveryActionsExact(before.recoveryActions, before.recoveryResetSeconds); err != nil {
		rollbackErrors = append(rollbackErrors, fmt.Errorf("restore native service recovery actions: %w", err))
	}
	if err := service.SetRecoveryActionsOnNonCrashFailures(before.recoverNonCrash); err != nil {
		rollbackErrors = append(rollbackErrors, fmt.Errorf("restore native service recovery flag: %w", err))
	}
	if err := verifyNativeServiceRecovery(service, before); err != nil {
		rollbackErrors = append(rollbackErrors, err)
	}
	if before.securityDescriptor == "" {
		rollbackErrors = append(rollbackErrors, errors.New("prior native service security descriptor is unavailable"))
	} else if err := service.SetSecurityDescriptor(before.securityDescriptor); err != nil {
		rollbackErrors = append(rollbackErrors, fmt.Errorf("restore prior native service security descriptor: %w", err))
	} else if current, err := service.SecurityDescriptor(); err != nil {
		rollbackErrors = append(rollbackErrors, fmt.Errorf("verify prior native service security descriptor: %w", err))
	} else if err := compareNativeSecurityDescriptorStrings(current, before.securityDescriptor); err != nil {
		rollbackErrors = append(rollbackErrors,
			fmt.Errorf("prior native service security descriptor did not verify after rollback: %w", err))
	}
	if beforeResume != nil {
		if err := beforeResume(); err != nil {
			rollbackErrors = append(rollbackErrors,
				fmt.Errorf("restore native credential before prior service restart: %w", err))
		}
	}
	// Never start a service after an incomplete configuration/recovery restore:
	// BinaryPathName may still name the rejected replacement.
	if len(rollbackErrors) == 0 && serviceWasOperational(before.status.State) {
		if err := service.Start(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("restart prior native service: %w", err))
		} else if err := waitForNativeServiceState(ctx, service, svc.Running, wait); err != nil {
			rollbackErrors = append(rollbackErrors, fmt.Errorf("wait for prior native service: %w", err))
		}
	}
	return len(rollbackErrors) == 0, errors.Join(rollbackErrors...)
}

func nativeServiceConfigsEqual(first, second mgr.Config) bool {
	return first.ServiceType == second.ServiceType &&
		first.StartType == second.StartType &&
		first.ErrorControl == second.ErrorControl &&
		first.BinaryPathName == second.BinaryPathName &&
		first.LoadOrderGroup == second.LoadOrderGroup &&
		first.TagId == second.TagId &&
		slices.Equal(first.Dependencies, second.Dependencies) &&
		isEquivalentServiceAccount(first.ServiceStartName, second.ServiceStartName) &&
		first.DisplayName == second.DisplayName &&
		first.Description == second.Description &&
		first.SidType == second.SidType &&
		first.DelayedAutoStart == second.DelayedAutoStart
}

func verifyNativeServiceRecovery(service nativeManagedService, before nativeServiceSnapshot) error {
	actions, err := service.RecoveryActions()
	if err != nil {
		return fmt.Errorf("verify native service recovery actions: %w", err)
	}
	reset, err := service.ResetPeriod()
	if err != nil {
		return fmt.Errorf("verify native service recovery reset period: %w", err)
	}
	nonCrash, err := service.RecoveryActionsOnNonCrashFailures()
	if err != nil {
		return fmt.Errorf("verify native service recovery flag: %w", err)
	}
	if !slices.Equal(actions, before.recoveryActions) || reset != before.recoveryResetSeconds ||
		nonCrash != before.recoverNonCrash {
		return errors.New("prior native service recovery policy did not verify after rollback")
	}
	return nil
}

func stopNativeService(
	ctx context.Context,
	service nativeManagedService,
	wait func(context.Context, time.Duration) error,
) error {
	status, err := service.Query()
	if err != nil {
		return err
	}
	if status.State == svc.Stopped {
		return nil
	}
	if status.State != svc.StopPending {
		if _, err := service.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			return err
		}
	}
	return waitForNativeServiceState(ctx, service, svc.Stopped, wait)
}

func waitForNativeServiceState(
	ctx context.Context,
	service nativeManagedService,
	want svc.State,
	wait func(context.Context, time.Duration) error,
) error {
	for {
		status, err := service.Query()
		if err != nil {
			return err
		}
		if status.State == want {
			return nil
		}
		if want == svc.Running && status.State == svc.Stopped && status.Win32ExitCode != 0 {
			return fmt.Errorf("service stopped during startup (win32=%d service=%d)",
				status.Win32ExitCode, status.ServiceSpecificExitCode)
		}
		if err := wait(ctx, nativeServiceStatePoll); err != nil {
			return err
		}
	}
}

func settleNativeServiceSnapshot(
	ctx context.Context,
	service nativeManagedService,
	status svc.Status,
	wait func(context.Context, time.Duration) error,
) (svc.Status, error) {
	for {
		switch status.State {
		case svc.Stopped, svc.Running:
			return status, nil
		case svc.StartPending, svc.StopPending, svc.ContinuePending:
			if err := wait(ctx, nativeServiceStatePoll); err != nil {
				return svc.Status{}, fmt.Errorf("wait for %s stable state: %w", NativeBrokerServiceName, err)
			}
			var err error
			status, err = service.Query()
			if err != nil {
				return svc.Status{}, fmt.Errorf("query %s stable state: %w", NativeBrokerServiceName, err)
			}
		default:
			return svc.Status{}, fmt.Errorf(
				"%s is in unsupported state %d; stop or resume it before transactional replacement",
				NativeBrokerServiceName, status.State,
			)
		}
	}
}

func verifyNativeBroker(ctx context.Context, password string) error {
	if strings.TrimSpace(password) == "" {
		return errors.New("native broker credential is empty")
	}
	client := viiperclient.NewWithConfig(api.DefaultListenAddress, &viiperclient.Config{
		DialTimeout: time.Second, ReadTimeout: 2 * time.Second,
		WriteTimeout: 2 * time.Second, Password: password,
	})
	var lastErr error
	for {
		response, err := client.PingCtx(ctx)
		if err == nil {
			err = validateNativeBrokerPing(response)
		}
		if err == nil {
			return nil
		}
		lastErr = err
		if err := waitContext(ctx, 100*time.Millisecond); err != nil {
			return fmt.Errorf("broker did not satisfy the native contract: %w (last ping: %v)", err, lastErr)
		}
	}
}

func validateNativeBrokerPing(response *viipertypes.PingResponse) error {
	if response == nil {
		return errors.New("empty ping response")
	}
	if response.Server != "VIIPER" || !strings.EqualFold(response.Transport, "native-ude") {
		return fmt.Errorf("unexpected broker identity server=%q transport=%q", response.Server, response.Transport)
	}
	if response.Ready == nil || !*response.Ready {
		return errors.New("native broker reports not ready")
	}
	if response.NativeUDE == nil {
		return errors.New("native broker omitted its negotiated driver contract")
	}
	native := response.NativeUDE
	requiredCapabilities := uint32(
		udecx.CapabilityIsochronous |
			udecx.CapabilityDeviceLifecycle |
			udecx.CapabilityInputReports,
	)
	if native.ABIMajor != udecx.ABIMajor || native.ABIMinor != udecx.ABIMinor {
		return fmt.Errorf("native broker ABI=%d.%d expected=%d.%d",
			native.ABIMajor, native.ABIMinor, udecx.ABIMajor, udecx.ABIMinor)
	}
	if native.Capabilities != requiredCapabilities {
		return fmt.Errorf("native broker capabilities=%#x expected exact=%#x", native.Capabilities, requiredCapabilities)
	}
	// ExpectedDriverPackageVersion is currently broker compile-time metadata,
	// not an attestation read from the installed driver. ABI and negotiated
	// capabilities are authoritative here; do not misrepresent that echoed
	// constant as installed-package verification.
	return nil
}

func waitContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func acquireNativeInstallMutex(timeout time.Duration) (func(), error) {
	// Win32 mutexes are owned by OS threads. Keep this goroutine pinned through
	// its deferred release so the Go scheduler cannot move ReleaseMutex to a
	// non-owner thread and leave service installation permanently serialized.
	runtime.LockOSThread()
	name, err := windows.UTF16PtrFromString(nativeInstallMutexName)
	if err != nil {
		runtime.UnlockOSThread()
		return nil, err
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;SY)(A;;GA;;;BA)")
	if err != nil {
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("create native install mutex security descriptor: %w", err)
	}
	attributes := windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
	}
	handle, err := windows.CreateMutex(&attributes, false, name)
	if err != nil {
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("create native install mutex: %w", err)
	}
	status, err := windows.WaitForSingleObject(handle, uint32(timeout/time.Millisecond))
	if err != nil {
		windows.CloseHandle(handle) //nolint:errcheck
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("wait for native install mutex: %w", err)
	}
	if status != windows.WAIT_OBJECT_0 && status != windows.WAIT_ABANDONED {
		windows.CloseHandle(handle) //nolint:errcheck
		runtime.UnlockOSThread()
		return nil, errors.New("another VIIPER native install, update, or uninstall is still running")
	}
	return func() {
		windows.ReleaseMutex(handle) //nolint:errcheck
		windows.CloseHandle(handle)  //nolint:errcheck
		runtime.UnlockOSThread()
	}, nil
}

type nativeFileAttributeTagInfo struct {
	FileAttributes uint32
	ReparseTag     uint32
}

func lockNativeServiceExecutable(executable string) (func(), error) {
	return lockNativeServiceExecutableReadOnly(executable)
}

// lockNativePriorServiceExecutable proves that a preexisting service already
// points at an installer-owned image without changing any filesystem metadata.
// The snapshot operation runs before transactional rollback is armed, so it
// must be strictly read-only. Older or user-writable layouts fail closed and
// can be repaired explicitly rather than being silently adopted as LocalSystem
// code.
func lockNativePriorServiceExecutable(executable string) (func(), error) {
	return lockNativeServiceExecutableReadOnly(executable)
}

func lockNativeServiceExecutableReadOnly(executable string) (func(), error) {
	programFiles, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return nil, fmt.Errorf("resolve Program Files: %w", err)
	}
	_, err = nativeServiceExecutableParent(programFiles, executable)
	if err != nil {
		return nil, err
	}
	programFiles = filepath.Clean(programFiles)
	executable = filepath.Clean(executable)
	relative, _ := filepath.Rel(programFiles, executable)
	parts := strings.Split(relative, string(filepath.Separator))

	var handles []windows.Handle
	closeHandles := func() {
		for index := len(handles) - 1; index >= 0; index-- {
			windows.CloseHandle(handles[index]) //nolint:errcheck
		}
		handles = nil
	}
	fail := func(err error) (func(), error) {
		closeHandles()
		return nil, err
	}

	// Reject every reparse point between the known folder and the executable.
	// Keep non-delete-shared handles to every component through authenticated
	// service startup and require that the package installer already established
	// the exact protected owner/DACL contract. Rewriting an ACL here would not
	// revoke dangerous handles opened under a former permissive DACL, so trust
	// must be proven without mutating the image or its parents.
	rootHandle, err := openNativePathWithoutReparse(programFiles, windows.FILE_READ_ATTRIBUTES, true)
	if err != nil {
		return nil, fmt.Errorf("open Program Files without reparse traversal: %w", err)
	}
	handles = append(handles, rootHandle)
	current := programFiles
	for index, part := range parts {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		isDirectory := index < len(parts)-1
		access := uint32(windows.FILE_READ_ATTRIBUTES | windows.READ_CONTROL)
		if isDirectory {
		} else {
			access |= windows.GENERIC_READ
		}
		handle, openErr := openNativePathWithoutReparse(current, access, isDirectory)
		if openErr != nil {
			return fail(fmt.Errorf("open protected broker path %s: %w", current, openErr))
		}
		handles = append(handles, handle)
		if isDirectory {
			if err := validateNativeSecurityDescriptor(handle, nativeBrokerDirectorySDDL); err != nil {
				return fail(fmt.Errorf("validate protected broker directory %s: %w", current, err))
			}
		}
	}
	executableHandle := handles[len(handles)-1]
	if err := requireSingleNativeFileLink(executableHandle); err != nil {
		return fail(fmt.Errorf("reject hard-linked broker executable: %w", err))
	}
	if err := validateNativeSecurityDescriptor(executableHandle, nativeBrokerExecutableSDDL); err != nil {
		return fail(fmt.Errorf("validate protected broker executable: %w", err))
	}
	header := make([]byte, 2)
	var read uint32
	if err := windows.ReadFile(executableHandle, header, &read, nil); err != nil {
		return fail(fmt.Errorf("read broker executable header: %w", err))
	}
	if read != uint32(len(header)) || header[0] != 'M' || header[1] != 'Z' {
		return fail(errors.New("native broker executable is not a Windows PE image"))
	}
	return closeHandles, nil
}

func nativeServiceExecutableParent(programFiles, executable string) (string, error) {
	programFiles = filepath.Clean(programFiles)
	executable = filepath.Clean(executable)
	relative, err := filepath.Rel(programFiles, executable)
	if err != nil || relative == "." || filepath.IsAbs(relative) || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("native broker must be installed below Program Files, got %s", executable)
	}
	parent := filepath.Dir(executable)
	parts := strings.Split(relative, string(filepath.Separator))
	allowed := len(parts) == 2 && strings.EqualFold(parts[0], "VIIPER") &&
		strings.EqualFold(parts[1], "viiper.exe")
	allowed = allowed || len(parts) == 3 && strings.EqualFold(parts[0], "DS4Windows") &&
		strings.EqualFold(parts[1], "VIIPER") && strings.EqualFold(parts[2], "viiper.exe")
	if !allowed {
		return "", fmt.Errorf("native broker must use a managed Program Files VIIPER path, got %s", executable)
	}
	return parent, nil
}

func openNativePathWithoutReparse(path string, access uint32, directory bool) (windows.Handle, error) {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	flags := uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if directory {
		flags |= windows.FILE_FLAG_BACKUP_SEMANTICS
	}
	shareMode := uint32(windows.FILE_SHARE_READ)
	if directory {
		// Directory contents may still be read/written by trusted installers, but
		// omitting DELETE keeps every validated ancestor from being renamed or
		// removed until the native service transaction commits.
		shareMode |= windows.FILE_SHARE_WRITE
	}
	handle, err := windows.CreateFile(
		pointer,
		access,
		shareMode,
		nil,
		windows.OPEN_EXISTING,
		flags,
		0,
	)
	if err != nil {
		return 0, err
	}
	info := nativeFileAttributeTagInfo{}
	if err := windows.GetFileInformationByHandleEx(
		handle,
		windows.FileAttributeTagInfo,
		(*byte)(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		windows.CloseHandle(handle) //nolint:errcheck
		return 0, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		windows.CloseHandle(handle) //nolint:errcheck
		return 0, errors.New("path is a reparse point")
	}
	if directory != (info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) {
		windows.CloseHandle(handle) //nolint:errcheck
		return 0, errors.New("path type does not match the expected broker object")
	}
	return handle, nil
}

func applyNativeACLToHandle(handle windows.Handle, sddl string) error {
	return setNativeObjectSecurityDescriptor(handle, windows.SE_FILE_OBJECT, sddl)
}

func nativeObjectSecurityDescriptor(handle windows.Handle, objectType windows.SE_OBJECT_TYPE) (string, error) {
	descriptor, err := windows.GetSecurityInfo(
		handle,
		objectType,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return "", err
	}
	if descriptor == nil || !descriptor.IsValid() {
		return "", errors.New("object returned an invalid security descriptor")
	}
	sddl := descriptor.String()
	if sddl == "" {
		return "", errors.New("object security descriptor could not be serialized")
	}
	return sddl, nil
}

func setNativeObjectSecurityDescriptor(
	handle windows.Handle,
	objectType windows.SE_OBJECT_TYPE,
	sddl string,
) error {
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	control, _, err := descriptor.Control()
	if err != nil {
		return err
	}
	securityInformation := windows.SECURITY_INFORMATION(
		windows.OWNER_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION,
	)
	if control&windows.SE_DACL_PROTECTED != 0 {
		securityInformation |= windows.PROTECTED_DACL_SECURITY_INFORMATION
	} else {
		securityInformation |= windows.UNPROTECTED_DACL_SECURITY_INFORMATION
	}
	return windows.SetSecurityInfo(
		handle,
		objectType,
		securityInformation,
		owner,
		nil,
		dacl,
		nil,
	)
}

func protectNativeServiceObject(service nativeManagedService) error {
	if err := service.SetSecurityDescriptor(nativeBrokerServiceSDDL); err != nil {
		return fmt.Errorf("apply protected %s service DACL: %w", NativeBrokerServiceName, err)
	}
	actual, err := service.SecurityDescriptor()
	if err != nil {
		return fmt.Errorf("verify protected %s service DACL: %w", NativeBrokerServiceName, err)
	}
	return compareNativeSecurityDescriptorStrings(actual, nativeBrokerServiceSDDL)
}

func compareNativeSecurityDescriptorStrings(actual, expected string) error {
	actualDescriptor, err := windows.SecurityDescriptorFromString(actual)
	if err != nil {
		return fmt.Errorf("parse actual security descriptor: %w", err)
	}
	expectedDescriptor, err := windows.SecurityDescriptorFromString(expected)
	if err != nil {
		return fmt.Errorf("parse expected security descriptor: %w", err)
	}
	return nativeSecurityDescriptorsEqual(actualDescriptor, expectedDescriptor)
}

func requireSingleNativeFileLink(handle windows.Handle) error {
	info := windows.ByHandleFileInformation{}
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return fmt.Errorf("query file link identity: %w", err)
	}
	return validateNativeFileLinkCount(info.NumberOfLinks)
}

func validateNativeFileLinkCount(numberOfLinks uint32) error {
	if numberOfLinks != 1 {
		return fmt.Errorf("expected exactly one file link, found %d", numberOfLinks)
	}
	return nil
}

func serviceWasOperational(state svc.State) bool {
	return state == svc.Running
}

func provisionNativeServiceCredential(userSID string) (nativeCredential, error) {
	path, err := nativeServiceKeyFilePath()
	if err != nil {
		return nativeCredential{}, err
	}
	if _, err := validateNativeInstallingUserSID(userSID); err != nil {
		return nativeCredential{}, err
	}
	directory := filepath.Dir(path)
	directoryHandle, err := secureNativeCredentialDirectory(directory, userSID)
	if err != nil {
		return nativeCredential{}, err
	}
	defer windows.CloseHandle(directoryHandle) //nolint:errcheck

	prior, existed, err := readNativeCredential(path, userSID)
	if err != nil {
		return nativeCredential{}, fmt.Errorf("read credential: %w", err)
	}
	password, err := rotatedNativeServiceKey(prior, auth.GenerateKey)
	if err != nil {
		return nativeCredential{}, fmt.Errorf("generate credential: %w", err)
	}
	if err := writeNativeCredentialAtomically(path, []byte(password), userSID); err != nil {
		return nativeCredential{}, err
	}
	return nativeCredential{
		path: path, password: password, userSID: userSID,
		created: !existed, replaced: existed, priorBytes: append([]byte(nil), prior...),
	}, nil
}

func resolveNativeInstallingUserSID(explicit string) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		return validateNativeInstallingUserSID(explicit)
	}
	currentUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("query installer token user SID: %w", err)
	}
	currentSID, err := validateNativeInstallingUserSID(currentUser.User.Sid.String())
	if err != nil && !currentUser.User.Sid.IsWellKnown(windows.WinLocalSystemSid) {
		return "", err
	}

	// Elevation can change the process identity: deferred MSI work commonly runs
	// as LocalSystem, and over-the-shoulder UAC runs as a different administrator.
	// Prefer the shell owner when it is visible in this session. A session-0
	// LocalSystem installer has no shell window, so use the active-console token.
	interactiveSID := ""
	interactiveErr := error(nil)
	if token, tokenErr := nativeInteractiveUserToken("", windows.TOKEN_QUERY); tokenErr == nil {
		defer token.Close() //nolint:errcheck
		user, userErr := token.GetTokenUser()
		if userErr != nil {
			return "", fmt.Errorf("query interactive installer user SID: %w", userErr)
		}
		interactiveSID, interactiveErr = validateNativeInstallingUserSID(user.User.Sid.String())
	} else {
		interactiveErr = tokenErr
	}
	selected, err := selectNativeInstallingUserSID(
		currentSID,
		currentUser.User.Sid.IsWellKnown(windows.WinLocalSystemSid),
		interactiveSID,
		interactiveErr,
	)
	if err != nil {
		return "", err
	}
	return validateNativeInstallingUserSID(selected)
}

func selectNativeInstallingUserSID(
	currentSID string,
	currentIsLocalSystem bool,
	interactiveSID string,
	interactiveErr error,
) (string, error) {
	if interactiveErr == nil && strings.TrimSpace(interactiveSID) != "" {
		return interactiveSID, nil
	}
	if currentIsLocalSystem {
		return "", errors.Join(
			interactiveErr,
			errors.New("cannot identify the interactive installing user; pass --target-user-sid from the bootstrapper"),
		)
	}
	if strings.TrimSpace(currentSID) == "" {
		return "", errors.Join(interactiveErr, errors.New("installer token has no user SID"))
	}
	return currentSID, nil
}

func validateNativeInstallingUserSID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, `\/`) {
		return "", errors.New("installing user SID is missing or invalid")
	}
	sid, err := windows.StringToSid(value)
	if err != nil {
		return "", fmt.Errorf("parse installing user SID: %w", err)
	}
	if sid.IsWellKnown(windows.WinLocalSystemSid) || sid.IsWellKnown(windows.WinLocalServiceSid) ||
		sid.IsWellKnown(windows.WinNetworkServiceSid) {
		return "", errors.New("installing user SID names a service identity")
	}
	_, _, accountType, err := sid.LookupAccount("")
	if err != nil {
		return "", fmt.Errorf("resolve installing user SID: %w", err)
	}
	if accountType != windows.SidTypeUser {
		return "", fmt.Errorf("installing user SID is not a user account (type=%d)", accountType)
	}
	return sid.String(), nil
}

func nativeInteractiveUserToken(expectedSID string, access uint32) (windows.Token, error) {
	var shellErr error
	if shellWindow := windows.GetShellWindow(); shellWindow != 0 {
		var shellPID uint32
		if _, err := windows.GetWindowThreadProcessId(shellWindow, &shellPID); err != nil {
			shellErr = fmt.Errorf("query interactive shell process: %w", err)
		} else if shellPID == 0 {
			shellErr = errors.New("interactive shell reported no process identifier")
		} else if shellProcess, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, shellPID); err != nil {
			shellErr = fmt.Errorf("open interactive shell process: %w", err)
		} else {
			var shellToken windows.Token
			err = windows.OpenProcessToken(shellProcess, access, &shellToken)
			windows.CloseHandle(shellProcess) //nolint:errcheck
			if err != nil {
				shellErr = fmt.Errorf("open interactive shell token: %w", err)
			} else if err := validateNativeInteractiveToken(shellToken, expectedSID); err != nil {
				shellToken.Close() //nolint:errcheck
				shellErr = err
			} else {
				return shellToken, nil
			}
		}
	}

	session := windows.WTSGetActiveConsoleSessionId()
	if session == ^uint32(0) {
		return 0, errors.Join(shellErr, errors.New("Windows reports no active console session"))
	}
	var token windows.Token
	if err := windows.WTSQueryUserToken(session, &token); err != nil {
		return 0, errors.Join(shellErr, fmt.Errorf("query active-console user token: %w", err))
	}
	if err := validateNativeInteractiveToken(token, expectedSID); err != nil {
		token.Close() //nolint:errcheck
		return 0, errors.Join(shellErr, err)
	}
	return token, nil
}

func validateNativeInteractiveToken(token windows.Token, expectedSID string) error {
	user, err := token.GetTokenUser()
	if err != nil {
		return fmt.Errorf("query interactive user token: %w", err)
	}
	actual, err := validateNativeInstallingUserSID(user.User.Sid.String())
	if err != nil {
		return err
	}
	if expectedSID != "" && !strings.EqualFold(actual, expectedSID) {
		return fmt.Errorf("interactive user SID %s does not match installer target %s", actual, expectedSID)
	}
	return nil
}

func expandNativeUserEnvironment(expectedSID, value string) (string, error) {
	if strings.IndexByte(value, 0) >= 0 {
		return "", errors.New("target-user environment string contains NUL")
	}
	token, err := nativeInteractiveUserToken(expectedSID, windows.TOKEN_QUERY)
	if err != nil {
		return "", err
	}
	defer token.Close() //nolint:errcheck
	source, err := windows.UTF16PtrFromString(value)
	if err != nil {
		return "", err
	}
	// Windows paths are bounded to 32,767 UTF-16 code units. The API does not
	// expose a size-probe contract, so allocate that maximum once and fail closed
	// if userenv.dll rejects it.
	destination := make([]uint16, 32768)
	result, _, callErr := expandEnvironmentStringsForUserW.Call(
		uintptr(token),
		uintptr(unsafe.Pointer(source)),
		uintptr(unsafe.Pointer(&destination[0])),
		uintptr(len(destination)),
	)
	if result == 0 {
		if callErr == nil || errors.Is(callErr, windows.ERROR_SUCCESS) {
			callErr = errors.New("ExpandEnvironmentStringsForUserW returned false")
		}
		return "", fmt.Errorf("expand environment for target interactive user: %w", callErr)
	}
	return windows.UTF16ToString(destination), nil
}

func rotatedNativeServiceKey(prior []byte, generate func() (string, error)) (string, error) {
	priorKey := strings.TrimSpace(string(prior))
	for attempt := 0; attempt < 4; attempt++ {
		password, err := generate()
		if err != nil {
			return "", err
		}
		password = strings.TrimSpace(password)
		if password != "" && password != priorKey {
			return password, nil
		}
	}
	return "", errors.New("credential generator did not produce a fresh nonempty key")
}

func secureNativeCredentialDirectory(directory, userSID string) (windows.Handle, error) {
	programData, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, windows.KF_FLAG_DEFAULT)
	if err != nil {
		return 0, fmt.Errorf("resolve ProgramData known folder: %w", err)
	}
	programData = filepath.Clean(programData)
	if !strings.EqualFold(filepath.Clean(directory), filepath.Join(programData, "VIIPER")) {
		return 0, fmt.Errorf("credential directory escaped ProgramData: %s", directory)
	}
	programDataHandle, err := openNativePathWithoutReparse(programData, windows.FILE_READ_ATTRIBUTES, true)
	if err != nil {
		return 0, fmt.Errorf("open ProgramData without reparse traversal: %w", err)
	}
	defer windows.CloseHandle(programDataHandle) //nolint:errcheck
	sddl := nativeCredentialDirectorySDDL(userSID)
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return 0, fmt.Errorf("build credential directory security descriptor: %w", err)
	}
	pointer, err := windows.UTF16PtrFromString(directory)
	if err != nil {
		return 0, err
	}
	attributes := windows.SecurityAttributes{
		Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor,
	}
	created := false
	if err := windows.CreateDirectory(pointer, &attributes); err == nil {
		created = true
	} else if !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return 0, fmt.Errorf("atomically create protected credential directory: %w", err)
	}
	// Never take ownership of or re-ACL an existing ProgramData directory. A
	// standard user can pre-create it and keep an already-authorized directory
	// handle even after a later DACL change. Only an atomically created directory
	// or an existing directory that already has our exact protected owner/DACL is
	// eligible to contain the service credential.
	directoryHandle, err := openNativePathWithoutReparse(
		directory,
		windows.READ_CONTROL,
		true,
	)
	if err != nil {
		return 0, fmt.Errorf("open credential directory without reparse traversal: %w", err)
	}
	if err := validateNativeSecurityDescriptor(directoryHandle, sddl); err != nil {
		windows.CloseHandle(directoryHandle) //nolint:errcheck
		origin := "existing"
		if created {
			origin = "newly created"
		}
		return 0, fmt.Errorf("reject %s credential directory security: %w", origin, err)
	}
	return directoryHandle, nil
}

func validateNativeSecurityDescriptor(handle windows.Handle, expectedSDDL string) error {
	actual, err := windows.GetSecurityInfo(
		handle,
		windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		return fmt.Errorf("query security descriptor: %w", err)
	}
	expected, err := windows.SecurityDescriptorFromString(expectedSDDL)
	if err != nil {
		return err
	}
	return nativeSecurityDescriptorsEqual(actual, expected)
}

func nativeSecurityDescriptorsEqual(actual, expected *windows.SECURITY_DESCRIPTOR) error {
	actualOwner, _, err := actual.Owner()
	if err != nil {
		return err
	}
	expectedOwner, _, err := expected.Owner()
	if err != nil {
		return err
	}
	if actualOwner == nil || expectedOwner == nil || !actualOwner.Equals(expectedOwner) {
		return errors.New("security descriptor owner is not the trusted installer owner")
	}
	actualDACL, actualDefaulted, err := actual.DACL()
	if err != nil {
		return err
	}
	expectedDACL, expectedDefaulted, err := expected.DACL()
	if err != nil {
		return err
	}
	actualSDDL := actual.String()
	expectedCanonicalSDDL := expected.String()
	if actualDACL == nil || expectedDACL == nil || actualDefaulted != expectedDefaulted ||
		actualSDDL == "" || expectedCanonicalSDDL == "" || actualSDDL != expectedCanonicalSDDL {
		return errors.New("security descriptor DACL is not the canonical protected DACL")
	}
	actualControl, _, err := actual.Control()
	if err != nil {
		return err
	}
	expectedControl, _, err := expected.Control()
	if err != nil {
		return err
	}
	if actualControl&windows.SE_DACL_PROTECTED != expectedControl&windows.SE_DACL_PROTECTED ||
		actualControl&windows.SE_DACL_PRESENT != expectedControl&windows.SE_DACL_PRESENT {
		return errors.New("security descriptor protection flags do not match")
	}
	return nil
}

func readNativeCredential(path, userSID string) ([]byte, bool, error) {
	handle, err := openNativePathWithoutReparse(
		path,
		windows.GENERIC_READ|windows.READ_CONTROL|windows.WRITE_DAC|windows.WRITE_OWNER,
		false,
	)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil, false, nil
		}
		return nil, false, err
	}
	// A standard user can pre-create ProgramData\VIIPER before its DACL is
	// hardened. Reject a planted hard link before taking ownership or changing
	// its security descriptor, because those operations affect the underlying
	// file and every link to it.
	if err := requireSingleNativeFileLink(handle); err != nil {
		windows.CloseHandle(handle) //nolint:errcheck
		return nil, false, fmt.Errorf("reject hard-linked credential: %w", err)
	}
	if err := applyNativeACLToHandle(handle, nativeCredentialFileSDDL(userSID)); err != nil {
		windows.CloseHandle(handle) //nolint:errcheck
		return nil, false, err
	}
	file := os.NewFile(uintptr(handle), path)
	if file == nil {
		windows.CloseHandle(handle) //nolint:errcheck
		return nil, false, errors.New("wrap credential file handle")
	}
	defer file.Close() //nolint:errcheck
	contents, err := io.ReadAll(io.LimitReader(file, 64*1024+1))
	if err != nil {
		return nil, false, err
	}
	if len(contents) > 64*1024 {
		return nil, false, fmt.Errorf("credential is unexpectedly large: more than %d bytes", 64*1024)
	}
	return contents, true, nil
}

func writeNativeCredentialAtomically(path string, contents []byte, userSID string) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".viiper-key-*.tmp")
	if err != nil {
		return fmt.Errorf("create credential staging file: %w", err)
	}
	temporaryPath := temporary.Name()
	cleanupTemporary := true
	defer func() {
		temporary.Close() //nolint:errcheck
		if cleanupTemporary {
			os.Remove(temporaryPath) //nolint:errcheck
		}
	}()
	if err := applyNativeACLToHandle(windows.Handle(temporary.Fd()), nativeCredentialFileSDDL(userSID)); err != nil {
		return fmt.Errorf("protect credential staging file: %w", err)
	}
	if _, err := temporary.Write(contents); err != nil {
		return fmt.Errorf("write credential staging file: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("flush credential staging file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close credential staging file: %w", err)
	}
	if err := replaceFileAtomically(temporaryPath, path); err != nil {
		return fmt.Errorf("publish credential atomically: %w", err)
	}
	cleanupTemporary = false
	return nil
}

func replaceFileAtomically(source, destination string) error {
	sourcePointer, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	destinationPointer, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(sourcePointer, destinationPointer,
		windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

func rollbackNativeServiceCredential(credential nativeCredential) error {
	if credential.created {
		if err := os.Remove(credential.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if credential.replaced {
		return writeNativeCredentialAtomically(credential.path, credential.priorBytes, credential.userSID)
	}
	return nil
}

func nativeCredentialDirectorySDDL(userSID string) string {
	return "O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;GRGX;;;" + userSID + ")"
}

func nativeCredentialFileSDDL(userSID string) string {
	return "O:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;GR;;;" + userSID + ")"
}

func snapshotNativeLegacyStartup(ctx context.Context, userSID string) (nativeLegacyState, error) {
	if _, err := validateNativeInstallingUserSID(userSID); err != nil {
		return nativeLegacyState{}, err
	}
	state := nativeLegacyState{userSID: userSID}
	succeeded := false
	defer func() {
		if !succeeded && state.release != nil {
			state.release()
		}
	}()
	scheduledCommand, scheduledXML, scheduledActive, scheduledEnabled, found, err := currentScheduledTaskCommand(ctx)
	if err != nil {
		return state, err
	}
	if found {
		if err := validateNativeScheduledTaskState(scheduledActive, scheduledEnabled); err != nil {
			return state, err
		}
		if scheduledEnabled || scheduledActive {
			verify, release, lockErr := lockNativeLegacyTaskExecutable(scheduledCommand.executable)
			if lockErr != nil {
				return state, fmt.Errorf("lock RunVIIPER action through migration: %w", lockErr)
			}
			state.verifyTaskAction = verify
			appendNativeLegacyRelease(&state, release)
		}
		currentXML := scheduledXML
		state.scheduledAction = &scheduledCommand
		state.scheduledXML = &scheduledXML
		state.scheduledCurrentXML = &currentXML
		state.scheduledActive = scheduledActive
		state.scheduledEnabled = scheduledEnabled
	}
	hive, err := registry.OpenKey(registry.USERS, userSID, registry.READ)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return state, fmt.Errorf("target user hive HKU\\%s is not loaded; resume migration after that user signs in", userSID)
		}
		return state, fmt.Errorf("open target user hive HKU\\%s: %w", userSID, err)
	}
	state.userHive = hive
	appendNativeLegacyRelease(&state, func() { hive.Close() }) //nolint:errcheck
	runKey, err := registry.OpenKey(hive, runKeyPath, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil && !errors.Is(err, registry.ErrNotExist) {
		return state, fmt.Errorf("open target user Run key: %w", err)
	}
	if err == nil {
		state.runKey = runKey
		state.runKeyExisted = true
		appendNativeLegacyRelease(&state, func() { runKey.Close() }) //nolint:errcheck
	}
	runRegistration, found, err := currentNativeRunRegistration(state)
	if err != nil {
		return state, err
	}
	if found {
		state.runValue = &runRegistration
		if strings.TrimSpace(runRegistration.value) != "" {
			expand := func(value string) (string, error) { return value, nil }
			if runRegistration.valueType == registry.EXPAND_SZ {
				expand = func(value string) (string, error) {
					return expandNativeUserEnvironment(userSID, value)
				}
			}
			command, err := parseWindowsCommand(runRegistration.value, expand)
			if err != nil {
				return state, fmt.Errorf("parse VIIPER Run command: %w", err)
			}
			command.source = legacyCommandRun
			state.commands = append(state.commands, command)
		}
	}
	succeeded = true
	return state, nil
}

func validateNativeScheduledTaskState(active, enabled bool) error {
	if active && !enabled {
		return errors.New("RunVIIPER is active while disabled and cannot be restored transactionally")
	}
	return nil
}

func appendNativeLegacyRelease(state *nativeLegacyState, release func()) {
	if release == nil {
		return
	}
	prior := state.release
	state.release = func() {
		release()
		if prior != nil {
			prior()
		}
	}
}

func currentNativeRunRegistration(state nativeLegacyState) (nativeRunRegistration, bool, error) {
	if state.userHive == 0 {
		return nativeRunRegistration{}, false, errors.New("target user hive is not retained by the transaction")
	}
	key := state.runKey
	closeKey := false
	if !state.runKeyExisted {
		var err error
		key, err = registry.OpenKey(state.userHive, runKeyPath, registry.QUERY_VALUE)
		if errors.Is(err, registry.ErrNotExist) {
			return nativeRunRegistration{}, false, nil
		}
		if err != nil {
			return nativeRunRegistration{}, false, fmt.Errorf("open retained target-user Run key: %w", err)
		}
		closeKey = true
	}
	if key == 0 {
		return nativeRunRegistration{}, false, errors.New("retained target-user Run key is unavailable")
	}
	if closeKey {
		defer key.Close() //nolint:errcheck
	}
	return readNativeRunRegistration(key)
}

func readNativeRunRegistration(key registry.Key) (nativeRunRegistration, bool, error) {
	value, valueType, err := key.GetStringValue(runValueKey)
	if errors.Is(err, registry.ErrNotExist) {
		return nativeRunRegistration{}, false, nil
	}
	if err != nil {
		return nativeRunRegistration{}, false, err
	}
	if valueType != registry.SZ && valueType != registry.EXPAND_SZ {
		return nativeRunRegistration{}, false, fmt.Errorf("VIIPER Run value has unsupported registry type %d", valueType)
	}
	return nativeRunRegistration{value: value, valueType: valueType}, true, nil
}

func nativeRunRegistrationsEqual(first nativeRunRegistration, second nativeRunRegistration) bool {
	return first.value == second.value && first.valueType == second.valueType
}

func validateNativeRunRegistrationSnapshot(
	expected *nativeRunRegistration,
	current nativeRunRegistration,
	found bool,
) error {
	if expected == nil {
		if found {
			return errors.New("VIIPER Run registration appeared during native service migration")
		}
		return nil
	}
	if !found || !nativeRunRegistrationsEqual(current, *expected) {
		return errors.New("VIIPER Run registration changed or disappeared during native service migration")
	}
	return nil
}

func setNativeRunRegistration(key registry.Key, value nativeRunRegistration) error {
	switch value.valueType {
	case registry.SZ:
		return key.SetStringValue(runValueKey, value.value)
	case registry.EXPAND_SZ:
		return key.SetExpandStringValue(runValueKey, value.value)
	default:
		return fmt.Errorf("cannot restore VIIPER Run value with registry type %d", value.valueType)
	}
}

func nativeUserRunKeyPath(userSID string) (string, error) {
	userSID = strings.TrimSpace(userSID)
	if userSID == "" || strings.ContainsAny(userSID, `\/`) {
		return "", errors.New("installing user SID is missing or invalid")
	}
	if _, err := windows.StringToSid(userSID); err != nil {
		return "", fmt.Errorf("parse installing user SID: %w", err)
	}
	return userSID + `\` + runKeyPath, nil
}

func parseWindowsCommand(
	commandLine string,
	expand func(string) (string, error),
) (nativeLegacyCommand, error) {
	arguments, err := windows.DecomposeCommandLine(commandLine)
	if err != nil {
		return nativeLegacyCommand{}, err
	}
	if len(arguments) == 0 || strings.TrimSpace(arguments[0]) == "" {
		return nativeLegacyCommand{}, errors.New("startup command has no executable")
	}
	if expand == nil {
		return nativeLegacyCommand{}, errors.New("startup command environment expander is required")
	}
	executable, err := expand(arguments[0])
	if err != nil {
		return nativeLegacyCommand{}, fmt.Errorf("expand target-user startup executable: %w", err)
	}
	executable = filepath.Clean(executable)
	if !strings.EqualFold(filepath.Base(executable), "viiper.exe") {
		return nativeLegacyCommand{}, fmt.Errorf("startup command is not VIIPER: %s", executable)
	}
	return nativeLegacyCommand{executable: executable, arguments: arguments[1:]}, nil
}

func currentScheduledTaskCommand(ctx context.Context) (nativeLegacyCommand, string, bool, bool, bool, error) {
	// The script is a fixed program: no path, account, or other caller-controlled
	// text is interpolated into it. JSON preserves spaces and quoting exactly,
	// while CommandLineToArgvW below applies Windows' own argument grammar.
	const script = `$ErrorActionPreference='Stop';` +
		`[Console]::OutputEncoding=[Text.UTF8Encoding]::new();` +
		`$m=@(Get-ScheduledTask -ErrorAction Stop|Where-Object{$_.TaskPath -ceq '\' -and $_.TaskName -ieq 'RunVIIPER'});` +
		`if($m.Count -gt 1){throw 'multiple root RunVIIPER tasks found'};$t=$null;if($m.Count -eq 1){$t=$m[0]};` +
		`if($null -eq $t){[pscustomobject]@{Found=$false}|ConvertTo-Json -Compress;exit 0};` +
		`$a=@($t.Actions);if($a.Count -ne 1){throw 'scheduled task must contain exactly one action'};` +
		`$x=Export-ScheduledTask -TaskName 'RunVIIPER' -TaskPath '\' -ErrorAction Stop;` +
		`$s=[string]$t.State;` +
		`[pscustomobject]@{Found=$true;Name=$t.TaskName;Active=($s -eq 'Running' -or $s -eq 'Queued');Enabled=[bool]$t.Settings.Enabled;Execute=$a[0].Execute;Arguments=$a[0].Arguments;WorkingDirectory=$a[0].WorkingDirectory;Xml=$x}|ConvertTo-Json -Compress`
	powershell, err := trustedSystemExecutable("WindowsPowerShell", "v1.0", "powershell.exe")
	if err != nil {
		return nativeLegacyCommand{}, "", false, false, false, fmt.Errorf("resolve trusted PowerShell: %w", err)
	}
	output, err := exec.CommandContext(ctx, powershell, "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		return nativeLegacyCommand{}, "", false, false, false, fmt.Errorf("scheduled task query failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	var action struct {
		Found            bool
		Name             string
		Active           bool
		Enabled          bool
		Execute          string
		Arguments        string
		WorkingDirectory string
		XML              string
	}
	if err := json.Unmarshal(output, &action); err != nil {
		return nativeLegacyCommand{}, "", false, false, false, fmt.Errorf("decode scheduled task action: %w", err)
	}
	if !action.Found {
		return nativeLegacyCommand{}, "", false, false, false, nil
	}
	if !nativeScheduledTaskNameMatches(action.Name) {
		return nativeLegacyCommand{}, "", false, false, false,
			fmt.Errorf("Task Scheduler returned unexpected task identity %q", action.Name)
	}
	if strings.TrimSpace(action.XML) == "" {
		return nativeLegacyCommand{}, "", false, false, false, errors.New("scheduled task export returned empty XML")
	}
	// Task Scheduler owns expansion of its action environment. Preserve the raw
	// action path for exact comparison and never reinterpret it under the
	// elevated installer or LocalSystem environment.
	executable := filepath.Clean(strings.Trim(action.Execute, `"`))
	if !strings.EqualFold(filepath.Base(executable), "viiper.exe") {
		return nativeLegacyCommand{}, "", false, false, false, fmt.Errorf("%s action is not a VIIPER executable: %s", runScheduledTask, executable)
	}
	arguments, err := decomposeWindowsArguments(action.Arguments)
	if err != nil {
		return nativeLegacyCommand{}, "", false, false, false, fmt.Errorf("parse %s arguments: %w", runScheduledTask, err)
	}
	return nativeLegacyCommand{
		executable: executable, arguments: arguments,
		workingDirectory: strings.TrimSpace(action.WorkingDirectory),
	}, action.XML, action.Active, action.Enabled, true, nil
}

func nativeScheduledTaskNameMatches(name string) bool {
	return strings.EqualFold(name, runScheduledTask)
}

func lockNativeLegacyTaskExecutable(executable string) (func() error, func(), error) {
	executable = filepath.Clean(executable)
	if !filepath.IsAbs(executable) || strings.Contains(executable, "%") {
		return nil, nil, fmt.Errorf("RunVIIPER action must use an absolute, already-expanded executable path: %s", executable)
	}
	volume := filepath.VolumeName(executable)
	if len(volume) != 2 || volume[1] != ':' {
		return nil, nil, fmt.Errorf("RunVIIPER action must use a local drive path: %s", executable)
	}
	root := volume + `\`
	relative, err := filepath.Rel(root, executable)
	if err != nil || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, `..\`) {
		return nil, nil, fmt.Errorf("derive RunVIIPER action path components: %w", err)
	}
	parts := strings.Split(relative, `\`)
	var handles []windows.Handle
	closeHandles := func() {
		for index := len(handles) - 1; index >= 0; index-- {
			windows.CloseHandle(handles[index]) //nolint:errcheck
		}
		handles = nil
	}
	fail := func(err error) (func() error, func(), error) {
		closeHandles()
		return nil, nil, err
	}
	current := root
	rootHandle, err := openNativePathWithoutReparse(current, windows.FILE_READ_ATTRIBUTES, true)
	if err != nil {
		return nil, nil, err
	}
	handles = append(handles, rootHandle)
	for index, part := range parts {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		isDirectory := index < len(parts)-1
		access := uint32(windows.FILE_READ_ATTRIBUTES)
		if !isDirectory {
			access |= windows.GENERIC_READ
		}
		handle, openErr := openNativePathWithoutReparse(current, access, isDirectory)
		if openErr != nil {
			return fail(fmt.Errorf("open locked RunVIIPER component %s: %w", current, openErr))
		}
		handles = append(handles, handle)
	}
	leaf := handles[len(handles)-1]
	if err := requireSingleNativeFileLink(leaf); err != nil {
		return fail(fmt.Errorf("reject hard-linked RunVIIPER action: %w", err))
	}
	verify := func() error {
		finalPath, err := nativeFinalPathByHandle(leaf)
		if err != nil {
			return err
		}
		if !strings.EqualFold(finalPath, executable) {
			return fmt.Errorf("RunVIIPER action path identity changed: requested=%s final=%s", executable, finalPath)
		}
		return nil
	}
	if err := verify(); err != nil {
		return fail(err)
	}
	header := make([]byte, 2)
	var read uint32
	if err := windows.ReadFile(leaf, header, &read, nil); err != nil {
		return fail(err)
	}
	if read != 2 || header[0] != 'M' || header[1] != 'Z' {
		return fail(errors.New("RunVIIPER action is not a Windows PE image"))
	}
	return verify, closeHandles, nil
}

func nativeFinalPathByHandle(handle windows.Handle) (string, error) {
	buffer := make([]uint16, 32768)
	length, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), 0)
	if err != nil {
		return "", fmt.Errorf("resolve final path by handle: %w", err)
	}
	if length == 0 || length >= uint32(len(buffer)) {
		return "", errors.New("final path by handle exceeded the Windows path bound")
	}
	path := windows.UTF16ToString(buffer[:length])
	if strings.HasPrefix(path, `\\?\UNC\`) {
		path = `\\` + strings.TrimPrefix(path, `\\?\UNC\`)
	} else {
		path = strings.TrimPrefix(path, `\\?\`)
	}
	return filepath.Clean(path), nil
}

func decomposeWindowsArguments(argumentLine string) ([]string, error) {
	if strings.TrimSpace(argumentLine) == "" {
		return nil, nil
	}
	arguments, err := windows.DecomposeCommandLine("viiper.exe " + argumentLine)
	if err != nil {
		return nil, err
	}
	if len(arguments) == 0 {
		return nil, errors.New("argument decomposition returned no executable")
	}
	return arguments[1:], nil
}

func stopNativeLegacyStartup(ctx context.Context, state *nativeLegacyState, logger *slog.Logger) error {
	return stopNativeLegacyStartupWith(ctx, state, logger, nativeLegacyStopOperations{
		stopScheduled: stopNativeScheduledTask,
		openProcesses: openLegacyProcessesByExecutable,
		terminate:     terminateVerifiedLegacyProcess,
		closeHandle: func(handle windows.Handle) {
			windows.CloseHandle(handle) //nolint:errcheck
		},
	})
}

type nativeLegacyStopOperations struct {
	stopScheduled func(context.Context, string, bool) (nativeScheduledStopResult, error)
	openProcesses func(string, string) ([]nativeLegacyProcess, error)
	terminate     func(nativeLegacyProcess) error
	closeHandle   func(windows.Handle)
}

func stopNativeLegacyStartupWith(
	ctx context.Context,
	state *nativeLegacyState,
	logger *slog.Logger,
	operations nativeLegacyStopOperations,
) error {
	if state.scheduledAction != nil {
		if state.scheduledXML == nil {
			return errors.New("cannot stop RunVIIPER without snapshotted task XML")
		}
		// Once PowerShell is launched, process termination or context cancellation
		// can occur after Disable-ScheduledTask but before JSON reaches Go. Mark the
		// registration as potentially changed before the call so the outer
		// transaction always runs the controlled task rollback probe on failure.
		state.scheduledDisabled = true
		state.scheduledStopped = state.scheduledActive
		result, err := operations.stopScheduled(ctx, *state.scheduledXML, state.scheduledActive)
		if err != nil {
			return err
		}
		if strings.TrimSpace(result.currentXML) == "" {
			return errors.New("RunVIIPER stop returned no current task XML")
		}
		currentXML := result.currentXML
		state.scheduledCurrentXML = &currentXML
		state.scheduledDisabled = result.disabled
		state.scheduledStopped = state.scheduledActive && result.stopped
	}
	seen := make(map[string]bool)
	for index := range state.commands {
		key := strings.ToLower(filepath.Clean(state.commands[index].executable))
		if seen[key] {
			state.commands[index].running = false
			continue
		}
		seen[key] = true
		processes, err := operations.openProcesses(state.commands[index].executable, state.userSID)
		if err != nil {
			return err
		}
		// Record the need to restart before the first termination. A later
		// termination failure must not lose the fact that migration already
		// changed the legacy process set.
		state.commands[index].running = len(processes) != 0
		for processIndex, process := range processes {
			if err := operations.terminate(process); err != nil {
				for closeIndex := processIndex; closeIndex < len(processes); closeIndex++ {
					operations.closeHandle(processes[closeIndex].handle)
				}
				return err
			}
			operations.closeHandle(process.handle)
			logger.Info("terminated legacy VIIPER process", "pid", process.pid)
		}
	}
	return nil
}

func stopNativeScheduledTask(
	ctx context.Context,
	expectedXML string,
	expectedActive bool,
) (nativeScheduledStopResult, error) {
	if strings.TrimSpace(expectedXML) == "" {
		return nativeScheduledStopResult{}, errors.New("cannot compare-and-stop RunVIIPER without snapshotted task XML")
	}
	snapshotCheck := `if($active){throw 'RunVIIPER started after migration snapshot'}`
	if expectedActive {
		snapshotCheck = `if(-not $active){throw 'RunVIIPER stopped after migration snapshot'}`
	}
	script := `$ErrorActionPreference='Stop';` +
		`[Console]::OutputEncoding=[Text.UTF8Encoding]::new();` +
		`$b=[Console]::In.ReadToEnd();$x=[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($b));` +
		`$m=@(Get-ScheduledTask -ErrorAction Stop|Where-Object{$_.TaskPath -ceq '\' -and $_.TaskName -ieq 'RunVIIPER'});` +
		`if($m.Count -ne 1){throw 'expected exactly one root RunVIIPER task'};$t=$m[0];` +
		`$c=Export-ScheduledTask -TaskName 'RunVIIPER' -TaskPath '\' -ErrorAction Stop;` +
		`if($c -cne $x){throw 'RunVIIPER changed during migration'};` +
		`$s=[string]$t.State;$active=($s -eq 'Running' -or $s -eq 'Queued');` + snapshotCheck + `;` +
		`$stopped=$false;` +
		`if([bool]$t.Settings.Enabled){Disable-ScheduledTask -TaskName 'RunVIIPER' -TaskPath '\' -ErrorAction Stop|Out-Null};` +
		`$t=Get-ScheduledTask -TaskName 'RunVIIPER' -TaskPath '\' -ErrorAction Stop;$s=[string]$t.State;` +
		`$nowActive=($s -eq 'Running' -or $s -eq 'Queued');if($nowActive){` +
		`Stop-ScheduledTask -TaskName 'RunVIIPER' -TaskPath '\' -ErrorAction Stop;` +
		`$end=[DateTime]::UtcNow.AddSeconds(5);do{Start-Sleep -Milliseconds 50;` +
		`$t=Get-ScheduledTask -TaskName 'RunVIIPER' -TaskPath '\' -ErrorAction Stop;$s=[string]$t.State;` +
		`if($s -ne 'Running' -and $s -ne 'Queued'){$stopped=$true;break}}while([DateTime]::UtcNow -lt $end);` +
		`if(-not $stopped){throw 'RunVIIPER did not stop'}};` +
		`$current=Export-ScheduledTask -TaskName 'RunVIIPER' -TaskPath '\' -ErrorAction Stop;` +
		`[pscustomobject]@{Stopped=$nowActive;Disabled=$true;CurrentXML=$current}|ConvertTo-Json -Compress`
	powershell, err := trustedSystemExecutable("WindowsPowerShell", "v1.0", "powershell.exe")
	if err != nil {
		return nativeScheduledStopResult{}, fmt.Errorf("resolve trusted PowerShell: %w", err)
	}
	command := exec.CommandContext(ctx, powershell, "-NoProfile", "-NonInteractive", "-Command", script)
	command.Stdin = strings.NewReader(encodeNativeTaskXML(expectedXML))
	output, err := command.CombinedOutput()
	if err != nil {
		return nativeScheduledStopResult{}, fmt.Errorf("compare-disable-and-stop RunVIIPER scheduled task: %w: %s",
			err, strings.TrimSpace(string(output)))
	}
	var result struct {
		Stopped    bool
		Disabled   bool
		CurrentXML string
	}
	if err := json.Unmarshal(output, &result); err != nil {
		return nativeScheduledStopResult{}, fmt.Errorf("decode RunVIIPER stop result: %w", err)
	}
	if strings.TrimSpace(result.CurrentXML) == "" {
		return nativeScheduledStopResult{}, errors.New("RunVIIPER stop returned empty task XML")
	}
	return nativeScheduledStopResult{
		stopped: result.Stopped, disabled: result.Disabled, currentXML: result.CurrentXML,
	}, nil
}

func encodeNativeTaskXML(value string) string {
	return base64.StdEncoding.EncodeToString([]byte(value))
}

func encodeNativeTaskRestorePayload(original, current string) string {
	payload, err := json.Marshal(struct {
		Original string
		Current  string
	}{Original: original, Current: current})
	if err != nil {
		panic("fixed scheduled-task restore payload could not be encoded: " + err.Error())
	}
	return base64.StdEncoding.EncodeToString(payload)
}

type nativeLegacyProcess struct {
	handle windows.Handle
	pid    uint32
}

func openLegacyProcessesByExecutable(target, expectedUserSID string) ([]nativeLegacyProcess, error) {
	target = filepath.Clean(target)
	if _, err := validateNativeInstallingUserSID(expectedUserSID); err != nil {
		return nil, err
	}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snapshot) //nolint:errcheck
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	if err := windows.Process32First(snapshot, &entry); err != nil {
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			return nil, nil
		}
		return nil, err
	}
	var result []nativeLegacyProcess
	closeResult := func() {
		for _, process := range result {
			windows.CloseHandle(process.handle) //nolint:errcheck
		}
	}
	for {
		entryNameMatches := strings.EqualFold(windows.UTF16ToString(entry.ExeFile[:]), filepath.Base(target))
		if entryNameMatches && entry.ProcessID != uint32(os.Getpid()) {
			process, openErr := windows.OpenProcess(
				windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE|windows.SYNCHRONIZE,
				false,
				entry.ProcessID,
			)
			if openErr == nil {
				keepHandle := false
				buffer := make([]uint16, 32768)
				size := uint32(len(buffer))
				if queryErr := windows.QueryFullProcessImageName(process, 0, &buffer[0], &size); queryErr == nil {
					actual := filepath.Clean(windows.UTF16ToString(buffer[:size]))
					if strings.EqualFold(actual, target) {
						var processToken windows.Token
						if tokenErr := windows.OpenProcessToken(process, windows.TOKEN_QUERY, &processToken); tokenErr != nil {
							windows.CloseHandle(process) //nolint:errcheck
							closeResult()
							return nil, fmt.Errorf("query owner of possible legacy VIIPER pid %d: %w", entry.ProcessID, tokenErr)
						}
						owner, tokenErr := processToken.GetTokenUser()
						processToken.Close() //nolint:errcheck
						if tokenErr != nil {
							windows.CloseHandle(process) //nolint:errcheck
							closeResult()
							return nil, fmt.Errorf("read owner of possible legacy VIIPER pid %d: %w", entry.ProcessID, tokenErr)
						}
						if strings.EqualFold(owner.User.Sid.String(), expectedUserSID) {
							result = append(result, nativeLegacyProcess{handle: process, pid: entry.ProcessID})
							keepHandle = true
						}
					}
				} else if status, _ := windows.WaitForSingleObject(process, 0); status != windows.WAIT_OBJECT_0 {
					windows.CloseHandle(process) //nolint:errcheck
					closeResult()
					return nil, fmt.Errorf("revalidate possible legacy VIIPER pid %d: %w", entry.ProcessID, queryErr)
				}
				if !keepHandle {
					windows.CloseHandle(process) //nolint:errcheck
				}
			} else if !errors.Is(openErr, windows.ERROR_INVALID_PARAMETER) {
				closeResult()
				return nil, fmt.Errorf("open possible legacy VIIPER pid %d: %w", entry.ProcessID, openErr)
			}
		}
		if err := windows.Process32Next(snapshot, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				break
			}
			closeResult()
			return nil, err
		}
	}
	return result, nil
}

func terminateVerifiedLegacyProcess(process nativeLegacyProcess) error {
	status, err := windows.WaitForSingleObject(process.handle, 0)
	if err != nil {
		return fmt.Errorf("query legacy VIIPER pid %d: %w", process.pid, err)
	}
	if status == windows.WAIT_OBJECT_0 {
		return nil
	}
	if err := windows.TerminateProcess(process.handle, 1); err != nil {
		if status, _ := windows.WaitForSingleObject(process.handle, 0); status == windows.WAIT_OBJECT_0 {
			return nil
		}
		return fmt.Errorf("terminate legacy VIIPER pid %d: %w", process.pid, err)
	}
	status, err = windows.WaitForSingleObject(process.handle, 5_000)
	if err != nil {
		return fmt.Errorf("wait for legacy VIIPER pid %d: %w", process.pid, err)
	}
	if status != windows.WAIT_OBJECT_0 {
		return fmt.Errorf("legacy VIIPER pid %d did not terminate within 5 seconds", process.pid)
	}
	return nil
}

func removeNativeLegacyRegistrations(ctx context.Context, state nativeLegacyState) error {
	currentRun, runFound, err := currentNativeRunRegistration(state)
	if err != nil {
		return err
	}
	if err := validateNativeRunRegistrationSnapshot(state.runValue, currentRun, runFound); err != nil {
		return err
	}
	if state.runValue != nil {
		if state.runKey == 0 {
			return errors.New("cannot remove VIIPER Run registration without its retained key")
		}
		if err := state.runKey.DeleteValue(runValueKey); err != nil {
			return fmt.Errorf("remove VIIPER Run registration: %w", err)
		}
		if _, found, err := currentNativeRunRegistration(state); err != nil {
			return fmt.Errorf("verify VIIPER Run registration removal: %w", err)
		} else if found {
			return errors.New("VIIPER Run registration still exists after removal")
		}
	}
	currentAction, currentXML, _, _, taskFound, err := currentScheduledTaskCommand(ctx)
	if err != nil {
		return restoreNativeLegacyRegistrationsAfterRemoval(ctx, state, err, false)
	}
	if err := validateNativeScheduledTaskSnapshot(state, currentAction, currentXML, taskFound); err != nil {
		return restoreNativeLegacyRegistrationsAfterRemoval(ctx, state, err, false)
	}
	if state.scheduledAction != nil {
		// Keep the exact registered task disabled instead of unregistering it.
		// Exported task XML omits its registered ACL and cannot recreate a
		// Password-logon credential, so delete/re-register cannot be an exact
		// transaction. A disabled task has no native-mode ownership and remains
		// losslessly reversible on rollback.
		if !state.scheduledDisabled {
			return restoreNativeLegacyRegistrationsAfterRemoval(
				ctx,
				state,
				errors.New("RunVIIPER was not disabled before native ownership commit"),
				false,
			)
		}
	}
	return nil
}

func validateNativeScheduledTaskSnapshot(
	state nativeLegacyState,
	currentAction nativeLegacyCommand,
	currentXML string,
	found bool,
) error {
	if state.scheduledAction == nil {
		if found {
			return errors.New("RunVIIPER scheduled task appeared during native service migration")
		}
		return nil
	}
	if state.scheduledXML == nil || state.scheduledCurrentXML == nil {
		return errors.New("RunVIIPER task XML state is incomplete")
	}
	if !found {
		return errors.New("RunVIIPER scheduled task disappeared during native service migration")
	}
	if !nativeLegacyCommandsEqual(currentAction, *state.scheduledAction) ||
		currentXML != *state.scheduledCurrentXML {
		return errors.New("RunVIIPER scheduled task changed during native service migration")
	}
	return nil
}

func restoreNativeLegacyRegistrationsAfterRemoval(
	ctx context.Context,
	state nativeLegacyState,
	cause error,
	restoreScheduledTask bool,
) error {
	var restoreErrors []error
	if state.runValue != nil {
		if state.runKey == 0 || !state.runKeyExisted {
			restoreErrors = append(restoreErrors,
				errors.New("restore VIIPER Run registration: retained Run key is unavailable"))
		} else {
			current, found, restoreErr := currentNativeRunRegistration(state)
			switch {
			case restoreErr != nil:
			case found && nativeRunRegistrationsEqual(current, *state.runValue):
				// Another recovery path already restored the exact data and type.
			case found:
				restoreErr = errors.New("refusing to overwrite a concurrently changed VIIPER Run registration")
			default:
				restoreErr = setNativeRunRegistration(state.runKey, *state.runValue)
			}
			if restoreErr != nil {
				restoreErrors = append(restoreErrors, fmt.Errorf("restore VIIPER Run registration: %w", restoreErr))
			}
		}
	}
	if restoreScheduledTask && state.scheduledXML != nil {
		currentXML := *state.scheduledXML
		if state.scheduledCurrentXML != nil {
			currentXML = *state.scheduledCurrentXML
		}
		if err := restoreNativeScheduledTask(ctx, *state.scheduledXML, currentXML); err != nil {
			restoreErrors = append(restoreErrors, err)
		}
	}
	return errors.Join(cause, errors.Join(restoreErrors...))
}

func restoreNativeScheduledTask(ctx context.Context, taskXML, expectedCurrentXML string) error {
	if strings.TrimSpace(taskXML) == "" {
		return errors.New("cannot restore RunVIIPER from empty task XML")
	}
	if strings.TrimSpace(expectedCurrentXML) == "" {
		return errors.New("cannot restore RunVIIPER without its expected current task XML")
	}
	_, currentXML, _, _, found, err := currentScheduledTaskCommand(ctx)
	if err != nil {
		return fmt.Errorf("query RunVIIPER before rollback: %w", err)
	}
	if !found {
		return errors.New("RunVIIPER disappeared during rollback")
	}
	if currentXML == taskXML {
		return nil
	}
	if expectedCurrentXML != taskXML && currentXML != expectedCurrentXML {
		return errors.New("RunVIIPER changed outside the installer disable transition")
	}
	if err := validateNativeTaskDisabledOnly(taskXML, currentXML); err != nil {
		return fmt.Errorf("refuse to enable unvalidated RunVIIPER task: %w", err)
	}
	const script = `$ErrorActionPreference='Stop';` +
		`$b=[Console]::In.ReadToEnd();$p=[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($b))|ConvertFrom-Json;` +
		`$x=[string]$p.Original;$expected=[string]$p.Current;` +
		`if([string]::IsNullOrWhiteSpace($x) -or [string]::IsNullOrWhiteSpace($expected)){throw 'empty scheduled-task XML'};` +
		`$m=@(Get-ScheduledTask -ErrorAction Stop|Where-Object{$_.TaskPath -ceq '\' -and $_.TaskName -ieq 'RunVIIPER'});` +
		`if($m.Count -gt 1){throw 'multiple root RunVIIPER tasks found'};$t=$null;if($m.Count -eq 1){$t=$m[0]};` +
		`if($null -eq $t){throw 'RunVIIPER disappeared during rollback'};` +
		`$c=Export-ScheduledTask -TaskName 'RunVIIPER' -TaskPath '\' -ErrorAction Stop;` +
		`if($c -cne $expected){throw 'RunVIIPER changed after structural rollback validation'};` +
		`Enable-ScheduledTask -TaskName 'RunVIIPER' -TaskPath '\' -ErrorAction Stop|Out-Null;` +
		`$verify=Export-ScheduledTask -TaskName 'RunVIIPER' -TaskPath '\' -ErrorAction Stop;` +
		`if($verify -cne $x){Disable-ScheduledTask -TaskName 'RunVIIPER' -TaskPath '\' -ErrorAction Stop|Out-Null;throw 'RunVIIPER did not verify after rollback'}`
	powershell, err := trustedSystemExecutable("WindowsPowerShell", "v1.0", "powershell.exe")
	if err != nil {
		return fmt.Errorf("resolve trusted PowerShell: %w", err)
	}
	command := exec.CommandContext(ctx, powershell, "-NoProfile", "-NonInteractive", "-Command", script)
	command.Stdin = strings.NewReader(encodeNativeTaskRestorePayload(taskXML, currentXML))
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("restore RunVIIPER scheduled task: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func validateNativeTaskDisabledOnly(original, current string) error {
	originalCanonical, originalEnabled, originalHasEnabled, err := canonicalNativeTaskXML(original)
	if err != nil {
		return fmt.Errorf("parse original task XML: %w", err)
	}
	currentCanonical, currentEnabled, currentHasEnabled, err := canonicalNativeTaskXML(current)
	if err != nil {
		return fmt.Errorf("parse current task XML: %w", err)
	}
	if !originalHasEnabled || !currentHasEnabled || !originalEnabled || currentEnabled {
		return errors.New("task does not represent an enabled-to-disabled transition")
	}
	if originalCanonical != currentCanonical {
		return errors.New("task XML differs outside Settings/Enabled")
	}
	return nil
}

func canonicalNativeTaskXML(value string) (string, bool, bool, error) {
	decoder := xml.NewDecoder(strings.NewReader(value))
	decoder.Strict = true
	var canonical strings.Builder
	var stack []xml.Name
	enabled := false
	hasEnabled := false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", false, false, err
		}
		switch typed := token.(type) {
		case xml.StartElement:
			stack = append(stack, typed.Name)
			attributes := make([]string, 0, len(typed.Attr))
			for _, attribute := range typed.Attr {
				attributes = append(attributes,
					attribute.Name.Space+"\x00"+attribute.Name.Local+"="+strconv.Quote(attribute.Value))
			}
			sort.Strings(attributes)
			canonical.WriteString("S")
			canonical.WriteString(typed.Name.Space)
			canonical.WriteByte(0)
			canonical.WriteString(typed.Name.Local)
			canonical.WriteByte('[')
			canonical.WriteString(strings.Join(attributes, ","))
			canonical.WriteByte(']')
		case xml.EndElement:
			canonical.WriteString("E")
			canonical.WriteString(typed.Name.Space)
			canonical.WriteByte(0)
			canonical.WriteString(typed.Name.Local)
			if len(stack) == 0 || stack[len(stack)-1] != typed.Name {
				return "", false, false, errors.New("task XML element stack is inconsistent")
			}
			stack = stack[:len(stack)-1]
		case xml.CharData:
			text := string(typed)
			if nativeTaskEnabledElement(stack) {
				value := strings.TrimSpace(text)
				if value == "" {
					continue
				}
				parsed, err := strconv.ParseBool(value)
				if err != nil || hasEnabled {
					return "", false, false, errors.New("task XML has an invalid Settings/Enabled value")
				}
				enabled, hasEnabled = parsed, true
				canonical.WriteString("T<enabled>")
			} else if strings.TrimSpace(text) != "" {
				canonical.WriteString("T")
				canonical.WriteString(strconv.Quote(text))
			}
		case xml.Comment:
			canonical.WriteString("C")
			canonical.WriteString(strconv.Quote(string(typed)))
		case xml.ProcInst:
			canonical.WriteString("P")
			canonical.WriteString(typed.Target)
			canonical.WriteString(strconv.Quote(string(typed.Inst)))
		case xml.Directive:
			canonical.WriteString("D")
			canonical.WriteString(strconv.Quote(string(typed)))
		}
	}
	if len(stack) != 0 {
		return "", false, false, errors.New("task XML ended with open elements")
	}
	return canonical.String(), enabled, hasEnabled, nil
}

func nativeTaskEnabledElement(stack []xml.Name) bool {
	return len(stack) >= 3 && stack[len(stack)-1].Local == "Enabled" &&
		stack[len(stack)-2].Local == "Settings" && stack[0].Local == "Task"
}

func nativeLegacyCommandsEqual(first, second nativeLegacyCommand) bool {
	if !strings.EqualFold(filepath.Clean(first.executable), filepath.Clean(second.executable)) ||
		!strings.EqualFold(filepath.Clean(first.workingDirectory), filepath.Clean(second.workingDirectory)) ||
		len(first.arguments) != len(second.arguments) {
		return false
	}
	for index := range first.arguments {
		if first.arguments[index] != second.arguments[index] {
			return false
		}
	}
	return true
}

func restartNativeLegacyStartup(ctx context.Context, state nativeLegacyState) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if state.scheduledStopped {
		if state.scheduledXML == nil {
			return errors.New("cannot restart RunVIIPER without snapshotted task XML")
		}
		if state.verifyTaskAction == nil {
			return errors.New("cannot restart RunVIIPER without its retained action identity")
		}
		if err := state.verifyTaskAction(); err != nil {
			return fmt.Errorf("revalidate RunVIIPER action before rollback restart: %w", err)
		}
		if err := startNativeScheduledTask(ctx, *state.scheduledXML); err != nil {
			return err
		}
	}
	started := make(map[string]bool)
	for _, command := range state.commands {
		key := strings.ToLower(filepath.Clean(command.executable))
		if !command.running || started[key] {
			continue
		}
		started[key] = true
		var err error
		switch command.source {
		case legacyCommandRun:
			current, found, queryErr := currentNativeRunRegistration(state)
			if queryErr != nil {
				err = fmt.Errorf("verify VIIPER Run registration before restart: %w", queryErr)
			} else if state.runValue == nil || !found || !nativeRunRegistrationsEqual(current, *state.runValue) {
				err = errors.New("refusing to restart HKCU VIIPER because its registration changed during migration")
			} else {
				err = startNativeLegacyCommandAsShellUser(command, state.userSID)
			}
		default:
			err = errors.New("legacy VIIPER command has no trusted startup source")
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func startNativeScheduledTask(ctx context.Context, expectedXML string) error {
	if strings.TrimSpace(expectedXML) == "" {
		return errors.New("cannot restart RunVIIPER without snapshotted task XML")
	}
	const script = `$ErrorActionPreference='Stop';` +
		`$b=[Console]::In.ReadToEnd();$x=[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($b));` +
		`$m=@(Get-ScheduledTask -ErrorAction Stop|Where-Object{$_.TaskPath -ceq '\' -and $_.TaskName -ieq 'RunVIIPER'});` +
		`if($m.Count -ne 1){throw 'expected exactly one root RunVIIPER task'};$t=$m[0];` +
		`$c=Export-ScheduledTask -TaskName 'RunVIIPER' -TaskPath '\' -ErrorAction Stop;` +
		`if($c -cne $x){throw 'RunVIIPER changed before restart'};` +
		`$s=[string]$t.State;if($s -eq 'Running' -or $s -eq 'Queued'){exit 0};` +
		`Start-ScheduledTask -TaskName 'RunVIIPER' -TaskPath '\' -ErrorAction Stop`
	powershell, err := trustedSystemExecutable("WindowsPowerShell", "v1.0", "powershell.exe")
	if err != nil {
		return fmt.Errorf("resolve trusted PowerShell: %w", err)
	}
	command := exec.CommandContext(ctx, powershell, "-NoProfile", "-NonInteractive", "-Command", script)
	command.Stdin = strings.NewReader(encodeNativeTaskXML(expectedXML))
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("restart RunVIIPER scheduled task: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func startNativeLegacyCommandAsShellUser(command nativeLegacyCommand, expectedUserSID string) error {
	shellToken, err := nativeInteractiveUserToken(
		expectedUserSID,
		windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE|windows.TOKEN_ASSIGN_PRIMARY|
			windows.TOKEN_ADJUST_DEFAULT|windows.TOKEN_ADJUST_SESSIONID,
	)
	if err != nil {
		return fmt.Errorf("open target interactive user token: %w", err)
	}
	defer shellToken.Close() //nolint:errcheck

	commandLine, err := windowsCommandLine(command.executable, command.arguments...)
	if err != nil {
		return err
	}
	application, err := windows.UTF16PtrFromString(command.executable)
	if err != nil {
		return err
	}
	mutableCommandLine, err := windows.UTF16FromString(commandLine)
	if err != nil {
		return err
	}
	var currentDirectory *uint16
	if command.workingDirectory != "" {
		currentDirectory, err = windows.UTF16PtrFromString(command.workingDirectory)
		if err != nil {
			return err
		}
	}
	desktop, err := windows.UTF16PtrFromString(`winsta0\default`)
	if err != nil {
		return err
	}
	var environment *uint16
	if err := windows.CreateEnvironmentBlock(&environment, shellToken, false); err != nil {
		return fmt.Errorf("create interactive user environment: %w", err)
	}
	defer windows.DestroyEnvironmentBlock(environment) //nolint:errcheck
	startup := windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{})), Desktop: desktop}
	process := windows.ProcessInformation{}
	if err := windows.CreateProcessAsUser(
		shellToken,
		application,
		&mutableCommandLine[0],
		nil,
		nil,
		false,
		windows.CREATE_UNICODE_ENVIRONMENT,
		environment,
		currentDirectory,
		&startup,
		&process,
	); err != nil {
		return fmt.Errorf("restart HKCU VIIPER under the interactive shell token: %w", err)
	}
	windows.CloseHandle(process.Thread)  //nolint:errcheck
	windows.CloseHandle(process.Process) //nolint:errcheck
	return nil
}

func hasRunningLegacyCommand(state nativeLegacyState) bool {
	if state.scheduledStopped {
		return true
	}
	for _, command := range state.commands {
		if command.running {
			return true
		}
	}
	return false
}

func isLocalSystemServiceAccount(account string) bool {
	account = strings.TrimSpace(account)
	return account == "" || strings.EqualFold(account, "LocalSystem") ||
		strings.EqualFold(account, `.\LocalSystem`) ||
		strings.EqualFold(account, `NT AUTHORITY\SYSTEM`)
}

func isEquivalentServiceAccount(first, second string) bool {
	if isLocalSystemServiceAccount(first) && isLocalSystemServiceAccount(second) {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(first), strings.TrimSpace(second))
}
