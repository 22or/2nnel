package config

// ServerConfig holds all server-side settings.
type ServerConfig struct {
	Domain       string
	Port         int
	AuthToken    string
	Dev          bool
	TLSCert      string
	TLSKey       string
	ACMECache    string
	AllowedPorts []int
	TCPPortMin   int
	TCPPortMax   int
}

// ClientConfig holds all client-side settings (from flags or YAML).
type ClientConfig struct {
	Server    string         `mapstructure:"server"     yaml:"server"`
	AuthToken string         `mapstructure:"auth_token" yaml:"auth_token"`
	Tunnels   []TunnelConfig `mapstructure:"tunnels"    yaml:"tunnels"`
}

// TunnelConfig describes one tunnel.
type TunnelConfig struct {
	Name       string `mapstructure:"name"        yaml:"name"`
	Local      string `mapstructure:"local"       yaml:"local"`
	Type       string `mapstructure:"type"        yaml:"type"`        // "http" or "tcp"
	Subdomain  string `mapstructure:"subdomain"   yaml:"subdomain"`   // HTTP tunnels
	RemotePort int    `mapstructure:"remote_port" yaml:"remote_port"` // TCP tunnels
}
