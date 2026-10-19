// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package virtualmachine_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	vimtypes "github.com/vmware/govmomi/vim25/types"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vspherepolv1 "github.com/vmware-tanzu/vm-operator/external/vsphere-policy/api/v1alpha1"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha6"
	pkgcfg "github.com/vmware-tanzu/vm-operator/pkg/config"
	"github.com/vmware-tanzu/vm-operator/pkg/constants/testlabels"
	pkgctx "github.com/vmware-tanzu/vm-operator/pkg/context"
	"github.com/vmware-tanzu/vm-operator/pkg/providers/vsphere/virtualmachine"
	"github.com/vmware-tanzu/vm-operator/test/builder"
)

var _ = Describe("AffinityLabelPairs", func() {
	var (
		vm          *vmopv1.VirtualMachine
		vmCtx       pkgctx.VirtualMachineContext
		constraints virtualmachine.AffinityRuleConstraints
	)

	BeforeEach(func() {
		vm = builder.DummyVirtualMachine()
		vm.Name = "test-vm"

		constraints = virtualmachine.AffinityRuleConstraints{
			ConfigureHostRules: true,
			ConfigureZoneRules: true,
		}

		vmCtx = pkgctx.VirtualMachineContext{
			Context: pkgcfg.NewContext(),
			Logger:  suite.GetLogger().WithValues("vmName", vm.GetName()),
			VM:      vm,
		}
	})

	When("the VM has no affinity spec", func() {
		It("returns nil", func() {
			Expect(virtualmachine.AffinityLabelPairs(vmCtx, constraints)).To(BeNil())
		})
	})

	When("the VM affinity references matchLabels", func() {
		BeforeEach(func() {
			vmCtx.VM.Spec.Affinity = &vmopv1.AffinitySpec{
				VMAffinity: &vmopv1.VMAffinitySpec{
					RequiredDuringSchedulingPreferredDuringExecution: []vmopv1.VMAffinityTerm{
						{
							LabelSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{"app": "nginx"},
							},
							TopologyKey: corev1.LabelHostname,
						},
					},
				},
			}
		})

		It("returns the referenced pair", func() {
			Expect(virtualmachine.AffinityLabelPairs(vmCtx, constraints)).To(ConsistOf(
				virtualmachine.LabelPair{Key: "app", Value: "nginx"},
			))
		})
	})

	When("the VM affinity references matchExpressions with the In operator", func() {
		BeforeEach(func() {
			vmCtx.VM.Spec.Affinity = &vmopv1.AffinitySpec{
				VMAffinity: &vmopv1.VMAffinitySpec{
					RequiredDuringSchedulingPreferredDuringExecution: []vmopv1.VMAffinityTerm{
						{
							LabelSelector: &metav1.LabelSelector{
								MatchExpressions: []metav1.LabelSelectorRequirement{
									{
										Key:      "app",
										Operator: metav1.LabelSelectorOpIn,
										Values:   []string{"nginx", "apache"},
									},
								},
							},
							TopologyKey: corev1.LabelHostname,
						},
					},
				},
			}
		})

		It("returns one pair per value", func() {
			Expect(virtualmachine.AffinityLabelPairs(vmCtx, constraints)).To(ConsistOf(
				virtualmachine.LabelPair{Key: "app", Value: "nginx"},
				virtualmachine.LabelPair{Key: "app", Value: "apache"},
			))
		})
	})

	When("the VM affinity references an unsupported matchExpressions operator", func() {
		BeforeEach(func() {
			vmCtx.VM.Spec.Affinity = &vmopv1.AffinitySpec{
				VMAffinity: &vmopv1.VMAffinitySpec{
					RequiredDuringSchedulingPreferredDuringExecution: []vmopv1.VMAffinityTerm{
						{
							LabelSelector: &metav1.LabelSelector{
								MatchExpressions: []metav1.LabelSelectorRequirement{
									{
										Key:      "app",
										Operator: metav1.LabelSelectorOpExists,
									},
								},
							},
							TopologyKey: corev1.LabelHostname,
						},
					},
				},
			}
		})

		It("ignores the term rather than failing", func() {
			Expect(virtualmachine.AffinityLabelPairs(vmCtx, constraints)).To(BeEmpty())
		})
	})

	When("the VM affinity references a mix of eligible and ineligible terms", func() {
		BeforeEach(func() {
			vmCtx.VM.Spec.Affinity = &vmopv1.AffinitySpec{
				VMAffinity: &vmopv1.VMAffinitySpec{
					RequiredDuringSchedulingPreferredDuringExecution: []vmopv1.VMAffinityTerm{
						{
							LabelSelector: &metav1.LabelSelector{
								MatchExpressions: []metav1.LabelSelectorRequirement{
									{
										Key:      "app",
										Operator: metav1.LabelSelectorOpExists,
									},
								},
							},
							TopologyKey: corev1.LabelHostname,
						},
						{
							LabelSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{"env": "prod"},
							},
							TopologyKey: corev1.LabelHostname,
						},
					},
				},
			}
		})

		It("skips the failing term and still returns the eligible pair", func() {
			Expect(virtualmachine.AffinityLabelPairs(vmCtx, constraints)).To(ConsistOf(
				virtualmachine.LabelPair{Key: "env", Value: "prod"},
			))
		})
	})

	When("the VM anti-affinity references matchLabels", func() {
		BeforeEach(func() {
			vmCtx.VM.Spec.Affinity = &vmopv1.AffinitySpec{
				VMAntiAffinity: &vmopv1.VMAntiAffinitySpec{
					PreferredDuringSchedulingPreferredDuringExecution: []vmopv1.VMAffinityTerm{
						{
							LabelSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{"tier": "backend"},
							},
							TopologyKey: corev1.LabelTopologyZone,
						},
					},
				},
			}
		})

		It("returns the referenced pair", func() {
			Expect(virtualmachine.AffinityLabelPairs(vmCtx, constraints)).To(ConsistOf(
				virtualmachine.LabelPair{Key: "tier", Value: "backend"},
			))
		})
	})

	When("the VM references the same pair from multiple terms", func() {
		BeforeEach(func() {
			vmCtx.VM.Spec.Affinity = &vmopv1.AffinitySpec{
				VMAffinity: &vmopv1.VMAffinitySpec{
					RequiredDuringSchedulingPreferredDuringExecution: []vmopv1.VMAffinityTerm{
						{
							LabelSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{"app": "nginx"},
							},
							TopologyKey: corev1.LabelHostname,
						},
					},
					PreferredDuringSchedulingPreferredDuringExecution: []vmopv1.VMAffinityTerm{
						{
							LabelSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{"app": "nginx"},
							},
							TopologyKey: corev1.LabelHostname,
						},
					},
				},
			}
		})

		It("returns the pair only once", func() {
			Expect(virtualmachine.AffinityLabelPairs(vmCtx, constraints)).To(ConsistOf(
				virtualmachine.LabelPair{Key: "app", Value: "nginx"},
			))
		})
	})

	When("the referenced label has an empty value", func() {
		BeforeEach(func() {
			vmCtx.VM.Spec.Affinity = &vmopv1.AffinitySpec{
				VMAffinity: &vmopv1.VMAffinitySpec{
					RequiredDuringSchedulingPreferredDuringExecution: []vmopv1.VMAffinityTerm{
						{
							LabelSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{"app": ""},
							},
							TopologyKey: corev1.LabelHostname,
						},
					},
				},
			}
		})

		It("returns the pair with the empty value", func() {
			Expect(virtualmachine.AffinityLabelPairs(vmCtx, constraints)).To(ConsistOf(
				virtualmachine.LabelPair{Key: "app", Value: ""},
			))
		})
	})

	When("the referenced label key is prefixed", func() {
		BeforeEach(func() {
			vmCtx.VM.Spec.Affinity = &vmopv1.AffinitySpec{
				VMAffinity: &vmopv1.VMAffinitySpec{
					RequiredDuringSchedulingPreferredDuringExecution: []vmopv1.VMAffinityTerm{
						{
							LabelSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{"example.com/app": "nginx"},
							},
							TopologyKey: corev1.LabelHostname,
						},
					},
				},
			}
		})

		It("returns the pair with the full prefixed key", func() {
			Expect(virtualmachine.AffinityLabelPairs(vmCtx, constraints)).To(ConsistOf(
				virtualmachine.LabelPair{Key: "example.com/app", Value: "nginx"},
			))
		})
	})

	When("the VM references pairs via both host and zone topology terms", func() {
		BeforeEach(func() {
			vmCtx.VM.Spec.Affinity = &vmopv1.AffinitySpec{
				VMAffinity: &vmopv1.VMAffinitySpec{
					RequiredDuringSchedulingPreferredDuringExecution: []vmopv1.VMAffinityTerm{
						{
							LabelSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{"zone-pair": "z"},
							},
							TopologyKey: corev1.LabelTopologyZone,
						},
						{
							LabelSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{"host-pair": "h"},
							},
							TopologyKey: corev1.LabelHostname,
						},
					},
				},
			}
		})

		It("returns both pairs regardless of topology key", func() {
			Expect(virtualmachine.AffinityLabelPairs(vmCtx, constraints)).To(ConsistOf(
				virtualmachine.LabelPair{Key: "zone-pair", Value: "z"},
				virtualmachine.LabelPair{Key: "host-pair", Value: "h"},
			))
		})
	})
})

