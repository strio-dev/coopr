package buildah

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"go.podman.io/buildah/pkg/sshagent"
)

const (
	gitAuthHeaderSecret = "GIT_AUTH_HEADER"
	gitAuthTokenSecret  = "GIT_AUTH_TOKEN"
	gitKnownHostsSecret = "GIT_KNOWN_HOSTS"
)

type addSourceSSHProvider interface {
	addSourceSSH(id string) (socket string, cleanup func() error, found bool, err error)
}

func (b nativeBuilder) addSourceSSH(id string) (string, func() error, bool, error) {
	return (buildCredentialSource{sshSpecs: b.sshSpecs, processLabel: b.ProcessLabel}).addSourceSSH(id)
}

type buildCredentialSource struct {
	secretSpecs  []string
	sshSpecs     []string
	processLabel string
}

func (credentials buildCredentialSource) addSourceSSH(id string) (string, func() error, bool, error) {
	sources, err := parseOperationSSH(credentials.sshSpecs)
	if err != nil {
		return "", nil, false, fmt.Errorf("parse SSH source: %w", err)
	}
	source, found := sources[id]
	if !found {
		return "", nil, false, nil
	}
	agent, err := sshagent.NewAgentServer(source)
	if err != nil {
		return "", nil, false, fmt.Errorf("create read-only SSH agent: %w", err)
	}
	socket, err := agent.Serve(credentials.processLabel)
	if err != nil {
		return "", nil, false, errors.Join(fmt.Errorf("serve read-only SSH agent: %w", err), agent.Shutdown())
	}
	return socket, agent.Shutdown, true, nil
}

func (source buildCredentialSource) addSourceSecret(id string) ([]byte, bool, error) {
	secrets, err := parseOperationSecrets(source.secretSpecs)
	if err != nil {
		return nil, false, err
	}
	secret, found := secrets[id]
	if !found {
		return nil, false, nil
	}
	value, err := secret.ResolveValue()
	if err != nil {
		return nil, false, fmt.Errorf("resolve build secret %q: %w", id, err)
	}
	return value, true, nil
}

func gitAddEnvironment(builder any, remote string) ([]string, func() error, bool, error) {
	environment := gitWorkerEnvironment(os.Environ())
	environment = removeEnvironment(environment, "HOME", "XDG_CONFIG_HOME", "SSH_AUTH_SOCK")
	environment = append(environment, "HOME=/dev/null", "XDG_CONFIG_HOME=/dev/null")

	if isSSHGitRemote(remote) {
		provider, ok := builder.(addSourceSSHProvider)
		if !ok {
			return nil, nil, false, errors.New("SSH Git ADD requires an SSH-capable build executor")
		}
		socket, cleanup, found, err := provider.addSourceSSH("default")
		if err != nil {
			return nil, nil, false, err
		}
		if !found {
			return nil, nil, false, errors.New("SSH Git ADD requires --ssh default or --ssh default=PATH")
		}
		knownHosts, knownHostsCleanup, err := gitAddKnownHosts(builder, remote)
		if err != nil {
			return nil, cleanup, false, err
		}
		cleanup = joinCleanups(knownHostsCleanup, cleanup)
		return append(environment,
			"SSH_AUTH_SOCK="+socket,
			"GIT_SSH_COMMAND=ssh -F /dev/null -o StrictHostKeyChecking=yes -o GlobalKnownHostsFile=/dev/null -o UserKnownHostsFile="+shellQuote(knownHosts),
		), cleanup, false, nil
	}

	authorization, found, err := gitAddAuthorization(builder, remote)
	if err != nil || !found {
		return environment, nil, false, err
	}
	credentialScope := gitAuthorizationScope(remote)
	return append(environment,
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=http."+credentialScope+".extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: "+authorization,
		"GIT_CONFIG_KEY_1=http."+credentialScope+".followRedirects",
		"GIT_CONFIG_VALUE_1=false",
	), nil, true, nil
}

func gitAuthorizationScope(remote string) string {
	return remote
}

