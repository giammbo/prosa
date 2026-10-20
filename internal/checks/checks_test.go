package checks

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zclconf/go-cty/cty"

	"github.com/giammbo/prosa/internal/lock"
)

var now = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

// dbState is the refreshed state of app/db in the tests.
var dbState = cty.ObjectVal(map[string]cty.Value{
	"name":        cty.StringVal("staging-main-db"),
	"size":        cty.NumberIntVal(10),
	"encrypted":   cty.True,
	"public":      cty.False,
	"tags":        cty.MapVal(map[string]cty.Value{"env": cty.StringVal("staging")}),
	"zones":       cty.ListVal([]cty.Value{cty.StringVal("a"), cty.StringVal("b")}),
	"groups":      cty.SetVal([]cty.Value{cty.StringVal("sg-1")}),
	"restored_at": cty.StringVal("2026-10-18T02:00:00Z"),
	"empty":       cty.StringVal(""),
	"unset":       cty.NullVal(cty.String),
	"secret": cty.ListVal([]cty.Value{cty.ObjectVal(map[string]cty.Value{
		"arn": cty.StringVal("arn:aws:secretsmanager:eu-south-1:1:secret:db"),
	})}),
})

func lookup(node string) (cty.Value, error) {
	switch node {
	case "app/db":
		return dbState, nil
	case "app/broken":
		return cty.NilVal, errors.New("read failed")
	}
	return cty.NilVal, nil
}

func attr(path, op string, value any) lock.Check {
	return lock.Check{Name: path + " " + op, Type: "resource_attr", Resource: "app/db", Path: path, Op: op, Value: value}
}

func TestResourceAttr(t *testing.T) {
	for _, tc := range []struct {
		ck             lock.Check
		status, detail string
	}{
		{attr("name", "eq", "staging-main-db"), Pass, ""},
		{attr("name", "eq", "other"), Fail, `name does not equal "other"`},
		{attr("size", "eq", json.Number("10")), Pass, ""},
		{attr("size", "eq", 10), Pass, ""},
		{attr("size", "eq", 10.0), Pass, ""},
		{attr("size", "eq", 11), Fail, "size does not equal 11"},
		{attr("encrypted", "eq", true), Pass, ""},
		{attr("public", "eq", false), Pass, ""},
		{attr("public", "eq", true), Fail, "public does not equal true"},
		{attr("public", "ne", true), Pass, ""},
		{attr("public", "ne", false), Fail, "public equals false"},
		{attr("size", "gte", 10), Pass, ""},
		{attr("size", "gte", 11), Fail, "size is less than 11"},
		{attr("size", "lte", 10), Pass, ""},
		{attr("size", "lte", 9), Fail, "size is greater than 9"},
		{attr("name", "contains", "main"), Pass, ""},
		{attr("name", "contains", "prod"), Fail, `name does not contain "prod"`},
		{attr("zones", "contains", "b"), Pass, ""},
		{attr("zones", "contains", "c"), Fail, `zones does not contain "c"`},
		{attr("groups", "contains", "sg-1"), Pass, ""},
		{attr("name", "matches", "^staging-"), Pass, ""},
		{attr("name", "matches", "^prod-"), Fail, `name does not match "^prod-"`},
		{attr("restored_at", "within", "24h"), Pass, ""},
		{attr("restored_at", "within", "1h"), Fail, "restored_at is older than 1h"},
		{attr("empty", "within", "24h"), Fail, "empty is not set"},
		{attr("name", "within", "24h"), Fail, "name is not an RFC 3339 timestamp"},
		{attr("tags.env", "eq", "staging"), Pass, ""},
		{attr("tags.team", "exists", nil), Fail, "tags.team is not set"},
		{attr("secret.0.arn", "exists", nil), Pass, ""},
		{attr("secret.0.arn", "matches", "^arn:aws:secretsmanager:"), Pass, ""},
		{attr("empty", "exists", nil), Fail, "empty is not set"},
		{attr("unset", "exists", nil), Fail, "unset is not set"},
		{attr("unset", "eq", "x"), Fail, "unset is not set"},
		{attr("zones", "exists", nil), Pass, ""},
		{attr("zones", "eq", []any{"a", "b"}), Pass, ""},
		{attr("secret.1.arn", "exists", nil), Fail, `secret: no element 1`},
		{attr("nope", "eq", "x"), Fail, `no attribute "nope"`},
		{attr("size", "eq", "ten"), Fail, `value "ten" does not fit size, a number`},
		{lock.Check{Name: "gone", Type: "resource_attr", Resource: "app/gone", Path: "name", Op: "exists"}, Fail, "app/gone does not exist"},
		{lock.Check{Name: "broken", Type: "resource_attr", Resource: "app/broken", Path: "name", Op: "exists"}, Fail, "read app/broken: provider error"},
		{lock.Check{Name: "later", Type: "unverifiable", Reason: "needs the run history"}, Skipped, "needs the run history"},
		{lock.Check{Name: "odd", Type: "odd"}, Fail, `unknown check type "odd"`},
	} {
		got := Run(context.Background(), []lock.Check{tc.ck}, lookup, Options{Now: func() time.Time { return now }})
		want := Result{Name: tc.ck.Name, Status: tc.status, Detail: tc.detail}
		if len(got) != 1 || got[0] != want {
			t.Errorf("%s: got %+v, want %+v", tc.ck.Name, got, want)
		}
	}
}

