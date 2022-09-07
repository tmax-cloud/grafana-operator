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

package grafanadatasource

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/go-logr/logr"
	grafanav1alpha1 "github.com/grafana-operator/grafana-operator/v4/api/integreatly/v1alpha1"
	"github.com/grafana-operator/grafana-operator/v4/controllers/common"
	"github.com/grafana-operator/grafana-operator/v4/controllers/config"
	"github.com/grafana-operator/grafana-operator/v4/controllers/constants"
	v1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/record"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	integreatlyorgv1alpha1 "github.com/grafana-operator/grafana-operator/v4/api/integreatly/v1alpha1"
)

// GrafanaDatasourceReconciler reconciles a GrafanaDatasource object
type GrafanaDatasourceReconciler struct {
	// This Client, initialized using mgr.Client() above, is a split Client
	// that reads objects from the cache and writes to the apiserver
	Client            client.Client
	Scheme            *runtime.Scheme
	Context           context.Context
	Cancel            context.CancelFunc
	GrafanaClientImpl *GrafanaClient
	config            *config.ControllerConfig
	Recorder          record.EventRecorder
	transport         *http.Transport
	state             common.ControllerState
	Logger            logr.Logger
}

const (
	DatasourcesApiVersion = 1
	ControllerName        = "controller_grafanadatasource"
)

var log = logf.Log.WithName(ControllerName)

var _ reconcile.Reconciler = &GrafanaDatasourceReconciler{}

// +kubebuilder:rbac:groups=integreatly.org,resources=grafanadatasources,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=integreatly.org,resources=grafanadatasources/status,verbs=get;update;patch

// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.7.0/pkg/reconcile
func (r *GrafanaDatasourceReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	log = r.Logger.WithValues(ControllerName, request.NamespacedName)
	log.V(1).Info("Start datasource reconcile")
	// If Grafana is not running there is no need to continue
	/*if !r.state.GrafanaReady {
		log.V(1).Info("no grafana instance available")
		return reconcile.Result{Requeue: false}, nil
	}*/

	getClient, err := r.getClient()
	if err != nil {
		return reconcile.Result{RequeueAfter: config.RequeueDelay}, err
	}

	// Initial request?
	if request.Name == "" {
		return r.reconcileDataSources(request, getClient)
	}

	// Check if the label selectors are available yet. If not then the grafana controller
	// has not finished initializing and we can't continue. Reschedule for later.

	// Fetch the GrafanaDashboard instance
	instance := &grafanav1alpha1.GrafanaDataSource{}
	err = r.Client.Get(r.Context, request.NamespacedName, instance)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			// If some dashboard has been deleted, then always re sync the world
			log.V(1).Info("deleting datasource", "namespace", request.Namespace, "name", request.Name)
			return r.reconcileDataSources(request, getClient)
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	// If the dashboard does not match the label selectors then we ignore it
	/*cr := instance.DeepCopy()
	if !r.isMatch(cr) {
		log.V(1).Info(fmt.Sprintf("dashboard %v/%v found but selectors do not match",
			cr.Namespace, cr.Name))
		return ctrl.Result{}, nil
	}*/
	// Otherwise always re sync all dashboards in the namespace
	return r.reconcileDataSources(request, getClient)
}

func (r *GrafanaDatasourceReconciler) reconcileDataSources(request reconcile.Request, grafanaClient GrafanaClient) (reconcile.Result, error) { // nolint
	// Collect known and namespace dashboards
	knownDatasources, _ := grafanaClient.GetDatasourceList()
	namespaceDatasources := &grafanav1alpha1.GrafanaDataSourceList{}

	opts := &client.ListOptions{
		Namespace: request.Namespace,
	}

	err := r.Client.List(r.Context, namespaceDatasources, opts)
	if err != nil {
		return reconcile.Result{}, err
	}

	// Prepare lists
	var datasourcesToDelete []*grafanav1alpha1.GrafanaDataSource

	// Dashboards to delete: dashboards that are known but not found
	// any longer in the namespace
	var ds_url []string
	//log.V(1).Info("known datasources : ", *knownDatasources[0].Name)
	for i, kds := range knownDatasources {
		ds_url = append(ds_url, *kds.URL)
		log.V(1).Info(ds_url[i], " ", *kds.URL)
	}

	// Process new/updated dashboards
	for i, datasource := range namespaceDatasources.Items {
		// Is this a dashboard we care about (matches the label selectors)?
		//log.Log.V(1).Info(namespaceDashboards.Items[i].ObjectMeta.GetAnnotations()["userId"])

		// Process the dashboard. Use the known hash of an existing dashboard
		// to determine if an update is required
		//{
		//r.manageError(&namespaceDatasources.Items[i], err)
		//} else
		if !checkdup(ds_url, &namespaceDatasources.Items[i]) {
			datasourcesToDelete = append(datasourcesToDelete, &namespaceDatasources.Items[i])
			log.V(1).Info("datasource to delete : ", datasource.Spec.Datasources[0].Name)
		}

		// Check known dashboards exist on grafana instance and recreate if not

		// Check labels only when DashboardNamespaceSelector isnt empty
		_, err := grafanaClient.CreateGrafanaDatasource(datasource)

		if err != nil {
			log.V(4).Error(err, "failed to create datasource")
			r.manageError(&namespaceDatasources.Items[i], err)
			continue
		}

		r.manageSuccess(&namespaceDatasources.Items[i])
	}

	for _, dashboard := range datasourcesToDelete {
		_, err := grafanaClient.DeleteDatasourceByUID(dashboard.Spec.Datasources[0].Uid)
		if err != nil {
			log.V(4).Error(err, "error deleting dashboard, status was",
				"dashboardUID", dashboard.Spec.Datasources[0].Uid)
		}

		log.V(1).Info(fmt.Sprintf("delete Datasource success"))

		// Mark the dashboards as synced so that the current state can be written
		// to the Grafana CR by the grafana controller

		// Refresh the list of known datasource after the dashboard has been removed
		//knownDatasources = r.config.GetDatasources(request.Namespace)

	}

	return reconcile.Result{Requeue: false}, nil
}

