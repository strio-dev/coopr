// Package api defines Coopr's versioned, transport-safe execution contract.
// It has no native engine dependencies and is safe for independent clients.
package api

import (
	"encoding/json"
	"time"
)

const Version = 1
const MaxRequestBytes = 4 << 20
const MaxEventBytes = 1 << 20

type Info struct {
	APIVersion   int      `json:"apiVersion"`
	Version      string   `json:"version"`
	Capabilities []string `json:"capabilities"`
}
type Request struct {
	ComponentLoad *ComponentLoad `json:"componentLoad,omitempty"`
	Kind          string         `json:"kind"`
	Program       *Program       `json:"program,omitempty"`
	Build         *Build         `json:"build,omitempty"`
	Publish       *Publish       `json:"publish,omitempty"`
	Options       Options        `json:"options"`
}
type Program struct {
	Platform string  `json:"platform,omitempty"`
	States   []State `json:"states"`
	Output   string  `json:"output"`
}
type State struct {
	ID         string      `json:"id"`
	From       Source      `json:"from"`
	Operations []Operation `json:"operations,omitempty"`
}
type Source struct {
	Context string `json:"context,omitempty"`
	Image   string `json:"image,omitempty"`
	State   string `json:"state,omitempty"`
}
type Operation struct {
	Kind        string   `json:"kind"`
	Args        []string `json:"args,omitempty"`
	Mounts      []Mount  `json:"mounts,omitempty"`
	Sources     []string `json:"sources,omitempty"`
	Destination string   `json:"destination,omitempty"`
	From        Source   `json:"from,omitempty"`
	Name        string   `json:"name,omitempty"`
	Value       string   `json:"value,omitempty"`
	Path        string   `json:"path,omitempty"`
}
type Mount struct {
	HostPath string `json:"hostPath,omitempty"`
	Relabel  string `json:"relabel,omitempty"`
	Type     string `json:"type"`
	Target   string `json:"target"`
	Source   string `json:"source,omitempty"`
	ID       string `json:"id,omitempty"`
	Sharing  string `json:"sharing,omitempty"`
	From     Source `json:"from,omitempty"`
	ReadOnly bool   `json:"readOnly,omitempty"`
}
type Store struct {
	RunRoot       string   `json:"runRoot"`
	GraphRoot     string   `json:"graphRoot"`
	Driver        string   `json:"driver,omitempty"`
	DriverOptions []string `json:"driverOptions,omitempty"`
}

// Output selects optional image and filesystem exports. With OCI empty, image
// execute requests retain their native image without serializing an OCI layout.
// Native definition builds and components still require OCI output.
type Output struct {
	OCI        string       `json:"oci,omitempty"`
	Filesystem string       `json:"filesystem,omitempty"`
	Exports    []FileExport `json:"exports,omitempty"`
}

// FileExport selects a literal path from a typed program state, or the final
// output when State is empty. It also captures an ordinary failed RUN there.
// Destinations are fresh, caller-owned paths, separate from image identity.
type FileExport struct {
	State       string `json:"state,omitempty"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
}

type ExportResult struct {
	State       string `json:"state,omitempty"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Status      string `json:"status"` // passed, failed or skipped
	Error       string `json:"error,omitempty"`
}
type Cache struct {
	MountDir     string   `json:"mountDir,omitempty"`
	LocalDir     string   `json:"localDir,omitempty"`
	NoCache      bool     `json:"noCache,omitempty"`
	RegistryFrom []string `json:"registryFrom,omitempty"`
	RegistryTo   []string `json:"registryTo,omitempty"`
}
type Registry struct {
	AuthFile            string `json:"authFile,omitempty"`
	CertDir             string `json:"certDir,omitempty"`
	PullPolicy          string `json:"pullPolicy,omitempty"`
	SignaturePolicyPath string `json:"signaturePolicyPath,omitempty"`
	TLSVerify           *bool  `json:"tlsVerify,omitempty"`
}
type Options struct {
	Contexts   map[string]string `json:"contexts,omitempty"`
	Store      Store             `json:"store"`
	ContextDir string            `json:"contextDir,omitempty"`
	Runtime    string            `json:"runtime,omitempty"`
	Isolation  string            `json:"isolation,omitempty"`
	Network    string            `json:"network,omitempty"`
	Jobs       int               `json:"jobs,omitempty"`
	Output     Output            `json:"output"`
	Cache      Cache             `json:"cache"`
	Registry   Registry          `json:"registry"`
}
type Build struct {
	Platform          string            `json:"platform,omitempty"`
	Files             []string          `json:"files"`
	Target            string            `json:"target,omitempty"`
	Args              map[string]string `json:"args,omitempty"`
	ComponentStoreDir string            `json:"componentStoreDir,omitempty"`
}
type ComponentLoad struct {
	Name              string `json:"name,omitempty"`
	Archive           string `json:"archive"`
	ComponentStoreDir string `json:"componentStoreDir"`
}

type Publish struct {
	Artifact          string   `json:"artifact"`
	Source            string   `json:"source"`
	SourceDigest      string   `json:"sourceDigest,omitempty"`
	Destinations      []string `json:"destinations"`
	ComponentStoreDir string   `json:"componentStoreDir,omitempty"`
}

// CacheStats counts Coopr portable-cache decisions, not all native reuse.
type CacheStats struct{ Hits, Misses, Stored, Skipped, Errors int }
type Destination struct {
	Destination string `json:"destination"`
	Reference   string `json:"reference,omitempty"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
}
type Result struct {
	ProgressDropped uint64                     `json:"progressDropped,omitempty"`
	ImageID         string                     `json:"imageID,omitempty"`
	ManifestDigest  string                     `json:"manifestDigest,omitempty"`
	Platform        string                     `json:"platform,omitempty"`
	OCI             string                     `json:"oci,omitempty"`
	Filesystem      string                     `json:"filesystem,omitempty"`
	Reference       string                     `json:"reference,omitempty"`
	Cache           *CacheStats                `json:"cache,omitempty"`
	Metadata        map[string]json.RawMessage `json:"metadata,omitempty"`
	Destinations    []Destination              `json:"destinations,omitempty"`
	Exports         []ExportResult             `json:"exports,omitempty"`
}
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

type Event struct {
	Type     string             `json:"type"`
	Data     []byte             `json:"data,omitempty"`
	Progress *OperationProgress `json:"progress,omitempty"`
	Result   *Result            `json:"result,omitempty"`
	Error    *Error             `json:"error,omitempty"`
}

// OperationProgress describes observed execution work without affecting cache
// identity or exposing an execution backend's private event types.
type OperationProgress struct {
	ID            string     `json:"id"`
	Stage         string     `json:"stage,omitempty"`
	Instruction   string     `json:"instruction,omitempty"`
	Operation     int        `json:"operation,omitempty"`
	Phase         string     `json:"phase"`
	State         string     `json:"state"`
	Cache         string     `json:"cache,omitempty"`
	Started       time.Time  `json:"started"`
	Completed     *time.Time `json:"completed,omitempty"`
	DurationNanos int64      `json:"durationNanos,omitempty"`
	Error         string     `json:"error,omitempty"`
}
type Status struct {
	ID     string  `json:"id"`
	State  string  `json:"state"`
	Result *Result `json:"result,omitempty"`
	Error  *Error  `json:"error,omitempty"`
}
