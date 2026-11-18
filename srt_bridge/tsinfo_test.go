package main

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestNorm(t *testing.T) {
	cases := []struct {
		h    int
		i    bool
		rate float64
		want string
	}{
		{1080, false, 50.02, "1080p50"},
		{1080, false, 59.95, "1080p59.94"},
		{720, false, 25, "720p25"},
		{1080, true, 25, "1080i50"},
		{1080, true, 50, "1080i50"},
		{2160, false, 0, "2160p"},
		{1080, false, 23.97, "1080p23.98"},
	}
	for _, c := range cases {
		if got := norm(c.h, c.i, c.rate); got != c.want {
			t.Errorf("norm(%d,%v,%v) = %q, want %q", c.h, c.i, c.rate, got, c.want)
		}
	}
}

// TestProbeFiles reads real TS files, for example made with ffmpeg:
// SRT_TS_FILES="a.ts=1080p50 HEVC 2,2,2,2 AAC;b.ts=..." go test -run TestProbeFiles -v .
func TestProbeFiles(t *testing.T) {
	list := os.Getenv("SRT_TS_FILES")
	if list == "" {
		t.Skip("SRT_TS_FILES not set")
	}
	for _, item := range strings.Split(list, ";") {
		name, want, _ := strings.Cut(item, "=")
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		p := newTSProbe()
		for off := 0; off+7*188 <= len(b); off += 7 * 188 {
			p.feed(b[off : off+7*188])
		}
		m := p.out.Load()
		if m == nil {
			t.Errorf("%s: nothing found", name)
			continue
		}
		var ch []string
		for _, c := range m.Audio {
			ch = append(ch, strconv.Itoa(c))
		}
		got := m.Video + " " + m.VideoCodec + " " + strings.Join(ch, ",") + " " + m.AudioCodec
		if got != want {
			t.Errorf("%s: got %q, want %q", name, got, want)
		} else {
			t.Logf("%s: %s", name, got)
		}
	}
}
