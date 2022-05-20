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

package grafanadashboard

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"

	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"

	"github.com/grafana-operator/grafana-operator/v4/api/integreatly/v1alpha1"
	grafanav1alpha1 "github.com/grafana-operator/grafana-operator/v4/api/integreatly/v1alpha1"
	integreatlyorgv1alpha1 "github.com/grafana-operator/grafana-operator/v4/api/integreatly/v1alpha1"
	"github.com/grafana-operator/grafana-operator/v4/controllers/common"
	"github.com/grafana-operator/grafana-operator/v4/controllers/config"
	"github.com/grafana-operator/grafana-operator/v4/controllers/constants"
	"github.com/grafana-operator/grafana-operator/v4/controllers/grafana"
	"github.com/grafana-operator/grafana-operator/v4/controllers/model"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	ControllerName = "controller_grafanadashboard"
)

// GrafanaDashboardReconciler reconciles a GrafanaDashboard object
type GrafanaDashboardReconciler struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver

	Client    client.Client
	Scheme    *runtime.Scheme
	transport *http.Transport
	config    *config.ControllerConfig
	context   context.Context
	cancel    context.CancelFunc
	recorder  record.EventRecorder
	state     common.ControllerState
	Log       logr.Logger
}

// +kubebuilder:rbac:groups=integreatly.org,resources=grafanadashboards,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=integreatly.org,resources=grafanadashboards/status,verbs=get;update;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the GrafanaDashboard object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.7.0/pkg/reconcile
func (r *GrafanaDashboardReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	logger := r.Log.WithValues(ControllerName, request.NamespacedName)

	// If Grafana is not running there is no need to continue
	if !r.state.GrafanaReady {
		logger.Info("no grafana instance available")
		return reconcile.Result{Requeue: false}, nil
	}

	getClient, err := r.getClient()
	if err != nil {
		return reconcile.Result{RequeueAfter: config.RequeueDelay}, err
	}

	// Initial request?
	if request.Name == "" {
		return r.reconcileDashboards(request, getClient)
	}

	// Check if the label selectors are available yet. If not then the grafana controller
	// has not finished initializing and we can't continue. Reschedule for later.
	if r.state.DashboardSelectors == nil {
		return reconcile.Result{RequeueAfter: config.RequeueDelay}, nil
	}

	// Fetch the GrafanaDashboard instance
	instance := &grafanav1alpha1.GrafanaDashboard{}
	err = r.Client.Get(r.context, request.NamespacedName, instance)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			// If some dashboard has been deleted, then always re sync the world
			logger.Info("deleting dashboard", "namespace", request.Namespace, "name", request.Name)
			return r.reconcileDashboards(request, getClient)
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	// If the dashboard does not match the label selectors then we ignore it
	cr := instance.DeepCopy()
	if !r.isMatch(cr) {
		logger.V(1).Info(fmt.Sprintf("dashboard %v/%v found but selectors do not match",
			cr.Namespace, cr.Name))
		return ctrl.Result{}, nil
	}
	// Otherwise always re sync all dashboards in the namespace
	return r.reconcileDashboards(request, getClient)
}

// Add creates a new GrafanaDashboard Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager, namespace string) error {
	return SetupWithManager(mgr, newReconciler(mgr), namespace)
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)

	return &GrafanaDashboardReconciler{
		Client: mgr.GetClient(),
		/* #nosec G402 */
		transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		},
		Log:      mgr.GetLogger(),
		config:   config.GetControllerConfig(),
		context:  ctx,
		cancel:   cancel,
		recorder: mgr.GetEventRecorderFor(ControllerName),
		state:    common.ControllerState{},
	}
}

// SetupWithManager sets up the controller with the Manager.
func SetupWithManager(mgr ctrl.Manager, r reconcile.Reconciler, namespace string) error {
	c, err := controller.New("grafanadashboard-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource GrafanaDashboard
	err = c.Watch(&source.Kind{Type: &grafanav1alpha1.GrafanaDashboard{}}, &handler.EnqueueRequestForObject{})
	if err == nil {
		log.Log.Info("Starting dashboard controller")
	}

	ref := r.(*GrafanaDashboardReconciler) // nolint
	ticker := time.NewTicker(config.RequeueDelay)
	sendEmptyRequest := func() {
		request := reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: namespace,
				Name:      "",
			},
		}
		_, err = r.Reconcile(ref.context, request)
		if err != nil {
			return
		}
	}

	go func() {
		for range ticker.C {
			log.Log.Info("running periodic dashboard resync")
			sendEmptyRequest()
		}
	}()

	go func() {
		for stateChange := range common.ControllerEvents {
			// Controller state updated
			ref.state = stateChange
		}
	}()
	return ctrl.NewControllerManagedBy(mgr).
		For(&integreatlyorgv1alpha1.GrafanaDashboard{}).
		Complete(r)
}

