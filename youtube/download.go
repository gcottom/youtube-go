package youtube

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

// DownloadOptions configures the format selection, output path, and optional muxer.
type DownloadOptions struct {
	// Output is the destination path. An empty path uses the video title and ID.
	Output string
	// AppendExtension treats a nonempty Output as a stem and adds the selected file extension.
	AppendExtension bool
	// Format is a selector such as best, bestaudio, 18, or 137+140. The default is best.
	Format string
	// FFmpeg overrides the ffmpeg executable found on PATH.
	FFmpeg string
}

// SelectFormats resolves itags, best/b, bestvideo/bv, bestaudio/ba, hls, and dash.
// Join separate video and audio selectors with +, or provide alternatives with /.
// The default prefers separate streams when FFmpeg is available and the video
// stream has comparable or better resolution and frame rate than the best muxed
// stream. A manifest is a fallback when FFmpeg is available.
func SelectFormats(formats []Format, selector string, ffmpegAvailable bool) ([]Format, error) {
	if strings.TrimSpace(selector) == "" {
		selector = "best"
	}
	var lastErr error
	for _, choice := range strings.Split(selector, "/") {
		selected, err := selectFormatChoice(formats, strings.TrimSpace(choice), ffmpegAvailable)
		if err == nil {
			return selected, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("select format %q: %w", selector, lastErr)
}

func selectFormatChoice(formats []Format, selector string, ffmpegAvailable bool) ([]Format, error) {
	if first, second, combined := strings.Cut(selector, "+"); combined {
		if !ffmpegAvailable {
			return nil, errors.New("merging separate streams requires FFmpeg")
		}
		video, err := selectSingleFormat(formats, strings.TrimSpace(first))
		if err != nil {
			return nil, err
		}
		audio, err := selectSingleFormat(formats, strings.TrimSpace(second))
		if err != nil {
			return nil, err
		}
		if !isHTTPFormat(video) || !isHTTPFormat(audio) || !video.HasVideo || video.HasAudio || audio.HasVideo || !audio.HasAudio {
			return nil, errors.New("a merge requires a video-only stream followed by an audio-only stream")
		}
		return []Format{video, audio}, nil
	}
	if selector == "best" || selector == "b" {
		return selectBestFormats(formats, ffmpegAvailable)
	}
	format, err := selectSingleFormat(formats, selector)
	if err != nil {
		return nil, err
	}
	if !isHTTPFormat(format) && !ffmpegAvailable {
		return nil, fmt.Errorf("downloading %s requires FFmpeg", format.Protocol)
	}
	return []Format{format}, nil
}

func selectBestFormats(formats []Format, ffmpegAvailable bool) ([]Format, error) {
	muxed, hasMuxed := bestFormat(formats, func(f Format) bool {
		return isHTTPFormat(f) && f.HasVideo && f.HasAudio
	})
	video, videoErr := selectSingleFormat(formats, "bestvideo")
	audio, audioErr := selectSingleFormat(formats, "bestaudio")
	manifest, hasManifest := bestFormat(formats, func(f Format) bool {
		return isManifestFormat(f) && f.HasVideo && f.HasAudio
	})
	canMerge := videoErr == nil && audioErr == nil
	if ffmpegAvailable && canMerge && (!hasMuxed || compareVideoQuality(video, muxed) >= 0) {
		return []Format{video, audio}, nil
	}
	if hasMuxed {
		return []Format{muxed}, nil
	}
	if hasManifest && ffmpegAvailable {
		return []Format{manifest}, nil
	}
	if canMerge || hasManifest {
		return nil, errors.New("the available audio and video require FFmpeg")
	}
	return nil, errors.New("no playable format contains both audio and video")
}

func selectSingleFormat(formats []Format, selector string) (Format, error) {
	var match func(Format) bool
	switch selector {
	case "bestvideo", "bv":
		match = func(f Format) bool { return isHTTPFormat(f) && f.HasVideo && !f.HasAudio }
	case "bestaudio", "ba":
		match = func(f Format) bool { return isHTTPFormat(f) && f.HasAudio && !f.HasVideo }
	case "hls", "dash":
		match = func(f Format) bool { return f.Protocol == selector }
	default:
		itag, err := strconv.Atoi(selector)
		if err != nil || itag <= 0 {
			return Format{}, fmt.Errorf("unsupported selector %q", selector)
		}
		match = func(f Format) bool { return f.Itag == itag }
	}
	if format, ok := bestFormat(formats, match); ok {
		return format, nil
	}
	return Format{}, fmt.Errorf("format %q is unavailable", selector)
}

func bestFormat(formats []Format, match func(Format) bool) (Format, bool) {
	var best Format
	found := false
	for _, format := range formats {
		if (!isHTTPFormat(format) && !isManifestFormat(format)) || format.URL == "" || !match(format) {
			continue
		}
		quality := compareVideoQuality(format, best)
		if quality == 0 && format.HasAudio && best.HasAudio && format.DefaultAudio != best.DefaultAudio {
			if format.DefaultAudio {
				quality = 1
			} else {
				quality = -1
			}
		}
		if !found || quality > 0 || quality == 0 && format.Bitrate > best.Bitrate {
			best, found = format, true
		}
	}
	return best, found
}

func compareVideoQuality(a, b Format) int {
	if a.Height != b.Height {
		return a.Height - b.Height
	}
	if a.Width != b.Width {
		return a.Width - b.Width
	}
	return a.FPS - b.FPS
}

func isHTTPFormat(f Format) bool {
	return f.Protocol == "" || f.Protocol == "https" || f.Protocol == "http"
}

func isManifestFormat(f Format) bool {
	return f.Protocol == "hls" || f.Protocol == "dash"
}

// Download writes the selected media and returns its destination path. It stages
// the completed file beside its destination and refuses to overwrite existing files.
func (c *Client) Download(ctx context.Context, video *Video, opts DownloadOptions) (string, error) {
	if video == nil {
		return "", errors.New("a video is required")
	}
	ffmpeg, ffmpegErr := exec.LookPath("ffmpeg")
	if opts.FFmpeg != "" {
		ffmpeg, ffmpegErr = exec.LookPath(opts.FFmpeg)
		if ffmpegErr != nil {
			return "", fmt.Errorf("find FFmpeg: %w", ffmpegErr)
		}
	}
	formats, err := SelectFormats(video.Formats, opts.Format, ffmpegErr == nil)
	if err != nil {
		return "", err
	}
	extension := downloadExtension(formats)
	output := opts.Output
	if output == "" {
		output = safeVideoName(video) + "." + extension
	} else if opts.AppendExtension {
		output += "." + extension
	} else if len(formats) > 1 || isManifestFormat(formats[0]) {
		switch ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(output), ".")); ext {
		case "mp4", "mov", "mkv", "webm", "m4a", "ts":
			extension = ext
		}
	}
	if _, err := os.Lstat(output); err == nil {
		return "", fmt.Errorf("output %q already exists: %w", output, os.ErrExist)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("check output: %w", err)
	}
	dir := filepath.Dir(output)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create output directory: %w", err)
	}
	stage, err := os.MkdirTemp(dir, ".youtube-go-*")
	if err != nil {
		return "", fmt.Errorf("stage download: %w", err)
	}
	defer os.RemoveAll(stage)
	file := filepath.Join(stage, "media."+extension)
	switch {
	case len(formats) == 2:
		if err := c.downloadAndMerge(ctx, ffmpeg, formats, stage, file); err != nil {
			return "", err
		}
	case isManifestFormat(formats[0]):
		if err := c.downloadManifest(ctx, ffmpeg, formats[0], file); err != nil {
			return "", err
		}
	default:
		if err := c.downloadHTTP(ctx, formats[0], file); err != nil {
			return "", err
		}
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	info, err := os.Stat(file)
	if err != nil {
		return "", fmt.Errorf("check completed download: %w", err)
	}
	if info.Size() == 0 {
		return "", errors.New("downloaded media is empty")
	}
	// A hard link publishes atomically and fails if another process has already
	// created the destination. Both paths are on the destination filesystem.
	if err := os.Link(file, output); err != nil {
		return "", fmt.Errorf("publish output %q: %w", output, err)
	}
	return output, nil
}

