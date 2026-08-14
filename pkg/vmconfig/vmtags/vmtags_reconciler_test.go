// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package vmtags_test

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/mo"
	vimtypes "github.com/vmware/govmomi/vim25/types"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	vspherepolv1 "github.com/vmware-tanzu/vm-operator/external/vsphere-policy/api/v1alpha1"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha6"
	pkgcfg "github.com/vmware-tanzu/vm-operator/pkg/config"
	"github.com/vmware-tanzu/vm-operator/pkg/providers/vsphere/virtualmachine"
	"github.com/vmware-tanzu/vm-operator/pkg/vmconfig/vmtags"
	"github.com/vmware-tanzu/vm-operator/test/builder"
)

const testNamespace = "test-ns"

// newVM returns a minimal VM with the given name/UID, carrying the given
// labels, and referencing the given label pairs via a required VM-affinity
// term (TopologyKey deliberately not zone/hostname, so the term is eligible
// regardless of the VMPlacementPolicies/VMAffinityDuringExecution flags).
func newVM(name string, uid types.UID, labels map[string]string, referenced ...virtualmachine.LabelPair) *vmopv1.VirtualMachine {
	vm := &vmopv1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testNamespace,
			Name:      name,
			UID:       uid,
			Labels:    labels,
		},
	}

	if len(referenced) > 0 {
		var terms []vmopv1.VMAffinityTerm
		for _, pair := range referenced {
			terms = append(terms, vmopv1.VMAffinityTerm{
				LabelSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{pair.Key: pair.Value},
				},
				TopologyKey: "custom-topology-key",
			})
		}

		vm.Spec.Affinity = &vmopv1.AffinitySpec{
			VMAffinity: &vmopv1.VMAffinitySpec{
				RequiredDuringSchedulingPreferredDuringExecution: terms,
			},
		}
	}

	return vm
}

func getTag(k8sClient ctrlclient.Client, name string) *vspherepolv1.Tag {
	tag := &vspherepolv1.Tag{}
	err := k8sClient.Get(
		context.Background(),
		ctrlclient.ObjectKey{Namespace: testNamespace, Name: name},
		tag)
	if err != nil {
		return nil
	}
	return tag
}

func ownerUIDs(tag *vspherepolv1.Tag) []string {
	uids := make([]string, 0, len(tag.OwnerReferences))
	for _, ref := range tag.OwnerReferences {
		uids = append(uids, string(ref.UID))
	}
	return uids
}

var _ = Describe("New", func() {
	It("returns a reconciler", func() {
		Expect(vmtags.New()).ToNot(BeNil())
	})
})

var _ = Describe("Name", func() {
	It("returns 'vmtags'", func() {
		Expect(vmtags.New().Name()).To(Equal("vmtags"))
	})
})

var _ = Describe("OnResult", func() {
	It("returns nil", func() {
		var ctx context.Context
		Expect(vmtags.New().OnResult(ctx, nil, mo.VirtualMachine{}, nil)).To(Succeed())
	})
})

