// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package virtualmachine

import (
	"context"
	"fmt"
	"sort"
	"strings"

	vimtypes "github.com/vmware/govmomi/vim25/types"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vspherepolv1 "github.com/vmware-tanzu/vm-operator/external/vsphere-policy/api/v1alpha1"

	pkgctx "github.com/vmware-tanzu/vm-operator/pkg/context"
	pkgutil "github.com/vmware-tanzu/vm-operator/pkg/util"
	kubeutil "github.com/vmware-tanzu/vm-operator/pkg/util/kube"
)

// ExtraConfigVMTagsKey is the ExtraConfig key that records the
// comma-joined set of "key:value" vCenter tags this feature has applied to
// the VM.
const ExtraConfigVMTagsKey = "vmservice.tags"

const RecordTagInExtraConfig = true

// LabelPair is a label key/value pair referenced by a VM's own
// spec.affinity.
type LabelPair struct {
	Key   string
	Value string
}

// TagResourceName returns the derived name of the Tag resource that
// represents the given label key/value pair: "tag-" followed by the first
// 17 hex characters of the SHA-1 sum of "<key>:<value>".
func TagResourceName(key, value string) string {
	return "tag-" + pkgutil.SHA1Sum17(VCenterTagName(key, value))
}

// VCenterTagName returns the vCenter tag name for the given label
// key/value pair: "<key>:<value>". The tag category is the namespace.
func VCenterTagName(key, value string) string {
	return key + ":" + value
}

// AppendExistingTagSpecs appends an add-only TagSpec for each of the VM's
// own labels that has a matching Tag resource.
// When configureExtraConfig is true, the tags are
// appended to configSpec.ExtraConfig under ExtraConfigVMTagsKey.
func AppendExistingTagSpecs(
	ctx context.Context,
	k8sClient ctrlclient.Client,
	vmCtx pkgctx.VirtualMachineContext,
	configSpec *vimtypes.VirtualMachineConfigSpec,
	configureExtraConfig bool) error {

	labels := kubeutil.RemoveVMOperatorLabels(vmCtx.VM.Labels)
	if len(labels) == 0 {
		return nil
	}

	pairs := make([]LabelPair, 0, len(labels))
	for k, v := range labels {
		pairs = append(pairs, LabelPair{Key: k, Value: v})
	}

	// Sort by the full "key:value" tag name so the order matches how
	// pkg/vmconfig/vmtags's reconciler orders its own comma-joined
	// ExtraConfig value (via sets.List, which sorts on the whole string).
	// Sorting on the label key alone can disagree with that ordering.
	sort.Slice(pairs, func(i, j int) bool {
		return VCenterTagName(pairs[i].Key, pairs[i].Value) < VCenterTagName(pairs[j].Key, pairs[j].Value)
	})

	var applied []string //nolint:prealloc

	for _, pair := range pairs {
		key, value := pair.Key, pair.Value
		name := TagResourceName(key, value)

		tag := &vspherepolv1.Tag{}
		err := k8sClient.Get(
			ctx,
			ctrlclient.ObjectKey{Namespace: vmCtx.VM.Namespace, Name: name},
			tag)

		if apierrors.IsNotFound(err) {
			continue
		}

		if err != nil {
			return fmt.Errorf("failed to get Tag %q: %w", name, err)
		}

		tagName := VCenterTagName(key, value)
		configSpec.TagSpecs = append(configSpec.TagSpecs, vimtypes.TagSpec{
			ArrayUpdateSpec: vimtypes.ArrayUpdateSpec{
				Operation: vimtypes.ArrayUpdateOperationAdd,
			},
			Id: vimtypes.TagId{
				NameId: &vimtypes.TagIdNameId{
					Tag:      tagName,
					Category: vmCtx.VM.Namespace,
				},
			},
		})

		applied = append(applied, tagName)
	}

	if configureExtraConfig && len(applied) > 0 {
		configSpec.ExtraConfig = append(configSpec.ExtraConfig, &vimtypes.OptionValue{
			Key:   ExtraConfigVMTagsKey,
			Value: strings.Join(applied, ","),
		})
	}

	return nil
}
