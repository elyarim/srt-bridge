// AAC sound (ADTS, as it comes in MPEG-TS) with fdk-aac: mono to 7.1, LC, HE-AAC and HE-AAC v2.
// The AAC decoder of Windows takes at most 6 channels (measured 2026-11-17), so it is not used.
// Every channel is passed on as it arrives: no downmix, no upmix, no limiter, in WAV channel order.
#pragma once
#include "aacdecoder_lib.h"
#include <cstdint>
#include <stdexcept>
#include <vector>

class AudioDecoder {
    HANDLE_AACDECODER handle = nullptr;
    std::vector<INT_PCM> pcm = std::vector<INT_PCM>(8 * 4096);

public:
    int channels = 0, rate = 0;
    uint64_t errors = 0;

    AudioDecoder() {
        handle = aacDecoder_Open(TT_MP4_ADTS, 1);
        if (!handle) throw std::runtime_error("AAC decoder could not start");
        aacDecoder_SetParam(handle, AAC_PCM_MIN_OUTPUT_CHANNELS, -1);
        aacDecoder_SetParam(handle, AAC_PCM_MAX_OUTPUT_CHANNELS, -1);
        aacDecoder_SetParam(handle, AAC_PCM_OUTPUT_CHANNEL_MAPPING, 1);
        aacDecoder_SetParam(handle, AAC_PCM_LIMITER_ENABLE, 0);
    }
    ~AudioDecoder() { if (handle) aacDecoder_Close(handle); }
    AudioDecoder(const AudioDecoder&) = delete;
    AudioDecoder& operator=(const AudioDecoder&) = delete;

    // One PES payload (one or more ADTS frames). onSound(planar float, samples per channel, channels, time, rate).
    template <class F> void decode(const uint8_t* data, size_t size, int64_t time, F&& onSound) {
        // Fill reads from the end of the buffer back by "left" bytes and says how many it could not take yet.
        UCHAR* in = const_cast<UCHAR*>(data);
        const UINT total = static_cast<UINT>(size);
        UINT left = total;
        while (left > 0) {
            const UINT before = left;
            if (aacDecoder_Fill(handle, &in, &total, &left) != AAC_DEC_OK) { ++errors; return; }
            if (left == before) { ++errors; return; } // took nothing: never loop for ever
            for (;;) {
                const AAC_DECODER_ERROR e = aacDecoder_DecodeFrame(handle, pcm.data(), static_cast<INT>(pcm.size()), 0);
                if (e == AAC_DEC_NOT_ENOUGH_BITS) break;
                if (e != AAC_DEC_OK) { ++errors; if (!IS_OUTPUT_VALID(e)) continue; }
                const CStreamInfo* info = aacDecoder_GetStreamInfo(handle);
                if (!info || info->numChannels <= 0 || info->frameSize <= 0 || info->sampleRate <= 0) continue;
                channels = info->numChannels;
                rate = info->sampleRate;
                const int count = info->frameSize;
                // Channel code 7 is "7.1 wide" in the AAC standard (two extra front channels), but encoders, ffmpeg
                // among them, write ordinary 7.1 with side channels under it, and ffmpeg/vMix decode it that way.
                // fdk-aac follows the standard and hands the sender's channels 7/8 out first and 1/2 last
                // (measured 2026-11-17 with a level per channel); put them back so the order is the sender's.
                static const int wide[8] = {6, 7, 2, 3, 4, 5, 0, 1};
                const bool remap = channels == 8 && info->channelConfig == 7;
                // Interleaved 16 bit -> planar float, channel by channel.
                std::vector<float> planar(static_cast<size_t>(count) * channels);
                for (int c = 0; c < channels; ++c) {
                    float* to = planar.data() + static_cast<size_t>(c) * count;
                    const INT_PCM* from = pcm.data() + (remap ? wide[c] : c);
                    for (int i = 0; i < count; ++i) to[i] = from[static_cast<size_t>(i) * channels] / 32768.0f;
                }
                onSound(planar, count, channels, time, rate);
                time += static_cast<int64_t>(count) * 10000000 / rate;
            }
        }
    }
};
