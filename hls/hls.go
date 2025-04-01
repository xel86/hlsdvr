package hls

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Individual streams variants (m3u8 playlists) found in a m3u8 variant playlist
type M3U8StreamVariant struct {
	Url       string
	Width     int
	Height    int
	FrameRate float64
	Bandwidth int
}

// A m3u8 media playlist with multiple links to downloadable segments to a live stream
type M3U8MediaPlaylist struct {
	TargetDuration     int // In seconds
	MediaSequenceStart int
	Segments           []M3U8Segment
	Ended              bool
}

// A segment from a m3u8 media playlist containing a url to a single downloadable video segment.
type M3U8Segment struct {
	Url              string
	MediaSequenceNum int

	// Optional tag(s) that may not be present.
	DateTime time.Time

	// The actual media data in bytes that was downloaded from the segment's url.
	// The reason this is inside the struct is to make it easy to preserve the order of the segments
	// after sending them into multiple goroutines to download by simply passing it as a reference.
	Data *SegmentData
}

type SegmentData struct {
	Bytes []byte
	Err   error
}

type Recorder struct {
	ctx        context.Context
	identifier string // string to help identify what this recorder is recording in logs
	httpClient *http.Client
	url        string
	outputPath string

	// m3u8 media playlists are "sliding" when live; the oldest segments get pushed out when new ones come in.
	// This means between two checks for the same media playlist, there may be overlapping segments.
	// To avoid this we keep track of the media sequence numbers we've attempted to download.
	// An m3u8 media playlist gives a single media sequence number which correspondes to the first segment present.
	// sequence numbers for each additional segment are found by simply incrementing by 1 for each.
	// https://datatracker.ietf.org/doc/html/rfc8216#section-6.3.5
	lastSeenSeqNum int
}

type RecordingReport struct {
	BytesDownloaded   int
	RecordingDuration time.Time
	GracefulEnd       bool // This is true only if the m3u8 playlist delievered the #EXT-X-ENDLIST tag.
}

func GetM3U8PlaylistData(httpClient *http.Client, url string) (string, error) {
	resp, err := httpClient.Get(url)
	if err != nil {
		return "", fmt.Errorf("error performaing GET request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("error, received status code %v: %v",
			resp.StatusCode, string(body))
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read request response body: %w", err)
	}

	playlist := string(bodyBytes)
	if !strings.Contains(playlist, "#EXTM3U") {
		return "", fmt.Errorf("playlist response does not appear to be valid m3u8")
	}

	return playlist, nil
}

// Return a sorted (by resolution & frame rate) slice of stream variants from a m3u8 variant playlist.
// The first element will be the stream with the best quality (highest resolution and frame rate).
func GetM3U8StreamVariants(httpClient *http.Client, variantPlaylistUrl string) ([]M3U8StreamVariant, error) {
	playlistData, err := GetM3U8PlaylistData(httpClient, variantPlaylistUrl)
	if err != nil {
		return nil, fmt.Errorf("error getting m3u8 playlist data: %v", err)
	}
	streamVariants, err := parseM3U8VariantPlaylist(playlistData)
	if err != nil {
		return nil, fmt.Errorf("error parsing m3u8 variant playlist: %v", err)
	}

	sortStreamVariants(streamVariants)

	return streamVariants, nil
}

