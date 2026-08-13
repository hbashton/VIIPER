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
#include <newdev.h>
#include <wincrypt.h>
#include <mscat.h>
#include <setupapi.h>
#include <sddl.h>
#include <softpub.h>
#include <wintrust.h>
#include <aclapi.h>

#include "../include/ViiperUdeProtocol.h"

#include <algorithm>
#include <array>
#include <charconv>
#include <cerrno>
#include <cctype>
#include <cstdio>
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
        if (!outcome.error.recoveryRecord.empty()) {
            stream << L" recoveryRecord=" << std::quoted(outcome.error.recoveryRecord)
                   << L" recoveryRecordWritten="
                   << (outcome.error.recoveryRecordWritten ? 1 : 0);
            if (!outcome.error.recoveryRecordWritten) {
                stream << L" recoveryRecordPhase="
                       << std::quoted(outcome.error.recoveryRecordPhase)
                       << L" recoveryRecordWin32Error="
                       << outcome.error.recoveryRecordError
                       << L" recoveryRecordMessage="
                       << std::quoted(outcome.error.recoveryRecordMessage);
            }
        }
        if (!outcome.error.recoveryBackup.empty()) {
            stream << L" recoveryBackup=" << std::quoted(outcome.error.recoveryBackup)
                   << L" recoveryBackupRetained="
                   << (outcome.error.recoveryBackupRetained ? 1 : 0);
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
        } else {
            value->push_back(static_cast<char>(0xe0U | (codePoint >> 12U)));
            value->push_back(static_cast<char>(0x80U | ((codePoint >> 6U) & 0x3fU)));
            value->push_back(static_cast<char>(0x80U | (codePoint & 0x3fU)));
        }
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
                if (position_ + 4 > text_.size()) {
                    *message = "short JSON unicode escape";
                    return false;
                }
                uint32_t codePoint = 0;
                for (unsigned index = 0; index < 4; ++index) {
                    const char digit = text_[position_++];
                    codePoint <<= 4U;
                    if (digit >= '0' && digit <= '9') codePoint |= static_cast<uint32_t>(digit - '0');
                    else if (digit >= 'a' && digit <= 'f') codePoint |= static_cast<uint32_t>(digit - 'a' + 10);
                    else if (digit >= 'A' && digit <= 'F') codePoint |= static_cast<uint32_t>(digit - 'A' + 10);
                    else {
                        *message = "invalid JSON unicode escape";
                        return false;
                    }
                }
                if (codePoint >= 0xd800U && codePoint <= 0xdfffU) {
                    *message = "surrogate JSON escapes are not permitted in install manifests";
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
    if (status != ERROR_SUCCESS) {
        return SetError(error, L"catalog-member-policy", static_cast<DWORD>(status),
            L"package file is not a valid member of the exact trusted driver catalog");
    }
    return true;
}

