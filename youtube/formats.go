package youtube

import (
	"context"
	"mime"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

type formatURL struct {
	format       Format
	url          *url.URL
	signature    string
	signatureKey string
	throttle     string
	pathThrottle bool
}

func mediaURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil {
		return nil
	}
	return u
}

func (pr *playerResponse) formatURLs() []formatURL {
	s := pr.StreamingData
	var results []formatURL
	for _, group := range [][]streamFormat{s.Formats, s.AdaptiveFormats} {
		for _, f := range group {
			if f.Itag <= 0 || len(f.DRMFamilies) > 0 {
				continue
			}
			item := formatURL{url: mediaURL(f.URL)}
			if item.url == nil {
				cipher := f.SignatureCipher
				if cipher == "" {
					cipher = f.Cipher
				}
				q, err := url.ParseQuery(cipher)
				if err != nil {
					continue
				}
				item.url, item.signature, item.signatureKey = mediaURL(q.Get("url")), q.Get("s"), q.Get("sp")
				if item.signatureKey == "" {
					item.signatureKey = "signature"
				}
			}
			if item.url == nil {
				continue
			}
			item.throttle = item.url.Query().Get("n")
			mediaType, params, _ := mime.ParseMediaType(f.MimeType)
			hasVideo, hasAudio := strings.HasPrefix(mediaType, "video/"), strings.HasPrefix(mediaType, "audio/")
			for _, codec := range strings.Split(params["codecs"], ",") {
				codec = strings.TrimSpace(codec)
				for _, prefix := range []string{"mp4a", "opus", "vorbis", "aac", "ac-3", "ec-3", "flac"} {
					hasAudio = hasAudio || strings.HasPrefix(codec, prefix)
				}
			}
			if !hasAudio && !hasVideo {
				continue
			}
			ext := strings.TrimPrefix(strings.TrimPrefix(mediaType, "video/"), "audio/")
			if ext == "mp4" && !hasVideo {
				ext = "m4a"
			}
			length, _ := strconv.ParseInt(f.ContentLength, 10, 64)
			item.format = Format{
				Itag: f.Itag, MimeType: f.MimeType, Extension: ext, Protocol: "https",
				Width: f.Width, Height: f.Height, FPS: f.FPS, Bitrate: f.Bitrate, ContentLength: length,
				HasVideo: hasVideo, HasAudio: hasAudio, AudioTrack: f.AudioTrack.ID, DefaultAudio: f.AudioTrack.AudioIsDefault,
				userAgent: pr.userAgent,
			}
			results = append(results, item)
		}
	}
	for _, manifest := range []struct{ protocol, rawURL string }{{"hls", s.HLSManifestURL}, {"dash", s.DASHManifestURL}} {
		u := mediaURL(manifest.rawURL)
		if u == nil {
			continue
		}
		item := formatURL{
			format: Format{Protocol: manifest.protocol, Extension: "mp4", HasAudio: true, HasVideo: true, userAgent: pr.userAgent},
			url:    u, throttle: u.Query().Get("n"),
		}
		parts := strings.Split(u.Path, "/")
		for i := 1; item.throttle == "" && i+1 < len(parts); i++ {
			if parts[i] == "n" && parts[i+1] != "" {
				item.throttle, item.pathThrottle = parts[i+1], true
			}
		}
		results = append(results, item)
	}
	return results
}

func (c *Client) resolveFormats(ctx context.Context, jsURL string, candidates []formatURL) ([]Format, []string) {
	var signatures, throttles []string
	for _, item := range candidates {
		if item.signature != "" && !slices.Contains(signatures, item.signature) {
			signatures = append(signatures, item.signature)
		}
		if item.throttle != "" && !slices.Contains(throttles, item.throttle) {
			throttles = append(throttles, item.throttle)
		}
	}

	var warnings []string
	var answers ChallengeAnswers
	if len(signatures)+len(throttles) != 0 {
		switch {
		case c.solver == nil:
			warnings = append(warnings, "some formats need JavaScript challenge solving, which is disabled")
		default:
			if player := c.playerScript(ctx, jsURL); len(player) > 0 {
				var err error
				answers, err = c.solver.Solve(ctx, player, signatures, throttles)
				if err != nil {
					warnings = append(warnings, "player JavaScript: "+err.Error())
				}
			} else {
				warnings = append(warnings, "could not fetch player JavaScript for some formats")
			}
		}
	}
	type formatKey struct {
		protocol string
		itag     int
		track    string
	}
	seen := make(map[formatKey]bool)
	formats := make([]Format, 0, len(candidates))
	skipped := 0
	for _, item := range candidates {
		signed, throttle := answers.Signatures[item.signature], answers.Throttles[item.throttle]
		if (item.signature != "" && signed == "") || (item.throttle != "" && throttle == "") {
			skipped++
			continue
		}
		key := formatKey{item.format.Protocol, item.format.Itag, item.format.AudioTrack}
		if seen[key] {
			continue
		}
		seen[key] = true
		u := *item.url
		if signed != "" || (throttle != "" && !item.pathThrottle) {
			query := u.Query()
			if signed != "" {
				query.Set(item.signatureKey, signed)
			}
			if throttle != "" && !item.pathThrottle {
				query.Set("n", throttle)
			}
			u.RawQuery = query.Encode()
		}
		if throttle != "" && item.pathThrottle {
			u.Path = strings.Replace(u.Path, "/n/"+item.throttle+"/", "/n/"+throttle+"/", 1)
			u.RawPath = ""
		}
		item.format.URL = u.String()
		formats = append(formats, item.format)
	}
	if skipped > 0 {
		warnings = append(warnings, strconv.Itoa(skipped)+" stream URLs were skipped because their player challenges were unresolved")
	}
	return formats, warnings
}
