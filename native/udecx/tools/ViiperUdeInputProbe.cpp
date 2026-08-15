#define WIN32_LEAN_AND_MEAN
#define NOMINMAX
#include <windows.h>
#include <hidsdi.h>
#include <setupapi.h>

#include <algorithm>
#include <chrono>
#include <cstdint>
#include <filesystem>
#include <fstream>
#include <iostream>
#include <memory>
#include <set>
#include <stdexcept>
#include <string>
#include <thread>
#include <vector>

namespace {

class DeviceInfoSet final {
public:
    explicit DeviceInfoSet(HDEVINFO value = INVALID_HANDLE_VALUE) : value_(value) {}
    ~DeviceInfoSet() {
        if (value_ != INVALID_HANDLE_VALUE) SetupDiDestroyDeviceInfoList(value_);
    }
    DeviceInfoSet(const DeviceInfoSet&) = delete;
    DeviceInfoSet& operator=(const DeviceInfoSet&) = delete;
    HDEVINFO get() const { return value_; }
private:
    HDEVINFO value_;
};

class Handle final {
public:
    explicit Handle(HANDLE value = INVALID_HANDLE_VALUE) : value_(value) {}
    ~Handle() {
        if (value_ != INVALID_HANDLE_VALUE && value_ != nullptr) CloseHandle(value_);
    }
    Handle(const Handle&) = delete;
    Handle& operator=(const Handle&) = delete;
    Handle(Handle&& other) noexcept : value_(other.value_) {
        other.value_ = INVALID_HANDLE_VALUE;
    }
    Handle& operator=(Handle&& other) noexcept {
        if (this != &other) {
            if (value_ != INVALID_HANDLE_VALUE && value_ != nullptr) CloseHandle(value_);
            value_ = other.value_;
            other.value_ = INVALID_HANDLE_VALUE;
        }
        return *this;
    }
    HANDLE get() const { return value_; }
    bool valid() const { return value_ != INVALID_HANDLE_VALUE && value_ != nullptr; }
private:
    HANDLE value_;
};

class PreparsedData final {
public:
    explicit PreparsedData(PHIDP_PREPARSED_DATA value = nullptr) : value_(value) {}
    ~PreparsedData() { if (value_ != nullptr) HidD_FreePreparsedData(value_); }
    PreparsedData(const PreparsedData&) = delete;
    PreparsedData& operator=(const PreparsedData&) = delete;
    PHIDP_PREPARSED_DATA get() const { return value_; }
private:
    PHIDP_PREPARSED_DATA value_;
};

std::string Win32Error(const char* operation) {
    return std::string(operation) + " failed with Win32 error " +
        std::to_string(GetLastError());
}

std::string WideToUtf8(const std::wstring& value) {
    if (value.empty()) return {};
    const int size = WideCharToMultiByte(CP_UTF8, WC_ERR_INVALID_CHARS, value.data(),
        static_cast<int>(value.size()), nullptr, 0, nullptr, nullptr);
    if (size <= 0) throw std::runtime_error(Win32Error("WideCharToMultiByte"));
    std::string result(static_cast<size_t>(size), '\0');
    if (WideCharToMultiByte(CP_UTF8, WC_ERR_INVALID_CHARS, value.data(),
            static_cast<int>(value.size()), result.data(), size, nullptr, nullptr) != size) {
        throw std::runtime_error("WideCharToMultiByte returned a short conversion");
    }
    return result;
}

std::wstring Utf8ToWide(const std::string& value) {
    if (value.empty()) return {};
    const int size = MultiByteToWideChar(CP_UTF8, MB_ERR_INVALID_CHARS, value.data(),
        static_cast<int>(value.size()), nullptr, 0);
    if (size <= 0) throw std::runtime_error(Win32Error("MultiByteToWideChar"));
    std::wstring result(static_cast<size_t>(size), L'\0');
    if (MultiByteToWideChar(CP_UTF8, MB_ERR_INVALID_CHARS, value.data(),
            static_cast<int>(value.size()), result.data(), size) != size) {
        throw std::runtime_error("MultiByteToWideChar returned a short conversion");
    }
    return result;
}

std::set<std::wstring> EnumerateHidPaths() {
    GUID hidGuid{};
    HidD_GetHidGuid(&hidGuid);
    DeviceInfoSet devices(SetupDiGetClassDevsW(
        &hidGuid, nullptr, nullptr, DIGCF_PRESENT | DIGCF_DEVICEINTERFACE));
    if (devices.get() == INVALID_HANDLE_VALUE) {
        throw std::runtime_error(Win32Error("SetupDiGetClassDevs(HID)"));
    }

    std::set<std::wstring> paths;
    for (DWORD index = 0;; ++index) {
        SP_DEVICE_INTERFACE_DATA interfaceData{};
        interfaceData.cbSize = sizeof(interfaceData);
        if (!SetupDiEnumDeviceInterfaces(
                devices.get(), nullptr, &hidGuid, index, &interfaceData)) {
            if (GetLastError() == ERROR_NO_MORE_ITEMS) break;
            throw std::runtime_error(Win32Error("SetupDiEnumDeviceInterfaces(HID)"));
        }

        DWORD required = 0;
        SetupDiGetDeviceInterfaceDetailW(
            devices.get(), &interfaceData, nullptr, 0, &required, nullptr);
        if (GetLastError() != ERROR_INSUFFICIENT_BUFFER ||
            required < sizeof(SP_DEVICE_INTERFACE_DETAIL_DATA_W)) {
            throw std::runtime_error(Win32Error("SetupDiGetDeviceInterfaceDetail(size)"));
        }
        std::vector<std::uint8_t> storage(required);
        auto* detail = reinterpret_cast<SP_DEVICE_INTERFACE_DETAIL_DATA_W*>(storage.data());
        detail->cbSize = sizeof(SP_DEVICE_INTERFACE_DETAIL_DATA_W);
        if (!SetupDiGetDeviceInterfaceDetailW(
                devices.get(), &interfaceData, detail, required, nullptr, nullptr)) {
            throw std::runtime_error(Win32Error("SetupDiGetDeviceInterfaceDetail(HID)"));
        }
        paths.emplace(detail->DevicePath);
    }
    return paths;
}

void WriteSnapshot(const std::filesystem::path& path) {
    std::ofstream output(path, std::ios::binary | std::ios::trunc);
    if (!output) throw std::runtime_error("could not create HID snapshot");
    for (const auto& devicePath : EnumerateHidPaths()) {
        output << WideToUtf8(devicePath) << "\n";
    }
    output.flush();
    if (!output) throw std::runtime_error("could not write HID snapshot");
}

std::set<std::wstring> ReadSnapshot(const std::filesystem::path& path) {
    std::ifstream input(path, std::ios::binary);
    if (!input) throw std::runtime_error("could not open HID snapshot");
    std::set<std::wstring> result;
    std::string line;
    while (std::getline(input, line)) {
        if (!line.empty() && line.back() == '\r') line.pop_back();
        if (!line.empty()) result.emplace(Utf8ToWide(line));
    }
    if (!input.eof()) throw std::runtime_error("could not read HID snapshot");
    return result;
}

struct OpenHid final {
    Handle file;
    USHORT inputReportLength = 0;
    USHORT outputReportLength = 0;
    std::wstring path;
};

std::unique_ptr<OpenHid> TryOpenGamepad(
    const std::wstring& path,
    USHORT vendorId,
    USHORT productId,
    DWORD desiredAccess) {
    Handle file(CreateFileW(path.c_str(), desiredAccess,
        FILE_SHARE_READ | FILE_SHARE_WRITE, nullptr, OPEN_EXISTING,
        FILE_FLAG_OVERLAPPED, nullptr));
    if (!file.valid()) return nullptr;

    HIDD_ATTRIBUTES attributes{};
    attributes.Size = sizeof(attributes);
    if (!HidD_GetAttributes(file.get(), &attributes) ||
        attributes.VendorID != vendorId || attributes.ProductID != productId) {
        return nullptr;
    }

    PHIDP_PREPARSED_DATA rawData = nullptr;
    if (!HidD_GetPreparsedData(file.get(), &rawData)) return nullptr;
    PreparsedData data(rawData);
    HIDP_CAPS caps{};
    if (HidP_GetCaps(data.get(), &caps) != HIDP_STATUS_SUCCESS ||
        caps.UsagePage != 0x01 || caps.Usage != 0x05 || caps.InputReportByteLength == 0) {
        return nullptr;
    }

    auto result = std::make_unique<OpenHid>();
    result->file = std::move(file);
    result->inputReportLength = caps.InputReportByteLength;
    result->outputReportLength = caps.OutputReportByteLength;
    result->path = path;
    return result;
}

std::unique_ptr<OpenHid> WaitForNewGamepad(
    const std::set<std::wstring>& baseline,
    USHORT vendorId,
    USHORT productId,
    DWORD desiredAccess,
    std::chrono::seconds timeout) {
    const auto deadline = std::chrono::steady_clock::now() + timeout;
    do {
        std::unique_ptr<OpenHid> match;
        for (const auto& path : EnumerateHidPaths()) {
            if (baseline.contains(path)) continue;
            auto candidate = TryOpenGamepad(path, vendorId, productId, desiredAccess);
            if (!candidate) continue;
            if (match) {
                throw std::runtime_error(
                    "more than one new matching gamepad collection appeared; refusing an ambiguous latency measurement");
            }
            match = std::move(candidate);
        }
        if (match) return match;
        std::this_thread::sleep_for(std::chrono::milliseconds(50));
    } while (std::chrono::steady_clock::now() < deadline);
    throw std::runtime_error("the virtual HID gamepad collection did not appear before timeout");
}

std::uint32_t ParseUnsigned(const wchar_t* value, const wchar_t* name, std::uint32_t maximum) {
    wchar_t* end = nullptr;
    const unsigned long parsed = wcstoul(value, &end, 0);
    if (value == end || *end != L'\0' || parsed > maximum) {
        throw std::runtime_error("invalid " + WideToUtf8(name));
    }
    return static_cast<std::uint32_t>(parsed);
}

void CancelAndDrainOverlapped(HANDLE file, OVERLAPPED& overlapped) {
    if (!CancelIoEx(file, &overlapped)) {
        const DWORD error = GetLastError();
        if (error != ERROR_NOT_FOUND) {
            throw std::runtime_error(
                "CancelIoEx failed with Win32 error " + std::to_string(error));
        }
    }
    DWORD ignored = 0;
    if (!GetOverlappedResult(file, &overlapped, &ignored, TRUE)) {
        const DWORD error = GetLastError();
        if (error != ERROR_OPERATION_ABORTED && error != ERROR_NOT_FOUND) {
            throw std::runtime_error(
                "draining cancelled overlapped I/O failed with Win32 error " +
                std::to_string(error));
        }
    }
}

int Measure(
    const std::filesystem::path& snapshotPath,
    USHORT vendorId,
    USHORT productId,
    std::size_t markerOffset,
    std::size_t sampleCount) {
    const auto baseline = ReadSnapshot(snapshotPath);
    auto device = WaitForNewGamepad(
        baseline, vendorId, productId, GENERIC_READ, std::chrono::seconds(30));
    if (markerOffset >= device->inputReportLength) {
        throw std::runtime_error("marker offset exceeds the HID input report length");
    }

    Handle event(CreateEventW(nullptr, TRUE, FALSE, nullptr));
    if (!event.valid()) throw std::runtime_error(Win32Error("CreateEvent"));
    std::vector<std::uint8_t> report(device->inputReportLength);
    OVERLAPPED overlapped{};
    overlapped.hEvent = event.get();

    LARGE_INTEGER frequency{};
    if (!QueryPerformanceFrequency(&frequency) || frequency.QuadPart <= 0) {
        throw std::runtime_error(Win32Error("QueryPerformanceFrequency"));
    }
    // Reports already buffered during PnP enumeration predate the producer's
    // timestamp. Flush only this probe handle, then continuously use ReadFile
    // as prescribed by HIDClass rather than polling HidD_GetInputReport.
    if (!HidD_FlushQueue(device->file.get())) {
        throw std::runtime_error(Win32Error("HidD_FlushQueue"));
    }
    std::cout << "READY " << frequency.QuadPart << " "
              << device->inputReportLength << " " << WideToUtf8(device->path) << "\n";
    std::cout.flush();

    std::size_t matches = 0;
    int previousMarker = -1;
    const auto deadline = std::chrono::steady_clock::now() + std::chrono::seconds(30);
    while (matches < sampleCount && std::chrono::steady_clock::now() < deadline) {
        ResetEvent(event.get());
        std::fill(report.begin(), report.end(), std::uint8_t{0});
        DWORD transferred = 0;
        BOOL completed = ReadFile(device->file.get(), report.data(),
            static_cast<DWORD>(report.size()), &transferred, &overlapped);
        if (!completed) {
            const DWORD error = GetLastError();
            if (error != ERROR_IO_PENDING) {
                throw std::runtime_error(Win32Error("ReadFile(HID)"));
            }
            DWORD wait = WAIT_TIMEOUT;
            while (wait == WAIT_TIMEOUT && std::chrono::steady_clock::now() < deadline) {
                const auto remaining = std::chrono::duration_cast<std::chrono::milliseconds>(
                    deadline - std::chrono::steady_clock::now());
                const DWORD waitMilliseconds = remaining.count() <= 0
                    ? 0
                    : static_cast<DWORD>(std::min<std::int64_t>(remaining.count(), 1000));
                wait = WaitForSingleObject(event.get(), waitMilliseconds);
            }
            if (wait == WAIT_TIMEOUT) {
                CancelAndDrainOverlapped(device->file.get(), overlapped);
                break;
            }
            if (wait != WAIT_OBJECT_0 ||
                !GetOverlappedResult(device->file.get(), &overlapped, &transferred, FALSE)) {
                throw std::runtime_error(Win32Error("GetOverlappedResult(HID)"));
            }
        }
        if (transferred <= markerOffset) continue;
        const int marker = report[markerOffset];
        if ((marker != 0xFD && marker != 0xFE) || marker == previousMarker) continue;
        previousMarker = marker;
        LARGE_INTEGER observed{};
        QueryPerformanceCounter(&observed);
        std::cout << "MATCH " << marker << " " << observed.QuadPart << "\n";
        std::cout.flush();
        ++matches;
    }
    if (matches != sampleCount) {
        throw std::runtime_error("timed out before observing every unique input marker");
    }
    return 0;
}

std::vector<std::uint8_t> BuildFeedbackReport(
    const std::wstring& controllerKind,
    USHORT outputReportLength) {
    if (_wcsicmp(controllerKind.c_str(), L"dualshock4") == 0) {
        if (outputReportLength != 32) {
            throw std::runtime_error(
                "DualShock 4 HID output report length is not the expected 32 bytes");
        }
        std::vector<std::uint8_t> report(outputReportLength);
        report[0] = 0x05;
        report[4] = 0x23;
        report[5] = 0xA7;
        report[6] = 0x11;
        report[7] = 0x52;
        report[8] = 0xC3;
        report[9] = 0x04;
        report[10] = 0x09;
        return report;
    }
    if (_wcsicmp(controllerKind.c_str(), L"dualsense") == 0 ||
        _wcsicmp(controllerKind.c_str(), L"dualsense-edge") == 0) {
        if (outputReportLength != 48) {
            throw std::runtime_error(
                "DualSense HID output report length is not the expected 48 bytes");
        }
        std::vector<std::uint8_t> report(outputReportLength);
        report[0] = 0x02;
        report[1] = 0x0F; // compatible rumble and both adaptive triggers
        report[2] = 0x14; // player LEDs and lightbar
        report[3] = 0x22;
        report[4] = 0x88;
        report[11] = 0x21;
        report[12] = 0xFC;
        report[13] = 0x03;
        report[20] = 0x44;
        report[22] = 0x25;
        report[23] = 0x40;
        report[24] = 0x05;
        report[31] = 0x55;
        report[44] = 0x24;
        report[45] = 0x11;
        report[46] = 0x52;
        report[47] = 0xC3;
        return report;
    }
    throw std::runtime_error("unsupported feedback controller kind");
}

int SendFeedback(
    const std::filesystem::path& snapshotPath,
    USHORT vendorId,
    USHORT productId,
    const std::wstring& controllerKind) {
    const auto baseline = ReadSnapshot(snapshotPath);
    auto device = WaitForNewGamepad(
        baseline, vendorId, productId, GENERIC_READ | GENERIC_WRITE,
        std::chrono::seconds(30));
    if (device->outputReportLength == 0) {
        throw std::runtime_error("the virtual HID gamepad has no output report");
    }
    auto report = BuildFeedbackReport(controllerKind, device->outputReportLength);

    Handle event(CreateEventW(nullptr, TRUE, FALSE, nullptr));
    if (!event.valid()) throw std::runtime_error(Win32Error("CreateEvent"));
    OVERLAPPED overlapped{};
    overlapped.hEvent = event.get();
    DWORD transferred = 0;
    BOOL completed = WriteFile(device->file.get(), report.data(),
        static_cast<DWORD>(report.size()), &transferred, &overlapped);
    if (!completed) {
        const DWORD error = GetLastError();
        if (error != ERROR_IO_PENDING) {
            throw std::runtime_error(Win32Error("WriteFile(HID output)"));
        }
        const DWORD wait = WaitForSingleObject(event.get(), 10000);
        if (wait == WAIT_TIMEOUT) {
            CancelAndDrainOverlapped(device->file.get(), overlapped);
            throw std::runtime_error("timed out writing the HID output report");
        }
        if (wait != WAIT_OBJECT_0 ||
            !GetOverlappedResult(device->file.get(), &overlapped, &transferred, FALSE)) {
            throw std::runtime_error(Win32Error("GetOverlappedResult(HID output)"));
        }
    }
    if (transferred != static_cast<DWORD>(report.size())) {
        throw std::runtime_error("the HID output report completed with a short write");
    }
    std::cout << "WROTE " << transferred << " " << WideToUtf8(device->path) << "\n";
    return 0;
}

} // namespace