func TestRunKeepsOrder(t *testing.T) {
	defs := []lock.Check{attr("name", "exists", nil), {Name: "x", Type: "unverifiable", Reason: "r"}, attr("size", "eq", 1)}
	got := Run(context.Background(), defs, lookup, Options{})
	var names []string
	for _, r := range got {
		names = append(names, r.Name+"="+r.Status)
	}
	if want := "name exists=pass x=skipped size eq=fail"; strings.Join(names, " ") != want {
		t.Errorf("results = %v, want %s", names, want)
	}
}

func TestHTTPGetRetriesUntilStatus(t *testing.T) {
	var calls atomic.Int32
	srv := newTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" || calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	ck := lock.Check{Name: "health", Type: "http_get", URL: srv.URL + "/health", Status: 200, Within: "5s"}
	got := Run(context.Background(), []lock.Check{ck}, lookup, Options{HTTPClient: srv.Client(), RetryInterval: 10 * time.Millisecond})
	if want := (Result{Name: "health", Status: Pass}); got[0] != want {
		t.Errorf("got %+v, want %+v", got[0], want)
	}
	if n := calls.Load(); n != 3 {
		t.Errorf("server called %d times, want 3", n)
	}
}

func TestHTTPGetDoesNotFollowRedirects(t *testing.T) {
	srv := newTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			http.Redirect(w, r, "/login", http.StatusFound)
		}
	}))
	defer srv.Close()
	opts := Options{HTTPClient: srv.Client(), RetryInterval: 20 * time.Millisecond}
	ck := lock.Check{Name: "health", Type: "http_get", URL: srv.URL + "/health", Within: "100ms"}
	got := Run(context.Background(), []lock.Check{ck}, lookup, opts)
	if want := (Result{Name: "health", Status: Fail, Detail: "no 200 within 100ms: last answer 302"}); got[0] != want {
		t.Errorf("redirect: got %+v, want %+v", got[0], want)
	}
	ck.Status = http.StatusFound
	if got := Run(context.Background(), []lock.Check{ck}, lookup, opts); got[0].Status != Pass {
		t.Errorf("status 302 wanted: got %+v, want pass", got[0])
	}
	if srv.Client().CheckRedirect != nil {
		t.Error("the caller's client was modified")
	}
}

func TestHTTPGetDefaultStatusIs200(t *testing.T) {
	srv := newTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	ck := lock.Check{Name: "health", Type: "http_get", URL: srv.URL, Within: "1s"}
	got := Run(context.Background(), []lock.Check{ck}, lookup, Options{HTTPClient: srv.Client()})
	if got[0].Status != Pass {
		t.Errorf("got %+v, want pass", got[0])
	}
}

func TestHTTPGetFailsAfterWithin(t *testing.T) {
	srv := newTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	ck := lock.Check{Name: "health", Type: "http_get", URL: srv.URL, Status: 200, Within: "200ms"}
	start := time.Now()
	got := Run(context.Background(), []lock.Check{ck}, lookup, Options{HTTPClient: srv.Client(), RetryInterval: 20 * time.Millisecond})
	if want := (Result{Name: "health", Status: Fail, Detail: "no 200 within 200ms: last answer 500"}); got[0] != want {
		t.Errorf("got %+v, want %+v", got[0], want)
	}
	if d := time.Since(start); d < 200*time.Millisecond || d > 2*time.Second {
		t.Errorf("took %s, want about 200ms", d)
	}
}

