package buildah

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func progressStreamTestEvents() (OperationProgress, OperationProgress) {
	start := OperationProgress{ID: "phase", Phase: "execute", State: "started", Started: time.Unix(123, 0).UTC()}
	end := start
	completed := start.Started.Add(time.Second)
	end.State, end.Completed, end.DurationNanos = "completed", &completed, int64(time.Second)
	return start, end
}

func TestWorkerProgressStreamDeliversBeforeProducerCompletes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.json")
	received := make(chan OperationProgress, 4)
	receiver, err := newWorkerProgressReceiver(path, func(event OperationProgress) { received <- event })
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = receiver.close() }()
	buffer, writer, err := newWorkerProgressCapture(true, path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.finish()
	release, done := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		<-done
	}()
	start, end := progressStreamTestEvents()
	go func() { defer close(done); buffer.record(start); <-release; buffer.record(end) }()
	select {
	case event := <-received:
		if event.State != "started" {
			t.Fatalf("first event: %#v", event)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("start was not delivered while producer was blocked")
	}
	close(release)
	<-done
	dropped, summary := finishWorkerProgress(buffer, writer)
	if dropped != 0 || len(buffer.events) != 0 {
		t.Fatalf("stream retained events or dropped a phase: events=%d dropped=%d", len(buffer.events), dropped)
	}
	if err := receiver.finish(summary); err != nil {
		t.Fatal(err)
	}
	if summary.Events != 2 || len(received) != 1 || (<-received).State != "completed" {
		t.Fatal("transitions were lost or duplicated")
	}
}

