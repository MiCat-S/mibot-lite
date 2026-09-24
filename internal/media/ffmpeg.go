// Package media 调用外部的 ffmpeg，完成用纯 Go 做代价过高的编码：
// Telegram 视频贴纸要的 VP9，静态贴纸要的 WebP（Go 只有 WebP 解码器），
// 以及语音消息要的 Opus。
//
// ffmpeg 是子进程，空闲时没有任何开销；把编码器链接进常驻的程序则
// 正好相反。
package media

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ErrUnavailable 表示本机上找不到 ffmpeg。
var ErrUnavailable = errors.New("ffmpeg 不可用，请先安装 ffmpeg")

var candidates = []string{"/usr/bin/ffmpeg", "/usr/local/bin/ffmpeg", "/opt/homebrew/bin/ffmpeg"}

// FFmpeg 查找 ffmpeg 可执行文件。
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

// Frame 是序列中的一帧静态图，以及它的显示时长。
type Frame struct {
	// Path 是磁盘上的一个 PNG 文件。
	Path  string
	Delay time.Duration
}

// StickerWebM 把帧序列编码成 Telegram 视频贴纸要求的 VP9 WebM，
// 返回编码后的字节。
//
// 帧通过 ffmpeg 的 concat demuxer 输入，而不是先转成中间 GIF：GIF 会把
// 每一帧量化到 256 色，而最终的编码器根本不需要这一步。MiBox 先编码成
// GIF，是因为在它那里，只有 GIF 编码器能把这些帧拼起来。
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
	// concat demuxer 会忽略最后一项的 duration，所以把最后一帧再写一遍，
	// 让它的时长生效。
	script.WriteString("file '" + filepath.Base(frames[len(frames)-1].Path) + "'\n")
	if err := os.WriteFile(list, []byte(script.String()), 0o600); err != nil {
		return nil, err
	}

	output := filepath.Join(directory, "sticker.webm")
	// libvpx-vp9 在默认速度下，耗时和内存都远超一张 512 像素贴纸的需要。
	// cpu-used 牺牲一点在这个尺寸下谁也看不出来的画质，换来一次能跑完的
	// 编码；限制线程数则让编码器自身的内存占用保持在小服务承受得了的
	// 范围内。
	args := []string{
		"-nostdin", "-v", "error",
		"-f", "concat", "-safe", "0", "-i", "frames.txt",
		"-c:v", "libvpx-vp9", "-pix_fmt", "yuva420p", "-b:v", "0", "-crf", "41",
		"-deadline", "good", "-cpu-used", "5", "-row-mt", "1", "-threads", "2",
		"-auto-alt-ref", "0", "-an",
		"-vf", fmt.Sprintf("scale=%d:%d:flags=lanczos", width, height),
		"-y", "sticker.webm",
	}
	if err := run(ctx, binary, directory, args); err != nil {
		return nil, err
	}
	return readBounded(output, 20<<20)
}

// ToStickerWebM 把已有的视频或动图转成 VP9 WebM。
func ToStickerWebM(ctx context.Context, directory string, input []byte, extension string) ([]byte, error) {
	binary, err := FFmpeg()
	if err != nil {
		return nil, err
	}
	source := filepath.Join(directory, "input"+extension)
	if err := os.WriteFile(source, input, 0o600); err != nil {
		return nil, err
	}
	args := []string{"-nostdin", "-v", "error", "-i", filepath.Base(source),
		"-c:v", "libvpx-vp9", "-pix_fmt", "yuva420p", "-b:v", "400k",
		"-deadline", "good", "-cpu-used", "5", "-row-mt", "1", "-threads", "2",
		"-auto-alt-ref", "0", "-an", "-y", "converted.webm"}
	if err := run(ctx, binary, directory, args); err != nil {
		return nil, err
	}
	return readBounded(filepath.Join(directory, "converted.webm"), 20<<20)
}

// StickerWebP 把一张 PNG 编码成静态贴纸用的 WebP。
func StickerWebP(ctx context.Context, directory string, png []byte) ([]byte, error) {
	binary, err := FFmpeg()
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(directory, "sticker.png"), png, 0o600); err != nil {
		return nil, err
	}
	args := []string{"-nostdin", "-v", "error", "-i", "sticker.png", "-frames:v", "1",
		"-c:v", "libwebp", "-lossless", "0", "-quality", "95", "-y", "sticker.webp"}
	if err := run(ctx, binary, directory, args); err != nil {
		return nil, err
	}
	return readBounded(filepath.Join(directory, "sticker.webp"), 5<<20)
}

