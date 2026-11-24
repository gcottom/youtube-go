package youtube

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSelectFormats(t *testing.T) {
	formats := []Format{
		{Itag: 18, URL: "https://media.invalid/18", Height: 360, HasVideo: true, HasAudio: true},
		{Itag: 137, URL: "https://media.invalid/137", Height: 1080, HasVideo: true},
		{Itag: 140, URL: "https://media.invalid/140", Bitrate: 120000, HasAudio: true},
		{Itag: 251, URL: "https://media.invalid/251", Bitrate: 150000, HasAudio: true},
		{Itag: 251, URL: "https://media.invalid/alternate", Bitrate: 160000, HasAudio: true},
		{URL: "https://media.invalid/live.m3u8", Protocol: "hls", HasVideo: true, HasAudio: true},
		{URL: "https://media.invalid/stream.mpd", Protocol: "dash", HasVideo: true, HasAudio: true},
	}
	for _, test := range []struct {
		name, selector string
		ffmpeg         bool
		want           []int
		wantError      string
	}{
		{name: "default with ffmpeg", selector: "", ffmpeg: true, want: []int{137, 251}},
		{name: "default without ffmpeg", selector: "best", want: []int{18}},
		{name: "audio only", selector: "bestaudio", want: []int{251}},
		{name: "video only", selector: "bestvideo", want: []int{137}},
		{name: "exact progressive stream", selector: "18", want: []int{18}},
		{name: "exact pair", selector: "137+140", ffmpeg: true, want: []int{137, 140}},
		{name: "invalid merge", selector: "18+140", ffmpeg: true, wantError: "video-only stream"},
		{name: "fallback without ffmpeg", selector: "bv+ba/18", want: []int{18}},
		{name: "explicit hls", selector: "hls", ffmpeg: true, want: []int{0}},
		{name: "explicit dash", selector: "dash", ffmpeg: true, want: []int{0}},
		{name: "ffmpeg unavailable", selector: "hls", wantError: "requires FFmpeg"},
		{name: "unknown itag", selector: "19", wantError: "unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			selected, err := SelectFormats(formats, test.selector, test.ffmpeg)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("expected %q, got %v", test.wantError, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := make([]int, len(selected))
			for i, f := range selected {
				got[i] = f.Itag
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("selected %v, expected %v", got, test.want)
			}
			if (test.selector == "hls" || test.selector == "dash") && selected[0].Protocol != test.selector {
				t.Fatalf("got %q manifest, expected %q", selected[0].Protocol, test.selector)
			}
		})
	}
}

func TestSelectAudioPrefersDefaultTrack(t *testing.T) {
	formats := []Format{
		{Itag: 251, URL: "https://media.invalid/alternate", Bitrate: 160000, HasAudio: true},
		{Itag: 251, URL: "https://media.invalid/default", Bitrate: 110000, HasAudio: true, DefaultAudio: true},
	}
	selected, err := SelectFormats(formats, "bestaudio", false)
	if err != nil || len(selected) != 1 || !selected[0].DefaultAudio {
		t.Fatalf("selected %v, error = %v", selected, err)
	}
}

func TestDownloadPartialResponses(t *testing.T) {
	const media = "abcdefgh"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.UserAgent(); got != "stream-client" {
			t.Errorf("User-Agent %q, expected stream-client", got)
		}
		w.Header().Set("Content-Type", "video/mp4")
		switch r.Header.Get("Range") {
		case "bytes=0-7":
			w.Header().Set("Content-Range", "bytes 0-3/8")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, media[:4])
		case "bytes=4-7":
			w.Header().Set("Content-Range", "bytes 4-7/8")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, media[4:])
		default:
			t.Errorf("unexpected range %q", r.Header.Get("Range"))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		}
	}))
	defer server.Close()
	client := Client{http: server.Client(), userAgent: "default-client"}
	video := Video{ID: "abcdefghijk", Title: "Video", Formats: []Format{{
		Itag: 18, URL: server.URL, Extension: "mp4", ContentLength: int64(len(media)), HasAudio: true, HasVideo: true, userAgent: "stream-client",
	}}}
	dir := t.TempDir()
	output := filepath.Join(dir, "example.mp4")
	path, err := client.Download(context.Background(), &video, DownloadOptions{Output: filepath.Join(dir, "example"), AppendExtension: true})
	if err != nil || path != output {
		t.Fatalf("download path = %q, error = %v", path, err)
	}
	data, err := os.ReadFile(output)
	if err != nil || string(data) != media {
		t.Fatalf("download contains %q, error = %v", data, err)
	}
	if _, err := client.Download(context.Background(), &video, DownloadOptions{Output: output}); !errors.Is(err, os.ErrExist) {
		t.Fatalf("expected existing output error, got %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("unexpected stage files: %v, error = %v", entries, err)
	}
}

