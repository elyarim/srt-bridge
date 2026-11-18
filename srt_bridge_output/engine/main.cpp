// srt_bridge_decoder: one signal of the SRT Bridge, decoded and sent out as OMT and/or NDI.
// Input: MPEG-TS over plain UDP on 127.0.0.1 (the bridge sends it there on request).
// Prints one JSON status line per second; "stop" on stdin ends it.
#include <winsock2.h>
#include <ws2tcpip.h>
#include <Windows.h>
#include <algorithm>
#include <atomic>
#include <chrono>
#include <cstdint>
#include <cstring>
#include <iostream>
#include <memory>
#include <stdexcept>
#include <string>
#include <thread>
#include <cctype>
#include <vector>
#include "libomt.h"
#include "ts_demux.h"
#include "mf_decode.h"
#include "aac_decode.h"
#include "audio_join.h"
#include "sdi_out.h"

namespace {

std::atomic<bool> running{true};

BOOL WINAPI consoleHandler(DWORD event) {
    if (event == CTRL_C_EVENT || event == CTRL_BREAK_EVENT) { running = false; return TRUE; }
    return FALSE;
}

// The interface sends "stop" on stdin; a closed pipe (interface gone) stops as well.
bool stopRequested() {
    HANDLE handle = GetStdHandle(STD_INPUT_HANDLE);
    if (GetFileType(handle) != FILE_TYPE_PIPE) return false;
    DWORD available = 0;
    if (!PeekNamedPipe(handle, nullptr, 0, nullptr, &available, nullptr)) return true;
    if (!available) return false;
    char buffer[256];
    DWORD read = 0;
    if (!ReadFile(handle, buffer, (available < sizeof(buffer) ? available : static_cast<DWORD>(sizeof(buffer))), &read, nullptr)) return true;
    return std::string(buffer, read).find("stop") != std::string::npos;
}

std::string json(const std::string& value) {
    std::string result = "\"";
    const char hex[] = "0123456789abcdef";
    for (unsigned char c : value) {
        if (c == '"' || c == '\\') { result += '\\'; result += static_cast<char>(c); }
        else if (c < 32) { result += "\\u00"; result += hex[c >> 4]; result += hex[c & 15]; }
        else result += static_cast<char>(c);
    }
    return result + '"';
}

#include "ndi_runtime.h"
#include "sinks.h"

// The name part of an address "HOST (name)", as OMT and NDI announce it.
std::string namePart(const std::string& address) {
    const auto open = address.find(" (");
    const auto close = address.rfind(')');
    return open != std::string::npos && close != std::string::npos && close > open ? address.substr(open + 2, close - open - 2) : address;
}

std::string lower(std::string text) {
    for (auto& c : text) c = static_cast<char>(std::tolower(static_cast<unsigned char>(c)));
    return text;
}

// Looks on the network for a sender with this name. "" = the name is free,
// otherwise the address that already uses it. Takes two to five seconds.
std::string nameInUse(const std::string& name, bool omt, bool ndiOut) {
    const std::string wanted = lower(name);
    if (omt) {
        int count = 0;
        omt_discovery_getaddresses(&count); // starts the discovery
        std::this_thread::sleep_for(std::chrono::milliseconds(2500));
        count = 0;
        char** addresses = omt_discovery_getaddresses(&count);
        for (int i = 0; i < count; ++i)
            if (addresses[i] && lower(namePart(addresses[i])) == wanted) return std::string("OMT ") + addresses[i];
    }
    if (ndiOut) {
        ndi::Runtime runtime;
        for (const auto& source : ndi::findNdiSources(runtime, 2))
            if (lower(namePart(source)) == wanted) return "NDI " + source;
    }
    return {};
}

struct Rate { int n, d; };

// The nearest broadcast frame rate for the distance between two pictures (100 ns).
bool nearestRate(int64_t distance, Rate& rate) {
    static const Rate known[] = {{24000, 1001}, {24, 1}, {25, 1}, {30000, 1001}, {30, 1}, {50, 1}, {60000, 1001}, {60, 1}};
    for (const Rate& r : known) {
        const int64_t expected = 10000000LL * r.d / r.n;
        if (distance > expected - expected / 200 && distance < expected + expected / 200) { rate = r; return true; }
    }
    return false;
}

void nv12ToUyvy(const Nv12& in, std::vector<unsigned char>& out) {
    const int w = in.width & ~1, h = in.height;
    out.resize(static_cast<size_t>(w) * 2 * h);
    for (int y = 0; y < h; ++y) {
        // Interlaced 4:2:0 keeps the colour lines of each field apart.
        const int c = in.interlaced ? (((y >> 2) << 1) | (y & 1)) : (y >> 1);
        const uint8_t* luma = in.luma + static_cast<size_t>(y) * in.stride;
        const uint8_t* chroma = in.chroma + static_cast<size_t>(c) * in.stride;
        unsigned char* d = out.data() + static_cast<size_t>(y) * w * 2;
        for (int x = 0; x < w; x += 2, d += 4) {
            d[0] = chroma[x]; d[1] = luma[x]; d[2] = chroma[x + 1]; d[3] = luma[x + 1];
        }
    }
}

struct Options {
    int port = 0, seconds = 0, sdiBuffer = 3;
    std::string name = "SRT Bridge", out = "omt", sdiPort;
};

// The Blackmagic ports of this PC as JSON, for the interface.
void listSdi() {
    std::cout << '[';
    bool first = true;
    for (const auto& p : sdi::ports()) {
        std::cout << (first ? "" : ",") << "{\"key\":" << json(p.key) << ",\"name\":" << json(p.name) << ",\"model\":" << json(p.model)
                  << ",\"playback\":" << (p.playback ? "true" : "false") << ",\"busy\":" << (p.busy ? "true" : "false") << '}';
        first = false;
    }
    std::cout << "]\n";
}

void run(const Options& options) {
    const bool wantSdi = options.out == "sdi";
    const bool wantOmt = !wantSdi && options.out.find("omt") != std::string::npos, wantNdi = !wantSdi && options.out.find("ndi") != std::string::npos;
    if (!wantOmt && !wantNdi && !wantSdi) throw std::runtime_error("No output chosen: use --out omt, ndi, omt+ndi or sdi");
    if (wantSdi && options.sdiPort.empty()) throw std::runtime_error("--out sdi needs --sdi PORT");
    // Never go on air under a name somebody else already uses: wait until it is free. SDI has no name on the network.
    if (!wantSdi) std::cout << "{\"state\":\"checking\"}\n";
    while (!wantSdi) {
        const std::string used = nameInUse(options.name, wantOmt, wantNdi);
        if (used.empty()) break;
        std::cout << "{\"state\":\"blocked\",\"problem\":"
                  << json("The name \"" + options.name + "\" is already in use on the network (" + used + "). Choose another name.") << "}\n";
        for (int tick = 0; tick < 20; ++tick) {
            if (!running || stopRequested()) return;
            std::this_thread::sleep_for(std::chrono::milliseconds(100));
        }
    }
    WSADATA wsa{};
    if (WSAStartup(MAKEWORD(2, 2), &wsa) != 0) throw std::runtime_error("Network start failed");
    SOCKET sock = socket(AF_INET, SOCK_DGRAM, IPPROTO_UDP);
    if (sock == INVALID_SOCKET) throw std::runtime_error("UDP socket failed");
    int room = 8 << 20;
    setsockopt(sock, SOL_SOCKET, SO_RCVBUF, reinterpret_cast<const char*>(&room), sizeof(room));
    DWORD wait = 200;
    setsockopt(sock, SOL_SOCKET, SO_RCVTIMEO, reinterpret_cast<const char*>(&wait), sizeof(wait));
    sockaddr_in address{};
    address.sin_family = AF_INET;
    address.sin_port = htons(static_cast<u_short>(options.port));
    inet_pton(AF_INET, "127.0.0.1", &address.sin_addr);
    if (bind(sock, reinterpret_cast<sockaddr*>(&address), sizeof(address)) != 0)
        throw std::runtime_error("UDP port " + std::to_string(options.port) + " on 127.0.0.1 is in use");

    mfCheck(MFStartup(MF_VERSION, MFSTARTUP_LITE), "Media Foundation start");

    SinkGroup sinks;
    if (wantOmt) sinks.add(std::make_unique<OmtSink>(options.name, OMTQuality_Default));
    if (wantNdi) sinks.add(std::make_unique<NdiSink>(options.name));
    std::unique_ptr<sdi::Output> sdiOut;
    if (wantSdi) sdiOut = std::make_unique<sdi::Output>(options.sdiPort, options.sdiBuffer);
    const std::string outText = wantSdi ? "SDI" : sinks.describe();

    TsDemux demux;
    std::unique_ptr<VideoDecoder> video;
    EsKind videoKind = EsKind::None;
    std::vector<std::unique_ptr<AudioDecoder>> audio; // one per AAC track
    AudioJoin join;
    Packet picture, sound;
    Rate rate{50, 1};
    int64_t previousTime = -1;
    uint64_t pictures = 0, sounds = 0, sendErrors = 0;
    int width = 0, height = 0, audioRate = 0, audioChannels = 0;
    bool interlaced = false;
    std::string problem;

    auto onPicture = [&](const Nv12& frame) {
        Rate found{};
        if (previousTime >= 0 && nearestRate(frame.time - previousTime, found)) rate = found;
        previousTime = frame.time;
        width = frame.width & ~1; height = frame.height; interlaced = frame.interlaced;
        nv12ToUyvy(frame, picture.video);
        picture.frame = OMTMediaFrame{};
        picture.frame.Type = OMTFrameType_Video;
        picture.frame.Codec = OMTCodec_UYVY;
        picture.frame.Width = width;
        picture.frame.Height = height;
        picture.frame.Stride = width * 2;
        picture.frame.Flags = interlaced ? OMTVideoFlags_Interlaced : OMTVideoFlags_None;
        picture.frame.FrameRateN = rate.n;
        picture.frame.FrameRateD = rate.d;
        picture.frame.AspectRatio = 16.0f / 9;
        picture.frame.ColorSpace = OMTColorSpace_BT709;
        picture.frame.Timestamp = frame.time;
        picture.frame.Data = picture.video.data();
        picture.frame.DataLength = static_cast<int>(picture.video.size());
        if (sdiOut) sdiOut->picture(picture.video, width, height, rate.n, rate.d, interlaced, frame.time);
        else if (sinks.send(picture) < 0) ++sendErrors;
        ++pictures;
    };
    auto onSound = [&](std::vector<float>& planar, int count, int channels, int64_t time, int sampleRate) {
        audioRate = sampleRate;
        audioChannels = channels;
        sound.audio.swap(planar);
        sound.frame = OMTMediaFrame{};
        sound.frame.Type = OMTFrameType_Audio;
        sound.frame.Codec = OMTCodec_FPA1;
        sound.frame.Timestamp = time;
        sound.frame.SampleRate = sampleRate;
        sound.frame.Channels = channels;
        sound.frame.SamplesPerChannel = count;
        sound.frame.Data = sound.audio.data();
        sound.frame.DataLength = count * channels * 4;
        if (sdiOut) sdiOut->sound(sound.audio, count, channels, time, sampleRate);
        else if (sinks.send(sound) < 0) ++sendErrors;
        ++sounds;
    };
    demux.onUnit = [&](EsKind kind, int type, int track, int64_t pts, const uint8_t* data, size_t size) {
        const int64_t time = pts >= 0 ? pts * 1000 / 9 : 0; // 90 kHz -> 100 ns
        try {
            if (kind == EsKind::H264 || kind == EsKind::Hevc) {
                if (!video || kind != videoKind) {
                    video.reset();
                    video = std::make_unique<VideoDecoder>(kind == EsKind::Hevc);
                    videoKind = kind;
                    previousTime = -1;
                }
                video->decode(data, size, time, onPicture);
            } else if (kind == EsKind::Aac && track >= 0) {
                // One track goes out as it is; several are joined into one output with all their channels.
                const int tracks = std::max(demux.aacTracks, track + 1);
                while (static_cast<int>(audio.size()) < tracks) audio.push_back(std::make_unique<AudioDecoder>());
                join.setTracks(tracks);
                if (tracks == 1) audio[0]->decode(data, size, time, onSound);
                else audio[static_cast<size_t>(track)]->decode(data, size, time,
                    [&](std::vector<float>& planar, int count, int channels, int64_t t, int sampleRate) {
                        join.push(track, planar, count, channels, t, sampleRate, onSound);
                    });
            } else if (problem.empty()) {
                problem = "stream type " + std::to_string(type) + " is not supported";
            }
        } catch (const std::exception& e) {
            problem = e.what();
        }
    };

    auto audioErrors = [&] { uint64_t n = 0; for (auto& a : audio) n += a->errors; return n; };

    std::cout << "{\"state\":\"waiting\",\"out\":" << json(outText) << "}\n";
    std::vector<char> datagram(65536);
    const auto begin = std::chrono::steady_clock::now();
    auto lastStatus = begin, lastData = begin - std::chrono::hours(1);
    uint64_t shownPictures = 0;
    while (running) {
        const int got = recv(sock, datagram.data(), static_cast<int>(datagram.size()), 0);
        const auto now = std::chrono::steady_clock::now();
        if (got > 0) {
            lastData = now;
            demux.feed(reinterpret_cast<const uint8_t*>(datagram.data()), static_cast<size_t>(got));
        }
        if (now - lastStatus >= std::chrono::seconds(1)) {
            const double seconds = std::chrono::duration<double>(now - lastStatus).count();
            const bool receiving = now - lastData < std::chrono::seconds(1);
            std::string format;
            if (width) {
                const double fps = static_cast<double>(rate.n) / rate.d;
                char text[80];
                sprintf_s(text, "%s %dx%d %.4g%s", videoKind == EsKind::Hevc ? "HEVC" : "H.264", width, height,
                          interlaced ? fps * 2 : fps, interlaced ? "i" : "p");
                format = text;
            }
            // SDI: the mode on the port and the playout counters (late/dropped by the card, repeated/black pictures,
            // samples without sound, new anchors after a jump or drift).
            std::string sdiFields, shownProblem = problem;
            uint64_t sdiErrors = 0;
            if (sdiOut) {
                const auto s = sdiOut->status();
                if (!s.problem.empty()) shownProblem = shownProblem.empty() ? s.problem : s.problem + "; " + shownProblem;
                sdiErrors = s.counters.late + s.counters.dropped;
                sdiFields = ",\"sdi\":" + json(s.mode) + ",\"sdiLate\":" + std::to_string(s.counters.late) + ",\"sdiDropped\":" + std::to_string(s.counters.dropped) +
                            ",\"sdiRepeated\":" + std::to_string(s.counters.repeated) + ",\"sdiBlack\":" + std::to_string(s.counters.black) +
                            ",\"sdiSilent\":" + std::to_string(s.counters.silent) + ",\"sdiResyncs\":" + std::to_string(s.counters.resyncs) +
                            ",\"sdiSoundLeadMs\":" + std::to_string(s.counters.leadMs) + ",\"sdiSoundWaitMs\":" + std::to_string(s.counters.waitMs);
            }
            std::cout << "{\"state\":" << (receiving ? (pictures ? "\"running\"" : "\"starting\"") : "\"waiting\"")
                      << ",\"video\":" << json(format)
                      << ",\"fps\":" << static_cast<int>((pictures - shownPictures) / seconds + 0.5)
                      << ",\"pictures\":" << pictures
                      << ",\"audio\":" << (sounds ? json("AAC " + std::to_string(audioChannels) + " ch " + std::to_string(audioRate / 1000) + " kHz" +
                                                         (join.trackCount() > 1 ? " (" + std::to_string(join.trackCount()) + " tracks)" : ""))
                                                    : json(demux.audioType ? "not decoded" : ""))
                      << ",\"receivers\":" << sinks.connections()
                      << ",\"gaps\":" << demux.gaps
                      << ",\"errors\":" << ((video ? video->errors : 0) + audioErrors() + sendErrors + sdiErrors)
                      << ",\"problem\":" << json(shownProblem)
                      << ",\"out\":" << json(outText) << sdiFields << "}\n";
            shownPictures = pictures;
            lastStatus = now;
            if (stopRequested()) break;
            if (options.seconds && now - begin >= std::chrono::seconds(options.seconds)) break;
        }
    }
    video.reset();
    sdiOut.reset();
    closesocket(sock);
    MFShutdown();
    WSACleanup();
}

} // namespace