func TestWorkerProgressStreamProducerDoesNotWaitForBlockedObserver(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.json")
	entered, release := make(chan struct{}), make(chan struct{})
	var once, unblockOnce sync.Once
	unblock := func() { unblockOnce.Do(func() { close(release) }) }
	receiver, err := newWorkerProgressReceiver(path, func(OperationProgress) {
		once.Do(func() { close(entered); <-release })
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { unblock(); _ = receiver.close() }()
	buffer, writer, err := newWorkerProgressCapture(true, path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.finish()
	start, end := progressStreamTestEvents()
	buffer.record(start)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("observer did not enter")
	}
	done := make(chan *workerProgressSummary, 1)
	go func() {
		buffer.record(end)
		for i := 1; i < operationProgressPhaseLimit; i++ {
			start.ID = fmt.Sprintf("phase-%d", i)
			end.ID = start.ID
			buffer.record(start)
			buffer.record(end)
		}
		_, summary := finishWorkerProgress(buffer, writer)
		done <- summary
	}()
	var summary *workerProgressSummary
	select {
	case summary = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("producer waited for the blocked observer")
	}
	if summary.Error != "" || summary.Events != 2*operationProgressPhaseLimit {
		t.Fatal("producer failed while observer was blocked", summary)
	}
	unblock()
	if err := receiver.finish(summary); err != nil {
		t.Fatal(err)
	}
}

// Manual receivers make partial-frame/error tests independent of poll timing.
func manualProgressReceiver(t *testing.T) (*workerProgressReceiver, *os.File) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "result.json")
	file, err := openWorkerProgressFile(path, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	writer, err := openWorkerProgressFile(path, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	return &workerProgressReceiver{file: file, sink: func(OperationProgress) {}}, writer
}

func TestWorkerProgressStreamGrowingPartialFrame(t *testing.T) {
	receiver, writer := manualProgressReceiver(t)
	start, _ := progressStreamTestEvents()
	frame, err := json.Marshal(start)
	if err != nil {
		t.Fatal(err)
	}
	split := len(frame) / 2
	if _, err := writer.Write(frame[:split]); err != nil {
		t.Fatal(err)
	}
	if err := receiver.drain(false); err != nil {
		t.Fatal(err)
	}
	if receiver.validator.count != 0 || len(receiver.pending) != split {
		t.Fatal("partial frame was delivered")
	}
	if _, err := writer.Write(append(frame[split:], '\n')); err != nil {
		t.Fatal(err)
	}
	if err := receiver.drain(true); err != nil {
		t.Fatal(err)
	}
	if receiver.validator.count != 1 || len(receiver.pending) != 0 {
		t.Fatal("growing frame was not delivered once")
	}
}

func TestWorkerProgressStreamRejectsCorruption(t *testing.T) {
	start, _ := progressStreamTestEvents()
	encoded, err := json.Marshal(start)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		data []byte
		want string
	}{
		{"truncated", encoded, "incomplete frame"},
		{"malformed", []byte("{\n"), "decode live"},
		{"oversized", bytes.Repeat([]byte("x"), workerProgressFrameLimit), "exceeds bound"},
		{"unknown-field", []byte(`{"unknown":true}` + "\n"), "unknown field"},
		{"duplicate", append(append(append([]byte(nil), encoded...), '\n'), append(encoded, '\n')...), "invalid phase start"},
		{"trailing-json", append(append(encoded, []byte(" {}")...), '\n'), "trailing JSON"},
	} {
		t.Run(test.name, func(t *testing.T) {
			receiver, writer := manualProgressReceiver(t)
			if _, err := writer.Write(test.data); err != nil {
				t.Fatal(err)
			}
			if err := receiver.drain(true); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v; want %q", err, test.want)
			}
		})
	}
	t.Run("truncation-after-read", func(t *testing.T) {
		receiver, writer := manualProgressReceiver(t)
		if _, err := writer.Write(append(encoded, '\n')); err != nil {
			t.Fatal(err)
		}
		if err := receiver.drain(false); err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(writer.Name(), 0); err != nil {
			t.Fatal(err)
		}
		if err := receiver.drain(true); err == nil || !strings.Contains(err.Error(), "truncated") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestWorkerProgressStreamEncodedFrameBound(t *testing.T) {
	_, end := progressStreamTestEvents()
	for _, text := range []string{strings.Repeat("<", 1024), strings.Repeat(">", 1024), strings.Repeat("&", 1024), strings.Repeat("\"", 1024), strings.Repeat("\\", 1024)} {
		end.ID = strings.Repeat("<", operationProgressIDLimit)
		end.Stage, end.Instruction, end.Phase, end.Cache, end.Error = text, text, text, text, text
		end.State = "failed"
		encoded, err := json.Marshal(end)
		if err != nil {
			t.Fatal(err)
		}
		if len(encoded)+1 > workerProgressFrameLimit {
			t.Fatalf("encoded frame %d exceeds bound", len(encoded)+1)
		}
	}
	if 5*operationProgressTextLimit*6+operationProgressIDLimit*6+1024 >= workerProgressFrameLimit {
		t.Fatal("frame bound no longer covers worst-case escaping")
	}
}

type failingProgressWriter struct {
	calls int
	short bool
}

func (writer *failingProgressWriter) Write(data []byte) (int, error) {
	writer.calls++
	if writer.short {
		return len(data) / 2, nil
	}
	return 0, unix.ENOSPC
}
func (*failingProgressWriter) Close() error { return nil }

func TestWorkerProgressStreamStopsAfterWriteFailure(t *testing.T) {
	for _, short := range []bool{false, true} {
		t.Run(fmt.Sprint(short), func(t *testing.T) {
			output := &failingProgressWriter{short: short}
			writer := &workerProgressWriter{file: output}
			start, end := progressStreamTestEvents()
			writer.record(start)
			writer.record(end)
			summary := writer.finish()
			if output.calls != 1 || summary.Events != 0 || summary.Error == "" {
				t.Fatalf("continued after failed write: %#v calls=%d", summary, output.calls)
			}
			if short && !strings.Contains(summary.Error, io.ErrShortWrite.Error()) {
				t.Fatalf("short write: %#v", summary)
			}
		})
	}
}

func TestWorkerProgressStreamReservesCompletionsAndCountsDrops(t *testing.T) {
	var output bytes.Buffer
	writer := &workerProgressWriter{file: progressWriteCloser{Writer: &output}}
	buffer := newOperationProgressBuffer()
	buffer.limit = 2
	buffer.emit = writer.record
	start, end := progressStreamTestEvents()
	for _, id := range []string{"a", "b", "omitted"} {
		start.ID = id
		buffer.record(start)
	}
	buffer.addDropped(^uint64(0))
	buffer.addDropped(1)
	for _, id := range []string{"a", "b", "omitted"} {
		end.ID = id
		buffer.record(end)
	}
	dropped, summary := finishWorkerProgress(buffer, writer)
	if len(buffer.events) != 0 || dropped != ^uint64(0) || summary.Events != 4 || summary.Error != "" {
		t.Fatalf("events=%v dropped=%d summary=%#v", buffer.events, dropped, summary)
	}
	if bytes.Count(output.Bytes(), []byte{'\n'}) != 4 {
		t.Fatal("completion slots were not preserved")
	}
}

type progressWriteCloser struct{ io.Writer }

func (progressWriteCloser) Close() error { return nil }

func TestWorkerProgressStreamOwnershipAndClose(t *testing.T) {
	if _, err := workerProgressPath("result.json"); err == nil {
		t.Fatal("accepted relative path")
	}
	if _, err := workerProgressPath(filepath.Join(t.TempDir(), "other.json")); err == nil {
		t.Fatal("accepted arbitrary name")
	}
	path := filepath.Join(t.TempDir(), "result.json")
	receiver, err := newWorkerProgressReceiver(path, func(OperationProgress) {})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newWorkerProgressReceiver(path, func(OperationProgress) {}); err == nil {
		t.Fatal("reopened existing file")
	}
	if err := receiver.close(); err != nil {
		t.Fatal(err)
	}
	if err := receiver.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := receiver.file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("reader remains open: %v", err)
	}
	// Closing a receiver does not remove the job directory or ongoing worker's
	// inode: directory retention remains the supervisor caller's responsibility.
	streamPath, _ := workerProgressPath(path)
	if _, err := os.Stat(streamPath); err != nil {
		t.Fatalf("retained job data removed: %v", err)
	}
	if buffer, writer, err := newWorkerProgressCapture(false, path); err != nil || buffer != nil || writer != nil {
		t.Fatalf("disabled stream acquired resources: %v", err)
	}
}

func TestWorkerProgressStreamRejectsUnsafeFiles(t *testing.T) {
	for _, kind := range []string{"symlink", "fifo", "public"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "result.json")
			stream, _ := workerProgressPath(path)
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(filepath.Join(dir, "missing"), stream)
			case "fifo":
				err = unix.Mkfifo(stream, 0600)
			case "public":
				err = os.WriteFile(stream, nil, 0644)
			}
			if err != nil {
				t.Fatal(err)
			}
			if file, err := openWorkerProgressFile(path, false); err == nil {
				_ = file.Close()
				t.Fatal("accepted unsafe progress file")
			}
		})
	}
}

