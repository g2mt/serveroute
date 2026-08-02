// Package config loads and validates the serveroute config.yaml.
package config

import (
	"fmt"
	"net/netip"
	"os"
	"os/user"
	"regexp"
	"serveroute/internal/platform"
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
	SSHHost  string                   `yaml:"ssh_host"`
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
	// Headers is a map of additional HTTP headers to set on proxied requests.
	Headers map[string]string `yaml:"headers"`
	// ClientHeaders is a map of HTTP headers to set on responses before sending to the client.
	ClientHeaders map[string]string `yaml:"client_headers"`
}

// Compiled holds the validated config and pre-computed lookups.
type Compiled struct {
	*Config
	HostRegex *regexp.Regexp
	LocalHost string
	AllowNets []netip.Prefix
	// ServiceIndex maps host -> subdomain -> *ServiceConfig.
	// A single systemd unit may appear under multiple subdomains or hosts;
	// the watcher tolerates duplicate Add calls for the same unit name.
	ServiceIndex map[string]map[string]*ServiceConfig
}

// Load reads and parses config.yaml from path, setting default values
// for service-level pointer fields (Stoppable, User default to true).
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// Apply defaults for *bool fields so callers can dereference safely.
	trueVal := true
	for _, host := range cfg.Hosts {
		for k, svc := range host.Services {
			if svc.Stoppable == nil {
				svc.Stoppable = &trueVal
			}
			if svc.User == nil {
				svc.User = &trueVal
			}
			host.Services[k] = svc
		}
	}

	return &cfg, nil
}

// Compile validates the config and builds the lookup tables.
func Compile(cfg *Config) (*Compiled, error) {
	if cfg.Listen.HTTP == "" {
		return nil, fmt.Errorf("listen.http is required")
	}

	if cfg.StartPort == 0 {
		return nil, fmt.Errorf("start_port is required")
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
	hostRegex, err := compileDomainTemplate(cfg.DomainTemplate)
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

	// Build a cached templater for placeholder replacement across all services.
	templater := newServiceTemplater()

	// Build service index.
	serviceIndex := make(map[string]map[string]*ServiceConfig)
	for hostName, hostCfg := range cfg.Hosts {
		svcMap := make(map[string]*ServiceConfig)
		for svcKey, svc := range hostCfg.Services {
			subdomain := svc.Subdomain
			if subdomain == "" {
				subdomain = svcKey
			}
			// Copy to avoid aliasing loop variable.
			sc := svc
			// Apply template placeholders to all string fields.
			templater.apply(&sc, hostName)
			// Correct systemd unit suffixes
			if sc.Unit != "" {
				sc.Unit = platform.EnsureSuffix(sc.Unit)
			}
			svcMap[subdomain] = &sc
		}
		serviceIndex[hostName] = svcMap
	}

	return &Compiled{
		Config:       cfg,
		HostRegex:    hostRegex,
		LocalHost:    localHost,
		AllowNets:    allowNets,
		ServiceIndex: serviceIndex,
	}, nil
}

// serviceTemplater caches system-level values so placeholder replacement
// across many services doesn't re-query the OS on each call.
//
// Placeholders:
//
//	%h  – user home directory
//	%H  – host name of the host being processed
//	%R  – running machine's hostname
//	%u  – running user name
//	%U  – user UID
type serviceTemplater struct {
	homeDir  string
	hostname string
	username string
	uid      string
}

func newServiceTemplater() *serviceTemplater {
	homeDir, _ := os.UserHomeDir()
	hostname, _ := os.Hostname()
	currentUser, _ := user.Current()
	return &serviceTemplater{
		homeDir:  homeDir,
		hostname: hostname,
		username: currentUser.Username,
		uid:      currentUser.Uid,
	}
}

func (st *serviceTemplater) apply(svc *ServiceConfig, hostName string) {
	r := strings.NewReplacer(
		"%h", st.homeDir,
		"%H", hostName,
		"%R", st.hostname,
		"%u", st.username,
		"%U", st.uid,
	)

	svc.Subdomain = r.Replace(svc.Subdomain)
	svc.ServeFiles = r.Replace(svc.ServeFiles)
	svc.Unit = r.Replace(svc.Unit)
	svc.ForwardsTo = r.Replace(svc.ForwardsTo)
	svc.AllowOrigin = r.Replace(svc.AllowOrigin)

	// Apply template replacement to header key/value pairs.
	if svc.Headers != nil {
		newHeaders := make(map[string]string, len(svc.Headers))
		for k, v := range svc.Headers {
			newHeaders[r.Replace(k)] = r.Replace(v)
		}
		svc.Headers = newHeaders
	}

	// Apply template replacement to client header key/value pairs.
	if svc.ClientHeaders != nil {
		newHeaders := make(map[string]string, len(svc.ClientHeaders))
		for k, v := range svc.ClientHeaders {
			newHeaders[r.Replace(k)] = r.Replace(v)
		}
		svc.ClientHeaders = newHeaders
	}
}

// compileDomainTemplate converts the domain_template string into a regex.
// Placeholders ${subdomain} and ${host} are replaced with named capture groups.
func compileDomainTemplate(tmpl string) (*regexp.Regexp, error) {
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
