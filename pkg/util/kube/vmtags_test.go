// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package kube_test

import (
	"context"
	"errors"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha6"
	vspherepolv1 "github.com/vmware-tanzu/vm-operator/external/vsphere-policy/api/v1alpha1"
	"github.com/vmware-tanzu/vm-operator/test/builder"

	kubeutil "github.com/vmware-tanzu/vm-operator/pkg/util/kube"
)

var _ = Describe("TagOwnerReferencesUIDIndexerFunc", func() {
	When("the object is not a Tag", func() {
		It("should return nil", func() {
			Expect(kubeutil.TagOwnerReferencesUIDIndexerFunc(&corev1.Pod{})).To(BeNil())
		})
	})

	When("the object is a Tag", func() {
		When("there are no owner references", func() {
			It("should return nil", func() {
				tag := &vspherepolv1.Tag{}
				Expect(kubeutil.TagOwnerReferencesUIDIndexerFunc(tag)).To(BeNil())
			})
		})

		When("there is a single owner reference", func() {
			It("should return the owner's UID", func() {
				tag := &vspherepolv1.Tag{
					ObjectMeta: metav1.ObjectMeta{
						OwnerReferences: []metav1.OwnerReference{
							{UID: types.UID("vm-a-uid")},
						},
					},
				}
				Expect(kubeutil.TagOwnerReferencesUIDIndexerFunc(tag)).To(
					ConsistOf("vm-a-uid"))
			})
		})

		When("there are several owner references", func() {
			It("should return one entry per owner UID", func() {
				tag := &vspherepolv1.Tag{
					ObjectMeta: metav1.ObjectMeta{
						OwnerReferences: []metav1.OwnerReference{
							{UID: types.UID("vm-a-uid")},
							{UID: types.UID("vm-b-uid")},
							{UID: types.UID("vm-c-uid")},
						},
					},
				}
				Expect(kubeutil.TagOwnerReferencesUIDIndexerFunc(tag)).To(
					ConsistOf("vm-a-uid", "vm-b-uid", "vm-c-uid"))
			})
		})
	})
})

var _ = Describe("VMLabelKeyValueIndexerFunc", func() {
	When("the object is not a VirtualMachine", func() {
		It("should return nil", func() {
			Expect(kubeutil.VMLabelKeyValueIndexerFunc(&corev1.Pod{})).To(BeNil())
		})
	})

	When("the object is a VirtualMachine", func() {
		When("there are no labels", func() {
			It("should return nil", func() {
				vm := &vmopv1.VirtualMachine{}
				Expect(kubeutil.VMLabelKeyValueIndexerFunc(vm)).To(BeEmpty())
			})
		})

		When("there is a single label", func() {
			It("should return one key/value entry", func() {
				vm := &vmopv1.VirtualMachine{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app": "nginx",
						},
					},
				}
				Expect(kubeutil.VMLabelKeyValueIndexerFunc(vm)).To(ConsistOf("app:nginx"))
			})
		})

		When("there are multiple labels", func() {
			It("should return one entry per label", func() {
				vm := &vmopv1.VirtualMachine{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app": "nginx",
							"env": "prod",
						},
					},
				}
				Expect(kubeutil.VMLabelKeyValueIndexerFunc(vm)).To(
					ConsistOf("app:nginx", "env:prod"))
			})
		})

		When("a label has an empty value", func() {
			It("should return the key/value entry with an empty value", func() {
				vm := &vmopv1.VirtualMachine{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app": "",
						},
					},
				}
				Expect(kubeutil.VMLabelKeyValueIndexerFunc(vm)).To(ConsistOf("app:"))
			})
		})

		When("a label key is prefixed", func() {
			It("should return the entry with the full prefixed key", func() {
				vm := &vmopv1.VirtualMachine{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"example.com/app": "nginx",
						},
					},
				}
				Expect(kubeutil.VMLabelKeyValueIndexerFunc(vm)).To(
					ConsistOf("example.com/app:nginx"))
			})
		})

		When("the VM carries a reserved VM Operator label", func() {
			It("should filter it out of the index", func() {
				vm := &vmopv1.VirtualMachine{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app":                              "nginx",
							"vmoperator.vmware.com/some-label": "value",
						},
					},
				}
				Expect(kubeutil.VMLabelKeyValueIndexerFunc(vm)).To(ConsistOf("app:nginx"))
			})
		})

		When("the VM carries only reserved VM Operator labels", func() {
			It("should return an empty slice", func() {
				vm := &vmopv1.VirtualMachine{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"vmoperator.vmware.com/some-label": "value",
						},
					},
				}
				Expect(kubeutil.VMLabelKeyValueIndexerFunc(vm)).To(BeEmpty())
			})
		})

		When("the VM has several labels", func() {
			It("should not mutate the VM's own label map", func() {
				vm := &vmopv1.VirtualMachine{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app":                              "nginx",
							"env":                              "prod",
							"vmoperator.vmware.com/some-label": "value",
						},
					},
				}
				before := len(vm.Labels)
				_ = kubeutil.VMLabelKeyValueIndexerFunc(vm)
				Expect(vm.Labels).To(HaveLen(before))
				Expect(vm.Labels).To(HaveKey("vmoperator.vmware.com/some-label"))
			})
		})
	})
})

