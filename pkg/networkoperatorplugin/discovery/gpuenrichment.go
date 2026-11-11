// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
//
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
)

type gpuTuple struct{ index, pci, proximity string }

// pairNodeGPUs validates raw sibling evidence before choosing one GPU per
// retained PF. Balancing belongs to retained rails, not their collapsed planes.
// Node-local RDMA names are never written into shared config.
func pairNodeGPUs(identities []nodePFIdentity, observations map[string]nodeGPUObservation) map[string]map[string]gpuTuple {
	byNode := map[string]map[string][]nodePFIdentity{}
	for _, identity := range identities {
		if byNode[identity.node] == nil {
			byNode[identity.node] = map[string][]nodePFIdentity{}
		}
		byNode[identity.node][identity.retainedPCI] = append(byNode[identity.node][identity.retainedPCI], identity)
	}
	result := map[string]map[string]gpuTuple{}
	for node, retained := range byNode {
		observation := observations[node]
		if observation.err != nil || observation.cpuOnly {
			continue
		}
		pcis := make([]string, 0, len(retained))
		for pci := range retained {
			pcis = append(pcis, pci)
		}
		slices.Sort(pcis)
		usage := map[int]int{}
		result[node] = map[string]gpuTuple{}
		for _, pci := range pcis {
			best, candidates := commonNearestGPUs(retained[pci], observation)
			// Fallback for flat topology: when the nearest proximity is SYS the
			// PCIe fabric gives no GPU-local signal (e.g. GB300, where the
			// ConnectX-8 SuperNICs attach via the Grace C2C/NVLink fabric, so
			// nvidia-smi reports every GPU<->NIC pair as SYS). Narrow candidates
			// to GPUs sharing the NIC's NUMA node; pickConnectedGPU then
			// round-robins within that NUMA-local set. Skipped when NUMA is
			// unknown, preserving the original candidates.
			if best == proxSYS {
				if narrowed := gpusOnNICNUMA(pci, candidates, observation); len(narrowed) > 0 {
					candidates = narrowed
				}
			}
			chosen := pickConnectedGPU(candidates, pci, observation.data.gpuPCI, usage)
			if chosen < 0 {
				continue
			}
			usage[chosen]++
			tuple := gpuTuple{fmt.Sprintf("GPU%d", chosen), observation.data.gpuPCI[chosen], best.String()}
			for _, identity := range retained[pci] {
				result[node][identity.pci] = tuple
			}
		}
	}
	return result
}

// A retained PF can describe all its sibling planes only when they share a
// nearest GPU at the same proximity. Overlapping tie sets are compatible; an
// empty intersection, missing member or differing proximity is unknown.
func commonNearestGPUs(siblings []nodePFIdentity, observation nodeGPUObservation) (proximity, []int) {
	commonBest := proxUnknown
	var common []int
	for i, identity := range siblings {
		rdma := observation.host.rdma[identity.pci]
		row := observation.data.matrix[rdma]
		if identity.missing != "" || rdma == "" || len(row) == 0 || len(row) != len(observation.data.gpuPCI) {
			return proxUnknown, nil
		}
		best := proxUnknown
		var candidates []int
		for index, p := range row {
			if p == proxUnknown || observation.data.gpuPCI[index] == "" {
				return proxUnknown, nil
			}
			if best == proxUnknown || p < best {
				best, candidates = p, []int{index}
			} else if p == best {
				candidates = append(candidates, index)
			}
		}
		if i == 0 {
			commonBest, common = best, candidates
			continue
		}
		if best != commonBest {
			return proxUnknown, nil
		}
		common = slices.DeleteFunc(common, func(index int) bool { return !slices.Contains(candidates, index) })
		if len(common) == 0 {
			return proxUnknown, nil
		}
	}
	return commonBest, common
}

