// Package checks runs the checks compiled from the intents' prose (ADR-021):
// an attribute of a refreshed resource, an HTTPS endpoint, a TLS certificate,
// or a sentence that cannot be verified. Results are pass, fail or skipped,
// with a reason; they never carry an attribute value, since they go to the
// event log (ADR-023).
package checks

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/giammbo/prosa/internal/lock"
)

// The statuses of a Result.
const (
	Pass    = "pass"
	Fail    = "fail"
	Skipped = "skipped"
)

// The check types (lock.Check.Type).
const (
	ResourceAttr = "resource_attr"
	HTTPGet      = "http_get"
	TLSDays      = "tls_days"
	Unverifiable = "unverifiable"
)

// Defaults of the network checks.
const (
	DefaultStatus        = http.StatusOK
	DefaultWithin        = "60s"
	defaultRetryInterval = 2 * time.Second
	requestTimeout       = 10 * time.Second
	tlsTimeout           = 10 * time.Second
)

// Result is the outcome of one check: Name is the check's sentence, Status
// one of Pass, Fail and Skipped, Detail why it failed or was skipped. Detail
// never holds a value read from a resource, nor a provider message.
type Result struct {
	Name, Status, Detail string
}

// StateLookup returns the refreshed state of a resource, "<intent>/<key>":
// a null value (or cty.NilVal) when it does not exist, an error when it
// cannot be read.
type StateLookup func(resource string) (cty.Value, error)

