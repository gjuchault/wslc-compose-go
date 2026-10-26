// Package compose loads, interpolates and normalizes compose files into a
// Project, and turns that model into wslc argument vectors.
package compose

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Labels set on every container so state can be recovered from wslc alone.
const (
	LabelProject    = "com.wslc-compose.project"
	LabelService    = "com.wslc-compose.service"
	LabelIndex      = "com.wslc-compose.container-number"
	LabelConfigHash = "com.wslc-compose.config-hash"
	LabelOneOff     = "com.wslc-compose.oneoff" // "True" on containers created by `run`
)

// Healthcheck is the container health test; wslc runs Test with the shell.
type Healthcheck struct {
	Test        string `yaml:"test,omitempty"` // empty: keep the image's test
	Disable     bool   `yaml:"disable,omitempty"`
	Interval    string `yaml:"interval,omitempty"`
	Timeout     string `yaml:"timeout,omitempty"`
	StartPeriod string `yaml:"start_period,omitempty"`
	Retries     int    `yaml:"retries,omitempty"`
}

// depends_on conditions other than service_started, which needs no wait.
const (
	ConditionHealthy   = "service_healthy"
	ConditionCompleted = "service_completed_successfully"
)

type BuildConfig struct {
	Context    string            `yaml:"context"`
	Dockerfile string            `yaml:"dockerfile,omitempty"`
	Args       map[string]string `yaml:"args,omitempty"`
	Target     string            `yaml:"target,omitempty"`
	Pull       bool              `yaml:"pull,omitempty"`
}

// VolumeMount is a -v entry: either a bind mount or a named volume.
type VolumeMount struct {
	Type     string `yaml:"type"`             // "bind" | "volume" | "tmpfs"
	Source   string `yaml:"source,omitempty"` // host path or volume name (empty for tmpfs)
	Target   string `yaml:"target"`
	ReadOnly bool   `yaml:"read_only,omitempty"`
}

type PortMapping struct {
	Target    int    `yaml:"target"`
	Published string `yaml:"published,omitempty"` // empty (random), a port, or "ip:port"
	Protocol  string `yaml:"protocol"`
}

// Flag renders the mapping as a `wslc run -p` value.
func (p PortMapping) Flag() string {
	spec := strconv.Itoa(p.Target)
	if p.Published != "" {
		spec = p.Published + ":" + spec
	}
	if p.Protocol != "" && p.Protocol != "tcp" {
		spec += "/" + p.Protocol
	}
	return spec
}

type Service struct {
	Name           string              `yaml:"name"`
	Image          string              `yaml:"image,omitempty"`
	Build          *BuildConfig        `yaml:"build,omitempty"`
	Command        []string            `yaml:"command,omitempty"`
	Entrypoint     []string            `yaml:"entrypoint,omitempty"`
	ContainerName  string              `yaml:"container_name,omitempty"`
	Environment    map[string]*string  `yaml:"environment,omitempty"` // nil value: pass through from host
	EnvFiles       []string            `yaml:"env_files,omitempty"`
	Ports          []PortMapping       `yaml:"ports,omitempty"`
	Volumes        []*VolumeMount      `yaml:"volumes,omitempty"`
	Tmpfs          []string            `yaml:"tmpfs,omitempty"`
	Networks       []string            `yaml:"networks,omitempty"` // project-resolved network names
	NetworkAliases map[string][]string `yaml:"network_aliases,omitempty"`
	DependsOn      []string            `yaml:"depends_on,omitempty"`
	// DependsOnConditions holds the dependencies to wait for, by condition.
	// It does not change the container, so it is left out of ConfigHash.
	DependsOnConditions map[string]string `yaml:"depends_on_conditions,omitempty" json:"-"`
	// Healthcheck is omitted from ConfigHash when unset, so that the hash
	// of existing services without one does not change.
	Healthcheck *Healthcheck      `yaml:"healthcheck,omitempty" json:",omitempty"`
	Hostname    string            `yaml:"hostname,omitempty"`
	Domainname  string            `yaml:"domainname,omitempty"`
	DNS         []string          `yaml:"dns,omitempty"`
	DNSSearch   []string          `yaml:"dns_search,omitempty"`
	DNSOpt      []string          `yaml:"dns_opt,omitempty"`
	User        string            `yaml:"user,omitempty"`
	WorkingDir  string            `yaml:"working_dir,omitempty"`
	Labels      map[string]string `yaml:"labels,omitempty"`
	MemLimit    string            `yaml:"mem_limit,omitempty"`
	CPUs        string            `yaml:"cpus,omitempty"`
	ShmSize     string            `yaml:"shm_size,omitempty"`
	Ulimits     []string          `yaml:"ulimits,omitempty"`
	StopSignal  string            `yaml:"stop_signal,omitempty"`
	GPUs        string            `yaml:"gpus,omitempty"`
	StdinOpen   bool              `yaml:"stdin_open,omitempty"`
	TTY         bool              `yaml:"tty,omitempty"`
	Replicas    int               `yaml:"replicas"`
	Profiles    []string          `yaml:"profiles,omitempty"`
	Restart     string            `yaml:"restart,omitempty"` // accepted but not enforceable by wslc yet
}