func applyGPUObservations(ctx context.Context, groups []config.ClusterConfig, identities []nodePFIdentity, observations map[string]nodeGPUObservation) {
	tuples := pairNodeGPUs(identities, observations)
	for i := range groups {
		group := &groups[i]
		allCPU := len(group.WorkerNodes) > 0
		for _, node := range group.WorkerNodes {
			allCPU = allCPU && observations[node].cpuOnly
		}
		if allCPU {
			continue
		}
		if group.GPUType == "" {
			product, complete := "", len(group.WorkerNodes) > 0
			var evidence []string
			for _, node := range group.WorkerNodes {
				candidate := observations[node].product
				if candidate == "" || (product != "" && candidate != product) {
					complete = false
				}
				product = candidate
				if candidate == "" {
					candidate = "unavailable"
				}
				evidence = append(evidence, node+": "+candidate)
			}
			if complete {
				group.GPUType = product
			} else {
				ui.FromContext(ctx).Warning("GPU product unconfirmed for group %q; worker products are incomplete or disagree, leaving gpuType unset: %s", group.Identifier, strings.Join(evidence, "; "))
			}
		}

		complete := true
		var reasons []string
		var skipped []string
		for p := range group.PFs {
			pf := &group.PFs[p]
			pf.ConnectedGPU, pf.ConnectedGPUPCIAddress, pf.GPUProximity = "", "", ""
			var agreed gpuTuple
			pfComplete, count := true, 0
			for _, identity := range identities {
				if identity.groupIndex != i || identity.retainedPCI != normalizePCI(pf.PciAddress) {
					continue
				}
				// Tolerate a per-node probe failure: a flaky or timed-out worker
				// must not discard GPU pairing for the whole homogeneous group.
				// Skip it and rely on the workers that produced evidence; if every
				// worker failed, count stays 0 and the PF is left unset below.
				if observations[identity.node].err != nil {
					skipped = append(skipped, identity.node+": "+truncateForLog(observations[identity.node].err.Error(), 180))
					continue
				}
				count++
				candidate, found := tuples[identity.node][identity.pci]
				if identity.missing != "" || !found || (agreed.index != "" && agreed != candidate) {
					pfComplete = false
					reason := identity.node + "/" + identity.pci
					switch {
					case identity.missing != "":
						reason += ": " + identity.missing
					case !found:
						reason += ": incomplete PCI/RDMA or matrix coverage"
					default:
						reason += ": worker or collapsed PF disagreement"
					}
					reasons = append(reasons, reason)
				}
				agreed = candidate
			}
			if pfComplete && count > 0 && agreed.index != "" {
				pf.ConnectedGPU, pf.ConnectedGPUPCIAddress, pf.GPUProximity = agreed.index, agreed.pci, agreed.proximity
			} else if !isDualPortBlueField3(pf.DeviceID, pf.Model) {
				complete = false
			}
		}
		if complete {
			reclassifyAndReassignRails(group.PFs)
		}
		slices.Sort(skipped)
		skipped = slices.Compact(skipped)
		switch {
		case len(reasons) > 0 || !complete:
			reasons = append(reasons, skipped...)
			slices.Sort(reasons)
			reasons = slices.Compact(reasons)
			ui.FromContext(ctx).Warning("GPU topology incomplete for group %q; unavailable GPU fields stay unset and hardware/preset fallback applies: %s", group.Identifier, strings.Join(reasons, "; "))
		case len(skipped) > 0:
			ui.FromContext(ctx).Warning("GPU topology for group %q resolved from a subset of workers; skipped unreachable worker probe(s): %s", group.Identifier, strings.Join(skipped, "; "))
		}
	}
}

// Presets may replace the measured index. Reconcile the accompanying PCI
// address with all-worker evidence so a preset never leaves a stale live tuple.
func reconcileGPUPCIAddresses(group *config.ClusterConfig, observations map[string]nodeGPUObservation) {
	for i := range group.PFs {
		pf := &group.PFs[i]
		pf.ConnectedGPUPCIAddress = ""
		index, err := strconv.Atoi(strings.TrimPrefix(pf.ConnectedGPU, "GPU"))
		if err != nil || !strings.HasPrefix(pf.ConnectedGPU, "GPU") {
			continue
		}
		pci, complete := "", len(group.WorkerNodes) > 0
		for _, node := range group.WorkerNodes {
			observation := observations[node]
			candidate := observation.data.gpuPCI[index]
			if observation.err != nil || candidate == "" || (pci != "" && candidate != pci) {
				complete = false
			}
			pci = candidate
		}
		if complete {
			pf.ConnectedGPUPCIAddress = pci
		}
	}
}

// gpusOnNICNUMA narrows GPU candidates to those sharing the NIC's NUMA node.
// Used as a fallback when PCIe proximity is non-discriminating (flat SYS). It
// returns nil when the NIC NUMA is unknown or no candidate GPU has a known
// matching node, so callers keep the original (proximity-derived) candidates.
func gpusOnNICNUMA(nicPCI string, candidates []int, observation nodeGPUObservation) []int {
	nicNUMA, ok := nicNUMANode(nicPCI, observation)
	if !ok {
		return nil
	}
	var narrowed []int
	for _, index := range candidates {
		gpuPCI := observation.data.gpuPCI[index]
		if gpuPCI == "" {
			continue
		}
		numa, ok := observation.host.gpuNUMA[gpuPCI]
		if !ok || numa < 0 {
			continue
		}
		if numa == nicNUMA {
			narrowed = append(narrowed, index)
		}
	}
	return narrowed
}

// nicNUMANode returns the NIC's NUMA node from the per-PF discovery attributes.
// The boolean is false when the attribute is absent or not a non-negative int.
func nicNUMANode(nicPCI string, observation nodeGPUObservation) (int, bool) {
	attributes, ok := observation.data.pfAttributes[nicPCI]
	if !ok {
		return 0, false
	}
	value := attributes[pfAttributeNUMANode]
	if value == "" {
		return 0, false
	}
	numa, err := strconv.Atoi(value)
	if err != nil || numa < 0 {
		return 0, false
	}
	return numa, true
}