func checkdup(knownDatasources []string, item *grafanav1alpha1.GrafanaDataSource) bool {
	for _, d := range knownDatasources {
		if item.Spec.Datasources[0].Url == d {
			return true
		}
	}
	return false
}

/*
func (r *GrafanaDatasourceReconciler) reconcileDataSources(state *GrafanaClientImpl, grafanaClient GrafanaClientImpl) error {
	var dataSourcesToAddOrUpdate []grafanav1alpha1.GrafanaDataSource
	var dataSourcesToDelete []string

	// check if a given datasource (by its key) is found on the cluster
	foundOnCluster := func(key string) bool {
		for _, ds := range state.ClusterDataSources.Items {
			if key == ds.Spec.Datasources[0].Url {
				return true
			}
		}
		return false
	}

	// Data sources to add or update: we always update the config map and let
	// Kubernetes figure out if any changes have to be applied
	dataSourcesToAddOrUpdate = append(dataSourcesToAddOrUpdate, state.ClusterDataSources.Items...)

	// Data sources to delete: if a datasourcedashboard is in the configmap but cannot
	// be found on the cluster then we assume it has been deleted and remove
	// it from the configmap
	for ds := range state.KnownDataSources.Items {
		if !foundOnCluster(state.KnownDataSources.Items[ds].Spec.Datasources[0].Url) {
			dataSourcesToDelete = append(dataSourcesToDelete, string(state.KnownDataSources.Items[ds].UID))
		}
	}

	// apply dataSourcesToDelete
	for _, ds := range dataSourcesToDelete {
		log.V(1).Info("deleting datasource", "datasource", ds)
		if ds != "" {
			//delete(state.KnownDataSources.Data, ds)
			grafanaClient.DeleteDatasourceByUID(ds)
		}
	}

	// apply dataSourcesToAddOrUpdate
	var updated []grafanav1alpha1.GrafanaDataSource // nolint
	for i := range dataSourcesToAddOrUpdate {
		/*	pipeline := NewDatasourcePipeline(&dataSourcesToAddOrUpdate[i])
			err := pipeline.ProcessDatasource(state.KnownDataSources.)
			if err != nil {
				r.manageError(&dataSourcesToAddOrUpdate[i], err)
				continue
			}
		updated = append(updated, dataSourcesToAddOrUpdate[i])
	}

	// update the hash of the newly reconciled datasources
	/*hash, err := r.updateHash(state.KnownDataSources)
	if err != nil {
		r.manageError(nil, err)
		return err
	}
	for ds := range state.KnownDataSources.Items {
		if state.KnownDataSources.Items[ds].Annotations == nil {
			state.KnownDataSources.Items[ds].Annotations = map[string]string{}
		}
	}

	// Compare the last hash to the previous one, update if changed
	/*lastHash := state.KnownDataSources.Annotations[constants.LastConfigAnnotation]
	if lastHash != hash {
		state.KnownDataSources.Annotations[constants.LastConfigAnnotation] = hash

		// finally, update the configmap
		err = r.Client.Update(r.Context, state.KnownDataSources)
		if err != nil {
			r.Recorder.Event(state.KnownDataSources, "Warning", "UpdateError", err.Error())
		} else {
			r.manageSuccess(updated)
		}
	}
	var proof_ds []grafanav1alpha1.GrafanaDataSource
	for ds := range updated {
		_, err2 := state.CreateGrafanaDatasource(updated[ds])

		if err2 != nil {
			r.manageError(nil, err2)
		} else {
			proof_ds = append(proof_ds, updated[ds])
		}

	}

	return nil
}*/

