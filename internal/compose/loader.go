package compose

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

var ComposeFilenames = []string{
	"compose.yaml",
	"compose.yml",
	"docker-compose.yml",
	"docker-compose.yaml",
}

// compose keys we accept but wslc cannot enforce (yet)
var unsupportedKeys = []struct{ key, message string }{
	{"healthcheck", "healthchecks are not supported by wslc; depends_on conditions fall back to 'started'"},
	{"cap_add", "capabilities are not configurable with wslc"},
	{"cap_drop", "capabilities are not configurable with wslc"},
	{"privileged", "privileged mode is not supported by wslc"},
	{"devices", "device mapping is not supported by wslc (see 'gpus' for GPUs)"},
	{"extra_hosts", "extra_hosts is not supported by wslc"},
	{"sysctls", "sysctls are not supported by wslc"},
	{"secrets", "secrets are not supported; use environment/env_file instead"},
	{"configs", "configs are not supported; use bind mounts instead"},
	{"init", "init is not supported by wslc"},
	{"pid", "pid mode is not supported by wslc"},
	{"ipc", "ipc mode is not supported by wslc"},
	{"read_only", "read_only rootfs is not supported by wslc"},
	{"security_opt", "security_opt is not supported by wslc"},
	{"logging", "logging drivers are not configurable with wslc"},
}

var winPathRE = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

// caseInsensitiveEnv mirrors Windows, where environment variable names are
// case-insensitive (PATH and Path are the same variable).
var caseInsensitiveEnv = runtime.GOOS == "windows"

func envKey(name string) string {
	if caseInsensitiveEnv {
		return strings.ToUpper(name)
	}
	return name
}

// ComposeError is a user-facing error in the compose file or its usage.
type ComposeError struct{ Msg string }

func (e *ComposeError) Error() string { return e.Msg }

func errorf(format string, args ...any) error {
	return &ComposeError{fmt.Sprintf(format, args...)}
}

// OverrideFilenames are loaded on top of an auto-detected compose file.
var OverrideFilenames = []string{
	"compose.override.yml",
	"compose.override.yaml",
	"docker-compose.override.yml",
	"docker-compose.override.yaml",
}

func FindComposeFile(directory string) string {
	return findFile(directory, ComposeFilenames)
}

func FindOverrideFile(directory string) string {
	return findFile(directory, OverrideFilenames)
}

func findFile(directory string, names []string) string {
	for _, name := range names {
		path := filepath.Join(directory, name)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
	}
	return ""
}

func LoadDotenv(path string) (map[string]string, error) {
	env := map[string]string{}
	data, err := os.ReadFile(path) // whole file: lines have no length limit
	if os.IsNotExist(err) {
		return env, nil
	}
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		key, value, _ := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if strings.HasPrefix(key, "export ") {
			key = strings.TrimSpace(key[len("export "):])
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && value[0] == value[len(value)-1] && (value[0] == '"' || value[0] == '\'') {
			value = value[1 : len(value)-1]
		}
		env[key] = value
	}
	return env, nil
}

var projectNameRE = regexp.MustCompile(`[^a-z0-9_-]`)

func NormalizeProjectName(raw string) (string, error) {
	name := strings.ToLower(strings.TrimSpace(raw))
	name = projectNameRE.ReplaceAllString(name, "")
	name = strings.TrimLeft(name, "_-")
	if name == "" {
		return "", errorf("cannot derive a valid project name from %q", raw)
	}
	return name, nil
}

func asCommand(v any) ([]string, error) {
	if s, ok := v.(string); ok {
		return shlexSplit(s)
	}
	return asList(v), nil
}

// asMapping accepts both list ("K=V") and mapping syntax for labels/build args.
func asMapping(v any, what string) (map[string]string, error) {
	result := map[string]string{}
	switch x := v.(type) {
	case nil:
	case *Mapping:
		for _, k := range x.Keys {
			result[k] = scalar(x.Values[k])
		}
	case []any:
		for _, item := range x {
			key, val, _ := strings.Cut(scalar(item), "=")
			result[key] = val
		}
	default:
		return nil, errorf("%s: expected list or mapping, got %s", what, kindOf(v))
	}
	return result, nil
}

