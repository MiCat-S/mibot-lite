package session

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"net"
	"strings"
	"testing"

	"github.com/gotd/td/tg"
)

func sample() *StringSession {
	key := make([]byte, AuthKeyLength)
	for index := range key {
		key[index] = byte(index)
	}
	return &StringSession{DC: 2, Address: "149.154.167.50", Port: 443, AuthKey: key}
}

func TestRoundTrip(t *testing.T) {
	original := sample()
	encoded, err := original.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(encoded, "1") {
		t.Fatalf("encoded session must start with the version byte, got %q", encoded[:1])
	}
	parsed, err := ParseStringSession(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.DC != original.DC || parsed.Address != original.Address || parsed.Port != original.Port || !bytes.Equal(parsed.AuthKey, original.AuthKey) {
		t.Fatalf("round trip changed the session: %+v", parsed)
	}
}

// A session that came out of gramjs must convert to the same DC and the
// same auth key, byte for byte: this is what lets an existing MiBox
// deployment keep its login.
func TestParseGramjsLayout(t *testing.T) {
	key := bytes.Repeat([]byte{7}, AuthKeyLength)
	address := []byte("91.108.56.130")
	payload := []byte{5}
	payload = binary.BigEndian.AppendUint16(payload, uint16(len(address)))
	payload = append(payload, address...)
	payload = binary.BigEndian.AppendUint16(payload, 443)
	payload = append(payload, key...)
	parsed, err := ParseStringSession("1" + base64.StdEncoding.EncodeToString(payload))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.DC != 5 || parsed.Address != "91.108.56.130" || parsed.Port != 443 || !bytes.Equal(parsed.AuthKey, key) {
		t.Fatalf("unexpected parse: %+v", parsed)
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]string{
		"empty":         "",
		"wrong version": "2abc",
		"not base64":    "1!!!!",
		"short key":     "1" + base64.StdEncoding.EncodeToString([]byte{2, 0, 1, 'a', 1, 187}),
	}
	for name, input := range cases {
		if _, err := ParseStringSession(input); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestDataCarriesKeyAndAddress(t *testing.T) {
	data := sample().Data()
	if data.DC != 2 || data.Addr != "149.154.167.50:443" {
		t.Fatalf("unexpected session data: %+v", data)
	}
	if len(data.AuthKey) != AuthKeyLength || len(data.AuthKeyID) != 8 {
		t.Fatalf("auth key %d bytes, id %d bytes", len(data.AuthKey), len(data.AuthKeyID))
	}
	back, err := FromData(data)
	if err != nil {
		t.Fatal(err)
	}
	if back.Port != 443 || back.Address != "149.154.167.50" {
		t.Fatalf("FromData lost the address: %+v", back)
	}
}

// A session gotd created itself carries no address — it reconnects from
// the data-centre id and its config. Exporting one to the gramjs format,
// which has no room for a config, has to look the address up. This path
// never ran until a real --login produced a session, and it failed with
// `session address "" is not host:port`.
func TestResolveAddressWithoutStoredAddress(t *testing.T) {
	data := sample().Data()
	data.Addr = ""
	address, err := ResolveAddress(data)
	if err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" || port == "" {
		t.Fatalf("resolved %q: %v", address, err)
	}
	// The whole export has to work, not just the lookup.
	parsed, err := FromData(data)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := parsed.Encode()
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseStringSession(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if back.DC != data.DC || !bytes.Equal(back.AuthKey, data.AuthKey) {
		t.Fatalf("round trip changed the session: %+v", back)
	}
}

// The session's own config wins over the published list: a client told to
// use a different address should keep using it.
func TestResolveAddressPrefersSessionConfig(t *testing.T) {
	data := sample().Data()
	data.Addr = ""
	data.Config.DCOptions = []tg.DCOption{
		{ID: data.DC, IPAddress: "10.0.0.7", Port: 8443},
		{ID: data.DC, IPAddress: "10.0.0.8", Port: 8443, MediaOnly: true},
	}
	address, err := ResolveAddress(data)
	if err != nil {
		t.Fatal(err)
	}
	if address != "10.0.0.7:8443" {
		t.Fatalf("resolved %q, want the plain option from the session config", address)
	}
}

// Media-only, CDN and obfuscated-only endpoints are not what an ordinary
// client connects to, so they are never chosen.
func TestResolveAddressSkipsSpecialEndpoints(t *testing.T) {
	data := sample().Data()
	data.Addr = ""
	data.Config.DCOptions = []tg.DCOption{
		{ID: data.DC, IPAddress: "10.0.0.1", Port: 443, MediaOnly: true},
		{ID: data.DC, IPAddress: "10.0.0.2", Port: 443, CDN: true},
		{ID: data.DC, IPAddress: "10.0.0.3", Port: 443, TCPObfuscatedOnly: true},
	}
	address, err := ResolveAddress(data)
	if err != nil {
		t.Fatal(err)
	}
	if address == "10.0.0.1:443" || address == "10.0.0.2:443" || address == "10.0.0.3:443" {
		t.Fatalf("picked a special endpoint: %s", address)
	}
}
