// SDI output on a Blackmagic DeckLink port: the decoded pictures and sound are played out with the card's own clock
// (scheduled playback). Taken over from Z:\Projects\SDI_converter2\src\playout.h (Player, mode choice, port lease),
// fed directly by the decoder instead of an OMT receiver, and with as many sound channels as the source has.
// The output mode follows the picture: same size, frame rate and scan type; a change restarts the output in the new mode.
#pragma once
#include <atlbase.h>
#pragma warning(disable : 4192) // the Windows RECT definition is reused on purpose
#include "decklink_import.h"
#include <array>
#include <cmath>
#include <condition_variable>
#include <deque>
#include <mutex>

namespace sdi {

inline void check(HRESULT result, const char* action) {
    if (FAILED(result)) {
        char text[200];
        sprintf_s(text, "%s failed (0x%08lX)", action, static_cast<unsigned long>(result));
        throw std::runtime_error(text);
    }
}

inline std::string utf8(BSTR text) {
    if (!text) return {};
    const int size = WideCharToMultiByte(CP_UTF8, 0, text, SysStringLen(text), nullptr, 0, nullptr, nullptr);
    std::string result(static_cast<size_t>(size), '\0');
    WideCharToMultiByte(CP_UTF8, 0, text, SysStringLen(text), result.data(), size, nullptr, nullptr);
    return result;
}

inline int64_t nowMs() { return static_cast<int64_t>(GetTickCount64()); }

// Empty when Blackmagic Desktop Video is not installed.
inline std::vector<CComPtr<IDeckLink>> devices() {
    CComPtr<IDeckLinkIterator> iterator;
    std::vector<CComPtr<IDeckLink>> result;
    if (FAILED(iterator.CoCreateInstance(__uuidof(CDeckLinkIterator)))) return result;
    for (;;) {
        CComPtr<IDeckLink> device;
        if (iterator->Next(&device) != S_OK) break;
        result.push_back(device);
    }
    return result;
}

inline int64_t attribute(IDeckLink* device, BMDDeckLinkAttributeID id, int64_t fallback) {
    CComPtr<IDeckLinkProfileAttributes> attributes;
    __int64 value = 0;
    if (SUCCEEDED(device->QueryInterface(__uuidof(IDeckLinkProfileAttributes), reinterpret_cast<void**>(&attributes))) &&
        attributes->GetInt(id, &value) == S_OK)
        return value;
    return fallback;
}

inline bool canPlay(IDeckLink* device) { return (attribute(device, BMDDeckLinkVideoIOSupport, 0) & bmdDeviceSupportsPlayback) != 0; }

// The SDK's persistent id survives a new order of the devices; older cards fall back to the index.
inline std::string deviceKey(IDeckLink* device, int index) {
    const int64_t id = attribute(device, BMDDeckLinkPersistentID, -1);
    return id >= 0 ? "persistent-" + std::to_string(id) : "index-" + std::to_string(index);
}

inline bool isBusy(IDeckLink* device) {
    CComPtr<IDeckLinkStatus> status;
    __int64 busy = 0;
    return SUCCEEDED(device->QueryInterface(__uuidof(IDeckLinkStatus), reinterpret_cast<void**>(&status))) && status &&
           status->GetInt(bmdDeckLinkStatusBusy, &busy) == S_OK && busy;
}

inline std::string deviceName(IDeckLink* device) {
    CComBSTR name;
    return device->GetDisplayName(&name) == S_OK ? utf8(name) : std::string("DeckLink");
}

inline std::string modelName(IDeckLink* device) {
    CComBSTR name;
    return device->GetModelName(&name) == S_OK ? utf8(name) : std::string();
}

struct Port {
    std::string key, name, model;
    bool playback = false, busy = false;
};

inline std::vector<Port> ports() {
    std::vector<Port> result;
    auto all = devices();
    for (size_t i = 0; i < all.size(); ++i)
        result.push_back({deviceKey(all[i], static_cast<int>(i)), deviceName(all[i]), modelName(all[i]), canPlay(all[i]), isBusy(all[i])});
    return result;
}

// A port is held by one process only. The lease of SDI Converter 2 is taken as well, so the two programs never share a port.
class PortLease {
    std::vector<HANDLE> handles;
public:
    explicit PortLease(const std::string& key) {
        for (const char* prefix : {"Local\\SrtBridgeOutput-Port-", "Local\\SDIConverter2-Port-"}) {
            HANDLE handle = CreateMutexA(nullptr, FALSE, (prefix + key).c_str());
            if (!handle) { release(); throw std::runtime_error("Cannot reserve the SDI port"); }
            const DWORD result = WaitForSingleObject(handle, 0);
            if (result != WAIT_OBJECT_0 && result != WAIT_ABANDONED) {
                CloseHandle(handle);
                release();
                throw std::runtime_error("The SDI port is already used by another signal or by SDI Converter 2");
            }
            handles.push_back(handle);
        }
    }
    void release() { for (HANDLE h : handles) { ReleaseMutex(h); CloseHandle(h); } handles.clear(); }
    ~PortLease() { release(); }
    PortLease(const PortLease&) = delete;
    PortLease& operator=(const PortLease&) = delete;
};

// Interlaced pictures are woven full-height frames at the FRAME rate (1080i50 -> 25), as the decoder delivers them.
struct ModeInfo {
    BMDDisplayMode id = bmdModeUnknown;
    int width = 0, height = 0, rateN = 0, rateD = 1;
    bool interlaced = false;
    std::string name;
};

inline ModeInfo describe(IDeckLinkDisplayMode* mode) {
    ModeInfo info;
    info.id = mode->GetDisplayMode();
    info.width = mode->GetWidth();
    info.height = mode->GetHeight();
    __int64 duration = 0, scale = 0;
    check(mode->GetFrameRate(&duration, &scale), "Read frame rate");
    info.rateN = static_cast<int>(scale);
    info.rateD = static_cast<int>(duration);
    const auto fields = mode->GetFieldDominance();
    info.interlaced = fields == bmdUpperFieldFirst || fields == bmdLowerFieldFirst;
    CComBSTR name;
    if (mode->GetName(&name) == S_OK) info.name = utf8(name);
    return info;
}

// Time of frame `index` in 100 ns / in 48 kHz samples; exact for 59.94 (800.8 samples per frame).
inline int64_t frameTime(int64_t index, const ModeInfo& m) { return index * 10000000 * m.rateD / m.rateN; }
inline int64_t audioPosition(int64_t index, const ModeInfo& m) { return index * 48000 * m.rateD / m.rateN; }

struct Format {
    int width = 0, height = 0, rateN = 0, rateD = 1;
    bool interlaced = false;
    bool valid() const { return width > 0 && height > 0 && rateN > 0 && rateD > 0; }
    bool operator==(const Format& o) const {
        return width == o.width && height == o.height && interlaced == o.interlaced &&
               static_cast<int64_t>(rateN) * o.rateD == static_cast<int64_t>(o.rateN) * rateD;
    }
    std::string text() const {
        char buffer[80];
        const double fps = rateD > 0 ? static_cast<double>(rateN) / rateD : 0.0;
        sprintf_s(buffer, "%dx%d %.4g%s", width, height, interlaced ? fps * 2 : fps, interlaced ? "i" : "p");
        return buffer;
    }
};

inline ModeInfo pickMode(IDeckLinkOutput_v14_2_1* output, const Format& in) {
    CComPtr<IDeckLinkDisplayModeIterator> iterator;
    check(output->GetDisplayModeIterator(&iterator), "Output mode list");
    for (;;) {
        CComPtr<IDeckLinkDisplayMode> display;
        if (iterator->Next(&display) != S_OK) break;
        const ModeInfo mode = describe(display);
        if (mode.width != in.width || mode.height != in.height || mode.interlaced != in.interlaced ||
            static_cast<int64_t>(mode.rateN) * in.rateD != static_cast<int64_t>(in.rateN) * mode.rateD)
            continue;
        BMDDisplayMode actual = mode.id;
        long ok = 0;
        if (output->DoesSupportVideoMode(bmdVideoConnectionSDI, mode.id, bmdFormat8BitYUV, bmdNoVideoOutputConversion,
                                         bmdSupportedVideoModeDefault, &actual, &ok) == S_OK && ok)
            return mode;
    }
    throw std::runtime_error("The SDI port has no output mode for " + in.text());
}

inline int32_t pcm32(float x) {
    if (!std::isfinite(x)) return 0;
    const double v = std::clamp(static_cast<double>(x), -1.0, 1.0);
    return v >= 1 ? INT32_MAX : static_cast<int32_t>(v * 2147483648.0);
}

struct Picture { int64_t time = 0; std::vector<uint8_t> pixels; };
struct Chunk { int64_t time = 0; int samples = 0, channels = 0; std::vector<float> planar; };

// Pictures and sound waiting for their turn, by time stamp (100 ns, the clock of the stream).
class Buffer {
    std::mutex mutex;
    std::deque<Picture> video;
    std::deque<Chunk> audio;
    int64_t previous = -1;
    uint64_t generation = 0, discarded = 0;
public:
    struct State { size_t count; int64_t last; uint64_t generation, discarded; };
    // The least the sound reached past a picture when that picture arrived (100 ns; negative: the sound lags),
    // since the last call with the same reader (0 = status, 1 = playout). INT64_MAX: nothing measured.
    int64_t soundLead(int reader) {
        std::lock_guard lock(mutex);
        const int64_t value = worst[reader];
        worst[reader] = INT64_MAX;
        return value;
    }
private:
    int64_t worst[2] = {INT64_MAX, INT64_MAX};
    int64_t audioEnd = 0; // end of the newest sound, kept when played sound is removed
public:
    void clear() {
        std::lock_guard lock(mutex);
        video.clear(); audio.clear(); previous = -1; audioEnd = 0; ++generation;
        worst[0] = worst[1] = INT64_MAX;
    }
    void push(Picture picture) {
        std::lock_guard lock(mutex);
        if (previous >= 0 && (picture.time < previous || picture.time - previous > 10000000)) { video.clear(); audio.clear(); ++generation; }
        if (picture.time == previous) return;
        previous = picture.time;
        // Worst case since the last reading: how far the sound reaches past this picture when it arrives.
        if (audioEnd > 0) {
            const int64_t reach = audioEnd - picture.time;
            for (auto& w : worst) w = std::min(w, reach);
        }
        // Room for the wait for the sound (Output::maxWait 200 ms) plus the card's frames, also at 60p.
        if (video.size() >= 24) { video.pop_front(); ++discarded; }
        video.push_back(std::move(picture));
    }
    // Sound may run ahead of the pictures: ffmpeg (and vMix) packs 320 ms per PES and muxes up to 0.7 s early. Keep 2 s.
    void push(Chunk chunk) {
        std::lock_guard lock(mutex);
        if (!audio.empty() && chunk.time < audio.back().time) audio.clear();
        audioEnd = chunk.time + static_cast<int64_t>(chunk.samples) * 10000000 / 48000;
        while (!audio.empty() && (audio.size() >= 256 || chunk.time - audio.front().time > 20000000)) audio.pop_front();
        audio.push_back(std::move(chunk));
    }
    State state() {
        std::lock_guard lock(mutex);
        return {video.size(), previous, generation, discarded};
    }
    bool picture(int64_t target, int64_t tolerance, Picture& selected) {
        std::lock_guard lock(mutex);
        bool found = false;
        while (!video.empty() && video.front().time <= target + tolerance) {
            if (found) ++discarded;
            selected = std::move(video.front());
            video.pop_front();
            found = true;
        }
        return found;
    }
    // The sound for `count` samples from `target` on, interleaved PCM32 with `channels` per sample; missing parts stay silent.
    void sound(int64_t target, int count, int channels, std::vector<int32_t>& out, uint64_t& missing) {
        std::lock_guard lock(mutex);
        out.assign(static_cast<size_t>(count) * channels, 0);
        std::vector<char> present(static_cast<size_t>(count), 0);
        const int64_t end = target + static_cast<int64_t>(count) * 10000000 / 48000;
        while (!audio.empty() && audio.front().time + static_cast<int64_t>(audio.front().samples) * 10000000 / 48000 <= target) audio.pop_front();
        for (const auto& chunk : audio) {
            if (chunk.time >= end) break;
            const int64_t offset = std::llround(static_cast<double>(chunk.time - target) * 48000 / 10000000);
            const int use = std::min(chunk.channels, channels);
            for (int i = 0; i < chunk.samples; ++i) {
                const int64_t dest = offset + i;
                if (dest < 0 || dest >= count) continue;
                int32_t* frame = &out[static_cast<size_t>(dest) * channels];
                for (int c = 0; c < use; ++c) frame[c] = pcm32(chunk.planar[static_cast<size_t>(c) * chunk.samples + i]);
                present[static_cast<size_t>(dest)] = 1;
            }
        }
        missing += static_cast<uint64_t>(std::count(present.begin(), present.end(), 0));
    }
};

// Scheduled playback in one mode, as in SDI Converter 2: a pool of frames, the last picture repeats when none is due,
// black when no picture came for 500 ms.
class Player : public IDeckLinkVideoOutputCallback_v14_2_1 {
    std::atomic<ULONG> refs{1};
    struct Slot {
        CComPtr<IDeckLinkMutableVideoFrame_v14_2_1> frame;
        std::atomic<bool> busy{false};
    };
    std::array<Slot, 12> slots;
    std::vector<int32_t> pcm;
    CComPtr<IDeckLinkOutput_v14_2_1> output;
    ModeInfo format;
    int bufferFrames;
    bool videoEnabled = false, audioEnabled = false, started = false, closed = false;
    std::atomic<bool> stopped{false};
    std::mutex wakeMutex;
    std::condition_variable wake;
    Picture previous;
    bool havePrevious = false;
    int64_t origin = 0, index = 0;
    uint64_t generation = 0;
    // How far behind the newest picture the frame being scheduled now starts. The card holds bufferFrames frames,
    // so the last of them must have its sound complete when it is scheduled: bufferFrames frames plus the wait for
    // the sound, which the sender decides (ffmpeg test stream: the sound lags up to 66 ms behind a fresh picture;
    // without the wait 65-90 % of the samples were missing).
    int64_t wait;
    int64_t delay() const { return frameTime(bufferFrames, format) + wait; }
    void anchor(int64_t newest) { origin = newest - delay() - frameTime(index, format); }

public:
    const int channels;
    int64_t soundWait() const { return wait; }
    // A longer wait for the sound: the output moves back once (one picture repeats, a gap in the sound).
    void setWait(int64_t value, Buffer& buffer) {
        wait = value;
        anchor(buffer.state().last);
        ++resyncs;
    }
    std::atomic<uint64_t> completed{0}, late{0}, dropped{0};
    uint64_t repeated = 0, black = 0, silentSamples = 0, resyncs = 0;

