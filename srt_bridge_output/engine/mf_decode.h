// Decoders of Windows (Media Foundation): H.264 / HEVC pictures. Sound: aac_decode.h (fdk-aac),
// because the AAC decoder of Windows stops at 5.1.
// No FFmpeg. HEVC needs the "HEVC Video Extensions" of Windows on this PC.
#pragma once
#include <mfapi.h>
#include <mfidl.h>
#include <mftransform.h>
#include <mferror.h>
#include <wrl/client.h>
#include <cstdint>
#include <stdexcept>
#include <string>
#include <vector>

using Microsoft::WRL::ComPtr;

inline void mfCheck(HRESULT hr, const char* what) {
    if (FAILED(hr)) {
        char text[200];
        sprintf_s(text, "%s failed (0x%08lX)", what, static_cast<unsigned long>(hr));
        throw std::runtime_error(text);
    }
}

// CODECAPI_AVLowLatencyMode: the decoder hands out a picture as soon as it has it.
inline const GUID lowLatencyMode = {0x9c27891a, 0xed7a, 0x40e1, {0x88, 0xe8, 0xb2, 0x27, 0x27, 0xa0, 0x24, 0xee}};

inline ComPtr<IMFTransform> findDecoder(const GUID& category, const GUID& major, const GUID& subtype, const char* name) {
    MFT_REGISTER_TYPE_INFO input{major, subtype};
    IMFActivate** list = nullptr;
    UINT32 count = 0;
    mfCheck(MFTEnumEx(category, MFT_ENUM_FLAG_SYNCMFT | MFT_ENUM_FLAG_SORTANDFILTER, &input, nullptr, &list, &count), "Decoder search");
    ComPtr<IMFTransform> result;
    HRESULT hr = E_FAIL;
    for (UINT32 i = 0; i < count; ++i) {
        if (!result) hr = list[i]->ActivateObject(IID_PPV_ARGS(&result));
        list[i]->Release();
    }
    CoTaskMemFree(list);
    if (!result) throw std::runtime_error(std::string("Windows has no decoder for ") + name + (count ? " (it could not be started)" : ""));
    (void)hr;
    return result;
}

inline ComPtr<IMFSample> makeSample(const uint8_t* data, size_t size, DWORD capacity) {
    ComPtr<IMFMediaBuffer> buffer;
    mfCheck(MFCreateMemoryBuffer(capacity, &buffer), "Buffer");
    if (data) {
        BYTE* target = nullptr;
        mfCheck(buffer->Lock(&target, nullptr, nullptr), "Buffer lock");
        memcpy(target, data, size);
        buffer->Unlock();
        buffer->SetCurrentLength(static_cast<DWORD>(size));
    }
    ComPtr<IMFSample> sample;
    mfCheck(MFCreateSample(&sample), "Sample");
    sample->AddBuffer(buffer.Get());
    return sample;
}

// One decoded picture, NV12, still inside the decoder's buffer.
struct Nv12 {
    const uint8_t* luma;
    const uint8_t* chroma;
    int stride, width, height;
    bool interlaced;
    int64_t time; // 100 ns
};

class VideoDecoder {
    ComPtr<IMFTransform> mft;
    bool provides = false;
    DWORD outSize = 0;
    int width = 0, height = 0, coded = 0, stride = 0;

    bool pickOutput() {
        for (DWORD i = 0;; ++i) {
            ComPtr<IMFMediaType> type;
            if (FAILED(mft->GetOutputAvailableType(0, i, &type))) return false;
            GUID subtype{};
            type->GetGUID(MF_MT_SUBTYPE, &subtype);
            if (subtype != MFVideoFormat_NV12) continue;
            if (FAILED(mft->SetOutputType(0, type.Get(), 0))) return false;
            UINT32 w = 0, h = 0;
            MFGetAttributeSize(type.Get(), MF_MT_FRAME_SIZE, &w, &h);
            width = static_cast<int>(w); coded = height = static_cast<int>(h);
            MFVideoArea area{};
            if (SUCCEEDED(type->GetBlob(MF_MT_MINIMUM_DISPLAY_APERTURE, reinterpret_cast<UINT8*>(&area), sizeof(area), nullptr)) &&
                area.Area.cx > 0 && area.Area.cy > 0) {
                width = area.Area.cx; height = area.Area.cy;
            } else if (h == 1088) height = 1080;
            stride = static_cast<int>(MFGetAttributeUINT32(type.Get(), MF_MT_DEFAULT_STRIDE, w));
            if (stride <= 0) stride = static_cast<int>(w);
            MFT_OUTPUT_STREAM_INFO info{};
            mft->GetOutputStreamInfo(0, &info);
            provides = (info.dwFlags & (MFT_OUTPUT_STREAM_PROVIDES_SAMPLES | MFT_OUTPUT_STREAM_CAN_PROVIDE_SAMPLES)) != 0;
            outSize = info.cbSize ? info.cbSize : static_cast<DWORD>(stride * coded * 3 / 2);
            return true;
        }
    }

public:
    uint64_t errors = 0;

