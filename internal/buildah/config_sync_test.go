package buildah

import (
	"bytes"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"coopr/internal/imageconfig"

	upstream "go.podman.io/buildah"
	"go.podman.io/buildah/define"
	"go.podman.io/buildah/docker"
)

func TestSyncBuilderConfigReplacesRuntimeFieldsAndPreservesLogicalExtensions(t *testing.T) {
	builder := configSyncBuilder()
	seedBuilderConfig(builder)
	logical, err := imageconfig.Parse([]byte(`{
  "author":"component author",
  "x-top":{"preserve":true},
  "config":{
    "User":"1000:1001",
    "Hostname":"explicit-component",
    "ExposedPorts":{"8443/tcp":{},"5353/udp":{}},
	    "Env":["A=one","B=two=parts","A=last","REMOVE"],
    "Entrypoint":["/usr/bin/component","--flag"],
    "Cmd":["serve"],
    "Volumes":{"/cache":{},"/state":{}},
    "WorkingDir":"/srv/component",
    "Labels":{"a":"first","z":"last"},
    "StopSignal":"SIGQUIT",
    "ArgsEscaped":true,
    "Shell":["/bin/bash","-ceu"],
	"Healthcheck":{"Test":["CMD-SHELL","true"],"Interval":30000000000,"Timeout":3000000000,"StartPeriod":5000000000,"StartInterval":1000000000,"Retries":4},
	"OnBuild":["RUN make generated","COPY output /output"],
    "x-nested":{"preserve":true}
  }
}`))
	if err != nil {
		t.Fatal(err)
	}
	before, err := logical.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}

	if err := syncBuilderConfig(builder, logical); err != nil {
		t.Fatal(err)
	}
	if builder.Maintainer() != "component author" || builder.User() != "1000:1001" || builder.WorkDir() != "/srv/component" || builder.StopSignal() != "SIGQUIT" {
		t.Fatalf("scalar config was not synchronized: author=%q user=%q workdir=%q signal=%q", builder.Maintainer(), builder.User(), builder.WorkDir(), builder.StopSignal())
	}
	if builder.Hostname() != "explicit-component" {
		t.Fatalf("explicit hostname = %q", builder.Hostname())
	}
	if !reflect.DeepEqual(builder.Cmd(), []string{"serve"}) || !reflect.DeepEqual(builder.Entrypoint(), []string{"/usr/bin/component", "--flag"}) || !reflect.DeepEqual(builder.Shell(), []string{"/bin/bash", "-ceu"}) {
		t.Fatalf("vector config was not synchronized: cmd=%v entrypoint=%v shell=%v", builder.Cmd(), builder.Entrypoint(), builder.Shell())
	}
	if !reflect.DeepEqual(builder.Env(), []string{"B=two=parts", "A=last"}) {
		t.Fatalf("environment = %v", builder.Env())
	}
	if !reflect.DeepEqual(builder.Labels(), map[string]string{"a": "first", "z": "last"}) {
		t.Fatalf("labels = %v", builder.Labels())
	}
	ports := builder.Ports()
	volumes := builder.Volumes()
	sort.Strings(ports)
	sort.Strings(volumes)
	if !reflect.DeepEqual(ports, []string{"5353/udp", "8443/tcp"}) || !reflect.DeepEqual(volumes, []string{"/cache", "/state"}) {
		t.Fatalf("ports=%v volumes=%v", ports, volumes)
	}
	if !builder.OCIv1.Config.ArgsEscaped || !builder.Docker.Config.ArgsEscaped { //nolint:staticcheck // Verify legacy field synchronization.
		t.Fatal("ArgsEscaped was not synchronized to both builder formats")
	}
	healthcheck := builder.Healthcheck()
	if healthcheck == nil || !reflect.DeepEqual(healthcheck.Test, []string{"CMD-SHELL", "true"}) || healthcheck.Interval != 30*time.Second || healthcheck.Timeout != 3*time.Second || healthcheck.StartPeriod != 5*time.Second || healthcheck.StartInterval != time.Second || healthcheck.Retries != 4 {
		t.Fatalf("healthcheck was not synchronized: %#v", healthcheck)
	}
	if !reflect.DeepEqual(builder.OnBuild(), []string{"RUN make generated", "COPY output /output"}) {
		t.Fatalf("OnBuild was not synchronized: %v", builder.OnBuild())
	}
	after, err := logical.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || !bytes.Contains(after, []byte(`"x-top"`)) || !bytes.Contains(after, []byte(`"x-nested"`)) {
		t.Fatalf("logical sidecar changed or lost extensions:\n%s", after)
	}
}

