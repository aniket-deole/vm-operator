// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package virtualmachine

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	vimtypes "github.com/vmware/govmomi/vim25/types"

	pkgcfg "github.com/vmware-tanzu/vm-operator/pkg/config"
	"github.com/vmware-tanzu/vm-operator/pkg/vmconfig/policy"
)

// removeTagAssociations is unexported, so this is a white-box (internal
// package) test rather than the black-box _test package convention used
// elsewhere: vcsim's Reconfigure does not apply or expose TagSpecs (see the
// TODO in cleanup_test.go), so the only way to assert on the exact set of
// emitted TagSpecs is to call the pure function directly.
var _ = Describe("removeTagAssociations", func() {
	const vmNamespace = "test-ns"

	var (
		ctx        context.Context
		config     *vimtypes.VirtualMachineConfigInfo
		configSpec *vimtypes.VirtualMachineConfigSpec
	)

	nameIDRemoveTagSpec := func(tag, category string) vimtypes.TagSpec {
		return vimtypes.TagSpec{
			ArrayUpdateSpec: vimtypes.ArrayUpdateSpec{
				Operation: vimtypes.ArrayUpdateOperationRemove,
			},
			Id: vimtypes.TagId{
				NameId: &vimtypes.TagIdNameId{Tag: tag, Category: category},
			},
		}
	}

	uuidRemoveTagSpec := func(uuid string) vimtypes.TagSpec {
		return vimtypes.TagSpec{
			ArrayUpdateSpec: vimtypes.ArrayUpdateSpec{
				Operation: vimtypes.ArrayUpdateOperationRemove,
			},
			Id: vimtypes.TagId{Uuid: uuid},
		}
	}

	BeforeEach(func() {
		ctx = pkgcfg.NewContextWithDefaultConfig()

		config = &vimtypes.VirtualMachineConfigInfo{
			ExtraConfig: []vimtypes.BaseOptionValue{
				&vimtypes.OptionValue{
					Key:   ExtraConfigVMTagsKey,
					Value: "team:blue,team:red",
				},
				&vimtypes.OptionValue{
					Key:   policy.ExtraConfigPolicyTagsKey,
					Value: "policy-uuid-1,policy-uuid-2",
				},
			},
		}
		configSpec = &vimtypes.VirtualMachineConfigSpec{}
	})

	When("config is nil", func() {
		It("does nothing", func() {
			removeTagAssociations(ctx, vmNamespace, nil, configSpec)
			Expect(configSpec.TagSpecs).To(BeEmpty())
		})
	})

	When("TaggingAPI is enabled", func() {
		BeforeEach(func() {
			pkgcfg.SetContext(ctx, func(c *pkgcfg.Config) {
				c.Features.TaggingAPI = true
			})
		})

		It("emits a NameId removal for each recorded VM tag", func() {
			removeTagAssociations(ctx, vmNamespace, config, configSpec)

			Expect(configSpec.TagSpecs).To(ConsistOf(
				nameIDRemoveTagSpec("team:blue", vmNamespace),
				nameIDRemoveTagSpec("team:red", vmNamespace),
			))
		})

		When("VSpherePolicies is also enabled", func() {
			BeforeEach(func() {
				pkgcfg.SetContext(ctx, func(c *pkgcfg.Config) {
					c.Features.VSpherePolicies = true
				})
			})

			It("still emits the vSphere policy path's UUID removals alongside the affinity tag removals", func() {
				removeTagAssociations(ctx, vmNamespace, config, configSpec)

				Expect(configSpec.TagSpecs).To(ConsistOf(
					nameIDRemoveTagSpec("team:blue", vmNamespace),
					nameIDRemoveTagSpec("team:red", vmNamespace),
					uuidRemoveTagSpec("policy-uuid-1"),
					uuidRemoveTagSpec("policy-uuid-2"),
				))
			})
		})
	})

	When("TaggingAPI is disabled", func() {
		BeforeEach(func() {
			pkgcfg.SetContext(ctx, func(c *pkgcfg.Config) {
				c.Features.TaggingAPI = false
			})
		})

		It("emits nothing for this feature", func() {
			removeTagAssociations(ctx, vmNamespace, config, configSpec)
			Expect(configSpec.TagSpecs).To(BeEmpty())
		})

		When("VSpherePolicies is enabled", func() {
			BeforeEach(func() {
				pkgcfg.SetContext(ctx, func(c *pkgcfg.Config) {
					c.Features.VSpherePolicies = true
				})
			})

			It("still emits the vSphere policy path's UUID removals", func() {
				removeTagAssociations(ctx, vmNamespace, config, configSpec)

				Expect(configSpec.TagSpecs).To(ConsistOf(
					uuidRemoveTagSpec("policy-uuid-1"),
					uuidRemoveTagSpec("policy-uuid-2"),
				))
			})
		})
	})
})