var _ reconcile.Reconciler = &GrafanaDashboardReconciler{}

// Check if a given dashboard (by name) is present in the list of
// dashboards in the namespace
func inNamespace(namespaceDashboards *grafanav1alpha1.GrafanaDashboardList, item *grafanav1alpha1.GrafanaDashboardRef) bool {
	for _, d := range namespaceDashboards.Items {
		if d.Name == item.Name && d.Namespace == item.Namespace {
			return true
		}
	}
	return false
}

// Returns the hash of a dashboard if it is known
func findHash(knownDashboards []*integreatlyorgv1alpha1.GrafanaDashboardRef, item *grafanav1alpha1.GrafanaDashboard) string {
	for _, d := range knownDashboards {
		if item.Name == d.Name && item.Namespace == d.Namespace {
			return d.Hash
		}
	}
	return ""
}

// Returns the UID of a dashboard if it is known
func findUid(knownDashboards []*integreatlyorgv1alpha1.GrafanaDashboardRef, item *grafanav1alpha1.GrafanaDashboard) string {
	for _, d := range knownDashboards {
		if item.Name == d.Name && item.Namespace == d.Namespace {
			return d.UID
		}
	}
	return ""
}

func (r *GrafanaDashboardReconciler) reconcileDashboards(request reconcile.Request, grafanaClient GrafanaClient) (reconcile.Result, error) { // nolint
	// Collect known and namespace dashboards
	knownDashboards := r.config.GetDashboards(request.Namespace)
	namespaceDashboards := &grafanav1alpha1.GrafanaDashboardList{}

	opts := &client.ListOptions{
		Namespace: request.Namespace,
	}

	err := r.Client.List(r.context, namespaceDashboards, opts)
	if err != nil {
		return reconcile.Result{}, err
	}

	// Prepare lists
	var dashboardsToDelete []*grafanav1alpha1.GrafanaDashboardRef

	// Dashboards to delete: dashboards that are known but not found
	// any longer in the namespace
	for _, dashboard := range knownDashboards {
		if !inNamespace(namespaceDashboards, dashboard) {
			dashboardsToDelete = append(dashboardsToDelete, dashboard)
		}
	}

	// Process new/updated dashboards
	for i, dashboard := range namespaceDashboards.Items {
		// Is this a dashboard we care about (matches the label selectors)?
		if !r.isMatch(&namespaceDashboards.Items[i]) {
			log.Log.Info("dashboard found but selectors do not match",
				"namespace", dashboard.Namespace, "name", dashboard.Name)
			continue
		}
		//log.Log.Info(namespaceDashboards.Items[i].ObjectMeta.GetAnnotations()["userId"])

		folderName := dashboard.Namespace
		if dashboard.Spec.CustomFolderName != "" {
			folderName = dashboard.Spec.CustomFolderName
		}

		folder, err := grafanaClient.CreateOrUpdateFolder(folderName)

		if err != nil {
			log.Log.Error(err, "failed to get or create namespace folder for dashboard", "folder", folderName, "dashboard", request.Name)
			r.manageError(&namespaceDashboards.Items[i], err)
			continue
		}

		var folderId int64
		if folder.ID == nil {
			folderId = 0
		} else {
			folderId = *folder.ID
		}

		// Process the dashboard. Use the known hash of an existing dashboard
		// to determine if an update is required
		knownHash := findHash(knownDashboards, &namespaceDashboards.Items[i])
		knownUid := findUid(knownDashboards, &namespaceDashboards.Items[i])
		pipeline := NewDashboardPipeline(r.Client, &namespaceDashboards.Items[i], r.context)
		processed, err := pipeline.ProcessDashboard(knownHash, &folderId, folderName, false)

		// Check known dashboards exist on grafana instance and recreate if not
		if knownUid != "" {
			response, err := grafanaClient.GetDashboard(knownUid)
			if err != nil {
				log.Log.Error(err, "Failed to search Grafana for dashboard")
			}

			if *response.Dashboard.ID == uint(0) {
				log.Log.Info(fmt.Sprintf("Dashboard %v has been deleted via grafana console. Recreating.", namespaceDashboards.Items[i].ObjectMeta.Name))
				processed, err = pipeline.ProcessDashboard(knownHash, &folderId, folderName, true)

				if err != nil {
					log.Log.Error(err, "cannot process dashboard", "namespace", dashboard.Namespace, "name", dashboard.Name)
					r.manageError(&namespaceDashboards.Items[i], err)
					continue
				}
			}
		}

		if err != nil {
			log.Log.Error(err, "cannot process dashboard", "namespace", dashboard.Namespace, "name", dashboard.Name)
			r.manageError(&namespaceDashboards.Items[i], err)
			continue
		}

		if processed == nil {
			r.config.SetPluginsFor(&namespaceDashboards.Items[i])
			continue
		}
		// Check labels only when DashboardNamespaceSelector isnt empty
		if r.state.DashboardNamespaceSelector != nil {
			matchesNamespaceLabels, err := r.checkNamespaceLabels(&namespaceDashboards.Items[i])
			if err != nil {
				r.manageError(&namespaceDashboards.Items[i], err)
				continue
			}

			if !matchesNamespaceLabels {
				log.Log.Info("dashboard %v skipped because the namespace labels do not match", "dashboard", dashboard.Name)
				continue
			}
		}

		_, err = grafanaClient.CreateOrUpdateDashboard(processed, folderId, folderName)
		if err != nil {
			//log.Log.Error(err, "cannot submit dashboard %v/%v", "namespace", dashboard.Namespace, "name", dashboard.Name)
			klog.Info(folderName)
			r.manageError(&namespaceDashboards.Items[i], err)

			continue
		}

		r.manageSuccess(&namespaceDashboards.Items[i], &folderId, folderName, grafanaClient)
	}

	for _, dashboard := range dashboardsToDelete {
		status, err := grafanaClient.DeleteDashboardByUID(dashboard.UID)
		if err != nil {
			log.Log.Error(err, "error deleting dashboard, status was",
				"dashboardUID", dashboard.UID,
				"status.Status", *status.Status,
				"status.Message", *status.Message)
		}

		log.Log.Info(fmt.Sprintf("delete result was %v", *status.Message))

		r.config.RemovePluginsFor(dashboard.Namespace, dashboard.Name)
		r.config.RemoveDashboard(dashboard.UID)

		// Mark the dashboards as synced so that the current state can be written
		// to the Grafana CR by the grafana controller
		r.config.AddConfigItem(config.ConfigGrafanaDashboardsSynced, true)

		// Refresh the list of known dashboards after the dashboard has been removed
		knownDashboards = r.config.GetDashboards(request.Namespace)

		// Check for empty managed folders (namespace-named) and delete obsolete ones
		if dashboard.FolderName == "" || dashboard.FolderName == dashboard.Namespace {
			if safe := grafanaClient.SafeToDelete(knownDashboards, dashboard.FolderId); !safe {
				log.Log.Info("folder cannot be deleted as it's being used by other dashboards")
				break
			}
			if err = grafanaClient.DeleteFolder(dashboard.FolderId); err != nil {
				log.Log.Error(err, "delete dashboard folder failed", "dashboard.folderId", *dashboard.FolderId)
			}
		}
	}

	return reconcile.Result{Requeue: false}, nil
}

