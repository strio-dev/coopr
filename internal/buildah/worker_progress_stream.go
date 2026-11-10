package buildah

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Five bounded text fields can each expand sixfold under JSON escaping, as can
// the ID. The remaining keys, timestamps, states and integers fit within 1 KiB:
// 5*1024*6 + 128*6 + 1024 = 32512 < 32768 (including the newline).
const workerProgressFrameLimit = 32 << 10
const workerProgressFileLimit = 2 * operationProgressPhaseLimit * workerProgressFrameLimit
const workerProgressPoll = 20 * time.Millisecond

type workerProgressSummary struct {
	Events int    `json:"events"`
	Bytes  int64  `json:"bytes"`
	Error  string `json:"error,omitempty"`
}

func workerProgressPath(resultPath string) (string, error) {
	if !filepath.IsAbs(resultPath) || filepath.Clean(resultPath) != resultPath || filepath.Base(resultPath) != "result.json" {
		return "", errors.New("live worker progress requires an absolute worker result.json path")
	}
	return filepath.Join(filepath.Dir(resultPath), "progress.ndjson"), nil
}

// This channel conveys observations only. Its name derives from the existing
// result location, never from a separately supplied destination or descriptor.
func openWorkerProgressFile(resultPath string, create bool) (*os.File, error) {
	path, err := workerProgressPath(resultPath)
	if err != nil {
		return nil, err
	}
	flags := unix.O_WRONLY | unix.O_APPEND | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
	if create {
		flags = unix.O_RDONLY | unix.O_CREAT | unix.O_EXCL | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
	}
	fd, err := unix.Open(path, flags, 0600)
	if err != nil {
		return nil, fmt.Errorf("open private worker progress: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	var stat unix.Stat_t
	if err == nil {
		err = unix.Fstat(fd, &stat)
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		_ = file.Close()
		return nil, errors.Join(errors.New("worker progress must be a private owned regular file"), err)
	}
	return file, nil
}

type workerProgressWriter struct {
	mu      sync.Mutex
	file    io.WriteCloser
	summary workerProgressSummary
	closed  bool
}

func newWorkerProgressCapture(live bool, resultPath string) (*operationProgressBuffer, *workerProgressWriter, error) {
	if !live {
		return nil, nil, nil
	}
	buffer := newOperationProgressBuffer()
	file, err := openWorkerProgressFile(resultPath, false)
	if err != nil {
		return nil, nil, err
	}
	writer := &workerProgressWriter{file: file}
	buffer.emit = writer.record
	return buffer, writer, nil
}

func (writer *workerProgressWriter) record(event OperationProgress) {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.closed || writer.summary.Error != "" {
		return
	}
	frame, err := json.Marshal(event)
	if err == nil && len(frame)+1 > workerProgressFrameLimit {
		err = errors.New("worker progress frame exceeds bound")
	}
	if err == nil && (writer.summary.Events >= 2*operationProgressPhaseLimit || writer.summary.Bytes+int64(len(frame)+1) > workerProgressFileLimit) {
		err = errors.New("worker progress exceeds admitted phase bound")
	}
	if err == nil {
		frame = append(frame, '\n')
		var n int
		n, err = writer.file.Write(frame)
		writer.summary.Bytes += int64(n)
		if err == nil && n != len(frame) {
			err = io.ErrShortWrite
		}
		if err == nil {
			writer.summary.Events++
		}
	}
	if err != nil {
		writer.summary.Error = operationProgressText(err.Error())
	}
}

func (writer *workerProgressWriter) finish() *workerProgressSummary {
	if writer == nil {
		return nil
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if !writer.closed {
		writer.closed = true
		if err := writer.file.Close(); err != nil && writer.summary.Error == "" {
			writer.summary.Error = operationProgressText(err.Error())
		}
	}
	summary := writer.summary
	return &summary
}

func finishWorkerProgress(buffer *operationProgressBuffer, writer *workerProgressWriter) (uint64, *workerProgressSummary) {
	_, dropped := buffer.snapshot()
	return dropped, writer.finish()
}

// One goroutine owns reading, validation and the synchronous callback. Callback
// implementations must return, just as on the direct execution path. Stop joins
// it; no callback goroutine is abandoned if a caller supplies a blocking sink.
type workerProgressReceiver struct {
	file      *os.File
	stop      chan struct{}
	done      chan struct{}
	once      sync.Once
	sink      OperationProgressFunc
	validator operationProgressValidator
	pending   []byte
	bytes     int64
	err       error
	block     [workerProgressFrameLimit]byte
}

func newWorkerProgressReceiver(resultPath string, sink OperationProgressFunc) (*workerProgressReceiver, error) {
	if sink == nil {
		return nil, nil
	}
	file, err := openWorkerProgressFile(resultPath, true)
	if err != nil {
		return nil, err
	}
	receiver := &workerProgressReceiver{file: file, stop: make(chan struct{}), done: make(chan struct{}), sink: sink}
	go receiver.follow()
	return receiver, nil
}

func (receiver *workerProgressReceiver) follow() {
	defer close(receiver.done)
	ticker := time.NewTicker(workerProgressPoll)
	defer ticker.Stop()
	for {
		if receiver.err == nil {
			receiver.err = receiver.drain(false)
		}
		select {
		case <-receiver.stop:
			return
		case <-ticker.C:
		}
	}
}

// drain is bounded by the protocol's total file limit even if a corrupt writer
// continuously appends. A suffix is retained only up to one bounded frame.
func (receiver *workerProgressReceiver) drain(final bool) error {
	info, err := receiver.file.Stat()
	if err != nil {
		return err
	}
	if info.Size() < receiver.bytes {
		return errors.New("worker progress was truncated")
	}
	if info.Size() > workerProgressFileLimit {
		return errors.New("worker progress exceeds file bound")
	}
	block := receiver.block[:]
	for {
		n, readErr := receiver.file.Read(block)
		receiver.bytes += int64(n)
		if receiver.bytes > workerProgressFileLimit {
			return errors.New("worker progress exceeds file bound")
		}
		input := block[:n]
		for len(input) != 0 {
			end := bytes.IndexByte(input, '\n')
			if end < 0 {
				if len(receiver.pending)+len(input) >= workerProgressFrameLimit {
					return errors.New("worker progress frame exceeds bound")
				}
				receiver.pending = append(receiver.pending, input...)
				break
			}
			if len(receiver.pending)+end+1 > workerProgressFrameLimit {
				return errors.New("worker progress frame exceeds bound")
			}
			receiver.pending = append(receiver.pending, input[:end]...)
			var event OperationProgress
			decoder := json.NewDecoder(bytes.NewReader(receiver.pending))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&event); err != nil {
				return fmt.Errorf("decode live worker progress: %w", err)
			}
			var extra any
			if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
				return errors.New("live worker progress contains trailing JSON")
			}
			if err := receiver.validator.accept(event); err != nil {
				return fmt.Errorf("invalid live worker progress: %w", err)
			}
			receiver.pending = receiver.pending[:0]
			receiver.sink(cloneOperationProgress(event))
			input = input[end+1:]
		}
		if errors.Is(readErr, io.EOF) {
			if final {
				info, err := receiver.file.Stat()
				if err != nil {
					return err
				}
				if info.Size() != receiver.bytes {
					return errors.New("worker progress final file size changed")
				}
			}
			if final && len(receiver.pending) != 0 {
				return errors.New("live worker progress ends with an incomplete frame")
			}
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

// close is also the pre-launch failure/incomplete-wait path. Incomplete workers
// retain their job directory in the caller, but no telemetry reader survives.
func (receiver *workerProgressReceiver) close() error {
	if receiver == nil {
		return nil
	}
	receiver.once.Do(func() {
		close(receiver.stop)
		<-receiver.done
		receiver.err = errors.Join(receiver.err, receiver.file.Close())
	})
	return receiver.err
}

func (receiver *workerProgressReceiver) finish(summary *workerProgressSummary) error {
	if receiver == nil {
		if summary != nil {
			return errors.New("unexpected live worker progress response")
		}
		return nil
	}
	// Stop polling only after the caller has confirmed the worker's exit. Then
	// drain the stable inode, including observations from cancellation cleanup.
	receiver.once.Do(func() {
		close(receiver.stop)
		<-receiver.done
		if receiver.err == nil {
			receiver.err = receiver.drain(true)
		}
		receiver.err = errors.Join(receiver.err, receiver.file.Close())
	})
	if summary == nil {
		return errors.Join(receiver.err, errors.New("worker did not confirm exclusive live progress ownership"))
	}
	if summary.Events < 0 || summary.Events > 2*operationProgressPhaseLimit || summary.Bytes < 0 || summary.Bytes > workerProgressFileLimit {
		return errors.Join(receiver.err, errors.New("invalid live worker progress summary"))
	}
	if summary.Events != receiver.validator.count || summary.Bytes != receiver.bytes {
		return errors.Join(receiver.err, errors.New("live worker progress final count or offset mismatch"))
	}
	if summary.Error != "" {
		return errors.Join(receiver.err, fmt.Errorf("worker progress transport failed: %s", operationProgressText(summary.Error)))
	}
	return receiver.err
}
