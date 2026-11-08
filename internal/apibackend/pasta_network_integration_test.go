package apibackend

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/strio-dev/coopr/api"
	"github.com/strio-dev/coopr/client"
)

// The dedicated rootless-pasta check provisions the resolver and requires every
// named child to pass. Other suites do not qualify Pasta by skipping this test.
func TestPublicClientPastaNetworkWorkload(t *testing.T) {
	if testing.Short() || os.Getenv("COOPR_TEST_PASTA_REQUIRED") != "1" {
		t.Skip("requires the explicit rootless-pasta resolver/network fixture")
	}
	if os.Geteuid() == 0 || !apiTestCGOEnabled {
		t.Fatal("Pasta qualification requires an unprivileged native CGO execution")
	}
	if _, err := exec.LookPath("busybox"); err != nil {
		t.Fatal("required static BusyBox fixture is unavailable", err)
	}
	host := os.Getenv("COOPR_TEST_PASTA_HOST")
	if address := net.ParseIP(host); address == nil || address.To4() == nil || address.IsLoopback() {
		t.Fatal("COOPR_TEST_PASTA_HOST must be the routable test-VLAN IPv4 address", host)
	}
	helperPackage, evidence := os.Getenv("COOPR_TEST_PASTA_PACKAGE"), os.Getenv("COOPR_TEST_PASTA_EVIDENCE")
	if !filepath.IsAbs(helperPackage) || !filepath.IsAbs(evidence) {
		t.Fatal("explicit helper package and evidence directory are required")
	}
	for _, variant := range []struct {
		name string
		jobs int
	}{{"serial", 1}, {"isolated-stage", 2}} {
		t.Run(variant.name, func(t *testing.T) {
			pastaNetworkVariant(t, host, helperPackage, evidence, variant.name, variant.jobs)
		})
	}
}

type pastaNetworkArrival struct {
	path, netNamespace, userNamespace string
	reply                             chan string
	disconnected                      chan struct{}
}

