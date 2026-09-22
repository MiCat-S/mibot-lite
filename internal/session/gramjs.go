// Package session converts a gramjs/teleproto StringSession (what MiBox's
// Node runtime stores in config.json) into gotd's session storage, and back.
//
// The point is that an account that already signed in under MiBox keeps its
// session: point mibot-lite at the same directory and it connects without
// asking for a code.
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

// AuthKeyLength is the fixed size of a Telegram auth key.
const AuthKeyLength = 256

// Version is the only StringSession version teleproto writes or accepts.
const Version = '1'

// StringSession is a decoded gramjs session.
type StringSession struct {
	DC      int
	Address string
	Port    int
	AuthKey []byte
}

// ParseStringSession decodes a gramjs/teleproto StringSession.
//
// Layout written by teleproto's save():
//
//	| 1     | byte    | DC id                     |
//	| 2     | int16BE | server address length     |
//	| n     | bytes   | server address, as text   |
//	| 2     | int16BE | port                      |
//	| 256   | bytes   | auth key                  |
//
// Two other layouts are accepted, matching teleproto's reader: a Telethon
// session (base64 payload of exactly 352 characters, raw 4-byte IPv4) and a
// raw 16-byte IPv6 address when the length field reads above 100.
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

// Data converts the decoded session into gotd's session data. The config and
// server salt are not part of a StringSession and stay zero; gotd refetches
// both on the first connection.
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

// Import decodes a gramjs StringSession and stores it in gotd's session
// storage. A storage that already holds a session is left untouched unless
// overwrite is set.
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

// decodeBase64 accepts what Node's Buffer.from(value, "base64") accepts.
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

// Encode writes the StringSession layout teleproto's save() produces.
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

// FromData is the inverse of Data.
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

// ResolveAddress finds the host:port for a session's data centre.
//
// A session gotd created itself carries no address: it reconnects from the
// data-centre id plus the config it keeps alongside, and never needs one
// written down — only the importers for other clients' formats fill the
// field. A gramjs StringSession has no room for a config, the address is
// part of the string, so exporting one has to look the address up: from
// the config the session already carries, and failing that from the
// published production list.
//
// This is the direction that had never run. Every session until now came
// from a gramjs string, where Data() puts the address there itself, so the
// export path was only ever exercised on data that already had one.
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

// pickOption chooses the address an ordinary client connects to: not a
// media-only, CDN or obfuscated-only endpoint. IPv4 wins when both are
// offered, because it is what every reader of this format has seen.
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
