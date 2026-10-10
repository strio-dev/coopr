package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"go.podman.io/common/libimage"
)

func TestHistoryOutputFormats(t *testing.T) {
	created := time.Now().Add(-48 * time.Hour)
	entry := libimage.ImageHistory{ID: strings.Repeat("a", 64), Created: &created, CreatedBy: strings.Repeat("x", 60), Size: 1234, Comment: "example"}
	for _, test := range []struct {
		name, format   string
		quiet, noTrunc bool
		want           []string
		absent         []string
	}{
		{name: "default", want: []string{"ID", "CREATED BY", "2 days ago", "1.23kB", strings.Repeat("x", 42) + "..."}, absent: []string{strings.Repeat("a", 64)}},
		{name: "quiet", quiet: true, want: []string{strings.Repeat("a", 12) + "\n"}, absent: []string{"CREATED", "example"}},
		{name: "full", quiet: true, noTrunc: true, want: []string{entry.ID + "\n"}},
		{name: "table", format: "table {{.ID}}\t{{.Size}}", want: []string{"ID", "SIZE", "1.23kB"}},
		{name: "template", format: "{{.CreatedSince}}|{{.CreatedAt}}|{{.Size}}", want: []string{"2 days ago|", created.Format(time.RFC3339), "|1.23kB"}, absent: []string{"CREATED"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out bytes.Buffer
			if err := writeImageHistory(&out, []libimage.ImageHistory{entry}, test.format, test.quiet, test.noTrunc, true); err != nil {
				t.Fatal(err)
			}
			for _, value := range test.want {
				if !strings.Contains(out.String(), value) {
					t.Errorf("output %q lacks %q", out.String(), value)
				}
			}
			for _, value := range test.absent {
				if strings.Contains(out.String(), value) {
					t.Errorf("output %q contains %q", out.String(), value)
				}
			}
		})
	}
}

func TestHistoryOutputPropagatesWriterFailure(t *testing.T) {
	want := errors.New("history output failed")
	if err := writeImageHistory(&maintenanceFailingWriter{err: want}, []libimage.ImageHistory{{ID: "image"}}, "", false, false, true); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
}

func TestHistoryNonHumanDates(t *testing.T) {
	created := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	var out bytes.Buffer
	if err := writeImageHistory(&out, []libimage.ImageHistory{{ID: "image", Created: &created}}, "", false, false, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), created.Format(time.RFC3339)) || strings.Contains(out.String(), " ago") {
		t.Fatalf("non-human history=%q", &out)
	}
}
