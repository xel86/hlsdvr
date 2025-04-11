package hls

import (
	"reflect"
	"testing"
	"time"
)

func TestParseM3U8VariantPlaylist(t *testing.T) {
	playlist := `#EXTM3U
#EXT-X-MEDIA:TYPE=VIDEO,GROUP-ID="chunked",NAME="1080p60 (source)",AUTOSELECT=YES,DEFAULT=YES
#EXT-X-STREAM-INF:BANDWIDTH=6810351,RESOLUTION=1920x1080,CODECS="avc1.64002A,mp4a.40.2",VIDEO="chunked",FRAME-RATE=60.000
https://hls-1.m3u8
#EXT-X-MEDIA:TYPE=VIDEO,GROUP-ID="720p60",NAME="720p60",AUTOSELECT=YES,DEFAULT=YES
#EXT-X-STREAM-INF:BANDWIDTH=3422999,RESOLUTION=1280x720,CODECS="avc1.4D401F,mp4a.40.2",VIDEO="720p60",FRAME-RATE=60.000
https://hls-2.m3u8
#EXT-X-MEDIA:TYPE=VIDEO,GROUP-ID="720p30",NAME="720p",AUTOSELECT=YES,DEFAULT=YES
#EXT-X-STREAM-INF:BANDWIDTH=2373000,RESOLUTION=1280x720,CODECS="avc1.4D401F,mp4a.40.2",VIDEO="720p30",FRAME-RATE=30.000
https://hls-3.m3u8
#EXT-X-MEDIA:TYPE=VIDEO,GROUP-ID="480p30",NAME="480p",AUTOSELECT=YES,DEFAULT=YES
#EXT-X-STREAM-INF:BANDWIDTH=1427999,RESOLUTION=852x480,CODECS="avc1.4D401F,mp4a.40.2",VIDEO="480p30",FRAME-RATE=29.999
https://hls-4.m3u8`

	expected := []M3U8StreamVariant{
		M3U8StreamVariant{Url: "https://hls-1.m3u8", Bandwidth: 6810351, Width: 1920, Height: 1080, FrameRate: 60.0},
		M3U8StreamVariant{Url: "https://hls-2.m3u8", Bandwidth: 3422999, Width: 1280, Height: 720, FrameRate: 60.0},
		M3U8StreamVariant{Url: "https://hls-3.m3u8", Bandwidth: 2373000, Width: 1280, Height: 720, FrameRate: 30.0},
		M3U8StreamVariant{Url: "https://hls-4.m3u8", Bandwidth: 1427999, Width: 852, Height: 480, FrameRate: 29.999},
	}

	variants, err := parseM3U8VariantPlaylist(playlist)
	if err != nil {
		t.Errorf("failed to parse m3u8 variant playlist: %v", err)
		return
	}

	if !reflect.DeepEqual(variants, expected) {
		t.Errorf("stream variants were not parsed as expected: %+v", variants)
	}
}

