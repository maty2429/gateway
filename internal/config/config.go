package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a wrapper around time.Duration to support parsing duration strings from YAML.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	dur, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(dur)
	return nil
}

func (d Duration) Duration() time.Duration {
	return time.Duration(d)
}

type TLSConfig struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

type ServerConfig struct {
	Environment       string     `yaml:"environment"`
	Addr              string     `yaml:"addr"`
	ReadTimeout       Duration   `yaml:"read_timeout"`
	ReadHeaderTimeout Duration   `yaml:"read_header_timeout"`
	WriteTimeout      Duration   `yaml:"write_timeout"`
	IdleTimeout       Duration   `yaml:"idle_timeout"`
	MaxBodyBytes      int64      `yaml:"max_body_bytes"`
	TLS               *TLSConfig `yaml:"tls"`
}

type AuthConfig struct {
	JWTIssuer    string   `yaml:"jwt_issuer"`
	JWTAudience  string   `yaml:"jwt_audience"` // deprecated single-audience form
	JWTAudiences []string `yaml:"jwt_audiences"`
	JWKSRefresh  Duration `yaml:"jwks_refresh"`
	CookieSecure bool     `yaml:"cookie_secure"`
}

type CORSConfig struct {
	AllowedOrigins []string `yaml:"allowed_origins"`
}

type RateLimitConfig struct {
	DefaultRPS   float64 `yaml:"default_rps"`
	DefaultBurst int     `yaml:"default_burst"`
}

type RouteRateLimit struct {
	RPS   float64 `yaml:"rps"`
	Burst int     `yaml:"burst"`
}

type UpstreamConfig struct {
	Protocol string             `yaml:"protocol"`
	Address  string             `yaml:"address"`
	Timeout  Duration           `yaml:"timeout"`
	TLS      *UpstreamTLSConfig `yaml:"tls"`
}

type UpstreamTLSConfig struct {
	CAFile     string `yaml:"ca_file"`
	CertFile   string `yaml:"cert_file"`
	KeyFile    string `yaml:"key_file"`
	ServerName string `yaml:"server_name"`
}

type RouteConfig struct {
	Method      string          `yaml:"method"`
	Path        string          `yaml:"path"`
	Upstream    string          `yaml:"upstream"`
	Auth        string          `yaml:"auth"` // "required" or "none"
	RateLimit   *RouteRateLimit `yaml:"rate_limit"`
	Idempotency bool            `yaml:"idempotency"`
}

type Config struct {
	Server    ServerConfig              `yaml:"server"`
	Auth      AuthConfig                `yaml:"auth"`
	CORS      CORSConfig                `yaml:"cors"`
	RateLimit RateLimitConfig           `yaml:"rate_limit"`
	Upstreams map[string]UpstreamConfig `yaml:"upstreams"`
	Routes    []RouteConfig             `yaml:"routes"`
}

// Load loads the configuration from a file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse yaml config: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return &cfg, nil
}

// Validate checks the configuration for basic semantic errors.
func (c *Config) Validate() error {
	if c.Server.Addr == "" {
		return fmt.Errorf("server address cannot be empty")
	}
	if c.Auth.JWTIssuer == "" {
		return fmt.Errorf("auth jwt_issuer cannot be empty")
	}
	if c.Auth.JWTAudience == "" && len(c.Auth.JWTAudiences) == 0 {
		return fmt.Errorf("auth must configure at least one JWT audience")
	}
	if c.Server.Environment == "production" {
		if !c.Auth.CookieSecure {
			return fmt.Errorf("production requires auth cookie_secure=true")
		}
		if len(c.CORS.AllowedOrigins) == 0 {
			return fmt.Errorf("production requires a non-empty CORS allowed_origins list")
		}
	}
	authUpstream, hasAuth := c.Upstreams["auth"]
	if !hasAuth || authUpstream.Protocol != "grpc" {
		return fmt.Errorf("a gRPC upstream named auth is required")
	}

	for name, upstream := range c.Upstreams {
		if upstream.Protocol != "http" && upstream.Protocol != "grpc" {
			return fmt.Errorf("upstream %s: protocol must be 'http' or 'grpc', got %q", name, upstream.Protocol)
		}
		if upstream.Address == "" {
			return fmt.Errorf("upstream %s: address cannot be empty", name)
		}
		if c.Server.Environment == "production" && name == "auth" && upstream.Protocol == "grpc" {
			if upstream.TLS == nil || upstream.TLS.CAFile == "" || upstream.TLS.CertFile == "" || upstream.TLS.KeyFile == "" || upstream.TLS.ServerName == "" {
				return fmt.Errorf("upstream auth: production requires mTLS ca_file, cert_file, key_file and server_name")
			}
		}
	}

	for i, route := range c.Routes {
		if route.Method == "" {
			return fmt.Errorf("route %d: method cannot be empty", i)
		}
		if route.Path == "" {
			return fmt.Errorf("route %d: path cannot be empty", i)
		}
		if route.Upstream == "" {
			return fmt.Errorf("route %d: upstream cannot be empty", i)
		}
		if _, ok := c.Upstreams[route.Upstream]; !ok {
			return fmt.Errorf("route %d: referenced upstream %q does not exist", i, route.Upstream)
		}
		if route.Auth != "required" && route.Auth != "limited" && route.Auth != "none" && route.Auth != "" {
			return fmt.Errorf("route %d: auth must be 'required', 'limited', 'none' or empty, got %q", i, route.Auth)
		}
	}

	return nil
}