    explicit VideoDecoder(bool hevc) {
        mft = findDecoder(MFT_CATEGORY_VIDEO_DECODER, MFMediaType_Video, hevc ? MFVideoFormat_HEVC : MFVideoFormat_H264,
                          hevc ? "HEVC (install the HEVC Video Extensions)" : "H.264");
        ComPtr<IMFAttributes> attributes;
        if (SUCCEEDED(mft->GetAttributes(&attributes)) && attributes) attributes->SetUINT32(lowLatencyMode, TRUE);
        ComPtr<IMFMediaType> type;
        mfCheck(MFCreateMediaType(&type), "Media type");
        type->SetGUID(MF_MT_MAJOR_TYPE, MFMediaType_Video);
        type->SetGUID(MF_MT_SUBTYPE, hevc ? MFVideoFormat_HEVC : MFVideoFormat_H264);
        MFSetAttributeSize(type.Get(), MF_MT_FRAME_SIZE, 1920, 1080); // a hint; the decoder reports the real size
        type->SetUINT32(MF_MT_INTERLACE_MODE, MFVideoInterlace_MixedInterlaceOrProgressive);
        mfCheck(mft->SetInputType(0, type.Get(), 0), "Decoder input type");
        pickOutput();
        mft->ProcessMessage(MFT_MESSAGE_NOTIFY_BEGIN_STREAMING, 0);
        mft->ProcessMessage(MFT_MESSAGE_NOTIFY_START_OF_STREAM, 0);
    }

    template <class F> void drain(F&& onPicture) {
        for (int guard = 0; guard < 64; ++guard) {
            MFT_OUTPUT_DATA_BUFFER out{};
            ComPtr<IMFSample> sample;
            if (!provides) { sample = makeSample(nullptr, 0, outSize); out.pSample = sample.Get(); }
            DWORD status = 0;
            HRESULT hr = mft->ProcessOutput(0, 1, &out, &status);
            if (out.pEvents) out.pEvents->Release();
            if (provides && out.pSample) sample.Attach(out.pSample);
            if (hr == MF_E_TRANSFORM_STREAM_CHANGE) { if (!pickOutput()) { ++errors; return; } continue; }
            if (hr == MF_E_TRANSFORM_NEED_MORE_INPUT) return;
            if (FAILED(hr)) { ++errors; return; }
            if (!sample) continue;
            ComPtr<IMFMediaBuffer> buffer;
            if (FAILED(sample->ConvertToContiguousBuffer(&buffer))) { ++errors; continue; }
            Nv12 picture{};
            picture.width = width; picture.height = height;
            picture.interlaced = MFGetAttributeUINT32(sample.Get(), MFSampleExtension_Interlaced, 0) != 0;
            sample->GetSampleTime(&picture.time);
            ComPtr<IMF2DBuffer> flat;
            BYTE* base = nullptr;
            LONG pitch = 0;
            if (SUCCEEDED(buffer.As(&flat)) && SUCCEEDED(flat->Lock2D(&base, &pitch)) && pitch > 0) {
                picture.luma = base; picture.stride = pitch; picture.chroma = base + static_cast<size_t>(pitch) * coded;
                onPicture(picture);
                flat->Unlock2D();
            } else if (SUCCEEDED(buffer->Lock(&base, nullptr, nullptr))) {
                picture.luma = base; picture.stride = stride; picture.chroma = base + static_cast<size_t>(stride) * coded;
                onPicture(picture);
                buffer->Unlock();
            } else ++errors;
        }
    }

    // One access unit (Annex B) in, zero or more pictures out.
    template <class F> void decode(const uint8_t* data, size_t size, int64_t time, F&& onPicture) {
        auto sample = makeSample(data, size, static_cast<DWORD>(size));
        sample->SetSampleTime(time);
        HRESULT hr = mft->ProcessInput(0, sample.Get(), 0);
        if (hr == MF_E_NOTACCEPTING) { drain(onPicture); hr = mft->ProcessInput(0, sample.Get(), 0); }
        if (FAILED(hr)) { ++errors; return; }
        drain(onPicture);
    }
};