    Player(CComPtr<IDeckLinkOutput_v14_2_1> value, const ModeInfo& mode, int depth, int audioChannels, int64_t soundWait)
        : output(value), format(mode), bufferFrames(depth), wait(soundWait), channels(audioChannels) {}

    HRESULT STDMETHODCALLTYPE QueryInterface(REFIID iid, void** result) override {
        if (!result) return E_POINTER;
        *result = nullptr;
        if (iid == IID_IUnknown || iid == __uuidof(IDeckLinkVideoOutputCallback_v14_2_1)) {
            *result = static_cast<IDeckLinkVideoOutputCallback_v14_2_1*>(this);
            AddRef();
            return S_OK;
        }
        return E_NOINTERFACE;
    }
    ULONG STDMETHODCALLTYPE AddRef() override { return ++refs; }
    ULONG STDMETHODCALLTYPE Release() override {
        const auto value = --refs;
        if (!value) delete this;
        return value;
    }
    HRESULT STDMETHODCALLTYPE ScheduledFrameCompleted(IDeckLinkVideoFrame_v14_2_1* frame, _BMDOutputFrameCompletionResult result) override {
        if (result == bmdOutputFrameDisplayedLate) ++late;
        else if (result == bmdOutputFrameDropped) ++dropped;
        if (result != bmdOutputFrameFlushed) ++completed;
        for (auto& slot : slots)
            if (slot.frame.p == frame) { slot.busy.store(false, std::memory_order_release); break; }
        wake.notify_one();
        return S_OK;
    }
    HRESULT STDMETHODCALLTYPE ScheduledPlaybackHasStopped() override { stopped = true; wake.notify_one(); return S_OK; }

