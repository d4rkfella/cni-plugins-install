package constants

import "time"

const (
	// File permissions
	DirPerm  = 0o755
	FilePerm = 0o644

	// Backup settings
	BackupPrefix = ".backup-"

	// HTTP settings
	DefaultUserAgent = "cni-plugins-installer/1.0"

	// Default configuration
	DefaultBaseURL         = "https://github.com/containernetworking/plugins/releases/download"
	DefaultTargetDir       = "/host/opt/cni/bin"
	DefaultDownloadTimeout = 30 * time.Second
	DefaultMaxRetries      = 3
	DefaultBufferSize      = 32 * 1024 // 32KB buffer
)

// ManagedPlugins defines the set of CNI plugin binaries managed by this tool.
var ManagedPlugins = map[string]bool{
	"bandwidth":   true,
	"bridge":      true,
	"dhcp":        true,
	"dummy":       true,
	"firewall":    true,
	"host-device": true,
	"host-local":  true,
	"ipvlan":      true,
	"loopback":    true,
	"macvlan":     true,
	"portmap":     true,
	"ptp":         true,
	"sbr":         true,
	"static":      true,
	"tap":         true,
	"tuning":      true,
	"vlan":        true,
	"vrf":         true,
}