func asEnvironment(v any) (map[string]*string, error) {
	result := map[string]*string{}
	switch x := v.(type) {
	case nil:
	case *Mapping:
		for _, k := range x.Keys {
			if item := x.Values[k]; item == nil {
				result[k] = nil
			} else {
				s := scalar(item)
				result[k] = &s
			}
		}
	case []any:
		for _, item := range x {
			key, val, found := strings.Cut(scalar(item), "=")
			if found {
				result[key] = &val
			} else {
				result[key] = nil
			}
		}
	default:
		return nil, errorf("environment: expected list or mapping, got %s", kindOf(v))
	}
	return result, nil
}

func ParsePort(spec any) ([]PortMapping, error) {
	if m, ok := spec.(*Mapping); ok { // long syntax
		target, err := strconv.Atoi(scalar(m.Get("target")))
		if err != nil {
			return nil, errorf("invalid port target: %s", kindOf(m.Get("target")))
		}
		pub := scalar(m.Get("published"))
		if hostIP := scalar(m.Get("host_ip")); hostIP != "" && pub != "" {
			pub = hostIP + ":" + pub
		}
		protocol := "tcp"
		if m.Has("protocol") {
			protocol = scalar(m.Get("protocol"))
		}
		return []PortMapping{{target, pub, protocol}}, nil
	}

	text := scalar(spec)
	protocol := "tcp"
	if i := strings.LastIndex(text, "/"); i >= 0 {
		text, protocol = text[:i], text[i+1:]
	}

	var host, target string
	hasHost := true
	parts := strings.Split(text, ":")
	switch len(parts) {
	case 1:
		target, hasHost = parts[0], false
	case 2:
		host, target = parts[0], parts[1]
	case 3:
		host, target = parts[0]+":"+parts[1], parts[2]
	default:
		return nil, errorf("invalid port mapping: %q", scalar(spec))
	}

	expand := func(rng string) ([]int, error) {
		lo, hi, isRange := strings.Cut(rng, "-")
		start, err := strconv.Atoi(lo)
		if err != nil || start < 0 || start > 65535 {
			return nil, errorf("invalid port mapping: %q", scalar(spec))
		}
		end := start
		if isRange {
			if end, err = strconv.Atoi(hi); err != nil || end < 0 || end > 65535 {
				return nil, errorf("invalid port mapping: %q", scalar(spec))
			}
		}
		var out []int
		for p := start; p <= end; p++ {
			out = append(out, p)
		}
		return out, nil
	}

	targets, err := expand(target)
	if err != nil {
		return nil, err
	}
	var result []PortMapping
	if !hasHost {
		for _, t := range targets {
			result = append(result, PortMapping{t, "", protocol})
		}
		return result, nil
	}
	ipPrefix, hostPorts := "", host
	if strings.Count(host, ":") == 1 { // ip:port form
		ip, ports, _ := strings.Cut(host, ":")
		ipPrefix, hostPorts = ip+":", ports
	}
	hosts, err := expand(hostPorts)
	if err != nil {
		return nil, err
	}
	if len(hosts) != len(targets) {
		return nil, errorf("port range mismatch in %q", scalar(spec))
	}
	for i, t := range targets {
		result = append(result, PortMapping{t, ipPrefix + strconv.Itoa(hosts[i]), protocol})
	}
	return result, nil
}

