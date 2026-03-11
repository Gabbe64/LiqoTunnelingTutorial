// Copyright 2019-2026 The Liqo Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package generic provides a scaffold to build generic tunneling protocol processes
// with standard Kubernetes controller-runtime manager setup and connection handling.
package generic

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/liqotech/liqo/pkg/gateway"

	"github.com/liqotech/liqo/pkg/gateway/concurrent"

	"github.com/liqotech/liqo/pkg/utils/mapper"
)

// GwContainerOptions groups common setup parameters for a Generic Gateway Container manager.
type GwContainerOptions struct {
	GwOptions         *gateway.Options
	Config            *rest.Config
	Scheme            *runtime.Scheme
	ExtraCacheOptions *cache.Options // Optional, to restrict namespaces, etc.
}

// SetupAndStartManager encapsulates standard generic controller manager initializations,
// configures Health & Ready probes, and coordinates startup with the gateway orchestrator
// via IPC (when leader election is enabled), before invoking the protocol-specific setup
// and starting the manager.
func SetupAndStartManager(ctx context.Context, opts GwContainerOptions, setupProtocolFunc func(mgr ctrl.Manager) error) error {
	gwoptions := opts.GwOptions

	// Create the manager options
	mgrOpts := ctrl.Options{
		MapperProvider: mapper.LiqoMapperProvider(opts.Scheme),
		Scheme:         opts.Scheme,
		Metrics: server.Options{
			BindAddress: gwoptions.MetricsAddress,
		},
		HealthProbeBindAddress: gwoptions.ProbeAddr,
		LeaderElection:         false,
	}

	if opts.ExtraCacheOptions != nil {
		mgrOpts.Cache = *opts.ExtraCacheOptions
	}

	var mgr ctrl.Manager
	var err error
	mgr, err = ctrl.NewManager(opts.Config, mgrOpts)
	if err != nil {
		return fmt.Errorf("unable to create manager: %w", err)
	}

	// Register the healthiness probes.
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("unable to set up healthz probe: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("unable to set up readyz probe: %w", err)
	}

	// Give way for the custom protocol to inject runnable processes, specific reconcilers or route configurations.
	if setupProtocolFunc != nil {
		if err := setupProtocolFunc(mgr); err != nil {
			return fmt.Errorf("unable to setup external protocol framework hooks: %w", err)
		}
	}

	// Connect to the gateway orchestrator and wait for the start signal.
	if gwoptions.LeaderElection {
		runnable, err := concurrent.NewRunnableGuest(gwoptions.ContainerName)
		if err != nil {
			return fmt.Errorf("unable to create runnable guest: %w", err)
		}
		if err := runnable.Start(ctx); err != nil {
			return fmt.Errorf("unable to start runnable guest: %w", err)
		}
		defer runnable.Close()
	}

	// Start the manager.
	return mgr.Start(ctx)
}
