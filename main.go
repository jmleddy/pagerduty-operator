/*
Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"

	hivev1 "github.com/openshift/hive/apis/hive/v1"
	"github.com/openshift/operator-custom-metrics/pkg/metrics"
	pagerdutyv1alpha1 "github.com/jmleddy/pagerduty-operator/api/v1alpha1"
	operatorconfig "github.com/jmleddy/pagerduty-operator/config"
	"github.com/jmleddy/pagerduty-operator/controllers/pagerdutyintegration"
	"github.com/jmleddy/pagerduty-operator/pkg/localmetrics"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap/zapcore"
	corev1 "k8s.io/api/core/v1"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	//+kubebuilder:scaffold:imports
)

var (
	scheme   = k8sruntime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
	// metricsPort the port on which metrics is hosted, don't pick one that's already used
	metricsPort = "8080"
	metricsPath = "/metrics"
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(hivev1.AddToScheme(scheme))
	utilruntime.Must(pagerdutyv1alpha1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
}

func printVersion() {
	setupLog.Info(fmt.Sprintf("Go Version: %s", runtime.Version()))
	setupLog.Info(fmt.Sprintf("Go OS/Arch: %s/%s", runtime.GOOS, runtime.GOARCH))
}

func main() {
	var enableLeaderElection bool
	var probeAddr string
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	opts := zap.Options{
		Development: false,
		TimeEncoder: zapcore.RFC3339TimeEncoder,
		// Remove misleading controller-runtime stack traces https://github.com/kubernetes-sigs/kubebuilder/issues/1593
		StacktraceLevel: zapcore.DPanicLevel,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Print configuration info
	printVersion()
	if err := operatorconfig.SetIsFedramp(); err != nil {
		setupLog.Error(err, "failed to get fedramp value")
		os.Exit(1)
	}
	if operatorconfig.IsFedramp() {
		setupLog.Info("running in fedramp environment.")
	}

	webhookServer := webhook.NewServer(webhook.Options{
			Port:    9443,
	})

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{
			BindAddress: ":0",
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "3cdd1aa9.pagerduty.10gen.io",
		WebhookServer:          webhookServer,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&pagerdutyintegration.PagerDutyIntegrationReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PagerDutyIntegration")
		os.Exit(1)
	}
	//+kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	// Configure custom metrics
	metricsServer := metrics.NewBuilder(operatorconfig.OperatorNamespace, operatorconfig.OperatorName).
		WithPort(metricsPort).
		WithPath(metricsPath).
		WithCollectors(localmetrics.MetricsList).
		WithRoute().
		GetConfig()

	if err := metrics.ConfigureMetrics(context.TODO(), *metricsServer); err != nil {
		setupLog.Error(err, "failed to configure custom metrics")
		os.Exit(1)
	}

	// Add runnable custom metrics
	err = mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		client := mgr.GetClient()
		pdAPISecret := &corev1.Secret{}
		err = client.Get(context.TODO(), types.NamespacedName{Namespace: operatorconfig.OperatorNamespace, Name: operatorconfig.PagerDutyAPISecretName}, pdAPISecret)
		if err != nil {
			setupLog.Error(err, "Failed to get secret")
			return err
		}
		var APIKey = string(pdAPISecret.Data[operatorconfig.PagerDutyAPISecretKey])
		timer := prometheus.NewTimer(localmetrics.MetricPagerDutyHeartbeat)
		localmetrics.UpdateAPIMetrics(APIKey, timer)

		return nil
	}))
	if err != nil {
		setupLog.Error(err, "unable add a runnable to the manager")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