bool VerifyLocalTestPackageSigner(
    const std::filesystem::path& infPath,
    std::string_view expectedCertificateSha256,
    Error* error) {
    SP_INF_SIGNER_INFO_W signer{};
    signer.cbSize = sizeof(signer);
    if (!SetupVerifyInfFileW(infPath.c_str(), nullptr, &signer)) {
        const DWORD code = GetLastError();
        if (code != ERROR_AUTHENTICODE_TRUSTED_PUBLISHER) {
            return SetError(error, L"inf-local-test-signature", code);
        }
    }
    if (signer.CatalogFile[0] == L'\0' || signer.DigitalSigner[0] == L'\0') {
        return SetError(error, L"inf-local-test-signature", ERROR_INVALID_DATA,
            L"local test INF did not report a catalog and signer");
    }
    const std::filesystem::path reportedCatalogPath = signer.CatalogFile;
    if (_wcsicmp(reportedCatalogPath.filename().c_str(), kCatalogName) != 0) {
        return SetError(error, L"inf-local-test-signature", ERROR_INVALID_DATA,
            L"local test INF did not report the exact VIIPER catalog");
    }
    const std::filesystem::path catalogPath = infPath.parent_path() / kCatalogName;
    if (!VerifyDriverCatalogMember(catalogPath, infPath, error) ||
        !VerifyDriverCatalogMember(catalogPath,
            infPath.parent_path() / kDriverFileName, error)) {
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
    if (!VerifyDriverCatalogMember(catalogPath, infPath, error) ||
        !VerifyDriverCatalogMember(catalogPath,
            packageInfPath.parent_path() / kDriverFileName, error)) {
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
    if (!VerifyInfSignature(path, &catalogPath, error)) {
        return false;
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
        if (!LoadOwnedPackage(infDirectory / data.cFileName, false, &package, &owned, &packageError)) {
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

enum class CandidateDisposition {
    InstallRequired,
    Exact,
};

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
        if (!LoadOwnedPackage(infDirectory / device.publishedInf, true, &package, &owned, error)) {
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

bool RemoveDevice(
    HDEVINFO set,
    SP_DEVINFO_DATA& data,
    uint64_t transactionDeadlineUnixMs,
    const wchar_t* deadlinePhase,
    bool* mutationStarted,
    bool* rebootRequired,
    Error* error) {
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
        if (!LoadOwnedPackage(infDirectory / device.publishedInf, true, &package, &owned, error) || !owned) {
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
    const size_t idCharacters = std::size(kHardwareId) + 1;
    std::vector<wchar_t> identifiers(idCharacters, L'\0');
    std::copy(std::begin(kHardwareId), std::end(kHardwareId), identifiers.begin());
    if (!CheckTransactionDeadline(transactionDeadlineUnixMs,
            L"transaction-deadline-before-root-properties", error)) {
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
        return SetLastErrorDetail(error, L"set-root-hardware-id");
    }
    if (!CheckTransactionDeadline(transactionDeadlineUnixMs,
            L"transaction-deadline-before-root-registration", error)) {
        return false;
    }
    MarkTransactionMutationStarted();
    if (mutationStarted != nullptr) {
        *mutationStarted = true;
    }
    if (!SetupDiCallClassInstaller(DIF_REGISTERDEVICE, set->get(), data)) {
        return SetLastErrorDetail(error, L"register-root-devnode");
    }
    if (registrationSucceeded != nullptr) {
        *registrationSucceeded = true;
    }
    wchar_t instanceId[MAX_DEVICE_ID_LEN]{};
    if (!SetupDiGetDeviceInstanceIdW(
            set->get(), data, instanceId, static_cast<DWORD>(std::size(instanceId)), nullptr)) {
        return SetLastErrorDetail(error, L"verify-generated-root-instance-id");
    }
    if (!IsGeneratedRootInstanceIdForDeviceName(instanceId, kRootDeviceName)) {
        return SetError(error, L"verify-generated-root-instance-id", ERROR_INVALID_DATA,
            L"SetupAPI generated a root identity outside the VIIPER-owned namespace");
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

bool InstallPreinstalledDriverOnDevice(
    HDEVINFO set,
    SP_DEVINFO_DATA* device,
    const PackageInfo& publishedPackage,
    uint64_t transactionDeadlineUnixMs,
    bool* mutationStarted,
    bool* rebootRequired,
    Error* error) {
    if (!SetupDiBuildDriverInfoList(set, device, SPDIT_COMPATDRIVER)) {
        return SetLastErrorDetail(error, L"repair-build-compatible-driver-list");
    }
    const auto destroyList = [&]() {
        return SetupDiDestroyDriverInfoList(set, device, SPDIT_COMPATDRIVER) != FALSE;
    };

    SP_DRVINFO_DATA_W selected{};
    size_t exactMatches = 0;
    for (DWORD index = 0;; ++index) {
        SP_DRVINFO_DATA_W driver{};
        driver.cbSize = sizeof(driver);
        if (!SetupDiEnumDriverInfoW(set, device, SPDIT_COMPATDRIVER, index, &driver)) {
            if (GetLastError() != ERROR_NO_MORE_ITEMS) {
                const DWORD code = GetLastError();
                destroyList();
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
            destroyList();
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
            destroyList();
            return SetError(error, L"repair-compatible-driver-detail", code);
        }
        if (DriverInfoUsesPublishedPackage(
                detail->InfFileName, publishedPackage.publishedName)) {
            selected = driver;
            ++exactMatches;
        }
    }
    if (exactMatches != 1) {
        destroyList();
        return SetError(error, L"repair-exact-driver-selection",
            exactMatches == 0 ? ERROR_NOT_FOUND : ERROR_DUPLICATE_SERVICE_NAME,
            L"compatible driver list must contain exactly one node for the exact preinstalled package");
    }
    if (transactionDeadlineUnixMs != 0 &&
        !CheckTransactionDeadline(transactionDeadlineUnixMs,
            L"transaction-deadline-before-driver-selection", error)) {
        destroyList();
        return false;
    }
    MarkTransactionMutationStarted();
    if (mutationStarted != nullptr) {
        *mutationStarted = true;
    }
    if (!SetupDiSetSelectedDriverW(set, device, &selected)) {
        const DWORD code = GetLastError();
        destroyList();
        return SetError(error, L"repair-select-exact-driver", code);
    }
    if (transactionDeadlineUnixMs != 0 &&
        !CheckTransactionDeadline(transactionDeadlineUnixMs,
            L"transaction-deadline-before-device-binding", error)) {
        destroyList();
        return false;
    }
    BOOL reboot = FALSE;
    if (!DiInstallDevice(nullptr, set, device, &selected, 0, &reboot)) {
        const DWORD code = GetLastError();
        destroyList();
        return SetError(error, L"repair-install-preinstalled-driver", code);
    }
    if (!destroyList()) {
        return SetLastErrorDetail(error, L"repair-destroy-compatible-driver-list");
    }
    *rebootRequired = *rebootRequired || reboot != FALSE;
    return true;
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

enum class ExactRootRegistrationMode {
    Upgrade,
    Rollback,
};

bool RegisterRootDeviceExact(
    const GUID& classGuid,
    const std::wstring& instanceId,
    uint64_t transactionDeadlineUnixMs,
    ExactRootRegistrationMode mode,
    bool* mutationStarted,
    bool* registrationSucceeded,
    DeviceInfoSet* set,
    SP_DEVINFO_DATA* data,
    Error* error) {
    if (registrationSucceeded != nullptr) {
        *registrationSucceeded = false;
    }
    const bool rollback = mode == ExactRootRegistrationMode::Rollback;
    if (!IsOwnedGeneratedRootInstanceId(instanceId)) {
        return SetError(error, rollback ? L"rollback-instance-id" : L"upgrade-instance-id",
            ERROR_INVALID_DATA,
            L"captured root devnode identity is outside the VIIPER or legacy generated root namespace");
    }
    *set = DeviceInfoSet(SetupDiCreateDeviceInfoList(&classGuid, nullptr));
    if (!*set) {
        return SetLastErrorDetail(error, rollback
            ? L"rollback-create-device-info-list" : L"upgrade-create-device-info-list");
    }
    *data = SP_DEVINFO_DATA{};
    data->cbSize = sizeof(*data);
    // With DICD_GENERATE_ID absent, SetupAPI treats DeviceName as the complete
    // device instance ID. Upgrade and rollback must never substitute a fresh
    // ROOT instance.
    if (!SetupDiCreateDeviceInfoW(set->get(), instanceId.c_str(), &classGuid,
            nullptr, nullptr, 0, data)) {
        return SetLastErrorDetail(error, rollback
            ? L"rollback-create-exact-root-devnode" : L"upgrade-create-exact-root-devnode");
    }
    const size_t idCharacters = std::size(kHardwareId) + 1;
    std::vector<wchar_t> identifiers(idCharacters, L'\0');
    std::copy(std::begin(kHardwareId), std::end(kHardwareId), identifiers.begin());
    if (transactionDeadlineUnixMs != 0 &&
        !CheckTransactionDeadline(transactionDeadlineUnixMs,
            rollback ? L"rollback-deadline-before-root-properties"
                     : L"upgrade-deadline-before-root-properties", error)) {
        return false;
    }
    MarkTransactionMutationStarted();
    if (mutationStarted != nullptr) {
        *mutationStarted = true;
    }
    if (!SetupDiSetDeviceRegistryPropertyW(set->get(), data, SPDRP_HARDWAREID,
            reinterpret_cast<const BYTE*>(identifiers.data()),
            static_cast<DWORD>(identifiers.size() * sizeof(wchar_t)))) {
        return SetLastErrorDetail(error, rollback
            ? L"rollback-set-root-hardware-id" : L"upgrade-set-root-hardware-id");
    }
    if (transactionDeadlineUnixMs != 0 &&
        !CheckTransactionDeadline(transactionDeadlineUnixMs,
            rollback ? L"rollback-deadline-before-root-registration"
                     : L"upgrade-deadline-before-root-registration", error)) {
        return false;
    }
    MarkTransactionMutationStarted();
    if (mutationStarted != nullptr) {
        *mutationStarted = true;
    }
    if (!SetupDiCallClassInstaller(DIF_REGISTERDEVICE, set->get(), data)) {
        return SetLastErrorDetail(error, rollback
            ? L"rollback-register-exact-root-devnode" : L"upgrade-register-exact-root-devnode");
    }
    if (registrationSucceeded != nullptr) {
        *registrationSucceeded = true;
    }
    return true;
}

bool VerifyAbiHealth(
    uint64_t deadlineUnixMs,
    const std::string* expectedBuildIdentity,
    Error* error,
    bool requirePristineRuntime = false) {
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
    LARGE_INTEGER counter{};
    QueryPerformanceCounter(&counter);
    VIIPER_UDE_NEGOTIATE_REQUEST request{};
    request.Header.Magic = VIIPER_UDE_MAGIC;
    request.Header.Major = VIIPER_UDE_ABI_MAJOR;
    request.Header.Minor = VIIPER_UDE_ABI_MINOR;
    request.Header.Size = sizeof(request);
    request.ClientNonce = static_cast<VIIPER_UDE_UINT64>(counter.QuadPart) ^ GetTickCount64();
    if (request.ClientNonce == 0) request.ClientNonce = 1;
    request.RequestedCapabilities = VIIPER_UDE_ADVERTISED_CAPABILITIES;
    VIIPER_UDE_NEGOTIATE_RESPONSE response{};
    DWORD returned = 0;
    WinHandle event(CreateEventW(nullptr, TRUE, FALSE, nullptr));
    if (!event) {
        return SetLastErrorDetail(error, L"abi-negotiate-event");
    }
    OVERLAPPED overlapped{};
    overlapped.hEvent = event.get();
    const BOOL completed = DeviceIoControl(device.get(), IOCTL_VIIPER_UDE_NEGOTIATE,
        &request, sizeof(request), &response, sizeof(response), &returned, &overlapped);
    if (!completed && GetLastError() != ERROR_IO_PENDING) {
        return SetLastErrorDetail(error, L"abi-negotiate");
    }
    if (!completed) {
        const uint64_t now = CurrentUnixMilliseconds();
        if (deadlineUnixMs <= now) {
            const BOOL cancelled = CancelIoEx(device.get(), &overlapped);
            const DWORD cancelError = cancelled ? ERROR_SUCCESS : GetLastError();
            const DWORD drain = WaitForSingleObject(event.get(), kCancelledIoDrainMs);
            if ((!cancelled && cancelError != ERROR_NOT_FOUND) || drain != WAIT_OBJECT_0) {
                return SetError(error, L"abi-negotiate-drain",
                    !cancelled && cancelError != ERROR_NOT_FOUND
                        ? cancelError : ERROR_OPERATION_ABORTED,
                    L"expired native ABI negotiation could not be cancelled and drained safely");
            }
            DWORD ignored = 0;
            GetOverlappedResult(device.get(), &overlapped, &ignored, FALSE);
            return SetError(error, L"abi-negotiate-timeout", ERROR_TIMEOUT,
                L"native ABI negotiation exceeded the package transaction deadline");
        }
        const uint64_t remaining = deadlineUnixMs - now;
        const DWORD waitMilliseconds = static_cast<DWORD>(
            std::min<uint64_t>(remaining, static_cast<uint64_t>(MAXDWORD - 1)));
        const DWORD wait = WaitForSingleObject(event.get(), waitMilliseconds);
        if (wait == WAIT_TIMEOUT) {
            const BOOL cancelled = CancelIoEx(device.get(), &overlapped);
            const DWORD cancelError = cancelled ? ERROR_SUCCESS : GetLastError();
            const DWORD drain = WaitForSingleObject(event.get(), kCancelledIoDrainMs);
            if (drain == WAIT_OBJECT_0) {
                DWORD ignored = 0;
                GetOverlappedResult(device.get(), &overlapped, &ignored, FALSE);
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
            CancelIoEx(device.get(), &overlapped);
            const DWORD drain = WaitForSingleObject(event.get(), kCancelledIoDrainMs);
            if (drain != WAIT_OBJECT_0) {
                return SetError(error, L"abi-negotiate-drain", ERROR_OPERATION_ABORTED,
                    L"failed native ABI wait could not be drained safely");
            }
            SetLastError(waitError);
            return SetLastErrorDetail(error, L"abi-negotiate-wait");
        }
        if (!GetOverlappedResult(device.get(), &overlapped, &returned, FALSE)) {
            return SetLastErrorDetail(error, L"abi-negotiate-result");
        }
    }
    std::string loadedBuildIdentity;
    loadedBuildIdentity.reserve(VIIPER_UDE_BUILD_IDENTITY_BYTES * 2);
    static constexpr char digits[] = "0123456789abcdef";
    for (VIIPER_UDE_UINT8 byte : response.BuildIdentity) {
        loadedBuildIdentity.push_back(digits[byte >> 4U]);
        loadedBuildIdentity.push_back(digits[byte & 0x0fU]);
    }
    if (returned != sizeof(response) || response.Header.Magic != VIIPER_UDE_MAGIC ||
        response.Header.Major != VIIPER_UDE_ABI_MAJOR ||
        response.Header.Minor != VIIPER_UDE_ABI_MINOR ||
        response.Header.Size != sizeof(response) || response.Header.Flags != 0 ||
        response.ClientNonce != request.ClientNonce || response.DriverNonce == 0 ||
        response.Capabilities != VIIPER_UDE_ADVERTISED_CAPABILITIES ||
        response.MaxDevices != VIIPER_UDE_MAX_DEVICES ||
        response.MaxDescriptorBytes != VIIPER_UDE_MAX_DESCRIPTOR_BYTES ||
        response.MaxTransferBytes != VIIPER_UDE_MAX_TRANSFER_BYTES ||
        response.MaxIsoPackets != VIIPER_UDE_MAX_ISO_PACKETS ||
        response.MaxPendingOperations != VIIPER_UDE_MAX_PENDING_OPERATIONS ||
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
        if (statsReturned != sizeof(stats) || stats.Header.Magic != VIIPER_UDE_MAGIC ||
            stats.Header.Major != VIIPER_UDE_ABI_MAJOR ||
            stats.Header.Minor != VIIPER_UDE_ABI_MINOR ||
            stats.Header.Size != sizeof(stats) || stats.Header.Flags != 0) {
            return SetError(error, L"upgrade-pristine-stats", ERROR_REVISION_MISMATCH,
                L"loaded driver returned an invalid pristine-runtime statistics record");
        }
        if (stats.OperationsDequeued != 0 || stats.OperationsCompleted != 0 ||
            stats.OperationsCancelled != 0 || stats.OperationsPurged != 0 ||
            stats.LateCompletions != 0 || stats.InvalidMessages != 0 ||
            stats.QueueExhaustions != 0 || stats.IsoPackets != 0 ||
            stats.BytesToDevice != 0 || stats.BytesFromDevice != 0 ||
            stats.NotificationEvents != 0 || stats.NotificationEventOverflows != 0 ||
            stats.ActiveDevices != 0 || stats.PendingOperations != 0 ||
            stats.WaitingDequeues != 0 || stats.CleanupRetries != 0 ||
            stats.InputReportsSubmitted != 0 || stats.InputReportsCompleted != 0) {
            return SetError(error, L"upgrade-runtime-reboot-boundary",
                ERROR_SUCCESS_REBOOT_REQUIRED,
                L"the loaded native bus has serviced virtual-device work since boot; restart Windows and rerun the identical package command before creating another virtual device");
        }
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

std::set<std::wstring> PublishedNames(const std::vector<PackageInfo>& packages) {
    std::set<std::wstring> names;
    for (const PackageInfo& package : packages) {
        std::wstring lower = package.publishedName;
        std::transform(lower.begin(), lower.end(), lower.begin(), [](wchar_t character) {
            return static_cast<wchar_t>(towlower(character));
        });
        names.insert(std::move(lower));
    }
    return names;
}

std::vector<size_t> NewPackageIndices(
    const std::vector<PackageInfo>& prior,
    const std::vector<PackageInfo>& current) {
    const std::set<std::wstring> priorNames = PublishedNames(prior);
    std::vector<size_t> indices;
    for (size_t index = 0; index < current.size(); ++index) {
        std::wstring lower = current[index].publishedName;
        std::transform(lower.begin(), lower.end(), lower.begin(), [](wchar_t character) {
            return static_cast<wchar_t>(towlower(character));
        });
        if (!priorNames.contains(lower)) {
            indices.push_back(index);
        }
    }
    return indices;
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
                transactionDeadlineUnixMs, ExactRootRegistrationMode::Rollback,
                nullptr, nullptr,
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

bool RollbackInstall(const Snapshot& prior, bool* rebootRequired, Error* error) {
    if (!RestorePriorBinding(prior, 0, rebootRequired, error)) {
        return false;
    }
    std::vector<PackageInfo> current;
    if (!EnumerateOwnedPackages(&current, error)) {
        return false;
    }
    for (size_t index : NewPackageIndices(prior.packages, current)) {
        if (!UninstallPackage(current[index], rebootRequired, error)) {
            return false;
        }
    }
    if (!prior.devices.empty() && !*rebootRequired) {
        // Driver rollback can prove exact instance/package binding, but the
        // prior transient started/problem state is not a restorable identity.
        return VerifyInstalledBinding(
            prior.devices[0].package, prior.devices[0].publishedInf, true, error);
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
    if (!SetEvent(options.brokerQuiesceRequest)) {
        return SetLastErrorDetail(error, L"broker-quiescence-request");
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
    if (!SetEvent(options.brokerHandoff)) {
        return SetLastErrorDetail(error, L"broker-handoff-signal");
    }
    return true;
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
    if (!LoadOwnedPackage(lockedInfPath, true, candidate, &owned, error) || !owned ||
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
    if (!CreateProcessW(options.brokerExecutable.c_str(), mutableCommand.data(), nullptr, nullptr,
            TRUE, CREATE_NO_WINDOW | EXTENDED_STARTUPINFO_PRESENT, nullptr,
            options.brokerExecutable.parent_path().c_str(),
            &startup.StartupInfo, &process)) {
        const DWORD code = GetLastError();
        deleteAttributeList();
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

    // Same-version bytes are immutable. An exact package with a missing,
    // stopped, or stale binding may repair only the ROOT topology from the
    // already-published exact INF. It selects that preinstalled package for the
    // specific devnode and calls DiInstallDevice; it never calls
    // DiInstallDriverW or UpdateDriverForPlugAndPlayDevicesW and therefore
    // cannot replace same-version DriverStore content.
    const bool topologyRepair =
        disposition == CandidateDisposition::Exact && !exactBindingHealthy;
    const bool driverMutation =
        disposition == CandidateDisposition::InstallRequired || topologyRepair;
    bool driverMutationStarted = false;
    DeviceInfoSet created;
    SP_DEVINFO_DATA createdData{};
    createdData.cbSize = sizeof(createdData);
    bool createdHere = false;
    bool registrationSucceeded = false;
    GUID candidateClassGuid{};
    wchar_t candidateClassName[MAX_CLASS_NAME_LEN]{};
    const bool needsRootRegistration =
        disposition == CandidateDisposition::InstallRequired ||
        (topologyRepair && prior.devices.empty());
    if (needsRootRegistration &&
        !SetupDiGetINFClassW(candidate.infPath.c_str(), &candidateClassGuid,
            candidateClassName, MAX_CLASS_NAME_LEN, nullptr)) {
        SetLastErrorDetail(&outcome.error, L"candidate-inf-class");
        outcome.exitCode = ExitCode::PreflightRejected;
        return outcome;
    }

    // The service mutex remains owned by the outer package transaction. Ask it
    // to stop only a trusted running broker after classification proves a
    // driver mutation is necessary, then keep that mutex held across exact
    // root replacement and binding verification. This prevents the broker from
    // retaining a UdeCx handle that turns synchronous removal into a reboot.
    if (driverMutation && !options.brokerExecutable.empty() &&
        !RequestBrokerQuiescence(options, &outcome.error)) {
        outcome.exitCode = ExitCode::PreflightRejected;
        return outcome;
    }

    // UdeCx child deletion is asynchronous. Older installed images could
    // release their logical device slot before framework teardown settled,
    // leaving DiUninstallDevice blocked indefinitely even though the broker
    // reported an empty bus. Once the trusted broker is stopped, require a
    // zero-lifetime-work runtime before replacing an existing root. A restart
    // resets these counters and guarantees that no pre-upgrade child object
    // can survive into root removal.
    if (disposition == CandidateDisposition::InstallRequired &&
        !prior.devices.empty() && outcome.error.code == ERROR_SUCCESS &&
        !VerifyAbiHealth(
            options.transactionDeadlineUnixMs, nullptr, &outcome.error, true)) {
        if (outcome.error.code == ERROR_SUCCESS_REBOOT_REQUIRED) {
            outcome.rebootRequired = true;
        }
    }

    if (outcome.error.code == ERROR_SUCCESS &&
        disposition == CandidateDisposition::InstallRequired) {
        // Updating a running root bus in place makes DiInstallDriverW report a
        // reboot even though this helper immediately restores the old package.
        // Remove only the exact captured VIIPER-owned devnode first, prove its
        // topology is gone, then stage and bind the candidate to the same root
        // identity. Snapshot rollback recreates the old exact identity/package
        // if any later step fails.
        if (!prior.devices.empty()) {
            DeviceInfoSet replaced = OpenRootDevices();
            std::vector<std::pair<SP_DEVINFO_DATA, DeviceState>> replacements;
            if (!replaced) {
                SetLastErrorDetail(&outcome.error, L"upgrade-open-root-devices");
            } else if (!FindExactDevices(replaced.get(), &replacements, &outcome.error)) {
                // Exact enumeration recorded the failure.
            } else if (replacements.size() != 1 ||
                _wcsicmp(replacements[0].second.instanceId.c_str(),
                    prior.devices[0].instanceId.c_str()) != 0 ||
                _wcsicmp(replacements[0].second.publishedInf.c_str(),
                    prior.devices[0].publishedInf.c_str()) != 0 ||
                !(replacements[0].second.version == prior.devices[0].version)) {
                SetError(&outcome.error, L"upgrade-root-identity", ERROR_REVISION_MISMATCH,
                    L"captured root devnode identity or package binding changed before replacement");
            } else {
                bool removalReboot = false;
                if (RemoveDevice(replaced.get(), replacements[0].first,
                        options.transactionDeadlineUnixMs,
                        L"upgrade-deadline-before-device-removal",
                        &driverMutationStarted, &removalReboot, &outcome.error)) {
                    outcome.rebootRequired = removalReboot;
                    if (removalReboot) {
                        SetError(&outcome.error, L"upgrade-device-removal-reboot-boundary",
                            ERROR_SUCCESS_REBOOT_REQUIRED,
                            L"the exact prior root bus could not be removed synchronously; the captured binding will be restored before restart");
                    } else {
                        Snapshot afterRemoval;
                        if (!CaptureSnapshot(&afterRemoval, &outcome.error)) {
                            // Exact inventory recorded the failure.
                        } else if (!afterRemoval.devices.empty()) {
                            SetError(&outcome.error, L"upgrade-device-removal-verification",
                                ERROR_DEVICE_IN_USE,
                                L"the exact prior root bus remained present after synchronous removal");
                        }
                    }
                }
                outcome.changed = outcome.changed || driverMutationStarted;
            }
        }

        if (outcome.error.code == ERROR_SUCCESS &&
            CheckTransactionDeadline(options,
                L"transaction-deadline-before-driver-install", &outcome.error)) {
            BOOL installReboot = FALSE;
            const DWORD installFlags = downgrade ? DIIRFLAG_FORCE_INF : 0;
            MarkTransactionMutationStarted();
            driverMutationStarted = true;
            outcome.changed = true;
            if (!DiInstallDriverW(nullptr, candidate.infPath.c_str(), installFlags, &installReboot)) {
                SetLastErrorDetail(&outcome.error, L"install-driver-package");
            } else {
                outcome.rebootRequired = outcome.rebootRequired || installReboot != FALSE;
                if (installReboot) {
                    SetError(&outcome.error, L"driver-package-reboot-boundary",
                        ERROR_SUCCESS_REBOOT_REQUIRED,
                        L"Windows could not stage the candidate driver package without a restart; the captured binding will be restored first");
                } else if (!FindPublishedCandidate(candidate, &publishedCandidate, &outcome.error)) {
                    // Exact Driver Store inventory recorded the failure.
                } else if (options.production && !VerifyMicrosoftHardwareInfSigner(
                               publishedCandidate.infPath, &outcome.error)) {
                    // The staged package must retain its exact production HLK/WHCP policy.
                }
            }
        }
    }

    if (outcome.error.code == ERROR_SUCCESS && driverMutation &&
        (prior.devices.empty() || disposition == CandidateDisposition::InstallRequired)) {
        bool registeredAndVerified = false;
        if (prior.devices.empty()) {
            registeredAndVerified = RegisterRootDevice(
                candidateClassGuid, options.transactionDeadlineUnixMs,
                &driverMutationStarted, &registrationSucceeded,
                &created, &createdData, &outcome.error);
        } else {
            registeredAndVerified = RegisterRootDeviceExact(
                candidateClassGuid, prior.devices[0].instanceId,
                options.transactionDeadlineUnixMs, ExactRootRegistrationMode::Upgrade,
                &driverMutationStarted, &registrationSucceeded,
                &created, &createdData, &outcome.error);
        }
        createdHere = registrationSucceeded;
        if (registeredAndVerified) {
            InstallPreinstalledDriverOnDevice(
                created.get(), &createdData, publishedCandidate,
                options.transactionDeadlineUnixMs, &driverMutationStarted,
                &outcome.rebootRequired, &outcome.error);
        }
        outcome.changed = outcome.changed || driverMutationStarted;
    }

    if (outcome.error.code == ERROR_SUCCESS && topologyRepair && !prior.devices.empty()) {
        DeviceInfoSet repairSet = OpenRootDevices();
        std::vector<std::pair<SP_DEVINFO_DATA, DeviceState>> repairDevices;
        if (!repairSet) {
            SetLastErrorDetail(&outcome.error, L"repair-open-root-devices");
        } else if (!FindExactDevices(repairSet.get(), &repairDevices, &outcome.error)) {
            // Exact enumeration recorded the failure.
        } else if (repairDevices.size() != 1 ||
            _wcsicmp(repairDevices[0].second.instanceId.c_str(),
                prior.devices[0].instanceId.c_str()) != 0) {
            SetError(&outcome.error, L"repair-root-identity", ERROR_REVISION_MISMATCH,
                L"root devnode identity changed before exact topology repair");
        } else {
            InstallPreinstalledDriverOnDevice(
                repairSet.get(), &repairDevices[0].first, publishedCandidate,
                options.transactionDeadlineUnixMs, &driverMutationStarted,
                &outcome.rebootRequired, &outcome.error);
            outcome.changed = outcome.changed || driverMutationStarted;
        }
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
    if (outcome.error.code != ERROR_SUCCESS && driverMutationStarted) {
        const Error installError = outcome.error;
        Error rollbackError;
        bool rollbackReboot = outcome.rebootRequired;
        if (createdHere) {
            Error cleanupError;
            if (!RemoveDevice(created.get(), createdData, 0, nullptr, nullptr,
                    &rollbackReboot, &cleanupError)) {
                outcome.rollback = L"failed";
                outcome.rebootRequired = rollbackReboot;
                outcome.error = std::move(cleanupError);
                outcome.exitCode = ExitCode::RollbackFailed;
                return outcome;
            }
        }
        if (RollbackInstall(prior, &rollbackReboot, &rollbackError)) {
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
        outcome.exitCode = ExitCode::RollbackFailed;
        return outcome;
    }
    if (outcome.error.code != ERROR_SUCCESS) {
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
                outcome.rollback = L"failed";
                outcome.error = std::move(brokerError);
                outcome.exitCode = ExitCode::RollbackFailed;
                return outcome;
            }
            if (!driverMutationStarted) {
                outcome.rollback = brokerChanged ? L"succeeded" : L"not-needed";
                outcome.error = std::move(brokerError);
                outcome.exitCode = outcome.error.code == ERROR_SUCCESS_REBOOT_REQUIRED
                    ? ExitCode::RebootRequired : ExitCode::Failure;
                return outcome;
            }
            Error rollbackError;
            bool rollbackReboot = outcome.rebootRequired;
            if (createdHere) {
                Error cleanupError;
                if (!RemoveDevice(created.get(), createdData, 0, nullptr, nullptr,
                        &rollbackReboot, &cleanupError)) {
                    outcome.rollback = L"failed";
                    outcome.rebootRequired = rollbackReboot;
                    outcome.error = std::move(cleanupError);
                    outcome.exitCode = ExitCode::RollbackFailed;
                    return outcome;
                }
            }
            if (RollbackInstall(prior, &rollbackReboot, &rollbackError)) {
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
            outcome.exitCode = ExitCode::RollbackFailed;
            return outcome;
        }
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

bool BackupPackages(
    const std::vector<PackageInfo>& packages,
    BackupDirectory* root,
    std::vector<PackageBackup>* backups,
    Error* error) {
    if (!root->Create(error)) {
        return false;
    }
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
        const std::filesystem::path destination = root->path() / std::to_wstring(index);
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
        if (!LoadOwnedPackage(backupInf, true, &verified, &owned, error) || !owned) {
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
            "6e25b6972fd774d00cc3c081dfe2244fa6ad24ddf1551012c0297c741da849b5") {
        if (outcome.error.code == ERROR_SUCCESS) {
            SetError(&outcome.error, L"self-test-build-identity", ERROR_INVALID_DATA);
        }
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
    const std::vector<size_t> cleanup = NewPackageIndices(
        {priorPackage}, {preservedPackage, newPackage});
    if (cleanup != std::vector<size_t>{1}) {
        SetError(&outcome.error, L"self-test-rollback-cleanup", ERROR_INVALID_DATA);
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
            {L"install", L"verify", L"remove", L"status", L"self-test"}) {
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
