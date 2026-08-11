#define WIN32_LEAN_AND_MEAN
#define NOMINMAX
#include <windows.h>
#include <audioclient.h>
#include <mmdeviceapi.h>
#include <ksmedia.h>

#include <algorithm>
#include <atomic>
#include <chrono>
#include <cmath>
#include <cstdio>
#include <cstdint>
#include <cwchar>
#include <filesystem>
#include <fstream>
#include <iostream>
#include <iterator>
#include <memory>
#include <set>
#include <stdexcept>
#include <string>
#include <thread>
#include <vector>

namespace {

constexpr double kPi = 3.14159265358979323846;

template <typename T>
class ComPtr final {
public:
    ComPtr() = default;
    ~ComPtr() { reset(); }
    ComPtr(const ComPtr&) = delete;
    ComPtr& operator=(const ComPtr&) = delete;
    ComPtr(ComPtr&& other) noexcept : value_(other.value_) { other.value_ = nullptr; }
    ComPtr& operator=(ComPtr&& other) noexcept {
        if (this != &other) {
            reset();
            value_ = other.value_;
            other.value_ = nullptr;
        }
        return *this;
    }
    T* get() const { return value_; }
    T** put() {
        reset();
        return &value_;
    }
    T* operator->() const { return value_; }
    explicit operator bool() const { return value_ != nullptr; }
    void reset() {
        if (value_ != nullptr) {
            value_->Release();
            value_ = nullptr;
        }
    }
private:
    T* value_ = nullptr;
};

class Handle final {
public:
    explicit Handle(HANDLE value = nullptr) : value_(value) {}
    ~Handle() { if (value_ != nullptr) CloseHandle(value_); }
    Handle(const Handle&) = delete;
    Handle& operator=(const Handle&) = delete;
    HANDLE get() const { return value_; }
private:
    HANDLE value_;
};

class ComApartment final {
public:
    ComApartment() {
        const HRESULT result = CoInitializeEx(nullptr, COINIT_MULTITHREADED);
        if (FAILED(result)) {
            throw std::runtime_error("CoInitializeEx failed: 0x" + hex(result));
        }
        initialized_ = true;
    }
    ~ComApartment() { if (initialized_) CoUninitialize(); }
    static std::string hex(HRESULT value) {
        char buffer[16]{};
        sprintf_s(buffer, "%08lX", static_cast<unsigned long>(value));
        return buffer;
    }
private:
    bool initialized_ = false;
};

[[noreturn]] void ThrowHRESULT(const char* operation, HRESULT result) {
    throw std::runtime_error(std::string(operation) + " failed: 0x" + ComApartment::hex(result));
}

void CheckHRESULT(const char* operation, HRESULT result) {
    if (FAILED(result)) ThrowHRESULT(operation, result);
}

std::string WideToUtf8(const std::wstring& value) {
    if (value.empty()) return {};
    const int size = WideCharToMultiByte(CP_UTF8, WC_ERR_INVALID_CHARS, value.data(),
        static_cast<int>(value.size()), nullptr, 0, nullptr, nullptr);
    if (size <= 0) throw std::runtime_error("WideCharToMultiByte failed");
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
    if (size <= 0) throw std::runtime_error("MultiByteToWideChar failed");
    std::wstring result(static_cast<size_t>(size), L'\0');
    if (MultiByteToWideChar(CP_UTF8, MB_ERR_INVALID_CHARS, value.data(),
            static_cast<int>(value.size()), result.data(), size) != size) {
        throw std::runtime_error("MultiByteToWideChar returned a short conversion");
    }
    return result;
}

struct EndpointSet {
    std::set<std::wstring> render;
    std::set<std::wstring> capture;
};

struct MediaFormat final {
    DWORD sampleRate = 0;
    WORD channels = 0;
};

struct RenderStats final {
    uint64_t frames = 0;
    uint64_t bufferFrames = 0;
    uint64_t events = 0;
    uint64_t underruns = 0;
    double maximumEventGapMilliseconds = 0.0;
    MediaFormat format{};
};

struct CaptureStats final {
    uint64_t frames = 0;
    uint64_t nonSilentFrames = 0;
    uint64_t packets = 0;
    uint64_t discontinuities = 0;
    uint64_t timestampErrors = 0;
    uint64_t positionRegressions = 0;
    uint64_t qpcRegressions = 0;
    double maximumEventGapMilliseconds = 0.0;
    MediaFormat format{};
};

struct ExpectedMediaFormat final {
    DWORD renderSampleRate = 0;
    WORD renderChannels = 0;
    DWORD captureSampleRate = 0;
    WORD captureChannels = 0;
};

ExpectedMediaFormat ExpectedFormatFor(const std::wstring& controller) {
    if (_wcsicmp(controller.c_str(), L"dualsense") == 0 ||
        _wcsicmp(controller.c_str(), L"dualsenseedge") == 0) {
        return ExpectedMediaFormat{48000, 4, 48000, 2};
    }
    if (_wcsicmp(controller.c_str(), L"dualshock4") == 0) {
        return ExpectedMediaFormat{32000, 2, 16000, 1};
    }
    throw std::runtime_error("unsupported controller media contract: " + WideToUtf8(controller));
}

double EventGapMilliseconds(std::chrono::steady_clock::time_point previous,
    std::chrono::steady_clock::time_point current) {
    return std::chrono::duration<double, std::milli>(current - previous).count();
}

std::set<std::wstring> Enumerate(EDataFlow flow) {
    ComApartment apartment;
    ComPtr<IMMDeviceEnumerator> enumerator;
    CheckHRESULT("CoCreateInstance(MMDeviceEnumerator)", CoCreateInstance(
        __uuidof(MMDeviceEnumerator), nullptr, CLSCTX_INPROC_SERVER,
        __uuidof(IMMDeviceEnumerator), reinterpret_cast<void**>(enumerator.put())));
    ComPtr<IMMDeviceCollection> collection;
    CheckHRESULT("EnumAudioEndpoints", enumerator->EnumAudioEndpoints(
        flow, DEVICE_STATE_ACTIVE, collection.put()));
    UINT count = 0;
    CheckHRESULT("IMMDeviceCollection::GetCount", collection->GetCount(&count));
    std::set<std::wstring> result;
    for (UINT index = 0; index < count; ++index) {
        ComPtr<IMMDevice> device;
        CheckHRESULT("IMMDeviceCollection::Item", collection->Item(index, device.put()));
        LPWSTR id = nullptr;
        CheckHRESULT("IMMDevice::GetId", device->GetId(&id));
        result.emplace(id);
        CoTaskMemFree(id);
    }
    return result;
}

EndpointSet EnumerateEndpoints() {
    return EndpointSet{Enumerate(eRender), Enumerate(eCapture)};
}

void WriteSnapshot(const std::filesystem::path& path, const EndpointSet& endpoints) {
    std::ofstream output(path, std::ios::binary | std::ios::trunc);
    if (!output) throw std::runtime_error("could not create endpoint snapshot");
    for (const auto& id : endpoints.render) output << "R\t" << WideToUtf8(id) << "\n";
    for (const auto& id : endpoints.capture) output << "C\t" << WideToUtf8(id) << "\n";
    output.flush();
    if (!output) throw std::runtime_error("could not write endpoint snapshot");
}

EndpointSet ReadSnapshot(const std::filesystem::path& path) {
    std::ifstream input(path, std::ios::binary);
    if (!input) throw std::runtime_error("could not open endpoint snapshot");
    EndpointSet result;
    std::string line;
    while (std::getline(input, line)) {
        if (!line.empty() && line.back() == '\r') line.pop_back();
        if (line.size() < 3 || line[1] != '\t') {
            throw std::runtime_error("invalid endpoint snapshot record");
        }
        auto& destination = line[0] == 'R' ? result.render : result.capture;
        if (line[0] != 'R' && line[0] != 'C') {
            throw std::runtime_error("invalid endpoint snapshot flow");
        }
        destination.insert(Utf8ToWide(line.substr(2)));
    }
    return result;
}

std::vector<std::wstring> Difference(const std::set<std::wstring>& current,
    const std::set<std::wstring>& baseline) {
    std::vector<std::wstring> result;
    std::set_difference(current.begin(), current.end(), baseline.begin(), baseline.end(),
        std::back_inserter(result));
    return result;
}

ComPtr<IMMDevice> OpenEndpoint(const std::wstring& endpointId) {
    ComPtr<IMMDeviceEnumerator> enumerator;
    CheckHRESULT("CoCreateInstance(MMDeviceEnumerator)", CoCreateInstance(
        __uuidof(MMDeviceEnumerator), nullptr, CLSCTX_INPROC_SERVER,
        __uuidof(IMMDeviceEnumerator), reinterpret_cast<void**>(enumerator.put())));
    ComPtr<IMMDevice> result;
    CheckHRESULT("IMMDeviceEnumerator::GetDevice", enumerator->GetDevice(
        endpointId.c_str(), result.put()));
    return result;
}

bool IsFloatFormat(const WAVEFORMATEX* format) {
    if (format->wFormatTag == WAVE_FORMAT_IEEE_FLOAT) return true;
    if (format->wFormatTag != WAVE_FORMAT_EXTENSIBLE ||
        format->cbSize < sizeof(WAVEFORMATEXTENSIBLE) - sizeof(WAVEFORMATEX)) return false;
    const auto* extensible = reinterpret_cast<const WAVEFORMATEXTENSIBLE*>(format);
    return IsEqualGUID(extensible->SubFormat, KSDATAFORMAT_SUBTYPE_IEEE_FLOAT) != FALSE;
}

bool IsPCMFormat(const WAVEFORMATEX* format) {
    if (format->wFormatTag == WAVE_FORMAT_PCM) return true;
    if (format->wFormatTag != WAVE_FORMAT_EXTENSIBLE ||
        format->cbSize < sizeof(WAVEFORMATEXTENSIBLE) - sizeof(WAVEFORMATEX)) return false;
    const auto* extensible = reinterpret_cast<const WAVEFORMATEXTENSIBLE*>(format);
    return IsEqualGUID(extensible->SubFormat, KSDATAFORMAT_SUBTYPE_PCM) != FALSE;
}

void FillTone(BYTE* data, UINT32 frames, const WAVEFORMATEX* format, double& phase) {
    if (format->nChannels == 0 || format->nSamplesPerSec == 0 || format->nBlockAlign == 0) {
        throw std::runtime_error("audio endpoint returned an invalid mix format");
    }
    const UINT32 sampleBytes = format->nBlockAlign / format->nChannels;
    if (sampleBytes == 0 || sampleBytes * format->nChannels != format->nBlockAlign) {
        throw std::runtime_error("audio endpoint returned an unsupported block alignment");
    }
    const bool floating = IsFloatFormat(format);
    const bool pcm = IsPCMFormat(format);
    if (!floating && !pcm) throw std::runtime_error("audio endpoint mix format is neither PCM nor float");
    if ((floating && sampleBytes != 4) || (!floating && sampleBytes != 1 && sampleBytes != 2 &&
            sampleBytes != 3 && sampleBytes != 4)) {
        throw std::runtime_error("audio endpoint mix format has an unsupported sample width");
    }

    constexpr double frequency = 523.251130601;
    constexpr double amplitude = 0.08;
    const double increment = 2.0 * kPi * frequency / format->nSamplesPerSec;
    for (UINT32 frame = 0; frame < frames; ++frame) {
        const double sample = std::sin(phase) * amplitude;
        phase += increment;
        if (phase >= 2.0 * kPi) phase -= 2.0 * kPi;
        for (WORD channel = 0; channel < format->nChannels; ++channel) {
            BYTE* destination = data + static_cast<size_t>(frame) * format->nBlockAlign +
                static_cast<size_t>(channel) * sampleBytes;
            if (floating) {
                *reinterpret_cast<float*>(destination) = static_cast<float>(sample);
            } else if (sampleBytes == 1) {
                destination[0] = static_cast<BYTE>(std::clamp(128.0 + sample * 127.0, 0.0, 255.0));
            } else {
                const auto scaled = static_cast<int64_t>(std::llround(sample *
                    (sampleBytes == 2 ? 32767.0 : sampleBytes == 3 ? 8388607.0 : 2147483647.0)));
                for (UINT32 byte = 0; byte < sampleBytes; ++byte) {
                    destination[byte] = static_cast<BYTE>((scaled >> (byte * 8)) & 0xff);
                }
            }
        }
    }
}

RenderStats ExerciseRender(const std::wstring& endpointId, std::chrono::seconds duration) {
    ComApartment apartment;
    auto device = OpenEndpoint(endpointId);
    ComPtr<IAudioClient> client;
    CheckHRESULT("IMMDevice::Activate(IAudioClient)", device->Activate(
        __uuidof(IAudioClient), CLSCTX_INPROC_SERVER, nullptr,
        reinterpret_cast<void**>(client.put())));
    WAVEFORMATEX* rawFormat = nullptr;
    CheckHRESULT("IAudioClient::GetMixFormat", client->GetMixFormat(&rawFormat));
    std::unique_ptr<WAVEFORMATEX, decltype(&CoTaskMemFree)> format(rawFormat, CoTaskMemFree);
    CheckHRESULT("IAudioClient::Initialize(render)", client->Initialize(
        AUDCLNT_SHAREMODE_SHARED,
        AUDCLNT_STREAMFLAGS_EVENTCALLBACK | AUDCLNT_STREAMFLAGS_NOPERSIST,
        0, 0, format.get(), nullptr));
    Handle event(CreateEventW(nullptr, FALSE, FALSE, nullptr));
    if (event.get() == nullptr) throw std::runtime_error("CreateEvent(render) failed");
    CheckHRESULT("IAudioClient::SetEventHandle(render)", client->SetEventHandle(event.get()));
    ComPtr<IAudioRenderClient> render;
    CheckHRESULT("IAudioClient::GetService(IAudioRenderClient)", client->GetService(
        __uuidof(IAudioRenderClient), reinterpret_cast<void**>(render.put())));
    UINT32 bufferFrames = 0;
    CheckHRESULT("IAudioClient::GetBufferSize(render)", client->GetBufferSize(&bufferFrames));
    BYTE* data = nullptr;
    double phase = 0.0;
    CheckHRESULT("IAudioRenderClient::GetBuffer(prime)", render->GetBuffer(bufferFrames, &data));
    FillTone(data, bufferFrames, format.get(), phase);
    CheckHRESULT("IAudioRenderClient::ReleaseBuffer(prime)", render->ReleaseBuffer(bufferFrames, 0));
    CheckHRESULT("IAudioClient::Start(render)", client->Start());

    RenderStats stats{};
    stats.frames = bufferFrames;
    stats.bufferFrames = bufferFrames;
    stats.format = MediaFormat{format->nSamplesPerSec, format->nChannels};
    auto previousEvent = std::chrono::steady_clock::now();
    bool warmedUp = false;
    const auto deadline = std::chrono::steady_clock::now() + duration;
    while (std::chrono::steady_clock::now() < deadline) {
        const DWORD wait = WaitForSingleObject(event.get(), 2000);
        if (wait != WAIT_OBJECT_0) throw std::runtime_error("render event timed out");
        const auto eventTime = std::chrono::steady_clock::now();
        if (stats.events != 0) {
            stats.maximumEventGapMilliseconds = std::max(
                stats.maximumEventGapMilliseconds,
                EventGapMilliseconds(previousEvent, eventTime));
        }
        previousEvent = eventTime;
        ++stats.events;
        UINT32 padding = 0;
        CheckHRESULT("IAudioClient::GetCurrentPadding", client->GetCurrentPadding(&padding));
        if (padding > bufferFrames) throw std::runtime_error("render padding exceeds buffer size");
        if (warmedUp && padding == 0) ++stats.underruns;
        warmedUp = true;
        const UINT32 available = bufferFrames - padding;
        if (available == 0) continue;
        CheckHRESULT("IAudioRenderClient::GetBuffer", render->GetBuffer(available, &data));
        FillTone(data, available, format.get(), phase);
        CheckHRESULT("IAudioRenderClient::ReleaseBuffer", render->ReleaseBuffer(available, 0));
        stats.frames += available;
    }
    CheckHRESULT("IAudioClient::Stop(render)", client->Stop());
    return stats;
}

CaptureStats ExerciseCapture(const std::wstring& endpointId, std::chrono::seconds duration) {
    ComApartment apartment;
    auto device = OpenEndpoint(endpointId);
    ComPtr<IAudioClient> client;
    CheckHRESULT("IMMDevice::Activate(IAudioClient)", device->Activate(
        __uuidof(IAudioClient), CLSCTX_INPROC_SERVER, nullptr,
        reinterpret_cast<void**>(client.put())));
    WAVEFORMATEX* rawFormat = nullptr;
    CheckHRESULT("IAudioClient::GetMixFormat(capture)", client->GetMixFormat(&rawFormat));
    std::unique_ptr<WAVEFORMATEX, decltype(&CoTaskMemFree)> format(rawFormat, CoTaskMemFree);
    CheckHRESULT("IAudioClient::Initialize(capture)", client->Initialize(
        AUDCLNT_SHAREMODE_SHARED,
        AUDCLNT_STREAMFLAGS_EVENTCALLBACK | AUDCLNT_STREAMFLAGS_NOPERSIST,
        0, 0, format.get(), nullptr));
    Handle event(CreateEventW(nullptr, FALSE, FALSE, nullptr));
    if (event.get() == nullptr) throw std::runtime_error("CreateEvent(capture) failed");
    CheckHRESULT("IAudioClient::SetEventHandle(capture)", client->SetEventHandle(event.get()));
    ComPtr<IAudioCaptureClient> capture;
    CheckHRESULT("IAudioClient::GetService(IAudioCaptureClient)", client->GetService(
        __uuidof(IAudioCaptureClient), reinterpret_cast<void**>(capture.put())));
    CheckHRESULT("IAudioClient::Start(capture)", client->Start());

    CaptureStats stats{};
    stats.format = MediaFormat{format->nSamplesPerSec, format->nChannels};
    auto previousEvent = std::chrono::steady_clock::now();
    uint64_t previousDevicePosition = 0;
    uint64_t previousQpcPosition = 0;
    bool havePosition = false;
    bool firstPacket = true;
    const auto deadline = std::chrono::steady_clock::now() + duration;
    while (std::chrono::steady_clock::now() < deadline) {
        const DWORD wait = WaitForSingleObject(event.get(), 2000);
        if (wait != WAIT_OBJECT_0) throw std::runtime_error("capture event timed out");
        const auto eventTime = std::chrono::steady_clock::now();
        if (stats.packets != 0) {
            stats.maximumEventGapMilliseconds = std::max(
                stats.maximumEventGapMilliseconds,
                EventGapMilliseconds(previousEvent, eventTime));
        }
        previousEvent = eventTime;
        for (;;) {
            UINT32 packetFrames = 0;
            CheckHRESULT("IAudioCaptureClient::GetNextPacketSize", capture->GetNextPacketSize(&packetFrames));
            if (packetFrames == 0) break;
            BYTE* data = nullptr;
            DWORD flags = 0;
            uint64_t devicePosition = 0;
            uint64_t qpcPosition = 0;
            CheckHRESULT("IAudioCaptureClient::GetBuffer", capture->GetBuffer(
                &data, &packetFrames, &flags, &devicePosition, &qpcPosition));
            if (!firstPacket && (flags & AUDCLNT_BUFFERFLAGS_DATA_DISCONTINUITY) != 0) {
                ++stats.discontinuities;
            }
            if (!firstPacket && (flags & AUDCLNT_BUFFERFLAGS_TIMESTAMP_ERROR) != 0) {
                ++stats.timestampErrors;
            }
            if (havePosition) {
                if (devicePosition < previousDevicePosition) ++stats.positionRegressions;
                if (qpcPosition < previousQpcPosition) ++stats.qpcRegressions;
            }
            previousDevicePosition = devicePosition;
            previousQpcPosition = qpcPosition;
            havePosition = true;
            firstPacket = false;
            ++stats.packets;
            stats.frames += packetFrames;
            if ((flags & AUDCLNT_BUFFERFLAGS_SILENT) == 0 && data != nullptr) {
                for (UINT32 frame = 0; frame < packetFrames; ++frame) {
                    const BYTE* sample = data + static_cast<size_t>(frame) * format->nBlockAlign;
                    if (std::any_of(sample, sample + format->nBlockAlign,
                            [](BYTE value) { return value != 0; })) {
                        ++stats.nonSilentFrames;
                    }
                }
            }
            CheckHRESULT("IAudioCaptureClient::ReleaseBuffer", capture->ReleaseBuffer(packetFrames));
        }
    }
    CheckHRESULT("IAudioClient::Stop(capture)", client->Stop());
    return stats;
}

void ValidateFrameCount(const char* lane, uint64_t frames, DWORD sampleRate,
    int seconds, uint64_t allowance) {
    const uint64_t expected = static_cast<uint64_t>(sampleRate) *
        static_cast<uint64_t>(seconds);
    const uint64_t minimum = expected * 95 / 100;
    const uint64_t maximum = expected * 105 / 100 + allowance;
    if (frames < minimum || frames > maximum) {
        throw std::runtime_error(std::string(lane) + " frame cadence is outside the 5% contract: got " +
            std::to_string(frames) + ", expected approximately " + std::to_string(expected));
    }
}

int Exercise(const std::filesystem::path& snapshotPath, int seconds,
    const std::wstring& controller) {
    const EndpointSet baseline = ReadSnapshot(snapshotPath);
    std::vector<std::wstring> render;
    std::vector<std::wstring> capture;
    const auto deadline = std::chrono::steady_clock::now() + std::chrono::seconds(30);
    do {
        const EndpointSet current = EnumerateEndpoints();
        render = Difference(current.render, baseline.render);
        capture = Difference(current.capture, baseline.capture);
        if (render.size() == 1 && capture.size() == 1) break;
        if (render.size() > 1 || capture.size() > 1) {
            throw std::runtime_error("more than one new active endpoint appeared; refusing an ambiguous media test");
        }
        std::this_thread::sleep_for(std::chrono::milliseconds(100));
    } while (std::chrono::steady_clock::now() < deadline);
    if (render.size() != 1 || capture.size() != 1) {
        throw std::runtime_error("the virtual controller did not expose exactly one new render and capture endpoint");
    }

    std::exception_ptr renderError;
    std::exception_ptr captureError;
    RenderStats renderStats{};
    CaptureStats captureStats{};
    const auto duration = std::chrono::seconds(seconds);
    std::thread renderThread([&] {
        try { renderStats = ExerciseRender(render[0], duration); }
        catch (...) { renderError = std::current_exception(); }
    });
    std::thread captureThread([&] {
        try { captureStats = ExerciseCapture(capture[0], duration); }
        catch (...) { captureError = std::current_exception(); }
    });
    renderThread.join();
    captureThread.join();
    if (renderError) std::rethrow_exception(renderError);
    if (captureError) std::rethrow_exception(captureError);
    if (renderStats.frames == 0 || captureStats.frames == 0) {
        throw std::runtime_error("CoreAudio endpoint completed no frames");
    }
    const ExpectedMediaFormat expected = ExpectedFormatFor(controller);
    if (renderStats.format.sampleRate != expected.renderSampleRate ||
        renderStats.format.channels != expected.renderChannels) {
        throw std::runtime_error("render mix format does not match the virtual controller descriptor");
    }
    if (captureStats.format.sampleRate != expected.captureSampleRate ||
        captureStats.format.channels != expected.captureChannels) {
        throw std::runtime_error("capture mix format does not match the virtual controller descriptor");
    }
    ValidateFrameCount("render", renderStats.frames, renderStats.format.sampleRate,
        seconds, renderStats.bufferFrames);
    ValidateFrameCount("capture", captureStats.frames, captureStats.format.sampleRate,
        seconds, 0);
    if (renderStats.underruns != 0) {
        throw std::runtime_error("render stream exhausted its CoreAudio buffer " +
            std::to_string(renderStats.underruns) + " time(s)");
    }
    if (captureStats.discontinuities != 0 || captureStats.timestampErrors != 0 ||
        captureStats.positionRegressions != 0 || captureStats.qpcRegressions != 0) {
        throw std::runtime_error("capture stream reported a discontinuity or non-monotonic clock");
    }
    if (captureStats.nonSilentFrames < captureStats.frames / 2) {
        throw std::runtime_error("capture stream did not preserve the injected non-silent microphone PCM");
    }
    std::cout << "renderFrames=" << renderStats.frames
              << " renderEvents=" << renderStats.events
              << " renderBufferFrames=" << renderStats.bufferFrames
              << " renderUnderruns=" << renderStats.underruns
              << " renderMaxEventGapMs=" << renderStats.maximumEventGapMilliseconds
              << " captureFrames=" << captureStats.frames
              << " captureNonSilentFrames=" << captureStats.nonSilentFrames
              << " capturePackets=" << captureStats.packets
              << " captureDiscontinuities=" << captureStats.discontinuities
              << " captureTimestampErrors=" << captureStats.timestampErrors
              << " capturePositionRegressions=" << captureStats.positionRegressions
              << " captureQpcRegressions=" << captureStats.qpcRegressions
              << " captureMaxEventGapMs=" << captureStats.maximumEventGapMilliseconds
              << "\n";
    return 0;
}

} // namespace

int wmain(int argc, wchar_t** argv) {
    try {
        if (argc == 3 && _wcsicmp(argv[1], L"snapshot") == 0) {
            WriteSnapshot(argv[2], EnumerateEndpoints());
            return 0;
        }
        if (argc == 5 && _wcsicmp(argv[1], L"exercise") == 0) {
            const int seconds = _wtoi(argv[3]);
            if (seconds < 1 || seconds > 300) throw std::runtime_error("duration must be 1 through 300 seconds");
            return Exercise(argv[2], seconds, argv[4]);
        }
        std::wcerr << L"Usage:\n"
                   << L"  ViiperUdeMediaProbe.exe snapshot <snapshot-path>\n"
                   << L"  ViiperUdeMediaProbe.exe exercise <snapshot-path> <seconds> <dualsense|dualsenseedge|dualshock4>\n";
        return 2;
    } catch (const std::exception& error) {
        std::cerr << "VIIPER UDE media probe failed: " << error.what() << "\n";
        return 1;
    }
}