    void initialize() {
        check(output->EnableVideoOutput(format.id, bmdVideoOutputFlagDefault), "Enable SDI video (port may be in use)");
        videoEnabled = true;
        check(output->EnableAudioOutput(bmdAudioSampleRate48kHz, bmdAudioSampleType32bitInteger, channels, bmdAudioOutputStreamTimestamped),
              "Enable embedded SDI sound");
        audioEnabled = true;
        for (auto& slot : slots)
            check(output->CreateVideoFrame(format.width, format.height, format.width * 2, bmdFormat8BitYUV, bmdFrameFlagDefault, &slot.frame),
                  "Allocate SDI frame");
        check(output->SetScheduledFrameCompletionCallback(this), "Install playback callback");
        check(output->BeginAudioPreroll(), "Begin audio preroll");
    }

    void fill(Buffer& buffer, bool signal) {
        unsigned int pending = 0;
        check(output->GetBufferedVideoFrameCount(&pending), "Read output buffer");
        const int64_t period = frameTime(1, format);
        while (pending < static_cast<unsigned int>(bufferFrames)) {
            const auto state = buffer.state();
            int64_t target = origin + frameTime(index, format);
            // New stream, or the source clock drifted too far from the card's: anchor again.
            if (state.generation != generation || (started && signal && std::llabs(state.last - delay() - target) > period * 6)) {
                anchor(state.last);
                target = origin + frameTime(index, format);
                generation = state.generation;
                ++resyncs;
                havePrevious = false;
            }
            Slot* free = nullptr;
            for (auto& slot : slots) {
                bool expected = false;
                if (slot.busy.compare_exchange_strong(expected, true)) { free = &slot; break; }
            }
            if (!free) throw std::runtime_error("SDI frame pool exhausted");
            const bool fresh = buffer.picture(target, period / 3, previous);
            if (fresh) havePrevious = true;
            void* data = nullptr;
            check(free->frame->GetBytes(&data), "Access SDI frame");
            if (havePrevious && signal && previous.pixels.size() == static_cast<size_t>(format.width) * format.height * 2) {
                std::memcpy(data, previous.pixels.data(), previous.pixels.size());
                if (!fresh) ++repeated;
            } else {
                auto pixels = static_cast<uint8_t*>(data);
                for (int i = 0; i < format.width * format.height * 2; i += 4) { pixels[i] = 128; pixels[i + 1] = 16; pixels[i + 2] = 128; pixels[i + 3] = 16; }
                ++black;
            }
            const int samples = static_cast<int>(audioPosition(index + 1, format) - audioPosition(index, format));
            buffer.sound(target, samples, channels, pcm, silentSamples);
            if (!signal) std::fill(pcm.begin(), pcm.end(), 0);
            unsigned int written = 0;
            check(output->ScheduleAudioSamples(pcm.data(), samples, static_cast<__int64>(audioPosition(index, format)), 48000, &written),
                  "Schedule SDI sound");
            if (written != static_cast<unsigned int>(samples)) throw std::runtime_error("SDI sound buffer took only part of a packet");
            check(output->ScheduleVideoFrame(free->frame, index * format.rateD, format.rateD, format.rateN), "Schedule SDI picture");
            ++index;
            ++pending;
        }
    }
    void start(Buffer& buffer) {
        const auto state = buffer.state();
        anchor(state.last); // the first frame is a waiting one, delay() old (Output waits for enough pictures)
        generation = state.generation;
        fill(buffer, true);
        check(output->EndAudioPreroll(), "End audio preroll");
        check(output->StartScheduledPlayback(0, format.rateN, 1.0), "Start SDI playback");
        started = true;
    }
    void tick(Buffer& buffer, bool signal) {
        __int64 streamTime = 0;
        double speed = 0;
        check(output->GetScheduledStreamTime(format.rateN, &streamTime, &speed), "Read SDI clock");
        const int64_t frame = streamTime / format.rateD;
        if (frame >= index) { // the card ran past what was scheduled: skip ahead and anchor again
            index = frame + bufferFrames;
            anchor(buffer.state().last);
            ++resyncs;
        }
        fill(buffer, signal);
        std::unique_lock lock(wakeMutex);
        wake.wait_for(lock, std::chrono::milliseconds(2));
    }
    void close() noexcept {
        if (closed) return;
        closed = true;
        if (started) {
            __int64 actual = 0;
            output->StopScheduledPlayback(0, &actual, format.rateN);
            std::unique_lock lock(wakeMutex);
            wake.wait_for(lock, std::chrono::seconds(2), [this] { return stopped.load(); });
        }
        if (audioEnabled) output->DisableAudioOutput();
        if (videoEnabled) output->DisableVideoOutput();
        output->SetScheduledFrameCompletionCallback(nullptr);
    }
    ~Player() { close(); }
};

// One SDI port for one signal. The decoder thread hands in pictures and sound; a worker plays them out.
class Output {
    const std::string key;
    const int depth;
    Buffer buffer;
    std::mutex mutex;
    Format seen, expected;
    bool changed = false;
    int sourceChannels = 0, sourceRate = 0;
    std::string modeText, problemText;
    std::atomic<int64_t> lastPicture{0};
    std::atomic<bool> active{true};
    std::thread worker;
    struct Counters { uint64_t late = 0, dropped = 0, repeated = 0, black = 0, silent = 0, resyncs = 0; int64_t leadMs = 0, waitMs = 0; } counters;