int wmain(int argc, wchar_t** argv) {
    try {
        if (argc == 3 && _wcsicmp(argv[1], L"snapshot") == 0) {
            WriteSnapshot(argv[2]);
            return 0;
        }
        if (argc == 8 && _wcsicmp(argv[1], L"measure") == 0) {
            const auto vendorId = static_cast<USHORT>(ParseUnsigned(argv[3], L"vendor ID", 0xFFFF));
            const auto productId = static_cast<USHORT>(ParseUnsigned(argv[4], L"product ID", 0xFFFF));
            const auto offset = static_cast<std::size_t>(ParseUnsigned(argv[5], L"marker offset", 4095));
            const auto samples = static_cast<std::size_t>(ParseUnsigned(argv[6], L"sample count", 10000));
            // argv[7] is a versioned invocation token. Requiring it catches a
            // stale helper copied from a different native ABI package.
            if (wcscmp(argv[7], L"qpc-v1") != 0) {
                throw std::runtime_error("unsupported latency probe contract");
            }
            if (samples == 0) throw std::runtime_error("sample count must be nonzero");
            return Measure(argv[2], vendorId, productId, offset, samples);
        }
        if (argc == 7 && _wcsicmp(argv[1], L"feedback") == 0) {
            const auto vendorId = static_cast<USHORT>(ParseUnsigned(argv[3], L"vendor ID", 0xFFFF));
            const auto productId = static_cast<USHORT>(ParseUnsigned(argv[4], L"product ID", 0xFFFF));
            if (wcscmp(argv[6], L"hid-output-v1") != 0) {
                throw std::runtime_error("unsupported HID output probe contract");
            }
            return SendFeedback(argv[2], vendorId, productId, argv[5]);
        }
        std::wcerr << L"Usage:\n"
                   << L"  ViiperUdeInputProbe.exe snapshot <snapshot-path>\n"
                   << L"  ViiperUdeInputProbe.exe measure <snapshot-path> <vid> <pid> <offset> <samples> qpc-v1\n"
                   << L"  ViiperUdeInputProbe.exe feedback <snapshot-path> <vid> <pid> <controller-kind> hid-output-v1\n";
        return 2;
    } catch (const std::exception& error) {
        std::cerr << "VIIPER UDE input probe failed: " << error.what() << "\n";
        return 1;
    }
}