func TestParseM3U8MediaPlaylist(t *testing.T) {
	playlist := `#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:6
#EXT-X-MEDIA-SEQUENCE:2523
#EXT-X-CUSTOM-START:test
#EXT-X-PROGRAM-DATE-TIME:2025-03-28T02:37:11.689Z
#EXTINF:2.000,live
https://segment-1.ts
#EXT-X-PROGRAM-DATE-TIME:2025-03-28T02:37:13.689Z
#EXTINF:2.000,live
https://segment-2.ts
#EXT-X-PROGRAM-DATE-TIME:2025-03-28T02:37:15.689Z
#EXTINF:2.000,live
https://segment-3.ts
#EXT-X-PROGRAM-DATE-TIME:2025-03-28T02:37:17.689Z
#EXTINF:2.000,live
https://segment-4.ts
#EXT-X-CUSTOM-END:https://test-custom`

	time1, _ := time.Parse(time.RFC3339Nano, "2025-03-28T02:37:11.689Z")
	time2, _ := time.Parse(time.RFC3339Nano, "2025-03-28T02:37:13.689Z")
	time3, _ := time.Parse(time.RFC3339Nano, "2025-03-28T02:37:15.689Z")
	time4, _ := time.Parse(time.RFC3339Nano, "2025-03-28T02:37:17.689Z")
	expected := M3U8MediaPlaylist{
		TargetDuration:     6,
		MediaSequenceStart: 2523,
		Segments: []M3U8Segment{
			M3U8Segment{Url: "https://segment-1.ts", DateTime: time1, MediaSequenceNum: 2523},
			M3U8Segment{Url: "https://segment-2.ts", DateTime: time2, MediaSequenceNum: 2524},
			M3U8Segment{Url: "https://segment-3.ts", DateTime: time3, MediaSequenceNum: 2525},
			M3U8Segment{Url: "https://segment-4.ts", DateTime: time4, MediaSequenceNum: 2526},
		},
	}

	actual, err := parseM3U8MediaPlaylist(playlist)
	if err != nil {
		t.Errorf("failed to parse m3u8 media playlist: %v", err)
		return
	}

	// oh it would be too easy to use reflect.DeepEqual here but go complains because M3U8Segment
	// contains an embedded error field that it doesn't like...
	if actual.TargetDuration != expected.TargetDuration {
		t.Errorf("m3u8 media playlist target duration parsed incorrectly: \n%d\n vs \n%d\n",
			expected.TargetDuration, actual.TargetDuration)
	}
	if actual.MediaSequenceStart != expected.MediaSequenceStart {
		t.Errorf("m3u8 media playlist media sequence parsed incorrectly: \n%d\n vs \n%d\n",
			expected.MediaSequenceStart, actual.MediaSequenceStart)
	}
	if len(actual.Segments) != len(expected.Segments) {
		t.Errorf("m3u8 media playlist segments parsed incorrectly: \n%+v\n vs \n%+v\n",
			expected.Segments, actual.Segments)
	}

	for i := range actual.Segments {
		if (actual.Segments[i].Url != expected.Segments[i].Url) &&
			(actual.Segments[i].DateTime != expected.Segments[i].DateTime) &&
			(actual.Segments[i].MediaSequenceNum != expected.Segments[i].MediaSequenceNum) {
			t.Errorf("m3u8 media playlist segments parsed incorrectly: \n%+v\n vs \n%+v\n",
				expected.Segments, actual.Segments)
		}
	}
}

func TestSortStreamVariants(t *testing.T) {
	actual := []M3U8StreamVariant{
		M3U8StreamVariant{Url: "worst.m3u8", Width: 284, Height: 160, FrameRate: 30.0},
		M3U8StreamVariant{Url: "poor.m3u8", Width: 640, Height: 360, FrameRate: 30.0},
		M3U8StreamVariant{Url: "best.m3u8", Width: 1920, Height: 1080, FrameRate: 60.0},
		M3U8StreamVariant{Url: "bandwidth.m3u8", Bandwidth: 1, Width: 1920, Height: 1080, FrameRate: 60.0},
		M3U8StreamVariant{Url: "medium.m3u8", Width: 1280, Height: 720, FrameRate: 30.0},
		M3U8StreamVariant{Url: "good.m3u8", Width: 1280, Height: 720, FrameRate: 60.0},
	}

	expected := []M3U8StreamVariant{
		M3U8StreamVariant{Url: "bandwidth.m3u8", Bandwidth: 1, Width: 1920, Height: 1080, FrameRate: 60.0},
		M3U8StreamVariant{Url: "best.m3u8", Width: 1920, Height: 1080, FrameRate: 60.0},
		M3U8StreamVariant{Url: "good.m3u8", Width: 1280, Height: 720, FrameRate: 60.0},
		M3U8StreamVariant{Url: "medium.m3u8", Width: 1280, Height: 720, FrameRate: 30.0},
		M3U8StreamVariant{Url: "poor.m3u8", Width: 640, Height: 360, FrameRate: 30.0},
		M3U8StreamVariant{Url: "worst.m3u8", Width: 284, Height: 160, FrameRate: 30.0},
	}

	sortStreamVariants(actual)

	if !reflect.DeepEqual(actual, expected) {
		t.Errorf("stream variants were not sorted as expected: %+v", actual)
	}
}
