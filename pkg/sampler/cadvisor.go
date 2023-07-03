package sampler

import (
	v1 "github.com/google/cadvisor/info/v1"

	"sigs.k8s.io/usage-metrics-collector/pkg/cadvisor"
)

func ContainerKeyFromCAdvisorContainerInfo(info v1.ContainerInfo) ContainerKey {
	labels := info.Spec.Labels
	containerName := labels[cadvisor.ContainerLabelContainerName]
	podNamespace := labels[cadvisor.ContainerLabelPodNamespace]
	podName := labels[cadvisor.ContainerLabelPodName]
	podUID := labels[cadvisor.ContainerLabelPodUID]
	return ContainerKey{
		ContainerID:   containerName,
		NamespaceName: podNamespace,
		PodName:       podName,
		PodUID:        podUID,
		ContainerName: containerName,
	}
}