func TestSyncBuilderConfigClearsReplacedCollectionsAndScalars(t *testing.T) {
	builder := configSyncBuilder()
	seedBuilderConfig(builder)
	logical, err := imageconfig.Parse([]byte(`{"config":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := syncBuilderConfig(builder, logical); err != nil {
		t.Fatal(err)
	}
	if builder.Maintainer() != "" || builder.User() != "" || builder.WorkDir() != "" || builder.StopSignal() != "" {
		t.Fatalf("scalar values were not cleared: author=%q user=%q workdir=%q signal=%q", builder.Maintainer(), builder.User(), builder.WorkDir(), builder.StopSignal())
	}
	if len(builder.Cmd()) != 0 || len(builder.Entrypoint()) != 0 || len(builder.Shell()) != 0 || len(builder.Env()) != 0 || len(builder.Labels()) != 0 || len(builder.Ports()) != 0 || len(builder.Volumes()) != 0 {
		t.Fatalf("runtime values were not cleared: cmd=%v entrypoint=%v shell=%v env=%v labels=%v ports=%v volumes=%v", builder.Cmd(), builder.Entrypoint(), builder.Shell(), builder.Env(), builder.Labels(), builder.Ports(), builder.Volumes())
	}
	if builder.OCIv1.Config.ArgsEscaped || builder.Docker.Config.ArgsEscaped { //nolint:staticcheck // Verify legacy field clearing.
		t.Fatal("ArgsEscaped was not cleared")
	}
	if builder.Healthcheck() != nil || len(builder.OnBuild()) != 0 {
		t.Fatalf("Docker-only metadata was not cleared: healthcheck=%#v onbuild=%v", builder.Healthcheck(), builder.OnBuild())
	}
}

func TestSyncBuilderConfigValidatesInputs(t *testing.T) {
	logical := imageconfig.New()
	if err := syncBuilderConfig(nil, logical); err == nil || !strings.Contains(err.Error(), "builder") {
		t.Fatalf("nil builder error = %v", err)
	}
	if err := syncBuilderConfig(configSyncBuilder(), nil); err == nil || !strings.Contains(err.Error(), "logical") {
		t.Fatalf("nil logical config error = %v", err)
	}
}

func TestSyncBuilderConfigRetainsGeneratedHostnameWhenUnspecified(t *testing.T) {
	builder := configSyncBuilder()
	builder.SetHostname("generated-for-builder")
	if err := syncBuilderConfig(builder, imageconfig.New()); err != nil {
		t.Fatal(err)
	}
	if builder.Hostname() != "generated-for-builder" {
		t.Fatalf("unspecified hostname replaced builder default: %q", builder.Hostname())
	}
}

func configSyncBuilder() *upstream.Builder {
	return &upstream.Builder{
		Format: define.Dockerv2ImageManifest,
		Docker: docker.V2Image{V1Image: docker.V1Image{Config: &docker.Config{}}},
	}
}

func seedBuilderConfig(builder *upstream.Builder) {
	builder.SetMaintainer("stale author")
	builder.SetUser("stale-user")
	builder.SetWorkDir("/stale")
	builder.SetCmd([]string{"stale-cmd"})
	builder.SetEntrypoint([]string{"stale-entrypoint"})
	builder.SetStopSignal("SIGTERM")
	builder.SetShell([]string{"/bin/stale", "-c"})
	builder.SetEnv("STALE", "yes")
	builder.SetEnv("REMOVE", "stale")
	builder.SetLabel("stale", "yes")
	builder.SetPort("8080/tcp")
	builder.AddVolume("/stale")
	builder.OCIv1.Config.ArgsEscaped = true //nolint:staticcheck // Seed legacy field replacement coverage.
	builder.Docker.Config.ArgsEscaped = true
	builder.SetHealthcheck(&docker.HealthConfig{Test: []string{"CMD", "stale"}, Interval: time.Second, Retries: 1})
	builder.SetOnBuild("RUN stale")
}
