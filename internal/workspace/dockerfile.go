package workspace

import (
	"errors"
	"net"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const MaxDockerfileBytes = 1 << 20
const generationImageNamespace = "mcpws-generation-v1"

var ErrDockerfilePolicy = errors.New("Dockerfile syntax or image source is unsupported")

// BuildPlan is constructed only by the compiler. The adapter receives the
// compiled text, never the original unchecked source or caller context paths.
type BuildPlan struct {
	dockerfile string
	sources    []string
}

func (p BuildPlan) DockerfileContents() string { return p.dockerfile }
func (p BuildPlan) ImageSources() []string     { return append([]string(nil), p.sources...) }

var variableName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
var repositoryPart = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$`)
var imageTag = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
var stageName = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// normalizeSource parses the distribution-style name, registry, tag and digest
// separately. No short-name search, URL/transport or default tag is inferred.
func normalizeSource(ref string) (string, error) {
	fail := func() (string, error) { return "", ErrDockerfilePolicy }
	if len(ref) > 1024 || strings.ContainsAny(ref, " \t\r\n\\$\"'\x00") {
		return fail()
	}
	host, rest, ok := strings.Cut(ref, "/")
	if !ok || rest == "" {
		return fail()
	}
	host = strings.ToLower(host)
	hostname := host
	if strings.HasPrefix(host, "[") {
		if strings.HasSuffix(host, "]") {
			hostname = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
			if net.ParseIP(hostname) == nil {
				return fail()
			}
		} else {
			h, p, err := net.SplitHostPort(host)
			if err != nil {
				return fail()
			}
			hostname = h
			port, err := strconv.Atoi(p)
			if err != nil || port < 1 || port > 65535 {
				return fail()
			}
		}
	} else if strings.Contains(host, ":") {
		h, p, err := net.SplitHostPort(host)
		if err != nil {
			return fail()
		}
		hostname = h
		port, err := strconv.Atoi(p)
		if err != nil || port < 1 || port > 65535 {
			return fail()
		}
	}
	hostname = strings.TrimSuffix(hostname, ".")
	portText := ""
	if _, p, err := net.SplitHostPort(host); err == nil {
		n, _ := strconv.Atoi(p)
		portText = strconv.Itoa(n)
	}
	if hostname == "localhost" || hostname == "localhost.localdomain" || hostname == "ip6-localhost" || hostname == "ip6-loopback" || strings.HasSuffix(hostname, ".localhost") {
		return fail()
	}
	if ip := net.ParseIP(hostname); ip != nil {
		if ip.IsLoopback() || ip.IsUnspecified() {
			return fail()
		}
		if portText != "" {
			host = net.JoinHostPort(ip.String(), portText)
		} else if strings.Contains(ip.String(), ":") {
			host = "[" + ip.String() + "]"
		} else {
			host = ip.String()
		}
	} else {
		// Reject legacy numeric/hex/octal address spellings rather than delegating
		// their interpretation to host resolver behavior.
		numeric := true
		for _, part := range strings.Split(hostname, ".") {
			if part == "" {
				continue
			}
			if strings.HasPrefix(part, "0x") {
				part = strings.TrimPrefix(part, "0x")
				if part == "" {
					numeric = false
				}
				for _, c := range part {
					if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
						numeric = false
					}
				}
			} else {
				for _, c := range part {
					if !(c >= '0' && c <= '9') {
						numeric = false
					}
				}
			}
		}
		if numeric {
			return fail()
		}
		if !strings.Contains(hostname, ".") && !strings.Contains(host, ":") {
			return fail()
		}
		for _, part := range strings.Split(hostname, ".") {
			if part == "" || len(part) > 63 || part[0] == '-' || part[len(part)-1] == '-' {
				return fail()
			}
			for _, c := range part {
				if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
					return fail()
				}
			}
		}
	}
	if net.ParseIP(hostname) == nil {
		host = hostname
		if portText != "" && portText != "443" {
			host = net.JoinHostPort(hostname, portText)
		}
	}
	if host == "index.docker.io" {
		host = "docker.io"
	}
	name, digest, hasDigest := strings.Cut(rest, "@")
	if hasDigest && (!strings.HasPrefix(digest, "sha256:") || !validHex(strings.TrimPrefix(digest, "sha256:"))) {
		return fail()
	}
	repository, tag, hasTag := strings.Cut(name, ":")
	if hasTag && !imageTag.MatchString(tag) {
		return fail()
	}
	if !hasDigest && !hasTag {
		return fail()
	}
	for _, part := range strings.Split(repository, "/") {
		if !repositoryPart.MatchString(part) || part == generationImageNamespace {
			return fail()
		}
	}
	if host == "docker.io" && !strings.Contains(repository, "/") {
		rest = "library/" + rest
	}
	return host + "/" + rest, nil
}

// expand supports only fully known simple substitutions. Shell modifiers,
// automatic platform args and escaped/quoted image positions fail closed.
func expand(value string, vars map[string]string) (string, error) {
	var out strings.Builder
	for n := 0; n < len(value); {
		if value[n] != '$' {
			out.WriteByte(value[n])
			n++
			continue
		}
		n++
		start := n
		name := ""
		if n < len(value) && value[n] == '{' {
			n++
			start = n
			for n < len(value) && value[n] != '}' {
				n++
			}
			if n == len(value) {
				return "", ErrDockerfilePolicy
			}
			name = value[start:n]
			n++
		} else {
			for n < len(value) && (value[n] == '_' || value[n] >= 'a' && value[n] <= 'z' || value[n] >= 'A' && value[n] <= 'Z' || value[n] >= '0' && value[n] <= '9') {
				n++
			}
			name = value[start:n]
		}
		v, ok := vars[name]
		if !variableName.MatchString(name) || !ok {
			return "", ErrDockerfilePolicy
		}
		out.WriteString(v)
		if out.Len() > MaxDockerfileBytes {
			return "", ErrDockerfilePolicy
		}
	}
	return out.String(), nil
}

// compileDockerfile deliberately accepts a narrow grammar and emits normalized
// image positions. Continuations are parsed before instructions; directives,
// heredocs, ONBUILD, ADD and RUN options are rejected instead of delegated to
// a potentially different frontend. Ordinary COPY has only Dockerfile context.
func compileDockerfile(source string, known []string) (BuildPlan, error) {
	fail := func() (BuildPlan, error) { return BuildPlan{}, ErrDockerfilePolicy }
	if source == "" || len(source) > MaxDockerfileBytes || !utf8.ValidString(source) || strings.ContainsRune(source, 0) || strings.Contains(source, "<<") {
		return fail()
	}
	denied := map[string]bool{}
	deniedDigests := map[string]bool{}
	for _, v := range known {
		if _, d, ok := strings.Cut(v, "@sha256:"); ok && validHex(d) {
			deniedDigests[d] = true
		}
		if d := strings.TrimPrefix(v, "sha256:"); validHex(d) {
			deniedDigests[d] = true
		}
		if n, err := normalizeSource(v); err == nil {
			denied[n] = true
		}
	}
	global, local := map[string]string{}, map[string]string{}
	stages := map[string]int{}
	count := 0
	sources := []string{}
	var output strings.Builder
	var pending strings.Builder
	resolve := func(v string, vars map[string]string, allowScratch bool) (string, error) {
		v, err := expand(v, vars)
		if err != nil {
			return "", err
		}
		if allowScratch && v == "scratch" {
			return v, nil
		}
		if index, ok := stages[strings.ToLower(v)]; ok && (allowScratch || index < count-1) {
			return strings.ToLower(v), nil
		}
		if idx, err := strconv.Atoi(v); err == nil && idx >= 0 && idx < count-1 && !allowScratch {
			return strconv.Itoa(idx), nil
		}
		n, err := normalizeSource(v)
		_, digest, hasDigest := strings.Cut(n, "@sha256:")
		if err != nil || denied[n] || hasDigest && deniedDigests[digest] {
			return "", ErrDockerfilePolicy
		}
		sources = append(sources, n)
		return n, nil
	}
	for _, raw := range strings.Split(strings.ReplaceAll(source, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "#") {
			low := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(line, "#")))
			if strings.HasPrefix(low, "syntax") || strings.HasPrefix(low, "escape") {
				return fail()
			}
			continue
		}
		if line == "" {
			continue
		}
		if strings.ContainsRune(line, '\r') {
			return fail()
		}
		if strings.HasSuffix(line, "\\") {
			pending.WriteString(strings.TrimSuffix(line, "\\"))
			pending.WriteByte(' ')
			continue
		}
		line = pending.String() + line
		pending.Reset()
		at := strings.IndexAny(line, " \t")
		if at < 0 {
			return fail()
		}
		instruction := strings.ToUpper(line[:at])
		args := strings.TrimSpace(line[at:])
		if args == "" {
			return fail()
		}
		switch instruction {
		case "ARG":
			name, value, set := strings.Cut(args, "=")
			if !variableName.MatchString(name) {
				return fail()
			}
			vars := global
			if count > 0 {
				vars = local
			}
			if set {
				v, err := expand(value, vars)
				if err != nil {
					return fail()
				}
				vars[name] = v
			} else if count > 0 {
				if v, ok := global[name]; ok {
					vars[name] = v
				} else {
					delete(vars, name)
				}
			} else {
				delete(vars, name)
			}
		case "FROM":
			fields := strings.Fields(args)
			if len(fields) != 1 && !(len(fields) == 3 && strings.EqualFold(fields[1], "AS")) {
				return fail()
			}
			ref, err := resolve(fields[0], global, true)
			if err != nil {
				return fail()
			}
			args = ref
			if len(fields) == 3 {
				alias := strings.ToLower(fields[2])
				if !stageName.MatchString(alias) || alias == "scratch" {
					return fail()
				}
				if _, exists := stages[alias]; exists {
					return fail()
				}
				stages[alias] = count
				args += " AS " + alias
			}
			count++
			local = map[string]string{}
		case "ENV":
			if count == 0 {
				return fail()
			}
			name, value, set := strings.Cut(args, "=")
			if !set || !variableName.MatchString(name) || strings.ContainsAny(value, " \t\"'\\") {
				return fail()
			}
			v, err := expand(value, local)
			if err != nil {
				return fail()
			}
			local[name] = v
		case "COPY":
			if count == 0 {
				return fail()
			}
			remaining := args
			flags := []string{}
			seen := map[string]bool{}
			for strings.HasPrefix(remaining, "--") {
				at := strings.IndexAny(remaining, " \t")
				if at < 0 {
					return fail()
				}
				flag := remaining[:at]
				remaining = strings.TrimSpace(remaining[at:])
				key, value, set := strings.Cut(flag, "=")
				if !set || value == "" || seen[key] {
					return fail()
				}
				seen[key] = true
				switch key {
				case "--from":
					ref, err := resolve(value, local, false)
					if err != nil {
						return fail()
					}
					flag = "--from=" + ref
				case "--chown", "--chmod":
					if strings.ContainsAny(value, "$\"'\\") {
						return fail()
					}
				default:
					return fail()
				}
				flags = append(flags, flag)
			}
			if remaining == "" || strings.ContainsAny(remaining, "\r\n") {
				return fail()
			}
			args = strings.Join(append(flags, remaining), " ")
		case "RUN":
			if count == 0 || strings.HasPrefix(args, "--") {
				return fail()
			}
		case "CMD", "ENTRYPOINT", "WORKDIR", "USER", "EXPOSE", "VOLUME", "STOPSIGNAL", "SHELL", "HEALTHCHECK":
			if count == 0 {
				return fail()
			}
		default:
			return fail()
		}
		output.WriteString(instruction + " " + args + "\n")
		if output.Len() > MaxDockerfileBytes {
			return fail()
		}
	}
	if pending.Len() != 0 || count == 0 {
		return fail()
	}
	return BuildPlan{output.String(), sources}, nil
}
