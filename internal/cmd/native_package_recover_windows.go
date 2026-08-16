//go:build windows

package cmd

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"
)

const nativePackageRecoveryDriverServiceName = "ViiperUde"

const (
	nativePackageRecoveryMaximumCertificateBytes   = 1024 * 1024
	nativePackageRecoveryMaximumAuthorizationBytes = 256 * 1024
	nativePackageRecoveryMaximumEvidenceBytes      = 16 * 1024 * 1024
	nativePackageRecoveryTrustLeaseDirectoryName   = "VIIPER-TrustManager"
	nativePackageRecoveryTrustLeaseFileName        = "lease-v1.lock"
	nativePackageRecoveryTrustLeaseDirectorySDDL   = "O:BAG:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
	nativePackageRecoveryTrustLeaseFileSDDL        = "O:BAG:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)"
	nativePackageRecoveryMarkerPreparingName       = "failed-install-recovery-preparing-v1.json"
	nativePackageRecoveryMarkerPendingName         = "failed-install-recovery-pending-v1.json"
	nativePackageRecoveryMarkerSettledName         = "failed-install-recovery-settled-v1.json"
	nativePackageRecoveryMaximumMarkerBytes        = 4096
	nativePackageLocalTrustPreparingName           = "local-test-trust-preparing-v1.json"
	nativePackageLocalTrustPendingName             = "local-test-trust-pending-v1.json"
	nativePackageLocalTrustOwnedName               = "local-test-trust-owned-v1.json"
	nativePackageLocalTrustUninstallingName        = "local-test-trust-uninstalling-v1.json"
)

type nativePackageR4RecoveryStateIdentity struct {
	Schema        string `json:"schema"`
	Machine       string `json:"machine"`
	TargetUserSID string `json:"targetUserSid"`
}

type nativePackageRecoveryTrustCounts struct {
	root             int
	trustedPublisher int
}

type nativePackageRecoveryMarkerPaths struct {
	directory string
	lease     string
	preparing string
	pending   string
	settled   string
}

type nativePackageRecoveryMarkerState struct {
	paths       nativePackageRecoveryMarkerPaths
	bytes       []byte
	sha256      string
	wasSettled  bool
	wasResuming bool
}

type nativePackageRecoveryMarkerRecord struct {
	Schema                  string `json:"schema"`
	RootAuthorizationSHA256 string `json:"rootAuthorizationSha256"`
	CertificateSHA256       string `json:"certificateSha256"`
	SourceRevision          string `json:"sourceRevision"`
}

func validateNativePackageRecoveryMarkerAdmission(
	present int,
	allowPartial bool,
	trustBefore nativePackageRecoveryTrustCounts,
) error {
	if present > 1 {
		return errors.New("multiple failed-install recovery marker states exist")
	}
	if present == 0 && allowPartial &&
		(trustBefore.root != 1 || trustBefore.trustedPublisher != 1) {
		return errors.New(
			"partial trust on retry requires an exact pre-existing protected recovery marker",
		)
	}
	if present == 1 && !allowPartial {
		return errors.New("an existing recovery marker requires a bound retry authorization")
	}
	return nil
}

func resolveNativePackageRecoveryMarkerPaths() (nativePackageRecoveryMarkerPaths, error) {
	programData, err := windows.KnownFolderPath(
		windows.FOLDERID_ProgramData, windows.KF_FLAG_DEFAULT,
	)
	if err != nil {
		return nativePackageRecoveryMarkerPaths{}, fmt.Errorf(
			"resolve ProgramData trust-marker root: %w", err,
		)
	}
	programData = filepath.Clean(programData)
	directory := filepath.Join(programData, nativePackageRecoveryTrustLeaseDirectoryName)
	paths := nativePackageRecoveryMarkerPaths{
		directory: directory,
		lease:     filepath.Join(directory, nativePackageRecoveryTrustLeaseFileName),
		preparing: filepath.Join(directory, nativePackageRecoveryMarkerPreparingName),
		pending:   filepath.Join(directory, nativePackageRecoveryMarkerPendingName),
		settled:   filepath.Join(directory, nativePackageRecoveryMarkerSettledName),
	}
	if !strings.EqualFold(filepath.Dir(directory), programData) ||
		!strings.EqualFold(filepath.Dir(paths.lease), directory) ||
		!strings.EqualFold(filepath.Dir(paths.preparing), directory) ||
		!strings.EqualFold(filepath.Dir(paths.pending), directory) ||
		!strings.EqualFold(filepath.Dir(paths.settled), directory) {
		return nativePackageRecoveryMarkerPaths{}, errors.New(
			"native package recovery marker escaped fixed ProgramData",
		)
	}
	return paths, nil
}

func canonicalNativePackageRecoveryMarker(
	request nativePackageRecoverRequest,
) ([]byte, string) {
	contents := []byte(fmt.Sprintf(
		"{\"schema\":\"viiper.native.failed-install-recovery-marker/v1\",\"rootAuthorizationSha256\":\"%s\",\"certificateSha256\":\"%s\",\"sourceRevision\":\"%s\"}\n",
		request.recoveryRootAuthorizationSHA256,
		request.expectedCertificateSHA256,
		request.sourceRevision,
	))
	digest := sha256.Sum256(contents)
	return contents, hex.EncodeToString(digest[:])
}

func readExactNativePackageRecoveryMarker(path string, expected []byte) (bool, error) {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	handle, err := windows.CreateFile(
		pointer,
		windows.GENERIC_READ|windows.READ_CONTROL,
		windows.FILE_SHARE_READ,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
			errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return false, nil
		}
		return false, err
	}
	defer windows.CloseHandle(handle) //nolint:errcheck
	information := windows.ByHandleFileInformation{}
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return false, err
	}
	if information.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		return false, errors.New("recovery marker is not a regular non-reparse file")
	}
	if err := validateNativeFileLinkCount(information.NumberOfLinks); err != nil {
		return false, fmt.Errorf("validate recovery marker link count: %w", err)
	}
	if err := validateNativeSecurityDescriptor(
		handle, nativePackageRecoveryTrustLeaseFileSDDL,
	); err != nil {
		return false, fmt.Errorf("validate recovery marker security: %w", err)
	}
	contents, err := readNativePackageRecoveryFile(
		handle, nativePackageRecoveryMaximumMarkerBytes,
	)
	if err != nil {
		return false, err
	}
	if !bytes.Equal(contents, expected) {
		return false, errors.New("recovery marker bytes do not match this exact authorization, certificate, and source")
	}
	return true, nil
}

