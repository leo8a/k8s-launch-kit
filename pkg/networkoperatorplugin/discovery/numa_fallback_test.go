// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Host probe now emits an optional third numa_node field per GPU.
func TestParseGPUHostObservationNUMA(t *testing.T) {
	out := "GPU\t0000:08:00.0\t0\nGPU\t0000:18:00.0\t1\nHOSTDONE\t2\n" +
		"NIC\t0000:03:00.0\tmlx5_0\nDONE\n"
	host, err := parseGPUHostObservation(out)
	require.NoError(t, err)
	assert.Equal(t, 0, host.gpuNUMA[normalizePCI("0000:08:00.0")])
	assert.Equal(t, 1, host.gpuNUMA[normalizePCI("0000:18:00.0")])

	// Legacy two-field GPU records parse with an unknown (-1) node.
	legacy, err := parseGPUHostObservation("GPU\t0000:08:00.0\nHOSTDONE\t1\nDONE\n")
	require.NoError(t, err)
	assert.Equal(t, -1, legacy.gpuNUMA[normalizePCI("0000:08:00.0")])
}

func TestGPUsOnNICNUMA(t *testing.T) {
	obs := nodeGPUObservation{
		host: gpuHostObservation{gpuNUMA: map[string]int{
			"0000:08:00.0": 0, "0000:09:00.0": 0, "0000:18:00.0": 1, "0000:19:00.0": 1,
		}},
		data: topoData{
			gpuPCI:       map[int]string{0: "0000:08:00.0", 1: "0000:09:00.0", 2: "0000:18:00.0", 3: "0000:19:00.0"},
			pfAttributes: map[string]map[string]string{"0000:03:00.0": {pfAttributeNUMANode: "0"}, "0000:13:00.0": {pfAttributeNUMANode: "1"}},
		},
	}
	all := []int{0, 1, 2, 3}
	assert.Equal(t, []int{0, 1}, gpusOnNICNUMA("0000:03:00.0", all, obs), "NIC on NUMA 0 narrows to GPU0/1")
	assert.Equal(t, []int{2, 3}, gpusOnNICNUMA("0000:13:00.0", all, obs), "NIC on NUMA 1 narrows to GPU2/3")

	// Unknown NIC NUMA -> nil (caller keeps original candidates).
	assert.Nil(t, gpusOnNICNUMA("0000:99:00.0", all, obs))

	// Unknown GPU NUMA is skipped.
	obs.host.gpuNUMA["0000:08:00.0"] = -1
	assert.Equal(t, []int{1}, gpusOnNICNUMA("0000:03:00.0", all, obs))
}

// End-to-end: a flat (all-SYS) GPU<->NIC matrix is resolved by NUMA locality,
// so each rail pairs with a GPU on its own NUMA node (the GB300 case).
func TestPairNodeGPUsFlatTopologyUsesNUMA(t *testing.T) {
	flat := func() map[int]proximity { return map[int]proximity{0: proxSYS, 1: proxSYS, 2: proxSYS, 3: proxSYS} }
	obs := nodeGPUObservation{
		host: gpuHostObservation{
			rdma:    map[string]string{"0000:03:00.0": "mlx5_0", "0000:13:00.0": "mlx5_2"},
			gpuNUMA: map[string]int{"0000:08:00.0": 0, "0000:09:00.0": 0, "0000:18:00.0": 1, "0000:19:00.0": 1},
		},
		data: topoData{
			gpuPCI:       map[int]string{0: "0000:08:00.0", 1: "0000:09:00.0", 2: "0000:18:00.0", 3: "0000:19:00.0"},
			matrix:       map[string]map[int]proximity{"mlx5_0": flat(), "mlx5_2": flat()},
			pfAttributes: map[string]map[string]string{"0000:03:00.0": {pfAttributeNUMANode: "0"}, "0000:13:00.0": {pfAttributeNUMANode: "1"}},
		},
	}
	identities := []nodePFIdentity{
		{groupIndex: 0, node: "n", retainedPCI: "0000:03:00.0", pci: "0000:03:00.0", rdmaDevice: "mlx5_0"},
		{groupIndex: 0, node: "n", retainedPCI: "0000:13:00.0", pci: "0000:13:00.0", rdmaDevice: "mlx5_2"},
	}
	result := pairNodeGPUs(identities, map[string]nodeGPUObservation{"n": obs})
	assert.Equal(t, "GPU0", result["n"]["0000:03:00.0"].index, "rail on NUMA 0 -> GPU0")
	assert.Equal(t, "GPU2", result["n"]["0000:13:00.0"].index, "rail on NUMA 1 -> GPU2 (NUMA-local)")
}

// A per-node probe failure (e.g. one worker's exec times out) must not discard
// GPU pairing for the whole homogeneous group; the healthy worker resolves it.
func TestApplyGPUObservationsToleratesNodeError(t *testing.T) {
	groups, devices, members, _ := fabricFixtures(2, true)
	identities := buildPFIdentities(groups, devices, members, true)
	good := gpuParsedFixture(t)
	good.data.matrix["mlx5_local0"] = map[int]proximity{0: proxPHB, 1: proxPHB}
	good.data.matrix["mlx5_local1"] = map[int]proximity{0: proxPHB, 1: proxPHB}
	observations := map[string]nodeGPUObservation{
		"worker-0": good,
		"worker-1": {err: context.DeadlineExceeded},
	}
	applyGPUObservations(context.Background(), groups, identities, observations)
	assert.Equal(t, "GPU0", groups[0].PFs[0].ConnectedGPU, "resolved from the healthy worker despite worker-1 timeout")
}
