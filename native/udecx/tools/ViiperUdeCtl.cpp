/*
 * Copyright (c) 2026 VIIPER Project contributors
 *
 * Driver-store mutation follows the documented SetupAPI/NewDev contracts.
 * Installs are source-manifest bound, signature checked, version ordered, and
 * rolled back to the exact previously published INF if post-install health
 * verification fails. Removal touches only the exact signed VIIPER package
 * contract and exact ROOT\VIIPER\UDE devnodes.
 */

#define WIN32_LEAN_AND_MEAN
#define NOMINMAX
#ifndef _WIN32_WINNT
#define _WIN32_WINNT 0x0A00
#endif
#include <windows.h>
#include <cfgmgr32.h>
#include <devguid.h>
#include <initguid.h>
#include <devpkey.h>
#include <knownfolders.h>
#include <newdev.h>
#include <objbase.h>
#include <wincrypt.h>
#include <mscat.h>
#include <setupapi.h>
#include <sddl.h>
#include <shlobj.h>
#include <softpub.h>
#include <wintrust.h>
#include <aclapi.h>

#include "../include/ViiperUdeProtocol.h"

#include <algorithm>
#include <array>
#include <atomic>
#include <charconv>
#include <cerrno>
#include <cctype>
#include <cstdio>
#include <cstddef>
#include <cstdint>
#include <cstring>
#include <cwchar>
#include <filesystem>
#include <iomanip>
#include <initializer_list>
#include <iostream>
#include <iterator>
#include <limits>
#include <map>
#include <memory>
#include <optional>
#include <set>
#include <sstream>
#include <string>
#include <string_view>
#include <thread>
#include <utility>
#include <variant>
#include <vector>

// MinGW's setupapi/newdev headers lag these Vista/Windows 10 declarations.
// Keep the signatures identical to the Windows SDK so the independent
// Windows-target syntax gate can compile the same source as MSVC CI.
#if defined(__MINGW32__)
extern "C" {
WINSETUPAPI BOOL WINAPI SetupGetInfPublishedNameW(PCWSTR, PWSTR, DWORD, PDWORD);
WINSETUPAPI BOOL WINAPI SetupGetInfDriverStoreLocationW(
    PCWSTR, PSP_ALTPLATFORM_INFO, PCWSTR, PWSTR, DWORD, PDWORD);
BOOL WINAPI DiUninstallDriverW(HWND, LPCWSTR, DWORD, PBOOL);
}
#endif

#pragma comment(lib, "Cfgmgr32.lib")
#pragma comment(lib, "Newdev.lib")
#pragma comment(lib, "Setupapi.lib")
#pragma comment(lib, "Advapi32.lib")
#pragma comment(lib, "Crypt32.lib")
#pragma comment(lib, "Wintrust.lib")
#pragma comment(lib, "Shell32.lib")
#pragma comment(lib, "Ole32.lib")

namespace {

constexpr wchar_t kHardwareId[] = L"ROOT\\VIIPER\\UDE";
constexpr wchar_t kEnumerator[] = L"ROOT";
// DICD_GENERATE_ID derives ROOT\\<DeviceName>\\#### from this value. Keep
// new devnodes in a VIIPER-owned instance namespace instead of the USB class
// namespace used by older builds.
constexpr wchar_t kRootDeviceName[] = L"VIIPERUDE";
constexpr wchar_t kLegacyRootDeviceName[] = L"USB";
constexpr wchar_t kServiceName[] = L"ViiperUde";
constexpr wchar_t kProviderName[] = L"VIIPER Project";
constexpr wchar_t kCatalogName[] = L"ViiperUde.cat";
constexpr wchar_t kDriverFileName[] = L"ViiperUde.sys";

struct AbiCompatibilityProfile {
    VIIPER_UDE_UINT16 minor;
    VIIPER_UDE_UINT32 capabilities;
    DWORD statsSize;
    bool hasReservedPortFields;
};

constexpr std::array<AbiCompatibilityProfile, 3> kAbiCompatibilityProfiles{{
    {12, 29, 152, true},
    {11, 29, 144, false},
    {10, 13, 144, false},
}};

constexpr bool AbiCompatibilityProfilesAreValid() noexcept {
    return kAbiCompatibilityProfiles[0].minor == VIIPER_UDE_ABI_MINOR &&
        kAbiCompatibilityProfiles[0].capabilities == VIIPER_UDE_ADVERTISED_CAPABILITIES &&
        kAbiCompatibilityProfiles[0].statsSize == sizeof(VIIPER_UDE_STATS) &&
        kAbiCompatibilityProfiles[0].hasReservedPortFields &&
        kAbiCompatibilityProfiles[1].minor == 11 &&
        kAbiCompatibilityProfiles[1].capabilities == 29 &&
        kAbiCompatibilityProfiles[1].statsSize == 144 &&
        !kAbiCompatibilityProfiles[1].hasReservedPortFields &&
        kAbiCompatibilityProfiles[2].minor == 10 &&
        kAbiCompatibilityProfiles[2].capabilities == 13 &&
        kAbiCompatibilityProfiles[2].statsSize == 144 &&
        !kAbiCompatibilityProfiles[2].hasReservedPortFields &&
        kAbiCompatibilityProfiles[0].minor == kAbiCompatibilityProfiles[1].minor + 1 &&
        kAbiCompatibilityProfiles[1].minor == kAbiCompatibilityProfiles[2].minor + 1;
}

static_assert(VIIPER_UDE_ABI_MAJOR == 1, "ABI compatibility table major drift");
static_assert(VIIPER_UDE_ABI_MINOR == 12, "ABI compatibility table current minor drift");
static_assert(VIIPER_UDE_ADVERTISED_CAPABILITIES == 29,
    "ABI compatibility table current capabilities drift");
static_assert(sizeof(VIIPER_UDE_STATS) == 152,
    "ABI compatibility table current statistics size drift");
static_assert(offsetof(VIIPER_UDE_STATS, ReservedPorts) == 144,
    "ABI 1.10/1.11 statistics boundary drift");
static_assert(AbiCompatibilityProfilesAreValid(),
    "ABI compatibility profiles must be exact and strictly descending");

constexpr bool SameAbiCompatibilityProfile(
    const AbiCompatibilityProfile& left,
    const AbiCompatibilityProfile& right) noexcept {
    return left.minor == right.minor &&
        left.capabilities == right.capabilities &&
        left.statsSize == right.statsSize &&
        left.hasReservedPortFields == right.hasReservedPortFields;
}

constexpr bool IsKnownAbiCompatibilityProfile(
    const AbiCompatibilityProfile& profile) noexcept {
    return std::any_of(kAbiCompatibilityProfiles.begin(),
        kAbiCompatibilityProfiles.end(),
        [&](const AbiCompatibilityProfile& known) {
            return SameAbiCompatibilityProfile(profile, known);
        });
}
constexpr wchar_t kModelSection[] = L"Standard.NTamd64.10.0...17763";
constexpr wchar_t kInstallSection[] = L"ViiperUde_Install";
constexpr wchar_t kTransactionNamespace[] = L"VIIPER_UDE_DRIVER_TRANSACTION_NAMESPACE_V1";
constexpr wchar_t kTransactionBoundary[] = L"VIIPER_UDE_DRIVER_TRANSACTION_BOUNDARY_V1";
constexpr wchar_t kTransactionMutex[] = L"VIIPER_UDE_DRIVER_TRANSACTION_V1";
constexpr wchar_t kTransactionObjectSecurity[] =
    L"D:P(A;;GA;;;SY)(A;;GA;;;BA)";
constexpr size_t kMaximumManifestBytes = 1024U * 1024U;
constexpr uint64_t kMaximumTransactionDurationMs = 4ULL * 60ULL * 1000ULL;
// The child can spend 45 seconds in its inner SCM/credential rollback and then
// up to two minutes in the outer protected-image rollback. Keep a bounded
// margin beyond both budgets while retaining the driver mutex until exit.
constexpr uint64_t kBrokerRollbackCeilingMs = 3ULL * 60ULL * 1000ULL;
constexpr uint64_t kDriverRollbackCeilingMs = 2ULL * 60ULL * 1000ULL;
constexpr DWORD kCancelledIoDrainMs = 5000;
constexpr size_t kMaximumBrokerProofBytes = 64U * 1024U;
constexpr size_t kMaximumBrokerDiagnosticCharacters = 1024U;
constexpr std::string_view kBrokerDiagnosticPrefix = "VIIPER: error: ";
constexpr wchar_t kRollbackDirectorySecurity[] =
    L"O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)";
constexpr wchar_t kRecoveryRecordSecurity[] =
    L"O:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)";
constexpr wchar_t kRecoveryRecordName[] = L"recovery-v1.json";
constexpr wchar_t kRecoveryRecordTemporaryName[] = L"recovery-v1.json.tmp";
constexpr size_t kMaximumRecoveryRecordBytes = 256U * 1024U;
constexpr wchar_t kInstallRecoveryProductDirectory[] = L"VIIPER";
constexpr wchar_t kInstallRecoveryComponentDirectory[] = L"UdeCx";
constexpr wchar_t kInstallRecoveryTransactionsDirectory[] = L"Transactions";
constexpr wchar_t kInstallRecoveryActiveDirectory[] = L"active-v2";
constexpr wchar_t kInstallRecoverySettledPrefix[] = L"settled-v2-";
constexpr wchar_t kInstallRecoveryJournalPrefix[] = L"journal-";
constexpr wchar_t kInstallRecoveryJournalSuffix[] = L".json";
constexpr wchar_t kInstallRecoveryTemporarySuffix[] = L".tmp";
constexpr wchar_t kInstallRecoveryPriorDirectory[] = L"prior";
constexpr wchar_t kInstallRecoveryCandidateDirectory[] = L"candidate";
constexpr size_t kMaximumInstallRecoveryRecords = 96;
constexpr std::string_view kInstallRecoveryKind =
    "VIIPER-UDE-install-switch-recovery";
constexpr std::string_view kZeroSha256 =
    "0000000000000000000000000000000000000000000000000000000000000000";
constexpr std::string_view kHardwareVerificationOid = "1.3.6.1.4.1.311.10.3.5";
constexpr std::string_view kAttestationVerificationOid = "1.3.6.1.4.1.311.10.3.5.1";

uint64_t CurrentUnixMilliseconds();

// A fixed-size, allocation-free copy lets the top-level exception boundary
// report the protected write-ahead record after C++ stack unwinding has closed
// all transaction handles. Only one helper transaction exists per process.
std::array<wchar_t, MAX_PATH> gActiveRecoveryRecord{};
bool gActiveRecoveryRecordWritten = false;
std::array<wchar_t, MAX_PATH> gActiveBackupRoot{};
bool gActiveBackupRootRetained = false;
bool gTransactionMutationStarted = false;
bool gLastSynchronousMutationTimedOut = false;

void MarkTransactionMutationStarted() noexcept {
    gTransactionMutationStarted = true;
}

void ClearActiveRecoveryEvidence() noexcept {
    gActiveRecoveryRecord.fill(L'\0');
    gActiveRecoveryRecordWritten = false;
    gActiveBackupRoot.fill(L'\0');
    gActiveBackupRootRetained = false;
}

constexpr GUID kViiperInterfaceGuid = {
    0x32d03f48, 0x725b, 0x4baa, {0x97, 0x0f, 0x7f, 0x5d, 0xe6, 0xc4, 0x46, 0x87}};

enum class ExitCode : int {
    Success = 0,
    Failure = 1,
    Usage = 2,
    RollbackFailed = 3,
    PreflightRejected = 4,
    RebootRequired = ERROR_SUCCESS_REBOOT_REQUIRED,
};

struct Error {
    DWORD code = ERROR_SUCCESS;
    std::wstring phase;
    std::wstring message;
    std::optional<DWORD> nestedExitCode;
    std::wstring recoveryRecord;
    bool recoveryRecordWritten = false;
    DWORD recoveryRecordError = ERROR_SUCCESS;
    std::wstring recoveryRecordPhase;
    std::wstring recoveryRecordMessage;
    std::wstring recoveryBackup;
    bool recoveryBackupRetained = false;
};

bool CheckTransactionDeadline(uint64_t deadlineUnixMs, const wchar_t* phase, Error* error);
bool IsGeneratedRootInstanceIdForDeviceName(
    const std::wstring& instanceId, const wchar_t* deviceName);
bool IsOwnedGeneratedRootInstanceId(const std::wstring& instanceId);

struct Outcome {
    bool success = false;
    bool changed = false;
    bool rebootRequired = false;
    ExitCode exitCode = ExitCode::Failure;
    Error error;
    std::wstring rollback = L"not-needed";
};

std::wstring FormatError(DWORD error) {
    wchar_t* raw = nullptr;
    const DWORD flags = FORMAT_MESSAGE_ALLOCATE_BUFFER |
        FORMAT_MESSAGE_FROM_SYSTEM | FORMAT_MESSAGE_IGNORE_INSERTS;
    const DWORD count = FormatMessageW(
        flags, nullptr, error, 0, reinterpret_cast<wchar_t*>(&raw), 0, nullptr);
    std::wstring message = count != 0 && raw != nullptr ? std::wstring(raw, count) : L"unknown error";
    if (raw != nullptr) {
        LocalFree(raw);
    }
    while (!message.empty() &&
        (message.back() == L'\r' || message.back() == L'\n' ||
         message.back() == L' ' || message.back() == L'.')) {
        message.pop_back();
    }
    return message;
}

bool SetError(Error* error, const wchar_t* phase, DWORD code, std::wstring message = {}) {
    if (error != nullptr) {
        error->code = code;
        error->phase = phase;
        error->message = message.empty() ? FormatError(code) : std::move(message);
        error->nestedExitCode.reset();
    }
    SetLastError(code);
    return false;
}

bool SetLastErrorDetail(Error* error, const wchar_t* phase, std::wstring message = {}) {
    return SetError(error, phase, GetLastError(), std::move(message));
}

void EmitOutcome(const wchar_t* operation, const Outcome& outcome) {
    std::wostream& stream = outcome.success ? std::wcout : std::wcerr;
    stream << L"result=" << (outcome.success ? L"success" : L"error")
           << L" operation=" << operation
           << L" changed=" << (outcome.changed ? 1 : 0)
           << L" rebootRequired=" << (outcome.rebootRequired ? 1 : 0)
           << L" rollback=" << outcome.rollback
           << L" exitCode=" << static_cast<int>(outcome.exitCode);
    if (!outcome.success) {
        stream << L" phase=" << std::quoted(outcome.error.phase)
               << L" win32Error=" << outcome.error.code;
        if (outcome.error.nestedExitCode) {
            stream << L" nestedExitCode=" << *outcome.error.nestedExitCode;
        }
        stream << L" message=" << std::quoted(outcome.error.message);
        const std::wstring recoveryRecord =
            !outcome.error.recoveryRecord.empty()
                ? outcome.error.recoveryRecord
                : gActiveRecoveryRecord[0] != L'\0'
                    ? std::wstring(gActiveRecoveryRecord.data())
                    : std::wstring{};
        const bool recoveryRecordWritten =
            !outcome.error.recoveryRecord.empty()
                ? outcome.error.recoveryRecordWritten
                : gActiveRecoveryRecordWritten;
        if (!recoveryRecord.empty()) {
            stream << L" recoveryRecord=" << std::quoted(recoveryRecord)
                   << L" recoveryRecordWritten="
                   << (recoveryRecordWritten ? 1 : 0);
            if (!recoveryRecordWritten &&
                !outcome.error.recoveryRecord.empty()) {
                stream << L" recoveryRecordPhase="
                       << std::quoted(outcome.error.recoveryRecordPhase)
                       << L" recoveryRecordWin32Error="
                       << outcome.error.recoveryRecordError
                       << L" recoveryRecordMessage="
                       << std::quoted(outcome.error.recoveryRecordMessage);
            }
        }
        const std::wstring recoveryBackup =
            !outcome.error.recoveryBackup.empty()
                ? outcome.error.recoveryBackup
                : gActiveBackupRoot[0] != L'\0'
                    ? std::wstring(gActiveBackupRoot.data())
                    : std::wstring{};
        if (!recoveryBackup.empty()) {
            stream << L" recoveryBackup=" << std::quoted(recoveryBackup)
                   << L" recoveryBackupRetained="
                   << ((!outcome.error.recoveryBackup.empty()
                           ? outcome.error.recoveryBackupRetained
                           : gActiveBackupRootRetained) ? 1 : 0);
        }
    }
    stream << L"\n";
}

class WinHandle final {
public:
    WinHandle() noexcept = default;
    explicit WinHandle(HANDLE value) noexcept : value_(value) {}
    ~WinHandle() { reset(); }
    WinHandle(const WinHandle&) = delete;
    WinHandle& operator=(const WinHandle&) = delete;
    WinHandle(WinHandle&& other) noexcept : value_(other.release()) {}
    WinHandle& operator=(WinHandle&& other) noexcept {
        if (this != &other) {
            reset(other.release());
        }
        return *this;
    }
    HANDLE get() const noexcept { return value_; }
    explicit operator bool() const noexcept {
        return value_ != nullptr && value_ != INVALID_HANDLE_VALUE;
    }
    HANDLE release() noexcept {
        HANDLE value = value_;
        value_ = INVALID_HANDLE_VALUE;
        return value;
    }
    void reset(HANDLE value = INVALID_HANDLE_VALUE) noexcept {
        if (*this) {
            CloseHandle(value_);
        }
        value_ = value;
    }

private:
    HANDLE value_ = INVALID_HANDLE_VALUE;
};

class SynchronousMutationWatchdog final {
public:
    SynchronousMutationWatchdog(
        uint64_t deadlineUnixMs,
        const wchar_t* apiName)
        : apiName_(apiName == nullptr ? L"unknown" : apiName) {
        completion_.reset(CreateEventW(nullptr, TRUE, FALSE, nullptr));
        if (!completion_) {
            throw std::bad_alloc();
        }
        thread_ = std::thread([this, deadlineUnixMs]() noexcept {
            for (;;) {
                const uint64_t now = CurrentUnixMilliseconds();
                const DWORD waitMilliseconds = deadlineUnixMs <= now
                    ? 0
                    : static_cast<DWORD>(std::min<uint64_t>(
                        deadlineUnixMs - now,
                        std::numeric_limits<DWORD>::max() - 1ULL));
                const DWORD wait = WaitForSingleObject(
                    completion_.get(), waitMilliseconds);
                if (wait == WAIT_OBJECT_0) {
                    return;
                }
                if (wait == WAIT_TIMEOUT) {
                    timedOut_.store(true, std::memory_order_release);
                    std::wstring diagnostic =
                        L"VIIPER: authoritative synchronous mutation exceeded its deadline; "
                        L"the owner remains alive and is still waiting for ";
                    diagnostic += apiName_;
                    diagnostic += L" to return.\n";
                    OutputDebugStringW(diagnostic.c_str());
                    WaitForSingleObject(completion_.get(), INFINITE);
                    return;
                }
                timedOut_.store(true, std::memory_order_release);
                return;
            }
        });
    }

    ~SynchronousMutationWatchdog() noexcept {
        Complete();
    }

    SynchronousMutationWatchdog(const SynchronousMutationWatchdog&) = delete;
    SynchronousMutationWatchdog& operator=(const SynchronousMutationWatchdog&) = delete;

    bool Complete() noexcept {
        if (completion_) {
            SetEvent(completion_.get());
        }
        if (thread_.joinable()) {
            thread_.join();
        }
        return timedOut_.load(std::memory_order_acquire);
    }

private:
    std::wstring apiName_;
    WinHandle completion_;
    std::thread thread_;
    std::atomic<bool> timedOut_{false};
};

template <typename Callback>
auto InvokeAuthoritativeSynchronousMutation(
    uint64_t deadlineUnixMs,
    const wchar_t* apiName,
    Callback&& callback) -> decltype(callback()) {
    SynchronousMutationWatchdog watchdog(deadlineUnixMs, apiName);
    auto result = callback();
    const DWORD callbackError = GetLastError();
    gLastSynchronousMutationTimedOut = watchdog.Complete();
    SetLastError(callbackError);
    return result;
}

class DeviceInfoSet final {
public:
    explicit DeviceInfoSet(HDEVINFO value = INVALID_HANDLE_VALUE) noexcept : value_(value) {}
    ~DeviceInfoSet() {
        if (value_ != INVALID_HANDLE_VALUE) {
            SetupDiDestroyDeviceInfoList(value_);
        }
    }
    DeviceInfoSet(const DeviceInfoSet&) = delete;
    DeviceInfoSet& operator=(const DeviceInfoSet&) = delete;
    DeviceInfoSet(DeviceInfoSet&& other) noexcept : value_(other.value_) {
        other.value_ = INVALID_HANDLE_VALUE;
    }
    DeviceInfoSet& operator=(DeviceInfoSet&& other) noexcept {
        if (this != &other) {
            if (value_ != INVALID_HANDLE_VALUE) {
                SetupDiDestroyDeviceInfoList(value_);
            }
            value_ = other.value_;
            other.value_ = INVALID_HANDLE_VALUE;
        }
        return *this;
    }
    HDEVINFO get() const noexcept { return value_; }
    explicit operator bool() const noexcept { return value_ != INVALID_HANDLE_VALUE; }

private:
    HDEVINFO value_;
};

class InfHandle final {
public:
    explicit InfHandle(HINF value = INVALID_HANDLE_VALUE) noexcept : value_(value) {}
    ~InfHandle() {
        if (value_ != INVALID_HANDLE_VALUE) {
            SetupCloseInfFile(value_);
        }
    }
    InfHandle(const InfHandle&) = delete;
    InfHandle& operator=(const InfHandle&) = delete;
    HINF get() const noexcept { return value_; }
    explicit operator bool() const noexcept { return value_ != INVALID_HANDLE_VALUE; }

private:
    HINF value_;
};

class TransactionMutex final {
public:
    ~TransactionMutex() {
        if (owned_ && mutex_) {
            ReleaseMutex(mutex_.get());
        }
        mutex_.reset();
        if (namespace_ != nullptr) {
            ClosePrivateNamespace(namespace_, 0);
        }
        if (boundary_ != nullptr) {
            DeleteBoundaryDescriptor(boundary_);
        }
    }

    bool Acquire(Error* error) {
        BYTE administratorsBuffer[SECURITY_MAX_SID_SIZE]{};
        DWORD administratorsSize = sizeof(administratorsBuffer);
        if (!CreateWellKnownSid(WinBuiltinAdministratorsSid, nullptr,
                administratorsBuffer, &administratorsSize)) {
            return SetLastErrorDetail(error, L"transaction-boundary-sid");
        }
        boundary_ = CreateBoundaryDescriptorW(kTransactionBoundary, 0);
        if (boundary_ == nullptr ||
            !AddSIDToBoundaryDescriptor(&boundary_, administratorsBuffer)) {
            return SetLastErrorDetail(error, L"transaction-boundary");
        }

        PSECURITY_DESCRIPTOR descriptor = nullptr;
        if (!ConvertStringSecurityDescriptorToSecurityDescriptorW(
                kTransactionObjectSecurity, SDDL_REVISION_1, &descriptor, nullptr)) {
            return SetLastErrorDetail(error, L"transaction-security");
        }
        SECURITY_ATTRIBUTES security{};
        security.nLength = sizeof(security);
        security.lpSecurityDescriptor = descriptor;
        security.bInheritHandle = FALSE;

        namespace_ = CreatePrivateNamespaceW(&security, boundary_, kTransactionNamespace);
        DWORD namespaceError = GetLastError();
        if (namespace_ == nullptr && namespaceError == ERROR_ALREADY_EXISTS) {
            namespace_ = OpenPrivateNamespaceW(boundary_, kTransactionNamespace);
            namespaceError = GetLastError();
        }
        if (namespace_ == nullptr) {
            LocalFree(descriptor);
            SetLastError(namespaceError);
            return SetLastErrorDetail(error, L"transaction-namespace");
        }

        const std::wstring mutexName =
            std::wstring(kTransactionNamespace) + L"\\" + kTransactionMutex;
        mutex_.reset(CreateMutexExW(&security, mutexName.c_str(), 0, MUTEX_ALL_ACCESS));
        const DWORD mutexError = GetLastError();
        LocalFree(descriptor);
        if (!mutex_) {
            SetLastError(mutexError);
            return SetLastErrorDetail(error, L"transaction-mutex");
        }
        const DWORD wait = WaitForSingleObject(mutex_.get(), 0);
        if (wait == WAIT_OBJECT_0 || wait == WAIT_ABANDONED) {
            // WAIT_ABANDONED is safe here: all mutable state is inventoried
            // again before the first SetupAPI operation, so no prior process
            // state is trusted merely because the lock changed owners.
            owned_ = true;
            abandoned_ = wait == WAIT_ABANDONED;
            return true;
        }
        if (wait == WAIT_TIMEOUT) {
            return SetError(error, L"transaction-mutex", ERROR_INSTALL_ALREADY_RUNNING,
                L"another VIIPER native driver transaction is active");
        }
        return SetLastErrorDetail(error, L"transaction-mutex-wait");
    }

private:
    HANDLE namespace_ = nullptr;
    HANDLE boundary_ = nullptr;
    WinHandle mutex_;
    bool owned_ = false;
    bool abandoned_ = false;
};

bool IsElevated() {
    WinHandle token;
    HANDLE raw = nullptr;
    if (!OpenProcessToken(GetCurrentProcess(), TOKEN_QUERY, &raw)) {
        return false;
    }
    token.reset(raw);
    TOKEN_ELEVATION elevation{};
    DWORD returned = 0;
    return GetTokenInformation(token.get(), TokenElevation, &elevation, sizeof(elevation), &returned) &&
        elevation.TokenIsElevated != 0;
}

struct Version {
    std::array<uint32_t, 4> parts{};

    friend bool operator==(const Version&, const Version&) = default;
    friend bool operator<(const Version& left, const Version& right) {
        return left.parts < right.parts;
    }
};

std::wstring VersionToString(const Version& version) {
    std::wostringstream stream;
    stream << version.parts[0] << L'.' << version.parts[1] << L'.'
           << version.parts[2] << L'.' << version.parts[3];
    return stream.str();
}

bool ParseVersion(std::wstring_view text, Version* version) {
    Version parsed{};
    size_t start = 0;
    for (size_t index = 0; index < parsed.parts.size(); ++index) {
        const size_t end = text.find(L'.', start);
        if ((end == std::wstring_view::npos) != (index == parsed.parts.size() - 1)) {
            return false;
        }
        const size_t limit = end == std::wstring_view::npos ? text.size() : end;
        if (limit == start) {
            return false;
        }
        uint32_t value = 0;
        for (size_t cursor = start; cursor < limit; ++cursor) {
            if (text[cursor] < L'0' || text[cursor] > L'9') {
                return false;
            }
            const uint32_t digit = static_cast<uint32_t>(text[cursor] - L'0');
            if (value > (65535U - digit) / 10U) {
                return false;
            }
            value = value * 10U + digit;
        }
        parsed.parts[index] = value;
        start = limit + 1;
    }
    if (version != nullptr) {
        *version = parsed;
    }
    return true;
}

std::string LowerAscii(std::string value) {
    std::transform(value.begin(), value.end(), value.begin(), [](unsigned char character) {
        return static_cast<char>(std::tolower(character));
    });
    return value;
}

bool IsHexRevision(const std::string& value) {
    if (value.size() != 40 && value.size() != 64) {
        return false;
    }
    return std::all_of(value.begin(), value.end(), [](unsigned char character) {
        return std::isxdigit(character) != 0;
    });
}

bool CopySha256Argument(
    const wchar_t* value,
    const wchar_t* name,
    std::string* destination,
    Error* error) {
    const std::wstring wide = value;
    destination->clear();
    destination->reserve(wide.size());
    for (const wchar_t character : wide) {
        if (character > 0x7f) {
            return SetError(error, L"arguments", ERROR_INVALID_PARAMETER,
                std::wstring(name) + L" SHA-256 must contain ASCII hexadecimal characters");
        }
        destination->push_back(static_cast<char>(character));
    }
    if (destination->size() != 64 ||
        !std::all_of(destination->begin(), destination->end(),
            [](unsigned char character) { return std::isxdigit(character) != 0; })) {
        return SetError(error, L"arguments", ERROR_INVALID_PARAMETER,
            std::wstring(name) + L" SHA-256 must contain exactly 64 hexadecimal characters");
    }
    return true;
}

struct JsonValue {
    using Object = std::map<std::string, JsonValue>;
    using Array = std::vector<JsonValue>;
    std::variant<std::nullptr_t, bool, int64_t, std::string, Array, Object> value;
};

class JsonParser final {
public:
    explicit JsonParser(std::string_view text) : text_(text) {}

    bool Parse(JsonValue* value, std::string* message) {
        SkipWhitespace();
        if (!ParseValue(value, 0, message)) {
            return false;
        }
        SkipWhitespace();
        if (position_ != text_.size()) {
            *message = "trailing data after JSON value";
            return false;
        }
        return true;
    }

private:
    void SkipWhitespace() {
        while (position_ < text_.size() &&
            (text_[position_] == ' ' || text_[position_] == '\t' ||
             text_[position_] == '\r' || text_[position_] == '\n')) {
            ++position_;
        }
    }

    bool ParseValue(JsonValue* value, unsigned depth, std::string* message) {
        if (depth > 16) {
            *message = "JSON nesting limit exceeded";
            return false;
        }
        SkipWhitespace();
        if (position_ >= text_.size()) {
            *message = "unexpected end of JSON";
            return false;
        }
        const char current = text_[position_];
        if (current == '{') {
            JsonValue::Object object;
            if (!ParseObject(&object, depth + 1, message)) {
                return false;
            }
            value->value = std::move(object);
            return true;
        }
        if (current == '[') {
            JsonValue::Array array;
            if (!ParseArray(&array, depth + 1, message)) {
                return false;
            }
            value->value = std::move(array);
            return true;
        }
        if (current == '"') {
            std::string stringValue;
            if (!ParseString(&stringValue, message)) {
                return false;
            }
            value->value = std::move(stringValue);
            return true;
        }
        if (Match("true")) {
            value->value = true;
            return true;
        }
        if (Match("false")) {
            value->value = false;
            return true;
        }
        if (Match("null")) {
            value->value = nullptr;
            return true;
        }
        return ParseInteger(value, message);
    }

    bool ParseObject(JsonValue::Object* object, unsigned depth, std::string* message) {
        ++position_;
        SkipWhitespace();
        if (Consume('}')) {
            return true;
        }
        for (;;) {
            std::string key;
            if (!ParseString(&key, message)) {
                return false;
            }
            SkipWhitespace();
            if (!Consume(':')) {
                *message = "expected ':' in JSON object";
                return false;
            }
            JsonValue child;
            if (!ParseValue(&child, depth, message)) {
                return false;
            }
            if (!object->emplace(std::move(key), std::move(child)).second) {
                *message = "duplicate JSON object key";
                return false;
            }
            SkipWhitespace();
            if (Consume('}')) {
                return true;
            }
            if (!Consume(',')) {
                *message = "expected ',' in JSON object";
                return false;
            }
            SkipWhitespace();
        }
    }

    bool ParseArray(JsonValue::Array* array, unsigned depth, std::string* message) {
        ++position_;
        SkipWhitespace();
        if (Consume(']')) {
            return true;
        }
        for (;;) {
            JsonValue child;
            if (!ParseValue(&child, depth, message)) {
                return false;
            }
            array->push_back(std::move(child));
            SkipWhitespace();
            if (Consume(']')) {
                return true;
            }
            if (!Consume(',')) {
                *message = "expected ',' in JSON array";
                return false;
            }
            SkipWhitespace();
        }
    }

    static void AppendUtf8(uint32_t codePoint, std::string* value) {
        if (codePoint <= 0x7fU) {
            value->push_back(static_cast<char>(codePoint));
        } else if (codePoint <= 0x7ffU) {
            value->push_back(static_cast<char>(0xc0U | (codePoint >> 6U)));
            value->push_back(static_cast<char>(0x80U | (codePoint & 0x3fU)));
        } else if (codePoint <= 0xffffU) {
            value->push_back(static_cast<char>(0xe0U | (codePoint >> 12U)));
            value->push_back(static_cast<char>(0x80U | ((codePoint >> 6U) & 0x3fU)));
            value->push_back(static_cast<char>(0x80U | (codePoint & 0x3fU)));
        } else {
            value->push_back(static_cast<char>(0xf0U | (codePoint >> 18U)));
            value->push_back(static_cast<char>(0x80U | ((codePoint >> 12U) & 0x3fU)));
            value->push_back(static_cast<char>(0x80U | ((codePoint >> 6U) & 0x3fU)));
            value->push_back(static_cast<char>(0x80U | (codePoint & 0x3fU)));
        }
    }

    bool ParseUnicodeEscape(uint32_t* codePoint, std::string* message) {
        if (position_ + 4 > text_.size()) {
            *message = "short JSON unicode escape";
            return false;
        }
        uint32_t parsed = 0;
        for (unsigned index = 0; index < 4; ++index) {
            const char digit = text_[position_++];
            parsed <<= 4U;
            if (digit >= '0' && digit <= '9') parsed |= static_cast<uint32_t>(digit - '0');
            else if (digit >= 'a' && digit <= 'f') parsed |= static_cast<uint32_t>(digit - 'a' + 10);
            else if (digit >= 'A' && digit <= 'F') parsed |= static_cast<uint32_t>(digit - 'A' + 10);
            else {
                *message = "invalid JSON unicode escape";
                return false;
            }
        }
        *codePoint = parsed;
        return true;
    }

    bool ParseString(std::string* value, std::string* message) {
        if (!Consume('"')) {
            *message = "expected JSON string";
            return false;
        }
        value->clear();
        while (position_ < text_.size()) {
            const unsigned char character = static_cast<unsigned char>(text_[position_++]);
            if (character == '"') {
                return true;
            }
            if (character < 0x20U) {
                *message = "control character in JSON string";
                return false;
            }
            if (character != '\\') {
                value->push_back(static_cast<char>(character));
                continue;
            }
            if (position_ >= text_.size()) {
                *message = "unterminated JSON escape";
                return false;
            }
            const char escaped = text_[position_++];
            switch (escaped) {
            case '"': value->push_back('"'); break;
            case '\\': value->push_back('\\'); break;
            case '/': value->push_back('/'); break;
            case 'b': value->push_back('\b'); break;
            case 'f': value->push_back('\f'); break;
            case 'n': value->push_back('\n'); break;
            case 'r': value->push_back('\r'); break;
            case 't': value->push_back('\t'); break;
            case 'u': {
                uint32_t codePoint = 0;
                if (!ParseUnicodeEscape(&codePoint, message)) {
                    return false;
                }
                if (codePoint >= 0xd800U && codePoint <= 0xdbffU) {
                    if (position_ + 2 > text_.size() || text_[position_] != '\\' ||
                        text_[position_ + 1] != 'u') {
                        *message = "high surrogate JSON escape lacks a low surrogate";
                        return false;
                    }
                    position_ += 2;
                    uint32_t low = 0;
                    if (!ParseUnicodeEscape(&low, message) ||
                        low < 0xdc00U || low > 0xdfffU) {
                        *message = "invalid low surrogate JSON escape";
                        return false;
                    }
                    codePoint = 0x10000U +
                        ((codePoint - 0xd800U) << 10U) + (low - 0xdc00U);
                } else if (codePoint >= 0xdc00U && codePoint <= 0xdfffU) {
                    *message = "unpaired low surrogate JSON escape";
                    return false;
                }
                AppendUtf8(codePoint, value);
                break;
            }
            default:
                *message = "invalid JSON escape";
                return false;
            }
        }
        *message = "unterminated JSON string";
        return false;
    }

    bool ParseInteger(JsonValue* value, std::string* message) {
        const size_t start = position_;
        bool negative = false;
        if (position_ < text_.size() && text_[position_] == '-') {
            negative = true;
            ++position_;
        }
        if (position_ >= text_.size() || text_[position_] < '0' || text_[position_] > '9') {
            *message = "expected JSON value";
            return false;
        }
        if (text_[position_] == '0' && position_ + 1 < text_.size() &&
            text_[position_ + 1] >= '0' && text_[position_ + 1] <= '9') {
            *message = "leading zero in JSON integer";
            return false;
        }
        uint64_t magnitude = 0;
        while (position_ < text_.size() && text_[position_] >= '0' && text_[position_] <= '9') {
            const uint64_t digit = static_cast<uint64_t>(text_[position_++] - '0');
            if (magnitude > (static_cast<uint64_t>(INT64_MAX) - digit) / 10U) {
                *message = "JSON integer out of range";
                return false;
            }
            magnitude = magnitude * 10U + digit;
        }
        if (position_ < text_.size() &&
            (text_[position_] == '.' || text_[position_] == 'e' || text_[position_] == 'E')) {
            *message = "non-integer JSON number is not permitted in install manifests";
            return false;
        }
        if (position_ == start) {
            *message = "expected JSON integer";
            return false;
        }
        const int64_t signedValue = negative ? -static_cast<int64_t>(magnitude) : static_cast<int64_t>(magnitude);
        value->value = signedValue;
        return true;
    }

    bool Match(std::string_view expected) {
        if (text_.substr(position_, expected.size()) != expected) {
            return false;
        }
        position_ += expected.size();
        return true;
    }

    bool Consume(char expected) {
        if (position_ >= text_.size() || text_[position_] != expected) {
            return false;
        }
        ++position_;
        return true;
    }

    std::string_view text_;
    size_t position_ = 0;
};

const JsonValue* ObjectField(const JsonValue::Object& object, const char* name) {
    const auto iterator = object.find(name);
    return iterator == object.end() ? nullptr : &iterator->second;
}

bool ReadSmallHandle(HANDLE file, std::string* contents, Error* error) {
    LARGE_INTEGER beginning{};
    if (!SetFilePointerEx(file, beginning, nullptr, FILE_BEGIN)) {
        return SetLastErrorDetail(error, L"manifest-seek");
    }
    LARGE_INTEGER size{};
    if (!GetFileSizeEx(file, &size)) {
        return SetLastErrorDetail(error, L"manifest-size");
    }
    if (size.QuadPart <= 0 || static_cast<uint64_t>(size.QuadPart) > kMaximumManifestBytes) {
        return SetError(error, L"manifest-size", ERROR_FILE_TOO_LARGE,
            L"manifest must be nonempty and no larger than one MiB");
    }
    contents->assign(static_cast<size_t>(size.QuadPart), '\0');
    DWORD read = 0;
    if (!ReadFile(file, contents->data(), static_cast<DWORD>(contents->size()), &read, nullptr) ||
        static_cast<size_t>(read) != contents->size()) {
        return SetLastErrorDetail(error, L"manifest-read");
    }
    if (contents->size() >= 3 &&
        static_cast<unsigned char>((*contents)[0]) == 0xefU &&
        static_cast<unsigned char>((*contents)[1]) == 0xbbU &&
        static_cast<unsigned char>((*contents)[2]) == 0xbfU) {
        contents->erase(0, 3);
    }
    return true;
}

bool Sha256Handle(HANDLE file, std::string* digest, Error* error) {
    HCRYPTPROV provider = 0;
    HCRYPTHASH hash = 0;
    if (!CryptAcquireContextW(&provider, nullptr, nullptr, PROV_RSA_AES, CRYPT_VERIFYCONTEXT)) {
        return SetLastErrorDetail(error, L"sha256-provider");
    }
    const auto releaseProvider = [&]() { CryptReleaseContext(provider, 0); };
    if (!CryptCreateHash(provider, CALG_SHA_256, 0, 0, &hash)) {
        const DWORD code = GetLastError();
        releaseProvider();
        return SetError(error, L"sha256-create", code);
    }
    LARGE_INTEGER beginning{};
    if (!SetFilePointerEx(file, beginning, nullptr, FILE_BEGIN)) {
        const DWORD code = GetLastError();
        CryptDestroyHash(hash);
        releaseProvider();
        return SetError(error, L"sha256-seek", code);
    }
    std::array<BYTE, 64 * 1024> buffer{};
    for (;;) {
        DWORD read = 0;
        if (!ReadFile(file, buffer.data(), static_cast<DWORD>(buffer.size()), &read, nullptr)) {
            const DWORD code = GetLastError();
            CryptDestroyHash(hash);
            releaseProvider();
            return SetError(error, L"sha256-read", code);
        }
        if (read == 0) {
            break;
        }
        if (!CryptHashData(hash, buffer.data(), read, 0)) {
            const DWORD code = GetLastError();
            CryptDestroyHash(hash);
            releaseProvider();
            return SetError(error, L"sha256-update", code);
        }
    }
    std::array<BYTE, 32> bytes{};
    DWORD length = static_cast<DWORD>(bytes.size());
    if (!CryptGetHashParam(hash, HP_HASHVAL, bytes.data(), &length, 0) || length != bytes.size()) {
        const DWORD code = GetLastError();
        CryptDestroyHash(hash);
        releaseProvider();
        return SetError(error, L"sha256-finish", code);
    }
    CryptDestroyHash(hash);
    releaseProvider();
    static constexpr char digits[] = "0123456789ABCDEF";
    digest->clear();
    digest->reserve(bytes.size() * 2);
    for (BYTE byte : bytes) {
        digest->push_back(digits[byte >> 4U]);
        digest->push_back(digits[byte & 0x0fU]);
    }
    return true;
}

bool Sha256File(const std::filesystem::path& path, std::string* digest, Error* error) {
    WinHandle file(CreateFileW(path.c_str(), GENERIC_READ, FILE_SHARE_READ, nullptr,
        OPEN_EXISTING, FILE_ATTRIBUTE_NORMAL | FILE_FLAG_SEQUENTIAL_SCAN, nullptr));
    if (!file) {
        return SetLastErrorDetail(error, L"sha256-open");
    }
    return Sha256Handle(file.get(), digest, error);
}

bool Sha256Data(std::string_view data, std::string* digest, Error* error) {
    HCRYPTPROV provider = 0;
    HCRYPTHASH hash = 0;
    if (!CryptAcquireContextW(&provider, nullptr, nullptr, PROV_RSA_AES, CRYPT_VERIFYCONTEXT)) {
        return SetLastErrorDetail(error, L"sha256-data-provider");
    }
    const auto releaseProvider = [&]() { CryptReleaseContext(provider, 0); };
    if (!CryptCreateHash(provider, CALG_SHA_256, 0, 0, &hash)) {
        const DWORD code = GetLastError();
        releaseProvider();
        return SetError(error, L"sha256-data-create", code);
    }
    const bool updated = data.size() <= MAXDWORD && CryptHashData(hash,
        reinterpret_cast<const BYTE*>(data.data()), static_cast<DWORD>(data.size()), 0) != FALSE;
    if (!updated) {
        const DWORD code = data.size() > MAXDWORD ? ERROR_FILE_TOO_LARGE : GetLastError();
        CryptDestroyHash(hash);
        releaseProvider();
        return SetError(error, L"sha256-data-update", code);
    }
    std::array<BYTE, VIIPER_UDE_BUILD_IDENTITY_BYTES> bytes{};
    DWORD length = static_cast<DWORD>(bytes.size());
    if (!CryptGetHashParam(hash, HP_HASHVAL, bytes.data(), &length, 0)) {
        const DWORD code = GetLastError();
        CryptDestroyHash(hash);
        releaseProvider();
        return SetError(error, L"sha256-data-finish", code);
    }
    if (length != bytes.size()) {
        CryptDestroyHash(hash);
        releaseProvider();
        return SetError(error, L"sha256-data-finish", ERROR_INVALID_DATA);
    }
    CryptDestroyHash(hash);
    releaseProvider();
    static constexpr char digits[] = "0123456789abcdef";
    digest->clear();
    digest->reserve(bytes.size() * 2);
    for (BYTE byte : bytes) {
        digest->push_back(digits[byte >> 4U]);
        digest->push_back(digits[byte & 0x0fU]);
    }
    return true;
}

bool DeriveDriverBuildIdentity(
    const std::string& sourceRevision,
    std::string* digest,
    Error* error) {
    if (!IsHexRevision(sourceRevision)) {
        return SetError(error, L"build-identity-source", ERROR_INVALID_DATA,
            L"driver build identity requires an exact 40- or 64-digit source revision");
    }
    std::ostringstream preimage;
    preimage << "VIIPER-UDE-BUILD-IDENTITY/v1\n"
        << "sourceRevision=" << LowerAscii(sourceRevision) << "\n"
        << "driverPackageVersion=" << VIIPER_UDE_DRIVER_PACKAGE_VERSION << "\n"
        << "abi=" << VIIPER_UDE_ABI_MAJOR << "." << VIIPER_UDE_ABI_MINOR << "\n"
        << "capabilities=0x" << std::hex << std::nouppercase << std::setw(8)
        << std::setfill('0') << VIIPER_UDE_ADVERTISED_CAPABILITIES << "\n";
    return Sha256Data(preimage.str(), digest, error);
}

bool FileLength(const std::filesystem::path& path, uint64_t* length, Error* error) {
    std::error_code fileError;
    const uintmax_t size = std::filesystem::file_size(path, fileError);
    if (fileError) {
        return SetError(error, L"manifest-file-size", static_cast<DWORD>(fileError.value()));
    }
    *length = static_cast<uint64_t>(size);
    return true;
}

bool VerifyLocalTestPackageSigner(
    const std::filesystem::path& infPath,
    std::string_view expectedCertificateSha256,
    Error* error);

bool ValidateManifest(
    const std::string& rawManifest,
    const std::string& expectedRevision,
    bool production,
    bool localTest,
    const std::filesystem::path& packageDirectory,
    Error* error) {
    std::string raw = rawManifest;
    if (raw.size() >= 3 && static_cast<unsigned char>(raw[0]) == 0xefU &&
        static_cast<unsigned char>(raw[1]) == 0xbbU &&
        static_cast<unsigned char>(raw[2]) == 0xbfU) {
        raw.erase(0, 3);
    }
    JsonValue root;
    std::string parseMessage;
    if (!JsonParser(raw).Parse(&root, &parseMessage)) {
        return SetError(error, L"manifest-parse", ERROR_INVALID_DATA,
            std::wstring(parseMessage.begin(), parseMessage.end()));
    }
    const auto* object = std::get_if<JsonValue::Object>(&root.value);
    if (object == nullptr) {
        return SetError(error, L"manifest-contract", ERROR_INVALID_DATA, L"manifest root must be an object");
    }
    const JsonValue* schema = ObjectField(*object, "schema");
    const JsonValue* revision = ObjectField(*object, "sourceRevision");
    const JsonValue* releaseEligible = ObjectField(*object, "releaseEligible");
    const JsonValue* signingRoute = ObjectField(*object, "signingRoute");
    const JsonValue* testSignerCertificateSha256 =
        ObjectField(*object, "testSignerCertificateSha256");
    const JsonValue* driverVersion = ObjectField(*object, "driverPackageVersion");
    const JsonValue* driverMajor = ObjectField(*object, "driverABIMajor");
    const JsonValue* driverMinor = ObjectField(*object, "driverABIMinor");
    const JsonValue* driverCapabilities = ObjectField(*object, "driverCapabilities");
    const JsonValue* driverBuildIdentity = ObjectField(*object, "driverBuildIdentity");
    const JsonValue* files = ObjectField(*object, "files");
    const auto* schemaValue = schema == nullptr ? nullptr : std::get_if<int64_t>(&schema->value);
    const auto* revisionValue = revision == nullptr ? nullptr : std::get_if<std::string>(&revision->value);
    const auto* releaseValue = releaseEligible == nullptr ? nullptr : std::get_if<bool>(&releaseEligible->value);
    const auto* routeValue = signingRoute == nullptr ? nullptr : std::get_if<std::string>(&signingRoute->value);
    const auto* testSignerCertificateSha256Value = testSignerCertificateSha256 == nullptr ?
        nullptr : std::get_if<std::string>(&testSignerCertificateSha256->value);
    const auto* driverVersionValue = driverVersion == nullptr ? nullptr : std::get_if<std::string>(&driverVersion->value);
    const auto* driverMajorValue = driverMajor == nullptr ? nullptr : std::get_if<int64_t>(&driverMajor->value);
    const auto* driverMinorValue = driverMinor == nullptr ? nullptr : std::get_if<int64_t>(&driverMinor->value);
    const auto* driverCapabilitiesValue = driverCapabilities == nullptr ? nullptr : std::get_if<std::string>(&driverCapabilities->value);
    const auto* driverBuildIdentityValue = driverBuildIdentity == nullptr ? nullptr : std::get_if<std::string>(&driverBuildIdentity->value);
    const auto* fileArray = files == nullptr ? nullptr : std::get_if<JsonValue::Array>(&files->value);
    std::string expectedBuildIdentity;
    if (!DeriveDriverBuildIdentity(expectedRevision, &expectedBuildIdentity, error)) {
        return false;
    }
    std::ostringstream expectedCapabilities;
    expectedCapabilities << "0x" << std::hex << std::nouppercase << std::setw(8)
        << std::setfill('0') << VIIPER_UDE_ADVERTISED_CAPABILITIES;
    if (schemaValue == nullptr || *schemaValue != 2 || revisionValue == nullptr ||
        LowerAscii(*revisionValue) != LowerAscii(expectedRevision) || releaseValue == nullptr ||
        routeValue == nullptr || fileArray == nullptr || driverVersionValue == nullptr ||
        *driverVersionValue != VIIPER_UDE_DRIVER_PACKAGE_VERSION || driverMajorValue == nullptr ||
        *driverMajorValue != VIIPER_UDE_ABI_MAJOR || driverMinorValue == nullptr ||
        *driverMinorValue != VIIPER_UDE_ABI_MINOR || driverCapabilitiesValue == nullptr ||
        *driverCapabilitiesValue != expectedCapabilities.str() || driverBuildIdentityValue == nullptr ||
        *driverBuildIdentityValue != expectedBuildIdentity) {
        return SetError(error, L"manifest-contract", ERROR_INVALID_DATA,
            L"manifest schema, source revision, loaded-driver identity, release route, or file list is invalid");
    }
    if (production) {
        if (!*releaseValue || *routeValue != "HLK/WHCP") {
            return SetError(error, L"manifest-release-route", ERROR_INVALID_DATA,
                L"production installation requires a release-eligible HLK/WHCP manifest");
        }
    } else if (localTest) {
        const bool signerDigestValid = testSignerCertificateSha256Value != nullptr &&
            testSignerCertificateSha256Value->size() == 64 &&
            std::all_of(testSignerCertificateSha256Value->begin(),
                testSignerCertificateSha256Value->end(), [](char value) {
                    return (value >= '0' && value <= '9') ||
                        (value >= 'a' && value <= 'f');
                });
        if (*releaseValue || *routeValue != "LocalTest" || !signerDigestValid) {
            return SetError(error, L"manifest-release-route", ERROR_INVALID_DATA,
                L"local test installation requires its explicit non-release LocalTest manifest and signer digest");
        }
        if (!VerifyLocalTestPackageSigner(
                packageDirectory / L"ViiperUde.inf",
                *testSignerCertificateSha256Value,
                error)) {
            return false;
        }
    } else if (*releaseValue || *routeValue != "ControlledTestAttestation") {
        return SetError(error, L"manifest-release-route", ERROR_INVALID_DATA,
            L"controlled-test installation requires its testing-only attestation manifest");
    }
    const std::set<std::string> expectedNames = {
        "ViiperUde.inf", "ViiperUde.sys", "ViiperUde.pdb", "ViiperUde.cat"};
    if (fileArray->size() != expectedNames.size()) {
        return SetError(error, L"manifest-files", ERROR_INVALID_DATA,
            L"manifest must describe exactly the four VIIPER package files");
    }
    std::set<std::string> seen;
    for (const JsonValue& entry : *fileArray) {
        const auto* entryObject = std::get_if<JsonValue::Object>(&entry.value);
        if (entryObject == nullptr) {
            return SetError(error, L"manifest-files", ERROR_INVALID_DATA, L"manifest file entry is not an object");
        }
        const JsonValue* nameNode = ObjectField(*entryObject, "name");
        const JsonValue* lengthNode = ObjectField(*entryObject, "length");
        const JsonValue* hashNode = ObjectField(*entryObject, "sha256");
        const auto* name = nameNode == nullptr ? nullptr : std::get_if<std::string>(&nameNode->value);
        const auto* length = lengthNode == nullptr ? nullptr : std::get_if<int64_t>(&lengthNode->value);
        const auto* hash = hashNode == nullptr ? nullptr : std::get_if<std::string>(&hashNode->value);
        if (name == nullptr || length == nullptr || *length < 0 || hash == nullptr ||
            !expectedNames.contains(*name) || !seen.insert(*name).second) {
            return SetError(error, L"manifest-files", ERROR_INVALID_DATA,
                L"manifest has an unexpected, duplicate, or malformed file entry");
        }
        // Production intake binds both INF and PDB to this manifest. The
        // installer pins that validated manifest hash, but the public runtime
        // package deliberately omits the PDB because Windows needs only
        // INF/SYS/CAT. Recheck the unchanged INF here; retaining the PDB entry
        // proves this is the exact intake manifest, not a weaker replacement.
        if (*name == "ViiperUde.inf") {
            const std::filesystem::path filePath = packageDirectory / std::wstring(name->begin(), name->end());
            uint64_t actualLength = 0;
            std::string actualHash;
            if (!FileLength(filePath, &actualLength, error) || !Sha256File(filePath, &actualHash, error)) {
                return false;
            }
            if (actualLength != static_cast<uint64_t>(*length) ||
                LowerAscii(actualHash) != LowerAscii(*hash)) {
                return SetError(error, L"manifest-hash", ERROR_CRC,
                    L"INF does not match the source-bound submission manifest");
            }
        }
    }
    return seen == expectedNames;
}

bool GetInfField(
    HINF inf,
    const wchar_t* section,
    const wchar_t* key,
    DWORD field,
    std::wstring* value,
    Error* error) {
    INFCONTEXT context{};
    if (!SetupFindFirstLineW(inf, section, key, &context)) {
        return SetLastErrorDetail(error, L"inf-contract-line");
    }
    DWORD required = 0;
    if (!SetupGetStringFieldW(&context, field, nullptr, 0, &required) ||
        required == 0) {
        return SetLastErrorDetail(error, L"inf-contract-field");
    }
    std::vector<wchar_t> buffer(required);
    if (!SetupGetStringFieldW(&context, field, buffer.data(), required, nullptr)) {
        return SetLastErrorDetail(error, L"inf-contract-field");
    }
    *value = buffer.data();
    return true;
}

bool ValidateSingleModelLine(HINF inf, Error* error) {
    INFCONTEXT context{};
    if (!SetupFindFirstLineW(inf, kModelSection, nullptr, &context)) {
        return SetLastErrorDetail(error, L"inf-model");
    }
    std::wstring install;
    std::wstring hardware;
    if (SetupGetFieldCount(&context) != 2) {
        return SetError(error, L"inf-model", ERROR_INVALID_DATA,
            L"VIIPER model entry must contain only install section and exact hardware ID");
    }
    DWORD required = 0;
    SetupGetStringFieldW(&context, 1, nullptr, 0, &required);
    std::vector<wchar_t> installBuffer(required);
    if (required == 0 || !SetupGetStringFieldW(&context, 1, installBuffer.data(), required, nullptr)) {
        return SetLastErrorDetail(error, L"inf-model-install");
    }
    install = installBuffer.data();
    required = 0;
    SetupGetStringFieldW(&context, 2, nullptr, 0, &required);
    std::vector<wchar_t> hardwareBuffer(required);
    if (required == 0 || !SetupGetStringFieldW(&context, 2, hardwareBuffer.data(), required, nullptr)) {
        return SetLastErrorDetail(error, L"inf-model-hardware-id");
    }
    hardware = hardwareBuffer.data();
    INFCONTEXT next{};
    if (_wcsicmp(install.c_str(), kInstallSection) != 0 ||
        _wcsicmp(hardware.c_str(), kHardwareId) != 0 ||
        SetupFindNextLine(&context, &next)) {
        return SetError(error, L"inf-model", ERROR_INVALID_DATA,
            L"INF must contain exactly one VIIPER root model entry");
    }
    return true;
}

struct PackageInfo {
    std::filesystem::path infPath;
    std::wstring publishedName;
    Version version{};
    std::string infSha256;
    std::string sysSha256;
    std::string catSha256;
};

bool SamePackageBytes(const PackageInfo& left, const PackageInfo& right) {
    return left.infSha256 == right.infSha256 &&
        left.sysSha256 == right.sysSha256 &&
        left.catSha256 == right.catSha256;
}

std::string PackageBytesKey(const PackageInfo& package) {
    return package.infSha256 + ":" + package.sysSha256 + ":" + package.catSha256;
}

bool GetDriverStoreInfPath(
    const std::filesystem::path& publishedPath,
    std::filesystem::path* storePath,
    Error* error);

bool InspectInfContract(
    const std::filesystem::path& infPath,
    bool* owned,
    Version* version,
    Error* error) {
    *owned = false;
    UINT errorLine = 0;
    InfHandle inf(SetupOpenInfFileW(infPath.c_str(), nullptr, INF_STYLE_WIN4, &errorLine));
    if (!inf) {
        return true;
    }
    GUID classGuid{};
    wchar_t className[MAX_CLASS_NAME_LEN]{};
    if (!SetupDiGetINFClassW(infPath.c_str(), &classGuid, className, MAX_CLASS_NAME_LEN, nullptr) ||
        !IsEqualGUID(classGuid, GUID_DEVCLASS_USB)) {
        return true;
    }
    std::wstring provider;
    std::wstring catalog;
    std::wstring driverVersion;
    std::wstring pnpLockdown;
    std::wstring copyFile;
    std::wstring sourceDisk;
    std::wstring service;
    Error local;
    if (!GetInfField(inf.get(), L"Version", L"Provider", 1, &provider, &local) ||
        !GetInfField(inf.get(), L"Version", L"CatalogFile", 1, &catalog, &local) ||
        !GetInfField(inf.get(), L"Version", L"DriverVer", 2, &driverVersion, &local) ||
        !GetInfField(inf.get(), L"Version", L"PnpLockDown", 1, &pnpLockdown, &local) ||
        !GetInfField(inf.get(), L"ViiperUde_Install.NT", L"CopyFiles", 1, &copyFile, &local) ||
        !GetInfField(inf.get(), L"SourceDisksFiles", kDriverFileName, 1, &sourceDisk, &local) ||
        !GetInfField(inf.get(), L"ViiperUde_Install.NT.Services", L"AddService", 1, &service, &local)) {
        return true;
    }
    if (_wcsicmp(provider.c_str(), kProviderName) != 0 ||
        _wcsicmp(catalog.c_str(), kCatalogName) != 0 ||
        pnpLockdown != L"1" || _wcsicmp(copyFile.c_str(), L"@ViiperUde.sys") != 0 ||
        sourceDisk != L"1" || _wcsicmp(service.c_str(), kServiceName) != 0) {
        return true;
    }
    Version parsed{};
    if (!ParseVersion(driverVersion, &parsed)) {
        return SetError(error, L"inf-version", ERROR_INVALID_DATA,
            L"VIIPER DriverVer must contain a four-component numeric version");
    }
    if (!ValidateSingleModelLine(inf.get(), error)) {
        return false;
    }
    *owned = true;
    *version = parsed;
    return true;
}

bool VerifyInfSignature(
    const std::filesystem::path& infPath,
    std::filesystem::path* catalogPath,
    Error* error) {
    SP_INF_SIGNER_INFO_W signer{};
    signer.cbSize = sizeof(signer);
    if (!SetupVerifyInfFileW(infPath.c_str(), nullptr, &signer)) {
        const DWORD code = GetLastError();
        // SetupAPI reports a valid non-WHQL Authenticode package by returning
        // FALSE with this classification. The local-test route has already
        // installed the exact source-bound signer into TrustedPublisher; its
        // manifest validation below still proves that exact certificate and
        // the INF/SYS membership in the exact catalog. Production separately
        // requires the Microsoft hardware publisher and never relies on this.
        if (code != ERROR_AUTHENTICODE_TRUSTED_PUBLISHER) {
            return SetError(error, L"inf-signature", code);
        }
    }
    if (signer.CatalogFile[0] == L'\0' || signer.DigitalSigner[0] == L'\0') {
        return SetError(error, L"inf-signature", ERROR_INVALID_DATA,
            L"signed INF did not report a catalog and signer");
    }
    if (catalogPath != nullptr) {
        *catalogPath = signer.CatalogFile;
    }
    return true;
}

bool IsProductionHardwareVerificationUsage(const std::vector<std::string_view>& usages) {
    const bool hardware = std::find(usages.begin(), usages.end(),
        kHardwareVerificationOid) != usages.end();
    const bool attestation = std::find(usages.begin(), usages.end(),
        kAttestationVerificationOid) != usages.end();
    return hardware && !attestation;
}

template <typename Function>
bool LoadWinTrustFunction(
    HMODULE module,
    const char* name,
    Function* function,
    Error* error) {
    const FARPROC address = GetProcAddress(module, name);
    if (address == nullptr) {
        return SetLastErrorDetail(error, L"catalog-policy-api",
            L"required Windows catalog policy API is unavailable");
    }
    static_assert(sizeof(address) == sizeof(*function));
    std::memcpy(function, &address, sizeof(address));
    return true;
}

bool VerifyDriverCatalogMember(
    const std::filesystem::path& catalogPath,
    const std::filesystem::path& memberPath,
    bool allowUntrustedLocalTestRoot,
    Error* error) {
    WinHandle member(CreateFileW(memberPath.c_str(), GENERIC_READ, FILE_SHARE_READ, nullptr,
        OPEN_EXISTING, FILE_ATTRIBUTE_NORMAL | FILE_FLAG_OPEN_REPARSE_POINT, nullptr));
    if (!member) {
        return SetLastErrorDetail(error, L"catalog-member-open");
    }
    FILE_ATTRIBUTE_TAG_INFO attributes{};
    if (!GetFileInformationByHandleEx(member.get(), FileAttributeTagInfo,
            &attributes, sizeof(attributes)) ||
        (attributes.FileAttributes &
            (FILE_ATTRIBUTE_DIRECTORY | FILE_ATTRIBUTE_REPARSE_POINT)) != 0) {
        return SetError(error, L"catalog-member-open", ERROR_REPARSE_TAG_MISMATCH,
            L"catalog member must be a regular non-reparse file");
    }
    using AcquireContext2 = BOOL (WINAPI*)(
        HCATADMIN*, const GUID*, PCWSTR, PCCERT_STRONG_SIGN_PARA, DWORD);
    using CalculateHash2 = BOOL (WINAPI*)(HCATADMIN, HANDLE, DWORD*, BYTE*, DWORD);
    using ReleaseContext = BOOL (WINAPI*)(HCATADMIN, DWORD);
    HMODULE winTrust = LoadLibraryExW(
        L"wintrust.dll", nullptr, LOAD_LIBRARY_SEARCH_SYSTEM32);
    if (winTrust == nullptr) {
        return SetLastErrorDetail(error, L"catalog-policy-library");
    }
    AcquireContext2 acquireContext = nullptr;
    CalculateHash2 calculateHash = nullptr;
    ReleaseContext releaseContext = nullptr;
    if (!LoadWinTrustFunction(winTrust, "CryptCATAdminAcquireContext2",
            &acquireContext, error) ||
        !LoadWinTrustFunction(winTrust, "CryptCATAdminCalcHashFromFileHandle2",
            &calculateHash, error) ||
        !LoadWinTrustFunction(winTrust, "CryptCATAdminReleaseContext",
            &releaseContext, error)) {
        FreeLibrary(winTrust);
        return false;
    }
    HCATADMIN administrator = nullptr;
    // Let Windows select the catalog's approved hash algorithm. Microsoft
    // explicitly recommends this over hard-coding an algorithm that policy may
    // retire; the returned context is also supplied to WinVerifyTrust below.
    if (!acquireContext(&administrator, nullptr, nullptr, nullptr, 0)) {
        const DWORD code = GetLastError();
        FreeLibrary(winTrust);
        return SetError(error, L"catalog-admin", code);
    }
    const auto releasePolicy = [&]() {
        releaseContext(administrator, 0);
        FreeLibrary(winTrust);
    };
    DWORD hashSize = 0;
    if (!calculateHash(
            administrator, member.get(), &hashSize, nullptr, 0) || hashSize == 0) {
        const DWORD code = GetLastError();
        releasePolicy();
        return SetError(error, L"catalog-member-hash", code);
    }
    std::vector<BYTE> hash(hashSize);
    if (!calculateHash(
            administrator, member.get(), &hashSize, hash.data(), 0)) {
        const DWORD code = GetLastError();
        releasePolicy();
        return SetError(error, L"catalog-member-hash", code);
    }
    hash.resize(hashSize);
    static constexpr wchar_t digits[] = L"0123456789ABCDEF";
    std::wstring memberTag;
    memberTag.reserve(hash.size() * 2);
    for (BYTE value : hash) {
        memberTag.push_back(digits[value >> 4U]);
        memberTag.push_back(digits[value & 0x0fU]);
    }
    WINTRUST_CATALOG_INFO catalog{};
    catalog.cbStruct = sizeof(catalog);
    catalog.pcwszCatalogFilePath = catalogPath.c_str();
    catalog.pcwszMemberTag = memberTag.c_str();
    catalog.pcwszMemberFilePath = memberPath.c_str();
    catalog.hMemberFile = member.get();
    catalog.pbCalculatedFileHash = hash.data();
    catalog.cbCalculatedFileHash = static_cast<DWORD>(hash.size());
    catalog.hCatAdmin = administrator;
    WINTRUST_DATA trust{};
    trust.cbStruct = sizeof(trust);
    trust.dwUIChoice = WTD_UI_NONE;
    trust.fdwRevocationChecks = WTD_REVOKE_NONE;
    trust.dwUnionChoice = WTD_CHOICE_CATALOG;
    trust.pCatalog = &catalog;
    trust.dwStateAction = WTD_STATEACTION_VERIFY;
    trust.dwProvFlags = WTD_CACHE_ONLY_URL_RETRIEVAL;
    // This operation proves that the exact member hash is present in the
    // supplied, Authenticode-trusted catalog. Microsoft documents
    // DRIVER_ACTION_VERIFY as the WHQL-specific add-on policy; using it here
    // incorrectly rejects a valid test-signed package before deployment.
    // Production hardware-publisher policy is enforced separately by
    // VerifyMicrosoftHardwareInfSigner.
    GUID action = WINTRUST_ACTION_GENERIC_VERIFY_V2;
    const LONG status = WinVerifyTrust(reinterpret_cast<HWND>(INVALID_HANDLE_VALUE), &action, &trust);
    trust.dwStateAction = WTD_STATEACTION_CLOSE;
    WinVerifyTrust(reinterpret_cast<HWND>(INVALID_HANDLE_VALUE), &action, &trust);
    releasePolicy();
    if (status != ERROR_SUCCESS &&
        !(allowUntrustedLocalTestRoot &&
            status == static_cast<LONG>(CERT_E_UNTRUSTEDROOT))) {
        return SetError(error, L"catalog-member-policy", static_cast<DWORD>(status),
            L"package file is not a valid member of the exact trusted driver catalog");
    }
    return true;
}

bool VerifyLocalTestPackageSigner(
    const std::filesystem::path& infPath,
    std::string_view expectedCertificateSha256,
    Error* error) {
    // InspectInfContract already pins CatalogFile to kCatalogName. Verify the
    // exact member hashes and signer directly so a clean packaging machine
    // does not need to trust the disposable WDK certificate first.
    const std::filesystem::path catalogPath = infPath.parent_path() / kCatalogName;
    if (!VerifyDriverCatalogMember(catalogPath, infPath, true, error) ||
        !VerifyDriverCatalogMember(catalogPath,
            infPath.parent_path() / kDriverFileName, true, error)) {
        return false;
    }

    DWORD encoding = 0;
    HCERTSTORE store = nullptr;
    HCRYPTMSG message = nullptr;
    if (!CryptQueryObject(CERT_QUERY_OBJECT_FILE, catalogPath.c_str(),
            CERT_QUERY_CONTENT_FLAG_PKCS7_SIGNED | CERT_QUERY_CONTENT_FLAG_PKCS7_SIGNED_EMBED,
            CERT_QUERY_FORMAT_FLAG_BINARY, 0, &encoding, nullptr, nullptr,
            &store, &message, nullptr)) {
        return SetLastErrorDetail(error, L"local-test-catalog-signature-open");
    }
    const auto closeCatalog = [&]() {
        if (message != nullptr) {
            CryptMsgClose(message);
        }
        if (store != nullptr) {
            CertCloseStore(store, 0);
        }
    };
    DWORD signerSize = 0;
    if (!CryptMsgGetParam(message, CMSG_SIGNER_INFO_PARAM, 0, nullptr, &signerSize) ||
        signerSize < sizeof(CMSG_SIGNER_INFO)) {
        const DWORD code = GetLastError();
        closeCatalog();
        return SetError(error, L"local-test-catalog-signer-info", code);
    }
    std::vector<BYTE> signerBytes(signerSize);
    if (!CryptMsgGetParam(message, CMSG_SIGNER_INFO_PARAM, 0,
            signerBytes.data(), &signerSize)) {
        const DWORD code = GetLastError();
        closeCatalog();
        return SetError(error, L"local-test-catalog-signer-info", code);
    }
    const auto* signerInfo = reinterpret_cast<const CMSG_SIGNER_INFO*>(signerBytes.data());
    CERT_INFO certificateIdentity{};
    certificateIdentity.Issuer = signerInfo->Issuer;
    certificateIdentity.SerialNumber = signerInfo->SerialNumber;
    PCCERT_CONTEXT certificate = CertFindCertificateInStore(store, encoding, 0,
        CERT_FIND_SUBJECT_CERT, &certificateIdentity, nullptr);
    if (certificate == nullptr) {
        const DWORD code = GetLastError();
        closeCatalog();
        return SetError(error, L"local-test-catalog-signer-certificate", code);
    }
    std::string actualCertificateSha256;
    const std::string_view encodedCertificate(
        reinterpret_cast<const char*>(certificate->pbCertEncoded),
        certificate->cbCertEncoded);
    const bool hashed = Sha256Data(
        encodedCertificate, &actualCertificateSha256, error);
    CertFreeCertificateContext(certificate);
    closeCatalog();
    if (!hashed) {
        return false;
    }
    if (actualCertificateSha256 != expectedCertificateSha256) {
        return SetError(error, L"local-test-catalog-signer-certificate",
            ERROR_CRC,
            L"local test catalog signer does not match the source-bound manifest digest");
    }
    return true;
}

bool VerifyMicrosoftHardwareInfSigner(
    const std::filesystem::path& infPath,
    Error* error) {
    SP_INF_SIGNER_INFO_W signer{};
    signer.cbSize = sizeof(signer);
    if (!SetupVerifyInfFileW(infPath.c_str(), nullptr, &signer)) {
        return SetLastErrorDetail(error, L"inf-microsoft-signature");
    }
    if (_wcsicmp(signer.DigitalSigner,
            L"Microsoft Windows Hardware Compatibility Publisher") != 0) {
        return SetError(error, L"inf-microsoft-signature", ERROR_INVALID_DATA,
            L"driver catalog signer is not Microsoft Windows Hardware Compatibility Publisher");
    }

    std::filesystem::path packageInfPath = infPath;
    if (_wcsicmp(infPath.filename().c_str(), L"ViiperUde.inf") != 0 &&
        !GetDriverStoreInfPath(infPath, &packageInfPath, error)) {
        return false;
    }
    std::filesystem::path catalogPath = signer.CatalogFile;
    if (catalogPath.is_relative()) {
        catalogPath = packageInfPath.parent_path() / catalogPath.filename();
    }

    DWORD encoding = 0;
    HCERTSTORE store = nullptr;
    HCRYPTMSG message = nullptr;
    if (!VerifyDriverCatalogMember(catalogPath, infPath, false, error) ||
        !VerifyDriverCatalogMember(catalogPath,
            packageInfPath.parent_path() / kDriverFileName, false, error)) {
        return false;
    }
    if (!CryptQueryObject(CERT_QUERY_OBJECT_FILE, catalogPath.c_str(),
            CERT_QUERY_CONTENT_FLAG_PKCS7_SIGNED | CERT_QUERY_CONTENT_FLAG_PKCS7_SIGNED_EMBED,
            CERT_QUERY_FORMAT_FLAG_BINARY, 0, &encoding, nullptr, nullptr,
            &store, &message, nullptr)) {
        return SetLastErrorDetail(error, L"catalog-signature-open");
    }
    const auto closeCatalog = [&]() {
        if (message != nullptr) {
            CryptMsgClose(message);
        }
        if (store != nullptr) {
            CertCloseStore(store, 0);
        }
    };
    DWORD signerSize = 0;
    if (!CryptMsgGetParam(message, CMSG_SIGNER_INFO_PARAM, 0, nullptr, &signerSize) ||
        signerSize < sizeof(CMSG_SIGNER_INFO)) {
        const DWORD code = GetLastError();
        closeCatalog();
        return SetError(error, L"catalog-signer-info", code);
    }
    std::vector<BYTE> signerBytes(signerSize);
    if (!CryptMsgGetParam(message, CMSG_SIGNER_INFO_PARAM, 0,
            signerBytes.data(), &signerSize)) {
        const DWORD code = GetLastError();
        closeCatalog();
        return SetError(error, L"catalog-signer-info", code);
    }
    const auto* signerInfo = reinterpret_cast<const CMSG_SIGNER_INFO*>(signerBytes.data());
    CERT_INFO certificateIdentity{};
    certificateIdentity.Issuer = signerInfo->Issuer;
    certificateIdentity.SerialNumber = signerInfo->SerialNumber;
    PCCERT_CONTEXT certificate = CertFindCertificateInStore(store, encoding, 0,
        CERT_FIND_SUBJECT_CERT, &certificateIdentity, nullptr);
    if (certificate == nullptr) {
        const DWORD code = GetLastError();
        closeCatalog();
        return SetError(error, L"catalog-signer-certificate", code);
    }
    DWORD usageSize = 0;
    if (!CertGetEnhancedKeyUsage(certificate, CERT_FIND_EXT_ONLY_ENHKEY_USAGE_FLAG,
            nullptr, &usageSize) ||
        usageSize < sizeof(CERT_ENHKEY_USAGE)) {
        const DWORD code = GetLastError();
        CertFreeCertificateContext(certificate);
        closeCatalog();
        return SetError(error, L"catalog-signer-eku", code,
            L"production catalog signer must declare Windows Hardware Driver Verification EKU");
    }
    std::vector<BYTE> usageBytes(usageSize);
    auto* usage = reinterpret_cast<CERT_ENHKEY_USAGE*>(usageBytes.data());
    if (!CertGetEnhancedKeyUsage(certificate, CERT_FIND_EXT_ONLY_ENHKEY_USAGE_FLAG,
            usage, &usageSize)) {
        const DWORD code = GetLastError();
        CertFreeCertificateContext(certificate);
        closeCatalog();
        return SetError(error, L"catalog-signer-eku", code);
    }
    std::vector<std::string_view> usages;
    usages.reserve(usage->cUsageIdentifier);
    for (DWORD index = 0; index < usage->cUsageIdentifier; ++index) {
        const char* oid = usage->rgpszUsageIdentifier[index];
        if (oid != nullptr) {
            usages.emplace_back(oid);
        }
    }
    const bool productionUsage = IsProductionHardwareVerificationUsage(usages);
    CertFreeCertificateContext(certificate);
    closeCatalog();
    if (!productionUsage) {
        return SetError(error, L"catalog-signer-eku", ERROR_INVALID_DATA,
            L"production requires HLK/WHCP hardware verification and rejects attestation EKU");
    }
    return true;
}

bool LoadOwnedPackage(
    const std::filesystem::path& rawPath,
    bool requireOwned,
    bool allowUntrustedLocalTestRoot,
    PackageInfo* package,
    bool* owned,
    Error* error) {
    std::error_code pathError;
    const std::filesystem::path path = std::filesystem::canonical(rawPath, pathError);
    const bool regular = !pathError && std::filesystem::is_regular_file(path, pathError);
    if (pathError || !regular) {
        if (requireOwned) {
            return SetError(error, L"package-path", ERROR_FILE_NOT_FOUND);
        }
        *owned = false;
        return true;
    }
    Version version{};
    bool exact = false;
    if (!InspectInfContract(path, &exact, &version, error)) {
        return false;
    }
    if (!exact) {
        if (requireOwned) {
            return SetError(error, L"package-contract", ERROR_INVALID_DATA,
                L"INF does not match the exact VIIPER native driver contract");
        }
        *owned = false;
        return true;
    }
    std::filesystem::path catalogPath;
    if (allowUntrustedLocalTestRoot) {
        catalogPath = path.parent_path() / kCatalogName;
    } else {
        if (!VerifyInfSignature(path, &catalogPath, error)) {
            return false;
        }
    }
    std::filesystem::path packageInfPath = path;
    if (_wcsicmp(path.filename().c_str(), L"ViiperUde.inf") != 0 &&
        !GetDriverStoreInfPath(path, &packageInfPath, error)) {
        return false;
    }
    if (catalogPath.is_relative()) {
        catalogPath = packageInfPath.parent_path() / catalogPath.filename();
    }
    std::string infHash;
    std::string sysHash;
    std::string catHash;
    if (!Sha256File(path, &infHash, error) ||
        !Sha256File(packageInfPath.parent_path() / kDriverFileName, &sysHash, error) ||
        !Sha256File(catalogPath, &catHash, error)) {
        return false;
    }
    package->infPath = path;
    package->version = version;
    package->infSha256 = std::move(infHash);
    package->sysSha256 = std::move(sysHash);
    package->catSha256 = std::move(catHash);
    *owned = true;
    return true;
}

bool IsSafePublishedInfName(const std::wstring& value) {
    const std::filesystem::path path(value);
    if (path.has_parent_path() || path.filename().wstring() != value || value.size() < 9) {
        return false;
    }
    std::wstring lower = value;
    std::transform(lower.begin(), lower.end(), lower.begin(), [](wchar_t character) {
        return static_cast<wchar_t>(towlower(character));
    });
    if (!lower.starts_with(L"oem") || !lower.ends_with(L".inf")) {
        return false;
    }
    return std::all_of(lower.begin() + 3, lower.end() - 4, [](wchar_t character) {
        return character >= L'0' && character <= L'9';
    });
}

bool GetSystemInfDirectory(std::filesystem::path* directory, Error* error) {
    std::vector<wchar_t> buffer(MAX_PATH);
    const UINT length = GetWindowsDirectoryW(buffer.data(), static_cast<UINT>(buffer.size()));
    if (length == 0) {
        return SetLastErrorDetail(error, L"windows-directory");
    }
    if (static_cast<size_t>(length) >= buffer.size()) {
        buffer.resize(static_cast<size_t>(length) + 1);
        const UINT retry = GetWindowsDirectoryW(buffer.data(), static_cast<UINT>(buffer.size()));
        if (retry == 0 || static_cast<size_t>(retry) >= buffer.size()) {
            return SetLastErrorDetail(error, L"windows-directory");
        }
    }
    *directory = std::filesystem::path(buffer.data()) / L"INF";
    return true;
}

bool GetPublishedInfPath(
    const std::filesystem::path& infPath,
    std::filesystem::path* publishedPath,
    Error* error) {
    DWORD required = 0;
    SetupGetInfPublishedNameW(infPath.c_str(), nullptr, 0, &required);
    if (required == 0 || GetLastError() != ERROR_INSUFFICIENT_BUFFER) {
        return SetLastErrorDetail(error, L"published-inf");
    }
    std::vector<wchar_t> buffer(required);
    if (!SetupGetInfPublishedNameW(infPath.c_str(), buffer.data(), required, nullptr)) {
        return SetLastErrorDetail(error, L"published-inf");
    }
    const std::filesystem::path result(buffer.data());
    std::filesystem::path systemInf;
    if (!GetSystemInfDirectory(&systemInf, error)) {
        return false;
    }
    std::error_code parentError;
    std::error_code systemError;
    const std::filesystem::path canonicalParent = std::filesystem::canonical(result.parent_path(), parentError);
    const std::filesystem::path canonicalSystemInf = std::filesystem::canonical(systemInf, systemError);
    if (parentError || systemError || !IsSafePublishedInfName(result.filename().wstring()) ||
        _wcsicmp(canonicalParent.c_str(), canonicalSystemInf.c_str()) != 0) {
        return SetError(error, L"published-inf", ERROR_INVALID_NAME,
            L"SetupAPI returned a published INF outside the system INF directory");
    }
    *publishedPath = result;
    return true;
}

bool GetDriverStoreInfPath(
    const std::filesystem::path& publishedPath,
    std::filesystem::path* storePath,
    Error* error) {
    DWORD required = 0;
    SetupGetInfDriverStoreLocationW(publishedPath.c_str(), nullptr, nullptr, nullptr, 0, &required);
    if (required == 0 || GetLastError() != ERROR_INSUFFICIENT_BUFFER) {
        return SetLastErrorDetail(error, L"driver-store-inf");
    }
    std::vector<wchar_t> buffer(required);
    if (!SetupGetInfDriverStoreLocationW(
            publishedPath.c_str(), nullptr, nullptr, buffer.data(), required, nullptr)) {
        return SetLastErrorDetail(error, L"driver-store-inf");
    }
    *storePath = buffer.data();
    return true;
}

bool EnumerateOwnedPackages(std::vector<PackageInfo>* packages, Error* error) {
    packages->clear();
    std::filesystem::path infDirectory;
    if (!GetSystemInfDirectory(&infDirectory, error)) {
        return false;
    }
    const std::wstring pattern = (infDirectory / L"oem*.inf").wstring();
    WIN32_FIND_DATAW data{};
    HANDLE rawFind = FindFirstFileW(pattern.c_str(), &data);
    if (rawFind == INVALID_HANDLE_VALUE) {
        if (GetLastError() == ERROR_FILE_NOT_FOUND) {
            return true;
        }
        return SetLastErrorDetail(error, L"enumerate-published-inf");
    }
    do {
        if ((data.dwFileAttributes & FILE_ATTRIBUTE_DIRECTORY) != 0 ||
            !IsSafePublishedInfName(data.cFileName)) {
            continue;
        }
        PackageInfo package;
        bool owned = false;
        Error packageError;
        if (!LoadOwnedPackage(infDirectory / data.cFileName, false, false,
                &package, &owned, &packageError)) {
            FindClose(rawFind);
            *error = std::move(packageError);
            return false;
        }
        if (owned) {
            package.publishedName = data.cFileName;
            packages->push_back(std::move(package));
        }
    } while (FindNextFileW(rawFind, &data));
    const DWORD enumerationError = GetLastError();
    FindClose(rawFind);
    if (enumerationError != ERROR_NO_MORE_FILES) {
        return SetError(error, L"enumerate-published-inf", enumerationError);
    }
    std::sort(packages->begin(), packages->end(), [](const PackageInfo& left, const PackageInfo& right) {
        return _wcsicmp(left.publishedName.c_str(), right.publishedName.c_str()) < 0;
    });
    return true;
}

bool FindPublishedCandidate(
    const PackageInfo& candidate,
    PackageInfo* published,
    Error* error) {
    std::vector<PackageInfo> packages;
    if (!EnumerateOwnedPackages(&packages, error)) {
        return false;
    }
    size_t matches = 0;
    for (const PackageInfo& package : packages) {
        if (package.version == candidate.version && SamePackageBytes(package, candidate)) {
            *published = package;
            ++matches;
        }
    }
    if (matches != 1) {
        return SetError(error, L"published-candidate",
            matches == 0 ? ERROR_NOT_FOUND : ERROR_DUPLICATE_SERVICE_NAME,
            L"driver store must contain exactly one published copy of the candidate package");
    }
    return true;
}

bool MultiSzContains(const std::vector<BYTE>& value, const wchar_t* expected) {
    if (value.empty() || value.size() % sizeof(wchar_t) != 0) {
        return false;
    }
    const auto* current = reinterpret_cast<const wchar_t*>(value.data());
    const auto* end = current + value.size() / sizeof(wchar_t);
    while (current < end && *current != L'\0') {
        const size_t remaining = static_cast<size_t>(end - current);
        const size_t length = wcsnlen_s(current, remaining);
        if (length == remaining) {
            return false;
        }
        if (_wcsicmp(std::wstring(current, length).c_str(), expected) == 0) {
            return true;
        }
        current += length + 1;
    }
    return false;
}

bool HasExactHardwareId(HDEVINFO set, SP_DEVINFO_DATA& data) {
    DWORD type = 0;
    DWORD required = 0;
    if (SetupDiGetDeviceRegistryPropertyW(
            set, &data, SPDRP_HARDWAREID, &type, nullptr, 0, &required)) {
        return false;
    }
    if (GetLastError() != ERROR_INSUFFICIENT_BUFFER || required == 0 || type != REG_MULTI_SZ) {
        return false;
    }
    std::vector<BYTE> value(required);
    if (!SetupDiGetDeviceRegistryPropertyW(
            set, &data, SPDRP_HARDWAREID, &type, value.data(),
            static_cast<DWORD>(value.size()), nullptr)) {
        return false;
    }
    return type == REG_MULTI_SZ && MultiSzContains(value, kHardwareId);
}

bool ReadDevicePresence(HDEVINFO set, SP_DEVINFO_DATA& data, bool* present, Error* error) {
    DEVPROPTYPE type = 0;
    DEVPROP_BOOLEAN value = DEVPROP_FALSE;
    DWORD required = 0;
    if (!SetupDiGetDevicePropertyW(
            set, &data, &DEVPKEY_Device_IsPresent, &type,
            reinterpret_cast<PBYTE>(&value), sizeof(value), &required, 0)) {
        return SetLastErrorDetail(error, L"device-presence");
    }
    if (type != DEVPROP_TYPE_BOOLEAN || required != sizeof(value)) {
        return SetError(error, L"device-presence", ERROR_INVALID_DATA);
    }
    *present = value == DEVPROP_TRUE;
    return true;
}

bool ReadDevicePropertyString(
    HDEVINFO set,
    SP_DEVINFO_DATA& data,
    const DEVPROPKEY& key,
    std::wstring* value,
    Error* error) {
    DEVPROPTYPE type = 0;
    DWORD required = 0;
    if (SetupDiGetDevicePropertyW(set, &data, &key, &type, nullptr, 0, &required, 0)) {
        return SetError(error, L"device-property", ERROR_INVALID_DATA);
    }
    const DWORD code = GetLastError();
    if (code == ERROR_NOT_FOUND) {
        value->clear();
        return true;
    }
    if (code != ERROR_INSUFFICIENT_BUFFER || required < sizeof(wchar_t) || type != DEVPROP_TYPE_STRING) {
        return SetError(error, L"device-property", code);
    }
    std::vector<BYTE> buffer(required);
    if (!SetupDiGetDevicePropertyW(
            set, &data, &key, &type, buffer.data(), static_cast<DWORD>(buffer.size()), nullptr, 0)) {
        return SetLastErrorDetail(error, L"device-property");
    }
    *value = reinterpret_cast<const wchar_t*>(buffer.data());
    return true;
}

bool ReadService(HDEVINFO set, SP_DEVINFO_DATA& data, std::wstring* service, Error* error) {
    DWORD type = 0;
    DWORD required = 0;
    if (SetupDiGetDeviceRegistryPropertyW(set, &data, SPDRP_SERVICE, &type, nullptr, 0, &required)) {
        return SetError(error, L"device-service", ERROR_INVALID_DATA);
    }
    const DWORD code = GetLastError();
    if (code == ERROR_INVALID_DATA) {
        service->clear();
        return true;
    }
    if (code != ERROR_INSUFFICIENT_BUFFER || required < sizeof(wchar_t) || type != REG_SZ) {
        return SetError(error, L"device-service", code);
    }
    std::vector<BYTE> buffer(required);
    if (!SetupDiGetDeviceRegistryPropertyW(
            set, &data, SPDRP_SERVICE, &type, buffer.data(), static_cast<DWORD>(buffer.size()), nullptr)) {
        return SetLastErrorDetail(error, L"device-service");
    }
    *service = reinterpret_cast<const wchar_t*>(buffer.data());
    return true;
}

struct DeviceState {
    std::wstring instanceId;
    bool present = false;
    bool started = false;
    ULONG problem = 0;
    std::wstring service;
    std::wstring publishedInf;
    Version version{};
    PackageInfo package;
};

DeviceInfoSet OpenRootDevices() {
    return DeviceInfoSet(SetupDiGetClassDevsW(nullptr, kEnumerator, nullptr, DIGCF_ALLCLASSES));
}

bool FindExactDevices(HDEVINFO set, std::vector<std::pair<SP_DEVINFO_DATA, DeviceState>>* devices, Error* error) {
    devices->clear();
    for (DWORD index = 0;; ++index) {
        SP_DEVINFO_DATA data{};
        data.cbSize = sizeof(data);
        if (!SetupDiEnumDeviceInfo(set, index, &data)) {
            if (GetLastError() != ERROR_NO_MORE_ITEMS) {
                return SetLastErrorDetail(error, L"enumerate-root-devices");
            }
            break;
        }
        if (!HasExactHardwareId(set, data)) {
            continue;
        }
        DeviceState state;
        if (!ReadDevicePresence(set, data, &state.present, error) ||
            !ReadService(set, data, &state.service, error) ||
            !ReadDevicePropertyString(set, data, DEVPKEY_Device_DriverInfPath, &state.publishedInf, error)) {
            return false;
        }
        std::wstring driverVersion;
        if (!ReadDevicePropertyString(set, data, DEVPKEY_Device_DriverVersion, &driverVersion, error)) {
            return false;
        }
        if (!driverVersion.empty() && !ParseVersion(driverVersion, &state.version)) {
            return SetError(error, L"device-version", ERROR_INVALID_DATA,
                L"installed device exposes a malformed driver version");
        }
        DWORD required = 0;
        SetupDiGetDeviceInstanceIdW(set, &data, nullptr, 0, &required);
        if (required == 0 || GetLastError() != ERROR_INSUFFICIENT_BUFFER) {
            return SetLastErrorDetail(error, L"device-instance-id");
        }
        std::vector<wchar_t> instance(required);
        if (!SetupDiGetDeviceInstanceIdW(set, &data, instance.data(), required, nullptr)) {
            return SetLastErrorDetail(error, L"device-instance-id");
        }
        state.instanceId = instance.data();
        ULONG status = 0;
        ULONG problem = 0;
        const CONFIGRET configuration = CM_Get_DevNode_Status(&status, &problem, data.DevInst, 0);
        state.started = state.present && configuration == CR_SUCCESS && (status & DN_STARTED) != 0 && problem == 0;
        state.problem = configuration == CR_SUCCESS ? problem : static_cast<ULONG>(configuration);
        devices->emplace_back(data, std::move(state));
    }
    return true;
}

struct Snapshot {
    std::vector<DeviceState> devices;
    std::vector<PackageInfo> packages;
};

enum class InstallJournalPhase {
    Prepared,
    SetupCopyEntered,
    SetupCopyReturned,
    StageReceiptCaptured,
    QuiesceSignalEntered,
    QuiesceSignalReturned,
    RootRegistrationIntentCaptured,
    RootRegistrationEntered,
    RootRegistrationReturned,
    DiInstallEntered,
    DiInstallReturned,
    PriorAbiProfileCaptured,
    DriverValidated,
    BrokerHandoffEntered,
    BrokerHandoffReturned,
    BrokerChildEntered,
    BrokerChildSettled,
    RollbackBindingEntered,
    PartialRootRemovalEntered,
    PartialRootRemovalReturned,
    PartialRootRemovalRebootPending,
    RollbackBindingReturned,
    SetupUninstallEntered,
    SetupUninstallReturned,
    ForwardValidated,
    ExactPriorRestored,
    ForwardRebootPending,
    RestoreRebootPending,
    ManualReconciliationRequired,
};

enum class InstallJournalDirection {
    Forward,
    Rollback,
};

bool RecordActiveInstallJournalCutpoint(
    InstallJournalPhase phase,
    bool callSucceeded,
    DWORD callError,
    bool deadlineOverrun,
    Error* error);

bool RecordActiveInstallJournalCutpointWithReboot(
    InstallJournalPhase phase,
    bool callSucceeded,
    DWORD callError,
    bool deadlineOverrun,
    bool rebootRequired,
    bool freshRebootRequired,
    Error* error);

bool RecordActiveInstallJournalRollbackAuthorization(
    InstallJournalPhase phase,
    DWORD callError,
    Error* error);

bool RecordActiveInstallJournalRootRegistrationIntent(
    const std::wstring& instanceId,
    Error* error);

enum class CandidateDisposition {
    InstallRequired,
    Exact,
};

bool RequiresDriverMutation(
    CandidateDisposition disposition,
    bool exactBindingHealthy) noexcept {
    return disposition == CandidateDisposition::InstallRequired ||
        (disposition == CandidateDisposition::Exact && !exactBindingHealthy);
}

bool RequiresPristineRuntimeProof(
    CandidateDisposition disposition,
    bool exactBindingHealthy,
    bool rootPresent,
    bool rootStarted) noexcept {
    return RequiresDriverMutation(disposition, exactBindingHealthy) &&
        rootPresent && rootStarted;
}

bool ClassifyCandidatePackage(
    const PackageInfo& candidate,
    const std::vector<PackageInfo>& installedPackages,
    const std::optional<Version>& expectedDowngradeFrom,
    CandidateDisposition* disposition,
    bool* downgrade,
    Error* error) {
    if (disposition == nullptr || downgrade == nullptr) {
        return SetError(error, L"version-policy", ERROR_INVALID_PARAMETER,
            L"candidate package classification requires output storage");
    }
    *disposition = CandidateDisposition::InstallRequired;
    *downgrade = false;

    const bool conflictingSameVersion = std::any_of(
        installedPackages.begin(), installedPackages.end(), [&](const PackageInfo& package) {
            return package.version == candidate.version &&
                !SamePackageBytes(package, candidate);
        });
    if (conflictingSameVersion) {
        return SetError(error, L"version-policy", ERROR_REVISION_MISMATCH,
            L"same-version INF, SYS, or signing catalog replacement is rejected; increment DriverVer");
    }

    std::optional<PackageInfo> highest;
    for (const PackageInfo& package : installedPackages) {
        if (!highest || highest->version < package.version) {
            highest = package;
        }
    }
    if (!highest) {
        if (expectedDowngradeFrom) {
            return SetError(error, L"version-policy", ERROR_INVALID_PARAMETER,
                L"controlled downgrade guard is valid only for an actual downgrade");
        }
        return true;
    }

    if (candidate.version < highest->version) {
        *downgrade = true;
        if (!expectedDowngradeFrom || !(*expectedDowngradeFrom == highest->version)) {
            return SetError(error, L"version-policy", ERROR_REVISION_MISMATCH,
                L"downgrade rejected; pass --allow-controlled-downgrade with the exact installed version " +
                VersionToString(highest->version));
        }
        return true;
    }
    if (candidate.version == highest->version) {
        if (expectedDowngradeFrom) {
            return SetError(error, L"version-policy", ERROR_INVALID_PARAMETER,
                L"controlled downgrade guard is valid only for an actual downgrade");
        }
        *disposition = CandidateDisposition::Exact;
        return true;
    }
    if (expectedDowngradeFrom) {
        return SetError(error, L"version-policy", ERROR_INVALID_PARAMETER,
            L"controlled downgrade guard is valid only for an actual downgrade");
    }
    return true;
}

bool CaptureSnapshot(Snapshot* snapshot, Error* error) {
    snapshot->devices.clear();
    if (!EnumerateOwnedPackages(&snapshot->packages, error)) {
        return false;
    }
    DeviceInfoSet set = OpenRootDevices();
    if (!set) {
        return SetLastErrorDetail(error, L"open-root-devices");
    }
    std::vector<std::pair<SP_DEVINFO_DATA, DeviceState>> matches;
    if (!FindExactDevices(set.get(), &matches, error)) {
        return false;
    }
    std::filesystem::path infDirectory;
    if (!GetSystemInfDirectory(&infDirectory, error)) {
        return false;
    }
    for (auto& match : matches) {
        DeviceState& device = match.second;
        if (!IsOwnedGeneratedRootInstanceId(device.instanceId)) {
            return SetError(error, L"device-instance-ownership", ERROR_INVALID_DATA,
                L"ROOT\\VIIPER\\UDE has an instance ID outside the VIIPER or legacy generated root namespace");
        }
        if (_wcsicmp(device.service.c_str(), kServiceName) != 0 ||
            !IsSafePublishedInfName(device.publishedInf)) {
            return SetError(error, L"device-ownership", ERROR_NOT_FOUND,
                L"ROOT\\VIIPER\\UDE is bound to an unowned service or package; refusing mutation");
        }
        bool owned = false;
        PackageInfo package;
        if (!LoadOwnedPackage(infDirectory / device.publishedInf, true, false,
                &package, &owned, error)) {
            return false;
        }
        package.publishedName = device.publishedInf;
        if (!owned || !(device.version == package.version)) {
            return SetError(error, L"device-ownership", ERROR_REVISION_MISMATCH,
                L"devnode version does not match its exact signed published INF");
        }
        device.package = std::move(package);
        snapshot->devices.push_back(std::move(device));
    }
    return true;
}

bool SameRootBinding(const DeviceState& left, const DeviceState& right) noexcept {
    return _wcsicmp(left.instanceId.c_str(), right.instanceId.c_str()) == 0 &&
        left.present == right.present &&
        _wcsicmp(left.service.c_str(), right.service.c_str()) == 0 &&
        _wcsicmp(left.publishedInf.c_str(), right.publishedInf.c_str()) == 0 &&
        left.version == right.version && SamePackageBytes(left.package, right.package);
}

bool SameEnumeratedRootState(
    const DeviceState& left,
    const DeviceState& right) noexcept {
    return _wcsicmp(left.instanceId.c_str(), right.instanceId.c_str()) == 0 &&
        left.present == right.present && left.started == right.started &&
        left.problem == right.problem &&
        _wcsicmp(left.service.c_str(), right.service.c_str()) == 0 &&
        _wcsicmp(left.publishedInf.c_str(), right.publishedInf.c_str()) == 0 &&
        left.version == right.version;
}

bool SameCapturedRootState(
    const Snapshot& left,
    const Snapshot& right) noexcept {
    return left.devices.size() == right.devices.size() &&
        (left.devices.empty() ||
            (SameRootBinding(left.devices[0], right.devices[0]) &&
                left.devices[0].started == right.devices[0].started &&
                left.devices[0].problem == right.devices[0].problem));
}

bool RollbackLifecycleStateMatches(
    const DeviceState& captured,
    const DeviceState& restored) noexcept {
    if (captured.started) {
        return restored.started && restored.problem == 0;
    }
    return !restored.started && restored.problem == captured.problem;
}

bool CaptureAndVerifyRootUnchanged(
    const Snapshot& expected,
    const wchar_t* phase,
    Snapshot* observed,
    Error* error) {
    Snapshot current;
    if (!CaptureSnapshot(&current, error)) {
        return false;
    }
    if (!SameCapturedRootState(expected, current)) {
        return SetError(error, phase,
            ERROR_REVISION_MISMATCH,
            L"the captured root identity or lifecycle state changed before exact binding");
    }
    if (observed != nullptr) {
        *observed = std::move(current);
    }
    return true;
}

bool CaptureAndVerifyPreparedRootUnchanged(
    const DeviceState& expected,
    HDEVINFO set,
    DEVINST expectedDevInst,
    const wchar_t* phase,
    Error* error) {
    std::vector<std::pair<SP_DEVINFO_DATA, DeviceState>> matches;
    if (!FindExactDevices(set, &matches, error)) {
        return false;
    }
    if (matches.size() != 1 || matches[0].first.DevInst != expectedDevInst) {
        return SetError(error, phase, ERROR_REVISION_MISMATCH,
            L"the selected compatible-driver list no longer belongs to the captured root devnode");
    }

    DeviceState& observed = matches[0].second;
    std::filesystem::path infDirectory;
    if (!GetSystemInfDirectory(&infDirectory, error)) {
        return false;
    }
    bool owned = false;
    PackageInfo package;
    if (!IsOwnedGeneratedRootInstanceId(observed.instanceId) ||
        _wcsicmp(observed.service.c_str(), kServiceName) != 0 ||
        !IsSafePublishedInfName(observed.publishedInf) ||
        !LoadOwnedPackage(infDirectory / observed.publishedInf,
            true, false, &package, &owned, error)) {
        if (error->code == ERROR_SUCCESS) {
            SetError(error, phase, ERROR_ACCESS_DENIED,
                L"the selected root no longer has an exact owned package identity");
        }
        return false;
    }
    package.publishedName = observed.publishedInf;
    observed.package = std::move(package);
    if (!owned || !(observed.version == observed.package.version) ||
        !SameRootBinding(expected, observed) ||
        expected.started != observed.started || expected.problem != observed.problem) {
        return SetError(error, phase, ERROR_REVISION_MISMATCH,
            L"the selected root identity, lifecycle state, or package bytes changed before binding");
    }
    return true;
}

bool StageCandidatePackage(
    const PackageInfo& candidate,
    bool production,
    uint64_t transactionDeadlineUnixMs,
    bool* mutationStarted,
    bool* stagedHere,
    PackageInfo* published,
    Error* error) {
    *stagedHere = false;
    *published = PackageInfo{};
    const std::wstring sourcePath = candidate.infPath.native();
    if (sourcePath.empty() || sourcePath.size() >= MAX_PATH) {
        return SetError(error, L"stage-driver-package-path", ERROR_FILENAME_EXCED_RANGE,
            L"SetupCopyOEMInf requires a canonical source INF path shorter than MAX_PATH");
    }
    if (!CheckTransactionDeadline(transactionDeadlineUnixMs,
            L"transaction-deadline-before-driver-stage", error)) {
        return false;
    }
    std::filesystem::path systemInf;
    if (!GetSystemInfDirectory(&systemInf, error)) {
        return false;
    }
    std::error_code systemInfError;
    const std::filesystem::path canonicalSystemInf =
        std::filesystem::canonical(systemInf, systemInfError);
    if (systemInfError) {
        return SetError(error, L"stage-system-inf-directory",
            static_cast<DWORD>(systemInfError.value()),
            L"the canonical system INF directory could not be captured before staging");
    }

    std::array<wchar_t, MAX_PATH> destination{};
    DWORD required = 0;
    // SetupCopyOEMInf can publish bytes before returning or before subsequent
    // receipt validation. Mark the protected transaction as potentially
    // mutated before the API boundary; stagedHere remains success-only so
    // rollback never claims ownership of a preexisting or uncertain package.
    if (!RecordActiveInstallJournalCutpoint(
            InstallJournalPhase::SetupCopyEntered, true, ERROR_SUCCESS,
            false, error)) {
        return false;
    }
    MarkTransactionMutationStarted();
    const BOOL copied = InvokeAuthoritativeSynchronousMutation(
        transactionDeadlineUnixMs, L"SetupCopyOEMInfW", [&]() {
            return SetupCopyOEMInfW(
                sourcePath.c_str(), nullptr, SPOST_PATH, SP_COPY_NOOVERWRITE,
                destination.data(), static_cast<DWORD>(destination.size()),
                &required, nullptr);
        });
    const DWORD copyError = copied ? ERROR_SUCCESS : GetLastError();
    Error journalReturnError;
    const bool journalReturnRecorded = RecordActiveInstallJournalCutpoint(
        InstallJournalPhase::SetupCopyReturned, copied != FALSE,
        copyError, gLastSynchronousMutationTimedOut, &journalReturnError);
    if (copied) {
        if (mutationStarted != nullptr) {
            *mutationStarted = true;
        }
        *stagedHere = true;
    } else if (copyError != ERROR_FILE_EXISTS) {
        if (mutationStarted != nullptr) {
            *mutationStarted = true;
        }
    }
    if (!copied && copyError != ERROR_FILE_EXISTS) {
        // An unexpected API failure is not proof of package ownership. Leave
        // stagedHere false; common rollback will prove the prior inventory and
        // fail closed if SetupAPI nevertheless changed it.
        if (!journalReturnRecorded) {
            *error = std::move(journalReturnError);
            return false;
        }
        return SetError(error, L"stage-driver-package", copyError,
            L"add-only candidate import into the Driver Store failed");
    }

    const size_t destinationLength =
        wcsnlen_s(destination.data(), destination.size());
    bool receiptValid = destinationLength != 0 &&
        destinationLength < destination.size() &&
        required == destinationLength + 1;
    std::filesystem::path destinationPath;
    if (receiptValid) {
        destinationPath = destination.data();
        std::error_code parentError;
        const std::filesystem::path canonicalParent =
            std::filesystem::canonical(destinationPath.parent_path(), parentError);
        receiptValid = !parentError &&
            IsSafePublishedInfName(destinationPath.filename().wstring()) &&
            _wcsicmp(canonicalParent.c_str(), canonicalSystemInf.c_str()) == 0;
    }
    if (receiptValid) {
        // Preserve the API's exact, safe published-name receipt immediately.
        // Full bytes/catalog/signer verification below may still fail, but
        // rollback can then identify and verify only this package.
        *published = candidate;
        published->infPath = destinationPath;
        published->publishedName = destinationPath.filename().wstring();
    } else {
        PackageInfo recoveredReceipt;
        Error ignoredReceiptError;
        if (FindPublishedCandidate(
                candidate, &recoveredReceipt, &ignoredReceiptError)) {
            *published = std::move(recoveredReceipt);
        }
        return SetError(error, L"stage-published-inf", ERROR_INVALID_DATA,
            L"SetupCopyOEMInf returned a malformed published INF identity");
    }
    std::filesystem::path resolvedPublishedPath;
    PackageInfo verifiedPublished;
    if (!GetPublishedInfPath(candidate.infPath, &resolvedPublishedPath, error) ||
        _wcsicmp(resolvedPublishedPath.c_str(), destinationPath.c_str()) != 0 ||
        !FindPublishedCandidate(candidate, &verifiedPublished, error) ||
        _wcsicmp(verifiedPublished.infPath.c_str(), resolvedPublishedPath.c_str()) != 0 ||
        _wcsicmp(verifiedPublished.publishedName.c_str(),
            resolvedPublishedPath.filename().c_str()) != 0) {
        if (error->code == ERROR_SUCCESS) {
            SetError(error, L"stage-published-inf", ERROR_REVISION_MISMATCH,
                L"add-only staging did not resolve to the unique exact candidate package");
        }
        return false;
    }
    if (production &&
        !VerifyMicrosoftHardwareInfSigner(verifiedPublished.infPath, error)) {
        return false;
    }
    *published = std::move(verifiedPublished);
    if (!journalReturnRecorded) {
        *error = std::move(journalReturnError);
        return false;
    }
    if (gLastSynchronousMutationTimedOut) {
        return SetError(error, L"stage-driver-package-timeout", ERROR_TIMEOUT,
            L"SetupCopyOEMInfW exceeded the transaction deadline; its authoritative return and exact receipt were retained for rollback");
    }
    return true;
}

bool RemoveDevice(
    HDEVINFO set,
    SP_DEVINFO_DATA& data,
    uint64_t transactionDeadlineUnixMs,
    const wchar_t* deadlinePhase,
    bool* mutationStarted,
    bool* rebootRequired,
    Error* error,
    bool* freshRebootRequired = nullptr) {
    if (transactionDeadlineUnixMs != 0 &&
        !CheckTransactionDeadline(transactionDeadlineUnixMs, deadlinePhase, error)) {
        return false;
    }
    MarkTransactionMutationStarted();
    if (mutationStarted != nullptr) {
        *mutationStarted = true;
    }
    BOOL reboot = FALSE;
    if (!DiUninstallDevice(nullptr, set, &data, 0, &reboot)) {
        return SetLastErrorDetail(error, L"remove-devnode");
    }
    if (freshRebootRequired != nullptr) {
        *freshRebootRequired = reboot != FALSE;
    }
    *rebootRequired = *rebootRequired || reboot != FALSE;
    return true;
}

bool RemoveAllExactDevices(
    uint64_t transactionDeadlineUnixMs,
    bool* mutationStarted,
    bool* rebootRequired,
    Error* error) {
    DeviceInfoSet set = OpenRootDevices();
    if (!set) {
        return SetLastErrorDetail(error, L"open-root-devices");
    }
    std::vector<std::pair<SP_DEVINFO_DATA, DeviceState>> matches;
    if (!FindExactDevices(set.get(), &matches, error)) {
        return false;
    }
    std::filesystem::path infDirectory;
    if (!GetSystemInfDirectory(&infDirectory, error)) {
        return false;
    }
    for (auto& match : matches) {
        DeviceState& device = match.second;
        if (!IsOwnedGeneratedRootInstanceId(device.instanceId) ||
            _wcsicmp(device.service.c_str(), kServiceName) != 0 ||
            !IsSafePublishedInfName(device.publishedInf)) {
            return SetError(error, L"remove-ownership", ERROR_ACCESS_DENIED,
                L"refusing to remove an exact hardware ID not owned by the signed VIIPER package");
        }
        PackageInfo package;
        bool owned = false;
        if (!LoadOwnedPackage(infDirectory / device.publishedInf, true, false,
                &package, &owned, error) || !owned) {
            return false;
        }
    }
    for (auto& match : matches) {
        if (!RemoveDevice(set.get(), match.first, transactionDeadlineUnixMs,
                L"remove-deadline-before-device-mutation", mutationStarted,
                rebootRequired, error)) {
            return false;
        }
    }
    return true;
}

bool RegisterRootDevice(
    const GUID& classGuid,
    uint64_t transactionDeadlineUnixMs,
    bool* mutationStarted,
    bool* registrationSucceeded,
    DeviceInfoSet* set,
    SP_DEVINFO_DATA* data,
    Error* error) {
    if (registrationSucceeded != nullptr) {
        *registrationSucceeded = false;
    }
    *set = DeviceInfoSet(SetupDiCreateDeviceInfoList(&classGuid, nullptr));
    if (!*set) {
        return SetLastErrorDetail(error, L"create-device-info-list");
    }
    *data = SP_DEVINFO_DATA{};
    data->cbSize = sizeof(*data);
    if (!SetupDiCreateDeviceInfoW(
            set->get(), kRootDeviceName, &classGuid, nullptr, nullptr,
            DICD_GENERATE_ID, data)) {
        return SetLastErrorDetail(error, L"create-root-devnode");
    }
    DWORD instanceCharacters = 0;
    SetupDiGetDeviceInstanceIdW(
        set->get(), data, nullptr, 0, &instanceCharacters);
    if (instanceCharacters == 0U ||
        GetLastError() != ERROR_INSUFFICIENT_BUFFER) {
        return SetLastErrorDetail(
            error, L"capture-generated-root-instance-id");
    }
    std::vector<wchar_t> generatedInstance(instanceCharacters);
    if (!SetupDiGetDeviceInstanceIdW(
            set->get(), data, generatedInstance.data(),
            instanceCharacters, nullptr)) {
        return SetLastErrorDetail(
            error, L"capture-generated-root-instance-id");
    }
    const std::wstring intendedInstanceId = generatedInstance.data();
    if (!IsEqualGUID(classGuid, GUID_DEVCLASS_USB) ||
        !IsGeneratedRootInstanceIdForDeviceName(
            intendedInstanceId, kRootDeviceName)) {
        return SetError(error, L"capture-generated-root-instance-id",
            ERROR_INVALID_DATA,
            L"SetupAPI generated a root identity or class outside the exact VIIPER transaction namespace");
    }
    if (!RecordActiveInstallJournalRootRegistrationIntent(
            intendedInstanceId, error)) {
        return false;
    }
    const size_t idCharacters = std::size(kHardwareId) + 1;
    std::vector<wchar_t> identifiers(idCharacters, L'\0');
    std::copy(std::begin(kHardwareId), std::end(kHardwareId), identifiers.begin());
    if (!CheckTransactionDeadline(transactionDeadlineUnixMs,
            L"transaction-deadline-before-root-properties", error)) {
        return false;
    }
    if (!RecordActiveInstallJournalCutpoint(
            InstallJournalPhase::RootRegistrationEntered,
            true, ERROR_SUCCESS, false, error)) {
        return false;
    }
    MarkTransactionMutationStarted();
    if (mutationStarted != nullptr) {
        *mutationStarted = true;
    }
    if (!SetupDiSetDeviceRegistryPropertyW(
            set->get(), data, SPDRP_HARDWAREID,
            reinterpret_cast<const BYTE*>(identifiers.data()),
            static_cast<DWORD>(identifiers.size() * sizeof(wchar_t)))) {
        const DWORD code = GetLastError();
        Error journalError;
        if (!RecordActiveInstallJournalCutpoint(
                InstallJournalPhase::RootRegistrationReturned,
                false, code, false, &journalError)) {
            *error = std::move(journalError);
            return false;
        }
        return SetError(error, L"set-root-hardware-id", code);
    }
    if (!CheckTransactionDeadline(transactionDeadlineUnixMs,
            L"transaction-deadline-before-root-registration", error)) {
        return false;
    }
    MarkTransactionMutationStarted();
    if (mutationStarted != nullptr) {
        *mutationStarted = true;
    }
    const BOOL registered = InvokeAuthoritativeSynchronousMutation(
        transactionDeadlineUnixMs, L"SetupDiCallClassInstaller(DIF_REGISTERDEVICE)",
        [&]() {
            return SetupDiCallClassInstaller(
                DIF_REGISTERDEVICE, set->get(), data);
        });
    const DWORD registerError = registered ? ERROR_SUCCESS : GetLastError();
    Error journalReturnError;
    const bool journalReturnRecorded = RecordActiveInstallJournalCutpoint(
        InstallJournalPhase::RootRegistrationReturned,
        registered != FALSE, registerError, gLastSynchronousMutationTimedOut,
        &journalReturnError);
    if (!registered) {
        if (!journalReturnRecorded) {
            *error = std::move(journalReturnError);
            return false;
        }
        return SetError(error, L"register-root-devnode", registerError);
    }
    if (registrationSucceeded != nullptr) {
        *registrationSucceeded = true;
    }
    wchar_t instanceId[MAX_DEVICE_ID_LEN]{};
    if (!SetupDiGetDeviceInstanceIdW(
            set->get(), data, instanceId, static_cast<DWORD>(std::size(instanceId)), nullptr)) {
        return SetLastErrorDetail(error, L"verify-generated-root-instance-id");
    }
    if (!IsGeneratedRootInstanceIdForDeviceName(instanceId, kRootDeviceName) ||
        _wcsicmp(instanceId, intendedInstanceId.c_str()) != 0) {
        return SetError(error, L"verify-generated-root-instance-id", ERROR_INVALID_DATA,
            L"registered root identity changed from the exact durable pre-registration receipt");
    }
    if (!journalReturnRecorded) {
        *error = std::move(journalReturnError);
        return false;
    }
    if (gLastSynchronousMutationTimedOut) {
        return SetError(error, L"register-root-devnode-timeout", ERROR_TIMEOUT,
            L"root registration exceeded the transaction deadline; its authoritative return was retained for rollback");
    }
    return true;
}

bool DriverInfoUsesPublishedPackage(
    const std::filesystem::path& driverInfPath,
    const std::wstring& expectedPublishedName) {
    if (IsSafePublishedInfName(driverInfPath.filename().wstring())) {
        return _wcsicmp(
            driverInfPath.filename().c_str(), expectedPublishedName.c_str()) == 0;
    }
    std::filesystem::path publishedPath;
    Error ignored;
    return GetPublishedInfPath(driverInfPath, &publishedPath, &ignored) &&
        _wcsicmp(publishedPath.filename().c_str(), expectedPublishedName.c_str()) == 0;
}

struct PreparedDriverBinding {
    HDEVINFO set = INVALID_HANDLE_VALUE;
    SP_DEVINFO_DATA* device = nullptr;
    SP_DRVINFO_DATA_W selected{};
    bool active = false;

    PreparedDriverBinding() = default;
    PreparedDriverBinding(const PreparedDriverBinding&) = delete;
    PreparedDriverBinding& operator=(const PreparedDriverBinding&) = delete;

    ~PreparedDriverBinding() {
        Reset();
    }

    bool Reset() noexcept {
        if (!active) {
            return true;
        }
        const BOOL destroyed = SetupDiDestroyDriverInfoList(
            set, device, SPDIT_COMPATDRIVER);
        active = false;
        set = INVALID_HANDLE_VALUE;
        device = nullptr;
        selected = SP_DRVINFO_DATA_W{};
        return destroyed != FALSE;
    }
};

bool PreparePreinstalledDriverOnDevice(
    HDEVINFO set,
    SP_DEVINFO_DATA* device,
    const PackageInfo& publishedPackage,
    PreparedDriverBinding* prepared,
    Error* error) {
    if (prepared == nullptr || prepared->active ||
        set == INVALID_HANDLE_VALUE || device == nullptr) {
        return SetError(error, L"repair-prepare-driver-binding",
            ERROR_INVALID_PARAMETER);
    }
    if (!SetupDiBuildDriverInfoList(set, device, SPDIT_COMPATDRIVER)) {
        return SetLastErrorDetail(error, L"repair-build-compatible-driver-list");
    }
    prepared->set = set;
    prepared->device = device;
    prepared->active = true;

    SP_DRVINFO_DATA_W selected{};
    size_t exactMatches = 0;
    for (DWORD index = 0;; ++index) {
        SP_DRVINFO_DATA_W driver{};
        driver.cbSize = sizeof(driver);
        if (!SetupDiEnumDriverInfoW(set, device, SPDIT_COMPATDRIVER, index, &driver)) {
            if (GetLastError() != ERROR_NO_MORE_ITEMS) {
                const DWORD code = GetLastError();
                prepared->Reset();
                return SetError(error, L"repair-enumerate-compatible-driver", code);
            }
            break;
        }
        DWORD required = 0;
        SP_DRVINFO_DETAIL_DATA_W probe{};
        probe.cbSize = sizeof(probe);
        if (!SetupDiGetDriverInfoDetailW(
                set, device, &driver, &probe, sizeof(probe), &required) &&
            GetLastError() != ERROR_INSUFFICIENT_BUFFER) {
            const DWORD code = GetLastError();
            prepared->Reset();
            return SetError(error, L"repair-compatible-driver-detail", code);
        }
        const DWORD detailBytes = std::max<DWORD>(
            required, static_cast<DWORD>(sizeof(SP_DRVINFO_DETAIL_DATA_W)));
        std::vector<BYTE> detailBuffer(detailBytes);
        auto* detail = reinterpret_cast<SP_DRVINFO_DETAIL_DATA_W*>(detailBuffer.data());
        detail->cbSize = sizeof(SP_DRVINFO_DETAIL_DATA_W);
        if (!SetupDiGetDriverInfoDetailW(
                set, device, &driver, detail, detailBytes, nullptr)) {
            const DWORD code = GetLastError();
            prepared->Reset();
            return SetError(error, L"repair-compatible-driver-detail", code);
        }
        if (DriverInfoUsesPublishedPackage(
                detail->InfFileName, publishedPackage.publishedName)) {
            selected = driver;
            ++exactMatches;
        }
    }
    if (exactMatches != 1) {
        prepared->Reset();
        return SetError(error, L"repair-exact-driver-selection",
            exactMatches == 0 ? ERROR_NOT_FOUND : ERROR_DUPLICATE_SERVICE_NAME,
            L"compatible driver list must contain exactly one node for the exact preinstalled package");
    }
    prepared->selected = selected;
    return true;
}

bool CommitPreparedDriverBinding(
    PreparedDriverBinding* prepared,
    uint64_t transactionDeadlineUnixMs,
    bool* mutationStarted,
    bool* rebootRequired,
    Error* error) {
    if (prepared == nullptr || !prepared->active ||
        prepared->set == INVALID_HANDLE_VALUE || prepared->device == nullptr ||
        prepared->selected.cbSize != sizeof(SP_DRVINFO_DATA_W)) {
        return SetError(error, L"repair-commit-driver-binding",
            ERROR_INVALID_PARAMETER);
    }
    if (transactionDeadlineUnixMs != 0 &&
        !CheckTransactionDeadline(transactionDeadlineUnixMs,
            L"transaction-deadline-before-selected-device-binding", error)) {
        return false;
    }
    MarkTransactionMutationStarted();
    if (mutationStarted != nullptr) {
        *mutationStarted = true;
    }
    if (!SetupDiSetSelectedDriverW(
            prepared->set, prepared->device, &prepared->selected)) {
        const DWORD code = GetLastError();
        prepared->Reset();
        return SetError(error, L"repair-select-exact-driver", code);
    }
    BOOL reboot = FALSE;
    if (!RecordActiveInstallJournalCutpoint(
            InstallJournalPhase::DiInstallEntered, true, ERROR_SUCCESS,
            false, error)) {
        prepared->Reset();
        return false;
    }
    const BOOL installed = InvokeAuthoritativeSynchronousMutation(
        transactionDeadlineUnixMs, L"DiInstallDevice", [&]() {
            return DiInstallDevice(nullptr, prepared->set, prepared->device,
                &prepared->selected, 0, &reboot);
        });
    const DWORD installError = installed ? ERROR_SUCCESS : GetLastError();
    const bool combinedRebootRequired =
        *rebootRequired || reboot != FALSE;
    *rebootRequired = combinedRebootRequired;
    Error journalReturnError;
    const bool journalReturnRecorded =
        RecordActiveInstallJournalCutpointWithReboot(
        InstallJournalPhase::DiInstallReturned, installed != FALSE,
        installError, gLastSynchronousMutationTimedOut,
        combinedRebootRequired, reboot != FALSE,
        &journalReturnError);
    if (!installed) {
        const DWORD code = installError;
        prepared->Reset();
        if (!journalReturnRecorded) {
            *error = std::move(journalReturnError);
            return false;
        }
        return SetError(error, L"repair-install-preinstalled-driver", code);
    }
    if (!prepared->Reset()) {
        return SetLastErrorDetail(error, L"repair-destroy-compatible-driver-list");
    }
    if (!journalReturnRecorded) {
        *error = std::move(journalReturnError);
        return false;
    }
    if (gLastSynchronousMutationTimedOut) {
        return SetError(error, L"repair-install-preinstalled-driver-timeout",
            ERROR_TIMEOUT,
            L"DiInstallDevice exceeded the transaction deadline; its authoritative return was retained for rollback");
    }
    return true;
}

bool InstallPreinstalledDriverOnDevice(
    HDEVINFO set,
    SP_DEVINFO_DATA* device,
    const PackageInfo& publishedPackage,
    uint64_t transactionDeadlineUnixMs,
    bool* mutationStarted,
    bool* rebootRequired,
    Error* error) {
    PreparedDriverBinding prepared;
    return PreparePreinstalledDriverOnDevice(
               set, device, publishedPackage, &prepared, error) &&
        CommitPreparedDriverBinding(
            &prepared, transactionDeadlineUnixMs, mutationStarted,
            rebootRequired, error);
}

bool IsGeneratedRootInstanceIdForDeviceName(
    const std::wstring& instanceId,
    const wchar_t* deviceName) {
    const std::wstring prefix = std::wstring(L"ROOT\\") + deviceName + L"\\";
    if (instanceId.size() != prefix.size() + 4 ||
        _wcsnicmp(instanceId.c_str(), prefix.c_str(), prefix.size()) != 0) {
        return false;
    }
    for (size_t index = prefix.size(); index < instanceId.size(); ++index) {
        if (instanceId[index] < L'0' || instanceId[index] > L'9') {
            return false;
        }
    }
    return true;
}

bool IsOwnedGeneratedRootInstanceId(const std::wstring& instanceId) {
    return IsGeneratedRootInstanceIdForDeviceName(instanceId, kRootDeviceName) ||
        IsGeneratedRootInstanceIdForDeviceName(instanceId, kLegacyRootDeviceName);
}

bool RegisterRootDeviceExact(
    const GUID& classGuid,
    const std::wstring& instanceId,
    uint64_t transactionDeadlineUnixMs,
    bool* mutationStarted,
    bool* registrationSucceeded,
    DeviceInfoSet* set,
    SP_DEVINFO_DATA* data,
    Error* error) {
    if (registrationSucceeded != nullptr) {
        *registrationSucceeded = false;
    }
    if (!IsOwnedGeneratedRootInstanceId(instanceId)) {
        return SetError(error, L"rollback-instance-id",
            ERROR_INVALID_DATA,
            L"captured root devnode identity is outside the VIIPER or legacy generated root namespace");
    }
    *set = DeviceInfoSet(SetupDiCreateDeviceInfoList(&classGuid, nullptr));
    if (!*set) {
        return SetLastErrorDetail(error, L"rollback-create-device-info-list");
    }
    *data = SP_DEVINFO_DATA{};
    data->cbSize = sizeof(*data);
    // With DICD_GENERATE_ID absent, SetupAPI treats DeviceName as the complete
    // device instance ID. Rollback must never substitute a fresh ROOT instance.
    if (!SetupDiCreateDeviceInfoW(set->get(), instanceId.c_str(), &classGuid,
            nullptr, nullptr, 0, data)) {
        return SetLastErrorDetail(error, L"rollback-create-exact-root-devnode");
    }
    const size_t idCharacters = std::size(kHardwareId) + 1;
    std::vector<wchar_t> identifiers(idCharacters, L'\0');
    std::copy(std::begin(kHardwareId), std::end(kHardwareId), identifiers.begin());
    if (transactionDeadlineUnixMs != 0 &&
        !CheckTransactionDeadline(transactionDeadlineUnixMs,
            L"rollback-deadline-before-root-properties", error)) {
        return false;
    }
    if (!RecordActiveInstallJournalCutpoint(
            InstallJournalPhase::RootRegistrationEntered,
            true, ERROR_SUCCESS, false, error)) {
        return false;
    }
    MarkTransactionMutationStarted();
    if (mutationStarted != nullptr) {
        *mutationStarted = true;
    }
    if (!SetupDiSetDeviceRegistryPropertyW(set->get(), data, SPDRP_HARDWAREID,
            reinterpret_cast<const BYTE*>(identifiers.data()),
            static_cast<DWORD>(identifiers.size() * sizeof(wchar_t)))) {
        const DWORD code = GetLastError();
        Error journalError;
        if (!RecordActiveInstallJournalCutpoint(
                InstallJournalPhase::RootRegistrationReturned,
                false, code, false, &journalError)) {
            *error = std::move(journalError);
            return false;
        }
        return SetError(error, L"rollback-set-root-hardware-id", code);
    }
    if (transactionDeadlineUnixMs != 0 &&
        !CheckTransactionDeadline(transactionDeadlineUnixMs,
            L"rollback-deadline-before-root-registration", error)) {
        return false;
    }
    MarkTransactionMutationStarted();
    if (mutationStarted != nullptr) {
        *mutationStarted = true;
    }
    const BOOL registered = InvokeAuthoritativeSynchronousMutation(
        transactionDeadlineUnixMs,
        L"SetupDiCallClassInstaller(DIF_REGISTERDEVICE)", [&]() {
            return SetupDiCallClassInstaller(
                DIF_REGISTERDEVICE, set->get(), data);
        });
    const DWORD registerError = registered ? ERROR_SUCCESS : GetLastError();
    Error journalReturnError;
    const bool journalReturnRecorded = RecordActiveInstallJournalCutpoint(
        InstallJournalPhase::RootRegistrationReturned,
        registered != FALSE, registerError, gLastSynchronousMutationTimedOut,
        &journalReturnError);
    if (!registered) {
        if (!journalReturnRecorded) {
            *error = std::move(journalReturnError);
            return false;
        }
        return SetError(error, L"rollback-register-exact-root-devnode",
            registerError);
    }
    if (registrationSucceeded != nullptr) {
        *registrationSucceeded = true;
    }
    if (!journalReturnRecorded) {
        *error = std::move(journalReturnError);
        return false;
    }
    if (gLastSynchronousMutationTimedOut) {
        return SetError(error, L"rollback-register-exact-root-devnode-timeout",
            ERROR_TIMEOUT,
            L"exact root registration exceeded the rollback deadline; its authoritative return was retained");
    }
    return true;
}

bool IssueAbiNegotiation(
    HANDLE device,
    uint64_t deadlineUnixMs,
    VIIPER_UDE_UINT16 abiMinor,
    VIIPER_UDE_UINT32 requestedCapabilities,
    VIIPER_UDE_NEGOTIATE_RESPONSE* response,
    VIIPER_UDE_UINT64* clientNonce,
    DWORD* returnedBytes,
    Error* error) {
    if (response == nullptr || clientNonce == nullptr || returnedBytes == nullptr) {
        return SetError(error, L"abi-negotiate-arguments", ERROR_INVALID_PARAMETER);
    }
    LARGE_INTEGER counter{};
    QueryPerformanceCounter(&counter);
    VIIPER_UDE_NEGOTIATE_REQUEST request{};
    request.Header.Magic = VIIPER_UDE_MAGIC;
    request.Header.Major = VIIPER_UDE_ABI_MAJOR;
    request.Header.Minor = abiMinor;
    request.Header.Size = sizeof(request);
    request.ClientNonce = static_cast<VIIPER_UDE_UINT64>(counter.QuadPart) ^ GetTickCount64();
    if (request.ClientNonce == 0) request.ClientNonce = 1;
    request.RequestedCapabilities = requestedCapabilities;
    *response = VIIPER_UDE_NEGOTIATE_RESPONSE{};
    DWORD returned = 0;
    WinHandle event(CreateEventW(nullptr, TRUE, FALSE, nullptr));
    if (!event) {
        return SetLastErrorDetail(error, L"abi-negotiate-event");
    }
    OVERLAPPED overlapped{};
    overlapped.hEvent = event.get();
    const BOOL completed = DeviceIoControl(device, IOCTL_VIIPER_UDE_NEGOTIATE,
        &request, sizeof(request), response, sizeof(*response), &returned, &overlapped);
    if (!completed && GetLastError() != ERROR_IO_PENDING) {
        return SetLastErrorDetail(error, L"abi-negotiate");
    }
    if (!completed) {
        const uint64_t now = CurrentUnixMilliseconds();
        if (deadlineUnixMs <= now) {
            const BOOL cancelled = CancelIoEx(device, &overlapped);
            const DWORD cancelError = cancelled ? ERROR_SUCCESS : GetLastError();
            const DWORD drain = WaitForSingleObject(event.get(), kCancelledIoDrainMs);
            if ((!cancelled && cancelError != ERROR_NOT_FOUND) || drain != WAIT_OBJECT_0) {
                return SetError(error, L"abi-negotiate-drain",
                    !cancelled && cancelError != ERROR_NOT_FOUND
                        ? cancelError : ERROR_OPERATION_ABORTED,
                    L"expired native ABI negotiation could not be cancelled and drained safely");
            }
            DWORD ignored = 0;
            GetOverlappedResult(device, &overlapped, &ignored, FALSE);
            return SetError(error, L"abi-negotiate-timeout", ERROR_TIMEOUT,
                L"native ABI negotiation exceeded the package transaction deadline");
        }
        const uint64_t remaining = deadlineUnixMs - now;
        const DWORD waitMilliseconds = static_cast<DWORD>(
            std::min<uint64_t>(remaining, static_cast<uint64_t>(MAXDWORD - 1)));
        const DWORD wait = WaitForSingleObject(event.get(), waitMilliseconds);
        if (wait == WAIT_TIMEOUT) {
            const BOOL cancelled = CancelIoEx(device, &overlapped);
            const DWORD cancelError = cancelled ? ERROR_SUCCESS : GetLastError();
            const DWORD drain = WaitForSingleObject(event.get(), kCancelledIoDrainMs);
            if (drain == WAIT_OBJECT_0) {
                DWORD ignored = 0;
                GetOverlappedResult(device, &overlapped, &ignored, FALSE);
            }
            if (!cancelled && cancelError != ERROR_NOT_FOUND) {
                return SetError(error, L"abi-negotiate-cancel", cancelError,
                    L"timed-out native ABI negotiation could not be cancelled");
            }
            if (drain != WAIT_OBJECT_0) {
                return SetError(error, L"abi-negotiate-drain", ERROR_OPERATION_ABORTED,
                    L"timed-out native ABI negotiation did not complete cancellation within the rollback ceiling");
            }
            return SetError(error, L"abi-negotiate-timeout", ERROR_TIMEOUT,
                L"native ABI negotiation exceeded the package transaction deadline");
        }
        if (wait != WAIT_OBJECT_0) {
            const DWORD waitError = GetLastError();
            CancelIoEx(device, &overlapped);
            const DWORD drain = WaitForSingleObject(event.get(), kCancelledIoDrainMs);
            if (drain != WAIT_OBJECT_0) {
                return SetError(error, L"abi-negotiate-drain", ERROR_OPERATION_ABORTED,
                    L"failed native ABI wait could not be drained safely");
            }
            SetLastError(waitError);
            return SetLastErrorDetail(error, L"abi-negotiate-wait");
        }
        if (!GetOverlappedResult(device, &overlapped, &returned, FALSE)) {
            return SetLastErrorDetail(error, L"abi-negotiate-result");
        }
    }
    *clientNonce = request.ClientNonce;
    *returnedBytes = returned;
    return true;
}

enum class AbiHealthPurpose {
    ExactCandidate,
    PristineUpgrade,
    PristineRecheck,
    RollbackHealth,
};

bool IsAbiRetryEligible(
    AbiHealthPurpose purpose,
    const std::string* expectedBuildIdentity,
    const Error& error) {
    return purpose == AbiHealthPurpose::PristineUpgrade &&
        expectedBuildIdentity == nullptr &&
        (error.code == ERROR_REVISION_MISMATCH ||
            error.code == ERROR_INVALID_PARAMETER) &&
        (error.phase == L"abi-negotiate" ||
            error.phase == L"abi-negotiate-result");
}

bool RuntimeStatsArePristine(
    const VIIPER_UDE_STATS& stats,
    const AbiCompatibilityProfile& profile) noexcept {
    return stats.OperationsDequeued == 0 && stats.OperationsCompleted == 0 &&
        stats.OperationsCancelled == 0 && stats.OperationsPurged == 0 &&
        stats.LateCompletions == 0 && stats.InvalidMessages == 0 &&
        stats.QueueExhaustions == 0 && stats.IsoPackets == 0 &&
        stats.BytesToDevice == 0 && stats.BytesFromDevice == 0 &&
        stats.NotificationEvents == 0 && stats.NotificationEventOverflows == 0 &&
        stats.ActiveDevices == 0 && stats.PendingOperations == 0 &&
        stats.WaitingDequeues == 0 && stats.CleanupRetries == 0 &&
        stats.InputReportsSubmitted == 0 && stats.InputReportsCompleted == 0 &&
        (!profile.hasReservedPortFields || stats.ReservedPorts == 0);
}

bool AbiNegotiationResponseMatchesProfile(
    const VIIPER_UDE_NEGOTIATE_RESPONSE& response,
    DWORD returned,
    VIIPER_UDE_UINT64 clientNonce,
    const AbiCompatibilityProfile& profile) noexcept {
    return returned == sizeof(response) &&
        response.Header.Magic == VIIPER_UDE_MAGIC &&
        response.Header.Major == VIIPER_UDE_ABI_MAJOR &&
        response.Header.Minor == profile.minor &&
        response.Header.Size == sizeof(response) && response.Header.Flags == 0 &&
        response.ClientNonce == clientNonce && response.DriverNonce != 0 &&
        response.Capabilities == profile.capabilities &&
        response.MaxDevices == VIIPER_UDE_MAX_DEVICES &&
        response.MaxDescriptorBytes == VIIPER_UDE_MAX_DESCRIPTOR_BYTES &&
        response.MaxTransferBytes == VIIPER_UDE_MAX_TRANSFER_BYTES &&
        response.MaxIsoPackets == VIIPER_UDE_MAX_ISO_PACKETS &&
        response.MaxPendingOperations == VIIPER_UDE_MAX_PENDING_OPERATIONS;
}

bool StatsRecordMatchesProfile(
    const VIIPER_UDE_STATS& stats,
    DWORD returned,
    const AbiCompatibilityProfile& profile) noexcept {
    return returned == profile.statsSize &&
        stats.Header.Magic == VIIPER_UDE_MAGIC &&
        stats.Header.Major == VIIPER_UDE_ABI_MAJOR &&
        stats.Header.Minor == profile.minor &&
        stats.Header.Size == profile.statsSize && stats.Header.Flags == 0 &&
        (!profile.hasReservedPortFields ||
            (stats.ReservedPorts <= VIIPER_UDE_MAX_DEVICES && stats.Reserved == 0));
}

bool VerifyAbiHealth(
    uint64_t deadlineUnixMs,
    const std::string* expectedBuildIdentity,
    Error* error,
    AbiHealthPurpose purpose = AbiHealthPurpose::ExactCandidate,
    const AbiCompatibilityProfile* requiredProfile = nullptr,
    AbiCompatibilityProfile* negotiatedProfile = nullptr) {
    const bool requiresKnownProfile =
        purpose == AbiHealthPurpose::PristineRecheck ||
        purpose == AbiHealthPurpose::RollbackHealth;
    if ((requiresKnownProfile && requiredProfile == nullptr) ||
        (!requiresKnownProfile && requiredProfile != nullptr) ||
        (purpose != AbiHealthPurpose::ExactCandidate &&
            expectedBuildIdentity != nullptr)) {
        return SetError(error, L"abi-health-purpose", ERROR_INVALID_PARAMETER,
            L"ABI health purpose and compatibility profile are inconsistent");
    }
    DeviceInfoSet set(SetupDiGetClassDevsW(
        &kViiperInterfaceGuid, nullptr, nullptr, DIGCF_PRESENT | DIGCF_DEVICEINTERFACE));
    if (!set) {
        return SetLastErrorDetail(error, L"abi-interface-enumeration");
    }
    std::wstring interfacePath;
    size_t exactCount = 0;
    for (DWORD index = 0;; ++index) {
        SP_DEVICE_INTERFACE_DATA interfaceData{};
        interfaceData.cbSize = sizeof(interfaceData);
        if (!SetupDiEnumDeviceInterfaces(set.get(), nullptr, &kViiperInterfaceGuid, index, &interfaceData)) {
            if (GetLastError() != ERROR_NO_MORE_ITEMS) {
                return SetLastErrorDetail(error, L"abi-interface-enumeration");
            }
            break;
        }
        SP_DEVINFO_DATA deviceData{};
        deviceData.cbSize = sizeof(deviceData);
        DWORD required = 0;
        SetupDiGetDeviceInterfaceDetailW(
            set.get(), &interfaceData, nullptr, 0, &required, &deviceData);
        if (required == 0 || GetLastError() != ERROR_INSUFFICIENT_BUFFER) {
            return SetLastErrorDetail(error, L"abi-interface-detail");
        }
        std::vector<BYTE> buffer(required);
        auto* detail = reinterpret_cast<SP_DEVICE_INTERFACE_DETAIL_DATA_W*>(buffer.data());
        detail->cbSize = sizeof(SP_DEVICE_INTERFACE_DETAIL_DATA_W);
        if (!SetupDiGetDeviceInterfaceDetailW(
                set.get(), &interfaceData, detail, required, nullptr, &deviceData)) {
            return SetLastErrorDetail(error, L"abi-interface-detail");
        }
        if (!HasExactHardwareId(set.get(), deviceData)) {
            continue;
        }
        std::wstring service;
        if (!ReadService(set.get(), deviceData, &service, error)) {
            return false;
        }
        if (_wcsicmp(service.c_str(), kServiceName) != 0) {
            return SetError(error, L"abi-interface-ownership", ERROR_ACCESS_DENIED);
        }
        ++exactCount;
        interfacePath = detail->DevicePath;
    }
    if (exactCount != 1) {
        return SetError(error, L"abi-interface-count",
            exactCount == 0 ? ERROR_DEVICE_NOT_AVAILABLE : ERROR_DUPLICATE_SERVICE_NAME);
    }
    WinHandle device(CreateFileW(interfacePath.c_str(), GENERIC_READ | GENERIC_WRITE,
        0, nullptr, OPEN_EXISTING, FILE_ATTRIBUTE_NORMAL | FILE_FLAG_OVERLAPPED, nullptr));
    if (!device) {
        return SetLastErrorDetail(error, L"abi-interface-open",
            L"native broker interface is unavailable or still owned by another process");
    }
    const AbiCompatibilityProfile* profiles = nullptr;
    size_t profileCount = 0;
    bool requirePristineRuntime = false;
    switch (purpose) {
    case AbiHealthPurpose::ExactCandidate:
        profiles = &kAbiCompatibilityProfiles[0];
        profileCount = 1;
        break;
    case AbiHealthPurpose::PristineUpgrade:
        profiles = kAbiCompatibilityProfiles.data();
        profileCount = kAbiCompatibilityProfiles.size();
        requirePristineRuntime = true;
        break;
    case AbiHealthPurpose::PristineRecheck:
        profiles = requiredProfile;
        profileCount = 1;
        requirePristineRuntime = true;
        break;
    case AbiHealthPurpose::RollbackHealth:
        profiles = requiredProfile;
        profileCount = 1;
        requirePristineRuntime = true;
        break;
    }

    VIIPER_UDE_NEGOTIATE_RESPONSE response{};
    VIIPER_UDE_UINT64 clientNonce = 0;
    DWORD returned = 0;
    const AbiCompatibilityProfile* selectedProfile = nullptr;
    for (size_t index = 0; index < profileCount; ++index) {
        response = VIIPER_UDE_NEGOTIATE_RESPONSE{};
        clientNonce = 0;
        returned = 0;
        if (IssueAbiNegotiation(device.get(), deadlineUnixMs, profiles[index].minor,
                profiles[index].capabilities, &response, &clientNonce, &returned, error)) {
            selectedProfile = &profiles[index];
            break;
        }
        if (index + 1 == profileCount ||
            !IsAbiRetryEligible(purpose, expectedBuildIdentity, *error)) {
            return false;
        }
        *error = Error{};
    }
    if (selectedProfile == nullptr) {
        return SetError(error, L"abi-negotiate", ERROR_REVISION_MISMATCH,
            L"no compatible native driver ABI profile was negotiated");
    }

    std::string loadedBuildIdentity;
    loadedBuildIdentity.reserve(VIIPER_UDE_BUILD_IDENTITY_BYTES * 2);
    static constexpr char digits[] = "0123456789abcdef";
    for (VIIPER_UDE_UINT8 byte : response.BuildIdentity) {
        loadedBuildIdentity.push_back(digits[byte >> 4U]);
        loadedBuildIdentity.push_back(digits[byte & 0x0fU]);
    }
    if (!AbiNegotiationResponseMatchesProfile(
            response, returned, clientNonce, *selectedProfile) ||
        (expectedBuildIdentity != nullptr &&
            loadedBuildIdentity != *expectedBuildIdentity)) {
        return SetError(error, L"abi-negotiate", ERROR_REVISION_MISMATCH,
            L"loaded driver health response does not match the source-bound package identity");
    }
    if (requirePristineRuntime) {
        VIIPER_UDE_STATS stats{};
        DWORD statsReturned = 0;
        WinHandle statsEvent(CreateEventW(nullptr, TRUE, FALSE, nullptr));
        if (!statsEvent) {
            return SetLastErrorDetail(error, L"upgrade-pristine-stats-event");
        }
        OVERLAPPED statsOverlapped{};
        statsOverlapped.hEvent = statsEvent.get();
        const BOOL statsCompleted = DeviceIoControl(
            device.get(), IOCTL_VIIPER_UDE_QUERY_STATS,
            nullptr, 0, &stats, sizeof(stats), &statsReturned, &statsOverlapped);
        if (!statsCompleted && GetLastError() != ERROR_IO_PENDING) {
            return SetLastErrorDetail(error, L"upgrade-pristine-stats");
        }
        if (!statsCompleted) {
            const uint64_t now = CurrentUnixMilliseconds();
            if (deadlineUnixMs <= now) {
                const BOOL cancelled = CancelIoEx(device.get(), &statsOverlapped);
                const DWORD cancelError = cancelled ? ERROR_SUCCESS : GetLastError();
                const DWORD drain = WaitForSingleObject(statsEvent.get(), kCancelledIoDrainMs);
                if ((!cancelled && cancelError != ERROR_NOT_FOUND) || drain != WAIT_OBJECT_0) {
                    return SetError(error, L"upgrade-pristine-stats-drain",
                        !cancelled && cancelError != ERROR_NOT_FOUND
                            ? cancelError : ERROR_OPERATION_ABORTED,
                        L"expired pristine-runtime query could not be cancelled and drained safely");
                }
                DWORD ignored = 0;
                GetOverlappedResult(device.get(), &statsOverlapped, &ignored, FALSE);
                return SetError(error, L"upgrade-pristine-stats-timeout", ERROR_TIMEOUT,
                    L"pristine-runtime query exceeded the package transaction deadline");
            }
            const uint64_t remaining = deadlineUnixMs - now;
            const DWORD waitMilliseconds = static_cast<DWORD>(
                std::min<uint64_t>(remaining, static_cast<uint64_t>(MAXDWORD - 1)));
            const DWORD wait = WaitForSingleObject(statsEvent.get(), waitMilliseconds);
            if (wait == WAIT_TIMEOUT) {
                const BOOL cancelled = CancelIoEx(device.get(), &statsOverlapped);
                const DWORD cancelError = cancelled ? ERROR_SUCCESS : GetLastError();
                const DWORD drain = WaitForSingleObject(statsEvent.get(), kCancelledIoDrainMs);
                if (drain == WAIT_OBJECT_0) {
                    DWORD ignored = 0;
                    GetOverlappedResult(device.get(), &statsOverlapped, &ignored, FALSE);
                }
                if (!cancelled && cancelError != ERROR_NOT_FOUND) {
                    return SetError(error, L"upgrade-pristine-stats-cancel", cancelError,
                        L"timed-out pristine-runtime query could not be cancelled");
                }
                if (drain != WAIT_OBJECT_0) {
                    return SetError(error, L"upgrade-pristine-stats-drain",
                        ERROR_OPERATION_ABORTED,
                        L"timed-out pristine-runtime query did not drain safely");
                }
                return SetError(error, L"upgrade-pristine-stats-timeout", ERROR_TIMEOUT,
                    L"pristine-runtime query exceeded the package transaction deadline");
            }
            if (wait != WAIT_OBJECT_0) {
                const DWORD waitError = GetLastError();
                CancelIoEx(device.get(), &statsOverlapped);
                const DWORD drain = WaitForSingleObject(statsEvent.get(), kCancelledIoDrainMs);
                if (drain != WAIT_OBJECT_0) {
                    return SetError(error, L"upgrade-pristine-stats-drain",
                        ERROR_OPERATION_ABORTED,
                        L"failed pristine-runtime wait could not be drained safely");
                }
                SetLastError(waitError);
                return SetLastErrorDetail(error, L"upgrade-pristine-stats-wait");
            }
            if (!GetOverlappedResult(
                    device.get(), &statsOverlapped, &statsReturned, FALSE)) {
                return SetLastErrorDetail(error, L"upgrade-pristine-stats-result");
            }
        }
        if (!StatsRecordMatchesProfile(stats, statsReturned, *selectedProfile)) {
            return SetError(error, L"upgrade-pristine-stats", ERROR_REVISION_MISMATCH,
                L"loaded driver returned an invalid pristine-runtime statistics record");
        }
        if (!RuntimeStatsArePristine(stats, *selectedProfile)) {
            return SetError(error, L"upgrade-runtime-reboot-boundary",
                ERROR_SUCCESS_REBOOT_REQUIRED,
                L"the loaded native bus has serviced virtual-device work since boot; restart Windows and rerun the identical package command before creating another virtual device");
        }
    }
    if (negotiatedProfile != nullptr) {
        *negotiatedProfile = *selectedProfile;
    }
    return true;
}

bool VerifyInstalledBinding(
    const PackageInfo& candidate,
    const std::wstring& publishedName,
    bool allowStopped,
    Error* error) {
    Snapshot snapshot;
    if (!CaptureSnapshot(&snapshot, error)) {
        return false;
    }
    if (snapshot.devices.size() != 1 || !snapshot.devices[0].present ||
        _wcsicmp(snapshot.devices[0].publishedInf.c_str(), publishedName.c_str()) != 0 ||
        !(snapshot.devices[0].version == candidate.version) ||
        !SamePackageBytes(snapshot.devices[0].package, candidate)) {
        return SetError(error, L"install-verification", ERROR_REVISION_MISMATCH,
            L"installed devnode is not bound to the exact candidate package");
    }
    if (!allowStopped && !snapshot.devices[0].started) {
        return SetError(error, L"install-start", ERROR_DEVICE_NOT_AVAILABLE,
            L"installed driver did not start; problem=" + std::to_wstring(snapshot.devices[0].problem));
    }
    return true;
}

bool VerifyInstalled(
    const PackageInfo& candidate,
    const std::wstring& publishedName,
    bool allowStopped,
    uint64_t healthDeadlineUnixMs,
    const std::string* expectedBuildIdentity,
    Error* error) {
    return VerifyInstalledBinding(candidate, publishedName, allowStopped, error) &&
        (allowStopped || VerifyAbiHealth(
            healthDeadlineUnixMs, expectedBuildIdentity, error));
}

bool UninstallPackage(const PackageInfo& package, bool* rebootRequired, Error* error) {
    BOOL reboot = FALSE;
    MarkTransactionMutationStarted();
    if (!DiUninstallDriverW(nullptr, package.infPath.c_str(), 0, &reboot)) {
        return SetLastErrorDetail(error, L"remove-driver-package");
    }
    *rebootRequired = *rebootRequired || reboot != FALSE;
    return true;
}

bool SamePackageInventory(
    const std::vector<PackageInfo>& left,
    const std::vector<PackageInfo>& right) noexcept {
    if (left.size() != right.size()) {
        return false;
    }
    for (size_t index = 0; index < left.size(); ++index) {
        if (_wcsicmp(left[index].publishedName.c_str(),
                right[index].publishedName.c_str()) != 0 ||
            !(left[index].version == right[index].version) ||
            !SamePackageBytes(left[index], right[index])) {
            return false;
        }
    }
    return true;
}

bool ContainsExactPackage(
    const std::vector<PackageInfo>& packages,
    const PackageInfo& candidate) noexcept {
    return std::any_of(packages.begin(), packages.end(),
        [&](const PackageInfo& package) {
            return package.version == candidate.version &&
                SamePackageBytes(package, candidate);
        });
}

bool VerifyPackageInventory(
    const std::vector<PackageInfo>& expected,
    const wchar_t* phase,
    Error* error) {
    std::vector<PackageInfo> observed;
    if (!EnumerateOwnedPackages(&observed, error)) {
        return false;
    }
    if (!SamePackageInventory(expected, observed)) {
        return SetError(error, phase, ERROR_REVISION_MISMATCH,
            L"the exact captured Driver Store inventory changed during the protected transaction");
    }
    return true;
}

bool RemoveStagedCandidateExact(
    const PackageInfo& stagedCandidate,
    uint64_t rollbackDeadlineUnixMs,
    Error* error) {
    if (!IsSafePublishedInfName(stagedCandidate.publishedName)) {
        return SetError(error, L"rollback-staged-package-identity", ERROR_INVALID_NAME,
            L"the staged-here candidate lacks a safe exact published INF identity");
    }
    std::vector<PackageInfo> current;
    if (!EnumerateOwnedPackages(&current, error)) {
        return false;
    }
    size_t matches = 0;
    for (const PackageInfo& package : current) {
        if (_wcsicmp(package.publishedName.c_str(),
                stagedCandidate.publishedName.c_str()) == 0) {
            if (!(package.version == stagedCandidate.version) ||
                !SamePackageBytes(package, stagedCandidate)) {
                return SetError(error, L"rollback-staged-package-identity",
                    ERROR_REVISION_MISMATCH,
                    L"the staged-here published INF no longer matches the exact candidate");
            }
            ++matches;
        }
    }
    if (matches != 1) {
        return SetError(error, L"rollback-staged-package-identity",
            matches == 0 ? ERROR_NOT_FOUND : ERROR_DUPLICATE_SERVICE_NAME,
            L"rollback requires exactly one matching staged-here published INF");
    }
    Snapshot topology;
    if (!CaptureSnapshot(&topology, error)) {
        return false;
    }
    for (const DeviceState& device : topology.devices) {
        if (_wcsicmp(device.publishedInf.c_str(),
                stagedCandidate.publishedName.c_str()) == 0) {
            return SetError(error, L"rollback-staged-package-in-use",
                ERROR_DEVICE_IN_USE,
                L"rollback refuses to remove a staged-here package still bound to a root device");
        }
    }
    if (!CheckTransactionDeadline(rollbackDeadlineUnixMs,
            L"install-rollback-deadline-staged-package", error)) {
        return false;
    }
    if (!RecordActiveInstallJournalCutpoint(
            InstallJournalPhase::SetupUninstallEntered,
            true, ERROR_SUCCESS, false, error)) {
        return false;
    }
    MarkTransactionMutationStarted();
    const BOOL removed = InvokeAuthoritativeSynchronousMutation(
        rollbackDeadlineUnixMs, L"SetupUninstallOEMInfW", [&]() {
            return SetupUninstallOEMInfW(
                stagedCandidate.publishedName.c_str(), 0, nullptr);
        });
    const DWORD removeError = removed ? ERROR_SUCCESS : GetLastError();
    Error journalReturnError;
    const bool journalReturnRecorded = RecordActiveInstallJournalCutpoint(
        InstallJournalPhase::SetupUninstallReturned,
        removed != FALSE, removeError, gLastSynchronousMutationTimedOut,
        &journalReturnError);
    if (!removed) {
        if (!journalReturnRecorded) {
            *error = std::move(journalReturnError);
            return false;
        }
        return SetError(error, L"rollback-staged-package-remove", removeError,
            L"the exact unbound staged-here candidate could not be removed");
    }
    if (!journalReturnRecorded) {
        *error = std::move(journalReturnError);
        return false;
    }
    if (gLastSynchronousMutationTimedOut) {
        return SetError(error, L"rollback-staged-package-remove-timeout",
            ERROR_TIMEOUT,
            L"SetupUninstallOEMInfW exceeded the rollback deadline; its authoritative return was retained");
    }
    return true;
}

bool RestorePriorBinding(
    const Snapshot& prior,
    uint64_t transactionDeadlineUnixMs,
    bool* rebootRequired,
    Error* error) {
    if (prior.devices.size() > 1) {
        return SetError(error, L"rollback-topology", ERROR_DUPLICATE_SERVICE_NAME,
            L"rollback refuses an unsupported multi-devnode native topology");
    }

    Snapshot current;
    if (!CaptureSnapshot(&current, error) || current.devices.size() > 1) {
        if (error->code == ERROR_SUCCESS) {
            SetError(error, L"rollback-topology", ERROR_DUPLICATE_SERVICE_NAME,
                L"rollback observed an unexpected multi-devnode native topology");
        }
        return false;
    }
    const auto sameIdentity = [](const std::wstring& left, const std::wstring& right) {
        return _wcsicmp(left.c_str(), right.c_str()) == 0;
    };
    const bool keepCurrent = !prior.devices.empty() && !current.devices.empty() &&
        sameIdentity(prior.devices[0].instanceId, current.devices[0].instanceId);

    if (!current.devices.empty() && !keepCurrent) {
        DeviceInfoSet set = OpenRootDevices();
        if (!set) {
            return SetLastErrorDetail(error, L"rollback-open-root-devices");
        }
        std::vector<std::pair<SP_DEVINFO_DATA, DeviceState>> matches;
        if (!FindExactDevices(set.get(), &matches, error) || matches.size() != 1) {
            if (error->code == ERROR_SUCCESS) {
                SetError(error, L"rollback-topology", ERROR_REVISION_MISMATCH);
            }
            return false;
        }
        bool removalReboot = false;
        if (!RemoveDevice(set.get(), matches[0].first, transactionDeadlineUnixMs,
                L"rollback-deadline-before-device-removal", nullptr,
                &removalReboot, error)) {
            return false;
        }
        *rebootRequired = *rebootRequired || removalReboot;
    }
    if (prior.devices.empty()) {
        Snapshot restored;
        if (!CaptureSnapshot(&restored, error) || !restored.devices.empty()) {
            if (error->code == ERROR_SUCCESS) {
                SetError(error, L"rollback-identity-verification", ERROR_REVISION_MISMATCH,
                    L"rollback did not restore the captured empty devnode topology");
            }
            return false;
        }
        return true;
    }

    const DeviceState& expected = prior.devices[0];
    const PackageInfo& package = expected.package;
    DeviceInfoSet target;
    SP_DEVINFO_DATA targetData{};
    targetData.cbSize = sizeof(targetData);
    if (!keepCurrent) {
        GUID classGuid{};
        wchar_t className[MAX_CLASS_NAME_LEN]{};
        if (!SetupDiGetINFClassW(package.infPath.c_str(), &classGuid, className,
                MAX_CLASS_NAME_LEN, nullptr)) {
            return SetLastErrorDetail(error, L"rollback-inf-class");
        }
        if (!RegisterRootDeviceExact(classGuid, expected.instanceId,
                transactionDeadlineUnixMs, nullptr, nullptr,
                &target, &targetData, error)) {
            return false;
        }
    } else {
        target = OpenRootDevices();
        if (!target) {
            return SetLastErrorDetail(error, L"rollback-open-retained-root-device");
        }
        std::vector<std::pair<SP_DEVINFO_DATA, DeviceState>> matches;
        if (!FindExactDevices(target.get(), &matches, error) || matches.size() != 1 ||
            !sameIdentity(matches[0].second.instanceId, expected.instanceId)) {
            if (error->code == ERROR_SUCCESS) {
                SetError(error, L"rollback-retained-root-identity", ERROR_REVISION_MISMATCH);
            }
            return false;
        }
        targetData = matches[0].first;
    }
    if (!InstallPreinstalledDriverOnDevice(
            target.get(), &targetData, package, transactionDeadlineUnixMs, nullptr,
            rebootRequired, error)) {
        return false;
    }

    Snapshot restored;
    if (!CaptureSnapshot(&restored, error) || restored.devices.size() != 1 ||
        !sameIdentity(restored.devices[0].instanceId, expected.instanceId) ||
        !SamePackageBytes(restored.devices[0].package, expected.package)) {
        if (error->code == ERROR_SUCCESS) {
            SetError(error, L"rollback-identity-verification", ERROR_REVISION_MISMATCH,
                L"rollback did not restore the exact captured devnode identity and package binding");
        }
        return false;
    }
    return true;
}

bool RollbackInstall(
    const Snapshot& prior,
    const PackageInfo* stagedHereCandidate,
    bool bindingMutationStarted,
    const AbiCompatibilityProfile* priorAbiProfile,
    uint64_t rollbackDeadlineUnixMs,
    bool* rebootRequired,
    Error* error) {
    if (!CheckTransactionDeadline(rollbackDeadlineUnixMs,
            L"install-rollback-deadline-root", error)) {
        return false;
    }
    if (bindingMutationStarted) {
        if (!RestorePriorBinding(
                prior, rollbackDeadlineUnixMs, rebootRequired, error)) {
            return false;
        }
    } else if (!CaptureAndVerifyRootUnchanged(
                   prior, L"rollback-stage-root-invariance", nullptr, error)) {
        return false;
    }
    if (stagedHereCandidate != nullptr &&
        !RemoveStagedCandidateExact(
            *stagedHereCandidate, rollbackDeadlineUnixMs, error)) {
        return false;
    }
    if (!CheckTransactionDeadline(rollbackDeadlineUnixMs,
            L"install-rollback-deadline-inventory", error)) {
        return false;
    }
    if (!VerifyPackageInventory(
            prior.packages, L"rollback-package-inventory", error)) {
        return false;
    }
    if (bindingMutationStarted && !prior.devices.empty() && !*rebootRequired) {
        Snapshot restored;
        if (!CaptureSnapshot(&restored, error) || restored.devices.size() != 1 ||
            !SameRootBinding(prior.devices[0], restored.devices[0])) {
            if (error->code == ERROR_SUCCESS) {
                SetError(error, L"rollback-runtime-binding-verification",
                    ERROR_REVISION_MISMATCH,
                    L"rollback did not preserve the exact captured root and package identity");
            }
            return false;
        }
        if (prior.devices[0].started) {
            if (!RollbackLifecycleStateMatches(
                    prior.devices[0], restored.devices[0])) {
                return SetError(error, L"rollback-runtime-start-verification",
                    ERROR_DEVICE_NOT_AVAILABLE,
                    L"rollback did not restore the formerly-running root to started/problem-zero state");
            }
            if (priorAbiProfile == nullptr) {
                return SetError(error, L"rollback-runtime-abi-profile",
                    ERROR_REVISION_MISMATCH,
                    L"rollback lacks the exact known-compatible ABI profile captured before binding");
            }
            return VerifyAbiHealth(
                rollbackDeadlineUnixMs, nullptr, error,
                AbiHealthPurpose::RollbackHealth, priorAbiProfile, nullptr);
        }
        if (!RollbackLifecycleStateMatches(
                prior.devices[0], restored.devices[0])) {
            return SetError(error, L"rollback-stopped-state-verification",
                ERROR_DEVICE_NOT_AVAILABLE,
                L"rollback changed the captured stopped/problem lifecycle state");
        }
    }
    return true;
}

bool LockPackageFiles(
    const std::filesystem::path& directory,
    std::vector<WinHandle>* locks,
    Error* error) {
    locks->clear();
    for (const wchar_t* name : {L"ViiperUde.inf", L"ViiperUde.sys", L"ViiperUde.cat"}) {
        WinHandle file(CreateFileW((directory / name).c_str(), GENERIC_READ, FILE_SHARE_READ,
            nullptr, OPEN_EXISTING, FILE_ATTRIBUTE_NORMAL | FILE_FLAG_OPEN_REPARSE_POINT, nullptr));
        if (!file) {
            return SetLastErrorDetail(error, L"package-lock",
                L"INF, SYS, and CAT must exist and remain immutable during installation");
        }
        FILE_ATTRIBUTE_TAG_INFO attributes{};
        if (!GetFileInformationByHandleEx(file.get(), FileAttributeTagInfo,
                &attributes, sizeof(attributes)) ||
            (attributes.FileAttributes &
                (FILE_ATTRIBUTE_DIRECTORY | FILE_ATTRIBUTE_REPARSE_POINT)) != 0) {
            return SetError(error, L"package-lock", ERROR_REPARSE_TAG_MISMATCH,
                L"package inputs must be regular non-reparse files");
        }
        locks->push_back(std::move(file));
    }
    return true;
}

bool ValidateExactPackageDirectory(const std::filesystem::path& directory, Error* error) {
    static const std::set<std::wstring> expected = {
        L"ViiperUde.inf", L"ViiperUde.sys", L"ViiperUde.cat"};
    const DWORD attributes = GetFileAttributesW(directory.c_str());
    if (attributes == INVALID_FILE_ATTRIBUTES ||
        (attributes & FILE_ATTRIBUTE_DIRECTORY) == 0 ||
        (attributes & FILE_ATTRIBUTE_REPARSE_POINT) != 0) {
        return SetError(error, L"package-directory", ERROR_REPARSE_TAG_MISMATCH,
            L"signed package path must be a regular non-reparse directory");
    }
    std::set<std::wstring> seen;
    std::error_code enumerationError;
    for (std::filesystem::directory_iterator iterator(directory, enumerationError), end;
         !enumerationError && iterator != end; iterator.increment(enumerationError)) {
        std::error_code typeError;
        if (!iterator->is_regular_file(typeError) || typeError ||
            !expected.contains(iterator->path().filename().wstring()) ||
            !seen.insert(iterator->path().filename().wstring()).second) {
            return SetError(error, L"package-directory", ERROR_INVALID_DATA,
                L"signed runtime package directory must contain only INF, SYS, and CAT");
        }
    }
    if (enumerationError || seen != expected) {
        return SetError(error, L"package-directory", ERROR_INVALID_DATA,
            L"signed package directory changed or is incomplete");
    }
    return true;
}

struct InstallOptions {
    std::filesystem::path infPath;
    std::filesystem::path manifestPath;
    std::string manifestSha256;
    std::string sourceRevision;
    std::string expectedInfSha256;
    std::string expectedSysSha256;
    std::string expectedCatSha256;
    bool production = true;
    bool localTest = false;
    std::optional<Version> expectedDowngradeFrom;
    std::filesystem::path brokerExecutable;
    std::string brokerSha256;
    std::filesystem::path brokerToken;
    std::string brokerTokenSha256;
    std::wstring targetUserSid;
    uint64_t transactionDeadlineUnixMs = 0;
    HANDLE brokerQuiesceRequest = nullptr;
    HANDLE brokerQuiesceReady = nullptr;
    HANDLE brokerQuiesceAbort = nullptr;
    HANDLE brokerHandoff = nullptr;
};

struct BrokerCommitProof;
struct InstallJournalStateData;

class InstallJournal final {
public:
    InstallJournal();
    ~InstallJournal();
    InstallJournal(const InstallJournal&) = delete;
    InstallJournal& operator=(const InstallJournal&) = delete;

    bool Prepare(
        const Snapshot& prior,
        const PackageInfo& candidate,
        const std::filesystem::path& candidateDirectory,
        const std::vector<PackageInfo>& expectedInventory,
        const InstallOptions& options,
        Error* error);
    bool Record(
        InstallJournalPhase phase,
        const PackageInfo* publishedCandidate,
        bool packageStagedHere,
        bool bindingMutationStarted,
        bool rebootRequired,
        bool callSucceeded,
        DWORD callError,
        bool deadlineOverrun,
        Error* error);
    bool RecordCutpoint(
        InstallJournalPhase phase,
        bool callSucceeded,
        DWORD callError,
        bool deadlineOverrun,
        bool rebootRequired,
        bool freshRebootRequired,
        Error* error);
    bool RecordAuthoritativeReturn(
        InstallJournalPhase phase,
        const PackageInfo* publishedCandidate,
        bool packageStagedHere,
        bool bindingMutationStarted,
        bool rebootRequired,
        bool freshRebootRequired,
        bool callSucceeded,
        DWORD callError,
        bool deadlineOverrun,
        Error* error);
    bool RecordPriorAbiProfile(
        const AbiCompatibilityProfile& profile,
        const PackageInfo& publishedCandidate,
        bool packageStagedHere,
        Error* error);
    bool RecordRootRegistrationIntent(
        const std::wstring& instanceId,
        Error* error);
    bool RecordBrokerProof(
        const BrokerCommitProof& proof,
        Error* error);
    bool RecordRollbackAuthorization(
        InstallJournalPhase phase,
        DWORD callError,
        Error* error);
    bool RetireAfterForwardValidation(
        const PackageInfo& candidate,
        const std::wstring& publishedName,
        bool rebootRequired,
        uint64_t deadlineUnixMs,
        Error* error);
    bool RetireAfterPriorValidation(
        bool rebootRequired,
        Error* error);
    bool RemoveAuthorizedPriorEmptyRootAfterAdmission(
        uint64_t rollbackDeadlineUnixMs,
        bool* rebootRequired,
        bool* rootRemovalRebootPending,
        Error* error);
    bool VerifyPriorTopologyBeforePackageRollback(Error* error) const;
    void AttachEvidence(Error* error) const;

private:
    bool RecordNext(
        InstallJournalStateData next,
        InstallJournalPhase phase,
        const PackageInfo* publishedCandidate,
        bool packageStagedHere,
        bool bindingMutationStarted,
        bool rebootRequired,
        bool callSucceeded,
        DWORD callError,
        bool deadlineOverrun,
        bool freshRebootRequired,
        Error* error);
    struct Impl;
    std::unique_ptr<Impl> impl_;
};

InstallJournal* gActiveInstallJournal = nullptr;

class ActiveInstallJournalScope final {
public:
    explicit ActiveInstallJournalScope(InstallJournal* journal) noexcept
        : prior_(gActiveInstallJournal) {
        gActiveInstallJournal = journal;
    }
    ~ActiveInstallJournalScope() {
        gActiveInstallJournal = prior_;
    }
    ActiveInstallJournalScope(const ActiveInstallJournalScope&) = delete;
    ActiveInstallJournalScope& operator=(const ActiveInstallJournalScope&) = delete;

private:
    InstallJournal* prior_;
};

bool ReconcileInstallJournal(
    bool explicitRecovery,
    uint64_t deadlineUnixMs,
    Outcome* outcome);

uint64_t CurrentUnixMilliseconds() {
    FILETIME now{};
    GetSystemTimeAsFileTime(&now);
    ULARGE_INTEGER ticks{};
    ticks.LowPart = now.dwLowDateTime;
    ticks.HighPart = now.dwHighDateTime;
    constexpr uint64_t windowsToUnixEpochTicks = 116444736000000000ULL;
    return ticks.QuadPart <= windowsToUnixEpochTicks
        ? 0 : (ticks.QuadPart - windowsToUnixEpochTicks) / 10000ULL;
}

bool CheckTransactionDeadline(const InstallOptions& options, const wchar_t* phase, Error* error) {
    if (options.transactionDeadlineUnixMs == 0 ||
        CurrentUnixMilliseconds() >= options.transactionDeadlineUnixMs) {
        return SetError(error, phase, ERROR_TIMEOUT,
            L"native package transaction deadline expired before the next mutation");
    }
    return true;
}

bool CheckTransactionDeadline(uint64_t deadlineUnixMs, const wchar_t* phase, Error* error) {
    if (deadlineUnixMs == 0 || CurrentUnixMilliseconds() >= deadlineUnixMs) {
        return SetError(error, phase, ERROR_TIMEOUT,
            L"native package transaction deadline expired before the next mutation");
    }
    return true;
}

bool ValidateTransactionDeadlineBudget(const InstallOptions& options, Error* error) {
    const uint64_t now = CurrentUnixMilliseconds();
    if (options.transactionDeadlineUnixMs <= now ||
        options.transactionDeadlineUnixMs - now > kMaximumTransactionDurationMs) {
        return SetError(error, L"transaction-deadline", ERROR_INVALID_PARAMETER,
            L"transaction deadline is expired or exceeds the four-minute package budget");
    }
    return true;
}

bool RequestBrokerQuiescence(const InstallOptions& options, Error* error) {
    if (options.brokerQuiesceRequest == nullptr ||
        options.brokerQuiesceReady == nullptr ||
        options.brokerQuiesceAbort == nullptr) {
        return SetError(error, L"broker-quiescence-handles", ERROR_INVALID_HANDLE,
            L"driver mutation requires the inherited broker quiescence handshake");
    }
    if (!CheckTransactionDeadline(
            options, L"transaction-deadline-before-broker-quiescence", error)) {
        return false;
    }
    if (!RecordActiveInstallJournalCutpoint(
            InstallJournalPhase::QuiesceSignalEntered,
            true, ERROR_SUCCESS, false, error)) {
        return false;
    }
    if (!SetEvent(options.brokerQuiesceRequest)) {
        const DWORD code = GetLastError();
        Error journalError;
        if (!RecordActiveInstallJournalCutpoint(
                InstallJournalPhase::QuiesceSignalReturned,
                false, code, false, &journalError)) {
            *error = std::move(journalError);
            return false;
        }
        return SetError(error, L"broker-quiescence-request", code);
    }
    if (!RecordActiveInstallJournalCutpoint(
            InstallJournalPhase::QuiesceSignalReturned,
            true, ERROR_SUCCESS, false, error)) {
        return false;
    }
    const std::array<HANDLE, 2> responses{
        options.brokerQuiesceReady, options.brokerQuiesceAbort,
    };
    for (;;) {
        const uint64_t now = CurrentUnixMilliseconds();
        if (now >= options.transactionDeadlineUnixMs) {
            return SetError(error, L"broker-quiescence-timeout", ERROR_TIMEOUT,
                L"native broker did not prove quiescence before the package deadline");
        }
        const DWORD waitMilliseconds = static_cast<DWORD>(std::min<uint64_t>(
            options.transactionDeadlineUnixMs - now,
            std::numeric_limits<DWORD>::max() - 1ULL));
        const DWORD wait = WaitForMultipleObjects(
            static_cast<DWORD>(responses.size()), responses.data(), FALSE, waitMilliseconds);
        if (wait == WAIT_OBJECT_0) {
            return true;
        }
        if (wait == WAIT_OBJECT_0 + 1) {
            return SetError(error, L"broker-quiescence-aborted", ERROR_OPERATION_ABORTED,
                L"the outer package transaction could not safely quiesce the native broker service");
        }
        if (wait == WAIT_TIMEOUT) {
            continue;
        }
        if (wait == WAIT_FAILED) {
            return SetLastErrorDetail(error, L"broker-quiescence-wait");
        }
        return SetError(error, L"broker-quiescence-wait", ERROR_INVALID_HANDLE,
            L"broker quiescence wait returned an unexpected event state");
    }
}

bool SignalBrokerHandoff(const InstallOptions& options, Error* error) {
    if (options.brokerHandoff == nullptr) {
        return SetError(error, L"broker-handoff-handle", ERROR_INVALID_HANDLE,
            L"authenticated broker commit requires the inherited service-lock handoff");
    }
    if (!CheckTransactionDeadline(
            options, L"transaction-deadline-before-broker-handoff", error) ||
        !RecordActiveInstallJournalCutpoint(
            InstallJournalPhase::BrokerHandoffEntered,
            true, ERROR_SUCCESS, false, error)) {
        return false;
    }
    if (!SetEvent(options.brokerHandoff)) {
        const DWORD code = GetLastError();
        Error journalError;
        if (!RecordActiveInstallJournalRollbackAuthorization(
                InstallJournalPhase::BrokerHandoffReturned,
                code, &journalError)) {
            *error = std::move(journalError);
            return false;
        }
        return SetError(error, L"broker-handoff-signal", code);
    }
    return RecordActiveInstallJournalCutpoint(
        InstallJournalPhase::BrokerHandoffReturned,
        true, ERROR_SUCCESS, false, error);
}

bool ValidateTransactionDeadlineBudget(uint64_t deadlineUnixMs, Error* error) {
    const uint64_t now = CurrentUnixMilliseconds();
    if (deadlineUnixMs <= now || deadlineUnixMs - now > kMaximumTransactionDurationMs) {
        return SetError(error, L"transaction-deadline", ERROR_INVALID_PARAMETER,
            L"transaction deadline is expired or exceeds the four-minute package budget");
    }
    return true;
}

bool ValidateCandidateInputs(
    const InstallOptions& options,
    std::filesystem::path* packageDirectory,
    std::vector<WinHandle>* packageLocks,
    PackageInfo* candidate,
    Error* error) {
    if (!ValidateTransactionDeadlineBudget(options, error)) {
        return false;
    }
    std::error_code candidatePathError;
    const std::filesystem::path lockedInfPath =
        std::filesystem::canonical(options.infPath, candidatePathError);
    if (candidatePathError || lockedInfPath.filename().wstring() != L"ViiperUde.inf") {
        return SetError(error, L"package-path", ERROR_FILE_NOT_FOUND);
    }
    *packageDirectory = lockedInfPath.parent_path();
    if (!ValidateExactPackageDirectory(*packageDirectory, error) ||
        !LockPackageFiles(*packageDirectory, packageLocks, error)) {
        return false;
    }
    bool owned = false;
    if (!LoadOwnedPackage(lockedInfPath, true, options.localTest,
            candidate, &owned, error) || !owned ||
        (options.production && !VerifyMicrosoftHardwareInfSigner(lockedInfPath, error))) {
        return false;
    }
    if (_stricmp(candidate->infSha256.c_str(), options.expectedInfSha256.c_str()) != 0 ||
        _stricmp(candidate->sysSha256.c_str(), options.expectedSysSha256.c_str()) != 0 ||
        _stricmp(candidate->catSha256.c_str(), options.expectedCatSha256.c_str()) != 0) {
        return SetError(error, L"package-runtime-hash", ERROR_CRC,
            L"INF, SYS, or CAT does not match the installer-reviewed runtime package bytes");
    }
    WinHandle manifest(CreateFileW(options.manifestPath.c_str(), GENERIC_READ, FILE_SHARE_READ,
        nullptr, OPEN_EXISTING, FILE_ATTRIBUTE_NORMAL | FILE_FLAG_OPEN_REPARSE_POINT, nullptr));
    if (!manifest) {
        error->phase = L"manifest-installer-open";
        return SetLastErrorDetail(error, L"manifest-installer-open");
    }
    FILE_ATTRIBUTE_TAG_INFO manifestAttributes{};
    if (!GetFileInformationByHandleEx(manifest.get(), FileAttributeTagInfo,
            &manifestAttributes, sizeof(manifestAttributes)) ||
        (manifestAttributes.FileAttributes &
            (FILE_ATTRIBUTE_DIRECTORY | FILE_ATTRIBUTE_REPARSE_POINT)) != 0) {
        return SetError(error, L"manifest-installer-open", ERROR_REPARSE_TAG_MISMATCH,
            L"source-bound manifest must be a regular non-reparse file");
    }
    std::string actualManifestSha256;
    if (!Sha256Handle(manifest.get(), &actualManifestSha256, error)) {
        error->phase = L"manifest-installer-hash";
        return false;
    }
    if (_stricmp(actualManifestSha256.c_str(), options.manifestSha256.c_str()) != 0) {
        return SetError(error, L"manifest-installer-hash", ERROR_CRC,
            L"source-bound manifest does not match the installer-embedded SHA-256");
    }
    std::string manifestContents;
    if (!ReadSmallHandle(manifest.get(), &manifestContents, error)) {
        return false;
    }
    return ValidateManifest(manifestContents, options.sourceRevision, options.production,
        options.localTest,
        *packageDirectory, error) &&
        CheckTransactionDeadline(options, L"transaction-deadline-preflight", error);
}

Outcome Verify(const InstallOptions& options) {
    Outcome outcome;
    std::filesystem::path packageDirectory;
    std::vector<WinHandle> packageLocks;
    PackageInfo candidate;
    if (!ValidateCandidateInputs(
            options, &packageDirectory, &packageLocks, &candidate, &outcome.error)) {
        outcome.exitCode = ExitCode::PreflightRejected;
        return outcome;
    }
    outcome.success = true;
    outcome.exitCode = ExitCode::Success;
    return outcome;
}

bool IsSafeTargetUserSid(const std::wstring& sid) {
    return sid.size() >= 5 && sid.size() <= 184 &&
        (sid.starts_with(L"S-") || sid.starts_with(L"s-")) &&
        std::all_of(sid.begin() + 2, sid.end(), [](wchar_t value) {
            return (value >= L'0' && value <= L'9') || value == L'-';
        });
}

std::wstring QuoteWindowsArgument(const std::wstring& value) {
    std::wstring quoted(1, L'"');
    size_t backslashes = 0;
    for (const wchar_t character : value) {
        if (character == L'\\') {
            ++backslashes;
            continue;
        }
        if (character == L'"') {
            quoted.append(backslashes * 2 + 1, L'\\');
            quoted.push_back(L'"');
            backslashes = 0;
            continue;
        }
        quoted.append(backslashes, L'\\');
        backslashes = 0;
        quoted.push_back(character);
    }
    quoted.append(backslashes * 2, L'\\');
    quoted.push_back(L'"');
    return quoted;
}

std::wstring BuildBrokerCommitCommandLine(const InstallOptions& options) {
    return QuoteWindowsArgument(options.brokerExecutable.wstring()) +
        L" native-package-broker-commit --token-file " +
        QuoteWindowsArgument(options.brokerToken.wstring()) +
        L" --expected-token-sha-256 " +
        QuoteWindowsArgument(std::wstring(
            options.brokerTokenSha256.begin(), options.brokerTokenSha256.end())) +
        L" --expected-broker-sha-256 " +
        QuoteWindowsArgument(std::wstring(
            options.brokerSha256.begin(), options.brokerSha256.end())) +
        L" --target-user-sid " +
        QuoteWindowsArgument(options.targetUserSid) +
        L" --transaction-deadline-unix-ms " +
        QuoteWindowsArgument(std::to_wstring(options.transactionDeadlineUnixMs));
}

struct BrokerCommitProof {
    bool success = false;
    bool changed = false;
    std::string rollback;
    DWORD exitCode = ERROR_GEN_FAILURE;
    bool driverRollbackAuthorized = false;
    std::wstring diagnostic;
};

bool BrokerProofFieldsAreCanonical(
    bool success,
    bool changed,
    std::string_view rollback,
    DWORD exitCode,
    bool driverRollbackAuthorized) noexcept {
    return (success && !driverRollbackAuthorized &&
               rollback == "not-needed" && exitCode == 0U) ||
        (!success && !changed && driverRollbackAuthorized &&
            rollback == "not-needed" && exitCode == 4U) ||
        (!success && changed && driverRollbackAuthorized &&
            rollback == "succeeded" && exitCode == 1U) ||
        (!success && changed && !driverRollbackAuthorized &&
            rollback == "failed" && exitCode == 3U);
}

bool IsUnsafeBrokerDiagnosticCharacter(wchar_t value) {
    const uint32_t codePoint = static_cast<uint16_t>(value);
    // The outer structured result is consumed by installers and logs. Preserve
    // printable ASCII only; quotes and backslashes are escaped by std::quoted,
    // while every control, direction mark, separator, and non-ASCII glyph is
    // made visibly inert instead of being allowed to reshape that record.
    return codePoint < 0x20U || codePoint > 0x7eU;
}

bool SanitizeBrokerDiagnostic(
    std::string_view payload,
    std::wstring* diagnostic) {
    static_assert(kMaximumBrokerDiagnosticCharacters > 3U);
    if (diagnostic == nullptr || payload.empty() ||
        payload.size() > static_cast<size_t>(std::numeric_limits<int>::max())) {
        return false;
    }
    const int payloadBytes = static_cast<int>(payload.size());
    const int required = MultiByteToWideChar(
        CP_UTF8, MB_ERR_INVALID_CHARS, payload.data(), payloadBytes, nullptr, 0);
    if (required <= 0) {
        return false;
    }
    std::wstring converted(static_cast<size_t>(required), L'\0');
    if (MultiByteToWideChar(CP_UTF8, MB_ERR_INVALID_CHARS, payload.data(), payloadBytes,
            converted.data(), required) != required) {
        return false;
    }
    for (wchar_t& character : converted) {
        if (IsUnsafeBrokerDiagnosticCharacter(character)) {
            character = L'?';
        }
    }
    if (converted.size() > kMaximumBrokerDiagnosticCharacters) {
        converted.resize(kMaximumBrokerDiagnosticCharacters - 3U);
        converted.append(L"...");
    }
    *diagnostic = std::move(converted);
    return true;
}

bool ParseBrokerCommitProof(
    const std::string& output,
    DWORD processExitCode,
    BrokerCommitProof* proof,
    Error* error) {
    struct CanonicalProof {
        std::string_view record;
        bool success;
        bool changed;
        std::string_view rollback;
        DWORD exitCode;
        bool driverRollbackAuthorized;
    };
    static constexpr std::array<CanonicalProof, 5> canonicalProofs = {{
        {"result=success operation=native-package-broker-commit changed=0 rollback=not-needed exitCode=0",
            true, false, "not-needed", ERROR_SUCCESS, false},
        {"result=success operation=native-package-broker-commit changed=1 rollback=not-needed exitCode=0",
            true, true, "not-needed", ERROR_SUCCESS, false},
        {"result=error operation=native-package-broker-commit changed=0 rollback=not-needed exitCode=4",
            false, false, "not-needed", 4, true},
        {"result=error operation=native-package-broker-commit changed=1 rollback=succeeded exitCode=1",
            false, true, "succeeded", 1, true},
        {"result=error operation=native-package-broker-commit changed=1 rollback=failed exitCode=3",
            false, true, "failed", 3, false},
    }};
    std::optional<BrokerCommitProof> parsed;
    std::optional<std::wstring> diagnostic;
    bool diagnosticSeen = false;
    bool diagnosticRejected = false;
    size_t cursor = 0;
    while (cursor < output.size()) {
        const size_t newline = output.find('\n', cursor);
        const bool terminated = newline != std::string::npos;
        std::string line = output.substr(
            cursor, terminated ? newline - cursor : output.size() - cursor);
        if (!line.empty() && line.back() == '\r') {
            line.pop_back();
        }
        if (line.starts_with("result=")) {
            if (!terminated || parsed) {
                return SetError(error, L"broker-proof", ERROR_INVALID_DATA,
                    L"nested broker must emit exactly one newline-terminated canonical outcome");
            }
            const auto match = std::find_if(
                canonicalProofs.begin(), canonicalProofs.end(),
                [&](const CanonicalProof& candidate) {
                    return candidate.record == line;
                });
            if (match == canonicalProofs.end()) {
                return SetError(error, L"broker-proof", ERROR_INVALID_DATA,
                    L"nested broker outcome is not in canonical byte form");
            }
            parsed = BrokerCommitProof{
                match->success,
                match->changed,
                std::string(match->rollback),
                match->exitCode,
                match->driverRollbackAuthorized,
            };
        }
        if (line.starts_with(kBrokerDiagnosticPrefix)) {
            if (!terminated || diagnosticSeen) {
                diagnosticRejected = true;
                diagnostic.reset();
            } else {
                std::wstring sanitized;
                if (SanitizeBrokerDiagnostic(
                        std::string_view(line).substr(kBrokerDiagnosticPrefix.size()),
                        &sanitized)) {
                    diagnostic = std::move(sanitized);
                } else {
                    diagnosticRejected = true;
                }
            }
            diagnosticSeen = true;
        }
        if (!terminated) {
            break;
        }
        cursor = newline + 1;
    }
    if (!parsed || parsed->exitCode != processExitCode) {
        return SetError(error, L"broker-proof", ERROR_INVALID_DATA,
            L"nested broker process exit and structured outcome are missing or inconsistent");
    }
    // Diagnostics are never transaction authority. Ambiguous, malformed,
    // unterminated, or success-adjacent text is discarded; only the exact
    // canonical result above controls changed/rollback classification.
    if (!parsed->success && !diagnosticRejected && diagnostic) {
        parsed->diagnostic = std::move(*diagnostic);
    }
    *proof = std::move(*parsed);
    return true;
}

bool SetBrokerCommitFailure(const BrokerCommitProof& proof, Error* error) {
    std::wstring message = proof.driverRollbackAuthorized
        ? L"nested broker transaction failed after proving a settled state"
        : L"nested broker transaction failed with indeterminate service state";
    if (!proof.diagnostic.empty()) {
        message.append(L"; nested diagnostic: ");
        message.append(proof.diagnostic);
    }
    const wchar_t* phase = proof.changed ? L"broker-health" : L"broker-preflight";
    SetError(error, phase, ERROR_INSTALL_FAILURE, std::move(message));
    if (error != nullptr) {
        error->nestedExitCode = proof.exitCode;
    }
    return false;
}

bool DrainBrokerProofPipe(
    HANDLE pipe,
    std::string* output,
    bool* overflow,
    Error* error) {
    for (;;) {
        DWORD available = 0;
        if (!PeekNamedPipe(pipe, nullptr, 0, nullptr, &available, nullptr)) {
            const DWORD code = GetLastError();
            if (code == ERROR_BROKEN_PIPE) {
                return true;
            }
            return SetError(error, L"broker-proof-read", code);
        }
        if (available == 0) {
            return true;
        }
        std::array<char, 4096> buffer{};
        const DWORD requested = std::min<DWORD>(
            available, static_cast<DWORD>(buffer.size()));
        DWORD read = 0;
        if (!ReadFile(pipe, buffer.data(), requested, &read, nullptr)) {
            const DWORD code = GetLastError();
            if (code == ERROR_BROKEN_PIPE) {
                return true;
            }
            return SetError(error, L"broker-proof-read", code);
        }
        const size_t retained = std::min<size_t>(
            read, kMaximumBrokerProofBytes -
                std::min(output->size(), kMaximumBrokerProofBytes));
        output->append(buffer.data(), retained);
        if (retained != read) {
            *overflow = true;
        }
    }
}

bool RunBrokerInstall(
    const InstallOptions& options,
    bool* driverRollbackAuthorized,
    bool* brokerChanged,
    Error* error) {
    // Until CreateProcess succeeds, no nested SCM/image mutation can have
    // started, so the caller may safely restore its captured driver snapshot.
    *driverRollbackAuthorized = true;
    *brokerChanged = false;
    if (options.brokerExecutable.empty() || !options.brokerExecutable.is_absolute() ||
        options.brokerExecutable.filename().wstring() != L"viiper.exe" ||
        options.brokerToken.empty() || !options.brokerToken.is_absolute() ||
        options.brokerToken.extension().wstring() != L".token" ||
        options.brokerTokenSha256.size() != 64 ||
        !std::all_of(options.brokerTokenSha256.begin(), options.brokerTokenSha256.end(),
            [](unsigned char value) { return std::isxdigit(value) != 0; }) ||
        !IsSafeTargetUserSid(options.targetUserSid)) {
        return SetError(error, L"broker-arguments", ERROR_INVALID_PARAMETER,
            L"broker executable, protected transaction token, and target SID do not match the native package contract");
    }
    WinHandle broker(CreateFileW(options.brokerExecutable.c_str(),
        GENERIC_READ | FILE_READ_ATTRIBUTES, FILE_SHARE_READ, nullptr, OPEN_EXISTING,
        FILE_FLAG_OPEN_REPARSE_POINT, nullptr));
    if (!broker) {
        return SetLastErrorDetail(error, L"broker-open");
    }
    FILE_ATTRIBUTE_TAG_INFO attributes{};
    if (!GetFileInformationByHandleEx(broker.get(), FileAttributeTagInfo,
            &attributes, sizeof(attributes)) ||
        (attributes.FileAttributes & FILE_ATTRIBUTE_REPARSE_POINT) != 0 ||
        (attributes.FileAttributes & FILE_ATTRIBUTE_DIRECTORY) != 0) {
        return SetError(error, L"broker-path", ERROR_REPARSE_TAG_MISMATCH,
            L"broker executable must be a regular non-reparse file");
    }
    std::array<unsigned char, 2> header{};
    DWORD read = 0;
    if (!ReadFile(broker.get(), header.data(), static_cast<DWORD>(header.size()), &read, nullptr) ||
        read != static_cast<DWORD>(header.size()) || header[0] != 'M' || header[1] != 'Z') {
        return SetError(error, L"broker-image", ERROR_BAD_EXE_FORMAT,
            L"broker executable is not a Windows PE image");
    }
    std::string actualBrokerSha256;
    if (!Sha256Handle(broker.get(), &actualBrokerSha256, error)) {
        error->phase = L"broker-hash";
        return false;
    }
    if (_stricmp(actualBrokerSha256.c_str(), options.brokerSha256.c_str()) != 0) {
        return SetError(error, L"broker-hash", ERROR_CRC,
            L"staged native broker does not match the installer-bound SHA-256");
    }

    std::wstring commandLine = BuildBrokerCommitCommandLine(options);
    std::vector<wchar_t> mutableCommand(commandLine.begin(), commandLine.end());
    mutableCommand.push_back(L'\0');
    SECURITY_ATTRIBUTES inheritedSecurity{};
    inheritedSecurity.nLength = sizeof(inheritedSecurity);
    inheritedSecurity.bInheritHandle = TRUE;
    HANDLE rawProofRead = INVALID_HANDLE_VALUE;
    HANDLE rawProofWrite = INVALID_HANDLE_VALUE;
    if (!CreatePipe(
            &rawProofRead, &rawProofWrite, &inheritedSecurity, 0)) {
        return SetLastErrorDetail(error, L"broker-proof-pipe");
    }
    WinHandle proofRead(rawProofRead);
    WinHandle proofWrite(rawProofWrite);
    if (!SetHandleInformation(
            proofRead.get(), HANDLE_FLAG_INHERIT, 0)) {
        return SetLastErrorDetail(error, L"broker-proof-pipe-inheritance");
    }
    WinHandle nullInput(CreateFileW(
        L"NUL", GENERIC_READ, FILE_SHARE_READ | FILE_SHARE_WRITE,
        &inheritedSecurity, OPEN_EXISTING, FILE_ATTRIBUTE_NORMAL, nullptr));
    if (!nullInput) {
        return SetLastErrorDetail(error, L"broker-null-input");
    }

    SIZE_T attributeBytes = 0;
    InitializeProcThreadAttributeList(nullptr, 1, 0, &attributeBytes);
    if (attributeBytes == 0 || GetLastError() != ERROR_INSUFFICIENT_BUFFER) {
        return SetLastErrorDetail(error, L"broker-handle-list-size");
    }
    std::vector<BYTE> attributeStorage(attributeBytes);
    STARTUPINFOEXW startup{};
    startup.StartupInfo.cb = sizeof(startup);
    startup.StartupInfo.dwFlags = STARTF_USESTDHANDLES;
    startup.StartupInfo.hStdInput = nullInput.get();
    startup.StartupInfo.hStdOutput = proofWrite.get();
    startup.StartupInfo.hStdError = proofWrite.get();
    startup.lpAttributeList = reinterpret_cast<PPROC_THREAD_ATTRIBUTE_LIST>(
        attributeStorage.data());
    if (!InitializeProcThreadAttributeList(
            startup.lpAttributeList, 1, 0, &attributeBytes)) {
        return SetLastErrorDetail(error, L"broker-handle-list-init");
    }
    const auto deleteAttributeList = [&]() {
        DeleteProcThreadAttributeList(startup.lpAttributeList);
    };
    HANDLE inheritedHandles[] = {nullInput.get(), proofWrite.get()};
    if (!UpdateProcThreadAttribute(
            startup.lpAttributeList, 0, PROC_THREAD_ATTRIBUTE_HANDLE_LIST,
            inheritedHandles, sizeof(inheritedHandles), nullptr, nullptr)) {
        const DWORD code = GetLastError();
        deleteAttributeList();
        return SetError(error, L"broker-handle-list-update", code);
    }
    PROCESS_INFORMATION process{};
    if (!RecordActiveInstallJournalCutpoint(
            InstallJournalPhase::BrokerChildEntered,
            true, ERROR_SUCCESS, false, error)) {
        deleteAttributeList();
        return false;
    }
    if (!CreateProcessW(options.brokerExecutable.c_str(), mutableCommand.data(), nullptr, nullptr,
            TRUE, CREATE_NO_WINDOW | EXTENDED_STARTUPINFO_PRESENT, nullptr,
            options.brokerExecutable.parent_path().c_str(),
            &startup.StartupInfo, &process)) {
        const DWORD code = GetLastError();
        deleteAttributeList();
        Error journalError;
        if (!RecordActiveInstallJournalRollbackAuthorization(
                InstallJournalPhase::BrokerChildSettled,
                code, &journalError)) {
            *error = std::move(journalError);
            return false;
        }
        return SetError(error, L"broker-start", code);
    }
    MarkTransactionMutationStarted();
    deleteAttributeList();
    // From this point forward, only an exact child proof may authorize driver
    // rollback. A crash, malformed output, or late/indeterminate exit must not
    // compound an unknown SCM transaction with a second SetupAPI mutation.
    *driverRollbackAuthorized = false;
    WinHandle processHandle(process.hProcess);
    WinHandle threadHandle(process.hThread);
    proofWrite.reset();
    nullInput.reset();
    // The child shares the exact outer deadline and owns a separately bounded
    // rollback. Poll and drain rather than hard-terminate: cancellation is
    // cooperative, and this helper retains the driver mutex until the child
    // exits so a foreign helper cannot overlap an indeterminate SCM mutation.
    const uint64_t brokerCeiling =
        options.transactionDeadlineUnixMs + kBrokerRollbackCeilingMs;
    bool exceededCeiling = false;
    bool proofOverflow = false;
    bool proofReadFailed = false;
    bool waitFailed = false;
    Error proofReadError;
    Error waitError;
    std::string brokerOutput;
    for (;;) {
        if (!proofReadFailed && !DrainBrokerProofPipe(
                proofRead.get(), &brokerOutput, &proofOverflow, &proofReadError)) {
            proofReadFailed = true;
            proofRead.reset();
        }
        const uint64_t now = CurrentUnixMilliseconds();
        if (now >= brokerCeiling && !exceededCeiling) {
            exceededCeiling = true;
            std::wcerr
                << L"native broker exceeded its transaction and rollback deadline; "
                   L"retaining the driver transaction lock until the child exits\n";
        }
        const DWORD waitSlice = static_cast<DWORD>(
            exceededCeiling ? 250 : std::min<uint64_t>(250, brokerCeiling - now));
        const DWORD wait = WaitForSingleObject(processHandle.get(), waitSlice);
        if (wait == WAIT_OBJECT_0) {
            break;
        }
        if (wait != WAIT_TIMEOUT) {
            if (!waitFailed) {
                DWORD code = GetLastError();
                if (code == ERROR_SUCCESS) {
                    code = ERROR_GEN_FAILURE;
                }
                waitError = Error{code, L"broker-wait", FormatError(code)};
                waitFailed = true;
            }
            // A failed wait is ambiguous, not permission to release the driver
            // transaction mutex while the nested SCM child may still mutate.
            // Retain ownership and use the process exit query only as a
            // termination observation; the final outcome remains indeterminate.
            DWORD observedExit = STILL_ACTIVE;
            if (GetExitCodeProcess(processHandle.get(), &observedExit) &&
                observedExit != STILL_ACTIVE) {
                break;
            }
            Sleep(250);
        }
    }
    if (!proofReadFailed && !DrainBrokerProofPipe(
            proofRead.get(), &brokerOutput, &proofOverflow, &proofReadError)) {
        proofReadFailed = true;
    }
    DWORD exitCode = ERROR_GEN_FAILURE;
    if (!GetExitCodeProcess(processHandle.get(), &exitCode)) {
        return SetLastErrorDetail(error, L"broker-exit");
    }
    if (exceededCeiling) {
        return SetError(error, L"broker-wait-ceiling", ERROR_TIMEOUT,
            L"native broker exited only after its transaction and rollback deadline; external reconciliation is required");
    }
    if (waitFailed) {
        *error = std::move(waitError);
        return false;
    }
    if (proofReadFailed) {
        *error = std::move(proofReadError);
        return false;
    }
    if (proofOverflow) {
        return SetError(error, L"broker-proof", ERROR_BUFFER_OVERFLOW,
            L"nested broker output exceeded the bounded proof channel");
    }
    BrokerCommitProof proof;
    if (!ParseBrokerCommitProof(brokerOutput, exitCode, &proof, error)) {
        return false;
    }
    *driverRollbackAuthorized = proof.driverRollbackAuthorized;
    *brokerChanged = proof.changed;
    if (gActiveInstallJournal != nullptr &&
        !gActiveInstallJournal->RecordBrokerProof(proof, error)) {
        return false;
    }
    if (!proof.success) {
        return SetBrokerCommitFailure(proof, error);
    }
    return true;
}

Outcome Install(const InstallOptions& options) {
    Outcome outcome;
    if (!IsElevated()) {
        SetError(&outcome.error, L"elevation", ERROR_ELEVATION_REQUIRED);
        outcome.exitCode = ExitCode::PreflightRejected;
        return outcome;
    }
    TransactionMutex mutex;
    if (!mutex.Acquire(&outcome.error)) {
        outcome.exitCode = ExitCode::PreflightRejected;
        return outcome;
    }
    Outcome recoveryOutcome;
    if (!ReconcileInstallJournal(
            false, options.transactionDeadlineUnixMs, &recoveryOutcome)) {
        return recoveryOutcome;
    }
    std::filesystem::path packageDirectory;
    std::vector<WinHandle> packageLocks;
    PackageInfo candidate;
    if (!ValidateCandidateInputs(
            options, &packageDirectory, &packageLocks, &candidate, &outcome.error)) {
        outcome.exitCode = ExitCode::PreflightRejected;
        return outcome;
    }
    std::string expectedBuildIdentity;
    if (!DeriveDriverBuildIdentity(
            options.sourceRevision, &expectedBuildIdentity, &outcome.error)) {
        outcome.exitCode = ExitCode::PreflightRejected;
        return outcome;
    }
    if (!ValidateExactPackageDirectory(packageDirectory, &outcome.error) ||
        !CheckTransactionDeadline(options, L"transaction-deadline-before-driver", &outcome.error)) {
        outcome.exitCode = ExitCode::PreflightRejected;
        return outcome;
    }
    Snapshot prior;
    if (!CaptureSnapshot(&prior, &outcome.error)) {
        outcome.exitCode = ExitCode::PreflightRejected;
        return outcome;
    }
    if (prior.devices.size() > 1 ||
        (!prior.devices.empty() && !prior.devices[0].present)) {
        SetError(&outcome.error, L"install-topology", ERROR_DUPLICATE_SERVICE_NAME,
            L"installation requires zero devices or one present exact owned root devnode");
        outcome.exitCode = ExitCode::PreflightRejected;
        return outcome;
    }
    CandidateDisposition disposition = CandidateDisposition::InstallRequired;
    bool downgrade = false;
    if (!ClassifyCandidatePackage(
            candidate, prior.packages, options.expectedDowngradeFrom,
            &disposition, &downgrade, &outcome.error)) {
        outcome.exitCode = ExitCode::PreflightRejected;
        return outcome;
    }
    // Re-enumerate at the last possible point before SetupAPI reopens the
    // package paths. The four leaf handles already deny write/delete sharing.
    if (!ValidateExactPackageDirectory(packageDirectory, &outcome.error) ||
        !CheckTransactionDeadline(options, L"transaction-deadline-before-driver", &outcome.error)) {
        outcome.exitCode = ExitCode::PreflightRejected;
        return outcome;
    }
    PackageInfo publishedCandidate;
    std::vector<PackageInfo> expectedTransactionInventory = prior.packages;
    bool exactBindingHealthy = false;
    if (disposition == CandidateDisposition::Exact) {
        if (!FindPublishedCandidate(candidate, &publishedCandidate, &outcome.error) ||
            (options.production && !VerifyMicrosoftHardwareInfSigner(
                publishedCandidate.infPath, &outcome.error))) {
            outcome.exitCode = ExitCode::PreflightRejected;
            return outcome;
        }
        exactBindingHealthy = prior.devices.size() == 1 && prior.devices[0].present &&
            prior.devices[0].started &&
            _wcsicmp(prior.devices[0].publishedInf.c_str(),
                publishedCandidate.publishedName.c_str()) == 0 &&
            prior.devices[0].version == candidate.version &&
            SamePackageBytes(prior.devices[0].package, candidate);
    }

    InstallJournal installJournal;
    if (!installJournal.Prepare(
            prior, candidate, packageDirectory,
            expectedTransactionInventory, options, &outcome.error)) {
        outcome.exitCode = ExitCode::PreflightRejected;
        return outcome;
    }
    ActiveInstallJournalScope activeJournal(&installJournal);
    if (disposition == CandidateDisposition::Exact &&
        !installJournal.Record(
            InstallJournalPhase::Prepared, &publishedCandidate,
            false, false, false, true, ERROR_SUCCESS, false,
            &outcome.error)) {
        outcome.exitCode = ExitCode::PreflightRejected;
        return outcome;
    }

    // Same-version bytes are immutable. An exact package with a missing,
    // stopped, or stale binding may repair only the ROOT topology from the
    // already-published exact INF. It selects that preinstalled package for the
    // specific devnode and calls DiInstallDevice, so it cannot replace
    // same-version Driver Store content or auto-bind any other device.
    const bool driverMutation =
        RequiresDriverMutation(disposition, exactBindingHealthy);
    bool driverMutationStarted = false;
    bool packageStagedHere = false;
    bool bindingMutationStarted = false;
    std::optional<AbiCompatibilityProfile> priorAbiProfile;
    DeviceInfoSet created;
    SP_DEVINFO_DATA createdData{};
    createdData.cbSize = sizeof(createdData);
    bool registrationSucceeded = false;
    GUID candidateClassGuid{};
    wchar_t candidateClassName[MAX_CLASS_NAME_LEN]{};
    const bool needsRootRegistration = driverMutation && prior.devices.empty();
    if (needsRootRegistration &&
        !SetupDiGetINFClassW(candidate.infPath.c_str(), &candidateClassGuid,
            candidateClassName, MAX_CLASS_NAME_LEN, nullptr)) {
        SetLastErrorDetail(&outcome.error, L"candidate-inf-class");
        outcome.exitCode = ExitCode::PreflightRejected;
        return outcome;
    }

    // Import a new candidate with the add-only Driver Store API while the
    // captured root remains fully intact. Prove the unique published bytes,
    // catalog/signer, and unchanged root binding before asking the broker to
    // quiesce. Every failure after a successful import falls through to the
    // snapshot rollback path, which removes the new package and preserves the
    // prior binding.
    if (disposition == CandidateDisposition::InstallRequired) {
        const bool stageSucceeded = StageCandidatePackage(
                candidate, options.production, options.transactionDeadlineUnixMs,
                &driverMutationStarted, &packageStagedHere,
                &publishedCandidate, &outcome.error);
        const Error stageError = outcome.error;
        Error stageJournalError;
        const PackageInfo* stageReceipt =
            IsSafePublishedInfName(publishedCandidate.publishedName)
                ? &publishedCandidate : nullptr;
        const bool stageCallReturned =
            driverMutationStarted || stageReceipt != nullptr;
        if (stageCallReturned && !installJournal.Record(
                InstallJournalPhase::StageReceiptCaptured, stageReceipt,
                packageStagedHere, bindingMutationStarted,
                outcome.rebootRequired, stageSucceeded,
                stageSucceeded ? ERROR_SUCCESS : stageError.code,
                gLastSynchronousMutationTimedOut, &stageJournalError)) {
            outcome.error = std::move(stageJournalError);
        } else if (!stageSucceeded) {
            outcome.error = stageError;
        }
        if (!stageSucceeded) {
            // Exact staging proof recorded the failure.
        } else {
            if (!packageStagedHere &&
                !ContainsExactPackage(prior.packages, candidate)) {
                SetError(&outcome.error, L"stage-concurrent-publication", ERROR_RETRY,
                    L"an exact candidate appeared after the Driver Store snapshot; rerun the identical transaction");
            }
            if (outcome.error.code == ERROR_SUCCESS && packageStagedHere) {
                expectedTransactionInventory.push_back(publishedCandidate);
                std::sort(expectedTransactionInventory.begin(),
                    expectedTransactionInventory.end(),
                    [](const PackageInfo& left, const PackageInfo& right) {
                        return _wcsicmp(left.publishedName.c_str(),
                            right.publishedName.c_str()) < 0;
                    });
            }
            if (outcome.error.code == ERROR_SUCCESS &&
                !VerifyPackageInventory(expectedTransactionInventory,
                    L"stage-package-inventory-verification", &outcome.error)) {
                // Any concurrent package publication fails closed.
            } else if (outcome.error.code == ERROR_SUCCESS &&
                !CaptureAndVerifyRootUnchanged(
                    prior, L"stage-root-binding-verification",
                    nullptr, &outcome.error)) {
                // The candidate remained staged so common rollback can remove it.
            }
        }
        outcome.changed = outcome.changed || driverMutationStarted;
    }

    // The service mutex remains owned by the outer package transaction. Ask it
    // to stop only a trusted running broker after exact candidate publication
    // and root-invariance proof, then keep that mutex held across exact
    // selected-device binding and verification. This prevents the broker from
    // retaining a UdeCx handle across the package switch.
    if (outcome.error.code == ERROR_SUCCESS && driverMutation &&
        !options.brokerExecutable.empty() &&
        !RequestBrokerQuiescence(options, &outcome.error)) {
        // A newly staged package is a completed mutation and must take the
        // common rollback path even when broker quiescence fails.
    }

    if (outcome.error.code == ERROR_SUCCESS && driverMutation &&
        !VerifyPackageInventory(expectedTransactionInventory,
            L"post-quiescence-package-inventory-verification", &outcome.error)) {
        // Quiescence never authorizes concurrent Driver Store changes.
    }

    Snapshot preBinding;
    if (outcome.error.code == ERROR_SUCCESS && driverMutation &&
        !CaptureAndVerifyRootUnchanged(
            prior, L"post-quiescence-root-verification",
            &preBinding, &outcome.error)) {
        // Full identity and lifecycle state must still match immediately before
        // runtime admission and exact device binding.
    }

    // UdeCx child deletion is asynchronous. Older installed images could
    // release their logical device slot before framework teardown settled.
    // Once the trusted broker is stopped, require a zero-lifetime-work runtime
    // before any in-place package binding mutation of a running root.
    // A PnP-stopped exact owned root has no live UdeCx stack or ABI endpoint;
    // its captured devnode/package identity is already the quiescence proof.
    // Do not start an old driver solely to replace it. Exact binding and
    // rollback checks below still guard the stopped-root transaction. For a
    // running root, a restart resets these counters and guarantees that no
    // pre-replacement child object can survive into rebinding.
    const bool currentRootPresent = preBinding.devices.size() == 1 &&
        preBinding.devices[0].present;
    const bool requiresPristineRuntimeProof =
        outcome.error.code == ERROR_SUCCESS &&
        RequiresPristineRuntimeProof(
            disposition, exactBindingHealthy, currentRootPresent,
            currentRootPresent && preBinding.devices[0].started);
    if (outcome.error.code == ERROR_SUCCESS && requiresPristineRuntimeProof) {
        AbiCompatibilityProfile negotiatedProfile{};
        if (!VerifyAbiHealth(
                options.transactionDeadlineUnixMs, nullptr, &outcome.error,
                AbiHealthPurpose::PristineUpgrade, nullptr,
                &negotiatedProfile)) {
            if (outcome.error.code == ERROR_SUCCESS_REBOOT_REQUIRED) {
                outcome.rebootRequired = true;
            }
        } else {
            if (!installJournal.RecordPriorAbiProfile(
                    negotiatedProfile, publishedCandidate,
                    packageStagedHere, &outcome.error)) {
                // The exact compatibility profile must be durable before bind.
            } else {
                priorAbiProfile = negotiatedProfile;
            }
        }
    }

    if (outcome.error.code == ERROR_SUCCESS && driverMutation) {
        if (prior.devices.empty()) {
            const bool inventoryVerified = VerifyPackageInventory(
                expectedTransactionInventory,
                L"final-pre-bind-package-inventory-verification", &outcome.error);
            const bool registeredAndVerified = inventoryVerified && RegisterRootDevice(
                    candidateClassGuid, options.transactionDeadlineUnixMs,
                    &bindingMutationStarted, &registrationSucceeded,
                    &created, &createdData, &outcome.error);
            if (registeredAndVerified) {
                InstallPreinstalledDriverOnDevice(
                    created.get(), &createdData, publishedCandidate,
                    options.transactionDeadlineUnixMs, &bindingMutationStarted,
                    &outcome.rebootRequired, &outcome.error);
            }
        } else {
            DeviceInfoSet bindingSet = OpenRootDevices();
            std::vector<std::pair<SP_DEVINFO_DATA, DeviceState>> bindingDevices;
            PreparedDriverBinding prepared;
            if (!bindingSet) {
                SetLastErrorDetail(&outcome.error, L"binding-open-root-devices");
            } else if (!FindExactDevices(
                           bindingSet.get(), &bindingDevices, &outcome.error)) {
                // Exact enumeration recorded the failure.
            } else if (bindingDevices.size() != 1 ||
                !SameEnumeratedRootState(
                    bindingDevices[0].second, prior.devices[0])) {
                SetError(&outcome.error, L"binding-root-invariance",
                    ERROR_REVISION_MISMATCH,
                    L"the captured root identity or lifecycle state changed before compatible-driver preparation");
            } else if (!PreparePreinstalledDriverOnDevice(
                           bindingSet.get(), &bindingDevices[0].first,
                           publishedCandidate, &prepared, &outcome.error)) {
                // Exact compatible-driver selection recorded the failure.
            } else if (!CaptureAndVerifyRootUnchanged(
                           prior,
                           L"final-pre-bind-root-topology-verification",
                           nullptr, &outcome.error)) {
                // A fresh global set catches roots absent from the prepared set.
            } else if (!VerifyPackageInventory(expectedTransactionInventory,
                           L"final-pre-bind-package-inventory-verification",
                           &outcome.error)) {
                // No concurrent package can enter the selected-driver window.
            } else if (!CaptureAndVerifyPreparedRootUnchanged(
                           prior.devices[0], bindingSet.get(),
                           bindingDevices[0].first.DevInst,
                           L"final-pre-bind-root-verification", &outcome.error)) {
                // The final same-devnode proof includes exact package bytes.
            } else if (requiresPristineRuntimeProof &&
                (!priorAbiProfile.has_value() ||
                    !VerifyAbiHealth(
                        options.transactionDeadlineUnixMs, nullptr,
                        &outcome.error, AbiHealthPurpose::PristineRecheck,
                        priorAbiProfile.has_value()
                            ? &priorAbiProfile.value() : nullptr,
                        nullptr))) {
                if (outcome.error.code == ERROR_SUCCESS) {
                    SetError(&outcome.error, L"final-pre-bind-abi-profile",
                        ERROR_REVISION_MISMATCH,
                        L"the exact pre-quiescence ABI profile is unavailable for final pristine proof");
                } else if (outcome.error.code == ERROR_SUCCESS_REBOOT_REQUIRED) {
                    outcome.rebootRequired = true;
                }
            } else {
                CommitPreparedDriverBinding(
                    &prepared, options.transactionDeadlineUnixMs,
                    &bindingMutationStarted, &outcome.rebootRequired,
                    &outcome.error);
            }
        }
        driverMutationStarted = driverMutationStarted || bindingMutationStarted;
        outcome.changed = outcome.changed || bindingMutationStarted;
    }

    if (outcome.error.code == ERROR_SUCCESS) {
        CheckTransactionDeadline(options, L"transaction-deadline-before-verify", &outcome.error);
    }
    if (outcome.error.code == ERROR_SUCCESS &&
        !(options.brokerExecutable.empty()
            ? VerifyInstalled(candidate, publishedCandidate.publishedName,
                outcome.rebootRequired, options.transactionDeadlineUnixMs,
                &expectedBuildIdentity, &outcome.error)
            : VerifyInstalledBinding(candidate, publishedCandidate.publishedName,
                outcome.rebootRequired, &outcome.error))) {
        // Verification recorded the exact failure.
    }
    if (outcome.error.code == ERROR_SUCCESS && driverMutation &&
        !VerifyPackageInventory(expectedTransactionInventory,
            L"post-bind-package-inventory-verification", &outcome.error)) {
        // A concurrent package mutation invalidates the transaction outcome.
    }
    if (outcome.error.code == ERROR_SUCCESS &&
        !installJournal.Record(
            InstallJournalPhase::DriverValidated,
            IsSafePublishedInfName(publishedCandidate.publishedName)
                ? &publishedCandidate : nullptr,
            packageStagedHere, bindingMutationStarted,
            outcome.rebootRequired, true, ERROR_SUCCESS, false,
            &outcome.error)) {
        // A durable validation boundary is required before broker handoff.
    }
    const auto verifyPostAdmissionRollbackInventory =
        [&](const wchar_t* phase, Error* error) {
            std::vector<PackageInfo> exactInventory = prior.packages;
            if (packageStagedHere) {
                if (!IsSafePublishedInfName(
                        publishedCandidate.publishedName) ||
                    !SamePackageBytes(publishedCandidate, candidate) ||
                    !(publishedCandidate.version == candidate.version)) {
                    return SetError(error, phase, ERROR_INVALID_DATA,
                        L"staged-here rollback lacks its exact published candidate receipt");
                }
                if (!ContainsExactPackage(
                        exactInventory, publishedCandidate)) {
                    exactInventory.push_back(publishedCandidate);
                }
            }
            std::sort(exactInventory.begin(), exactInventory.end(),
                [](const PackageInfo& left,
                   const PackageInfo& right) {
                    return _wcsicmp(left.publishedName.c_str(),
                        right.publishedName.c_str()) < 0;
                });
            return VerifyPackageInventory(exactInventory, phase, error);
        };
    if (outcome.error.code != ERROR_SUCCESS && driverMutationStarted) {
        const Error installError = outcome.error;
        Error rollbackError;
        bool rollbackReboot = outcome.rebootRequired;
        const uint64_t rollbackDeadline =
            CurrentUnixMilliseconds() + kDriverRollbackCeilingMs;
        if (!installJournal.Record(
                InstallJournalPhase::RollbackBindingEntered,
                IsSafePublishedInfName(publishedCandidate.publishedName)
                    ? &publishedCandidate : nullptr,
                packageStagedHere, bindingMutationStarted,
                rollbackReboot, true, ERROR_SUCCESS, false,
                &rollbackError)) {
            outcome.rollback = L"failed";
            outcome.error = std::move(rollbackError);
            outcome.exitCode = ExitCode::RollbackFailed;
            return outcome;
        }
        if (!verifyPostAdmissionRollbackInventory(
                L"install-rollback-post-admission-inventory",
                &rollbackError)) {
            outcome.rollback = L"failed";
            outcome.rebootRequired = rollbackReboot;
            outcome.error = std::move(rollbackError);
            outcome.exitCode = ExitCode::RollbackFailed;
            return outcome;
        }
        const bool rollbackRebootAtAdmission = rollbackReboot;
        bool rootRemovalRebootPending = false;
        if (!installJournal.RemoveAuthorizedPriorEmptyRootAfterAdmission(
                rollbackDeadline, &rollbackReboot,
                &rootRemovalRebootPending,
                &rollbackError)) {
            outcome.rollback = L"failed";
            outcome.rebootRequired = rollbackReboot;
            outcome.error = std::move(rollbackError);
            outcome.exitCode = ExitCode::RollbackFailed;
            return outcome;
        }
        if (rootRemovalRebootPending) {
            outcome.rollback = L"not-needed";
            outcome.rebootRequired = true;
            SetError(&outcome.error,
                L"install-partial-root-removal-reboot-pending",
                ERROR_SUCCESS_REBOOT_REQUIRED,
                L"receipt-bound root removal requires a restart before package rollback can continue");
            outcome.exitCode = ExitCode::RebootRequired;
            return outcome;
        }
        if (!verifyPostAdmissionRollbackInventory(
                L"install-rollback-pre-package-inventory",
                &rollbackError) ||
            !installJournal.VerifyPriorTopologyBeforePackageRollback(
                &rollbackError)) {
            outcome.rollback = L"failed";
            outcome.rebootRequired = rollbackReboot;
            outcome.error = std::move(rollbackError);
            outcome.exitCode = ExitCode::RollbackFailed;
            return outcome;
        }
        const PackageInfo* stagedHereCandidate =
            packageStagedHere ? &publishedCandidate : nullptr;
        const bool restoreBindingThroughStrictSnapshot =
            !prior.devices.empty() && bindingMutationStarted;
        if (RollbackInstall(
                prior, stagedHereCandidate,
                restoreBindingThroughStrictSnapshot,
                priorAbiProfile.has_value() ? &priorAbiProfile.value() : nullptr,
                rollbackDeadline, &rollbackReboot, &rollbackError)) {
            Error journalError;
            if (!installJournal.RecordAuthoritativeReturn(
                    InstallJournalPhase::RollbackBindingReturned,
                    IsSafePublishedInfName(publishedCandidate.publishedName)
                        ? &publishedCandidate : nullptr,
                    packageStagedHere, bindingMutationStarted,
                    rollbackReboot,
                    rollbackReboot && !rollbackRebootAtAdmission,
                    true, ERROR_SUCCESS, false,
                    &journalError) ||
                !installJournal.RetireAfterPriorValidation(
                    rollbackReboot, &journalError)) {
                outcome.rollback = L"failed";
                outcome.rebootRequired = rollbackReboot;
                outcome.error = std::move(journalError);
                outcome.exitCode = ExitCode::RollbackFailed;
                return outcome;
            }
            outcome.rollback = L"succeeded";
            outcome.rebootRequired = rollbackReboot;
            outcome.error = installError;
            outcome.exitCode = installError.code == ERROR_SUCCESS_REBOOT_REQUIRED
                ? ExitCode::RebootRequired : ExitCode::Failure;
            return outcome;
        }
        outcome.rollback = L"failed";
        outcome.rebootRequired = rollbackReboot;
        outcome.error = std::move(rollbackError);
        Error journalError;
        installJournal.RecordAuthoritativeReturn(
            InstallJournalPhase::RollbackBindingReturned,
            IsSafePublishedInfName(publishedCandidate.publishedName)
                ? &publishedCandidate : nullptr,
            packageStagedHere, bindingMutationStarted,
            rollbackReboot,
            rollbackReboot && !rollbackRebootAtAdmission,
            false, outcome.error.code, false,
            &journalError);
        installJournal.Record(
            InstallJournalPhase::ManualReconciliationRequired,
            IsSafePublishedInfName(publishedCandidate.publishedName)
                ? &publishedCandidate : nullptr,
            packageStagedHere, bindingMutationStarted,
            rollbackReboot, false, outcome.error.code, false,
            &journalError);
        outcome.exitCode = ExitCode::RollbackFailed;
        return outcome;
    }
    if (outcome.error.code != ERROR_SUCCESS) {
        Error journalError;
        if (!installJournal.RetireAfterPriorValidation(
                outcome.rebootRequired, &journalError)) {
            outcome.error = std::move(journalError);
            outcome.exitCode = ExitCode::RollbackFailed;
            return outcome;
        }
        outcome.exitCode = outcome.rebootRequired &&
                outcome.error.code == ERROR_SUCCESS_REBOOT_REQUIRED
            ? ExitCode::RebootRequired : ExitCode::PreflightRejected;
        return outcome;
    }

    if (!options.brokerExecutable.empty()) {
        Error brokerError;
        bool driverRollbackAuthorized = true;
        bool brokerChanged = false;
        if (outcome.rebootRequired) {
            SetError(&brokerError, L"broker-reboot-boundary", ERROR_SUCCESS_REBOOT_REQUIRED,
                L"driver activation requires a restart; legacy ownership remains active and broker migration was not attempted");
        } else if (!CheckTransactionDeadline(
                options, L"transaction-deadline-before-broker", &brokerError) ||
            !SignalBrokerHandoff(options, &brokerError) ||
            !RunBrokerInstall(
                options, &driverRollbackAuthorized, &brokerChanged, &brokerError)) {
            // The broker command includes authenticated health verification and
            // rolls back its own SCM/credential/legacy transaction. Keep the
            // driver snapshot alive in this process until that proof succeeds.
        }
        outcome.changed = outcome.changed || brokerChanged;
        if (brokerError.code != ERROR_SUCCESS) {
            if (!driverRollbackAuthorized) {
                Error journalError;
                installJournal.Record(
                    InstallJournalPhase::ManualReconciliationRequired,
                    IsSafePublishedInfName(publishedCandidate.publishedName)
                        ? &publishedCandidate : nullptr,
                    packageStagedHere, bindingMutationStarted,
                    outcome.rebootRequired, false, brokerError.code,
                    false, &journalError);
                outcome.rollback = L"failed";
                outcome.error = std::move(brokerError);
                installJournal.AttachEvidence(&outcome.error);
                outcome.exitCode = ExitCode::RollbackFailed;
                return outcome;
            }
            if (!driverMutationStarted) {
                Error journalError;
                if (!installJournal.RetireAfterPriorValidation(
                        outcome.rebootRequired, &journalError)) {
                    outcome.rollback = L"failed";
                    outcome.error = std::move(journalError);
                    outcome.exitCode = ExitCode::RollbackFailed;
                    return outcome;
                }
                outcome.rollback = brokerChanged ? L"succeeded" : L"not-needed";
                outcome.error = std::move(brokerError);
                outcome.exitCode = outcome.error.code == ERROR_SUCCESS_REBOOT_REQUIRED
                    ? ExitCode::RebootRequired : ExitCode::Failure;
                return outcome;
            }
            Error rollbackError;
            bool rollbackReboot = outcome.rebootRequired;
            const uint64_t rollbackDeadline =
                CurrentUnixMilliseconds() + kDriverRollbackCeilingMs;
            if (!installJournal.Record(
                    InstallJournalPhase::RollbackBindingEntered,
                    IsSafePublishedInfName(publishedCandidate.publishedName)
                        ? &publishedCandidate : nullptr,
                    packageStagedHere, bindingMutationStarted,
                    rollbackReboot, true, ERROR_SUCCESS, false,
                    &rollbackError)) {
                outcome.rollback = L"failed";
                outcome.error = std::move(rollbackError);
                outcome.exitCode = ExitCode::RollbackFailed;
                return outcome;
            }
            if (!verifyPostAdmissionRollbackInventory(
                    L"install-broker-rollback-post-admission-inventory",
                    &rollbackError)) {
                outcome.rollback = L"failed";
                outcome.rebootRequired = rollbackReboot;
                outcome.error = std::move(rollbackError);
                outcome.exitCode = ExitCode::RollbackFailed;
                return outcome;
            }
            const bool rollbackRebootAtAdmission = rollbackReboot;
            bool rootRemovalRebootPending = false;
            if (!installJournal.RemoveAuthorizedPriorEmptyRootAfterAdmission(
                    rollbackDeadline, &rollbackReboot,
                    &rootRemovalRebootPending,
                    &rollbackError)) {
                outcome.rollback = L"failed";
                outcome.rebootRequired = rollbackReboot;
                outcome.error = std::move(rollbackError);
                outcome.exitCode = ExitCode::RollbackFailed;
                return outcome;
            }
            if (rootRemovalRebootPending) {
                outcome.rollback = L"not-needed";
                outcome.rebootRequired = true;
                SetError(&outcome.error,
                    L"install-partial-root-removal-reboot-pending",
                    ERROR_SUCCESS_REBOOT_REQUIRED,
                    L"receipt-bound root removal requires a restart before package rollback can continue");
                outcome.exitCode = ExitCode::RebootRequired;
                return outcome;
            }
            if (!verifyPostAdmissionRollbackInventory(
                    L"install-broker-rollback-pre-package-inventory",
                    &rollbackError) ||
                !installJournal.VerifyPriorTopologyBeforePackageRollback(
                    &rollbackError)) {
                outcome.rollback = L"failed";
                outcome.rebootRequired = rollbackReboot;
                outcome.error = std::move(rollbackError);
                outcome.exitCode = ExitCode::RollbackFailed;
                return outcome;
            }
            const PackageInfo* stagedHereCandidate =
                packageStagedHere ? &publishedCandidate : nullptr;
            const bool restoreBindingThroughStrictSnapshot =
                !prior.devices.empty() && bindingMutationStarted;
            if (RollbackInstall(
                    prior, stagedHereCandidate,
                    restoreBindingThroughStrictSnapshot,
                    priorAbiProfile.has_value() ? &priorAbiProfile.value() : nullptr,
                    rollbackDeadline, &rollbackReboot, &rollbackError)) {
                Error journalError;
                if (!installJournal.RecordAuthoritativeReturn(
                        InstallJournalPhase::RollbackBindingReturned,
                        IsSafePublishedInfName(publishedCandidate.publishedName)
                            ? &publishedCandidate : nullptr,
                        packageStagedHere, bindingMutationStarted,
                        rollbackReboot,
                        rollbackReboot && !rollbackRebootAtAdmission,
                        true, ERROR_SUCCESS, false,
                        &journalError) ||
                    !installJournal.RetireAfterPriorValidation(
                        rollbackReboot, &journalError)) {
                    outcome.rollback = L"failed";
                    outcome.rebootRequired = rollbackReboot;
                    outcome.error = std::move(journalError);
                    outcome.exitCode = ExitCode::RollbackFailed;
                    return outcome;
                }
                outcome.rollback = L"succeeded";
                outcome.rebootRequired = rollbackReboot;
                outcome.error = std::move(brokerError);
                outcome.exitCode = outcome.error.code == ERROR_SUCCESS_REBOOT_REQUIRED
                    ? ExitCode::RebootRequired : ExitCode::Failure;
                return outcome;
            }
            outcome.rollback = L"failed";
            outcome.rebootRequired = rollbackReboot;
            outcome.error = std::move(rollbackError);
            Error journalError;
            installJournal.RecordAuthoritativeReturn(
                InstallJournalPhase::RollbackBindingReturned,
                IsSafePublishedInfName(publishedCandidate.publishedName)
                    ? &publishedCandidate : nullptr,
                packageStagedHere, bindingMutationStarted,
                rollbackReboot,
                rollbackReboot && !rollbackRebootAtAdmission,
                false, outcome.error.code, false,
                &journalError);
            installJournal.Record(
                InstallJournalPhase::ManualReconciliationRequired,
                IsSafePublishedInfName(publishedCandidate.publishedName)
                    ? &publishedCandidate : nullptr,
                packageStagedHere, bindingMutationStarted,
                rollbackReboot, false, outcome.error.code, false,
                &journalError);
            outcome.exitCode = ExitCode::RollbackFailed;
            return outcome;
        }
    }

    if (!installJournal.RetireAfterForwardValidation(
            candidate, publishedCandidate.publishedName,
            outcome.rebootRequired, options.transactionDeadlineUnixMs,
            &outcome.error)) {
        outcome.rollback = L"failed";
        outcome.exitCode = ExitCode::RollbackFailed;
        return outcome;
    }
    outcome.success = true;
    outcome.rollback = L"not-needed";
    outcome.exitCode = outcome.rebootRequired ? ExitCode::RebootRequired : ExitCode::Success;
    return outcome;
}

struct PackageBackup {
    PackageInfo original;
    std::filesystem::path directory;
    std::filesystem::path infPath;
    std::vector<WinHandle> locks;
};

class LocalSecurityDescriptor final {
public:
    LocalSecurityDescriptor() = default;

    ~LocalSecurityDescriptor() {
        if (value_ != nullptr) {
            LocalFree(value_);
        }
    }

    LocalSecurityDescriptor(const LocalSecurityDescriptor&) = delete;
    LocalSecurityDescriptor& operator=(const LocalSecurityDescriptor&) = delete;

    bool Initialize(const wchar_t* sddl, const wchar_t* phase, Error* error) {
        if (!ConvertStringSecurityDescriptorToSecurityDescriptorW(
                sddl, SDDL_REVISION_1, &value_, nullptr)) {
            return SetLastErrorDetail(error, phase);
        }
        attributes_ = SECURITY_ATTRIBUTES{};
        attributes_.nLength = sizeof(attributes_);
        attributes_.lpSecurityDescriptor = value_;
        attributes_.bInheritHandle = FALSE;
        return true;
    }

    SECURITY_ATTRIBUTES* attributes() noexcept { return &attributes_; }

private:
    PSECURITY_DESCRIPTOR value_ = nullptr;
    SECURITY_ATTRIBUTES attributes_{};
};

bool VerifyProtectedFileSystemSecurity(
    HANDLE handle,
    bool directory,
    const wchar_t* phase,
    Error* error) {
    PSID owner = nullptr;
    PACL dacl = nullptr;
    PSECURITY_DESCRIPTOR descriptor = nullptr;
    const DWORD securityError = GetSecurityInfo(
        handle, SE_FILE_OBJECT, OWNER_SECURITY_INFORMATION | DACL_SECURITY_INFORMATION,
        &owner, nullptr, &dacl, nullptr, &descriptor);
    if (securityError != ERROR_SUCCESS) {
        return SetError(error, phase, securityError);
    }
    const auto fail = [&](DWORD code, std::wstring message) {
        LocalFree(descriptor);
        return SetError(error, phase, code, std::move(message));
    };

    BYTE administratorsBuffer[SECURITY_MAX_SID_SIZE]{};
    DWORD administratorsSize = sizeof(administratorsBuffer);
    BYTE systemBuffer[SECURITY_MAX_SID_SIZE]{};
    DWORD systemSize = sizeof(systemBuffer);
    if (!CreateWellKnownSid(WinBuiltinAdministratorsSid, nullptr,
            administratorsBuffer, &administratorsSize) ||
        !CreateWellKnownSid(WinLocalSystemSid, nullptr,
            systemBuffer, &systemSize)) {
        const DWORD code = GetLastError();
        return fail(code, L"could not construct protected backup principals");
    }
    SECURITY_DESCRIPTOR_CONTROL control = 0;
    DWORD revision = 0;
    ACL_SIZE_INFORMATION information{};
    if (owner == nullptr || !EqualSid(owner, administratorsBuffer) || dacl == nullptr ||
        !GetSecurityDescriptorControl(descriptor, &control, &revision) ||
        (control & SE_DACL_PROTECTED) == 0 ||
        !GetAclInformation(dacl, &information, sizeof(information), AclSizeInformation) ||
        information.AceCount != 2) {
        return fail(ERROR_INVALID_SECURITY_DESCR,
            L"protected backup owner or DACL is not exact");
    }

    const BYTE expectedFlags = directory
        ? static_cast<BYTE>(OBJECT_INHERIT_ACE | CONTAINER_INHERIT_ACE) : 0;
    bool administratorsSeen = false;
    bool systemSeen = false;
    for (DWORD index = 0; index < information.AceCount; ++index) {
        void* rawAce = nullptr;
        if (!GetAce(dacl, index, &rawAce) || rawAce == nullptr) {
            const DWORD code = GetLastError();
            return fail(code == ERROR_SUCCESS ? ERROR_INVALID_ACL : code,
                L"protected backup DACL could not be enumerated");
        }
        const auto* ace = static_cast<const ACCESS_ALLOWED_ACE*>(rawAce);
        if (ace->Header.AceType != ACCESS_ALLOWED_ACE_TYPE ||
            ace->Header.AceFlags != expectedFlags || ace->Mask != FILE_ALL_ACCESS) {
            return fail(ERROR_INVALID_ACL,
                L"protected backup DACL contains an unexpected access rule");
        }
        PSID sid = const_cast<DWORD*>(&ace->SidStart);
        if (EqualSid(sid, administratorsBuffer)) {
            if (administratorsSeen) {
                return fail(ERROR_INVALID_ACL,
                    L"protected backup DACL duplicates the Administrators rule");
            }
            administratorsSeen = true;
        } else if (EqualSid(sid, systemBuffer)) {
            if (systemSeen) {
                return fail(ERROR_INVALID_ACL,
                    L"protected backup DACL duplicates the LocalSystem rule");
            }
            systemSeen = true;
        } else {
            return fail(ERROR_INVALID_ACL,
                L"protected backup DACL grants an unexpected principal");
        }
    }
    LocalFree(descriptor);
    if (!administratorsSeen || !systemSeen) {
        return SetError(error, phase, ERROR_INVALID_ACL,
            L"protected backup DACL is missing an exact principal");
    }
    return true;
}

constexpr ACCESS_MASK kProductReadExecuteMask =
    FILE_LIST_DIRECTORY | FILE_TRAVERSE | FILE_READ_EA |
    FILE_READ_ATTRIBUTES | READ_CONTROL | SYNCHRONIZE;

ACCESS_MASK NormalizeProductDirectoryAccessMask(
    ACCESS_MASK mask) noexcept {
    GENERIC_MAPPING mapping{
        FILE_GENERIC_READ,
        FILE_GENERIC_WRITE,
        FILE_GENERIC_EXECUTE,
        FILE_ALL_ACCESS,
    };
    MapGenericMask(&mask, &mapping);
    return mask;
}

bool ProductDirectoryMaskIsReadExecuteOnly(
    ACCESS_MASK mask) noexcept {
    mask = NormalizeProductDirectoryAccessMask(mask);
    return mask != 0 && (mask & ~kProductReadExecuteMask) == 0;
}

bool VerifyProtectedProductDirectorySecurity(
    HANDLE handle,
    const std::wstring* exactTargetUserSid,
    Error* error) {
    PSID owner = nullptr;
    PACL dacl = nullptr;
    PSECURITY_DESCRIPTOR descriptor = nullptr;
    const DWORD securityError = GetSecurityInfo(
        handle, SE_FILE_OBJECT,
        OWNER_SECURITY_INFORMATION | DACL_SECURITY_INFORMATION,
        &owner, nullptr, &dacl, nullptr, &descriptor);
    if (securityError != ERROR_SUCCESS) {
        return SetError(error, L"install-journal-product-security",
            securityError);
    }
    const auto fail = [&](DWORD code, std::wstring message) {
        LocalFree(descriptor);
        return SetError(error, L"install-journal-product-security",
            code, std::move(message));
    };
    BYTE administratorsBuffer[SECURITY_MAX_SID_SIZE]{};
    DWORD administratorsSize = sizeof(administratorsBuffer);
    BYTE systemBuffer[SECURITY_MAX_SID_SIZE]{};
    DWORD systemSize = sizeof(systemBuffer);
    if (!CreateWellKnownSid(WinBuiltinAdministratorsSid, nullptr,
            administratorsBuffer, &administratorsSize) ||
        !CreateWellKnownSid(WinLocalSystemSid, nullptr,
            systemBuffer, &systemSize)) {
        return fail(GetLastError(),
            L"could not construct product-directory principals");
    }
    PSID targetUser = nullptr;
    if (exactTargetUserSid != nullptr &&
        (!IsSafeTargetUserSid(*exactTargetUserSid) ||
            !ConvertStringSidToSidW(
                exactTargetUserSid->c_str(), &targetUser))) {
        return fail(GetLastError() == ERROR_SUCCESS
                ? ERROR_INVALID_SID : GetLastError(),
            L"could not construct the exact product-directory target user principal");
    }
    const auto freeTargetUser = [&]() {
        if (targetUser != nullptr) {
            LocalFree(targetUser);
            targetUser = nullptr;
        }
    };
    SECURITY_DESCRIPTOR_CONTROL control = 0;
    DWORD revision = 0;
    ACL_SIZE_INFORMATION information{};
    if (owner == nullptr ||
        (exactTargetUserSid != nullptr
            ? !EqualSid(owner, administratorsBuffer)
            : (!EqualSid(owner, administratorsBuffer) &&
                !EqualSid(owner, systemBuffer))) ||
        dacl == nullptr ||
        !GetSecurityDescriptorControl(descriptor, &control, &revision) ||
        (control & SE_DACL_PROTECTED) == 0 ||
        !GetAclInformation(dacl, &information, sizeof(information),
            AclSizeInformation) ||
        (exactTargetUserSid != nullptr
            ? information.AceCount != 3U
            : information.AceCount < 2U)) {
        freeTargetUser();
        return fail(ERROR_INVALID_SECURITY_DESCR,
            L"product directory must have a protected Administrators/LocalSystem-owned DACL");
    }
    constexpr BYTE inheritedFlags =
        OBJECT_INHERIT_ACE | CONTAINER_INHERIT_ACE;
    bool administratorsSeen = false;
    bool systemSeen = false;
    bool targetUserSeen = false;
    for (DWORD index = 0; index < information.AceCount; ++index) {
        void* rawAce = nullptr;
        if (!GetAce(dacl, index, &rawAce) || rawAce == nullptr) {
            const DWORD code = GetLastError();
            freeTargetUser();
            return fail(code == ERROR_SUCCESS ? ERROR_INVALID_ACL : code,
                L"product directory DACL could not be enumerated");
        }
        const auto* ace = static_cast<const ACCESS_ALLOWED_ACE*>(rawAce);
        if (ace->Header.AceType != ACCESS_ALLOWED_ACE_TYPE ||
            (ace->Header.AceFlags & ~inheritedFlags) != 0) {
            freeTargetUser();
            return fail(ERROR_INVALID_ACL,
                L"product directory contains a deny, inherited, or otherwise unsupported access rule");
        }
        PSID sid = const_cast<DWORD*>(&ace->SidStart);
        const ACCESS_MASK normalizedMask =
            NormalizeProductDirectoryAccessMask(ace->Mask);
        if (EqualSid(sid, administratorsBuffer) ||
            EqualSid(sid, systemBuffer)) {
            bool& seen = EqualSid(sid, administratorsBuffer)
                ? administratorsSeen : systemSeen;
            if (seen || ace->Header.AceFlags != inheritedFlags ||
                normalizedMask != FILE_ALL_ACCESS) {
                freeTargetUser();
                return fail(ERROR_INVALID_ACL,
                    L"product directory Administrators/LocalSystem rules are not exact full-control entries");
            }
            seen = true;
            continue;
        }
        if (exactTargetUserSid != nullptr &&
            EqualSid(sid, targetUser)) {
            if (targetUserSeen ||
                ace->Header.AceFlags != inheritedFlags ||
                normalizedMask != kProductReadExecuteMask) {
                freeTargetUser();
                return fail(ERROR_INVALID_ACL,
                    L"product directory target-user rule is not exact inherited read/execute access");
            }
            targetUserSeen = true;
            continue;
        }
        if (!ProductDirectoryMaskIsReadExecuteOnly(ace->Mask)) {
            freeTargetUser();
            return fail(ERROR_INVALID_ACL,
                L"product directory grants a non-system principal create, write, delete, ownership, or ACL authority");
        }
        if (exactTargetUserSid != nullptr) {
            freeTargetUser();
            return fail(ERROR_INVALID_ACL,
                L"product directory grants read/execute access to a principal other than the requested target user");
        }
    }
    freeTargetUser();
    LocalFree(descriptor);
    if (!administratorsSeen || !systemSeen ||
        (exactTargetUserSid != nullptr && !targetUserSeen)) {
        return SetError(error, L"install-journal-product-security",
            ERROR_INVALID_ACL,
            L"product directory is missing exact Administrators or LocalSystem full control");
    }
    return true;
}

bool CreateProtectedBackupDirectory(
    const std::filesystem::path& path,
    Error* error) {
    LocalSecurityDescriptor security;
    if (!security.Initialize(
            kRollbackDirectorySecurity, L"rollback-backup-directory-security", error)) {
        return false;
    }
    if (!CreateDirectoryW(path.c_str(), security.attributes())) {
        return SetLastErrorDetail(error, L"rollback-backup-create");
    }
    WinHandle directory(CreateFileW(
        path.c_str(), FILE_READ_ATTRIBUTES | READ_CONTROL,
        FILE_SHARE_READ | FILE_SHARE_WRITE, nullptr, OPEN_EXISTING,
        FILE_ATTRIBUTE_NORMAL | FILE_FLAG_OPEN_REPARSE_POINT |
            FILE_FLAG_BACKUP_SEMANTICS,
        nullptr));
    if (!directory) {
        return SetLastErrorDetail(error, L"rollback-backup-directory-open");
    }
    FILE_ATTRIBUTE_TAG_INFO attributes{};
    if (!GetFileInformationByHandleEx(
            directory.get(), FileAttributeTagInfo, &attributes, sizeof(attributes)) ||
        (attributes.FileAttributes & FILE_ATTRIBUTE_DIRECTORY) == 0 ||
        (attributes.FileAttributes & FILE_ATTRIBUTE_REPARSE_POINT) != 0) {
        return SetError(error, L"rollback-backup-directory-open",
            ERROR_REPARSE_TAG_MISMATCH);
    }
    return VerifyProtectedFileSystemSecurity(
        directory.get(), true, L"rollback-backup-directory-security", error);
}

bool CopyProtectedBackupFile(
    const std::filesystem::path& sourcePath,
    const std::filesystem::path& destinationPath,
    Error* error) {
    WinHandle source(CreateFileW(
        sourcePath.c_str(), GENERIC_READ | FILE_READ_ATTRIBUTES,
        FILE_SHARE_READ, nullptr, OPEN_EXISTING,
        FILE_ATTRIBUTE_NORMAL | FILE_FLAG_OPEN_REPARSE_POINT |
            FILE_FLAG_SEQUENTIAL_SCAN,
        nullptr));
    if (!source) {
        return SetLastErrorDetail(error, L"rollback-backup-source-open");
    }
    FILE_ATTRIBUTE_TAG_INFO sourceAttributes{};
    if (!GetFileInformationByHandleEx(
            source.get(), FileAttributeTagInfo, &sourceAttributes,
            sizeof(sourceAttributes)) ||
        (sourceAttributes.FileAttributes &
            (FILE_ATTRIBUTE_DIRECTORY | FILE_ATTRIBUTE_REPARSE_POINT)) != 0) {
        return SetError(error, L"rollback-backup-source-open",
            ERROR_REPARSE_TAG_MISMATCH,
            L"rollback sources must be regular non-reparse files");
    }

    LocalSecurityDescriptor security;
    if (!security.Initialize(
            kRecoveryRecordSecurity, L"rollback-backup-file-security", error)) {
        return false;
    }
    WinHandle destination(CreateFileW(
        destinationPath.c_str(),
        GENERIC_READ | GENERIC_WRITE | FILE_READ_ATTRIBUTES | READ_CONTROL,
        FILE_SHARE_READ, security.attributes(), CREATE_NEW,
        FILE_ATTRIBUTE_NORMAL | FILE_FLAG_OPEN_REPARSE_POINT |
            FILE_FLAG_SEQUENTIAL_SCAN | FILE_FLAG_WRITE_THROUGH,
        nullptr));
    if (!destination) {
        return SetLastErrorDetail(error, L"rollback-backup-file-create");
    }
    FILE_ATTRIBUTE_TAG_INFO destinationAttributes{};
    const BOOL queriedDestination = GetFileInformationByHandleEx(
        destination.get(), FileAttributeTagInfo, &destinationAttributes,
        sizeof(destinationAttributes));
    const DWORD destinationQueryError = queriedDestination
        ? ERROR_SUCCESS : GetLastError();
    if (!queriedDestination ||
        (destinationAttributes.FileAttributes &
            (FILE_ATTRIBUTE_DIRECTORY | FILE_ATTRIBUTE_REPARSE_POINT)) != 0) {
        return SetError(error, L"rollback-backup-file-create",
            queriedDestination ? ERROR_REPARSE_TAG_MISMATCH : destinationQueryError);
    }
    if (!VerifyProtectedFileSystemSecurity(
            destination.get(), false, L"rollback-backup-file-security", error)) {
        return false;
    }

    std::array<BYTE, 64U * 1024U> buffer{};
    for (;;) {
        DWORD read = 0;
        if (!ReadFile(source.get(), buffer.data(),
                static_cast<DWORD>(buffer.size()), &read, nullptr)) {
            return SetLastErrorDetail(error, L"rollback-backup-file-read");
        }
        if (read == 0) {
            break;
        }
        DWORD offset = 0;
        while (offset < read) {
            DWORD written = 0;
            if (!WriteFile(destination.get(), buffer.data() + offset,
                    read - offset, &written, nullptr) || written == 0) {
                const DWORD writeError = GetLastError();
                const DWORD code = writeError == ERROR_SUCCESS
                    ? ERROR_WRITE_FAULT : writeError;
                return SetError(error, L"rollback-backup-file-write", code);
            }
            offset += written;
        }
    }
    if (!FlushFileBuffers(destination.get())) {
        return SetLastErrorDetail(error, L"rollback-backup-file-flush");
    }
    return true;
}

class BackupDirectory final {
public:
    ~BackupDirectory() noexcept {
        try {
            if (!path_.empty() && !preserve_) {
                root_.reset();
                std::error_code removalError;
                std::filesystem::remove_all(path_, removalError);
                std::error_code presenceError;
                const bool remains = std::filesystem::exists(path_, presenceError);
                if (!removalError && !presenceError && !remains) {
                    path_.clear();
                    ClearActiveRecoveryEvidence();
                }
            }
        } catch (...) {
            // The top-level boundary must remain able to emit the fixed active
            // evidence path after unwinding; a destructor must never terminate it.
        }
    }

    bool Create(Error* error) {
        std::vector<wchar_t> windowsDirectory(MAX_PATH);
        const UINT length = GetWindowsDirectoryW(
            windowsDirectory.data(), static_cast<UINT>(windowsDirectory.size()));
        if (length == 0 || static_cast<size_t>(length) >= windowsDirectory.size()) {
            return SetLastErrorDetail(error, L"rollback-backup-root");
        }
        const std::filesystem::path parent =
            std::filesystem::path(windowsDirectory.data()) / L"Temp";
        parent_.reset(CreateFileW(
            parent.c_str(), FILE_READ_ATTRIBUTES,
            FILE_SHARE_READ | FILE_SHARE_WRITE, nullptr, OPEN_EXISTING,
            FILE_ATTRIBUTE_NORMAL | FILE_FLAG_OPEN_REPARSE_POINT |
                FILE_FLAG_BACKUP_SEMANTICS,
            nullptr));
        if (!parent_) {
            return SetLastErrorDetail(error, L"rollback-backup-parent");
        }
        FILE_ATTRIBUTE_TAG_INFO parentAttributes{};
        if (!GetFileInformationByHandleEx(
                parent_.get(), FileAttributeTagInfo, &parentAttributes,
                sizeof(parentAttributes)) ||
            (parentAttributes.FileAttributes & FILE_ATTRIBUTE_DIRECTORY) == 0 ||
            (parentAttributes.FileAttributes & FILE_ATTRIBUTE_REPARSE_POINT) != 0) {
            return SetError(error, L"rollback-backup-parent",
                ERROR_REPARSE_TAG_MISMATCH,
                L"Windows temporary directory must be a regular non-reparse directory");
        }

        PSECURITY_DESCRIPTOR descriptor = nullptr;
        if (!ConvertStringSecurityDescriptorToSecurityDescriptorW(
                kRollbackDirectorySecurity, SDDL_REVISION_1, &descriptor, nullptr)) {
            return SetLastErrorDetail(error, L"rollback-backup-security");
        }
        SECURITY_ATTRIBUTES security{};
        security.nLength = sizeof(security);
        security.lpSecurityDescriptor = descriptor;
        security.bInheritHandle = FALSE;

        HCRYPTPROV provider = 0;
        if (!CryptAcquireContextW(
                &provider, nullptr, nullptr, PROV_RSA_AES,
                CRYPT_VERIFYCONTEXT | CRYPT_SILENT)) {
            const DWORD code = GetLastError();
            LocalFree(descriptor);
            return SetError(error, L"rollback-backup-random", code);
        }
        static constexpr wchar_t digits[] = L"0123456789abcdef";
        for (size_t attempt = 0; attempt < 32; ++attempt) {
            std::array<BYTE, 16> random{};
            if (!CryptGenRandom(provider, static_cast<DWORD>(random.size()), random.data())) {
                const DWORD code = GetLastError();
                CryptReleaseContext(provider, 0);
                LocalFree(descriptor);
                return SetError(error, L"rollback-backup-random", code);
            }
            std::wstring suffix;
            suffix.reserve(random.size() * 2);
            for (BYTE value : random) {
                suffix.push_back(digits[value >> 4U]);
                suffix.push_back(digits[value & 0x0fU]);
            }
            const std::filesystem::path candidate =
                parent / (L"VIIPER-UDE-rollback-" + suffix);
            if (!CreateDirectoryW(candidate.c_str(), &security)) {
                if (GetLastError() == ERROR_ALREADY_EXISTS) {
                    continue;
                }
                const DWORD code = GetLastError();
                CryptReleaseContext(provider, 0);
                LocalFree(descriptor);
                return SetError(error, L"rollback-backup-root", code);
            }
            try {
                path_ = candidate;
            } catch (...) {
                RemoveDirectoryW(candidate.c_str());
                CryptReleaseContext(provider, 0);
                LocalFree(descriptor);
                throw;
            }
            const std::wstring& rootValue = path_.native();
            constexpr size_t recordNameLength = std::size(kRecoveryRecordName) - 1;
            const size_t recordLength = rootValue.size() + 1 + recordNameLength;
            if (rootValue.empty() || rootValue.size() >= gActiveBackupRoot.size() ||
                recordLength >= gActiveRecoveryRecord.size()) {
                if (RemoveDirectoryW(candidate.c_str())) {
                    path_.clear();
                }
                CryptReleaseContext(provider, 0);
                LocalFree(descriptor);
                return SetError(error, L"rollback-backup-root",
                    ERROR_FILENAME_EXCED_RANGE,
                    L"protected rollback paths exceed the exception-safe reporting bound");
            }
            ClearActiveRecoveryEvidence();
            std::copy(rootValue.begin(), rootValue.end(), gActiveBackupRoot.begin());
            gActiveBackupRootRetained = true;
            std::copy(rootValue.begin(), rootValue.end(), gActiveRecoveryRecord.begin());
            gActiveRecoveryRecord[rootValue.size()] = L'\\';
            std::copy_n(kRecoveryRecordName, recordNameLength,
                gActiveRecoveryRecord.begin() + rootValue.size() + 1);
            root_.reset(CreateFileW(
                candidate.c_str(), FILE_READ_ATTRIBUTES | READ_CONTROL,
                FILE_SHARE_READ | FILE_SHARE_WRITE, nullptr, OPEN_EXISTING,
                FILE_ATTRIBUTE_NORMAL | FILE_FLAG_OPEN_REPARSE_POINT |
                    FILE_FLAG_BACKUP_SEMANTICS,
                nullptr));
            if (!root_) {
                const DWORD code = GetLastError();
                if (RemoveDirectoryW(candidate.c_str())) {
                    path_.clear();
                    ClearActiveRecoveryEvidence();
                }
                CryptReleaseContext(provider, 0);
                LocalFree(descriptor);
                return SetError(error, L"rollback-backup-root-lock", code);
            }
            FILE_ATTRIBUTE_TAG_INFO rootAttributes{};
            if (!GetFileInformationByHandleEx(
                    root_.get(), FileAttributeTagInfo, &rootAttributes,
                    sizeof(rootAttributes)) ||
                (rootAttributes.FileAttributes & FILE_ATTRIBUTE_DIRECTORY) == 0 ||
                (rootAttributes.FileAttributes & FILE_ATTRIBUTE_REPARSE_POINT) != 0) {
                root_.reset();
                if (RemoveDirectoryW(candidate.c_str())) {
                    path_.clear();
                    ClearActiveRecoveryEvidence();
                }
                CryptReleaseContext(provider, 0);
                LocalFree(descriptor);
                return SetError(error, L"rollback-backup-root-lock",
                    ERROR_REPARSE_TAG_MISMATCH);
            }
            if (!VerifyProtectedFileSystemSecurity(
                    root_.get(), true, L"rollback-backup-root-security", error)) {
                root_.reset();
                if (RemoveDirectoryW(candidate.c_str())) {
                    path_.clear();
                    ClearActiveRecoveryEvidence();
                }
                CryptReleaseContext(provider, 0);
                LocalFree(descriptor);
                return false;
            }
            CryptReleaseContext(provider, 0);
            LocalFree(descriptor);
            return true;
        }
        CryptReleaseContext(provider, 0);
        LocalFree(descriptor);
        return SetError(error, L"rollback-backup-root", ERROR_ALREADY_EXISTS,
            L"could not allocate a unique protected rollback directory");
    }

    const std::filesystem::path& path() const noexcept { return path_; }

    std::filesystem::path RecoveryRecordPath() const {
        return path_ / kRecoveryRecordName;
    }

    bool ArmPreservation(const std::filesystem::path& recoveryPath, Error* error) {
        const std::wstring& value = recoveryPath.native();
        if (!gActiveBackupRootRetained || gActiveRecoveryRecord[0] == L'\0' ||
            value != gActiveRecoveryRecord.data()) {
            return SetError(error, L"recovery-record-path", ERROR_INVALID_DATA,
                L"published recovery record does not match the tracked protected backup root");
        }
        gActiveRecoveryRecordWritten = true;
        preserve_ = true;
        return true;
    }

    void AttachRecoveryRecord(Error* error) const {
        if (error == nullptr) {
            return;
        }
        if (gActiveBackupRootRetained && gActiveBackupRoot[0] != L'\0') {
            error->recoveryBackup = gActiveBackupRoot.data();
            error->recoveryBackupRetained = true;
        } else if (!path_.empty()) {
            error->recoveryBackup = path_.wstring();
            error->recoveryBackupRetained = true;
        }
        if (gActiveRecoveryRecord[0] != L'\0') {
            error->recoveryRecord = gActiveRecoveryRecord.data();
            error->recoveryRecordWritten = gActiveRecoveryRecordWritten;
        } else if (!path_.empty()) {
            error->recoveryRecord = RecoveryRecordPath().wstring();
            error->recoveryRecordWritten = false;
        }
        if (!error->recoveryRecord.empty() && !error->recoveryRecordWritten) {
            error->recoveryRecordError = ERROR_FILE_NOT_FOUND;
            error->recoveryRecordPhase = L"recovery-record-not-published";
            error->recoveryRecordMessage =
                L"the retained backup predates a verified write-ahead recovery record";
        }
    }

    bool Cleanup(std::vector<PackageBackup>* backups, Error* error) {
        if (backups != nullptr) {
            backups->clear();
        }
        root_.reset();
        if (path_.empty()) {
            preserve_ = false;
            ClearActiveRecoveryEvidence();
            return true;
        }
        std::error_code removalError;
        std::filesystem::remove_all(path_, removalError);
        std::error_code presenceError;
        const bool remains = std::filesystem::exists(path_, presenceError);
        if (removalError || presenceError || remains) {
            preserve_ = true;
            const DWORD code = removalError
                ? static_cast<DWORD>(removalError.value())
                : presenceError ? static_cast<DWORD>(presenceError.value())
                                : ERROR_DIR_NOT_EMPTY;
            SetError(error, L"rollback-backup-cleanup", code,
                L"protected rollback backup cleanup could not be verified");
            AttachRecoveryRecord(error);
            return false;
        }
        path_.clear();
        preserve_ = false;
        ClearActiveRecoveryEvidence();
        return true;
    }

private:
    std::filesystem::path path_;
    WinHandle parent_;
    WinHandle root_;
    bool preserve_ = false;
};

bool BackupPackagesIntoDirectory(
    const std::vector<PackageInfo>& packages,
    const std::filesystem::path& baseDirectory,
    std::vector<PackageBackup>* backups,
    Error* error) {
    backups->clear();
    for (size_t index = 0; index < packages.size(); ++index) {
        std::filesystem::path storeInf;
        if (!GetDriverStoreInfPath(packages[index].infPath, &storeInf, error)) {
            return false;
        }
        std::filesystem::path resolvedPublished;
        if (!GetPublishedInfPath(storeInf, &resolvedPublished, error) ||
            _wcsicmp(resolvedPublished.filename().c_str(), packages[index].publishedName.c_str()) != 0) {
            if (error->code == ERROR_SUCCESS) {
                SetError(error, L"rollback-backup-published-inf", ERROR_REVISION_MISMATCH);
            }
            return false;
        }
        const std::filesystem::path destination =
            baseDirectory / std::to_wstring(index);
        std::filesystem::path signerCatalog;
        if (!VerifyInfSignature(storeInf, &signerCatalog, error)) {
            return false;
        }
        if (signerCatalog.is_relative()) {
            signerCatalog = storeInf.parent_path() / signerCatalog.filename();
        }
        if (signerCatalog.empty()) {
            return SetError(error, L"rollback-backup-catalog", ERROR_FILE_NOT_FOUND,
                L"signed rollback package did not resolve a catalog payload");
        }
        const std::filesystem::path backupInf = destination / L"ViiperUde.inf";
        if (!CreateProtectedBackupDirectory(destination, error) ||
            !CopyProtectedBackupFile(storeInf, backupInf, error) ||
            !CopyProtectedBackupFile(
                storeInf.parent_path() / kDriverFileName,
                destination / kDriverFileName, error) ||
            !CopyProtectedBackupFile(
                signerCatalog, destination / kCatalogName, error) ||
            !ValidateExactPackageDirectory(destination, error)) {
            return false;
        }
        PackageInfo verified;
        bool owned = false;
        if (!LoadOwnedPackage(backupInf, true, false,
                &verified, &owned, error) || !owned) {
            return false;
        }
        if (!(verified.version == packages[index].version) ||
            !SamePackageBytes(verified, packages[index])) {
            return SetError(error, L"rollback-backup-identity", ERROR_REVISION_MISMATCH,
                L"protected rollback copy does not match the captured signed package");
        }
        std::vector<WinHandle> locks;
        if (!LockPackageFiles(destination, &locks, error)) {
            error->phase = L"rollback-backup-lock";
            return false;
        }
        backups->push_back(PackageBackup{
            packages[index], destination, backupInf, std::move(locks)});
    }
    return true;
}

bool BackupPackages(
    const std::vector<PackageInfo>& packages,
    BackupDirectory* root,
    std::vector<PackageBackup>* backups,
    Error* error) {
    return root->Create(error) &&
        BackupPackagesIntoDirectory(packages, root->path(), backups, error);
}

bool IsSha256Digest(std::string_view value) {
    return value.size() == 64 &&
        std::all_of(value.begin(), value.end(), [](unsigned char character) {
            return std::isxdigit(character) != 0;
        });
}

void AppendJsonString(std::string* output, std::wstring_view value) {
    static constexpr char digits[] = "0123456789abcdef";
    output->push_back('"');
    for (wchar_t character : value) {
        const uint32_t codePoint = static_cast<uint32_t>(character);
        if (codePoint == '"' || codePoint == '\\') {
            output->push_back('\\');
            output->push_back(static_cast<char>(codePoint));
        } else if (codePoint >= 0x20U && codePoint <= 0x7eU) {
            output->push_back(static_cast<char>(codePoint));
        } else if (codePoint <= 0xffffU) {
            output->append("\\u");
            output->push_back(digits[(codePoint >> 12U) & 0x0fU]);
            output->push_back(digits[(codePoint >> 8U) & 0x0fU]);
            output->push_back(digits[(codePoint >> 4U) & 0x0fU]);
            output->push_back(digits[codePoint & 0x0fU]);
        } else {
            const uint32_t supplementary = codePoint - 0x10000U;
            const uint32_t high = 0xd800U + (supplementary >> 10U);
            const uint32_t low = 0xdc00U + (supplementary & 0x3ffU);
            for (uint32_t surrogate : {high, low}) {
                output->append("\\u");
                output->push_back(digits[(surrogate >> 12U) & 0x0fU]);
                output->push_back(digits[(surrogate >> 8U) & 0x0fU]);
                output->push_back(digits[(surrogate >> 4U) & 0x0fU]);
                output->push_back(digits[surrogate & 0x0fU]);
            }
        }
    }
    output->push_back('"');
}

void AppendJsonAsciiString(std::string* output, std::string_view value) {
    std::wstring wide;
    wide.reserve(value.size());
    for (unsigned char character : value) {
        wide.push_back(static_cast<wchar_t>(character));
    }
    AppendJsonString(output, wide);
}

bool IsSafeRecoveryRelativePath(const std::filesystem::path& path) {
    if (path.empty() || path.is_absolute() || path.has_root_name() ||
        path.has_root_directory() || path.lexically_normal() != path) {
        return false;
    }
    size_t components = 0;
    for (const std::filesystem::path& component : path) {
        const std::wstring value = component.wstring();
        if (value.empty() || value == L"." || value == L".." ||
            value.find(L':') != std::wstring::npos ||
            std::any_of(value.begin(), value.end(), [](wchar_t character) {
                return character < 0x20;
            })) {
            return false;
        }
        ++components;
    }
    return components != 0;
}

bool RecoveryRelativePath(
    const std::filesystem::path& root,
    const std::filesystem::path& target,
    std::wstring* relative,
    Error* error) {
    const std::filesystem::path candidate = target.lexically_relative(root);
    if (!IsSafeRecoveryRelativePath(candidate) ||
        (root / candidate).lexically_normal() != target.lexically_normal()) {
        return SetError(error, L"recovery-record-path", ERROR_INVALID_NAME,
            L"rollback recovery paths must remain relative to the protected backup root");
    }
    *relative = candidate.generic_wstring();
    return true;
}

bool BuildRemoveRecoveryRecord(
    const Snapshot& prior,
    const std::vector<PackageBackup>& backups,
    const std::filesystem::path& root,
    std::string* record,
    Error* error) {
    if (backups.size() != prior.packages.size()) {
        return SetError(error, L"recovery-record-binding", ERROR_INVALID_DATA,
            L"rollback backup count does not match the captured package inventory");
    }
    record->clear();
    record->append(
        "{\"schema\":1,\"kind\":\"VIIPER-UDE-remove-rollback-recovery\","
        "\"state\":\"prepared-remove-transaction\","
        "\"hardwareId\":\"ROOT\\\\VIIPER\\\\UDE\",\"automaticRestore\":false,"
        "\"requiredValidation\":[\"inf-signature\",\"inf-catalog-membership\","
        "\"sys-catalog-membership\",\"inf-sha256\",\"sys-sha256\",\"cat-sha256\"],"
        "\"devices\":[");
    for (size_t index = 0; index < prior.devices.size(); ++index) {
        const DeviceState& device = prior.devices[index];
        size_t packageIndex = prior.packages.size();
        size_t packageMatches = 0;
        for (size_t candidate = 0; candidate < prior.packages.size(); ++candidate) {
            const PackageInfo& package = prior.packages[candidate];
            if (_wcsicmp(package.publishedName.c_str(), device.publishedInf.c_str()) == 0 &&
                package.version == device.version &&
                SamePackageBytes(package, device.package)) {
                packageIndex = candidate;
                ++packageMatches;
            }
        }
        if (!IsOwnedGeneratedRootInstanceId(device.instanceId) ||
            !IsSafePublishedInfName(device.publishedInf) ||
            _wcsicmp(device.service.c_str(), kServiceName) != 0 ||
            !(device.version == device.package.version) ||
            _wcsicmp(device.package.publishedName.c_str(), device.publishedInf.c_str()) != 0 ||
            packageMatches != 1 || packageIndex >= backups.size() ||
            _wcsicmp(backups[packageIndex].original.publishedName.c_str(),
                device.publishedInf.c_str()) != 0 ||
            !(backups[packageIndex].original.version == device.version) ||
            !SamePackageBytes(backups[packageIndex].original, device.package) ||
            !IsSha256Digest(device.package.infSha256) ||
            !IsSha256Digest(device.package.sysSha256) ||
            !IsSha256Digest(device.package.catSha256)) {
            return SetError(error, L"recovery-record-device", ERROR_INVALID_DATA,
                L"captured devnode identity is not safe for a recovery record");
        }
        if (index != 0) record->push_back(',');
        record->append("{\"instanceId\":");
        AppendJsonString(record, device.instanceId);
        record->append(",\"present\":");
        record->append(device.present ? "true" : "false");
        record->append(",\"started\":");
        record->append(device.started ? "true" : "false");
        record->append(",\"problem\":");
        record->append(std::to_string(device.problem));
        record->append(",\"service\":");
        AppendJsonString(record, device.service);
        record->append(",\"publishedInf\":");
        AppendJsonString(record, device.publishedInf);
        record->append(",\"packageIndex\":");
        record->append(std::to_string(packageIndex));
        record->append(",\"version\":");
        AppendJsonString(record, VersionToString(device.version));
        record->append(",\"infSha256\":");
        AppendJsonAsciiString(record, LowerAscii(device.package.infSha256));
        record->append(",\"sysSha256\":");
        AppendJsonAsciiString(record, LowerAscii(device.package.sysSha256));
        record->append(",\"catSha256\":");
        AppendJsonAsciiString(record, LowerAscii(device.package.catSha256));
        record->push_back('}');
    }
    record->append("],\"packages\":[");
    for (size_t index = 0; index < prior.packages.size(); ++index) {
        const PackageInfo& package = prior.packages[index];
        const PackageBackup& backup = backups[index];
        const bool duplicatePublishedName = std::any_of(
            prior.packages.begin(), prior.packages.end(), [&](const PackageInfo& candidate) {
                return &candidate != &package &&
                    _wcsicmp(candidate.publishedName.c_str(), package.publishedName.c_str()) == 0;
            });
        if (!IsSafePublishedInfName(package.publishedName) ||
            duplicatePublishedName ||
            !(backup.original.version == package.version) ||
            _wcsicmp(backup.original.publishedName.c_str(), package.publishedName.c_str()) != 0 ||
            backup.infPath.parent_path() != backup.directory ||
            _wcsicmp(backup.infPath.filename().c_str(), L"ViiperUde.inf") != 0 ||
            !SamePackageBytes(backup.original, package) ||
            !IsSha256Digest(package.infSha256) ||
            !IsSha256Digest(package.sysSha256) ||
            !IsSha256Digest(package.catSha256)) {
            return SetError(error, L"recovery-record-package", ERROR_INVALID_DATA,
                L"protected rollback package does not match the captured inventory");
        }
        std::wstring relativeDirectory;
        std::wstring relativeInf;
        std::wstring relativeSys;
        std::wstring relativeCat;
        if (!RecoveryRelativePath(root, backup.directory, &relativeDirectory, error) ||
            relativeDirectory != std::to_wstring(index) ||
            !RecoveryRelativePath(root, backup.infPath, &relativeInf, error) ||
            !RecoveryRelativePath(root, backup.directory / kDriverFileName, &relativeSys, error) ||
            !RecoveryRelativePath(root, backup.directory / kCatalogName, &relativeCat, error)) {
            if (error->code == ERROR_SUCCESS) {
                SetError(error, L"recovery-record-path", ERROR_INVALID_NAME);
            }
            return false;
        }
        if (index != 0) record->push_back(',');
        record->append("{\"publishedInf\":");
        AppendJsonString(record, package.publishedName);
        record->append(",\"version\":");
        AppendJsonString(record, VersionToString(package.version));
        record->append(",\"infSha256\":");
        AppendJsonAsciiString(record, LowerAscii(package.infSha256));
        record->append(",\"sysSha256\":");
        AppendJsonAsciiString(record, LowerAscii(package.sysSha256));
        record->append(",\"catSha256\":");
        AppendJsonAsciiString(record, LowerAscii(package.catSha256));
        record->append(",\"backupInf\":");
        AppendJsonString(record, relativeInf);
        record->append(",\"backupSys\":");
        AppendJsonString(record, relativeSys);
        record->append(",\"backupCat\":");
        AppendJsonString(record, relativeCat);
        record->push_back('}');
    }
    record->append("]}\n");
    if (record->size() > kMaximumRecoveryRecordBytes) {
        return SetError(error, L"recovery-record-size", ERROR_FILE_TOO_LARGE);
    }
    return true;
}

bool WriteProtectedRecoveryRecord(
    const std::filesystem::path& path,
    std::string_view record,
    Error* error) {
    if (path.filename() != kRecoveryRecordName ||
        record.empty() || record.size() > kMaximumRecoveryRecordBytes) {
        return SetError(error, L"recovery-record-create", ERROR_INVALID_PARAMETER);
    }
    const std::filesystem::path temporaryPath =
        path.parent_path() / kRecoveryRecordTemporaryName;
    LocalSecurityDescriptor security;
    if (!security.Initialize(
            kRecoveryRecordSecurity, L"recovery-record-security", error)) {
        return false;
    }
    WinHandle file(CreateFileW(temporaryPath.c_str(),
        GENERIC_READ | GENERIC_WRITE | FILE_READ_ATTRIBUTES | READ_CONTROL,
        FILE_SHARE_READ, security.attributes(), CREATE_NEW,
        FILE_ATTRIBUTE_NORMAL | FILE_FLAG_WRITE_THROUGH |
            FILE_FLAG_OPEN_REPARSE_POINT,
        nullptr));
    const DWORD createError = GetLastError();
    if (!file) {
        return SetError(error, L"recovery-record-create", createError);
    }
    const auto discardTemporary = [&]() noexcept {
        file.reset();
        DeleteFileW(temporaryPath.c_str());
    };
    FILE_ATTRIBUTE_TAG_INFO attributes{};
    const BOOL queriedAttributes = GetFileInformationByHandleEx(
        file.get(), FileAttributeTagInfo, &attributes, sizeof(attributes));
    const DWORD attributeError = queriedAttributes ? ERROR_SUCCESS : GetLastError();
    if (!queriedAttributes ||
        (attributes.FileAttributes &
            (FILE_ATTRIBUTE_DIRECTORY | FILE_ATTRIBUTE_REPARSE_POINT)) != 0) {
        const DWORD code = queriedAttributes
            ? ERROR_REPARSE_TAG_MISMATCH : attributeError;
        SetError(error, L"recovery-record-create", code,
            L"recovery record must be a regular non-reparse file");
        discardTemporary();
        return false;
    }
    if (!VerifyProtectedFileSystemSecurity(
            file.get(), false, L"recovery-record-security", error)) {
        discardTemporary();
        return false;
    }
    size_t offset = 0;
    while (offset < record.size()) {
        const DWORD requested = static_cast<DWORD>(std::min<size_t>(
            record.size() - offset, MAXDWORD));
        DWORD written = 0;
        if (!WriteFile(file.get(), record.data() + offset, requested,
                &written, nullptr) || written == 0) {
            const DWORD writeError = GetLastError();
            const DWORD code = writeError == ERROR_SUCCESS
                ? ERROR_WRITE_FAULT : writeError;
            SetError(error, L"recovery-record-write", code);
            discardTemporary();
            return false;
        }
        offset += written;
    }
    if (!FlushFileBuffers(file.get())) {
        SetLastErrorDetail(error, L"recovery-record-flush");
        discardTemporary();
        return false;
    }
    file.reset();
    if (!MoveFileExW(
            temporaryPath.c_str(), path.c_str(), MOVEFILE_WRITE_THROUGH)) {
        const DWORD code = GetLastError();
        DeleteFileW(temporaryPath.c_str());
        return SetError(error, L"recovery-record-publish", code);
    }

    file.reset(CreateFileW(path.c_str(),
        GENERIC_READ | GENERIC_WRITE | FILE_READ_ATTRIBUTES | READ_CONTROL,
        FILE_SHARE_READ, nullptr, OPEN_EXISTING,
        FILE_ATTRIBUTE_NORMAL | FILE_FLAG_WRITE_THROUGH |
            FILE_FLAG_OPEN_REPARSE_POINT,
        nullptr));
    if (!file) {
        return SetLastErrorDetail(error, L"recovery-record-reopen");
    }
    attributes = {};
    const BOOL queriedPublished = GetFileInformationByHandleEx(
        file.get(), FileAttributeTagInfo, &attributes, sizeof(attributes));
    const DWORD publishedQueryError = queriedPublished
        ? ERROR_SUCCESS : GetLastError();
    if (!queriedPublished ||
        (attributes.FileAttributes &
            (FILE_ATTRIBUTE_DIRECTORY | FILE_ATTRIBUTE_REPARSE_POINT)) != 0) {
        const DWORD code = queriedPublished
            ? ERROR_REPARSE_TAG_MISMATCH : publishedQueryError;
        return SetError(error, L"recovery-record-reopen", code,
            L"published recovery record must be a regular non-reparse file");
    }
    if (!VerifyProtectedFileSystemSecurity(
            file.get(), false, L"recovery-record-security", error)) {
        return false;
    }
    offset = 0;
    std::array<char, 4096> verification{};
    while (offset < record.size()) {
        const DWORD requested = static_cast<DWORD>(std::min<size_t>(
            verification.size(), record.size() - offset));
        DWORD read = 0;
        if (!ReadFile(file.get(), verification.data(), requested, &read, nullptr)) {
            return SetLastErrorDetail(error, L"recovery-record-verify");
        }
        if (read != requested ||
            std::memcmp(verification.data(), record.data() + offset, read) != 0) {
            return SetError(error, L"recovery-record-verify", ERROR_CRC,
                L"published recovery record bytes do not match the flushed transaction journal");
        }
        offset += read;
    }
    char trailing = 0;
    DWORD trailingRead = 0;
    if (!ReadFile(file.get(), &trailing, 1, &trailingRead, nullptr)) {
        return SetLastErrorDetail(error, L"recovery-record-verify");
    }
    if (trailingRead != 0) {
        return SetError(error, L"recovery-record-verify", ERROR_FILE_INVALID,
            L"published recovery record contains trailing bytes");
    }
    if (!FlushFileBuffers(file.get())) {
        return SetLastErrorDetail(error, L"recovery-record-published-flush");
    }
    return true;
}

const char* InstallJournalPhaseName(InstallJournalPhase phase) noexcept {
    switch (phase) {
    case InstallJournalPhase::Prepared: return "Prepared";
    case InstallJournalPhase::SetupCopyEntered: return "SetupCopyEntered";
    case InstallJournalPhase::SetupCopyReturned: return "SetupCopyReturned";
    case InstallJournalPhase::StageReceiptCaptured:
        return "StageReceiptCaptured";
    case InstallJournalPhase::QuiesceSignalEntered: return "QuiesceSignalEntered";
    case InstallJournalPhase::QuiesceSignalReturned: return "QuiesceSignalReturned";
    case InstallJournalPhase::RootRegistrationIntentCaptured:
        return "RootRegistrationIntentCaptured";
    case InstallJournalPhase::RootRegistrationEntered: return "RootRegistrationEntered";
    case InstallJournalPhase::RootRegistrationReturned: return "RootRegistrationReturned";
    case InstallJournalPhase::DiInstallEntered: return "DiInstallEntered";
    case InstallJournalPhase::DiInstallReturned: return "DiInstallReturned";
    case InstallJournalPhase::PriorAbiProfileCaptured:
        return "PriorAbiProfileCaptured";
    case InstallJournalPhase::DriverValidated: return "DriverValidated";
    case InstallJournalPhase::BrokerHandoffEntered: return "BrokerHandoffEntered";
    case InstallJournalPhase::BrokerHandoffReturned: return "BrokerHandoffReturned";
    case InstallJournalPhase::BrokerChildEntered: return "BrokerChildEntered";
    case InstallJournalPhase::BrokerChildSettled: return "BrokerChildSettled";
    case InstallJournalPhase::RollbackBindingEntered: return "RollbackBindingEntered";
    case InstallJournalPhase::PartialRootRemovalEntered:
        return "PartialRootRemovalEntered";
    case InstallJournalPhase::PartialRootRemovalReturned:
        return "PartialRootRemovalReturned";
    case InstallJournalPhase::PartialRootRemovalRebootPending:
        return "PartialRootRemovalRebootPending";
    case InstallJournalPhase::RollbackBindingReturned: return "RollbackBindingReturned";
    case InstallJournalPhase::SetupUninstallEntered: return "SetupUninstallEntered";
    case InstallJournalPhase::SetupUninstallReturned: return "SetupUninstallReturned";
    case InstallJournalPhase::ForwardValidated: return "ForwardValidated";
    case InstallJournalPhase::ExactPriorRestored: return "ExactPriorRestored";
    case InstallJournalPhase::ForwardRebootPending: return "ForwardRebootPending";
    case InstallJournalPhase::RestoreRebootPending: return "RestoreRebootPending";
    case InstallJournalPhase::ManualReconciliationRequired:
        return "ManualReconciliationRequired";
    }
    return "ManualReconciliationRequired";
}

std::optional<InstallJournalPhase> ParseInstallJournalPhase(
    std::string_view value) noexcept {
    for (InstallJournalPhase phase : {
            InstallJournalPhase::Prepared,
            InstallJournalPhase::SetupCopyEntered,
            InstallJournalPhase::SetupCopyReturned,
            InstallJournalPhase::StageReceiptCaptured,
            InstallJournalPhase::QuiesceSignalEntered,
            InstallJournalPhase::QuiesceSignalReturned,
            InstallJournalPhase::RootRegistrationIntentCaptured,
            InstallJournalPhase::RootRegistrationEntered,
            InstallJournalPhase::RootRegistrationReturned,
            InstallJournalPhase::DiInstallEntered,
            InstallJournalPhase::DiInstallReturned,
            InstallJournalPhase::PriorAbiProfileCaptured,
            InstallJournalPhase::DriverValidated,
            InstallJournalPhase::BrokerHandoffEntered,
            InstallJournalPhase::BrokerHandoffReturned,
            InstallJournalPhase::BrokerChildEntered,
            InstallJournalPhase::BrokerChildSettled,
            InstallJournalPhase::RollbackBindingEntered,
            InstallJournalPhase::PartialRootRemovalEntered,
            InstallJournalPhase::PartialRootRemovalReturned,
            InstallJournalPhase::PartialRootRemovalRebootPending,
            InstallJournalPhase::RollbackBindingReturned,
            InstallJournalPhase::SetupUninstallEntered,
            InstallJournalPhase::SetupUninstallReturned,
            InstallJournalPhase::ForwardValidated,
            InstallJournalPhase::ExactPriorRestored,
            InstallJournalPhase::ForwardRebootPending,
            InstallJournalPhase::RestoreRebootPending,
            InstallJournalPhase::ManualReconciliationRequired}) {
        if (value == InstallJournalPhaseName(phase)) {
            return phase;
        }
    }
    return std::nullopt;
}

bool InstallJournalPhaseRequiresPriorAbiProfile(
    InstallJournalPhase phase) noexcept {
    switch (phase) {
    case InstallJournalPhase::PriorAbiProfileCaptured:
    case InstallJournalPhase::DiInstallEntered:
    case InstallJournalPhase::DiInstallReturned:
        return true;
    default:
        return false;
    }
}

const char* InstallJournalDirectionName(
    InstallJournalDirection direction) noexcept {
    return direction == InstallJournalDirection::Rollback
        ? "rollback" : "forward";
}

std::optional<InstallJournalDirection> ParseInstallJournalDirection(
    std::string_view value) noexcept {
    if (value == "forward") return InstallJournalDirection::Forward;
    if (value == "rollback") return InstallJournalDirection::Rollback;
    return std::nullopt;
}

bool Utf8ToWide(std::string_view value, std::wstring* wide, Error* error) {
    if (value.size() > static_cast<size_t>(std::numeric_limits<int>::max())) {
        return SetError(error, L"install-journal-utf8", ERROR_BUFFER_OVERFLOW);
    }
    if (value.empty()) {
        wide->clear();
        return true;
    }
    const int bytes = static_cast<int>(value.size());
    const int required = MultiByteToWideChar(
        CP_UTF8, MB_ERR_INVALID_CHARS, value.data(), bytes, nullptr, 0);
    if (required <= 0) {
        return SetLastErrorDetail(error, L"install-journal-utf8");
    }
    wide->assign(static_cast<size_t>(required), L'\0');
    if (MultiByteToWideChar(
            CP_UTF8, MB_ERR_INVALID_CHARS, value.data(), bytes,
            wide->data(), required) != required) {
        return SetLastErrorDetail(error, L"install-journal-utf8");
    }
    return true;
}

void AppendJsonUtf8String(std::string* output, std::string_view value) {
    static constexpr char digits[] = "0123456789abcdef";
    output->push_back('"');
    for (unsigned char character : value) {
        if (character == '"' || character == '\\') {
            output->push_back('\\');
            output->push_back(static_cast<char>(character));
        } else if (character < 0x20U) {
            output->append("\\u00");
            output->push_back(digits[(character >> 4U) & 0x0fU]);
            output->push_back(digits[character & 0x0fU]);
        } else {
            output->push_back(static_cast<char>(character));
        }
    }
    output->push_back('"');
}

bool ResolveInstallRecoveryPaths(
    std::filesystem::path* programData,
    std::filesystem::path* product,
    std::filesystem::path* component,
    std::filesystem::path* transactions,
    std::filesystem::path* active,
    Error* error) {
    PWSTR raw = nullptr;
    const HRESULT result = SHGetKnownFolderPath(
        FOLDERID_ProgramData, KF_FLAG_DEFAULT, nullptr, &raw);
    if (FAILED(result) || raw == nullptr) {
        return SetError(error, L"install-journal-programdata",
            HRESULT_CODE(result == S_OK ? E_FAIL : result));
    }
    try {
        *programData = std::filesystem::path(raw).lexically_normal();
        *product = *programData / kInstallRecoveryProductDirectory;
        *component = *product / kInstallRecoveryComponentDirectory;
        *transactions = *component / kInstallRecoveryTransactionsDirectory;
        *active = *transactions / kInstallRecoveryActiveDirectory;
    } catch (...) {
        CoTaskMemFree(raw);
        throw;
    }
    CoTaskMemFree(raw);
    if (!programData->is_absolute() ||
        active->lexically_relative(*programData).empty()) {
        return SetError(error, L"install-journal-programdata", ERROR_INVALID_NAME,
            L"known ProgramData did not resolve an absolute journal parent");
    }
    return true;
}

bool OpenStableDirectory(
    const std::filesystem::path& path,
    bool exactProtectedSecurity,
    WinHandle* handle,
    Error* error) {
    handle->reset(CreateFileW(
        path.c_str(), FILE_LIST_DIRECTORY | FILE_READ_ATTRIBUTES | READ_CONTROL,
        FILE_SHARE_READ | FILE_SHARE_WRITE, nullptr, OPEN_EXISTING,
        FILE_ATTRIBUTE_NORMAL | FILE_FLAG_OPEN_REPARSE_POINT |
            FILE_FLAG_BACKUP_SEMANTICS,
        nullptr));
    if (!*handle) {
        return SetLastErrorDetail(error, L"install-journal-directory-open");
    }
    FILE_ATTRIBUTE_TAG_INFO attributes{};
    if (!GetFileInformationByHandleEx(
            handle->get(), FileAttributeTagInfo, &attributes,
            sizeof(attributes)) ||
        (attributes.FileAttributes & FILE_ATTRIBUTE_DIRECTORY) == 0 ||
        (attributes.FileAttributes & FILE_ATTRIBUTE_REPARSE_POINT) != 0) {
        return SetError(error, L"install-journal-directory-open",
            ERROR_REPARSE_TAG_MISMATCH,
            L"install journal components must be regular non-reparse directories");
    }
    return !exactProtectedSecurity || VerifyProtectedFileSystemSecurity(
        handle->get(), true, L"install-journal-directory-security", error);
}

bool CreateOrOpenInstallRecoveryDirectoryWithSecurity(
    const std::filesystem::path& path,
    bool allowExisting,
    bool exactProtectedSecurity,
    const wchar_t* securitySddl,
    WinHandle* handle,
    bool* created,
    Error* error) {
    LocalSecurityDescriptor security;
    if (!security.Initialize(
            securitySddl,
            L"install-journal-directory-security", error)) {
        return false;
    }
    *created = false;
    if (CreateDirectoryW(path.c_str(), security.attributes())) {
        *created = true;
    } else {
        const DWORD code = GetLastError();
        if (code != ERROR_ALREADY_EXISTS || !allowExisting) {
            return SetError(error, L"install-journal-directory-create",
                code == ERROR_ALREADY_EXISTS ? ERROR_INSTALL_SUSPEND : code,
                code == ERROR_ALREADY_EXISTS
                    ? L"an unfinished native driver transaction already exists"
                    : std::wstring{});
        }
    }
    return OpenStableDirectory(path, exactProtectedSecurity, handle, error);
}

bool CreateOrOpenInstallRecoveryDirectory(
    const std::filesystem::path& path,
    bool allowExisting,
    bool exactProtectedSecurity,
    WinHandle* handle,
    bool* created,
    Error* error) {
    return CreateOrOpenInstallRecoveryDirectoryWithSecurity(
        path, allowExisting, exactProtectedSecurity,
        kRollbackDirectorySecurity, handle, created, error);
}

bool BuildInstallRecoveryProductDirectorySecurity(
    const std::wstring& targetUserSid,
    std::wstring* sddl,
    Error* error) {
    if (!IsSafeTargetUserSid(targetUserSid)) {
        return SetError(error, L"install-journal-product-security",
            ERROR_INVALID_SID,
            L"fresh product-directory creation requires one canonical target-user SID");
    }
    *sddl = L"O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
        L"(A;OICI;GRGX;;;" + targetUserSid + L")";
    return true;
}

bool InstallRecoveryChainHasActive(
    bool productExists,
    bool componentExists,
    bool transactionsExist,
    bool activeExists) noexcept {
    return productExists && componentExists &&
        transactionsExist && activeExists;
}

bool OpenExistingInstallRecoveryDirectory(
    const std::filesystem::path& path,
    bool exactProtectedSecurity,
    WinHandle* handle,
    bool* exists,
    Error* error) {
    *exists = false;
    const DWORD attributes = GetFileAttributesW(path.c_str());
    if (attributes == INVALID_FILE_ATTRIBUTES) {
        const DWORD code = GetLastError();
        if (code == ERROR_FILE_NOT_FOUND || code == ERROR_PATH_NOT_FOUND) {
            return true;
        }
        return SetError(error, L"install-journal-discovery", code);
    }
    if (!OpenStableDirectory(path, exactProtectedSecurity, handle, error)) {
        return false;
    }
    *exists = true;
    return true;
}

struct InstallRecoveryDirectory {
    std::filesystem::path programData;
    std::filesystem::path product;
    std::filesystem::path component;
    std::filesystem::path transactions;
    std::filesystem::path active;
    WinHandle programDataHandle;
    WinHandle productHandle;
    WinHandle componentHandle;
    WinHandle transactionsHandle;
    WinHandle activeHandle;
    bool activeCreated = false;

    bool OpenChain(
        bool createActive,
        const std::wstring* exactTargetUserSid,
        bool* exists,
        Error* error) {
        *exists = false;
        if (!ResolveInstallRecoveryPaths(
                &programData, &product, &component, &transactions, &active,
                error) ||
            !OpenStableDirectory(
                programData, false, &programDataHandle, error)) {
            return false;
        }
        if (!createActive) {
            bool productExists = false;
            bool componentExists = false;
            bool transactionsExist = false;
            bool activeExists = false;
            if (!OpenExistingInstallRecoveryDirectory(
                    product, false, &productHandle,
                    &productExists, error)) {
                return false;
            }
            if (!productExists) return true;
            if (!VerifyProtectedProductDirectorySecurity(
                    productHandle.get(), nullptr, error) ||
                !OpenExistingInstallRecoveryDirectory(
                    component, true, &componentHandle,
                    &componentExists, error)) {
                return false;
            }
            if (!componentExists) return true;
            if (!OpenExistingInstallRecoveryDirectory(
                    transactions, true, &transactionsHandle,
                    &transactionsExist, error)) {
                return false;
            }
            if (!transactionsExist) return true;
            if (!OpenExistingInstallRecoveryDirectory(
                    active, true, &activeHandle,
                    &activeExists, error)) {
                return false;
            }
            *exists = InstallRecoveryChainHasActive(
                productExists, componentExists,
                transactionsExist, activeExists);
            return true;
        }
        if (exactTargetUserSid == nullptr) {
            return SetError(error, L"install-journal-product-security",
                ERROR_INVALID_PARAMETER,
                L"fresh install journal creation requires the exact target-user SID");
        }
        std::wstring productSecurity;
        bool created = false;
        if (!BuildInstallRecoveryProductDirectorySecurity(
                *exactTargetUserSid, &productSecurity, error) ||
            !CreateOrOpenInstallRecoveryDirectoryWithSecurity(
                product, true, false, productSecurity.c_str(),
                &productHandle, &created, error) ||
            !VerifyProtectedProductDirectorySecurity(
                productHandle.get(), exactTargetUserSid, error) ||
            !CreateOrOpenInstallRecoveryDirectory(
                component, true, true, &componentHandle, &created, error) ||
            !CreateOrOpenInstallRecoveryDirectory(
                transactions, true, true, &transactionsHandle, &created,
                error)) {
            return false;
        }
        if (createActive) {
            const bool opened = CreateOrOpenInstallRecoveryDirectory(
                active, false, true, &activeHandle, &created, error);
            activeCreated = created;
            if (!opened) {
                return false;
            }
            *exists = true;
            return true;
        }
        if (!OpenStableDirectory(active, true, &activeHandle, error)) {
            return false;
        }
        *exists = true;
        return true;
    }
};

bool RetireInstallRecoveryActiveDirectory(
    InstallRecoveryDirectory* directory,
    std::string_view transactionId,
    Error* error) {
    if (directory == nullptr || !IsSha256Digest(transactionId) ||
        directory->active.filename() != kInstallRecoveryActiveDirectory) {
        return SetError(error, L"install-journal-retire-identity",
            ERROR_INVALID_PARAMETER);
    }
    std::wstring transactionIdWide(
        transactionId.begin(), transactionId.end());
    const std::filesystem::path tombstone =
        directory->transactions /
        (std::wstring(kInstallRecoverySettledPrefix) + transactionIdWide);
    directory->activeHandle.reset();
    if (!MoveFileExW(directory->active.c_str(), tombstone.c_str(),
            MOVEFILE_WRITE_THROUGH)) {
        return SetLastErrorDetail(error, L"install-journal-retire-rename",
            L"terminal journal could not be atomically moved out of active admission");
    }
    const DWORD activeAttributes = GetFileAttributesW(directory->active.c_str());
    const DWORD activeError = activeAttributes == INVALID_FILE_ATTRIBUTES
        ? GetLastError() : ERROR_SUCCESS;
    if (activeAttributes != INVALID_FILE_ATTRIBUTES ||
        (activeError != ERROR_FILE_NOT_FOUND &&
            activeError != ERROR_PATH_NOT_FOUND)) {
        if (error != nullptr) {
            error->recoveryBackup = tombstone.wstring();
            error->recoveryBackupRetained = true;
        }
        return SetError(error, L"install-journal-retire-active-absence",
            activeAttributes != INVALID_FILE_ATTRIBUTES
                ? ERROR_ALREADY_EXISTS : activeError,
            L"atomic retirement did not prove active-v2 absent");
    }
    WinHandle tombstoneHandle;
    if (!OpenStableDirectory(
            tombstone, true, &tombstoneHandle, error)) {
        if (error != nullptr) {
            error->recoveryBackup = tombstone.wstring();
            error->recoveryBackupRetained = true;
        }
        return false;
    }
    ClearActiveRecoveryEvidence();
    tombstoneHandle.reset();

    // Once active-v2 is atomically absent, cleanup is intentionally
    // best-effort. A power loss may leave a settled-v2-* tombstone, but it is
    // outside active admission and its transaction-bound name cannot be
    // confused with an unfinished transaction.
    std::error_code removalError;
    std::filesystem::remove_all(tombstone, removalError);
    if (removalError) {
        std::wstring diagnostic =
            L"VIIPER: settled install journal tombstone retained after cleanup error ";
        diagnostic += std::to_wstring(removalError.value());
        diagnostic += L".\n";
        OutputDebugStringW(diagnostic.c_str());
    }
    return true;
}

bool PublishInstallRecoveryEvidence(
    const std::filesystem::path& active,
    uint64_t sequence,
    Error* error) {
    std::wostringstream name;
    name << kInstallRecoveryJournalPrefix << std::setw(8) << std::setfill(L'0')
         << sequence << kInstallRecoveryJournalSuffix;
    const std::filesystem::path record = active / name.str();
    const std::wstring activeValue = active.wstring();
    const std::wstring recordValue = record.wstring();
    if (activeValue.empty() || recordValue.empty() ||
        activeValue.size() >= gActiveBackupRoot.size() ||
        recordValue.size() >= gActiveRecoveryRecord.size()) {
        return SetError(error, L"install-journal-evidence",
            ERROR_FILENAME_EXCED_RANGE,
            L"fixed recovery journal path exceeds the exception-safe reporting bound");
    }
    ClearActiveRecoveryEvidence();
    std::copy(activeValue.begin(), activeValue.end(), gActiveBackupRoot.begin());
    std::copy(recordValue.begin(), recordValue.end(), gActiveRecoveryRecord.begin());
    gActiveBackupRootRetained = true;
    return true;
}

bool GetBootIdentifier(std::string* identifier, Error* error) {
    using NtQuerySystemInformationFn = LONG(NTAPI*)(ULONG, PVOID, ULONG, PULONG);
    struct BootEnvironmentInformation {
        GUID bootIdentifier;
        ULONG firmwareType;
        ULONGLONG bootFlags;
    } information{};
    const HMODULE ntdll = GetModuleHandleW(L"ntdll.dll");
    const auto query = ntdll == nullptr ? nullptr
        : reinterpret_cast<NtQuerySystemInformationFn>(
            GetProcAddress(ntdll, "NtQuerySystemInformation"));
    if (query == nullptr || query(90U, &information,
            static_cast<ULONG>(sizeof(information)), nullptr) < 0) {
        return SetError(error, L"install-journal-boot-identifier",
            ERROR_NOT_SUPPORTED,
            L"the current boot session could not be identified durably");
    }
    wchar_t value[64]{};
    if (StringFromGUID2(information.bootIdentifier, value,
            static_cast<int>(std::size(value))) <= 0) {
        return SetError(error, L"install-journal-boot-identifier",
            ERROR_INVALID_DATA);
    }
    identifier->clear();
    for (wchar_t character : std::wstring_view(value)) {
        if (character == L'{' || character == L'}' || character == L'-') {
            continue;
        }
        if (character > 0x7f) {
            return SetError(error, L"install-journal-boot-identifier",
                ERROR_INVALID_DATA);
        }
        identifier->push_back(static_cast<char>(
            std::tolower(static_cast<unsigned char>(character))));
    }
    if (identifier->size() != 32U) {
        return SetError(error, L"install-journal-boot-identifier",
            ERROR_INVALID_DATA);
    }
    return true;
}

bool IsCanonicalBootIdentifier(std::string_view identifier) noexcept {
    return identifier.size() == 32U &&
        std::all_of(identifier.begin(), identifier.end(),
            [](unsigned char character) {
                return (character >= '0' && character <= '9') ||
                    (character >= 'a' && character <= 'f');
            });
}

struct InstallJournalStateData {
    InstallJournalPhase phase = InstallJournalPhase::Prepared;
    InstallJournalDirection direction = InstallJournalDirection::Forward;
    bool rollbackAuthorized = false;
    uint64_t sequence = 0;
    std::string previousDigest = std::string(kZeroSha256);
    std::string lastDigest;
    std::string transactionId;
    std::string bootIdentifier;
    std::string pendingRebootBootIdentifier;
    std::string sourceRevision;
    bool production = true;
    bool localTest = false;
    bool brokerRequired = false;
    bool brokerEntered = false;
    bool brokerSettled = false;
    bool hasBrokerProof = false;
    bool brokerProofSuccess = false;
    bool brokerProofChanged = false;
    bool brokerDriverRollbackAuthorized = false;
    std::string brokerProofRollback;
    DWORD brokerProofExitCode = ERROR_SUCCESS;
    bool hasPriorAbiProfile = false;
    AbiCompatibilityProfile priorAbiProfile{};
    bool hasRootRegistrationIntent = false;
    std::wstring rootRegistrationInstanceId;
    enum class PartialRootRemovalBinding {
        None,
        Unbound,
        Candidate,
    } partialRootRemovalBinding = PartialRootRemovalBinding::None;
    std::string partialRootRemovalBootIdentifier;
    Snapshot prior;
    PackageInfo candidate;
    PackageInfo publishedCandidate;
    bool hasPublishedCandidate = false;
    std::vector<PackageInfo> expectedInventory;
    bool packageStagedHere = false;
    bool bindingMutationStarted = false;
    bool rebootRequired = false;
    bool freshRebootRequired = false;
    bool callSucceeded = true;
    DWORD callError = ERROR_SUCCESS;
    bool deadlineOverrun = false;
};

bool ValidateInstallJournalTransition(
    const InstallJournalStateData* previous,
    const InstallJournalStateData& next,
    Error* error);

bool VerifyInstallJournalRawPriorTopology(
    const InstallJournalStateData& state,
    Error* error);

bool VerifyInstallJournalRawForwardTopology(
    const InstallJournalStateData& state,
    Error* error);

void AppendPackageIdentityJson(
    std::string* output,
    const PackageInfo& package,
    std::wstring_view backupInf) {
    output->append("{\"publishedInf\":");
    AppendJsonString(output, package.publishedName);
    output->append(",\"version\":");
    AppendJsonString(output, VersionToString(package.version));
    output->append(",\"infSha256\":");
    AppendJsonAsciiString(output, LowerAscii(package.infSha256));
    output->append(",\"sysSha256\":");
    AppendJsonAsciiString(output, LowerAscii(package.sysSha256));
    output->append(",\"catSha256\":");
    AppendJsonAsciiString(output, LowerAscii(package.catSha256));
    output->append(",\"backupInf\":");
    AppendJsonString(output, backupInf);
    output->push_back('}');
}

bool BuildInstallJournalPayload(
    const InstallJournalStateData& state,
    std::string* payload,
    Error* error) {
    const bool priorRequiresAbiProfile =
        state.prior.devices.size() == 1U &&
        state.prior.devices[0].started &&
        state.prior.devices[0].problem == 0;
    const bool authoritativeRebootReturn =
        state.phase == InstallJournalPhase::DiInstallReturned ||
        state.phase == InstallJournalPhase::RollbackBindingReturned ||
        state.phase ==
            InstallJournalPhase::PartialRootRemovalReturned;
    const bool partialRootRemovalPhase =
        state.phase == InstallJournalPhase::PartialRootRemovalEntered ||
        state.phase == InstallJournalPhase::PartialRootRemovalReturned ||
        state.phase == InstallJournalPhase::
            PartialRootRemovalRebootPending;
    const bool hasPartialRootRemovalBinding =
        state.partialRootRemovalBinding !=
            InstallJournalStateData::PartialRootRemovalBinding::None;
    const bool rebootPendingPhase =
        state.phase == InstallJournalPhase::ForwardRebootPending ||
        state.phase == InstallJournalPhase::RestoreRebootPending;
    if (!IsSha256Digest(state.previousDigest) ||
        !IsCanonicalBootIdentifier(state.bootIdentifier) ||
        (!state.pendingRebootBootIdentifier.empty() &&
            !IsCanonicalBootIdentifier(
                state.pendingRebootBootIdentifier)) ||
        (!state.partialRootRemovalBootIdentifier.empty() &&
            !IsCanonicalBootIdentifier(
                state.partialRootRemovalBootIdentifier)) ||
        (partialRootRemovalPhase &&
            (state.partialRootRemovalBootIdentifier.empty() ||
                !hasPartialRootRemovalBinding)) ||
        (state.partialRootRemovalBootIdentifier.empty() !=
            !hasPartialRootRemovalBinding) ||
        (!state.partialRootRemovalBootIdentifier.empty() &&
            (!state.hasRootRegistrationIntent ||
                !state.prior.devices.empty() ||
                state.direction != InstallJournalDirection::Rollback ||
                !state.rollbackAuthorized)) ||
        (!state.rebootRequired &&
            !state.pendingRebootBootIdentifier.empty()) ||
        (rebootPendingPhase &&
            state.pendingRebootBootIdentifier.empty()) ||
        (state.freshRebootRequired &&
            (!state.rebootRequired ||
                state.pendingRebootBootIdentifier.empty() ||
                !authoritativeRebootReturn)) ||
        !IsSha256Digest(state.candidate.infSha256) ||
        !IsSha256Digest(state.candidate.sysSha256) ||
        !IsSha256Digest(state.candidate.catSha256) ||
        (state.hasPriorAbiProfile &&
            !IsKnownAbiCompatibilityProfile(state.priorAbiProfile)) ||
        (state.hasRootRegistrationIntent &&
            (!state.prior.devices.empty() ||
                !state.hasPublishedCandidate ||
                !IsGeneratedRootInstanceIdForDeviceName(
                    state.rootRegistrationInstanceId,
                    kRootDeviceName))) ||
        (!state.hasRootRegistrationIntent &&
            (!state.rootRegistrationInstanceId.empty() ||
                state.phase ==
                    InstallJournalPhase::RootRegistrationIntentCaptured ||
                (state.prior.devices.empty() &&
                    state.bindingMutationStarted))) ||
        (state.phase == InstallJournalPhase::RootRegistrationIntentCaptured &&
            (state.direction != InstallJournalDirection::Forward ||
                state.bindingMutationStarted)) ||
        (state.hasBrokerProof &&
            !BrokerProofFieldsAreCanonical(
                state.brokerProofSuccess,
                state.brokerProofChanged,
                state.brokerProofRollback,
                state.brokerProofExitCode,
                state.brokerDriverRollbackAuthorized)) ||
        (state.hasBrokerProof &&
            state.brokerDriverRollbackAuthorized !=
                state.rollbackAuthorized) ||
        (state.brokerSettled && !state.hasBrokerProof &&
            !state.rollbackAuthorized) ||
        ((state.direction == InstallJournalDirection::Rollback) !=
            state.rollbackAuthorized) ||
        (priorRequiresAbiProfile &&
            (InstallJournalPhaseRequiresPriorAbiProfile(state.phase) ||
                state.bindingMutationStarted) &&
            !state.hasPriorAbiProfile) ||
        state.sequence >= kMaximumInstallRecoveryRecords) {
        return SetError(error, L"install-journal-state", ERROR_INVALID_DATA);
    }
    payload->clear();
    payload->append("{\"sequence\":");
    payload->append(std::to_string(state.sequence));
    payload->append(",\"previousSha256\":");
    AppendJsonAsciiString(payload, LowerAscii(state.previousDigest));
    payload->append(",\"phase\":");
    AppendJsonAsciiString(payload, InstallJournalPhaseName(state.phase));
    payload->append(",\"direction\":");
    AppendJsonAsciiString(payload,
        InstallJournalDirectionName(state.direction));
    payload->append(",\"rollbackAuthorized\":");
    payload->append(state.rollbackAuthorized ? "true" : "false");
    payload->append(",\"transactionId\":");
    AppendJsonAsciiString(payload, state.transactionId);
    payload->append(",\"bootIdentifier\":");
    AppendJsonAsciiString(payload, state.bootIdentifier);
    payload->append(",\"pendingRebootBootIdentifier\":");
    if (state.pendingRebootBootIdentifier.empty()) {
        payload->append("null");
    } else {
        AppendJsonAsciiString(
            payload, state.pendingRebootBootIdentifier);
    }
    payload->append(",\"sourceRevision\":");
    AppendJsonAsciiString(payload, LowerAscii(state.sourceRevision));
    payload->append(",\"production\":");
    payload->append(state.production ? "true" : "false");
    payload->append(",\"localTest\":");
    payload->append(state.localTest ? "true" : "false");
    payload->append(",\"brokerRequired\":");
    payload->append(state.brokerRequired ? "true" : "false");
    payload->append(",\"brokerEntered\":");
    payload->append(state.brokerEntered ? "true" : "false");
    payload->append(",\"brokerSettled\":");
    payload->append(state.brokerSettled ? "true" : "false");
    payload->append(",\"brokerProof\":");
    if (state.hasBrokerProof) {
        payload->append("{\"success\":");
        payload->append(state.brokerProofSuccess ? "true" : "false");
        payload->append(",\"changed\":");
        payload->append(state.brokerProofChanged ? "true" : "false");
        payload->append(",\"rollback\":");
        AppendJsonAsciiString(payload, state.brokerProofRollback);
        payload->append(",\"exitCode\":");
        payload->append(std::to_string(state.brokerProofExitCode));
        payload->append(",\"driverRollbackAuthorized\":");
        payload->append(state.brokerDriverRollbackAuthorized
            ? "true" : "false");
        payload->push_back('}');
    } else {
        payload->append("null");
    }
    payload->append(",\"priorAbiProfile\":");
    if (state.hasPriorAbiProfile) {
        payload->append("{\"minor\":");
        payload->append(std::to_string(state.priorAbiProfile.minor));
        payload->append(",\"capabilities\":");
        payload->append(std::to_string(state.priorAbiProfile.capabilities));
        payload->append(",\"statsSize\":");
        payload->append(std::to_string(state.priorAbiProfile.statsSize));
        payload->append(",\"hasReservedPortFields\":");
        payload->append(state.priorAbiProfile.hasReservedPortFields
            ? "true" : "false");
        payload->push_back('}');
    } else {
        payload->append("null");
    }
    payload->append(",\"rootRegistrationInstanceId\":");
    if (state.hasRootRegistrationIntent) {
        AppendJsonString(payload, state.rootRegistrationInstanceId);
    } else {
        payload->append("null");
    }
    payload->append(",\"partialRootRemovalBootIdentifier\":");
    if (state.partialRootRemovalBootIdentifier.empty()) {
        payload->append("null");
    } else {
        AppendJsonAsciiString(
            payload, state.partialRootRemovalBootIdentifier);
    }
    payload->append(",\"partialRootRemovalBinding\":");
    switch (state.partialRootRemovalBinding) {
    case InstallJournalStateData::PartialRootRemovalBinding::None:
        payload->append("null");
        break;
    case InstallJournalStateData::PartialRootRemovalBinding::Unbound:
        AppendJsonAsciiString(payload, "unbound");
        break;
    case InstallJournalStateData::PartialRootRemovalBinding::Candidate:
        AppendJsonAsciiString(payload, "candidate");
        break;
    }
    payload->append(",\"packageStagedHere\":");
    payload->append(state.packageStagedHere ? "true" : "false");
    payload->append(",\"bindingMutationStarted\":");
    payload->append(state.bindingMutationStarted ? "true" : "false");
    payload->append(",\"rebootRequired\":");
    payload->append(state.rebootRequired ? "true" : "false");
    payload->append(",\"freshRebootRequired\":");
    payload->append(state.freshRebootRequired ? "true" : "false");
    payload->append(",\"callSucceeded\":");
    payload->append(state.callSucceeded ? "true" : "false");
    payload->append(",\"callError\":");
    payload->append(std::to_string(state.callError));
    payload->append(",\"deadlineOverrun\":");
    payload->append(state.deadlineOverrun ? "true" : "false");
    payload->append(",\"candidate\":");
    AppendPackageIdentityJson(payload, state.candidate,
        std::wstring(kInstallRecoveryCandidateDirectory) + L"/ViiperUde.inf");
    payload->append(",\"publishedCandidate\":");
    if (state.hasPublishedCandidate) {
        AppendPackageIdentityJson(payload, state.publishedCandidate,
            std::wstring(kInstallRecoveryCandidateDirectory) + L"/ViiperUde.inf");
    } else {
        payload->append("null");
    }
    payload->append(",\"priorPackages\":[");
    for (size_t index = 0; index < state.prior.packages.size(); ++index) {
        if (index != 0) payload->push_back(',');
        AppendPackageIdentityJson(payload, state.prior.packages[index],
            std::wstring(kInstallRecoveryPriorDirectory) + L"/" +
                std::to_wstring(index) + L"/ViiperUde.inf");
    }
    payload->append("],\"priorDevices\":[");
    for (size_t index = 0; index < state.prior.devices.size(); ++index) {
        if (index != 0) payload->push_back(',');
        const DeviceState& device = state.prior.devices[index];
        payload->append("{\"instanceId\":");
        AppendJsonString(payload, device.instanceId);
        payload->append(",\"present\":");
        payload->append(device.present ? "true" : "false");
        payload->append(",\"started\":");
        payload->append(device.started ? "true" : "false");
        payload->append(",\"problem\":");
        payload->append(std::to_string(device.problem));
        payload->append(",\"service\":");
        AppendJsonString(payload, device.service);
        payload->append(",\"publishedInf\":");
        AppendJsonString(payload, device.publishedInf);
        payload->append(",\"version\":");
        AppendJsonString(payload, VersionToString(device.version));
        payload->append(",\"packageInfSha256\":");
        AppendJsonAsciiString(payload, LowerAscii(device.package.infSha256));
        payload->append(",\"packageSysSha256\":");
        AppendJsonAsciiString(payload, LowerAscii(device.package.sysSha256));
        payload->append(",\"packageCatSha256\":");
        AppendJsonAsciiString(payload, LowerAscii(device.package.catSha256));
        payload->push_back('}');
    }
    payload->append("],\"expectedInventory\":[");
    for (size_t index = 0; index < state.expectedInventory.size(); ++index) {
        if (index != 0) payload->push_back(',');
        AppendPackageIdentityJson(payload, state.expectedInventory[index], L"");
    }
    payload->append("]}");
    if (payload->size() > kMaximumRecoveryRecordBytes) {
        return SetError(error, L"install-journal-size", ERROR_FILE_TOO_LARGE);
    }
    return true;
}

bool WriteInstallJournalRecord(
    const std::filesystem::path& active,
    InstallJournalStateData* state,
    Error* error) {
    std::string payload;
    std::string digest;
    if (!BuildInstallJournalPayload(*state, &payload, error) ||
        !Sha256Data(payload, &digest, error)) {
        return false;
    }
    std::string record = "{\"schema\":2,\"kind\":";
    AppendJsonAsciiString(&record, kInstallRecoveryKind);
    record.append(",\"payloadSha256\":");
    AppendJsonAsciiString(&record, digest);
    record.append(",\"payload\":");
    AppendJsonUtf8String(&record, payload);
    record.append("}\n");
    if (record.size() > kMaximumRecoveryRecordBytes) {
        return SetError(error, L"install-journal-size", ERROR_FILE_TOO_LARGE);
    }

    std::wostringstream finalName;
    finalName << kInstallRecoveryJournalPrefix << std::setw(8)
              << std::setfill(L'0') << state->sequence
              << kInstallRecoveryJournalSuffix;
    const std::filesystem::path finalPath = active / finalName.str();
    const std::filesystem::path temporaryPath =
        active / (finalName.str() + kInstallRecoveryTemporarySuffix);
    LocalSecurityDescriptor security;
    if (!security.Initialize(
            kRecoveryRecordSecurity, L"install-journal-file-security", error)) {
        return false;
    }
    WinHandle file(CreateFileW(
        temporaryPath.c_str(),
        GENERIC_READ | GENERIC_WRITE | FILE_READ_ATTRIBUTES | READ_CONTROL,
        FILE_SHARE_READ, security.attributes(), CREATE_NEW,
        FILE_ATTRIBUTE_NORMAL | FILE_FLAG_OPEN_REPARSE_POINT |
            FILE_FLAG_WRITE_THROUGH,
        nullptr));
    if (!file) {
        return SetLastErrorDetail(error, L"install-journal-create");
    }
    const auto discard = [&]() noexcept {
        file.reset();
        DeleteFileW(temporaryPath.c_str());
    };
    FILE_ATTRIBUTE_TAG_INFO attributes{};
    if (!GetFileInformationByHandleEx(
            file.get(), FileAttributeTagInfo, &attributes, sizeof(attributes)) ||
        (attributes.FileAttributes &
            (FILE_ATTRIBUTE_DIRECTORY | FILE_ATTRIBUTE_REPARSE_POINT)) != 0 ||
        !VerifyProtectedFileSystemSecurity(
            file.get(), false, L"install-journal-file-security", error)) {
        if (error->code == ERROR_SUCCESS) {
            SetError(error, L"install-journal-create", ERROR_REPARSE_TAG_MISMATCH);
        }
        discard();
        return false;
    }
    size_t offset = 0;
    while (offset < record.size()) {
        DWORD written = 0;
        const DWORD requested = static_cast<DWORD>(std::min<size_t>(
            record.size() - offset, MAXDWORD));
        if (!WriteFile(file.get(), record.data() + offset, requested,
                &written, nullptr) || written == 0) {
            const DWORD code = GetLastError() == ERROR_SUCCESS
                ? ERROR_WRITE_FAULT : GetLastError();
            SetError(error, L"install-journal-write", code);
            discard();
            return false;
        }
        offset += written;
    }
    if (!FlushFileBuffers(file.get())) {
        SetLastErrorDetail(error, L"install-journal-flush");
        discard();
        return false;
    }
    file.reset();
    if (!MoveFileExW(
            temporaryPath.c_str(), finalPath.c_str(), MOVEFILE_WRITE_THROUGH)) {
        const DWORD code = GetLastError();
        DeleteFileW(temporaryPath.c_str());
        return SetError(error, L"install-journal-publish", code);
    }
    file.reset(CreateFileW(
        finalPath.c_str(), GENERIC_READ | FILE_READ_ATTRIBUTES | READ_CONTROL,
        FILE_SHARE_READ, nullptr, OPEN_EXISTING,
        FILE_ATTRIBUTE_NORMAL | FILE_FLAG_OPEN_REPARSE_POINT |
            FILE_FLAG_SEQUENTIAL_SCAN,
        nullptr));
    if (!file || !VerifyProtectedFileSystemSecurity(
            file.get(), false, L"install-journal-file-security", error)) {
        if (!file) SetLastErrorDetail(error, L"install-journal-reopen");
        return false;
    }
    std::string observed(record.size(), '\0');
    DWORD read = 0;
    if (!ReadFile(file.get(), observed.data(), static_cast<DWORD>(observed.size()),
            &read, nullptr) || read != observed.size() || observed != record) {
        return SetError(error, L"install-journal-readback", ERROR_CRC,
            L"published journal record does not match the flushed bytes");
    }
    char trailing = 0;
    DWORD trailingRead = 0;
    if (!ReadFile(file.get(), &trailing, 1, &trailingRead, nullptr) ||
        trailingRead != 0) {
        return SetError(error, L"install-journal-readback", ERROR_FILE_INVALID,
            L"published journal record has trailing bytes");
    }
    state->lastDigest = digest;
    state->previousDigest = digest;
    ++state->sequence;
    gActiveRecoveryRecordWritten = true;
    return true;
}

bool GenerateInstallTransactionId(std::string* identifier, Error* error) {
    HCRYPTPROV provider = 0;
    if (!CryptAcquireContextW(
            &provider, nullptr, nullptr, PROV_RSA_AES,
            CRYPT_VERIFYCONTEXT | CRYPT_SILENT)) {
        return SetLastErrorDetail(error, L"install-journal-transaction-id");
    }
    std::array<BYTE, 32> random{};
    const BOOL generated = CryptGenRandom(
        provider, static_cast<DWORD>(random.size()), random.data());
    const DWORD code = generated ? ERROR_SUCCESS : GetLastError();
    CryptReleaseContext(provider, 0);
    if (!generated) {
        return SetError(error, L"install-journal-transaction-id", code);
    }
    static constexpr char digits[] = "0123456789abcdef";
    identifier->clear();
    identifier->reserve(random.size() * 2U);
    for (BYTE value : random) {
        identifier->push_back(digits[value >> 4U]);
        identifier->push_back(digits[value & 0x0fU]);
    }
    return true;
}

bool CopyCandidateIntoInstallJournal(
    const std::filesystem::path& sourceDirectory,
    const std::filesystem::path& destinationDirectory,
    const PackageInfo& expected,
    bool localTest,
    PackageInfo* verified,
    std::vector<WinHandle>* locks,
    Error* error) {
    if (!CopyProtectedBackupFile(
            sourceDirectory / L"ViiperUde.inf",
            destinationDirectory / L"ViiperUde.inf", error) ||
        !CopyProtectedBackupFile(
            sourceDirectory / kDriverFileName,
            destinationDirectory / kDriverFileName, error) ||
        !CopyProtectedBackupFile(
            sourceDirectory / kCatalogName,
            destinationDirectory / kCatalogName, error) ||
        !ValidateExactPackageDirectory(destinationDirectory, error)) {
        return false;
    }
    bool owned = false;
    if (!LoadOwnedPackage(
            destinationDirectory / L"ViiperUde.inf", true, localTest,
            verified, &owned, error) || !owned ||
        !(verified->version == expected.version) ||
        !SamePackageBytes(*verified, expected)) {
        if (error->code == ERROR_SUCCESS) {
            SetError(error, L"install-journal-candidate-identity",
                ERROR_REVISION_MISMATCH,
                L"protected candidate copy differs from the reviewed package bytes");
        }
        return false;
    }
    verified->publishedName.clear();
    return LockPackageFiles(destinationDirectory, locks, error);
}

struct InstallJournal::Impl {
    InstallRecoveryDirectory directory;
    InstallJournalStateData state;
    std::vector<PackageBackup> priorBackups;
    std::vector<WinHandle> candidateLocks;
    bool preparedRecord = false;
    bool retired = false;
    bool poisoned = false;
    bool evidenceMayBeDurable = false;
    bool forwardRootRegistrationEntered = false;
    bool forwardDiInstallEntered = false;
    bool partialRootRemovalEntered = false;

    ~Impl() noexcept {
        if (preparedRecord || retired || poisoned || evidenceMayBeDurable ||
            !directory.activeCreated ||
            directory.active.empty()) {
            return;
        }
        candidateLocks.clear();
        priorBackups.clear();
        directory.activeHandle.reset();
        std::error_code ignored;
        std::filesystem::remove_all(directory.active, ignored);
        std::error_code presenceError;
        if (!std::filesystem::exists(directory.active, presenceError) &&
            !presenceError) {
            ClearActiveRecoveryEvidence();
        }
    }
};

InstallJournal::InstallJournal() = default;
InstallJournal::~InstallJournal() = default;

bool InstallJournal::Prepare(
    const Snapshot& prior,
    const PackageInfo& candidate,
    const std::filesystem::path& candidateDirectory,
    const std::vector<PackageInfo>& expectedInventory,
    const InstallOptions& options,
    Error* error) {
    impl_ = std::make_unique<Impl>();
    bool exists = false;
    if (!impl_->directory.OpenChain(
            true, &options.targetUserSid, &exists, error) || !exists ||
        !PublishInstallRecoveryEvidence(impl_->directory.active, 0, error)) {
        return false;
    }
    bool created = false;
    WinHandle priorDirectoryHandle;
    WinHandle candidateDirectoryHandle;
    const std::filesystem::path priorDirectory =
        impl_->directory.active / kInstallRecoveryPriorDirectory;
    const std::filesystem::path protectedCandidateDirectory =
        impl_->directory.active / kInstallRecoveryCandidateDirectory;
    if (!CreateOrOpenInstallRecoveryDirectory(
            priorDirectory, false, true, &priorDirectoryHandle, &created,
            error) ||
        !CreateOrOpenInstallRecoveryDirectory(
            protectedCandidateDirectory, false, true,
            &candidateDirectoryHandle, &created, error) ||
        !BackupPackagesIntoDirectory(
            prior.packages, priorDirectory, &impl_->priorBackups, error)) {
        return false;
    }
    PackageInfo protectedCandidate;
    if (!CopyCandidateIntoInstallJournal(
            candidateDirectory, protectedCandidateDirectory, candidate,
            options.localTest, &protectedCandidate, &impl_->candidateLocks,
            error)) {
        return false;
    }

    impl_->state.prior = prior;
    impl_->state.candidate = candidate;
    impl_->state.expectedInventory = expectedInventory;
    impl_->state.production = options.production;
    impl_->state.localTest = options.localTest;
    impl_->state.brokerRequired = !options.brokerExecutable.empty();
    impl_->state.sourceRevision = options.sourceRevision;
    if (!GetBootIdentifier(&impl_->state.bootIdentifier, error)) {
        return false;
    }
    if (!options.brokerTokenSha256.empty()) {
        impl_->state.transactionId = LowerAscii(options.brokerTokenSha256);
    } else if (!GenerateInstallTransactionId(
                   &impl_->state.transactionId, error)) {
        return false;
    }
    impl_->state.phase = InstallJournalPhase::Prepared;
    if (!ValidateInstallJournalTransition(
            nullptr, impl_->state, error)) {
        return false;
    }
    impl_->evidenceMayBeDurable = true;
    if (!WriteInstallJournalRecord(
            impl_->directory.active, &impl_->state, error)) {
        impl_->poisoned = true;
        return false;
    }
    impl_->preparedRecord = true;
    if (!PublishInstallRecoveryEvidence(
            impl_->directory.active, impl_->state.sequence - 1U, error)) {
        impl_->poisoned = true;
        return false;
    }
    gActiveRecoveryRecordWritten = true;
    return true;
}

bool InstallJournal::Record(
    InstallJournalPhase phase,
    const PackageInfo* publishedCandidate,
    bool packageStagedHere,
    bool bindingMutationStarted,
    bool rebootRequired,
    bool callSucceeded,
    DWORD callError,
    bool deadlineOverrun,
    Error* error) {
    if (!impl_) {
        return SetError(error, L"install-journal-state", ERROR_INVALID_STATE,
            L"install journal is not armed for a phase transition");
    }
    return RecordNext(impl_->state, phase, publishedCandidate,
        packageStagedHere, bindingMutationStarted, rebootRequired,
        callSucceeded, callError, deadlineOverrun, false, error);
}

bool InstallJournal::RecordAuthoritativeReturn(
    InstallJournalPhase phase,
    const PackageInfo* publishedCandidate,
    bool packageStagedHere,
    bool bindingMutationStarted,
    bool rebootRequired,
    bool freshRebootRequired,
    bool callSucceeded,
    DWORD callError,
    bool deadlineOverrun,
    Error* error) {
    if (!impl_ ||
        (phase != InstallJournalPhase::DiInstallReturned &&
            phase != InstallJournalPhase::RollbackBindingReturned &&
            phase !=
                InstallJournalPhase::PartialRootRemovalReturned)) {
        return SetError(error, L"install-journal-reboot-return",
            ERROR_INVALID_PARAMETER);
    }
    InstallJournalStateData next = impl_->state;
    if (freshRebootRequired &&
        !GetBootIdentifier(
            &next.pendingRebootBootIdentifier, error)) {
        return false;
    }
    return RecordNext(std::move(next), phase, publishedCandidate,
        packageStagedHere, bindingMutationStarted, rebootRequired,
        callSucceeded, callError, deadlineOverrun,
        freshRebootRequired, error);
}

bool InstallJournal::RecordNext(
    InstallJournalStateData next,
    InstallJournalPhase phase,
    const PackageInfo* publishedCandidate,
    bool packageStagedHere,
    bool bindingMutationStarted,
    bool rebootRequired,
    bool callSucceeded,
    DWORD callError,
    bool deadlineOverrun,
    bool freshRebootRequired,
    Error* error) {
    if (!impl_ || !impl_->preparedRecord || impl_->retired ||
        impl_->poisoned) {
        return SetError(error, L"install-journal-state", ERROR_INVALID_STATE,
            impl_ && impl_->poisoned
                ? L"install journal is poisoned after an indeterminate durable append; restart into recovery"
                : L"install journal is not armed for a phase transition");
    }
    next.phase = phase;
    next.packageStagedHere =
        next.packageStagedHere || packageStagedHere;
    const bool phaseMayMutateBinding =
        phase == InstallJournalPhase::RootRegistrationEntered ||
        phase == InstallJournalPhase::RootRegistrationReturned ||
        phase == InstallJournalPhase::DiInstallEntered ||
        phase == InstallJournalPhase::DiInstallReturned;
    next.bindingMutationStarted =
        next.bindingMutationStarted || bindingMutationStarted ||
        phaseMayMutateBinding;
    next.rebootRequired = next.rebootRequired || rebootRequired;
    next.freshRebootRequired = freshRebootRequired;
    next.callSucceeded = callSucceeded;
    next.callError = callError;
    next.deadlineOverrun = next.deadlineOverrun || deadlineOverrun;
    if (phase == InstallJournalPhase::RollbackBindingEntered) {
        next.direction = InstallJournalDirection::Rollback;
        next.rollbackAuthorized = true;
    }
    if (publishedCandidate != nullptr) {
        next.publishedCandidate = *publishedCandidate;
        next.hasPublishedCandidate = true;
        if (packageStagedHere &&
            !ContainsExactPackage(
                next.expectedInventory, *publishedCandidate)) {
            next.expectedInventory.push_back(*publishedCandidate);
            std::sort(next.expectedInventory.begin(),
                next.expectedInventory.end(),
                [](const PackageInfo& left, const PackageInfo& right) {
                    return _wcsicmp(left.publishedName.c_str(),
                        right.publishedName.c_str()) < 0;
                });
        }
    }
    if (phase == InstallJournalPhase::BrokerHandoffEntered ||
        phase == InstallJournalPhase::BrokerHandoffReturned ||
        phase == InstallJournalPhase::BrokerChildEntered ||
        phase == InstallJournalPhase::BrokerChildSettled) {
        next.brokerEntered = true;
    }
    if (phase == InstallJournalPhase::BrokerChildSettled) {
        next.brokerSettled = true;
    }
    if (!ValidateInstallJournalTransition(&impl_->state, next, error) ||
        !WriteInstallJournalRecord(
            impl_->directory.active, &next, error)) {
        impl_->poisoned = true;
        return false;
    }
    impl_->state = std::move(next);
    if (impl_->state.direction == InstallJournalDirection::Forward &&
        phase == InstallJournalPhase::RootRegistrationEntered) {
        impl_->forwardRootRegistrationEntered = true;
    }
    if (impl_->state.direction == InstallJournalDirection::Forward &&
        phase == InstallJournalPhase::DiInstallEntered) {
        impl_->forwardDiInstallEntered = true;
    }
    if (phase == InstallJournalPhase::PartialRootRemovalEntered) {
        impl_->partialRootRemovalEntered = true;
    }
    if (!PublishInstallRecoveryEvidence(
            impl_->directory.active, impl_->state.sequence - 1U, error)) {
        impl_->poisoned = true;
        return false;
    }
    gActiveRecoveryRecordWritten = true;
    return true;
}

bool InstallJournal::RecordCutpoint(
    InstallJournalPhase phase,
    bool callSucceeded,
    DWORD callError,
    bool deadlineOverrun,
    bool rebootRequired,
    bool freshRebootRequired,
    Error* error) {
    if (!impl_) {
        return true;
    }
    const PackageInfo* publishedCandidate =
        impl_->state.hasPublishedCandidate
            ? &impl_->state.publishedCandidate : nullptr;
    if (phase == InstallJournalPhase::DiInstallReturned ||
        phase == InstallJournalPhase::RollbackBindingReturned ||
        phase == InstallJournalPhase::PartialRootRemovalReturned) {
        return RecordAuthoritativeReturn(phase, publishedCandidate,
            impl_->state.packageStagedHere,
            impl_->state.bindingMutationStarted,
            impl_->state.rebootRequired || rebootRequired,
            freshRebootRequired, callSucceeded, callError,
            deadlineOverrun, error);
    }
    if (freshRebootRequired) {
        return SetError(error, L"install-journal-reboot-return",
            ERROR_INVALID_PARAMETER,
            L"fresh reboot authority is legal only on an authoritative returned phase");
    }
    return Record(phase, publishedCandidate,
        impl_->state.packageStagedHere,
        impl_->state.bindingMutationStarted,
        impl_->state.rebootRequired || rebootRequired,
        callSucceeded, callError, deadlineOverrun, error);
}

bool InstallJournal::RecordPriorAbiProfile(
    const AbiCompatibilityProfile& profile,
    const PackageInfo& publishedCandidate,
    bool packageStagedHere,
    Error* error) {
    if (!impl_ || !IsKnownAbiCompatibilityProfile(profile) ||
        impl_->state.prior.devices.size() != 1U ||
        !impl_->state.prior.devices[0].started ||
        impl_->state.prior.devices[0].problem != 0) {
        return SetError(error, L"install-journal-prior-abi-profile",
            ERROR_INVALID_PARAMETER);
    }
    if (impl_->state.hasPriorAbiProfile &&
        !SameAbiCompatibilityProfile(
            impl_->state.priorAbiProfile, profile)) {
        return SetError(error, L"install-journal-prior-abi-profile",
            ERROR_REVISION_MISMATCH,
            L"captured prior ABI profile changed within one transaction");
    }
    InstallJournalStateData next = impl_->state;
    next.priorAbiProfile = profile;
    next.hasPriorAbiProfile = true;
    return RecordNext(std::move(next),
        InstallJournalPhase::PriorAbiProfileCaptured,
        &publishedCandidate, packageStagedHere,
        impl_->state.bindingMutationStarted,
        impl_->state.rebootRequired, true, ERROR_SUCCESS, false,
        false, error);
}

bool InstallJournal::RecordRootRegistrationIntent(
    const std::wstring& instanceId,
    Error* error) {
    if (!impl_ || !impl_->state.prior.devices.empty() ||
        !impl_->state.hasPublishedCandidate ||
        !IsGeneratedRootInstanceIdForDeviceName(
            instanceId, kRootDeviceName)) {
        return SetError(error, L"install-journal-root-intent",
            ERROR_INVALID_PARAMETER);
    }
    if (impl_->state.hasRootRegistrationIntent &&
        _wcsicmp(impl_->state.rootRegistrationInstanceId.c_str(),
            instanceId.c_str()) != 0) {
        return SetError(error, L"install-journal-root-intent",
            ERROR_REVISION_MISMATCH,
            L"generated root identity changed within one transaction");
    }
    InstallJournalStateData next = impl_->state;
    next.hasRootRegistrationIntent = true;
    next.rootRegistrationInstanceId = instanceId;
    return RecordNext(std::move(next),
        InstallJournalPhase::RootRegistrationIntentCaptured,
        &impl_->state.publishedCandidate,
        impl_->state.packageStagedHere,
        impl_->state.bindingMutationStarted,
        impl_->state.rebootRequired,
        true, ERROR_SUCCESS, false, false, error);
}

bool InstallJournal::RecordBrokerProof(
    const BrokerCommitProof& proof,
    Error* error) {
    if (!impl_ || !BrokerProofFieldsAreCanonical(
            proof.success, proof.changed, proof.rollback,
            proof.exitCode, proof.driverRollbackAuthorized)) {
        return SetError(error, L"install-journal-broker-proof",
            ERROR_INVALID_DATA);
    }
    if (impl_->state.hasBrokerProof &&
        (impl_->state.brokerProofSuccess != proof.success ||
            impl_->state.brokerProofChanged != proof.changed ||
            impl_->state.brokerProofRollback != proof.rollback ||
            impl_->state.brokerProofExitCode != proof.exitCode ||
            impl_->state.brokerDriverRollbackAuthorized !=
                proof.driverRollbackAuthorized)) {
        return SetError(error, L"install-journal-broker-proof",
            ERROR_REVISION_MISMATCH,
            L"settled broker proof changed within one transaction");
    }
    InstallJournalStateData next = impl_->state;
    next.brokerEntered = true;
    next.brokerSettled = true;
    next.hasBrokerProof = true;
    next.brokerProofSuccess = proof.success;
    next.brokerProofChanged = proof.changed;
    next.brokerProofRollback = proof.rollback;
    next.brokerProofExitCode = proof.exitCode;
    next.brokerDriverRollbackAuthorized =
        proof.driverRollbackAuthorized;
    if (proof.driverRollbackAuthorized) {
        next.direction = InstallJournalDirection::Rollback;
        next.rollbackAuthorized = true;
    }
    return RecordNext(std::move(next),
        InstallJournalPhase::BrokerChildSettled,
        impl_->state.hasPublishedCandidate
            ? &impl_->state.publishedCandidate : nullptr,
        impl_->state.packageStagedHere,
        impl_->state.bindingMutationStarted,
        impl_->state.rebootRequired,
        proof.success, proof.exitCode, false, false, error);
}

bool InstallJournal::RecordRollbackAuthorization(
    InstallJournalPhase phase,
    DWORD callError,
    Error* error) {
    if (!impl_ ||
        (phase != InstallJournalPhase::BrokerHandoffReturned &&
            phase != InstallJournalPhase::BrokerChildSettled)) {
        return SetError(error, L"install-journal-rollback-authorization",
            ERROR_INVALID_PARAMETER);
    }
    InstallJournalStateData next = impl_->state;
    next.direction = InstallJournalDirection::Rollback;
    next.rollbackAuthorized = true;
    return RecordNext(std::move(next), phase,
        impl_->state.hasPublishedCandidate
            ? &impl_->state.publishedCandidate : nullptr,
        impl_->state.packageStagedHere,
        impl_->state.bindingMutationStarted,
        impl_->state.rebootRequired,
        false, callError, false, false, error);
}

bool RecordActiveInstallJournalCutpoint(
    InstallJournalPhase phase,
    bool callSucceeded,
    DWORD callError,
    bool deadlineOverrun,
    Error* error) {
    return gActiveInstallJournal == nullptr ||
        gActiveInstallJournal->RecordCutpoint(
            phase, callSucceeded, callError, deadlineOverrun,
            false, false, error);
}

bool RecordActiveInstallJournalCutpointWithReboot(
    InstallJournalPhase phase,
    bool callSucceeded,
    DWORD callError,
    bool deadlineOverrun,
    bool rebootRequired,
    bool freshRebootRequired,
    Error* error) {
    return gActiveInstallJournal == nullptr ||
        gActiveInstallJournal->RecordCutpoint(
            phase, callSucceeded, callError, deadlineOverrun,
            rebootRequired, freshRebootRequired, error);
}

bool RecordActiveInstallJournalRollbackAuthorization(
    InstallJournalPhase phase,
    DWORD callError,
    Error* error) {
    return gActiveInstallJournal == nullptr ||
        gActiveInstallJournal->RecordRollbackAuthorization(
            phase, callError, error);
}

bool RecordActiveInstallJournalRootRegistrationIntent(
    const std::wstring& instanceId,
    Error* error) {
    return gActiveInstallJournal == nullptr ||
        gActiveInstallJournal->RecordRootRegistrationIntent(
            instanceId, error);
}

void InstallJournal::AttachEvidence(Error* error) const {
    if (error == nullptr || !impl_) {
        return;
    }
    error->recoveryBackup = impl_->directory.active.wstring();
    error->recoveryBackupRetained = true;
    if (gActiveRecoveryRecord[0] != L'\0') {
        error->recoveryRecord = gActiveRecoveryRecord.data();
        error->recoveryRecordWritten = gActiveRecoveryRecordWritten;
    }
}

bool InstallJournal::RetireAfterForwardValidation(
    const PackageInfo& candidate,
    const std::wstring& publishedName,
    bool rebootRequired,
    uint64_t deadlineUnixMs,
    Error* error) {
    if (!impl_ || !impl_->preparedRecord || impl_->retired ||
        impl_->poisoned) {
        return SetError(error, L"install-journal-retire", ERROR_INVALID_STATE);
    }
    if (rebootRequired) {
        if (impl_->state.pendingRebootBootIdentifier.empty()) {
            return SetError(error, L"install-journal-forward-reboot-epoch",
                ERROR_INVALID_DATA,
                L"forward reboot pending lacks an authoritative returned reboot epoch");
        }
        return Record(InstallJournalPhase::ForwardRebootPending,
            &impl_->state.publishedCandidate,
            impl_->state.packageStagedHere,
            impl_->state.bindingMutationStarted, true, true,
            ERROR_SUCCESS_REBOOT_REQUIRED, false, error);
    }
    std::string expectedBuildIdentity;
    if (!DeriveDriverBuildIdentity(
            impl_->state.sourceRevision, &expectedBuildIdentity, error) ||
        !VerifyInstallJournalRawForwardTopology(impl_->state, error) ||
        !VerifyInstalled(candidate, publishedName, false, deadlineUnixMs,
            &expectedBuildIdentity, error) ||
        !VerifyPackageInventory(
            impl_->state.expectedInventory,
            L"install-journal-retire-inventory", error) ||
        !Record(InstallJournalPhase::ForwardValidated,
            &impl_->state.publishedCandidate,
            impl_->state.packageStagedHere,
            impl_->state.bindingMutationStarted, false, true,
            ERROR_SUCCESS, false, error) ||
        !VerifyInstallJournalRawForwardTopology(impl_->state, error) ||
        !VerifyInstalled(candidate, publishedName, false, deadlineUnixMs,
            &expectedBuildIdentity, error) ||
        !VerifyPackageInventory(
            impl_->state.expectedInventory,
            L"install-journal-retire-revalidation", error)) {
        return false;
    }
    impl_->candidateLocks.clear();
    impl_->priorBackups.clear();
    if (!RetireInstallRecoveryActiveDirectory(
            &impl_->directory, impl_->state.transactionId, error)) {
        return false;
    }
    impl_->retired = true;
    impl_->preparedRecord = false;
    ClearActiveRecoveryEvidence();
    return true;
}

bool InstallJournal::RetireAfterPriorValidation(
    bool rebootRequired,
    Error* error) {
    if (!impl_ || !impl_->preparedRecord || impl_->retired ||
        impl_->poisoned) {
        return SetError(error, L"install-journal-retire", ERROR_INVALID_STATE);
    }
    if (rebootRequired &&
        !impl_->state.pendingRebootBootIdentifier.empty()) {
        return Record(InstallJournalPhase::RestoreRebootPending,
            impl_->state.hasPublishedCandidate
                ? &impl_->state.publishedCandidate : nullptr,
            impl_->state.packageStagedHere,
            impl_->state.bindingMutationStarted, true, true,
            ERROR_SUCCESS_REBOOT_REQUIRED, false, error);
    }
    const auto validatePrior = [&]() {
        Snapshot observed;
        if (!VerifyInstallJournalRawPriorTopology(impl_->state, error) ||
            !CaptureSnapshot(&observed, error) ||
            !SameCapturedRootState(impl_->state.prior, observed) ||
            !SamePackageInventory(
                impl_->state.prior.packages, observed.packages)) {
            return false;
        }
        if (impl_->state.bindingMutationStarted &&
            !impl_->state.prior.devices.empty() &&
            impl_->state.prior.devices[0].started) {
            if (!impl_->state.hasPriorAbiProfile) {
                return SetError(error,
                    L"install-journal-prior-abi-profile",
                    ERROR_REVISION_MISMATCH,
                    L"started prior root lacks its durable exact ABI profile");
            }
            return VerifyAbiHealth(
                CurrentUnixMilliseconds() + 15000U, nullptr, error,
                AbiHealthPurpose::RollbackHealth,
                &impl_->state.priorAbiProfile, nullptr);
        }
        return true;
    };
    if (!validatePrior()) {
        if (error->code == ERROR_SUCCESS) {
            SetError(error, L"install-journal-prior-revalidation",
                ERROR_REVISION_MISMATCH,
                L"exact captured root and package inventory were not restored");
        }
        return false;
    }
    if (!Record(InstallJournalPhase::ExactPriorRestored,
            impl_->state.hasPublishedCandidate
                ? &impl_->state.publishedCandidate : nullptr,
            impl_->state.packageStagedHere,
            impl_->state.bindingMutationStarted, false, true,
            ERROR_SUCCESS, false, error) ||
        !validatePrior()) {
        return false;
    }
    impl_->candidateLocks.clear();
    impl_->priorBackups.clear();
    if (!RetireInstallRecoveryActiveDirectory(
            &impl_->directory, impl_->state.transactionId, error)) {
        return false;
    }
    impl_->retired = true;
    impl_->preparedRecord = false;
    ClearActiveRecoveryEvidence();
    return true;
}

bool RequireJournalObject(
    const JsonValue& value,
    const JsonValue::Object** object,
    Error* error) {
    *object = std::get_if<JsonValue::Object>(&value.value);
    return *object != nullptr || SetError(
        error, L"install-journal-parse", ERROR_INVALID_DATA,
        L"journal field must be a JSON object");
}

bool RequireJournalArray(
    const JsonValue::Object& object,
    const char* name,
    const JsonValue::Array** array,
    Error* error) {
    const JsonValue* field = ObjectField(object, name);
    *array = field == nullptr ? nullptr
        : std::get_if<JsonValue::Array>(&field->value);
    return *array != nullptr || SetError(
        error, L"install-journal-parse", ERROR_INVALID_DATA,
        L"journal array field is missing or malformed");
}

bool RequireJournalString(
    const JsonValue::Object& object,
    const char* name,
    std::string* value,
    Error* error) {
    const JsonValue* field = ObjectField(object, name);
    const std::string* stringValue = field == nullptr ? nullptr
        : std::get_if<std::string>(&field->value);
    if (stringValue == nullptr) {
        return SetError(error, L"install-journal-parse", ERROR_INVALID_DATA,
            L"journal string field is missing or malformed");
    }
    *value = *stringValue;
    return true;
}

bool RequireJournalBool(
    const JsonValue::Object& object,
    const char* name,
    bool* value,
    Error* error) {
    const JsonValue* field = ObjectField(object, name);
    const bool* boolValue = field == nullptr ? nullptr
        : std::get_if<bool>(&field->value);
    if (boolValue == nullptr) {
        return SetError(error, L"install-journal-parse", ERROR_INVALID_DATA,
            L"journal Boolean field is missing or malformed");
    }
    *value = *boolValue;
    return true;
}

bool RequireJournalUnsigned(
    const JsonValue::Object& object,
    const char* name,
    uint64_t maximum,
    uint64_t* value,
    Error* error) {
    const JsonValue* field = ObjectField(object, name);
    const int64_t* integer = field == nullptr ? nullptr
        : std::get_if<int64_t>(&field->value);
    if (integer == nullptr || *integer < 0 ||
        static_cast<uint64_t>(*integer) > maximum) {
        return SetError(error, L"install-journal-parse", ERROR_INVALID_DATA,
            L"journal integer field is missing or out of range");
    }
    *value = static_cast<uint64_t>(*integer);
    return true;
}

bool ParseJournalPackageIdentity(
    const JsonValue& value,
    const std::filesystem::path& active,
    bool requirePublishedName,
    PackageInfo* package,
    std::filesystem::path* backupInf,
    Error* error) {
    const JsonValue::Object* object = nullptr;
    std::string published;
    std::string version;
    std::string infSha;
    std::string sysSha;
    std::string catSha;
    std::string relative;
    if (!RequireJournalObject(value, &object, error) ||
        !RequireJournalString(*object, "publishedInf", &published, error) ||
        !RequireJournalString(*object, "version", &version, error) ||
        !RequireJournalString(*object, "infSha256", &infSha, error) ||
        !RequireJournalString(*object, "sysSha256", &sysSha, error) ||
        !RequireJournalString(*object, "catSha256", &catSha, error) ||
        !RequireJournalString(*object, "backupInf", &relative, error)) {
        return false;
    }
    std::wstring publishedWide;
    std::wstring versionWide;
    std::wstring relativeWide;
    if (!Utf8ToWide(published, &publishedWide, error) ||
        !Utf8ToWide(version, &versionWide, error) ||
        !Utf8ToWide(relative, &relativeWide, error) ||
        !ParseVersion(versionWide, &package->version) ||
        !IsSha256Digest(infSha) || !IsSha256Digest(sysSha) ||
        !IsSha256Digest(catSha) ||
        (requirePublishedName && !IsSafePublishedInfName(publishedWide))) {
        return SetError(error, L"install-journal-package", ERROR_INVALID_DATA,
            L"journal package identity is malformed");
    }
    package->publishedName = std::move(publishedWide);
    package->infSha256 = LowerAscii(std::move(infSha));
    package->sysSha256 = LowerAscii(std::move(sysSha));
    package->catSha256 = LowerAscii(std::move(catSha));
    if (relativeWide.empty()) {
        backupInf->clear();
        package->infPath.clear();
        return true;
    }
    const std::filesystem::path relativePath(relativeWide);
    if (!IsSafeRecoveryRelativePath(relativePath)) {
        return SetError(error, L"install-journal-package-path",
            ERROR_INVALID_NAME);
    }
    *backupInf = (active / relativePath).lexically_normal();
    if (backupInf->lexically_relative(active).empty()) {
        return SetError(error, L"install-journal-package-path",
            ERROR_INVALID_NAME);
    }
    package->infPath = *backupInf;
    return true;
}

bool ParseInstallJournalPayload(
    std::string_view payload,
    const std::filesystem::path& active,
    InstallJournalStateData* state,
    Error* error) {
    JsonValue root;
    std::string parseMessage;
    if (!JsonParser(payload).Parse(&root, &parseMessage)) {
        std::wstring message;
        Utf8ToWide(parseMessage, &message, nullptr);
        return SetError(error, L"install-journal-parse", ERROR_INVALID_DATA,
            L"journal payload is not canonical JSON: " + message);
    }
    const JsonValue::Object* object = nullptr;
    if (!RequireJournalObject(root, &object, error)) {
        return false;
    }
    uint64_t sequence = 0;
    uint64_t callError = 0;
    std::string previous;
    std::string phase;
    std::string direction;
    if (!RequireJournalUnsigned(*object, "sequence",
            kMaximumInstallRecoveryRecords - 1U, &sequence, error) ||
        !RequireJournalString(*object, "previousSha256", &previous, error) ||
        !RequireJournalString(*object, "phase", &phase, error) ||
        !RequireJournalString(*object, "direction", &direction, error) ||
        !RequireJournalBool(*object, "rollbackAuthorized",
            &state->rollbackAuthorized, error) ||
        !RequireJournalString(*object, "transactionId", &state->transactionId, error) ||
        !RequireJournalString(*object, "bootIdentifier", &state->bootIdentifier, error) ||
        !RequireJournalString(*object, "sourceRevision", &state->sourceRevision, error) ||
        !RequireJournalBool(*object, "production", &state->production, error) ||
        !RequireJournalBool(*object, "localTest", &state->localTest, error) ||
        !RequireJournalBool(*object, "brokerRequired", &state->brokerRequired, error) ||
        !RequireJournalBool(*object, "brokerEntered", &state->brokerEntered, error) ||
        !RequireJournalBool(*object, "brokerSettled", &state->brokerSettled, error) ||
        !RequireJournalBool(*object, "packageStagedHere", &state->packageStagedHere, error) ||
        !RequireJournalBool(*object, "bindingMutationStarted", &state->bindingMutationStarted, error) ||
        !RequireJournalBool(*object, "rebootRequired", &state->rebootRequired, error) ||
        !RequireJournalBool(*object, "freshRebootRequired",
            &state->freshRebootRequired, error) ||
        !RequireJournalBool(*object, "callSucceeded", &state->callSucceeded, error) ||
        !RequireJournalUnsigned(*object, "callError", MAXDWORD, &callError, error) ||
        !RequireJournalBool(*object, "deadlineOverrun", &state->deadlineOverrun, error)) {
        return false;
    }
    const JsonValue* pendingRebootNode =
        ObjectField(*object, "pendingRebootBootIdentifier");
    if (pendingRebootNode == nullptr) {
        return SetError(error, L"install-journal-reboot-epoch",
            ERROR_INVALID_DATA);
    }
    if (std::holds_alternative<std::nullptr_t>(
            pendingRebootNode->value)) {
        state->pendingRebootBootIdentifier.clear();
    } else {
        const auto* pendingBoot =
            std::get_if<std::string>(&pendingRebootNode->value);
        if (pendingBoot == nullptr ||
            !IsCanonicalBootIdentifier(*pendingBoot)) {
            return SetError(error, L"install-journal-reboot-epoch",
                ERROR_INVALID_DATA,
                L"pending reboot boot identifier is not one canonical boot epoch");
        }
        state->pendingRebootBootIdentifier = *pendingBoot;
    }
    const JsonValue* brokerProofNode = ObjectField(*object, "brokerProof");
    if (brokerProofNode == nullptr) {
        return SetError(error, L"install-journal-broker-proof",
            ERROR_INVALID_DATA);
    }
    if (std::holds_alternative<std::nullptr_t>(brokerProofNode->value)) {
        state->hasBrokerProof = false;
    } else {
        const JsonValue::Object* brokerProofObject = nullptr;
        uint64_t exitCode = 0;
        if (!RequireJournalObject(
                *brokerProofNode, &brokerProofObject, error) ||
            brokerProofObject->size() != 5U ||
            !RequireJournalBool(*brokerProofObject, "success",
                &state->brokerProofSuccess, error) ||
            !RequireJournalBool(*brokerProofObject, "changed",
                &state->brokerProofChanged, error) ||
            !RequireJournalString(*brokerProofObject, "rollback",
                &state->brokerProofRollback, error) ||
            !RequireJournalUnsigned(*brokerProofObject, "exitCode", MAXDWORD,
                &exitCode, error) ||
            !RequireJournalBool(*brokerProofObject,
                "driverRollbackAuthorized",
                &state->brokerDriverRollbackAuthorized, error)) {
            return false;
        }
        state->brokerProofExitCode = static_cast<DWORD>(exitCode);
        if (!BrokerProofFieldsAreCanonical(
                state->brokerProofSuccess,
                state->brokerProofChanged,
                state->brokerProofRollback,
                state->brokerProofExitCode,
                state->brokerDriverRollbackAuthorized)) {
            return SetError(error, L"install-journal-broker-proof",
                ERROR_INVALID_DATA,
                L"durable child proof is not a canonical settled outcome");
        }
        state->hasBrokerProof = true;
    }
    const JsonValue* profileNode = ObjectField(*object, "priorAbiProfile");
    if (profileNode == nullptr) {
        return SetError(error, L"install-journal-prior-abi-profile",
            ERROR_INVALID_DATA);
    }
    if (std::holds_alternative<std::nullptr_t>(profileNode->value)) {
        state->hasPriorAbiProfile = false;
    } else {
        const JsonValue::Object* profileObject = nullptr;
        uint64_t minor = 0;
        uint64_t capabilities = 0;
        uint64_t statsSize = 0;
        if (!RequireJournalObject(*profileNode, &profileObject, error) ||
            profileObject->size() != 4U ||
            !RequireJournalUnsigned(*profileObject, "minor", UINT16_MAX,
                &minor, error) ||
            !RequireJournalUnsigned(*profileObject, "capabilities", UINT32_MAX,
                &capabilities, error) ||
            !RequireJournalUnsigned(*profileObject, "statsSize", MAXDWORD,
                &statsSize, error) ||
            !RequireJournalBool(*profileObject, "hasReservedPortFields",
                &state->priorAbiProfile.hasReservedPortFields, error)) {
            return false;
        }
        state->priorAbiProfile.minor =
            static_cast<VIIPER_UDE_UINT16>(minor);
        state->priorAbiProfile.capabilities =
            static_cast<VIIPER_UDE_UINT32>(capabilities);
        state->priorAbiProfile.statsSize = static_cast<DWORD>(statsSize);
        if (!IsKnownAbiCompatibilityProfile(state->priorAbiProfile)) {
            return SetError(error, L"install-journal-prior-abi-profile",
                ERROR_REVISION_MISMATCH,
                L"journal prior ABI profile is not an exact supported contract");
        }
        state->hasPriorAbiProfile = true;
    }
    const JsonValue* rootIntentNode =
        ObjectField(*object, "rootRegistrationInstanceId");
    if (rootIntentNode == nullptr) {
        return SetError(error, L"install-journal-root-intent",
            ERROR_INVALID_DATA);
    }
    if (std::holds_alternative<std::nullptr_t>(rootIntentNode->value)) {
        state->hasRootRegistrationIntent = false;
        state->rootRegistrationInstanceId.clear();
    } else {
        const auto* encodedInstanceId =
            std::get_if<std::string>(&rootIntentNode->value);
        if (encodedInstanceId == nullptr ||
            !Utf8ToWide(*encodedInstanceId,
                &state->rootRegistrationInstanceId, error) ||
            !IsGeneratedRootInstanceIdForDeviceName(
                state->rootRegistrationInstanceId, kRootDeviceName)) {
            return SetError(error, L"install-journal-root-intent",
                ERROR_INVALID_DATA,
                L"durable root registration intent is not one exact generated VIIPER instance ID");
        }
        state->hasRootRegistrationIntent = true;
    }
    const JsonValue* rootRemovalBootNode =
        ObjectField(*object,
            "partialRootRemovalBootIdentifier");
    if (rootRemovalBootNode == nullptr) {
        return SetError(error,
            L"install-journal-partial-root-removal",
            ERROR_INVALID_DATA);
    }
    if (std::holds_alternative<std::nullptr_t>(
            rootRemovalBootNode->value)) {
        state->partialRootRemovalBootIdentifier.clear();
    } else {
        const auto* removalBoot =
            std::get_if<std::string>(&rootRemovalBootNode->value);
        if (removalBoot == nullptr ||
            !IsCanonicalBootIdentifier(*removalBoot)) {
            return SetError(error,
                L"install-journal-partial-root-removal",
                ERROR_INVALID_DATA,
                L"partial root removal lacks one canonical attempt boot epoch");
        }
        state->partialRootRemovalBootIdentifier = *removalBoot;
    }
    const JsonValue* rootRemovalBindingNode =
        ObjectField(*object, "partialRootRemovalBinding");
    if (rootRemovalBindingNode == nullptr) {
        return SetError(error,
            L"install-journal-partial-root-removal",
            ERROR_INVALID_DATA);
    }
    if (std::holds_alternative<std::nullptr_t>(
            rootRemovalBindingNode->value)) {
        state->partialRootRemovalBinding =
            InstallJournalStateData::PartialRootRemovalBinding::None;
    } else {
        const auto* binding =
            std::get_if<std::string>(&rootRemovalBindingNode->value);
        if (binding == nullptr ||
            (*binding != "unbound" && *binding != "candidate")) {
            return SetError(error,
                L"install-journal-partial-root-removal",
                ERROR_INVALID_DATA,
                L"partial root removal pre-call binding shape is not canonical");
        }
        state->partialRootRemovalBinding = *binding == "unbound"
            ? InstallJournalStateData::PartialRootRemovalBinding::Unbound
            : InstallJournalStateData::PartialRootRemovalBinding::Candidate;
    }
    const std::optional<InstallJournalPhase> parsedPhase =
        ParseInstallJournalPhase(phase);
    const std::optional<InstallJournalDirection> parsedDirection =
        ParseInstallJournalDirection(direction);
    const bool partialRootRemovalPhase = parsedPhase &&
        (*parsedPhase == InstallJournalPhase::PartialRootRemovalEntered ||
            *parsedPhase == InstallJournalPhase::PartialRootRemovalReturned ||
            *parsedPhase == InstallJournalPhase::
                PartialRootRemovalRebootPending);
    const bool hasPartialRootRemovalBinding =
        state->partialRootRemovalBinding !=
            InstallJournalStateData::PartialRootRemovalBinding::None;
    if (!parsedPhase || !parsedDirection || !IsSha256Digest(previous) ||
        !IsSha256Digest(state->transactionId) ||
        !IsCanonicalBootIdentifier(state->bootIdentifier) ||
        (!state->pendingRebootBootIdentifier.empty() &&
            !state->rebootRequired) ||
        (partialRootRemovalPhase &&
            (state->partialRootRemovalBootIdentifier.empty() ||
                !hasPartialRootRemovalBinding)) ||
        (state->partialRootRemovalBootIdentifier.empty() !=
            !hasPartialRootRemovalBinding) ||
        (!state->partialRootRemovalBootIdentifier.empty() &&
            (!state->hasRootRegistrationIntent ||
                !state->prior.devices.empty() ||
                *parsedDirection != InstallJournalDirection::Rollback ||
                !state->rollbackAuthorized)) ||
        ((*parsedPhase == InstallJournalPhase::ForwardRebootPending ||
             *parsedPhase == InstallJournalPhase::RestoreRebootPending) &&
            state->pendingRebootBootIdentifier.empty()) ||
        (state->freshRebootRequired &&
            (!state->rebootRequired ||
                state->pendingRebootBootIdentifier.empty() ||
                (*parsedPhase != InstallJournalPhase::DiInstallReturned &&
                    *parsedPhase != InstallJournalPhase::
                        RollbackBindingReturned &&
                    *parsedPhase != InstallJournalPhase::
                        PartialRootRemovalReturned))) ||
        !IsHexRevision(state->sourceRevision) ||
        (state->production && state->localTest) ||
        (state->brokerSettled && !state->brokerEntered) ||
        (state->hasBrokerProof &&
            (!state->brokerEntered || !state->brokerSettled ||
                state->brokerDriverRollbackAuthorized !=
                    state->rollbackAuthorized)) ||
        (state->brokerSettled && !state->hasBrokerProof &&
            !state->rollbackAuthorized)) {
        return SetError(error, L"install-journal-state", ERROR_INVALID_DATA,
            L"journal phase or transaction identity is inconsistent");
    }
    state->sequence = sequence;
    state->previousDigest = LowerAscii(std::move(previous));
    state->phase = *parsedPhase;
    state->direction = *parsedDirection;
    state->callError = static_cast<DWORD>(callError);

    const JsonValue* candidateNode = ObjectField(*object, "candidate");
    std::filesystem::path candidateBackup;
    if (candidateNode == nullptr ||
        !ParseJournalPackageIdentity(*candidateNode, active, false,
            &state->candidate, &candidateBackup, error) ||
        candidateBackup != active / kInstallRecoveryCandidateDirectory /
            L"ViiperUde.inf") {
        if (error->code == ERROR_SUCCESS) {
            SetError(error, L"install-journal-candidate-path",
                ERROR_INVALID_NAME);
        }
        return false;
    }
    const JsonValue* publishedNode = ObjectField(*object, "publishedCandidate");
    if (publishedNode == nullptr) {
        return SetError(error, L"install-journal-parse", ERROR_INVALID_DATA);
    }
    if (std::holds_alternative<std::nullptr_t>(publishedNode->value)) {
        state->hasPublishedCandidate = false;
    } else {
        std::filesystem::path ignoredBackup;
        if (!ParseJournalPackageIdentity(*publishedNode, active, true,
                &state->publishedCandidate, &ignoredBackup, error) ||
            !SamePackageBytes(state->publishedCandidate, state->candidate) ||
            !(state->publishedCandidate.version == state->candidate.version)) {
            return SetError(error, L"install-journal-published-candidate",
                ERROR_REVISION_MISMATCH);
        }
        state->hasPublishedCandidate = true;
    }

    const JsonValue::Array* priorPackages = nullptr;
    const JsonValue::Array* priorDevices = nullptr;
    const JsonValue::Array* expectedInventory = nullptr;
    if (!RequireJournalArray(*object, "priorPackages", &priorPackages, error) ||
        !RequireJournalArray(*object, "priorDevices", &priorDevices, error) ||
        !RequireJournalArray(*object, "expectedInventory", &expectedInventory, error) ||
        priorPackages->size() > 32U || priorDevices->size() > 1U ||
        expectedInventory->size() > 33U) {
        return SetError(error, L"install-journal-inventory",
            ERROR_INVALID_DATA);
    }
    state->prior.packages.clear();
    for (size_t index = 0; index < priorPackages->size(); ++index) {
        PackageInfo package;
        std::filesystem::path backupInf;
        const std::filesystem::path expectedBackup =
            active / kInstallRecoveryPriorDirectory /
            std::to_wstring(index) / L"ViiperUde.inf";
        if (!ParseJournalPackageIdentity(
                (*priorPackages)[index], active, true,
                &package, &backupInf, error) || backupInf != expectedBackup) {
            if (error->code == ERROR_SUCCESS) {
                SetError(error, L"install-journal-prior-package-path",
                    ERROR_INVALID_NAME);
            }
            return false;
        }
        state->prior.packages.push_back(std::move(package));
    }
    state->expectedInventory.clear();
    for (const JsonValue& packageValue : *expectedInventory) {
        PackageInfo package;
        std::filesystem::path backupInf;
        if (!ParseJournalPackageIdentity(
                packageValue, active, true, &package, &backupInf, error) ||
            !backupInf.empty()) {
            return SetError(error, L"install-journal-expected-inventory",
                ERROR_INVALID_DATA);
        }
        state->expectedInventory.push_back(std::move(package));
    }

    state->prior.devices.clear();
    for (const JsonValue& deviceValue : *priorDevices) {
        const JsonValue::Object* deviceObject = nullptr;
        std::string instanceId;
        std::string service;
        std::string publishedInf;
        std::string versionValue;
        std::string infSha;
        std::string sysSha;
        std::string catSha;
        uint64_t problem = 0;
        DeviceState device;
        if (!RequireJournalObject(deviceValue, &deviceObject, error) ||
            !RequireJournalString(*deviceObject, "instanceId", &instanceId, error) ||
            !RequireJournalBool(*deviceObject, "present", &device.present, error) ||
            !RequireJournalBool(*deviceObject, "started", &device.started, error) ||
            !RequireJournalUnsigned(*deviceObject, "problem", MAXDWORD, &problem, error) ||
            !RequireJournalString(*deviceObject, "service", &service, error) ||
            !RequireJournalString(*deviceObject, "publishedInf", &publishedInf, error) ||
            !RequireJournalString(*deviceObject, "version", &versionValue, error) ||
            !RequireJournalString(*deviceObject, "packageInfSha256", &infSha, error) ||
            !RequireJournalString(*deviceObject, "packageSysSha256", &sysSha, error) ||
            !RequireJournalString(*deviceObject, "packageCatSha256", &catSha, error) ||
            !Utf8ToWide(instanceId, &device.instanceId, error) ||
            !Utf8ToWide(service, &device.service, error) ||
            !Utf8ToWide(publishedInf, &device.publishedInf, error)) {
            return false;
        }
        std::wstring versionWide;
        if (!Utf8ToWide(versionValue, &versionWide, error) ||
            !ParseVersion(versionWide, &device.version) ||
            !IsOwnedGeneratedRootInstanceId(device.instanceId) ||
            _wcsicmp(device.service.c_str(), kServiceName) != 0 ||
            !IsSafePublishedInfName(device.publishedInf) ||
            !IsSha256Digest(infSha) || !IsSha256Digest(sysSha) ||
            !IsSha256Digest(catSha)) {
            return SetError(error, L"install-journal-prior-device",
                ERROR_INVALID_DATA);
        }
        device.problem = static_cast<ULONG>(problem);
        size_t matches = 0;
        for (const PackageInfo& package : state->prior.packages) {
            if (_wcsicmp(package.publishedName.c_str(),
                    device.publishedInf.c_str()) == 0 &&
                package.version == device.version &&
                _stricmp(package.infSha256.c_str(), infSha.c_str()) == 0 &&
                _stricmp(package.sysSha256.c_str(), sysSha.c_str()) == 0 &&
                _stricmp(package.catSha256.c_str(), catSha.c_str()) == 0) {
                device.package = package;
                ++matches;
            }
        }
        if (matches != 1U) {
            return SetError(error, L"install-journal-prior-device-package",
                ERROR_REVISION_MISMATCH);
        }
        state->prior.devices.push_back(std::move(device));
    }
    const bool priorRequiresAbiProfile =
        state->prior.devices.size() == 1U &&
        state->prior.devices[0].started &&
        state->prior.devices[0].problem == 0;
    if ((state->hasPriorAbiProfile && !priorRequiresAbiProfile) ||
        (state->phase == InstallJournalPhase::PriorAbiProfileCaptured &&
            !state->hasPriorAbiProfile) ||
        ((state->direction == InstallJournalDirection::Rollback) !=
            state->rollbackAuthorized) ||
        (priorRequiresAbiProfile &&
            (InstallJournalPhaseRequiresPriorAbiProfile(state->phase) ||
                state->bindingMutationStarted) &&
            !state->hasPriorAbiProfile) ||
        (state->hasRootRegistrationIntent &&
            (!state->prior.devices.empty() ||
                !state->hasPublishedCandidate)) ||
        (!state->hasRootRegistrationIntent &&
            (state->phase ==
                    InstallJournalPhase::RootRegistrationIntentCaptured ||
                (state->prior.devices.empty() &&
                    state->bindingMutationStarted))) ||
        (state->phase == InstallJournalPhase::RootRegistrationIntentCaptured &&
            (state->direction != InstallJournalDirection::Forward ||
                state->bindingMutationStarted))) {
        return SetError(error, L"install-journal-prior-abi-profile",
            ERROR_INVALID_DATA,
            L"journal ABI profile or root registration intent does not match the captured prior lifecycle");
    }
    std::string canonicalPayload;
    if (!BuildInstallJournalPayload(*state, &canonicalPayload, error) ||
        canonicalPayload != payload) {
        if (error->code == ERROR_SUCCESS) {
            SetError(error, L"install-journal-canonical-payload",
                ERROR_INVALID_DATA,
                L"journal payload is not in exact canonical byte form");
        }
        return false;
    }
    return true;
}

bool ReadInstallJournalFile(
    const std::filesystem::path& path,
    std::string* record,
    Error* error) {
    WinHandle file(CreateFileW(
        path.c_str(), GENERIC_READ | FILE_READ_ATTRIBUTES | READ_CONTROL,
        FILE_SHARE_READ, nullptr, OPEN_EXISTING,
        FILE_ATTRIBUTE_NORMAL | FILE_FLAG_OPEN_REPARSE_POINT |
            FILE_FLAG_SEQUENTIAL_SCAN,
        nullptr));
    if (!file) {
        return SetLastErrorDetail(error, L"install-journal-read");
    }
    FILE_ATTRIBUTE_TAG_INFO attributes{};
    if (!GetFileInformationByHandleEx(
            file.get(), FileAttributeTagInfo, &attributes, sizeof(attributes)) ||
        (attributes.FileAttributes &
            (FILE_ATTRIBUTE_DIRECTORY | FILE_ATTRIBUTE_REPARSE_POINT)) != 0 ||
        !VerifyProtectedFileSystemSecurity(
            file.get(), false, L"install-journal-file-security", error)) {
        if (error->code == ERROR_SUCCESS) {
            SetError(error, L"install-journal-read",
                ERROR_REPARSE_TAG_MISMATCH);
        }
        return false;
    }
    LARGE_INTEGER size{};
    if (!GetFileSizeEx(file.get(), &size) || size.QuadPart <= 0 ||
        static_cast<uint64_t>(size.QuadPart) > kMaximumRecoveryRecordBytes) {
        return SetError(error, L"install-journal-size",
            ERROR_FILE_TOO_LARGE);
    }
    record->assign(static_cast<size_t>(size.QuadPart), '\0');
    DWORD read = 0;
    if (!ReadFile(file.get(), record->data(),
            static_cast<DWORD>(record->size()), &read, nullptr) ||
        static_cast<size_t>(read) != record->size()) {
        return SetLastErrorDetail(error, L"install-journal-read");
    }
    return true;
}

bool ValidateAndDiscardInstallJournalTemporaryFile(
    const std::filesystem::path& path,
    Error* error) {
    WinHandle file(CreateFileW(
        path.c_str(), GENERIC_READ | FILE_READ_ATTRIBUTES | READ_CONTROL,
        FILE_SHARE_READ, nullptr, OPEN_EXISTING,
        FILE_ATTRIBUTE_NORMAL | FILE_FLAG_OPEN_REPARSE_POINT,
        nullptr));
    if (!file) {
        return SetLastErrorDetail(error, L"install-journal-temp-open");
    }
    FILE_ATTRIBUTE_TAG_INFO attributes{};
    BY_HANDLE_FILE_INFORMATION identity{};
    LARGE_INTEGER size{};
    if (!GetFileInformationByHandleEx(
            file.get(), FileAttributeTagInfo, &attributes,
            sizeof(attributes)) ||
        (attributes.FileAttributes &
            (FILE_ATTRIBUTE_DIRECTORY | FILE_ATTRIBUTE_REPARSE_POINT)) != 0 ||
        !GetFileInformationByHandle(file.get(), &identity) ||
        identity.nNumberOfLinks != 1U ||
        !GetFileSizeEx(file.get(), &size) || size.QuadPart < 0 ||
        static_cast<uint64_t>(size.QuadPart) > kMaximumRecoveryRecordBytes ||
        !VerifyProtectedFileSystemSecurity(
            file.get(), false, L"install-journal-file-security", error)) {
        if (error->code == ERROR_SUCCESS) {
            SetError(error, L"install-journal-temp-identity",
                ERROR_INVALID_DATA,
                L"unpublished journal temp must be a bounded, single-link, protected regular file");
        }
        return false;
    }
    file.reset();
    if (!DeleteFileW(path.c_str())) {
        return SetLastErrorDetail(error, L"install-journal-temp-discard");
    }
    const DWORD remaining = GetFileAttributesW(path.c_str());
    const DWORD absenceError = remaining == INVALID_FILE_ATTRIBUTES
        ? GetLastError() : ERROR_SUCCESS;
    if (remaining != INVALID_FILE_ATTRIBUTES ||
        (absenceError != ERROR_FILE_NOT_FOUND &&
            absenceError != ERROR_PATH_NOT_FOUND)) {
        return SetError(error, L"install-journal-temp-discard",
            remaining != INVALID_FILE_ATTRIBUTES
                ? ERROR_ALREADY_EXISTS : absenceError,
            L"unpublished journal temp absence could not be proven");
    }
    return true;
}

bool SameJournalPackageIdentity(
    const PackageInfo& left,
    const PackageInfo& right) noexcept {
    return _wcsicmp(left.publishedName.c_str(),
               right.publishedName.c_str()) == 0 &&
        left.version == right.version && SamePackageBytes(left, right);
}

bool SameInstallJournalImmutableState(
    const InstallJournalStateData& left,
    const InstallJournalStateData& right) noexcept {
    if (left.transactionId != right.transactionId ||
        left.bootIdentifier != right.bootIdentifier ||
        left.sourceRevision != right.sourceRevision ||
        left.production != right.production ||
        left.localTest != right.localTest ||
        left.brokerRequired != right.brokerRequired ||
        !SameJournalPackageIdentity(left.candidate, right.candidate) ||
        !SamePackageInventory(left.prior.packages, right.prior.packages) ||
        left.prior.devices.size() != right.prior.devices.size()) {
        return false;
    }
    return left.prior.devices.empty() ||
        (SameRootBinding(left.prior.devices[0], right.prior.devices[0]) &&
            left.prior.devices[0].started == right.prior.devices[0].started &&
            left.prior.devices[0].problem == right.prior.devices[0].problem);
}

bool SameDurableBrokerProof(
    const InstallJournalStateData& left,
    const InstallJournalStateData& right) noexcept {
    return left.hasBrokerProof == right.hasBrokerProof &&
        (!left.hasBrokerProof ||
            (left.brokerProofSuccess == right.brokerProofSuccess &&
                left.brokerProofChanged == right.brokerProofChanged &&
                left.brokerDriverRollbackAuthorized ==
                    right.brokerDriverRollbackAuthorized &&
                left.brokerProofRollback == right.brokerProofRollback &&
                left.brokerProofExitCode == right.brokerProofExitCode));
}

int ForwardInstallJournalPhaseRank(
    InstallJournalPhase phase) noexcept {
    switch (phase) {
    case InstallJournalPhase::Prepared: return 0;
    case InstallJournalPhase::SetupCopyEntered: return 10;
    case InstallJournalPhase::SetupCopyReturned: return 11;
    case InstallJournalPhase::StageReceiptCaptured: return 12;
    case InstallJournalPhase::QuiesceSignalEntered: return 20;
    case InstallJournalPhase::QuiesceSignalReturned: return 21;
    case InstallJournalPhase::PriorAbiProfileCaptured: return 30;
    case InstallJournalPhase::RootRegistrationIntentCaptured: return 39;
    case InstallJournalPhase::RootRegistrationEntered: return 40;
    case InstallJournalPhase::RootRegistrationReturned: return 41;
    case InstallJournalPhase::DiInstallEntered: return 50;
    case InstallJournalPhase::DiInstallReturned: return 51;
    case InstallJournalPhase::DriverValidated: return 60;
    case InstallJournalPhase::BrokerHandoffEntered: return 70;
    case InstallJournalPhase::BrokerHandoffReturned: return 71;
    case InstallJournalPhase::BrokerChildEntered: return 80;
    case InstallJournalPhase::BrokerChildSettled: return 81;
    case InstallJournalPhase::PartialRootRemovalEntered: return 82;
    case InstallJournalPhase::PartialRootRemovalReturned: return 83;
    case InstallJournalPhase::PartialRootRemovalRebootPending: return 84;
    case InstallJournalPhase::ForwardValidated: return 90;
    case InstallJournalPhase::ForwardRebootPending: return 90;
    case InstallJournalPhase::ExactPriorRestored: return 90;
    case InstallJournalPhase::RestoreRebootPending: return 90;
    case InstallJournalPhase::ManualReconciliationRequired: return 90;
    default: return -1;
    }
}

bool IsInstallJournalTerminalPhase(InstallJournalPhase phase) noexcept {
    return phase == InstallJournalPhase::ForwardValidated ||
        phase == InstallJournalPhase::ExactPriorRestored ||
        phase == InstallJournalPhase::ForwardRebootPending ||
        phase == InstallJournalPhase::RestoreRebootPending ||
        phase == InstallJournalPhase::ManualReconciliationRequired;
}

bool MatchingInstallJournalReturn(
    InstallJournalPhase entered,
    InstallJournalPhase returned) noexcept {
    return (entered == InstallJournalPhase::SetupCopyEntered &&
               returned == InstallJournalPhase::SetupCopyReturned) ||
        (entered == InstallJournalPhase::QuiesceSignalEntered &&
            returned == InstallJournalPhase::QuiesceSignalReturned) ||
        (entered == InstallJournalPhase::RootRegistrationEntered &&
            returned == InstallJournalPhase::RootRegistrationReturned) ||
        (entered == InstallJournalPhase::DiInstallEntered &&
            returned == InstallJournalPhase::DiInstallReturned) ||
        (entered == InstallJournalPhase::BrokerHandoffEntered &&
            returned == InstallJournalPhase::BrokerHandoffReturned) ||
        (entered == InstallJournalPhase::BrokerChildEntered &&
            returned == InstallJournalPhase::BrokerChildSettled) ||
        (entered == InstallJournalPhase::PartialRootRemovalEntered &&
            returned == InstallJournalPhase::PartialRootRemovalReturned) ||
        (entered == InstallJournalPhase::SetupUninstallEntered &&
            returned == InstallJournalPhase::SetupUninstallReturned);
}

bool LegalForwardInstallJournalPhaseTransition(
    InstallJournalPhase previous,
    InstallJournalPhase next) noexcept {
    if (previous == next) {
        return previous == InstallJournalPhase::Prepared ||
            previous == InstallJournalPhase::SetupCopyReturned ||
            previous == InstallJournalPhase::QuiesceSignalReturned ||
            previous == InstallJournalPhase::RootRegistrationReturned ||
            previous == InstallJournalPhase::DiInstallReturned ||
            previous == InstallJournalPhase::BrokerHandoffReturned ||
            previous == InstallJournalPhase::BrokerChildSettled;
    }
    switch (next) {
    case InstallJournalPhase::SetupCopyEntered:
        return previous == InstallJournalPhase::Prepared;
    case InstallJournalPhase::SetupCopyReturned:
    case InstallJournalPhase::QuiesceSignalReturned:
    case InstallJournalPhase::RootRegistrationReturned:
    case InstallJournalPhase::DiInstallReturned:
    case InstallJournalPhase::BrokerHandoffReturned:
    case InstallJournalPhase::BrokerChildSettled:
        return MatchingInstallJournalReturn(previous, next);
    case InstallJournalPhase::StageReceiptCaptured:
        return previous == InstallJournalPhase::SetupCopyReturned;
    case InstallJournalPhase::QuiesceSignalEntered:
        return previous == InstallJournalPhase::Prepared ||
            previous == InstallJournalPhase::StageReceiptCaptured;
    case InstallJournalPhase::PriorAbiProfileCaptured:
        return previous == InstallJournalPhase::Prepared ||
            previous == InstallJournalPhase::StageReceiptCaptured ||
            previous == InstallJournalPhase::QuiesceSignalReturned;
    case InstallJournalPhase::RootRegistrationIntentCaptured:
        return previous == InstallJournalPhase::Prepared ||
            previous == InstallJournalPhase::StageReceiptCaptured ||
            previous == InstallJournalPhase::QuiesceSignalReturned ||
            previous == InstallJournalPhase::PriorAbiProfileCaptured;
    case InstallJournalPhase::RootRegistrationEntered:
        return previous ==
            InstallJournalPhase::RootRegistrationIntentCaptured;
    case InstallJournalPhase::DiInstallEntered:
        return previous == InstallJournalPhase::Prepared ||
            previous == InstallJournalPhase::StageReceiptCaptured ||
            previous == InstallJournalPhase::QuiesceSignalReturned ||
            previous == InstallJournalPhase::PriorAbiProfileCaptured ||
            previous == InstallJournalPhase::RootRegistrationReturned;
    case InstallJournalPhase::DriverValidated:
        return previous == InstallJournalPhase::Prepared ||
            previous == InstallJournalPhase::DiInstallReturned;
    case InstallJournalPhase::BrokerHandoffEntered:
        return previous == InstallJournalPhase::DriverValidated;
    case InstallJournalPhase::BrokerChildEntered:
        return previous == InstallJournalPhase::BrokerHandoffReturned;
    case InstallJournalPhase::ForwardValidated:
    case InstallJournalPhase::ForwardRebootPending:
        return previous == InstallJournalPhase::DriverValidated ||
            previous == InstallJournalPhase::BrokerChildSettled;
    default:
        return false;
    }
}

bool LegalRollbackInstallJournalPhaseTransition(
    InstallJournalPhase previous,
    InstallJournalPhase next) noexcept {
    if (next == InstallJournalPhase::ManualReconciliationRequired ||
        next == InstallJournalPhase::ExactPriorRestored ||
        next == InstallJournalPhase::RestoreRebootPending) {
        return true;
    }
    if (next == InstallJournalPhase::RollbackBindingEntered) {
        return !IsInstallJournalTerminalPhase(previous);
    }
    if (previous == next) return true;
    if (previous == InstallJournalPhase::BrokerHandoffReturned ||
        previous == InstallJournalPhase::BrokerChildSettled) {
        return next == InstallJournalPhase::RollbackBindingEntered;
    }
    if (previous == InstallJournalPhase::RollbackBindingEntered) {
        return next == InstallJournalPhase::PartialRootRemovalEntered ||
            next == InstallJournalPhase::RootRegistrationEntered ||
            next == InstallJournalPhase::DiInstallEntered ||
            next == InstallJournalPhase::SetupUninstallEntered ||
            next == InstallJournalPhase::RollbackBindingReturned;
    }
    if (previous == InstallJournalPhase::RootRegistrationEntered ||
        previous == InstallJournalPhase::DiInstallEntered ||
        previous == InstallJournalPhase::SetupUninstallEntered) {
        return MatchingInstallJournalReturn(previous, next);
    }
    if (previous == InstallJournalPhase::PartialRootRemovalEntered) {
        return next == InstallJournalPhase::PartialRootRemovalReturned ||
            next == InstallJournalPhase::
                PartialRootRemovalRebootPending;
    }
    if (previous == InstallJournalPhase::RootRegistrationReturned) {
        return next == InstallJournalPhase::DiInstallEntered ||
            next == InstallJournalPhase::RollbackBindingReturned;
    }
    if (previous == InstallJournalPhase::DiInstallReturned) {
        return next == InstallJournalPhase::SetupUninstallEntered ||
            next == InstallJournalPhase::RollbackBindingReturned;
    }
    if (previous == InstallJournalPhase::PartialRootRemovalReturned) {
        return next == InstallJournalPhase::PartialRootRemovalEntered ||
            next == InstallJournalPhase::PartialRootRemovalRebootPending ||
            next == InstallJournalPhase::SetupUninstallEntered ||
            next == InstallJournalPhase::RollbackBindingReturned;
    }
    if (previous ==
        InstallJournalPhase::PartialRootRemovalRebootPending) {
        return next == InstallJournalPhase::RollbackBindingEntered;
    }
    if (previous == InstallJournalPhase::SetupUninstallReturned) {
        return next == InstallJournalPhase::RollbackBindingReturned;
    }
    if (previous == InstallJournalPhase::RollbackBindingReturned) {
        return next == InstallJournalPhase::ExactPriorRestored ||
            next == InstallJournalPhase::RestoreRebootPending;
    }
    return false;
}

bool ValidateInstallJournalTransition(
    const InstallJournalStateData* previous,
    const InstallJournalStateData& next,
    Error* error) {
    if (previous == nullptr) {
        if (next.sequence != 0U ||
            next.phase != InstallJournalPhase::Prepared ||
            next.direction != InstallJournalDirection::Forward ||
            next.rollbackAuthorized || next.brokerEntered ||
            next.brokerSettled || next.hasBrokerProof ||
            next.hasPriorAbiProfile ||
            next.hasRootRegistrationIntent ||
            !next.rootRegistrationInstanceId.empty() ||
            next.partialRootRemovalBinding !=
                InstallJournalStateData::PartialRootRemovalBinding::None ||
            !next.partialRootRemovalBootIdentifier.empty() ||
            !next.pendingRebootBootIdentifier.empty() ||
            next.freshRebootRequired ||
            next.hasPublishedCandidate ||
            next.packageStagedHere || next.bindingMutationStarted ||
            next.rebootRequired || next.deadlineOverrun ||
            !next.callSucceeded || next.callError != ERROR_SUCCESS) {
            return SetError(error, L"install-journal-initial-state",
                ERROR_INVALID_DATA,
                L"first journal record is not the exact immutable Prepared state");
        }
        return true;
    }
    const InstallJournalStateData& prior = *previous;
    if (IsInstallJournalTerminalPhase(prior.phase) ||
        (prior.direction == InstallJournalDirection::Rollback &&
            next.direction != InstallJournalDirection::Rollback) ||
        (prior.rollbackAuthorized && !next.rollbackAuthorized) ||
        (prior.brokerEntered && !next.brokerEntered) ||
        (prior.brokerSettled && !next.brokerSettled) ||
        (prior.packageStagedHere && !next.packageStagedHere) ||
        (prior.bindingMutationStarted && !next.bindingMutationStarted) ||
        (prior.rebootRequired && !next.rebootRequired) ||
        (prior.deadlineOverrun && !next.deadlineOverrun) ||
        (prior.hasPublishedCandidate && !next.hasPublishedCandidate) ||
        (prior.hasPriorAbiProfile && !next.hasPriorAbiProfile) ||
        (prior.hasRootRegistrationIntent &&
            !next.hasRootRegistrationIntent) ||
        (prior.partialRootRemovalBinding !=
                InstallJournalStateData::PartialRootRemovalBinding::None &&
            next.partialRootRemovalBinding ==
                InstallJournalStateData::PartialRootRemovalBinding::None) ||
        (!prior.partialRootRemovalBootIdentifier.empty() &&
            next.partialRootRemovalBootIdentifier.empty()) ||
        (prior.hasBrokerProof && !next.hasBrokerProof)) {
        return SetError(error, L"install-journal-monotonic-state",
            ERROR_INVALID_DATA,
            L"terminal, direction, ownership, or diagnostic state regressed");
    }
    const bool pendingRebootEpochChanged =
        prior.pendingRebootBootIdentifier !=
            next.pendingRebootBootIdentifier;
    const bool authoritativeRebootReturn =
        next.phase == InstallJournalPhase::DiInstallReturned ||
        next.phase == InstallJournalPhase::RollbackBindingReturned ||
        next.phase ==
            InstallJournalPhase::PartialRootRemovalReturned;
    if ((pendingRebootEpochChanged &&
            !next.freshRebootRequired) ||
        (next.freshRebootRequired &&
            (!authoritativeRebootReturn ||
                !next.rebootRequired ||
                !IsCanonicalBootIdentifier(
                    next.pendingRebootBootIdentifier))) ||
        (!next.freshRebootRequired &&
            pendingRebootEpochChanged) ||
        (!next.rebootRequired &&
            !next.pendingRebootBootIdentifier.empty()) ||
        ((next.phase == InstallJournalPhase::ForwardRebootPending ||
             next.phase == InstallJournalPhase::RestoreRebootPending) &&
            next.pendingRebootBootIdentifier.empty())) {
        return SetError(error, L"install-journal-reboot-epoch-chain",
            ERROR_INVALID_DATA,
            L"pending reboot epoch changed outside an authoritative fresh-reboot returned record");
    }
    if (prior.hasPublishedCandidate &&
        !SameJournalPackageIdentity(
            prior.publishedCandidate, next.publishedCandidate)) {
        return SetError(error, L"install-journal-publication-chain",
            ERROR_REVISION_MISMATCH,
            L"published candidate identity changed across records");
    }
    if (!prior.hasPublishedCandidate && next.hasPublishedCandidate &&
        next.phase != InstallJournalPhase::Prepared &&
        next.phase != InstallJournalPhase::StageReceiptCaptured) {
        return SetError(error, L"install-journal-publication-chain",
            ERROR_INVALID_DATA,
            L"candidate publication first appeared outside exact prepublication or stage receipt");
    }
    if (!prior.packageStagedHere && next.packageStagedHere &&
        next.phase != InstallJournalPhase::StageReceiptCaptured) {
        return SetError(error, L"install-journal-stage-ownership-chain",
            ERROR_INVALID_DATA,
            L"transaction-owned stage identity first appeared outside its exact durable receipt");
    }
    if (!SamePackageInventory(
            prior.expectedInventory, next.expectedInventory)) {
        std::vector<PackageInfo> permittedInventory =
            prior.expectedInventory;
        if (!prior.packageStagedHere && next.packageStagedHere &&
            next.hasPublishedCandidate &&
            !ContainsExactPackage(
                permittedInventory, next.publishedCandidate)) {
            permittedInventory.push_back(next.publishedCandidate);
            std::sort(permittedInventory.begin(), permittedInventory.end(),
                [](const PackageInfo& left, const PackageInfo& right) {
                    return _wcsicmp(left.publishedName.c_str(),
                        right.publishedName.c_str()) < 0;
                });
        }
        if (!SamePackageInventory(
                permittedInventory, next.expectedInventory)) {
            return SetError(error, L"install-journal-inventory-chain",
                ERROR_REVISION_MISMATCH,
                L"expected package inventory changed outside exact stage publication");
        }
    }
    if (prior.hasPriorAbiProfile &&
        !SameAbiCompatibilityProfile(
            prior.priorAbiProfile, next.priorAbiProfile)) {
        return SetError(error, L"install-journal-prior-abi-profile-chain",
            ERROR_REVISION_MISMATCH,
            L"durable prior ABI profile changed across records");
    }
    if (prior.hasRootRegistrationIntent &&
        _wcsicmp(prior.rootRegistrationInstanceId.c_str(),
            next.rootRegistrationInstanceId.c_str()) != 0) {
        return SetError(error, L"install-journal-root-intent-chain",
            ERROR_REVISION_MISMATCH,
            L"durable generated root registration identity changed across records");
    }
    const bool rootRemovalBootChanged =
        prior.partialRootRemovalBootIdentifier !=
            next.partialRootRemovalBootIdentifier;
    const bool rootRemovalBindingChanged =
        prior.partialRootRemovalBinding !=
            next.partialRootRemovalBinding;
    const bool hasRootRemovalBinding =
        next.partialRootRemovalBinding !=
            InstallJournalStateData::PartialRootRemovalBinding::None;
    if (((rootRemovalBootChanged || rootRemovalBindingChanged) &&
            next.phase !=
                InstallJournalPhase::PartialRootRemovalEntered) ||
        (next.partialRootRemovalBootIdentifier.empty() !=
            !hasRootRemovalBinding) ||
        (!next.partialRootRemovalBootIdentifier.empty() &&
            (!next.hasRootRegistrationIntent ||
                !next.prior.devices.empty() ||
                next.direction != InstallJournalDirection::Rollback ||
                !next.rollbackAuthorized)) ||
        ((next.phase ==
                InstallJournalPhase::PartialRootRemovalEntered ||
             next.phase ==
                InstallJournalPhase::PartialRootRemovalReturned ||
             next.phase == InstallJournalPhase::
                PartialRootRemovalRebootPending) &&
            (next.partialRootRemovalBootIdentifier.empty() ||
                !hasRootRemovalBinding))) {
        return SetError(error,
            L"install-journal-partial-root-removal-chain",
            ERROR_INVALID_DATA,
            L"partial root removal boot authority changed outside its exact rollback entry record");
    }
    if (prior.hasBrokerProof && !SameDurableBrokerProof(prior, next)) {
        return SetError(error, L"install-journal-broker-proof-chain",
            ERROR_REVISION_MISMATCH,
            L"settled broker proof changed across records");
    }
    if (!prior.hasPriorAbiProfile && next.hasPriorAbiProfile &&
        next.phase != InstallJournalPhase::PriorAbiProfileCaptured) {
        return SetError(error, L"install-journal-prior-abi-profile-chain",
            ERROR_INVALID_DATA,
            L"durable prior ABI profile first appeared outside its capture phase");
    }
    if (!prior.hasRootRegistrationIntent &&
        next.hasRootRegistrationIntent &&
        (next.phase !=
                InstallJournalPhase::RootRegistrationIntentCaptured ||
            next.direction != InstallJournalDirection::Forward ||
            !next.prior.devices.empty() ||
            !next.hasPublishedCandidate ||
            !IsGeneratedRootInstanceIdForDeviceName(
                next.rootRegistrationInstanceId, kRootDeviceName))) {
        return SetError(error, L"install-journal-root-intent-chain",
            ERROR_INVALID_DATA,
            L"root registration identity first appeared outside exact forward pre-registration admission");
    }
    if (!prior.hasBrokerProof && next.hasBrokerProof &&
        next.phase != InstallJournalPhase::BrokerChildSettled) {
        return SetError(error, L"install-journal-broker-proof-chain",
            ERROR_INVALID_DATA,
            L"durable broker proof first appeared outside child settlement");
    }
    if (!prior.brokerEntered && next.brokerEntered &&
        next.phase != InstallJournalPhase::BrokerHandoffEntered) {
        return SetError(error, L"install-journal-broker-chain",
            ERROR_INVALID_DATA,
            L"broker ownership first appeared outside handoff admission");
    }
    if (!prior.brokerSettled && next.brokerSettled &&
        next.phase != InstallJournalPhase::BrokerChildSettled) {
        return SetError(error, L"install-journal-broker-chain",
            ERROR_INVALID_DATA,
            L"broker settlement first appeared outside child settlement");
    }
    if (!prior.rollbackAuthorized && next.rollbackAuthorized &&
        (next.direction != InstallJournalDirection::Rollback ||
            (next.phase != InstallJournalPhase::BrokerHandoffReturned &&
                next.phase != InstallJournalPhase::BrokerChildSettled &&
                next.phase != InstallJournalPhase::RollbackBindingEntered))) {
        return SetError(error, L"install-journal-rollback-authorization-chain",
            ERROR_INVALID_DATA,
            L"rollback authority first appeared outside an authoritative admission record");
    }
    if ((next.phase == InstallJournalPhase::ExactPriorRestored ||
            next.phase == InstallJournalPhase::RestoreRebootPending) &&
        next.brokerEntered &&
        (next.direction != InstallJournalDirection::Rollback ||
            !next.rollbackAuthorized)) {
        return SetError(error, L"install-journal-terminal-authority",
            ERROR_INVALID_DATA,
            L"prior terminal phase lacks durable broker-safe rollback authority");
    }
    if ((next.phase == InstallJournalPhase::ForwardValidated ||
            next.phase == InstallJournalPhase::ForwardRebootPending) &&
        next.brokerRequired &&
        (!next.brokerEntered || !next.brokerSettled ||
            !next.hasBrokerProof || !next.brokerProofSuccess ||
            next.brokerDriverRollbackAuthorized ||
            next.direction != InstallJournalDirection::Forward ||
            next.rollbackAuthorized)) {
        return SetError(error, L"install-journal-terminal-authority",
            ERROR_INVALID_DATA,
            L"forward terminal phase lacks exact canonical broker commit authority");
    }
    if (prior.direction == InstallJournalDirection::Forward &&
        next.direction == InstallJournalDirection::Rollback) {
        if (!next.rollbackAuthorized ||
            (next.phase != InstallJournalPhase::BrokerHandoffReturned &&
                next.phase != InstallJournalPhase::BrokerChildSettled &&
                next.phase != InstallJournalPhase::RollbackBindingEntered)) {
            return SetError(error, L"install-journal-direction-chain",
                ERROR_INVALID_DATA,
                L"forward ownership changed to rollback without a legal durable admission");
        }
        return true;
    }
    if (next.direction == InstallJournalDirection::Rollback) {
        if (!LegalRollbackInstallJournalPhaseTransition(
                prior.phase, next.phase)) {
            return SetError(error, L"install-journal-phase-chain",
                ERROR_INVALID_DATA,
                L"rollback journal phase transition is not legal");
        }
        return true;
    }
    if (next.phase == InstallJournalPhase::RollbackBindingEntered ||
        next.phase == InstallJournalPhase::RollbackBindingReturned ||
        next.phase == InstallJournalPhase::PartialRootRemovalEntered ||
        next.phase == InstallJournalPhase::PartialRootRemovalReturned ||
        next.phase == InstallJournalPhase::
            PartialRootRemovalRebootPending ||
        next.phase == InstallJournalPhase::SetupUninstallEntered ||
        next.phase == InstallJournalPhase::SetupUninstallReturned) {
        return SetError(error, L"install-journal-phase-chain",
            ERROR_INVALID_DATA,
            L"rollback-only phase was published in forward direction");
    }
    if (next.phase == InstallJournalPhase::ManualReconciliationRequired ||
        next.phase == InstallJournalPhase::ExactPriorRestored ||
        next.phase == InstallJournalPhase::RestoreRebootPending) {
        return true;
    }
    if (!LegalForwardInstallJournalPhaseTransition(
            prior.phase, next.phase)) {
        return SetError(error, L"install-journal-phase-chain",
            ERROR_INVALID_DATA,
            L"forward journal phase transition is not legal");
    }
    return true;
}

struct LoadedInstallJournal {
    InstallRecoveryDirectory directory;
    InstallJournalStateData state;
    bool hasRecord = false;
    bool forwardRootRegistrationEntered = false;
    bool forwardDiInstallEntered = false;
    bool partialRootRemovalEntered = false;
    std::vector<WinHandle> evidenceLocks;
};

bool ParseInstallJournalEnvelope(
    std::string_view record,
    const std::filesystem::path& active,
    InstallJournalStateData* state,
    std::string* digest,
    Error* error) {
    JsonValue root;
    std::string parseMessage;
    if (!JsonParser(record).Parse(&root, &parseMessage)) {
        std::wstring message;
        Utf8ToWide(parseMessage, &message, nullptr);
        return SetError(error, L"install-journal-chain",
            ERROR_INVALID_DATA,
            L"journal envelope is truncated or malformed: " + message);
    }
    const JsonValue::Object* object = nullptr;
    uint64_t schema = 0;
    std::string kind;
    std::string payloadDigest;
    std::string payload;
    if (!RequireJournalObject(root, &object, error) ||
        object->size() != 4U ||
        !RequireJournalUnsigned(*object, "schema", 2U, &schema, error) ||
        schema != 2U ||
        !RequireJournalString(*object, "kind", &kind, error) ||
        kind != kInstallRecoveryKind ||
        !RequireJournalString(
            *object, "payloadSha256", &payloadDigest, error) ||
        !RequireJournalString(*object, "payload", &payload, error) ||
        !IsSha256Digest(payloadDigest)) {
        if (error->code == ERROR_SUCCESS) {
            SetError(error, L"install-journal-chain", ERROR_INVALID_DATA,
                L"journal envelope is not the exact v2 contract");
        }
        return false;
    }
    std::string canonicalEnvelope = "{\"schema\":2,\"kind\":";
    AppendJsonAsciiString(&canonicalEnvelope, kInstallRecoveryKind);
    canonicalEnvelope.append(",\"payloadSha256\":");
    AppendJsonAsciiString(&canonicalEnvelope, LowerAscii(payloadDigest));
    canonicalEnvelope.append(",\"payload\":");
    AppendJsonUtf8String(&canonicalEnvelope, payload);
    canonicalEnvelope.append("}\n");
    if (record != canonicalEnvelope) {
        return SetError(error, L"install-journal-canonical-envelope",
            ERROR_INVALID_DATA,
            L"journal envelope is not in exact canonical byte form");
    }
    std::string observedDigest;
    if (!Sha256Data(payload, &observedDigest, error) ||
        _stricmp(observedDigest.c_str(), payloadDigest.c_str()) != 0) {
        if (error->code == ERROR_SUCCESS) {
            SetError(error, L"install-journal-chain", ERROR_CRC,
                L"journal payload hash does not match its published receipt");
        }
        return false;
    }
    if (!ParseInstallJournalPayload(payload, active, state, error)) {
        return false;
    }
    *digest = LowerAscii(std::move(payloadDigest));
    return true;
}

bool ParseJournalRecordFileName(
    std::wstring_view name,
    uint64_t* sequence) noexcept {
    const std::wstring_view prefix(kInstallRecoveryJournalPrefix);
    const std::wstring_view suffix(kInstallRecoveryJournalSuffix);
    if (!name.starts_with(prefix) || !name.ends_with(suffix) ||
        name.size() != prefix.size() + 8U + suffix.size()) {
        return false;
    }
    uint64_t parsed = 0;
    for (size_t index = prefix.size(); index < prefix.size() + 8U; ++index) {
        if (name[index] < L'0' || name[index] > L'9') {
            return false;
        }
        parsed = parsed * 10U + static_cast<uint64_t>(name[index] - L'0');
    }
    *sequence = parsed;
    return true;
}

bool ParseJournalTemporaryFileName(
    std::wstring_view name,
    uint64_t* sequence) noexcept {
    const std::wstring_view temporarySuffix(
        kInstallRecoveryTemporarySuffix);
    return name.ends_with(temporarySuffix) &&
        ParseJournalRecordFileName(
            name.substr(0, name.size() - temporarySuffix.size()),
            sequence);
}

bool InstallJournalTemporarySequenceIsRecoverable(
    uint64_t temporarySequence,
    size_t publishedRecordCount) noexcept {
    return publishedRecordCount < kMaximumInstallRecoveryRecords &&
        temporarySequence == publishedRecordCount;
}

bool ValidateLoadedInstallJournalEvidence(
    LoadedInstallJournal* loaded,
    Error* error) {
    WinHandle priorHandle;
    WinHandle candidateHandle;
    if (!OpenStableDirectory(
            loaded->directory.active / kInstallRecoveryPriorDirectory,
            true, &priorHandle, error) ||
        !OpenStableDirectory(
            loaded->directory.active / kInstallRecoveryCandidateDirectory,
            true, &candidateHandle, error)) {
        return false;
    }
    loaded->evidenceLocks.push_back(std::move(priorHandle));
    loaded->evidenceLocks.push_back(std::move(candidateHandle));

    const std::filesystem::path candidateDirectory =
        loaded->directory.active / kInstallRecoveryCandidateDirectory;
    PackageInfo candidateCopy;
    bool owned = false;
    if (!ValidateExactPackageDirectory(candidateDirectory, error) ||
        !LoadOwnedPackage(candidateDirectory / L"ViiperUde.inf", true,
            loaded->state.localTest, &candidateCopy, &owned, error) ||
        !owned || !(candidateCopy.version == loaded->state.candidate.version) ||
        !SamePackageBytes(candidateCopy, loaded->state.candidate)) {
        if (error->code == ERROR_SUCCESS) {
            SetError(error, L"install-journal-candidate-evidence",
                ERROR_REVISION_MISMATCH);
        }
        return false;
    }
    std::vector<WinHandle> candidateLocks;
    if (!LockPackageFiles(candidateDirectory, &candidateLocks, error)) {
        return false;
    }
    for (WinHandle& lock : candidateLocks) {
        loaded->evidenceLocks.push_back(std::move(lock));
    }
    for (size_t index = 0; index < loaded->state.prior.packages.size(); ++index) {
        PackageInfo& prior = loaded->state.prior.packages[index];
        const std::filesystem::path directory =
            loaded->directory.active / kInstallRecoveryPriorDirectory /
            std::to_wstring(index);
        WinHandle directoryHandle;
        if (!OpenStableDirectory(directory, true, &directoryHandle, error) ||
            !ValidateExactPackageDirectory(directory, error)) {
            return false;
        }
        loaded->evidenceLocks.push_back(std::move(directoryHandle));
        PackageInfo copy;
        owned = false;
        if (!LoadOwnedPackage(directory / L"ViiperUde.inf", true, false,
                &copy, &owned, error) || !owned ||
            !(copy.version == prior.version) ||
            !SamePackageBytes(copy, prior)) {
            if (error->code == ERROR_SUCCESS) {
                SetError(error, L"install-journal-prior-evidence",
                    ERROR_REVISION_MISMATCH);
            }
            return false;
        }
        std::vector<WinHandle> locks;
        if (!LockPackageFiles(directory, &locks, error)) {
            return false;
        }
        for (WinHandle& lock : locks) {
            loaded->evidenceLocks.push_back(std::move(lock));
        }
    }
    std::filesystem::path systemInf;
    if (!GetSystemInfDirectory(&systemInf, error)) {
        return false;
    }
    for (PackageInfo& package : loaded->state.prior.packages) {
        package.infPath = systemInf / package.publishedName;
    }
    for (DeviceState& device : loaded->state.prior.devices) {
        for (const PackageInfo& package : loaded->state.prior.packages) {
            if (_wcsicmp(package.publishedName.c_str(),
                    device.publishedInf.c_str()) == 0) {
                device.package = package;
            }
        }
    }
    if (loaded->state.hasPublishedCandidate) {
        loaded->state.publishedCandidate.infPath =
            systemInf / loaded->state.publishedCandidate.publishedName;
    }
    return true;
}

bool LoadInstallJournal(
    InstallRecoveryDirectory&& directory,
    LoadedInstallJournal* loaded,
    Error* error) {
    loaded->directory = std::move(directory);
    std::map<uint64_t, std::filesystem::path> records;
    std::optional<std::pair<uint64_t, std::filesystem::path>> temporary;
    std::error_code enumerationError;
    for (std::filesystem::directory_iterator iterator(
             loaded->directory.active, enumerationError), end;
         !enumerationError && iterator != end;
         iterator.increment(enumerationError)) {
        const std::wstring name = iterator->path().filename().wstring();
        const DWORD attributes = GetFileAttributesW(iterator->path().c_str());
        if (attributes == INVALID_FILE_ATTRIBUTES ||
            (attributes & FILE_ATTRIBUTE_REPARSE_POINT) != 0) {
            return SetError(error, L"install-journal-discovery",
                ERROR_REPARSE_TAG_MISMATCH);
        }
        if ((attributes & FILE_ATTRIBUTE_DIRECTORY) != 0 &&
            (name == kInstallRecoveryPriorDirectory ||
                name == kInstallRecoveryCandidateDirectory)) {
            continue;
        }
        uint64_t sequence = 0;
        if ((attributes & FILE_ATTRIBUTE_DIRECTORY) == 0 &&
            ParseJournalRecordFileName(name, &sequence)) {
            if (sequence >= kMaximumInstallRecoveryRecords ||
                !records.emplace(sequence, iterator->path()).second) {
                return SetError(error, L"install-journal-chain",
                    ERROR_DUPLICATE_SERVICE_NAME);
            }
            continue;
        }
        if ((attributes & FILE_ATTRIBUTE_DIRECTORY) == 0 &&
            ParseJournalTemporaryFileName(name, &sequence)) {
            if (sequence >= kMaximumInstallRecoveryRecords || temporary) {
                return SetError(error, L"install-journal-temp-chain",
                    ERROR_INVALID_DATA,
                    L"transaction contains more than one canonical unpublished temp or an out-of-range temp");
            }
            temporary.emplace(sequence, iterator->path());
            continue;
        }
        return SetError(error, L"install-journal-discovery",
            ERROR_INVALID_DATA,
            L"protected transaction directory contains an unexpected entry");
    }
    if (enumerationError) {
        return SetError(error, L"install-journal-discovery",
            static_cast<DWORD>(enumerationError.value()));
    }
    if ((!records.empty() &&
            (records.begin()->first != 0U ||
                records.rbegin()->first + 1U != records.size())) ||
        (temporary && !InstallJournalTemporarySequenceIsRecoverable(
            temporary->first, records.size()))) {
        return SetError(error, L"install-journal-chain", ERROR_INVALID_DATA,
            L"journal sequence or unpublished temp is missing, stale, or out of order");
    }
    if (temporary &&
        !ValidateAndDiscardInstallJournalTemporaryFile(
            temporary->second, error)) {
        return false;
    }
    if (records.empty()) {
        loaded->hasRecord = false;
        return true;
    }
    std::string priorDigest(kZeroSha256);
    std::optional<InstallJournalStateData> immutable;
    std::optional<InstallJournalStateData> previousState;
    std::optional<AbiCompatibilityProfile> capturedPriorAbiProfile;
    for (const auto& [expectedSequence, path] : records) {
        std::string record;
        InstallJournalStateData parsed;
        std::string digest;
        if (!ReadInstallJournalFile(path, &record, error) ||
            !ParseInstallJournalEnvelope(
                record, loaded->directory.active, &parsed, &digest, error) ||
            parsed.sequence != expectedSequence ||
            _stricmp(parsed.previousDigest.c_str(), priorDigest.c_str()) != 0 ||
            (immutable && !SameInstallJournalImmutableState(
                *immutable, parsed)) ||
            !ValidateInstallJournalTransition(
                previousState ? &*previousState : nullptr,
                parsed, error)) {
            if (error->code == ERROR_SUCCESS) {
                SetError(error, L"install-journal-chain", ERROR_CRC,
                    L"journal hash chain or immutable transaction identity changed");
            }
            return false;
        }
        if (parsed.hasPriorAbiProfile) {
            if (!capturedPriorAbiProfile &&
                parsed.phase != InstallJournalPhase::PriorAbiProfileCaptured) {
                return SetError(error,
                    L"install-journal-prior-abi-profile-chain",
                    ERROR_INVALID_DATA,
                    L"durable prior ABI profile first appeared outside its capture phase");
            }
            if (capturedPriorAbiProfile &&
                !SameAbiCompatibilityProfile(
                    *capturedPriorAbiProfile, parsed.priorAbiProfile)) {
                return SetError(error,
                    L"install-journal-prior-abi-profile-chain",
                    ERROR_REVISION_MISMATCH,
                    L"durable prior ABI profile changed across phase records");
            }
            capturedPriorAbiProfile = parsed.priorAbiProfile;
        } else if (capturedPriorAbiProfile) {
            return SetError(error,
                L"install-journal-prior-abi-profile-chain",
                ERROR_INVALID_DATA,
                L"durable prior ABI profile disappeared from a later phase record");
        }
        if (parsed.direction == InstallJournalDirection::Forward) {
            loaded->forwardRootRegistrationEntered =
                loaded->forwardRootRegistrationEntered ||
                parsed.phase == InstallJournalPhase::RootRegistrationEntered;
            loaded->forwardDiInstallEntered =
                loaded->forwardDiInstallEntered ||
                parsed.phase == InstallJournalPhase::DiInstallEntered;
        }
        loaded->partialRootRemovalEntered =
            loaded->partialRootRemovalEntered ||
            parsed.phase ==
                InstallJournalPhase::PartialRootRemovalEntered;
        if (!immutable) {
            immutable = parsed;
        }
        previousState = parsed;
        priorDigest = digest;
        loaded->state = std::move(parsed);
        loaded->state.lastDigest = digest;
    }
    loaded->state.previousDigest = priorDigest;
    loaded->state.sequence = records.size();
    loaded->hasRecord = true;
    return ValidateLoadedInstallJournalEvidence(loaded, error);
}

bool RetireLoadedInstallJournal(
    LoadedInstallJournal* loaded,
    Error* error) {
    loaded->evidenceLocks.clear();
    return RetireInstallRecoveryActiveDirectory(
        &loaded->directory, loaded->state.transactionId, error);
}

bool CurrentStateMatchesPrior(
    const InstallJournalStateData& state,
    uint64_t deadlineUnixMs,
    Error* error) {
    Snapshot observed;
    if (!CaptureSnapshot(&observed, error)) {
        return false;
    }
    if (!SameCapturedRootState(state.prior, observed) ||
        !SamePackageInventory(state.prior.packages, observed.packages)) {
        return SetError(error, L"install-journal-prior-state",
            ERROR_REVISION_MISMATCH);
    }
    if (state.bindingMutationStarted && !state.prior.devices.empty() &&
        state.prior.devices[0].started) {
        if (!state.hasPriorAbiProfile) {
            return SetError(error, L"install-journal-prior-abi-profile",
                ERROR_REVISION_MISMATCH,
                L"started prior root lacks its durable exact ABI profile");
        }
        return VerifyAbiHealth(deadlineUnixMs, nullptr, error,
            AbiHealthPurpose::RollbackHealth,
            &state.priorAbiProfile, nullptr);
    }
    return true;
}

bool RootSnapshotIsAuthorizedForInstallRollback(
    const InstallJournalStateData& state,
    const Snapshot& observed) noexcept {
    if (state.prior.devices.empty()) {
        if (observed.devices.empty()) return true;
        if (observed.devices.size() != 1U ||
            !state.bindingMutationStarted ||
            !state.hasPublishedCandidate) {
            return false;
        }
        const DeviceState& current = observed.devices[0];
        if (!IsOwnedGeneratedRootInstanceId(current.instanceId) ||
            !current.present ||
            _wcsicmp(current.service.c_str(), kServiceName) != 0 ||
            _wcsicmp(current.publishedInf.c_str(),
                state.publishedCandidate.publishedName.c_str()) != 0 ||
            !(current.version == state.candidate.version) ||
            !SamePackageBytes(current.package, state.candidate)) {
            return false;
        }
        return true;
    }
    if (observed.devices.size() != 1U) {
        return false;
    }
    const DeviceState& prior = state.prior.devices[0];
    const DeviceState& current = observed.devices[0];
    if (_wcsicmp(prior.instanceId.c_str(), current.instanceId.c_str()) != 0 ||
        !current.present ||
        _wcsicmp(current.service.c_str(), kServiceName) != 0) {
        return false;
    }
    if (SameRootBinding(prior, current)) return true;
    if (state.hasPublishedCandidate &&
        _wcsicmp(current.publishedInf.c_str(),
            state.publishedCandidate.publishedName.c_str()) == 0 &&
        current.version == state.candidate.version &&
        SamePackageBytes(current.package, state.candidate)) {
        return true;
    }
    return false;
}

enum class PartialInstallRootRecoveryAction {
    PriorEmpty,
    RemoveUnboundExactRoot,
    RemoveCandidateBoundExactRoot,
    PendingExactRootRemoval,
    Manual,
};

struct PartialInstallRootRecoveryFacts {
    bool priorEmpty = false;
    bool bindingMutationStarted = false;
    bool forwardRootRegistrationEntered = false;
    bool forwardDiInstallEntered = false;
    bool partialRootRemovalEntered = false;
    size_t relatedRootCount = 0;
    bool hardwareIdAbsent = false;
    bool exactHardwareId = false;
    bool exactClass = false;
    bool exactGeneratedInstance = false;
    bool present = false;
    bool serviceEmpty = false;
    bool publishedInfEmpty = false;
    bool driverVersionEmpty = false;
    bool exactCandidateService = false;
    bool exactCandidateInf = false;
    bool exactCandidateVersion = false;
    bool exactCandidateBytes = false;
    bool pendingRemovalLifecycle = false;
};

PartialInstallRootRecoveryAction ClassifyPartialInstallRootRecovery(
    const PartialInstallRootRecoveryFacts& facts) noexcept {
    if (!facts.priorEmpty) {
        return PartialInstallRootRecoveryAction::Manual;
    }
    if (facts.relatedRootCount == 0U) {
        return PartialInstallRootRecoveryAction::PriorEmpty;
    }
    if (facts.relatedRootCount != 1U ||
        !facts.bindingMutationStarted ||
        !facts.forwardRootRegistrationEntered ||
        !facts.exactClass || !facts.exactGeneratedInstance) {
        return PartialInstallRootRecoveryAction::Manual;
    }
    const bool emptyBinding = facts.serviceEmpty &&
        facts.publishedInfEmpty && facts.driverVersionEmpty;
    const bool exactCandidateBinding =
        facts.forwardDiInstallEntered &&
        facts.exactCandidateService && facts.exactCandidateInf &&
        facts.exactCandidateVersion && facts.exactCandidateBytes;
    const bool hasCandidateBindingFragment =
        facts.exactCandidateService || facts.exactCandidateInf ||
        facts.exactCandidateVersion;
    const bool canonicalPendingBinding =
        (facts.serviceEmpty || facts.exactCandidateService) &&
        (facts.publishedInfEmpty || facts.exactCandidateInf) &&
        (facts.driverVersionEmpty || facts.exactCandidateVersion) &&
        (facts.publishedInfEmpty || facts.exactCandidateBytes) &&
        (!hasCandidateBindingFragment || facts.forwardDiInstallEntered);
    if (facts.partialRootRemovalEntered &&
        (facts.hardwareIdAbsent || facts.exactHardwareId) &&
        facts.pendingRemovalLifecycle &&
        canonicalPendingBinding) {
        return PartialInstallRootRecoveryAction::PendingExactRootRemoval;
    }
    if (!facts.exactHardwareId || !facts.present) {
        return PartialInstallRootRecoveryAction::Manual;
    }
    if (emptyBinding) {
        return PartialInstallRootRecoveryAction::RemoveUnboundExactRoot;
    }
    if (exactCandidateBinding) {
        return PartialInstallRootRecoveryAction::RemoveCandidateBoundExactRoot;
    }
    return PartialInstallRootRecoveryAction::Manual;
}

bool InstallJournalRecoveryUsesStrictBindingRestore(
    const InstallJournalStateData& state) noexcept {
    return !state.prior.devices.empty() &&
        state.bindingMutationStarted;
}

bool IsInGeneratedRootDeviceNamespace(
    const std::wstring& instanceId,
    const wchar_t* deviceName) {
    const std::wstring prefix = std::wstring(L"ROOT\\") + deviceName + L"\\";
    return instanceId.size() >= prefix.size() &&
        _wcsnicmp(instanceId.c_str(), prefix.c_str(), prefix.size()) == 0;
}

bool ReadInstallRecoveryRootInstanceId(
    HDEVINFO set,
    SP_DEVINFO_DATA& data,
    std::wstring* instanceId,
    Error* error) {
    DWORD required = 0;
    SetupDiGetDeviceInstanceIdW(set, &data, nullptr, 0, &required);
    if (required == 0 || GetLastError() != ERROR_INSUFFICIENT_BUFFER) {
        return SetLastErrorDetail(
            error, L"install-journal-raw-root-instance-id");
    }
    std::vector<wchar_t> value(required);
    if (!SetupDiGetDeviceInstanceIdW(
            set, &data, value.data(), required, nullptr)) {
        return SetLastErrorDetail(
            error, L"install-journal-raw-root-instance-id");
    }
    *instanceId = value.data();
    return true;
}

struct InstallRecoveryHardwareIdObservation {
    bool absent = false;
    bool containsExpected = false;
    bool exact = false;
};

bool ClassifyCanonicalInstallRecoveryHardwareIds(
    const std::vector<wchar_t>& value,
    InstallRecoveryHardwareIdObservation* observation) noexcept {
    *observation = InstallRecoveryHardwareIdObservation{};
    if (value.size() < 3U || value.back() != L'\0' ||
        value[value.size() - 2U] != L'\0' || value.front() == L'\0') {
        return false;
    }
    size_t cursor = 0;
    size_t entries = 0;
    while (cursor < value.size() - 1U) {
        const auto terminator = std::find(
            value.begin() + static_cast<std::ptrdiff_t>(cursor),
            value.end() - 1, L'\0');
        if (terminator == value.end() - 1 ||
            terminator == value.begin() +
                static_cast<std::ptrdiff_t>(cursor)) {
            return false;
        }
        const size_t length = static_cast<size_t>(
            terminator - (value.begin() +
                static_cast<std::ptrdiff_t>(cursor)));
        const size_t expectedLength = wcslen(kHardwareId);
        if (length == expectedLength &&
            _wcsnicmp(value.data() + cursor,
                kHardwareId, expectedLength) == 0) {
            observation->containsExpected = true;
        }
        ++entries;
        cursor += length + 1U;
    }
    if (cursor != value.size() - 1U) {
        return false;
    }
    observation->exact = entries == 1U &&
        observation->containsExpected;
    return true;
}

bool ReadInstallRecoveryHardwareIds(
    HDEVINFO set,
    SP_DEVINFO_DATA& data,
    InstallRecoveryHardwareIdObservation* observation,
    Error* error) {
    *observation = InstallRecoveryHardwareIdObservation{};
    DWORD type = 0;
    DWORD required = 0;
    if (SetupDiGetDeviceRegistryPropertyW(
            set, &data, SPDRP_HARDWAREID, &type, nullptr, 0, &required)) {
        return SetError(error, L"install-journal-raw-root-hardware-id",
            ERROR_INVALID_DATA,
            L"root hardware ID query returned an invalid zero-length value");
    }
    const DWORD queryError = GetLastError();
    if (queryError == ERROR_INVALID_DATA) {
        observation->absent = true;
        return true;
    }
    if (queryError != ERROR_INSUFFICIENT_BUFFER || type != REG_MULTI_SZ ||
        required < 3U * sizeof(wchar_t) ||
        required % sizeof(wchar_t) != 0U ||
        required > 64U * 1024U) {
        return SetError(error, L"install-journal-raw-root-hardware-id",
            queryError == ERROR_SUCCESS ? ERROR_INVALID_DATA : queryError,
            L"root hardware ID is unreadable or not an exact MULTI_SZ value");
    }
    std::vector<wchar_t> value(required / sizeof(wchar_t));
    DWORD returned = 0;
    DWORD returnedType = 0;
    if (!SetupDiGetDeviceRegistryPropertyW(
            set, &data, SPDRP_HARDWAREID, &returnedType,
            reinterpret_cast<PBYTE>(value.data()), required, &returned)) {
        return SetLastErrorDetail(
            error, L"install-journal-raw-root-hardware-id");
    }
    if (returnedType != REG_MULTI_SZ || returned != required ||
        !ClassifyCanonicalInstallRecoveryHardwareIds(
            value, observation)) {
        return SetError(error, L"install-journal-raw-root-hardware-id",
            ERROR_INVALID_DATA,
            L"root hardware ID changed during observation or is not canonical MULTI_SZ data");
    }
    return true;
}

bool DecodeCanonicalInstallRecoveryString(
    const std::vector<wchar_t>& buffer,
    std::wstring* value) noexcept {
    if (buffer.empty() || buffer.back() != L'\0' ||
        std::find(buffer.begin(), buffer.end() - 1, L'\0') !=
            buffer.end() - 1) {
        return false;
    }
    value->assign(buffer.data(), buffer.size() - 1U);
    return true;
}

bool ReadCanonicalInstallRecoveryService(
    HDEVINFO set,
    SP_DEVINFO_DATA& data,
    std::wstring* service,
    Error* error) {
    DWORD type = 0;
    DWORD required = 0;
    if (SetupDiGetDeviceRegistryPropertyW(
            set, &data, SPDRP_SERVICE, &type, nullptr, 0, &required)) {
        return SetError(error, L"install-journal-raw-root-service",
            ERROR_INVALID_DATA);
    }
    const DWORD queryError = GetLastError();
    if (queryError == ERROR_INVALID_DATA) {
        service->clear();
        return true;
    }
    if (queryError != ERROR_INSUFFICIENT_BUFFER || type != REG_SZ ||
        required < sizeof(wchar_t) ||
        required % sizeof(wchar_t) != 0U ||
        required > 64U * 1024U) {
        return SetError(error, L"install-journal-raw-root-service",
            queryError == ERROR_SUCCESS ? ERROR_INVALID_DATA : queryError,
            L"root service is unreadable or not canonical REG_SZ data");
    }
    std::vector<wchar_t> buffer(required / sizeof(wchar_t));
    DWORD returned = 0;
    DWORD returnedType = 0;
    if (!SetupDiGetDeviceRegistryPropertyW(
            set, &data, SPDRP_SERVICE, &returnedType,
            reinterpret_cast<PBYTE>(buffer.data()), required, &returned)) {
        return SetLastErrorDetail(
            error, L"install-journal-raw-root-service");
    }
    if (returnedType != REG_SZ || returned != required ||
        !DecodeCanonicalInstallRecoveryString(buffer, service)) {
        return SetError(error, L"install-journal-raw-root-service",
            ERROR_INVALID_DATA,
            L"root service changed during observation or contains hidden string data");
    }
    return true;
}

bool ReadCanonicalInstallRecoveryDevicePropertyString(
    HDEVINFO set,
    SP_DEVINFO_DATA& data,
    const DEVPROPKEY& key,
    const wchar_t* phase,
    std::wstring* value,
    Error* error) {
    DEVPROPTYPE type = 0;
    DWORD required = 0;
    if (SetupDiGetDevicePropertyW(
            set, &data, &key, &type, nullptr, 0, &required, 0)) {
        return SetError(error, phase, ERROR_INVALID_DATA);
    }
    const DWORD queryError = GetLastError();
    if (queryError == ERROR_NOT_FOUND) {
        value->clear();
        return true;
    }
    if (queryError != ERROR_INSUFFICIENT_BUFFER ||
        type != DEVPROP_TYPE_STRING || required < sizeof(wchar_t) ||
        required % sizeof(wchar_t) != 0U ||
        required > 64U * 1024U) {
        return SetError(error, phase,
            queryError == ERROR_SUCCESS ? ERROR_INVALID_DATA : queryError,
            L"root device property is unreadable or not canonical string data");
    }
    std::vector<wchar_t> buffer(required / sizeof(wchar_t));
    DWORD returned = 0;
    DEVPROPTYPE returnedType = 0;
    if (!SetupDiGetDevicePropertyW(
            set, &data, &key, &returnedType,
            reinterpret_cast<PBYTE>(buffer.data()), required,
            &returned, 0)) {
        return SetLastErrorDetail(error, phase);
    }
    if (returnedType != DEVPROP_TYPE_STRING || returned != required ||
        !DecodeCanonicalInstallRecoveryString(buffer, value)) {
        return SetError(error, phase, ERROR_INVALID_DATA,
            L"root device property changed during observation or contains hidden string data");
    }
    return true;
}

struct InstallRecoveryRootObservation {
    PartialInstallRootRecoveryAction action =
        PartialInstallRootRecoveryAction::Manual;
    DeviceInfoSet set{INVALID_HANDLE_VALUE};
    SP_DEVINFO_DATA data{};
};

bool ObservePriorEmptyInstallRecoveryRoot(
    const LoadedInstallJournal& loaded,
    InstallRecoveryRootObservation* observation,
    Error* error) {
    *observation = InstallRecoveryRootObservation{};
    DeviceInfoSet set = OpenRootDevices();
    if (!set) {
        return SetLastErrorDetail(error, L"install-journal-raw-root-open");
    }
    struct RelatedRoot {
        SP_DEVINFO_DATA data{};
        std::wstring instanceId;
        bool hardwareIdAbsent = false;
        bool exactHardwareId = false;
    };
    std::vector<RelatedRoot> related;
    for (DWORD index = 0;; ++index) {
        SP_DEVINFO_DATA data{};
        data.cbSize = sizeof(data);
        if (!SetupDiEnumDeviceInfo(set.get(), index, &data)) {
            if (GetLastError() != ERROR_NO_MORE_ITEMS) {
                return SetLastErrorDetail(
                    error, L"install-journal-raw-root-enumeration");
            }
            break;
        }
        std::wstring instanceId;
        if (!ReadInstallRecoveryRootInstanceId(
                set.get(), data, &instanceId, error)) {
            return false;
        }
        InstallRecoveryHardwareIdObservation hardwareIds;
        if (!ReadInstallRecoveryHardwareIds(
                set.get(), data, &hardwareIds, error)) {
            return false;
        }
        const bool inTransactionNamespace =
            IsInGeneratedRootDeviceNamespace(instanceId, kRootDeviceName);
        if (!hardwareIds.containsExpected && !inTransactionNamespace) {
            continue;
        }
        related.push_back(RelatedRoot{
            data, std::move(instanceId), hardwareIds.absent,
            hardwareIds.exact});
    }

    PartialInstallRootRecoveryFacts facts;
    facts.priorEmpty = loaded.state.prior.devices.empty();
    facts.bindingMutationStarted = loaded.state.bindingMutationStarted;
    facts.forwardRootRegistrationEntered =
        loaded.forwardRootRegistrationEntered;
    facts.forwardDiInstallEntered = loaded.forwardDiInstallEntered;
    facts.partialRootRemovalEntered =
        loaded.partialRootRemovalEntered;
    facts.relatedRootCount = related.size();
    if (related.size() == 1U) {
        RelatedRoot& root = related[0];
        facts.hardwareIdAbsent = root.hardwareIdAbsent;
        facts.exactHardwareId = root.exactHardwareId;
        if (!ReadDevicePresence(
                set.get(), root.data, &facts.present, error)) {
            return false;
        }
        if (!facts.present) {
            facts.pendingRemovalLifecycle = true;
        } else {
            ULONG status = 0;
            ULONG problem = 0;
            const CONFIGRET configuration = CM_Get_DevNode_Status(
                &status, &problem, root.data.DevInst, 0);
            if (configuration != CR_SUCCESS) {
                return SetError(error,
                    L"install-journal-raw-root-lifecycle",
                    ERROR_INVALID_DATA,
                    L"present receipt-bound root lifecycle could not be observed canonically");
            }
            facts.pendingRemovalLifecycle =
                problem == CM_PROB_WILL_BE_REMOVED;
        }
        facts.exactClass =
            IsEqualGUID(root.data.ClassGuid, GUID_DEVCLASS_USB) != FALSE;
        facts.exactGeneratedInstance =
            loaded.state.hasRootRegistrationIntent &&
            IsGeneratedRootInstanceIdForDeviceName(
                root.instanceId, kRootDeviceName) &&
            _wcsicmp(root.instanceId.c_str(),
                loaded.state.rootRegistrationInstanceId.c_str()) == 0;

        std::wstring service;
        std::wstring publishedInf;
        std::wstring driverVersion;
        if (!ReadCanonicalInstallRecoveryService(
                set.get(), root.data, &service, error) ||
            !ReadCanonicalInstallRecoveryDevicePropertyString(
                set.get(), root.data, DEVPKEY_Device_DriverInfPath,
                L"install-journal-raw-root-driver-inf",
                &publishedInf, error) ||
            !ReadCanonicalInstallRecoveryDevicePropertyString(
                set.get(), root.data, DEVPKEY_Device_DriverVersion,
                L"install-journal-raw-root-driver-version",
                &driverVersion, error)) {
            return false;
        }
        facts.serviceEmpty = service.empty();
        facts.publishedInfEmpty = publishedInf.empty();
        facts.driverVersionEmpty = driverVersion.empty();
        facts.exactCandidateService =
            _wcsicmp(service.c_str(), kServiceName) == 0;
        facts.exactCandidateInf = loaded.state.hasPublishedCandidate &&
            _wcsicmp(publishedInf.c_str(),
                loaded.state.publishedCandidate.publishedName.c_str()) == 0;
        Version observedVersion;
        facts.exactCandidateVersion = !driverVersion.empty() &&
            ParseVersion(driverVersion, &observedVersion) &&
            observedVersion == loaded.state.candidate.version;
        if (facts.exactCandidateInf) {
            PackageInfo verified;
            bool owned = false;
            if (!LoadOwnedPackage(
                    loaded.state.publishedCandidate.infPath,
                    true, false, &verified, &owned, error)) {
                return false;
            }
            verified.publishedName = publishedInf;
            facts.exactCandidateBytes = owned &&
                SameJournalPackageIdentity(
                    verified, loaded.state.publishedCandidate) &&
                SamePackageBytes(verified, loaded.state.candidate);
        }
    }

    observation->action = ClassifyPartialInstallRootRecovery(facts);
    if (observation->action == PartialInstallRootRecoveryAction::Manual) {
        return SetError(error, L"install-journal-raw-root-authority",
            related.size() > 1U
                ? ERROR_DUPLICATE_SERVICE_NAME : ERROR_REVISION_MISMATCH,
            L"prior-empty recovery observed a foreign, ambiguous, or unauthorized partial root topology");
    }
    if (observation->action ==
            PartialInstallRootRecoveryAction::RemoveUnboundExactRoot ||
        observation->action ==
            PartialInstallRootRecoveryAction::RemoveCandidateBoundExactRoot) {
        observation->set = std::move(set);
        observation->data = related[0].data;
    }
    return true;
}

bool VerifyInstallJournalRawPriorTopology(
    const InstallJournalStateData& state,
    Error* error) {
    if (!state.hasRootRegistrationIntent ||
        !state.prior.devices.empty()) {
        return true;
    }
    LoadedInstallJournal active;
    active.state = state;
    active.forwardRootRegistrationEntered = true;
    active.forwardDiInstallEntered = true;
    InstallRecoveryRootObservation observation;
    if (!ObservePriorEmptyInstallRecoveryRoot(
            active, &observation, error)) {
        return false;
    }
    return observation.action ==
            PartialInstallRootRecoveryAction::PriorEmpty ||
        SetError(error, L"install-journal-prior-raw-root",
            ERROR_REVISION_MISMATCH,
            L"prior-empty retirement still has a related root present");
}

bool VerifyInstallJournalRawForwardTopology(
    const InstallJournalStateData& state,
    Error* error) {
    if (!state.hasRootRegistrationIntent ||
        !state.prior.devices.empty()) {
        return true;
    }
    LoadedInstallJournal active;
    active.state = state;
    active.forwardRootRegistrationEntered = true;
    active.forwardDiInstallEntered = true;
    InstallRecoveryRootObservation observation;
    if (!ObservePriorEmptyInstallRecoveryRoot(
            active, &observation, error)) {
        return false;
    }
    return observation.action == PartialInstallRootRecoveryAction::
            RemoveCandidateBoundExactRoot ||
        SetError(error, L"install-journal-forward-raw-root",
            ERROR_REVISION_MISMATCH,
            L"forward retirement lacks exactly one receipt-bound candidate root and no related extras");
}

bool InstallJournal::VerifyPriorTopologyBeforePackageRollback(
    Error* error) const {
    if (!impl_ || !impl_->preparedRecord || impl_->retired ||
        impl_->poisoned ||
        impl_->state.direction != InstallJournalDirection::Rollback ||
        !impl_->state.rollbackAuthorized) {
        return SetError(error,
            L"install-journal-pre-package-root-authority",
            ERROR_INVALID_STATE);
    }
    return VerifyInstallJournalRawPriorTopology(impl_->state, error);
}

bool InstallJournal::RemoveAuthorizedPriorEmptyRootAfterAdmission(
    uint64_t rollbackDeadlineUnixMs,
    bool* rebootRequired,
    bool* rootRemovalRebootPending,
    Error* error) {
    if (rootRemovalRebootPending == nullptr) {
        return SetError(error, L"install-journal-raw-root-cleanup",
            ERROR_INVALID_PARAMETER);
    }
    *rootRemovalRebootPending = false;
    if (!impl_ || !impl_->preparedRecord || impl_->retired ||
        impl_->poisoned) {
        return SetError(error, L"install-journal-raw-root-cleanup",
            ERROR_INVALID_STATE);
    }
    if (!impl_->state.prior.devices.empty() ||
        !impl_->state.hasRootRegistrationIntent) {
        return true;
    }
    const auto observe = [&](bool removalMayHaveRun,
                             InstallRecoveryRootObservation* observed,
                             Error* observationError) {
        LoadedInstallJournal active;
        active.state = impl_->state;
        active.forwardRootRegistrationEntered =
            impl_->forwardRootRegistrationEntered;
        active.forwardDiInstallEntered =
            impl_->forwardDiInstallEntered;
        active.partialRootRemovalEntered =
            removalMayHaveRun && impl_->partialRootRemovalEntered;
        return ObservePriorEmptyInstallRecoveryRoot(
            active, observed, observationError);
    };
    InstallRecoveryRootObservation observation;
    if (!observe(false, &observation, error)) {
        return false;
    }
    if (observation.action ==
            PartialInstallRootRecoveryAction::PriorEmpty) {
        return true;
    }
    if (observation.action !=
            PartialInstallRootRecoveryAction::RemoveUnboundExactRoot &&
        observation.action != PartialInstallRootRecoveryAction::
            RemoveCandidateBoundExactRoot) {
        return SetError(error, L"install-journal-raw-root-cleanup",
            ERROR_REVISION_MISMATCH,
            L"post-admission root topology is outside the exact receipt-bound cleanup authority");
    }
    if (!CheckTransactionDeadline(rollbackDeadlineUnixMs,
            L"install-rollback-deadline-receipt-root", error)) {
        return false;
    }
    InstallJournalStateData entered = impl_->state;
    if (!GetBootIdentifier(
            &entered.partialRootRemovalBootIdentifier, error)) {
        return false;
    }
    entered.partialRootRemovalBinding =
        observation.action == PartialInstallRootRecoveryAction::
                RemoveCandidateBoundExactRoot
            ? InstallJournalStateData::PartialRootRemovalBinding::Candidate
            : InstallJournalStateData::PartialRootRemovalBinding::Unbound;
    const PackageInfo* publishedCandidate =
        impl_->state.hasPublishedCandidate
            ? &impl_->state.publishedCandidate : nullptr;
    if (!RecordNext(std::move(entered),
            InstallJournalPhase::PartialRootRemovalEntered,
            publishedCandidate, impl_->state.packageStagedHere,
            impl_->state.bindingMutationStarted,
            impl_->state.rebootRequired, true, ERROR_SUCCESS,
            false, false, error)) {
        return false;
    }
    if (!VerifyPackageInventory(impl_->state.expectedInventory,
            L"install-partial-root-removal-post-admission-inventory",
            error)) {
        return false;
    }
    InstallRecoveryRootObservation confirmed;
    if (!observe(false, &confirmed, error)) {
        return false;
    }
    if (confirmed.action == PartialInstallRootRecoveryAction::PriorEmpty ||
        confirmed.action ==
            PartialInstallRootRecoveryAction::PendingExactRootRemoval) {
        if (!Record(InstallJournalPhase::PartialRootRemovalRebootPending,
                publishedCandidate, impl_->state.packageStagedHere,
                impl_->state.bindingMutationStarted, true, false,
                ERROR_SUCCESS_REBOOT_REQUIRED, false, error)) {
            return false;
        }
        *rebootRequired = true;
        *rootRemovalRebootPending = true;
        return true;
    }
    if (confirmed.action != observation.action) {
        return SetError(error, L"install-journal-raw-root-cleanup",
            ERROR_REVISION_MISMATCH,
            L"receipt-bound root topology changed after durable removal admission");
    }

    bool freshRemovalReboot = false;
    Error removalError;
    const bool removed = RemoveDevice(
        confirmed.set.get(), confirmed.data, 0,
        L"install-rollback-deadline-receipt-root",
        nullptr, rebootRequired, &removalError,
        &freshRemovalReboot);
    Error returnRecordError;
    if (!RecordAuthoritativeReturn(
            InstallJournalPhase::PartialRootRemovalReturned,
            publishedCandidate, impl_->state.packageStagedHere,
            impl_->state.bindingMutationStarted,
            *rebootRequired, freshRemovalReboot,
            removed, removed ? ERROR_SUCCESS : removalError.code,
            false, &returnRecordError)) {
        *error = std::move(returnRecordError);
        return false;
    }
    if (!removed) {
        *error = std::move(removalError);
        return false;
    }
    InstallRecoveryRootObservation after;
    if (!observe(true, &after, error)) {
        return false;
    }
    if (freshRemovalReboot) {
        if (after.action != PartialInstallRootRecoveryAction::PriorEmpty &&
            after.action != PartialInstallRootRecoveryAction::
                PendingExactRootRemoval) {
            return SetError(error,
                L"install-journal-raw-root-cleanup",
                ERROR_REVISION_MISMATCH,
                L"successful reboot-requiring root removal left a noncanonical topology");
        }
        if (!Record(InstallJournalPhase::PartialRootRemovalRebootPending,
                publishedCandidate, impl_->state.packageStagedHere,
                impl_->state.bindingMutationStarted, true, true,
                ERROR_SUCCESS_REBOOT_REQUIRED, false, error)) {
            return false;
        }
        *rootRemovalRebootPending = true;
        return true;
    }
    if (after.action != PartialInstallRootRecoveryAction::PriorEmpty) {
        return SetError(error, L"install-journal-raw-root-cleanup",
            ERROR_REVISION_MISMATCH,
            L"successful root removal without a restart did not restore exact prior-empty topology");
    }
    return true;
}

bool CurrentRootIsAuthorizedForInstallRollback(
    const LoadedInstallJournal& loaded,
    InstallRecoveryRootObservation* observation,
    Error* error) {
    *observation = InstallRecoveryRootObservation{};
    const InstallJournalStateData& state = loaded.state;
    if (state.prior.devices.empty()) {
        return ObservePriorEmptyInstallRecoveryRoot(
            loaded, observation, error);
    }
    Snapshot observed;
    if (!CaptureSnapshot(&observed, error)) {
        if (error->code == ERROR_SUCCESS) {
            SetError(error, L"install-journal-rollback-root",
                ERROR_INVALID_DATA);
        }
        return false;
    }
    if (!RootSnapshotIsAuthorizedForInstallRollback(state, observed)) {
        return SetError(error, L"install-journal-rollback-root",
            ERROR_REVISION_MISMATCH,
            L"current root is missing, foreign, duplicated, or bound outside the exact prior/candidate transaction authority");
    }
    observation->action = PartialInstallRootRecoveryAction::PriorEmpty;
    return true;
}

bool CurrentStateMatchesForward(
    const InstallJournalStateData& state,
    uint64_t deadlineUnixMs,
    Error* error) {
    if (!state.hasPublishedCandidate) {
        return SetError(error, L"install-journal-forward-state",
            ERROR_INVALID_DATA);
    }
    std::string expectedBuildIdentity;
    return DeriveDriverBuildIdentity(
               state.sourceRevision, &expectedBuildIdentity, error) &&
        VerifyInstalled(state.candidate,
            state.publishedCandidate.publishedName, false,
            deadlineUnixMs, &expectedBuildIdentity, error) &&
        VerifyPackageInventory(state.expectedInventory,
            L"install-journal-forward-inventory", error);
}

bool RecoveryStateMatchesPrior(
    const LoadedInstallJournal& loaded,
    uint64_t deadlineUnixMs,
    Error* error) {
    if (loaded.state.hasRootRegistrationIntent &&
        loaded.state.prior.devices.empty()) {
        InstallRecoveryRootObservation rawRoot;
        if (!ObservePriorEmptyInstallRecoveryRoot(
                loaded, &rawRoot, error)) {
            return false;
        }
        if (rawRoot.action !=
            PartialInstallRootRecoveryAction::PriorEmpty) {
            return SetError(error, L"install-journal-prior-raw-root",
                ERROR_REVISION_MISMATCH,
                L"exact prior-empty restoration still has the transaction root present");
        }
    }
    return CurrentStateMatchesPrior(
        loaded.state, deadlineUnixMs, error);
}

bool RecoveryStateMatchesForward(
    const LoadedInstallJournal& loaded,
    uint64_t deadlineUnixMs,
    Error* error) {
    if (loaded.state.hasRootRegistrationIntent &&
        loaded.state.prior.devices.empty()) {
        InstallRecoveryRootObservation rawRoot;
        if (!ObservePriorEmptyInstallRecoveryRoot(
                loaded, &rawRoot, error)) {
            return false;
        }
        if (rawRoot.action != PartialInstallRootRecoveryAction::
                RemoveCandidateBoundExactRoot) {
            return SetError(error, L"install-journal-forward-raw-root",
                ERROR_REVISION_MISMATCH,
                L"forward validation lacks the exact receipt-bound candidate root topology");
        }
    }
    return CurrentStateMatchesForward(
        loaded.state, deadlineUnixMs, error);
}

void SetInstallJournalRecoveryOutcome(
    LoadedInstallJournal* loaded,
    const wchar_t* phase,
    DWORD code,
    std::wstring message,
    ExitCode exitCode,
    Outcome* outcome) {
    SetError(&outcome->error, phase, code, std::move(message));
    outcome->exitCode = exitCode;
    outcome->rebootRequired = exitCode == ExitCode::RebootRequired;
    outcome->rollback = exitCode == ExitCode::RollbackFailed
        ? L"failed" : L"not-needed";
    outcome->error.recoveryBackup = loaded->directory.active.wstring();
    outcome->error.recoveryBackupRetained = true;
    if (gActiveRecoveryRecord[0] != L'\0') {
        outcome->error.recoveryRecord = gActiveRecoveryRecord.data();
        outcome->error.recoveryRecordWritten = gActiveRecoveryRecordWritten;
    }
}

bool InstallJournalNeedsRestoreRebootPending(
    const InstallJournalStateData& state,
    bool sameBoot) noexcept {
    return sameBoot &&
        state.direction == InstallJournalDirection::Rollback &&
        state.phase == InstallJournalPhase::RollbackBindingReturned &&
        state.callSucceeded && state.rebootRequired;
}

bool InstallJournalRollbackRetryRebootSeed(
    const InstallJournalStateData& state,
    bool sameBoot) noexcept {
    return sameBoot && state.rebootRequired;
}

bool InstallJournalHasAuthoritativeRollbackSettlement(
    const InstallJournalStateData& state) noexcept {
    return state.bindingMutationStarted &&
        state.direction == InstallJournalDirection::Rollback &&
        state.rollbackAuthorized &&
        state.phase == InstallJournalPhase::RollbackBindingReturned &&
        state.callSucceeded;
}

enum class PartialRootRemovalRecoveryDisposition {
    ContinueRollback,
    RetryRemoval,
    RebootPending,
    Manual,
};

PartialRootRemovalRecoveryDisposition
ClassifyPartialRootRemovalJournalRecovery(
    InstallJournalPhase phase,
    bool callSucceeded,
    bool freshRebootRequired,
    bool sameRemovalBoot,
    InstallJournalStateData::PartialRootRemovalBinding recordedBinding,
    PartialInstallRootRecoveryAction rootAction) noexcept {
    const bool absent = rootAction ==
        PartialInstallRootRecoveryAction::PriorEmpty;
    const bool exactOriginal =
        rootAction ==
            PartialInstallRootRecoveryAction::RemoveUnboundExactRoot ||
        rootAction == PartialInstallRootRecoveryAction::
            RemoveCandidateBoundExactRoot;
    const bool exactPending = rootAction ==
        PartialInstallRootRecoveryAction::PendingExactRootRemoval;
    const bool exactOriginalMatchesRecord =
        (recordedBinding ==
                InstallJournalStateData::PartialRootRemovalBinding::Unbound &&
            rootAction == PartialInstallRootRecoveryAction::
                RemoveUnboundExactRoot) ||
        (recordedBinding == InstallJournalStateData::
                PartialRootRemovalBinding::Candidate &&
            rootAction == PartialInstallRootRecoveryAction::
                RemoveCandidateBoundExactRoot);
    if (phase == InstallJournalPhase::PartialRootRemovalEntered) {
        if (sameRemovalBoot) {
            if (exactOriginal && exactOriginalMatchesRecord) {
                return PartialRootRemovalRecoveryDisposition::RetryRemoval;
            }
            if (absent || exactPending) {
                return PartialRootRemovalRecoveryDisposition::RebootPending;
            }
        } else if (absent) {
            return PartialRootRemovalRecoveryDisposition::ContinueRollback;
        }
        return PartialRootRemovalRecoveryDisposition::Manual;
    }
    if (phase == InstallJournalPhase::PartialRootRemovalReturned) {
        if (!callSucceeded) {
            return PartialRootRemovalRecoveryDisposition::Manual;
        }
        if (freshRebootRequired && sameRemovalBoot) {
            return absent || exactPending
                ? PartialRootRemovalRecoveryDisposition::RebootPending
                : PartialRootRemovalRecoveryDisposition::Manual;
        }
        return absent
            ? PartialRootRemovalRecoveryDisposition::ContinueRollback
            : PartialRootRemovalRecoveryDisposition::Manual;
    }
    if (phase ==
        InstallJournalPhase::PartialRootRemovalRebootPending) {
        if (sameRemovalBoot) {
            return absent || exactPending
                ? PartialRootRemovalRecoveryDisposition::RebootPending
                : PartialRootRemovalRecoveryDisposition::Manual;
        }
        return absent
            ? PartialRootRemovalRecoveryDisposition::ContinueRollback
            : PartialRootRemovalRecoveryDisposition::Manual;
    }
    return PartialRootRemovalRecoveryDisposition::ContinueRollback;
}

bool ReconcileInstallJournal(
    bool explicitRecovery,
    uint64_t deadlineUnixMs,
    Outcome* outcome) {
    // Automatic admission and the explicit recover command deliberately run
    // the same reconciler under the same global transaction mutex. The mode
    // changes diagnostics only; it cannot weaken recovery authority.
    *outcome = Outcome{};
    InstallRecoveryDirectory directory;
    bool exists = false;
    Error discoveryError;
    if (!directory.OpenChain(
            false, nullptr, &exists, &discoveryError)) {
        outcome->error = std::move(discoveryError);
        outcome->exitCode = ExitCode::RollbackFailed;
        return false;
    }
    if (!exists) {
        outcome->success = true;
        outcome->exitCode = ExitCode::Success;
        return true;
    }
    if (!PublishInstallRecoveryEvidence(directory.active, 0, &discoveryError)) {
        outcome->error = std::move(discoveryError);
        outcome->exitCode = ExitCode::RollbackFailed;
        return false;
    }
    LoadedInstallJournal loaded;
    if (!LoadInstallJournal(
            std::move(directory), &loaded, &discoveryError)) {
        SetInstallJournalRecoveryOutcome(&loaded,
            L"install-journal-manual-reconciliation",
            discoveryError.code == ERROR_SUCCESS
                ? ERROR_INVALID_DATA : discoveryError.code,
            L"the durable transaction chain or protected package evidence is invalid; no driver mutation was attempted: " +
                discoveryError.message,
            ExitCode::RollbackFailed, outcome);
        return false;
    }
    if (!loaded.hasRecord) {
        if (!GenerateInstallTransactionId(
                &loaded.state.transactionId, &outcome->error) ||
            !RetireLoadedInstallJournal(&loaded, &outcome->error)) {
            outcome->exitCode = ExitCode::RollbackFailed;
            return false;
        }
        outcome->success = true;
        outcome->exitCode = ExitCode::Success;
        return true;
    }
    gActiveRecoveryRecordWritten = true;
    PublishInstallRecoveryEvidence(
        loaded.directory.active, loaded.state.sequence - 1U, nullptr);
    gActiveRecoveryRecordWritten = true;

    const auto appendInstallJournalRecord =
        [&](InstallJournalPhase phase,
            bool callSucceeded,
            DWORD callError,
            bool rebootRequired,
            bool freshRebootRequired,
            InstallJournalStateData::PartialRootRemovalBinding
                partialRootRemovalBinding,
            Error* error) {
        InstallJournalStateData next = loaded.state;
        next.phase = phase;
        next.callSucceeded = callSucceeded;
        next.callError = callError;
        next.rebootRequired = next.rebootRequired || rebootRequired;
        next.freshRebootRequired = freshRebootRequired;
        if (phase == InstallJournalPhase::PartialRootRemovalEntered) {
            if (partialRootRemovalBinding ==
                    InstallJournalStateData::
                        PartialRootRemovalBinding::None ||
                !GetBootIdentifier(
                    &next.partialRootRemovalBootIdentifier, error)) {
                if (error->code == ERROR_SUCCESS) {
                    SetError(error,
                        L"install-journal-partial-root-removal",
                        ERROR_INVALID_PARAMETER);
                }
                return false;
            }
            next.partialRootRemovalBinding =
                partialRootRemovalBinding;
        } else if (partialRootRemovalBinding !=
            InstallJournalStateData::PartialRootRemovalBinding::None) {
            return SetError(error,
                L"install-journal-partial-root-removal",
                ERROR_INVALID_PARAMETER);
        }
        if (freshRebootRequired &&
            !GetBootIdentifier(
                &next.pendingRebootBootIdentifier, error)) {
            return false;
        }
        if (phase == InstallJournalPhase::RollbackBindingEntered) {
            next.direction = InstallJournalDirection::Rollback;
            next.rollbackAuthorized = true;
        }
        if (phase == InstallJournalPhase::RootRegistrationEntered ||
            phase == InstallJournalPhase::RootRegistrationReturned ||
            phase == InstallJournalPhase::DiInstallEntered ||
            phase == InstallJournalPhase::DiInstallReturned) {
            next.bindingMutationStarted = true;
        }
        if (!ValidateInstallJournalTransition(
                &loaded.state, next, error) ||
            !WriteInstallJournalRecord(
                loaded.directory.active, &next, error)) {
            return false;
        }
        loaded.state = std::move(next);
        if (phase == InstallJournalPhase::PartialRootRemovalEntered) {
            loaded.partialRootRemovalEntered = true;
        }
        if (!PublishInstallRecoveryEvidence(
                loaded.directory.active, loaded.state.sequence - 1U, error)) {
            return false;
        }
        gActiveRecoveryRecordWritten = true;
        return true;
    };
    const auto appendPhase =
        [&](InstallJournalPhase phase,
            bool callSucceeded,
            DWORD callError,
            bool rebootRequired,
            Error* error) {
            return appendInstallJournalRecord(phase, callSucceeded,
                callError, rebootRequired, false,
                InstallJournalStateData::PartialRootRemovalBinding::None,
                error);
        };
    const auto appendPartialRootRemovalEntered =
        [&](InstallJournalStateData::PartialRootRemovalBinding binding,
            Error* error) {
            return appendInstallJournalRecord(
                InstallJournalPhase::PartialRootRemovalEntered,
                true, ERROR_SUCCESS, loaded.state.rebootRequired,
                false, binding, error);
        };
    const auto appendAuthoritativeReturn =
        [&](InstallJournalPhase phase,
            bool callSucceeded,
            DWORD callError,
            bool rebootRequired,
            bool freshRebootRequired,
            Error* error) {
            if (phase != InstallJournalPhase::DiInstallReturned &&
                phase != InstallJournalPhase::RollbackBindingReturned &&
                phase != InstallJournalPhase::
                    PartialRootRemovalReturned) {
                return SetError(error,
                    L"install-journal-reboot-return",
                    ERROR_INVALID_PARAMETER);
            }
            return appendInstallJournalRecord(phase, callSucceeded,
                callError, rebootRequired,
                freshRebootRequired,
                InstallJournalStateData::PartialRootRemovalBinding::None,
                error);
        };
    const auto finishSuccess = [&](bool changed) {
        outcome->success = true;
        outcome->changed = changed;
        outcome->exitCode = ExitCode::Success;
        outcome->rollback = changed ? L"succeeded" : L"not-needed";
        return true;
    };
    const auto manual = [&](std::wstring message, const Error* cause = nullptr) {
        message.insert(0, explicitRecovery
            ? L"explicit recover: " : L"automatic admission recovery: ");
        if (cause != nullptr && !cause->message.empty()) {
            message.append(L"; observed: ");
            message.append(cause->message);
        }
        SetInstallJournalRecoveryOutcome(&loaded,
            L"install-journal-manual-reconciliation",
            cause != nullptr && cause->code != ERROR_SUCCESS
                ? cause->code : ERROR_INSTALL_SUSPEND,
            std::move(message), ExitCode::RollbackFailed, outcome);
        return false;
    };
    const auto rebootPending = [&](const wchar_t* message) {
        SetInstallJournalRecoveryOutcome(&loaded,
            L"install-journal-reboot-pending",
            ERROR_SUCCESS_REBOOT_REQUIRED, message,
            ExitCode::RebootRequired, outcome);
        return false;
    };

    std::string currentBoot;
    Error bootError;
    if (!GetBootIdentifier(&currentBoot, &bootError)) {
        return manual(L"the current boot session cannot be compared with the durable transaction", &bootError);
    }
    const bool sameBoot =
        !loaded.state.pendingRebootBootIdentifier.empty() &&
        currentBoot == loaded.state.pendingRebootBootIdentifier;
    const bool samePartialRootRemovalBoot =
        !loaded.state.partialRootRemovalBootIdentifier.empty() &&
        currentBoot == loaded.state.partialRootRemovalBootIdentifier;
    if (loaded.state.phase == InstallJournalPhase::ManualReconciliationRequired) {
        return manual(L"a prior authoritative owner retained the transaction for manual reconciliation");
    }
    const bool partialRootRemovalPhase =
        loaded.state.phase ==
            InstallJournalPhase::PartialRootRemovalEntered ||
        loaded.state.phase ==
            InstallJournalPhase::PartialRootRemovalReturned ||
        loaded.state.phase == InstallJournalPhase::
            PartialRootRemovalRebootPending;
    if (partialRootRemovalPhase) {
        InstallRecoveryRootObservation observedRoot;
        Error observationError;
        if (!ObservePriorEmptyInstallRecoveryRoot(
                loaded, &observedRoot, &observationError)) {
            return manual(
                L"partial root removal topology is not within the exact durable receipt authority",
                &observationError);
        }
        const PartialRootRemovalRecoveryDisposition disposition =
            ClassifyPartialRootRemovalJournalRecovery(
                loaded.state.phase, loaded.state.callSucceeded,
                loaded.state.freshRebootRequired,
                samePartialRootRemovalBoot,
                loaded.state.partialRootRemovalBinding,
                observedRoot.action);
        if (disposition ==
            PartialRootRemovalRecoveryDisposition::Manual) {
            return manual(
                L"partial root removal outcome and current topology do not prove a safe automatic continuation");
        }
        if (disposition ==
            PartialRootRemovalRecoveryDisposition::RebootPending) {
            if (loaded.state.phase != InstallJournalPhase::
                    PartialRootRemovalRebootPending) {
                Error pendingError;
                if (!appendPhase(InstallJournalPhase::
                        PartialRootRemovalRebootPending,
                        loaded.state.phase == InstallJournalPhase::
                            PartialRootRemovalReturned &&
                            loaded.state.callSucceeded,
                        ERROR_SUCCESS_REBOOT_REQUIRED, true,
                        &pendingError)) {
                    return manual(
                        L"partial root removal requires a restart but its durable pending phase could not be published",
                        &pendingError);
                }
            }
            return rebootPending(
                L"receipt-bound root removal must cross the recorded restart before package rollback can continue");
        }
    }
    if (loaded.state.phase == InstallJournalPhase::ForwardRebootPending && sameBoot) {
        return rebootPending(
            L"forward driver activation is still pending the recorded restart");
    }
    if (loaded.state.phase == InstallJournalPhase::RestoreRebootPending && sameBoot) {
        return rebootPending(
            L"exact prior-state restoration is still pending the recorded restart");
    }
    if (InstallJournalNeedsRestoreRebootPending(
            loaded.state, sameBoot)) {
        Error pendingError;
        if (!appendPhase(InstallJournalPhase::RestoreRebootPending,
                true, ERROR_SUCCESS_REBOOT_REQUIRED, true,
                &pendingError)) {
            return manual(
                L"rollback returned with a required restart but its pending phase could not be published",
                &pendingError);
        }
        return rebootPending(
            L"exact prior-state rollback returned successfully and still requires the recorded restart");
    }

    const bool forwardTerminal =
        loaded.state.phase == InstallJournalPhase::ForwardValidated ||
        loaded.state.phase == InstallJournalPhase::ForwardRebootPending;
    if (forwardTerminal) {
        const bool exactBrokerCommit =
            loaded.state.brokerRequired && loaded.state.brokerEntered &&
            loaded.state.brokerSettled && loaded.state.hasBrokerProof &&
            loaded.state.brokerProofSuccess &&
            !loaded.state.brokerDriverRollbackAuthorized &&
            loaded.state.direction == InstallJournalDirection::Forward &&
            !loaded.state.rollbackAuthorized;
        if ((loaded.state.brokerRequired && !exactBrokerCommit) ||
            (!loaded.state.brokerRequired && loaded.state.brokerEntered)) {
            return manual(
                L"terminal forward state lacks its exact canonical broker commit authority");
        }
        Error validationError;
        if (!RecoveryStateMatchesForward(
                loaded, deadlineUnixMs, &validationError)) {
            return manual(
                L"a terminal forward record did not revalidate; exact evidence was retained",
                &validationError);
        }
        if (!RetireLoadedInstallJournal(&loaded, &outcome->error)) {
            outcome->exitCode = ExitCode::RollbackFailed;
            return false;
        }
        return finishSuccess(false);
    }
    const bool restoreTerminal =
        loaded.state.phase == InstallJournalPhase::ExactPriorRestored ||
        loaded.state.phase == InstallJournalPhase::RestoreRebootPending;
    if (restoreTerminal) {
        if (loaded.state.brokerEntered &&
            (loaded.state.direction != InstallJournalDirection::Rollback ||
                !loaded.state.rollbackAuthorized)) {
            return manual(
                L"terminal prior state lacks durable broker-safe rollback authority");
        }
        Error validationError;
        if (!RecoveryStateMatchesPrior(
                loaded, deadlineUnixMs, &validationError)) {
            return manual(
                L"a terminal prior-state record did not revalidate; exact evidence was retained",
                &validationError);
        }
        if (!RetireLoadedInstallJournal(&loaded, &outcome->error)) {
            outcome->exitCode = ExitCode::RollbackFailed;
            return false;
        }
        return finishSuccess(false);
    }

    const bool durableBrokerForwardSuccess =
        loaded.state.brokerRequired && loaded.state.brokerEntered &&
        loaded.state.brokerSettled && loaded.state.hasBrokerProof &&
        loaded.state.brokerProofSuccess &&
        !loaded.state.brokerDriverRollbackAuthorized &&
        loaded.state.direction == InstallJournalDirection::Forward;
    const bool durableNonBrokerForwardSuccess =
        !loaded.state.brokerRequired && !loaded.state.brokerEntered &&
        loaded.state.phase == InstallJournalPhase::DriverValidated &&
        loaded.state.direction == InstallJournalDirection::Forward;
    if (loaded.state.hasPublishedCandidate &&
        (durableBrokerForwardSuccess || durableNonBrokerForwardSuccess)) {
        Error forwardValidation;
        if (RecoveryStateMatchesForward(
                loaded, deadlineUnixMs, &forwardValidation)) {
            Error appendError;
            if (!appendPhase(InstallJournalPhase::ForwardValidated,
                    true, ERROR_SUCCESS, false, &appendError) ||
                !RecoveryStateMatchesForward(
                    loaded, deadlineUnixMs, &appendError) ||
                !RetireLoadedInstallJournal(&loaded, &appendError)) {
                return manual(
                    L"the validated forward state could not be terminally recorded and retired",
                    &appendError);
            }
            return finishSuccess(false);
        }
        if (durableBrokerForwardSuccess) {
            return manual(
                L"the child durably committed but the forward driver state did not revalidate; no driver rollback is authorized",
                &forwardValidation);
        }
    }

    if (loaded.state.brokerEntered &&
        loaded.state.phase == InstallJournalPhase::BrokerHandoffReturned &&
        loaded.state.callSucceeded && !loaded.state.brokerSettled &&
        !loaded.state.hasBrokerProof &&
        loaded.state.direction == InstallJournalDirection::Forward) {
        Error authorizationError;
        if (!appendPhase(InstallJournalPhase::RollbackBindingEntered,
                true, ERROR_SUCCESS, false, &authorizationError)) {
            return manual(
                L"pre-child rollback authority could not be durably admitted",
                &authorizationError);
        }
    }
    if (loaded.state.phase == InstallJournalPhase::BrokerChildEntered &&
        loaded.state.direction == InstallJournalDirection::Forward &&
        !loaded.state.rollbackAuthorized &&
        !loaded.state.hasBrokerProof) {
        return manual(
            L"broker child creation was admitted without a durable canonical settlement proof; driver evidence was retained and no mutation was attempted");
    }

    const bool priorRequiresAbiProfile =
        loaded.state.bindingMutationStarted &&
        loaded.state.prior.devices.size() == 1U &&
        loaded.state.prior.devices[0].started &&
        loaded.state.prior.devices[0].problem == 0;
    if (priorRequiresAbiProfile && !loaded.state.hasPriorAbiProfile) {
        Snapshot observedBeforeProfile;
        Error profileError;
        AbiCompatibilityProfile negotiatedProfile{};
        if (!CaptureSnapshot(&observedBeforeProfile, &profileError) ||
            !SameCapturedRootState(
                loaded.state.prior, observedBeforeProfile) ||
            !VerifyAbiHealth(deadlineUnixMs, nullptr, &profileError,
                AbiHealthPurpose::PristineUpgrade, nullptr,
                &negotiatedProfile)) {
            if (profileError.code == ERROR_SUCCESS) {
                SetError(&profileError,
                    L"install-journal-prior-abi-profile",
                    ERROR_REVISION_MISMATCH,
                    L"missing prior ABI profile cannot be negotiated after the captured binding changed");
            }
            return manual(
                L"started prior root lacks a durable exact ABI profile; no recovery mutation was attempted",
                &profileError);
        }
        loaded.state.priorAbiProfile = negotiatedProfile;
        loaded.state.hasPriorAbiProfile = true;
        if (!appendPhase(InstallJournalPhase::PriorAbiProfileCaptured,
                true, ERROR_SUCCESS, false, &profileError)) {
            return manual(
                L"the exact prior ABI profile was negotiated but could not be published before recovery mutation",
                &profileError);
        }
    }

    const bool rollbackWasAuthorized =
        loaded.state.direction == InstallJournalDirection::Rollback &&
        loaded.state.rollbackAuthorized;
    Error priorValidation;
    const bool priorValid =
        RecoveryStateMatchesPrior(
            loaded, deadlineUnixMs, &priorValidation);
    if (priorValid &&
        (!loaded.state.bindingMutationStarted ||
            InstallJournalHasAuthoritativeRollbackSettlement(
                loaded.state))) {
        if (loaded.state.brokerEntered &&
            !rollbackWasAuthorized) {
            return manual(
                L"the driver resembles the prior state, but broker handoff lacks a durable settled rollback authorization; evidence was retained");
        }
        Error appendError;
        if (!appendPhase(InstallJournalPhase::ExactPriorRestored,
                true, ERROR_SUCCESS, false, &appendError) ||
            !RecoveryStateMatchesPrior(
                loaded, deadlineUnixMs, &appendError) ||
            !RetireLoadedInstallJournal(&loaded, &appendError)) {
            return manual(
                L"the prior state was present but could not be terminally recorded and retired",
                &appendError);
        }
        return finishSuccess(false);
    }

    std::vector<PackageInfo> currentPackages;
    Error inventoryError;
    if (!EnumerateOwnedPackages(&currentPackages, &inventoryError)) {
        return manual(L"the current Driver Store inventory could not be classified", &inventoryError);
    }
    if (!loaded.state.hasPublishedCandidate) {
        size_t matches = 0;
        for (const PackageInfo& package : currentPackages) {
            if (package.version == loaded.state.candidate.version &&
                SamePackageBytes(package, loaded.state.candidate) &&
                !ContainsExactPackage(loaded.state.prior.packages, package)) {
                loaded.state.publishedCandidate = package;
                loaded.state.hasPublishedCandidate = true;
                ++matches;
            }
        }
        if (matches > 1U) {
            return manual(L"more than one exact candidate publication exists");
        }
        if (matches == 1U) {
            return manual(
                L"an exact candidate publication exists without a durable StageReceiptCaptured ownership record; it may be concurrent and will not be removed automatically");
        }
    }

    Error forwardValidation;
    const bool forwardValid = loaded.state.hasPublishedCandidate &&
        RecoveryStateMatchesForward(
            loaded, deadlineUnixMs, &forwardValidation);
    const bool settledForwardBroker =
        loaded.state.brokerRequired && loaded.state.brokerEntered &&
        loaded.state.brokerSettled && loaded.state.hasBrokerProof &&
        loaded.state.brokerProofSuccess &&
        !loaded.state.brokerDriverRollbackAuthorized &&
        loaded.state.direction == InstallJournalDirection::Forward;
    if (forwardValid &&
        ((!loaded.state.brokerRequired &&
             !loaded.state.brokerEntered &&
             loaded.state.phase == InstallJournalPhase::DriverValidated &&
             loaded.state.direction == InstallJournalDirection::Forward) ||
            settledForwardBroker)) {
        Error appendError;
        if (!appendPhase(InstallJournalPhase::ForwardValidated,
                true, ERROR_SUCCESS, false, &appendError) ||
            !RecoveryStateMatchesForward(
                loaded, deadlineUnixMs, &appendError) ||
            !RetireLoadedInstallJournal(&loaded, &appendError)) {
            return manual(
                L"the validated forward state could not be terminally recorded and retired",
                &appendError);
        }
        return finishSuccess(false);
    }

    if (loaded.state.brokerEntered &&
        !rollbackWasAuthorized) {
        return manual(
            L"broker handoff was entered without a durable, settled rollback authorization; driver evidence was retained and no mutation was attempted");
    }
    for (const PackageInfo& priorPackage : loaded.state.prior.packages) {
        const size_t exactMatches = static_cast<size_t>(std::count_if(
            currentPackages.begin(), currentPackages.end(),
            [&](const PackageInfo& current) {
                return _wcsicmp(current.publishedName.c_str(),
                           priorPackage.publishedName.c_str()) == 0 &&
                    current.version == priorPackage.version &&
                    SamePackageBytes(current, priorPackage);
            }));
        if (exactMatches != 1U) {
            return manual(
                L"the exact prior published package name and bytes are not available in the Driver Store; protected package bytes were retained, but automatic republishing cannot promise the same OEM identity");
        }
    }

    bool stagedCandidateStillPresent = false;
    if (loaded.state.packageStagedHere &&
        loaded.state.hasPublishedCandidate) {
        size_t publishedNameMatches = 0;
        for (const PackageInfo& current : currentPackages) {
            if (_wcsicmp(current.publishedName.c_str(),
                    loaded.state.publishedCandidate.publishedName.c_str()) != 0) {
                continue;
            }
            ++publishedNameMatches;
            if (!SameJournalPackageIdentity(
                    current, loaded.state.publishedCandidate)) {
                return manual(
                    L"the staged-here published name now identifies different bytes; no automatic removal was attempted");
            }
            stagedCandidateStillPresent = true;
        }
        if (publishedNameMatches > 1U) {
            return manual(
                L"the staged-here published identity is duplicated; no automatic removal was attempted");
        }
    }
    std::vector<PackageInfo> exactPreRollbackInventory =
        loaded.state.prior.packages;
    if (stagedCandidateStillPresent) {
        exactPreRollbackInventory.push_back(
            loaded.state.publishedCandidate);
    }
    std::sort(exactPreRollbackInventory.begin(),
        exactPreRollbackInventory.end(),
        [](const PackageInfo& left, const PackageInfo& right) {
            return _wcsicmp(left.publishedName.c_str(),
                right.publishedName.c_str()) < 0;
        });
    if (!SamePackageInventory(
            currentPackages, exactPreRollbackInventory)) {
        return manual(
            L"current Driver Store inventory is not exactly the captured prior set plus the one transaction-owned staged candidate; no rollback mutation was attempted");
    }
    Error rootAuthorityError;
    InstallRecoveryRootObservation rootAuthority;
    if (!CurrentRootIsAuthorizedForInstallRollback(
            loaded, &rootAuthority, &rootAuthorityError)) {
        return manual(
            L"current root topology is outside the exact rollback authority of this transaction; no device mutation was attempted",
            &rootAuthorityError);
    }

    Error appendError;
    if (!appendPhase(InstallJournalPhase::RollbackBindingEntered,
            true, ERROR_SUCCESS, false, &appendError)) {
        return manual(
            L"write-ahead rollback admission could not be published; no recovery mutation was attempted",
            &appendError);
    }
    Error confirmedInventoryError;
    if (!VerifyPackageInventory(exactPreRollbackInventory,
            L"install-journal-post-admission-inventory",
            &confirmedInventoryError)) {
        return manual(
            L"Driver Store inventory changed after write-ahead rollback admission; no device mutation was attempted",
            &confirmedInventoryError);
    }
    InstallRecoveryRootObservation confirmedRoot;
    Error confirmedRootError;
    if (!CurrentRootIsAuthorizedForInstallRollback(
            loaded, &confirmedRoot, &confirmedRootError)) {
        return manual(
            L"current root topology changed after write-ahead rollback admission; no device mutation was attempted",
            &confirmedRootError);
    }
    const PackageInfo* stagedCandidate =
        loaded.state.packageStagedHere &&
            loaded.state.hasPublishedCandidate && stagedCandidateStillPresent
            ? &loaded.state.publishedCandidate : nullptr;
    bool rollbackReboot = InstallJournalRollbackRetryRebootSeed(
        loaded.state, sameBoot);
    const bool rollbackRebootAtAdmission = rollbackReboot;
    Error rollbackError;
    const uint64_t rollbackDeadline =
        CurrentUnixMilliseconds() + kDriverRollbackCeilingMs;
    bool rollbackSucceeded = true;
    if (confirmedRoot.action ==
            PartialInstallRootRecoveryAction::RemoveUnboundExactRoot ||
        confirmedRoot.action ==
            PartialInstallRootRecoveryAction::RemoveCandidateBoundExactRoot) {
        if (!CheckTransactionDeadline(rollbackDeadline,
                L"install-journal-rollback-deadline-partial-root",
                &rollbackError)) {
            return manual(
                L"receipt-bound root removal missed its deadline before durable API admission",
                &rollbackError);
        }
        Error removalEnteredError;
        const InstallJournalStateData::PartialRootRemovalBinding
            removalBinding = confirmedRoot.action ==
                    PartialInstallRootRecoveryAction::
                        RemoveCandidateBoundExactRoot
                ? InstallJournalStateData::PartialRootRemovalBinding::Candidate
                : InstallJournalStateData::PartialRootRemovalBinding::Unbound;
        if (!appendPartialRootRemovalEntered(
                removalBinding, &removalEnteredError)) {
            return manual(
                L"receipt-bound root removal could not publish its exact write-ahead API admission",
                &removalEnteredError);
        }
        Error removalInventoryError;
        if (!VerifyPackageInventory(exactPreRollbackInventory,
                L"install-journal-partial-root-removal-inventory",
                &removalInventoryError)) {
            return manual(
                L"Driver Store inventory changed after receipt-bound root removal admission; no device API was called",
                &removalInventoryError);
        }
        InstallRecoveryRootObservation removalRoot;
        Error removalRootError;
        LoadedInstallJournal preCallLoaded;
        preCallLoaded.state = loaded.state;
        preCallLoaded.forwardRootRegistrationEntered =
            loaded.forwardRootRegistrationEntered;
        preCallLoaded.forwardDiInstallEntered =
            loaded.forwardDiInstallEntered;
        preCallLoaded.partialRootRemovalEntered = false;
        if (!CurrentRootIsAuthorizedForInstallRollback(
                preCallLoaded, &removalRoot, &removalRootError)) {
            return manual(
                L"root topology changed after receipt-bound root removal admission; no device API was called",
                &removalRootError);
        }
        if (removalRoot.action ==
                PartialInstallRootRecoveryAction::PriorEmpty ||
            removalRoot.action == PartialInstallRootRecoveryAction::
                PendingExactRootRemoval) {
            Error pendingError;
            if (!appendPhase(InstallJournalPhase::
                    PartialRootRemovalRebootPending,
                    false, ERROR_SUCCESS_REBOOT_REQUIRED, true,
                    &pendingError)) {
                return manual(
                    L"indeterminate receipt-bound root removal could not publish its conservative reboot boundary",
                    &pendingError);
            }
            return rebootPending(
                L"receipt-bound root removal changed after admission and must cross the recorded restart before package rollback");
        }
        if (removalRoot.action != confirmedRoot.action) {
            return manual(
                L"receipt-bound root identity changed after durable removal admission; no device API was called");
        }
        bool freshRemovalReboot = false;
        rollbackSucceeded = RemoveDevice(
            removalRoot.set.get(), removalRoot.data, 0,
            L"install-journal-rollback-deadline-partial-root",
            nullptr, &rollbackReboot, &rollbackError,
            &freshRemovalReboot);
        Error removalReturnedError;
        if (!appendAuthoritativeReturn(
                InstallJournalPhase::PartialRootRemovalReturned,
                rollbackSucceeded,
                rollbackSucceeded ? ERROR_SUCCESS : rollbackError.code,
                rollbackReboot, freshRemovalReboot,
                &removalReturnedError)) {
            return manual(
                L"receipt-bound root removal returned but its exact authoritative outcome could not be published",
                &removalReturnedError);
        }
        if (!rollbackSucceeded) {
            return manual(
                L"receipt-bound root removal returned failure; exact evidence was retained",
                &rollbackError);
        }
        InstallRecoveryRootObservation afterRemoval;
        Error afterRemovalError;
        if (!ObservePriorEmptyInstallRecoveryRoot(
                loaded, &afterRemoval, &afterRemovalError)) {
            return manual(
                L"receipt-bound root removal returned but its resulting topology is not canonical",
                &afterRemovalError);
        }
        if (freshRemovalReboot) {
            if (afterRemoval.action !=
                    PartialInstallRootRecoveryAction::PriorEmpty &&
                afterRemoval.action != PartialInstallRootRecoveryAction::
                    PendingExactRootRemoval) {
                return manual(
                    L"reboot-requiring receipt-bound root removal left an unauthorized topology");
            }
            Error pendingError;
            if (!appendPhase(InstallJournalPhase::
                    PartialRootRemovalRebootPending,
                    true, ERROR_SUCCESS_REBOOT_REQUIRED, true,
                    &pendingError)) {
                return manual(
                    L"receipt-bound root removal requires a restart but its pending phase could not be published",
                    &pendingError);
            }
            return rebootPending(
                L"receipt-bound root removal returned successfully and requires the recorded restart before package rollback");
        }
        if (afterRemoval.action !=
            PartialInstallRootRecoveryAction::PriorEmpty) {
            return manual(
                L"receipt-bound root removal returned without a restart but exact prior-empty topology was not restored");
        }
    }
    if (rollbackSucceeded) {
        rollbackSucceeded = VerifyPackageInventory(
                exactPreRollbackInventory,
                L"install-journal-pre-package-rollback-inventory",
                &rollbackError) &&
            VerifyInstallJournalRawPriorTopology(
                loaded.state, &rollbackError);
    }
    if (rollbackSucceeded) {
        const bool restoreBindingThroughStrictSnapshot =
            InstallJournalRecoveryUsesStrictBindingRestore(
                loaded.state);
        rollbackSucceeded = RollbackInstall(
            loaded.state.prior, stagedCandidate,
            restoreBindingThroughStrictSnapshot,
            loaded.state.hasPriorAbiProfile
                ? &loaded.state.priorAbiProfile : nullptr,
            rollbackDeadline, &rollbackReboot, &rollbackError);
    }
    if (!rollbackSucceeded) {
        Error ignored;
        appendAuthoritativeReturn(
            InstallJournalPhase::RollbackBindingReturned,
            false, rollbackError.code, rollbackReboot,
            rollbackReboot && !rollbackRebootAtAdmission,
            &ignored);
        appendPhase(InstallJournalPhase::ManualReconciliationRequired,
            false, rollbackError.code, rollbackReboot, &ignored);
        return manual(L"authoritative exact-prior recovery failed", &rollbackError);
    }
    Error returnedError;
    if (!appendAuthoritativeReturn(
            InstallJournalPhase::RollbackBindingReturned,
            true, ERROR_SUCCESS, rollbackReboot,
            rollbackReboot && !rollbackRebootAtAdmission,
            &returnedError)) {
        return manual(
            L"rollback returned authoritatively, but its returned phase could not be published",
            &returnedError);
    }
    if (rollbackReboot) {
        Error pendingError;
        if (!appendPhase(InstallJournalPhase::RestoreRebootPending,
                true, ERROR_SUCCESS_REBOOT_REQUIRED, true,
                &pendingError)) {
            return manual(
                L"rollback requires reboot but its pending state could not be published",
                &pendingError);
        }
        return rebootPending(
            L"exact prior-state rollback completed with a required restart; evidence remains retained");
    }
    Error restoredError;
    if (!RecoveryStateMatchesPrior(
            loaded, rollbackDeadline, &restoredError) ||
        !appendPhase(InstallJournalPhase::ExactPriorRestored,
            true, ERROR_SUCCESS, false, &restoredError) ||
        !RecoveryStateMatchesPrior(
            loaded, rollbackDeadline, &restoredError) ||
        !RetireLoadedInstallJournal(&loaded, &restoredError)) {
        return manual(
            L"rollback returned but exact prior-state revalidation or journal retirement failed",
            &restoredError);
    }
    return finishSuccess(true);
}

bool RollbackRemove(
    const Snapshot& prior,
    const std::vector<PackageBackup>& backups,
    uint64_t rollbackDeadlineUnixMs,
    bool* rebootRequired,
    Error* error) {
    for (const PackageBackup& backup : backups) {
        if (!CheckTransactionDeadline(
                rollbackDeadlineUnixMs, L"remove-rollback-deadline-package", error)) {
            return false;
        }
        BOOL reboot = FALSE;
        MarkTransactionMutationStarted();
        if (!DiInstallDriverW(nullptr, backup.infPath.c_str(), 0, &reboot)) {
            return SetLastErrorDetail(error, L"remove-rollback-package");
        }
        *rebootRequired = *rebootRequired || reboot != FALSE;
    }
    if (!CheckTransactionDeadline(
            rollbackDeadlineUnixMs, L"remove-rollback-deadline-binding", error)) {
        return false;
    }
    Snapshot restorablePrior = prior;
    std::vector<PackageInfo> reinstalledPackages;
    if (!EnumerateOwnedPackages(&reinstalledPackages, error)) {
        return false;
    }
    for (DeviceState& device : restorablePrior.devices) {
        const auto package = std::find_if(reinstalledPackages.begin(), reinstalledPackages.end(),
            [&](const PackageInfo& candidate) {
                return SamePackageBytes(candidate, device.package) &&
                    candidate.version == device.package.version;
            });
        if (package == reinstalledPackages.end()) {
            return SetError(error, L"remove-rollback-package-identity", ERROR_NOT_FOUND,
                L"the exact captured package was not republished for devnode restoration");
        }
        device.package = *package;
        device.publishedInf = package->publishedName;
    }
    if (!RestorePriorBinding(
            restorablePrior, rollbackDeadlineUnixMs, rebootRequired, error)) {
        return false;
    }

    if (!CheckTransactionDeadline(
            rollbackDeadlineUnixMs, L"remove-rollback-deadline-verify", error)) {
        return false;
    }
    Snapshot restored;
    if (!CaptureSnapshot(&restored, error)) {
        return false;
    }
    std::multiset<std::pair<Version, std::string>> expectedPackages;
    std::multiset<std::pair<Version, std::string>> actualPackages;
    for (const PackageInfo& package : prior.packages) {
        expectedPackages.emplace(package.version, PackageBytesKey(package));
    }
    for (const PackageInfo& package : restored.packages) {
        actualPackages.emplace(package.version, PackageBytesKey(package));
    }
    if (expectedPackages != actualPackages || restored.devices.size() != prior.devices.size()) {
        return SetError(error, L"remove-rollback-verification", ERROR_REVISION_MISMATCH,
            L"rollback did not restore the exact prior package and devnode topology");
    }
    if (!prior.devices.empty()) {
        if (_wcsicmp(restored.devices[0].instanceId.c_str(), prior.devices[0].instanceId.c_str()) != 0 ||
            !SamePackageBytes(restored.devices[0].package, prior.devices[0].package)) {
            return SetError(error, L"remove-rollback-verification", ERROR_REVISION_MISMATCH,
                L"rollback restored a different devnode identity or active package");
        }
        if (!*rebootRequired && prior.devices[0].started) {
            if (!CheckTransactionDeadline(
                    rollbackDeadlineUnixMs, L"remove-rollback-deadline-health", error)) {
                return false;
            }
            const uint64_t healthDeadline = std::min(
                rollbackDeadlineUnixMs, CurrentUnixMilliseconds() + 15000);
            if (!VerifyAbiHealth(healthDeadline, nullptr, error)) {
                return false;
            }
        }
    }
    return true;
}

struct RemoveOptions {
    uint64_t transactionDeadlineUnixMs = 0;
};

Outcome Remove(const RemoveOptions& options) {
    Outcome outcome;
    if (!ValidateTransactionDeadlineBudget(options.transactionDeadlineUnixMs, &outcome.error)) {
        outcome.exitCode = ExitCode::PreflightRejected;
        return outcome;
    }
    if (!IsElevated()) {
        SetError(&outcome.error, L"elevation", ERROR_ELEVATION_REQUIRED);
        outcome.exitCode = ExitCode::PreflightRejected;
        return outcome;
    }
    TransactionMutex mutex;
    if (!mutex.Acquire(&outcome.error)) {
        outcome.exitCode = ExitCode::PreflightRejected;
        return outcome;
    }
    Outcome recoveryOutcome;
    if (!ReconcileInstallJournal(
            false, options.transactionDeadlineUnixMs, &recoveryOutcome)) {
        return recoveryOutcome;
    }
    if (!CheckTransactionDeadline(
            options.transactionDeadlineUnixMs, L"remove-deadline-before-snapshot", &outcome.error)) {
        outcome.exitCode = ExitCode::PreflightRejected;
        return outcome;
    }
    Snapshot prior;
    if (!CaptureSnapshot(&prior, &outcome.error)) {
        outcome.exitCode = ExitCode::PreflightRejected;
        return outcome;
    }
    if (prior.devices.size() > 1 ||
        (!prior.devices.empty() && !prior.devices[0].present)) {
        SetError(&outcome.error, L"remove-topology", ERROR_DUPLICATE_SERVICE_NAME,
            L"removal requires zero devices or one present exact owned root devnode");
        outcome.exitCode = ExitCode::PreflightRejected;
        return outcome;
    }
    if (prior.devices.empty() && prior.packages.empty()) {
        outcome.success = true;
        outcome.exitCode = ExitCode::Success;
        return outcome;
    }
    BackupDirectory backupRoot;
    std::vector<PackageBackup> backups;
    const auto rejectBeforeMutation = [&](Error failure) {
        Error cleanupError;
        if (!backupRoot.Cleanup(&backups, &cleanupError)) {
            outcome.error = std::move(cleanupError);
        } else {
            outcome.error = std::move(failure);
        }
        outcome.exitCode = ExitCode::PreflightRejected;
    };
    if (!BackupPackages(prior.packages, &backupRoot, &backups, &outcome.error)) {
        Error failure = std::move(outcome.error);
        rejectBeforeMutation(std::move(failure));
        return outcome;
    }
    std::string recoveryRecord;
    const std::filesystem::path recoveryPath = backupRoot.RecoveryRecordPath();
    Error recoveryError;
    if (!BuildRemoveRecoveryRecord(
            prior, backups, backupRoot.path(), &recoveryRecord, &recoveryError) ||
        !WriteProtectedRecoveryRecord(recoveryPath, recoveryRecord, &recoveryError) ||
        !backupRoot.ArmPreservation(recoveryPath, &recoveryError)) {
        rejectBeforeMutation(std::move(recoveryError));
        return outcome;
    }
    if (!CheckTransactionDeadline(
            options.transactionDeadlineUnixMs, L"remove-deadline-before-device", &outcome.error)) {
        Error failure = std::move(outcome.error);
        rejectBeforeMutation(std::move(failure));
        return outcome;
    }
    bool mutationStarted = false;
    bool reboot = false;
    Error mutationError;
    bool mutationSucceeded = RemoveAllExactDevices(
        options.transactionDeadlineUnixMs, &mutationStarted, &reboot, &mutationError);
    outcome.changed = mutationStarted;
    if (mutationSucceeded && !CheckTransactionDeadline(
            options.transactionDeadlineUnixMs, L"remove-deadline-after-device", &mutationError)) {
        mutationSucceeded = false;
    }
    if (mutationSucceeded) {
        for (const PackageInfo& package : prior.packages) {
            if (!CheckTransactionDeadline(
                    options.transactionDeadlineUnixMs, L"remove-deadline-before-package", &mutationError)) {
                mutationSucceeded = false;
                break;
            }
            mutationStarted = true;
            outcome.changed = true;
            if (!UninstallPackage(package, &reboot, &mutationError)) {
                mutationSucceeded = false;
                break;
            }
            if (!CheckTransactionDeadline(
                    options.transactionDeadlineUnixMs, L"remove-deadline-after-package", &mutationError)) {
                mutationSucceeded = false;
                break;
            }
        }
    }
    if (mutationSucceeded && !reboot) {
        if (!CheckTransactionDeadline(
                options.transactionDeadlineUnixMs, L"remove-deadline-before-verify", &mutationError)) {
            mutationSucceeded = false;
        }
    }
    if (mutationSucceeded && !reboot) {
        Snapshot verified;
        if (!CaptureSnapshot(&verified, &mutationError) ||
            !verified.devices.empty() || !verified.packages.empty()) {
            if (mutationError.code == ERROR_SUCCESS) {
                SetError(&mutationError, L"remove-verification", ERROR_DEVICE_IN_USE);
            }
            mutationSucceeded = false;
        }
    }
    if (!mutationSucceeded) {
        if (!mutationStarted) {
            rejectBeforeMutation(std::move(mutationError));
            return outcome;
        }
        Error rollbackError;
        bool rollbackReboot = reboot;
        // Forward work owns the caller's absolute deadline. Rollback receives
        // one fresh, bounded ceiling from the instant failure is observed; it
        // must not inherit the unused portion of a long forward deadline and
        // silently expand into a six-minute transaction.
        const uint64_t rollbackDeadline =
            CurrentUnixMilliseconds() + kDriverRollbackCeilingMs;
        if (RollbackRemove(
                prior, backups, rollbackDeadline, &rollbackReboot, &rollbackError)) {
            outcome.rollback = L"succeeded";
            outcome.rebootRequired = rollbackReboot;
            Error cleanupError;
            if (!backupRoot.Cleanup(&backups, &cleanupError)) {
                outcome.error = std::move(cleanupError);
                return outcome;
            }
            outcome.error = std::move(mutationError);
            return outcome;
        }
        backupRoot.AttachRecoveryRecord(&rollbackError);
        outcome.rollback = L"failed";
        outcome.rebootRequired = rollbackReboot;
        outcome.error = std::move(rollbackError);
        outcome.exitCode = ExitCode::RollbackFailed;
        return outcome;
    }
    Error cleanupError;
    if (!backupRoot.Cleanup(&backups, &cleanupError)) {
        outcome.rollback = L"failed";
        outcome.rebootRequired = reboot;
        outcome.error = std::move(cleanupError);
        outcome.exitCode = ExitCode::RollbackFailed;
        return outcome;
    }
    outcome.success = true;
    outcome.rebootRequired = reboot;
    outcome.exitCode = reboot ? ExitCode::RebootRequired : ExitCode::Success;
    return outcome;
}

Outcome Recover(uint64_t transactionDeadlineUnixMs) {
    Outcome outcome;
    if (!ValidateTransactionDeadlineBudget(
            transactionDeadlineUnixMs, &outcome.error)) {
        outcome.exitCode = ExitCode::PreflightRejected;
        return outcome;
    }
    if (!IsElevated()) {
        SetError(&outcome.error, L"elevation", ERROR_ELEVATION_REQUIRED);
        outcome.exitCode = ExitCode::PreflightRejected;
        return outcome;
    }
    TransactionMutex mutex;
    if (!mutex.Acquire(&outcome.error)) {
        outcome.exitCode = ExitCode::PreflightRejected;
        return outcome;
    }
    if (!ReconcileInstallJournal(
            true, transactionDeadlineUnixMs, &outcome)) {
        return outcome;
    }
    outcome.success = true;
    outcome.exitCode = ExitCode::Success;
    return outcome;
}

enum class InstallJournalRecoveryModelAction {
    RetirePrior,
    RetireForward,
    RollbackPrior,
    RebootPending,
    Manual,
};

InstallJournalRecoveryModelAction ClassifyInstallJournalRecoveryModel(
    InstallJournalPhase phase,
    bool chainValid,
    bool securityValid,
    bool sameBoot,
    bool priorValid,
    bool forwardValid,
    bool brokerEntered,
    bool brokerSettled,
    bool brokerSucceeded) noexcept {
    if (!chainValid || !securityValid ||
        phase == InstallJournalPhase::ManualReconciliationRequired) {
        return InstallJournalRecoveryModelAction::Manual;
    }
    if ((phase == InstallJournalPhase::ForwardRebootPending ||
            phase == InstallJournalPhase::RestoreRebootPending) &&
        sameBoot) {
        return InstallJournalRecoveryModelAction::RebootPending;
    }
    if (priorValid) {
        return InstallJournalRecoveryModelAction::RetirePrior;
    }
    if (forwardValid &&
        (phase == InstallJournalPhase::ForwardValidated ||
            phase == InstallJournalPhase::ForwardRebootPending ||
            (!brokerEntered && phase == InstallJournalPhase::DriverValidated) ||
            (brokerSettled && brokerSucceeded))) {
        return InstallJournalRecoveryModelAction::RetireForward;
    }
    if (brokerEntered && !brokerSettled) {
        return InstallJournalRecoveryModelAction::Manual;
    }
    return InstallJournalRecoveryModelAction::RollbackPrior;
}

bool RunInstallJournalModelSelfTest(Error* error) {
    const std::wstring modelTargetUserSid =
        L"S-1-5-21-1-2-3-1001";
    std::wstring modelProductSecurity;
    if (!ProductDirectoryMaskIsReadExecuteOnly(0x001200a9U) ||
        !ProductDirectoryMaskIsReadExecuteOnly(
            GENERIC_READ | GENERIC_EXECUTE) ||
        ProductDirectoryMaskIsReadExecuteOnly(
            GENERIC_READ | GENERIC_WRITE) ||
        ProductDirectoryMaskIsReadExecuteOnly(
            0x001200a9U | FILE_ADD_FILE) ||
        ProductDirectoryMaskIsReadExecuteOnly(
            0x001200a9U | FILE_DELETE_CHILD) ||
        ProductDirectoryMaskIsReadExecuteOnly(
            0x001200a9U | DELETE) ||
        ProductDirectoryMaskIsReadExecuteOnly(
            0x001200a9U | WRITE_DAC) ||
        ProductDirectoryMaskIsReadExecuteOnly(
            0x001200a9U | WRITE_OWNER) ||
        !BuildInstallRecoveryProductDirectorySecurity(
            modelTargetUserSid, &modelProductSecurity, error) ||
        modelProductSecurity !=
            L"O:BAD:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
            L"(A;OICI;GRGX;;;S-1-5-21-1-2-3-1001)" ||
        !InstallRecoveryChainHasActive(true, true, true, true) ||
        InstallRecoveryChainHasActive(true, false, false, false) ||
        InstallRecoveryChainHasActive(true, true, false, false) ||
        InstallRecoveryChainHasActive(true, true, true, false) ||
        InstallRecoveryChainHasActive(false, false, false, false)) {
        return SetError(error,
            L"self-test-install-journal-product-security",
            ERROR_INVALID_DATA);
    }
    for (InstallJournalPhase phase : {
            InstallJournalPhase::Prepared,
            InstallJournalPhase::SetupCopyEntered,
            InstallJournalPhase::SetupCopyReturned,
            InstallJournalPhase::StageReceiptCaptured,
            InstallJournalPhase::QuiesceSignalEntered,
            InstallJournalPhase::QuiesceSignalReturned,
            InstallJournalPhase::RootRegistrationIntentCaptured,
            InstallJournalPhase::RootRegistrationEntered,
            InstallJournalPhase::RootRegistrationReturned,
            InstallJournalPhase::DiInstallEntered,
            InstallJournalPhase::DiInstallReturned,
            InstallJournalPhase::PriorAbiProfileCaptured,
            InstallJournalPhase::DriverValidated,
            InstallJournalPhase::BrokerHandoffEntered,
            InstallJournalPhase::BrokerHandoffReturned,
            InstallJournalPhase::BrokerChildEntered,
            InstallJournalPhase::BrokerChildSettled,
            InstallJournalPhase::RollbackBindingEntered,
            InstallJournalPhase::PartialRootRemovalEntered,
            InstallJournalPhase::PartialRootRemovalReturned,
            InstallJournalPhase::PartialRootRemovalRebootPending,
            InstallJournalPhase::RollbackBindingReturned,
            InstallJournalPhase::SetupUninstallEntered,
            InstallJournalPhase::SetupUninstallReturned,
            InstallJournalPhase::ForwardValidated,
            InstallJournalPhase::ExactPriorRestored,
            InstallJournalPhase::ForwardRebootPending,
            InstallJournalPhase::RestoreRebootPending,
            InstallJournalPhase::ManualReconciliationRequired}) {
        const char* name = InstallJournalPhaseName(phase);
        const std::optional<InstallJournalPhase> parsed =
            ParseInstallJournalPhase(name);
        if (!parsed || *parsed != phase) {
            return SetError(error, L"self-test-install-journal-phase",
                ERROR_INVALID_DATA);
        }
    }
    if (ParseInstallJournalPhase("setup-copy-entered") ||
        ParseInstallJournalDirection("Forward") ||
        ParseInstallJournalDirection("rollback ") ||
        ParseInstallJournalDirection("forward") !=
            InstallJournalDirection::Forward ||
        ParseInstallJournalDirection("rollback") !=
            InstallJournalDirection::Rollback ||
        InstallJournalPhaseRequiresPriorAbiProfile(
            InstallJournalPhase::DriverValidated) ||
        !InstallJournalPhaseRequiresPriorAbiProfile(
            InstallJournalPhase::DiInstallEntered) ||
        !IsKnownAbiCompatibilityProfile(kAbiCompatibilityProfiles[0]) ||
        !IsSafeRecoveryRelativePath(
            std::filesystem::path(L"candidate") / L"ViiperUde.inf") ||
        IsSafeRecoveryRelativePath(std::filesystem::path(L"..") / L"escape") ||
        IsSafeRecoveryRelativePath(
            std::filesystem::path(L"C:\\Windows\\INF\\oem1.inf"))) {
        return SetError(error, L"self-test-install-journal-security-model",
            ERROR_INVALID_DATA);
    }
    uint64_t recordSequence = 0;
    if (!ParseJournalRecordFileName(
            L"journal-00000042.json", &recordSequence) ||
        recordSequence != 42U ||
        ParseJournalRecordFileName(L"journal-42.json", &recordSequence) ||
        ParseJournalRecordFileName(
            L"journal-00000042.json.tmp", &recordSequence) ||
        !ParseJournalTemporaryFileName(
            L"journal-00000042.json.tmp", &recordSequence) ||
        recordSequence != 42U ||
        ParseJournalTemporaryFileName(
            L"journal-00000042.json.tmp.tmp", &recordSequence) ||
        !InstallJournalTemporarySequenceIsRecoverable(42U, 42U) ||
        InstallJournalTemporarySequenceIsRecoverable(41U, 42U) ||
        InstallJournalTemporarySequenceIsRecoverable(43U, 42U)) {
        return SetError(error, L"self-test-install-journal-cutpoint",
            ERROR_INVALID_DATA);
    }

    InstallJournalStateData state;
    state.transactionId = std::string(64, 'a');
    state.bootIdentifier = std::string(32, 'b');
    state.sourceRevision = std::string(40, 'c');
    state.candidate.version.parts = {1, 2, 3, 4};
    state.candidate.infSha256 = std::string(64, 'd');
    state.candidate.sysSha256 = std::string(64, 'e');
    state.candidate.catSha256 = std::string(64, 'f');
    std::string payload;
    std::string digest;
    if (!BuildInstallJournalPayload(state, &payload, error) ||
        !Sha256Data(payload, &digest, error)) {
        return false;
    }
    Error transitionError;
    if (!ValidateInstallJournalTransition(nullptr, state,
            &transitionError)) {
        *error = std::move(transitionError);
        return false;
    }
    InstallJournalStateData entered = state;
    entered.sequence = 1U;
    entered.phase = InstallJournalPhase::SetupCopyEntered;
    InstallJournalStateData returned = entered;
    returned.sequence = 2U;
    returned.phase = InstallJournalPhase::SetupCopyReturned;
    InstallJournalStateData receipt = returned;
    receipt.sequence = 3U;
    receipt.phase = InstallJournalPhase::StageReceiptCaptured;
    if (!ValidateInstallJournalTransition(&state, entered, error) ||
        !ValidateInstallJournalTransition(&entered, returned, error) ||
        !ValidateInstallJournalTransition(&returned, receipt, error)) {
        return false;
    }
    InstallJournalStateData illegalSkip = state;
    illegalSkip.sequence = 1U;
    illegalSkip.phase = InstallJournalPhase::BrokerChildEntered;
    Error illegalTransition;
    if (ValidateInstallJournalTransition(
            &state, illegalSkip, &illegalTransition) ||
        illegalTransition.code == ERROR_SUCCESS) {
        return SetError(error, L"self-test-install-journal-phase-chain",
            ERROR_INVALID_DATA);
    }
    InstallJournalStateData sticky = returned;
    sticky.deadlineOverrun = true;
    InstallJournalStateData cleared = sticky;
    cleared.phase = InstallJournalPhase::StageReceiptCaptured;
    cleared.deadlineOverrun = false;
    Error stickyError;
    if (ValidateInstallJournalTransition(
            &sticky, cleared, &stickyError) ||
        stickyError.code == ERROR_SUCCESS) {
        return SetError(error, L"self-test-install-journal-sticky-chain",
            ERROR_INVALID_DATA);
    }
    for (InstallJournalPhase interrupted : {
            InstallJournalPhase::RollbackBindingEntered,
            InstallJournalPhase::RootRegistrationEntered,
            InstallJournalPhase::RootRegistrationReturned,
            InstallJournalPhase::DiInstallEntered,
            InstallJournalPhase::DiInstallReturned,
            InstallJournalPhase::SetupUninstallEntered,
            InstallJournalPhase::SetupUninstallReturned,
            InstallJournalPhase::RollbackBindingReturned}) {
        InstallJournalStateData interruptedState = state;
        interruptedState.direction = InstallJournalDirection::Rollback;
        interruptedState.rollbackAuthorized = true;
        interruptedState.phase = interrupted;
        InstallJournalStateData readmitted = interruptedState;
        readmitted.phase = InstallJournalPhase::RollbackBindingEntered;
        if (!ValidateInstallJournalTransition(
                &interruptedState, readmitted, error)) {
            return false;
        }
    }
    struct CanonicalBrokerProofCase {
        bool success;
        bool changed;
        const char* rollback;
        DWORD exitCode;
        bool rollbackAuthorized;
    };
    constexpr std::array<CanonicalBrokerProofCase, 5> brokerProofCases{{
        {true, false, "not-needed", 0U, false},
        {true, true, "not-needed", 0U, false},
        {false, false, "not-needed", 4U, true},
        {false, true, "succeeded", 1U, true},
        {false, true, "failed", 3U, false},
    }};
    for (const CanonicalBrokerProofCase& proof : brokerProofCases) {
        if (!BrokerProofFieldsAreCanonical(
                proof.success, proof.changed, proof.rollback,
                proof.exitCode, proof.rollbackAuthorized)) {
            return SetError(error,
                L"self-test-install-journal-broker-proof",
                ERROR_INVALID_DATA);
        }
    }
    if (BrokerProofFieldsAreCanonical(
            false, false, "not-needed", 1U, true)) {
        return SetError(error,
            L"self-test-install-journal-broker-proof",
            ERROR_INVALID_DATA);
    }
    std::string envelope = "{\"schema\":2,\"kind\":";
    AppendJsonAsciiString(&envelope, kInstallRecoveryKind);
    envelope.append(",\"payloadSha256\":");
    AppendJsonAsciiString(&envelope, digest);
    envelope.append(",\"payload\":");
    AppendJsonUtf8String(&envelope, payload);
    envelope.append("}\n");
    InstallJournalStateData parsedState;
    std::string parsedDigest;
    const std::filesystem::path modelRoot =
        std::filesystem::path(L"C:\\ProgramData\\VIIPER\\UdeCx\\Transactions\\active-v2");
    if (!ParseInstallJournalEnvelope(
            envelope, modelRoot, &parsedState, &parsedDigest, error) ||
        parsedDigest != digest || parsedState.sequence != 0U ||
        parsedState.previousDigest != kZeroSha256 ||
        !SameJournalPackageIdentity(
            parsedState.candidate, state.candidate)) {
        if (error->code == ERROR_SUCCESS) {
            SetError(error, L"self-test-install-journal-chain",
                ERROR_INVALID_DATA);
        }
        return false;
    }
    for (const CanonicalBrokerProofCase& proof : brokerProofCases) {
        InstallJournalStateData proofState = state;
        proofState.phase = InstallJournalPhase::BrokerChildSettled;
        proofState.brokerRequired = true;
        proofState.brokerEntered = true;
        proofState.brokerSettled = true;
        proofState.hasBrokerProof = true;
        proofState.brokerProofSuccess = proof.success;
        proofState.brokerProofChanged = proof.changed;
        proofState.brokerProofRollback = proof.rollback;
        proofState.brokerProofExitCode = proof.exitCode;
        proofState.brokerDriverRollbackAuthorized =
            proof.rollbackAuthorized;
        proofState.rollbackAuthorized = proof.rollbackAuthorized;
        proofState.direction = proof.rollbackAuthorized
            ? InstallJournalDirection::Rollback
            : InstallJournalDirection::Forward;
        std::string proofPayload;
        std::string proofDigest;
        if (!BuildInstallJournalPayload(proofState, &proofPayload, error) ||
            !Sha256Data(proofPayload, &proofDigest, error)) {
            return false;
        }
        std::string proofEnvelope = "{\"schema\":2,\"kind\":";
        AppendJsonAsciiString(&proofEnvelope, kInstallRecoveryKind);
        proofEnvelope.append(",\"payloadSha256\":");
        AppendJsonAsciiString(&proofEnvelope, proofDigest);
        proofEnvelope.append(",\"payload\":");
        AppendJsonUtf8String(&proofEnvelope, proofPayload);
        proofEnvelope.append("}\n");
        InstallJournalStateData proofRoundTrip;
        std::string observedProofDigest;
        if (!ParseInstallJournalEnvelope(
                proofEnvelope, modelRoot, &proofRoundTrip,
                &observedProofDigest, error) ||
            observedProofDigest != proofDigest ||
            !SameDurableBrokerProof(proofState, proofRoundTrip) ||
            proofRoundTrip.direction != proofState.direction ||
            proofRoundTrip.rollbackAuthorized !=
                proofState.rollbackAuthorized) {
            return SetError(error,
                L"self-test-install-journal-broker-proof-roundtrip",
                ERROR_INVALID_DATA);
        }
    }
    InstallJournalStateData durableReceipt = returned;
    durableReceipt.phase = InstallJournalPhase::StageReceiptCaptured;
    durableReceipt.hasPublishedCandidate = true;
    durableReceipt.publishedCandidate = durableReceipt.candidate;
    durableReceipt.publishedCandidate.publishedName = L"oem42.inf";
    durableReceipt.packageStagedHere = true;
    durableReceipt.expectedInventory.push_back(
        durableReceipt.publishedCandidate);
    if (!ValidateInstallJournalTransition(
            &returned, durableReceipt, error)) {
        return false;
    }
    InstallJournalStateData rootIntent = durableReceipt;
    rootIntent.phase =
        InstallJournalPhase::RootRegistrationIntentCaptured;
    rootIntent.hasRootRegistrationIntent = true;
    rootIntent.rootRegistrationInstanceId =
        L"ROOT\\VIIPERUDE\\0042";
    InstallJournalStateData rootRegistrationEntered = rootIntent;
    rootRegistrationEntered.phase =
        InstallJournalPhase::RootRegistrationEntered;
    rootRegistrationEntered.bindingMutationStarted = true;
    if (!ValidateInstallJournalTransition(
            &durableReceipt, rootIntent, error) ||
        !ValidateInstallJournalTransition(
            &rootIntent, rootRegistrationEntered, error)) {
        return false;
    }
    InstallJournalStateData changedRootIntent =
        rootRegistrationEntered;
    changedRootIntent.phase =
        InstallJournalPhase::RootRegistrationReturned;
    changedRootIntent.rootRegistrationInstanceId =
        L"ROOT\\VIIPERUDE\\0043";
    Error changedRootIntentError;
    if (ValidateInstallJournalTransition(
            &rootRegistrationEntered, changedRootIntent,
            &changedRootIntentError) ||
        changedRootIntentError.code == ERROR_SUCCESS) {
        return SetError(error,
            L"self-test-install-journal-root-intent-chain",
            ERROR_INVALID_DATA);
    }
    std::string rootIntentPayload;
    if (!BuildInstallJournalPayload(
            rootIntent, &rootIntentPayload, error) ||
        rootIntentPayload.find(
            "\"rootRegistrationInstanceId\":\"ROOT\\\\VIIPERUDE\\\\0042\"") ==
            std::string::npos) {
        return SetError(error,
            L"self-test-install-journal-root-intent-roundtrip",
            ERROR_INVALID_DATA);
    }

    PartialInstallRootRecoveryFacts partialRoot;
    partialRoot.priorEmpty = true;
    partialRoot.bindingMutationStarted = true;
    partialRoot.forwardRootRegistrationEntered = true;
    if (ClassifyPartialInstallRootRecovery(partialRoot) !=
            PartialInstallRootRecoveryAction::PriorEmpty) {
        return SetError(error,
            L"self-test-install-journal-partial-root-before-register",
            ERROR_INVALID_DATA);
    }
    partialRoot.relatedRootCount = 1U;
    partialRoot.exactHardwareId = true;
    partialRoot.exactClass = true;
    partialRoot.exactGeneratedInstance = true;
    partialRoot.present = true;
    partialRoot.serviceEmpty = true;
    partialRoot.publishedInfEmpty = true;
    partialRoot.driverVersionEmpty = true;
    if (ClassifyPartialInstallRootRecovery(partialRoot) !=
            PartialInstallRootRecoveryAction::RemoveUnboundExactRoot) {
        return SetError(error,
            L"self-test-install-journal-partial-root-after-register",
            ERROR_INVALID_DATA,
            L"failed/timed-out DIF_REGISTER exact receipt root was not cleanup-authorized");
    }
    partialRoot.serviceEmpty = false;
    partialRoot.publishedInfEmpty = false;
    partialRoot.driverVersionEmpty = false;
    partialRoot.exactCandidateService = true;
    partialRoot.exactCandidateInf = true;
    partialRoot.exactCandidateVersion = true;
    partialRoot.exactCandidateBytes = true;
    if (ClassifyPartialInstallRootRecovery(partialRoot) !=
            PartialInstallRootRecoveryAction::Manual) {
        return SetError(error,
            L"self-test-install-journal-partial-root-before-diinstall",
            ERROR_INVALID_DATA);
    }
    partialRoot.forwardDiInstallEntered = true;
    if (ClassifyPartialInstallRootRecovery(partialRoot) !=
            PartialInstallRootRecoveryAction::RemoveCandidateBoundExactRoot) {
        return SetError(error,
            L"self-test-install-journal-partial-root-after-diinstall",
            ERROR_INVALID_DATA);
    }
    PartialInstallRootRecoveryFacts pendingRoot = partialRoot;
    pendingRoot.partialRootRemovalEntered = true;
    pendingRoot.present = false;
    pendingRoot.pendingRemovalLifecycle = true;
    if (ClassifyPartialInstallRootRecovery(pendingRoot) !=
            PartialInstallRootRecoveryAction::PendingExactRootRemoval) {
        return SetError(error,
            L"self-test-install-journal-partial-root-pending-candidate",
            ERROR_INVALID_DATA);
    }
    pendingRoot.exactHardwareId = false;
    pendingRoot.hardwareIdAbsent = true;
    pendingRoot.serviceEmpty = true;
    pendingRoot.exactCandidateService = false;
    pendingRoot.driverVersionEmpty = true;
    pendingRoot.exactCandidateVersion = false;
    if (ClassifyPartialInstallRootRecovery(pendingRoot) !=
            PartialInstallRootRecoveryAction::PendingExactRootRemoval) {
        return SetError(error,
            L"self-test-install-journal-partial-root-pending-cleared",
            ERROR_INVALID_DATA);
    }
    pendingRoot.pendingRemovalLifecycle = false;
    if (ClassifyPartialInstallRootRecovery(pendingRoot) !=
            PartialInstallRootRecoveryAction::Manual) {
        return SetError(error,
            L"self-test-install-journal-partial-root-not-pending",
            ERROR_INVALID_DATA);
    }
    for (const auto mutateUnauthorized : {
            0, 1, 2, 3, 4, 5}) {
        PartialInstallRootRecoveryFacts unauthorized = partialRoot;
        switch (mutateUnauthorized) {
        case 0: unauthorized.relatedRootCount = 2U; break;
        case 1: unauthorized.exactHardwareId = false; break;
        case 2: unauthorized.exactClass = false; break;
        case 3: unauthorized.exactGeneratedInstance = false; break;
        case 4: unauthorized.present = false; break;
        case 5: unauthorized.forwardRootRegistrationEntered = false; break;
        }
        if (ClassifyPartialInstallRootRecovery(unauthorized) !=
                PartialInstallRootRecoveryAction::Manual) {
            return SetError(error,
                L"self-test-install-journal-partial-root-manual",
                ERROR_INVALID_DATA);
        }
    }
    if (InstallJournalRecoveryUsesStrictBindingRestore(
            rootRegistrationEntered)) {
        return SetError(error,
            L"self-test-install-journal-prior-empty-strict-restore",
            ERROR_INVALID_DATA);
    }

    InstallJournalStateData partialRollback = rootRegistrationEntered;
    partialRollback.sequence += 1U;
    partialRollback.phase = InstallJournalPhase::RollbackBindingEntered;
    partialRollback.direction = InstallJournalDirection::Rollback;
    partialRollback.rollbackAuthorized = true;
    InstallJournalStateData partialRemovalEntered = partialRollback;
    partialRemovalEntered.sequence += 1U;
    partialRemovalEntered.phase =
        InstallJournalPhase::PartialRootRemovalEntered;
    partialRemovalEntered.partialRootRemovalBootIdentifier =
        std::string(32, '1');
    partialRemovalEntered.partialRootRemovalBinding =
        InstallJournalStateData::PartialRootRemovalBinding::Unbound;
    InstallJournalStateData partialRemovalReturned =
        partialRemovalEntered;
    partialRemovalReturned.sequence += 1U;
    partialRemovalReturned.phase =
        InstallJournalPhase::PartialRootRemovalReturned;
    InstallJournalStateData partialRemovalPending =
        partialRemovalEntered;
    partialRemovalPending.sequence += 1U;
    partialRemovalPending.phase = InstallJournalPhase::
        PartialRootRemovalRebootPending;
    partialRemovalPending.rebootRequired = true;
    partialRemovalPending.callSucceeded = false;
    partialRemovalPending.callError = ERROR_SUCCESS_REBOOT_REQUIRED;
    if (!ValidateInstallJournalTransition(
            &rootRegistrationEntered, partialRollback, error) ||
        !ValidateInstallJournalTransition(
            &partialRollback, partialRemovalEntered, error) ||
        !ValidateInstallJournalTransition(
            &partialRemovalEntered, partialRemovalReturned, error) ||
        !ValidateInstallJournalTransition(
            &partialRemovalEntered, partialRemovalPending, error)) {
        return false;
    }
    InstallJournalStateData illegalRemovalShape =
        partialRemovalReturned;
    illegalRemovalShape.partialRootRemovalBinding =
        InstallJournalStateData::PartialRootRemovalBinding::Candidate;
    Error illegalRemovalShapeError;
    if (ValidateInstallJournalTransition(
            &partialRemovalEntered, illegalRemovalShape,
            &illegalRemovalShapeError) ||
        illegalRemovalShapeError.code == ERROR_SUCCESS) {
        return SetError(error,
            L"self-test-install-journal-partial-root-shape-chain",
            ERROR_INVALID_DATA);
    }
    std::string partialRemovalPayload;
    std::string partialRemovalDigest;
    if (!BuildInstallJournalPayload(
            partialRemovalEntered, &partialRemovalPayload, error) ||
        partialRemovalPayload.find(
            "\"partialRootRemovalBinding\":\"unbound\"") ==
            std::string::npos ||
        !Sha256Data(partialRemovalPayload,
            &partialRemovalDigest, error)) {
        return false;
    }
    InstallJournalStateData partialRemovalFreshReturn =
        partialRemovalEntered;
    partialRemovalFreshReturn.sequence += 1U;
    partialRemovalFreshReturn.phase =
        InstallJournalPhase::PartialRootRemovalReturned;
    partialRemovalFreshReturn.rebootRequired = true;
    partialRemovalFreshReturn.freshRebootRequired = true;
    partialRemovalFreshReturn.pendingRebootBootIdentifier =
        std::string(32, '2');
    InstallJournalStateData partialRemovalFreshPending =
        partialRemovalFreshReturn;
    partialRemovalFreshPending.sequence += 1U;
    partialRemovalFreshPending.phase = InstallJournalPhase::
        PartialRootRemovalRebootPending;
    partialRemovalFreshPending.freshRebootRequired = false;
    if (!ValidateInstallJournalTransition(
            &partialRemovalEntered, partialRemovalFreshReturn, error) ||
        !ValidateInstallJournalTransition(
            &partialRemovalFreshReturn,
            partialRemovalFreshPending, error)) {
        return false;
    }
    std::string partialFreshPayload;
    std::string partialFreshDigest;
    if (!BuildInstallJournalPayload(
            partialRemovalFreshReturn, &partialFreshPayload, error) ||
        !Sha256Data(partialFreshPayload,
            &partialFreshDigest, error)) {
        return false;
    }
    std::string partialFreshEnvelope = "{\"schema\":2,\"kind\":";
    AppendJsonAsciiString(&partialFreshEnvelope, kInstallRecoveryKind);
    partialFreshEnvelope.append(",\"payloadSha256\":");
    AppendJsonAsciiString(&partialFreshEnvelope, partialFreshDigest);
    partialFreshEnvelope.append(",\"payload\":");
    AppendJsonUtf8String(&partialFreshEnvelope, partialFreshPayload);
    partialFreshEnvelope.append("}\n");
    InstallJournalStateData partialFreshRoundTrip;
    std::string observedPartialFreshDigest;
    if (!ParseInstallJournalEnvelope(
            partialFreshEnvelope, modelRoot, &partialFreshRoundTrip,
            &observedPartialFreshDigest, error) ||
        observedPartialFreshDigest != partialFreshDigest ||
        partialFreshRoundTrip.partialRootRemovalBinding !=
            InstallJournalStateData::PartialRootRemovalBinding::Unbound ||
        partialFreshRoundTrip.partialRootRemovalBootIdentifier !=
            partialRemovalFreshReturn.partialRootRemovalBootIdentifier ||
        partialFreshRoundTrip.pendingRebootBootIdentifier !=
            partialRemovalFreshReturn.pendingRebootBootIdentifier ||
        !partialFreshRoundTrip.freshRebootRequired) {
        return SetError(error,
            L"self-test-install-journal-partial-root-roundtrip",
            ERROR_INVALID_DATA);
    }
    for (const InstallJournalStateData* interruptedPartial : {
            &partialRemovalReturned, &partialRemovalFreshPending}) {
        InstallJournalStateData readmittedPartial =
            *interruptedPartial;
        readmittedPartial.sequence += 1U;
        readmittedPartial.phase =
            InstallJournalPhase::RollbackBindingEntered;
        readmittedPartial.freshRebootRequired = false;
        if (!ValidateInstallJournalTransition(
                interruptedPartial, readmittedPartial, error)) {
            return false;
        }
    }
    struct PartialRemovalRecoveryCase {
        InstallJournalPhase phase;
        bool callSucceeded;
        bool freshRebootRequired;
        bool sameBoot;
        InstallJournalStateData::PartialRootRemovalBinding binding;
        PartialInstallRootRecoveryAction root;
        PartialRootRemovalRecoveryDisposition expected;
    };
    const std::array<PartialRemovalRecoveryCase, 13>
        partialRemovalCases{{
            {InstallJournalPhase::PartialRootRemovalEntered,
                true, false, true,
                InstallJournalStateData::PartialRootRemovalBinding::Unbound,
                PartialInstallRootRecoveryAction::RemoveUnboundExactRoot,
                PartialRootRemovalRecoveryDisposition::RetryRemoval},
            {InstallJournalPhase::PartialRootRemovalEntered,
                true, false, true,
                InstallJournalStateData::PartialRootRemovalBinding::Unbound,
                PartialInstallRootRecoveryAction::RemoveCandidateBoundExactRoot,
                PartialRootRemovalRecoveryDisposition::Manual},
            {InstallJournalPhase::PartialRootRemovalEntered,
                true, false, true,
                InstallJournalStateData::PartialRootRemovalBinding::Unbound,
                PartialInstallRootRecoveryAction::PriorEmpty,
                PartialRootRemovalRecoveryDisposition::RebootPending},
            {InstallJournalPhase::PartialRootRemovalEntered,
                true, false, false,
                InstallJournalStateData::PartialRootRemovalBinding::Unbound,
                PartialInstallRootRecoveryAction::PriorEmpty,
                PartialRootRemovalRecoveryDisposition::ContinueRollback},
            {InstallJournalPhase::PartialRootRemovalEntered,
                true, false, false,
                InstallJournalStateData::PartialRootRemovalBinding::Unbound,
                PartialInstallRootRecoveryAction::RemoveUnboundExactRoot,
                PartialRootRemovalRecoveryDisposition::Manual},
            {InstallJournalPhase::PartialRootRemovalReturned,
                true, false, true,
                InstallJournalStateData::PartialRootRemovalBinding::Unbound,
                PartialInstallRootRecoveryAction::PriorEmpty,
                PartialRootRemovalRecoveryDisposition::ContinueRollback},
            {InstallJournalPhase::PartialRootRemovalReturned,
                true, false, true,
                InstallJournalStateData::PartialRootRemovalBinding::Unbound,
                PartialInstallRootRecoveryAction::PendingExactRootRemoval,
                PartialRootRemovalRecoveryDisposition::Manual},
            {InstallJournalPhase::PartialRootRemovalReturned,
                true, true, true,
                InstallJournalStateData::PartialRootRemovalBinding::Unbound,
                PartialInstallRootRecoveryAction::PendingExactRootRemoval,
                PartialRootRemovalRecoveryDisposition::RebootPending},
            {InstallJournalPhase::PartialRootRemovalReturned,
                true, true, false,
                InstallJournalStateData::PartialRootRemovalBinding::Unbound,
                PartialInstallRootRecoveryAction::PriorEmpty,
                PartialRootRemovalRecoveryDisposition::ContinueRollback},
            {InstallJournalPhase::PartialRootRemovalReturned,
                false, false, true,
                InstallJournalStateData::PartialRootRemovalBinding::Unbound,
                PartialInstallRootRecoveryAction::PriorEmpty,
                PartialRootRemovalRecoveryDisposition::Manual},
            {InstallJournalPhase::PartialRootRemovalRebootPending,
                true, false, true,
                InstallJournalStateData::PartialRootRemovalBinding::Unbound,
                PartialInstallRootRecoveryAction::PendingExactRootRemoval,
                PartialRootRemovalRecoveryDisposition::RebootPending},
            {InstallJournalPhase::PartialRootRemovalRebootPending,
                true, false, false,
                InstallJournalStateData::PartialRootRemovalBinding::Unbound,
                PartialInstallRootRecoveryAction::PriorEmpty,
                PartialRootRemovalRecoveryDisposition::ContinueRollback},
            {InstallJournalPhase::PartialRootRemovalRebootPending,
                true, false, false,
                InstallJournalStateData::PartialRootRemovalBinding::Unbound,
                PartialInstallRootRecoveryAction::PendingExactRootRemoval,
                PartialRootRemovalRecoveryDisposition::Manual},
        }};
    for (const PartialRemovalRecoveryCase& test :
            partialRemovalCases) {
        if (ClassifyPartialRootRemovalJournalRecovery(
                test.phase, test.callSucceeded,
                test.freshRebootRequired, test.sameBoot,
                test.binding, test.root) != test.expected) {
            return SetError(error,
                L"self-test-install-journal-partial-root-recovery-matrix",
                ERROR_INVALID_DATA);
        }
    }

    std::vector<wchar_t> canonicalHardwareId(
        std::begin(kHardwareId), std::end(kHardwareId));
    canonicalHardwareId.push_back(L'\0');
    InstallRecoveryHardwareIdObservation hardwareObservation;
    std::vector<wchar_t> malformedHardwareId = canonicalHardwareId;
    malformedHardwareId.push_back(L'x');
    if (!ClassifyCanonicalInstallRecoveryHardwareIds(
            canonicalHardwareId, &hardwareObservation) ||
        !hardwareObservation.containsExpected ||
        !hardwareObservation.exact ||
        ClassifyCanonicalInstallRecoveryHardwareIds(
            malformedHardwareId, &hardwareObservation)) {
        return SetError(error,
            L"self-test-install-journal-raw-hardware-id",
            ERROR_INVALID_DATA);
    }
    const std::vector<wchar_t> canonicalService{
        L'V', L'i', L'i', L'p', L'e', L'r', L'U', L'd', L'e', L'\0'};
    std::vector<wchar_t> hiddenService = canonicalService;
    hiddenService.push_back(L'x');
    hiddenService.push_back(L'\0');
    const std::vector<wchar_t> hiddenAfterEmpty{
        L'\0', L'x', L'\0'};
    std::wstring decodedService;
    if (!DecodeCanonicalInstallRecoveryString(
            canonicalService, &decodedService) ||
        decodedService != kServiceName ||
        DecodeCanonicalInstallRecoveryString(
            hiddenService, &decodedService) ||
        DecodeCanonicalInstallRecoveryString(
            hiddenAfterEmpty, &decodedService)) {
        return SetError(error,
            L"self-test-install-journal-raw-string",
            ERROR_INVALID_DATA);
    }
    InstallJournalStateData ambiguousOwnership = durableReceipt;
    ambiguousOwnership.phase = InstallJournalPhase::RollbackBindingEntered;
    ambiguousOwnership.direction = InstallJournalDirection::Rollback;
    ambiguousOwnership.rollbackAuthorized = true;
    Error ownershipError;
    if (ValidateInstallJournalTransition(
            &returned, ambiguousOwnership, &ownershipError) ||
        ownershipError.code == ERROR_SUCCESS) {
        return SetError(error,
            L"self-test-install-journal-stage-ownership",
            ERROR_INVALID_DATA);
    }

    InstallJournalStateData rootAuthority = durableReceipt;
    rootAuthority.bindingMutationStarted = true;
    DeviceState priorDevice;
    priorDevice.instanceId = L"ROOT\\VIIPERUDE\\0000";
    priorDevice.present = true;
    priorDevice.service = kServiceName;
    priorDevice.publishedInf = L"oem41.inf";
    priorDevice.version.parts = {1, 2, 3, 3};
    priorDevice.package = rootAuthority.candidate;
    priorDevice.package.version = priorDevice.version;
    priorDevice.package.publishedName = priorDevice.publishedInf;
    rootAuthority.prior.devices = {priorDevice};
    Snapshot priorRootSnapshot;
    priorRootSnapshot.devices = {priorDevice};
    Snapshot candidateRootSnapshot = priorRootSnapshot;
    candidateRootSnapshot.devices[0].publishedInf =
        rootAuthority.publishedCandidate.publishedName;
    candidateRootSnapshot.devices[0].version = rootAuthority.candidate.version;
    candidateRootSnapshot.devices[0].package = rootAuthority.candidate;
    candidateRootSnapshot.devices[0].package.publishedName =
        rootAuthority.publishedCandidate.publishedName;
    Snapshot foreignRootSnapshot = candidateRootSnapshot;
    foreignRootSnapshot.devices[0].instanceId = L"ROOT\\VIIPERUDE\\9999";
    Snapshot extraRootSnapshot = candidateRootSnapshot;
    extraRootSnapshot.devices.push_back(candidateRootSnapshot.devices[0]);
    if (!RootSnapshotIsAuthorizedForInstallRollback(
            rootAuthority, priorRootSnapshot) ||
        !RootSnapshotIsAuthorizedForInstallRollback(
            rootAuthority, candidateRootSnapshot) ||
        RootSnapshotIsAuthorizedForInstallRollback(
            rootAuthority, foreignRootSnapshot) ||
        RootSnapshotIsAuthorizedForInstallRollback(
            rootAuthority, extraRootSnapshot)) {
        return SetError(error,
            L"self-test-install-journal-root-authority",
            ERROR_INVALID_DATA);
    }
    std::vector<PackageInfo> exactInventory{
        priorDevice.package, rootAuthority.publishedCandidate};
    std::vector<PackageInfo> extraInventory = exactInventory;
    PackageInfo externalPackage = rootAuthority.candidate;
    externalPackage.publishedName = L"oem99.inf";
    externalPackage.version.parts[3] += 5;
    extraInventory.push_back(externalPackage);
    std::vector<PackageInfo> conflictingInventory = exactInventory;
    conflictingInventory[1].sysSha256[0] = '0';
    if (SamePackageInventory(exactInventory, extraInventory) ||
        SamePackageInventory(exactInventory, conflictingInventory)) {
        return SetError(error,
            L"self-test-install-journal-inventory-authority",
            ERROR_INVALID_DATA);
    }
    InstallJournalStateData rebootCutpoint = state;
    rebootCutpoint.direction = InstallJournalDirection::Rollback;
    rebootCutpoint.rollbackAuthorized = true;
    rebootCutpoint.bindingMutationStarted = true;
    rebootCutpoint.phase = InstallJournalPhase::RollbackBindingReturned;
    rebootCutpoint.callSucceeded = true;
    rebootCutpoint.rebootRequired = true;
    rebootCutpoint.freshRebootRequired = true;
    rebootCutpoint.pendingRebootBootIdentifier =
        std::string(32, 'd');
    if (!InstallJournalNeedsRestoreRebootPending(rebootCutpoint, true) ||
        InstallJournalNeedsRestoreRebootPending(rebootCutpoint, false) ||
        !InstallJournalRollbackRetryRebootSeed(rebootCutpoint, true) ||
        InstallJournalRollbackRetryRebootSeed(rebootCutpoint, false) ||
        !InstallJournalHasAuthoritativeRollbackSettlement(
            rebootCutpoint)) {
        return SetError(error,
            L"self-test-install-journal-reboot-cutpoint",
            ERROR_INVALID_DATA);
    }
    InstallJournalStateData rollbackEntered = state;
    rollbackEntered.direction = InstallJournalDirection::Rollback;
    rollbackEntered.rollbackAuthorized = true;
    rollbackEntered.phase =
        InstallJournalPhase::RollbackBindingEntered;
    InstallJournalStateData firstBootReturn = rollbackEntered;
    firstBootReturn.phase =
        InstallJournalPhase::RollbackBindingReturned;
    firstBootReturn.rebootRequired = true;
    firstBootReturn.freshRebootRequired = true;
    firstBootReturn.pendingRebootBootIdentifier =
        std::string(32, 'd');
    InstallJournalStateData retryEntered = firstBootReturn;
    retryEntered.phase =
        InstallJournalPhase::RollbackBindingEntered;
    retryEntered.freshRebootRequired = false;
    InstallJournalStateData laterBootReturn = retryEntered;
    laterBootReturn.phase =
        InstallJournalPhase::RollbackBindingReturned;
    laterBootReturn.freshRebootRequired = true;
    laterBootReturn.pendingRebootBootIdentifier =
        std::string(32, 'e');
    InstallJournalStateData laterBootPending = laterBootReturn;
    laterBootPending.phase =
        InstallJournalPhase::RestoreRebootPending;
    laterBootPending.freshRebootRequired = false;
    if (!ValidateInstallJournalTransition(
            &rollbackEntered, firstBootReturn, error) ||
        !ValidateInstallJournalTransition(
            &firstBootReturn, retryEntered, error) ||
        !ValidateInstallJournalTransition(
            &retryEntered, laterBootReturn, error) ||
        !ValidateInstallJournalTransition(
            &laterBootReturn, laterBootPending, error) ||
        laterBootPending.pendingRebootBootIdentifier ==
            laterBootPending.bootIdentifier) {
        return SetError(error,
            L"self-test-install-journal-reboot-epoch",
            ERROR_INVALID_DATA,
            L"later-boot rollback NeedReboot did not replace and retain the pending epoch");
    }
    InstallJournalStateData illegalEpochChange = laterBootPending;
    illegalEpochChange.phase =
        InstallJournalPhase::ExactPriorRestored;
    illegalEpochChange.pendingRebootBootIdentifier =
        std::string(32, 'f');
    Error illegalEpochError;
    if (ValidateInstallJournalTransition(
            &laterBootPending, illegalEpochChange,
            &illegalEpochError) ||
        illegalEpochError.code == ERROR_SUCCESS) {
        return SetError(error,
            L"self-test-install-journal-reboot-epoch-chain",
            ERROR_INVALID_DATA);
    }
    std::string rebootPayload;
    std::string rebootDigest;
    if (!BuildInstallJournalPayload(
            laterBootReturn, &rebootPayload, error) ||
        !Sha256Data(rebootPayload, &rebootDigest, error)) {
        return false;
    }
    std::string rebootEnvelope = "{\"schema\":2,\"kind\":";
    AppendJsonAsciiString(&rebootEnvelope, kInstallRecoveryKind);
    rebootEnvelope.append(",\"payloadSha256\":");
    AppendJsonAsciiString(&rebootEnvelope, rebootDigest);
    rebootEnvelope.append(",\"payload\":");
    AppendJsonUtf8String(&rebootEnvelope, rebootPayload);
    rebootEnvelope.append("}\n");
    InstallJournalStateData rebootRoundTrip;
    std::string observedRebootDigest;
    if (!ParseInstallJournalEnvelope(
            rebootEnvelope, modelRoot, &rebootRoundTrip,
            &observedRebootDigest, error) ||
        rebootRoundTrip.pendingRebootBootIdentifier !=
            laterBootReturn.pendingRebootBootIdentifier ||
        !rebootRoundTrip.freshRebootRequired ||
        observedRebootDigest != rebootDigest) {
        return SetError(error,
            L"self-test-install-journal-reboot-epoch-roundtrip",
            ERROR_INVALID_DATA);
    }
    AbiCompatibilityProfile malformedProfile =
        kAbiCompatibilityProfiles[0];
    ++malformedProfile.statsSize;
    if (IsKnownAbiCompatibilityProfile(malformedProfile)) {
        return SetError(error,
            L"self-test-install-journal-abi-profile",
            ERROR_INVALID_DATA);
    }
    std::string truncated = envelope.substr(0, envelope.size() - 3U);
    Error truncatedError;
    if (ParseInstallJournalEnvelope(
            truncated, modelRoot, &parsedState, &parsedDigest,
            &truncatedError) || truncatedError.code == ERROR_SUCCESS) {
        return SetError(error, L"self-test-install-journal-truncated-chain",
            ERROR_INVALID_DATA);
    }
    std::string tampered = envelope;
    const size_t payloadOffset = tampered.find("previousSha256");
    if (payloadOffset == std::string::npos) {
        return SetError(error, L"self-test-install-journal-chain",
            ERROR_INVALID_DATA);
    }
    tampered[payloadOffset] = 'P';
    Error tamperedError;
    if (ParseInstallJournalEnvelope(
            tampered, modelRoot, &parsedState, &parsedDigest,
            &tamperedError) || tamperedError.code == ERROR_SUCCESS) {
        return SetError(error, L"self-test-install-journal-hash-chain",
            ERROR_INVALID_DATA);
    }

    struct RecoveryCase {
        InstallJournalPhase phase;
        bool chainValid;
        bool securityValid;
        bool sameBoot;
        bool priorValid;
        bool forwardValid;
        bool brokerEntered;
        bool brokerSettled;
        bool brokerSucceeded;
        InstallJournalRecoveryModelAction expected;
    };
    const std::array<RecoveryCase, 8> cases{{
        {InstallJournalPhase::Prepared, true, true, true, true,
            false, false, false, false,
            InstallJournalRecoveryModelAction::RetirePrior},
        {InstallJournalPhase::DriverValidated, true, true, true, false,
            true, false, false, false,
            InstallJournalRecoveryModelAction::RetireForward},
        {InstallJournalPhase::SetupCopyReturned, true, true, true, false,
            false, false, false, false,
            InstallJournalRecoveryModelAction::RollbackPrior},
        {InstallJournalPhase::BrokerChildEntered, true, true, true, false,
            true, true, false, false,
            InstallJournalRecoveryModelAction::Manual},
        {InstallJournalPhase::ForwardRebootPending, true, true, true, false,
            false, false, false, false,
            InstallJournalRecoveryModelAction::RebootPending},
        {InstallJournalPhase::RestoreRebootPending, true, true, false, true,
            false, false, false, false,
            InstallJournalRecoveryModelAction::RetirePrior},
        {InstallJournalPhase::ForwardValidated, false, true, false, false,
            true, false, false, false,
            InstallJournalRecoveryModelAction::Manual},
        {InstallJournalPhase::ForwardValidated, true, false, false, false,
            true, false, false, false,
            InstallJournalRecoveryModelAction::Manual},
    }};
    for (const RecoveryCase& test : cases) {
        if (ClassifyInstallJournalRecoveryModel(
                test.phase, test.chainValid, test.securityValid,
                test.sameBoot, test.priorValid, test.forwardValid,
                test.brokerEntered, test.brokerSettled,
                test.brokerSucceeded) != test.expected) {
            return SetError(error, L"self-test-install-journal-recovery-model",
                ERROR_INVALID_DATA);
        }
    }
    return true;
}

Outcome SelfTest();

Outcome Status() {
    Outcome outcome;
    const Outcome deterministic = SelfTest();
    if (!deterministic.success) {
        return deterministic;
    }
    Snapshot snapshot;
    if (!CaptureSnapshot(&snapshot, &outcome.error)) {
        return outcome;
    }
    if (snapshot.devices.size() > 1) {
        SetError(&outcome.error, L"status-topology", ERROR_DUPLICATE_SERVICE_NAME);
        return outcome;
    }
    outcome.success = true;
    outcome.exitCode = ExitCode::Success;
    std::wcout << L"devices=" << snapshot.devices.size()
               << L" packages=" << snapshot.packages.size();
    if (snapshot.devices.size() == 1) {
        std::wcout << L" present=" << (snapshot.devices[0].present ? 1 : 0)
                   << L" started=" << (snapshot.devices[0].started ? 1 : 0)
                   << L" version=" << VersionToString(snapshot.devices[0].version)
                   << L" publishedInf=" << snapshot.devices[0].publishedInf
                   << L" problem=" << snapshot.devices[0].problem;
    }
    std::wcout << L"\n";
    return outcome;
}

Outcome SelfTest() {
    Outcome outcome;
    if (!RunInstallJournalModelSelfTest(&outcome.error)) {
        return outcome;
    }
    InstallOptions brokerCommandOptions;
    brokerCommandOptions.brokerExecutable = LR"(C:\Program Files\VIIPER\viiper.exe)";
    brokerCommandOptions.brokerToken = LR"(C:\ProgramData\VIIPER\package.token)";
    brokerCommandOptions.brokerTokenSha256 = std::string(64, 'a');
    brokerCommandOptions.brokerSha256 = std::string(64, 'b');
    brokerCommandOptions.targetUserSid = L"S-1-5-21-1-2-3-1001";
    brokerCommandOptions.transactionDeadlineUnixMs = 123456789;
    const std::wstring brokerCommandLine =
        BuildBrokerCommitCommandLine(brokerCommandOptions);
    if (brokerCommandLine.find(L" --expected-token-sha-256 ") == std::wstring::npos ||
        brokerCommandLine.find(L" --expected-broker-sha-256 ") == std::wstring::npos) {
        SetError(&outcome.error, L"self-test-broker-command", ERROR_INVALID_DATA,
            L"nested broker command does not match the compiled Kong CLI contract");
        return outcome;
    }
    Version one{};
    Version two{};
    if (!ParseVersion(L"1.2.3.4", &one) || !ParseVersion(L"1.2.4.0", &two) ||
        !(one < two) || ParseVersion(L"1.2.3", nullptr) ||
        ParseVersion(L"1.2.3.70000", nullptr)) {
        SetError(&outcome.error, L"self-test-version", ERROR_INVALID_DATA);
        return outcome;
    }
    PackageInfo candidate;
    candidate.version = two;
    candidate.infSha256 = "candidate-inf";
    candidate.sysSha256 = "candidate-sys";
    candidate.catSha256 = "candidate-cat";
    CandidateDisposition disposition = CandidateDisposition::Exact;
    bool downgrade = true;
    Error classificationError;
    if (!ClassifyCandidatePackage(
            candidate, {}, std::nullopt, &disposition, &downgrade, &classificationError) ||
        disposition != CandidateDisposition::InstallRequired || downgrade) {
        SetError(&outcome.error, L"self-test-package-classification", ERROR_INVALID_DATA,
            L"an absent candidate was not classified as an install");
        return outcome;
    }
    PackageInfo exact = candidate;
    classificationError = {};
    if (!ClassifyCandidatePackage(
            candidate, {exact}, std::nullopt, &disposition, &downgrade, &classificationError) ||
        disposition != CandidateDisposition::Exact || downgrade) {
        SetError(&outcome.error, L"self-test-package-classification", ERROR_INVALID_DATA,
            L"an exact same-version candidate was not classified as repair-only");
        return outcome;
    }
    if (!RequiresDriverMutation(CandidateDisposition::InstallRequired, false) ||
        !RequiresDriverMutation(CandidateDisposition::Exact, false) ||
        RequiresDriverMutation(CandidateDisposition::Exact, true) ||
        !RequiresPristineRuntimeProof(
            CandidateDisposition::InstallRequired, false, true, true) ||
        !RequiresPristineRuntimeProof(
            CandidateDisposition::Exact, false, true, true) ||
        RequiresPristineRuntimeProof(
            CandidateDisposition::Exact, true, true, true) ||
        RequiresPristineRuntimeProof(
            CandidateDisposition::InstallRequired, false, false, false) ||
        RequiresPristineRuntimeProof(
            CandidateDisposition::Exact, false, true, false)) {
        SetError(&outcome.error, L"self-test-pristine-runtime-decision", ERROR_INVALID_DATA,
            L"pristine-runtime admission does not cover exactly the running-root mutation boundary");
        return outcome;
    }
    PackageInfo conflict = candidate;
    conflict.infSha256 = "different-inf";
    classificationError = {};
    if (ClassifyCandidatePackage(
            candidate, {conflict}, std::nullopt,
            &disposition, &downgrade, &classificationError) ||
        classificationError.phase != L"version-policy") {
        SetError(&outcome.error, L"self-test-package-classification", ERROR_INVALID_DATA,
            L"same-version content replacement was not rejected");
        return outcome;
    }
    conflict = candidate;
    conflict.sysSha256 = "different-sys";
    classificationError = {};
    if (ClassifyCandidatePackage(
            candidate, {conflict}, std::nullopt,
            &disposition, &downgrade, &classificationError) ||
        classificationError.phase != L"version-policy") {
        SetError(&outcome.error, L"self-test-package-classification", ERROR_INVALID_DATA,
            L"same-version SYS replacement was not rejected");
        return outcome;
    }
    conflict = candidate;
    conflict.catSha256 = "different-cat";
    classificationError = {};
    if (ClassifyCandidatePackage(
            candidate, {conflict}, std::nullopt,
            &disposition, &downgrade, &classificationError) ||
        classificationError.phase != L"version-policy") {
        SetError(&outcome.error, L"self-test-package-classification", ERROR_INVALID_DATA,
            L"same-version catalog replacement was not rejected");
        return outcome;
    }
    PackageInfo newer = candidate;
    newer.version.parts[3] += 1;
    classificationError = {};
    if (ClassifyCandidatePackage(
            candidate, {newer}, std::nullopt,
            &disposition, &downgrade, &classificationError) ||
        classificationError.phase != L"version-policy") {
        SetError(&outcome.error, L"self-test-package-classification", ERROR_INVALID_DATA,
            L"implicit downgrade was not rejected");
        return outcome;
    }
    classificationError = {};
    if (!ClassifyCandidatePackage(
            candidate, {newer}, newer.version,
            &disposition, &downgrade, &classificationError) ||
        disposition != CandidateDisposition::InstallRequired || !downgrade) {
        SetError(&outcome.error, L"self-test-package-classification", ERROR_INVALID_DATA,
            L"exact controlled-downgrade guard was not honored");
        return outcome;
    }
    Version wrongDowngradeGuard = newer.version;
    ++wrongDowngradeGuard.parts[3];
    classificationError = {};
    if (ClassifyCandidatePackage(
            candidate, {newer}, wrongDowngradeGuard,
            &disposition, &downgrade, &classificationError) ||
        classificationError.phase != L"version-policy") {
        SetError(&outcome.error, L"self-test-package-classification", ERROR_INVALID_DATA,
            L"incorrect controlled-downgrade guard was accepted");
        return outcome;
    }
    std::string buildIdentity;
    if (!DeriveDriverBuildIdentity(
            "0123456789abcdef0123456789abcdef01234567",
            &buildIdentity, &outcome.error) ||
        buildIdentity !=
            "6796b0cf22a80984b283662a50a3b364c46218e37766a2e1880b38851b65d9ad") {
        if (outcome.error.code == ERROR_SUCCESS) {
            SetError(&outcome.error, L"self-test-build-identity", ERROR_INVALID_DATA);
        }
        return outcome;
    }
    const std::array<AbiHealthPurpose, 4> abiPurposes{
        AbiHealthPurpose::ExactCandidate,
        AbiHealthPurpose::PristineUpgrade,
        AbiHealthPurpose::PristineRecheck,
        AbiHealthPurpose::RollbackHealth,
    };
    const std::array<DWORD, 2> retryCodes{
        ERROR_REVISION_MISMATCH, ERROR_INVALID_PARAMETER,
    };
    const std::array<std::wstring, 2> retryPhases{
        L"abi-negotiate", L"abi-negotiate-result",
    };
    for (const AbiHealthPurpose purpose : abiPurposes) {
        for (const DWORD code : retryCodes) {
            for (const std::wstring& phase : retryPhases) {
                Error retryError;
                retryError.code = code;
                retryError.phase = phase;
                const bool expected = purpose == AbiHealthPurpose::PristineUpgrade;
                if (IsAbiRetryEligible(purpose, nullptr, retryError) != expected ||
                    IsAbiRetryEligible(purpose, &buildIdentity, retryError)) {
                    SetError(&outcome.error, L"self-test-abi-retry", ERROR_INVALID_DATA,
                        L"ABI retry escaped the strict pristine-upgrade mismatch boundary");
                    return outcome;
                }
            }
        }
    }
    Error unrelatedRetryError;
    unrelatedRetryError.code = ERROR_ACCESS_DENIED;
    unrelatedRetryError.phase = L"abi-negotiate";
    Error wrongPhaseRetryError;
    wrongPhaseRetryError.code = ERROR_REVISION_MISMATCH;
    wrongPhaseRetryError.phase = L"abi-negotiate-timeout";
    if (IsAbiRetryEligible(
            AbiHealthPurpose::PristineUpgrade, nullptr, unrelatedRetryError) ||
        IsAbiRetryEligible(
            AbiHealthPurpose::PristineUpgrade, nullptr, wrongPhaseRetryError)) {
        SetError(&outcome.error, L"self-test-abi-retry", ERROR_INVALID_DATA,
            L"ABI retry accepted a non-version negotiation failure");
        return outcome;
    }

    const auto makeNegotiationResponse = [](const AbiCompatibilityProfile& profile) {
        VIIPER_UDE_NEGOTIATE_RESPONSE response{};
        response.Header.Magic = VIIPER_UDE_MAGIC;
        response.Header.Major = VIIPER_UDE_ABI_MAJOR;
        response.Header.Minor = profile.minor;
        response.Header.Size = sizeof(response);
        response.ClientNonce = 0x123456789abcdef0ULL;
        response.DriverNonce = 1;
        response.Capabilities = profile.capabilities;
        response.MaxDevices = VIIPER_UDE_MAX_DEVICES;
        response.MaxDescriptorBytes = VIIPER_UDE_MAX_DESCRIPTOR_BYTES;
        response.MaxTransferBytes = VIIPER_UDE_MAX_TRANSFER_BYTES;
        response.MaxIsoPackets = VIIPER_UDE_MAX_ISO_PACKETS;
        response.MaxPendingOperations = VIIPER_UDE_MAX_PENDING_OPERATIONS;
        return response;
    };
    const auto negotiationValidationIsExhaustive =
        [&](const AbiCompatibilityProfile& profile) {
            const VIIPER_UDE_NEGOTIATE_RESPONSE response =
                makeNegotiationResponse(profile);
            const auto rejects = [&](auto mutate) {
                VIIPER_UDE_NEGOTIATE_RESPONSE changed = response;
                mutate(changed);
                return !AbiNegotiationResponseMatchesProfile(
                    changed, sizeof(changed), response.ClientNonce, profile);
            };
            return AbiNegotiationResponseMatchesProfile(
                       response, sizeof(response), response.ClientNonce, profile) &&
                !AbiNegotiationResponseMatchesProfile(
                    response, sizeof(response) - 1, response.ClientNonce, profile) &&
                rejects([](auto& value) { value.Header.Magic ^= 1; }) &&
                rejects([](auto& value) { ++value.Header.Major; }) &&
                rejects([](auto& value) { ++value.Header.Minor; }) &&
                rejects([](auto& value) { ++value.Header.Size; }) &&
                rejects([](auto& value) { value.Header.Flags = 1; }) &&
                rejects([](auto& value) { ++value.ClientNonce; }) &&
                rejects([](auto& value) { value.DriverNonce = 0; }) &&
                rejects([](auto& value) { ++value.Capabilities; }) &&
                rejects([](auto& value) { ++value.MaxDevices; }) &&
                rejects([](auto& value) { ++value.MaxDescriptorBytes; }) &&
                rejects([](auto& value) { ++value.MaxTransferBytes; }) &&
                rejects([](auto& value) { ++value.MaxIsoPackets; }) &&
                rejects([](auto& value) { ++value.MaxPendingOperations; });
        };
    const auto statsValidationIsExhaustive =
        [](const AbiCompatibilityProfile& profile) {
            VIIPER_UDE_STATS stats{};
            stats.Header.Magic = VIIPER_UDE_MAGIC;
            stats.Header.Major = VIIPER_UDE_ABI_MAJOR;
            stats.Header.Minor = profile.minor;
            stats.Header.Size = profile.statsSize;
            const auto rejects = [&](auto mutate) {
                VIIPER_UDE_STATS changed = stats;
                mutate(changed);
                return !StatsRecordMatchesProfile(
                    changed, profile.statsSize, profile);
            };
            const bool commonFieldsExact = StatsRecordMatchesProfile(
                    stats, profile.statsSize, profile) &&
                !StatsRecordMatchesProfile(stats, profile.statsSize - 1, profile) &&
                rejects([](auto& value) { value.Header.Magic ^= 1; }) &&
                rejects([](auto& value) { ++value.Header.Major; }) &&
                rejects([](auto& value) { ++value.Header.Minor; }) &&
                rejects([](auto& value) { ++value.Header.Size; }) &&
                rejects([](auto& value) { value.Header.Flags = 1; });
            stats.ReservedPorts = VIIPER_UDE_MAX_DEVICES + 1;
            stats.Reserved = 1;
            const bool reservedRangeExact = profile.hasReservedPortFields
                ? !StatsRecordMatchesProfile(stats, profile.statsSize, profile)
                : StatsRecordMatchesProfile(stats, profile.statsSize, profile);
            return commonFieldsExact && reservedRangeExact;
        };
    for (const AbiCompatibilityProfile& profile : kAbiCompatibilityProfiles) {
        if (!negotiationValidationIsExhaustive(profile) ||
            !statsValidationIsExhaustive(profile)) {
            SetError(&outcome.error, L"self-test-abi-profile-validation",
                ERROR_INVALID_DATA,
                L"an ABI profile response or statistics field escaped exact validation");
            return outcome;
        }
    }

    VIIPER_UDE_STATS pristineStats{};
    const auto rejectsNonzeroRuntimeCounter = [](auto member) {
        VIIPER_UDE_STATS stats{};
        stats.*member = 1;
        return !RuntimeStatsArePristine(stats, kAbiCompatibilityProfiles[0]);
    };
    if (!RuntimeStatsArePristine(pristineStats, kAbiCompatibilityProfiles[0]) ||
        !rejectsNonzeroRuntimeCounter(&VIIPER_UDE_STATS::OperationsDequeued) ||
        !rejectsNonzeroRuntimeCounter(&VIIPER_UDE_STATS::OperationsCompleted) ||
        !rejectsNonzeroRuntimeCounter(&VIIPER_UDE_STATS::OperationsCancelled) ||
        !rejectsNonzeroRuntimeCounter(&VIIPER_UDE_STATS::OperationsPurged) ||
        !rejectsNonzeroRuntimeCounter(&VIIPER_UDE_STATS::LateCompletions) ||
        !rejectsNonzeroRuntimeCounter(&VIIPER_UDE_STATS::InvalidMessages) ||
        !rejectsNonzeroRuntimeCounter(&VIIPER_UDE_STATS::QueueExhaustions) ||
        !rejectsNonzeroRuntimeCounter(&VIIPER_UDE_STATS::IsoPackets) ||
        !rejectsNonzeroRuntimeCounter(&VIIPER_UDE_STATS::BytesToDevice) ||
        !rejectsNonzeroRuntimeCounter(&VIIPER_UDE_STATS::BytesFromDevice) ||
        !rejectsNonzeroRuntimeCounter(&VIIPER_UDE_STATS::NotificationEvents) ||
        !rejectsNonzeroRuntimeCounter(&VIIPER_UDE_STATS::NotificationEventOverflows) ||
        !rejectsNonzeroRuntimeCounter(&VIIPER_UDE_STATS::ActiveDevices) ||
        !rejectsNonzeroRuntimeCounter(&VIIPER_UDE_STATS::PendingOperations) ||
        !rejectsNonzeroRuntimeCounter(&VIIPER_UDE_STATS::WaitingDequeues) ||
        !rejectsNonzeroRuntimeCounter(&VIIPER_UDE_STATS::CleanupRetries) ||
        !rejectsNonzeroRuntimeCounter(&VIIPER_UDE_STATS::InputReportsSubmitted) ||
        !rejectsNonzeroRuntimeCounter(&VIIPER_UDE_STATS::InputReportsCompleted) ||
        !rejectsNonzeroRuntimeCounter(&VIIPER_UDE_STATS::ReservedPorts)) {
        SetError(&outcome.error, L"self-test-pristine-runtime-stats", ERROR_INVALID_DATA,
            L"a nonzero runtime counter escaped the pre-mutation reboot boundary");
        return outcome;
    }
    pristineStats.ReservedPorts = 1;
    if (!RuntimeStatsArePristine(pristineStats, kAbiCompatibilityProfiles[1]) ||
        !RuntimeStatsArePristine(pristineStats, kAbiCompatibilityProfiles[2])) {
        SetError(&outcome.error, L"self-test-pristine-runtime-stats", ERROR_INVALID_DATA,
            L"a legacy ABI inspected a counter outside its returned statistics record");
        return outcome;
    }
    JsonValue value;
    std::string message;
    if (!JsonParser(R"({"schema":1,"files":[]})").Parse(&value, &message) ||
        JsonParser(R"({"schema":1,"schema":1})").Parse(&value, &message) ||
        JsonParser(R"({"schema":1.0})").Parse(&value, &message) ||
        !IsSafePublishedInfName(L"oem42.inf") || IsSafePublishedInfName(L"..\\oem42.inf")) {
        SetError(&outcome.error, L"self-test-contract", ERROR_INVALID_DATA);
        return outcome;
    }
    if (!IsOwnedGeneratedRootInstanceId(L"ROOT\\VIIPERUDE\\0000") ||
        !IsOwnedGeneratedRootInstanceId(L"root\\usb\\0042") ||
        IsOwnedGeneratedRootInstanceId(L"ROOT\\VIIPER\\UDE\\0000") ||
        IsOwnedGeneratedRootInstanceId(L"ROOT\\USB\\42") ||
        IsOwnedGeneratedRootInstanceId(L"ROOT\\USB\\00A0")) {
        SetError(&outcome.error, L"self-test-root-instance-id", ERROR_INVALID_DATA,
            L"generated root instance namespace validation is not exact");
        return outcome;
    }
    PackageInfo priorPackage;
    priorPackage.publishedName = L"OEM7.INF";
    PackageInfo preservedPackage;
    preservedPackage.publishedName = L"oem7.inf";
    PackageInfo newPackage;
    newPackage.publishedName = L"oem9.inf";
    newPackage.sysSha256 = "new-sys";
    PackageInfo changedPackage = preservedPackage;
    changedPackage.sysSha256 = "changed";
    if (!SamePackageInventory({priorPackage}, {preservedPackage}) ||
        SamePackageInventory({priorPackage}, {preservedPackage, newPackage}) ||
        SamePackageInventory({priorPackage}, {changedPackage}) ||
        !ContainsExactPackage({priorPackage}, preservedPackage) ||
        ContainsExactPackage({priorPackage}, newPackage)) {
        SetError(&outcome.error, L"self-test-rollback-inventory", ERROR_INVALID_DATA,
            L"rollback package inventory comparison is not exact and name-bound");
        return outcome;
    }
    DeviceState capturedRoot;
    capturedRoot.instanceId = L"ROOT\\VIIPERUDE\\0000";
    capturedRoot.present = true;
    capturedRoot.started = true;
    capturedRoot.service = kServiceName;
    capturedRoot.publishedInf = L"oem7.inf";
    capturedRoot.version = one;
    capturedRoot.package = priorPackage;
    capturedRoot.package.infSha256 = "prior-inf";
    capturedRoot.package.sysSha256 = "prior-sys";
    capturedRoot.package.catSha256 = "prior-cat";
    Snapshot capturedRootSnapshot;
    capturedRootSnapshot.devices.push_back(capturedRoot);
    Snapshot observedRootSnapshot = capturedRootSnapshot;
    observedRootSnapshot.packages.push_back(newPackage);
    if (!SameCapturedRootState(capturedRootSnapshot, observedRootSnapshot)) {
        SetError(&outcome.error, L"self-test-stage-root-invariance", ERROR_INVALID_DATA,
            L"add-only package publication changed the captured root comparison");
        return outcome;
    }
    observedRootSnapshot = capturedRootSnapshot;
    DeviceState concurrentRoot = capturedRoot;
    concurrentRoot.instanceId = L"ROOT\\VIIPERUDE\\0001";
    observedRootSnapshot.devices.push_back(std::move(concurrentRoot));
    if (SameCapturedRootState(capturedRootSnapshot, observedRootSnapshot)) {
        SetError(&outcome.error, L"self-test-stage-root-invariance", ERROR_INVALID_DATA,
            L"a concurrently registered second root escaped global topology verification");
        return outcome;
    }
    observedRootSnapshot = capturedRootSnapshot;
    observedRootSnapshot.devices[0].started = false;
    if (SameCapturedRootState(capturedRootSnapshot, observedRootSnapshot)) {
        SetError(&outcome.error, L"self-test-stage-root-invariance", ERROR_INVALID_DATA,
            L"a root lifecycle change escaped post-stage verification");
        return outcome;
    }
    observedRootSnapshot = capturedRootSnapshot;
    observedRootSnapshot.devices[0].publishedInf = L"oem9.inf";
    if (SameCapturedRootState(capturedRootSnapshot, observedRootSnapshot)) {
        SetError(&outcome.error, L"self-test-stage-root-invariance", ERROR_INVALID_DATA,
            L"a root package rebind escaped post-stage verification");
        return outcome;
    }
    if (!SameCapturedRootState(Snapshot{}, Snapshot{}) ||
        SameCapturedRootState(Snapshot{}, capturedRootSnapshot)) {
        SetError(&outcome.error, L"self-test-stage-root-invariance", ERROR_INVALID_DATA,
            L"absent-root post-stage verification is not exact");
        return outcome;
    }
    DeviceState stoppedRoot = capturedRoot;
    stoppedRoot.started = false;
    stoppedRoot.problem = CM_PROB_DISABLED;
    DeviceState restoredStoppedRoot = stoppedRoot;
    if (!RollbackLifecycleStateMatches(stoppedRoot, restoredStoppedRoot)) {
        SetError(&outcome.error, L"self-test-rollback-lifecycle", ERROR_INVALID_DATA,
            L"an exact stopped/problem rollback state was rejected");
        return outcome;
    }
    restoredStoppedRoot.started = true;
    restoredStoppedRoot.problem = 0;
    if (RollbackLifecycleStateMatches(stoppedRoot, restoredStoppedRoot)) {
        SetError(&outcome.error, L"self-test-rollback-lifecycle", ERROR_INVALID_DATA,
            L"rollback accepted a captured stopped root that was unexpectedly started");
        return outcome;
    }
    restoredStoppedRoot = stoppedRoot;
    ++restoredStoppedRoot.problem;
    if (RollbackLifecycleStateMatches(stoppedRoot, restoredStoppedRoot) ||
        !RollbackLifecycleStateMatches(capturedRoot, capturedRoot)) {
        SetError(&outcome.error, L"self-test-rollback-lifecycle", ERROR_INVALID_DATA,
            L"rollback lifecycle comparison is not exact for stopped or running roots");
        return outcome;
    }
    const std::filesystem::path recoveryRoot =
        LR"(C:\Windows\Temp\VIIPER-UDE-rollback-self-test)";
    if (!IsSafeRecoveryRelativePath(
            std::filesystem::path(L"0") / L"ViiperUde.inf") ||
        IsSafeRecoveryRelativePath(std::filesystem::path(L"..") / L"escape") ||
        IsSafeRecoveryRelativePath(
            std::filesystem::path(L"0") / L".." / L"escape") ||
        IsSafeRecoveryRelativePath(std::filesystem::path(LR"(C:\escape)")) ||
        IsSafeRecoveryRelativePath(
            std::filesystem::path(L"0") / L"ViiperUde.inf:stream")) {
        SetError(&outcome.error, L"self-test-recovery-path", ERROR_INVALID_DATA,
            L"rollback recovery relative-path validation is not fail-closed");
        return outcome;
    }
    PackageInfo recoveryPackage;
    recoveryPackage.infPath = LR"(C:\Windows\INF\oem42.inf)";
    recoveryPackage.publishedName = L"oem42.inf";
    recoveryPackage.version.parts = {0, 1, 0, 6};
    recoveryPackage.infSha256 = std::string(64, 'A');
    recoveryPackage.sysSha256 = std::string(64, 'B');
    recoveryPackage.catSha256 = std::string(64, 'C');
    DeviceState recoveryDevice;
    recoveryDevice.instanceId = LR"(ROOT\VIIPERUDE\0000)";
    recoveryDevice.present = true;
    recoveryDevice.started = true;
    recoveryDevice.service = kServiceName;
    recoveryDevice.publishedInf = recoveryPackage.publishedName;
    recoveryDevice.version = recoveryPackage.version;
    recoveryDevice.package = recoveryPackage;
    Snapshot recoverySnapshot;
    recoverySnapshot.devices.push_back(std::move(recoveryDevice));
    recoverySnapshot.packages.push_back(recoveryPackage);
    std::vector<PackageBackup> recoveryBackups;
    recoveryBackups.push_back(PackageBackup{
        recoveryPackage,
        recoveryRoot / L"0",
        recoveryRoot / L"0" / L"ViiperUde.inf",
        {}});
    std::string firstRecoveryRecord;
    std::string secondRecoveryRecord;
    Error recoveryRecordError;
    JsonValue recoveryRecordValue;
    std::string recoveryRecordParseError;
    if (!BuildRemoveRecoveryRecord(
            recoverySnapshot, recoveryBackups, recoveryRoot,
            &firstRecoveryRecord, &recoveryRecordError) ||
        !BuildRemoveRecoveryRecord(
            recoverySnapshot, recoveryBackups, recoveryRoot,
            &secondRecoveryRecord, &recoveryRecordError) ||
        firstRecoveryRecord != secondRecoveryRecord ||
        !JsonParser(firstRecoveryRecord).Parse(
            &recoveryRecordValue, &recoveryRecordParseError) ||
        firstRecoveryRecord.find("\"automaticRestore\":false") == std::string::npos ||
        firstRecoveryRecord.find(
            "\"requiredValidation\":[\"inf-signature\"") == std::string::npos ||
        firstRecoveryRecord.find("\"state\":\"prepared-remove-transaction\"") ==
            std::string::npos ||
        firstRecoveryRecord.find("\"packageIndex\":0") == std::string::npos ||
        firstRecoveryRecord.find("\"backupInf\":\"0/ViiperUde.inf\"") ==
            std::string::npos ||
        firstRecoveryRecord.find("C:") != std::string::npos) {
        if (recoveryRecordError.code == ERROR_SUCCESS) {
            SetError(&recoveryRecordError, L"self-test-recovery-record", ERROR_INVALID_DATA,
                L"rollback recovery record is not canonical and relative-path bound");
        }
        outcome.error = std::move(recoveryRecordError);
        return outcome;
    }
    if (!IsSafeTargetUserSid(L"S-1-5-21-1-2-3-1001") ||
        IsSafeTargetUserSid(L"S-1-5-21-bad") ||
        QuoteWindowsArgument(LR"(C:\Program Files\VIIPER\viiper.exe)") !=
            LR"("C:\Program Files\VIIPER\viiper.exe")" ||
        QuoteWindowsArgument(LR"(value\"quoted)") != LR"("value\\\"quoted")") {
        SetError(&outcome.error, L"self-test-broker-command", ERROR_INVALID_DATA);
        return outcome;
    }
    const std::string brokerSuccess =
        "result=success operation=native-package-broker-commit changed=0 "
        "rollback=not-needed exitCode=0\n";
    const std::string brokerPreflightFailure =
        "result=error operation=native-package-broker-commit changed=0 "
        "rollback=not-needed exitCode=4\n";
    BrokerCommitProof brokerProof;
    Error brokerProofError;
    if (!ParseBrokerCommitProof(
            brokerSuccess, ERROR_SUCCESS, &brokerProof, &brokerProofError) ||
        !brokerProof.success || brokerProof.changed ||
        brokerProof.driverRollbackAuthorized ||
        brokerProof.rollback != "not-needed") {
        SetError(&outcome.error, L"self-test-broker-proof", ERROR_INVALID_DATA,
            L"valid broker success proof was rejected or misclassified");
        return outcome;
    }
    brokerProof = {};
    brokerProofError = {};
    if (!ParseBrokerCommitProof(
            brokerPreflightFailure, 4, &brokerProof, &brokerProofError) ||
        brokerProof.success || brokerProof.changed ||
        !brokerProof.driverRollbackAuthorized) {
        SetError(&outcome.error, L"self-test-broker-proof", ERROR_INVALID_DATA,
            L"pre-mutation broker failure proof was rejected or misclassified");
        return outcome;
    }
    brokerProof = {};
    brokerProofError = {};
    const std::wstring expectedBrokerDiagnostic =
        L"outer native package transaction mutex is not held";
    if (!ParseBrokerCommitProof(
            brokerPreflightFailure + std::string(kBrokerDiagnosticPrefix) +
                "outer native package transaction mutex is not held\n",
            4, &brokerProof, &brokerProofError) ||
        brokerProof.success || brokerProof.changed ||
        !brokerProof.driverRollbackAuthorized ||
        brokerProof.diagnostic != expectedBrokerDiagnostic) {
        SetError(&outcome.error, L"self-test-broker-diagnostic", ERROR_INVALID_DATA,
            L"exact nested broker error diagnostic was rejected or changed proof authority");
        return outcome;
    }
    Error mappedBrokerError;
    if (SetBrokerCommitFailure(brokerProof, &mappedBrokerError) ||
        mappedBrokerError.code != ERROR_INSTALL_FAILURE ||
        !mappedBrokerError.nestedExitCode || *mappedBrokerError.nestedExitCode != 4 ||
        mappedBrokerError.phase != L"broker-preflight" ||
        mappedBrokerError.message.find(expectedBrokerDiagnostic) == std::wstring::npos) {
        SetError(&outcome.error, L"self-test-broker-diagnostic", ERROR_INVALID_DATA,
            L"nested broker application exit was not separated from the outer Win32 failure");
        return outcome;
    }
    brokerProof = {};
    brokerProofError = {};
    const std::string unsafeBrokerDiagnostic =
        brokerPreflightFailure + std::string(kBrokerDiagnosticPrefix) +
        "left\tmiddle\x01"
        "right\x7f"
        "\xe2\x80\xae"
        "tail \"quoted\" \\ path\n";
    if (!ParseBrokerCommitProof(
            unsafeBrokerDiagnostic, 4, &brokerProof, &brokerProofError) ||
        brokerProof.diagnostic != L"left?middle?right??tail \"quoted\" \\ path" ||
        brokerProof.success || brokerProof.changed ||
        !brokerProof.driverRollbackAuthorized) {
        SetError(&outcome.error, L"self-test-broker-diagnostic", ERROR_INVALID_DATA,
            L"nested broker diagnostic controls were not sanitized without changing proof authority");
        return outcome;
    }
    brokerProof = {};
    brokerProofError = {};
    const std::string oversizedBrokerDiagnostic(
        kMaximumBrokerDiagnosticCharacters + 32U, 'x');
    if (!ParseBrokerCommitProof(
            brokerPreflightFailure + std::string(kBrokerDiagnosticPrefix) +
                oversizedBrokerDiagnostic + "\n",
            4, &brokerProof, &brokerProofError) ||
        brokerProof.diagnostic.size() != kMaximumBrokerDiagnosticCharacters ||
        !brokerProof.diagnostic.ends_with(L"...") ||
        brokerProof.success || brokerProof.changed ||
        !brokerProof.driverRollbackAuthorized) {
        SetError(&outcome.error, L"self-test-broker-diagnostic", ERROR_INVALID_DATA,
            L"nested broker diagnostic was not deterministically capped");
        return outcome;
    }
    brokerProof = {};
    brokerProofError = {};
    if (!ParseBrokerCommitProof(
            brokerPreflightFailure + std::string(kBrokerDiagnosticPrefix) +
                std::string("\xc3\x28", 2) + "\n",
            4, &brokerProof, &brokerProofError) ||
        !brokerProof.diagnostic.empty() || brokerProof.success || brokerProof.changed ||
        !brokerProof.driverRollbackAuthorized) {
        SetError(&outcome.error, L"self-test-broker-diagnostic", ERROR_INVALID_DATA,
            L"malformed UTF-8 diagnostic changed canonical broker proof authority");
        return outcome;
    }
    brokerProof = {};
    brokerProofError = {};
    if (!ParseBrokerCommitProof(
            brokerPreflightFailure + std::string(kBrokerDiagnosticPrefix) + "first\n" +
                std::string(kBrokerDiagnosticPrefix) + "second\n",
            4, &brokerProof, &brokerProofError) ||
        !brokerProof.diagnostic.empty() || brokerProof.success || brokerProof.changed ||
        !brokerProof.driverRollbackAuthorized) {
        SetError(&outcome.error, L"self-test-broker-diagnostic", ERROR_INVALID_DATA,
            L"ambiguous diagnostics changed canonical broker proof authority");
        return outcome;
    }
    brokerProof = {};
    brokerProofError = {};
    if (!ParseBrokerCommitProof(
            brokerSuccess + std::string(kBrokerDiagnosticPrefix) + "contradiction\n",
            ERROR_SUCCESS, &brokerProof, &brokerProofError) ||
        !brokerProof.success || brokerProof.changed ||
        brokerProof.driverRollbackAuthorized || !brokerProof.diagnostic.empty()) {
        SetError(&outcome.error, L"self-test-broker-diagnostic", ERROR_INVALID_DATA,
            L"diagnostic text overrode a canonical broker success proof");
        return outcome;
    }
    brokerProof = {};
    brokerProofError = {};
    if (!ParseBrokerCommitProof(
            brokerPreflightFailure + std::string(kBrokerDiagnosticPrefix) + "unterminated",
            4, &brokerProof, &brokerProofError) ||
        !brokerProof.diagnostic.empty() || brokerProof.success || brokerProof.changed ||
        !brokerProof.driverRollbackAuthorized) {
        SetError(&outcome.error, L"self-test-broker-diagnostic", ERROR_INVALID_DATA,
            L"unterminated diagnostic changed canonical broker proof authority");
        return outcome;
    }
    brokerProof = {};
    brokerProofError = {};
    if (!ParseBrokerCommitProof(
            "result=error operation=native-package-broker-commit changed=1 "
            "rollback=succeeded exitCode=1\n",
            1, &brokerProof, &brokerProofError) ||
        brokerProof.success || !brokerProof.changed ||
        !brokerProof.driverRollbackAuthorized) {
        SetError(&outcome.error, L"self-test-broker-proof", ERROR_INVALID_DATA,
            L"settled broker rollback proof was rejected or misclassified");
        return outcome;
    }
    brokerProof = {};
    brokerProofError = {};
    if (!ParseBrokerCommitProof(
            "result=error operation=native-package-broker-commit changed=1 "
            "rollback=failed exitCode=3\n",
            3, &brokerProof, &brokerProofError) ||
        brokerProof.success || !brokerProof.changed ||
        brokerProof.driverRollbackAuthorized) {
        SetError(&outcome.error, L"self-test-broker-proof", ERROR_INVALID_DATA,
            L"indeterminate broker rollback proof was not kept fail-closed");
        return outcome;
    }
    brokerProof = {};
    brokerProofError = {};
    if (ParseBrokerCommitProof(
            brokerSuccess + brokerSuccess, ERROR_SUCCESS,
            &brokerProof, &brokerProofError) ||
        brokerProofError.phase != L"broker-proof") {
        SetError(&outcome.error, L"self-test-broker-proof", ERROR_INVALID_DATA,
            L"duplicate broker outcomes were not rejected");
        return outcome;
    }
    brokerProof = {};
    brokerProofError = {};
    if (ParseBrokerCommitProof(
            "result=error exitCode=04 rollback=not-needed changed=0 "
            "operation=native-package-broker-commit\n",
            4, &brokerProof, &brokerProofError) ||
        brokerProofError.phase != L"broker-proof") {
        SetError(&outcome.error, L"self-test-broker-proof", ERROR_INVALID_DATA,
            L"noncanonical broker outcome was accepted");
        return outcome;
    }
    brokerProof = {};
    brokerProofError = {};
    if (ParseBrokerCommitProof(
            "result=error operation=native-package-broker-commit changed=0 "
            "rollback=not-needed exitCode=4",
            4, &brokerProof, &brokerProofError) ||
        brokerProofError.phase != L"broker-proof") {
        SetError(&outcome.error, L"self-test-broker-proof", ERROR_INVALID_DATA,
            L"unterminated broker outcome was accepted");
        return outcome;
    }
    if (!IsProductionHardwareVerificationUsage({kHardwareVerificationOid}) ||
        IsProductionHardwareVerificationUsage(
            {kHardwareVerificationOid, kAttestationVerificationOid}) ||
        IsProductionHardwareVerificationUsage({kAttestationVerificationOid}) ||
        IsProductionHardwareVerificationUsage({})) {
        SetError(&outcome.error, L"self-test-production-eku", ERROR_INVALID_DATA);
        return outcome;
    }
    outcome.success = true;
    outcome.exitCode = ExitCode::Success;
    return outcome;
}

bool ParseInheritedEventHandle(
    const wchar_t* value,
    const wchar_t* name,
    HANDLE* handle,
    Error* error) {
    const std::wstring text = value == nullptr ? L"" : value;
    if (text.empty() || text.size() > 20 ||
        !std::all_of(text.begin(), text.end(), [](wchar_t character) {
            return character >= L'0' && character <= L'9';
        })) {
        return SetError(error, L"arguments", ERROR_INVALID_PARAMETER,
            std::wstring(name) + L" handle must contain only decimal digits");
    }
    const wchar_t* begin = text.data();
    wchar_t* end = nullptr;
    errno = 0;
    const unsigned long long parsed = std::wcstoull(begin, &end, 10);
    if (errno == ERANGE || end == begin || end != begin + text.size() || parsed == 0 ||
        parsed > static_cast<unsigned long long>(std::numeric_limits<uintptr_t>::max())) {
        return SetError(error, L"arguments", ERROR_INVALID_HANDLE,
            std::wstring(name) + L" handle is outside the process handle range");
    }
    const HANDLE candidate = reinterpret_cast<HANDLE>(static_cast<uintptr_t>(parsed));
    if (candidate == INVALID_HANDLE_VALUE) {
        return SetError(error, L"arguments", ERROR_INVALID_HANDLE,
            std::wstring(name) + L" handle is invalid");
    }
    DWORD flags = 0;
    if (!GetHandleInformation(candidate, &flags) || (flags & HANDLE_FLAG_INHERIT) == 0) {
        return SetError(error, L"arguments", ERROR_INVALID_HANDLE,
            std::wstring(name) + L" handle was not explicitly inherited");
    }
    const DWORD wait = WaitForSingleObject(candidate, 0);
    if (wait != WAIT_TIMEOUT) {
        return SetError(error, L"arguments", ERROR_INVALID_HANDLE,
            std::wstring(name) + L" event must begin nonsignaled and waitable");
    }
    *handle = candidate;
    return true;
}

bool ParseInstallOptions(int argc, wchar_t** argv, InstallOptions* options, Error* error) {
    if (argc < 8) {
        return SetError(error, L"arguments", ERROR_INVALID_PARAMETER);
    }
    options->infPath = argv[2];
    bool manifestSeen = false;
    bool manifestHashSeen = false;
    bool revisionSeen = false;
    bool modeSeen = false;
    bool infHashSeen = false;
    bool sysHashSeen = false;
    bool catHashSeen = false;
    bool brokerSeen = false;
    bool brokerHashSeen = false;
    bool brokerTokenSeen = false;
    bool brokerTokenHashSeen = false;
    bool targetUserSeen = false;
    bool transactionDeadlineSeen = false;
    bool brokerQuiesceRequestSeen = false;
    bool brokerQuiesceReadySeen = false;
    bool brokerQuiesceAbortSeen = false;
    bool brokerHandoffSeen = false;
    for (int index = 3; index < argc; ++index) {
        const std::wstring argument = argv[index];
        if (_wcsicmp(argument.c_str(), L"--manifest") == 0 && index + 1 < argc && !manifestSeen) {
            options->manifestPath = argv[++index];
            manifestSeen = true;
        } else if (_wcsicmp(argument.c_str(), L"--manifest-sha256") == 0 &&
            index + 1 < argc && !manifestHashSeen) {
            const std::wstring wide = argv[++index];
            options->manifestSha256.clear();
            options->manifestSha256.reserve(wide.size());
            for (const wchar_t value : wide) {
                if (value > 0x7f) {
                    return SetError(error, L"arguments", ERROR_INVALID_PARAMETER,
                        L"manifest SHA-256 must contain ASCII hexadecimal characters");
                }
                options->manifestSha256.push_back(static_cast<char>(value));
            }
            if (options->manifestSha256.size() != 64 ||
                !std::all_of(options->manifestSha256.begin(), options->manifestSha256.end(),
                    [](unsigned char value) { return std::isxdigit(value) != 0; })) {
                return SetError(error, L"arguments", ERROR_INVALID_PARAMETER,
                    L"manifest SHA-256 must contain exactly 64 hexadecimal characters");
            }
            manifestHashSeen = true;
        } else if (_wcsicmp(argument.c_str(), L"--source-revision") == 0 &&
            index + 1 < argc && !revisionSeen) {
            const std::wstring wide = argv[++index];
            options->sourceRevision.clear();
            options->sourceRevision.reserve(wide.size());
            for (const wchar_t value : wide) {
                if (value > 0x7f) {
                    return SetError(error, L"arguments", ERROR_INVALID_PARAMETER,
                        L"source revision must contain ASCII hexadecimal characters");
                }
                options->sourceRevision.push_back(static_cast<char>(value));
            }
            if (!IsHexRevision(options->sourceRevision)) {
                return SetError(error, L"arguments", ERROR_INVALID_PARAMETER,
                    L"source revision must contain exactly 40 or 64 hexadecimal characters");
            }
            revisionSeen = true;
        } else if (_wcsicmp(argument.c_str(), L"--validation-mode") == 0 &&
            index + 1 < argc && !modeSeen) {
            const std::wstring mode = argv[++index];
            if (_wcsicmp(mode.c_str(), L"production") == 0) {
                options->production = true;
                options->localTest = false;
            } else if (_wcsicmp(mode.c_str(), L"controlled-test") == 0) {
                options->production = false;
                options->localTest = false;
            } else if (_wcsicmp(mode.c_str(), L"local-test") == 0) {
                options->production = false;
                options->localTest = true;
            } else {
                return SetError(error, L"arguments", ERROR_INVALID_PARAMETER,
                    L"validation mode must be production, controlled-test, or local-test");
            }
            modeSeen = true;
        } else if (_wcsicmp(argument.c_str(), L"--expected-inf-sha256") == 0 &&
            index + 1 < argc && !infHashSeen) {
            if (!CopySha256Argument(
                    argv[++index], L"runtime INF", &options->expectedInfSha256, error)) {
                return false;
            }
            infHashSeen = true;
        } else if (_wcsicmp(argument.c_str(), L"--expected-sys-sha256") == 0 &&
            index + 1 < argc && !sysHashSeen) {
            if (!CopySha256Argument(
                    argv[++index], L"runtime SYS", &options->expectedSysSha256, error)) {
                return false;
            }
            sysHashSeen = true;
        } else if (_wcsicmp(argument.c_str(), L"--expected-cat-sha256") == 0 &&
            index + 1 < argc && !catHashSeen) {
            if (!CopySha256Argument(
                    argv[++index], L"runtime CAT", &options->expectedCatSha256, error)) {
                return false;
            }
            catHashSeen = true;
        } else if (_wcsicmp(argument.c_str(), L"--allow-controlled-downgrade") == 0 &&
            index + 1 < argc && !options->expectedDowngradeFrom) {
            Version expected{};
            if (!ParseVersion(argv[++index], &expected)) {
                return SetError(error, L"arguments", ERROR_INVALID_PARAMETER,
                    L"controlled downgrade requires the exact installed four-part version");
            }
            options->expectedDowngradeFrom = expected;
        } else if (_wcsicmp(argument.c_str(), L"--broker-executable") == 0 &&
            index + 1 < argc && !brokerSeen) {
            options->brokerExecutable = argv[++index];
            brokerSeen = true;
        } else if (_wcsicmp(argument.c_str(), L"--broker-sha256") == 0 &&
            index + 1 < argc && !brokerHashSeen) {
            const std::wstring wide = argv[++index];
            options->brokerSha256.clear();
            options->brokerSha256.reserve(wide.size());
            for (const wchar_t value : wide) {
                if (value > 0x7f) {
                    return SetError(error, L"arguments", ERROR_INVALID_PARAMETER,
                        L"broker SHA-256 must contain ASCII hexadecimal characters");
                }
                options->brokerSha256.push_back(static_cast<char>(value));
            }
            if (options->brokerSha256.size() != 64 ||
                !std::all_of(options->brokerSha256.begin(), options->brokerSha256.end(),
                    [](unsigned char value) { return std::isxdigit(value) != 0; })) {
                return SetError(error, L"arguments", ERROR_INVALID_PARAMETER,
                    L"broker SHA-256 must contain exactly 64 hexadecimal characters");
            }
            brokerHashSeen = true;
        } else if (_wcsicmp(argument.c_str(), L"--target-user-sid") == 0 &&
            index + 1 < argc && !targetUserSeen) {
            options->targetUserSid = argv[++index];
            targetUserSeen = true;
        } else if (_wcsicmp(argument.c_str(), L"--broker-token") == 0 &&
            index + 1 < argc && !brokerTokenSeen) {
            options->brokerToken = argv[++index];
            brokerTokenSeen = true;
        } else if (_wcsicmp(argument.c_str(), L"--broker-token-sha256") == 0 &&
            index + 1 < argc && !brokerTokenHashSeen) {
            const std::wstring wide = argv[++index];
            options->brokerTokenSha256.clear();
            options->brokerTokenSha256.reserve(wide.size());
            for (const wchar_t value : wide) {
                if (value > 0x7f) {
                    return SetError(error, L"arguments", ERROR_INVALID_PARAMETER,
                        L"broker token SHA-256 must contain ASCII hexadecimal characters");
                }
                options->brokerTokenSha256.push_back(static_cast<char>(value));
            }
            if (options->brokerTokenSha256.size() != 64 ||
                !std::all_of(options->brokerTokenSha256.begin(), options->brokerTokenSha256.end(),
                    [](unsigned char value) { return std::isxdigit(value) != 0; })) {
                return SetError(error, L"arguments", ERROR_INVALID_PARAMETER,
                    L"broker token SHA-256 must contain exactly 64 hexadecimal characters");
            }
            brokerTokenHashSeen = true;
        } else if (_wcsicmp(argument.c_str(), L"--transaction-deadline-unix-ms") == 0 &&
            index + 1 < argc && !transactionDeadlineSeen) {
            const std::wstring value = argv[++index];
            if (value.empty() || value.size() > 20 ||
                !std::all_of(value.begin(), value.end(), [](wchar_t character) {
                    return character >= L'0' && character <= L'9';
                })) {
                return SetError(error, L"arguments", ERROR_INVALID_PARAMETER,
                    L"transaction deadline must contain only Unix-millisecond digits");
            }
            const wchar_t* begin = value.data();
            wchar_t* end = nullptr;
            errno = 0;
            const unsigned long long parsed = std::wcstoull(begin, &end, 10);
            if (errno == ERANGE || end == begin || end != begin + value.size() || parsed == 0) {
                return SetError(error, L"arguments", ERROR_INVALID_PARAMETER,
                    L"transaction deadline must be positive Unix milliseconds");
            }
            options->transactionDeadlineUnixMs = static_cast<uint64_t>(parsed);
            transactionDeadlineSeen = true;
        } else if (_wcsicmp(argument.c_str(), L"--broker-quiesce-request-handle") == 0 &&
            index + 1 < argc && !brokerQuiesceRequestSeen) {
            if (!ParseInheritedEventHandle(argv[++index], L"broker quiesce request",
                    &options->brokerQuiesceRequest, error)) {
                return false;
            }
            brokerQuiesceRequestSeen = true;
        } else if (_wcsicmp(argument.c_str(), L"--broker-quiesce-ready-handle") == 0 &&
            index + 1 < argc && !brokerQuiesceReadySeen) {
            if (!ParseInheritedEventHandle(argv[++index], L"broker quiesce ready",
                    &options->brokerQuiesceReady, error)) {
                return false;
            }
            brokerQuiesceReadySeen = true;
        } else if (_wcsicmp(argument.c_str(), L"--broker-quiesce-abort-handle") == 0 &&
            index + 1 < argc && !brokerQuiesceAbortSeen) {
            if (!ParseInheritedEventHandle(argv[++index], L"broker quiesce abort",
                    &options->brokerQuiesceAbort, error)) {
                return false;
            }
            brokerQuiesceAbortSeen = true;
        } else if (_wcsicmp(argument.c_str(), L"--broker-handoff-handle") == 0 &&
            index + 1 < argc && !brokerHandoffSeen) {
            if (!ParseInheritedEventHandle(argv[++index], L"broker handoff",
                    &options->brokerHandoff, error)) {
                return false;
            }
            brokerHandoffSeen = true;
        } else {
            return SetError(error, L"arguments", ERROR_INVALID_PARAMETER,
                L"unknown, duplicate, or incomplete install option");
        }
    }
    if (!manifestSeen || !manifestHashSeen || !revisionSeen || !modeSeen ||
        !infHashSeen || !sysHashSeen || !catHashSeen ||
        !transactionDeadlineSeen ||
        brokerSeen != targetUserSeen || brokerSeen != brokerHashSeen ||
        brokerSeen != brokerTokenSeen || brokerSeen != brokerTokenHashSeen ||
        brokerSeen != brokerQuiesceRequestSeen || brokerSeen != brokerQuiesceReadySeen ||
        brokerSeen != brokerQuiesceAbortSeen || brokerSeen != brokerHandoffSeen) {
        return SetError(error, L"arguments", ERROR_INVALID_PARAMETER,
            L"manifest, its installer hash, source revision, validation mode, and exact INF/SYS/CAT hashes are required; broker executable, hashes, protected token, target SID, and inherited quiescence/handoff events must be supplied together");
    }
    if (brokerSeen) {
        const std::set<uintptr_t> coordinationHandles{
            reinterpret_cast<uintptr_t>(options->brokerQuiesceRequest),
            reinterpret_cast<uintptr_t>(options->brokerQuiesceReady),
            reinterpret_cast<uintptr_t>(options->brokerQuiesceAbort),
            reinterpret_cast<uintptr_t>(options->brokerHandoff),
        };
        if (coordinationHandles.size() != 4) {
            return SetError(error, L"arguments", ERROR_INVALID_HANDLE,
                L"broker quiescence and handoff require four distinct inherited events");
        }
    }
    return true;
}

bool ParseRemoveOptions(int argc, wchar_t** argv, RemoveOptions* options, Error* error) {
    if (argc == 2) {
        options->transactionDeadlineUnixMs =
            CurrentUnixMilliseconds() + kMaximumTransactionDurationMs;
        return true;
    }
    if (argc != 4 ||
        _wcsicmp(argv[2], L"--transaction-deadline-unix-ms") != 0) {
        return SetError(error, L"arguments", ERROR_INVALID_PARAMETER,
            L"remove accepts only an optional absolute transaction deadline");
    }
    const std::wstring value = argv[3];
    if (value.empty() || value.size() > 20 ||
        !std::all_of(value.begin(), value.end(), [](wchar_t character) {
            return character >= L'0' && character <= L'9';
        })) {
        return SetError(error, L"arguments", ERROR_INVALID_PARAMETER,
            L"transaction deadline must contain only Unix-millisecond digits");
    }
    const wchar_t* begin = value.data();
    wchar_t* end = nullptr;
    errno = 0;
    const unsigned long long parsed = std::wcstoull(begin, &end, 10);
    if (errno == ERANGE || end == begin || end != begin + value.size() || parsed == 0) {
        return SetError(error, L"arguments", ERROR_INVALID_PARAMETER,
            L"transaction deadline must be positive Unix milliseconds");
    }
    options->transactionDeadlineUnixMs = static_cast<uint64_t>(parsed);
    return true;
}

void Usage() {
    std::wcerr
        << L"usage:\n"
        << L"  ViiperUdeCtl.exe install <ViiperUde.inf> --manifest <submission.json> --manifest-sha256 <64 hex> "
           L"--source-revision <40-or-64 hex> --validation-mode <production|controlled-test|local-test> "
           L"--expected-inf-sha256 <64 hex> --expected-sys-sha256 <64 hex> "
           L"--expected-cat-sha256 <64 hex> "
           L"--transaction-deadline-unix-ms <positive integer> "
           L"[--allow-controlled-downgrade <exact-installed-version>] "
            L"--broker-executable <managed-viiper.exe> --broker-sha256 <64 hex> "
            L"--broker-token <protected-token> --broker-token-sha256 <64 hex> "
            L"--target-user-sid <SID> "
            L"--broker-quiesce-request-handle <inherited-handle> "
            L"--broker-quiesce-ready-handle <inherited-handle> "
            L"--broker-quiesce-abort-handle <inherited-handle> "
            L"--broker-handoff-handle <inherited-handle>\n"
        << L"  ViiperUdeCtl.exe verify <ViiperUde.inf> --manifest <submission.json> --manifest-sha256 <64 hex> "
           L"--source-revision <40-or-64 hex> --validation-mode <production|controlled-test|local-test> "
           L"--expected-inf-sha256 <64 hex> --expected-sys-sha256 <64 hex> "
           L"--expected-cat-sha256 <64 hex> "
           L"--transaction-deadline-unix-ms <positive integer>\n"
        << L"  ViiperUdeCtl.exe remove [--transaction-deadline-unix-ms <positive integer>]\n"
        << L"  ViiperUdeCtl.exe recover [--transaction-deadline-unix-ms <positive integer>]\n"
        << L"  ViiperUdeCtl.exe status\n"
        << L"  ViiperUdeCtl.exe self-test\n";
}

} // namespace

int RunViiperUdeCtl(int argc, wchar_t** argv) {
    ClearActiveRecoveryEvidence();
    gTransactionMutationStarted = false;
    if (argc >= 3 &&
        (_wcsicmp(argv[1], L"install") == 0 || _wcsicmp(argv[1], L"verify") == 0)) {
        InstallOptions options;
        Error argumentError;
        if (!ParseInstallOptions(argc, argv, &options, &argumentError)) {
            Usage();
            Outcome outcome;
            outcome.error = std::move(argumentError);
            outcome.exitCode = ExitCode::Usage;
            EmitOutcome(argv[1], outcome);
            return static_cast<int>(outcome.exitCode);
        }
        if (_wcsicmp(argv[1], L"install") == 0 &&
            (options.production || options.localTest) &&
            options.brokerExecutable.empty()) {
            Outcome outcome;
            SetError(&outcome.error, L"broker-required", ERROR_INVALID_PARAMETER,
                L"production and local-test driver installation require the authenticated broker transaction");
            outcome.exitCode = ExitCode::PreflightRejected;
            EmitOutcome(argv[1], outcome);
            return static_cast<int>(outcome.exitCode);
        }
        Outcome outcome =
            _wcsicmp(argv[1], L"verify") == 0 ? Verify(options) : Install(options);
        EmitOutcome(argv[1], outcome);
        return static_cast<int>(outcome.exitCode);
    }
    if (argc >= 2 && _wcsicmp(argv[1], L"remove") == 0) {
        RemoveOptions options;
        Error argumentError;
        if (!ParseRemoveOptions(argc, argv, &options, &argumentError)) {
            Usage();
            Outcome outcome;
            outcome.error = std::move(argumentError);
            outcome.exitCode = ExitCode::Usage;
            EmitOutcome(L"remove", outcome);
            return static_cast<int>(outcome.exitCode);
        }
        Outcome outcome = Remove(options);
        EmitOutcome(L"remove", outcome);
        return static_cast<int>(outcome.exitCode);
    }
    if (argc >= 2 && _wcsicmp(argv[1], L"recover") == 0) {
        RemoveOptions options;
        Error argumentError;
        if (!ParseRemoveOptions(argc, argv, &options, &argumentError)) {
            Usage();
            Outcome outcome;
            outcome.error = std::move(argumentError);
            outcome.exitCode = ExitCode::Usage;
            EmitOutcome(L"recover", outcome);
            return static_cast<int>(outcome.exitCode);
        }
        Outcome outcome = Recover(options.transactionDeadlineUnixMs);
        EmitOutcome(L"recover", outcome);
        return static_cast<int>(outcome.exitCode);
    }
    if (argc == 2 && _wcsicmp(argv[1], L"status") == 0) {
        Outcome outcome = Status();
        EmitOutcome(L"status", outcome);
        return static_cast<int>(outcome.exitCode);
    }
    if (argc == 2 && _wcsicmp(argv[1], L"self-test") == 0) {
        Outcome outcome = SelfTest();
        EmitOutcome(L"self-test", outcome);
        return static_cast<int>(outcome.exitCode);
    }
    Usage();
    Outcome outcome;
    SetError(&outcome.error, L"arguments", ERROR_INVALID_PARAMETER);
    outcome.exitCode = ExitCode::Usage;
    EmitOutcome(L"unknown", outcome);
    return static_cast<int>(outcome.exitCode);
}

const wchar_t* ExceptionOperation(int argc, wchar_t** argv) noexcept {
    if (argc < 2 || argv == nullptr || argv[1] == nullptr) {
        return L"unknown";
    }
    for (const wchar_t* operation :
            {L"install", L"verify", L"remove", L"recover", L"status", L"self-test"}) {
        if (_wcsicmp(argv[1], operation) == 0) {
            return operation;
        }
    }
    return L"unknown";
}

int wmain(int argc, wchar_t** argv) {
    try {
        return RunViiperUdeCtl(argc, argv);
    } catch (...) {
        const wchar_t* operation = ExceptionOperation(argc, argv);
        const bool changed = gTransactionMutationStarted;
        const ExitCode exitCode = changed
            ? ExitCode::RollbackFailed : ExitCode::PreflightRejected;
        std::fwprintf(stderr,
            L"result=error operation=%ls changed=%d rebootRequired=0 "
            L"rollback=%ls exitCode=%d phase=\"unhandled-cpp-exception\" "
            L"win32Error=%lu message=\"%ls\"",
            operation, changed ? 1 : 0, changed ? L"failed" : L"not-needed",
            static_cast<int>(exitCode), static_cast<unsigned long>(ERROR_GEN_FAILURE),
            changed
                ? L"unhandled C++ exception after transaction mutation; external reconciliation is required"
                : L"unhandled C++ exception before transaction mutation");
        if (gActiveRecoveryRecord[0] != L'\0') {
            std::fwprintf(stderr,
                L" recoveryRecord=\"%ls\" recoveryRecordWritten=%d",
                gActiveRecoveryRecord.data(),
                gActiveRecoveryRecordWritten ? 1 : 0);
        }
        if (gActiveBackupRootRetained && gActiveBackupRoot[0] != L'\0') {
            std::fwprintf(stderr,
                L" recoveryBackup=\"%ls\" recoveryBackupRetained=1",
                gActiveBackupRoot.data());
        }
        std::fwprintf(stderr, L"\n");
        std::fflush(stderr);
        return static_cast<int>(exitCode);
    }
}
