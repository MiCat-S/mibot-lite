package whois

import (
	"strings"
	"testing"
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
