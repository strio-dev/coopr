package apiservice

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/strio-dev/coopr/api"
)

func TestBackendPanicDoesNotAcknowledgeCleanup(t *testing.T) {
	server := New(context.Background(), "test", func(context.Context, api.Request, io.Writer, io.Writer) (api.Result, error) { panic("backend failure") })
	path := "/v1/executions/" + strings.Repeat("a", 32)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"kind":"execute"}`)))
	var event api.Event
	if err := json.Unmarshal(response.Body.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	if event.Error == nil || event.Error.Code != "cleanup_failed" {
		t.Errorf("panic result = %#v; cleanup cannot be confirmed", event.Error)
	}
	ack := httptest.NewRecorder()
	server.ServeHTTP(ack, httptest.NewRequest(http.MethodDelete, path, nil))
	var status api.Status
	if err := json.Unmarshal(ack.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Error == nil || status.Error.Code != "cleanup_failed" {
		t.Errorf("panic acknowledgement = %#v; cleanup cannot be confirmed", status.Error)
	}
	server.Drain()
}
