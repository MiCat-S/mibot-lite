// Package session 把 gramjs/teleproto 的 StringSession（MiBox 的 Node
// 运行时存在 config.json 里的就是它）转换成 gotd 的会话存储，也能转回去。
//
// 目的是让已经在 MiBox 下登录过的账号保住会话：把 mibot-lite 指向同一个
// 目录，它就能直接连上，不用再要验证码。
package session

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	gotdcrypto "github.com/gotd/td/crypto"
	gotdsession "github.com/gotd/td/session"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"
)

// AuthKeyLength 是 Telegram auth key 的固定长度。
const AuthKeyLength = 256

// Version 是 teleproto 唯一会写出或接受的 StringSession 版本。
const Version = '1'

// StringSession 是解码后的 gramjs 会话。
type StringSession struct {
	DC      int
	Address string
	Port    int
	AuthKey []byte
}

// ParseStringSession 解码 gramjs/teleproto 的 StringSession。
//
// teleproto 的 save() 写出的布局：
//
//	| 1     | byte    | DC id                     |
//	| 2     | int16BE | 服务器地址长度            |
//	| n     | bytes   | 服务器地址（文本）        |
//	| 2     | int16BE | 端口                      |
//	| 256   | bytes   | auth key                  |
//
// 与 teleproto 的读取逻辑一致，另外还接受两种布局：Telethon 会话
// （base64 载荷正好 352 个字符，IPv4 地址是原始的 4 字节），以及
// 长度字段读出来大于 100 时，原始 16 字节的 IPv6 地址。
func ParseStringSession(text string) (*StringSession, error) {
	if len(text) < 1 {
		return nil, errors.New("session string is empty")
	}
	if text[0] != Version {
		return nil, fmt.Errorf("unsupported session version %q, expected %q", text[0], Version)
	}
	payload := text[1:]
	data, err := decodeBase64(payload)
	if err != nil {
		return nil, fmt.Errorf("decode session: %w", err)
	}
	if len(data) < 1 {
		return nil, errors.New("session payload is empty")
	}

	result := &StringSession{DC: int(data[0])}
	offset := 1
	switch {
	case len(payload) == 352:
		if len(data) < offset+4 {
			return nil, errors.New("session payload is truncated before its address")
		}
		result.Address = net.IP(data[offset : offset+4]).String()
		offset += 4
	default:
		if len(data) < offset+2 {
			return nil, errors.New("session payload is truncated before its address length")
		}
		addressLength := int(int16(binary.BigEndian.Uint16(data[offset : offset+2])))
		if addressLength > 100 {
			if len(data) < offset+16 {
				return nil, errors.New("session payload is truncated before its IPv6 address")
			}
			result.Address = net.IP(data[offset : offset+16]).String()
			offset += 16
			break
		}
		if addressLength < 0 {
			return nil, fmt.Errorf("session address length is negative: %d", addressLength)
		}
		offset += 2
		if len(data) < offset+addressLength {
			return nil, errors.New("session payload is truncated inside its address")
		}
		result.Address = string(data[offset : offset+addressLength])
		offset += addressLength
	}

	if len(data) < offset+2 {
		return nil, errors.New("session payload is truncated before its port")
	}
	result.Port = int(binary.BigEndian.Uint16(data[offset : offset+2]))
	offset += 2

	key := data[offset:]
	if len(key) != AuthKeyLength {
		return nil, fmt.Errorf("session auth key is %d bytes, expected %d", len(key), AuthKeyLength)
	}
	result.AuthKey = append([]byte(nil), key...)

	if result.Address == "" {
		return nil, errors.New("session has no server address")
	}
	if result.Port <= 0 || result.Port > 65535 {
		return nil, fmt.Errorf("session port is out of range: %d", result.Port)
	}
	return result, nil
}

// Data 把解码后的会话转换成 gotd 的会话数据。config 和 server salt
// 不在 StringSession 里，保持零值；gotd 第一次连接时会重新获取这两项。
func (s *StringSession) Data() *gotdsession.Data {
	var key gotdcrypto.Key
	copy(key[:], s.AuthKey)
	id := key.WithID().ID
	return &gotdsession.Data{
		DC:        s.DC,
		Addr:      net.JoinHostPort(s.Address, strconv.Itoa(s.Port)),
		AuthKey:   key[:],
		AuthKeyID: id[:],
	}
}

// Import 解码 gramjs StringSession 并存进 gotd 的会话存储。
// 存储里已经有会话的话，除非设置了 overwrite，否则不动它。
func Import(ctx context.Context, storage gotdsession.Storage, text string, overwrite bool) (*StringSession, error) {
	parsed, err := ParseStringSession(text)
	if err != nil {
		return nil, err
	}
	loader := gotdsession.Loader{Storage: storage}
	if !overwrite {
		switch _, err := loader.Load(ctx); {
		case err == nil:
			return parsed, nil
		case errors.Is(err, gotdsession.ErrNotFound):
		default:
			return nil, fmt.Errorf("inspect existing session: %w", err)
		}
	}
	if err := loader.Save(ctx, parsed.Data()); err != nil {
		return nil, fmt.Errorf("store session: %w", err)
	}
	return parsed, nil
}

