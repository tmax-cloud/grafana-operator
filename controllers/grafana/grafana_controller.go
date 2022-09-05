package grafana

import (
	"bytes"
	"context"
	"encoding/json"
	stdErr "errors"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/http"
	"reflect"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	grafanav1alpha1 "github.com/grafana-operator/grafana-operator/v4/api/integreatly/v1alpha1"
	"github.com/grafana-operator/grafana-operator/v4/controllers/common"
	"github.com/grafana-operator/grafana-operator/v4/controllers/config"
	"github.com/grafana-operator/grafana-operator/v4/controllers/constants"
	"github.com/grafana-operator/grafana-operator/v4/controllers/model"
	routev1 "github.com/openshift/api/route/v1"
	"k8s.io/api/admission/v1beta1"
	v12 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	netv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var Clientset *kubernetes.Clientset

const ControllerName = "grafana-controller"
const DefaultClientTimeoutSeconds = 5

var log = logf.Log.WithName(ControllerName)

// +kubebuilder:rbac:groups=integreatly.org,resources=grafanas;grafanas/finalizers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=integreatly.org,resources=grafanas/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=extensions;apps,resources=deployments;deployments/finalizers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch;create
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps;secrets;serviceaccounts;services;persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=route.openshift.io,resources=routes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete

// SetupWithManager sets up the controller with the Manager.

func (r *ReconcileGrafana) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&grafanav1alpha1.Grafana{}).
		Owns(&grafanav1alpha1.Grafana{}).
		Complete(r)
}

// Add creates a new Grafana Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager, autodetectChannel chan schema.GroupVersionKind, _ string) error {
	return add(mgr, NewReconciler(mgr), autodetectChannel)
}

