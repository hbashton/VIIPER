/*
 * Copyright (c) 2026 VIIPER Project contributors
 *
 * Root-devnode creation follows the SetupAPI sequence documented by the
 * Microsoft DevCon sample and usbip-win2's BSD-2-Clause devnode utility.
 * See ../THIRD_PARTY_NOTICES.md.
 */

#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <cfgmgr32.h>
#include <devguid.h>
#include <newdev.h>
#include <setupapi.h>

#include <algorithm>
#include <cwchar>
#include <filesystem>
#include <iostream>
#include <iterator>
#include <string>
#include <vector>

#pragma comment(lib, "Cfgmgr32.lib")
#pragma comment(lib, "Newdev.lib")
#pragma comment(lib, "Setupapi.lib")
#pragma comment(lib, "Advapi32.lib")

namespace {

constexpr wchar_t kHardwareId[] = L"ROOT\\VIIPER\\UDE";
constexpr wchar_t kEnumerator[] = L"ROOT";

class DeviceInfoSet final {
public:
    explicit DeviceInfoSet(HDEVINFO value) noexcept : value_(value) {}
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
    while (!message.empty() && (message.back() == L'\r' || message.back() == L'\n' ||
        message.back() == L' ' || message.back() == L'.')) {
        message.pop_back();
    }
    return message;
}

bool Fail(const wchar_t* operation, DWORD error = GetLastError()) {
    std::wcerr << L"error: " << operation << L" failed (" << error << L"): "
               << FormatError(error) << L"\n";
    return false;
}

bool IsElevated() {
    HANDLE token = nullptr;
    if (!OpenProcessToken(GetCurrentProcess(), TOKEN_QUERY, &token)) {
        return false;
    }
    TOKEN_ELEVATION elevation{};
    DWORD returned = 0;
    const BOOL ok = GetTokenInformation(
        token, TokenElevation, &elevation, sizeof(elevation), &returned);
    CloseHandle(token);
    return ok && elevation.TokenIsElevated != 0;
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

struct DeviceMatch {
    SP_DEVINFO_DATA data{sizeof(SP_DEVINFO_DATA)};
    bool started = false;
    ULONG problem = 0;
};

std::vector<DeviceMatch> FindDevices(HDEVINFO set) {
    std::vector<DeviceMatch> matches;
    for (DWORD index = 0;; ++index) {
        SP_DEVINFO_DATA data{sizeof(SP_DEVINFO_DATA)};
        if (!SetupDiEnumDeviceInfo(set, index, &data)) {
            if (GetLastError() != ERROR_NO_MORE_ITEMS) {
                Fail(L"SetupDiEnumDeviceInfo");
            }
            break;
        }
        if (!HasExactHardwareId(set, data)) {
            continue;
        }
        ULONG status = 0;
        ULONG problem = 0;
        const CONFIGRET result = CM_Get_DevNode_Status(&status, &problem, data.DevInst, 0);
        matches.push_back(DeviceMatch{
            data,
            result == CR_SUCCESS && (status & DN_STARTED) != 0 && problem == 0,
            result == CR_SUCCESS ? problem : static_cast<ULONG>(result),
        });
    }
    return matches;
}

DeviceInfoSet OpenRootDevices() {
    return DeviceInfoSet(SetupDiGetClassDevsW(
        nullptr, kEnumerator, nullptr, DIGCF_ALLCLASSES | DIGCF_PRESENT));
}

bool RemoveDevice(HDEVINFO set, SP_DEVINFO_DATA& data, bool* rebootRequired) {
    BOOL reboot = FALSE;
    if (!DiUninstallDevice(nullptr, set, &data, 0, &reboot)) {
        return Fail(L"DiUninstallDevice");
    }
    *rebootRequired = *rebootRequired || reboot != FALSE;
    return true;
}

bool RegisterRootDevice(
    const GUID& classGuid,
    const std::wstring& className,
    DeviceInfoSet& set,
    SP_DEVINFO_DATA* data) {
    set = DeviceInfoSet(SetupDiCreateDeviceInfoList(&classGuid, nullptr));
    if (!set) {
        return Fail(L"SetupDiCreateDeviceInfoList");
    }
    *data = SP_DEVINFO_DATA{sizeof(SP_DEVINFO_DATA)};
    if (!SetupDiCreateDeviceInfoW(
            set.get(), className.c_str(), &classGuid, nullptr, nullptr,
            DICD_GENERATE_ID, data)) {
        return Fail(L"SetupDiCreateDeviceInfo");
    }
    const size_t idChars = std::size(kHardwareId) + 1;
    std::vector<wchar_t> ids(idChars, L'\0');
    std::copy(std::begin(kHardwareId), std::end(kHardwareId), ids.begin());
    if (!SetupDiSetDeviceRegistryPropertyW(
            set.get(), data, SPDRP_HARDWAREID,
            reinterpret_cast<const BYTE*>(ids.data()),
            static_cast<DWORD>(ids.size() * sizeof(wchar_t)))) {
        return Fail(L"SetupDiSetDeviceRegistryProperty(HardwareId)");
    }
    if (!SetupDiCallClassInstaller(DIF_REGISTERDEVICE, set.get(), data)) {
        return Fail(L"SetupDiCallClassInstaller(DIF_REGISTERDEVICE)");
    }
    return true;
}

bool Install(const wchar_t* rawInfPath) {
    if (!IsElevated()) {
        SetLastError(ERROR_ELEVATION_REQUIRED);
        return Fail(L"administrator check");
    }
    std::error_code pathError;
    const std::filesystem::path infPath = std::filesystem::canonical(rawInfPath, pathError);
    if (pathError || !std::filesystem::is_regular_file(infPath)) {
        SetLastError(ERROR_FILE_NOT_FOUND);
        return Fail(L"resolve INF path");
    }

    GUID classGuid{};
    wchar_t className[MAX_CLASS_NAME_LEN]{};
    if (!SetupDiGetINFClassW(
            infPath.c_str(), &classGuid, className, MAX_CLASS_NAME_LEN, nullptr)) {
        return Fail(L"SetupDiGetINFClass");
    }
    if (!IsEqualGUID(classGuid, GUID_DEVCLASS_USB)) {
        SetLastError(ERROR_CLASS_MISMATCH);
        return Fail(L"validate INF class");
    }

    DeviceInfoSet existing = OpenRootDevices();
    if (!existing) {
        return Fail(L"SetupDiGetClassDevs(ROOT)");
    }
    const auto matches = FindDevices(existing.get());
    if (matches.size() > 1) {
        SetLastError(ERROR_DUPLICATE_SERVICE_NAME);
        return Fail(L"validate unique VIIPER UDE controller");
    }

    DeviceInfoSet created(INVALID_HANDLE_VALUE);
    SP_DEVINFO_DATA createdData{sizeof(SP_DEVINFO_DATA)};
    bool createdHere = false;
    if (matches.empty()) {
        if (!RegisterRootDevice(classGuid, className, created, &createdData)) {
            return false;
        }
        createdHere = true;
    }

    BOOL rebootRequired = FALSE;
    if (!UpdateDriverForPlugAndPlayDevicesW(
            nullptr, kHardwareId, infPath.c_str(), INSTALLFLAG_FORCE, &rebootRequired)) {
        const DWORD updateError = GetLastError();
        if (createdHere) {
            bool ignoredReboot = false;
            RemoveDevice(created.get(), createdData, &ignoredReboot);
        }
        return Fail(L"UpdateDriverForPlugAndPlayDevices", updateError);
    }

    DeviceInfoSet verified = OpenRootDevices();
    if (!verified) {
        return Fail(L"reopen VIIPER UDE controller");
    }
    const auto installed = FindDevices(verified.get());
    if (installed.size() != 1) {
        SetLastError(installed.empty() ? ERROR_DEVICE_NOT_AVAILABLE : ERROR_DUPLICATE_SERVICE_NAME);
        return Fail(L"verify installed VIIPER UDE controller");
    }
    if (!rebootRequired && !installed[0].started) {
        std::wcerr << L"error: VIIPER UDE controller did not start; problem="
                   << installed[0].problem << L"\n";
        return false;
    }
    std::wcout << L"installed=1 started=" << (installed[0].started ? 1 : 0)
               << L" rebootRequired=" << (rebootRequired ? 1 : 0) << L"\n";
    return true;
}

bool Remove() {
    if (!IsElevated()) {
        SetLastError(ERROR_ELEVATION_REQUIRED);
        return Fail(L"administrator check");
    }
    DeviceInfoSet set = OpenRootDevices();
    if (!set) {
        return Fail(L"SetupDiGetClassDevs(ROOT)");
    }
    auto matches = FindDevices(set.get());
    bool rebootRequired = false;
    for (auto& match : matches) {
        if (!RemoveDevice(set.get(), match.data, &rebootRequired)) {
            return false;
        }
    }
    DeviceInfoSet verified = OpenRootDevices();
    if (!verified) {
        return Fail(L"verify removed VIIPER UDE controller");
    }
    if (!FindDevices(verified.get()).empty() && !rebootRequired) {
        SetLastError(ERROR_DEVICE_IN_USE);
        return Fail(L"verify removed VIIPER UDE controller");
    }
    std::wcout << L"removed=" << matches.size()
               << L" rebootRequired=" << (rebootRequired ? 1 : 0) << L"\n";
    return true;
}

bool Status() {
    DeviceInfoSet set = OpenRootDevices();
    if (!set) {
        return Fail(L"SetupDiGetClassDevs(ROOT)");
    }
    const auto matches = FindDevices(set.get());
    std::wcout << L"devices=" << matches.size();
    if (matches.size() == 1) {
        std::wcout << L" started=" << (matches[0].started ? 1 : 0)
                   << L" problem=" << matches[0].problem;
    }
    std::wcout << L"\n";
    return matches.size() <= 1;
}

void Usage() {
    std::wcerr << L"usage:\n"
               << L"  ViiperUdeCtl.exe install <absolute-path-to-ViiperUde.inf>\n"
               << L"  ViiperUdeCtl.exe remove\n"
               << L"  ViiperUdeCtl.exe status\n";
}

} // namespace

int wmain(int argc, wchar_t** argv) {
    if (argc == 3 && _wcsicmp(argv[1], L"install") == 0) {
        return Install(argv[2]) ? 0 : 1;
    }
    if (argc == 2 && _wcsicmp(argv[1], L"remove") == 0) {
        return Remove() ? 0 : 1;
    }
    if (argc == 2 && _wcsicmp(argv[1], L"status") == 0) {
        return Status() ? 0 : 1;
    }
    Usage();
    return 2;
}
