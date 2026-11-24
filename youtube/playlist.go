package youtube

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Playlist is an ordered collection from a playlist page or a channel tab.
type Playlist struct {
	ID      string          `json:"id"`
	Title   string          `json:"title"`
	URL     string          `json:"url"`
	Entries []PlaylistEntry `json:"entries"`
}

// PlaylistEntry describes a video or a nested collection listed by YouTube.
type PlaylistEntry struct {
	ID              string `json:"id"`
	Title           string `json:"title"`
	URL             string `json:"url"`
	Kind            string `json:"kind"`
	Channel         string `json:"channel,omitempty"`
	DurationSeconds int    `json:"duration_seconds,omitempty"`
}

var (
	playlistIDPattern     = regexp.MustCompile(`^[A-Za-z0-9_-]{2,128}$`)
	barePlaylistIDPattern = regexp.MustCompile(`^(?:(?:PL|UU|LL|FL|OL|UL|RD|TL)[A-Za-z0-9_-]{12,126}|WL|LL)$`)
	playlistChannelID     = regexp.MustCompile(`^UC[A-Za-z0-9_-]{22}$`)
)

type playlistSource struct {
	id   string
	path string
	tab  string
}

// IsPlaylist reports whether input identifies a playlist or supported channel tab.
func IsPlaylist(input string) bool {
	_, err := parsePlaylistSource(input)
	return err == nil
}

func parsePlaylistSource(input string) (playlistSource, error) {
	input = strings.TrimSpace(input)
	if playlistChannelID.MatchString(input) {
		return playlistSource{input, "/channel/" + input + "/videos", "videos"}, nil
	}
	if barePlaylistIDPattern.MatchString(input) {
		return playlistSource{id: input, path: playlistPath(input)}, nil
	}
	if strings.HasPrefix(input, "@") {
		input = youtubeBase + "/" + input
	}
	parsed, err := parseYouTubeURL(input)
	if err != nil {
		return playlistSource{}, err
	}
	if id := parsed.Query().Get("list"); playlistIDPattern.MatchString(id) {
		return playlistSource{id: id, path: playlistPath(id)}, nil
	}
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	n := 2
	if strings.HasPrefix(segments[0], "@") && len(segments[0]) > 1 {
		n = 1
	} else if len(segments) < 2 || segments[1] == "" || segments[1] == "." || segments[1] == ".." || !supportedChannelPrefix(segments[0]) {
		return playlistSource{}, fmt.Errorf("expected a YouTube playlist or channel URL")
	}
	tab := "videos"
	if len(segments) == n+1 {
		tab = segments[n]
	}
	switch tab {
	case "featured", "videos", "shorts", "streams", "playlists", "podcasts":
	default:
		return playlistSource{}, fmt.Errorf("unsupported YouTube channel tab %q", tab)
	}
	if len(segments) > n+1 {
		return playlistSource{}, fmt.Errorf("expected a YouTube channel URL")
	}
	path := &url.URL{Path: "/" + strings.Join(segments[:n], "/") + "/" + tab}
	options := url.Values{}
	for _, key := range []string{"view", "sort", "flow", "shelf_id"} {
		if value := parsed.Query().Get(key); value != "" {
			options.Set(key, value)
		}
	}
	path.RawQuery = options.Encode()
	return playlistSource{segments[n-1], path.String(), tab}, nil
}

func supportedChannelPrefix(prefix string) bool {
	return prefix == "channel" || prefix == "c" || prefix == "user"
}

func playlistPath(id string) string {
	return "/playlist?list=" + url.QueryEscape(id)
}

