package project

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// ErrNoRegion is the error of ResolveRegion when nothing sets a region.
var ErrNoRegion = errors.New("no AWS region: set AWS_REGION or a region in your AWS profile")

// ResolveRegion returns the AWS region, as the AWS CLI would with Prosa's
// own override first: PROSA_AWS_REGION, AWS_REGION, AWS_DEFAULT_REGION, then
// the region of the profile AWS_PROFILE (default "default") in the file
// AWS_CONFIG_FILE (default ~/.aws/config). getenv reads the environment.
// A missing config file is the same as one without a region.
func ResolveRegion(getenv func(string) string) (string, error) {
	if r := firstSet(getenv("PROSA_AWS_REGION"), getenv("AWS_REGION"), getenv("AWS_DEFAULT_REGION")); r != "" {
		return r, nil
	}
	profile := firstSet(getenv("AWS_PROFILE"), "default")
	path := getenv("AWS_CONFIG_FILE")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", ErrNoRegion
		}
		path = filepath.Join(home, ".aws", "config")
	}
	if r := profileRegion(path, profile); r != "" {
		return r, nil
	}
	return "", ErrNoRegion
}

// profileRegion reads the region of profile in the AWS config file at path;
// "" when the file, the profile or the region is missing. The section is
// [default] or [profile <name>].
func profileRegion(path, profile string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	in := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "" || line[0] == '#' || line[0] == ';':
		case line[0] == '[' && strings.HasSuffix(line, "]"):
			name := strings.TrimSpace(line[1 : len(line)-1])
			name = strings.TrimSpace(strings.TrimPrefix(name, "profile "))
			in = name == profile
		case in:
			if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == "region" {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

// firstSet returns the first non-empty value.
func firstSet(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
