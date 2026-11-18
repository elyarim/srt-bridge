// Several sound tracks of one signal (vMix sends 8 channels as 4 stereo tracks) become one output with all channels:
// track 1 = channels 1-2, track 2 = channels 3-4, and so on. The tracks are lined up by their time stamps.
// A track that stops is filled with silence, so the number of channels at the receiver stays the same.
#pragma once
#include <algorithm>
#include <climits>
#include <cstdint>
#include <cstdlib>
#include <vector>

class AudioJoin {
    struct Track {
        int channels = 0;
        int64_t base = 0;                     // time of a reference sample, 100 ns
        int64_t done = 0;                     // samples handed out since then; counted, so no rounding adds up
        std::vector<std::vector<float>> fifo; // one vector per channel
        size_t size() const { return fifo.empty() ? 0 : fifo[0].size(); }
        void drop(size_t n) {
            for (auto& f : fifo) f.erase(f.begin(), f.begin() + static_cast<std::ptrdiff_t>(std::min(n, f.size())));
            done += static_cast<int64_t>(n);
        }
        void restart(int64_t time, int ch) { channels = ch; base = time; done = 0; fifo.assign(static_cast<size_t>(ch), {}); }
    };
    std::vector<Track> tracks;
    int rate = 0;
    bool aligned = false;
    static constexpr int block = 1024;

    int64_t span(int64_t samples) const { return samples * 10000000 / rate; }
    int64_t startOf(const Track& t) const { return t.base + span(t.done); }
    void resetAll() { for (auto& t : tracks) { t.fifo.clear(); t.channels = 0; } aligned = false; }

public:
    static constexpr int maxChannels = 32;

    // Number of AAC tracks in the stream; a change starts over.
    void setTracks(int n) {
        if (n == static_cast<int>(tracks.size())) return;
        tracks.assign(static_cast<size_t>(n), Track{});
        aligned = false;
    }
    int trackCount() const { return static_cast<int>(tracks.size()); }

    // emit(planar, samples per channel, channels, time, rate)
    template <class F> void push(int index, const std::vector<float>& planar, int count, int channels, int64_t time, int sampleRate, F&& emit) {
        if (index < 0 || index >= static_cast<int>(tracks.size()) || sampleRate <= 0) return;
        if (sampleRate != rate) { rate = sampleRate; resetAll(); }
        Track& t = tracks[static_cast<size_t>(index)];
        if (t.channels && channels != t.channels) resetAll(); // a track changed its layout: start all over
        if (t.size() == 0) { t.restart(time, channels); }
        else if (std::llabs(time - (startOf(t) + span(static_cast<int64_t>(t.size())))) > 500000) {
            // More than 50 ms off where it should continue: the track jumped (lost data, new start); start it over.
            t.restart(time, channels);
            aligned = false;
        }
        for (int c = 0; c < channels; ++c) {
            auto& f = t.fifo[static_cast<size_t>(c)];
            f.insert(f.end(), planar.begin() + static_cast<std::ptrdiff_t>(c) * count, planar.begin() + static_cast<std::ptrdiff_t>(c + 1) * count);
        }
        drain(emit);
    }

private:
    template <class F> void drain(F&& emit) {
        size_t most = 0;
        int64_t common = LLONG_MIN;
        for (auto& t : tracks) if (t.size()) { most = std::max(most, t.size()); common = std::max(common, startOf(t)); }
        if (common == LLONG_MIN) return;
        // A track with no sound while another has 1 s waiting is treated as silent (2 channels if never heard).
        // Not less: ffmpeg (and vMix, which uses it) packs about 15 AAC frames = 320 ms into one PES per track,
        // up to its 0.7 s mux delay, so the tracks arrive in turns. 300 ms filled silence too early (measured).
        const size_t patience = static_cast<size_t>(rate);
        for (auto& t : tracks)
            if (t.size() == 0 && most > patience) {                t.restart(common, t.channels ? t.channels : 2);
                for (auto& f : t.fifo) f.assign(most, 0.0f);
            }
        for (auto& t : tracks) if (t.size() == 0) return; // still waiting for a track
        if (!aligned) {
            // Line up: every track starts at the latest first sample of all.
            for (auto& t : tracks) {
                const int64_t early = (common - startOf(t)) * rate / 10000000;                if (early > 0) t.drop(static_cast<size_t>(early));
            }
            aligned = true;
        }
        int total = 0;
        for (auto& t : tracks) total += t.channels;
        total = std::min(total, maxChannels);
        for (;;) {
            for (auto& t : tracks) if (t.size() < block) return;
            std::vector<float> out(static_cast<size_t>(block) * total);
            int c = 0;
            for (auto& t : tracks)
                for (auto& f : t.fifo) {
                    if (c < total) std::copy(f.begin(), f.begin() + block, out.begin() + static_cast<std::ptrdiff_t>(c) * block);
                    ++c;
                }
            const int64_t time = startOf(tracks[0]);
            for (auto& t : tracks) t.drop(block);
            emit(out, block, total, time, rate);
        }
    }
};