// Playlist lists entries in server order. A zero limit reads all continuation
// pages. A continuation failure returns entries collected so far and an error.
func (c *Client) Playlist(ctx context.Context, input string, limit int) (*Playlist, error) {
	if limit < 0 {
		return nil, fmt.Errorf("playlist limit must be nonnegative")
	}
	source, err := parsePlaylistSource(input)
	if err != nil {
		return nil, err
	}
	page, data, err := c.collectionPage(ctx, source.path)
	if err != nil {
		return nil, err
	}
	tab := playlistTab(data)
	if tab == nil {
		return nil, fmt.Errorf("YouTube collection has no selected tab")
	}
	result := &Playlist{
		ID: source.id, URL: youtubeBase + source.path, Entries: make([]PlaylistEntry, 0),
	}
	state := playlistItems{result: result, limit: limit, includeCollections: source.tab != ""}
	if source.tab != "" {
		metadata := playlistAt(data, "metadata", "channelMetadataRenderer")
		if id := playlistString(playlistAt(metadata, "externalId")); id != "" {
			result.ID = id
		}
		result.Title = playlistText(playlistAt(metadata, "title"))
		switch source.tab {
		case "videos", "shorts", "streams":
			state.channel = result.Title
		}
		if title := playlistText(playlistAt(tab, "title")); title != "" && result.Title != "" {
			result.Title += " - " + title
		}
	} else {
		result.Title = playlistText(playlistAt(data, "metadata", "playlistMetadataRenderer", "title"))
		if result.Title == "" {
			result.Title = playlistText(playlistAt(data, "header", "playlistHeaderRenderer", "title"))
		}
	}
	if result.Title == "" {
		result.Title = result.ID
	}
	state.read(playlistAt(tab, "content"))
	err = c.collectionContinuations(ctx, page, data, &state, "browse", "")
	return result, err
}

func (c *Client) collectionPage(ctx context.Context, path string) ([]byte, playlistObject, error) {
	page, err := c.request(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("load YouTube collection: %w", err)
	}
	initial, err := embeddedJSON(page, "ytInitialData")
	if err != nil {
		return nil, nil, fmt.Errorf("read YouTube collection: %w", err)
	}
	var data playlistObject
	if err := json.Unmarshal(initial, &data); err != nil {
		return nil, nil, fmt.Errorf("decode YouTube collection: %w", err)
	}
	if err := playlistAlert(data); err != nil {
		return nil, nil, err
	}
	return page, data, nil
}

func (c *Client) collectionContinuations(ctx context.Context, page []byte, data playlistObject, state *playlistItems, endpoint, searchQuery string) error {
	if state.done() || state.next.token == "" {
		return nil
	}
	config, err := playlistConfig(page)
	if err != nil {
		return err
	}
	seen := make(map[string]bool)
	for state.next.token != "" && !state.done() {
		next := state.next
		if seen[next.token] {
			break
		}
		seen[next.token] = true
		if visitor := playlistString(playlistAt(data, "responseContext", "visitorData")); visitor != "" {
			config.visitor = visitor
			config.clientContext["visitorData"] = visitor
		}
		data, err = c.playlistContinuation(ctx, config, next, endpoint, searchQuery)
		if err != nil {
			return err
		}
		state.next = playlistContinuationToken{}
		for _, field := range []string{"onResponseReceivedActions", "onResponseReceivedEndpoints", "onResponseReceivedCommands", "continuationContents"} {
			state.read(data[field])
		}
	}
	return nil
}

type playlistWebConfig struct {
	context       playlistObject
	clientContext playlistObject
	clientNumber  string
	clientVersion string
	visitor       string
	apiKey        string
}

func playlistConfig(page []byte) (playlistWebConfig, error) {
	raw, err := embeddedJSON(page, "ytcfg.set")
	if err != nil {
		return playlistWebConfig{}, fmt.Errorf("read YouTube collection web client configuration: %w", err)
	}
	var data playlistObject
	if err := json.Unmarshal(raw, &data); err != nil {
		return playlistWebConfig{}, fmt.Errorf("decode YouTube collection web client configuration: %w", err)
	}
	requestContext, _ := data["INNERTUBE_CONTEXT"].(map[string]any)
	clientContext, _ := requestContext["client"].(map[string]any)
	if clientContext == nil {
		return playlistWebConfig{}, fmt.Errorf("YouTube collection continuation is missing web client configuration")
	}
	name := "1"
	if value, ok := data["INNERTUBE_CONTEXT_CLIENT_NAME"].(float64); ok {
		name = strconv.Itoa(int(value))
	}
	visitor := playlistString(clientContext["visitorData"])
	if visitor == "" {
		visitor = playlistString(data["VISITOR_DATA"])
		if visitor != "" {
			clientContext["visitorData"] = visitor
		}
	}
	return playlistWebConfig{
		context: requestContext, clientContext: clientContext, clientNumber: name,
		clientVersion: playlistString(clientContext["clientVersion"]), visitor: visitor,
		apiKey: playlistString(data["INNERTUBE_API_KEY"]),
	}, nil
}