var _ = Describe("Reconcile", func() {
	var (
		ctx        context.Context
		k8sClient  ctrlclient.Client
		vimClient  *vim25.Client
		moVM       mo.VirtualMachine
		vm         *vmopv1.VirtualMachine
		configSpec *vimtypes.VirtualMachineConfigSpec
	)

	BeforeEach(func() {
		ctx = pkgcfg.NewContextWithDefaultConfig()
		k8sClient = builder.NewFakeClient()
		vimClient = &vim25.Client{}
		moVM = mo.VirtualMachine{}
		configSpec = &vimtypes.VirtualMachineConfigSpec{}
		vm = newVM("vm-a", "vm-a-uid", map[string]string{"team": "blue"},
			virtualmachine.LabelPair{Key: "team", Value: "blue"})
	})

	When("a panic is expected", func() {
		When("ctx is nil", func() {
			It("panics", func() {
				var nilCtx context.Context
				fn := func() {
					_ = vmtags.New().Reconcile(nilCtx, k8sClient, vimClient, vm, moVM, configSpec)
				}
				Expect(fn).To(PanicWith("context is nil"))
			})
		})

		When("k8sClient is nil", func() {
			It("panics", func() {
				fn := func() {
					_ = vmtags.New().Reconcile(ctx, nil, vimClient, vm, moVM, configSpec)
				}
				Expect(fn).To(PanicWith("k8sClient is nil"))
			})
		})

		When("vimClient is nil", func() {
			It("panics", func() {
				fn := func() {
					_ = vmtags.New().Reconcile(ctx, k8sClient, nil, vm, moVM, configSpec)
				}
				Expect(fn).To(PanicWith("vimClient is nil"))
			})
		})

		When("vm is nil", func() {
			It("panics", func() {
				fn := func() {
					_ = vmtags.New().Reconcile(ctx, k8sClient, vimClient, nil, moVM, configSpec)
				}
				Expect(fn).To(PanicWith("vm is nil"))
			})
		})

		When("configSpec is nil", func() {
			It("panics", func() {
				fn := func() {
					_ = vmtags.New().Reconcile(ctx, k8sClient, vimClient, vm, moVM, nil)
				}
				Expect(fn).To(PanicWith("configSpec is nil"))
			})
		})
	})

	When("no panic is expected", func() {
		It("ensures the owned Tag and emits its TagSpec/ExtraConfig", func() {
			Expect(vmtags.New().Reconcile(ctx, k8sClient, vimClient, vm, moVM, configSpec)).To(Succeed())

			name := virtualmachine.TagResourceName("team", "blue")
			Expect(getTag(k8sClient, name)).ToNot(BeNil())

			Expect(configSpec.TagSpecs).To(HaveLen(1))
			Expect(configSpec.TagSpecs[0].Id.NameId.Tag).To(Equal("team:blue"))
			Expect(configSpec.TagSpecs[0].ArrayUpdateSpec.Operation).To(
				Equal(vimtypes.ArrayUpdateOperationAdd))

			Expect(configSpec.ExtraConfig).To(HaveLen(1))
		})

		When("the VM is enqueued by a fan-out and its desired tag set already matches its ExtraConfig record", func() {
			var (
				otherOwnedTag *vspherepolv1.Tag
				writes        int
			)

			BeforeEach(func() {
				// A label-only VM: it carries "team:blue" but does not
				// reference it via affinity, so ReconcileTagCRs ensures
				// nothing and ReconcileTagSpecs' desired set comes only
				// from the label-matched Tag below.
				vm = newVM("vm-a", "vm-a-uid", map[string]string{"team": "blue"})

				other := newVM("vm-other", "vm-other-uid", nil)
				otherOwnedTag = &vspherepolv1.Tag{
					ObjectMeta: metav1.ObjectMeta{
						Name:      virtualmachine.TagResourceName("team", "blue"),
						Namespace: testNamespace,
						Labels:    map[string]string{"team": "blue"},
					},
					Spec: vspherepolv1.TagSpec{Key: "team", Value: "blue"},
				}
				Expect(controllerutil.SetOwnerReference(
					other, otherOwnedTag, builder.NewScheme())).To(Succeed())

				moVM.Config = &vimtypes.VirtualMachineConfigInfo{
					ExtraConfig: []vimtypes.BaseOptionValue{
						&vimtypes.OptionValue{
							Key:   virtualmachine.ExtraConfigVMTagsKey,
							Value: "team:blue",
						},
					},
				}

				writes = 0
				withFuncs := interceptor.Funcs{
					Create: func(ctx context.Context, c ctrlclient.WithWatch, obj ctrlclient.Object, opts ...ctrlclient.CreateOption) error {
						writes++
						return c.Create(ctx, obj, opts...)
					},
					Patch: func(ctx context.Context, c ctrlclient.WithWatch, obj ctrlclient.Object, patch ctrlclient.Patch, opts ...ctrlclient.PatchOption) error {
						writes++
						return c.Patch(ctx, obj, patch, opts...)
					},
				}
				k8sClient = builder.NewFakeClientWithInterceptors(withFuncs, otherOwnedTag)
			})

			It("emits no TagSpec, appends no ExtraConfig entry, and writes nothing", func() {
				Expect(vmtags.New().Reconcile(ctx, k8sClient, vimClient, vm, moVM, configSpec)).To(Succeed())

				Expect(configSpec.TagSpecs).To(BeEmpty())
				Expect(configSpec.ExtraConfig).To(BeEmpty())
				Expect(writes).To(Equal(0))
			})
		})

		When("the VM carries a label it does not reference via affinity, matching a Tag owned by another VM", func() {
			var otherOwnedTag *vspherepolv1.Tag

			BeforeEach(func() {
				// vm-a carries "team:blue" but has no spec.affinity, so it
				// does not reference the pair: label carriage alone must
				// never make it an owner.
				vm = newVM("vm-a", "vm-a-uid", map[string]string{"team": "blue"})

				other := newVM("vm-other", "vm-other-uid", nil)
				otherOwnedTag = &vspherepolv1.Tag{
					ObjectMeta: metav1.ObjectMeta{
						Name:      virtualmachine.TagResourceName("team", "blue"),
						Namespace: testNamespace,
						Labels:    map[string]string{"team": "blue"},
					},
					Spec: vspherepolv1.TagSpec{Key: "team", Value: "blue"},
				}
				Expect(controllerutil.SetOwnerReference(
					other, otherOwnedTag, builder.NewScheme())).To(Succeed())

				k8sClient = builder.NewFakeClient(otherOwnedTag)
			})

			It("emits a TagSpec{add} for the label but does not add this VM as an owner", func() {
				Expect(vmtags.New().Reconcile(ctx, k8sClient, vimClient, vm, moVM, configSpec)).To(Succeed())

				Expect(configSpec.TagSpecs).To(HaveLen(1))
				Expect(configSpec.TagSpecs[0].Id.NameId.Tag).To(Equal("team:blue"))
				Expect(configSpec.TagSpecs[0].ArrayUpdateSpec.Operation).To(
					Equal(vimtypes.ArrayUpdateOperationAdd))

				tag := getTag(k8sClient, otherOwnedTag.Name)
				Expect(tag).ToNot(BeNil())
				Expect(ownerUIDs(tag)).To(ConsistOf("vm-other-uid"))
			})
		})

		When("the VM references a pair it does not carry, e.g. a VmToVmGroupsAntiAffinity target", func() {
			BeforeEach(func() {
				// vm-a references "team:blue" (an anti-affinity target group)
				// but carries "team:red" itself: it establishes the Tag's
				// ownership without becoming a carrier of the vCenter tag.
				vm = newVM("vm-a", "vm-a-uid", map[string]string{"team": "red"},
					virtualmachine.LabelPair{Key: "team", Value: "blue"})
			})

			It("creates the Tag with this VM as owner but emits no TagSpec/ExtraConfig for this VM", func() {
				Expect(vmtags.New().Reconcile(ctx, k8sClient, vimClient, vm, moVM, configSpec)).To(Succeed())

				name := virtualmachine.TagResourceName("team", "blue")
				tag := getTag(k8sClient, name)
				Expect(tag).ToNot(BeNil())
				Expect(ownerUIDs(tag)).To(ConsistOf("vm-a-uid"))

				Expect(configSpec.TagSpecs).To(BeEmpty())
				Expect(configSpec.ExtraConfig).To(BeEmpty())
			})
		})
	})
})

