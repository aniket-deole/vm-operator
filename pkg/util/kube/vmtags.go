// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha6"
	vspherepolv1 "github.com/vmware-tanzu/vm-operator/external/vsphere-policy/api/v1alpha1"
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

// RegisterVMTagsIndexes registers the two field indexes used to query
// Tag and VirtualMachine objects on their label relationships:
// TagOwnerReferencesUIDIndexKey and VMLabelKeyValueIndexKey.
//
// It is called once per manager, from the VirtualMachine controller's
// AddToManager under the Features.TaggingAPI gate, because every consumer of
// these indexes is reached through that controller's reconcile path: the Tag
// watch's own TagToVirtualMachineMapper (VMLabelKeyValueIndexKey), and
// pruneStaleOwnership/ReleaseOwnership (TagOwnerReferencesUIDIndexKey). The
// Tag controller queries none of them, so registration lives with the
// consumer rather than with the reconciler for the indexed type — that keeps
// a manager that wires only the VM controller, such as a per-controller
// envtest suite, from starting with an index its watch requires missing.
//
// Registering the same object/field pair twice returns an error, so this must
// not also be called from another AddToManager in the same manager.
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
// Potential optimization is to only reconcile VMs when
// - we add the first owner -> Tag is created
// - we remove the last owner -> Tag is deleted
// As owner membership does not change the behavior of that tag, there is
// no point in starting unnecessary reconciles.
// TagFanOutPredicate already filters events these.
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
