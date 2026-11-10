package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestServiceFlagsRejectInvalidInputs(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		message string
	}{
		{[]string{"system", "service"}, "--socket is required"},
		{[]string{"system", "service", "--socket", "relative"}, "socket path must be absolute"},
		{[]string{"system", "service", "--socket", "/tmp/unused.sock", "--parent-fd", "2"}, "descriptor >= 3"},
		{[]string{"system", "service", "--socket", "/tmp/unused.sock", "--parent-fd", "999999"}, "bad file descriptor"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(tc.args, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), tc.message) {
			t.Fatalf("args=%v code=%d stderr=%q", tc.args, code, &stderr)
		}
	}
}
