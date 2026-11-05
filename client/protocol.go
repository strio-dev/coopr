package client

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"

	"github.com/strio-dev/coopr/api"
)

// decodeObject accepts additive fields, but not ambiguous duplicate keys,
// non-object payloads, or trailing JSON values.
func decodeObject(data []byte, out any) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return nil, errors.New("coopr response must be a JSON object")
	}
	fields := map[string]json.RawMessage{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		name, ok := token.(string)
		if !ok {
			return nil, errors.New("invalid Coopr object key")
		}
		if _, present := fields[name]; present {
			return nil, fmt.Errorf("duplicate Coopr response field %q", name)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[name] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, errors.New("coopr response contains trailing data")
	}
	// encoding/json accepts case-folded struct field names. Reject alternate
	// spellings of protocol control fields before that matching can conceal a
	// mixed payload or change cancellation/cleanup classification.
	var canonical []string
	switch out.(type) {
	case *api.Event:
		canonical = []string{"type", "data", "progress", "result", "error"}
	case *api.Status:
		canonical = []string{"id", "state", "result", "error"}
	case *api.Info:
		canonical = []string{"apiVersion", "version", "capabilities"}
	case *api.Error:
		canonical = []string{"code", "message"}
	case *api.OperationProgress:
		canonical = []string{"id", "stage", "instruction", "operation", "phase", "state", "cache", "started", "completed", "durationNanos", "error"}
	}
	for name := range fields {
		for _, field := range canonical {
			if name != field && strings.EqualFold(name, field) {
				return nil, fmt.Errorf("noncanonical Coopr control field %q", name)
			}
		}
	}
	if err := json.Unmarshal(data, out); err != nil {
		return nil, err
	}
	for field, target := range map[string]any{"error": &api.Error{}, "progress": &api.OperationProgress{}} {
		if _, event := out.(*api.Event); !event {
			if _, status := out.(*api.Status); !status || field != "error" {
				continue
			}
		}
		if raw, present := fields[field]; present && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			if _, err := decodeObject(raw, target); err != nil {
				return nil, err
			}
		}
	}
	return fields, nil
}

type terminalError struct{ error }

func consume(reader io.Reader, streams StreamOptions) (out api.Result, retErr error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 32<<10), api.MaxEventBytes)
	terminal := false
	for scanner.Scan() {
		if terminal {
			return out, errors.Join(retErr, errors.New("coopr sent data after its terminal result"))
		}
		var event api.Event
		fields, err := decodeObject(scanner.Bytes(), &event)
		if err != nil {
			return out, fmt.Errorf("invalid Coopr event: %w", err)
		}
		for _, field := range []string{"data", "progress", "result", "error"} {
			if _, present := fields[field]; !present {
				continue
			}
			allowed := event.Type == "result" && (field == "result" || field == "error") || event.Type == "progress" && field == "progress" || (event.Type == "stdout" || event.Type == "stderr") && field == "data"
			if !allowed {
				return out, errors.New("coopr event contains another payload")
			}
		}
		switch event.Type {
		case "stdout", "stderr":
			writer := streams.Stdout
			if event.Type == "stderr" {
				writer = streams.Stderr
			}
			if writer != nil {
				if n, err := writer.Write(event.Data); err != nil {
					return out, err
				} else if n != len(event.Data) {
					return out, io.ErrShortWrite
				}
			}
		case "progress":
			if event.Progress == nil {
				return out, errors.New("coopr progress payload is missing")
			}
			if err := validateProgress(*event.Progress); err != nil {
				return out, fmt.Errorf("invalid Coopr progress: %w", err)
			}
			if streams.Progress != nil {
				streams.Progress(*event.Progress)
			}
		case "result":
			if event.Result == nil && event.Error == nil {
				return out, errors.New("coopr terminal result is empty")
			}
			if event.Error != nil && !validError(event.Error) {
				return out, errors.New("coopr terminal error has no code")
			}
			terminal = true
			if event.Result != nil {
				out = *event.Result
			}
			if event.Error != nil {
				retErr = remoteError(event.Error)
			}
		default:
			return out, fmt.Errorf("unknown Coopr event type %q", event.Type)
		}
	}
	if err := scanner.Err(); err != nil {
		return out, errors.Join(retErr, err)
	}
	if !terminal {
		return out, errors.New("coopr response ended without a terminal result")
	}
	if retErr != nil {
		return out, &terminalError{retErr}
	}
	return out, nil
}

func validError(failure *api.Error) bool {
	return strings.TrimSpace(failure.Code) != "" && !strings.ContainsFunc(failure.Code, unicode.IsControl)
}

func validateStatus(status api.Status, id string) error {
	if status.ID != id {
		return errors.New("coopr status execution ID differs from request")
	}
	if status.Error != nil && !validError(status.Error) {
		return errors.New("coopr status error has no code")
	}
	switch status.State {
	case "running":
		if status.Error != nil || status.Result != nil {
			return errors.New("running Coopr status contains terminal payload")
		}
	case "succeeded":
		if status.Error != nil {
			return errors.New("successful Coopr status contains an error")
		}
	case "failed":
		if status.Error == nil {
			return errors.New("failed Coopr status has no error")
		}
	case "canceled":
		if status.Error != nil && status.Error.Code != "canceled" && status.Error.Code != "cleanup_failed" {
			return errors.New("canceled Coopr status contains an inconsistent error")
		}
	default:
		return fmt.Errorf("unknown Coopr status state %q", status.State)
	}
	return nil
}

func validateProgress(event api.OperationProgress) error {
	if strings.TrimSpace(event.ID) == "" || strings.TrimSpace(event.Phase) == "" {
		return errors.New("id and phase are required")
	}
	for _, value := range []string{event.ID, event.Stage, event.Instruction, event.Phase, event.Cache, event.Error} {
		if len(value) > 1024 || strings.ContainsFunc(value, unicode.IsControl) {
			return errors.New("progress text exceeds its limit or contains control characters")
		}
	}
	if event.Operation < 0 || event.Started.IsZero() || event.DurationNanos < 0 {
		return errors.New("invalid operation or timing")
	}
	switch event.State {
	case "started":
		if event.Completed != nil || event.DurationNanos != 0 || event.Cache != "" || event.Error != "" {
			return errors.New("started phase contains completion fields")
		}
	case "completed", "failed", "canceled":
		if event.Completed == nil || event.Completed.IsZero() {
			return errors.New("completed phase has invalid completion time")
		}
		if event.State == "completed" && event.Error != "" {
			return errors.New("successful phase contains an error")
		}
	default:
		return errors.New("unknown phase state")
	}
	return nil
}
