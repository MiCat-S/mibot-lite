// Package mibox 读取 MiBox 部署里的数据，供 --import-mibox 迁移用。
//
// MiBox 有些数据存在 SQLite 里（比如别名）。mibot-lite 为了省内存不带 SQLite，
// 这里只实现迁移需要的那一小部分：按 SQLite 文件格式读出一张普通表的全部行。
// 格式见 https://www.sqlite.org/fileformat.html 。
package mibox

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
)

// sqliteFile 是整个读进内存的 SQLite 数据库。迁移的数据库只有几十 KB，没必要按页读。
type sqliteFile struct {
	data     []byte
	pageSize int
	usable   int
}

func parseSQLite(data []byte) (*sqliteFile, error) {
	if len(data) < 100 || string(data[:16]) != "SQLite format 3\x00" {
		return nil, errors.New("不是 SQLite 数据库")
	}
	pageSize := int(binary.BigEndian.Uint16(data[16:18]))
	if pageSize == 1 {
		pageSize = 65536
	}
	if pageSize < 512 || len(data)%pageSize != 0 {
		return nil, errors.New("SQLite 页大小异常")
	}
	return &sqliteFile{data: data, pageSize: pageSize, usable: pageSize - int(data[20])}, nil
}

// page 返回第 n 页（从 1 开始）。
func (f *sqliteFile) page(n uint32) ([]byte, error) {
	start := int(n-1) * f.pageSize
	if n == 0 || start+f.pageSize > len(f.data) {
		return nil, fmt.Errorf("页号 %d 超出文件", n)
	}
	return f.data[start : start+f.pageSize], nil
}

// rows 按 rowid 顺序读出以 root 为根的表 B 树里的每一行。
func (f *sqliteFile) rows(root uint32) ([][]any, error) {
	var out [][]any
	visited := map[uint32]bool{}
	var walk func(n uint32) error
	walk = func(n uint32) error {
		if visited[n] {
			return errors.New("SQLite B 树有环")
		}
		visited[n] = true
		page, err := f.page(n)
		if err != nil {
			return err
		}
		header := 0
		if n == 1 {
			header = 100
		}
		kind := page[header]
		cells := int(binary.BigEndian.Uint16(page[header+3 : header+5]))
		pointers := header + 8
		if kind == 0x05 {
			pointers = header + 12
		}
		if pointers+2*cells > len(page) {
			return errors.New("SQLite 单元数异常")
		}
		for index := 0; index < cells; index++ {
			offset := int(binary.BigEndian.Uint16(page[pointers+2*index:]))
			if offset+4 > len(page) {
				return errors.New("SQLite 单元偏移异常")
			}
			switch kind {
			case 0x05: // 内部页：左子页号 + rowid
				if err := walk(binary.BigEndian.Uint32(page[offset:])); err != nil {
					return err
				}
			case 0x0D: // 叶子页：负载长度、rowid、负载
				size, used := varint(page[offset:])
				_, rowidLen := varint(page[offset+used:])
				payload, err := f.payload(page, offset+used+rowidLen, int(size))
				if err != nil {
					return err
				}
				row, err := record(payload)
				if err != nil {
					return err
				}
				out = append(out, row)
			default:
				return fmt.Errorf("不是表 B 树页（类型 0x%02x）", kind)
			}
		}
		if kind == 0x05 {
			return walk(binary.BigEndian.Uint32(page[header+8:]))
		}
		return nil
	}
	return out, walk(root)
}

