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
#include <windows.h>
#include <cfgmgr32.h>
#include <devguid.h>
#include <devpkey.h>
#include <newdev.h>
#include <setupapi.h>
#include <wincrypt.h>

#include "../include/ViiperUdeProtocol.h"

#include <algorithm>
#include <array>
#include <cctype>
#include <cstdint>
#include <cwchar>
#include <filesystem>
#include <iomanip>
#include <iostream>
#include <iterator>
#include <map>
#include <optional>
#include <set>
#include <sstream>
#include <string>
#include <string_view>
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

namespace {

constexpr wchar_t kHardwareId[] = L"ROOT\\VIIPER\\UDE";
constexpr wchar_t kEnumerator[] = L"ROOT";
constexpr wchar_t kServiceName[] = L"ViiperUde";
constexpr wchar_t kProviderName[] = L"VIIPER Project";
constexpr wchar_t kCatalogName[] = L"ViiperUde.cat";
constexpr wchar_t kDriverFileName[] = L"ViiperUde.sys";
constexpr wchar_t kModelSection[] = L"Standard.NTamd64.10.0...17763";
constexpr wchar_t kInstallSection[] = L"ViiperUde_Install";
constexpr wchar_t kTransactionMutex[] = L"Global\\VIIPER_UDE_DRIVER_TRANSACTION_V1";
constexpr size_t kMaximumManifestBytes = 1024U * 1024U;

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
};

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
               << L" win32Error=" << outcome.error.code
               << L" message=" << std::quoted(outcome.error.message);
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
    bool Acquire(Error* error) {
        WinHandle handle(CreateMutexW(nullptr, FALSE, kTransactionMutex));
        if (!handle) {
            return SetLastErrorDetail(error, L"transaction-mutex");
        }
        if (GetLastError() == ERROR_ALREADY_EXISTS) {
            return SetError(error, L"transaction-mutex", ERROR_INSTALL_ALREADY_RUNNING,
                L"another VIIPER native driver transaction is active");
        }
        handle_ = std::move(handle);
        return true;
    }

private:
    WinHandle handle_;
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
    if (value.size() < 40 || value.size() > 64) {
        return false;
    }
    return std::all_of(value.begin(), value.end(), [](unsigned char character) {
        return std::isxdigit(character) != 0;
    });
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

