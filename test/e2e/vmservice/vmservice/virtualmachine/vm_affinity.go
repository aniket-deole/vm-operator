// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package virtualmachine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cespare/xxhash/v2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/vmware/govmomi/vapi/tags"
	"github.com/vmware/govmomi/vim25"
	vimtypes "github.com/vmware/govmomi/vim25/types"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	e2eframework "k8s.io/kubernetes/test/e2e/framework"
	capiutil "sigs.k8s.io/cluster-api/util"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	vspherepolv1alpha1 "github.com/vmware-tanzu/vm-operator/external/vsphere-policy/api/v1alpha1"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha6"
	"github.com/vmware-tanzu/vm-operator/test/e2e/framework"
	"github.com/vmware-tanzu/vm-operator/test/e2e/infrastructure/vsphere/testbed"
	"github.com/vmware-tanzu/vm-operator/test/e2e/infrastructure/vsphere/vcenter"
	"github.com/vmware-tanzu/vm-operator/test/e2e/infrastructure/vsphere/wcp"
	"github.com/vmware-tanzu/vm-operator/test/e2e/manifestbuilders"
	"github.com/vmware-tanzu/vm-operator/test/e2e/utils"
	"github.com/vmware-tanzu/vm-operator/test/e2e/vmservice/common"
	e2eConfig "github.com/vmware-tanzu/vm-operator/test/e2e/vmservice/config"
	"github.com/vmware-tanzu/vm-operator/test/e2e/vmservice/consts"
	"github.com/vmware-tanzu/vm-operator/test/e2e/vmservice/lib/vmoperator"
	"github.com/vmware-tanzu/vm-operator/test/e2e/vmservice/skipper"
	"github.com/vmware-tanzu/vm-operator/test/e2e/vmservice/vmservice"
	"github.com/vmware-tanzu/vm-operator/test/e2e/wcpframework"
)

// affinityTagResourceName returns the derived name of the Tag resource for a
// namespace and label key/value pair: "tag-" followed by the 16 hex character
// XXHash64 digest of "<namespace>:<key>:<value>". Byte-for-byte what
// pkg/providers/vsphere/virtualmachine.TagResourceName computes; reimplemented
// here rather than imported so this e2e module does not pick up that
// package's production dependency graph.
func affinityTagResourceName(namespace, key, value string) string {
	return fmt.Sprintf("tag-%016x", xxhash.Sum64String(namespace+":"+key+":"+value))
}

// vCenterTagName returns the vCenter tag name for a label key/value pair:
// "<key>:<value>". Byte-for-byte what
// pkg/providers/vsphere/virtualmachine.TagName computes; reimplemented
// here rather than imported so this e2e module does not pick up that
// package's production dependency graph for two one-line derivations.
func vCenterTagName(key, value string) string {
	return key + ":" + value
}

// VMAffinityTagSpecInput is the input to VMAffinityTagSpec.
type VMAffinityTagSpecInput struct {
	ClusterProxy     wcpframework.WCPClusterProxyInterface
	Config           *e2eConfig.E2EConfig
	WCPClient        wcp.WorkloadManagementAPI
	ArtifactFolder   string
	WCPNamespaceName string
}

