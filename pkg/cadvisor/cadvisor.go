package cadvisor

import (
	v1 "github.com/google/cadvisor/info/v1"
)

const (
	ContainerLabelContainerName = "io.kubernetes.container.name"
	ContainerLabelPodUID        = "io.kubernetes.pod.uid"
	ContainerLabelPodName       = "io.kubernetes.pod.name"
	ContainerLabelPodNamespace  = "io.kubernetes.pod.namespace"
)

func AddInterfaceStats(a, b v1.InterfaceStats) v1.InterfaceStats {
	a.RxDropped += b.RxDropped
	a.RxErrors += b.RxErrors
	a.RxBytes += b.RxBytes
	a.RxPackets += b.RxPackets

	a.TxDropped += b.TxDropped
	a.TxErrors += b.TxErrors
	a.TxBytes += b.TxBytes
	a.TxPackets += b.TxPackets

	return a
}

func DeltaInterfaceStats(a, b v1.InterfaceStats) v1.InterfaceStats {
	a.RxDropped -= b.RxDropped
	a.RxErrors -= b.RxErrors
	a.RxBytes -= b.RxBytes
	a.RxPackets -= b.RxPackets

	a.TxDropped -= b.TxDropped
	a.TxErrors -= b.TxErrors
	a.TxBytes -= b.TxBytes
	a.TxPackets -= b.TxPackets

	return a
}

func DivideInterfaceStats(stats v1.InterfaceStats, factor uint64) v1.InterfaceStats {
	stats.RxDropped = stats.RxDropped / factor
	stats.RxErrors = stats.RxErrors / factor
	stats.RxBytes = stats.RxBytes / factor
	stats.RxPackets = stats.RxPackets / factor

	stats.TxDropped = stats.TxDropped / factor
	stats.TxErrors = stats.TxErrors / factor
	stats.TxBytes = stats.TxBytes / factor
	stats.TxPackets = stats.TxPackets / factor

	return stats
}

func HasResetInInterfaceStats(stats v1.InterfaceStats) bool {
	return stats.RxDropped < 0 ||
		stats.RxErrors < 0 ||
		stats.RxBytes < 0 ||
		stats.RxPackets < 0 ||
		stats.TxDropped < 0 ||
		stats.TxErrors < 0 ||
		stats.TxBytes < 0 ||
		stats.TxPackets < 0
}

func AddNetworkStats(a, b v1.NetworkStats) v1.NetworkStats {
	a.InterfaceStats = AddInterfaceStats(a.InterfaceStats, b.InterfaceStats)
	AddInterfaceStats(a.InterfaceStats, b.InterfaceStats)
	return a
}
func DeltaNetworkStats(a, b v1.NetworkStats) v1.NetworkStats {
	a.InterfaceStats = DeltaInterfaceStats(a.InterfaceStats, b.InterfaceStats)
	return a
}
func DivideNetworkStats(stats v1.NetworkStats, factor uint64) v1.NetworkStats {
	stats.InterfaceStats = DivideInterfaceStats(stats.InterfaceStats, factor)
	return stats
}
func HasResetInNetworkStats(stats v1.NetworkStats) bool {
	return HasResetInInterfaceStats(stats.InterfaceStats)
}

func AddContainerStats(a, b v1.ContainerStats) v1.ContainerStats {
	a.Network = AddNetworkStats(a.Network, b.Network)
	return a
}

func DeltaContainerStats(a, b v1.ContainerStats) v1.ContainerStats {
	a.Network = DeltaNetworkStats(a.Network, b.Network)
	return a
}

func DivideContainerStats(stats v1.ContainerStats, factor uint64) v1.ContainerStats {
	stats.Network = DivideNetworkStats(stats.Network, factor)
	return stats
}

func HasResetInContainerStats(stats v1.ContainerStats) bool {
	return HasResetInNetworkStats(stats.Network)
}