func (i *GrafanaDatasourceReconciler) updateHash(known *v1.ConfigMap) (string, error) {
	if known == nil || known.Data == nil {
		return "", nil
	}

	// Make sure that we always use the same order when creating the hash
	keys := make([]string, 0, len(known.Data))

	for key := range known.Data {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	hash := sha256.New()
	for _, key := range keys {
		_, err := io.WriteString(hash, key)
		if err != nil {
			return "", err
		}

		_, err = io.WriteString(hash, known.Data[key])
		if err != nil {
			return "", err
		}
	}

	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

// Handle error case: update datasource with error message and status
func (r *GrafanaDatasourceReconciler) manageError(datasource *grafanav1alpha1.GrafanaDataSource, issue error) {
	r.Recorder.Event(datasource, "Warning", "ProcessingError", issue.Error())

	// datasource deleted
	if datasource == nil {
		return
	}

	datasource.Status.Phase = grafanav1alpha1.PhaseFailing
	datasource.Status.Message = issue.Error()

	err := r.Client.Status().Update(r.Context, datasource)
	if err != nil {
		// Ignore conclicts. Resource might just be outdated.
		if k8serrors.IsConflict(err) {
			return
		}
		log.V(4).Error(err, "error updating datasource status")
	}
}

// manage success case: datasource has been imported successfully and the configmap
// is updated
func (r *GrafanaDatasourceReconciler) manageSuccess(datasource *grafanav1alpha1.GrafanaDataSource) {

	log.V(1).Info("datasource successfully imported",
		"datasource.Namespace", datasource.Namespace,
		"datasource.Name", datasource.Name)

	datasource.Status.Phase = grafanav1alpha1.PhaseReconciling
	datasource.Status.Message = "success"

}

// SetupWithManager sets up the controller with the Manager.
/*
func SetupWithManager(mgr ctrl.Manager, r reconcile.Reconciler, namespace string) error {
	c, err := controller.New("grafanadatasource-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource GrafanaDashboard
	err = c.Watch(&source.Kind{Type: &grafanav1alpha1.GrafanaDataSource{}}, &handler.EnqueueRequestForObject{})
	if err == nil {
		log.V(1).Info("Starting datasource controller")
	}

	ref := r.(*GrafanaDatasourceReconciler) // nolint
	ticker := time.NewTicker(config.RequeueDelay)
	sendEmptyRequest := func() {
		request := reconcile.Request{
			NamespacedName: types.NamespacedName{
				Namespace: namespace,
				Name:      "",
			},
		}
		_, err = r.Reconcile(ref.Context, request)
		if err != nil {
			return
		}
	}

	go func() {
		for range ticker.C {
			log.V(1).Info("running periodic dashboard resync")
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
}*/

func (r *GrafanaDatasourceReconciler) getClient() (GrafanaClient, error) {

	url := r.state.AdminUrl
	if url == "" {
		return nil, errors.New("cannot get grafana admin url")
	}
	log.V(1).Info("url" + url)
	username := os.Getenv(constants.GrafanaAdminUserEnvVar)
	if username == "" {
		return nil, errors.New("invalid credentials (username)")
	}
	log.V(1).Info(username)
	password := os.Getenv(constants.GrafanaAdminPasswordEnvVar)
	if password == "" {
		return nil, errors.New("invalid credentials (password)")
	}
	log.V(1).Info(password)
	duration := time.Duration(r.state.ClientTimeout)
	r.transport = &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	}
	return NewGrafanaClient(url, username, password, r.transport, duration), nil
}

/*func Add(mgr manager.Manager, namespace string) error {
	return SetupWithManager(mgr, newReconciler(mgr), namespace)
}*/
func (r *GrafanaDatasourceReconciler) SetupWithManager(mgr ctrl.Manager) error {

	go func() {
		for stateChange := range common.ControllerEvents {
			// Controller state updated
			r.state = stateChange
		}
	}()
	return ctrl.NewControllerManagedBy(mgr).
		For(&integreatlyorgv1alpha1.GrafanaDataSource{}).
		Owns(&integreatlyorgv1alpha1.GrafanaDataSource{}).
		Complete(r)
}

/*
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)

	return &GrafanaDatasourceReconciler{
		Client: mgr.GetClient(),
		transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		},
		Logger:   mgr.GetLogger(),
		config:   config.GetControllerConfig(),
		Context:  ctx,
		Cancel:   cancel,
		Recorder: mgr.GetEventRecorderFor(ControllerName),
		State:    common.ControllerState{},
	}
}
*/