func parseM3U8VariantPlaylist(playlistData string) ([]M3U8StreamVariant, error) {
	var streams []M3U8StreamVariant

	lines := strings.Split(playlistData, "\n")

	for i, line := range lines {
		line = strings.TrimSpace(line)

		if len(line) == 0 {
			continue
		}

		if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			stream := M3U8StreamVariant{}

			bandwidthMatch := regexp.MustCompile(`BANDWIDTH=(\d+)`).FindStringSubmatch(line)
			if len(bandwidthMatch) >= 2 {
				stream.Bandwidth, _ = strconv.Atoi(bandwidthMatch[1])
			}

			resolutionMatch := regexp.MustCompile(`RESOLUTION=(\d+)x(\d+)`).FindStringSubmatch(line)
			if len(resolutionMatch) >= 3 {
				stream.Width, _ = strconv.Atoi(resolutionMatch[1])
				stream.Height, _ = strconv.Atoi(resolutionMatch[2])
			}

			frameRateMatch := regexp.MustCompile(`FRAME-RATE=([\d\.]+)`).FindStringSubmatch(line)
			if len(frameRateMatch) >= 2 {
				stream.FrameRate, _ = strconv.ParseFloat(frameRateMatch[1], 64)
			}

			// Get the stream m3u8 url from the next line
			// Only append the stream to the list if there is a url line.
			if (i + 1) < len(lines) {
				url := strings.TrimSpace(lines[i+1])
				if !strings.HasPrefix(url, "#") {
					stream.Url = url
					streams = append(streams, stream)
				}
			}
		}
	}

	if len(streams) == 0 {
		return nil, fmt.Errorf("No valid m3u8 urls found in variant playlist")
	}

	return streams, nil
}

// Sort an array of stream variants by quality
// first taking into account bandwidth, if available
// then taking into account the resolution (width & height) and the frame rate.
func sortStreamVariants(variants []M3U8StreamVariant) {
	sort.Slice(variants, func(i, j int) bool {
		if variants[i].Bandwidth != variants[j].Bandwidth {
			return variants[i].Bandwidth > variants[j].Bandwidth
		}

		if variants[i].Height != variants[j].Height {
			return variants[i].Height > variants[j].Height
		}

		if variants[i].Width != variants[j].Width {
			return variants[i].Width > variants[j].Width
		}

		return variants[i].FrameRate > variants[j].FrameRate
	})
}

func NewRecorder(ctx context.Context, identifer string,
	httpClient *http.Client, url string, outputPath string) *Recorder {
	return &Recorder{
		ctx:            ctx,
		identifier:     identifer,
		httpClient:     httpClient,
		url:            url,
		outputPath:     outputPath,
		lastSeenSeqNum: 0,
	}
}

