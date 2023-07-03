// Copyright 2023 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sampler

import (
	"container/ring"
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/containerd/containerd"
	"github.com/google/cadvisor/client"
	v1 "github.com/google/cadvisor/info/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/pointer"

	"sigs.k8s.io/usage-metrics-collector/pkg/api/samplerserverv1alpha1"
	"sigs.k8s.io/usage-metrics-collector/pkg/cadvisor"
	commonlog "sigs.k8s.io/usage-metrics-collector/pkg/log"
)

var (
	log = commonlog.Log.WithName("kube-metrics-node-sampler")
)

// sampleCache continuously reads metric samples from containerd into a buffer and caches them.
type sampleCache struct {
	samplerserverv1alpha1.Buffer

	// Reader is used to read container metrics.
	// +optional
	metricsReader metricsReader
	readerConfig  samplerserverv1alpha1.Reader

	// containerSamples stores the samples read from containerd
	samples      *ring.Ring
	samplesMutex sync.Mutex

	once sync.Once

	// useContainerMonitor use container monitor for metrics
	UseContainerMonitor bool
	ContainerdClient    *containerd.Client

	UseCadvisorMonitor bool
	cadvisorClient     *client.Client
}

// Start starts the cache reading from /sys/fs/cgroup
func (s *sampleCache) Start(ctx context.Context) error {
	log.Info("starting sampler")
	s.init()

	frequency := time.Minute / time.Duration(s.PollsPerMinute)
	ticker := time.NewTicker(frequency)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// stop scraping
			log.Info("stopping cgroup sampler", "err", ctx.Err())
			return nil
		case <-ticker.C:
			// retry fetching the sample in case of a failure
			// if fetch keeps failing return error for graceful shutdown
			if err := retry.OnError(retry.DefaultRetry, func(err error) bool { return true }, func() error {
				return s.fetchSample()
			}); err != nil {
				return err
			}
		}
	}
}

// getAllSamples returns all cached samples keyed by the ContainerID
func (s *sampleCache) getAllSamples() (allSampleInstants, int) {
	s.init()
	log := log.WithName("get-all-samples")

	// Get the raw sample data
	var count int
	var samples []sampleInstants
	func() {
		s.samplesMutex.Lock()
		defer s.samplesMutex.Unlock()
		s.samples.Next().Do(func(i interface{}) {
			if i == nil { // haven't populated this yet
				return
			}
			count++
			samples = append(samples, i.(sampleInstants))
		})
	}()

	all := allSampleInstants{
		containers: map[ContainerKey]*sampleResult{},
		node:       map[samplerserverv1alpha1.NodeAggregationLevel]*sampleResult{},
	}
	for i := range samples {
		sample := samples[i]
		// Index by container
		for k, v := range sample.containers {
			if !v.HasCPUData {
				// sample is missing normalized CPU information, skip it rather than returning 0 values
				continue
			}
			if v.CPUCoresNanoSec > uint64(s.metricsReader.MaxCPUCoresNanoSec) {
				// filter samples outside the acceptable range
				continue
			}
			if v.CPUThrottledUSec > uint64(s.metricsReader.MaxCPUCoresNanoSec) {
				// filter samples outside the acceptable range
				continue
			}
			c, ok := all.containers[k]
			if !ok {
				// we don't know if this container has all sample, but allocate the space anyway
				c = &sampleResult{values: make([]sampleInstant, 0, len(samples))}
				all.containers[k] = c
			}
			c.values = append(c.values, v)
		}

		for level, v := range sample.node {
			if !v.HasCPUData {
				// sample is missing normalized CPU information, skip it rather than returning 0 values
				continue
			}
			if v.CPUCoresNanoSec > uint64(s.metricsReader.MaxCPUCoresNanoSec) {
				// filter samples outside the acceptable range
				continue
			}
			if v.CPUThrottledUSec > uint64(s.metricsReader.MaxCPUCoresNanoSec) {
				// filter samples outside the acceptable range
				continue
			}

			n, ok := all.node[level]
			if !ok {
				// we don't know if this container has all samples, but allocate the space anyway
				n = &sampleResult{values: make([]sampleInstant, 0, len(samples))}
				all.node[level] = n
			}
			n.values = append(n.values, v)
		}
	}
	for _, c := range all.containers {
		// populate the mean values
		s.populateSummary(c)
	}
	for _, n := range all.node {
		// populate the mean values
		s.populateSummary(n)
	}
	log.V(3).Info("returning samples", "count", len(all.containers))
	return all, count
}