type playlistContinuationToken struct {
	token         string
	clickTracking string
}

func (c *Client) playlistContinuation(ctx context.Context, config playlistWebConfig, next playlistContinuationToken, endpoint, searchQuery string) (playlistObject, error) {
	query := url.Values{"prettyPrint": {"false"}}
	if config.apiKey != "" {
		query.Set("key", config.apiKey)
	}
	params := playlistObject{"context": config.context, "continuation": next.token}
	if next.clickTracking != "" {
		params["clickTracking"] = playlistObject{"clickTrackingParams": next.clickTracking}
	}
	if searchQuery != "" {
		params["query"] = searchQuery
	}
	payload, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("encode YouTube collection continuation: %w", err)
	}
	headers := http.Header{}
	headers.Set("Origin", c.baseURL)
	headers.Set("X-YouTube-Client-Name", config.clientNumber)
	if config.clientVersion != "" {
		headers.Set("X-YouTube-Client-Version", config.clientVersion)
	}
	if config.visitor != "" {
		headers.Set("X-Goog-Visitor-Id", config.visitor)
	}
	response, err := c.request(ctx, http.MethodPost, c.baseURL+"/youtubei/v1/"+endpoint+"?"+query.Encode(), bytes.NewReader(payload), headers)
	if err != nil {
		return nil, fmt.Errorf("load YouTube collection continuation: %w", err)
	}
	var data playlistObject
	if err := json.Unmarshal(response, &data); err != nil {
		return nil, fmt.Errorf("decode YouTube collection continuation: %w", err)
	}
	if err := playlistAlert(data); err != nil {
		return nil, err
	}
	if message := playlistString(playlistAt(data, "error", "message")); message != "" {
		return nil, fmt.Errorf("YouTube collection continuation: %s", message)
	}
	return data, nil
}

type playlistItems struct {
	result             *Playlist
	limit              int
	includeCollections bool
	channel            string
	next               playlistContinuationToken
}

func (p *playlistItems) done() bool {
	return p.limit > 0 && len(p.result.Entries) >= p.limit
}

func (p *playlistItems) read(item any) {
	if p.done() {
		return
	}
	switch value := item.(type) {
	case []any:
		for _, child := range value {
			if p.done() {
				break
			}
			p.read(child)
		}
	case map[string]any:
		for _, key := range []string{"playlistVideoRenderer", "playlistPanelVideoRenderer", "videoRenderer", "gridVideoRenderer", "reelItemRenderer", "shortsLockupViewModel", "lockupViewModel", "playlistRenderer", "gridPlaylistRenderer"} {
			if renderer, present := value[key]; present {
				entry := playlistEntry(key, renderer)
				if entry.ID != "" && (entry.Kind == "video" || p.includeCollections) {
					if entry.Channel == "" {
						entry.Channel = p.channel
					}
					p.result.Entries = append(p.result.Entries, entry)
				}
				return
			}
		}
		if p.next.token == "" {
			p.next = playlistNext(value)
		}
		// Follow content keys only. Renderer menus and endpoints can contain
		// unrelated videos, playlists, and continuation commands.
		for _, key := range []string{
			"sectionListRenderer", "itemSectionRenderer", "richGridRenderer", "richItemRenderer",
			"playlistVideoListRenderer", "gridRenderer", "shelfRenderer", "horizontalListRenderer", "expandedShelfContentsRenderer", "reelShelfRenderer",
			"playlistVideoListContinuation", "gridContinuation", "sectionListContinuation", "itemSectionContinuation",
			"appendContinuationItemsAction", "reloadContinuationItemsCommand", "continuationItems", "content", "contents", "items",
		} {
			if child, ok := value[key]; ok {
				p.read(child)
			}
		}
	}
}

