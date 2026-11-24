package youtube

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

const playlistTestWebConfig = `<script>ytcfg.set({
	"INNERTUBE_API_KEY":"fixture-key",
	"INNERTUBE_CONTEXT_CLIENT_NAME":1,
	"INNERTUBE_CONTEXT":{"client":{"clientName":"WEB","clientVersion":"2.fixture","visitorData":"initial-visitor"}}
});</script>`

func TestPlaylistPaginationAndOrder(t *testing.T) {
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/playlist":
			if r.Method != http.MethodGet || r.URL.Query().Get("list") != "PLtestPlaylistID" {
				t.Errorf("unexpected initial request: %s %s", r.Method, r.URL)
			}
			fmt.Fprint(w, playlistTestWebConfig, `<script>var ytInitialData = {
				"metadata":{"playlistMetadataRenderer":{"title":"Fixture playlist"}},
				"contents":{"twoColumnBrowseResultsRenderer":{"tabs":[
					{"tabRenderer":{"selected":true,"content":{"sectionListRenderer":{"contents":[
						{"itemSectionRenderer":{"contents":[
							{"playlistVideoRenderer":{"videoId":"aaaaaaaaaaa","title":{"runs":[{"text":"Video "},{"text":"one"}]},"shortBylineText":{"simpleText":"Artist"},"lengthText":{"simpleText":"1:02:03"}}},
							{"lockupViewModel":{"contentId":"bbbbbbbbbbb","contentType":"LOCKUP_CONTENT_TYPE_VIDEO","metadata":{"lockupMetadataViewModel":{"title":{"content":"Video two"}}},"contentImage":{"thumbnailViewModel":{"overlays":[{"thumbnailBottomOverlayViewModel":{"badges":[{"thumbnailBadgeViewModel":{"text":"7:17"}}]}}]}}}},
							{"lockupViewModel":{"contentId":"PLrecommended","contentType":"LOCKUP_CONTENT_TYPE_PLAYLIST"}}
						]}},
						{"continuationItemViewModel":{"continuationCommand":{"innertubeCommand":{"clickTrackingParams":"fixture-click","continuationCommand":{"token":"next-page"}}}}}
					]}}}},
					{"tabRenderer":{"selected":false,"content":{"videoRenderer":{"videoId":"xxxxxxxxxxx"}}}}
				]}},
				"sidebar":{"videoRenderer":{"videoId":"zzzzzzzzzzz"}}
			};</script>`)
		case "/youtubei/v1/browse":
			posts.Add(1)
			if r.Method != http.MethodPost || r.URL.Query().Get("key") != "fixture-key" {
				t.Errorf("unexpected continuation request: %s %s", r.Method, r.URL)
			}
			if r.Header.Get("X-YouTube-Client-Version") != "2.fixture" || r.Header.Get("X-Goog-Visitor-Id") != "initial-visitor" {
				t.Errorf("missing web client version or visitor on continuation: %v", r.Header)
			}
			var params map[string]any
			if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
				t.Errorf("decode continuation request: %v", err)
			}
			if params["continuation"] != "next-page" || playlistAt(params, "clickTracking", "clickTrackingParams") != "fixture-click" || playlistAt(params, "context", "client", "clientName") != "WEB" {
				t.Errorf("unexpected continuation parameters: %v", params)
			}
			fmt.Fprint(w, `{"onResponseReceivedActions":[{"appendContinuationItemsAction":{"continuationItems":[
				{"playlistVideoRenderer":{"videoId":"aaaaaaaaaaa","title":{"simpleText":"Video one again"}}},
				{"playlistVideoRenderer":{"videoId":"ccccccccccc","title":{"simpleText":"Video three"}}},
				{"continuationItemRenderer":{"continuationEndpoint":{"continuationCommand":{"token":"next-page"}}}}
			]}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := &Client{http: server.Client(), baseURL: server.URL}
	input := "https://www.youtube.com/watch?v=aaaaaaaaaaa&list=PLtestPlaylistID"
	result, err := client.Playlist(context.Background(), input, 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != "PLtestPlaylistID" || result.Title != "Fixture playlist" || result.URL != "https://www.youtube.com/playlist?list=PLtestPlaylistID" {
		t.Fatalf("unexpected playlist metadata: %+v", result)
	}
	entries := []PlaylistEntry{
		{ID: "aaaaaaaaaaa", Title: "Video one", URL: "https://www.youtube.com/watch?v=aaaaaaaaaaa", Kind: "video", Channel: "Artist", DurationSeconds: 3723},
		{ID: "bbbbbbbbbbb", Title: "Video two", URL: "https://www.youtube.com/watch?v=bbbbbbbbbbb", Kind: "video", DurationSeconds: 437},
		{ID: "aaaaaaaaaaa", Title: "Video one again", URL: "https://www.youtube.com/watch?v=aaaaaaaaaaa", Kind: "video"},
		{ID: "ccccccccccc", Title: "Video three", URL: "https://www.youtube.com/watch?v=ccccccccccc", Kind: "video"},
	}
	if !reflect.DeepEqual(result.Entries, entries) || posts.Load() != 1 {
		t.Fatalf("unexpected entries or continuation count: %+v, %d", result.Entries, posts.Load())
	}

	result, err = client.Playlist(context.Background(), input, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Entries, entries[:2]) || posts.Load() != 1 {
		t.Fatalf("limit did not stop before a continuation: %+v, %d", result.Entries, posts.Load())
	}
}

func TestPlaylistChannelTabs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var items string
		switch r.URL.Path {
		case "/@example/shorts":
			items = `{"richItemRenderer":{"content":{"shortsLockupViewModel":{"onTap":{"innertubeCommand":{"reelWatchEndpoint":{"videoId":"aaaaaaaaaaa"}}},"overlayMetadata":{"primaryText":{"content":"A short"}}}}}}`
		case "/@example/playlists":
			items = `{"richItemRenderer":{"content":{"lockupViewModel":{"contentId":"PLseriesID","contentType":"LOCKUP_CONTENT_TYPE_PODCAST","metadata":{"lockupMetadataViewModel":{"title":{"content":"A series"}}}}}}}`
		default:
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `<script>window["ytInitialData"] = {"metadata":{"channelMetadataRenderer":{"externalId":"UCabcdefghijklmnopqrstuv","title":"Example"}},"contents":{"twoColumnBrowseResultsRenderer":{"tabs":[{"tabRenderer":{"selected":true,"title":"%s","content":{"richGridRenderer":{"contents":[%s]}}}}]}}};</script>`, strings.TrimPrefix(r.URL.Path, "/@example/"), items)
	}))
	defer server.Close()
	client := &Client{http: server.Client(), baseURL: server.URL}
	for _, tc := range []struct {
		input string
		entry PlaylistEntry
	}{
		{"@example/shorts", PlaylistEntry{ID: "aaaaaaaaaaa", Title: "A short", Kind: "video", URL: "https://www.youtube.com/shorts/aaaaaaaaaaa", Channel: "Example"}},
		{"https://www.youtube.com/@example/playlists", PlaylistEntry{ID: "PLseriesID", Title: "A series", Kind: "playlist", URL: "https://www.youtube.com/playlist?list=PLseriesID"}},
	} {
		t.Run(tc.input, func(t *testing.T) {
			result, err := client.Playlist(context.Background(), tc.input, 1)
			if err != nil {
				t.Fatal(err)
			}
			if result.ID != "UCabcdefghijklmnopqrstuv" || !strings.HasPrefix(result.Title, "Example - ") || !reflect.DeepEqual(result.Entries, []PlaylistEntry{tc.entry}) {
				t.Fatalf("unexpected channel result: %+v", result)
			}
		})
	}
}

func TestParsePlaylistSource(t *testing.T) {
	for _, tc := range []struct {
		input, path string
	}{
		{"UCabcdefghijklmnopqrstuv", "/channel/UCabcdefghijklmnopqrstuv/videos"},
		{"PLtestPlaylistID", "/playlist?list=PLtestPlaylistID"},
		{"https://www.youtube.com/@test", "/@test/videos"},
		{"https://www.youtube.com/@tést/shorts", "/@t%C3%A9st/shorts"},
		{"https://www.youtube.com/user/test/featured", "/user/test/featured"},
		{"https://www.youtube.com/@test/playlists?feature=shared&sort=dd&view=1", "/@test/playlists?sort=dd&view=1"},
		{"https://youtu.be/aaaaaaaaaaa?list=PLtestPlaylistID", "/playlist?list=PLtestPlaylistID"},
		{"https://www.youtube.com/watch?v=aaaaaaaaaaa", ""},
		{"https://example.com/watch?list=PLtestPlaylistID", ""},
		{"https://www.youtube.com/@test/community", ""},
	} {
		t.Run(tc.input, func(t *testing.T) {
			source, err := parsePlaylistSource(tc.input)
			if tc.path == "" {
				if err == nil {
					t.Fatalf("unexpected accepted source: %+v", source)
				}
				return
			}
			if err != nil || source.path != tc.path {
				t.Fatalf("got path %q and error %v, want %q", source.path, err, tc.path)
			}
		})
	}
}
