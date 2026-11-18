// srt_bridge_aac.exe: turns the Opus sound of a WHIP sender into AAC (ADTS) for the MPEG-TS stream.
// Started by srt-bridge-whip.exe, which talks to it over stdin and stdout (binary, little endian).
//
// stdin, one record after the other:
//   u8 kind, i64 pts (48 kHz samples), u32 len, len bytes
//   kind 1 = Opus packet, kind 2 = lost packet (len = missing samples, no bytes follow), kind 3 = new sender (reset)
// stdout, one record per AAC frame:
//   i64 pts (48 kHz samples, first sample of the frame), u32 len, len bytes ADTS
// stderr: one JSON line at the start ({"ready":...}), then messages for the log.
//
// Low delay: AAC-LC stereo 48 kHz, no SBR, no afterburner. The delay of the encoder (nDelay)
// is taken off the timestamps, so sound and picture stay together.
#include "aacenc_lib.h"
#include "opus.h"
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <fcntl.h>
#include <io.h>
#include <vector>

static const int kRate = 48000;
static const int kChannels = 2;
static const int kMaxOpusSamples = 5760; // 120 ms, the longest Opus packet

static bool readAll(void* buf, size_t n) {
    return n == 0 || fread(buf, 1, n, stdin) == n;
}

struct Encoder {
    HANDLE_AACENCODER h = nullptr;
    int delay = 0;
    int frame = 1024;
    int bitrate = 192000;

    bool open() {
        close();
        if (aacEncOpen(&h, 0, kChannels) != AACENC_OK) return false;
        bool ok = aacEncoder_SetParam(h, AACENC_AOT, AOT_AAC_LC) == AACENC_OK &&
                  aacEncoder_SetParam(h, AACENC_SAMPLERATE, kRate) == AACENC_OK &&
                  aacEncoder_SetParam(h, AACENC_CHANNELMODE, MODE_2) == AACENC_OK &&
                  aacEncoder_SetParam(h, AACENC_CHANNELORDER, 1) == AACENC_OK &&
                  aacEncoder_SetParam(h, AACENC_BITRATEMODE, 0) == AACENC_OK &&
                  aacEncoder_SetParam(h, AACENC_BITRATE, bitrate) == AACENC_OK &&
                  aacEncoder_SetParam(h, AACENC_TRANSMUX, TT_MP4_ADTS) == AACENC_OK &&
                  aacEncoder_SetParam(h, AACENC_AFTERBURNER, 0) == AACENC_OK &&
                  aacEncEncode(h, nullptr, nullptr, nullptr, nullptr) == AACENC_OK;
        if (!ok) return false;
        AACENC_InfoStruct info{};
        if (aacEncInfo(h, &info) != AACENC_OK) return false;
        delay = (int)info.nDelay;
        frame = (int)info.frameLength;
        return true;
    }
    void close() {
        if (h) aacEncClose(&h);
        h = nullptr;
    }
};

struct State {
    OpusDecoder* dec = nullptr;
    Encoder enc;
    bool started = false;
    int64_t basePts = 0;   // pts of the first sample fed to the encoder since the last reset
    int64_t fed = 0;       // samples (per channel) fed since basePts
    int64_t frames = 0;    // AAC frames written since basePts
    std::vector<uint8_t> out;
};

static void writeFrame(State& s, const uint8_t* data, uint32_t len) {
    int64_t pts = s.basePts + s.frames * s.enc.frame - s.enc.delay;
    s.frames++;
    fwrite(&pts, 8, 1, stdout);
    fwrite(&len, 4, 1, stdout);
    fwrite(data, 1, len, stdout);
    fflush(stdout);
}