func gitAddKnownHosts(builder any, remote string) (string, func() error, error) {
	provider, ok := builder.(addSourceCredentialProvider)
	if !ok {
		return "", nil, errors.New("SSH Git ADD requires a GIT_KNOWN_HOSTS build secret")
	}
	host, err := gitSSHHost(remote)
	if err != nil {
		return "", nil, err
	}
	var contents []byte
	for _, id := range []string{gitKnownHostsSecret + "." + host, gitKnownHostsSecret} {
		value, found, err := provider.addSourceSecret(id)
		if err != nil {
			return "", nil, fmt.Errorf("resolve secret %q: %w", id, err)
		}
		if found {
			contents = value
			break
		}
	}
	if len(contents) == 0 {
		return "", nil, fmt.Errorf("SSH Git ADD for host %q requires --secret id=%s.%s or --secret id=%s", host, gitKnownHostsSecret, host, gitKnownHostsSecret)
	}
	file, err := os.CreateTemp("", "coopr-git-known-hosts-")
	if err != nil {
		return "", nil, fmt.Errorf("create Git known_hosts file: %w", err)
	}
	cleanup := func() error { return os.Remove(file.Name()) }
	if err := file.Chmod(0o600); err != nil {
		closeErr := file.Close()
		return "", nil, errors.Join(fmt.Errorf("secure Git known_hosts file: %w", err), closeErr, cleanup())
	}
	if _, err := file.Write(contents); err != nil {
		closeErr := file.Close()
		return "", nil, errors.Join(fmt.Errorf("write Git known_hosts file: %w", err), closeErr, cleanup())
	}
	if err := file.Close(); err != nil {
		return "", nil, errors.Join(fmt.Errorf("close Git known_hosts file: %w", err), cleanup())
	}
	return file.Name(), cleanup, nil
}

func gitSSHHost(remote string) (string, error) {
	if strings.HasPrefix(remote, "ssh://") {
		parsed, err := url.Parse(remote)
		if err != nil || parsed.Hostname() == "" {
			return "", fmt.Errorf("parse SSH Git remote %q", redactGitSource(remote))
		}
		return parsed.Hostname(), nil
	}
	userHost, _, found := strings.Cut(remote, ":")
	if !found {
		return "", fmt.Errorf("parse SCP-style Git remote %q", remote)
	}
	_, host, found := strings.Cut(userHost, "@")
	if !found || host == "" {
		return "", fmt.Errorf("parse SCP-style Git remote %q", remote)
	}
	return host, nil
}

func joinCleanups(cleanups ...func() error) func() error {
	return func() (err error) {
		for _, cleanup := range cleanups {
			if cleanup != nil {
				err = errors.Join(err, cleanup())
			}
		}
		return err
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func gitAddAuthorization(builder any, remote string) (string, bool, error) {
	provider, ok := builder.(addSourceCredentialProvider)
	if !ok {
		return "", false, nil
	}
	parsed, err := url.Parse(remote)
	if err != nil {
		return "", false, fmt.Errorf("parse Git remote: %w", err)
	}
	for _, candidate := range []struct {
		id    string
		token bool
	}{
		{id: gitAuthHeaderSecret + "." + parsed.Host},
		{id: gitAuthTokenSecret + "." + parsed.Host, token: true},
		{id: gitAuthHeaderSecret},
		{id: gitAuthTokenSecret, token: true},
	} {
		value, found, err := provider.addSourceSecret(candidate.id)
		if err != nil {
			return "", false, fmt.Errorf("resolve secret %q: %w", candidate.id, err)
		}
		if !found {
			continue
		}
		authorization := string(value)
		if candidate.token {
			authorization = "basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+authorization))
		} else if strings.ContainsAny(authorization, "\r\n\x00") {
			return "", false, fmt.Errorf("secret %q contains a prohibited control character", candidate.id)
		}
		return authorization, true, nil
	}
	return "", false, nil
}

func isSSHGitRemote(remote string) bool {
	return strings.HasPrefix(remote, "ssh://") || strings.HasPrefix(remote, "git@")
}

func removeEnvironment(environment []string, names ...string) []string {
	removed := make(map[string]struct{}, len(names))
	for _, name := range names {
		removed[name] = struct{}{}
	}
	filtered := environment[:0]
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if _, remove := removed[name]; !remove {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}