// NewReconciler returns a new reconcile.Reconciler
func NewReconciler(mgr manager.Manager) reconcile.Reconciler {
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)

	return &ReconcileGrafana{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Plugins:  NewPluginsHelper(),
		Context:  ctx,
		Cancel:   cancel,
		Config:   config.GetControllerConfig(),
		Recorder: mgr.GetEventRecorderFor(ControllerName),
	}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler, autodetectChannel chan schema.GroupVersionKind) error {
	// Create a new controller
	c, err := controller.New("grafana-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource Grafana
	err = c.Watch(&source.Kind{Type: &grafanav1alpha1.Grafana{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	if err = watchSecondaryResource(c, &v12.Deployment{}); err != nil {
		return err
	}

	if err = watchSecondaryResource(c, &netv1.Ingress{}); err != nil {
		return err
	}

	if err = watchSecondaryResource(c, &v1.ConfigMap{}); err != nil {
		return err
	}

	if err = watchSecondaryResource(c, &v1.Service{}); err != nil {
		return err
	}

	if err = watchSecondaryResource(c, &v1.ServiceAccount{}); err != nil {
		return err
	}

	go func() {
		for gvk := range autodetectChannel {
			cfg := config.GetControllerConfig()

			// Route already watched?
			if cfg.GetConfigBool(config.ConfigRouteWatch, false) {
				return
			}

			// Watch routes if they exist on the cluster
			if gvk.String() == routev1.SchemeGroupVersion.WithKind(common.RouteKind).String() {
				if err = watchSecondaryResource(c, &routev1.Route{}); err != nil {
					log.Error(err, fmt.Sprintf("error adding secondary watch for %v", common.RouteKind))
				} else {
					cfg.AddConfigItem(config.ConfigRouteWatch, true)
					log.V(1).Info(fmt.Sprintf("added secondary watch for %v", common.RouteKind))
				}
			}
		}
	}()

	return nil
}

var _ reconcile.Reconciler = &ReconcileGrafana{}

// ReconcileGrafana reconciles a Grafana object
type ReconcileGrafana struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	Client   client.Client
	Scheme   *runtime.Scheme
	Plugins  *PluginsHelperImpl
	Context  context.Context
	Log      logr.Logger
	Cancel   context.CancelFunc
	Config   *config.ControllerConfig
	Recorder record.EventRecorder
}

func watchSecondaryResource(c controller.Controller, resource client.Object) error {
	return c.Watch(&source.Kind{Type: resource}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &grafanav1alpha1.Grafana{},
	})
}

// Reconcile reads that state of the cluster for a Grafana object and makes changes based on the state read
// and what is in the Grafana.Spec
func (r *ReconcileGrafana) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	instance := &grafanav1alpha1.Grafana{}
	err := r.Client.Get(r.Context, request.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// Stop the dashboard controller from reconciling when grafana is not installed
			r.Config.RemoveConfigItem(config.ConfigDashboardLabelSelector)
			r.Config.Cleanup(true)

			common.ControllerEvents <- common.ControllerState{
				GrafanaReady: false,
			}

			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	cr := instance.DeepCopy()

	// Read current state
	currentState := common.NewClusterState()
	err = currentState.Read(ctx, cr, r.Client)
	if err != nil {
		log.Error(err, "error reading state")
		return r.manageError(cr, err, request)
	}

	// Get the actions required to reach the desired state

	reconciler := NewGrafanaReconciler()
	desiredState := reconciler.Reconcile(currentState, cr)

	// Run the actions to reach the desired state
	actionRunner := common.NewClusterActionRunner(ctx, r.Client, r.Scheme, cr)
	err = actionRunner.RunAll(desiredState)
	if err != nil {
		return r.manageError(cr, err, request)
	}
	/*label := cr.GetObjectMeta().GetLabels()
	log.Info(label["grafana"])
	if label["grafana"] != "hypercloud" || cr.GetNamespace() != "monitoring" {
		r.Client.Delete(r.Context, instance)
	}*/
	// Run the config map reconciler to discover jsonnet libraries
	err = reconcileConfigMaps(cr, r)
	if err != nil {
		return r.manageError(cr, err, request)
	}
	GiveAdmin()
	return r.manageSuccess(cr, currentState, request)
}

func (r *ReconcileGrafana) manageError(cr *grafanav1alpha1.Grafana, issue error, request reconcile.Request) (reconcile.Result, error) {
	r.Recorder.Event(cr, "Warning", "ProcessingError", issue.Error())
	cr.Status.Phase = grafanav1alpha1.PhaseFailing
	cr.Status.Message = issue.Error()

	instance := &grafanav1alpha1.Grafana{}
	err := r.Client.Get(r.Context, request.NamespacedName, instance)
	if err != nil {
		return reconcile.Result{}, err
	}

	if !reflect.DeepEqual(cr.Status, instance.Status) {
		err := r.Client.Status().Update(r.Context, cr)
		if err != nil {
			// Ignore conflicts, resource might just be outdated.
			if errors.IsConflict(err) {
				err = nil
			}
			return reconcile.Result{}, err
		}
	}

	r.Config.InvalidateDashboards()

	common.ControllerEvents <- common.ControllerState{
		GrafanaReady: false,
	}

	return reconcile.Result{RequeueAfter: config.RequeueDelay}, nil
}

// Try to find a suitable url to grafana
func (r *ReconcileGrafana) getGrafanaAdminUrl(cr *grafanav1alpha1.Grafana, state *common.ClusterState) (string, error) {
	// If preferService is true, we skip the routes and try to access grafana
	// by using the service.
	preferService := cr.GetPreferServiceValue()

	// First try to use the route if it exists. Prefer the route because it also works
	// when running the operator outside of the cluster
	if state.GrafanaRoute != nil && !preferService {
		return fmt.Sprintf("https://%v", state.GrafanaRoute.Spec.Host), nil
	}

	// Try the ingress first if on vanilla Kubernetes
	if state.GrafanaIngress != nil && !preferService {
		// If provided, use the hostname from the CR
		if cr.Spec.Ingress != nil && cr.Spec.Ingress.Hostname != "" {
			return fmt.Sprintf("https://%v", cr.Spec.Ingress.Hostname), nil
		}

		// Otherwise try to find something suitable, hostname or IP
		if len(state.GrafanaIngress.Status.LoadBalancer.Ingress) > 0 {
			ingress := state.GrafanaIngress.Status.LoadBalancer.Ingress[0]
			if ingress.Hostname != "" {
				return fmt.Sprintf("https://%v", ingress.Hostname), nil
			}
			return fmt.Sprintf("https://%v", ingress.IP), nil
		}
	}

	var servicePort = int32(model.GetGrafanaPort(cr))

	// Otherwise rely on the service
	if state.GrafanaService != nil {
		return fmt.Sprintf("http://%v.%v.svc.cluster.local:%d", state.GrafanaService.Name, cr.Namespace,
			servicePort), nil
	}

	return "", stdErr.New("failed to find admin url")
}

func (r *ReconcileGrafana) manageSuccess(cr *grafanav1alpha1.Grafana, state *common.ClusterState, request reconcile.Request) (reconcile.Result, error) {
	cr.Status.Phase = grafanav1alpha1.PhaseReconciling
	cr.Status.Message = "success"

	// Only update the status if the dashboard controller had a chance to sync the cluster
	// dashboards first. Otherwise reuse the existing dashboard config from the CR.
	if r.Config.GetConfigBool(config.ConfigGrafanaDashboardsSynced, false) {
		cr.Status.InstalledDashboards = r.Config.Dashboards
	} else {
		if r.Config.Dashboards == nil {
			r.Config.SetDashboards([]*grafanav1alpha1.GrafanaDashboardRef{})
		}
	}

	instance := &grafanav1alpha1.Grafana{}
	err := r.Client.Get(r.Context, request.NamespacedName, instance)
	if err != nil {
		return r.manageError(cr, err, request)
	}

	if !reflect.DeepEqual(cr.Status, instance.Status) {
		err := r.Client.Status().Update(r.Context, cr)
		if err != nil {
			return r.manageError(cr, err, request)
		}
	}
	// Make the Grafana API URL available to the dashboard controller
	url, err := r.getGrafanaAdminUrl(cr, state)
	if err != nil {
		return r.manageError(cr, err, request)
	}

	// Publish controller state
	controllerState := common.ControllerState{
		DashboardSelectors:         cr.Spec.DashboardLabelSelector,
		DashboardNamespaceSelector: cr.Spec.DashboardNamespaceSelector,
		AdminUrl:                   url,
		GrafanaReady:               true,
		ClientTimeout:              DefaultClientTimeoutSeconds,
	}

	if cr.Spec.Client != nil && cr.Spec.Client.TimeoutSeconds != nil {
		seconds := *cr.Spec.Client.TimeoutSeconds
		if seconds < 0 {
			seconds = DefaultClientTimeoutSeconds
		}
		controllerState.ClientTimeout = seconds
	}

	common.ControllerEvents <- controllerState

	log.V(1).Info("desired cluster state met")
	log.V(1).Info("Start to give admin to Tmax account")

	GiveAdmin() // create admin account
	return reconcile.Result{RequeueAfter: config.RequeueDelay}, nil
}

var grafanaId string
var grafanaPw string

func CreateGrafanaUser(email string) {

	httpposturl_user := "http://" + grafanaId + ":" + grafanaPw + "@" + constants.GrafanaMonitoringAddress + "api/admin/users"
	// get grafana api key
	if email == "kubernetes-admin" {
		email = "kubernetes-admin@localhost"
	}
	var grafana_user_body model.Grafana_user
	grafana_user_body.Email = email
	grafana_user_body.Name = RandomString(8)
	grafana_user_body.Login = RandomString(8)
	grafana_user_body.Password = "1234"

	json_body, _ := json.Marshal(grafana_user_body)

	request, _ := http.NewRequest("POST", httpposturl_user, bytes.NewBuffer(json_body))

	request.Header.Add("Content-Type", "application/json; charset=UTF-8")
	//	request.Header.Add("Authorization", util.GrafanaKey)
	client := &http.Client{}
	resp, err := client.Do(request)
	if err != nil {
		return
	} else {
		defer resp.Body.Close()

		log.V(1).Info(" Create Grafana User " + email + " Success ")
	}
}
func RandomString(n int) string {
	rand.Seed(time.Now().UnixNano())
	var letters = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789")

	s := make([]rune, n)
	for i := range s {
		s[i] = letters[rand.Intn(len(letters))]
	}
	return string(s)
}

func GetGrafanaKey() string {
	grafanaId, grafanaPw = "admin", "admin"
	// get grafana api key

	httpposturl := "http://" + grafanaId + ":" + grafanaPw + "@" + constants.GrafanaMonitoringAddress + "api/auth/keys"

	var GrafanaKeyBody model.GrafanaKeyBody

	GrafanaKeyBody.Name = RandomString(8)
	GrafanaKeyBody.Role = "Admin"
	GrafanaKeyBody.SecondsToLive = 300
	json_body, _ := json.Marshal(GrafanaKeyBody)
	request, _ := http.NewRequest("POST", httpposturl, bytes.NewBuffer(json_body))

	request.Header.Set("Content-Type", "application/json; charset=UTF-8")

	client := &http.Client{}
	response, err := client.Do(request)
	if err != nil {

		return err.Error()
	} else {
		defer response.Body.Close()
		body, _ := ioutil.ReadAll(response.Body)

		var grafana_resp model.Grafana_key
		json.Unmarshal([]byte(body), &grafana_resp)
		model.GrafanaKey = "Bearer " + grafana_resp.Key

		/////create grafana user
		log.V(1).Info("start to create grafana user")
	}
	return model.GrafanaKey
}

func GetGrafanaUser(email string) int {
	if email == "kubernetes-admin" {
		email = "kubernetes-admin@localhost"
	}
	grafanaId, grafanaPw = "admin", "admin"
	httpgeturl := "http://" + grafanaId + ":" + grafanaPw + "@" + constants.GrafanaMonitoringAddress + "api/users/lookup?loginOrEmail=" + email
	request, _ := http.NewRequest("GET", httpgeturl, nil)
	client := &http.Client{}
	resp, err := client.Do(request)
	var GrafanaUserGet model.Grafana_User_Get
	if err != nil {
		log.V(4).Error(err, "err")
		return 0
	} else {
		defer resp.Body.Close()

		body, _ := ioutil.ReadAll(resp.Body)
		json.Unmarshal([]byte(body), &GrafanaUserGet)
	}
	return GrafanaUserGet.Id
}

func GetCRBAdmin() string {
	log.V(1).Info("getting crb")
	var config *rest.Config
	var err error
	config, err = rest.InClusterConfig()
	Clientset, _ = kubernetes.NewForConfig(config)
	crbList, err := Clientset.RbacV1().ClusterRoleBindings().List(
		context.TODO(),
		metav1.ListOptions{},
	)

	//log.V(1).Info("crb list:", crbList)
	if crbList == nil {
		log.V(4).Error(err, "err")
	}

	var adminemail string
	for _, crb := range crbList.Items {
		log.V(1).Info(crb.Name)
		if crb.Name == "admin" {
			adminemail = crb.Subjects[0].Name
			log.V(1).Info("admin is " + adminemail)
			break
		}
	}
	return adminemail
}

func GiveAdmin() {
	var hc_admin string
	hc_admin = GetCRBAdmin() //"hc-admin@tmax.co.kr"
	log.V(1).Info("Getting admin CRB")
	//log.V(1).Info(hc_admin)
	if GetGrafanaUser(hc_admin) == 0 && hc_admin != "" {
		CreateGrafanaUser(hc_admin)
		id := GetGrafanaUser(hc_admin)
		adminBody := `{"isGrafanaAdmin": true}`
		grafanaId, grafanaPw := "admin", "admin"
		httpgeturl := "http://" + grafanaId + ":" + grafanaPw + "@" + constants.GrafanaMonitoringAddress + "api/admin/users/" + strconv.Itoa(id) + "/permissions"

		request, _ := http.NewRequest("PUT", httpgeturl, bytes.NewBuffer([]byte(adminBody)))

		request.Header.Set("Content-Type", "application/json; charset=UTF-8")

		client := &http.Client{}
		response, err := client.Do(request)
		if err != nil {
			log.V(1).Error(err, "err")

		} else {
			defer response.Body.Close()
		}

		//org permission
		httpgeturlorg := "http://" + grafanaId + ":" + grafanaPw + "@" + constants.GrafanaMonitoringAddress + "api/orgs/1/users/" + strconv.Itoa(id)
		adminorgBody := `{"role":"Admin"}`
		request, _ = http.NewRequest("PATCH", httpgeturlorg, bytes.NewBuffer([]byte(adminorgBody)))
		request.Header.Set("Content-Type", "application/json; charset=UTF-8")
		//request.Header.Set("Authorization", util.GrafanaKey)
		client2 := &http.Client{}
		response, err = client2.Do(request)
		if err != nil {
			log.V(1).Error(err, "err")

		} else {
			defer response.Body.Close()
		}
	}
}

func Grafanacheck(ar v1beta1.AdmissionReview) *v1beta1.AdmissionResponse {
	reviewResponse := v1beta1.AdmissionResponse{}

	g := grafanav1alpha1.Grafana{}
	log.V(1).Info("check grafana validate")
	err := json.Unmarshal(ar.Request.Object.Raw, &g)
	if err == nil {
		log.V(1).Info(ar.String())
		label := g.GetObjectMeta().GetLabels()
		if g.GetNamespace() != "monitoring" || label["grafana"] != "hypercloud" {
			return &v1beta1.AdmissionResponse{
				Allowed: false,
			}
		} else {
			return &v1beta1.AdmissionResponse{
				Allowed: true,
			}
		}
	}

	return &reviewResponse
}
