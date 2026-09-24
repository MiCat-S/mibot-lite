package media

import (
	"encoding/binary"
	"math"
	"testing"
)

// element 拼出一个 EBML 元素：ID 原样写入，长度用 8 字节的变长整数。
func element(id []byte, payload ...[]byte) []byte {
	var body []byte
	for _, part := range payload {
		body = append(body, part...)
	}
	size := make([]byte, 8)
	binary.BigEndian.PutUint64(size, uint64(len(body)))
	size[0] = 0x01
	return append(append(append([]byte{}, id...), size...), body...)
}

// unknownSize 拼出一个长度未知的容器，流式写出的 WebM 的 Segment 就是这样。
func unknownSize(id []byte, payload ...[]byte) []byte {
	out := append(append([]byte{}, id...), 0x01, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff)
	for _, part := range payload {
		out = append(out, part...)
	}
	return out
}

// concat 拼出一份新的字节串，不和任何参数共用底层数组。
func concat(parts ...[]byte) []byte {
	var out []byte
	for _, part := range parts {
		out = append(out, part...)
	}
	return out
}

func float64Bytes(value float64) []byte {
	out := make([]byte, 8)
	binary.BigEndian.PutUint64(out, math.Float64bits(value))
	return out
}

func float32Bytes(value float32) []byte {
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, math.Float32bits(value))
	return out
}

var (
	idHeader    = []byte{0x1a, 0x45, 0xdf, 0xa3}
	idSegment   = []byte{0x18, 0x53, 0x80, 0x67}
	idSeekHead  = []byte{0x11, 0x4d, 0x9b, 0x74}
	idInfo      = []byte{0x15, 0x49, 0xa9, 0x66}
	idTimescale = []byte{0x2a, 0xd7, 0xb1}
	idDuration  = []byte{0x44, 0x89}
	idTracks    = []byte{0x16, 0x54, 0xae, 0x6b}
	idTrack     = []byte{0xae}
	idVideo     = []byte{0xe0}
	idWidth     = []byte{0xb0}
	idHeight    = []byte{0xba}
	idCluster   = []byte{0x1f, 0x43, 0xb6, 0x75}
)

func TestWebMInfo(t *testing.T) {
	header := element(idHeader, element([]byte{0x42, 0x82}, []byte("webm")))
	video := element(idTracks, element(idTrack, element([]byte{0xd7}, []byte{1}), element(idVideo,
		element(idWidth, []byte{0x02, 0x00}), element(idHeight, []byte{0x01, 0x80}))))
	cluster := element(idCluster, []byte{0xe7, 0x81, 0x00})

	cases := []struct {
		name     string
		data     []byte
		w, h     int
		duration float64
	}{
		{"ffmpeg 写出的完整文件", concat(header, element(idSegment, element(idSeekHead, []byte{0xec, 0x80}),
			element(idInfo, element(idTimescale, []byte{0x0f, 0x42, 0x40}), element(idDuration, float64Bytes(2500))), video, cluster)),
			512, 384, 2.5},
		{"长度未知的 Segment、4 字节时长", concat(header, unknownSize(idSegment,
			element(idInfo, element(idDuration, float32Bytes(1200))), video, cluster)), 512, 384, 1.2},
		{"自定义时间单位", concat(header, element(idSegment,
			element(idInfo, element(idTimescale, []byte{0x98, 0x96, 0x80}), element(idDuration, float64Bytes(30))), video)), 512, 384, 0.3},
		{"没有时长", concat(header, element(idSegment, video)), 512, 384, 0},
	}
	for _, c := range cases {
		w, h, duration, err := WebMInfo(c.data)
		if err != nil || w != c.w || h != c.h || math.Abs(duration-c.duration) > 1e-6 {
			t.Errorf("%s：%dx%d %.3f 秒 %v，应为 %dx%d %.3f 秒", c.name, w, h, duration, err, c.w, c.h, c.duration)
		}
	}

	// 尺寸写在 Cluster 之后的不算（真实文件不会这样），截断到读不到尺寸的也不算。
	for name, data := range map[string][]byte{
		"不是 WebM":   []byte("RIFF....WEBPVP8 "),
		"只有文件头":     header,
		"尺寸在画面数据之后": concat(header, element(idSegment, cluster, video)),
		"截断在轨道中间":   concat(header, element(idSegment, video))[:len(header)+20],
	} {
		if _, _, _, err := WebMInfo(data); err == nil {
			t.Errorf("%s：应该读不出尺寸", name)
		}
	}
}