func decodeCanonicalNativePackageRecoveryMarker(
	contents []byte,
) (nativePackageRecoveryMarkerRecord, error) {
	value := nativePackageRecoveryMarkerRecord{}
	if len(contents) < 2 || len(contents) > nativePackageRecoveryMaximumMarkerBytes ||
		contents[len(contents)-1] != '\n' || bytes.IndexByte(contents[:len(contents)-1], '\n') >= 0 {
		return value, errors.New("settled recovery marker has invalid framing")
	}
	decoder := json.NewDecoder(bytes.NewReader(contents[:len(contents)-1]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, fmt.Errorf("decode settled recovery marker: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return value, errors.New("settled recovery marker has trailing JSON")
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return value, err
	}
	canonical = append(canonical, '\n')
	if !bytes.Equal(canonical, contents) {
		return value, errors.New("settled recovery marker is not canonical")
	}
	if value.Schema != "viiper.native.failed-install-recovery-marker/v1" ||
		!nativePackageSHA256.MatchString(value.RootAuthorizationSHA256) ||
		!nativePackageSHA256.MatchString(value.CertificateSHA256) ||
		!nativePackageHexRevision.MatchString(value.SourceRevision) {
		return value, errors.New("settled recovery marker identity is invalid")
	}
	return value, nil
}

func retireNativePackageSettledRecoveryMarker(path string) error {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(
		pointer,
		windows.GENERIC_READ|windows.READ_CONTROL|windows.DELETE,
		windows.FILE_SHARE_READ,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
			errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return nil
		}
		return err
	}
	closed := false
	defer func() {
		if !closed {
			windows.CloseHandle(handle) //nolint:errcheck
		}
	}()
	information := windows.ByHandleFileInformation{}
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return err
	}
	if information.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		return errors.New("settled recovery marker is not a regular non-reparse file")
	}
	if err := validateNativeFileLinkCount(information.NumberOfLinks); err != nil {
		return fmt.Errorf("validate settled recovery marker link count: %w", err)
	}
	if err := validateNativeSecurityDescriptor(
		handle, nativePackageRecoveryTrustLeaseFileSDDL,
	); err != nil {
		return fmt.Errorf("validate settled recovery marker security: %w", err)
	}
	contents, err := readNativePackageRecoveryFile(
		handle, nativePackageRecoveryMaximumMarkerBytes,
	)
	if err != nil {
		return err
	}
	if _, err := decodeCanonicalNativePackageRecoveryMarker(contents); err != nil {
		return err
	}
	if err := deleteNativePackageUninstallFileHandle(handle); err != nil {
		return fmt.Errorf("delete validated settled recovery marker by handle: %w", err)
	}
	if err := windows.CloseHandle(handle); err != nil {
		return fmt.Errorf("close retired settled recovery marker: %w", err)
	}
	closed = true
	if _, err := nativePathAttributes(path); err == nil {
		return errors.New("validated settled recovery marker remained after retirement")
	} else if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) &&
		!errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
		return fmt.Errorf("prove settled recovery marker retired: %w", err)
	}
	return nil
}

func createExactNativePackageRecoveryPreparation(path string, contents []byte) error {
	return createExactNativePackageRecoveryPreparationWithCutpoint(path, contents, nil)
}

func createExactNativePackageRecoveryPreparationWithCutpoint(
	path string,
	contents []byte,
	cutpoint func(string) error,
) error {
	if len(contents) == 0 || len(contents) > nativePackageRecoveryMaximumMarkerBytes {
		return errors.New("recovery marker preparation length is outside its exact bound")
	}
	security, err := nativeSecurityAttributes(nativePackageRecoveryTrustLeaseFileSDDL)
	if err != nil {
		return err
	}
	path = filepath.Clean(path)
	directory := filepath.Dir(path)
	var nonce [16]byte
	if _, err := cryptorand.Read(nonce[:]); err != nil {
		return fmt.Errorf("generate recovery marker scratch identity: %w", err)
	}
	scratch := filepath.Join(
		directory,
		"."+filepath.Base(path)+"."+hex.EncodeToString(nonce[:])+".scratch",
	)
	if !strings.EqualFold(filepath.Dir(scratch), directory) ||
		strings.EqualFold(scratch, path) {
		return errors.New("recovery marker scratch escaped its exact directory")
	}
	pointer, err := windows.UTF16PtrFromString(scratch)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(
		pointer,
		windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL,
		windows.FILE_SHARE_READ,
		security,
		windows.CREATE_NEW,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_WRITE_THROUGH|
			windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return err
	}
	closed := false
	published := false
	defer func() {
		if !closed {
			windows.CloseHandle(handle) //nolint:errcheck
		}
		if !published {
			deleteNativePackageFile(scratch) //nolint:errcheck
		}
	}()
	if cutpoint != nil {
		if err := cutpoint("scratch-created"); err != nil {
			return err
		}
	}
	information := windows.ByHandleFileInformation{}
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return err
	}
	if information.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 ||
		information.NumberOfLinks != 1 {
		return errors.New("new recovery marker preparation is not an exact regular single-link file")
	}
	if err := validateNativeSecurityDescriptor(
		handle, nativePackageRecoveryTrustLeaseFileSDDL,
	); err != nil {
		return fmt.Errorf("validate new recovery marker security: %w", err)
	}
	remaining := contents
	for len(remaining) != 0 {
		var written uint32
		if err := windows.WriteFile(handle, remaining, &written, nil); err != nil {
			return err
		}
		if written == 0 || int(written) > len(remaining) {
			return errors.New("recovery marker write made no bounded progress")
		}
		remaining = remaining[written:]
	}
	if cutpoint != nil {
		if err := cutpoint("scratch-written"); err != nil {
			return err
		}
	}
	if err := windows.FlushFileBuffers(handle); err != nil {
		return fmt.Errorf("flush recovery marker preparation: %w", err)
	}
	if cutpoint != nil {
		if err := cutpoint("scratch-flushed"); err != nil {
			return err
		}
	}
	if _, err := windows.SetFilePointer(handle, 0, nil, windows.FILE_BEGIN); err != nil {
		return err
	}
	readback, err := readNativePackageRecoveryFile(
		handle, nativePackageRecoveryMaximumMarkerBytes,
	)
	if err != nil {
		return err
	}
	if !bytes.Equal(readback, contents) {
		return errors.New("recovery marker preparation changed during write-through readback")
	}
	if cutpoint != nil {
		if err := cutpoint("scratch-verified"); err != nil {
			return err
		}
	}
	if err := windows.CloseHandle(handle); err != nil {
		return err
	}
	closed = true
	if cutpoint != nil {
		if err := cutpoint("before-publish"); err != nil {
			return err
		}
	}
	if err := moveNativePackageFile(scratch, path, false); err != nil {
		return fmt.Errorf("atomically publish complete recovery marker preparation: %w", err)
	}
	published = true
	if exists, err := readExactNativePackageRecoveryMarker(path, contents); err != nil {
		return fmt.Errorf("validate atomically published recovery marker preparation: %w", err)
	} else if !exists {
		return errors.New("atomically published recovery marker preparation is absent")
	}
	if cutpoint != nil {
		if err := cutpoint("after-publish"); err != nil {
			return err
		}
	}
	return nil
}