func TestDownloadRejectsIncorrectRange(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 1-3/4")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "bcd")
	}))
	defer server.Close()
	client := Client{http: server.Client()}
	video := Video{Formats: []Format{{Itag: 18, URL: server.URL, HasAudio: true, HasVideo: true}}}
	dir := t.TempDir()
	output := filepath.Join(dir, "example.mp4")
	if _, err := client.Download(context.Background(), &video, DownloadOptions{Output: output}); err == nil || !strings.Contains(err.Error(), "Content-Range") {
		t.Fatalf("expected Content-Range error, got %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed download left files: %v, error = %v", entries, err)
	}
}

func TestDownloadRequestsBoundedRangesForLargeStreams(t *testing.T) {
	const total = mediaChunkSize + 4
	media := strings.Repeat("a", int(mediaChunkSize)) + "last"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var start, end int64
		switch r.Header.Get("Range") {
		case fmt.Sprintf("bytes=0-%d", mediaChunkSize-1):
			start, end = 0, mediaChunkSize-1
		case fmt.Sprintf("bytes=%d-%d", mediaChunkSize, total-1):
			start, end = mediaChunkSize, total-1
		default:
			t.Errorf("unexpected range %q", r.Header.Get("Range"))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, media[start:end+1])
	}))
	defer server.Close()
	client := Client{http: server.Client()}
	output := filepath.Join(t.TempDir(), "large.mp4")
	if err := client.downloadHTTP(context.Background(), Format{Itag: 18, URL: server.URL, ContentLength: total}, output); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(output)
	if err != nil || string(got) != media {
		t.Fatalf("expected %d assembled bytes, got %d: %v", len(media), len(got), err)
	}
}

func TestConcurrentDownloadDoesNotOverwrite(t *testing.T) {
	var arrivals atomic.Int32
	ready := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if arrivals.Add(1) == 2 {
			close(ready)
		}
		select {
		case <-ready:
		case <-r.Context().Done():
			return
		}
		_, _ = fmt.Fprint(w, "complete media")
	}))
	defer server.Close()
	client := Client{http: server.Client()}
	video := Video{Formats: []Format{{Itag: 18, URL: server.URL, HasAudio: true, HasVideo: true}}}
	output := filepath.Join(t.TempDir(), "example.mp4")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			_, err := client.Download(ctx, &video, DownloadOptions{Output: output})
			results <- err
		}()
	}
	first, second := <-results, <-results
	if first == nil {
		first, second = second, first
	}
	if !errors.Is(first, os.ErrExist) || second != nil {
		t.Fatalf("expected one success and one file-exists error, got %v and %v", first, second)
	}
	data, err := os.ReadFile(output)
	if err != nil || string(data) != "complete media" {
		t.Fatalf("download contains %q, error = %v", data, err)
	}
}

func TestSafeVideoName(t *testing.T) {
	video := Video{Title: `CON.foo <test> / video?`, ID: "H2ap2SQgguA"}
	const want = `_CON.foo _test_ _ video_ [H2ap2SQgguA]`
	if got := safeVideoName(&video); got != want {
		t.Fatalf("got %q, expected %q", got, want)
	}
}