int main(int argc, char** argv) {
    SetConsoleOutputCP(CP_UTF8);
    SetConsoleCtrlHandler(consoleHandler, TRUE);
    std::cout.setf(std::ios::unitbuf);
    if (argc == 2 && std::string(argv[1]) == "--list-sdi") {
        if (FAILED(CoInitializeEx(nullptr, COINIT_MULTITHREADED))) return 1;
        listSdi();
        CoUninitialize();
        return 0;
    }
    Options options;
    for (int i = 1; i + 1 < argc; i += 2) {
        const std::string key = argv[i], value = argv[i + 1];
        if (key == "--udp") options.port = std::atoi(value.c_str());
        else if (key == "--name") options.name = value;
        else if (key == "--out") options.out = value;
        else if (key == "--sdi") options.sdiPort = value;
        else if (key == "--sdi-buffer") options.sdiBuffer = std::atoi(value.c_str());
        else if (key == "--seconds") options.seconds = std::atoi(value.c_str());
    }
    if (options.port < 1024 || options.port > 65535) {
        std::cout << "srt_bridge_decoder --udp PORT [--name NAME] [--out omt|ndi|omt+ndi|sdi] [--sdi PORT] [--sdi-buffer FRAMES] [--seconds N]\n"
                     "srt_bridge_decoder --list-sdi\n";
        return 1;
    }
    try {
        if (FAILED(CoInitializeEx(nullptr, COINIT_MULTITHREADED))) throw std::runtime_error("COM start failed");
        run(options);
        CoUninitialize();
    } catch (const std::exception& e) {
        std::cout << "{\"state\":\"failed\",\"problem\":" << json(e.what()) << "}\n";
        return 1;
    }
    return 0;
}
