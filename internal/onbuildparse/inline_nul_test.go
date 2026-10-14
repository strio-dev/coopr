package onbuildparse

import "testing"

func TestExternalOnBuildInlineNULBody(t *testing.T) {
	for _, kind := range []string{"COPY", "ADD"} {
		got, err := ParseDeferred(kind + " <<name /out/\nnul\x00exact\rafter\nname")
		if err != nil {
			t.Fatal(err)
		}
		if got[0].InlineFiles[0].Data != "nul\x00exact\rafter\n" {
			t.Fatalf("data = %q", got[0].InlineFiles[0].Data)
		}
	}
	for _, raw := range []string{"RUN true\x00", "COPY source\x00 /out/", "COPY <<bad\x00name /out/\ndata\nbad\x00name", "COPY <<name /out\x00/\ndata\nname", "COPY --chown=1\x00:2 <<name /out/\ndata\nname"} {
		if _, err := ParseDeferred(raw); err == nil {
			t.Errorf("accepted NUL header: %q", raw)
		}
	}
}