// feed gives interleaved 16-bit stereo samples to the encoder and writes every finished frame.
static bool feed(State& s, const int16_t* pcm, int samples) {
    int offset = 0;
    while (offset < samples) {
        void* inPtr = (void*)(pcm + offset * kChannels);
        INT inId = IN_AUDIO_DATA, inSize = (samples - offset) * kChannels * 2, inElem = 2;
        INT inCount = (samples - offset) * kChannels;
        AACENC_BufDesc inBuf{1, &inPtr, &inId, &inSize, &inElem};
        AACENC_InArgs inArgs{inCount, 0};
        void* outPtr = s.out.data();
        INT outId = OUT_BITSTREAM_DATA, outSize = (INT)s.out.size(), outElem = 1;
        AACENC_BufDesc outBuf{1, &outPtr, &outId, &outSize, &outElem};
        AACENC_OutArgs outArgs{};
        AACENC_ERROR err = aacEncEncode(s.enc.h, &inBuf, &outBuf, &inArgs, &outArgs);
        if (err != AACENC_OK) {
            fprintf(stderr, "AAC encoder error 0x%x\n", (unsigned)err);
            return false;
        }
        if (outArgs.numOutBytes > 0) writeFrame(s, s.out.data(), (uint32_t)outArgs.numOutBytes);
        int used = outArgs.numInSamples / kChannels;
        if (used <= 0 && outArgs.numOutBytes <= 0) break; // nothing taken and nothing given: wait for more
        offset += used;
    }
    s.fed += samples;
    return true;
}

static bool reset(State& s, int64_t pts) {
    if (!s.enc.open()) {
        fprintf(stderr, "AAC encoder could not be opened\n");
        return false;
    }
    opus_decoder_ctl(s.dec, OPUS_RESET_STATE);
    s.started = true;
    s.basePts = pts;
    s.fed = 0;
    s.frames = 0;
    return true;
}

int main(int argc, char** argv) {
    _setmode(_fileno(stdin), _O_BINARY);
    _setmode(_fileno(stdout), _O_BINARY);
    State s;
    for (int i = 1; i + 1 < argc; i++) {
        if (!strcmp(argv[i], "--bitrate")) s.enc.bitrate = atoi(argv[i + 1]);
    }
    if (s.enc.bitrate < 64000 || s.enc.bitrate > 320000) s.enc.bitrate = 192000;
    int err = 0;
    s.dec = opus_decoder_create(kRate, kChannels, &err);
    if (!s.dec || err != OPUS_OK || !s.enc.open()) {
        fprintf(stderr, "{\"ready\":false,\"error\":\"codec could not be opened\"}\n");
        return 1;
    }
    s.out.resize(8192);
    fprintf(stderr, "{\"ready\":true,\"delay\":%d,\"frame\":%d,\"bitrate\":%d,\"opus\":\"%s\"}\n", s.enc.delay, s.enc.frame, s.enc.bitrate, opus_get_version_string());
    fflush(stderr);

    std::vector<int16_t> pcm(kMaxOpusSamples * kChannels);
    std::vector<uint8_t> packet;
    for (;;) {
        uint8_t kind;
        int64_t pts;
        uint32_t len;
        if (!readAll(&kind, 1) || !readAll(&pts, 8) || !readAll(&len, 4)) break; // pipe closed: the end
        if (kind == 1) {
            if (len > 4000) return 2;
            packet.resize(len);
            if (!readAll(packet.data(), len)) break;
        }
        if (kind == 3) {
            s.started = false;
            continue;
        }
        if (!s.started && !reset(s, pts)) return 3;
        // Where this packet belongs on the timeline of the encoder.
        int64_t expect = s.basePts + s.fed;
        int64_t gap = pts - expect;
        if (gap > kRate || gap < -kRate / 10) {
            // More than a second missing, or far too early: start again from here.
            if (!reset(s, pts)) return 3;
            gap = 0;
        }
        while (gap > 0) { // a hole the sender did not tell about: filled with silence, the timeline stays
            int n = gap > kMaxOpusSamples ? kMaxOpusSamples : (int)gap;
            std::fill(pcm.begin(), pcm.begin() + n * kChannels, (int16_t)0);
            if (!feed(s, pcm.data(), n)) return 4;
            gap -= n;
        }
        int samples = 0;
        if (kind == 1) {
            samples = opus_decode(s.dec, packet.data(), (opus_int32)len, pcm.data(), kMaxOpusSamples, 0);
        } else if (kind == 2) {
            int want = (int)len;
            if (want <= 0 || want > kMaxOpusSamples) want = 960;
            samples = opus_decode(s.dec, nullptr, 0, pcm.data(), want, 0); // packet loss concealment
        }
        if (samples < 0) {
            fprintf(stderr, "Opus decode error %d\n", samples);
            continue;
        }
        int skip = gap < 0 ? (int)(-gap) : 0; // overlaps what was fed already
        if (skip >= samples) continue;
        if (!feed(s, pcm.data() + skip * kChannels, samples - skip)) return 4;
    }
    s.enc.close();
    if (s.dec) opus_decoder_destroy(s.dec);
    return 0;
}
