// Package media shells out to ffmpeg for the one thing that cannot be done
// in pure Go at a sane cost: encoding VP9, which is what a Telegram video
// sticker must be.
//
// ffmpeg is a child process, so it costs nothing while idle — which is the
// opposite of linking an encoder into the resident image.
package media

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ErrUnavailable means no ffmpeg was found on this host.
var ErrUnavailable = errors.New("ffmpeg 不可用，请先安装 ffmpeg")

var candidates = []string{"/usr/bin/ffmpeg", "/usr/local/bin/ffmpeg", "/opt/homebrew/bin/ffmpeg"}

// FFmpeg locates the ffmpeg binary.
func FFmpeg() (string, error) {
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	if found, err := exec.LookPath("ffmpeg"); err == nil {
		return found, nil
	}
	return "", ErrUnavailable
}

// Frame is one still in a sequence, with how long it shows.
type Frame struct {
	// Path is a PNG on disk.
	Path  string
	Delay time.Duration
}

// StickerWebM encodes a frame sequence as the VP9 WebM a Telegram video
// sticker requires, and returns the bytes.
//
// The frames are fed through ffmpeg's concat demuxer rather than an
// intermediate GIF: a GIF would quantise every frame to 256 colours on the
// way to a codec that does not need it. MiBox encoded a GIF first because
// its GIF encoder was the thing that could assemble the frames at all.
func StickerWebM(ctx context.Context, directory string, frames []Frame, width, height int) ([]byte, error) {
	binary, err := FFmpeg()
	if err != nil {
		return nil, err
	}
	if len(frames) == 0 {
		return nil, errors.New("no frames to encode")
	}
	list := filepath.Join(directory, "frames.txt")
	var script strings.Builder
	for _, frame := range frames {
		seconds := frame.Delay.Seconds()
		if seconds <= 0 {
			seconds = 0.1
		}
		script.WriteString("file '" + filepath.Base(frame.Path) + "'\n")
		script.WriteString("duration " + strconv.FormatFloat(seconds, 'f', 3, 64) + "\n")
	}
	// The concat demuxer ignores the final entry's duration, so the last
	// frame is repeated to give it one.
	script.WriteString("file '" + filepath.Base(frames[len(frames)-1].Path) + "'\n")
	if err := os.WriteFile(list, []byte(script.String()), 0o600); err != nil {
		return nil, err
	}

	output := filepath.Join(directory, "sticker.webm")
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, binary,
		"-nostdin", "-v", "error",
		"-f", "concat", "-safe", "0", "-i", "frames.txt",
		"-c:v", "libvpx-vp9", "-pix_fmt", "yuva420p", "-b:v", "0", "-crf", "41",
		"-auto-alt-ref", "0", "-an",
		"-vf", fmt.Sprintf("scale=%d:%d:flags=lanczos", width, height),
		"-y", "sticker.webm")
	command.Dir = directory
	if combined, err := command.CombinedOutput(); err != nil {
		detail := strings.TrimSpace(string(combined))
		if len(detail) > 300 {
			detail = detail[len(detail)-300:]
		}
		return nil, fmt.Errorf("ffmpeg failed: %s", detail)
	}
	return readBounded(output, 20<<20)
}

// ToStickerWebM converts an existing video or animation into a VP9 WebM.
func ToStickerWebM(ctx context.Context, directory string, input []byte, extension string) ([]byte, error) {
	binary, err := FFmpeg()
	if err != nil {
		return nil, err
	}
	source := filepath.Join(directory, "input"+extension)
	if err := os.WriteFile(source, input, 0o600); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "-nostdin", "-v", "error", "-i", filepath.Base(source),
		"-c:v", "libvpx-vp9", "-pix_fmt", "yuva420p", "-b:v", "400k", "-auto-alt-ref", "0", "-an", "-y", "converted.webm")
	command.Dir = directory
	if combined, err := command.CombinedOutput(); err != nil {
		detail := strings.TrimSpace(string(combined))
		if len(detail) > 300 {
			detail = detail[len(detail)-300:]
		}
		return nil, fmt.Errorf("ffmpeg failed: %s", detail)
	}
	return readBounded(filepath.Join(directory, "converted.webm"), 20<<20)
}

func readBounded(path string, limit int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() == 0 {
		return nil, errors.New("ffmpeg produced an empty file")
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("ffmpeg produced %d bytes, over the limit", info.Size())
	}
	return os.ReadFile(path)
}
