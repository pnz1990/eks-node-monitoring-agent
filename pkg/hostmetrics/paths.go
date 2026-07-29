package hostmetrics

import (
	"path/filepath"
	"strings"
)

// Paths locates the kernel interfaces the collectors read.
//
// PROVENANCE: mirrors node_exporter's --path.procfs / --path.sysfs /
// --path.rootfs / --path.udev.data flags (collector/paths.go). Reproduced because
// the agent runs in a container with the host root mounted elsewhere, so every
// read must be rebased. Unlike upstream these are plain struct fields rather than
// kingpin globals, which is what lets this package coexist with the agent's pflag
// configuration without a second flag library.
type Paths struct {
	// ProcFS is the procfs mount point, normally /proc.
	ProcFS string
	// SysFS is the sysfs mount point, normally /sys.
	SysFS string
	// RootFS is the root filesystem, normally /. Used for filesystem metrics and
	// for stripping the prefix from reported mount points.
	RootFS string
	// UdevData is the udev data directory, normally /run/udev/data.
	UdevData string
}

// Defaults for a process running directly on the host.
const (
	defaultProcFS   = "/proc"
	defaultSysFS    = "/sys"
	defaultRootFS   = "/"
	defaultUdevData = "/run/udev/data"
)

// withDefaults fills unset fields with the host defaults.
func (p Paths) withDefaults() Paths {
	if p.ProcFS == "" {
		p.ProcFS = defaultProcFS
	}
	if p.SysFS == "" {
		p.SysFS = defaultSysFS
	}
	if p.RootFS == "" {
		p.RootFS = defaultRootFS
	}
	if p.UdevData == "" {
		p.UdevData = defaultUdevData
	}
	return p
}

// ForHostRoot returns Paths rebased onto a mounted host root.
//
// The agent mounts the host filesystem at HOST_ROOT (/host in the shipped
// DaemonSet), so procfs is at /host/proc and so on. An empty or "/" hostRoot
// yields the plain host defaults.
func ForHostRoot(hostRoot string) Paths {
	if hostRoot == "" || hostRoot == "/" {
		return Paths{}.withDefaults()
	}
	root := strings.TrimSuffix(hostRoot, "/")
	return Paths{
		ProcFS:   filepath.Join(root, "proc"),
		SysFS:    filepath.Join(root, "sys"),
		RootFS:   root,
		UdevData: filepath.Join(root, "run", "udev", "data"),
	}
}

// procPath joins one or more elements onto the procfs root.
func (p Paths) procPath(elem ...string) string {
	return filepath.Join(append([]string{p.ProcFS}, elem...)...)
}

// sysPath joins one or more elements onto the sysfs root.
func (p Paths) sysPath(elem ...string) string {
	return filepath.Join(append([]string{p.SysFS}, elem...)...)
}

// rootPath joins one or more elements onto the rootfs.
func (p Paths) rootPath(elem ...string) string {
	return filepath.Join(append([]string{p.RootFS}, elem...)...)
}

// stripRootFS removes the rootfs prefix from an absolute path.
//
// PROVENANCE: node_exporter's rootfsStripPrefix (collector/paths.go). Mount points
// must be reported as the host sees them — "/var/lib/kubelet", not
// "/host/var/lib/kubelet" — or every filesystem dashboard label changes. This is
// pure label cosmetics and does no filtering, a distinction that matters because
// mount-point exclusion is applied to the *stripped* path.
func (p Paths) stripRootFS(path string) string {
	if p.RootFS == defaultRootFS {
		return path
	}
	stripped := strings.TrimPrefix(path, p.RootFS)
	if stripped == "" {
		return defaultRootFS
	}
	return stripped
}
