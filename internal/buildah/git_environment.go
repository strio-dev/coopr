package buildah

import (
	"strings"
)

// gitWorkerEnvironment prevents a build's remote Git inputs from depending on
// the host's Git configuration, credential helpers, or interactive prompts.
// Workers have their own process environment, so this never mutates the
// caller's environment while other stages run.
func gitWorkerEnvironment(parent []string) []string {
	environment := make([]string, 0, len(parent)+4)
	for _, entry := range parent {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "GIT_") || name == "SSH_ASKPASS" || name == "SSH_AUTH_SOCK" {
			continue
		}
		environment = append(environment, entry)
	}
	return append(environment,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL=git:http:https",
		"GIT_SSH_COMMAND=/bin/false",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_ASKPASS=/bin/false",
		"SSH_ASKPASS=/bin/false",
	)
}