var _ = Describe("ReconcileTagCRs", func() {
	var (
		ctx       context.Context
		k8sClient ctrlclient.Client
		vm        *vmopv1.VirtualMachine
		withObjs  []ctrlclient.Object
		withFuncs interceptor.Funcs
	)

	BeforeEach(func() {
		ctx = pkgcfg.NewContextWithDefaultConfig()
		withObjs = nil
		withFuncs = interceptor.Funcs{}
		vm = newVM("vm-a", "vm-a-uid", map[string]string{"team": "blue"},
			virtualmachine.LabelPair{Key: "team", Value: "blue"})
	})

	JustBeforeEach(func() {
		k8sClient = builder.NewFakeClientWithInterceptors(withFuncs, withObjs...)
	})

	When("a panic is expected", func() {
		When("ctx is nil", func() {
			It("panics", func() {
				var nilCtx context.Context
				fn := func() {
					_, _ = vmtags.ReconcileTagCRs(nilCtx, k8sClient, vm)
				}
				Expect(fn).To(PanicWith("context is nil"))
			})
		})

		When("k8sClient is nil", func() {
			It("panics", func() {
				fn := func() {
					_, _ = vmtags.ReconcileTagCRs(ctx, nil, vm)
				}
				Expect(fn).To(PanicWith("k8sClient is nil"))
			})
		})

		When("vm is nil", func() {
			It("panics", func() {
				fn := func() {
					_, _ = vmtags.ReconcileTagCRs(ctx, k8sClient, nil)
				}
				Expect(fn).To(PanicWith("vm is nil"))
			})
		})
	})

	When("the VM references and carries a label pair it does not yet own", func() {
		It("creates the Tag with this VM as its sole owner", func() {
			ownedTags, err := vmtags.ReconcileTagCRs(ctx, k8sClient, vm)
			Expect(err).ToNot(HaveOccurred())
			Expect(ownedTags).To(HaveLen(1))

			name := virtualmachine.TagResourceName("team", "blue")
			Expect(ownedTags[0].Name).To(Equal(name))
			Expect(ownedTags[0].Namespace).To(Equal(testNamespace))
			Expect(ownedTags[0].Labels).To(Equal(map[string]string{"team": "blue"}))
			Expect(ownedTags[0].Spec).To(Equal(vspherepolv1.TagSpec{Key: "team", Value: "blue"}))

			tag := getTag(k8sClient, name)
			Expect(tag).ToNot(BeNil())
			Expect(ownerUIDs(tag)).To(ConsistOf("vm-a-uid"))
		})
	})

	When("the VM references a pair but does not carry it", func() {
		BeforeEach(func() {
			vm = newVM("vm-a", "vm-a-uid", map[string]string{"team": "red"},
				virtualmachine.LabelPair{Key: "team", Value: "blue"})
		})

		It("creates the Tag with this VM as its sole owner, even though it does not carry the pair", func() {
			ownedTags, err := vmtags.ReconcileTagCRs(ctx, k8sClient, vm)
			Expect(err).ToNot(HaveOccurred())
			Expect(ownedTags).To(HaveLen(1))

			name := virtualmachine.TagResourceName("team", "blue")
			Expect(ownedTags[0].Name).To(Equal(name))
			Expect(ownedTags[0].Spec).To(Equal(vspherepolv1.TagSpec{Key: "team", Value: "blue"}))

			tag := getTag(k8sClient, name)
			Expect(tag).ToNot(BeNil())
			Expect(ownerUIDs(tag)).To(ConsistOf("vm-a-uid"))
		})
	})

	When("a Tag for the pair is already owned by another VM", func() {
		var otherOwnedTag *vspherepolv1.Tag

		BeforeEach(func() {
			other := newVM("vm-other", "vm-other-uid", nil)
			otherOwnedTag = &vspherepolv1.Tag{
				ObjectMeta: metav1.ObjectMeta{
					Name:      virtualmachine.TagResourceName("team", "blue"),
					Namespace: testNamespace,
					Labels:    map[string]string{"team": "blue"},
				},
				Spec: vspherepolv1.TagSpec{Key: "team", Value: "blue"},
			}
			Expect(controllerutil.SetOwnerReference(
				other, otherOwnedTag, builder.NewScheme())).To(Succeed())

			withObjs = append(withObjs, otherOwnedTag)
		})

		It("appends this VM's owner reference rather than replacing the existing one", func() {
			_, err := vmtags.ReconcileTagCRs(ctx, k8sClient, vm)
			Expect(err).ToNot(HaveOccurred())

			tag := getTag(k8sClient, otherOwnedTag.Name)
			Expect(tag).ToNot(BeNil())
			Expect(ownerUIDs(tag)).To(ConsistOf("vm-other-uid", "vm-a-uid"))
		})
	})

	When("the existing Tag's spec does not match the pair its derived name implies", func() {
		BeforeEach(func() {
			withObjs = append(withObjs, &vspherepolv1.Tag{
				ObjectMeta: metav1.ObjectMeta{
					Name:      virtualmachine.TagResourceName("team", "blue"),
					Namespace: testNamespace,
				},
				Spec: vspherepolv1.TagSpec{Key: "team", Value: "green"},
			})
		})

		It("returns an error rather than adopting the Tag", func() {
			_, err := vmtags.ReconcileTagCRs(ctx, k8sClient, vm)
			Expect(err).To(MatchError(ContainSubstring("expected")))
		})
	})

	When("the VM previously owned a pair it no longer references", func() {
		JustBeforeEach(func() {
			// First pass: the VM references and carries "team:blue", so it
			// becomes the Tag's owner.
			_, err := vmtags.ReconcileTagCRs(ctx, k8sClient, vm)
			Expect(err).ToNot(HaveOccurred())

			name := virtualmachine.TagResourceName("team", "blue")
			Expect(ownerUIDs(getTag(k8sClient, name))).To(ConsistOf("vm-a-uid"))

			// Second pass: the VM stops referencing the pair via affinity,
			// but still carries the label.
			vm.Spec.Affinity = nil
		})

		It("removes this VM's ownership on the next reconcile, even though it still carries the label", func() {
			ownedTags, err := vmtags.ReconcileTagCRs(ctx, k8sClient, vm)
			Expect(err).ToNot(HaveOccurred())
			Expect(ownedTags).To(BeEmpty())

			name := virtualmachine.TagResourceName("team", "blue")
			Expect(ownerUIDs(getTag(k8sClient, name))).To(BeEmpty())
		})
	})

	When("the VM is deleted", func() {
		JustBeforeEach(func() {
			_, err := vmtags.ReconcileTagCRs(ctx, k8sClient, vm)
			Expect(err).ToNot(HaveOccurred())
		})

		It("ReleaseOwnership removes this VM's ownership of every Tag it owns", func() {
			name := virtualmachine.TagResourceName("team", "blue")
			Expect(ownerUIDs(getTag(k8sClient, name))).To(ConsistOf("vm-a-uid"))

			Expect(vmtags.ReleaseOwnership(ctx, k8sClient, vm)).To(Succeed())

			Expect(ownerUIDs(getTag(k8sClient, name))).To(BeEmpty())
		})
	})

	When("reconciling twice with nothing changed", func() {
		JustBeforeEach(func() {
			_, err := vmtags.ReconcileTagCRs(ctx, k8sClient, vm)
			Expect(err).ToNot(HaveOccurred())
		})

		It("writes nothing on the second reconcile", func() {
			name := virtualmachine.TagResourceName("team", "blue")
			before := getTag(k8sClient, name)
			Expect(before).ToNot(BeNil())

			writes := 0
			withFuncs = interceptor.Funcs{
				Create: func(ctx context.Context, c ctrlclient.WithWatch, obj ctrlclient.Object, opts ...ctrlclient.CreateOption) error {
					writes++
					return c.Create(ctx, obj, opts...)
				},
				Patch: func(ctx context.Context, c ctrlclient.WithWatch, obj ctrlclient.Object, patch ctrlclient.Patch, opts ...ctrlclient.PatchOption) error {
					writes++
					return c.Patch(ctx, obj, patch, opts...)
				},
			}
			k8sClient = builder.NewFakeClientWithInterceptors(withFuncs, before)

			_, err := vmtags.ReconcileTagCRs(ctx, k8sClient, vm)
			Expect(err).ToNot(HaveOccurred())
			Expect(writes).To(Equal(0))

			after := getTag(k8sClient, name)
			Expect(after.ResourceVersion).To(Equal(before.ResourceVersion))
		})
	})

	When("another VM concurrently creates the Tag between our Get and our Create", func() {
		BeforeEach(func() {
			name := virtualmachine.TagResourceName("team", "blue")

			withFuncs.Create = func(ctx context.Context, c ctrlclient.WithWatch, obj ctrlclient.Object, opts ...ctrlclient.CreateOption) error {
				other := newVM("vm-other", "vm-other-uid", nil)
				racer := &vspherepolv1.Tag{
					ObjectMeta: metav1.ObjectMeta{
						Name:      name,
						Namespace: testNamespace,
						Labels:    map[string]string{"team": "blue"},
					},
					Spec: vspherepolv1.TagSpec{Key: "team", Value: "blue"},
				}
				Expect(controllerutil.SetOwnerReference(
					other, racer, builder.NewScheme())).To(Succeed())
				Expect(c.Create(ctx, racer)).To(Succeed())

				return apierrors.NewAlreadyExists(
					schema.GroupResource{Group: vspherepolv1.GroupName, Resource: "tags"}, name)
			}
		})

		It("re-gets the Tag and adopts it rather than returning a hard error", func() {
			ownedTags, err := vmtags.ReconcileTagCRs(ctx, k8sClient, vm)
			Expect(err).ToNot(HaveOccurred())
			Expect(ownedTags).To(HaveLen(1))

			name := virtualmachine.TagResourceName("team", "blue")
			tag := getTag(k8sClient, name)
			Expect(tag).ToNot(BeNil())
			Expect(ownerUIDs(tag)).To(ConsistOf("vm-other-uid", "vm-a-uid"))
		})
	})

	When("the Tag carries a stale owner reference from a same-named VM with a different UID", func() {
		var name string

		BeforeEach(func() {
			name = virtualmachine.TagResourceName("team", "blue")

			stale := newVM("vm-a", "vm-a-old-uid", nil)
			tag := &vspherepolv1.Tag{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: testNamespace,
					Labels:    map[string]string{"team": "blue"},
				},
				Spec: vspherepolv1.TagSpec{Key: "team", Value: "blue"},
			}
			Expect(controllerutil.SetOwnerReference(
				stale, tag, builder.NewScheme())).To(Succeed())

			withObjs = append(withObjs, tag)
		})

		It("upserts the ref with the VM's current UID rather than leaving it stale", func() {
			_, err := vmtags.ReconcileTagCRs(ctx, k8sClient, vm)
			Expect(err).ToNot(HaveOccurred())

			tag := getTag(k8sClient, name)
			Expect(tag).ToNot(BeNil())
			Expect(ownerUIDs(tag)).To(ConsistOf("vm-a-uid"))
		})
	})

	When("the UID index returns a Tag whose ref was already removed (stale cache read)", func() {
		BeforeEach(func() {
			// A Tag with no owner reference for this VM at all, standing in
			// for the real scenario: a prior reconcile's patch already
			// removed the ref, but the informer cache backing this List
			// has not yet observed it and still surfaces the Tag under the
			// VM's UID index entry.
			stale := &vspherepolv1.Tag{
				ObjectMeta: metav1.ObjectMeta{
					Name:      virtualmachine.TagResourceName("team", "blue"),
					Namespace: testNamespace,
				},
				Spec: vspherepolv1.TagSpec{Key: "team", Value: "blue"},
			}

			withFuncs.List = func(ctx context.Context, c ctrlclient.WithWatch, list ctrlclient.ObjectList, opts ...ctrlclient.ListOption) error {
				tagList, ok := list.(*vspherepolv1.TagList)
				if !ok {
					return c.List(ctx, list, opts...)
				}
				tagList.Items = []vspherepolv1.Tag{*stale}
				return nil
			}

			// The VM does not reference or carry the pair, so
			// pruneStaleOwnership runs with an empty keepNames set.
			vm = newVM("vm-a", "vm-a-uid", nil)
		})

		It("skips the already-absent ref instead of erroring", func() {
			ownedTags, err := vmtags.ReconcileTagCRs(ctx, k8sClient, vm)
			Expect(err).ToNot(HaveOccurred())
			Expect(ownedTags).To(BeEmpty())
		})
	})
})