    // The wait for the sound (100 ns): how far it lagged (`lead` from Buffer::soundLead) plus 20 ms, at most 200 ms.
    // No sound measured: 20 ms.
    static constexpr int64_t maxWait = 2000000;
    static int64_t waitFor(int64_t lead) {
        if (lead == INT64_MAX) return 200000;
        return std::clamp(-lead + 200000, static_cast<int64_t>(200000), maxWait);
    }

    // 2, 8, 16 or 32 channels, the smallest that holds the source, at most what the card has.
    static int sdiChannels(int source, int most) {
        int n = 2;
        for (int c : {8, 16, 32}) if (n < source && c <= most) n = c;
        return n;
    }
    void setProblem(const std::string& text) { std::lock_guard lock(mutex); problemText = text; }
    bool signal() const { const int64_t t = lastPicture; return t > 0 && nowMs() - t < 500; }

    // Waits up to `ms`; false when the output is stopping.
    bool pause(int ms) {
        for (int i = 0; i < ms / 10 && active; ++i) std::this_thread::sleep_for(std::chrono::milliseconds(10));
        return active;
    }

    void run() {
        CoInitializeEx(nullptr, COINIT_MULTITHREADED);
        while (active) {
            try {
                play();
            } catch (const std::exception& e) {
                setProblem(e.what());
                { std::lock_guard lock(mutex); modeText.clear(); expected = {}; }
                pause(2000); // try again: the port may become free, the card may come back
            }
        }
        CoUninitialize();
    }

