package youtube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const testVideoID = "jNQXAC9IVRw"

type fixtureSolver struct{ t *testing.T }

func (s fixtureSolver) Solve(_ context.Context, player []byte, signatures, throttles []string) (ChallengeAnswers, error) {
	if !strings.Contains(string(player), "20775") || !slices.Equal(signatures, []string{"sig"}) ||
		!slices.Equal(throttles, []string{"manifest", "slow"}) {
		s.t.Errorf("unexpected solver input: %q %v %v", player, signatures, throttles)
	}
	return ChallengeAnswers{
		Signatures: map[string]string{"sig": "signed"},
		Throttles:  map[string]string{"slow": "fast", "manifest": "decoded"},
	}, nil
}

func TestVideoExtraction(t *testing.T) {
	ctx := context.Background()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/watch":
			fmt.Fprintf(w, `<script>ytcfg.set({"VISITOR_DATA":"test-visitor","PLAYER_JS_URL":"\/s\/player\/id\/base.js"});
				window["ytInitialPlayerResponse"] = {"playabilityStatus":{"status":"OK"},
				"videoDetails":{"videoId":"%s","title":"Fixture {}","author":"Author","lengthSeconds":"120",
					"shortDescription":"0:00 Start\n0:20 Middle\n1:30 End"},
				"microformat":{"playerMicroformatRenderer":{"uploadDate":"2026-05-02"}},
				"captions":{"playerCaptionsTracklistRenderer":{"captionTracks":[{"baseUrl":"%s/caption/web",
					"languageCode":"en","name":{"simpleText":"English"}}]}}},
				ignored = "</script>";`, testVideoID, server.URL)
		case "/s/player/id/base.js":
			fmt.Fprint(w, "var p={signatureTimestamp:20775}")
		case "/youtubei/v1/player":
			var payload struct {
				VideoID         string
				PlaybackContext struct {
					ContentPlaybackContext struct{ SignatureTimestamp int }
				}
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.VideoID != testVideoID ||
				r.Header.Get("X-Goog-Visitor-Id") != "test-visitor" {
				t.Errorf("unexpected player request: %+v (%v)", payload, err)
			}
			switch r.Header.Get("X-YouTube-Client-Name") {
			case visionProfile.number:
				fmt.Fprintf(w, `{"videoDetails":{"videoId":%q},"streamingData":{"adaptiveFormats":[
				  {"itag":140,"url":"%s/media/audio","mimeType":"audio/mp4; codecs=\"mp4a.40.2\""},
				  {"itag":999,"url":"%s/media/drm","mimeType":"video/mp4","drmFamilies":["DRM"]}],
				  "hlsManifestUrl":"%s/hls/n/manifest/index.m3u8"},
				  "captions":{"playerCaptionsTracklistRenderer":{"captionTracks":[{"baseUrl":"%s/caption/native?xosf=1",
				  "languageCode":"en","name":{"simpleText":"English"}}]}}}`, testVideoID, server.URL, server.URL, server.URL, server.URL)
			case tvProfile.number:
				if payload.PlaybackContext.ContentPlaybackContext.SignatureTimestamp != 20775 {
					t.Errorf("missing signature timestamp")
				}
				cipher := url.Values{"url": {server.URL + "/media/progressive?n=slow"}, "s": {"sig"}, "sp": {"sig"}}
				fmt.Fprintf(w, `{"videoDetails":{"videoId":%q},"streamingData":{"formats":[
				  {"itag":18,"signatureCipher":%q,"mimeType":"video/mp4; codecs=\"avc1.42E01E, mp4a.40.2\""}]}}`, testVideoID, cipher.Encode())
			default:
				t.Errorf("unexpected playback client %s", r.Header.Get("X-YouTube-Client-Name"))
			}
		case "/caption/native":
			if r.URL.Query().Get("fmt") != "vtt" || r.URL.Query().Has("xosf") || r.UserAgent() != visionProfile.userAgent {
				t.Errorf("unexpected caption query: %s", r.URL)
			}
			fmt.Fprint(w, "WEBVTT\n\n00:00.000 --> 00:01.000\nfixture\n")
		default:
			t.Errorf("unexpected route %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := NewClient(Options{HTTPClient: server.Client(), Solver: fixtureSolver{t}})
	client.baseURL = server.URL
	video, err := client.Video(ctx, testVideoID)
	if err != nil {
		t.Fatal(err)
	}
	if video.Title != "Fixture {}" || video.Channel != "Author" || video.UploadDate != "2026-05-02" ||
		len(video.Chapters) != 3 || video.Chapters[2].EndSeconds != 120 || len(video.Warnings) != 0 {
		t.Fatalf("unexpected metadata: %+v", video)
	}
	if len(video.Formats) != 3 || video.Formats[0].Extension != "m4a" ||
		!strings.Contains(video.Formats[1].URL, "/n/decoded/") || !video.Formats[2].HasAudio ||
		!strings.Contains(video.Formats[2].URL, "n=fast&sig=signed") {
		t.Fatalf("unexpected formats: %+v", video.Formats)
	}
	dest := filepath.Join(t.TempDir(), "test.vtt")
	if _, err := client.WriteSubtitle(ctx, video, "en", dest); err != nil {
		t.Fatal(err)
	}
	if _, err := client.WriteSubtitle(ctx, video, "en", dest); !errors.Is(err, os.ErrExist) {
		t.Fatalf("expected existing file error, got %v", err)
	}
}

func TestVideoID(t *testing.T) {
	for _, input := range []string{
		testVideoID, "www.youtube.com/watch?v=" + testVideoID + "&t=1",
		"https://youtu.be/" + testVideoID, "https://www.youtube.com/shorts/" + testVideoID,
		"https://www.youtube-nocookie.com/embed/" + testVideoID,
	} {
		if id, err := VideoID(input); err != nil || id != testVideoID {
			t.Errorf("VideoID(%q) = %q, %v", input, id, err)
		}
	}
	for _, input := range []string{
		"https://youtube.com.example.com/watch?v=" + testVideoID, "https://youtube.com@evil.example/watch?v=" + testVideoID,
		"https://youtube.com/playlist?list=PL12345", "https://www.youtube.com/watch?v=bad-id",
	} {
		if _, err := VideoID(input); err == nil {
			t.Errorf("VideoID(%q) accepted invalid URL", input)
		}
	}
}