// recordingFieldIndexer is a minimal ctrlclient.FieldIndexer that records the
// object/field pairs it was asked to index, so RegisterVMTagsIndexes
// can be verified without a running manager or API server.
type recordingFieldIndexer struct {
	registered []recordedIndex
}

type recordedIndex struct {
	obj   ctrlclient.Object
	field string
}

func (r *recordingFieldIndexer) IndexField(
	_ context.Context,
	obj ctrlclient.Object,
	field string,
	_ ctrlclient.IndexerFunc) error {

	r.registered = append(r.registered, recordedIndex{obj: obj, field: field})
	return nil
}

var _ = Describe("RegisterVMTagsIndexes", func() {
	It("registers both indexes", func() {
		indexer := &recordingFieldIndexer{}
		Expect(kubeutil.RegisterVMTagsIndexes(context.Background(), indexer)).To(Succeed())

		Expect(indexer.registered).To(HaveLen(2))
		Expect(indexer.registered).To(ConsistOf(
			recordedIndex{obj: &vspherepolv1.Tag{}, field: kubeutil.TagOwnerReferencesUIDIndexKey},
			recordedIndex{obj: &vmopv1.VirtualMachine{}, field: kubeutil.VMLabelKeyValueIndexKey},
		))
	})

	When("an index registration fails", func() {
		It("propagates the error", func() {
			expectedErr := errors.New("fake")
			indexer := &erroringFieldIndexer{err: expectedErr}
			Expect(kubeutil.RegisterVMTagsIndexes(context.Background(), indexer)).To(
				MatchError(expectedErr))
		})
	})
})

// erroringFieldIndexer always fails on IndexField, to exercise
// RegisterVMTagsIndexes' error propagation.
type erroringFieldIndexer struct {
	err error
}

func (e *erroringFieldIndexer) IndexField(
	context.Context, ctrlclient.Object, string, ctrlclient.IndexerFunc) error {

	return e.err
}