func prepareNativePackageRecoveryMarker(
	request nativePackageRecoverRequest,
	trustBefore nativePackageRecoveryTrustCounts,
) (nativePackageRecoveryMarkerState, error) {
	paths, err := resolveNativePackageRecoveryMarkerPaths()
	if err != nil {
		return nativePackageRecoveryMarkerState{}, err
	}
	contents, digest := canonicalNativePackageRecoveryMarker(request)
	if !request.allowPartialCertificateState {
		if err := retireNativePackageSettledRecoveryMarker(paths.settled); err != nil {
			return nativePackageRecoveryMarkerState{}, fmt.Errorf(
				"retire validated prior recovery-chain settlement: %w", err,
			)
		}
	}
	preparing, err := readExactNativePackageRecoveryMarker(paths.preparing, contents)
	if err != nil {
		return nativePackageRecoveryMarkerState{}, fmt.Errorf("inspect preparing recovery marker: %w", err)
	}
	pending, err := readExactNativePackageRecoveryMarker(paths.pending, contents)
	if err != nil {
		return nativePackageRecoveryMarkerState{}, fmt.Errorf("inspect pending recovery marker: %w", err)
	}
	settled, err := readExactNativePackageRecoveryMarker(paths.settled, contents)
	if err != nil {
		return nativePackageRecoveryMarkerState{}, fmt.Errorf("inspect settled recovery marker: %w", err)
	}
	present := 0
	for _, exists := range []bool{preparing, pending, settled} {
		if exists {
			present++
		}
	}
	if err := validateNativePackageRecoveryMarkerAdmission(
		present, request.allowPartialCertificateState, trustBefore,
	); err != nil {
		return nativePackageRecoveryMarkerState{}, err
	}
	state := nativePackageRecoveryMarkerState{
		paths: paths, bytes: contents, sha256: digest,
		wasSettled: settled, wasResuming: preparing || pending || settled,
	}
	if settled {
		return state, nil
	}
	if preparing {
		if err := moveNativePackageFile(paths.preparing, paths.pending, false); err != nil {
			return nativePackageRecoveryMarkerState{}, fmt.Errorf(
				"publish prepared failed-install recovery marker: %w", err,
			)
		}
	} else if !pending {
		if err := createExactNativePackageRecoveryPreparation(
			paths.preparing, contents,
		); err != nil {
			return nativePackageRecoveryMarkerState{}, fmt.Errorf(
				"create failed-install recovery marker preparation: %w", err,
			)
		}
		if err := moveNativePackageFile(paths.preparing, paths.pending, false); err != nil {
			return nativePackageRecoveryMarkerState{}, fmt.Errorf(
				"publish failed-install recovery pending marker: %w", err,
			)
		}
	}
	if exists, err := readExactNativePackageRecoveryMarker(paths.pending, contents); err != nil {
		return nativePackageRecoveryMarkerState{}, fmt.Errorf("validate published pending recovery marker: %w", err)
	} else if !exists {
		return nativePackageRecoveryMarkerState{}, errors.New("pending recovery marker publication was not durable")
	}
	return state, nil
}

func settleNativePackageRecoveryMarker(state nativePackageRecoveryMarkerState) error {
	if state.wasSettled {
		if exists, err := readExactNativePackageRecoveryMarker(
			state.paths.settled, state.bytes,
		); err != nil || !exists {
			if err == nil {
				err = errors.New("settled recovery marker disappeared")
			}
			return err
		}
		return nil
	}
	if err := moveNativePackageFile(state.paths.pending, state.paths.settled, false); err != nil {
		return fmt.Errorf("atomically settle failed-install recovery marker: %w", err)
	}
	if exists, err := readExactNativePackageRecoveryMarker(
		state.paths.settled, state.bytes,
	); err != nil || !exists {
		if err == nil {
			err = errors.New("settled recovery marker was not durably readable")
		}
		return err
	}
	for _, path := range []string{state.paths.preparing, state.paths.pending} {
		if _, err := nativePathAttributes(path); err == nil {
			return fmt.Errorf("live recovery marker remained after settlement: %s", path)
		} else if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) &&
			!errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return fmt.Errorf("prove live recovery marker absent after settlement: %w", err)
		}
	}
	return nil
}

// requireNativePackageTrustRecoveryClear is called by every supported install
// path while the outer trust lease and package mutex are held. Presence of
// either pre-publication or pending state is a durable hard stop. An exact,
// protected, canonical settled marker is terminal evidence and is retired by
// handle so it cannot poison a later independent install/recovery chain.
func requireNativePackageTrustRecoveryClear() error {
	paths, err := resolveNativePackageRecoveryMarkerPaths()
	if err != nil {
		return err
	}
	for _, path := range []string{paths.preparing, paths.pending} {
		if _, err := nativePathAttributes(path); err == nil {
			return fmt.Errorf("failed-install trust recovery remains pending at %s", path)
		} else if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) &&
			!errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return fmt.Errorf("inspect failed-install trust recovery admission: %w", err)
		}
	}
	if err := retireNativePackageSettledRecoveryMarker(paths.settled); err != nil {
		return fmt.Errorf("retire prior settled failed-install recovery: %w", err)
	}
	return nil
}

func requireNativePackageRecoveryNoLocalTrustOwner() error {
	paths, err := resolveNativePackageRecoveryMarkerPaths()
	if err != nil {
		return err
	}
	for _, name := range []string{
		nativePackageLocalTrustPreparingName,
		nativePackageLocalTrustPendingName,
		nativePackageLocalTrustOwnedName,
		nativePackageLocalTrustUninstallingName,
	} {
		path := filepath.Join(paths.directory, name)
		if !strings.EqualFold(filepath.Dir(path), paths.directory) {
			return errors.New("local-test trust ownership marker escaped its protected root")
		}
		if _, err := nativePathAttributes(path); err == nil {
			return fmt.Errorf(
				"local-test trust ownership remains active at %s; failed-install recovery has no deletion authority",
				path,
			)
		} else if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) &&
			!errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return fmt.Errorf("inspect local-test trust ownership admission: %w", err)
		}
	}
	return nil
}

// requireNativePackageRecoveryOuterTrustLease returns a lifetime guard so a
// native install child can prove it is nested inside a supported signed outer
// manager. The caller must hold the guard until its package transaction joins.
func requireNativePackageRecoveryOuterTrustLease() (func(), error) {
	handle, directoryHandles, err := openNativePackageRecoveryTrustLease()
	if err != nil {
		return nil, err
	}
	if err := requireNativePackageRecoveryTrustLeaseHeld(handle); err != nil {
		windows.CloseHandle(handle) //nolint:errcheck
		closeNativePackageUninstallHandles(directoryHandles)
		return nil, err
	}
	return func() {
		windows.CloseHandle(handle) //nolint:errcheck
		closeNativePackageUninstallHandles(directoryHandles)
	}, nil
}