    void play() {
        auto all = devices();
        if (all.empty()) throw std::runtime_error("No Blackmagic card found (is Desktop Video installed?)");
        IDeckLink* device = nullptr;
        for (size_t i = 0; i < all.size(); ++i)
            if (deviceKey(all[i], static_cast<int>(i)) == key) device = all[i];
        if (!device) throw std::runtime_error("The chosen SDI port is not on this PC");
        PortLease lease(key);
        if (isBusy(device)) throw std::runtime_error("The SDI port is in use by another program");
        if (!canPlay(device)) throw std::runtime_error("The SDI port is set to input; change it in Blackmagic Desktop Video Setup");
        CComPtr<IDeckLinkOutput_v14_2_1> output;
        check(device->QueryInterface(__uuidof(IDeckLinkOutput_v14_2_1), reinterpret_cast<void**>(&output)), "DeckLink output");
        const int most = static_cast<int>(attribute(device, BMDDeckLinkMaximumAudioChannels, 2));
        setProblem("");
        while (active) {
            Format format;
            for (;;) {
                { std::lock_guard lock(mutex); format = seen; }
                if (format.valid()) break;
                if (!pause(20)) return;
            }
            if (!pause(300)) return; // the frame rate is known from the second picture on: let it settle
            int channels;
            {
                std::lock_guard lock(mutex);
                format = seen;
                channels = sdiChannels(sourceChannels, most);
            }
            ModeInfo mode;
            try { mode = pickMode(output, format); }
            catch (const std::exception& e) {
                setProblem(e.what());
                for (;;) { // wait for another picture format
                    if (!pause(200)) return;
                    std::lock_guard lock(mutex);
                    if (!(seen == format)) break;
                }
                setProblem("");
                continue;
            }
            buffer.clear();
            { std::lock_guard lock(mutex); expected = format; changed = false; }
            // For one second: how far the sound lags behind the pictures with this sender; that sets the wait.
            // The pictures of that second fill the buffer, enough for the card's frames and the wait.
            const auto begin = nowMs();
            bool restart = false;
            const size_t needed = static_cast<size_t>(depth) + static_cast<size_t>(maxWait / frameTime(1, mode)) + 1;
            while (nowMs() - begin < 1000 || buffer.state().count < needed) {
                if (!pause(10)) return;
                std::lock_guard lock(mutex);
                if (changed || nowMs() - begin > 10000) { restart = true; break; }
            }
            if (restart) continue;
            CComPtr<Player> player;
            player.Attach(new Player(output, mode, depth, channels, waitFor(buffer.soundLead(1))));
            try { player->initialize(); player->start(buffer); } catch (...) { player->close(); throw; }
            {
                std::lock_guard lock(mutex);
                modeText = mode.name + (channels > 2 ? ", " + std::to_string(channels) + " ch" : "");
            }
            try {
                auto checked = nowMs();
                while (active) {
                    {
                        std::lock_guard lock(mutex);
                        if (changed || sdiChannels(sourceChannels, most) != channels) break;
                        counters = {player->late, player->dropped, player->repeated, player->black, player->silentSamples, player->resyncs, 0,
                                    player->soundWait() / 10000};
                    }
                    // The sound lags more than at the start: wait longer (never shorter, that would jump each time).
                    if (nowMs() - checked >= 1000) {
                        checked = nowMs();
                        const int64_t needWait = waitFor(buffer.soundLead(1));
                        if (needWait > player->soundWait() + 50000) player->setWait(needWait, buffer);
                    }
                    player->tick(buffer, signal());
                }
            } catch (...) { player->close(); throw; }
            player->close();
            std::lock_guard lock(mutex);
            modeText.clear();
            expected = {};
        }
    }

public:
    Output(std::string portKey, int bufferFrames) : key(std::move(portKey)), depth(std::clamp(bufferFrames, 2, 8)) {
        worker = std::thread([this] { run(); });
    }
    ~Output() {
        active = false;
        if (worker.joinable()) worker.join();
    }

