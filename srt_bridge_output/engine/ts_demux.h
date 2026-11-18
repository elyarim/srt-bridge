// MPEG-TS demultiplexer: finds the first program, then hands out the elementary
// streams (the first picture, every sound track) one PES packet at a time with its PTS.
#pragma once
#include <cstdint>
#include <functional>
#include <map>
#include <vector>

enum class EsKind { None, H264, Hevc, Aac, Other };

inline EsKind esKind(int streamType) {
    switch (streamType) {
    case 0x1B: return EsKind::H264;
    case 0x24: return EsKind::Hevc;
    case 0x0F: return EsKind::Aac; // ADTS
    default:   return EsKind::Other;
    }
}

class TsDemux {
    struct Stream {
        int type = 0;
        int track = -1; // AAC tracks: 0, 1, 2 ... in the order of the PMT
        int counter = -1;
        bool started = false;
        std::vector<uint8_t> pes;
    };
    int pmtPid = -1;
    std::map<int, Stream> streams;
    std::vector<uint8_t> rest; // bytes of a packet that was split over two datagrams

public:
    // kind, stream type, AAC track (-1 otherwise), PTS in 90 kHz units (-1 = none), payload
    std::function<void(EsKind, int, int, int64_t, const uint8_t*, size_t)> onUnit;
    uint64_t packets = 0, syncLost = 0, gaps = 0;
    int videoType = 0, audioType = 0; // audioType: the first sound track's type
    int aacTracks = 0;

    void feed(const uint8_t* data, size_t size) {
        if (!rest.empty()) {
            rest.insert(rest.end(), data, data + size);
            size_t used = scan(rest.data(), rest.size());
            rest.erase(rest.begin(), rest.begin() + used);
            return;
        }
        size_t used = scan(data, size);
        if (used < size) rest.assign(data + used, data + size);
    }

private:
    size_t scan(const uint8_t* data, size_t size) {
        size_t at = 0;
        while (at + 188 <= size) {
            if (data[at] != 0x47) { ++at; ++syncLost; continue; }
            packet(data + at);
            at += 188;
        }
        // Keep a tail only if it can still be the start of a packet.
        while (at < size && data[at] != 0x47) ++at;
        return at;
    }

    void packet(const uint8_t* p) {
        ++packets;
        if (p[1] & 0x80) return; // transport error
        const int pid = ((p[1] & 0x1F) << 8) | p[2];
        const bool start = (p[1] & 0x40) != 0;
        const int adaptation = (p[3] >> 4) & 3;
        const int counter = p[3] & 0x0F;
        size_t off = 4;
        if (adaptation & 2) off += 1 + p[4];
        if (!(adaptation & 1) || off >= 188) return;
        const uint8_t* payload = p + off;
        const size_t length = 188 - off;

        if (pid == 0) { if (start) pat(payload, length); return; }
        if (pid == pmtPid) { if (start) pmt(payload, length); return; }
        auto found = streams.find(pid);
        if (found == streams.end()) return;
        Stream& s = found->second;
        if (s.counter >= 0 && counter != ((s.counter + 1) & 0x0F)) {
            ++gaps; // packets are missing: what was collected is incomplete
            s.started = false;
            s.pes.clear();
        }
        s.counter = counter;
        if (start) {
            flush(s);
            s.started = true;
            s.pes.assign(payload, payload + length);
        } else if (s.started) {
            s.pes.insert(s.pes.end(), payload, payload + length);
        }
        // A PES packet with a known length (audio) is complete without waiting for the next one.
        if (s.started && s.pes.size() >= 6) {
            const size_t declared = (static_cast<size_t>(s.pes[4]) << 8) | s.pes[5];
            if (declared && s.pes.size() >= declared + 6) flush(s);
        }
    }

    void pat(const uint8_t* p, size_t n) {
        if (n < 1 || 1u + p[0] + 12 > n) return;
        const uint8_t* s = p + 1 + p[0];
        const size_t room = n - 1 - p[0];
        if (s[0] != 0x00) return;
        size_t end = 3 + (((s[1] & 0x0F) << 8) | s[2]);
        if (end > room) end = room;
        if (end < 4) return;
        for (size_t at = 8; at + 4 <= end - 4; at += 4) {
            const int program = (s[at] << 8) | s[at + 1];
            if (program != 0) { // first real program
                const int pid = ((s[at + 2] & 0x1F) << 8) | s[at + 3];
                if (pid != pmtPid) { pmtPid = pid; streams.clear(); videoType = audioType = aacTracks = 0; }
                return;
            }
        }
    }

    void pmt(const uint8_t* p, size_t n) {
        if (n < 1 || 1u + p[0] + 16 > n) return;
        const uint8_t* s = p + 1 + p[0];
        const size_t room = n - 1 - p[0];
        if (s[0] != 0x02) return;
        size_t end = 3 + (((s[1] & 0x0F) << 8) | s[2]);
        if (end > room) end = room;
        if (end < 4) return;
        size_t at = 12 + (((s[10] & 0x0F) << 8) | s[11]);
        bool haveVideo = false;
        int sounds = 0, aac = 0;
        while (at + 5 <= end - 4) {
            const int type = s[at];
            const int pid = ((s[at + 1] & 0x1F) << 8) | s[at + 2];
            at += 5 + (((s[at + 3] & 0x0F) << 8) | s[at + 4]);
            const EsKind kind = esKind(type);
            const bool video = kind == EsKind::H264 || kind == EsKind::Hevc;
            const bool audioStream = type == 0x0F || type == 0x03 || type == 0x04 || type == 0x11;
            int track = -1;
            if (video && !haveVideo) { haveVideo = true; videoType = type; }
            else if (audioStream && sounds < 16) {
                if (sounds++ == 0) audioType = type;
                if (kind == EsKind::Aac) track = aac++;
            }
            else continue; // only the first picture is used, and up to 16 sound tracks
            Stream& stream = streams[pid];
            if (stream.type != type) { stream = Stream{}; stream.type = type; }
            stream.track = track;
        }
        aacTracks = aac;
    }

    void flush(Stream& s) {
        if (s.started && s.pes.size() >= 9 && s.pes[0] == 0 && s.pes[1] == 0 && s.pes[2] == 1) {
            const size_t header = 9 + s.pes[8];
            int64_t pts = -1;
            if ((s.pes[7] & 0x80) && s.pes.size() >= 14) {
                const uint8_t* t = s.pes.data() + 9;
                pts = (static_cast<int64_t>((t[0] >> 1) & 7) << 30) | (static_cast<int64_t>(t[1]) << 22) |
                      (static_cast<int64_t>(t[2] >> 1) << 15) | (static_cast<int64_t>(t[3]) << 7) | (t[4] >> 1);
            }
            if (header < s.pes.size() && onUnit)
                onUnit(esKind(s.type), s.type, s.track, pts, s.pes.data() + header, s.pes.size() - header);
        }
        s.pes.clear();
        s.started = false;
    }
};
