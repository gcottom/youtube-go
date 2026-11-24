package youtube

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestSearchContinuation(t *testing.T) {
	const query = "math & physics"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/results":
			if r.Method != http.MethodGet || r.URL.Query().Get("search_query") != query {
				t.Errorf("unexpected search request: %s %s", r.Method, r.URL)
			}
			fmt.Fprint(w, playlistTestWebConfig, `<script>var ytInitialData = {
				"contents":{"twoColumnSearchResultsRenderer":{"primaryContents":{"sectionListRenderer":{"contents":[
					{"itemSectionRenderer":{"contents":[
						{"videoRenderer":{"videoId":"aaaaaaaaaaa","title":{"simpleText":"Video one"}}},
						{"lockupViewModel":{"contentId":"PLcourseID","contentType":"LOCKUP_CONTENT_TYPE_COURSE","metadata":{"lockupMetadataViewModel":{"title":{"content":"Course"}}}}}
					]}},
					{"continuationItemRenderer":{"continuationEndpoint":{"continuationCommand":{"token":"next-search-page"}}}}
				]}},"secondaryContents":{"videoRenderer":{"videoId":"xxxxxxxxxxx"}}}}
			};</script>`)
		case "/youtubei/v1/search":
			var params playlistObject
			if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
				t.Errorf("decode search continuation: %v", err)
			}
			if r.Method != http.MethodPost || params["continuation"] != "next-search-page" || params["query"] != query {
				t.Errorf("unexpected search continuation request: %s %s %v", r.Method, r.URL, params)
			}
			fmt.Fprint(w, `{"onResponseReceivedCommands":[{"appendContinuationItemsAction":{"continuationItems":[
				{"videoRenderer":{"videoId":"bbbbbbbbbbb","title":{"simpleText":"Video two"}}},
				{"videoRenderer":{"videoId":"ccccccccccc","title":{"simpleText":"Video three"}}}
			]}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := &Client{http: server.Client(), baseURL: server.URL}
	result, err := client.Search(context.Background(), "  "+query+"  ", 3)
	if err != nil {
		t.Fatal(err)
	}
	entries := []PlaylistEntry{
		{ID: "aaaaaaaaaaa", Title: "Video one", Kind: "video", URL: "https://www.youtube.com/watch?v=aaaaaaaaaaa"},
		{ID: "PLcourseID", Title: "Course", Kind: "playlist", URL: "https://www.youtube.com/playlist?list=PLcourseID"},
		{ID: "bbbbbbbbbbb", Title: "Video two", Kind: "video", URL: "https://www.youtube.com/watch?v=bbbbbbbbbbb"},
	}
	if result.ID != query || result.Title != query || result.URL != "https://www.youtube.com/results?search_query=math+%26+physics" || !reflect.DeepEqual(result.Entries, entries) {
		t.Fatalf("unexpected search results: %+v", result)
	}
}