var _ = Describe("ReleaseOwnership", func() {
	var (
		ctx       context.Context
		k8sClient ctrlclient.Client
		vm        *vmopv1.VirtualMachine
		withObjs  []ctrlclient.Object
		withFuncs interceptor.Funcs
	)

	BeforeEach(func() {
		ctx = pkgcfg.NewContextWithDefaultConfig()
		withObjs = nil
		withFuncs = interceptor.Funcs{}
		vm = newVM("vm-a", "vm-a-uid", nil)
	})

	JustBeforeEach(func() {
		k8sClient = builder.NewFakeClientWithInterceptors(withFuncs, withObjs...)
	})

	When("the VM owns no Tags", func() {
		It("succeeds and writes nothing", func() {
			Expect(vmtags.ReleaseOwnership(ctx, k8sClient, vm)).To(Succeed())
		})
	})

	When("listing Tags owned by the VM fails", func() {
		BeforeEach(func() {
			withFuncs.List = func(ctx context.Context, c ctrlclient.WithWatch, list ctrlclient.ObjectList, opts ...ctrlclient.ListOption) error {
				return errors.New("boom")
			}
		})

		It("propagates the error", func() {
			Expect(vmtags.ReleaseOwnership(ctx, k8sClient, vm)).To(
				MatchError(ContainSubstring("boom")))
		})
	})

	When("patching a Tag's owner references fails", func() {
		BeforeEach(func() {
			tag := &vspherepolv1.Tag{
				ObjectMeta: metav1.ObjectMeta{
					Name:      virtualmachine.TagResourceName("team", "blue"),
					Namespace: testNamespace,
				},
				Spec: vspherepolv1.TagSpec{Key: "team", Value: "blue"},
			}
			Expect(controllerutil.SetOwnerReference(vm, tag, builder.NewScheme())).To(Succeed())
			withObjs = append(withObjs, tag)

			withFuncs.Patch = func(ctx context.Context, c ctrlclient.WithWatch, obj ctrlclient.Object, patch ctrlclient.Patch, opts ...ctrlclient.PatchOption) error {
				return errors.New("boom")
			}
		})

		It("propagates the error", func() {
			Expect(vmtags.ReleaseOwnership(ctx, k8sClient, vm)).To(
				MatchError(ContainSubstring("boom")))
		})
	})
})