// Options are the settings of a Run.
type Options struct {
	// Emulated reports an emulated cloud endpoint (PROSA_AWS_ENDPOINT):
	// the network checks are skipped, since the names they reach are not
	// served by the emulator (ADR-013).
	Emulated bool
	// Now is the clock of within and tls_days; time.Now when nil.
	Now func() time.Time
	// HTTPClient makes the requests of http_get; nil is a client with a
	// 10s timeout.
	HTTPClient *http.Client
	// RetryInterval is the pause between http_get attempts; 0 is 2s.
	RetryInterval time.Duration
	// TLSConfig is the base configuration of tls_days (nil: system roots);
	// its ServerName is always the check's host.
	TLSConfig *tls.Config
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// Ops lists the operators of resource_attr.
var Ops = []string{"eq", "ne", "gte", "lte", "contains", "matches", "within", "exists"}

// Run runs defs in order, reading resources with state, and returns one
// result per check.
func Run(ctx context.Context, defs []lock.Check, state StateLookup, opts Options) []Result {
	out := make([]Result, len(defs))
	for i, d := range defs {
		r := run(ctx, d, state, opts)
		r.Name = d.Name
		out[i] = r
	}
	return out
}

func run(ctx context.Context, d lock.Check, state StateLookup, opts Options) Result {
	switch d.Type {
	case ResourceAttr:
		return resourceAttr(d, state, opts)
	case HTTPGet:
		if opts.Emulated {
			return skipped("emulated endpoint")
		}
		return httpGet(ctx, d, opts)
	case TLSDays:
		if opts.Emulated {
			return skipped("emulated endpoint")
		}
		return tlsDays(ctx, d, opts)
	case Unverifiable:
		return skipped(d.Reason)
	}
	return failf("unknown check type %q", d.Type)
}

func pass() Result              { return Result{Status: Pass} }
func skipped(why string) Result { return Result{Status: Skipped, Detail: why} }
func failf(format string, args ...any) Result {
	return Result{Status: Fail, Detail: fmt.Sprintf(format, args...)}
}

// resourceAttr compares the attribute d.Path of the refreshed d.Resource.
func resourceAttr(d lock.Check, state StateLookup, opts Options) Result {
	v, err := state(d.Resource)
	if err != nil {
		// The provider's message may quote the resource's values: not in a
		// result, which the event log keeps (ADR-023).
		return failf("read %s: provider error", d.Resource)
	}
	if v == cty.NilVal || v.IsNull() {
		return failf("%s does not exist", d.Resource)
	}
	v, err = Resolve(v, d.Path)
	if err != nil {
		return failf("%v", err)
	}
	if d.Op == "exists" {
		if !isSet(v) {
			return failf("%s is not set", d.Path)
		}
		return pass()
	}
	if v.IsNull() || !v.IsKnown() {
		return failf("%s is not set", d.Path)
	}
	ty := v.Type()
	switch d.Op {
	case "eq", "ne":
		want, err := ConvertValue(d.Value, ty)
		if err != nil {
			return failf("value %s does not fit %s, a %s", show(d.Value), d.Path, ty.FriendlyName())
		}
		eq := v.Equals(want)
		switch {
		case d.Op == "eq" && !eq.True():
			return failf("%s does not equal %s", d.Path, show(d.Value))
		case d.Op == "ne" && eq.True():
			return failf("%s equals %s", d.Path, show(d.Value))
		}
		return pass()
	case "gte", "lte":
		want, err := ConvertValue(d.Value, cty.Number)
		if err != nil || !ty.Equals(cty.Number) {
			return failf("%s and %s are not both numbers", d.Path, show(d.Value))
		}
		if d.Op == "gte" && v.LessThan(want).True() {
			return failf("%s is less than %s", d.Path, show(d.Value))
		}
		if d.Op == "lte" && v.GreaterThan(want).True() {
			return failf("%s is greater than %s", d.Path, show(d.Value))
		}
		return pass()
	case "contains":
		if !contains(v, d.Value) {
			return failf("%s does not contain %s", d.Path, show(d.Value))
		}
		return pass()
	case "matches":
		re, err := regexp.Compile(fmt.Sprint(d.Value))
		if err != nil || !ty.Equals(cty.String) {
			return failf("%s is not a string or %s is not a regular expression", d.Path, show(d.Value))
		}
		if !re.MatchString(v.AsString()) {
			return failf("%s does not match %s", d.Path, show(d.Value))
		}
		return pass()
	case "within":
		return within(d, v, opts)
	}
	return failf("unknown operator %q", d.Op)
}

// within checks that the timestamp v is at most d.Value (a Go duration) old.
func within(d lock.Check, v cty.Value, opts Options) Result {
	s, _ := d.Value.(string)
	limit, err := time.ParseDuration(s)
	if err != nil {
		return failf("value %s is not a duration", show(d.Value))
	}
	if !v.Type().Equals(cty.String) {
		return failf("%s is not a timestamp", d.Path)
	}
	if v.AsString() == "" {
		return failf("%s is not set", d.Path)
	}
	ts, err := time.Parse(time.RFC3339, v.AsString())
	if err != nil {
		return failf("%s is not an RFC 3339 timestamp", d.Path)
	}
	if opts.now().Sub(ts) > limit {
		return failf("%s is older than %s", d.Path, s)
	}
	return pass()
}

// isSet reports whether v is a known value that is not null, not an empty
// string and not an empty collection.
func isSet(v cty.Value) bool {
	if v.IsNull() || !v.IsKnown() {
		return false
	}
	ty := v.Type()
	switch {
	case ty.Equals(cty.String):
		return v.AsString() != ""
	case ty.IsListType() || ty.IsSetType() || ty.IsMapType() || ty.IsTupleType():
		return v.LengthInt() > 0
	}
	return true
}

// contains reports whether the string v contains the string raw, or the
// collection v has an element equal to raw.
func contains(v cty.Value, raw any) bool {
	ty := v.Type()
	switch {
	case ty.Equals(cty.String):
		s, ok := raw.(string)
		return ok && strings.Contains(v.AsString(), s)
	case ty.IsListType() || ty.IsSetType() || ty.IsTupleType():
		for it := v.ElementIterator(); it.Next(); {
			_, e := it.Element()
			if e.IsNull() || !e.IsKnown() {
				continue
			}
			if want, err := ConvertValue(raw, e.Type()); err == nil && e.Equals(want).True() {
				return true
			}
		}
	}
	return false
}

// Resolve selects the dotted path in v: object attributes, map keys, list
// and tuple indexes. A missing map key or a null on the way yields a null
// value; an attribute v's type does not have, or an index past the end, is
// an error.
func Resolve(v cty.Value, path string) (cty.Value, error) {
	parts := strings.Split(path, ".")
	for i, a := range parts {
		at := strings.Join(parts[:i], ".")
		if v.IsNull() || !v.IsKnown() {
			return cty.NullVal(cty.DynamicPseudoType), nil
		}
		ty := v.Type()
		switch {
		case ty.IsObjectType():
			if !ty.HasAttribute(a) {
				if at == "" {
					return cty.NilVal, fmt.Errorf("no attribute %q", a)
				}
				return cty.NilVal, fmt.Errorf("%s has no attribute %q", at, a)
			}
			v = v.GetAttr(a)
		case ty.IsMapType():
			k := cty.StringVal(a)
			if !v.HasIndex(k).True() {
				return cty.NullVal(ty.ElementType()), nil
			}
			v = v.Index(k)
		case ty.IsListType() || ty.IsTupleType():
			n, err := strconv.Atoi(a)
			if err != nil || n < 0 {
				return cty.NilVal, fmt.Errorf("%s is a list: %q is not an index", at, a)
			}
			if n >= v.LengthInt() {
				return cty.NilVal, fmt.Errorf("%s: no element %d", at, n)
			}
			v = v.Index(cty.NumberIntVal(int64(n)))
		default:
			return cty.NilVal, fmt.Errorf("cannot select %q in %s, a %s", a, at, ty.FriendlyName())
		}
	}
	return v, nil
}

// ConvertValue converts raw, a plain JSON-like value (numbers as
// json.Number or Go numbers), to type ty; with cty.DynamicPseudoType, to the
// type its JSON form implies.
func ConvertValue(raw any, ty cty.Type) (cty.Value, error) {
	data, err := json.Marshal(raw)
	if err != nil {
		return cty.NilVal, err
	}
	if ty.Equals(cty.DynamicPseudoType) {
		if ty, err = ctyjson.ImpliedType(data); err != nil {
			return cty.NilVal, err
		}
	}
	return ctyjson.Unmarshal(data, ty)
}

// show renders a check's value as JSON, for a detail.
func show(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Sprint(v)
	}
	return strings.TrimSpace(b.String())
}

