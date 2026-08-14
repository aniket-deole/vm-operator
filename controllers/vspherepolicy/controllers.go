// © Broadcom. All Rights Reserved.
// The term "Broadcom" refers to Broadcom Inc. and/or its subsidiaries.
// SPDX-License-Identifier: Apache-2.0

package vspherepolicy

import (
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/vmware-tanzu/vm-operator/controllers/vspherepolicy/policyevaluation"
	"github.com/vmware-tanzu/vm-operator/controllers/vspherepolicy/tag"
	pkgcfg "github.com/vmware-tanzu/vm-operator/pkg/config"
	pkgctx "github.com/vmware-tanzu/vm-operator/pkg/context"
)

// AddToManager adds the controllers to the provided manager.
func AddToManager(ctx *pkgctx.ControllerManagerContext, mgr manager.Manager) error {
	if pkgcfg.FromContext(ctx).Features.VSpherePolicies {
		if err := policyevaluation.AddToManager(ctx, mgr); err != nil {
			return fmt.Errorf("failed to initialize policy evaluation controller: %w", err)
		}
	}

	if pkgcfg.FromContext(ctx).Features.TaggingAPI {
		if err := tag.AddToManager(ctx, mgr); err != nil {
			return fmt.Errorf("failed to initialize VM Tag controller: %w", err)
		}
	}

	return nil
}
