// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package virtualmachine_test

import (
	"context"
	"sync/atomic"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlmgr "sigs.k8s.io/controller-runtime/pkg/manager"

	vmopv1 "github.com/vmware-tanzu/vm-operator/api/v1alpha6"
	"github.com/vmware-tanzu/vm-operator/controllers/virtualmachine/virtualmachine"
	vspherepolv1 "github.com/vmware-tanzu/vm-operator/external/vsphere-policy/api/v1alpha1"
	pkgcfg "github.com/vmware-tanzu/vm-operator/pkg/config"
	"github.com/vmware-tanzu/vm-operator/pkg/constants/testlabels"
	pkgctx "github.com/vmware-tanzu/vm-operator/pkg/context"
	providerfake "github.com/vmware-tanzu/vm-operator/pkg/providers/fake"
	"github.com/vmware-tanzu/vm-operator/pkg/util/kube/cource"
	"github.com/vmware-tanzu/vm-operator/pkg/util/ovfcache"
	"github.com/vmware-tanzu/vm-operator/pkg/util/ptr"
	"github.com/vmware-tanzu/vm-operator/test/builder"
)

var _ = Describe("Tag watch wiring", Label(
	testlabels.Controller,
	testlabels.EnvTest,
	testlabels.API,
), func() {
	var (
		tagSuite     *builder.TestSuite
		intgCtx      *builder.IntegrationTestContext
		fakeProvider *providerfake.VMProvider
		reconcileCnt int32
		vm           *vmopv1.VirtualMachine
		vmKey        types.NamespacedName
	)

	getReconcileCount := func() int32 {
		return atomic.LoadInt32(&reconcileCnt)
	}

	BeforeEach(func() {
		virtualmachine.SkipNameValidation = ptr.To(true)

		fakeProvider = providerfake.NewVMProvider()
		reconcileCnt = 0
		fakeProvider.CreateOrUpdateVirtualMachineFn = func(_ context.Context, _ *vmopv1.VirtualMachine) error {
			atomic.AddInt32(&reconcileCnt, 1)
			return nil
		}

		ctx := ovfcache.WithContext(
			cource.WithContext(
				pkgcfg.UpdateContext(
					pkgcfg.NewContextWithDefaultConfig(),
					func(config *pkgcfg.Config) {
						config.Features.TaggingAPI = true
						config.AsyncCreateEnabled = false
						config.AsyncSignalEnabled = false
					})))

		tagSuite = builder.NewTestSuiteForControllerWithContext(
			ctx,
			virtualmachine.AddToManager,
			func(ctx *pkgctx.ControllerManagerContext, _ ctrlmgr.Manager) error {
				ctx.VMProvider = fakeProvider
				return nil
			})
		tagSuite.BeforeSuite()

		intgCtx = tagSuite.NewIntegrationTestContext()

		vm = &vmopv1.VirtualMachine{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: intgCtx.Namespace,
				Name:      "label-only-vm",
				Labels:    map[string]string{"team": "blue"},
			},
			Spec: vmopv1.VirtualMachineSpec{
				ImageName:    "dummy-image",
				ClassName:    "dummy-class",
				StorageClass: "my-storage-class",
				PowerState:   vmopv1.VirtualMachinePowerStateOn,
			},
		}
		vmKey = types.NamespacedName{Namespace: vm.Namespace, Name: vm.Name}
	})

	AfterEach(func() {
		intgCtx.AfterEach()
		intgCtx = nil

		tagSuite.AfterSuite()
		tagSuite = nil
	})

	It("reconciles a label-only VM when a matching Tag appears, and again when it is deleted", func() {
		Expect(intgCtx.Client.Create(intgCtx, vm)).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(getReconcileCount()).To(BeNumerically(">", 0))
		}).Should(Succeed(), "waiting for the VM's initial reconcile")

		baseline := getReconcileCount()

		tag := &vspherepolv1.Tag{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "tag-team-blue",
				Namespace: vm.Namespace,
			},
			Spec: vspherepolv1.TagSpec{
				Key:   "team",
				Value: "blue",
			},
		}
		Expect(intgCtx.Client.Create(intgCtx, tag)).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(getReconcileCount()).To(BeNumerically(">", baseline))
		}).Should(Succeed(), "waiting for the VM to be reconciled after the Tag appeared")

		baseline = getReconcileCount()

		Expect(intgCtx.Client.Delete(intgCtx, tag)).To(Succeed())

		Eventually(func(g Gomega) {
			g.Expect(getReconcileCount()).To(BeNumerically(">", baseline))
		}).Should(Succeed(), "waiting for the VM to be reconciled after the Tag was deleted")

		var out vmopv1.VirtualMachine
		Expect(intgCtx.Client.Get(intgCtx, vmKey, &out)).To(Succeed())
	})
})