var _ = Describe("TagResourceName", func() {
	It("is deterministic for the same triple", func() {
		Expect(virtualmachine.TagResourceName("ns", "app", "nginx")).To(
			Equal(virtualmachine.TagResourceName("ns", "app", "nginx")))
	})

	It("differs for different pairs", func() {
		Expect(virtualmachine.TagResourceName("ns", "app", "nginx")).ToNot(
			Equal(virtualmachine.TagResourceName("ns", "app", "apache")))
		Expect(virtualmachine.TagResourceName("ns", "app", "nginx")).ToNot(
			Equal(virtualmachine.TagResourceName("ns", "env", "nginx")))
	})

	It("differs for different namespaces with the same key/value pair", func() {
		Expect(virtualmachine.TagResourceName("ns-a", "app", "nginx")).ToNot(
			Equal(virtualmachine.TagResourceName("ns-b", "app", "nginx")))
	})

	It("is DNS-subdomain safe: \"tag-\" followed by 16 hex characters", func() {
		Expect(virtualmachine.TagResourceName("ns", "app", "nginx")).To(
			MatchRegexp(`^tag-[0-9a-f]{16}$`))
	})

	It("handles an empty value", func() {
		name := virtualmachine.TagResourceName("ns", "app", "")
		Expect(name).To(MatchRegexp(`^tag-[0-9a-f]{16}$`))
		Expect(name).ToNot(Equal(virtualmachine.TagResourceName("ns", "app", "x")))
	})

	It("handles a prefixed key", func() {
		Expect(virtualmachine.TagResourceName("ns", "example.com/app", "nginx")).To(
			MatchRegexp(`^tag-[0-9a-f]{16}$`))
	})

	It("distinguishes an empty value from every other value for the same key", func() {
		Expect(virtualmachine.TagResourceName("ns", "app", "")).ToNot(
			Equal(virtualmachine.TagResourceName("ns", "app", "nginx")))
	})
})

