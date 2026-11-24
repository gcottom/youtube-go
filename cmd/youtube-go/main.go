package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/gcottom/youtube-go/youtube"
)

const usage = `Usage: youtube-go <command> [options] <YouTube URL or video ID>

  info       Print video metadata and resolved formats as JSON
  formats    List available media formats
  download   Download one video, or a list with --playlist
  playlist   List playlist or channel entries as JSON
  search     Search YouTube and return matches as JSON
  subtitle   Save a WebVTT subtitle track
  thumbnail  Save the highest-resolution thumbnail

A URL or video ID without a command runs download. Use <command> -h for options.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		_, err := fmt.Fprint(stdout, usage)
		return err
	}
	command := args[0]
	switch command {
	case "info", "formats", "download", "playlist", "search", "subtitle", "thumbnail":
		args = args[1:]
	default:
		command = "download"
	}

	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		input := "YouTube URL or video ID"
		if command == "search" {
			input = "search query"
		}
		fmt.Fprintf(stderr, "Usage: youtube-go %s [options] <%s>\n", command, input)
		flags.PrintDefaults()
	}
	var node, output, selector, ffmpeg, language string
	var noJS, playlist bool
	var limit int
	if command != "playlist" && command != "search" {
		flags.StringVar(&node, "node", "", "Node.js executable (defaults to node on PATH)")
		flags.BoolVar(&noJS, "no-js", false, "skip formats that need JavaScript")
	}
	switch command {
	case "download":
		flags.StringVar(&output, "o", "", "output file, or destination directory with --playlist")
		flags.StringVar(&selector, "f", "best", "format selector, such as 18, 137+140, bestaudio, hls, or 18/best")
		flags.StringVar(&ffmpeg, "ffmpeg", "", "FFmpeg executable (defaults to ffmpeg on PATH)")
		flags.BoolVar(&playlist, "playlist", false, "download each video in a playlist or channel tab")
		flags.IntVar(&limit, "limit", 0, "maximum playlist entries to process (0 fetches all)")
	case "playlist":
		flags.IntVar(&limit, "limit", 0, "maximum entries to list (0 fetches all)")
	case "search":
		flags.IntVar(&limit, "limit", 20, "maximum search results")
	case "subtitle":
		flags.StringVar(&output, "o", "", "output .vtt file")
		flags.StringVar(&language, "lang", "", "subtitle language code (for example, en)")
	case "thumbnail":
		flags.StringVar(&output, "o", "", "output image file")
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errors.New("invalid options")
	}
	if flags.NArg() != 1 {
		return fmt.Errorf("provide exactly one argument. Run youtube-go %s -h for options", command)
	}
	if limit < 0 || (limit == 0 && command == "search") || (limit != 0 && command == "download" && !playlist) {
		return errors.New("invalid --limit value for this command")
	}
	input := flags.Arg(0)
	client := youtube.NewClient(youtube.Options{Solver: youtube.NodeSolver{Executable: node}, SkipJavaScript: noJS})
	if command == "playlist" || command == "search" {
		var list *youtube.Playlist
		var err error
		if command == "search" {
			list, err = client.Search(ctx, input, limit)
		} else {
			list, err = client.Playlist(ctx, input, limit)
		}
		if err != nil {
			return err
		}
		return printJSON(stdout, list)
	}
	if command == "download" && playlist {
		return downloadPlaylist(ctx, client, input, output, limit, youtube.DownloadOptions{Format: selector, FFmpeg: ffmpeg}, stdout, stderr)
	}

	video, err := client.Video(ctx, input)
	if err != nil {
		return err
	}
	if command != "info" {
		printWarnings(stderr, video)
	}
	switch command {
	case "info":
		return printJSON(stdout, video)
	case "formats":
		return printFormats(stdout, video.Formats)
	}

	var path string
	switch command {
	case "download":
		path, err = client.Download(ctx, video, youtube.DownloadOptions{Output: output, Format: selector, FFmpeg: ffmpeg})
	case "subtitle":
		path, err = client.WriteSubtitle(ctx, video, language, output)
	case "thumbnail":
		path, err = client.WriteThumbnail(ctx, video, output)
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, path)
	return err
}

func downloadPlaylist(ctx context.Context, client *youtube.Client, input, dir string, limit int, opts youtube.DownloadOptions, stdout, stderr io.Writer) error {
	list, err := client.Playlist(ctx, input, limit)
	if err != nil {
		return err
	}
	var done, failed int
	if dir == "" {
		dir = "."
	}
	for i, entry := range list.Entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if entry.Kind == "playlist" {
			fmt.Fprintf(stderr, "Skipping nested playlist %s\n", entry.ID)
			continue
		}
		video, err := client.Video(ctx, entry.ID)
		if err == nil {
			printWarnings(stderr, video)
			entryOptions := opts
			entryOptions.Output = filepath.Join(dir, fmt.Sprintf("%03d-%s", i+1, video.ID))
			entryOptions.AppendExtension = true
			var path string
			path, err = client.Download(ctx, video, entryOptions)
			if err == nil {
				done++
				fmt.Fprintln(stdout, path)
			}
		}
		if err != nil {
			failed++
			fmt.Fprintf(stderr, "%s: %v\n", entry.ID, err)
		}
	}
	if failed > 0 || done == 0 {
		return fmt.Errorf("downloaded %d of %d playlist entries (%d failed)", done, len(list.Entries), failed)
	}
	return nil
}

func printJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func printWarnings(output io.Writer, video *youtube.Video) {
	for _, warning := range video.Warnings {
		fmt.Fprintln(output, "Warning:", warning)
	}
}

func printFormats(output io.Writer, formats []youtube.Format) error {
	if len(formats) == 0 {
		return errors.New("no playable formats available")
	}
	table := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "ID\tEXT\tRESOLUTION\tFPS\tBITRATE\tSTREAM\tAUDIO TRACK")
	for _, format := range formats {
		id := format.Protocol
		if format.Itag > 0 {
			id = strconv.Itoa(format.Itag)
		}
		resolution := "-"
		if format.Width > 0 && format.Height > 0 {
			resolution = fmt.Sprintf("%dx%d", format.Width, format.Height)
		}
		fps, rate := "-", "-"
		if format.FPS > 0 {
			fps = strconv.Itoa(format.FPS)
		}
		if format.Bitrate > 0 {
			rate = fmt.Sprintf("%.0f kbps", float64(format.Bitrate)/1000)
		}
		var streams []string
		if format.HasVideo {
			streams = append(streams, "video")
		}
		if format.HasAudio {
			streams = append(streams, "audio")
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", id, format.Extension, resolution, fps, rate, strings.Join(streams, "+"), format.AudioTrack)
	}
	return table.Flush()
}