// Get an authenticated grafana API client
func (r *GrafanaDashboardReconciler) getClient() (GrafanaClient, error) {
	url := r.state.AdminUrl
	if url == "" {
		return nil, errors.New("cannot get grafana admin url")
	}

	username := os.Getenv(constants.GrafanaAdminUserEnvVar)
	if username == "" {
		return nil, errors.New("invalid credentials (username)")
	}

	password := os.Getenv(constants.GrafanaAdminPasswordEnvVar)
	if password == "" {
		return nil, errors.New("invalid credentials (password)")
	}

	duration := time.Duration(r.state.ClientTimeout)

	return NewGrafanaClient(url, username, password, r.transport, duration), nil
}

// Test if a given dashboard matches an array of label selectors
func (r *GrafanaDashboardReconciler) isMatch(item *grafanav1alpha1.GrafanaDashboard) bool {
	if r.state.DashboardSelectors == nil {
		return false
	}

	match, err := item.MatchesSelectors(r.state.DashboardSelectors)
	if err != nil {
		log.Log.Error(err, "error matching selectors",
			"item.Namespace", item.Namespace,
			"item.Name", item.Name)
		return false
	}
	return match
}

// check if the labels on a namespace match a given label selector
func (r *GrafanaDashboardReconciler) checkNamespaceLabels(dashboard *grafanav1alpha1.GrafanaDashboard) (bool, error) {
	key := client.ObjectKey{
		Name: dashboard.Namespace,
	}
	ns := &v1.Namespace{}
	err := r.Client.Get(r.context, key, ns)
	if err != nil {
		return false, err
	}
	selector, err := metav1.LabelSelectorAsSelector(r.state.DashboardNamespaceSelector)

	if err != nil {
		return false, err
	}

	return selector.Empty() || selector.Matches(labels.Set(ns.Labels)), nil
}