// httpGet asks d.URL, without following redirects, until it answers
// d.Status (DefaultStatus when 0) or d.Within (DefaultWithin when empty) has
// passed.
func httpGet(ctx context.Context, d lock.Check, opts Options) Result {
	want := d.Status
	if want == 0 {
		want = DefaultStatus
	}
	ws := d.Within
	if ws == "" {
		ws = DefaultWithin
	}
	limit, err := time.ParseDuration(ws)
	if err != nil {
		return failf("within %q is not a duration", ws)
	}
	client := &http.Client{Timeout: requestTimeout}
	if opts.HTTPClient != nil {
		c := *opts.HTTPClient // the caller's client stays as it is
		client = &c
	}
	// A redirect is an answer: its status is compared, never followed.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	interval := opts.RetryInterval
	if interval <= 0 {
		interval = defaultRetryInterval
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	var last string
	for {
		got, err := get(ctx, client, d.URL)
		switch {
		case err != nil && ctx.Err() != nil && last != "":
			// cut by the deadline: the previous answer says more
		case err != nil:
			last = err.Error()
		case got == want:
			return pass()
		default:
			last = fmt.Sprintf("last answer %d", got)
		}
		select {
		case <-ctx.Done():
			return failf("no %d within %s: %s", want, ws, last)
		case <-time.After(interval):
		}
	}
}

// get returns the status code of a GET of url.
func get(ctx context.Context, client *http.Client, url string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, nil
}

// tlsDays connects to d.Host (port 443 unless it names one), verifies its
// certificate and checks that it is valid for at least d.MinDays more days.
func tlsDays(ctx context.Context, d lock.Check, opts Options) Result {
	host, port := d.Host, "443"
	if h, p, err := net.SplitHostPort(d.Host); err == nil {
		host, port = h, p
	}
	cfg := &tls.Config{}
	if opts.TLSConfig != nil {
		cfg = opts.TLSConfig.Clone()
	}
	cfg.ServerName = host
	ctx, cancel := context.WithTimeout(ctx, tlsTimeout)
	defer cancel()
	dialer := &tls.Dialer{Config: cfg}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return failf("%v", err)
	}
	defer conn.Close()
	certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return failf("%s sent no certificate", d.Host)
	}
	notAfter := certs[0].NotAfter.UTC()
	left := notAfter.Sub(opts.now())
	if left < 0 {
		return failf("the certificate expired on %s", notAfter.Format(time.RFC3339))
	}
	if days := int(left / (24 * time.Hour)); days < d.MinDays {
		return failf("the certificate expires in %d days (%s), want at least %d", days, notAfter.Format(time.RFC3339), d.MinDays)
	}
	return pass()
}