// ConfigHash fingerprints the service definition to detect config drift.
func (s *Service) ConfigHash() string {
	blob, err := json.Marshal(s) // map keys are emitted sorted
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(blob)
	return hex.EncodeToString(sum[:])[:16]
}

type Network struct {
	Key      string `yaml:"key"`  // name used in the compose file
	Name     string `yaml:"name"` // actual wslc network name
	External bool   `yaml:"external"`
}

type Volume struct {
	Key      string `yaml:"key"`
	Name     string `yaml:"name"`
	External bool   `yaml:"external"`
}

type Project struct {
	Name      string              `yaml:"name"`
	Directory string              `yaml:"directory"`
	Services  map[string]*Service `yaml:"services"`
	Networks  map[string]*Network `yaml:"networks"`
	Volumes   map[string]*Volume  `yaml:"volumes"`
	Warnings  []string            `yaml:"warnings"`
}

func NewProject(name, directory string) *Project {
	return &Project{
		Name:      name,
		Directory: directory,
		Services:  map[string]*Service{},
		Networks:  map[string]*Network{},
		Volumes:   map[string]*Volume{},
		Warnings:  []string{},
	}
}

func (p *Project) ContainerName(s *Service, index int) string {
	if s.ContainerName != "" {
		return s.ContainerName
	}
	return fmt.Sprintf("%s-%s-%d", p.Name, s.Name, index)
}

// SortedServices returns services in dependency order (depends_on first).
// With names, only those services and their transitive dependencies are kept.
func (p *Project) SortedServices(names []string) ([]*Service, error) {
	selected := map[string]bool{}
	if len(names) == 0 {
		for name := range p.Services {
			selected[name] = true
		}
	} else {
		stack := append([]string(nil), names...)
		for _, n := range names {
			selected[n] = true
		}
		for len(stack) > 0 {
			name := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			svc, ok := p.Services[name]
			if !ok {
				continue
			}
			for _, dep := range svc.DependsOn {
				if !selected[dep] {
					selected[dep] = true
					stack = append(stack, dep)
				}
			}
		}
	}

	const visiting, done = 1, 2
	state := map[string]int{}
	var order []*Service
	var visit func(name string, chain []string) error
	visit = func(name string, chain []string) error {
		svc, ok := p.Services[name]
		if !ok || !selected[name] {
			return nil
		}
		switch state[name] {
		case done:
			return nil
		case visiting:
			return fmt.Errorf("circular depends_on: %s", strings.Join(append(chain, name), " -> "))
		}
		state[name] = visiting
		for _, dep := range svc.DependsOn {
			if err := visit(dep, append(append([]string(nil), chain...), name)); err != nil {
				return err
			}
		}
		state[name] = done
		order = append(order, svc)
		return nil
	}

	for _, name := range sortedKeys(selected) {
		if err := visit(name, nil); err != nil {
			return nil, err
		}
	}
	return order, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
