// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"context"
	"fmt"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	vspherepolv1 "github.com/vmware-tanzu/vm-operator/external/vsphere-policy/api/v1alpha1"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha6"
	pkglog "github.com/vmware-tanzu/vm-operator/pkg/log"
)

const (
	// TagOwnerReferencesUIDIndexKey is the field-index key used to
	// efficiently list every Tag owned by a given VirtualMachine UID.
	TagOwnerReferencesUIDIndexKey = "metadata.ownerReferences.uid"

	// VMLabelKeyValueIndexKey is the field-index key used to efficiently list
	// every VirtualMachine carrying a given label key/value pair.
	VMLabelKeyValueIndexKey = "metadata.labels.keyValue"
)

// tagLabelKeyValue joins a label key and value into the string indexed by
// VMLabelKeyValueIndexKey.
func tagLabelKeyValue(key, value string) string {
	return key + ":" + value
}

// TagOwnerReferencesUIDIndexerFunc extracts the value indexed by
// TagOwnerReferencesUIDIndexKey from a Tag object: one entry per
// owner-reference UID.
func TagOwnerReferencesUIDIndexerFunc(obj ctrlclient.Object) []string {
	tag, ok := obj.(*vspherepolv1.Tag)
	if !ok {
		return nil
	}

	ownerRefs := tag.GetOwnerReferences()
	if len(ownerRefs) == 0 {
		return nil
	}

	uids := make([]string, 0, len(ownerRefs))
	for _, ref := range ownerRefs {
		uids = append(uids, string(ref.UID))
	}

	return uids
}

// VMLabelKeyValueIndexerFunc extracts the value indexed by
// VMLabelKeyValueIndexKey from a VirtualMachine object: one
// "<key>:<value>" entry per label, after RemoveVMOperatorLabels. It does not
// mutate the cached object, since RemoveVMOperatorLabels returns a fresh map.
func VMLabelKeyValueIndexerFunc(obj ctrlclient.Object) []string {
	vm, ok := obj.(*vmopv1.VirtualMachine)
	if !ok {
		return nil
	}

	labels := RemoveVMOperatorLabels(vm.GetLabels())
	if len(labels) == 0 {
		return nil
	}

	values := make([]string, 0, len(labels))
	for k, v := range labels {
		values = append(values, tagLabelKeyValue(k, v))
	}

	return values
}

// RegisterVMTagsIndexes registers the Tag owner-UID and VM label-pair indexes.
// The VM controller calls it once per manager under Features.TaggingAPI:
// ownership cleanup uses TagOwnerReferencesUIDIndexKey, and the Tag watch's
// mapper uses VMLabelKeyValueIndexKey. Managers running only the VM controller
// therefore also have every index required by its reconciliation path.
// Registering the same object/field pair twice returns an error, so another
// controller must not register these indexes in the same manager.
func RegisterVMTagsIndexes(ctx context.Context, fieldIndexer ctrlclient.FieldIndexer) error {
	if err := fieldIndexer.IndexField(
		ctx,
		&vspherepolv1.Tag{},
		TagOwnerReferencesUIDIndexKey,
		TagOwnerReferencesUIDIndexerFunc); err != nil {
		return err
	}

	return fieldIndexer.IndexField(
		ctx,
		&vmopv1.VirtualMachine{},
		VMLabelKeyValueIndexKey,
		VMLabelKeyValueIndexerFunc)
}

// TagToVirtualMachineMapper returns a mapper function used to enqueue
// reconcile requests for VMs in response to an event on a Tag. It resolves
// the Tag to the VMs that carry its label with a single indexed List
// against VMLabelKeyValueIndexKey, rather than a namespace scan, and
// returns one request per hit — reconciling owners and label-only VMs
// alike.
//
// Only the first owner being added (Tag created) and the last owner being
// removed (Tag deleted) change which VMs carry the tag. Owner membership
// changes in between do not, so TagFanOutPredicate filters those events out.
func TagToVirtualMachineMapper(
	ctx context.Context,
	k8sClient ctrlclient.Client) handler.MapFunc {

	if ctx == nil {
		panic("context is nil")
	}

	if k8sClient == nil {
		panic("k8sClient is nil")
	}

	return func(ctx context.Context, o ctrlclient.Object) []reconcile.Request {
		if ctx == nil {
			panic("context is nil")
		}

		if o == nil {
			panic("object is nil")
		}
		tag, ok := o.(*vspherepolv1.Tag)
		if !ok {
			panic(fmt.Sprintf("object is %T", o))
		}

		logger := pkglog.FromContextOrDefault(ctx).
			WithValues("name", tag.GetName(), "namespace", tag.GetNamespace())
		logger.V(4).Info("Reconciling all VMs carrying a Tag's label")

		vmList := &vmopv1.VirtualMachineList{}
		if err := k8sClient.List(
			ctx,
			vmList,
			ctrlclient.InNamespace(tag.Namespace),
			ctrlclient.MatchingFields{
				VMLabelKeyValueIndexKey: tagLabelKeyValue(tag.Spec.Key, tag.Spec.Value),
			}); err != nil {

			logger.Error(err,
				"Failed to list VirtualMachines for reconciliation due to Tag watch")
			return nil
		}

		requests := make([]reconcile.Request, 0, len(vmList.Items))
		for i := range vmList.Items {
			vm := vmList.Items[i]
			requests = append(requests, reconcile.Request{
				NamespacedName: ctrlclient.ObjectKey{
					Namespace: vm.Namespace,
					Name:      vm.Name,
				},
			})
		}

		if len(requests) > 0 {
			logger.V(4).Info("Reconciling VMs due to Tag watch", "requests", requests)
		}

		return requests
	}
}

// TagFanOutPredicate returns a predicate that admits only the Tag events
// that can change a VM's desired tag set: create and delete. The Tag has no
// finalizer, so a delete is atomic — there is no persisted deletionTimestamp
// transition to watch for separately. spec.key/spec.value are immutable
// by admission and the mapper's key is derived from spec alone, so every
// update — the Tag controller's own status and label-mirror writes, and the
// VM path's owner-reference patches — is filtered out to avoid waking every
// VM carrying the label on writes that cannot change the answer.
func TagFanOutPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool {
			return true
		},
		UpdateFunc: func(event.UpdateEvent) bool {
			return false
		},
		DeleteFunc: func(event.DeleteEvent) bool {
			return true
		},
		GenericFunc: func(event.GenericEvent) bool {
			return false
		},
	}
}