// payload 取出一个叶子单元的完整负载，放不下的部分在溢出页链上。
func (f *sqliteFile) payload(page []byte, start, size int) ([]byte, error) {
	maxLocal := f.usable - 35
	if size <= maxLocal {
		if start+size > len(page) {
			return nil, errors.New("SQLite 负载越界")
		}
		return page[start : start+size], nil
	}
	minLocal := (f.usable-12)*32/255 - 23
	local := minLocal + (size-minLocal)%(f.usable-4)
	if local > maxLocal {
		local = minLocal
	}
	if start+local+4 > len(page) {
		return nil, errors.New("SQLite 负载越界")
	}
	out := append([]byte{}, page[start:start+local]...)
	next := binary.BigEndian.Uint32(page[start+local:])
	for len(out) < size {
		overflow, err := f.page(next)
		if err != nil {
			return nil, err
		}
		chunk := min(size-len(out), f.usable-4)
		out = append(out, overflow[4:4+chunk]...)
		next = binary.BigEndian.Uint32(overflow)
	}
	return out, nil
}

// varint 解一个 SQLite 变长整数，返回值和占用的字节数。
func varint(data []byte) (uint64, int) {
	var value uint64
	for index := 0; index < 9 && index < len(data); index++ {
		if index == 8 {
			return value<<8 | uint64(data[index]), 9
		}
		value = value<<7 | uint64(data[index]&0x7f)
		if data[index]&0x80 == 0 {
			return value, index + 1
		}
	}
	return value, len(data)
}

// record 解一条记录：文本是 string，整数是 int64，浮点是 float64，NULL 是 nil，BLOB 是 []byte。
func record(payload []byte) ([]any, error) {
	headerSize, used := varint(payload)
	if int(headerSize) > len(payload) {
		return nil, errors.New("SQLite 记录头越界")
	}
	var types []uint64
	for position := used; position < int(headerSize); {
		value, n := varint(payload[position:])
		types = append(types, value)
		position += n
	}
	body := payload[headerSize:]
	values := make([]any, 0, len(types))
	for _, kind := range types {
		size := 0
		switch {
		case kind >= 12 && kind%2 == 0:
			size = int(kind-12) / 2
		case kind >= 13:
			size = int(kind-13) / 2
		case kind >= 1 && kind <= 4:
			size = int(kind)
		case kind == 5:
			size = 6
		case kind == 6 || kind == 7:
			size = 8
		}
		if size > len(body) {
			return nil, errors.New("SQLite 记录越界")
		}
		field := body[:size]
		body = body[size:]
		switch {
		case kind == 0:
			values = append(values, nil)
		case kind == 8:
			values = append(values, int64(0))
		case kind == 9:
			values = append(values, int64(1))
		case kind == 7:
			values = append(values, math.Float64frombits(binary.BigEndian.Uint64(field)))
		case kind >= 1 && kind <= 6:
			var number int64
			for _, b := range field {
				number = number<<8 | int64(b)
			}
			shift := 64 - 8*uint(size)
			values = append(values, number<<shift>>shift)
		case kind >= 13 && kind%2 == 1:
			values = append(values, string(field))
		default:
			values = append(values, append([]byte{}, field...))
		}
	}
	return values, nil
}

// readTable 读出名为 name 的表的全部行。
func readTable(data []byte, name string) ([][]any, error) {
	file, err := parseSQLite(data)
	if err != nil {
		return nil, err
	}
	schema, err := file.rows(1)
	if err != nil {
		return nil, err
	}
	for _, row := range schema {
		if len(row) >= 4 && row[0] == "table" && row[1] == name {
			root, ok := row[3].(int64)
			if !ok || root <= 0 {
				return nil, errors.New("表的根页号异常")
			}
			return file.rows(uint32(root))
		}
	}
	return nil, fmt.Errorf("没有 %s 表", name)
}

// Aliases 解析 MiBox 的 assets/alias/alias.db（表 aliases：original → final）。
func Aliases(data []byte) (map[string]string, error) {
	rows, err := readTable(data, "aliases")
	if err != nil {
		return nil, err
	}
	aliases := make(map[string]string, len(rows))
	for _, row := range rows {
		if len(row) < 2 {
			continue
		}
		original, _ := row[0].(string)
		final, _ := row[1].(string)
		if original != "" && final != "" {
			aliases[original] = final
		}
	}
	return aliases, nil
}