// openNativePackageRecoveryTrustLease proves that the signed outer manager
// owns the protected trust-transaction lease before this hidden native command
// can inspect or mutate LocalMachine trust. The manager creates the fixed
// ProgramData object with its ACL at creation time and holds byte zero locked
// across certificate mutation, the joined child, and cleanup. This command
// never creates, repairs, or takes ownership of either object.
func openNativePackageRecoveryTrustLease() (windows.Handle, []windows.Handle, error) {
	paths, err := resolveNativePackageRecoveryMarkerPaths()
	if err != nil {
		return 0, nil, err
	}
	directory := paths.directory
	path := paths.lease
	directoryHandles, err := lockNativePackageDirectoryChain(directory)
	if err != nil {
		return 0, nil, fmt.Errorf("lock protected trust-lease directory chain: %w", err)
	}
	fail := func(cause error) (windows.Handle, []windows.Handle, error) {
		closeNativePackageUninstallHandles(directoryHandles)
		return 0, nil, cause
	}
	directoryHandle, err := openNativePathWithoutReparse(
		directory, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL, true,
	)
	if err != nil {
		return fail(fmt.Errorf("open protected trust-lease directory: %w", err))
	}
	if err := validateNativeSecurityDescriptor(
		directoryHandle, nativePackageRecoveryTrustLeaseDirectorySDDL,
	); err != nil {
		windows.CloseHandle(directoryHandle) //nolint:errcheck
		return fail(fmt.Errorf("validate protected trust-lease directory: %w", err))
	}
	directoryHandles = append(directoryHandles, directoryHandle)

	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return fail(err)
	}
	handle, err := windows.CreateFile(
		pointer,
		windows.GENERIC_READ|windows.READ_CONTROL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return fail(fmt.Errorf("open protected trust-lease file: %w", err))
	}
	closeAndFail := func(cause error) (windows.Handle, []windows.Handle, error) {
		windows.CloseHandle(handle) //nolint:errcheck
		return fail(cause)
	}
	information := windows.ByHandleFileInformation{}
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return closeAndFail(fmt.Errorf("inspect protected trust-lease file: %w", err))
	}
	if information.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 ||
		information.FileSizeHigh != 0 || information.FileSizeLow != 1 {
		return closeAndFail(errors.New(
			"protected trust-lease file is not the exact one-byte regular file",
		))
	}
	if err := validateNativeFileLinkCount(information.NumberOfLinks); err != nil {
		return closeAndFail(fmt.Errorf("validate protected trust-lease link count: %w", err))
	}
	if err := validateNativeSecurityDescriptor(
		handle, nativePackageRecoveryTrustLeaseFileSDDL,
	); err != nil {
		return closeAndFail(fmt.Errorf("validate protected trust-lease file: %w", err))
	}
	if _, err := windows.SetFilePointer(handle, 0, nil, windows.FILE_BEGIN); err != nil {
		return closeAndFail(fmt.Errorf("rewind protected trust-lease file: %w", err))
	}
	marker := []byte{0}
	var read uint32
	if err := windows.ReadFile(handle, marker, &read, nil); err != nil ||
		read != 1 || marker[0] != 1 {
		if err == nil {
			err = errors.New("trust-lease marker is not canonical")
		}
		return closeAndFail(fmt.Errorf("read protected trust-lease marker: %w", err))
	}
	return handle, directoryHandles, nil
}

func acquireNativePackageRecoveryTrustLease(
	ctx context.Context,
	deadline time.Time,
) (windows.Handle, []windows.Handle, error) {
	handle, directoryHandles, err := openNativePackageRecoveryTrustLease()
	if err != nil {
		return 0, nil, err
	}
	fail := func(cause error) (windows.Handle, []windows.Handle, error) {
		windows.CloseHandle(handle) //nolint:errcheck
		closeNativePackageUninstallHandles(directoryHandles)
		return 0, nil, cause
	}
	for {
		overlapped := windows.Overlapped{}
		err := windows.LockFileEx(
			handle,
			windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
			0,
			1,
			0,
			&overlapped,
		)
		if err == nil {
			return handle, directoryHandles, nil
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return fail(fmt.Errorf("acquire protected trust-lease byte: %w", err))
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fail(context.DeadlineExceeded)
		}
		pause := 50 * time.Millisecond
		if remaining < pause {
			pause = remaining
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return fail(ctx.Err())
		case <-timer.C:
		}
	}
}

func releaseNativePackageRecoveryTrustLease(
	handle windows.Handle,
	directoryHandles []windows.Handle,
) error {
	overlapped := windows.Overlapped{}
	unlockErr := windows.UnlockFileEx(handle, 0, 1, 0, &overlapped)
	closeErr := windows.CloseHandle(handle)
	closeNativePackageUninstallHandles(directoryHandles)
	if unlockErr != nil {
		return fmt.Errorf("release protected trust-lease byte: %w", unlockErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close protected trust-lease file: %w", closeErr)
	}
	return nil
}

func requireNativePackageRecoveryTrustLeaseHeld(handle windows.Handle) error {
	overlapped := windows.Overlapped{}
	err := windows.LockFileEx(
		handle,
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		&overlapped,
	)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("probe protected trust-lease owner: %w", err)
	}
	unlockErr := windows.UnlockFileEx(handle, 0, 1, 0, &overlapped)
	if unlockErr != nil {
		return fmt.Errorf("release unauthorized trust-lease probe: %w", unlockErr)
	}
	return errors.New("native package recovery requires the signed outer manager to hold the protected trust lease")
}

func lockNativePackageRecoveryBrokerJournalParent() ([]windows.Handle, error) {
	programData, err := windows.KnownFolderPath(
		windows.FOLDERID_ProgramData, windows.KF_FLAG_DEFAULT,
	)
	if err != nil {
		return nil, fmt.Errorf("resolve ProgramData broker-journal root: %w", err)
	}
	programData = filepath.Clean(programData)
	product := filepath.Join(programData, "VIIPER")
	root := filepath.Join(product, nativeBrokerJournalRootName)
	if !strings.EqualFold(filepath.Dir(product), programData) ||
		!strings.EqualFold(filepath.Dir(root), product) {
		return nil, errors.New("native broker journal escaped fixed ProgramData")
	}
	handles, err := lockNativePackageDirectoryChain(programData)
	if err != nil {
		return nil, fmt.Errorf("lock ProgramData for broker-journal absence: %w", err)
	}
	productHandle, err := openNativePathWithoutReparse(
		product, windows.FILE_READ_ATTRIBUTES, true,
	)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
			errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return handles, nil
		}
		closeNativePackageUninstallHandles(handles)
		return nil, fmt.Errorf("open broker-journal product parent: %w", err)
	}
	handles = append(handles, productHandle)
	rootHandle, err := openNativePathWithoutReparse(
		root, windows.FILE_READ_ATTRIBUTES, true,
	)
	if err != nil {
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
			errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			return handles, nil
		}
		closeNativePackageUninstallHandles(handles)
		return nil, fmt.Errorf("prove broker-journal root absent: %w", err)
	}
	windows.CloseHandle(rootHandle) //nolint:errcheck
	closeNativePackageUninstallHandles(handles)
	return nil, errors.New(
		"BrokerTransactions exists; exact R4 recordless recovery has no authority over any broker journal",
	)
}