// Describes the full VmToVmGroupsAntiAffinity lifecycle: a VM establishes a
// Tag purely by referencing a pair it does not carry, a second VM that
// carries the pair picks up the vCenter tag once the Tag exists, and
// deleting the referencing VM's ownership — followed by the Tag controller
// deleting the now-zero-owner Tag, simulated here since that controller is
// outside this package — makes the carrying VM lose the vCenter tag too.
var _ = Describe("VmToVmGroupsAntiAffinity lifecycle (reference without carriage)", func() {
	var (
		ctx        context.Context
		k8sClient  ctrlclient.Client
		vimClient  *vim25.Client
		configSpec *vimtypes.VirtualMachineConfigSpec
		vmA        *vmopv1.VirtualMachine
		vmB        *vmopv1.VirtualMachine
	)

	BeforeEach(func() {
		ctx = pkgcfg.NewContextWithDefaultConfig()
		k8sClient = builder.NewFakeClient()
		vimClient = &vim25.Client{}
		configSpec = &vimtypes.VirtualMachineConfigSpec{}

		// vm-a references "team:blue" without carrying it — an
		// anti-affinity term naming the target group's label.
		vmA = newVM("vm-a", "vm-a-uid", map[string]string{"team": "red"},
			virtualmachine.LabelPair{Key: "team", Value: "blue"})
		// vm-b carries "team:blue" with no affinity of its own — the
		// target group vm-a's anti-affinity is enforced against.
		vmB = newVM("vm-b", "vm-b-uid", map[string]string{"team": "blue"})
	})

	It("tags vm-b once vm-a's reconcile creates the Tag, then untags it once vm-a is deleted and the Tag is removed", func() {
		By("vm-a's reconcile creates the Tag and owns it, without tagging vm-a itself")
		Expect(vmtags.New().Reconcile(ctx, k8sClient, vimClient, vmA, mo.VirtualMachine{}, configSpec)).To(Succeed())

		name := virtualmachine.TagResourceName("team", "blue")
		tag := getTag(k8sClient, name)
		Expect(tag).ToNot(BeNil())
		Expect(ownerUIDs(tag)).To(ConsistOf("vm-a-uid"))
		Expect(configSpec.TagSpecs).To(BeEmpty())

		By("vm-b's reconcile carries the vCenter tag now that the Tag exists")
		vmBConfigSpec := &vimtypes.VirtualMachineConfigSpec{}
		Expect(vmtags.New().Reconcile(ctx, k8sClient, vimClient, vmB, mo.VirtualMachine{}, vmBConfigSpec)).To(Succeed())

		Expect(vmBConfigSpec.TagSpecs).To(HaveLen(1))
		Expect(vmBConfigSpec.TagSpecs[0].Id.NameId.Tag).To(Equal("team:blue"))
		Expect(vmBConfigSpec.TagSpecs[0].ArrayUpdateSpec.Operation).To(
			Equal(vimtypes.ArrayUpdateOperationAdd))

		By("deleting vm-a releases its ownership, leaving the Tag with no owners")
		Expect(vmtags.ReleaseOwnership(ctx, k8sClient, vmA)).To(Succeed())
		Expect(ownerUIDs(getTag(k8sClient, name))).To(BeEmpty())

		By("the Tag controller deletes the zero-owner Tag (simulated: out of this package's scope)")
		Expect(k8sClient.Delete(ctx, getTag(k8sClient, name))).To(Succeed())
		Expect(getTag(k8sClient, name)).To(BeNil())

		By("vm-b's next reconcile drops the vCenter tag it can no longer find a Tag for")
		vmBMoVM := mo.VirtualMachine{
			Config: &vimtypes.VirtualMachineConfigInfo{
				ExtraConfig: []vimtypes.BaseOptionValue{
					&vimtypes.OptionValue{
						Key:   virtualmachine.ExtraConfigVMTagsKey,
						Value: "team:blue",
					},
				},
			},
		}
		vmBConfigSpec = &vimtypes.VirtualMachineConfigSpec{}
		Expect(vmtags.New().Reconcile(ctx, k8sClient, vimClient, vmB, vmBMoVM, vmBConfigSpec)).To(Succeed())

		Expect(vmBConfigSpec.TagSpecs).To(HaveLen(1))
		Expect(vmBConfigSpec.TagSpecs[0].Id.NameId.Tag).To(Equal("team:blue"))
		Expect(vmBConfigSpec.TagSpecs[0].ArrayUpdateSpec.Operation).To(
			Equal(vimtypes.ArrayUpdateOperationRemove))
	})
})