func pastaNetworkVariant(t *testing.T, host, helperPackage, evidence, name string, jobs int) {
	t.Helper()
	record := map[string]any{"status": "FAILED", "variant": name, "jobs": jobs, "uid": os.Geteuid(), "helperPackage": helperPackage}
	t.Cleanup(func() {
		if !t.Failed() {
			record["status"] = "PASS"
		}
		data, err := json.MarshalIndent(record, "", "  ")
		if err != nil {
			t.Error(err)
			return
		}
		file, err := os.OpenFile(filepath.Join(evidence, name+".json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			t.Error(err)
			return
		}
		_, writeErr := file.Write(append(data, '\n'))
		if err := errors.Join(writeErr, file.Close()); err != nil {
			t.Error(err)
		}
	})
	f := newNativeAPIFixture(t)
	c, _ := publicNativeClient(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	hostname := name + ".pasta-proof.test"
	hosts, err := os.ReadFile("/etc/hosts")
	if err != nil || strings.Contains(strings.ToLower(string(hosts)), "pasta-proof.test") {
		t.Fatal("fixture DNS must not be supplied by /etc/hosts", string(hosts), err)
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(host, "18081"))
	if err != nil {
		t.Fatal("listen on the secondary test VLAN", err)
	}
	arrivals := make(chan pastaNetworkArrival, 4)
	abort := make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || (r.URL.Path != "/proof" && r.URL.Path != "/block") {
			http.NotFound(w, r)
			return
		}
		requests.Add(1)
		arrival := pastaNetworkArrival{path: r.URL.Path, netNamespace: r.Header.Get("X-Pasta-Netns"), userNamespace: r.Header.Get("X-Pasta-Userns"), reply: make(chan string), disconnected: make(chan struct{})}
		select {
		case arrivals <- arrival:
		case <-r.Context().Done():
			return
		}
		select {
		case value := <-arrival.reply:
			_, _ = io.WriteString(w, value+"\n")
		case <-r.Context().Done():
			close(arrival.disconnected)
		case <-abort:
		}
	}))
	_ = server.Listener.Close()
	server.Listener = listener
	server.Config.ReadHeaderTimeout = 5 * time.Second
	server.Start()
	t.Cleanup(func() { close(abort); server.Close() })
	record["dnsHostname"], record["httpAddress"] = hostname, listener.Addr().String()
	definitionPath, pidPath := filepath.Join(f.context, "pasta.coopr"), filepath.Join(f.root, "pasta.pid")
	network := "pasta:--map-gw,--pid," + pidPath
	if strings.ContainsAny(pidPath, ",\r\n") {
		t.Fatal("private PID path cannot contain Pasta option separators")
	}
	request := func(t *testing.T, label, route string) api.Request {
		script := fmt.Sprintf(`set -eu
unset http_proxy HTTP_PROXY https_proxy HTTPS_PROXY all_proxy ALL_PROXY
export no_proxy='*' NO_PROXY='*'
if /bin/busybox grep -iF 'pasta-proof.test' /etc/hosts; then exit 1; fi
netns=$(/bin/busybox readlink /proc/self/ns/net)
userns=$(/bin/busybox readlink /proc/self/ns/user)
/bin/busybox wget -T 90 -q --header="X-Pasta-Netns: $netns" --header="X-Pasta-Userns: $userns" -O /http-proof %s
printf 'PASTA_NETWORK_RUN_EXECUTED\n'
`, strconv.Quote("http://"+net.JoinHostPort(hostname, "18081")+route))
		definition := fmt.Sprintf("from \"scratch\" as=\"result\" {\n copy \"busybox\" \"/bin/busybox\"\n run { exec \"/bin/busybox\" \"sh\" \"-euc\" %q; }\n}\n", script)
		if jobs == 2 {
			definition = fmt.Sprintf(`from "scratch" as="network-input" {
 copy "busybox" "/bin/busybox"
 run { exec "/bin/busybox" "sh" "-euc" %q; }
}
from "scratch" as="parallel-input" {
 copy "/parallel-proof" { file "parallel-proof" "isolated-stage-peer\n"; }
}
from "network-input" as="result" {
 copy "/parallel-proof" "/parallel-proof" from="parallel-input"
}
`, script)
		}
		publicNativeWrite(t, definitionPath, definition, 0600)
		r := f.request(label, "unused", "shared")
		r.Kind, r.Program, r.Build = "build", nil, &api.Build{Files: []string{definitionPath}, Target: "result"}
		r.Options.Isolation, r.Options.Network, r.Options.Jobs = "rootless", network, jobs
		r.Options.Cache.NoCache = true
		return r
	}
	var runs []map[string]any
	for _, phase := range []string{"fresh-1", "fresh-2", "cancel"} {
		if !t.Run(phase, func(t *testing.T) {
			cancelled := phase == "cancel"
			route := "/proof"
			if cancelled {
				route = "/block"
			}
			r := request(t, phase, route)
			runCtx, cancelRun := context.WithCancel(ctx)
			defer cancelRun()
			done := startPublicNative(runCtx, c, r)
			var arrival pastaNetworkArrival
			select {
			case arrival = <-arrivals:
			case response := <-done:
				t.Fatalf("Pasta build ended before real HTTP traffic: %v\n%s", response.err, response.logs)
			case <-ctx.Done():
				t.Fatal("Pasta HTTP request did not arrive", ctx.Err())
			}
			if arrival.path != route {
				t.Fatal("unexpected HTTP request", arrival.path, route)
			}
			observed := map[string]any{"phase": phase, "netNamespace": arrival.netNamespace, "userNamespace": arrival.userNamespace}
			runs = append(runs, observed)
			record["runs"] = runs
			for kind, actual := range map[string]string{"net": arrival.netNamespace, "user": arrival.userNamespace} {
				parent, err := os.Readlink("/proc/self/ns/" + kind)
				if err != nil || !strings.HasPrefix(actual, kind+":[") || actual == parent {
					t.Fatal("RUN did not prove a distinct namespace", kind, actual, parent, err)
				}
				observed["caller"+kind+"Namespace"] = parent
			}
			helper := observePastaHelper(t, pidPath, helperPackage, arrival.netNamespace)
			owned := pastaOwnedDescendants(t)
			owned[helper.PID] = helper
			stageObserved, planObserved := false, false
			for _, process := range owned {
				if len(process.Command) != 0 {
					stageObserved = stageObserved || process.Command[0] == "coopr-buildah-stage-worker-v1"
					planObserved = planObserved || process.Command[0] == "coopr-buildah-plan-worker-v1"
				}
			}
			if !planObserved || (jobs == 2 && !stageObserved) {
				t.Fatal("actual owning plan/isolated-stage worker was not observed", owned)
			}
			observed["helper"], observed["ownedProcesses"], observed["isolatedStageObserved"] = helper, owned, stageObserved
			if cancelled {
				cancelRun()
			} else {
				var value [16]byte
				if _, err := rand.Read(value[:]); err != nil {
					t.Fatal(err)
				}
				nonce := hex.EncodeToString(value[:])
				observed["responseNonce"] = nonce
				select {
				case arrival.reply <- nonce:
				case response := <-done:
					t.Fatalf("Pasta exited before its controlled response: %v\n%s", response.err, response.logs)
				case <-ctx.Done():
					t.Fatal("Pasta response release timed out", ctx.Err())
				}
			}
			var response publicNativeResponse
			select {
			case response = <-done:
			case <-ctx.Done():
				t.Fatal("Pasta execution did not finish", ctx.Err())
			}
			// Check immediately after Execute returns, before status requests or
			// filesystem reads can hide a short post-acknowledgement leak.
			for pid, process := range owned {
				current, err := readPastaProcessIdentity(pid)
				if err == nil && current.Start == process.Start {
					t.Fatal("owned process remained after cleanup acknowledgement", process)
				}
				if err != nil && !os.IsNotExist(err) {
					t.Fatal("cannot verify acknowledged process cleanup", pid, err)
				}
			}
			observed["logs"] = response.logs
			if cancelled {
				if !errors.Is(response.err, context.Canceled) || errors.Is(response.err, client.ErrCleanupFailed) {
					t.Fatal("Pasta cancellation did not confirm cleanup", response.err)
				}
				var execution *client.ExecutionError
				if !errors.As(response.err, &execution) || execution.ID == "" {
					t.Fatal("cancellation lost execution identity", response.err)
				}
				status, err := c.Get(ctx, execution.ID)
				if err != nil || status.State != "canceled" {
					t.Fatal("Pasta cancellation status", status, err)
				}
				for _, path := range []string{r.Options.Output.OCI, r.Options.Output.Filesystem} {
					if _, err := os.Lstat(path); !os.IsNotExist(err) {
						t.Fatal("canceled Pasta build promoted output", path, err)
					}
				}
			} else {
				if response.err != nil {
					t.Fatalf("Pasta execution: %v\n%s", response.err, response.logs)
				}
				proof := publicNativeRead(t, filepath.Join(response.result.Filesystem, "http-proof"))
				if proof != observed["responseNonce"].(string)+"\n" {
					t.Fatal("Pasta RUN reused stale output or lacked fresh execution", proof, response.logs)
				}
				if jobs == 2 && publicNativeRead(t, filepath.Join(response.result.Filesystem, "parallel-proof")) != "isolated-stage-peer\n" {
					t.Fatal("parallel required peer output was not consumed")
				}
			}
			if cancelled {
				select {
				case <-arrival.disconnected:
				case <-time.After(2 * time.Second):
					t.Fatal("acknowledged cancellation retained its active HTTP connection")
				}
			}
			if err := os.Remove(pidPath); err != nil && !os.IsNotExist(err) {
				t.Fatal("remove the completed fixture's own PID observation file", err)
			}
			observed["cleanupBeforeAcknowledgement"] = true
		}) {
			return
		}
	}
	if requests.Load() != 3 {
		t.Fatal("freshness/cancellation did not each contact the HTTP peer", requests.Load())
	}
	if runs[0]["responseNonce"] == runs[1]["responseNonce"] {
		t.Fatal("equivalent uncached executions did not receive distinct fresh responses")
	}
	record["httpRequests"], record["freshResponses"], record["cancelledConnectionClosed"] = requests.Load(), 2, true
}