func readNativePackageRecoveryFile(handle windows.Handle, maximum uint64) ([]byte, error) {
	information := windows.ByHandleFileInformation{}
	if err := windows.GetFileInformationByHandle(handle, &information); err != nil {
		return nil, err
	}
	size := uint64(information.FileSizeHigh)<<32 | uint64(information.FileSizeLow)
	if size == 0 || size > maximum {
		return nil, fmt.Errorf("bound recovery artifact length %d is outside 1..%d", size, maximum)
	}
	if _, err := windows.SetFilePointer(handle, 0, nil, windows.FILE_BEGIN); err != nil {
		return nil, err
	}
	contents := make([]byte, int(size))
	position := 0
	for position < len(contents) {
		var read uint32
		if err := windows.ReadFile(handle, contents[position:], &read, nil); err != nil {
			return nil, err
		}
		if read == 0 {
			return nil, errors.New("bound recovery artifact ended before its authenticated length")
		}
		position += int(read)
	}
	if _, err := windows.SetFilePointer(handle, 0, nil, windows.FILE_BEGIN); err != nil {
		return nil, err
	}
	return contents, nil
}

func openNativePackageRecoveryCertificateStore(name string) (windows.Handle, error) {
	storeName, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	store, err := windows.CertOpenStore(
		windows.CERT_STORE_PROV_SYSTEM_W,
		0,
		0,
		windows.CERT_SYSTEM_STORE_LOCAL_MACHINE|
			windows.CERT_STORE_OPEN_EXISTING_FLAG|
			windows.CERT_STORE_MAXIMUM_ALLOWED_FLAG,
		uintptr(unsafe.Pointer(storeName)),
	)
	if err != nil {
		return 0, err
	}
	return store, nil
}

func nativePackageRecoveryCertificateMatches(
	certificate *windows.CertContext,
	expectedDER []byte,
) bool {
	if certificate == nil || certificate.EncodedCert == nil ||
		uint64(certificate.Length) != uint64(len(expectedDER)) {
		return false
	}
	return bytes.Equal(
		unsafe.Slice(certificate.EncodedCert, int(certificate.Length)),
		expectedDER,
	)
}

func deleteExactNativePackageRecoveryCertificate(
	store windows.Handle,
	expectedDER []byte,
) error {
	var previous *windows.CertContext
	for enumerated := 0; enumerated < 65536; enumerated++ {
		certificate, err := windows.CertEnumCertificatesInStore(store, previous)
		if err != nil {
			if errors.Is(err, syscall.Errno(windows.CRYPT_E_NOT_FOUND)) {
				return errors.New("exact recovery certificate disappeared before its authorized deletion")
			}
			return err
		}
		previous = certificate
		if nativePackageRecoveryCertificateMatches(certificate, expectedDER) {
			// CertDeleteCertificateFromStore always frees certificate. Do not
			// pass it back to CertEnumCertificatesInStore after this call.
			return windows.CertDeleteCertificateFromStore(certificate)
		}
	}
	if previous != nil {
		windows.CertFreeCertificateContext(previous) //nolint:errcheck
	}
	return errors.New("certificate store deletion exceeded its safety bound")
}

func validateNativePackageRecoveryTrustAdmission(
	counts nativePackageRecoveryTrustCounts,
	allowPartial bool,
) error {
	if !allowPartial {
		if counts.root != 1 || counts.trustedPublisher != 1 {
			return fmt.Errorf("initial recovery requires exact trust counts Root=1 TrustedPublisher=1; observed Root=%d TrustedPublisher=%d",
				counts.root, counts.trustedPublisher)
		}
		return nil
	}
	if counts.root < 0 || counts.root > 1 || counts.trustedPublisher < 0 || counts.trustedPublisher > 1 {
		return fmt.Errorf("bound recovery retry permits only zero/one exact trust counts; observed Root=%d TrustedPublisher=%d",
			counts.root, counts.trustedPublisher)
	}
	return nil
}

func executeNativePackageRecoveryHelper(
	helperPath string,
	arguments []string,
) (string, int, error) {
	command := exec.Command(helperPath, arguments...)
	command.Dir = filepath.Dir(helperPath)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		return "", 0, err
	}
	waitErr := waitNativePackageHelper(command)
	exitCode := 0
	if waitErr != nil {
		var exitError *exec.ExitError
		if !errors.As(waitErr, &exitError) {
			return output.String(), 0, waitErr
		}
		exitCode = exitError.ExitCode()
	}
	return output.String(), exitCode, waitErr
}

func requireNativePackageRecoveryServiceAbsent(name string) error {
	manager, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to SCM for %s recovery admission: %w", name, err)
	}
	defer manager.Disconnect() //nolint:errcheck
	service, err := manager.OpenService(name)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return nil
		}
		return fmt.Errorf("inspect %s recovery admission: %w", name, err)
	}
	defer service.Close() //nolint:errcheck
	return fmt.Errorf("service %s exists; a current or successor VIIPER topology blocks certificate cleanup", name)
}

func validateNativePackageFailedInstallRecoveryCapability(
	capability nativePackageFailedInstallRecoveryCapability,
	request nativePackageRecoverRequest,
	expectedLeasePath string,
	parentPID uint32,
	parentCreationFileTime uint64,
) error {
	if capability.Schema != nativePackageFailedInstallRecoveryCapabilitySchema ||
		len(capability.Nonce) != 32 || capability.Nonce != strings.ToLower(capability.Nonce) {
		return errors.New("failed-install recovery capability schema or nonce is noncanonical")
	}
	if _, err := hex.DecodeString(capability.Nonce); err != nil {
		return errors.New("failed-install recovery capability nonce is not 128-bit lowercase hexadecimal")
	}
	if !strings.EqualFold(filepath.Clean(capability.LeasePath), expectedLeasePath) ||
		capability.SourceRevision != request.sourceRevision ||
		capability.HelperSHA256 != request.expectedHelperSHA256 ||
		capability.CertificateSHA256 != request.expectedCertificateSHA256 ||
		capability.RecoveryAuthorizationSHA256 != request.expectedRecoveryAuthorizationSHA256 ||
		capability.RecoveryRootAuthorizationSHA256 != request.recoveryRootAuthorizationSHA256 ||
		capability.PackageLockSHA256 != request.currentPackageLockSHA256 ||
		capability.BundleManifestSHA256 != request.currentBundleManifestSHA256 ||
		capability.AllowPartialCertificateState != request.allowPartialCertificateState {
		return errors.New("failed-install recovery capability does not bind the exact lease, package, authorization, trust, and retry request")
	}
	if capability.ParentPID != parentPID ||
		capability.ParentCreationFileTime != parentCreationFileTime {
		return errors.New("failed-install recovery capability was not issued by this broker process parent")
	}
	return nil
}