func (r *Recorder) Record() (RecordingReport, error) {
	var report RecordingReport

	outFile, err := os.Create(r.outputPath)
	if err != nil {
		return report, fmt.Errorf("error creating initial file (%s): %v", r.outputPath, err)
	}
	defer outFile.Close()

	// Reload the m3u8 media playlist every X seconds designated by the target duration fetched.
	// Parse the playlist to get important tags and their values
	// Filter out segments we have already received & downloaded, and write them to disk.
	// We will allow a single retry for any failures related to getting or parsing the m3u8 playlist.
	// If we fail to parse or get the playlist two times in a row we will stop the recording with an error.
	retry := true
	for {
		if r.ctx.Err() != nil {
			return report, nil
		}

		playlistData, err := GetM3U8PlaylistData(r.httpClient, r.url)
		if err != nil {
			if retry {
				slog.Warn(fmt.Sprintf("hls (%s): failed to get m3u8 media playlist: %v. Retrying...", r.identifier, err))
				retry = false
				time.Sleep(1 * time.Second) // Arbitrary 1 second sleep in hope that playlist fixes itself.
				continue
			}
			return report, fmt.Errorf("failed to get m3u8 media playlist data: %v", err)
		}

		playlist, err := parseM3U8MediaPlaylist(playlistData)
		if err != nil {
			if retry {
				slog.Warn(fmt.Sprintf("hls (%s): failed to parse m3u8 media playlist: %v. Retrying...", r.identifier, err))
				retry = false
				time.Sleep(1 * time.Second) // Arbitrary 1 second sleep in hope that playlist fixes itself.
				continue
			}
			return report, fmt.Errorf("failed to parse m3u8 media playlist: %v", err)
		}

		// QUESTION: if there is no changes to the media playlist after target duration timed refresh
		// then the specification asks to wait for one-half the target duration before retrying?
		// https://datatracker.ietf.org/doc/html/rfc8216#section-6.3.4
		ticker := time.NewTicker(time.Duration(playlist.TargetDuration) * time.Second)

		// Discard any segments from the window we have already attempted to download on previous playlist reloads.
		nextNewSegmentIdx := 0
		for i, segment := range playlist.Segments {
			if r.lastSeenSeqNum >= segment.MediaSequenceNum {
				nextNewSegmentIdx = (i + 1)
			} else {
				break
			}
		}
		playlist.Segments = playlist.Segments[nextNewSegmentIdx:]

		// TODO: if there len(playlist.Segments) == 0 for X amount of iterations/seconds should we return?

		// QUESTION: if we contiously fail to download or get any new segments
		// should we return instead of contiously trying until the playlist is 404'd or ended?
		err = r.downloadSegmentsData(playlist.Segments)
		if err != nil {
			var segmentErrors string
			for _, segment := range playlist.Segments {
				if segment.Data.Err != nil {
					segmentErrors += segment.Data.Err.Error() + " "
				}
			}
			slog.Warn(fmt.Sprintf("hls: %v: %s", err, segmentErrors))
		}

		// Assign the last media sequence number from the current playlist segments as the last seen one.
		// This is regardless of whether it failed to download or not.
		// If we failed to download a segment it will be skipped, as the m3u8 specification implies
		// that previously seen segment URI's should not change between playlist reloads.
		// By the time this line will be called, we have already attempted to download the segment twice.
		if len(playlist.Segments) > 0 {
			newLastSeenSeqNum := playlist.Segments[len(playlist.Segments)-1].MediaSequenceNum
			slog.Debug(fmt.Sprintf("hls (%s): processed segments %d -> %d",
				r.identifier, r.lastSeenSeqNum, newLastSeenSeqNum))
			r.lastSeenSeqNum = newLastSeenSeqNum
		}

		// Take all the segments media data and combine them into a single unified chunk
		// This will greatly reduce the amount of times we have to write to the disk.
		// PERF: would a buffered writer do the same thing and produce better performance?
		var totalLen int
		for _, s := range playlist.Segments {
			totalLen += len(s.Data.Bytes)
		}
		combinedSegments := make([]byte, totalLen)
		var i int
		for _, s := range playlist.Segments {
			i += copy(combinedSegments[i:], s.Data.Bytes)
		}

		if len(combinedSegments) > 0 {
			n, err := outFile.Write(combinedSegments)
			if err != nil {
				return report, fmt.Errorf("Error writing media segment data to file: %v", err)
			}
			report.BytesDownloaded += n
		}

		// Check if the stream/playlist has ended
		// This is set to true if the playlist sent a #EXT-X-ENDLIST tag.
		if playlist.Ended {
			report.GracefulEnd = true
			return report, nil
		}

		// If we made it through a loop with no errors reset the retry flag
		// so we on a new hiccup in the future we will retry once again.
		if !retry {
			retry = true
			slog.Info("Successfully recovered from a playlist error after retrying!")
		}

		select {
		case <-r.ctx.Done():
			return report, nil
		case <-ticker.C:
			continue
		}
	}
}

// Download each segment's media data in bytes and set it for each M3U8Segment.
// If any of the segments fail to download, this function will return a non-descript error.
// Each failed M3U8Segment will have a specific error message and reasoning set inside of it.
// It is the caller's responsibility to handle the embedded error(s) inside any failed segment(s).
func (r *Recorder) downloadSegmentsData(segments []M3U8Segment) error {
	var wg sync.WaitGroup
	errChan := make(chan bool, len(segments))

	// Download each segment in the list in parallel and maintain the order of segments
	// in the original order in the passed in segments slice.
	for _, segment := range segments {
		wg.Add(1)
		go func() {
			defer wg.Done()

			// If getting a segment fails twice,
			// consider the segment faulty and will just be missing in the recording.
			err := r.getSegment(segment)
			if err != nil {
				time.Sleep(1 * time.Second) // Arbitrary 1 second sleep in hope that the url fixes itself in that time.
				err2 := r.getSegment(segment)
				if err2 != nil {
					segment.Data.Err = fmt.Errorf("(%d): %v", segment.MediaSequenceNum, err2)
					errChan <- true
				}
			}
		}()
	}

	wg.Wait()
	close(errChan)

	if len(errChan) != 0 {
		// It is the caller of downloadSegmentsData's responsibility to parse the error messages
		// out of the indidivual M3U8Segments
		return fmt.Errorf("Failed to download all segments successfully.")
	}

	return nil
}