// Handle success case: update dashboard metadata (id, uid) and update the list
// of plugins
func (r *GrafanaDashboardReconciler) manageSuccess(dashboard *grafanav1alpha1.GrafanaDashboard, folderId *int64, folderName string, grafanaClient GrafanaClient) {
	msg := fmt.Sprintf("dashboard %v/%v successfully submitted",
		dashboard.Namespace,
		dashboard.Name)
	r.recorder.Event(dashboard, "Normal", "Success", msg)
	log.Log.Info("dashboard successfully submitted", "name", dashboard.Name, "namespace", dashboard.Namespace)
	r.config.AddDashboard(dashboard, folderId, folderName)
	r.config.SetPluginsFor(dashboard)

	userlist1 := dashboard.GetAnnotations()["userId"]
	userlist := strings.Split(dashboard.GetAnnotations()["userId"], ",")
	var userIdList []int

	log.Log.Info("dashboard UID: " + dashboard.UID())
	klog.Infoln(userlist1)

	dashboardId := GetGrafanaDashboardId(dashboard.UID())
	if userlist[0] == "" {
		userlist[0] = userlist1
	}
	for _, user := range userlist {
		//user_tmp := strings.Trim(user, " ")
		log.Log.Info("dashboard is owned by " + user)
		var response int

		response = grafana.GetGrafanaUser(user)

		if response == 0 {
			/*httphyperauth := "https://" + model.HyperauthUrl + "/auth/realms/tmax/user/" + user_tmp + "/exists"
			request, _ := http.NewRequest("GET", httphyperauth, nil)
			client := &http.Client{}
			resp, _ := client.Do(request)
			body, _ := ioutil.ReadAll(resp.Body)
			if strings.Compare(string(body), "true") == 0 {

			}*/
			grafana.CreateGrafanaUser(user)
			response = grafana.GetGrafanaUser(user)
		}
		userIdList = append(userIdList, response)

	}
	log.Log.Info("Start to give grafana permission")
	r.CreateGrafanaPermission(userIdList, dashboardId, folderName, grafanaClient)

}

func GetGrafanaDashboardId(uid string) int {
	grafanaId, grafanaPw = "admin", "admin"
	httpgeturl := "http://" + grafanaId + ":" + grafanaPw + "@" + constants.GrafanaMonitoringAddress + "api/dashboards/uid/" + uid
	request, _ := http.NewRequest("GET", httpgeturl, nil)
	client := &http.Client{}
	resp, err := client.Do(request)
	var GrafanaDashboardGet model.Grafana_Dashboad_resp
	if err != nil {
		klog.Errorln(err)
		return 0
	} else {
		defer resp.Body.Close()

		body, _ := ioutil.ReadAll(resp.Body)
		json.Unmarshal([]byte(body), &GrafanaDashboardGet)
		//klog.Infof(string(body))
		//klog.Infof(strconv.Itoa(GrafanaDashboardGet.Dashboard.Id))
	}
	return GrafanaDashboardGet.Dashboard.Id
}

// Handle error case: update dashboard with error message and status
func (r *GrafanaDashboardReconciler) manageError(dashboard *grafanav1alpha1.GrafanaDashboard, issue error) {
	r.recorder.Event(dashboard, "Warning", "ProcessingError", issue.Error())
	// Ignore conflicts. Resource might just be outdated, also ignore if grafana isn't available.
	if k8serrors.IsConflict(issue) || k8serrors.IsServiceUnavailable(issue) {
		return
	}
	log.Log.Error(issue, "error updating dashboard")
}

func (r *GrafanaDashboardReconciler) SetupWithManager(mgr manager.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&integreatlyorgv1alpha1.GrafanaDashboard{}).
		Complete(r)
}