// splitVolumeSpec splits "src:dst[:opts]" respecting Windows drive letters (E:\x:/data).
func splitVolumeSpec(spec string) []string {
	parts := strings.Split(spec, ":")
	var merged []string
	for i := 0; i < len(parts); i++ {
		part := parts[i]
		if len(part) == 1 && isAlpha(part[0]) && i+1 < len(parts) &&
			(strings.HasPrefix(parts[i+1], `\`) || strings.HasPrefix(parts[i+1], "/")) {
			merged = append(merged, part+":"+parts[i+1])
			i++
		} else {
			merged = append(merged, part)
		}
	}
	return merged
}

func isAlpha(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

func isHostPath(source string) bool {
	for _, prefix := range []string{"/", "./", "../", "~", `\\`} {
		if strings.HasPrefix(source, prefix) {
			return true
		}
	}
	return source == "." || source == ".." || winPathRE.MatchString(source)
}

func ParseVolume(spec any, projectDir string) (*VolumeMount, error) {
	if m, ok := spec.(*Mapping); ok { // long syntax
		vtype := "volume"
		if m.Has("type") {
			vtype = scalar(m.Get("type"))
		}
		source := scalar(m.Get("source"))
		target := scalar(m.Get("target"))
		if target == "" {
			return nil, errorf("volume entry missing target (source %q)", source)
		}
		if vtype == "bind" && source != "" {
			source = resolveBindSource(source, projectDir)
		}
		return &VolumeMount{vtype, source, target, truthy(m.Get("read_only"))}, nil
	}

	text := scalar(spec)
	parts := splitVolumeSpec(text)
	var source, target, opts string
	switch len(parts) {
	case 1:
		return nil, errorf("anonymous volumes are not supported by wslc: %q; name the volume or use a bind mount", text)
	case 2:
		source, target = parts[0], parts[1]
	case 3:
		source, target, opts = parts[0], parts[1], parts[2]
	default:
		return nil, errorf("invalid volume spec: %q", text)
	}
	readOnly := false
	for _, opt := range strings.Split(opts, ",") {
		if opt == "ro" {
			readOnly = true
		}
	}
	if isHostPath(source) {
		return &VolumeMount{"bind", resolveBindSource(source, projectDir), target, readOnly}, nil
	}
	return &VolumeMount{"volume", source, target, readOnly}, nil
}

func isAbs(path string) bool {
	return filepath.IsAbs(path) || strings.HasPrefix(path, "/") || winPathRE.MatchString(path)
}

func resolveBindSource(source, projectDir string) string {
	if winPathRE.MatchString(source) || strings.HasPrefix(source, `\\`) {
		return source // already a Windows path, hand to wslc as-is
	}
	if source == "~" || strings.HasPrefix(source, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			source = filepath.Join(home, source[1:])
		}
	}
	if !isAbs(source) {
		source = filepath.Join(projectDir, source)
	}
	return source
}

func parseDependsOn(v any) ([]string, []string, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil, nil
	case []any:
		return asList(x), nil, nil
	case *Mapping:
		var deps, warnings []string
		for _, name := range x.Keys {
			deps = append(deps, name)
			condition := "service_started"
			if cfg := asMap(x.Values[name]); cfg.Has("condition") {
				condition = scalar(cfg.Get("condition"))
			}
			if condition != "service_started" {
				warnings = append(warnings, fmt.Sprintf(
					"depends_on condition %q on %q is treated as service_started", condition, name))
			}
		}
		return deps, warnings, nil
	}
	return nil, nil, errorf("depends_on: expected list or mapping")
}

func parseBuild(v any, projectDir string) (*BuildConfig, error) {
	if s, ok := v.(string); ok {
		return &BuildConfig{Context: resolveBindSource(s, projectDir)}, nil
	}
	m, err := expectMap(v, "build")
	if err != nil {
		return nil, err
	}
	context := "."
	if m.Has("context") {
		context = scalar(m.Get("context"))
	}
	args, err := asMapping(m.Get("args"), "build.args")
	if err != nil {
		return nil, err
	}
	return &BuildConfig{
		Context:    resolveBindSource(context, projectDir),
		Dockerfile: scalar(m.Get("dockerfile")),
		Args:       args,
		Target:     scalar(m.Get("target")),
		Pull:       truthy(m.Get("pull")),
	}, nil
}

func LoadProject(composeFile, projectName, envFile string) (*Project, error) {
	return LoadProjectFiles([]string{composeFile}, projectName, envFile)
}

// LoadProjectFiles loads compose files merged in order. Relative paths in
// every file resolve against the directory of the first one.
func LoadProjectFiles(composeFiles []string, projectName, envFile string) (*Project, error) {
	if len(composeFiles) == 0 {
		return nil, errorf("no compose file given")
	}
	files := make([]string, len(composeFiles))
	for i, f := range composeFiles {
		abs, err := filepath.Abs(f)
		if err != nil {
			return nil, err
		}
		files[i] = abs
	}
	composeFile := files[0]
	projectDir := filepath.Dir(composeFile)

	dotenvPath := envFile
	if dotenvPath == "" {
		dotenvPath = filepath.Join(projectDir, ".env")
	}
	dotenv, err := LoadDotenv(dotenvPath)
	if err != nil {
		return nil, err
	}
	env := map[string]string{}
	for k, v := range dotenv {
		env[envKey(k)] = v
	}
	for _, kv := range os.Environ() { // process env wins
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			env[envKey(k)] = v
		}
	}

	var trees []any
	for i, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		tree, err := parseYAML(data)
		if err != nil {
			return nil, errorf("%s: %v", file, err)
		}
		if _, ok := tree.(*Mapping); !ok {
			switch {
			case i == 0:
				return nil, errorf("%s: no 'services' section found", file)
			case tree != nil:
				return nil, errorf("%s: expected a mapping at the top level, got %s", file, kindOf(tree))
			}
			continue // an empty override file
		}
		trees = append(trees, tree)
	}
	tree := mergeFiles(trees)
	if raw, ok := tree.(*Mapping); !ok || !raw.Has("services") {
		return nil, errorf("%s: no 'services' section found", composeFile)
	}
	interpolated, err := InterpolateTree(tree, env)
	if err != nil {
		return nil, err
	}
	raw := interpolated.(*Mapping)

	nameSource := projectName
	if nameSource == "" {
		nameSource = env[envKey("COMPOSE_PROJECT_NAME")]
	}
	if nameSource == "" {
		nameSource = scalar(raw.Get("name"))
	}
	if nameSource == "" {
		nameSource = filepath.Base(projectDir)
	}
	name, err := NormalizeProjectName(nameSource)
	if err != nil {
		return nil, err
	}
	project := NewProject(name, projectDir)

	// --- top level networks & volumes -------------------------------------
	networks, err := expectMap(raw.Get("networks"), "networks")
	if err != nil {
		return nil, err
	}
	for _, key := range networks.Keys {
		cfg, err := expectMap(networks.Values[key], "networks."+key)
		if err != nil {
			return nil, err
		}
		netName, external := resourceName(name, key, cfg)
		project.Networks[key] = &Network{key, netName, external}
	}
	volumes, err := expectMap(raw.Get("volumes"), "volumes")
	if err != nil {
		return nil, err
	}
	for _, key := range volumes.Keys {
		cfg, err := expectMap(volumes.Values[key], "volumes."+key)
		if err != nil {
			return nil, err
		}
		volName, external := resourceName(name, key, cfg)
		project.Volumes[key] = &Volume{key, volName, external}
	}

	// --- services ----------------------------------------------------------
	services, err := expectMap(raw.Get("services"), "services")
	if err != nil {
		return nil, err
	}
	for _, svcName := range services.Keys {
		svc, err := loadService(project, svcName, services.Values[svcName])
		if err != nil {
			return nil, err
		}
		project.Services[svcName] = svc
	}
	return project, nil
}

// resourceName resolves the wslc name of a top-level network or volume.
func resourceName(project, key string, cfg *Mapping) (string, bool) {
	external := truthy(cfg.Get("external"))
	if name := scalar(cfg.Get("name")); name != "" {
		return name, external
	}
	if external {
		return key, true
	}
	return project + "_" + key, false
}

func loadService(project *Project, svcName string, cfgRaw any) (*Service, error) {
	cfg, ok := cfgRaw.(*Mapping)
	if !ok {
		if cfgRaw == nil {
			return nil, errorf("service %q is empty", svcName)
		}
		return nil, errorf("services.%s: expected a mapping, got %s", svcName, kindOf(cfgRaw))
	}
	projectDir := project.Directory
	svc := &Service{Name: svcName, Replicas: 1}
	field := func(key string) string { return "services." + svcName + "." + key }
	var err error

	for _, u := range unsupportedKeys {
		if cfg.Has(u.key) {
			project.Warnings = append(project.Warnings, fmt.Sprintf("%s: %s (ignoring '%s')", svcName, u.message, u.key))
		}
	}

	if err := checkFlatFields(cfg, field); err != nil {
		return nil, err
	}

	svc.Image = scalar(cfg.Get("image"))
	if cfg.Has("build") {
		if svc.Build, err = parseBuild(cfg.Get("build"), projectDir); err != nil {
			return nil, err
		}
	}
	if svc.Image == "" && svc.Build == nil {
		return nil, errorf("service %q needs 'image' or 'build'", svcName)
	}

	if svc.Command, err = asCommand(cfg.Get("command")); err != nil {
		return nil, errorf("%s: %v", field("command"), err)
	}
	if svc.Entrypoint, err = asCommand(cfg.Get("entrypoint")); err != nil {
		return nil, errorf("%s: %v", field("entrypoint"), err)
	}
	svc.ContainerName = scalar(cfg.Get("container_name"))
	if svc.Environment, err = asEnvironment(cfg.Get("environment")); err != nil {
		return nil, err
	}
	if svc.EnvFiles, err = loadEnvFiles(cfg.Get("env_file"), projectDir, field("env_file")); err != nil {
		return nil, err
	}
	ports, err := expectList(cfg.Get("ports"), field("ports"))
	if err != nil {
		return nil, err
	}
	for _, spec := range ports {
		mappings, err := ParsePort(spec)
		if err != nil {
			return nil, err
		}
		svc.Ports = append(svc.Ports, mappings...)
	}
	volumes, err := expectList(cfg.Get("volumes"), field("volumes"))
	if err != nil {
		return nil, err
	}
	for _, spec := range volumes {
		mount, err := ParseVolume(spec, projectDir)
		if err != nil {
			return nil, err
		}
		svc.Volumes = append(svc.Volumes, mount)
	}
	svc.Tmpfs = asList(cfg.Get("tmpfs"))

	// networks: list or mapping (with aliases); default network otherwise
	var keys []string
	aliases := map[string][]string{}
	switch x := cfg.Get("networks").(type) {
	case nil:
		keys = []string{"default"}
	case []any:
		keys = asList(x)
	case *Mapping:
		keys = x.Keys // declaration order: the first network is the one attached
		for _, k := range x.Keys {
			aliases[k] = asList(asMap(x.Values[k]).Get("aliases"))
		}
	default:
		return nil, errorf("%s: expected list or mapping", field("networks"))
	}
	svc.NetworkAliases = map[string][]string{}
	for _, key := range keys {
		if _, ok := project.Networks[key]; !ok {
			if key != "default" {
				return nil, errorf("service %q references undefined network %q", svcName, key)
			}
			project.Networks["default"] = &Network{"default", project.Name + "_default", false}
		}
		net := project.Networks[key]
		svc.Networks = append(svc.Networks, net.Name)
		svc.NetworkAliases[net.Name] = aliases[key]
	}

	// named volumes must be declared
	for _, mount := range svc.Volumes {
		if mount.Type != "volume" {
			continue
		}
		vol, ok := project.Volumes[mount.Source]
		if !ok {
			return nil, errorf("service %q references undefined volume %q", svcName, mount.Source)
		}
		mount.Source = vol.Name
	}

	deps, depWarnings, err := parseDependsOn(cfg.Get("depends_on"))
	if err != nil {
		return nil, err
	}
	svc.DependsOn = deps
	for _, w := range depWarnings {
		project.Warnings = append(project.Warnings, svcName+": "+w)
	}

	svc.Hostname = scalar(cfg.Get("hostname"))
	svc.Domainname = scalar(cfg.Get("domainname"))
	svc.DNS = asList(cfg.Get("dns"))
	svc.DNSSearch = asList(cfg.Get("dns_search"))
	svc.DNSOpt = asList(cfg.Get("dns_opt"))
	svc.User = scalar(cfg.Get("user"))
	svc.WorkingDir = scalar(cfg.Get("working_dir"))
	if svc.Labels, err = asMapping(cfg.Get("labels"), "labels"); err != nil {
		return nil, err
	}
	svc.StopSignal = scalar(cfg.Get("stop_signal"))
	svc.ShmSize = scalar(cfg.Get("shm_size"))
	svc.StdinOpen = truthy(cfg.Get("stdin_open"))
	svc.TTY = truthy(cfg.Get("tty"))
	svc.Profiles = asList(cfg.Get("profiles"))
	if restart := cfg.Get("restart"); truthy(restart) {
		svc.Restart = scalar(restart)
		if svc.Restart != "no" && svc.Restart != `"no"` {
			project.Warnings = append(project.Warnings, fmt.Sprintf(
				"%s: restart policies are not supported by wslc yet (ignoring 'restart: %s')", svcName, svc.Restart))
		}
	}

	ulimits, err := expectMap(cfg.Get("ulimits"), field("ulimits"))
	if err != nil {
		return nil, err
	}
	for _, item := range ulimits.Keys {
		if limit, ok := ulimits.Values[item].(*Mapping); ok {
			soft, hard := "-1", "-1"
			if limit.Has("soft") {
				soft = scalar(limit.Get("soft"))
			}
			if limit.Has("hard") {
				hard = scalar(limit.Get("hard"))
			}
			svc.Ulimits = append(svc.Ulimits, fmt.Sprintf("%s=%s:%s", item, soft, hard))
		} else {
			svc.Ulimits = append(svc.Ulimits, fmt.Sprintf("%s=%s", item, scalar(ulimits.Values[item])))
		}
	}

	if err := loadResources(svc, cfg, field); err != nil {
		return nil, err
	}
	return svc, nil
}

// flatPaths are the service values read with scalar or asList, which would
// render a nested mapping or list as Go syntax. "*" matches the values of a
// mapping and the items of a list; "#" only the items of a list; "[]" the
// items of a list, or the value itself when it is not a list. A list where a
// named key is expected is an error.
var flatPaths = []string{
	"image", "container_name", "hostname", "domainname", "user", "working_dir",
	"stop_signal", "shm_size", "restart", "mem_limit", "cpus",
	"command[]", "entrypoint[]", "env_file.*.path", "tmpfs[]", "dns[]", "dns_search[]", "dns_opt[]", "profiles[]",
	"environment.*", "labels.*",
	"build.context", "build.dockerfile", "build.target", "build.args.*",
	"ports.*.target", "ports.*.published", "ports.*.host_ip", "ports.*.protocol",
	"volumes.*.type", "volumes.*.source", "volumes.*.target",
	"networks.#", "networks.*.aliases[]", "depends_on.#", "depends_on.*.condition", "ulimits.*.soft", "ulimits.*.hard",
	"deploy.resources.limits.memory", "deploy.resources.limits.cpus",
	"deploy.resources.reservations.devices.*.capabilities[]", "deploy.resources.reservations.devices.*.count",
	"gpus.*.count",
}

func checkFlatFields(cfg *Mapping, field func(string) string) error {
	for _, path := range flatPaths {
		segments := strings.Split(strings.ReplaceAll(path, "[]", ".[]"), ".")
		if err := checkFlat(cfg.Get(segments[0]), segments[1:], field(segments[0])); err != nil {
			return err
		}
	}
	return nil
}

func checkFlat(v any, path []string, where string) error {
	if len(path) == 0 {
		switch v.(type) {
		case *Mapping, []any:
			return errorf("%s: expected a string, got %s", where, kindOf(v))
		}
		return nil
	}
	switch x := v.(type) {
	case []any:
		switch path[0] {
		case "*", "#", "[]":
			for i, item := range x {
				if err := checkFlat(item, path[1:], fmt.Sprintf("%s[%d]", where, i)); err != nil {
					return err
				}
			}
			return nil
		}
		return errorf("%s: expected a string or a mapping, got a list", where)
	case *Mapping:
		switch path[0] {
		case "#":
			return nil
		case "[]":
			return checkFlat(v, path[1:], where)
		case "*":
			for _, k := range x.Keys {
				if err := checkFlat(x.Values[k], path[1:], where+"."+k); err != nil {
					return err
				}
			}
			return nil
		}
		return checkFlat(x.Get(path[0]), path[1:], where+"."+path[0])
	}
	if path[0] == "[]" {
		return checkFlat(v, path[1:], where)
	}
	return nil
}

// loadEnvFiles reads env_file: a path, or a list of paths and of {path,
// required} mappings. A missing file that is not required is left out.
func loadEnvFiles(v any, projectDir, where string) ([]string, error) {
	items, isList := v.([]any)
	switch v.(type) {
	case nil:
	case *Mapping:
		return nil, errorf("%s: expected a string or a list, got a mapping", where)
	default:
		if !isList {
			items = []any{v}
		}
	}
	var files []string
	for _, item := range items {
		path, required := scalar(item), true
		if m, ok := item.(*Mapping); ok {
			path = scalar(m.Get("path"))
			required = !m.Has("required") || truthy(m.Get("required"))
		}
		if path == "" {
			return nil, errorf("%s: missing path", where)
		}
		if !isAbs(path) {
			path = filepath.Join(projectDir, path)
		}
		if _, err := os.Stat(path); err != nil && !required {
			continue
		}
		files = append(files, path)
	}
	return files, nil
}

// loadResources reads v2 style limits, deploy.resources and gpus.
func loadResources(svc *Service, cfg *Mapping, field func(string) string) error {
	svc.MemLimit = scalar(cfg.Get("mem_limit"))
	svc.CPUs = scalar(cfg.Get("cpus"))
	deploy, err := expectMap(cfg.Get("deploy"), field("deploy"))
	if err != nil {
		return err
	}
	resources, err := expectMap(deploy.Get("resources"), field("deploy.resources"))
	if err != nil {
		return err
	}
	limits, err := expectMap(resources.Get("limits"), field("deploy.resources.limits"))
	if err != nil {
		return err
	}
	if v := limits.Get("memory"); v != nil {
		svc.MemLimit = scalar(v)
	}
	if v := limits.Get("cpus"); v != nil {
		svc.CPUs = scalar(v)
	}
	if v := deploy.Get("replicas"); v != nil {
		n, err := strconv.Atoi(scalar(v))
		if err != nil || n < 0 {
			return errorf("%s: expected a non-negative integer, got %s", field("deploy.replicas"), kindOf(v))
		}
		svc.Replicas = n
	}
	reservations, err := expectMap(resources.Get("reservations"), field("deploy.resources.reservations"))
	if err != nil {
		return err
	}
	devices, err := expectList(reservations.Get("devices"), field("deploy.resources.reservations.devices"))
	if err != nil {
		return err
	}
	for _, d := range devices {
		device := asMap(d)
		for _, capability := range asList(device.Get("capabilities")) {
			if capability != "gpu" {
				continue
			}
			svc.GPUs = "all"
			if count := device.Get("count"); count != nil && scalar(count) != "all" {
				svc.GPUs = scalar(count)
			}
		}
	}
	switch gpus := cfg.Get("gpus").(type) {
	case []any: // device requests, like deploy.resources.reservations.devices
		svc.GPUs = "all"
		for _, d := range gpus {
			if count := asMap(d).Get("count"); count != nil && scalar(count) != "all" {
				svc.GPUs = scalar(count)
			}
		}
	default: // shorthand
		if truthy(gpus) {
			svc.GPUs = scalar(gpus)
		}
	}
	return nil
}
