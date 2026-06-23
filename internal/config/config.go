// Package config loads and validates the serveroute config.yaml.
package config

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"regexp"
	"serveroute/internal/systemd"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
)

// Config is the raw parsed configuration from config.yaml.
type Config struct {
	Listen            ListenConfig          `yaml:"listen"`
	SSLCertificate    string                `yaml:"ssl_certificate"`
	SSLCertificateKey string                `yaml:"ssl_certificate_key"`
	StartPort         int                   `yaml:"start_port"`
	Allow             []string              `yaml:"allow"`
	DomainTemplate    string                `yaml:"domain_template"`
	Hosts             map[string]HostConfig `yaml:"hosts"`
}

// ListenConfig holds the HTTP and optional HTTPS listen addresses.
type ListenConfig struct {
	HTTP  string `yaml:"http"`
	HTTPS string `yaml:"https"`
}

// HostConfig holds the services for a single host.
type HostConfig struct {
	Services map[string]ServiceConfig `yaml:"services"`
}

// ServiceConfig defines how a single subdomain is handled.
type ServiceConfig struct {
	// Subdomain overrides the default subdomain (the map key).
	Subdomain string `yaml:"subdomain"`
	// ServeFiles is a directory to serve static files from.
	ServeFiles string `yaml:"serve_files"`
	// Hidden hides this service from the API.
	Hidden bool `yaml:"hidden"`
	// Stoppable controls whether the systemd unit can be idled. Default true.
	Stoppable *bool `yaml:"stoppable"`
	// API enables built-in API handlers for this subdomain.
	API bool `yaml:"api"`
	// Unit names the systemd unit to control.
	Unit string `yaml:"unit"`
	// User selects the user systemd bus instead of system bus. Default true.
	User *bool `yaml:"user"`
	// StopsAfter is the idle timeout in seconds before stopping the unit.
	StopsAfter int `yaml:"stops_after"`
	// ForwardsTo is the URL to reverse-proxy requests to.
	ForwardsTo string `yaml:"forwards_to"`
	// AllowOrigin sets the Access-Control-Allow-Origin header on responses.
	AllowOrigin string `yaml:"allow_origin"`
}

// Stoppable returns true unless explicitly set to false.
func (s ServiceConfig) IsStoppable() bool {
	if s.Stoppable == nil {
		return true
	}
	return *s.Stoppable
}

// UsesUserBus returns true unless explicitly set to false.
func (s ServiceConfig) UsesUserBus() bool {
	if s.User == nil {
		return true
	}
	return *s.User
}

// Compiled holds the validated config and pre-computed lookups.
type Compiled struct {
	Raw       *Config
	HostRegex *regexp.Regexp
	LocalHost string
	HTTPPort  int // local HTTP port, extracted from listen address
	AllowNets []netip.Prefix
	// ServiceIndex maps host -> subdomain -> *ServiceConfig.
	// A single systemd unit may appear under multiple subdomains or hosts;
	// the watcher tolerates duplicate Add calls for the same unit name.
	ServiceIndex map[string]map[string]*ServiceConfig
}

// Load reads and parses config.yaml from path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return &cfg, nil
}

// Compile validates the config and builds the lookup tables.
func Compile(cfg *Config) (*Compiled, error) {
	if cfg.Listen.HTTP == "" {
		return nil, fmt.Errorf("listen.http is required")
	}

	// Parse allow list.
	allowNets := make([]netip.Prefix, 0, len(cfg.Allow))
	for _, a := range cfg.Allow {
		prefix, err := netip.ParsePrefix(a)
		if err != nil {
			// Try as a single IP.
			ip, err2 := netip.ParseAddr(a)
			if err2 != nil {
				return nil, fmt.Errorf("invalid allow entry %q: %w", a, err)
			}
			bits := 32
			if ip.Is6() {
				bits = 128
			}
			prefix = netip.PrefixFrom(ip, bits)
		}
		allowNets = append(allowNets, prefix)
	}

	// Compile domain template into a regex.
	hostRegex, err := compileTemplate(cfg.DomainTemplate)
	if err != nil {
		return nil, fmt.Errorf("compile domain_template: %w", err)
	}

	// Resolve local hostname.
	localHost, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("hostname: %w", err)
	}

	// Short hostname only (strip domain suffix).
	if idx := strings.IndexByte(localHost, '.'); idx >= 0 {
		localHost = localHost[:idx]
	}

	// Extract local HTTP port.
	_, portStr, err := net.SplitHostPort(cfg.Listen.HTTP)
	if err != nil {
		return nil, fmt.Errorf("parse listen.http %q: %w", cfg.Listen.HTTP, err)
	}
	localPort := 0
	fmt.Sscanf(portStr, "%d", &localPort)

	// Build service index and host port map.
	serviceIndex := make(map[string]map[string]*ServiceConfig)

	for hostName, hostCfg := range cfg.Hosts {
		svcMap := make(map[string]*ServiceConfig)
		for svcKey, svc := range hostCfg.Services {
			subdomain := svc.Subdomain
			if subdomain == "" {
				subdomain = svcKey
			}
			// Correct systemd unit suffixes
			if svc.Unit != "" {
				svc.Unit = systemd.EnsureSuffix(svc.Unit)
			}
			// Copy to avoid aliasing loop variable.
			sc := svc
			svcMap[subdomain] = &sc
		}
		serviceIndex[hostName] = svcMap
	}

	if cfg.StartPort == 0 {
		return nil, fmt.Errorf("start_port is required")
	}

	return &Compiled{
		Raw:          cfg,
		HostRegex:    hostRegex,
		LocalHost:    localHost,
		HTTPPort:     localPort,
		AllowNets:    allowNets,
		ServiceIndex: serviceIndex,
	}, nil
}

// compileTemplate converts the domain_template string into a regex.
// Placeholders ${subdomain} and ${host} are replaced with named capture groups.
func compileTemplate(tmpl string) (*regexp.Regexp, error) {
	pat := regexp.QuoteMeta(tmpl)

	// Replace escaped placeholders with capture groups in one pass.
	pat = strings.NewReplacer(
		// subdomain is optional: captures non-dot chars followed by a dot, or nothing.
		regexp.QuoteMeta("${subdomain}"), `(?:(?P<subdomain>[^.]+)\.)?`,
		regexp.QuoteMeta("${host}"), `(?P<host>[^.]+)`,
	).Replace(pat)

	re, err := regexp.Compile(`^` + pat + `$`)
	if err != nil {
		return nil, fmt.Errorf("template %q: %w", tmpl, err)
	}
	return re, nil
}

// StopsAfterDuration returns the stops_after value as a time.Duration.
func (s ServiceConfig) StopsAfterDuration() time.Duration {
	if s.StopsAfter <= 0 {
		return 0
	}
	return time.Duration(s.StopsAfter) * time.Second
}
