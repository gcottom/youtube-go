package youtube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

type Video struct {
	ID              string      `json:"id"`
	URL             string      `json:"url"`
	Title           string      `json:"title"`
	Channel         string      `json:"channel,omitempty"`
	ChannelID       string      `json:"channel_id,omitempty"`
	Description     string      `json:"description,omitempty"`
	DurationSeconds int         `json:"duration_seconds,omitempty"`
	ViewCount       int64       `json:"view_count,omitempty"`
	UploadDate      string      `json:"upload_date,omitempty"`
	PublishDate     string      `json:"publish_date,omitempty"`
	Live            bool        `json:"live"`
	Thumbnails      []Thumbnail `json:"thumbnails,omitempty"`
	Captions        []Caption   `json:"captions,omitempty"`
	Chapters        []Chapter   `json:"chapters,omitempty"`
	Formats         []Format    `json:"formats"`
	Warnings        []string    `json:"warnings,omitempty"`
}

type Thumbnail struct {
	URL    string `json:"url"`
	Width  int    `json:"width,omitempty"`
	Height int    `json:"height,omitempty"`
}

type Caption struct {
	Language  string `json:"language"`
	Name      string `json:"name,omitempty"`
	Automatic bool   `json:"automatic"`
	URL       string `json:"url"`

	sources []captionSource
}

type captionSource struct{ url, userAgent string }

type Chapter struct {
	Title        string `json:"title"`
	StartSeconds int    `json:"start_seconds"`
	EndSeconds   int    `json:"end_seconds,omitempty"`
}

type Format struct {
	Itag          int    `json:"itag,omitempty"`
	URL           string `json:"url"`
	MimeType      string `json:"mime_type,omitempty"`
	Extension     string `json:"extension,omitempty"`
	Protocol      string `json:"protocol"`
	Width         int    `json:"width,omitempty"`
	Height        int    `json:"height,omitempty"`
	FPS           int    `json:"fps,omitempty"`
	Bitrate       int    `json:"bitrate,omitempty"`
	ContentLength int64  `json:"content_length,omitempty"`
	HasVideo      bool   `json:"has_video"`
	HasAudio      bool   `json:"has_audio"`
	AudioTrack    string `json:"audio_track,omitempty"`
	DefaultAudio  bool   `json:"default_audio,omitempty"`

	userAgent string
}

type playerResponse struct {
	PlayabilityStatus struct {
		Status   string
		Reason   string
		Messages []string
	}
	ResponseContext struct{ VisitorData string }
	VideoDetails    struct {
		VideoID, Title, Author, ChannelID, ShortDescription, LengthSeconds, ViewCount string
		IsLive                                                                        bool
		Thumbnail                                                                     struct{ Thumbnails []Thumbnail }
	}
	Microformat struct {
		PlayerMicroformatRenderer struct {
			UploadDate, PublishDate string
			LiveBroadcastDetails    struct{ IsLiveNow bool }
		}
	}
	Captions struct {
		PlayerCaptionsTracklistRenderer struct {
			CaptionTracks []struct {
				BaseURL, LanguageCode, Kind string
				Name                        struct {
					SimpleText string
					Runs       []struct{ Text string }
				}
			}
		}
	}
	StreamingData struct {
		Formats, AdaptiveFormats                               []streamFormat
		HLSManifestURL, DASHManifestURL, ServerAbrStreamingURL string
	}
	Assets    struct{ JS string }
	userAgent string
}

type streamFormat struct {
	Itag, Width, Height, FPS, Bitrate                     int
	URL, MimeType, ContentLength, SignatureCipher, Cipher string
	DRMFamilies                                           []string
	AudioTrack                                            struct {
		ID             string
		AudioIsDefault bool
	}
}

type playerProfile struct {
	number, name, version, userAgent, make, model, os, osVersion string
}

// These profiles match the public playback clients used by the reference
// extractor. Keep profile versions together with the matching user agents.
var (
	visionProfile = playerProfile{
		"101", "VISIONOS", "1.02",
		"com.google.ios.youtube/21.26.4 (iPhone16,2; U; CPU iOS 18_3_2 like Mac OS X;)",
		"Apple", "RealityDevice17,1", "visionOS", "26.5.23O471",
	}
	tvProfile = playerProfile{
		"7", "TVHTML5", "5.20260707",
		"Mozilla/5.0 (SMART-TV; Linux; Tizen 2.4.0) AppleWebKit/538.1 (KHTML, like Gecko) Version/2.4.0 TV Safari/538.1",
		"Samsung", "UKS9800", "Tizen", "2.4.0",
	}
	producerProfile = playerProfile{
		number: "92", name: "IOS_PRODUCER", version: "0.147.945",
		userAgent: "com.google.ios.youtubeproducer/0.147.945 (iPhone16,2; U; CPU iOS 18_3_2 like Mac OS X;)",
	}
)