var _ = Describe("TagToVirtualMachineMapper", func() {
	const namespaceName = "fake-ns"

	var (
		ctx       context.Context
		k8sClient ctrlclient.Client
		withObjs  []ctrlclient.Object
		withFuncs interceptor.Funcs
		tag       *vspherepolv1.Tag
		mapFn     handler.MapFunc
		mapFnCtx  context.Context
		mapFnObj  ctrlclient.Object
		reqs      []reconcile.Request
	)

	BeforeEach(func() {
		reqs = nil
		withObjs = nil
		withFuncs = interceptor.Funcs{}

		ctx = context.Background()
		mapFnCtx = ctx

		tag = &vspherepolv1.Tag{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespaceName,
				Name:      "tag-1",
			},
			Spec: vspherepolv1.TagSpec{
				Key:   "app",
				Value: "nginx",
			},
		}
		mapFnObj = tag
	})

	JustBeforeEach(func() {
		k8sClient = fake.NewClientBuilder().
			WithScheme(builder.NewScheme()).
			WithInterceptorFuncs(withFuncs).
			WithObjects(withObjs...).
			WithIndex(
				&vmopv1.VirtualMachine{},
				kubeutil.VMLabelKeyValueIndexKey,
				kubeutil.VMLabelKeyValueIndexerFunc).
			Build()
	})

	When("panic is expected", func() {
		When("ctx is nil", func() {
			BeforeEach(func() {
				ctx = nil
			})
			It("should panic", func() {
				Expect(func() {
					_ = kubeutil.TagToVirtualMachineMapper(ctx, k8sClient)
				}).To(PanicWith("context is nil"))
			})
		})

		When("k8sClient is nil", func() {
			JustBeforeEach(func() {
				k8sClient = nil
			})
			It("should panic", func() {
				Expect(func() {
					_ = kubeutil.TagToVirtualMachineMapper(ctx, k8sClient)
				}).To(PanicWith("k8sClient is nil"))
			})
		})

		Context("mapFn", func() {
			JustBeforeEach(func() {
				mapFn = kubeutil.TagToVirtualMachineMapper(ctx, k8sClient)
				Expect(mapFn).ToNot(BeNil())
			})

			When("ctx is nil", func() {
				BeforeEach(func() {
					mapFnCtx = nil
				})
				It("should panic", func() {
					Expect(func() {
						_ = mapFn(mapFnCtx, mapFnObj)
					}).To(PanicWith("context is nil"))
				})
			})

			When("object is nil", func() {
				BeforeEach(func() {
					mapFnObj = nil
				})
				It("should panic", func() {
					Expect(func() {
						_ = mapFn(mapFnCtx, mapFnObj)
					}).To(PanicWith("object is nil"))
				})
			})

			When("object is not a Tag", func() {
				It("should panic", func() {
					obj := &vmopv1.VirtualMachine{}
					Expect(func() {
						_ = mapFn(mapFnCtx, obj)
					}).To(PanicWith(fmt.Sprintf("object is %T", obj)))
				})
			})
		})
	})

	When("panic is not expected", func() {
		JustBeforeEach(func() {
			mapFn = kubeutil.TagToVirtualMachineMapper(ctx, k8sClient)
			Expect(mapFn).ToNot(BeNil())
			reqs = mapFn(mapFnCtx, mapFnObj)
		})

		When("there is an error listing VMs", func() {
			BeforeEach(func() {
				withFuncs.List = func(
					ctx context.Context,
					c ctrlclient.WithWatch,
					list ctrlclient.ObjectList,
					opts ...ctrlclient.ListOption) error {

					if _, ok := list.(*vmopv1.VirtualMachineList); ok {
						return errors.New("fake")
					}
					return c.List(ctx, list, opts...)
				}
			})
			Specify("no reconcile requests should be returned", func() {
				Expect(reqs).To(BeEmpty())
			})
		})

		When("no VM carries the Tag's label", func() {
			Specify("no reconcile requests should be returned", func() {
				Expect(reqs).To(BeEmpty())
			})
		})

		When("a VM carries the same key with a different value", func() {
			BeforeEach(func() {
				withObjs = append(withObjs, &vmopv1.VirtualMachine{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: namespaceName,
						Name:      "vm-other-value",
						Labels:    map[string]string{"app": "not-nginx"},
					},
				})
			})
			Specify("no reconcile requests should be returned", func() {
				Expect(reqs).To(BeEmpty())
			})
		})

		When("a matching VM is in a different namespace", func() {
			BeforeEach(func() {
				withObjs = append(withObjs, &vmopv1.VirtualMachine{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "other-ns",
						Name:      "vm-other-ns",
						Labels:    map[string]string{"app": "nginx"},
					},
				})
			})
			Specify("no reconcile requests should be returned", func() {
				Expect(reqs).To(BeEmpty())
			})
		})

		When("a single VM carries the Tag's label", func() {
			BeforeEach(func() {
				withObjs = append(withObjs, &vmopv1.VirtualMachine{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: namespaceName,
						Name:      "vm-match",
						Labels:    map[string]string{"app": "nginx"},
					},
				})
			})
			Specify("one reconcile request should be returned", func() {
				Expect(reqs).To(ConsistOf(
					reconcile.Request{
						NamespacedName: types.NamespacedName{
							Namespace: namespaceName,
							Name:      "vm-match",
						},
					},
				))
			})
		})

		When("multiple VMs carry the Tag's label, including an owner", func() {
			BeforeEach(func() {
				withObjs = append(withObjs,
					&vmopv1.VirtualMachine{
						ObjectMeta: metav1.ObjectMeta{
							Namespace: namespaceName,
							Name:      "vm-owner",
							Labels:    map[string]string{"app": "nginx"},
							OwnerReferences: []metav1.OwnerReference{
								{UID: types.UID("vm-owner-uid")},
							},
						},
					},
					&vmopv1.VirtualMachine{
						ObjectMeta: metav1.ObjectMeta{
							Namespace: namespaceName,
							Name:      "vm-label-only",
							Labels:    map[string]string{"app": "nginx"},
						},
					},
					&vmopv1.VirtualMachine{
						ObjectMeta: metav1.ObjectMeta{
							Namespace: namespaceName,
							Name:      "vm-unrelated",
						},
					},
				)
			})
			Specify("one reconcile request per carrying VM should be returned", func() {
				Expect(reqs).To(ConsistOf(
					reconcile.Request{
						NamespacedName: types.NamespacedName{
							Namespace: namespaceName,
							Name:      "vm-owner",
						},
					},
					reconcile.Request{
						NamespacedName: types.NamespacedName{
							Namespace: namespaceName,
							Name:      "vm-label-only",
						},
					},
				))
			})
		})
	})
})

