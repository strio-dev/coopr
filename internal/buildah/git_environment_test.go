package buildah

import (
	"strings"
	"testing"
)

func TestGitWorkerEnvironmentRemovesHostGitInputs(t *testing.T) {
	environment := gitWorkerEnvironment([]string{
		"HOME=/tmp/user", "SOURCE_DATE_EPOCH=123", "GIT_CONFIG_GLOBAL=/tmp/user/.gitconfig",
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.helper",
		"GIT_CONFIG_VALUE_0=host-helper", "GIT_ASKPASS=/tmp/host-askpass",
		"SSH_ASKPASS=/tmp/host-askpass", "GIT_TERMINAL_PROMPT=1",
		"SSH_AUTH_SOCK=/tmp/ambient-agent", "GIT_ALLOW_PROTOCOL=ssh:ext", "GIT_SSH_COMMAND=ambient-ssh",
	})
	values := map[string]string{}
	for _, entry := range environment {
		name, value, _ := strings.Cut(entry, "=")
		values[name] = value
	}
	if values["HOME"] != "/tmp/user" || values["SOURCE_DATE_EPOCH"] != "123" {
		t.Fatalf("unrelated worker environment changed: %v", values)
	}
	for _, name := range []string{"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0"} {
		if _, exists := values[name]; exists {
			t.Fatalf("host Git configuration %s survived: %v", name, values)
		}
	}
	if values["SSH_AUTH_SOCK"] != "" || values["GIT_ALLOW_PROTOCOL"] != "git:http:https" || values["GIT_SSH_COMMAND"] != "/bin/false" {
		t.Fatalf("worker admitted ambient SSH: %v", values)
	}
	if values["GIT_CONFIG_GLOBAL"] != "/dev/null" || values["GIT_CONFIG_NOSYSTEM"] != "1" ||
		values["GIT_TERMINAL_PROMPT"] != "0" || values["GIT_ASKPASS"] != "/bin/false" ||
		values["SSH_ASKPASS"] != "/bin/false" {
		t.Fatalf("Git worker environment is not isolated: %v", values)
	}
}