func (s *sampleCache) fetchCAdvisorSample(samples *sampleInstants) error {
	if samples == nil {
		return nil
	}
	containers, err := s.cadvisorClient.SubcontainersInfo("kubelet", &v1.ContainerInfoRequest{})
	if err != nil {
		return fmt.Errorf("failed to retrieve kubelet containers: %w", err)
	}

	for _, container := range containers {
		// Assert pod containers.
		key := ContainerKeyFromCAdvisorContainerInfo(container)
		if key.PodUID == "" {
			continue // Skip all non-pod containers.
		}

		// Assert containers for current samples.
		sample, found := samples.containers[key]
		if !found {
			// Skip containers that are not found in provided samples.
			continue
		}

		// Assert container with stats.
		if len(container.Stats) == 0 {
			continue // Skip all containers without stats.
		}
		// Use the most recent container stats values.
		// TODO(ichekrygin): do we need to reassert stats order based on the ContainerStats.Timestamp?
		//  cadvisor/metrics doesn't seem to do that.
		sample.CAdvisorContainerStats = *container.Stats[0]
	}
	return nil
}

// fetchSample fetches a new Sample from containerd
func (s *sampleCache) fetchSample() error {
	log := log.WithName("fetch-sample")

	var cpuMetrics cpuMetrics
	var memoryMetrics memoryMetrics
	var err error

	if s.UseContainerMonitor {
		cpuMetrics, memoryMetrics, err = s.getContainerCPUAndMemoryCM()
	} else {
		cpuMetrics, memoryMetrics, err = s.getContainerCPUAndMemory()
	}

	if err != nil {
		log.Error(err, "failed to get cpu and memory metrics")
		return err
	}

	results := sampleInstants{
		containers: map[ContainerKey]sampleInstant{},
		node:       map[samplerserverv1alpha1.NodeAggregationLevel]sampleInstant{},
	}
	for key, cpu := range cpuMetrics {
		memory := memoryMetrics[key]

		sample := s.containerToSample(key, cpu, memory)
		log.V(5).Info("got sample", "sample", sample, "container", key, "found")
		results.containers[key] = sample
	}

	// node level

	nodeCPUMetrics := map[samplerserverv1alpha1.NodeAggregationLevel]containerCPUMetrics{}
	for level, files := range s.metricsReader.nodeCPUFiles {
		metrics, err := s.metricsReader.GetLevelCPUMetrics(files)
		if err != nil {
			return err
		}
		nodeCPUMetrics[level] = metrics
	}

	nodeMemoryMetrics := map[samplerserverv1alpha1.NodeAggregationLevel]containerMemoryMetrics{}
	for level, files := range s.metricsReader.nodeMemoryFiles {
		metrics, err := s.metricsReader.GetLevelMemoryMetrics(files)
		if err != nil {
			return err
		}
		nodeMemoryMetrics[level] = metrics
	}
	// assemble node metrics
	results.node = s.nodeToSample(nodeCPUMetrics, nodeMemoryMetrics)

	s.AddSample(results)
	return nil
}

func (s *sampleCache) getContainerCPUAndMemory() (cpuMetrics, memoryMetrics, error) {
	cpuMetrics, err := s.metricsReader.GetContainerCPUMetrics()
	if err != nil {
		log.Error(err, "failed to get cpu metrics")
		return nil, nil, err
	}
	if len(cpuMetrics) == 0 {
		log.Info("no cacheable results for cpu metrics", "paths", s.metricsReader.CPUPaths)
		return nil, nil, err
	}

	memoryMetrics, err := s.metricsReader.GetContainerMemoryMetrics()
	if err != nil {
		log.Error(err, "failed to get memory metrics")
		return nil, nil, err
	}
	if len(memoryMetrics) == 0 {
		log.Info("no cacheable results for memory metrics", "paths", s.metricsReader.MemoryPaths)
		return nil, nil, err
	}
	return cpuMetrics, memoryMetrics, nil
}