func playlistNext(item playlistObject) playlistContinuationToken {
	for _, endpoint := range []any{
		playlistAt(item, "continuationItemRenderer", "continuationEndpoint"),
		playlistAt(item, "continuationItemRenderer", "button", "buttonRenderer", "command"),
		playlistAt(item, "continuationItemViewModel", "continuationCommand", "innertubeCommand"),
	} {
		commands, _ := playlistAt(endpoint, "commandExecutorCommand", "commands").([]any)
		for _, command := range append(commands, endpoint) {
			if token := playlistString(playlistAt(command, "continuationCommand", "token")); token != "" {
				return playlistContinuationToken{token, playlistString(playlistAt(command, "clickTrackingParams"))}
			}
		}
	}
	for _, continuation := range playlistArray(item["continuations"]) {
		if data := playlistAt(continuation, "nextContinuationData"); data != nil {
			if token := playlistString(playlistAt(data, "continuation")); token != "" {
				return playlistContinuationToken{token, playlistString(playlistAt(data, "clickTrackingParams"))}
			}
		}
	}
	return playlistContinuationToken{}
}

func playlistEntry(key string, renderer any) PlaylistEntry {
	entry := PlaylistEntry{
		ID: playlistString(playlistAt(renderer, "videoId")), Kind: "video",
		Title:   playlistText(playlistAt(renderer, "title")),
		Channel: playlistText(playlistAt(renderer, "shortBylineText")),
	}
	shorts := key == "reelItemRenderer" || key == "shortsLockupViewModel" ||
		strings.HasPrefix(playlistString(playlistAt(renderer, "navigationEndpoint", "commandMetadata", "webCommandMetadata", "url")), "/shorts/")
	switch key {
	case "shortsLockupViewModel":
		entry.ID = playlistString(playlistAt(renderer, "onTap", "innertubeCommand", "reelWatchEndpoint", "videoId"))
		if entry.ID == "" {
			entry.ID = strings.TrimPrefix(playlistString(playlistAt(renderer, "entityId")), "shorts-shelf-item-")
		}
		entry.Title = playlistText(playlistAt(renderer, "overlayMetadata", "primaryText"))
	case "lockupViewModel":
		entry.ID = playlistString(playlistAt(renderer, "contentId"))
		entry.Title = playlistText(playlistAt(renderer, "metadata", "lockupMetadataViewModel", "title"))
		switch playlistString(playlistAt(renderer, "contentType")) {
		case "LOCKUP_CONTENT_TYPE_VIDEO":
			entry.Channel = playlistLockupChannel(renderer)
			entry.DurationSeconds = playlistLockupDuration(renderer)
		case "LOCKUP_CONTENT_TYPE_PLAYLIST", "LOCKUP_CONTENT_TYPE_PODCAST", "LOCKUP_CONTENT_TYPE_COURSE":
			entry.Kind = "playlist"
		default:
			return PlaylistEntry{}
		}
	case "playlistRenderer", "gridPlaylistRenderer":
		entry.ID = playlistString(playlistAt(renderer, "playlistId"))
		entry.Kind = "playlist"
	}
	if entry.Kind == "playlist" {
		if !playlistIDPattern.MatchString(entry.ID) {
			return PlaylistEntry{}
		}
		entry.URL = youtubeBase + playlistPath(entry.ID)
		return entry
	}
	if !videoIDPattern.MatchString(entry.ID) {
		return PlaylistEntry{}
	}
	if entry.Title == "" {
		entry.Title = playlistText(playlistAt(renderer, "headline"))
	}
	if entry.Channel == "" {
		entry.Channel = playlistText(playlistAt(renderer, "ownerText"))
	}
	if entry.DurationSeconds == 0 {
		entry.DurationSeconds = playlistDuration(playlistText(playlistAt(renderer, "lengthText")))
	}
	if entry.DurationSeconds == 0 {
		entry.DurationSeconds, _ = strconv.Atoi(playlistString(playlistAt(renderer, "lengthSeconds")))
	}
	entry.URL = youtubeBase + "/watch?v=" + entry.ID
	if shorts {
		entry.URL = youtubeBase + "/shorts/" + entry.ID
	}
	return entry
}

