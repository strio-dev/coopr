package client

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/strio-dev/coopr/api"
)

func TestRunNetworkCapabilityAndWire(t *testing.T) {
	for _, kind := range []string{"execute", "build", "component"} {
		for _, network := range []string{"", "none"} {
			for _, supported := range []bool{false, true} {
				t.Run(kind+"/"+network+map[bool]string{false: "/missing", true: "/supported"}[supported], func(t *testing.T) {
					info := allInfo
					if supported {
						info = strings.Replace(info, `"registry-cache"`, `"registry-cache","run-network"`, 1)
					}
					posts := 0
					c := testClient(t, info, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						posts++
						var got api.Request
						if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
							t.Error(err)
							return
						}
						if got.Kind != kind || got.Options.Network != network {
							t.Errorf("network wire changed: %+v", got)
							return
						}
						encode(t, w, api.Event{Type: "result", Result: &api.Result{ImageID: "result"}})
					}))
					req := api.Request{Kind: kind, Options: api.Options{Network: network}}
					_, err := c.Execute(t.Context(), req, StreamOptions{})
					if network != "" && !supported {
						if err == nil || !strings.Contains(err.Error(), api.CapabilityRunNetwork) || posts != 0 {
							t.Fatalf("preflight err=%v posts=%d", err, posts)
						}
					} else if err != nil || posts != 1 {
						t.Fatalf("err=%v posts=%d", err, posts)
					}
				})
			}
		}
	}
}