func lockAndVerifyNativePackageFailedInstallRecoveryCapability(
	request nativePackageRecoverRequest,
) ([]windows.Handle, windows.Handle, error) {
	capabilityPath := filepath.Clean(request.recoveryCapability)
	if filepath.Base(capabilityPath) != nativePackageFailedInstallRecoveryCapabilityName ||
		!strings.EqualFold(filepath.Dir(capabilityPath), filepath.Dir(request.brokerSource)) {
		return nil, 0, errors.New(
			"failed-install recovery capability is not the exact protected sibling of the staged broker",
		)
	}
	directoryHandles, err := lockNativePackageDirectoryChain(filepath.Dir(capabilityPath))
	if err != nil {
		return nil, 0, fmt.Errorf("lock failed-install recovery capability directory chain: %w", err)
	}
	keepDirectories := false
	defer func() {
		if !keepDirectories {
			closeNativePackageUninstallHandles(directoryHandles)
		}
	}()
	capabilityHandle, err := lockNativePackageInput(capabilityPath)
	if err != nil {
		return nil, 0, fmt.Errorf("lock failed-install recovery capability: %w", err)
	}
	keepCapability := false
	defer func() {
		if !keepCapability {
			windows.CloseHandle(capabilityHandle) //nolint:errcheck
		}
	}()
	if err := validateNativeSecurityDescriptor(
		capabilityHandle, nativePackageRecoveryTrustLeaseFileSDDL,
	); err != nil {
		return nil, 0, fmt.Errorf("validate failed-install recovery capability security: %w", err)
	}
	digest, err := hashNativePackageHandle(capabilityHandle)
	if err != nil {
		return nil, 0, fmt.Errorf("hash failed-install recovery capability: %w", err)
	}
	if digest != request.expectedRecoveryCapabilitySHA256 {
		return nil, 0, fmt.Errorf(
			"failed-install recovery capability SHA-256=%s expected=%s",
			digest, request.expectedRecoveryCapabilitySHA256,
		)
	}
	contents, err := readNativePackageCapabilityHandle(
		capabilityHandle, nativePackageRecoveryMaximumMarkerBytes,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("read failed-install recovery capability: %w", err)
	}
	capability := nativePackageFailedInstallRecoveryCapability{}
	if err := decodeCanonicalNativeBrokerJSON(
		contents, &capability, nativePackageRecoveryMaximumMarkerBytes,
	); err != nil {
		return nil, 0, fmt.Errorf("decode canonical failed-install recovery capability: %w", err)
	}
	paths, err := resolveNativePackageRecoveryMarkerPaths()
	if err != nil {
		return nil, 0, err
	}
	parentPID, parentCreationFileTime, err := nativePackageParentIdentity()
	if err != nil {
		return nil, 0, err
	}
	if err := validateNativePackageFailedInstallRecoveryCapability(
		capability, request, paths.lease, parentPID, parentCreationFileTime,
	); err != nil {
		return nil, 0, err
	}
	keepDirectories = true
	keepCapability = true
	return directoryHandles, capabilityHandle, nil
}

func lockAndValidateNativePackageR4RecoveryEvidence(
	authorization nativePackageR4RecoveryAuthorization,
) ([]windows.Handle, []windows.Handle, error) {
	predecessor := authorization.Predecessor
	directoryHandles := make([]windows.Handle, 0)
	fileHandles := make([]windows.Handle, 0, 5)
	fail := func(cause error) ([]windows.Handle, []windows.Handle, error) {
		closeNativePackageUninstallHandles(fileHandles)
		closeNativePackageUninstallHandles(directoryHandles)
		return nil, nil, cause
	}
	for _, directory := range []string{
		filepath.Dir(predecessor.StatePath),
		predecessor.InstallEvidenceDirectory,
	} {
		handles, err := lockNativePackageDirectoryChain(directory)
		if err != nil {
			return fail(fmt.Errorf("lock exact R4 predecessor evidence directory chain: %w", err))
		}
		directoryHandles = append(directoryHandles, handles...)
	}
	type evidenceFile struct {
		label  string
		path   string
		digest string
		state  bool
	}
	evidence := []evidenceFile{
		{"state", predecessor.StatePath, predecessor.StateSHA256, true},
		{"command", filepath.Join(predecessor.InstallEvidenceDirectory, "command.json"), predecessor.CommandSHA256, false},
		{"result", filepath.Join(predecessor.InstallEvidenceDirectory, "result.json"), predecessor.ResultSHA256, false},
		{"stdout", filepath.Join(predecessor.InstallEvidenceDirectory, "stdout.log"), predecessor.StdoutSHA256, false},
		{"stderr", filepath.Join(predecessor.InstallEvidenceDirectory, "stderr.log"), predecessor.StderrSHA256, false},
	}
	for _, item := range evidence {
		handle, err := lockNativePackageInput(item.path)
		if err != nil {
			return fail(fmt.Errorf("lock exact R4 predecessor %s evidence: %w", item.label, err))
		}
		fileHandles = append(fileHandles, handle)
		digest, err := hashNativePackageHandle(handle)
		if err != nil {
			return fail(fmt.Errorf("hash exact R4 predecessor %s evidence: %w", item.label, err))
		}
		if digest != item.digest {
			return fail(fmt.Errorf(
				"exact R4 predecessor %s SHA-256=%s expected=%s",
				item.label, digest, item.digest,
			))
		}
		if !item.state {
			continue
		}
		contents, err := readNativePackageRecoveryFile(
			handle, nativePackageRecoveryMaximumEvidenceBytes,
		)
		if err != nil {
			return fail(fmt.Errorf("read exact R4 predecessor state: %w", err))
		}
		state := nativePackageR4RecoveryStateIdentity{}
		if err := json.Unmarshal(contents, &state); err != nil {
			return fail(fmt.Errorf("parse exact R4 predecessor state identity: %w", err))
		}
		if state.Schema != "viiper.windows11.validation-state/v1" ||
			!strings.EqualFold(state.Machine, authorization.Machine) ||
			state.TargetUserSID != authorization.TargetUserSID {
			return fail(errors.New(
				"exact R4 predecessor state does not bind the recovery machine and target user",
			))
		}
	}
	return directoryHandles, fileHandles, nil
}

