/*
Copyright 2021.

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
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"runtime"
	"strings"

	"github.com/grafana-operator/grafana-operator/v4/controllers/grafananotificationchannel"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	apis "github.com/grafana-operator/grafana-operator/v4/api"
	"github.com/grafana-operator/grafana-operator/v4/controllers/common"
	grafanaconfig "github.com/grafana-operator/grafana-operator/v4/controllers/config"
	grafana "github.com/grafana-operator/grafana-operator/v4/controllers/grafana"
	"github.com/grafana-operator/grafana-operator/v4/controllers/grafanadashboard"
	"github.com/grafana-operator/grafana-operator/v4/controllers/grafanadatasource"
	controllers "github.com/grafana-operator/grafana-operator/v4/controllers/namespace"
	"github.com/grafana-operator/grafana-operator/v4/internal/k8sutil"
	"github.com/grafana-operator/grafana-operator/v4/version"
	routev1 "github.com/openshift/api/route/v1"
	"github.com/operator-framework/operator-lib/leader"
	adv1 "k8s.io/api/admission/v1"
	"k8s.io/client-go/rest"
	k8sconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	integreatlyorgv1alpha1 "github.com/grafana-operator/grafana-operator/v4/api/integreatly/v1alpha1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	// +kubebuilder:scaffold:imports
)

var (
	scheme                        = k8sruntime.NewScheme()
	setupLog                      = ctrl.Log.WithName("setup")
	flagImage                     string
	flagImageTag                  string
	flagPluginsInitContainerImage string
	flagPluginsInitContainerTag   string
	flagNamespaces                string
	scanAll                       bool
	flagJsonnetLocation           string
	metricsAddr                   string
	enableLeaderElection          bool
	probeAddr                     string
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(integreatlyorgv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func printVersion() {
	log.Log.V(1).Info(fmt.Sprintf("Go Version: %s", runtime.Version()))
	log.Log.V(1).Info(fmt.Sprintf("Go OS/Arch: %s/%s", runtime.GOOS, runtime.GOARCH))
	log.Log.V(1).Info(fmt.Sprintf("operator-sdk Version: %v", "v1.3.0"))
	log.Log.V(1).Info(fmt.Sprintf("operator Version: %v", version.Version))
}

func assignOpts() {
	flag.StringVar(&flagImage, "grafana-image", "", "Overrides the default Grafana image")
	flag.StringVar(&flagImageTag, "grafana-image-tag", "", "Overrides the default Grafana image tag")
	flag.StringVar(&flagPluginsInitContainerImage, "grafana-plugins-init-container-image", "", "Overrides the default Grafana Plugins Init Container image")
	flag.StringVar(&flagPluginsInitContainerTag, "grafana-plugins-init-container-tag", "", "Overrides the default Grafana Plugins Init Container tag")
	flag.StringVar(&flagNamespaces, "namespaces", LookupEnvOrString("DASHBOARD_NAMESPACES", ""), "Namespaces to scope the interaction of the Grafana operator. Mutually exclusive with --scan-all")
	flag.StringVar(&flagJsonnetLocation, "jsonnet-location", "", "Overrides the base path of the jsonnet libraries")
	flag.BoolVar(&scanAll, "scan-all", LookupEnvOrBool("DASHBOARD_NAMESPACES_ALL", false), "Scans all namespaces for dashboards")

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
}

var (
	port     int
	certFile string
	keyFile  string
)

type admitFunc func(adv1.AdmissionReview) *adv1.AdmissionResponse

func serve(w http.ResponseWriter, r *http.Request, admit admitFunc) {
	var body []byte
	if r.Body != nil {
		if data, err := ioutil.ReadAll(r.Body); err == nil {
			body = data
		}
	}
	contentType := r.Header.Get("Content-Type")
	if contentType != "application/json" {
		log.Log.V(4).Info("contentType=" + contentType + "expect application/json")
		return
	}

	requestedAdmissionReview := adv1.AdmissionReview{}
	responseAdmissionReview := adv1.AdmissionReview{}

	if err := json.Unmarshal(body, &requestedAdmissionReview); err != nil {
		log.Log.V(4).Error(err, "fail unmarshal")
		responseAdmissionReview.Response = &adv1.AdmissionResponse{
			Allowed: false,
		}
	} else {
		responseAdmissionReview.Response = admit(requestedAdmissionReview)
	}

	responseAdmissionReview.Response.UID = requestedAdmissionReview.Request.UID
	responseAdmissionReview.APIVersion = "admission.k8s.io/v1"
	responseAdmissionReview.Kind = "AdmissionReview"
	respBytes, err := json.Marshal(responseAdmissionReview)

	if err != nil {
		log.Log.V(4).Error(err, "fail admission validate")
		responseAdmissionReview.Response = &adv1.AdmissionResponse{
			Allowed: false,
		}
	}
	if _, err := w.Write(respBytes); err != nil {
		log.Log.V(4).Error(err, "fail admission validate")
		responseAdmissionReview.Response = &adv1.AdmissionResponse{
			Allowed: false,
		}
	}
}

func serveGrafana(w http.ResponseWriter, r *http.Request) {
	log.Log.V(1).Info("Http request: method=%s, uri=%s", r.Method, r.URL.Path)
	serve(w, r, grafana.Grafanacheck)
}
func main() { // nolint

	printVersion()
	assignOpts()
	flag.IntVar(&port, "port", 9443, "grafana-operator port")
	flag.StringVar(&certFile, "certFile", "/run/secrets/tls/tls.crt", "grafana-operator cert")
	flag.StringVar(&keyFile, "keyFile", "/run/secrets/tls/tls.key", "x509 Private key file for TLS connection")
	namespace, err := k8sutil.GetWatchNamespace()
	if err != nil {
		log.Log.V(4).Error(err, "failed to get watch namespace")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Namespace:              namespace,
		MetricsBindAddress:     metricsAddr,
		Port:                   8443,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "2c0156f0.integreatly.org",
	})
	if err != nil {
		setupLog.V(4).Error(err, "unable to start manager")
		os.Exit(1)
	}

	if scanAll && flagNamespaces != "" {
		fmt.Fprint(os.Stderr, "--scan-all and --namespaces both set. Please provide only one")
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/validate", serveGrafana)
	keyPair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		log.Log.V(4).Error(err, "Failed to load key pair")
	}

	whsvr := &http.Server{
		Addr:      fmt.Sprintf(":%d", 9443),
		Handler:   mux,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{keyPair}},
	}
	log.Log.V(1).Info("start webhook")
	go func() {
		if err := whsvr.ListenAndServeTLS("", ""); err != nil {
			log.Log.V(4).Error(err, "Failed to listen and serve grafana-operator")
		}
	}()
	// Controller configuration
	controllerConfig := grafanaconfig.GetControllerConfig()
	controllerConfig.AddConfigItem(grafanaconfig.ConfigGrafanaImage, flagImage)
	controllerConfig.AddConfigItem(grafanaconfig.ConfigGrafanaImageTag, flagImageTag)
	controllerConfig.AddConfigItem(grafanaconfig.ConfigPluginsInitContainerImage, flagPluginsInitContainerImage)
	controllerConfig.AddConfigItem(grafanaconfig.ConfigPluginsInitContainerTag, flagPluginsInitContainerTag)
	controllerConfig.AddConfigItem(grafanaconfig.ConfigOperatorNamespace, namespace)
	controllerConfig.AddConfigItem(grafanaconfig.ConfigDashboardLabelSelector, "")
	controllerConfig.AddConfigItem(grafanaconfig.ConfigJsonnetBasePath, flagJsonnetLocation)

	// Get the namespaces to scan for dashboards
	// It's either the same namespace as the controller's or it's all namespaces if the
	// --scan-all flag has been passed
	var dashboardNamespaces = []string{namespace}
	if scanAll {
		dashboardNamespaces = []string{""}
		log.Log.V(1).Info("Scanning for dashboards in all namespaces")
	}

	if flagNamespaces != "" {
		dashboardNamespaces = getSanitizedNamespaceList()
		if len(dashboardNamespaces) == 0 {
			fmt.Fprint(os.Stderr, "--namespaces provided but no valid namespaces in list")
			os.Exit(1)
		}
		log.Log.V(1).Info(fmt.Sprintf("Scanning for dashboards in the following namespaces: [%s]", strings.Join(dashboardNamespaces, ",")))
	}

	// Get a config to talk to the apiserver
	cfg, err := k8sconfig.GetConfig()
	if err != nil {
		log.Log.V(4).Error(err, "")
		os.Exit(1)
	}

	// Become the leader before proceeding
	err = leader.Become(context.TODO(), "grafana-operator-lock")
	if err != nil {
		log.Log.V(4).Error(err, "")
	}

	log.Log.V(1).Info("Registering Components.")

	// Starting the resource auto-detection for the grafana controller
	autodetect, err := common.NewAutoDetect(mgr)
	if err != nil {
		log.Log.V(4).Error(err, "failed to start the background process to auto-detect the operator capabilities")
	} else {
		autodetect.Start()
		defer autodetect.Stop()
	}

	// Setup Scheme for all resources
	if err := apis.AddToScheme(mgr.GetScheme()); err != nil {
		log.Log.V(4).Error(err, "")
		os.Exit(1)
	}

	// Setup Scheme for OpenShift routes
	if err := routev1.AddToScheme(mgr.GetScheme()); err != nil {
		log.Log.V(4).Error(err, "")
		os.Exit(1)
	}

	if err != nil {
		log.Log.V(4).Error(err, "error starting metrics service")
	}

	log.Log.V(1).Info("Starting the Cmd.")

	// Start one dashboard controller per watch namespace
	for _, ns := range dashboardNamespaces {
		startDashboardController(ns, cfg, context.Background())
		startNotificationChannelController(ns, cfg, context.Background())

	}
	//startDatasourceController("monitoring", cfg, context.Background())
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	if err = (&grafana.ReconcileGrafana{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Plugins:  grafana.NewPluginsHelper(),
		Context:  ctx,
		Cancel:   cancel,
		Config:   grafanaconfig.GetControllerConfig(),
		Recorder: mgr.GetEventRecorderFor("GrafanaDashboard"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.V(4).Error(err, "unable to create controller", "controller", "Grafana")
		os.Exit(1)
	}
	if err = (&grafanadashboard.GrafanaDashboardReconciler{
		Client: mgr.GetClient(),
		Log:    ctrl.Log.WithName("controllers").WithName("GrafanaDashboard"),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.V(4).Error(err, "unable to create controller", "controller", "GrafanaDashboard")
		os.Exit(1)
	}
	if err = (&controllers.NamespaceReconciler{
		Client: mgr.GetClient(),
		Logger: ctrl.Log.WithName("controllers").WithName("Namespace"),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.V(4).Error(err, "unable to create controller", "controller", "Namespace")
		os.Exit(1)
	}
	if err = (&grafanadatasource.GrafanaDatasourceReconciler{
		Client:   mgr.GetClient(),
		Context:  ctx,
		Cancel:   cancel,
		Logger:   ctrl.Log.WithName("controllers").WithName("GrafanaDatasource"),
		Scheme:   mgr.GetScheme(),
		Recorder: mgr.GetEventRecorderFor("GrafanaDatasource"),
		//State:    common.ControllerState{},
	}).SetupWithManager(mgr); err != nil {
		setupLog.V(4).Error(err, "unable to create controller", "controller", "GrafanaDatasource")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("health", healthz.Ping); err != nil {
		setupLog.V(4).Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("check", healthz.Ping); err != nil {
		setupLog.V(4).Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager with options",
		"watchNamespace", namespace,
		"dashboardNamespaces", flagNamespaces,
		"scanAll", scanAll)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.V(4).Error(err, "problem running manager")
		os.Exit(1)
	}
}

// Starts a separate controller for the dashboard reconciliation in the background
func startDashboardController(ns string, cfg *rest.Config, ctx context.Context) {
	// Create a new Cmd to provide shared dependencies and start components
	dashboardMgr, err := manager.New(cfg, manager.Options{
		MetricsBindAddress: "0",
		Namespace:          ns,
	})
	if err != nil {
		log.Log.V(4).Error(err, "")
		os.Exit(1)
	}

	// Setup Scheme for the dashboard resource
	if err := apis.AddToScheme(dashboardMgr.GetScheme()); err != nil {
		log.Log.V(4).Error(err, "")
		os.Exit(1)
	}

	// Use a separate manager for the dashboard controller
	err = grafanadashboard.Add(dashboardMgr, ns)
	if err != nil {
		log.Log.V(4).Error(err, "")
	}

	go func() {
		if err := dashboardMgr.Start(ctx); err != nil {
			log.Log.V(4).Error(err, "dashboard manager exited non-zero")
			os.Exit(1)
		}
	}()
}

// Starts a separate controller for the notification channels reconciliation in the background
func startNotificationChannelController(ns string, cfg *rest.Config, ctx context.Context) {
	// Create a new Cmd to provide shared dependencies and start components
	channelMgr, err := manager.New(cfg, manager.Options{
		MetricsBindAddress: "0",
		Namespace:          ns,
	})
	if err != nil {
		log.Log.V(4).Error(err, "")
		os.Exit(1)
	}

	// Setup Scheme for the notification channel resource
	if err := apis.AddToScheme(channelMgr.GetScheme()); err != nil {
		log.Log.V(4).Error(err, "")
		os.Exit(1)
	}

	// Use a separate manager for the dashboard controller
	err = grafananotificationchannel.Add(channelMgr, ns)
	if err != nil {
		log.Log.V(4).Error(err, "")
		os.Exit(1)
	}

	go func() {
		if err := channelMgr.Start(ctx); err != nil {
			log.Log.V(4).Error(err, "notification channel manager exited non-zero")
			os.Exit(1)
		}
	}()
}

// Get the trimmed and sanitized list of namespaces (if --namespaces was provided)
func getSanitizedNamespaceList() []string {
	provided := strings.Split(flagNamespaces, ",")
	var selected []string

	for _, v := range provided {
		v = strings.TrimSpace(v)

		if v != "" {
			selected = append(selected, v)
		}
	}

	return selected
}

func LookupEnvOrString(key string, defaultVal string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return defaultVal
}

func LookupEnvOrBool(key string, defaultVal bool) bool {
	if val, ok := os.LookupEnv(key); ok {
		return val == "true"
	}
	return defaultVal
}