func (c *Client) downloadHTTP(ctx context.Context, format Format, target string) error {
	if err := validateMediaURL(format.URL); err != nil {
		return err
	}
	file, err := os.Create(target)
	if err != nil {
		return fmt.Errorf("open staged media: %w", err)
	}
	written := int64(0)
	total := format.ContentLength
	for {
		n, fullLength, partErr := c.readStreamPart(ctx, format, file, written, total)
		if partErr != nil {
			err = partErr
			break
		}
		written += n
		total = fullLength
		if written == total {
			break
		}
	}
	if err := errors.Join(err, file.Close()); err != nil {
		return fmt.Errorf("download stream %d: %w", format.Itag, err)
	}
	return nil
}

func (c *Client) readStreamPart(ctx context.Context, format Format, target io.Writer, offset, total int64) (int64, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, format.URL, nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("User-Agent", c.mediaUserAgent(format))
	req.Header.Set("Accept-Encoding", "identity")
	// YouTube's CDN can stall on an unbounded request even when it serves
	// the same bytes in bounded ranges.
	if total > 0 {
		end := offset + min(mediaChunkSize, total-offset) - 1
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, end))
	} else if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	client := c.http
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, fmt.Errorf("request media: %w", redactHTTPError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return 0, 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mediaType == "text/html" || mediaType == "application/json" {
		return 0, 0, fmt.Errorf("server returned %s instead of media", mediaType)
	}
	expected := resp.ContentLength
	if resp.StatusCode == http.StatusPartialContent {
		start, end, length, ok := parseContentRange(resp.Header.Get("Content-Range"))
		if !ok || start != offset || (total > 0 && total != length) || (expected >= 0 && expected != end-start+1) {
			return 0, 0, errors.New("unexpected Content-Range in partial media response")
		}
		expected, total = end-start+1, length
	} else if offset > 0 {
		return 0, 0, errors.New("server ignored the requested media range")
	} else if total > 0 {
		if expected >= 0 && expected != total {
			return 0, 0, fmt.Errorf("response declares %d bytes, expected %d", expected, total)
		}
		expected = total
	}
	n, err := io.Copy(target, resp.Body)
	if err != nil {
		return 0, 0, fmt.Errorf("write staged media: %w", err)
	}
	if expected >= 0 && n != expected {
		return 0, 0, fmt.Errorf("response has %d bytes, expected %d", n, expected)
	}
	if resp.StatusCode == http.StatusOK {
		total = n
	}
	return n, total, nil
}

