// Copyright 2026 NVIDIA CORPORATION & AFFILIATES.
//
// SPDX-License-Identifier: Apache-2.0

package networkoperatorplugin

import (
	"context"
	"fmt"

	"github.com/nvidia/k8s-launch-kit/pkg/config"
	"github.com/nvidia/k8s-launch-kit/pkg/ui"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Offline fallbacks, used when discover cannot read a value from a PackageManifest.
const (
	olmCatalogCertified    = "certified-operators"
	olmCatalogRedHat       = "redhat-operators"
	olmCatalogSourceNS     = "openshift-marketplace"
	olmDefaultChannel      = "stable"
	olmNetworkOperatorPkg  = "nvidia-network-operator"
	olmMaintenanceOperator = "nvidia-maintenance-operator"
)

// fallbackOLMSource maps a package to its catalog: NVIDIA operators are
// certified, everything else is Red Hat.
func fallbackOLMSource(packageName string) string {
	switch packageName {
	case olmNetworkOperatorPkg, olmMaintenanceOperator:
		return olmCatalogCertified
	default:
		return olmCatalogRedHat
	}
}

// DefaultOCPOperatorSubscriptions returns offline install descriptors for the
// operators l8k configures (same selection as requiredOCPOperators). discover
// overrides channel/source/sourceNamespace from each PackageManifest.
func DefaultOCPOperatorSubscriptions(cfg *config.LaunchKitConfig) []config.OCPOperatorSubscription {
	operators := requiredOCPOperators(cfg, true)
	subs := make([]config.OCPOperatorSubscription, 0, len(operators))
	for _, op := range operators {
		subs = append(subs, config.OCPOperatorSubscription{
			Package:             op.packageName,
			Namespace:           op.namespace,
			Channel:             olmDefaultChannel,
			Source:              fallbackOLMSource(op.packageName),
			SourceNamespace:     olmCatalogSourceNS,
			InstallPlanApproval: config.DefaultOCPInstallPlanApproval,
		})
	}
	return subs
}

var packageManifestListGVK = schema.GroupVersionKind{
	Group:   "packages.operators.coreos.com",
	Version: "v1",
	Kind:    "PackageManifestList",
}

// DiscoverOCPOperatorInstall resolves each configured operator's
// channel/source/sourceNamespace from its OLM PackageManifest and stores the
// result in cfg.OCPOperatorInstall. A package with no manifest keeps the offline
// fallback and warns.
func DiscoverOCPOperatorInstall(ctx context.Context, c client.Client, cfg *config.LaunchKitConfig, out ui.Output) error {
	if cfg == nil || cfg.Flavor != config.FlavorOCP {
		return nil
	}
	operators := DefaultOCPOperatorSubscriptions(cfg)

	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(packageManifestListGVK)
	if err := c.List(ctx, list); err != nil {
		return fmt.Errorf("list OLM PackageManifests: %w", err)
	}

	byPackage := make(map[string]*unstructured.Unstructured, len(list.Items))
	for i := range list.Items {
		item := &list.Items[i]
		name, _, _ := unstructured.NestedString(item.Object, "status", "packageName")
		if name == "" {
			name = item.GetName()
		}
		if _, seen := byPackage[name]; !seen {
			byPackage[name] = item
		}
	}

	for i := range operators {
		op := &operators[i]
		manifest, ok := byPackage[op.Package]
		if !ok {
			if out != nil {
				out.Warning(
					"No OLM PackageManifest found for %q; using fallback channel %q / source %q. Verify the catalog is available before deploy.",
					op.Package, op.Channel, op.Source)
			}
			continue
		}
		if channel, _, _ := unstructured.NestedString(manifest.Object, "status", "defaultChannel"); channel != "" {
			op.Channel = channel
		}
		if source, _, _ := unstructured.NestedString(manifest.Object, "status", "catalogSource"); source != "" {
			op.Source = source
		}
		if sourceNS, _, _ := unstructured.NestedString(manifest.Object, "status", "catalogSourceNamespace"); sourceNS != "" {
			op.SourceNamespace = sourceNS
		}
	}

	cfg.OCPOperatorInstall = &config.OCPOperatorInstallConfig{
		Manage:    true,
		Operators: operators,
	}
	return nil
}

// EnsureOCPOperatorInstallDefaults backfills the ocpOperatorInstall block with
// offline defaults when management is on (e.g. generate without a prior
// discover). No-op otherwise.
func EnsureOCPOperatorInstallDefaults(cfg *config.LaunchKitConfig) {
	if cfg == nil || cfg.Flavor != config.FlavorOCP {
		return
	}
	if cfg.OCPOperatorInstall == nil || !cfg.OCPOperatorInstall.Manage {
		return
	}
	if len(cfg.OCPOperatorInstall.Operators) == 0 {
		cfg.OCPOperatorInstall.Operators = DefaultOCPOperatorSubscriptions(cfg)
		return
	}
	for i := range cfg.OCPOperatorInstall.Operators {
		op := &cfg.OCPOperatorInstall.Operators[i]
		if op.Channel == "" {
			op.Channel = olmDefaultChannel
		}
		if op.Source == "" {
			op.Source = fallbackOLMSource(op.Package)
		}
		if op.SourceNamespace == "" {
			op.SourceNamespace = olmCatalogSourceNS
		}
		if op.InstallPlanApproval == "" {
			op.InstallPlanApproval = config.DefaultOCPInstallPlanApproval
		}
	}
}