type pastaProcessIdentity struct {
	PID              int      `json:"pid"`
	Parent           int      `json:"parent"`
	Start            string   `json:"start"`
	Name             string   `json:"name"`
	Command          []string `json:"command,omitempty"`
	UID              int      `json:"uid,omitempty"`
	Executable       string   `json:"executable,omitempty"`
	ExecutableSHA256 string   `json:"executableSHA256,omitempty"`
	ExecutableError  string   `json:"executableError,omitempty"`
	NetworkNamespace string   `json:"networkNamespace,omitempty"`
	NetworkError     string   `json:"networkError,omitempty"`
}

func readPastaProcessIdentity(pid int) (pastaProcessIdentity, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return pastaProcessIdentity{}, err
	}
	text := string(data)
	start, end := strings.IndexByte(text, '('), strings.LastIndexByte(text, ')')
	if start < 0 || end <= start {
		return pastaProcessIdentity{}, errors.New("malformed process stat")
	}
	fields := strings.Fields(text[end+1:])
	if len(fields) < 20 {
		return pastaProcessIdentity{}, errors.New("missing process start identity")
	}
	parent, err := strconv.Atoi(fields[1])
	return pastaProcessIdentity{PID: pid, Parent: parent, Start: fields[19], Name: text[start+1 : end]}, err
}

