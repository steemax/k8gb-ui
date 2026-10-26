package main

import (
	"context"
	"strings"
	"testing"
)

func TestFlagsFailBeforeClusterConfiguration(t *testing.T) {
	for _, test := range []struct {
		args      []string
		errorText string
	}{
		{[]string{}, "--cluster-id is required"},
		{[]string{"--cluster-id=test"}, "--token-file is required"},
		{[]string{"--cluster-id=test", "--enable-native-site-traffic"}, "--enable-native-site-traffic requires --enable-writes"},
		{[]string{"--cluster-id=test", "--audit-configmap=INVALID"}, "--audit-configmap"},
		{[]string{"--cluster-id=test", "--audit-timeout=0s"}, "--audit-timeout"},
		{[]string{"--cluster-id=test", "--insecure-local", "--listen=:8081"}, "loopback"},
		{[]string{"--cluster-id=test", "--insecure-local", "--token-file=/never-read"}, "mutually exclusive"},
		{[]string{"--cluster-id=test", "--token-file=/nonexistent-test-only-token-file"}, "read bearer token"},
		{[]string{"extra"}, "unexpected positional"},
	} {
		if err := run(context.Background(), test.args); err == nil || !strings.Contains(err.Error(), test.errorText) {
			t.Errorf("%v: %v", test.args, err)
		}
	}
}

func TestHelpNeedsNoKubernetesAccess(t *testing.T) {
	if err := run(context.Background(), []string{"--help"}); err != nil {
		t.Fatal(err)
	}
}
