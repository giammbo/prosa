package provider

import (
	"bytes"
	"strings"
	"testing"

	"github.com/hashicorp/go-hclog"
)

func TestPluginLoggerLevels(t *testing.T) {
	const bin = "terraform-provider-aws"
	for _, tc := range []struct {
		env                        string
		ownWarn, provWarn, provErr bool
	}{
		{"", true, false, true},
		{"warn", true, true, true},
		{"debug", true, true, true},
		{"off", false, false, false},
	} {
		t.Setenv("PROSA_LOG", tc.env)
		var buf bytes.Buffer
		l := newLogger(&buf, "/cache/"+bin)
		l.Warn("own warning")
		prov := l.Named(bin)
		prov.Warn("provider warning")
		prov.Error("provider error")
		l.Named("stdio").Warn("stdio warning")
		out := buf.String()
		for line, want := range map[string]bool{
			"own warning": tc.ownWarn, "stdio warning": tc.ownWarn,
			"provider warning": tc.provWarn, "provider error": tc.provErr,
		} {
			if strings.Contains(out, line) != want {
				t.Errorf("PROSA_LOG=%q: %q logged = %v, want %v\n%s", tc.env, line, !want, want, out)
			}
		}
		if l.GetLevel() == hclog.NoLevel {
			t.Errorf("PROSA_LOG=%q: no level", tc.env)
		}
	}
}

func TestPluginEnv(t *testing.T) {
	host := []string{"PATH=/bin", "TF_LOG_PATH=/tmp/x.log", "TF_LOG_PATH_MASK=/tmp/%s.log", "AWS_ENDPOINT_URL=http://old", "TF_LOG=warn"}
	got := pluginEnv(host, []string{"AWS_ENDPOINT_URL=http://localhost:5555"})
	want := []string{"PATH=/bin", "TF_LOG=warn", "AWS_ENDPOINT_URL=http://localhost:5555"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("pluginEnv = %v, want %v", got, want)
	}
}

func TestPluginEnvUnsetsBareKeys(t *testing.T) {
	host := []string{"PATH=/bin", "AWS_ENDPOINT_URL=http://old", "AWS_ENDPOINT_URL_S3=http://s3"}
	got := pluginEnv(host, []string{"AWS_ENDPOINT_URL", "AWS_ENDPOINT_URL_S3"})
	if want := []string{"PATH=/bin"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("pluginEnv = %v, want %v", got, want)
	}
}