func observePastaHelper(t *testing.T, pidPath, expectedPackage, expectedNetwork string) pastaProcessIdentity {
	t.Helper()
	data, err := os.ReadFile(pidPath)
	if err != nil || len(data) > 32 {
		t.Fatal("Pasta did not write its controlled PID observation", string(data), err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		t.Fatal("invalid controlled Pasta PID", string(data), err)
	}
	process, err := readPastaProcessIdentity(pid)
	if err != nil {
		t.Fatal("Pasta was not active during HTTP work", err)
	}
	if process.Name != "pasta" && process.Name != "pasta.avx2" && process.Name != "passt" && process.Name != "passt.avx2" {
		t.Fatal("controlled PID did not identify a Pasta process", process)
	}
	data, err = os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		t.Fatal(err)
	}
	process.Command = strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
	if len(process.Command) == 0 || process.Command[0] != filepath.Join(expectedPackage, "bin", "pasta") {
		t.Fatal("executed helper is not the provisioned Pasta", process)
	}
	controlledPID, networkTarget := false, ""
	for i, argument := range process.Command {
		controlledPID = controlledPID || argument == "--pid" && i+1 < len(process.Command) && process.Command[i+1] == pidPath
		if argument == "--netns" && i+1 < len(process.Command) && strings.HasPrefix(process.Command[i+1], "/proc/") && strings.HasSuffix(process.Command[i+1], "/ns/net") {
			networkTarget = process.Command[i+1]
		}
	}
	if !controlledPID || networkTarget == "" {
		t.Fatal("observed Pasta is not attached to this controlled execution", process)
	}
	process.NetworkNamespace, err = os.Readlink(networkTarget)
	if os.IsPermission(err) {
		process.NetworkError = err.Error()
	} else if err != nil || process.NetworkNamespace != expectedNetwork {
		t.Fatal("Pasta target differs from the observed RUN network namespace", process, expectedNetwork, err)
	}
	data, err = os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatal(err)
	}
	uid := strconv.Itoa(os.Geteuid())
	matchedUID := false
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "Uid:" {
			matchedUID = fields[1] == uid && fields[2] == uid
		}
	}
	if !matchedUID {
		t.Fatal("Pasta is not running as the unprivileged caller", process, string(data))
	}
	process.UID = os.Geteuid()
	process.Executable, err = os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if os.IsPermission(err) {
		// Pasta can restrict ptrace-style proc links as part of its sandbox.
		// Record that restriction; package-qualified argv/PID/start/UID remain
		// required rather than disabling the sandbox to observe the link.
		process.ExecutableError = err.Error()
	} else if err != nil {
		t.Fatal(err)
	} else {
		if filepath.Dir(process.Executable) != filepath.Join(expectedPackage, "bin") {
			t.Fatal("Pasta executable escaped the provisioned package", process)
		}
		data, err := os.ReadFile(process.Executable)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		process.ExecutableSHA256 = hex.EncodeToString(sum[:])
	}
	current, err := readPastaProcessIdentity(pid)
	if err != nil || current.Start != process.Start {
		t.Fatal("Pasta identity changed while observed", process, current, err)
	}
	return process
}

func pastaOwnedDescendants(t *testing.T) map[int]pastaProcessIdentity {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	all := map[int]pastaProcessIdentity{}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		if process, err := readPastaProcessIdentity(pid); err == nil {
			all[pid] = process
		}
	}
	owned := map[int]pastaProcessIdentity{}
	for changed := true; changed; {
		changed = false
		for pid, process := range all {
			_, known := owned[pid]
			_, parentOwned := owned[process.Parent]
			if !known && (process.Parent == os.Getpid() || parentOwned) {
				if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
					process.Command = strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
				}
				owned[pid], changed = process, true
			}
		}
	}
	return owned
}
