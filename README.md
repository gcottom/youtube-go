# youtube-go

A Go library and command-line tool for public YouTube videos, playlists, channels, and search. The extractor and HTTP downloads use the Go standard library.

## Acknowledgments

Thank you to the yt-dlp maintainers and contributors, and to the youtube-dl contributors who came before them. They've put years of work into keeping downloads working as YouTube changes. This project uses yt-dlp's YouTube extractor as its behavior reference and bundles yt-dlp EJS to solve player challenges. [yt-dlp](https://github.com/yt-dlp/yt-dlp) · [youtube-dl](https://github.com/ytdl-org/youtube-dl) · [Reference extractor](https://github.com/yt-dlp/yt-dlp/tree/6f73a8d1664a58cdef8c25f01ffb5ab9cf730408/yt_dlp/extractor/youtube) · [yt-dlp EJS](https://github.com/yt-dlp/ejs)

## Install

With Go 1.23 or later, install the command with:

```text
go install github.com/gcottom/youtube-go/cmd/youtube-go@latest
```

To build and test a local checkout, run these commands from the project directory.

```text
go build ./cmd/youtube-go
go test ./...
```

The executable is `youtube-go.exe` on Windows and `youtube-go` on Linux and macOS.

- Node.js 22.13 or later resolves YouTube player signatures and throttling parameters. The bundled JavaScript solver runs locally. Use `--node` to set the Node path or `--no-js` to use only formats that need no JavaScript.
- FFmpeg combines separate audio and video without transcoding and handles HLS or DASH manifest downloads. Put it on PATH or pass `--ffmpeg` to the download command. Direct combined and audio-only downloads work without FFmpeg.

## CLI

Place options before the YouTube URL, ID, or quoted search query.

```powershell
.\youtube-go.exe info jNQXAC9IVRw
.\youtube-go.exe formats jNQXAC9IVRw
.\youtube-go.exe download -o zoo.mp4 -f 18 jNQXAC9IVRw
.\youtube-go.exe download --no-js -f bestaudio jNQXAC9IVRw
.\youtube-go.exe subtitle --lang en jNQXAC9IVRw
.\youtube-go.exe thumbnail jNQXAC9IVRw
.\youtube-go.exe playlist --limit 10 "https://www.youtube.com/@3blue1brown/videos"
.\youtube-go.exe search --limit 5 "3Blue1Brown linear algebra"
.\youtube-go.exe download --playlist --limit 2 -o clips "https://www.youtube.com/@3blue1brown/shorts"
```

Video input accepts IDs and watch, share, Shorts, live, or embed URLs. A watch URL with a playlist parameter selects its video unless you use the `playlist` command or `download --playlist`. Playlist input accepts IDs, URLs, and channel handles. Channel tabs include Home, Videos, Shorts, Live, Playlists, and Podcasts. The bare handle opens Videos.

Use `formats` to see available itags. Select an exact itag such as `18`, combine separate tracks with `137+140`, or use `best`, `bestaudio`, `bestvideo`, `hls`, or `dash`. Use `/` between fallback choices. The default `best` prefers competitive adaptive video with separate audio when FFmpeg is available, then combined HTTP media, then an FFmpeg-readable manifest. `bestaudio` downloads the best available audio-only source in its original codec.

Downloads refuse to overwrite existing files. Playlist downloads write numbered filenames with the selected extension, continue after individual failures, and report a nonzero exit status when any entries fail. Nested collections can be listed with `playlist` and downloaded by passing the nested URL directly.

## Go API

Import `github.com/gcottom/youtube-go/youtube` to use the library. The package exposes `NewClient`, `Video`, `Playlist`, `Search`, `Download`, `WriteSubtitle`, and `WriteThumbnail`. Methods accept a context for cancellation. Supply a custom `HTTPClient` or `ChallengeSolver` through `youtube.Options`.

```go
client := youtube.NewClient(youtube.Options{})
video, err := client.Video(ctx, "jNQXAC9IVRw")
if err != nil {
    return err
}
_, err = client.Download(ctx, video, youtube.DownloadOptions{Format: "18"})
return err
```

## Scope

The CLI exposes its own commands and format selector. Coverage includes public metadata, resolved stream URLs, thumbnail images, advertised manual and automatic WebVTT tracks, and chapters when present. HLS and DASH are exposed at the manifest level. Media URLs are temporary.

Account sign-in, private videos, DRM, YouTube mixes, SABR-only streams, Proof of Origin token generation, translated captions, comments, SponsorBlock, download resume, and full livestream DVR capture are outside the current scope. Available formats depend on the responses YouTube sends to the public player clients. Canceling a download discards the staged file. The download destination filesystem must support hard links for atomic publication.

## License

Project code is released under the [Unlicense](LICENSE). The bundled [yt-dlp EJS 0.8.0](https://github.com/yt-dlp/ejs/releases/tag/0.8.0) assets retain their [upstream license notices](youtube/ejs/NOTICE.txt), which also record the release checksums.