// VoiceOgg 把一段音频转成 Telegram 语音消息要的 Ogg Opus。
func VoiceOgg(ctx context.Context, directory string, audio []byte) ([]byte, error) {
	binary, err := FFmpeg()
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(directory, "speech.mp3"), audio, 0o600); err != nil {
		return nil, err
	}
	args := []string{"-nostdin", "-v", "error", "-i", "speech.mp3", "-vn",
		"-c:a", "libopus", "-b:a", "64k", "-vbr", "on", "-y", "voice.ogg"}
	if err := run(ctx, binary, directory, args); err != nil {
		return nil, err
	}
	return readBounded(filepath.Join(directory, "voice.ogg"), 20<<20)
}

// Tags 是写进 MP3 的 ID3 信息。
type Tags struct {
	Title, Artist, Album string
	// Cover 是封面图片（JPEG 或 PNG），为空时不加封面。
	Cover []byte
}

// TaggedMP3 给一段 MP3 重新编码并写上标题、歌手、专辑和封面，
// 播放器里显示成一首歌。
func TaggedMP3(ctx context.Context, directory string, audio []byte, tags Tags) ([]byte, error) {
	binary, err := FFmpeg()
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(directory, "speech.mp3"), audio, 0o600); err != nil {
		return nil, err
	}
	args := []string{"-nostdin", "-v", "error", "-i", "speech.mp3"}
	if len(tags.Cover) > 0 {
		if err := os.WriteFile(filepath.Join(directory, "cover.img"), tags.Cover, 0o600); err != nil {
			return nil, err
		}
		args = append(args, "-i", "cover.img", "-map", "0:a", "-map", "1:v",
			"-c:v", "mjpeg", "-disposition:v", "attached_pic",
			"-metadata:s:v", "title=Album cover", "-metadata:s:v", "comment=Cover (front)")
	} else {
		args = append(args, "-map", "0:a")
	}
	args = append(args, "-c:a", "libmp3lame", "-q:a", "2", "-id3v2_version", "3",
		"-metadata", "title="+tags.Title, "-metadata", "artist="+tags.Artist, "-metadata", "album="+tags.Album,
		"-y", "song.mp3")
	if err := run(ctx, binary, directory, args); err != nil {
		return nil, err
	}
	return readBounded(filepath.Join(directory, "song.mp3"), 30<<20)
}

// durationLine 是 ffmpeg 读输入时打印的时长，如 "Duration: 00:00:03.52"。
var durationLine = regexp.MustCompile(`Duration: (\d+):(\d{2}):(\d{2})\.(\d+)`)

// Duration 读出目录里一个音频文件的时长（向上取整到秒），读不出来返回 0。
// 只给 ffmpeg 输入不给输出时，它打印完文件信息就以错误退出，时长就在那段信息里。
func Duration(ctx context.Context, directory, name string) int {
	binary, err := FFmpeg()
	if err != nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "-nostdin", "-hide_banner", "-i", name)
	command.Dir = directory
	output, _ := command.CombinedOutput()
	match := durationLine.FindStringSubmatch(string(output))
	if match == nil {
		return 0
	}
	hours, _ := strconv.Atoi(match[1])
	minutes, _ := strconv.Atoi(match[2])
	seconds, _ := strconv.Atoi(match[3])
	total := hours*3600 + minutes*60 + seconds
	if strings.Trim(match[4], "0") != "" {
		total++
	}
	return total
}

// encodeTimeout 是单次运行 ffmpeg 的时限。
const encodeTimeout = 3 * time.Minute

// run 执行 ffmpeg，并把失败转成看得懂的信息。
//
// 被杀掉的 ffmpeg 什么都不输出，所以只报告它的输出的话，得到的就是
// 后面空空如也的 "ffmpeg failed:"，没说错，但毫无用处。超时就直接
// 报告为超时。
func run(ctx context.Context, binary, directory string, args []string) error {
	ctx, cancel := context.WithTimeout(ctx, encodeTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, binary, args...)
	command.Dir = directory
	combined, err := command.CombinedOutput()
	if err == nil {
		return nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("视频编码超过 %s 仍未完成", encodeTimeout)
	}
	detail := strings.TrimSpace(string(combined))
	if detail == "" {
		detail = err.Error()
	}
	if len(detail) > 300 {
		detail = detail[len(detail)-300:]
	}
	return fmt.Errorf("ffmpeg 失败：%s", detail)
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