    // UYVY, rows without gaps.
    void picture(const std::vector<unsigned char>& uyvy, int width, int height, int rateN, int rateD, bool interlaced, int64_t time) {
        const Format now{width, height, rateN, rateD, interlaced};
        {
            std::lock_guard lock(mutex);
            seen = now;
            if (!expected.valid()) return;
            if (!(now == expected)) { changed = true; return; }
        }
        lastPicture = nowMs();
        buffer.push(Picture{time, uyvy});
    }

    // Planar float, all channels of the source.
    void sound(const std::vector<float>& planar, int count, int channels, int64_t time, int rate) {
        {
            std::lock_guard lock(mutex);
            sourceChannels = channels;
            sourceRate = rate;
        }
        if (rate != 48000) return; // SDI carries 48 kHz only; reported in status()
        buffer.push(Chunk{time, count, channels, planar});
    }

    struct Status {
        std::string mode, problem;
        Counters counters;
    };
    Status status() {
        std::lock_guard lock(mutex);
        Status s{modeText, problemText, counters};
        const int64_t lead = buffer.soundLead(0); // called once a second: the worst of that second
        s.counters.leadMs = lead == INT64_MAX ? 0 : lead / 10000;
        if (s.problem.empty() && sourceRate && sourceRate != 48000)
            s.problem = "SDI carries 48 kHz sound only; this signal has " + std::to_string(sourceRate) + " Hz, the SDI sound is silent";
        return s;
    }
};

} // namespace sdi
