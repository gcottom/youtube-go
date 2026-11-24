package youtube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var timestampLine = regexp.MustCompile(`^\s*(?:[-•]\s*)?(\d{1,3}:\d{2}(?::\d{2})?)\s+[-–—:]?\s*(\S.*?)\s*$`)

func extractChapters(page []byte, description string, duration int) []Chapter {
	if data, err := embeddedJSON(page, "ytInitialData"); err == nil {
		var payload struct {
			PlayerOverlays struct {
				PlayerOverlayRenderer struct {
					DecoratedPlayerBarRenderer struct {
						DecoratedPlayerBarRenderer struct {
							PlayerBar struct {
								ChapteredPlayerBarRenderer struct {
									Chapters []struct {
										ChapterRenderer struct {
											Title                struct{ SimpleText string }
											TimeRangeStartMillis json.Number
										}
									}
								}
							}
						}
					}
				}
			}
		}
		if json.Unmarshal(data, &payload) == nil {
			var chapters []Chapter
			for _, wrapper := range payload.PlayerOverlays.PlayerOverlayRenderer.DecoratedPlayerBarRenderer.
				DecoratedPlayerBarRenderer.PlayerBar.ChapteredPlayerBarRenderer.Chapters {
				chapter := wrapper.ChapterRenderer
				ms, err := chapter.TimeRangeStartMillis.Int64()
				if title := strings.TrimSpace(chapter.Title.SimpleText); err == nil && title != "" && ms >= 0 {
					chapters = append(chapters, Chapter{Title: title, StartSeconds: int(ms / 1000)})
				}
			}
			if result := chapterEnds(chapters, duration); len(result) > 0 {
				return result
			}
		}
	}

	var chapters []Chapter
	for _, line := range strings.Split(description, "\n") {
		match := timestampLine.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		var seconds int
		valid := true
		for i, piece := range strings.Split(match[1], ":") {
			part, _ := strconv.Atoi(piece)
			valid = valid && (i == 0 || part < 60)
			seconds = seconds*60 + part
		}
		if valid {
			chapters = append(chapters, Chapter{Title: match[2], StartSeconds: seconds})
		}
	}
	if len(chapters) < 3 {
		return nil
	}
	return chapterEnds(chapters, duration)
}

func chapterEnds(chapters []Chapter, duration int) []Chapter {
	if len(chapters) == 0 || chapters[0].StartSeconds != 0 {
		return nil
	}
	for i := range chapters {
		if i+1 < len(chapters) {
			end := chapters[i+1].StartSeconds
			if end <= chapters[i].StartSeconds || (duration > 0 && end >= duration) {
				return nil
			}
			chapters[i].EndSeconds = end
		} else {
			chapters[i].EndSeconds = duration
		}
	}
	return chapters
}

// WriteSubtitle fetches a WebVTT track. Language defaults to the first
// available track, with manually written tracks preferred over automatic ones.
func (c *Client) WriteSubtitle(ctx context.Context, v *Video, language, output string) (string, error) {
	if v == nil {
		return "", errors.New("video is required")
	}
	var selected *Caption
	for i := range v.Captions {
		track := &v.Captions[i]
		if language != "" && !strings.EqualFold(track.Language, language) {
			continue
		}
		if selected == nil || (selected.Automatic && !track.Automatic) {
			selected = track
		}
	}
	if selected == nil {
		if language != "" {
			return "", fmt.Errorf("no YouTube subtitle track for %q", language)
		}
		return "", errors.New("no YouTube subtitle tracks were returned")
	}
	data, err := c.fetchSubtitle(ctx, selected)
	if err != nil {
		return "", err
	}
	if output == "" {
		suffix := selected.Language
		for _, r := range suffix {
			if r != '-' && r != '_' && (r < '0' || r > '9') && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') {
				suffix = "captions"
				break
			}
		}
		output = safeVideoName(v) + "." + suffix + ".vtt"
	}
	return output, writeMetadataFile(output, data)
}

func (c *Client) fetchSubtitle(ctx context.Context, caption *Caption) ([]byte, error) {
	sources := caption.sources
	if len(sources) == 0 {
		sources = []captionSource{{caption.URL, c.userAgent}}
	}
	var failures []error
	for _, source := range sources {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		u := mediaURL(source.url)
		if u == nil {
			failures = append(failures, errors.New("invalid YouTube subtitle URL"))
			continue
		}
		query := u.Query()
		query.Set("fmt", "vtt")
		query.Del("xosf")
		u.RawQuery = query.Encode()
		headers := http.Header{}
		headers.Set("User-Agent", source.userAgent)
		data, err := c.request(ctx, http.MethodGet, u.String(), nil, headers)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if bytes.HasPrefix(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), []byte("WEBVTT")) {
			return data, nil
		}
	}
	return nil, errors.Join(append([]error{fmt.Errorf("YouTube did not return a WebVTT track for %q", caption.Language)}, failures...)...)
}

// WriteThumbnail fetches the largest available thumbnail.
func (c *Client) WriteThumbnail(ctx context.Context, v *Video, output string) (string, error) {
	if v == nil || len(v.Thumbnails) == 0 {
		return "", errors.New("no YouTube thumbnails were returned")
	}
	best := slices.MaxFunc(v.Thumbnails, func(a, b Thumbnail) int { return a.Width - b.Width })
	u := mediaURL(best.URL)
	if u == nil {
		return "", errors.New("invalid YouTube thumbnail URL")
	}
	data, err := c.request(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	ext := map[string]string{"image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp", "image/gif": ".gif"}[http.DetectContentType(data)]
	if ext == "" {
		return "", errors.New("YouTube thumbnail response was not a supported image")
	}
	if output == "" {
		output = safeVideoName(v) + ext
	}
	return output, writeMetadataFile(output, data)
}

func writeMetadataFile(output string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	n, writeErr := file.Write(data)
	if writeErr == nil && n != len(data) {
		writeErr = io.ErrShortWrite
	}
	if err := errors.Join(writeErr, file.Close()); err != nil {
		_ = os.Remove(output)
		return err
	}
	return nil
}
