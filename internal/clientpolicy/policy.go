package clientpolicy

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
)

type PolicySet struct {
	Clients []ClientPolicy `json:"clients"`
	byID    map[string]ClientPolicy
}

type ClientPolicy struct {
	SANURI         string   `json:"san_uri"`
	SANDNS         string   `json:"san_dns"`
	Subject        string   `json:"subject"`
	AllowedTargets []string `json:"allowed_targets"`
	MaxConcurrency int      `json:"max_concurrency"`
}

func Load(path string) (*PolicySet, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var set PolicySet
	trimmed := strings.TrimSpace(string(data))
	if strings.HasPrefix(trimmed, "{") {
		if err := json.Unmarshal(data, &set); err != nil {
			return nil, err
		}
	} else {
		parsed, err := parseYAMLSubset(trimmed)
		if err != nil {
			return nil, err
		}
		set = parsed
	}
	if err := set.Validate(); err != nil {
		return nil, err
	}
	return &set, nil
}

func (s *PolicySet) Validate() error {
	if s == nil {
		return nil
	}
	if len(s.Clients) == 0 {
		return fmt.Errorf("client policy must contain at least one client")
	}
	s.byID = make(map[string]ClientPolicy, len(s.Clients))
	for i := range s.Clients {
		client := s.Clients[i]
		if strings.TrimSpace(client.Subject) != "" {
			subject, err := CanonicalSubject(client.Subject)
			if err != nil {
				return fmt.Errorf("client policy subject %q is invalid: %w", client.Subject, err)
			}
			client.Subject = subject
			s.Clients[i].Subject = subject
		}
		ids := client.identities()
		if len(ids) != 1 {
			return fmt.Errorf("each client policy must contain exactly one identity")
		}
		id := ids[0]
		if _, exists := s.byID[id]; exists {
			return fmt.Errorf("duplicate client policy identity %q", id)
		}
		for _, target := range client.AllowedTargets {
			if strings.TrimSpace(target) == "" {
				return fmt.Errorf("client policy %q contains empty allowed target", id)
			}
			if err := validateTarget(target); err != nil {
				return fmt.Errorf("client policy %q: %w", id, err)
			}
		}
		if client.MaxConcurrency < 0 {
			return fmt.Errorf("client policy %q max_concurrency must be >= 0", id)
		}
		s.byID[id] = client
	}
	return nil
}

func (s *PolicySet) Lookup(identity string) (ClientPolicy, bool) {
	if s == nil {
		return ClientPolicy{}, false
	}
	p, ok := s.byID[identity]
	return p, ok
}

func (p ClientPolicy) Identity() string {
	ids := p.identities()
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

func (p ClientPolicy) identities() []string {
	var ids []string
	for _, id := range []string{p.SANURI, p.SANDNS, p.Subject} {
		id = strings.TrimSpace(id)
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func parseYAMLSubset(raw string) (PolicySet, error) {
	var set PolicySet
	var current *ClientPolicy
	allowedTargetsListIndent := -1
	lines := strings.Split(raw, "\n")
	for _, rawLine := range lines {
		indent := leadingSpaces(rawLine)
		line := strings.TrimSpace(stripComment(rawLine))
		if line == "" || line == "clients:" {
			continue
		}
		if allowedTargetsListIndent >= 0 && strings.HasPrefix(line, "- ") && indent > allowedTargetsListIndent {
			current.AllowedTargets = append(current.AllowedTargets, unquote(strings.TrimSpace(strings.TrimPrefix(line, "- "))))
			continue
		}
		if strings.HasPrefix(line, "- ") {
			if current != nil {
				set.Clients = append(set.Clients, *current)
			}
			current = &ClientPolicy{}
			allowedTargetsListIndent = -1
			line = strings.TrimSpace(strings.TrimPrefix(line, "- "))
			if line == "" {
				continue
			}
		}
		if current == nil {
			return PolicySet{}, fmt.Errorf("policy entry outside clients list")
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return PolicySet{}, fmt.Errorf("invalid policy line %q", line)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "san_uri":
			current.SANURI = unquote(value)
		case "san_dns":
			current.SANDNS = unquote(value)
		case "subject":
			current.Subject = unquote(value)
		case "allowed_targets":
			if value == "" {
				current.AllowedTargets = nil
				allowedTargetsListIndent = indent
				continue
			}
			current.AllowedTargets = parseInlineList(value)
			allowedTargetsListIndent = -1
		case "max_concurrency":
			n, err := strconv.Atoi(unquote(value))
			if err != nil {
				return PolicySet{}, fmt.Errorf("invalid max_concurrency: %w", err)
			}
			current.MaxConcurrency = n
		default:
			return PolicySet{}, fmt.Errorf("unknown client policy key %q", key)
		}
	}
	if current != nil {
		set.Clients = append(set.Clients, *current)
	}
	return set, nil
}

func leadingSpaces(line string) int {
	return len(line) - len(strings.TrimLeft(line, " \t"))
}

func stripComment(line string) string {
	if i := strings.Index(line, "#"); i >= 0 {
		return line[:i]
	}
	return line
}

func parseInlineList(value string) []string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "[")
	value = strings.TrimSuffix(value, "]")
	var out []string
	for _, part := range strings.Split(value, ",") {
		item := unquote(strings.TrimSpace(part))
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

func unquote(value string) string {
	value = strings.TrimSpace(value)
	value = strings.Trim(value, `"`)
	value = strings.Trim(value, `'`)
	return value
}

func CanonicalSubject(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("subject is empty")
	}
	parts := splitUnescaped(raw, ',')
	attrs := make([]string, 0, len(parts))
	for _, part := range parts {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			return "", fmt.Errorf("subject attribute %q must be key=value", strings.TrimSpace(part))
		}
		key = strings.ToUpper(strings.TrimSpace(key))
		value = unescapeSubjectValue(unquote(strings.TrimSpace(value)))
		if key == "" || value == "" {
			return "", fmt.Errorf("subject attribute %q must include key and value", strings.TrimSpace(part))
		}
		attrs = append(attrs, key+"="+value)
	}
	sort.Strings(attrs)
	return strings.Join(attrs, ","), nil
}

func splitUnescaped(raw string, sep rune) []string {
	var parts []string
	var b strings.Builder
	escaped := false
	for _, r := range raw {
		if escaped {
			b.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' {
			escaped = true
			continue
		}
		if r == sep {
			parts = append(parts, b.String())
			b.Reset()
			continue
		}
		b.WriteRune(r)
	}
	if escaped {
		b.WriteRune('\\')
	}
	parts = append(parts, b.String())
	return parts
}

func unescapeSubjectValue(value string) string {
	value = strings.ReplaceAll(value, `\,`, ",")
	value = strings.ReplaceAll(value, `\=`, "=")
	value = strings.ReplaceAll(value, `\\`, `\`)
	return strings.TrimSpace(value)
}

func validateTarget(target string) error {
	host, port, err := net.SplitHostPort(strings.TrimSpace(target))
	if err != nil {
		return fmt.Errorf("allowed target %q must be host:port", target)
	}
	if strings.TrimSpace(host) == "" || strings.TrimSpace(port) == "" {
		return fmt.Errorf("allowed target %q must be host:port", target)
	}
	if port != "443" {
		return fmt.Errorf("allowed target %q must use port 443 in v1", target)
	}
	return nil
}