func TestWorkerProgressStreamFinalOwnershipAndOffsets(t *testing.T) {
	for _, kind := range []string{"missing-summary", "wrong-count", "wrong-offset", "transport-error", "partial-phase"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "result.json")
			received := make(chan OperationProgress, 2)
			receiver, err := newWorkerProgressReceiver(path, func(event OperationProgress) { received <- event })
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = receiver.close() }()
			buffer, writer, err := newWorkerProgressCapture(true, path)
			if err != nil {
				t.Fatal(err)
			}
			start, _ := progressStreamTestEvents()
			buffer.record(start)
			_, summary := finishWorkerProgress(buffer, writer)
			switch kind {
			case "missing-summary":
				summary = nil
			case "wrong-count":
				summary.Events++
			case "wrong-offset":
				summary.Bytes++
			case "transport-error":
				summary.Error = "disk full"
			}
			err = receiver.finish(summary)
			if kind == "partial-phase" {
				if err != nil {
					t.Fatal(err)
				}
				if len(received) != 1 || (<-received).State != "started" {
					t.Fatal("fabricated terminal transition")
				}
			} else if err == nil {
				t.Fatal("invalid ownership/offset was accepted")
			}
		})
	}
}

func TestWorkerProgressStreamStableReaderAndCloseOnExec(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.json")
	receiver, err := newWorkerProgressReceiver(path, func(OperationProgress) {})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = receiver.close() }()
	buffer, writer, err := newWorkerProgressCapture(true, path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.finish()
	for _, file := range []*os.File{receiver.file, writer.file.(*os.File)} {
		flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFD, 0)
		if err != nil {
			t.Fatal(err)
		}
		if flags&unix.FD_CLOEXEC == 0 {
			t.Fatal("progress file would leak through exec")
		}
	}
	streamPath, _ := workerProgressPath(path)
	if err := os.Rename(streamPath, streamPath+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(streamPath, []byte("malformed replacement\n"), 0600); err != nil {
		t.Fatal(err)
	}
	start, _ := progressStreamTestEvents()
	buffer.record(start)
	_, summary := finishWorkerProgress(buffer, writer)
	if err := receiver.finish(summary); err != nil {
		t.Fatalf("reader reopened replacement instead of following original inode: %v", err)
	}
}