// decodeBase64 接受 Node 的 Buffer.from(value, "base64") 能接受的输入。
func decodeBase64(text string) ([]byte, error) {
	encodings := []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	}
	var lastErr error
	for _, encoding := range encodings {
		data, err := encoding.DecodeString(text)
		if err == nil {
			return data, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

const maxAddressLength = 100

// Encode 按 teleproto 的 save() 生成的布局写出 StringSession。
func (s *StringSession) Encode() (string, error) {
	if s.DC <= 0 || s.DC > 255 {
		return "", fmt.Errorf("session DC is out of range: %d", s.DC)
	}
	if s.Address == "" {
		return "", errors.New("session has no server address")
	}
	if len(s.Address) > maxAddressLength {
		return "", fmt.Errorf("session address is %d bytes, more than the reader's %d", len(s.Address), maxAddressLength)
	}
	if s.Port <= 0 || s.Port > 65535 {
		return "", fmt.Errorf("session port is out of range: %d", s.Port)
	}
	if len(s.AuthKey) != AuthKeyLength {
		return "", fmt.Errorf("session auth key is %d bytes, expected %d", len(s.AuthKey), AuthKeyLength)
	}

	payload := make([]byte, 0, 1+2+len(s.Address)+2+AuthKeyLength)
	payload = append(payload, byte(s.DC))
	payload = binary.BigEndian.AppendUint16(payload, uint16(len(s.Address)))
	payload = append(payload, s.Address...)
	payload = binary.BigEndian.AppendUint16(payload, uint16(s.Port))
	payload = append(payload, s.AuthKey...)
	text := string(Version) + base64.StdEncoding.EncodeToString(payload)

	parsed, err := ParseStringSession(text)
	if err != nil {
		return "", fmt.Errorf("encoded session does not read back: %w", err)
	}
	if parsed.DC != s.DC || parsed.Address != s.Address || parsed.Port != s.Port || !bytes.Equal(parsed.AuthKey, s.AuthKey) {
		return "", errors.New("encoded session reads back as a different session")
	}
	return text, nil
}

// FromData 是 Data 的逆操作。
func FromData(data *gotdsession.Data) (*StringSession, error) {
	if data == nil {
		return nil, errors.New("no session data")
	}
	address, err := ResolveAddress(data)
	if err != nil {
		return nil, err
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("session address %q is not host:port: %w", address, err)
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		return nil, fmt.Errorf("session port %q is not a number", port)
	}
	return &StringSession{DC: data.DC, Address: host, Port: number, AuthKey: append([]byte(nil), data.AuthKey...)}, nil
}

// ResolveAddress 查出会话所在数据中心的 host:port。
//
// gotd 自己创建的会话不带地址：它靠数据中心 id 加上一起保存的 config
// 重新连接，从来不需要把地址记下来——只有导入其他客户端格式的代码
// 才会填这个字段。gramjs StringSession 里没有放 config 的地方，地址是
// 字符串的一部分，所以导出时必须把地址查出来：先查会话自带的 config，
// 查不到再查公开的生产环境地址列表。
//
// 这个方向以前从没实际跑过。之前所有会话都来自 gramjs 字符串，由 Data()
// 自己把地址填进去，所以导出路径只在本来就有地址的数据上跑过。
func ResolveAddress(data *gotdsession.Data) (string, error) {
	if data.Addr != "" {
		return data.Addr, nil
	}
	if address, ok := pickOption(data.Config.DCOptions, data.DC); ok {
		return address, nil
	}
	if address, ok := pickOption(dcs.Prod().Options, data.DC); ok {
		return address, nil
	}
	return "", fmt.Errorf("no address is known for data centre %d", data.DC)
}

// pickOption 选出普通客户端会连接的地址：不选仅限媒体、CDN 或仅限混淆
// 的端点。两种地址都有时优先 IPv4，因为这个格式的读取方见到的一直是 IPv4。
func pickOption(options []tg.DCOption, dc int) (string, bool) {
	fallback := ""
	for _, option := range options {
		if option.ID != dc || option.MediaOnly || option.CDN || option.TCPObfuscatedOnly {
			continue
		}
		address := net.JoinHostPort(option.IPAddress, strconv.Itoa(option.Port))
		if strings.Contains(option.IPAddress, ":") {
			if fallback == "" {
				fallback = address
			}
			continue
		}
		return address, true
	}
	return fallback, fallback != ""
}