var _ = Describe("TagName", func() {
	It("joins the key and value with a colon", func() {
		Expect(virtualmachine.TagName("app", "nginx")).To(Equal("app:nginx"))
	})

	It("handles an empty value", func() {
		Expect(virtualmachine.TagName("app", "")).To(Equal("app:"))
	})

	It("handles a prefixed key", func() {
		Expect(virtualmachine.TagName("example.com/app", "nginx")).To(
			Equal("example.com/app:nginx"))
	})
})

var _ = Describe("AppendExistingTagSpecs", Label(testlabels.Controller), func() {
	const (
		ns     = "test-ns"
		vmName = "test-vm"
	)

	var (
		vm                   *vmopv1.VirtualMachine
		vmCtx                pkgctx.VirtualMachineContext
		k8sClient            ctrlclient.Client
		initObjs             []ctrlclient.Object
		configSpec           vimtypes.VirtualMachineConfigSpec
		configureExtraConfig bool
		resultErr            error
	)

	dummyTag := func(key, value string) *vspherepolv1.Tag {
		return &vspherepolv1.Tag{
			ObjectMeta: metav1.ObjectMeta{
				Name:      virtualmachine.TagResourceName(ns, key, value),
				Namespace: ns,
			},
			Spec: vspherepolv1.TagSpec{
				Key:   key,
				Value: value,
			},
		}
	}

	BeforeEach(func() {
		vm = builder.DummyBasicVirtualMachine(vmName, ns)
		vm.Labels = map[string]string{"app": "web"}

		vmCtx = pkgctx.VirtualMachineContext{
			Context: pkgcfg.NewContext(),
			Logger:  suite.GetLogger().WithValues("vmName", vm.GetName()),
			VM:      vm,
		}

		initObjs = nil
		configSpec = vimtypes.VirtualMachineConfigSpec{}
		configureExtraConfig = false
	})

	JustBeforeEach(func() {
		k8sClient = builder.NewFakeClient(initObjs...)
		resultErr = virtualmachine.AppendExistingTagSpecs(
			vmCtx, k8sClient, vmCtx, &configSpec, configureExtraConfig)
	})

	When("a Tag exists matching one of the VM's labels", func() {
		BeforeEach(func() {
			initObjs = []ctrlclient.Object{dummyTag("app", "web")}
		})

		It("emits an add-only TagSpec for that label", func() {
			Expect(resultErr).ToNot(HaveOccurred())
			assertVMTags(configSpec, []string{"app:web"}, ns)
		})

		It("creates no Tag and writes no owner reference", func() {
			Expect(resultErr).ToNot(HaveOccurred())

			var tagList vspherepolv1.TagList
			Expect(k8sClient.List(vmCtx, &tagList, ctrlclient.InNamespace(ns))).To(Succeed())
			Expect(tagList.Items).To(HaveLen(1))
			Expect(tagList.Items[0].OwnerReferences).To(BeEmpty())
		})
	})

	When("placement inherits the create config", func() {
		BeforeEach(func() {
			initObjs = []ctrlclient.Object{dummyTag("app", "web")}
			configureExtraConfig = true
			pkgcfg.SetContext(vmCtx, func(config *pkgcfg.Config) {
				config.Features.TaggingAPI = true
			})
		})

		It("keeps one tag addition and preserves the create ExtraConfig record", func() {
			Expect(resultErr).ToNot(HaveOccurred())
			placementSpec, err := virtualmachine.CreateConfigSpecForPlacement(
				vmCtx, configSpec, map[string]string{})
			Expect(err).ToNot(HaveOccurred())

			Expect(virtualmachine.AppendExistingTagSpecs(
				vmCtx, k8sClient, vmCtx, &placementSpec,
				!virtualmachine.RecordTagInExtraConfig)).To(Succeed())

			assertVMTags(placementSpec, []string{"app:web"}, ns)
			assertVMTags(configSpec, []string{"app:web"}, ns)
			Expect(placementSpec.ExtraConfig).To(Equal(configSpec.ExtraConfig))
			Expect(placementSpec.ExtraConfig).To(HaveLen(1))
			Expect(placementSpec.ExtraConfig[0].GetOptionValue().Value).To(Equal("app:web"))
		})
	})

	When("the config already contains tag operations", func() {
		BeforeEach(func() {
			initObjs = []ctrlclient.Object{dummyTag("app", "web")}
			configureExtraConfig = true
		})

		DescribeTable("preserves existing operations and deduplicates only matching additions",
			func(existing vimtypes.TagSpec, wantCount int) {
				Expect(resultErr).ToNot(HaveOccurred())
				cs := vimtypes.VirtualMachineConfigSpec{TagSpecs: []vimtypes.TagSpec{existing}}
				Expect(virtualmachine.AppendExistingTagSpecs(
					vmCtx, k8sClient, vmCtx, &cs, configureExtraConfig)).To(Succeed())
				Expect(cs.TagSpecs).To(HaveLen(wantCount))
				Expect(cs.TagSpecs[0]).To(Equal(existing))
				Expect(cs.ExtraConfig).To(HaveLen(1))
				Expect(cs.ExtraConfig[0].GetOptionValue().Value).To(Equal("app:web"))
			},
			Entry("the same name/category addition", vimtypes.TagSpec{
				ArrayUpdateSpec: vimtypes.ArrayUpdateSpec{Operation: vimtypes.ArrayUpdateOperationAdd},
				Id:              vimtypes.TagId{NameId: &vimtypes.TagIdNameId{Tag: "app:web", Category: ns}},
			}, 1),
			Entry("the same name in another category", vimtypes.TagSpec{
				ArrayUpdateSpec: vimtypes.ArrayUpdateSpec{Operation: vimtypes.ArrayUpdateOperationAdd},
				Id:              vimtypes.TagId{NameId: &vimtypes.TagIdNameId{Tag: "app:web", Category: "other-ns"}},
			}, 2),
			Entry("a removal for the same name/category", vimtypes.TagSpec{
				ArrayUpdateSpec: vimtypes.ArrayUpdateSpec{Operation: vimtypes.ArrayUpdateOperationRemove},
				Id:              vimtypes.TagId{NameId: &vimtypes.TagIdNameId{Tag: "app:web", Category: ns}},
			}, 2),
			Entry("a UUID-based policy addition", vimtypes.TagSpec{
				ArrayUpdateSpec: vimtypes.ArrayUpdateSpec{Operation: vimtypes.ArrayUpdateOperationAdd},
				Id:              vimtypes.TagId{Uuid: "policy-tag-uuid"},
			}, 2),
		)
	})

	When("no Tag exists for the VM's label (first VM in a relationship)", func() {
		BeforeEach(func() {
			initObjs = nil
		})

		It("emits nothing", func() {
			Expect(resultErr).ToNot(HaveOccurred())
			Expect(configSpec.TagSpecs).To(BeEmpty())
		})
	})

	When("a Tag exists for the same key but a different value", func() {
		BeforeEach(func() {
			initObjs = []ctrlclient.Object{dummyTag("app", "other-value")}
		})

		It("emits nothing", func() {
			Expect(resultErr).ToNot(HaveOccurred())
			Expect(configSpec.TagSpecs).To(BeEmpty())
		})
	})

	When("configureExtraConfig is true", func() {
		BeforeEach(func() {
			initObjs = []ctrlclient.Object{dummyTag("app", "web")}
			configureExtraConfig = true
		})

		It("appends the ExtraConfig record for the emitted set", func() {
			Expect(resultErr).ToNot(HaveOccurred())

			var found bool

			for _, ov := range configSpec.ExtraConfig {
				if opt, ok := ov.(*vimtypes.OptionValue); ok &&
					opt.Key == virtualmachine.ExtraConfigVMTagsKey {

					found = true

					Expect(opt.Value).To(Equal("app:web"))
				}
			}

			Expect(found).To(BeTrue())
		})
	})

	When("configureExtraConfig is false", func() {
		BeforeEach(func() {
			initObjs = []ctrlclient.Object{dummyTag("app", "web")}
			configureExtraConfig = false
		})

		It("does not append the ExtraConfig record", func() {
			Expect(resultErr).ToNot(HaveOccurred())
			Expect(configSpec.ExtraConfig).To(BeEmpty())
		})
	})
})