const mediaChunkSize int64 = 8 << 20

func parseContentRange(value string) (start, end, total int64, ok bool) {
	value, unitFound := strings.CutPrefix(value, "bytes ")
	bounds, size, sizeFound := strings.Cut(value, "/")
	first, last, boundsFound := strings.Cut(bounds, "-")
	if !unitFound || !sizeFound || !boundsFound {
		return 0, 0, 0, false
	}
	start, startErr := strconv.ParseInt(first, 10, 64)
	end, endErr := strconv.ParseInt(last, 10, 64)
	total, totalErr := strconv.ParseInt(size, 10, 64)
	return start, end, total, startErr == nil && endErr == nil && totalErr == nil && start >= 0 && start <= end && end < total
}

func (c *Client) downloadAndMerge(ctx context.Context, ffmpeg string, formats []Format, stage, output string) error {
	video := filepath.Join(stage, "video."+streamExtension(formats[0]))
	audio := filepath.Join(stage, "audio."+streamExtension(formats[1]))
	if err := c.downloadHTTP(ctx, formats[0], video); err != nil {
		return err
	}
	if err := c.downloadHTTP(ctx, formats[1], audio); err != nil {
		return err
	}
	return runFFmpeg(ctx, ffmpeg, "-i", video, "-i", audio, "-map", "0:v:0", "-map", "1:a:0", "-c", "copy", output)
}

