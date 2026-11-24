package youtube

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// Search lists public video and playlist results for query in YouTube's order.
// A zero limit reads all continuation pages. A continuation failure returns
// results collected so far and an error.
func (c *Client) Search(ctx context.Context, query string, limit int) (*Playlist, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("YouTube search query is empty")
	}
	if limit < 0 {
		return nil, fmt.Errorf("search limit must be nonnegative")
	}
	path := "/results?search_query=" + url.QueryEscape(query)
	page, data, err := c.collectionPage(ctx, path)
	if err != nil {
		return nil, err
	}
	contents := playlistAt(data, "contents", "twoColumnSearchResultsRenderer", "primaryContents")
	if contents == nil {
		contents = playlistAt(data, "contents", "singleColumnSearchResultsRenderer", "primaryContents")
	}
	if contents == nil {
		return nil, fmt.Errorf("YouTube search response has no primary results")
	}
	result := &Playlist{ID: query, Title: query, URL: youtubeBase + path, Entries: make([]PlaylistEntry, 0)}
	state := playlistItems{result: result, limit: limit, includeCollections: true}
	state.read(contents)
	err = c.collectionContinuations(ctx, page, data, &state, "search", query)
	return result, err
}