func recoverNativePackage(
	ctx context.Context,
	logger *slog.Logger,
	request nativePackageRecoverRequest,
) (resultErr error) {
	_ = logger
	deadline, err := nativePackageRecoverDeadline(ctx)
	if err != nil {
		return err
	}
	capabilityDirectoryHandles, capabilityHandle, err :=
		lockAndVerifyNativePackageFailedInstallRecoveryCapability(request)
	if err != nil {
		return fmt.Errorf("verify parent-bound failed-install recovery authority: %w", err)
	}
	defer closeNativePackageUninstallHandles(capabilityDirectoryHandles)
	defer windows.CloseHandle(capabilityHandle) //nolint:errcheck
	if err := initializeNativePackageRecoveryTrustLease(); err != nil {
		return fmt.Errorf("initialize protected trust transaction: %w", err)
	}
	trustLease, trustLeaseDirectoryHandles, err := acquireNativePackageRecoveryTrustLease(
		ctx, deadline,
	)
	if err != nil {
		return fmt.Errorf("acquire protected trust transaction: %w", err)
	}
	defer func() {
		resultErr = errors.Join(resultErr, releaseNativePackageRecoveryTrustLease(
			trustLease, trustLeaseDirectoryHandles,
		))
	}()
	packageBudget := time.Until(deadline)
	releasePackage, err := acquireNamedNativePackageMutex(nativePackageMutexName, packageBudget)
	if err != nil {
		return fmt.Errorf("acquire native package recovery mutex: %w", err)
	}
	defer releasePackage()

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("native package recovery canceled before service lock: %w", err)
	}
	serviceBudget := time.Until(deadline)
	releaseService, err := acquireNativeInstallMutex(serviceBudget)
	if err != nil {
		return fmt.Errorf("acquire native broker service mutex after package mutex: %w", err)
	}
	defer releaseService()

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("native package recovery canceled before helper verification: %w", err)
	}
	directoryHandles, err := lockNativePackageDirectoryChain(filepath.Dir(request.driverHelper))
	if err != nil {
		return fmt.Errorf("lock packaged recovery helper directory chain: %w", err)
	}
	defer closeNativePackageUninstallHandles(directoryHandles)
	helper, err := lockNativePackageInput(request.driverHelper)
	if err != nil {
		return fmt.Errorf("lock packaged recovery helper: %w", err)
	}
	defer windows.CloseHandle(helper) //nolint:errcheck
	helperHash, err := hashNativePackageHandle(helper)
	if err != nil {
		return fmt.Errorf("hash packaged recovery helper: %w", err)
	}
	if !strings.EqualFold(helperHash, request.expectedHelperSHA256) {
		return fmt.Errorf("packaged recovery helper SHA-256=%s expected=%s",
			helperHash, request.expectedHelperSHA256)
	}
	if err := requireNativePackagePE(helper); err != nil {
		return fmt.Errorf("validate packaged recovery helper image: %w", err)
	}
	certificateDirectoryHandles, err := lockNativePackageDirectoryChain(
		filepath.Dir(request.certificatePath),
	)
	if err != nil {
		return fmt.Errorf("lock recovery certificate directory chain: %w", err)
	}
	defer closeNativePackageUninstallHandles(certificateDirectoryHandles)
	certificate, err := lockNativePackageInput(request.certificatePath)
	if err != nil {
		return fmt.Errorf("lock exact recovery certificate: %w", err)
	}
	defer windows.CloseHandle(certificate) //nolint:errcheck
	certificateHash, err := hashNativePackageHandle(certificate)
	if err != nil {
		return fmt.Errorf("hash exact recovery certificate: %w", err)
	}
	if !strings.EqualFold(certificateHash, request.expectedCertificateSHA256) {
		return fmt.Errorf("recovery certificate SHA-256=%s expected=%s",
			certificateHash, request.expectedCertificateSHA256)
	}
	certificateDER, err := readNativePackageRecoveryFile(
		certificate, nativePackageRecoveryMaximumCertificateBytes,
	)
	if err != nil {
		return fmt.Errorf("read exact recovery certificate bytes: %w", err)
	}
	authorizationDirectoryHandles, err := lockNativePackageDirectoryChain(
		filepath.Dir(request.recoveryAuthorization),
	)
	if err != nil {
		return fmt.Errorf("lock recovery authorization directory chain: %w", err)
	}
	defer closeNativePackageUninstallHandles(authorizationDirectoryHandles)
	authorization, err := lockNativePackageInput(request.recoveryAuthorization)
	if err != nil {
		return fmt.Errorf("lock recovery authorization receipt: %w", err)
	}
	defer windows.CloseHandle(authorization) //nolint:errcheck
	authorizationHash, err := hashNativePackageHandle(authorization)
	if err != nil {
		return fmt.Errorf("hash recovery authorization receipt: %w", err)
	}
	if !strings.EqualFold(
		authorizationHash, request.expectedRecoveryAuthorizationSHA256,
	) {
		return fmt.Errorf("recovery authorization SHA-256=%s expected=%s",
			authorizationHash, request.expectedRecoveryAuthorizationSHA256)
	}
	authorizationBytes, err := readNativePackageRecoveryFile(
		authorization, nativePackageRecoveryMaximumAuthorizationBytes,
	)
	if err != nil {
		return fmt.Errorf("read bounded recovery authorization receipt: %w", err)
	}
	hostname, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("resolve current recovery machine identity: %w", err)
	}
	authorizationValue, err := validateNativePackageR4RecoveryAuthorization(
		authorizationBytes, request, hostname,
	)
	if err != nil {
		return fmt.Errorf("validate exact R4 failed-install recovery authority: %w", err)
	}
	predecessorDirectoryHandles, predecessorFileHandles, err :=
		lockAndValidateNativePackageR4RecoveryEvidence(authorizationValue)
	if err != nil {
		return fmt.Errorf("lease exact retained R4 failed-install evidence: %w", err)
	}
	defer closeNativePackageUninstallHandles(predecessorFileHandles)
	defer closeNativePackageUninstallHandles(predecessorDirectoryHandles)

	// Rehash the non-write/delete-shared image immediately before process
	// creation. No broker/service inspection or removal occurs on this path.
	sealedHash, err := hashNativePackageHandle(helper)
	if err != nil {
		return fmt.Errorf("rehash sealed packaged recovery helper: %w", err)
	}
	if !strings.EqualFold(sealedHash, request.expectedHelperSHA256) {
		return errors.New("sealed packaged recovery helper changed before launch")
	}
	recoverArguments := []string{
		"recover-failed-install-recordless", "--transaction-deadline-unix-ms",
		strconv.FormatInt(deadline.UnixMilli(), 10),
	}
	// Never use CommandContext or terminate the helper. This exact operation is
	// verify-only and must return its single recordless-absence proof intact.
	recoverOutput, processExitCode, waitErr := executeNativePackageRecoveryHelper(
		request.driverHelper, recoverArguments,
	)
	if recoverOutput != "" {
		if _, err := os.Stdout.WriteString(recoverOutput); err != nil {
			return fmt.Errorf("publish packaged recovery helper evidence: %w", err)
		}
	}
	if waitErr != nil {
		var exitError *exec.ExitError
		if !errors.As(waitErr, &exitError) {
			return fmt.Errorf("join packaged recovery helper: %w", waitErr)
		}
	}
	_, proofErr := parseNativePackageRecoverProof(recoverOutput, processExitCode)
	if proofErr != nil {
		if waitErr != nil {
			return fmt.Errorf("verify packaged recovery helper proof: %w (process: %v)",
				proofErr, waitErr)
		}
		return fmt.Errorf("verify packaged recovery helper proof: %w", proofErr)
	}
	brokerJournalParentHandles, err := lockNativePackageRecoveryBrokerJournalParent()
	if err != nil {
		return &nativePackageRecoverExitError{
			cause:    fmt.Errorf("successor-preservation admission rejected: %w", err),
			exitCode: 4,
		}
	}
	defer closeNativePackageUninstallHandles(brokerJournalParentHandles)

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("native package recovery canceled before successor admission: %w", err)
	}
	statusOutput, statusExitCode, statusWaitErr := executeNativePackageRecoveryHelper(
		request.driverHelper, []string{"status"},
	)
	if statusOutput != "" {
		if _, err := os.Stdout.WriteString(statusOutput); err != nil {
			return fmt.Errorf("publish packaged recovery status evidence: %w", err)
		}
	}
	if statusWaitErr != nil {
		var exitError *exec.ExitError
		if !errors.As(statusWaitErr, &exitError) {
			return fmt.Errorf("join packaged recovery status helper: %w", statusWaitErr)
		}
	}
	if err := validateNativePackageRecoverEmptyStatus(
		statusOutput, statusExitCode,
	); err != nil {
		return &nativePackageRecoverExitError{
			cause:    fmt.Errorf("successor-preservation admission rejected: %w", err),
			exitCode: 4,
		}
	}
	for _, serviceName := range []string{
		NativeBrokerServiceName, nativePackageRecoveryDriverServiceName,
	} {
		if err := requireNativePackageRecoveryServiceAbsent(serviceName); err != nil {
			return &nativePackageRecoverExitError{
				cause:    fmt.Errorf("successor-preservation admission rejected: %w", err),
				exitCode: 4,
			}
		}
	}
	if err := requireNativePackageRecoveryNoLocalTrustOwner(); err != nil {
		return &nativePackageRecoverExitError{
			cause:    fmt.Errorf("successor-preservation admission rejected: %w", err),
			exitCode: 4,
		}
	}
	rootStore, err := openNativePackageRecoveryCertificateStore("Root")
	if err != nil {
		return fmt.Errorf("open LocalMachine Root for exact recovery: %w", err)
	}
	defer windows.CertCloseStore(rootStore, 0) //nolint:errcheck
	trustedPublisherStore, err := openNativePackageRecoveryCertificateStore("TrustedPublisher")
	if err != nil {
		return fmt.Errorf("open LocalMachine TrustedPublisher for exact recovery: %w", err)
	}
	defer windows.CertCloseStore(trustedPublisherStore, 0) //nolint:errcheck
	trustBefore, err := inspectNativePackageLocalTestTrust(
		rootStore, trustedPublisherStore, certificateDER,
	)
	if err != nil {
		return err
	}
	if err := validateNativePackageRecoveryTrustAdmission(
		trustBefore, request.allowPartialCertificateState,
	); err != nil {
		return &nativePackageRecoverExitError{
			cause:    fmt.Errorf("successor-preservation admission rejected: %w", err),
			exitCode: 4,
		}
	}
	markerState, err := prepareNativePackageRecoveryMarker(request, trustBefore)
	if err != nil {
		return &nativePackageRecoverExitError{
			cause:    fmt.Errorf("publish durable failed-install recovery authority: %w", err),
			exitCode: 4,
		}
	}
	if markerState.wasSettled &&
		(trustBefore.root != 0 || trustBefore.trustedPublisher != 0) {
		return &nativePackageRecoverExitError{
			cause: errors.New(
				"settled failed-install recovery marker requires exact trust counts Root=0 TrustedPublisher=0",
			),
			exitCode: 4,
		}
	}
	if trustBefore.root == 1 {
		if err := deleteExactNativePackageRecoveryCertificate(rootStore, certificateDER); err != nil {
			return fmt.Errorf("delete exact LocalMachine Root recovery certificate: %w", err)
		}
	}
	if trustBefore.trustedPublisher == 1 {
		if err := deleteExactNativePackageRecoveryCertificate(
			trustedPublisherStore, certificateDER,
		); err != nil {
			return fmt.Errorf("delete exact LocalMachine TrustedPublisher recovery certificate: %w", err)
		}
	}
	trustAfter, err := inspectNativePackageLocalTestTrust(
		rootStore, trustedPublisherStore, certificateDER,
	)
	if err != nil {
		return err
	}
	if trustAfter.root != 0 || trustAfter.trustedPublisher != 0 {
		return errors.New("exact failed-install certificate remained after native locked recovery")
	}
	if err := settleNativePackageRecoveryMarker(markerState); err != nil {
		return fmt.Errorf("settle durable failed-install recovery authority: %w", err)
	}
	resume := 0
	if request.allowPartialCertificateState {
		resume = 1
	}
	if _, err := fmt.Fprintf(os.Stdout,
		"recovery-receipt operation=native-package-recover activeJournal=0 devices=0 packages=0 brokerService=0 driverService=0 successor=0 trustRootBefore=%d trustTrustedPublisherBefore=%d trustRootAfter=0 trustTrustedPublisherAfter=0 marker=settled markerSha256=%s rootAuthorizationSha256=%s authorizationSha256=%s certificateSha256=%s sourceRevision=%s resume=%d\n",
		trustBefore.root, trustBefore.trustedPublisher,
		markerState.sha256,
		request.recoveryRootAuthorizationSHA256,
		request.expectedRecoveryAuthorizationSHA256,
		request.expectedCertificateSHA256, request.sourceRevision, resume,
	); err != nil {
		return fmt.Errorf("publish native package recovery receipt: %w", err)
	}
	return nil
}
