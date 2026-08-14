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
	pkgctx "github.com/vmware-tanzu/vm-operator/pkg/context"
	"github.com/vmware-tanzu/vm-operator/pkg/providers/vsphere/virtualmachine"
	"github.com/vmware-tanzu/vm-operator/test/builder"
)

var _ = Describe("AffinityLabelPairs", func() {
	var (
		vm    *vmopv1.VirtualMachine
		vmCtx pkgctx.VirtualMachineContext
	)

	BeforeEach(func() {
		vm = builder.DummyVirtualMachine()
		vm.Name = "test-vm"

		vmCtx = pkgctx.VirtualMachineContext{
			Context: pkgcfg.NewContext(),
			Logger:  suite.GetLogger().WithValues("vmName", vm.GetName()),
			VM:      vm,
		}
	})

	When("the VM has no affinity spec", func() {
		It("returns nil", func() {
			Expect(virtualmachine.AffinityLabelPairs(vmCtx)).To(BeNil())
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
			Expect(virtualmachine.AffinityLabelPairs(vmCtx)).To(ConsistOf(
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
			Expect(virtualmachine.AffinityLabelPairs(vmCtx)).To(ConsistOf(
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
			Expect(virtualmachine.AffinityLabelPairs(vmCtx)).To(BeEmpty())
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
			Expect(virtualmachine.AffinityLabelPairs(vmCtx)).To(ConsistOf(
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
			Expect(virtualmachine.AffinityLabelPairs(vmCtx)).To(ConsistOf(
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
			Expect(virtualmachine.AffinityLabelPairs(vmCtx)).To(ConsistOf(
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
			Expect(virtualmachine.AffinityLabelPairs(vmCtx)).To(ConsistOf(
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
			Expect(virtualmachine.AffinityLabelPairs(vmCtx)).To(ConsistOf(
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
			Expect(virtualmachine.AffinityLabelPairs(vmCtx)).To(ConsistOf(
				virtualmachine.LabelPair{Key: "zone-pair", Value: "z"},
				virtualmachine.LabelPair{Key: "host-pair", Value: "h"},
			))
		})
	})
})

var _ = Describe("TagResourceName", func() {
	It("is deterministic for the same pair", func() {
		Expect(virtualmachine.TagResourceName("app", "nginx")).To(
			Equal(virtualmachine.TagResourceName("app", "nginx")))
	})

	It("differs for different pairs", func() {
		Expect(virtualmachine.TagResourceName("app", "nginx")).ToNot(
			Equal(virtualmachine.TagResourceName("app", "apache")))
		Expect(virtualmachine.TagResourceName("app", "nginx")).ToNot(
			Equal(virtualmachine.TagResourceName("env", "nginx")))
	})

	It("is DNS-subdomain safe: \"tag-\" followed by 17 hex characters", func() {
		Expect(virtualmachine.TagResourceName("app", "nginx")).To(
			MatchRegexp(`^tag-[0-9a-f]{17}$`))
	})

	It("handles an empty value", func() {
		name := virtualmachine.TagResourceName("app", "")
		Expect(name).To(MatchRegexp(`^tag-[0-9a-f]{17}$`))
		Expect(name).ToNot(Equal(virtualmachine.TagResourceName("app", "x")))
	})

	It("handles a prefixed key", func() {
		Expect(virtualmachine.TagResourceName("example.com/app", "nginx")).To(
			MatchRegexp(`^tag-[0-9a-f]{17}$`))
	})

	It("distinguishes an empty value from every other value for the same key", func() {
		Expect(virtualmachine.TagResourceName("app", "")).ToNot(
			Equal(virtualmachine.TagResourceName("app", "nginx")))
	})
})

var _ = Describe("VCenterTagName", func() {
	It("joins the key and value with a colon", func() {
		Expect(virtualmachine.VCenterTagName("app", "nginx")).To(Equal("app:nginx"))
	})

	It("handles an empty value", func() {
		Expect(virtualmachine.VCenterTagName("app", "")).To(Equal("app:"))
	})

	It("handles a prefixed key", func() {
		Expect(virtualmachine.VCenterTagName("example.com/app", "nginx")).To(
			Equal("example.com/app:nginx"))
	})
})

var _ = Describe("AppendExistingTagSpecs", func() {
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
				Name:      virtualmachine.TagResourceName(key, value),
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