func (c *Client) downloadManifest(ctx context.Context, ffmpeg string, format Format, output string) error {
	if err := validateMediaURL(format.URL); err != nil {
		return err
	}
	args := []string{"-protocol_whitelist", "http,https,tcp,tls,crypto"}
	if userAgent := c.mediaUserAgent(format); userAgent != "" {
		args = append(args, "-user_agent", userAgent)
	}
	args = append(args, "-i", format.URL)
	if !format.HasAudio {
		args = append(args, "-map", "0:v:0")
	} else if !format.HasVideo {
		args = append(args, "-map", "0:a:0")
	}
	return runFFmpeg(ctx, ffmpeg, append(args, "-c", "copy", output)...)
}

func (c *Client) mediaUserAgent(format Format) string {
	if format.userAgent != "" {
		return format.userAgent
	}
	return c.userAgent
}

func validateMediaURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return errors.New("invalid HTTP media URL")
	}
	return nil
}

func runFFmpeg(ctx context.Context, ffmpeg string, args ...string) error {
	cmd := exec.CommandContext(ctx, ffmpeg, append([]string{"-hide_banner", "-nostdin", "-loglevel", "error"}, args...)...)
	var stderr boundedLog
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return fmt.Errorf("FFmpeg failed (%w): %s", err, detail)
		}
		return fmt.Errorf("FFmpeg failed: %w", err)
	}
	return nil
}

type boundedLog struct{ buffer bytes.Buffer }

func (log *boundedLog) String() string { return log.buffer.String() }

func (log *boundedLog) Write(p []byte) (int, error) {
	const maxLog = 8192
	if left := maxLog - log.buffer.Len(); left > 0 {
		_, _ = log.buffer.Write(p[:min(len(p), left)])
	}
	return len(p), nil
}

func downloadExtension(formats []Format) string {
	first := streamExtension(formats[0])
	if len(formats) == 1 {
		return first
	}
	switch audio := streamExtension(formats[1]); {
	case first == "mp4" && (audio == "mp4" || audio == "m4a"):
		return "mp4"
	case first == "webm" && audio == "webm":
		return "webm"
	default:
		return "mkv"
	}
}

func streamExtension(format Format) string {
	if ext := strings.ToLower(format.Extension); ext != "" && len(ext) <= 8 && strings.IndexFunc(ext, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	}) == -1 {
		return ext
	}
	mediaType, _, _ := mime.ParseMediaType(format.MimeType)
	switch mediaType {
	case "audio/mp4":
		return "m4a"
	case "video/mp4":
		return "mp4"
	case "video/webm", "audio/webm":
		return "webm"
	}
	if isManifestFormat(format) {
		return "mp4"
	}
	return "bin"
}

func safeVideoName(video *Video) string {
	clean := func(s string, maxBytes int) string {
		var b strings.Builder
		for _, r := range s {
			if unicode.IsControl(r) || strings.ContainsRune(`<>:"/\|?*`, r) {
				r = '_'
			}
			if b.Len()+len(string(r)) > maxBytes {
				break
			}
			b.WriteRune(r)
		}
		return strings.Trim(b.String(), " .")
	}
	title := clean(video.Title, 150)
	if title == "" {
		title = "YouTube video"
	}
	name := title
	if id := clean(video.ID, 24); id != "" {
		name += " [" + id + "]"
	}
	base, _, _ := strings.Cut(name, ".")
	switch strings.ToUpper(strings.TrimSpace(base)) {
	case "CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9", "COM¹", "COM²", "COM³",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9", "LPT¹", "LPT²", "LPT³":
		name = "_" + name
	}
	return name
}
