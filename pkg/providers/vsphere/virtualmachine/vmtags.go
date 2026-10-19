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
	"k8s.io/apimachinery/pkg/util/sets"
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

// RecordTagInExtraConfig requests recording applied tags on the VM.
const RecordTagInExtraConfig = true

// LabelPair is a label key/value pair referenced by a VM's own
// spec.affinity.
type LabelPair struct {
	Key   string
	Value string
}

// TagResourceName returns the derived name of the Tag resource that
// represents the given label key/value pair in the given namespace:
// "tag-" followed by the 16 hex character XXHash64 digest of
// "<namespace>:<key>:<value>".
// Using XXHash64 instead of SHA1 to avoid gosec warnings.
func TagResourceName(namespace, key, value string) string {
	return "tag-" + pkgutil.XXHash64Hex(namespace+":"+TagName(key, value))
}

// TagName returns the tag name for the given label
// key/value pair: "<key>:<value>". The tag category is the namespace.
func TagName(key, value string) string {
	return key + ":" + value
}

// AppendExistingTagSpecs appends an add-only TagSpec for each of the VM's
// own labels that has a matching Tag resource, unless the same name/category
// addition is already present in configSpec.TagSpecs.
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
		return TagName(pairs[i].Key, pairs[i].Value) < TagName(pairs[j].Key, pairs[j].Value)
	})

	// Placement inherits the create config, which may already contain these
	// additions. Deduplicate within this request without consulting the VM's
	// persisted ExtraConfig record.
	existingAdds := sets.New[string]()
	for _, tagSpec := range configSpec.TagSpecs {
		if nameID := tagSpec.Id.NameId; nameID != nil &&
			tagSpec.Operation == vimtypes.ArrayUpdateOperationAdd &&
			nameID.Category == vmCtx.VM.Namespace {

			existingAdds.Insert(nameID.Tag)
		}
	}

	var applied []string //nolint:prealloc

	for _, pair := range pairs {
		key, value := pair.Key, pair.Value
		name := TagResourceName(vmCtx.VM.Namespace, key, value)

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

		tagName := TagName(key, value)
		if !existingAdds.Has(tagName) {
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
			existingAdds.Insert(tagName)
		}

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