// AddSample adds a sample read from containerd.
// This function is public so that tests can add testdata to a Cache for integration testing.
func (s *sampleCache) AddSample(results sampleInstants) {
	s.samplesMutex.Lock()
	defer s.samplesMutex.Unlock()
	log.V(5).Info("caching samples", "container-count", len(results.containers))
	s.samples = s.samples.Next() // increment to the next element
	s.samples.Value = results
}

// containerToSample returns a sampleInstant for the container read from containerd
func (s *sampleCache) containerToSample(
	id ContainerKey,
	cpu containerCPUMetrics,
	memory containerMemoryMetrics,
) sampleInstant {
	last := s.lastSampleForContainer(id)
	sample := s.metricToSample(last, cpu, memory)
	return sample
}

func (s *sampleCache) nodeToSample(
	cpu map[samplerserverv1alpha1.NodeAggregationLevel]containerCPUMetrics,
	memory map[samplerserverv1alpha1.NodeAggregationLevel]containerMemoryMetrics,
) map[samplerserverv1alpha1.NodeAggregationLevel]sampleInstant {
	last := s.lastSampleForNode()
	samples := map[samplerserverv1alpha1.NodeAggregationLevel]sampleInstant{}
	for level := range cpu { // assume cpu and memory have the same aggregation levels
		samples[level] = s.metricToSample(last[level], cpu[level], memory[level])
	}
	return samples
}

// lastSampleForContainer returns the last Sample read for a container
func (s *sampleCache) lastSampleForContainer(id ContainerKey) sampleInstant {
	s.samplesMutex.Lock()
	defer s.samplesMutex.Unlock()

	if s.samples.Value == nil {
		return sampleInstant{}
	}
	last := s.samples.Value.(sampleInstants)
	if result, ok := last.containers[id]; ok {
		return result
	}
	return sampleInstant{}
}

func (s *sampleCache) lastSampleForNode() map[samplerserverv1alpha1.NodeAggregationLevel]sampleInstant {
	s.samplesMutex.Lock()
	defer s.samplesMutex.Unlock()

	if s.samples.Value == nil {
		return map[samplerserverv1alpha1.NodeAggregationLevel]sampleInstant{}
	}
	last := s.samples.Value.(sampleInstants)
	return last.node
}

// metricToSample parses the metric into a sample, deriving values from the last sample
func (s *sampleCache) metricToSample(
	last sampleInstant,
	cpu containerCPUMetrics,
	memory containerMemoryMetrics) sampleInstant {

	sample := sampleInstant{
		Time:                          cpu.usage.Time,
		CumulativeCPUUsec:             cpu.usage.UsageNanoSec,
		CumulativeCPUThrottlingUsec:   cpu.throttling.ThrottledNanoSec,
		CumulativeCPUPeriods:          cpu.throttling.TotalPeriods,
		CumulativeCPUThrottledPeriods: cpu.throttling.ThrottledPeriods,
		MemoryBytes:                   memory.RSS + memory.Cache,
		CumulativeMemoryOOMKill:       memory.OOMKills,
		CumulativeMemoryOOM:           memory.OOMs,
	}

	s.computeSampleDelta(&last, &sample)

	return sample
}