var grafanaId string
var grafanaPw string

func (r *GrafanaDashboardReconciler) CreateGrafanaPermission(userIdList []int, dashboardId int, folderName string, grafanaClient GrafanaClient) {

	// get grafana api key
	log.Log.Info(strconv.Itoa(userIdList[0]) + "start to give permission")
	//grafanaClient, err := r.getClient()

	folderlist, _ := grafanaClient.getAllFolders()
	var fuid string
	for _, folder := range folderlist {
		if folder.Title == folderName {
			fuid = folder.UID
			klog.Info("folder uid is " + fuid)
		}
	}
	model.GrafanaKey = grafana.GetGrafanaKey()
	httpposturl_per := "http://" + constants.GrafanaMonitoringAddress + "api/folders/" + fuid + "/permissions"
	permBody := `{
		"items": [`

	for i, user := range userIdList {

		permBody = permBody + `
			{
			"userId": ` + strconv.Itoa(user) + `,
			"permission": 1
			}`
		if i == len(userIdList)-1 {
			break
		}

		permBody = permBody + `,`
	}
	permBody = permBody + `
			]
		}`
	log.Log.Info(permBody)

	request, _ := http.NewRequest("POST", httpposturl_per, bytes.NewBuffer([]byte(permBody)))

	request.Header.Set("Content-Type", "application/json; charset=UTF-8")
	request.Header.Set("Authorization", model.GrafanaKey)
	client := &http.Client{}
	response, err := client.Do(request)
	if err != nil {
		klog.Errorln(err)
		return
	} else {
		defer response.Body.Close()
		resbody, _ := ioutil.ReadAll(response.Body)
		klog.Infof(string(resbody))
	}
}

type projectClient struct {
	restClient rest.Interface
	ns         string
}

func CreateUserDashboard(ctx context.Context, namespace string, user string) (result *v1alpha1.GrafanaDashboard, err error) {

	//	c := projectClient{}
	nsByte, err := ioutil.ReadFile("/var/dashboard/nsdashboard.json")
	if err != nil {
		return
	}
	res1 := strings.Replace(string(nsByte), "$namespace", namespace, -1)
	res := strings.Replace(res1, "$uid", namespace, -1)
	//res := strings.Replace(res2, "$source", "prometheus", -1)
	v := grafanav1alpha1.GrafanaDashboard{}
	v.APIVersion = "integreatly.org/v1alpha1"
	v.ObjectMeta.Namespace = namespace
	v.ObjectMeta.Name = "userdashboard-" + namespace
	v.Spec.Json = res
	v.TypeMeta.Kind = "GrafanaDashboard"
	label := map[string]string{}
	label["app"] = "grafana"
	v.ObjectMeta.SetLabels(label)
	anno := map[string]string{}
	anno["userId"] = user
	v.SetAnnotations(anno)

	result = &v1alpha1.GrafanaDashboard{}
	log.Log.Info("creating user NS Dashboard...")

	if err != nil {
		panic(err)
	}
	/*grafanav1alpha1.AddToScheme(scheme.Scheme)
	var config *rest.Config

	config, err = rest.InClusterConfig()

	crdConfig := *config
	crdConfig.ContentConfig.GroupVersion = &schema.GroupVersion{Group: grafanav1alpha1.GroupVersion.Group, Version: grafanav1alpha1.GroupVersion.Version}
	crdConfig.APIPath = "/apis"
	crdConfig.NegotiatedSerializer = serializer.NewCodecFactory(scheme.Scheme)
	crdConfig.UserAgent = rest.DefaultKubernetesUserAgent()
	Clientset, _ := kubernetes.NewForConfig(config)


	data, err := Clientset.RESTClient().Post().
		Namespace(namespace).
		Resource("grafanadashboards").
		Body(v).
		DoRaw(context.TODO())
	log.Log.Info(string(data))
	log.Log.Info(err.Error())
	*/

	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}
	Clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}
	body, err := json.Marshal(v)
	if err != nil {
		panic(err.Error())
	}
	err = Clientset.RESTClient().
		Post().
		AbsPath("/apis/integreatly.org/v1alpha1/namespaces/" + namespace + "/grafanadashboards").
		Body(body).
		Do(ctx).
		Into(result)
	//klog.Infoln(result)
	if err != nil {
		klog.Infoln(err.Error())
	}
	return
}