func TestHTTPGetUnreachable(t *testing.T) {
	srv := newTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()
	ck := lock.Check{Name: "health", Type: "http_get", URL: url, Within: "100ms"}
	got := Run(context.Background(), []lock.Check{ck}, lookup, Options{RetryInterval: 20 * time.Millisecond})
	if got[0].Status != Fail || !strings.HasPrefix(got[0].Detail, "no 200 within 100ms: ") {
		t.Errorf("got %+v, want a failure naming the last error", got[0])
	}
}

func TestNetworkChecksSkippedOnEmulatedEndpoint(t *testing.T) {
	defs := []lock.Check{
		{Name: "health", Type: "http_get", URL: "https://staging.acme.example/health", Status: 200, Within: "60s"},
		{Name: "cert", Type: "tls_days", Host: "staging.acme.example", MinDays: 20},
	}
	got := Run(context.Background(), defs, lookup, Options{Emulated: true})
	for i, r := range got {
		if want := (Result{Name: defs[i].Name, Status: Skipped, Detail: "emulated endpoint"}); r != want {
			t.Errorf("got %+v, want %+v", r, want)
		}
	}
}

// newTLSServer is httptest.NewTLSServer without the server's log of failed
// handshakes, which some tests cause on purpose.
func newTLSServer(h http.Handler) *httptest.Server {
	srv := httptest.NewUnstartedServer(h)
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	return srv
}

func tlsServer(t *testing.T) (*httptest.Server, *tls.Config) {
	t.Helper()
	srv := newTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return srv, &tls.Config{RootCAs: pool}
}

func TestTLSDays(t *testing.T) {
	srv, cfg := tlsServer(t)
	host := srv.Listener.Addr().String()
	notAfter := srv.Certificate().NotAfter
	ck := lock.Check{Name: "cert", Type: "tls_days", Host: host, MinDays: 20}

	got := Run(context.Background(), []lock.Check{ck}, lookup, Options{TLSConfig: cfg, Now: func() time.Time { return notAfter.Add(-21 * 24 * time.Hour) }})
	if want := (Result{Name: "cert", Status: Pass}); got[0] != want {
		t.Errorf("21 days left: got %+v, want %+v", got[0], want)
	}
	got = Run(context.Background(), []lock.Check{ck}, lookup, Options{TLSConfig: cfg, Now: func() time.Time { return notAfter.Add(-10 * 24 * time.Hour) }})
	want := Result{Name: "cert", Status: Fail, Detail: "the certificate expires in 10 days (" + notAfter.UTC().Format(time.RFC3339) + "), want at least 20"}
	if got[0] != want {
		t.Errorf("10 days left: got %+v, want %+v", got[0], want)
	}
}

func TestTLSDaysUntrustedCertificateFails(t *testing.T) {
	srv, _ := tlsServer(t)
	ck := lock.Check{Name: "cert", Type: "tls_days", Host: srv.Listener.Addr().String(), MinDays: 20}
	got := Run(context.Background(), []lock.Check{ck}, lookup, Options{})
	if got[0].Status != Fail || !strings.Contains(got[0].Detail, "certificate") {
		t.Errorf("got %+v, want a failed handshake", got[0])
	}
}

func TestConvertValue(t *testing.T) {
	for _, tc := range []struct {
		raw  any
		ty   cty.Type
		want cty.Value
	}{
		{json.Number("2"), cty.Number, cty.NumberIntVal(2)},
		{2, cty.Number, cty.NumberIntVal(2)},
		{false, cty.Bool, cty.False},
		{"x", cty.String, cty.StringVal("x")},
		{[]any{"a"}, cty.List(cty.String), cty.ListVal([]cty.Value{cty.StringVal("a")})},
		{"x", cty.DynamicPseudoType, cty.StringVal("x")},
	} {
		got, err := ConvertValue(tc.raw, tc.ty)
		if err != nil || !got.RawEquals(tc.want) {
			t.Errorf("ConvertValue(%#v, %s) = %#v, %v; want %#v", tc.raw, tc.ty.FriendlyName(), got, err, tc.want)
		}
	}
	if _, err := ConvertValue(map[string]any{"a": 1}, cty.String); err == nil {
		t.Error("an object for a string: want an error")
	}
}