// TODO(ichekrygin) - what happens when last == sample? This could occur if/when
//
//	cgroups resource file was not updated between two consecutive fetches.
//	Most of the sample metrics set/updated in this routine are derived using a variation of
//	"new" - "old"!
func (s *sampleCache) computeSampleDelta(last, sample *sampleInstant) {
	if last.Time.IsZero() {
		// only compute rate if the last sample was set
		return
	}

	// this should be roughly equal to the polling period, but we don't know for sure

	sec := getSeconds(last, sample)
	sample.HasCPUData = true
	sample.CPUCoresNanoSec = normalizeSeconds(last.CumulativeCPUUsec, sample.CumulativeCPUUsec, sec)
	sample.CPUThrottledUSec = normalizeSeconds(last.CumulativeCPUThrottlingUsec, sample.CumulativeCPUThrottlingUsec, sec)

	// normalize total cpu scheduling period and cpu scheduling throttle period
	sample.CPUPeriodsSec = normalizeSeconds(last.CumulativeCPUPeriods, sample.CumulativeCPUPeriods, sec)
	sample.CPUThrottledPeriodsSec = normalizeSeconds(last.CumulativeCPUThrottledPeriods, sample.CumulativeCPUThrottledPeriods, sec)

	deltaPeriods := float64(sample.CumulativeCPUPeriods) - float64(last.CumulativeCPUPeriods)
	if deltaPeriods != 0 {
		// Avoid posting a NaN if no scheduling periods have elapsed
		sample.CPUPercentPeriodsThrottled = (float64(sample.CumulativeCPUThrottledPeriods) - float64(last.CumulativeCPUThrottledPeriods)) / deltaPeriods
	}

	sample.MemoryOOM = sample.CumulativeMemoryOOM - last.CumulativeMemoryOOM
	sample.MemoryOOMKill = sample.CumulativeMemoryOOMKill - last.CumulativeMemoryOOMKill

	sample.CAdvisorContainerStats = cadvisor.DeltaContainerStats(sample.CAdvisorContainerStats, last.CAdvisorContainerStats)
}

func (s *sampleCache) populateSummary(sr *sampleResult) {
	count := int64(len(sr.values))
	if count < 2 {
		// don't have enough samples to have a meaningful summary
		return
	}

	// compute mean using the cumulative values from the first and last samples
	// NOTE: the first sample has a cpu value that we will not be
	// able to calculate because it is computed from the difference of
	// a sample that is no longer stored
	first := sr.values[0]
	sr.avg = sr.values[len(sr.values)-1]
	s.computeSampleDelta(&first, &sr.avg)

	var mem, cpuCoresNanoSec, cpuThrottledUSec uint64
	cadvisorContainerStats := v1.ContainerStats{}
	var l uint64
	for i := range sr.values {
		if i == 0 && pointer.BoolDeref(s.metricsReader.DropFirstValue, false) {
			// skip the first value (see comment above)
			continue
		}
		l++
		v := sr.values[i]
		mem += v.MemoryBytes
		cpuCoresNanoSec += v.CPUCoresNanoSec
		cpuThrottledUSec += v.CPUThrottledUSec
		cadvisorContainerStats = cadvisor.AddContainerStats(cadvisorContainerStats, v.CAdvisorContainerStats)
	}

	sr.avg.MemoryBytes = mem / l

	// Fallback on average of individual values if we found any anomalous values
	// Anomalous values will be thrown away, so first check if we don't have
	// the full count of values for the container.
	// Also do a secondary defensive sanity check to make sure the average falls
	// in the expected range.
	if len(sr.values) < s.Size ||
		sr.avg.CPUCoresNanoSec > s.metricsReader.MaxCPUCoresNanoSec ||
		int64(sr.avg.CPUCoresNanoSec) < s.metricsReader.MinCPUCoresNanoSec {
		sr.avg.CPUCoresNanoSec = cpuCoresNanoSec / l
	}
	if len(sr.values) < s.Size ||
		sr.avg.CPUThrottledUSec > s.metricsReader.MaxCPUCoresNanoSec ||
		int64(sr.avg.CPUThrottledUSec) < s.metricsReader.MinCPUCoresNanoSec {
		sr.avg.CPUThrottledUSec = cpuThrottledUSec / l
	}

	if len(sr.values) < s.Size || cadvisor.HasResetInContainerStats(sr.avg.CAdvisorContainerStats) {
		sr.avg.CAdvisorContainerStats = cadvisor.DivideContainerStats(cadvisorContainerStats, l)
	}
}

// getSeconds returns the number of seconds between 2 samples
func getSeconds(old, new *sampleInstant) float64 {
	return new.Time.Sub(old.Time).Seconds()
}

// normalizeSeconds takes the delta of values between 2 samples, and normalizes
// the value by dividing by the number of seconds between samples.
func normalizeSeconds(old, new uint64, sec float64) uint64 {
	return uint64(math.Max(float64(new-old)/sec, 0.))
}

// init intializes the cache before it is started
func (s *sampleCache) init() {
	s.once.Do(func() {
		s.metricsReader.Reader = s.readerConfig
		s.samples = ring.New(s.Size)
	})
}
