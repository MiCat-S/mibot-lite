// Package media 调用外部的 ffmpeg，完成唯一一件用纯 Go 做代价过高的
// 事：VP9 编码，Telegram 视频贴纸必须是这个格式。
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
