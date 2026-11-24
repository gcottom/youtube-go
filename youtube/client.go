// Package youtube extracts public YouTube metadata, playlists, and media.
package youtube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	youtubeBase      = "https://www.youtube.com"
	defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36"
	maxResponseBytes = 16 << 20
)

// Options configures a Client. The zero value uses the default HTTP transport
// and runs the bundled JavaScript solver with Node from PATH when needed.
type Options struct {
	HTTPClient     *http.Client
	Solver         ChallengeSolver
	SkipJavaScript bool
}

// Client can be reused across requests. A custom solver must support concurrent
// calls if the Client is used concurrently.
type Client struct {
	http      *http.Client
	baseURL   string
	userAgent string
	solver    ChallengeSolver

	playerMu    sync.Mutex
	playerCache map[string][]byte
}

func NewClient(opts Options) *Client {
	hc := opts.HTTPClient
	if hc == nil {
		if defaults, ok := http.DefaultTransport.(*http.Transport); ok {
			transport := defaults.Clone()
			transport.ResponseHeaderTimeout = 25 * time.Second
			hc = &http.Client{Transport: transport}
		} else {
			hc = http.DefaultClient
		}
	}
	solver := opts.Solver
	if opts.SkipJavaScript {
		solver = nil
	} else if solver == nil {
		solver = NodeSolver{}
	}
	return &Client{http: hc, baseURL: youtubeBase, userAgent: defaultUserAgent, solver: solver}
}

type httpStatusError struct {
	host   string
	status int
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("%s returned HTTP %d (%s)", e.host, e.status, http.StatusText(e.status))
}

func (c *Client) request(ctx context.Context, method, target string, body io.Reader, headers ...http.Header) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, header := range headers {
		for key, values := range header {
			req.Header[http.CanonicalHeaderKey(key)] = append([]string(nil), values...)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request to %s: %w", req.URL.Host, redactHTTPError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &httpStatusError{req.URL.Host, resp.StatusCode}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read response from %s: %w", req.URL.Host, err)
	}
	if len(data) > maxResponseBytes {
		return nil, fmt.Errorf("response from %s exceeded %d bytes", req.URL.Host, maxResponseBytes)
	}
	return data, nil
}

func redactHTTPError(err error) error {
	var requestError *url.Error
	if errors.As(err, &requestError) {
		return requestError.Err
	}
	return err
}

var videoIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

// VideoID recognizes a bare ID and YouTube watch, share, Shorts, live, and embed URLs.
func VideoID(input string) (string, error) {
	input = strings.TrimSpace(input)
	if videoIDPattern.MatchString(input) {
		return input, nil
	}
	u, err := parseYouTubeURL(input)
	if err != nil {
		return "", err
	}
	path := strings.Split(strings.Trim(u.Path, "/"), "/")
	var id string
	switch {
	case u.Hostname() == "youtu.be" && len(path) == 1:
		id = path[0]
	case len(path) == 1 && path[0] == "watch":
		id = u.Query().Get("v")
	case len(path) == 2 && (path[0] == "shorts" || path[0] == "live" || path[0] == "embed" || path[0] == "v" || path[0] == "e"):
		id = path[1]
	}
	if !videoIDPattern.MatchString(id) {
		return "", errors.New("expected a YouTube video URL or an 11-character video ID")
	}
	return id, nil
}

func parseYouTubeURL(input string) (*url.URL, error) {
	input = strings.TrimSpace(input)
	if strings.HasPrefix(input, "//") {
		input = "https:" + input
	} else if !strings.Contains(input, "://") {
		input = "https://" + input
	}
	u, err := url.Parse(input)
	if err != nil || u.User != nil || u.Port() != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("expected an HTTP or HTTPS YouTube URL")
	}
	host := strings.ToLower(u.Hostname())
	switch host {
	case "youtube.com", "www.youtube.com", "m.youtube.com", "music.youtube.com", "youtu.be", "www.youtu.be",
		"youtube-nocookie.com", "www.youtube-nocookie.com":
		if host == "www.youtu.be" {
			host = "youtu.be"
		}
		u.Host = host
		return u, nil
	default:
		return nil, errors.New("expected a YouTube URL")
	}
}

var (
	initialPlayerPattern = embeddedPattern("ytInitialPlayerResponse")
	initialDataPattern   = embeddedPattern("ytInitialData")
	configPattern        = regexp.MustCompile(`\bytcfg\.set\s*\(\s*\{`)
)

func embeddedPattern(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?:\b` + regexp.QuoteMeta(name) + `\b|["']` + regexp.QuoteMeta(name) + `["']\s*\])\s*=\s*\{`)
}

func embeddedJSON(page []byte, name string) ([]byte, error) {
	pattern := initialPlayerPattern
	switch name {
	case "ytInitialPlayerResponse":
	case "ytInitialData":
		pattern = initialDataPattern
	case "ytcfg.set":
		pattern = configPattern
	default:
		pattern = embeddedPattern(name)
	}
	for _, match := range pattern.FindAllIndex(page, -1) {
		var value json.RawMessage
		if err := json.NewDecoder(bytes.NewReader(page[match[1]-1:])).Decode(&value); err == nil {
			return value, nil
		}
	}
	return nil, fmt.Errorf("%s was missing or invalid", name)
}
