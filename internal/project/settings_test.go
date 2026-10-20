package project

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/giammbo/prosa/internal/engine"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// awsConfig writes an AWS CLI config file and returns its path.
func awsConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	content := `# comment
[default]
region = eu-south-1
output = json

[profile staging]
region=eu-west-1

[profile noregion]
output = json

[sso-session corp]
region = ap-southeast-2
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveRegionOrder(t *testing.T) {
	cfg := awsConfig(t)
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"PROSA_AWS_REGION first", map[string]string{"PROSA_AWS_REGION": "us-east-1", "AWS_REGION": "us-east-2", "AWS_DEFAULT_REGION": "us-west-1", "AWS_PROFILE": "staging", "AWS_CONFIG_FILE": cfg}, "us-east-1"},
		{"AWS_REGION second", map[string]string{"AWS_REGION": "us-east-2", "AWS_DEFAULT_REGION": "us-west-1", "AWS_PROFILE": "staging", "AWS_CONFIG_FILE": cfg}, "us-east-2"},
		{"AWS_DEFAULT_REGION third", map[string]string{"AWS_DEFAULT_REGION": "us-west-1", "AWS_PROFILE": "staging", "AWS_CONFIG_FILE": cfg}, "us-west-1"},
		{"AWS_PROFILE profile fourth", map[string]string{"AWS_PROFILE": "staging", "AWS_CONFIG_FILE": cfg}, "eu-west-1"},
		{"default profile last", map[string]string{"AWS_CONFIG_FILE": cfg}, "eu-south-1"},
	} {
		got, err := ResolveRegion(env(tc.env))
		if err != nil || got != tc.want {
			t.Errorf("%s: region = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
}

func TestResolveRegionMissing(t *testing.T) {
	cfg := awsConfig(t)
	missing := filepath.Join(t.TempDir(), "absent")
	const want = "no AWS region: set AWS_REGION or a region in your AWS profile"
	for name, e := range map[string]map[string]string{
		"profile without region": {"AWS_PROFILE": "noregion", "AWS_CONFIG_FILE": cfg},
		"unknown profile":        {"AWS_PROFILE": "ghost", "AWS_CONFIG_FILE": cfg},
		"no config file":         {"AWS_CONFIG_FILE": missing},
	} {
		_, err := ResolveRegion(env(e))
		if err == nil || err.Error() != want {
			t.Errorf("%s: err = %v, want %q", name, err, want)
		}
		if !errorsIsNoRegion(err) {
			t.Errorf("%s: err does not wrap ErrNoRegion", name)
		}
	}
}

func TestSettingsEndpoint(t *testing.T) {
	cfg := awsConfig(t)
	got, err := Settings(env(map[string]string{"AWS_CONFIG_FILE": cfg}))
	if err != nil {
		t.Fatal(err)
	}
	if want := (engine.AWSSettings{Region: "eu-south-1"}); !reflect.DeepEqual(got, want) {
		t.Errorf("settings = %+v, want %+v", got, want)
	}
	got, err = Settings(env(map[string]string{"AWS_CONFIG_FILE": cfg, "PROSA_AWS_REGION": "us-east-1", "PROSA_AWS_ENDPOINT": "http://localhost:5555"}))
	if err != nil {
		t.Fatal(err)
	}
	if want := (engine.AWSSettings{Region: "us-east-1", Endpoint: "http://localhost:5555"}); !reflect.DeepEqual(got, want) {
		t.Errorf("settings = %+v, want %+v", got, want)
	}
	if _, err := Settings(env(map[string]string{"AWS_CONFIG_FILE": filepath.Join(t.TempDir(), "absent")})); !errorsIsNoRegion(err) {
		t.Errorf("no region: err = %v, want ErrNoRegion", err)
	}
}

func errorsIsNoRegion(err error) bool { return errors.Is(err, ErrNoRegion) }