var _ = Describe("Initial placement tags", Label(testlabels.Controller), func() {
	DescribeTable("combines carried affinity labels with existing Tag resources",
		func(topology string, antiAffinity, tagExists, inheritCreate bool) {
			vm := builder.DummyBasicVirtualMachine("placement-vm", "placement-ns")
			vm.Labels = map[string]string{"app": "web", "tier": "backend", "unrelated": "value"}
			term := vmopv1.VMAffinityTerm{
				TopologyKey:   topology,
				LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
			}
			vm.Spec.Affinity = &vmopv1.AffinitySpec{}
			if antiAffinity {
				vm.Spec.Affinity.VMAntiAffinity = &vmopv1.VMAntiAffinitySpec{
					RequiredDuringSchedulingPreferredDuringExecution: []vmopv1.VMAffinityTerm{term},
				}
			} else {
				vm.Spec.Affinity.VMAffinity = &vmopv1.VMAffinitySpec{
					RequiredDuringSchedulingPreferredDuringExecution: []vmopv1.VMAffinityTerm{term},
				}
			}
			vmCtx := pkgctx.VirtualMachineContext{Context: pkgcfg.NewContext(), VM: vm}
			pkgcfg.SetContext(vmCtx, func(config *pkgcfg.Config) {
				config.Features.TaggingAPI = true
				config.Features.VMPlacementPolicies = true
				config.Features.VMAffinityDuringExecution = true
			})
			newTag := func(key, value string) *vspherepolv1.Tag {
				return &vspherepolv1.Tag{
					ObjectMeta: metav1.ObjectMeta{Namespace: vm.Namespace, Name: virtualmachine.TagResourceName(vm.Namespace, key, value)},
					Spec:       vspherepolv1.TagSpec{Key: key, Value: value},
				}
			}
			objects := []ctrlclient.Object{newTag("tier", "backend")}
			if tagExists {
				objects = append(objects, newTag("app", "web"))
			}
			client := builder.NewFakeClient(objects...)
			createSpec := vimtypes.VirtualMachineConfigSpec{}
			if inheritCreate {
				Expect(virtualmachine.AppendExistingTagSpecs(vmCtx, client, vmCtx, &createSpec,
					virtualmachine.RecordTagInExtraConfig)).To(Succeed())
			}
			original := createSpec
			placementSpec, err := virtualmachine.CreateConfigSpecForPlacement(vmCtx, createSpec, nil)
			Expect(err).ToNot(HaveOccurred())
			Expect(virtualmachine.AppendExistingTagSpecs(vmCtx, client, vmCtx, &placementSpec,
				!virtualmachine.RecordTagInExtraConfig)).To(Succeed())
			assertVMTags(placementSpec, []string{"app:web", "tier:backend"}, vm.Namespace)
			Expect(placementSpec.VmPlacementPolicies).To(HaveLen(1))
			Expect(createSpec).To(Equal(original))
			Expect(placementSpec.ExtraConfig).To(Equal(createSpec.ExtraConfig))
			var tags vspherepolv1.TagList
			Expect(client.List(vmCtx, &tags)).To(Succeed())
			Expect(tags.Items).To(HaveLen(len(objects)))
			for _, tag := range tags.Items {
				Expect(tag.OwnerReferences).To(BeEmpty())
			}
		},
		Entry("fresh host affinity", corev1.LabelHostname, false, false, false),
		Entry("fresh host anti-affinity", corev1.LabelHostname, true, false, false),
		Entry("fresh zone affinity", corev1.LabelTopologyZone, false, false, false),
		Entry("fresh zone anti-affinity", corev1.LabelTopologyZone, true, false, false),
		Entry("overlapping existing Tag", corev1.LabelHostname, true, true, false),
		Entry("overlapping inherited create tag", corev1.LabelHostname, true, true, true),
		Entry("fresh affinity with inherited label-only tag", corev1.LabelHostname, true, false, true),
	)
})