var _ = Describe("ReconcileTagSpecs", func() {
	var (
		ctx        context.Context
		k8sClient  ctrlclient.Client
		vm         *vmopv1.VirtualMachine
		moVM       mo.VirtualMachine
		configSpec *vimtypes.VirtualMachineConfigSpec
		ownedTags  []vspherepolv1.Tag
		withObjs   []ctrlclient.Object
		withFuncs  interceptor.Funcs
	)

	BeforeEach(func() {
		ctx = pkgcfg.NewContextWithDefaultConfig()
		moVM = mo.VirtualMachine{}
		configSpec = &vimtypes.VirtualMachineConfigSpec{}
		ownedTags = nil
		withObjs = nil
		withFuncs = interceptor.Funcs{}
		vm = newVM("vm-a", "vm-a-uid", map[string]string{"team": "blue"})
	})

	JustBeforeEach(func() {
		k8sClient = builder.NewFakeClientWithInterceptors(withFuncs, withObjs...)
	})

	When("a panic is expected", func() {
		When("ctx is nil", func() {
			It("panics", func() {
				var nilCtx context.Context
				fn := func() {
					_ = vmtags.ReconcileTagSpecs(nilCtx, k8sClient, vm, moVM, configSpec, ownedTags)
				}
				Expect(fn).To(PanicWith("context is nil"))
			})
		})

		When("k8sClient is nil", func() {
			It("panics", func() {
				fn := func() {
					_ = vmtags.ReconcileTagSpecs(ctx, nil, vm, moVM, configSpec, ownedTags)
				}
				Expect(fn).To(PanicWith("k8sClient is nil"))
			})
		})

		When("vm is nil", func() {
			It("panics", func() {
				fn := func() {
					_ = vmtags.ReconcileTagSpecs(ctx, k8sClient, nil, moVM, configSpec, ownedTags)
				}
				Expect(fn).To(PanicWith("vm is nil"))
			})
		})

		When("configSpec is nil", func() {
			It("panics", func() {
				fn := func() {
					_ = vmtags.ReconcileTagSpecs(ctx, k8sClient, vm, moVM, nil, ownedTags)
				}
				Expect(fn).To(PanicWith("configSpec is nil"))
			})
		})
	})

	When("the VM carries a label with no matching Tag and owns nothing", func() {
		It("emits nothing", func() {
			Expect(vmtags.ReconcileTagSpecs(ctx, k8sClient, vm, moVM, configSpec, ownedTags)).To(Succeed())
			Expect(configSpec.TagSpecs).To(BeEmpty())
			Expect(configSpec.ExtraConfig).To(BeEmpty())
		})
	})

	When("a Tag matching the VM's label exists, owned by another VM", func() {
		BeforeEach(func() {
			withObjs = append(withObjs, &vspherepolv1.Tag{
				ObjectMeta: metav1.ObjectMeta{
					Name:      virtualmachine.TagResourceName("team", "blue"),
					Namespace: testNamespace,
				},
				Spec: vspherepolv1.TagSpec{Key: "team", Value: "blue"},
			})
		})

		It("keeps the tag in the desired set even though this VM does not own it", func() {
			Expect(vmtags.ReconcileTagSpecs(ctx, k8sClient, vm, moVM, configSpec, ownedTags)).To(Succeed())

			Expect(configSpec.TagSpecs).To(HaveLen(1))
			Expect(configSpec.TagSpecs[0].Id.NameId.Tag).To(Equal("team:blue"))
			Expect(configSpec.TagSpecs[0].ArrayUpdateSpec.Operation).To(
				Equal(vimtypes.ArrayUpdateOperationAdd))
		})
	})

	When("a Tag was just created in this pass but the client's Get cannot yet observe it", func() {
		BeforeEach(func() {
			ownedTags = []vspherepolv1.Tag{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      virtualmachine.TagResourceName("team", "blue"),
						Namespace: testNamespace,
					},
					Spec: vspherepolv1.TagSpec{Key: "team", Value: "blue"},
				},
			}

			withFuncs.Get = func(ctx context.Context, c ctrlclient.WithWatch, key ctrlclient.ObjectKey, obj ctrlclient.Object, opts ...ctrlclient.GetOption) error {
				// Force a cache miss: the just-created Tag never shows up
				// via Get, regardless of what was actually stored.
				return apierrors.NewNotFound(
					vspherepolv1.GroupVersion.WithResource("tags").GroupResource(), key.Name)
			}
		})

		It("is still in the desired set, via the ownedTags parameter", func() {
			Expect(vmtags.ReconcileTagSpecs(ctx, k8sClient, vm, moVM, configSpec, ownedTags)).To(Succeed())

			Expect(configSpec.TagSpecs).To(HaveLen(1))
			Expect(configSpec.TagSpecs[0].Id.NameId.Tag).To(Equal("team:blue"))
			Expect(configSpec.TagSpecs[0].ArrayUpdateSpec.Operation).To(
				Equal(vimtypes.ArrayUpdateOperationAdd))
		})
	})

	When("the VM carries several distinct label keys", func() {
		BeforeEach(func() {
			vm.Labels = map[string]string{"team": "blue", "env": "prod"}

			withObjs = append(withObjs,
				&vspherepolv1.Tag{
					ObjectMeta: metav1.ObjectMeta{
						Name:      virtualmachine.TagResourceName("team", "blue"),
						Namespace: testNamespace,
					},
					Spec: vspherepolv1.TagSpec{Key: "team", Value: "blue"},
				},
				&vspherepolv1.Tag{
					ObjectMeta: metav1.ObjectMeta{
						Name:      virtualmachine.TagResourceName("env", "prod"),
						Namespace: testNamespace,
					},
					Spec: vspherepolv1.TagSpec{Key: "env", Value: "prod"},
				},
			)
		})

		It("issues exactly one Get per distinct label key, on the derived name", func() {
			var gottenKeys []ctrlclient.ObjectKey

			withFuncs.Get = func(ctx context.Context, c ctrlclient.WithWatch, key ctrlclient.ObjectKey, obj ctrlclient.Object, opts ...ctrlclient.GetOption) error {
				gottenKeys = append(gottenKeys, key)
				return c.Get(ctx, key, obj, opts...)
			}
			k8sClient = builder.NewFakeClientWithInterceptors(withFuncs, withObjs...)

			Expect(vmtags.ReconcileTagSpecs(ctx, k8sClient, vm, moVM, configSpec, ownedTags)).To(Succeed())

			Expect(gottenKeys).To(ConsistOf(
				ctrlclient.ObjectKey{Namespace: testNamespace, Name: virtualmachine.TagResourceName("team", "blue")},
				ctrlclient.ObjectKey{Namespace: testNamespace, Name: virtualmachine.TagResourceName("env", "prod")},
			))

			Expect(configSpec.TagSpecs).To(HaveLen(2))
		})
	})

	Context("ExtraConfig record", func() {
		BeforeEach(func() {
			withObjs = append(withObjs, &vspherepolv1.Tag{
				ObjectMeta: metav1.ObjectMeta{
					Name:      virtualmachine.TagResourceName("team", "blue"),
					Namespace: testNamespace,
				},
				Spec: vspherepolv1.TagSpec{Key: "team", Value: "blue"},
			})
		})

		When("no record exists yet", func() {
			It("writes the record and emits adds only", func() {
				Expect(vmtags.ReconcileTagSpecs(ctx, k8sClient, vm, moVM, configSpec, ownedTags)).To(Succeed())

				Expect(configSpec.TagSpecs).To(HaveLen(1))
				Expect(configSpec.TagSpecs[0].ArrayUpdateSpec.Operation).To(
					Equal(vimtypes.ArrayUpdateOperationAdd))

				Expect(configSpec.ExtraConfig).To(HaveLen(1))
				ov := configSpec.ExtraConfig[0].GetOptionValue()
				Expect(ov.Key).To(Equal(virtualmachine.ExtraConfigVMTagsKey))
				Expect(ov.Value).To(Equal("team:blue"))
			})
		})

		When("the record already matches the desired set", func() {
			BeforeEach(func() {
				moVM.Config = &vimtypes.VirtualMachineConfigInfo{
					ExtraConfig: []vimtypes.BaseOptionValue{
						&vimtypes.OptionValue{
							Key:   virtualmachine.ExtraConfigVMTagsKey,
							Value: "team:blue",
						},
					},
				}
			})

			It("emits nothing at all", func() {
				Expect(vmtags.ReconcileTagSpecs(ctx, k8sClient, vm, moVM, configSpec, ownedTags)).To(Succeed())
				Expect(configSpec.TagSpecs).To(BeEmpty())
				Expect(configSpec.ExtraConfig).To(BeEmpty())
			})
		})

		When("a pair drops out of the desired set", func() {
			BeforeEach(func() {
				moVM.Config = &vimtypes.VirtualMachineConfigInfo{
					ExtraConfig: []vimtypes.BaseOptionValue{
						&vimtypes.OptionValue{
							Key:   virtualmachine.ExtraConfigVMTagsKey,
							Value: "team:blue,env:prod",
						},
					},
				}
				// The VM no longer carries "env", and no Tag matches it
				// either, so "env:prod" drops out of the desired set.
			})

			It("emits a TagSpec{remove} and rewrites the record", func() {
				Expect(vmtags.ReconcileTagSpecs(ctx, k8sClient, vm, moVM, configSpec, ownedTags)).To(Succeed())

				Expect(configSpec.TagSpecs).To(HaveLen(1))
				Expect(configSpec.TagSpecs[0].Id.NameId.Tag).To(Equal("env:prod"))
				Expect(configSpec.TagSpecs[0].ArrayUpdateSpec.Operation).To(
					Equal(vimtypes.ArrayUpdateOperationRemove))

				Expect(configSpec.ExtraConfig).To(HaveLen(1))
				ov := configSpec.ExtraConfig[0].GetOptionValue()
				Expect(ov.Value).To(Equal("team:blue"))
			})
		})
	})

	Context("the vSphere policy path has already appended UUID-identified TagSpecs", func() {
		// vmconfig/policy runs before this reconciler in doReconfigure and
		// appends its own entries to the same slice, identified by tag UUID
		// rather than by name and category. This reconciler must add and
		// remove only its own NameId entries and leave those alone: it is
		// the reason it runs last, and nothing else asserts it.
		var policyTagSpecs []vimtypes.TagSpec

		BeforeEach(func() {
			policyTagSpecs = []vimtypes.TagSpec{
				{
					ArrayUpdateSpec: vimtypes.ArrayUpdateSpec{
						Operation: vimtypes.ArrayUpdateOperationAdd,
					},
					Id: vimtypes.TagId{Uuid: "policy-uuid-1"},
				},
				{
					ArrayUpdateSpec: vimtypes.ArrayUpdateSpec{
						Operation: vimtypes.ArrayUpdateOperationRemove,
					},
					Id: vimtypes.TagId{Uuid: "policy-uuid-2"},
				},
			}
			configSpec.TagSpecs = append(configSpec.TagSpecs, policyTagSpecs...)

			withObjs = append(withObjs, &vspherepolv1.Tag{
				ObjectMeta: metav1.ObjectMeta{
					Name:      virtualmachine.TagResourceName("team", "blue"),
					Namespace: testNamespace,
				},
				Spec: vspherepolv1.TagSpec{Key: "team", Value: "blue"},
			})

			// "env:prod" is recorded as applied but no longer justified, so
			// this reconcile emits one add and one remove of its own.
			moVM.Config = &vimtypes.VirtualMachineConfigInfo{
				ExtraConfig: []vimtypes.BaseOptionValue{
					&vimtypes.OptionValue{
						Key:   virtualmachine.ExtraConfigVMTagsKey,
						Value: "env:prod",
					},
				},
			}
		})

		It("leaves them untouched and appends only NameId entries", func() {
			Expect(vmtags.ReconcileTagSpecs(ctx, k8sClient, vm, moVM, configSpec, ownedTags)).To(Succeed())

			Expect(configSpec.TagSpecs).To(HaveLen(4))
			Expect(configSpec.TagSpecs[:2]).To(Equal(policyTagSpecs))

			for _, ts := range configSpec.TagSpecs[2:] {
				Expect(ts.Id.Uuid).To(BeEmpty())
				Expect(ts.Id.NameId).ToNot(BeNil())
				Expect(ts.Id.NameId.Category).To(Equal(testNamespace))
			}

			Expect(configSpec.TagSpecs[2].Id.NameId.Tag).To(Equal("env:prod"))
			Expect(configSpec.TagSpecs[2].ArrayUpdateSpec.Operation).To(
				Equal(vimtypes.ArrayUpdateOperationRemove))
			Expect(configSpec.TagSpecs[3].Id.NameId.Tag).To(Equal("team:blue"))
			Expect(configSpec.TagSpecs[3].ArrayUpdateSpec.Operation).To(
				Equal(vimtypes.ArrayUpdateOperationAdd))
		})
	})
})
