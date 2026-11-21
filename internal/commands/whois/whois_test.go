package whois

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MiCat-S/mibot-lite/internal/httpx"
	"github.com/MiCat-S/mibot-lite/internal/store"
)

func TestNormalizeDomain(t *testing.T) {
	for input, want := range map[string]string{
		"https://www.Example.com/path": "example.com",
		"github.com":                   "github.com",
		"a.b.co.uk":                    "a.b.co.uk",
	} {
		got, ok := normalizeDomain(input)
		if !ok || got != want {
			t.Errorf("%s -> %q (%v), want %q", input, got, ok, want)
		}
	}
	for _, input := range []string{"", "not a domain", "localhost", "-bad.com", strings.Repeat("a", 260) + ".com"} {
		if _, ok := normalizeDomain(input); ok {
			t.Errorf("%q should be rejected", input)
		}
	}
}

func TestWhoisReportAlreadyExpiredWithinOneDay(t *testing.T) {
	expired := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	pages := whoisReport("example.com", "Registry Expiry Date: "+expired)
	if !strings.Contains(pages[0], "已过期") {
		t.Errorf("summary reports an expired domain as current: %s", pages[0])
	}
}

func TestWhoisQueryReturnsUpstreamStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("query"); got != "example.com" {
			t.Errorf("query = %q", got)
		}
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	previous := whoisURL
	whoisURL = server.URL
	t.Cleanup(func() { whoisURL = previous })
	data := store.New(filepath.Join(t.TempDir(), "whois.json"), func() whoisData { return whoisData{} })
	result, err := whoisQuery(t.Context(), nil, "example.com", data)
	var status *httpx.StatusError
	if result != "" || !errors.As(err, &status) || status.Status != http.StatusTooManyRequests {
		t.Fatalf("result = %q, error = %v; want HTTP 429", result, err)
	}
}

func TestWhoisReportExtractsFields(t *testing.T) {
	raw := "Domain Name: EXAMPLE.COM\nRegistrar: Example Registrar\nCreation Date: 1995-08-14T04:00:00Z\n" +
		"Registry Expiry Date: 2030-08-13T04:00:00Z\nName Server: A.IANA-SERVERS.NET\nName Server: A.IANA-SERVERS.NET\nName Server: B.IANA-SERVERS.NET\n"
	pages := whoisReport("example.com", raw)
	head := pages[0]
	for _, want := range []string{"Example Registrar", "1995-08-14", "A.IANA-SERVERS.NET", "B.IANA-SERVERS.NET"} {
		if !strings.Contains(head, want) {
			t.Errorf("summary is missing %q:\n%s", want, head)
		}
	}
	if strings.Count(head, "A.IANA-SERVERS.NET") != 1 {
		t.Error("duplicate name servers should be collapsed")
	}
	if !strings.HasPrefix(head, "<pre>") || !strings.HasSuffix(pages[len(pages)-1], "</blockquote>") {
		t.Error("summary and raw record should be wrapped")
	}
}