bool ReadSmallFile(const std::filesystem::path& path, std::string* contents, Error* error) {
    WinHandle file(CreateFileW(path.c_str(), GENERIC_READ, FILE_SHARE_READ, nullptr,
        OPEN_EXISTING, FILE_ATTRIBUTE_NORMAL | FILE_FLAG_SEQUENTIAL_SCAN, nullptr));
    if (!file) {
        return SetLastErrorDetail(error, L"manifest-open");
    }
    LARGE_INTEGER size{};
    if (!GetFileSizeEx(file.get(), &size)) {
        return SetLastErrorDetail(error, L"manifest-size");
    }
    if (size.QuadPart <= 0 || static_cast<uint64_t>(size.QuadPart) > kMaximumManifestBytes) {
        return SetError(error, L"manifest-size", ERROR_FILE_TOO_LARGE,
            L"manifest must be nonempty and no larger than one MiB");
    }
    contents->assign(static_cast<size_t>(size.QuadPart), '\0');
    DWORD read = 0;
    if (!ReadFile(file.get(), contents->data(), static_cast<DWORD>(contents->size()), &read, nullptr) ||
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

bool Sha256File(const std::filesystem::path& path, std::string* digest, Error* error) {
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
    WinHandle file(CreateFileW(path.c_str(), GENERIC_READ, FILE_SHARE_READ, nullptr,
        OPEN_EXISTING, FILE_ATTRIBUTE_NORMAL | FILE_FLAG_SEQUENTIAL_SCAN, nullptr));
    if (!file) {
        const DWORD code = GetLastError();
        CryptDestroyHash(hash);
        releaseProvider();
        return SetError(error, L"sha256-open", code);
    }
    std::array<BYTE, 64 * 1024> buffer{};
    for (;;) {
        DWORD read = 0;
        if (!ReadFile(file.get(), buffer.data(), static_cast<DWORD>(buffer.size()), &read, nullptr)) {
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

bool FileLength(const std::filesystem::path& path, uint64_t* length, Error* error) {
    std::error_code fileError;
    const uintmax_t size = std::filesystem::file_size(path, fileError);
    if (fileError) {
        return SetError(error, L"manifest-file-size", static_cast<DWORD>(fileError.value()));
    }
    *length = static_cast<uint64_t>(size);
    return true;
}

bool ValidateManifest(
    const std::filesystem::path& manifestPath,
    const std::string& expectedRevision,
    bool production,
    const std::filesystem::path& packageDirectory,
    Error* error) {
    std::string raw;
    if (!ReadSmallFile(manifestPath, &raw, error)) {
        return false;
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
    const JsonValue* files = ObjectField(*object, "files");
    const auto* schemaValue = schema == nullptr ? nullptr : std::get_if<int64_t>(&schema->value);
    const auto* revisionValue = revision == nullptr ? nullptr : std::get_if<std::string>(&revision->value);
    const auto* releaseValue = releaseEligible == nullptr ? nullptr : std::get_if<bool>(&releaseEligible->value);
    const auto* routeValue = signingRoute == nullptr ? nullptr : std::get_if<std::string>(&signingRoute->value);
    const auto* fileArray = files == nullptr ? nullptr : std::get_if<JsonValue::Array>(&files->value);
    if (schemaValue == nullptr || *schemaValue != 1 || revisionValue == nullptr ||
        LowerAscii(*revisionValue) != LowerAscii(expectedRevision) || releaseValue == nullptr ||
        routeValue == nullptr || fileArray == nullptr) {
        return SetError(error, L"manifest-contract", ERROR_INVALID_DATA,
            L"manifest schema, source revision, release route, or file list is invalid");
    }
    if (production) {
        if (!*releaseValue || *routeValue != "HLK/WHCP") {
            return SetError(error, L"manifest-release-route", ERROR_INVALID_DATA,
                L"production installation requires a release-eligible HLK/WHCP manifest");
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
        if (*name == "ViiperUde.inf" || *name == "ViiperUde.pdb") {
            const std::filesystem::path filePath = packageDirectory / std::wstring(name->begin(), name->end());
            uint64_t actualLength = 0;
            std::string actualHash;
            if (!FileLength(filePath, &actualLength, error) || !Sha256File(filePath, &actualHash, error)) {
                return false;
            }
            if (actualLength != static_cast<uint64_t>(*length) ||
                LowerAscii(actualHash) != LowerAscii(*hash)) {
                return SetError(error, L"manifest-hash", ERROR_CRC,
                    L"INF or PDB does not match the source-bound submission manifest");
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
    SetupGetStringFieldW(&context, field, nullptr, 0, &required);
    if (required == 0 || GetLastError() != ERROR_INSUFFICIENT_BUFFER) {
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
};

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
        return SetLastErrorDetail(error, L"inf-signature");
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
    if (!VerifyInfSignature(path, nullptr, error)) {
        return false;
    }
    std::string hash;
    if (!Sha256File(path, &hash, error)) {
        return false;
    }
    package->infPath = path;
    package->version = version;
    package->infSha256 = std::move(hash);
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
        if (package.version == candidate.version && package.infSha256 == candidate.infSha256) {
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

bool RemoveDevice(HDEVINFO set, SP_DEVINFO_DATA& data, bool* rebootRequired, Error* error) {
    BOOL reboot = FALSE;
    if (!DiUninstallDevice(nullptr, set, &data, 0, &reboot)) {
        return SetLastErrorDetail(error, L"remove-devnode");
    }
    *rebootRequired = *rebootRequired || reboot != FALSE;
    return true;
}

bool RemoveAllExactDevices(bool* rebootRequired, Error* error) {
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
        if (_wcsicmp(device.service.c_str(), kServiceName) != 0 ||
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
        if (!RemoveDevice(set.get(), match.first, rebootRequired, error)) {
            return false;
        }
    }
    return true;
}

bool RegisterRootDevice(
    const GUID& classGuid,
    const std::wstring& className,
    DeviceInfoSet* set,
    SP_DEVINFO_DATA* data,
    Error* error) {
    *set = DeviceInfoSet(SetupDiCreateDeviceInfoList(&classGuid, nullptr));
    if (!*set) {
        return SetLastErrorDetail(error, L"create-device-info-list");
    }
    *data = SP_DEVINFO_DATA{};
    data->cbSize = sizeof(*data);
    if (!SetupDiCreateDeviceInfoW(
            set->get(), className.c_str(), &classGuid, nullptr, nullptr,
            DICD_GENERATE_ID, data)) {
        return SetLastErrorDetail(error, L"create-root-devnode");
    }
    const size_t idCharacters = std::size(kHardwareId) + 1;
    std::vector<wchar_t> identifiers(idCharacters, L'\0');
    std::copy(std::begin(kHardwareId), std::end(kHardwareId), identifiers.begin());
    if (!SetupDiSetDeviceRegistryPropertyW(
            set->get(), data, SPDRP_HARDWAREID,
            reinterpret_cast<const BYTE*>(identifiers.data()),
            static_cast<DWORD>(identifiers.size() * sizeof(wchar_t)))) {
        return SetLastErrorDetail(error, L"set-root-hardware-id");
    }
    if (!SetupDiCallClassInstaller(DIF_REGISTERDEVICE, set->get(), data)) {
        return SetLastErrorDetail(error, L"register-root-devnode");
    }
    return true;
}

bool VerifyAbiHealth(Error* error) {
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
        0, nullptr, OPEN_EXISTING, FILE_ATTRIBUTE_NORMAL, nullptr));
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
    request.RequestedCapabilities = VIIPER_UDE_CAP_ISOCHRONOUS |
        VIIPER_UDE_CAP_STREAMS | VIIPER_UDE_CAP_DEVICE_LIFECYCLE |
        VIIPER_UDE_CAP_INPUT_REPORTS;
    VIIPER_UDE_NEGOTIATE_RESPONSE response{};
    DWORD returned = 0;
    if (!DeviceIoControl(device.get(), IOCTL_VIIPER_UDE_NEGOTIATE,
            &request, sizeof(request), &response, sizeof(response), &returned, nullptr)) {
        return SetLastErrorDetail(error, L"abi-negotiate");
    }
    const VIIPER_UDE_UINT32 requiredCapabilities = VIIPER_UDE_CAP_ISOCHRONOUS |
        VIIPER_UDE_CAP_DEVICE_LIFECYCLE | VIIPER_UDE_CAP_INPUT_REPORTS;
    if (returned != sizeof(response) || response.Header.Magic != VIIPER_UDE_MAGIC ||
        response.Header.Major != VIIPER_UDE_ABI_MAJOR ||
        response.Header.Minor != VIIPER_UDE_ABI_MINOR ||
        response.Header.Size != sizeof(response) || response.Header.Flags != 0 ||
        response.ClientNonce != request.ClientNonce || response.DriverNonce == 0 ||
        (response.Capabilities & requiredCapabilities) != requiredCapabilities ||
        response.MaxDevices != VIIPER_UDE_MAX_DEVICES ||
        response.MaxDescriptorBytes != VIIPER_UDE_MAX_DESCRIPTOR_BYTES ||
        response.MaxTransferBytes != VIIPER_UDE_MAX_TRANSFER_BYTES ||
        response.MaxIsoPackets != VIIPER_UDE_MAX_ISO_PACKETS ||
        response.MaxPendingOperations != VIIPER_UDE_MAX_PENDING_OPERATIONS) {
        return SetError(error, L"abi-negotiate", ERROR_REVISION_MISMATCH,
            L"driver health response does not match the compiled broker ABI");
    }
    return true;
}

bool VerifyInstalled(
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
        snapshot.devices[0].package.infSha256 != candidate.infSha256) {
        return SetError(error, L"install-verification", ERROR_REVISION_MISMATCH,
            L"installed devnode is not bound to the exact candidate package");
    }
    if (!allowStopped && !snapshot.devices[0].started) {
        return SetError(error, L"install-start", ERROR_DEVICE_NOT_AVAILABLE,
            L"installed driver did not start; problem=" + std::to_wstring(snapshot.devices[0].problem));
    }
    return allowStopped || VerifyAbiHealth(error);
}

bool UninstallPackage(const PackageInfo& package, bool* rebootRequired, Error* error) {
    BOOL reboot = FALSE;
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

bool RestorePriorBinding(const Snapshot& prior, bool* rebootRequired, Error* error) {
    bool ignored = false;
    if (!RemoveAllExactDevices(&ignored, error)) {
        return false;
    }
    *rebootRequired = *rebootRequired || ignored;
    if (prior.devices.empty()) {
        return true;
    }
    const PackageInfo& package = prior.devices[0].package;
    GUID classGuid{};
    wchar_t className[MAX_CLASS_NAME_LEN]{};
    if (!SetupDiGetINFClassW(package.infPath.c_str(), &classGuid, className, MAX_CLASS_NAME_LEN, nullptr)) {
        return SetLastErrorDetail(error, L"rollback-inf-class");
    }
    DeviceInfoSet created;
    SP_DEVINFO_DATA createdData{};
    createdData.cbSize = sizeof(createdData);
    if (!RegisterRootDevice(classGuid, className, &created, &createdData, error)) {
        return false;
    }
    BOOL reboot = FALSE;
    if (!UpdateDriverForPlugAndPlayDevicesW(
            nullptr, kHardwareId, package.infPath.c_str(), INSTALLFLAG_FORCE, &reboot)) {
        return SetLastErrorDetail(error, L"rollback-bind-prior");
    }
    *rebootRequired = *rebootRequired || reboot != FALSE;
    return true;
}

bool RollbackInstall(const Snapshot& prior, bool* rebootRequired, Error* error) {
    if (!RestorePriorBinding(prior, rebootRequired, error)) {
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
        return VerifyInstalled(
            prior.devices[0].package, prior.devices[0].publishedInf, false, error);
    }
    return true;
}

bool LockPackageFiles(
    const std::filesystem::path& directory,
    std::vector<WinHandle>* locks,
    Error* error) {
    locks->clear();
    for (const wchar_t* name : {L"ViiperUde.inf", L"ViiperUde.sys", L"ViiperUde.pdb", L"ViiperUde.cat"}) {
        WinHandle file(CreateFileW((directory / name).c_str(), GENERIC_READ, FILE_SHARE_READ,
            nullptr, OPEN_EXISTING, FILE_ATTRIBUTE_NORMAL, nullptr));
        if (!file) {
            return SetLastErrorDetail(error, L"package-lock",
                L"all four package files must exist and remain immutable during installation");
        }
        locks->push_back(std::move(file));
    }
    return true;
}

struct InstallOptions {
    std::filesystem::path infPath;
    std::filesystem::path manifestPath;
    std::string sourceRevision;
    bool production = true;
    std::optional<Version> expectedDowngradeFrom;
};

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
    std::error_code candidatePathError;
    const std::filesystem::path lockedInfPath =
        std::filesystem::canonical(options.infPath, candidatePathError);
    if (candidatePathError || lockedInfPath.filename().wstring() != L"ViiperUde.inf") {
        SetError(&outcome.error, L"package-path", ERROR_FILE_NOT_FOUND);
        outcome.exitCode = ExitCode::PreflightRejected;
        return outcome;
    }
    const std::filesystem::path packageDirectory = lockedInfPath.parent_path();
    std::vector<WinHandle> packageLocks;
    if (!LockPackageFiles(packageDirectory, &packageLocks, &outcome.error)) {
        outcome.exitCode = ExitCode::PreflightRejected;
        return outcome;
    }
    PackageInfo candidate;
    bool owned = false;
    if (!LoadOwnedPackage(lockedInfPath, true, &candidate, &owned, &outcome.error) || !owned ||
        !ValidateManifest(options.manifestPath, options.sourceRevision, options.production,
            packageDirectory, &outcome.error)) {
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
    std::optional<PackageInfo> highest;
    for (const PackageInfo& package : prior.packages) {
        if (!highest || highest->version < package.version) {
            highest = package;
        }
    }
    bool downgrade = false;
    if (highest) {
        if (candidate.version < highest->version) {
            downgrade = true;
            if (!options.expectedDowngradeFrom ||
                !(*options.expectedDowngradeFrom == highest->version)) {
                SetError(&outcome.error, L"version-policy", ERROR_REVISION_MISMATCH,
                    L"downgrade rejected; pass --allow-controlled-downgrade with the exact installed version " +
                    VersionToString(highest->version));
                outcome.exitCode = ExitCode::PreflightRejected;
                return outcome;
            }
        } else if (candidate.version == highest->version) {
            const bool conflictingSameVersion = std::any_of(
                prior.packages.begin(), prior.packages.end(), [&](const PackageInfo& package) {
                    return package.version == candidate.version &&
                        package.infSha256 != candidate.infSha256;
                });
            if (conflictingSameVersion) {
                SetError(&outcome.error, L"version-policy", ERROR_REVISION_MISMATCH,
                    L"same-version package replacement is rejected; increment DriverVer");
                outcome.exitCode = ExitCode::PreflightRejected;
                return outcome;
            }
        }
    }
    if (options.expectedDowngradeFrom && !downgrade) {
        SetError(&outcome.error, L"version-policy", ERROR_INVALID_PARAMETER,
            L"controlled downgrade guard is valid only for an actual downgrade");
        outcome.exitCode = ExitCode::PreflightRejected;
        return outcome;
    }

    outcome.changed = true;
    BOOL installReboot = FALSE;
    const DWORD installFlags = downgrade ? DIIRFLAG_FORCE_INF : 0;
    if (!DiInstallDriverW(nullptr, candidate.infPath.c_str(), installFlags, &installReboot)) {
        const DWORD installCode = GetLastError();
        const Error installError{installCode, L"install-driver-package", FormatError(installCode)};
        Error rollbackError;
        bool rollbackReboot = false;
        if (RollbackInstall(prior, &rollbackReboot, &rollbackError)) {
            outcome.rollback = L"succeeded";
            outcome.rebootRequired = rollbackReboot;
            outcome.error = installError;
            return outcome;
        }
        outcome.rollback = L"failed";
        outcome.rebootRequired = rollbackReboot;
        outcome.error = std::move(rollbackError);
        outcome.exitCode = ExitCode::RollbackFailed;
        return outcome;
    }
    outcome.rebootRequired = installReboot != FALSE;

    DeviceInfoSet created;
    SP_DEVINFO_DATA createdData{};
    createdData.cbSize = sizeof(createdData);
    bool createdHere = false;
    if (prior.devices.empty()) {
        GUID classGuid{};
        wchar_t className[MAX_CLASS_NAME_LEN]{};
        if (!SetupDiGetINFClassW(candidate.infPath.c_str(), &classGuid, className, MAX_CLASS_NAME_LEN, nullptr)) {
            SetLastErrorDetail(&outcome.error, L"candidate-inf-class");
        } else {
            if (RegisterRootDevice(classGuid, className, &created, &createdData, &outcome.error)) {
                createdHere = true;
                BOOL bindReboot = FALSE;
                const DWORD bindFlags = downgrade ? INSTALLFLAG_FORCE : 0;
                if (UpdateDriverForPlugAndPlayDevicesW(
                        nullptr, kHardwareId, candidate.infPath.c_str(), bindFlags, &bindReboot)) {
                    outcome.rebootRequired = outcome.rebootRequired || bindReboot != FALSE;
                    outcome.error = {};
                } else {
                    SetLastErrorDetail(&outcome.error, L"bind-root-devnode");
                }
            }
        }
    }

    PackageInfo publishedCandidate;
    if (outcome.error.code == ERROR_SUCCESS &&
        !FindPublishedCandidate(candidate, &publishedCandidate, &outcome.error)) {
        // Candidate inventory recorded the exact failure.
    }
    if (outcome.error.code == ERROR_SUCCESS &&
        !VerifyInstalled(candidate, publishedCandidate.publishedName, outcome.rebootRequired, &outcome.error)) {
        // Verification recorded the exact failure.
    }
    if (outcome.error.code != ERROR_SUCCESS) {
        const Error installError = outcome.error;
        Error rollbackError;
        bool rollbackReboot = outcome.rebootRequired;
        if (createdHere) {
            Error cleanupError;
            if (!RemoveDevice(created.get(), createdData, &rollbackReboot, &cleanupError)) {
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
            return outcome;
        }
        outcome.rollback = L"failed";
        outcome.rebootRequired = rollbackReboot;
        outcome.error = std::move(rollbackError);
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
};

class BackupDirectory final {
public:
    ~BackupDirectory() {
        if (!path_.empty()) {
            std::error_code ignored;
            std::filesystem::remove_all(path_, ignored);
        }
    }

    bool Create(Error* error) {
        std::vector<wchar_t> temp(MAX_PATH);
        const DWORD length = GetTempPathW(static_cast<DWORD>(temp.size()), temp.data());
        if (length == 0 || static_cast<size_t>(length) >= temp.size()) {
            return SetLastErrorDetail(error, L"rollback-backup-root");
        }
        wchar_t candidate[MAX_PATH]{};
        if (!GetTempFileNameW(temp.data(), L"VUC", 0, candidate)) {
            return SetLastErrorDetail(error, L"rollback-backup-root");
        }
        DeleteFileW(candidate);
        if (!CreateDirectoryW(candidate, nullptr)) {
            return SetLastErrorDetail(error, L"rollback-backup-root");
        }
        path_ = candidate;
        return true;
    }

    const std::filesystem::path& path() const noexcept { return path_; }

private:
    std::filesystem::path path_;
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
        std::error_code copyError;
        std::filesystem::create_directory(destination, copyError);
        if (copyError) {
            return SetError(error, L"rollback-backup-create", static_cast<DWORD>(copyError.value()));
        }
        for (std::filesystem::recursive_directory_iterator iterator(storeInf.parent_path(), copyError), end;
             iterator != end && !copyError; iterator.increment(copyError)) {
            const std::filesystem::path relative =
                std::filesystem::relative(iterator->path(), storeInf.parent_path(), copyError);
            if (copyError) break;
            const std::filesystem::path target = destination / relative;
            if (iterator->is_directory()) {
                std::filesystem::create_directories(target, copyError);
            } else if (iterator->is_regular_file()) {
                std::filesystem::create_directories(target.parent_path(), copyError);
                if (!copyError) {
                    std::filesystem::copy_file(iterator->path(), target,
                        std::filesystem::copy_options::overwrite_existing, copyError);
                }
            }
        }
        if (copyError) {
            return SetError(error, L"rollback-backup-copy", static_cast<DWORD>(copyError.value()));
        }
        std::filesystem::path signerCatalog;
        if (!VerifyInfSignature(packages[index].infPath, &signerCatalog, error)) {
            return false;
        }
        const std::filesystem::path adjacentCatalog = destination / kCatalogName;
        const bool hasAdjacentCatalog = std::filesystem::is_regular_file(adjacentCatalog, copyError);
        if (copyError) {
            return SetError(error, L"rollback-backup-catalog", static_cast<DWORD>(copyError.value()));
        }
        if (!hasAdjacentCatalog) {
            if (signerCatalog.empty() || !std::filesystem::is_regular_file(signerCatalog, copyError)) {
                return SetError(error, L"rollback-backup-catalog", ERROR_FILE_NOT_FOUND,
                    L"cannot construct a self-contained signed rollback package");
            }
            std::filesystem::copy_file(signerCatalog, adjacentCatalog,
                std::filesystem::copy_options::overwrite_existing, copyError);
            if (copyError) {
                return SetError(error, L"rollback-backup-catalog", static_cast<DWORD>(copyError.value()));
            }
        }
        const std::filesystem::path backupInf = destination / storeInf.filename();
        PackageInfo verified;
        bool owned = false;
        if (!LoadOwnedPackage(backupInf, true, &verified, &owned, error) || !owned) {
            return false;
        }
        backups->push_back(PackageBackup{packages[index], destination, backupInf});
    }
    return true;
}

bool RollbackRemove(
    const Snapshot& prior,
    const std::vector<PackageBackup>& backups,
    bool* rebootRequired,
    Error* error) {
    for (const PackageBackup& backup : backups) {
        BOOL reboot = FALSE;
        if (!DiInstallDriverW(nullptr, backup.infPath.c_str(), 0, &reboot)) {
            return SetLastErrorDetail(error, L"remove-rollback-package");
        }
        *rebootRequired = *rebootRequired || reboot != FALSE;
    }
    bool removalReboot = false;
    if (!RemoveAllExactDevices(&removalReboot, error)) {
        return false;
    }
    *rebootRequired = *rebootRequired || removalReboot;
    if (!prior.devices.empty()) {
        const auto iterator = std::find_if(backups.begin(), backups.end(), [&](const PackageBackup& backup) {
            return _wcsicmp(backup.original.publishedName.c_str(),
                prior.devices[0].publishedInf.c_str()) == 0;
        });
        if (iterator == backups.end()) {
            return SetError(error, L"remove-rollback-binding", ERROR_NOT_FOUND);
        }
        GUID classGuid{};
        wchar_t className[MAX_CLASS_NAME_LEN]{};
        if (!SetupDiGetINFClassW(iterator->infPath.c_str(), &classGuid, className, MAX_CLASS_NAME_LEN, nullptr)) {
            return SetLastErrorDetail(error, L"remove-rollback-inf-class");
        }
        DeviceInfoSet created;
        SP_DEVINFO_DATA createdData{};
        createdData.cbSize = sizeof(createdData);
        if (!RegisterRootDevice(classGuid, className, &created, &createdData, error)) {
            return false;
        }
        BOOL reboot = FALSE;
        if (!UpdateDriverForPlugAndPlayDevicesW(
                nullptr, kHardwareId, iterator->infPath.c_str(), INSTALLFLAG_FORCE, &reboot)) {
            return SetLastErrorDetail(error, L"remove-rollback-binding");
        }
        *rebootRequired = *rebootRequired || reboot != FALSE;
    }
    if (!*rebootRequired) {
        Snapshot restored;
        if (!CaptureSnapshot(&restored, error)) {
            return false;
        }
        std::multiset<std::pair<Version, std::string>> expectedPackages;
        std::multiset<std::pair<Version, std::string>> actualPackages;
        for (const PackageInfo& package : prior.packages) {
            expectedPackages.emplace(package.version, package.infSha256);
        }
        for (const PackageInfo& package : restored.packages) {
            actualPackages.emplace(package.version, package.infSha256);
        }
        if (expectedPackages != actualPackages || restored.devices.size() != prior.devices.size()) {
            return SetError(error, L"remove-rollback-verification", ERROR_REVISION_MISMATCH,
                L"rollback did not restore the exact prior package and devnode set");
        }
        if (!prior.devices.empty()) {
            if (restored.devices[0].package.infSha256 != prior.devices[0].package.infSha256) {
                return SetError(error, L"remove-rollback-verification", ERROR_REVISION_MISMATCH,
                    L"rollback restored a different active package");
            }
            if (!VerifyAbiHealth(error)) {
                return false;
            }
        }
    }
    return true;
}

Outcome Remove() {
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
    Snapshot prior;
    if (!CaptureSnapshot(&prior, &outcome.error)) {
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
    if (!BackupPackages(prior.packages, &backupRoot, &backups, &outcome.error)) {
        outcome.exitCode = ExitCode::PreflightRejected;
        return outcome;
    }
    outcome.changed = true;
    bool reboot = false;
    Error mutationError;
    bool mutationSucceeded = RemoveAllExactDevices(&reboot, &mutationError);
    if (mutationSucceeded) {
        for (const PackageInfo& package : prior.packages) {
            if (!UninstallPackage(package, &reboot, &mutationError)) {
                mutationSucceeded = false;
                break;
            }
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
        Error rollbackError;
        bool rollbackReboot = reboot;
        if (RollbackRemove(prior, backups, &rollbackReboot, &rollbackError)) {
            outcome.rollback = L"succeeded";
            outcome.rebootRequired = rollbackReboot;
            outcome.error = mutationError;
            return outcome;
        }
        outcome.rollback = L"failed";
        outcome.rebootRequired = rollbackReboot;
        outcome.error = std::move(rollbackError);
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
    Version one{};
    Version two{};
    if (!ParseVersion(L"1.2.3.4", &one) || !ParseVersion(L"1.2.4.0", &two) ||
        !(one < two) || ParseVersion(L"1.2.3", nullptr) ||
        ParseVersion(L"1.2.3.70000", nullptr)) {
        SetError(&outcome.error, L"self-test-version", ERROR_INVALID_DATA);
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
    outcome.success = true;
    outcome.exitCode = ExitCode::Success;
    return outcome;
}

bool ParseInstallOptions(int argc, wchar_t** argv, InstallOptions* options, Error* error) {
    if (argc < 8) {
        return SetError(error, L"arguments", ERROR_INVALID_PARAMETER);
    }
    options->infPath = argv[2];
    bool manifestSeen = false;
    bool revisionSeen = false;
    bool modeSeen = false;
    for (int index = 3; index < argc; ++index) {
        const std::wstring argument = argv[index];
        if (_wcsicmp(argument.c_str(), L"--manifest") == 0 && index + 1 < argc && !manifestSeen) {
            options->manifestPath = argv[++index];
            manifestSeen = true;
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
                    L"source revision must contain 40 to 64 hexadecimal characters");
            }
            revisionSeen = true;
        } else if (_wcsicmp(argument.c_str(), L"--validation-mode") == 0 &&
            index + 1 < argc && !modeSeen) {
            const std::wstring mode = argv[++index];
            if (_wcsicmp(mode.c_str(), L"production") == 0) {
                options->production = true;
            } else if (_wcsicmp(mode.c_str(), L"controlled-test") == 0) {
                options->production = false;
            } else {
                return SetError(error, L"arguments", ERROR_INVALID_PARAMETER,
                    L"validation mode must be production or controlled-test");
            }
            modeSeen = true;
        } else if (_wcsicmp(argument.c_str(), L"--allow-controlled-downgrade") == 0 &&
            index + 1 < argc && !options->expectedDowngradeFrom) {
            Version expected{};
            if (!ParseVersion(argv[++index], &expected)) {
                return SetError(error, L"arguments", ERROR_INVALID_PARAMETER,
                    L"controlled downgrade requires the exact installed four-part version");
            }
            options->expectedDowngradeFrom = expected;
        } else {
            return SetError(error, L"arguments", ERROR_INVALID_PARAMETER,
                L"unknown, duplicate, or incomplete install option");
        }
    }
    if (!manifestSeen || !revisionSeen || !modeSeen) {
        return SetError(error, L"arguments", ERROR_INVALID_PARAMETER,
            L"manifest, source revision, and validation mode are all required");
    }
    return true;
}

void Usage() {
    std::wcerr
        << L"usage:\n"
        << L"  ViiperUdeCtl.exe install <ViiperUde.inf> --manifest <submission.json> "
           L"--source-revision <40-64 hex> --validation-mode <production|controlled-test> "
           L"[--allow-controlled-downgrade <exact-installed-version>]\n"
        << L"  ViiperUdeCtl.exe remove\n"
        << L"  ViiperUdeCtl.exe status\n"
        << L"  ViiperUdeCtl.exe self-test\n";
}

} // namespace

int wmain(int argc, wchar_t** argv) {
    if (argc >= 3 && _wcsicmp(argv[1], L"install") == 0) {
        InstallOptions options;
        Error argumentError;
        if (!ParseInstallOptions(argc, argv, &options, &argumentError)) {
            Usage();
            Outcome outcome;
            outcome.error = std::move(argumentError);
            outcome.exitCode = ExitCode::Usage;
            EmitOutcome(L"install", outcome);
            return static_cast<int>(outcome.exitCode);
        }
        Outcome outcome = Install(options);
        EmitOutcome(L"install", outcome);
        return static_cast<int>(outcome.exitCode);
    }
    if (argc == 2 && _wcsicmp(argv[1], L"remove") == 0) {
        Outcome outcome = Remove();
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
