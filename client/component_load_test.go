package client

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/strio-dev/coopr/api"
)

func TestComponentLoadCapabilityAndWire(t *testing.T) {
	load := api.ComponentLoad{Archive: "/artifacts/component.tar", ComponentStoreDir: "/state/components"}
	for _, supported := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "supported"}[supported], func(t *testing.T) {
			info := allInfo
			if supported {
				info = strings.Replace(info, `"registry-cache"`, `"registry-cache","component-load"`, 1)
			}
			posts := 0
			c := testClient(t, info, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				posts++
				var got api.Request
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Error(err)
					return
				}
				if got.Kind != "component-load" || got.ComponentLoad == nil || *got.ComponentLoad != load {
					t.Errorf("wire changed: %+v", got)
				}
				encode(t, w, api.Event{Type: "result", Result: &api.Result{Reference: "sha256:verified"}})
			}))
			result, err := c.LoadComponent(t.Context(), load, StreamOptions{})
			if !supported {
				if err == nil || !strings.Contains(err.Error(), "component-load") || posts != 0 {
					t.Fatalf("preflight err=%v posts=%d", err, posts)
				}
				return
			}
			if err != nil || result.Reference != "sha256:verified" || posts != 1 {
				t.Fatalf("result=%+v err=%v posts=%d", result, err, posts)
			}
		})
	}
}

func TestComponentLoadNameCapabilityAndWire(t *testing.T) {
	for _, name := range []string{"", "delivery:one"} {
		for _, supported := range []bool{false, true} {
			t.Run(name+map[bool]string{false: "/missing", true: "/supported"}[supported], func(t *testing.T) {
				load := api.ComponentLoad{Archive: "/artifacts/component.tar", ComponentStoreDir: "/state/components", Name: name}
				info := strings.Replace(allInfo, `"registry-cache"`, `"registry-cache","component-load"`, 1)
				if supported {
					info = strings.Replace(info, `"component-load"`, `"component-load","component-load-name"`, 1)
				}
				posts := 0
				c := testClient(t, info, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					posts++
					var got api.Request
					if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
						t.Error(err)
						return
					}
					if got.Kind != "component-load" || got.ComponentLoad == nil || *got.ComponentLoad != load {
						t.Errorf("wire changed: %+v", got)
						return
					}
					encode(t, w, api.Event{Type: "result", Result: &api.Result{Reference: "selected"}})
				}))
				result, err := c.LoadComponent(t.Context(), load, StreamOptions{})
				if name != "" && !supported {
					if err == nil || !strings.Contains(err.Error(), api.CapabilityComponentLoadName) || posts != 0 {
						t.Fatalf("preflight err=%v posts=%d", err, posts)
					}
					return
				}
				if err != nil || posts != 1 || result.Reference != "selected" {
					t.Fatalf("result=%+v err=%v posts=%d", result, err, posts)
				}
			})
		}
	}
}