func playlistLockupChannel(renderer any) string {
	rows := playlistArray(playlistAt(renderer, "metadata", "lockupMetadataViewModel", "metadata", "contentMetadataViewModel", "metadataRows"))
	for _, row := range rows {
		for _, part := range playlistArray(playlistAt(row, "metadataParts")) {
			text := playlistAt(part, "text")
			for _, command := range playlistArray(playlistAt(text, "commandRuns")) {
				id := playlistString(playlistAt(command, "onTap", "innertubeCommand", "browseEndpoint", "browseId"))
				if playlistChannelID.MatchString(id) {
					return playlistText(text)
				}
			}
		}
	}
	return ""
}

func playlistLockupDuration(renderer any) int {
	for _, overlay := range playlistArray(playlistAt(renderer, "contentImage", "thumbnailViewModel", "overlays")) {
		for _, badges := range []any{
			playlistAt(overlay, "thumbnailBottomOverlayViewModel", "badges"),
			playlistAt(overlay, "thumbnailOverlayBadgeViewModel", "thumbnailBadges"),
		} {
			for _, badge := range playlistArray(badges) {
				if seconds := playlistDuration(playlistText(playlistAt(badge, "thumbnailBadgeViewModel", "text"))); seconds > 0 {
					return seconds
				}
			}
		}
	}
	return 0
}

func playlistDuration(text string) int {
	parts := strings.Split(text, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0
	}
	seconds := 0
	for i, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 || value > 10000 || (i > 0 && value > 59) {
			return 0
		}
		seconds = seconds*60 + value
	}
	return seconds
}

func playlistTab(data playlistObject) any {
	for _, key := range []string{"twoColumnBrowseResultsRenderer", "singleColumnBrowseResultsRenderer"} {
		for _, item := range playlistArray(playlistAt(data, "contents", key, "tabs")) {
			for _, tab := range []any{playlistAt(item, "tabRenderer"), playlistAt(item, "expandableTabRenderer")} {
				if selected, _ := playlistAt(tab, "selected").(bool); selected {
					return tab
				}
			}
		}
	}
	return nil
}

func playlistAlert(data playlistObject) error {
	for _, alert := range playlistArray(data["alerts"]) {
		for _, key := range []string{"alertRenderer", "alertWithButtonRenderer"} {
			renderer := playlistAt(alert, key)
			if strings.EqualFold(playlistString(playlistAt(renderer, "type")), "error") {
				if message := playlistText(playlistAt(renderer, "text")); message != "" {
					return fmt.Errorf("YouTube collection: %s", message)
				}
			}
		}
	}
	return nil
}

type playlistObject = map[string]any

func playlistAt(value any, keys ...string) any {
	for _, key := range keys {
		object, _ := value.(map[string]any)
		value = object[key]
	}
	return value
}

func playlistArray(value any) []any {
	array, _ := value.([]any)
	return array
}

func playlistString(value any) string {
	text, _ := value.(string)
	return text
}

func playlistText(value any) string {
	if text := playlistString(value); text != "" {
		return text
	}
	for _, field := range []string{"simpleText", "content"} {
		if text := playlistString(playlistAt(value, field)); text != "" {
			return text
		}
	}
	var text strings.Builder
	for _, run := range playlistArray(playlistAt(value, "runs")) {
		text.WriteString(playlistString(playlistAt(run, "text")))
	}
	return text.String()
}
