package model

import "strings"

const hostOwnedInstallMethodPrefix = "host-owned:"

func HostOwnedInstallMethod(owner HostKind) string {
	if owner != HostCodex && owner != HostClaude && owner != HostSystem {
		return ""
	}
	return hostOwnedInstallMethodPrefix + string(owner)
}

func (b Binding) LifecycleOwner() HostKind {
	value := strings.TrimPrefix(b.InstallMethod, hostOwnedInstallMethodPrefix)
	if value == b.InstallMethod {
		return ""
	}
	owner := HostKind(value)
	if HostOwnedInstallMethod(owner) == "" {
		return ""
	}
	return owner
}

func (b Binding) HostOwned() bool { return b.LifecycleOwner() != "" }