type PlayabilityError struct {
	VideoID string
	Status  string
	Reason  string
}

func (e *PlayabilityError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("YouTube video %s: %s (%s)", e.VideoID, e.Reason, e.Status)
	}
	return fmt.Sprintf("YouTube video %s: %s", e.VideoID, e.Status)
}

// Video extracts metadata and playable stream URLs. Metadata can be available
// with no playable formats. Check Warnings when formats are missing.
func (c *Client) Video(ctx context.Context, input string) (*Video, error) {
	id, err := VideoID(input)
	if err != nil {
		return nil, err
	}
	v := &Video{ID: id, URL: youtubeBase + "/watch?v=" + id, Formats: []Format{}}
	page, pageErr := c.request(ctx, http.MethodGet, c.baseURL+"/watch?v="+id+"&hl=en", nil)
	web := decodePagePlayer(page, c.userAgent)
	jsURL := playerScriptURL(c.baseURL, page, web)

	var config struct {
		VisitorData string `json:"VISITOR_DATA"`
	}
	if data, err := embeddedJSON(page, "ytcfg.set"); err == nil {
		_ = json.Unmarshal(data, &config)
	}
	visitor := config.VisitorData
	if visitor == "" && web != nil {
		visitor = web.ResponseContext.VisitorData
	}
	if web != nil && web.permanentlyUnavailable() {
		return nil, web.playabilityError(id)
	}

	// The TV client needs the player signature timestamp to return the full
	// set of formats. The visionOS client can return direct URLs independently.
	type tvResult struct {
		response *playerResponse
		err      error
	}
	var tv <-chan tvResult
	if c.solver != nil {
		results := make(chan tvResult, 1)
		tv = results
		go func() {
			var sts int
			if js := c.playerScript(ctx, jsURL); len(js) > 0 {
				if match := timestampPattern.FindSubmatch(js); len(match) > 1 {
					sts, _ = strconv.Atoi(string(match[1]))
				}
			}
			resp, err := c.apiPlayer(ctx, id, tvProfile, visitor, sts)
			results <- tvResult{resp, err}
		}()
	}

	var responses []*playerResponse
	vision, visionErr := c.apiPlayer(ctx, id, visionProfile, visitor, 0)
	responses = append(responses, web, vision)
	var tvErr error
	if tv != nil {
		select {
		case result := <-tv:
			responses, tvErr = append(responses, result.response), result.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if !slices.ContainsFunc(responses, func(pr *playerResponse) bool { return pr.hasStream(id) }) {
		fallback, fallbackErr := c.apiPlayer(ctx, id, producerProfile, visitor, 0)
		responses = append(responses, fallback)
		if tvErr == nil {
			tvErr = fallbackErr
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	var candidates []formatURL
	var abrOnly bool
	for _, pr := range responses {
		if pr == nil || pr.VideoDetails.VideoID != id {
			continue
		}
		addMetadata(v, pr)
		if jsURL == "" {
			jsURL = playerScriptURL(c.baseURL, nil, pr)
		}
		// Use API player URLs ahead of web page URLs, which can have an
		// extra media token requirement even when the itag is identical.
		if pr != web {
			candidates = append(candidates, pr.formatURLs()...)
		}
		abrOnly = abrOnly || pr.StreamingData.ServerAbrStreamingURL != ""
	}
	if web != nil && web.VideoDetails.VideoID == id {
		candidates = append(candidates, web.formatURLs()...)
	}
	if len(candidates) > 0 {
		v.Formats, v.Warnings = c.resolveFormats(ctx, jsURL, candidates)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if len(v.Formats) == 0 {
		for _, pr := range responses {
			if pr != nil && pr.PlayabilityStatus.Status != "" && pr.PlayabilityStatus.Status != "OK" {
				if v.Title == "" || pr == web {
					return nil, pr.playabilityError(id)
				}
			}
		}
		if abrOnly {
			v.Warnings = append(v.Warnings, "YouTube returned SABR streams, which this client cannot download")
		}
		if err := errors.Join(pageErr, visionErr, tvErr); err != nil {
			v.Warnings = append(v.Warnings, "player requests: "+strings.ReplaceAll(err.Error(), "\n", "; "))
		}
		v.Warnings = append(v.Warnings, "no playable media URLs were returned")
	}
	if v.Title == "" {
		if len(v.Warnings) > 0 {
			return nil, fmt.Errorf("could not extract YouTube video %s: %s", id, strings.Join(v.Warnings, "; "))
		}
		return nil, fmt.Errorf("YouTube did not return metadata for video %s", id)
	}
	v.Chapters = extractChapters(page, v.Description, v.DurationSeconds)
	return v, nil
}

func decodePagePlayer(page []byte, userAgent string) *playerResponse {
	data, err := embeddedJSON(page, "ytInitialPlayerResponse")
	if err != nil {
		return nil
	}
	var response playerResponse
	if json.Unmarshal(data, &response) != nil {
		return nil
	}
	response.userAgent = userAgent
	return &response
}

func (pr *playerResponse) hasStream(id string) bool {
	return pr != nil && pr.VideoDetails.VideoID == id && len(pr.formatURLs()) > 0
}

func (pr *playerResponse) permanentlyUnavailable() bool {
	status := pr.PlayabilityStatus
	if status.Status != "ERROR" && status.Status != "UNPLAYABLE" && status.Status != "LOGIN_REQUIRED" && status.Status != "LIVE_STREAM_OFFLINE" {
		return false
	}
	// The watch page also uses LOGIN_REQUIRED for temporary service challenges
	// and UNPLAYABLE for client issues. Let the playback clients try those.
	reason := strings.ToLower(status.Reason + " " + strings.Join(status.Messages, " "))
	return strings.Contains(reason, "private video") || strings.Contains(reason, "video has been removed") ||
		strings.Contains(reason, "account associated with this video") || status.Status == "LIVE_STREAM_OFFLINE"
}

func (pr *playerResponse) playabilityError(id string) error {
	reason := pr.PlayabilityStatus.Reason
	if reason == "" {
		reason = strings.Join(pr.PlayabilityStatus.Messages, " ")
	}
	return &PlayabilityError{VideoID: id, Status: pr.PlayabilityStatus.Status, Reason: reason}
}

func (c *Client) apiPlayer(ctx context.Context, id string, p playerProfile, visitor string, sts int) (*playerResponse, error) {
	client := map[string]string{
		"clientName": p.name, "clientVersion": p.version, "userAgent": p.userAgent, "hl": "en",
	}
	if p.make != "" {
		client["deviceMake"], client["deviceModel"] = p.make, p.model
		client["osName"], client["osVersion"] = p.os, p.osVersion
	}
	if visitor != "" {
		client["visitorData"] = visitor
	}
	playback := map[string]any{"html5Preference": "HTML5_PREF_WANTS"}
	if sts > 0 {
		playback["signatureTimestamp"] = sts
	}
	data, err := json.Marshal(map[string]any{
		"videoId":         id,
		"context":         map[string]any{"client": client},
		"playbackContext": map[string]any{"contentPlaybackContext": playback},
		"contentCheckOk":  true,
		"racyCheckOk":     true,
	})
	if err != nil {
		return nil, err
	}
	headers := http.Header{}
	headers.Set("Origin", c.baseURL)
	headers.Set("User-Agent", p.userAgent)
	headers.Set("X-YouTube-Client-Name", p.number)
	headers.Set("X-YouTube-Client-Version", p.version)
	if visitor != "" {
		headers.Set("X-Goog-Visitor-Id", visitor)
	}
	body, err := c.request(ctx, http.MethodPost, c.baseURL+"/youtubei/v1/player?prettyPrint=false", bytes.NewReader(data), headers)
	if err != nil {
		return nil, fmt.Errorf("%s player: %w", p.name, err)
	}
	var response playerResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("decode %s player response: %w", p.name, err)
	}
	if got := response.VideoDetails.VideoID; got != "" && got != id {
		return nil, fmt.Errorf("%s player returned a different video", p.name)
	}
	response.userAgent = p.userAgent
	return &response, nil
}

var (
	scriptPattern    = regexp.MustCompile(`"(?:PLAYER_JS_URL|jsUrl|js)"\s*:\s*("(?:\\.|[^"\\])*")`)
	timestampPattern = regexp.MustCompile(`(?:signatureTimestamp|sts)\s*:\s*([0-9]{5})`)
)

func playerScriptURL(baseURL string, page []byte, pr *playerResponse) string {
	var base, err = url.Parse(baseURL)
	if err != nil {
		return ""
	}
	validate := func(raw string) string {
		u, err := base.Parse(raw)
		if err != nil || u.Host != base.Host || u.Scheme != base.Scheme || u.User != nil ||
			!strings.HasPrefix(u.Path, "/s/player/") || !strings.HasSuffix(u.Path, "/base.js") {
			return ""
		}
		return u.String()
	}
	if pr != nil {
		if playerURL := validate(pr.Assets.JS); playerURL != "" {
			return playerURL
		}
	}
	for _, match := range scriptPattern.FindAllSubmatch(page, -1) {
		var raw string
		if json.Unmarshal(match[1], &raw) == nil {
			if playerURL := validate(raw); playerURL != "" {
				return playerURL
			}
		}
	}
	return ""
}

func (c *Client) playerScript(ctx context.Context, jsURL string) []byte {
	if jsURL == "" {
		return nil
	}
	c.playerMu.Lock()
	cached := c.playerCache[jsURL]
	c.playerMu.Unlock()
	if cached != nil {
		return cached
	}
	data, err := c.request(ctx, http.MethodGet, jsURL, nil)
	if err != nil || len(data) == 0 {
		return nil
	}
	c.playerMu.Lock()
	defer c.playerMu.Unlock()
	if len(c.playerCache) >= 4 || c.playerCache == nil {
		c.playerCache = make(map[string][]byte)
	}
	c.playerCache[jsURL] = data
	return data
}

func addMetadata(v *Video, pr *playerResponse) {
	d, m := pr.VideoDetails, pr.Microformat.PlayerMicroformatRenderer
	if v.Title == "" {
		v.Title = d.Title
	}
	if v.Description == "" {
		v.Description = d.ShortDescription
	}
	if v.Channel == "" {
		v.Channel, v.ChannelID = d.Author, d.ChannelID
	}
	if v.DurationSeconds == 0 {
		v.DurationSeconds, _ = strconv.Atoi(d.LengthSeconds)
	}
	if v.ViewCount == 0 {
		v.ViewCount, _ = strconv.ParseInt(d.ViewCount, 10, 64)
	}
	if v.UploadDate == "" {
		v.UploadDate = m.UploadDate
	}
	if v.PublishDate == "" {
		v.PublishDate = m.PublishDate
	}
	v.Live = v.Live || d.IsLive || m.LiveBroadcastDetails.IsLiveNow
	for _, thumbnail := range d.Thumbnail.Thumbnails {
		if mediaURL(thumbnail.URL) != nil && !slices.ContainsFunc(v.Thumbnails, func(t Thumbnail) bool { return t.URL == thumbnail.URL }) {
			v.Thumbnails = append(v.Thumbnails, thumbnail)
		}
	}
	for _, track := range pr.Captions.PlayerCaptionsTracklistRenderer.CaptionTracks {
		if track.LanguageCode == "" || mediaURL(track.BaseURL) == nil {
			continue
		}
		name := track.Name.SimpleText
		if name == "" {
			for _, run := range track.Name.Runs {
				name += run.Text
			}
		}
		source := captionSource{track.BaseURL, pr.userAgent}
		caption := Caption{Language: track.LanguageCode, Automatic: track.Kind == "asr", Name: name, URL: track.BaseURL, sources: []captionSource{source}}
		index := slices.IndexFunc(v.Captions, func(t Caption) bool {
			return t.Language == caption.Language && t.Automatic == caption.Automatic && t.Name == caption.Name
		})
		if index < 0 {
			v.Captions = append(v.Captions, caption)
			continue
		}
		current := &v.Captions[index]
		if slices.ContainsFunc(current.sources, func(s captionSource) bool { return source == s }) {
			continue
		}
		// The browser can advertise a timed-text URL that needs an extra
		// token. Prefer the playback clients that also return media URLs.
		if current.sources[0].userAgent == defaultUserAgent && pr.userAgent != defaultUserAgent {
			current.sources = slices.Insert(current.sources, 0, source)
			current.URL = source.url
		} else {
			current.sources = append(current.sources, source)
		}
	}
}
