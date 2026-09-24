package media

import (
	"encoding/binary"
	"errors"
	"math"
)

// ErrNotWebM 表示这些字节不是能读出画面尺寸的 WebM 文件。
var ErrNotWebM = errors.New("not a WebM file")

// 用到的 EBML 元素 ID（含长度标记位，和文件里的字节一致）。
const (
	ebmlSegment   = 0x18538067
	ebmlInfo      = 0x1549A966
	ebmlTimescale = 0x2AD7B1
	ebmlDuration  = 0x4489
	ebmlTracks    = 0x1654AE6B
	ebmlTrack     = 0xAE
	ebmlVideo     = 0xE0
	ebmlWidth     = 0xB0
	ebmlHeight    = 0xBA
	ebmlCluster   = 0x1F43B675
)

// webmHeader 收集 WebMInfo 读到的字段。
type webmHeader struct {
	width, height int
	// scale 是时间单位的纳秒数，Duration 以它为单位；规范默认 1 毫秒。
	scale    uint64
	duration float64
}

// WebMInfo 从 WebM 文件头读出画面宽高和时长（秒），读不到时长时 duration 为 0。
//
// Telegram 发视频贴纸时要带 DocumentAttributeVideo，里面是宽高和时长。这里只走一遍
// EBML 的头部元素（Segment → Info、Tracks → TrackEntry → Video），读到第一个 Cluster
// 就停，不解码任何画面，也就不需要为了三个数去调 ffprobe。
func WebMInfo(data []byte) (width, height int, duration float64, err error) {
	if len(data) < 4 || data[0] != 0x1a || data[1] != 0x45 || data[2] != 0xdf || data[3] != 0xa3 {
		return 0, 0, 0, ErrNotWebM
	}
	header := webmHeader{scale: 1_000_000}
	walkEBML(data, &header)
	if header.width <= 0 || header.height <= 0 {
		return 0, 0, 0, ErrNotWebM
	}
	seconds := header.duration * float64(header.scale) / 1e9
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 {
		seconds = 0
	}
	return header.width, header.height, seconds, nil
}

// walkEBML 依次读 data 里的元素，进入关心的容器，其余按长度跳过。
// 读到 Cluster、长度未知又无法跳过的元素或截断的数据时返回 false，表示整个遍历该停了。
func walkEBML(data []byte, header *webmHeader) bool {
	for len(data) > 0 {
		id, idLength, ok := ebmlID(data)
		if !ok {
			return false
		}
		size, sizeLength, known, ok := ebmlSize(data[idLength:])
		if !ok {
			return false
		}
		body := data[idLength+sizeLength:]
		// 长度未知的容器一直延伸到父元素末尾；声明的长度超出现有数据说明文件被截断，
		// 两种情况都只能读完手上这些就停。
		complete := known && size <= uint64(len(body))
		if complete {
			body = body[:size]
		}
		switch id {
		case ebmlCluster:
			return false
		case ebmlSegment, ebmlInfo, ebmlTracks, ebmlTrack, ebmlVideo:
			if !walkEBML(body, header) || !complete {
				return false
			}
		case ebmlTimescale:
			if !complete {
				return false
			}
			if value, ok := ebmlUint(body); ok && value > 0 {
				header.scale = value
			}
		case ebmlDuration:
			if !complete {
				return false
			}
			switch len(body) {
			case 4:
				header.duration = float64(math.Float32frombits(binary.BigEndian.Uint32(body)))
			case 8:
				header.duration = math.Float64frombits(binary.BigEndian.Uint64(body))
			}
		case ebmlWidth, ebmlHeight:
			if !complete {
				return false
			}
			value, ok := ebmlUint(body)
			if !ok || value > 1<<16 {
				break
			}
			// 有多条视频轨时以第一条为准。
			if id == ebmlWidth && header.width == 0 {
				header.width = int(value)
			}
			if id == ebmlHeight && header.height == 0 {
				header.height = int(value)
			}
		default:
			if !complete {
				return false
			}
		}
		// 走到这里的元素都是完整的，size 不超过剩余数据。
		data = data[idLength+sizeLength+int(size):]
	}
	return true
}

// ebmlID 读元素 ID：首字节前导零的个数加一就是 ID 的字节数（1 到 4），标记位保留在值里。
func ebmlID(data []byte) (id uint32, length int, ok bool) {
	if len(data) == 0 || data[0] == 0 {
		return 0, 0, false
	}
	length = leadingZeros(data[0]) + 1
	if length > 4 || len(data) < length {
		return 0, 0, false
	}
	for _, b := range data[:length] {
		id = id<<8 | uint32(b)
	}
	return id, length, true
}

// ebmlSize 读元素长度：变长整数，去掉标记位就是值；值的各位全是 1 表示长度未知。
func ebmlSize(data []byte) (size uint64, length int, known, ok bool) {
	if len(data) == 0 || data[0] == 0 {
		return 0, 0, false, false
	}
	length = leadingZeros(data[0]) + 1
	if len(data) < length {
		return 0, 0, false, false
	}
	size = uint64(data[0]) & (0xff >> length)
	allOnes := size == uint64(0xff>>length)
	for _, b := range data[1:length] {
		size = size<<8 | uint64(b)
		allOnes = allOnes && b == 0xff
	}
	return size, length, !allOnes, true
}

// ebmlUint 读一个最多 8 字节的大端无符号整数。
func ebmlUint(body []byte) (uint64, bool) {
	if len(body) == 0 || len(body) > 8 {
		return 0, false
	}
	var value uint64
	for _, b := range body {
		value = value<<8 | uint64(b)
	}
	return value, true
}

func leadingZeros(b byte) int {
	count := 0
	for mask := byte(0x80); mask != 0 && b&mask == 0; mask >>= 1 {
		count++
	}
	return count
}