// VMAffinityTagSpec exercises namespace-scoped Tag ownership, vCenter tag
// carriage, and convergence for owners and label-only VMs.
//
// The suite gates on VMAffinityDuringExecutionCapabilityName because it
// references labels via a required term whose topologyKey is
// kubernetes.io/hostname, which the admission webhook only accepts once that
// capability is enabled (without it, only topology.kubernetes.io/zone is
// accepted).
func VMAffinityTagSpec(ctx context.Context, inputGetter func() VMAffinityTagSpecInput) {
	const (
		specName    = "vm-affinity-tag"
		vmKind      = "VirtualMachine"
		vmgKind     = "VirtualMachineGroup"
		affinityKey = "tier"
	)

	var (
		input           VMAffinityTagSpecInput
		config          *e2eConfig.E2EConfig
		clusterProxy    *common.VMServiceClusterProxy
		svClusterClient ctrlclient.Client
		wcpClient       wcp.WorkloadManagementAPI
		vCenterClient   *vim25.Client
		tagMgr          *tags.Manager

		linuxImageDisplayName string
		linuxVMIName          string

		vmNames []string

		vmgRootName    string
		vmgRootYaml    []byte
		vmgMemberNames []string
	)

	// affinityFor returns a spec.affinity referencing key:value via a
	// required VM-affinity term on the host topology.
	affinityFor := func(key, value string) *vmopv1.AffinitySpec {
		return &vmopv1.AffinitySpec{
			VMAffinity: &vmopv1.VMAffinitySpec{
				RequiredDuringSchedulingPreferredDuringExecution: []vmopv1.VMAffinityTerm{
					{
						LabelSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{key: value},
						},
						TopologyKey: corev1.LabelHostname,
					},
				},
			},
		}
	}

	// antiAffinityFor references a label pair through required zone anti-affinity.
	antiAffinityFor := func(key, value string) *vmopv1.AffinitySpec {
		return &vmopv1.AffinitySpec{
			VMAntiAffinity: &vmopv1.VMAntiAffinitySpec{
				RequiredDuringSchedulingPreferredDuringExecution: []vmopv1.VMAffinityTerm{
					{
						LabelSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{key: value},
						},
						TopologyKey: corev1.LabelTopologyZone,
					},
				},
			},
		}
	}

	// ensureVMInGroup adds vmName as a member of the suite's root
	// VirtualMachineGroup, creating the group on the first call and
	// applying the updated member list on every subsequent call.
	// spec.affinity requires a non-empty spec.groupName (webhook rule
	// validateVMAffinity, independent of this feature), so every VM this
	// suite creates must belong to a group.
	ensureVMInGroup := func(vmName string) {
		GinkgoHelper()

		vmgMemberNames = append(vmgMemberNames, vmName)

		vmgParameters := manifestbuilders.VirtualMachineGroupYaml{
			Namespace: input.WCPNamespaceName,
			Name:      vmgRootName,
		}
		for _, m := range vmgMemberNames {
			vmgParameters.Members = append(vmgParameters.Members, vmopv1.GroupMember{Kind: vmKind, Name: m})
		}
		vmgRootYaml = manifestbuilders.GetVirtualMachineGroupYaml(vmgParameters)

		if len(vmgMemberNames) == 1 {
			Expect(clusterProxy.CreateWithArgs(ctx, vmgRootYaml)).To(Succeed(), "failed to create VirtualMachineGroup %q", vmgRootName)
		} else {
			Expect(clusterProxy.ApplyWithArgs(ctx, vmgRootYaml)).To(Succeed(), "failed to update VirtualMachineGroup %q", vmgRootName)
		}
	}

	// vmYAML renders a powered-on VM carrying labels and, when affinity is
	// non-nil, referencing it via spec.affinity, as a member of groupName.
	vmYAML := func(vmName, groupName string, labels map[string]string, affinity *vmopv1.AffinitySpec) []byte {
		clusterResources := config.InfraConfig.ManagementClusterConfig.Resources

		return manifestbuilders.GetVirtualMachineYamlA5(manifestbuilders.VirtualMachineYaml{
			Namespace:        input.WCPNamespaceName,
			Name:             vmName,
			GroupName:        groupName,
			Labels:           labels,
			Affinity:         affinity,
			ImageName:        linuxVMIName,
			VMClassName:      clusterResources.VMClassName,
			StorageClassName: clusterResources.StorageClassName,
			PowerState:       "PoweredOn",
		})
	}

	// waitForVMReady waits for the VM to exist, be created, and be powered on.
	waitForVMReady := func(vmName string) {
		GinkgoHelper()

		vmoperator.WaitForVirtualMachineToExist(ctx, config, svClusterClient, input.WCPNamespaceName, vmName)
		vmoperator.WaitForVirtualMachineConditionCreated(ctx, config, svClusterClient, input.WCPNamespaceName, vmName)
		vmoperator.WaitForVirtualMachinePowerState(ctx, config, svClusterClient, input.WCPNamespaceName, vmName, "PoweredOn")
	}

	// createVM creates a powered-on VM in the suite's root VirtualMachineGroup.
	createVM := func(vmName string, labels map[string]string, affinity *vmopv1.AffinitySpec) {
		GinkgoHelper()

		ensureVMInGroup(vmName)

		yaml := vmYAML(vmName, vmgRootName, labels, affinity)
		e2eframework.Logf("VM YAML:\n%s", string(yaml))
		Expect(clusterProxy.CreateWithArgs(ctx, yaml)).To(Succeed(), "failed to create VM %q:\n %s", vmName, string(yaml))

		vmNames = append(vmNames, vmName)

		waitForVMReady(vmName)
	}

	// deleteVM deletes a VM and waits for it to be gone, removing it from
	// vmNames so AfterEach does not try again.
	deleteVM := func(vmName string) {
		GinkgoHelper()

		vm, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, vmName)
		Expect(err).ToNot(HaveOccurred(), "failed to get VM %q before deleting it", vmName)
		Expect(svClusterClient.Delete(ctx, vm)).To(Succeed(), "failed to delete VM %q", vmName)

		vmoperator.WaitForVirtualMachineToBeDeleted(ctx, config, svClusterClient, input.WCPNamespaceName, vmName)

		for i, n := range vmNames {
			if n == vmName {
				vmNames = append(vmNames[:i], vmNames[i+1:]...)
				break
			}
		}
	}

	// deferDeleteVM registers cleanup for a VM that a spec creates outside
	// the suite's root group.
	deferDeleteVM := func(vmName string) {
		DeferCleanup(func() {
			vm, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, vmName)
			if err == nil {
				Expect(svClusterClient.Delete(ctx, vm)).To(Succeed(), "failed to delete VM %q", vmName)
				vmoperator.WaitForVirtualMachineToBeDeleted(ctx, config, svClusterClient, input.WCPNamespaceName, vmName)
			}
		})
	}

	// createGroup creates a VirtualMachineGroup with the given members and
	// registers its cleanup.
	createGroup := func(groupName string, members ...string) {
		GinkgoHelper()

		groupParameters := manifestbuilders.VirtualMachineGroupYaml{
			Namespace: input.WCPNamespaceName,
			Name:      groupName,
		}
		for _, m := range members {
			groupParameters.Members = append(groupParameters.Members, vmopv1.GroupMember{Kind: vmKind, Name: m})
		}
		groupYaml := manifestbuilders.GetVirtualMachineGroupYaml(groupParameters)

		Expect(clusterProxy.CreateWithArgs(ctx, groupYaml)).To(Succeed(), "failed to create VirtualMachineGroup %q", groupName)
		DeferCleanup(func() {
			Expect(clusterProxy.DeleteWithArgs(ctx, groupYaml)).To(Succeed(), "failed to delete VirtualMachineGroup %q", groupName)
			vmoperator.WaitForVirtualMachineGroupToBeDeleted(ctx, config, svClusterClient, input.WCPNamespaceName, groupName)
		})
	}

	// getTag returns the Tag resource with the given derived name, or nil
	// if it does not exist (or is not yet observable).
	getTag := func(name string) *vspherepolv1alpha1.Tag {
		tag := &vspherepolv1alpha1.Tag{}
		if err := svClusterClient.Get(
			ctx,
			ctrlclient.ObjectKey{Namespace: input.WCPNamespaceName, Name: name},
			tag); err != nil {

			return nil
		}
		return tag
	}

	// waitForNoTag asserts that the Tag CR derived from key/value never
	// appears within the wait interval, so a regression that creates one
	// anyway is caught rather than racing to a false pass.
	waitForNoTag := func(key, value string) {
		GinkgoHelper()

		name := affinityTagResourceName(input.WCPNamespaceName, key, value)
		Consistently(func() bool {
			return getTag(name) == nil
		}, config.GetIntervals("default", "wait-vm-affinity-tag-applied")...).Should(
			BeTrue(), "expected no Tag %q to be created in namespace %q", name, input.WCPNamespaceName)
	}

	// waitForTagOwnerCount waits for the Tag CR derived from key/value to
	// have exactly count owner references.
	waitForTagOwnerCount := func(key, value string, count int) *vspherepolv1alpha1.Tag {
		GinkgoHelper()

		name := affinityTagResourceName(input.WCPNamespaceName, key, value)
		var tag *vspherepolv1alpha1.Tag

		Eventually(func(g Gomega) {
			tag = getTag(name)
			g.Expect(tag).ToNot(BeNil(), "Tag %q should exist", name)
			g.Expect(tag.OwnerReferences).To(HaveLen(count))
		}, config.GetIntervals("default", "wait-vm-affinity-tag-applied")...).Should(
			Succeed(), "timed out waiting for Tag %q to have %d owner(s)", name, count)

		return tag
	}

	// waitForTagDeleted waits for the Tag CR derived from key/value to be
	// removed from the namespace.
	waitForTagDeleted := func(key, value string) {
		GinkgoHelper()

		name := affinityTagResourceName(input.WCPNamespaceName, key, value)
		Eventually(func() bool {
			return getTag(name) == nil
		}, config.GetIntervals("default", "wait-tag-cr-deleted")...).Should(
			BeTrue(), "timed out waiting for Tag %q to be deleted from namespace %q", name, input.WCPNamespaceName)
	}

	// ownerUIDs returns the UIDs of the Tag's owner references.
	ownerUIDs := func(tag *vspherepolv1alpha1.Tag) []string {
		uids := make([]string, len(tag.OwnerReferences))
		for i, ref := range tag.OwnerReferences {
			uids[i] = string(ref.UID)
		}
		return uids
	}

	// hasAttachedVCenterTag reports whether the vCenter VM with the given
	// MoID has a tag named tagName in category categoryName attached,
	// resolving each attached tag's category by ID since GetAttachedTags
	// only returns the category ID.
	hasAttachedVCenterTag := func(vmMoID, tagName, categoryName string) (bool, error) {
		vmMoRef := vimtypes.ManagedObjectReference{Type: "VirtualMachine", Value: vmMoID}

		attached, err := tagMgr.GetAttachedTags(ctx, vmMoRef)
		if err != nil {
			return false, fmt.Errorf("failed to get attached tags for VM %q: %w", vmMoID, err)
		}

		for _, t := range attached {
			if t.Name != tagName {
				continue
			}

			category, err := tagMgr.GetCategory(ctx, t.CategoryID)
			if err != nil {
				return false, fmt.Errorf("failed to get category %q: %w", t.CategoryID, err)
			}

			if category.Name == categoryName {
				return true, nil
			}
		}

		return false, nil
	}

	// waitForVCenterTag waits for the vCenter tag "key:value" in category
	// input.WCPNamespaceName to be attached to the given VM.
	waitForVCenterTag := func(vmName, key, value string) {
		GinkgoHelper()

		vmMoID := vmoperator.GetVirtualMachineMOID(ctx, svClusterClient, input.WCPNamespaceName, vmName)
		tagName := vCenterTagName(key, value)

		Eventually(func(g Gomega) {
			has, err := hasAttachedVCenterTag(vmMoID, tagName, input.WCPNamespaceName)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(has).To(BeTrue())
		}, config.GetIntervals("default", "wait-vm-affinity-tag-applied")...).Should(
			Succeed(), "timed out waiting for vCenter tag %q to be attached to VM %q", tagName, vmName)
	}

	// consistentlyNoVCenterTag asserts the vCenter tag "key:value" in
	// category input.WCPNamespaceName is never attached to the given VM
	// for the duration of the wait interval.
	consistentlyNoVCenterTag := func(vmName, key, value string) {
		GinkgoHelper()

		vmMoID := vmoperator.GetVirtualMachineMOID(ctx, svClusterClient, input.WCPNamespaceName, vmName)
		tagName := vCenterTagName(key, value)

		Consistently(func(g Gomega) {
			has, err := hasAttachedVCenterTag(vmMoID, tagName, input.WCPNamespaceName)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(has).To(BeFalse())
		}, config.GetIntervals("default", "wait-vm-affinity-tag-applied")...).Should(
			Succeed(), "expected vCenter tag %q to never be attached to VM %q", tagName, vmName)
	}

	// waitForVCenterTagRemoved waits for the vCenter tag "key:value" in
	// category input.WCPNamespaceName to be detached from the given VM —
	// the converse of waitForVCenterTag, used when a fan-out reconcile is
	// expected to remove a tag that was previously attached.
	waitForVCenterTagRemoved := func(vmName, key, value string) {
		GinkgoHelper()

		vmMoID := vmoperator.GetVirtualMachineMOID(ctx, svClusterClient, input.WCPNamespaceName, vmName)
		tagName := vCenterTagName(key, value)

		Eventually(func(g Gomega) {
			has, err := hasAttachedVCenterTag(vmMoID, tagName, input.WCPNamespaceName)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(has).To(BeFalse())
		}, config.GetIntervals("default", "wait-vm-affinity-tag-applied")...).Should(
			Succeed(), "timed out waiting for vCenter tag %q to be removed from VM %q", tagName, vmName)
	}

	// consistentlyHasVCenterTag asserts the vCenter tag "key:value" in
	// category input.WCPNamespaceName stays attached to the given VM for the
	// duration of the wait interval — the converse of
	// consistentlyNoVCenterTag, used to prove a regression does not untag a
	// VM that should keep carrying the tag.
	consistentlyHasVCenterTag := func(vmName, key, value string) {
		GinkgoHelper()

		vmMoID := vmoperator.GetVirtualMachineMOID(ctx, svClusterClient, input.WCPNamespaceName, vmName)
		tagName := vCenterTagName(key, value)

		Consistently(func(g Gomega) {
			has, err := hasAttachedVCenterTag(vmMoID, tagName, input.WCPNamespaceName)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(has).To(BeTrue())
		}, config.GetIntervals("default", "wait-vm-affinity-tag-applied")...).Should(
			Succeed(), "expected vCenter tag %q to remain attached to VM %q", tagName, vmName)
	}

	// patchVM mutates an existing VM's spec via Get/mutate/Update, retrying
	// on conflict.
	patchVM := func(vmName string, mutate func(vm *vmopv1.VirtualMachine)) {
		GinkgoHelper()

		key := ctrlclient.ObjectKey{Namespace: input.WCPNamespaceName, Name: vmName}
		Eventually(func() error {
			vm := &vmopv1.VirtualMachine{}
			if err := svClusterClient.Get(ctx, key, vm); err != nil {
				return err
			}

			mutate(vm)

			return svClusterClient.Update(ctx, vm)
		}, config.GetIntervals("default", "wait-vm-affinity-tag-applied")...).Should(
			Succeed(), "failed to patch VM %q", vmName)
	}

	BeforeEach(func() {
		input = inputGetter()
		Expect(input.Config).ToNot(BeNil(), "Invalid argument. input.Config can't be nil when calling %s spec", specName)
		Expect(input.Config.InfraConfig).ToNot(BeNil(), "Invalid argument. input.Config.InfraConfig can't be nil when calling %s spec", specName)
		skipper.SkipUnlessInfraIs(input.Config.InfraConfig.InfraName, consts.WCP)

		Expect(input.ClusterProxy).ToNot(BeNil(), "Invalid argument. input.ClusterProxy can't be nil when calling %s spec", specName)
		Expect(input.WCPNamespaceName).ToNot(BeEmpty(), "Invalid argument. input.WCPNamespaceName can't be empty when calling %s spec", specName)
		Expect(os.MkdirAll(input.ArtifactFolder, 0755)).To(Succeed(), "Invalid argument. input.ArtifactFolder can't be created for %s spec", specName)

		config = input.Config
		clusterProxy = input.ClusterProxy.(*common.VMServiceClusterProxy)
		wcpClient = input.WCPClient
		cancelPodWatches := framework.WatchPodLogsAndEventsInNamespaces(ctx, []string{config.GetVariable("VMOPNamespace")}, clusterProxy.GetRESTConfig(), filepath.Join(input.ArtifactFolder, specName))
		DeferCleanup(cancelPodWatches)

		svClusterClient = clusterProxy.GetClient()

		skipper.SkipUnlessSupervisorCapabilityEnabled(ctx, clusterProxy, consts.VMAffinityDuringExecutionCapabilityName)

		// spec.affinity requires a non-empty spec.groupName, and
		// spec.groupName itself is rejected unless VM Groups is enabled.
		skipper.SkipUnlessSupervisorCapabilityEnabled(ctx, clusterProxy, consts.VMGroupsCapabilityName)

		skipper.SkipUnlessTaggingAPIEnabled(ctx, clusterProxy)

		// Skip entirely when the Tag CRD is not registered on this
		// Supervisor at all. Capability activation alone does not prove
		// that the operator has installed the CRD yet.
		if err := svClusterClient.List(ctx, &vspherepolv1alpha1.TagList{}, ctrlclient.InNamespace(input.WCPNamespaceName)); err != nil {
			if apimeta.IsNoMatchError(err) {
				framework.SkipInternalf(1, "skip the test as the Tag CRD is not registered on this Supervisor")
			}
			Expect(err).ToNot(HaveOccurred(), "failed to list Tags")
		}

		vCenterClient = vcenter.NewVimClientFromKubeconfig(ctx, clusterProxy.GetKubeconfigPath())
		restClient, err := vcenter.NewRestClient(ctx, vCenterClient, testbed.AdminUsername, testbed.AdminPassword)
		Expect(err).ToNot(HaveOccurred(), "failed to create REST client")
		tagMgr = tags.NewManager(restClient)

		clusterResources := config.InfraConfig.ManagementClusterConfig.Resources
		linuxImageDisplayName = vmservice.GetDefaultImageDisplayName(clusterResources)
		linuxVMIName = vmoperator.WaitForVirtualMachineImageName(ctx, &config.Config, svClusterClient, input.WCPNamespaceName, linuxImageDisplayName)

		vmNames = nil

		vmgRootName = fmt.Sprintf("%s-%s-root", specName, capiutil.RandomString(4))
		vmgRootYaml = nil
		vmgMemberNames = nil
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			for _, vmName := range vmNames {
				vmoperator.DescribeResourceIfExists(ctx, svClusterClient, clusterProxy.GetKubeconfigPath(), input.WCPNamespaceName, vmName, vmKind)
			}
			if len(vmgRootYaml) > 0 {
				vmoperator.DescribeResourceIfExists(ctx, svClusterClient, clusterProxy.GetKubeconfigPath(), input.WCPNamespaceName, vmgRootName, vmgKind)
			}
		}

		for _, vmName := range vmNames {
			By(fmt.Sprintf("Deleting VM %s", vmName))
			vm, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, vmName)
			if err == nil {
				Expect(svClusterClient.Delete(ctx, vm)).To(Succeed(), "failed to delete VM %q", vmName)
				vmoperator.WaitForVirtualMachineToBeDeleted(ctx, config, svClusterClient, input.WCPNamespaceName, vmName)
			}
		}
		vmNames = nil

		if len(vmgRootYaml) > 0 {
			By(fmt.Sprintf("Deleting the root VirtualMachineGroup %s", vmgRootName))
			Expect(clusterProxy.DeleteWithArgs(ctx, vmgRootYaml)).To(Succeed(), "failed to delete VirtualMachineGroup %q", vmgRootName)
			vmoperator.WaitForVirtualMachineGroupToBeDeleted(ctx, config, svClusterClient, input.WCPNamespaceName, vmgRootName)
		}
		if vCenterClient != nil {
			vcenter.LogoutVimClient(vCenterClient)
		}
	})

	Context("A VM owns the Tag for each label pair its affinity references", func() {
		It("creates a Tag CR with the VM as sole owner when its affinity references its own label",
			Label("core-functional", "extended-functional", "experimental"), func() {
				vmName := fmt.Sprintf("%s-%s", specName, capiutil.RandomString(6))

				By(fmt.Sprintf("Creating VM %s with label %s=blue and affinity referencing %s=blue", vmName, affinityKey, affinityKey))
				createVM(vmName, map[string]string{affinityKey: "blue"}, affinityFor(affinityKey, "blue"))

				By("Waiting for the Tag CR to be created with the derived name, label mirror, and this VM as sole owner")
				vm, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, vmName)
				Expect(err).ToNot(HaveOccurred())

				tag := waitForTagOwnerCount(affinityKey, "blue", 1)
				Expect(tag.Spec.Key).To(Equal(affinityKey))
				Expect(tag.Spec.Value).To(Equal("blue"))
				Expect(tag.Labels).To(HaveKeyWithValue(affinityKey, "blue"))
				Expect(tag.OwnerReferences[0].UID).To(Equal(vm.UID))
				Expect(tag.OwnerReferences[0].Name).To(Equal(vmName))
				waitForVCenterTag(vmName, affinityKey, "blue")
			})

		It("appends a second VM as an owner of the same Tag CR rather than replacing the first",
			Label("core-functional", "extended-functional", "experimental"), func() {
				vm1Name := fmt.Sprintf("%s-%s-1", specName, capiutil.RandomString(6))
				vm2Name := fmt.Sprintf("%s-%s-2", specName, capiutil.RandomString(6))

				By(fmt.Sprintf("Creating VM1 (%s) with label %s=blue and affinity referencing %s=blue", vm1Name, affinityKey, affinityKey))
				createVM(vm1Name, map[string]string{affinityKey: "blue"}, affinityFor(affinityKey, "blue"))
				waitForTagOwnerCount(affinityKey, "blue", 1)

				By(fmt.Sprintf("Creating VM2 (%s) with the same label and affinity", vm2Name))
				createVM(vm2Name, map[string]string{affinityKey: "blue"}, affinityFor(affinityKey, "blue"))

				By("Verifying exactly one Tag CR exists with both VMs recorded as owners")
				vm1, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, vm1Name)
				Expect(err).ToNot(HaveOccurred())
				vm2, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, vm2Name)
				Expect(err).ToNot(HaveOccurred())

				tag := waitForTagOwnerCount(affinityKey, "blue", 2)
				Expect(ownerUIDs(tag)).To(ConsistOf(string(vm1.UID), string(vm2.UID)))
				waitForVCenterTag(vm1Name, affinityKey, "blue")
				waitForVCenterTag(vm2Name, affinityKey, "blue")
			})

		It("creates a Tag CR only for the label pair referenced by affinity, not the unreferenced one",
			Label("core-functional", "extended-functional", "experimental"), func() {
				vmName := fmt.Sprintf("%s-%s", specName, capiutil.RandomString(6))

				By(fmt.Sprintf("Creating VM %s with labels %s=blue and env=prod, affinity referencing only %s=blue", vmName, affinityKey, affinityKey))
				createVM(vmName,
					map[string]string{affinityKey: "blue", "env": "prod"},
					affinityFor(affinityKey, "blue"))

				By("Verifying a Tag CR exists for the referenced pair")
				waitForTagOwnerCount(affinityKey, "blue", 1)

				By("Verifying no Tag CR exists for the unreferenced pair")
				waitForNoTag("env", "prod")
				waitForVCenterTag(vmName, affinityKey, "blue")
				consistentlyNoVCenterTag(vmName, "env", "prod")
			})
		// The grouped zone policy references the target label without requiring
		// the declaring VM to carry it. VMPlacementPolicies enables zone-rule
		// emission; the topology must support zone anti-affinity.
		It("creates a Tag CR with a VM as sole owner when its affinity references a label it does not itself carry",
			Label("core-functional", "extended-functional", "experimental"), func() {
				skipper.SkipUnlessSupervisorCapabilityEnabled(ctx, clusterProxy, consts.VMPlacementPoliciesCapabilityName)

				const (
					ownValue    = "us1-4-own"
					targetValue = "us1-4-target"
				)

				vmName := fmt.Sprintf("%s-%s", specName, capiutil.RandomString(6))

				By(fmt.Sprintf("Creating VM %s with label %s=%s and an anti-affinity term referencing %s=%s, which it does not carry", vmName, affinityKey, ownValue, affinityKey, targetValue))
				createVM(vmName, map[string]string{affinityKey: ownValue}, antiAffinityFor(affinityKey, targetValue))

				By("Verifying a Tag CR for the referenced pair is created with this VM as sole owner")
				vm, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, vmName)
				Expect(err).ToNot(HaveOccurred())

				tag := waitForTagOwnerCount(affinityKey, targetValue, 1)
				Expect(tag.OwnerReferences[0].UID).To(Equal(vm.UID))

				By("Verifying the referencing VM is not itself given the vCenter tag, since it does not carry that label")
				consistentlyNoVCenterTag(vmName, affinityKey, targetValue)

				By("Verifying a VM that does carry the referenced label is tagged once the Tag CR exists")
				carryingVMName := fmt.Sprintf("%s-%s-carrying", specName, capiutil.RandomString(6))
				createVM(carryingVMName, map[string]string{affinityKey: targetValue}, nil)
				waitForVCenterTag(carryingVMName, affinityKey, targetValue)

				By("Verifying the carrying VM was not added as an owner of the Tag CR")
				tagAfter := waitForTagOwnerCount(affinityKey, targetValue, 1)
				Expect(tagAfter.OwnerReferences[0].UID).To(Equal(vm.UID))
			})
	})

	Context("Tag ownership follows the VM's lifetime", func() {
		It("keeps the Tag CR with the surviving owner when one of two owning VMs is deleted",
			Label("core-functional", "extended-functional", "experimental"), func() {
				vm1Name := fmt.Sprintf("%s-%s-1", specName, capiutil.RandomString(6))
				vm2Name := fmt.Sprintf("%s-%s-2", specName, capiutil.RandomString(6))

				By(fmt.Sprintf("Creating VM1 (%s) and VM2 (%s), both with label %s=blue and affinity referencing it", vm1Name, vm2Name, affinityKey))
				createVM(vm1Name, map[string]string{affinityKey: "blue"}, affinityFor(affinityKey, "blue"))
				createVM(vm2Name, map[string]string{affinityKey: "blue"}, affinityFor(affinityKey, "blue"))
				waitForTagOwnerCount(affinityKey, "blue", 2)

				waitForVCenterTag(vm1Name, affinityKey, "blue")
				waitForVCenterTag(vm2Name, affinityKey, "blue")

				By(fmt.Sprintf("Deleting VM1 (%s)", vm1Name))
				deleteVM(vm1Name)

				By("Verifying the Tag CR survives with exactly one owner, VM2")
				vm2, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, vm2Name)
				Expect(err).ToNot(HaveOccurred())

				tag := waitForTagOwnerCount(affinityKey, "blue", 1)
				Expect(tag.OwnerReferences[0].UID).To(Equal(vm2.UID))
				waitForVCenterTag(vm2Name, affinityKey, "blue")
			})

		It("deletes the Tag CR once its only owning VM is deleted",
			Label("core-functional", "extended-functional", "experimental"), func() {
				vmName := fmt.Sprintf("%s-%s", specName, capiutil.RandomString(6))

				By(fmt.Sprintf("Creating VM %s with label %s=blue and affinity referencing it", vmName, affinityKey))
				createVM(vmName, map[string]string{affinityKey: "blue"}, affinityFor(affinityKey, "blue"))
				waitForTagOwnerCount(affinityKey, "blue", 1)
				waitForVCenterTag(vmName, affinityKey, "blue")

				By(fmt.Sprintf("Deleting the sole owning VM %s", vmName))
				deleteVM(vmName)

				By("Verifying the Tag CR is deleted from the namespace")
				waitForTagDeleted(affinityKey, "blue")
			})

	})

	Context("Pre-existing labeled VM is tagged when a later VM references the label", func() {
		// Each It below uses a label value unique to that scenario rather
		// than reusing a value shared across another Context's Its.
		// affinityTagResourceName derives one fixed Tag name per key/value pair, so
		// two Its racing on the same pair can have a new VM's
		// ensureOwnedTag Get-and-adopt read a cached copy of the *previous*
		// It's Tag after that Tag was already deleted on the server
		// (AfterEach waits only for the VMs it created to be deleted, not
		// for the Tag their deletion frees up). The reconcile recovers on
		// its own — the ownership patch fails under its optimistic lock and
		// is retried — but the retries make this Context's own waits flaky
		// for reasons that have nothing to do with what it asserts. A
		// unique value per It makes every Tag name here unique for the life
		// of the suite, independent of spec ordering or AfterEach timing.
		It("tags a pre-existing label-only VM once a later VM references the label",
			Label("core-functional", "extended-functional", "experimental"), func() {
				const value = "us2-preexisting"

				labelOnlyVMName := fmt.Sprintf("%s-%s-label-only", specName, capiutil.RandomString(6))
				referencingVMName := fmt.Sprintf("%s-%s-ref", specName, capiutil.RandomString(6))

				By(fmt.Sprintf("Creating label-only VM %s with %s=%s and no affinity", labelOnlyVMName, affinityKey, value))
				createVM(labelOnlyVMName, map[string]string{affinityKey: value}, nil)

				By("Verifying the label-only VM is not tagged before anything references its label")
				waitForNoTag(affinityKey, value)
				consistentlyNoVCenterTag(labelOnlyVMName, affinityKey, value)

				By(fmt.Sprintf("Creating VM %s whose affinity references %s=%s", referencingVMName, affinityKey, value))
				createVM(referencingVMName, map[string]string{affinityKey: value}, affinityFor(affinityKey, value))

				By("Verifying the Tag CR records only the referencing VM as owner")
				refVM, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, referencingVMName)
				Expect(err).ToNot(HaveOccurred())

				tag := waitForTagOwnerCount(affinityKey, value, 1)
				Expect(tag.OwnerReferences[0].UID).To(Equal(refVM.UID))
				Expect(tag.OwnerReferences[0].Name).To(Equal(referencingVMName))

				By("Verifying both the referencing VM and the label-only VM carry the vCenter tag")
				waitForVCenterTag(referencingVMName, affinityKey, value)
				waitForVCenterTag(labelOnlyVMName, affinityKey, value)
			})

		It("untags the label-only VM when the referencing VM is deleted",
			Label("core-functional", "extended-functional", "experimental"), func() {
				const value = "us2-untag-on-delete"

				labelOnlyVMName := fmt.Sprintf("%s-%s-label-only", specName, capiutil.RandomString(6))
				referencingVMName := fmt.Sprintf("%s-%s-ref", specName, capiutil.RandomString(6))

				By(fmt.Sprintf("Creating label-only VM %s and referencing VM %s, both with %s=%s", labelOnlyVMName, referencingVMName, affinityKey, value))
				createVM(labelOnlyVMName, map[string]string{affinityKey: value}, nil)
				createVM(referencingVMName, map[string]string{affinityKey: value}, affinityFor(affinityKey, value))

				tag := waitForTagOwnerCount(affinityKey, value, 1)
				Expect(tag.OwnerReferences[0].Name).To(Equal(referencingVMName))
				waitForVCenterTag(referencingVMName, affinityKey, value)
				waitForVCenterTag(labelOnlyVMName, affinityKey, value)

				By(fmt.Sprintf("Deleting the referencing VM %s", referencingVMName))
				deleteVM(referencingVMName)

				By("Verifying the Tag CR is deleted, driven by the VM controller's Tag watch")
				waitForTagDeleted(affinityKey, value)

				By("Verifying the vCenter tag is removed from the label-only VM")
				waitForVCenterTagRemoved(labelOnlyVMName, affinityKey, value)
			})

		It("does not tag a same-labeled VM in a different namespace",
			Label("core-functional", "extended-functional", "experimental"), func() {
				const value = "us2-ns-isolation"

				clusterResources := config.InfraConfig.ManagementClusterConfig.Resources
				secondNamespaceName := fmt.Sprintf("%s-%s-ns2", specName, capiutil.RandomString(6))

				By(fmt.Sprintf("Creating a second namespace %s", secondNamespaceName))
				clID := vmservice.GetContentLibraryUUIDByName(consts.VMServiceCLName, wcpClient)
				vmsvcSpecs := wcp.NewVMServiceSpecDetails([]string{clusterResources.VMClassName}, []string{clID})

				secondNamespaceCtx, err := clusterProxy.CreateWCPNamespace(
					ctx, config, vmsvcSpecs,
					clusterResources.StorageClassName,
					secondNamespaceName, input.ArtifactFolder)
				Expect(err).ToNot(HaveOccurred(), "failed to create a second test WCP namespace")
				wcp.WaitForNamespaceReady(wcpClient, secondNamespaceName)
				DeferCleanup(func() {
					clusterProxy.DeleteWCPNamespace(secondNamespaceCtx)
				})

				By("Waiting for the Linux VM Image to be available in the second namespace")
				linuxVMIName2 := vmoperator.WaitForVirtualMachineImageName(ctx, &config.Config, svClusterClient, secondNamespaceName, linuxImageDisplayName)

				otherNSVMName := fmt.Sprintf("%s-%s-other-ns", specName, capiutil.RandomString(6))
				By(fmt.Sprintf("Creating label-only VM %s with %s=%s in namespace %s", otherNSVMName, affinityKey, value, secondNamespaceName))
				vmYAML := manifestbuilders.GetVirtualMachineYamlA5(manifestbuilders.VirtualMachineYaml{
					Namespace:        secondNamespaceName,
					Name:             otherNSVMName,
					Labels:           map[string]string{affinityKey: value},
					ImageName:        linuxVMIName2,
					VMClassName:      clusterResources.VMClassName,
					StorageClassName: clusterResources.StorageClassName,
					PowerState:       "PoweredOn",
				})
				Expect(clusterProxy.CreateWithArgs(ctx, vmYAML)).To(Succeed(), "failed to create VM %q:\n %s", otherNSVMName, string(vmYAML))
				DeferCleanup(func() {
					vm, err := utils.GetVirtualMachine(ctx, svClusterClient, secondNamespaceName, otherNSVMName)
					if err == nil {
						Expect(svClusterClient.Delete(ctx, vm)).To(Succeed(), "failed to delete VM %q", otherNSVMName)
						vmoperator.WaitForVirtualMachineToBeDeleted(ctx, config, svClusterClient, secondNamespaceName, otherNSVMName)
					}
				})
				vmoperator.WaitForVirtualMachineToExist(ctx, config, svClusterClient, secondNamespaceName, otherNSVMName)
				vmoperator.WaitForVirtualMachineConditionCreated(ctx, config, svClusterClient, secondNamespaceName, otherNSVMName)
				vmoperator.WaitForVirtualMachinePowerState(ctx, config, svClusterClient, secondNamespaceName, otherNSVMName, "PoweredOn")

				referencingVMName := fmt.Sprintf("%s-%s-ref", specName, capiutil.RandomString(6))
				By(fmt.Sprintf("Creating VM %s in namespace %s whose affinity references %s=%s", referencingVMName, input.WCPNamespaceName, affinityKey, value))
				createVM(referencingVMName, map[string]string{affinityKey: value}, affinityFor(affinityKey, value))

				By("Verifying the Tag CR is created in the referencing VM's namespace")
				waitForTagOwnerCount(affinityKey, value, 1)

				By(fmt.Sprintf("Verifying no Tag CR exists in namespace %s", secondNamespaceName))
				var tagList vspherepolv1alpha1.TagList
				Expect(svClusterClient.List(ctx, &tagList, ctrlclient.InNamespace(secondNamespaceName))).To(Succeed())
				Expect(tagList.Items).To(BeEmpty())

				By("Verifying the other namespace's VM never gets the vCenter tag")
				otherVMMoID := vmoperator.GetVirtualMachineMOID(ctx, svClusterClient, secondNamespaceName, otherNSVMName)
				tagName := vCenterTagName(affinityKey, value)
				Consistently(func(g Gomega) {
					has, err := hasAttachedVCenterTag(otherVMMoID, tagName, secondNamespaceName)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(has).To(BeFalse())
				}, config.GetIntervals("default", "wait-vm-affinity-tag-applied")...).Should(
					Succeed(), "expected vCenter tag %q to never be attached to VM %q in namespace %q", tagName, otherNSVMName, secondNamespaceName)
			})

		It("tags a label-only VM created after the Tag CR already exists",
			Label("core-functional", "extended-functional", "experimental"), func() {
				const value = "us2-late-arrival"

				referencingVMName := fmt.Sprintf("%s-%s-ref", specName, capiutil.RandomString(6))
				labelOnlyVMName := fmt.Sprintf("%s-%s-label-only", specName, capiutil.RandomString(6))

				By(fmt.Sprintf("Creating VM %s whose affinity references %s=%s, creating the Tag CR", referencingVMName, affinityKey, value))
				createVM(referencingVMName, map[string]string{affinityKey: value}, affinityFor(affinityKey, value))
				tagBefore := waitForTagOwnerCount(affinityKey, value, 1)
				waitForVCenterTag(referencingVMName, affinityKey, value)

				By(fmt.Sprintf("Creating label-only VM %s after the Tag CR already exists", labelOnlyVMName))
				createVM(labelOnlyVMName, map[string]string{affinityKey: value}, nil)

				By("Verifying the new VM picks up the vCenter tag on its first reconcile")
				waitForVCenterTag(labelOnlyVMName, affinityKey, value)

				By("Verifying the new VM was not added as an owner of the Tag CR")
				tagAfter := waitForTagOwnerCount(affinityKey, value, 1)
				Expect(tagAfter.OwnerReferences[0].UID).To(Equal(tagBefore.OwnerReferences[0].UID))
			})
	})

	Context("Label participation changes and tagging converges", func() {
		// Each It below uses label values unique to that scenario, for the
		// same reason the label-only Context does (see its doc comment): a Tag
		// deleted by one It could still be cached after deletion when another It's VM
		// tries to adopt the same derived name.
		//
		// spec.affinity is immutable on update (webhook rule
		// validateImmutableVMAffinity), so the only way a live VM's
		// participation in a label pair changes is by changing its labels.
		// Ownership follows the affinity reference alone, not carriage, so
		// dropping the label never drops ownership — the VM's own
		// spec.affinity still references the pair, and that can't change.
		// Only the VM's own vCenter tag carriage reacts to the label change.

		It("keeps the Tag CR — ownership survives a label drop — and only removes the vCenter tag from the VM that dropped the label",
			Label("core-functional", "extended-functional", "experimental"), func() {
				const value = "us3-sole-owner-cleanup"

				vmName := fmt.Sprintf("%s-%s", specName, capiutil.RandomString(6))

				By(fmt.Sprintf("Creating VM %s with label %s=%s and affinity referencing it", vmName, affinityKey, value))
				createVM(vmName, map[string]string{affinityKey: value}, affinityFor(affinityKey, value))
				tagBefore := waitForTagOwnerCount(affinityKey, value, 1)
				waitForVCenterTag(vmName, affinityKey, value)

				By(fmt.Sprintf("Patching VM %s to drop the label while keeping the (immutable) affinity reference", vmName))
				patchVM(vmName, func(vm *vmopv1.VirtualMachine) {
					delete(vm.Labels, affinityKey)
				})

				By("Verifying the Tag CR survives with the VM still recorded as owner, since its spec.affinity still references the pair")
				vm, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, vmName)
				Expect(err).ToNot(HaveOccurred())

				tagAfter := waitForTagOwnerCount(affinityKey, value, 1)
				Expect(tagAfter.UID).To(Equal(tagBefore.UID))
				Expect(tagAfter.OwnerReferences[0].UID).To(Equal(vm.UID))

				By("Verifying the vCenter tag is removed from the VM, since it no longer carries the label")
				waitForVCenterTagRemoved(vmName, affinityKey, value)
			})

		DescribeTable("converges label gains and swaps without changing affinity or VM identity",
			Label("core-functional", "extended-functional", "experimental"),
			func(dropFirst bool) {
				const secondKey = "role"
				value := "us3-" + capiutil.RandomString(6)
				vmName := fmt.Sprintf("%s-%s", specName, capiutil.RandomString(6))
				peerName := vmName + "-peer"
				affinity := affinityFor(affinityKey, value)
				affinity.VMAffinity.RequiredDuringSchedulingPreferredDuringExecution = append(
					affinity.VMAffinity.RequiredDuringSchedulingPreferredDuringExecution,
					affinityFor(secondKey, value).VMAffinity.RequiredDuringSchedulingPreferredDuringExecution...)

				By("Establishing both vCenter tags through immutable affinity at creation")
				createVM(vmName, map[string]string{affinityKey: value, secondKey: value}, affinity)
				createVM(peerName, map[string]string{affinityKey: value}, affinityFor(affinityKey, value))
				firstTag := waitForTagOwnerCount(affinityKey, value, 2)
				secondTag := waitForTagOwnerCount(secondKey, value, 1)
				waitForVCenterTag(vmName, affinityKey, value)
				waitForVCenterTag(vmName, secondKey, value)
				waitForVCenterTag(peerName, affinityKey, value)
				vm, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, vmName)
				Expect(err).ToNot(HaveOccurred())
				peer, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, peerName)
				Expect(err).ToNot(HaveOccurred())

				By("Removing the second label while retaining both affinity references")
				patchVM(vmName, func(vm *vmopv1.VirtualMachine) {
					delete(vm.Labels, secondKey)
				})
				waitForVCenterTagRemoved(vmName, secondKey, value)
				consistentlyNoVCenterTag(vmName, secondKey, value)

				By("Gaining the second label, optionally dropping the first in the same update")
				patchVM(vmName, func(vm *vmopv1.VirtualMachine) {
					vm.Labels[secondKey] = value
					if dropFirst {
						delete(vm.Labels, affinityKey)
					}
				})
				waitForVCenterTag(vmName, secondKey, value)
				if dropFirst {
					waitForVCenterTagRemoved(vmName, affinityKey, value)
				} else {
					waitForVCenterTag(vmName, affinityKey, value)
				}

				By("Verifying stable ownership, tag carriage, and VM identity")
				Consistently(func(g Gomega) {
					current, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, vmName)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(current.UID).To(Equal(vm.UID))
					g.Expect(current.Status.UniqueID).To(Equal(vm.Status.UniqueID))
					g.Expect(current.Status.PowerState).To(Equal(vmopv1.VirtualMachinePowerStateOn))
					g.Expect(current.Spec.Affinity).To(Equal(vm.Spec.Affinity))
					first := getTag(firstTag.Name)
					second := getTag(secondTag.Name)
					g.Expect(first).ToNot(BeNil())
					g.Expect(second).ToNot(BeNil())
					g.Expect(first.UID).To(Equal(firstTag.UID))
					g.Expect(second.UID).To(Equal(secondTag.UID))
					g.Expect(ownerUIDs(first)).To(ConsistOf(string(vm.UID), string(peer.UID)))
					g.Expect(ownerUIDs(second)).To(ConsistOf(string(vm.UID)))
					for _, check := range []struct {
						moid     string
						key      string
						attached bool
					}{
						{vm.Status.UniqueID, affinityKey, !dropFirst},
						{vm.Status.UniqueID, secondKey, true},
						{peer.Status.UniqueID, affinityKey, true},
					} {
						has, err := hasAttachedVCenterTag(check.moid, vCenterTagName(check.key, value), input.WCPNamespaceName)
						g.Expect(err).ToNot(HaveOccurred())
						g.Expect(has).To(Equal(check.attached))
					}
				}, config.GetIntervals("default", "wait-vm-affinity-tag-applied")...).Should(Succeed())
			},
			Entry("gains a second referenced label while retaining the first tag (US3.4)", false),
			Entry("swaps referenced labels while a peer retains the dropped tag (US3.5)", true),
		)

		It("keeps the Tag CR for both owners and the label-only VM when one owner drops the label",
			Label("core-functional", "extended-functional", "experimental"), func() {
				const value = "us3-two-owner-partial-cleanup"

				prefix := fmt.Sprintf("%s-%s", specName, capiutil.RandomString(6))
				vm1Name := fmt.Sprintf("%s-1", prefix)
				vm2Name := fmt.Sprintf("%s-2", prefix)
				labelOnlyVMName := fmt.Sprintf("%s-label-only", prefix)

				By(fmt.Sprintf("Creating owner VMs %s and %s, plus label-only VM %s, all with %s=%s", vm1Name, vm2Name, labelOnlyVMName, affinityKey, value))
				createVM(vm1Name, map[string]string{affinityKey: value}, affinityFor(affinityKey, value))
				createVM(vm2Name, map[string]string{affinityKey: value}, affinityFor(affinityKey, value))
				createVM(labelOnlyVMName, map[string]string{affinityKey: value}, nil)

				waitForTagOwnerCount(affinityKey, value, 2)
				waitForVCenterTag(vm1Name, affinityKey, value)
				waitForVCenterTag(vm2Name, affinityKey, value)
				waitForVCenterTag(labelOnlyVMName, affinityKey, value)

				By(fmt.Sprintf("Patching VM1 (%s) to drop the label while keeping the (immutable) affinity reference", vm1Name))
				patchVM(vm1Name, func(vm *vmopv1.VirtualMachine) {
					delete(vm.Labels, affinityKey)
				})

				By("Verifying the Tag CR still records both VM1 and VM2 as owners, since VM1's ownership is unaffected by dropping the label")
				vm1, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, vm1Name)
				Expect(err).ToNot(HaveOccurred())
				vm2, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, vm2Name)
				Expect(err).ToNot(HaveOccurred())

				tag := waitForTagOwnerCount(affinityKey, value, 2)
				ownerUIDs := make([]string, len(tag.OwnerReferences))
				for i, ref := range tag.OwnerReferences {
					ownerUIDs[i] = string(ref.UID)
				}
				Expect(ownerUIDs).To(ConsistOf(string(vm1.UID), string(vm2.UID)))

				By("Verifying VM1 loses the vCenter tag because it no longer carries the label")
				waitForVCenterTagRemoved(vm1Name, affinityKey, value)

				By("Verifying VM2 keeps the vCenter tag, since it still carries the label")
				consistentlyHasVCenterTag(vm2Name, affinityKey, value)

				By("Verifying the label-only VM keeps the vCenter tag while the Tag CR survives")
				consistentlyHasVCenterTag(labelOnlyVMName, affinityKey, value)
			})
	})

	Context("Edge cases", func() {
		It("leaves a VM with the same label key but a different value untagged",
			Label("core-functional", "extended-functional", "experimental"), func() {
				const (
					referencedValue = "edge-samekey-referenced"
					otherValue      = "edge-samekey-other"
				)

				referencingVMName := fmt.Sprintf("%s-%s-ref", specName, capiutil.RandomString(6))
				otherValueVMName := fmt.Sprintf("%s-%s-other-value", specName, capiutil.RandomString(6))

				By(fmt.Sprintf("Creating VM %s carrying %s=%s with no affinity", otherValueVMName, affinityKey, otherValue))
				createVM(otherValueVMName, map[string]string{affinityKey: otherValue}, nil)

				By(fmt.Sprintf("Creating VM %s whose affinity references %s=%s", referencingVMName, affinityKey, referencedValue))
				createVM(referencingVMName, map[string]string{affinityKey: referencedValue}, affinityFor(affinityKey, referencedValue))

				By("Verifying the Tag CR and vCenter tag exist only for the referenced value")
				waitForTagOwnerCount(affinityKey, referencedValue, 1)
				waitForVCenterTag(referencingVMName, affinityKey, referencedValue)

				By("Verifying no Tag CR is created for the other value, and the other VM never carries the referenced tag")
				waitForNoTag(affinityKey, otherValue)
				consistentlyNoVCenterTag(otherValueVMName, affinityKey, referencedValue)
			})

		It("shares one Tag CR across VMs in different VirtualMachineGroups",
			Label("core-functional", "extended-functional", "experimental"), func() {
				const value = "cross-group"

				vm1Name := fmt.Sprintf("%s-%s-group1", specName, capiutil.RandomString(6))
				vm2Name := fmt.Sprintf("%s-%s-group2", specName, capiutil.RandomString(6))
				secondGroupName := fmt.Sprintf("%s-%s-group2", specName, capiutil.RandomString(4))

				By(fmt.Sprintf("Creating VM1 (%s) with label %s=%s and affinity referencing it, as a member of the suite's shared VirtualMachineGroup", vm1Name, affinityKey, value))
				createVM(vm1Name, map[string]string{affinityKey: value}, affinityFor(affinityKey, value))

				By(fmt.Sprintf("Creating a second, unrelated VirtualMachineGroup and VM2 (%s) with the same label and affinity, as its sole member", vm2Name))
				createGroup(secondGroupName, vm2Name)

				vm2Yaml := vmYAML(vm2Name, secondGroupName, map[string]string{affinityKey: value}, affinityFor(affinityKey, value))
				Expect(clusterProxy.CreateWithArgs(ctx, vm2Yaml)).To(Succeed(), "failed to create VM %q:\n %s", vm2Name, string(vm2Yaml))
				deferDeleteVM(vm2Name)
				waitForVMReady(vm2Name)

				By("Verifying one Tag CR with both VMs as owners, despite belonging to different VirtualMachineGroups")
				vm1, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, vm1Name)
				Expect(err).ToNot(HaveOccurred())
				vm2, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, vm2Name)
				Expect(err).ToNot(HaveOccurred())

				tag := waitForTagOwnerCount(affinityKey, value, 2)
				Expect(ownerUIDs(tag)).To(ConsistOf(string(vm1.UID), string(vm2.UID)))
				waitForVCenterTag(vm1Name, affinityKey, value)
				waitForVCenterTag(vm2Name, affinityKey, value)
			})

		It("converges a re-created VM back onto the same Tag CR",
			Label("core-functional", "extended-functional", "experimental"), func() {
				const value = "recreate-cycle"

				recreatedVMName := fmt.Sprintf("%s-%s-recreate", specName, capiutil.RandomString(6))
				survivorVMName := fmt.Sprintf("%s-%s-survivor", specName, capiutil.RandomString(6))
				groupName := fmt.Sprintf("%s-%s-recreate-group", specName, capiutil.RandomString(4))

				labels := map[string]string{affinityKey: value}
				affinity := affinityFor(affinityKey, value)

				// This spec uses its own group instead of the suite's root
				// group, whose member list createVM appends to: re-creating
				// a VM under the same name would otherwise duplicate the
				// member entry.
				By(fmt.Sprintf("Creating a dedicated VirtualMachineGroup naming %s and %s as members", recreatedVMName, survivorVMName))
				createGroup(groupName, recreatedVMName, survivorVMName)

				// The group names both VMs as members while neither exists
				// yet, so both Creates must land before waiting on either's
				// readiness: a VM's reconcile cannot get a group placement
				// result until every declared member exists.
				By(fmt.Sprintf("Creating VM %s and VM %s, both with label %s=%s and affinity referencing it", recreatedVMName, survivorVMName, affinityKey, value))
				for _, vmName := range []string{recreatedVMName, survivorVMName} {
					Expect(clusterProxy.CreateWithArgs(ctx, vmYAML(vmName, groupName, labels, affinity))).To(Succeed(), "failed to create VM %q", vmName)
					deferDeleteVM(vmName)
				}
				waitForVMReady(recreatedVMName)
				waitForVMReady(survivorVMName)

				tagBefore := waitForTagOwnerCount(affinityKey, value, 2)

				waitForVCenterTag(recreatedVMName, affinityKey, value)
				waitForVCenterTag(survivorVMName, affinityKey, value)

				By(fmt.Sprintf("Deleting VM %s while the survivor VM %s keeps the Tag CR alive", recreatedVMName, survivorVMName))
				deleteVM(recreatedVMName)

				tagAfterDelete := waitForTagOwnerCount(affinityKey, value, 1)
				Expect(tagAfterDelete.UID).To(Equal(tagBefore.UID), "expected the Tag CR to survive the delete since the survivor VM still owns it")

				By(fmt.Sprintf("Re-creating a new VM named %s with the same label and affinity", recreatedVMName))
				Expect(clusterProxy.CreateWithArgs(ctx, vmYAML(recreatedVMName, groupName, labels, affinity))).To(Succeed(), "failed to re-create VM %q", recreatedVMName)
				waitForVMReady(recreatedVMName)

				By("Verifying the re-created VM converges back onto the same Tag CR as a second owner")
				recreatedVM, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, recreatedVMName)
				Expect(err).ToNot(HaveOccurred())
				survivorVM, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, survivorVMName)
				Expect(err).ToNot(HaveOccurred())

				tagAfterRecreate := waitForTagOwnerCount(affinityKey, value, 2)
				Expect(tagAfterRecreate.UID).To(Equal(tagBefore.UID), "expected the same Tag CR object to persist through the delete/re-create cycle")
				Expect(ownerUIDs(tagAfterRecreate)).To(ConsistOf(string(survivorVM.UID), string(recreatedVM.UID)))
				waitForVCenterTag(recreatedVMName, affinityKey, value)
			})

		It("re-creates a deleted Tag CR with the same name and a new UID for a replacement owner",
			Label("core-functional", "extended-functional", "experimental"), func() {
				value := "recreate-tag-" + capiutil.RandomString(6)
				originalName := fmt.Sprintf("%s-%s-original", specName, capiutil.RandomString(6))
				replacementName := originalName
				groupName := fmt.Sprintf("%s-%s-recreate-tag", specName, capiutil.RandomString(4))
				createGroup(groupName, originalName)
				vmYaml := vmYAML(originalName, groupName, map[string]string{affinityKey: value}, affinityFor(affinityKey, value))
				Expect(clusterProxy.CreateWithArgs(ctx, vmYaml)).To(Succeed())
				deferDeleteVM(originalName)
				waitForVMReady(originalName)
				original, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, originalName)
				Expect(err).ToNot(HaveOccurred())
				tagBefore := waitForTagOwnerCount(affinityKey, value, 1)
				Expect(ownerUIDs(tagBefore)).To(ConsistOf(string(original.UID)))
				waitForVCenterTag(originalName, affinityKey, value)

				By("Deleting the sole owner and observing actual Tag disappearance before replacement")
				deleteVM(originalName)
				Eventually(func() error {
					err := svClusterClient.Get(ctx, ctrlclient.ObjectKeyFromObject(tagBefore), &vspherepolv1alpha1.Tag{})
					if apierrors.IsNotFound(err) {
						return nil
					}
					if err != nil {
						return err
					}
					return fmt.Errorf("Tag %s still exists", tagBefore.Name)
				}, config.GetIntervals("default", "wait-tag-cr-deleted")...).Should(Succeed())

				By("Creating a replacement immediately after confirmed deletion")
				Expect(clusterProxy.CreateWithArgs(ctx, vmYaml)).To(Succeed())
				waitForVMReady(replacementName)
				replacement, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, replacementName)
				Expect(err).ToNot(HaveOccurred())
				tagAfter := waitForTagOwnerCount(affinityKey, value, 1)
				Expect(replacement.UID).ToNot(Equal(original.UID))
				Expect(tagAfter.Name).To(Equal(tagBefore.Name))
				Expect(tagAfter.UID).ToNot(Equal(tagBefore.UID))
				Expect(tagAfter.Spec.Key).To(Equal(affinityKey))
				Expect(tagAfter.Spec.Value).To(Equal(value))
				Expect(ownerUIDs(tagAfter)).To(ConsistOf(string(replacement.UID)))
				waitForVCenterTag(replacementName, affinityKey, value)
			})

		It("converges two VMs created at once on a single Tag CR",
			Label("core-functional", "extended-functional", "experimental"), func() {
				const value = "concurrent-create"

				vm1Name := fmt.Sprintf("%s-%s-concurrent1", specName, capiutil.RandomString(6))
				vm2Name := fmt.Sprintf("%s-%s-concurrent2", specName, capiutil.RandomString(6))
				groupName := fmt.Sprintf("%s-%s-concurrent-group", specName, capiutil.RandomString(4))

				labels := map[string]string{affinityKey: value}
				affinity := affinityFor(affinityKey, value)

				// createVM waits for each VM to become ready before
				// returning, which would serialize the two creates and leave
				// the second VM adopting a Tag the first had long since
				// committed. Creating both back to back lets their first
				// reconciles overlap, so both race to create the Tag for the
				// same pair and the loser has to adopt the winner's resource.
				By(fmt.Sprintf("Creating a dedicated VirtualMachineGroup naming %s and %s as members", vm1Name, vm2Name))
				createGroup(groupName, vm1Name, vm2Name)

				By(fmt.Sprintf("Creating VM %s and VM %s back to back, both with label %s=%s and affinity referencing it", vm1Name, vm2Name, affinityKey, value))
				for _, vmName := range []string{vm1Name, vm2Name} {
					Expect(clusterProxy.CreateWithArgs(ctx, vmYAML(vmName, groupName, labels, affinity))).To(Succeed(), "failed to create VM %q", vmName)
					deferDeleteVM(vmName)
				}
				waitForVMReady(vm1Name)
				waitForVMReady(vm2Name)

				By("Verifying one Tag CR carries both VMs as owners rather than one create failing or overwriting the other")
				vm1, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, vm1Name)
				Expect(err).ToNot(HaveOccurred())
				vm2, err := utils.GetVirtualMachine(ctx, svClusterClient, input.WCPNamespaceName, vm2Name)
				Expect(err).ToNot(HaveOccurred())

				tag := waitForTagOwnerCount(affinityKey, value, 2)
				Expect(tag.Spec.Key).To(Equal(affinityKey))
				Expect(tag.Spec.Value).To(Equal(value))
				Expect(ownerUIDs(tag)).To(ConsistOf(string(vm1.UID), string(vm2.UID)))
				waitForVCenterTag(vm1Name, affinityKey, value)
				waitForVCenterTag(vm2Name, affinityKey, value)
			})
	})
}