var _ = Describe("TagFanOutPredicate", func() {
	var (
		pred   predicate.Predicate
		oldTag *vspherepolv1.Tag
		newTag *vspherepolv1.Tag
	)

	BeforeEach(func() {
		pred = kubeutil.TagFanOutPredicate()
		oldTag = &vspherepolv1.Tag{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "fake-ns",
				Name:      "tag-1",
			},
		}
		newTag = oldTag.DeepCopy()
	})

	When("Create", func() {
		It("should admit", func() {
			Expect(pred.Create(event.CreateEvent{Object: newTag})).To(BeTrue())
		})
	})

	When("Delete", func() {
		It("should admit", func() {
			Expect(pred.Delete(event.DeleteEvent{Object: oldTag})).To(BeTrue())
		})
	})

	When("Generic", func() {
		It("should reject", func() {
			Expect(pred.Generic(event.GenericEvent{Object: oldTag})).To(BeFalse())
		})
	})

	When("Update", func() {
		// The Tag carries no finalizer, so a delete is atomic and never
		// surfaces as an update setting deletionTimestamp — every update is
		// the Tag controller's own status/label-mirror write or the VM
		// path's owner-reference patch, none of which can change any VM's
		// desired tag set (spec.key/spec.value are immutable by
		// admission).
		When("the update is status-only", func() {
			It("should reject", func() {
				newTag.Status.ObservedGeneration = 1
				Expect(pred.Update(event.UpdateEvent{
					ObjectOld: oldTag,
					ObjectNew: newTag,
				})).To(BeFalse())
			})
		})

		When("the update is label-only", func() {
			It("should reject", func() {
				newTag.Labels = map[string]string{"app": "nginx"}
				Expect(pred.Update(event.UpdateEvent{
					ObjectOld: oldTag,
					ObjectNew: newTag,
				})).To(BeFalse())
			})
		})

		When("the update is owner-reference-only", func() {
			It("should reject", func() {
				newTag.OwnerReferences = []metav1.OwnerReference{
					{UID: types.UID("vm-a-uid")},
				}
				Expect(pred.Update(event.UpdateEvent{
					ObjectOld: oldTag,
					ObjectNew: newTag,
				})).To(BeFalse())
			})
		})
	})
})