// Fetch the segment media data from the segment's url and place it in the segment's data byte array.
// The reason it doesn't just return a byte array is to make it easy to keep segments in order
// while spliting the downloads to multiple goroutines.
func (r *Recorder) getSegment(segment M3U8Segment) error {
	resp, err := r.httpClient.Get(segment.Url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP error: %s", resp.Status)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("Failed to read response body: %v", err)
	}

	segment.Data.Bytes = data

	return nil
}

// Parse a m3u8 media playlist string into an M3U8MediaPlaylist struct
// A media playlist contains multiple segments, and some metadata for the playlist as a whole.
// https://datatracker.ietf.org/doc/html/rfc8216
func parseM3U8MediaPlaylist(playlistData string) (M3U8MediaPlaylist, error) {
	var playlist M3U8MediaPlaylist
	var segment M3U8Segment
	var isUrlNext bool

	lines := strings.SplitSeq(playlistData, "\n")

	for line := range lines {
		line = strings.TrimSpace(line)

		if len(line) == 0 {
			continue
		}

		// Hit after a #EXTINF tag was seen in the previous loop iteration
		// Set the url for the media download, and compute the media sequence number
		// based on this segments position in the playlist.
		if isUrlNext {
			isUrlNext = false
			segment.Url = line
			segment.MediaSequenceNum = playlist.MediaSequenceStart + len(playlist.Segments)
			segment.Data = &SegmentData{}
			playlist.Segments = append(playlist.Segments, segment)
			segment = M3U8Segment{}
		}

		if strings.HasPrefix(line, "#EXT-X-TARGETDURATION:") {
			v, err := strconv.Atoi(line[22:]) // 22 = len("#EXT-X-TARGETDURATION:")
			if err != nil {
				return M3U8MediaPlaylist{}, fmt.Errorf("error parsing target duration value %v", err)
			}
			playlist.TargetDuration = v
			continue
		}

		if strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:") {
			v, err := strconv.Atoi(line[22:]) // 22 = len("#EXT-X-MEDIA-SEQUENCE")
			if err != nil {
				return M3U8MediaPlaylist{}, fmt.Errorf("error parsing media sequence value %v", err)
			}
			playlist.MediaSequenceStart = v
			continue
		}

		if strings.HasPrefix(line, "#EXT-X-PROGRAM-DATE-TIME:") {
			t, err := FullTimeParse(line[25:]) // 25 = len("#EXT-X-PROGRAM-DATE-TIME:")
			if err != nil {
				// A malformed or incorrect date time isn't important enough to error out.
				segment.DateTime = time.Time{}
			}
			segment.DateTime = t
			continue
		}

		if strings.HasPrefix(line, "#EXTINF:") {
			isUrlNext = true
			continue
		}

		if strings.HasPrefix(line, "#EXT-X-ENDLIST") {
			playlist.Ended = true
			continue
		}
	}

	if len(playlist.Segments) == 0 {
		return M3U8MediaPlaylist{}, fmt.Errorf("No valid media segments found in m3u8 playlist")
	}

	return playlist, nil
}

// FullTimeParse implements ISO/IEC 8601:2004.
// Format for timestamps encoded in media playlists.
// Example: #EXT-X-PROGRAM-DATE-TIME:2025-03-28T02:37:11.689Z
func FullTimeParse(value string) (time.Time, error) {
	layouts := []string{
		"2006-01-02T15:04:05.999999999Z0700",
		"2006-01-02T15:04:05.999999999Z07:00",
		"2006-01-02T15:04:05.999999999Z07",
	}
	var (
		err error
		t   time.Time
	)
	for _, layout := range layouts {
		if t, err = time.Parse(layout, value); err == nil {
			return t, nil
		}
	}
	return t, err
}
