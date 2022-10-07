/*


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

package controllers

import (
	"context"

	"github.com/go-logr/logr"
	"github.com/grafana-operator/grafana-operator/v4/controllers/common"
	"github.com/grafana-operator/grafana-operator/v4/controllers/config"
	"github.com/grafana-operator/grafana-operator/v4/controllers/grafana"
	"github.com/grafana-operator/grafana-operator/v4/controllers/grafanadashboard"
	"github.com/grafana-operator/grafana-operator/v4/controllers/model"
	v1 "k8s.io/api/core/v1"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// NamespaceReconciler reconciles a Namespace object
type NamespaceReconciler struct {
	client.Client
	Logger  logr.Logger
	Context context.Context
	config  *config.ControllerConfig
	state   common.ControllerState
	Scheme  *runtime.Scheme
}

const (
	ControllerName = "controller_namespace"
)

// +kubebuilder:rbac:groups=*,resources=*,verbs=*

func (r *NamespaceReconciler) Reconcile(_ context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := r.Logger.WithValues(ControllerName, req.NamespacedName)
	_ = context.Background()

	// your logic here
	logger.V(4).Info("Reconciling Namespace")

	/*if !r.state.GrafanaReady {
		logger.V(1).Info("no grafana instance available")
		return reconcile.Result{Requeue: false}, nil
	}*/
	namespace := &v1.Namespace{}

	if err := r.Client.Get(context.TODO(), req.NamespacedName, namespace); err != nil {
		if errors.IsNotFound(err) {
			logger.V(4).Info("Namespace resource not found. Ignoring since object must be deleted.")
			return ctrl.Result{}, nil
		}
		logger.V(1).Error(err, "Failed to get Namespace")
		return ctrl.Result{}, err
	}
	ns_owner := namespace.Annotations["owner"]
	if ns_owner == "" {
		ns_owner = namespace.Annotations["creator"]
		if ns_owner == "" {
			ns_owner = "kubernetes-admin"
		}
	}

	model.GrafanaKey = grafana.GetGrafanaKey()
	//	klog.V(4).Info(s)
	/*defer func() {
		s := recover()
		if s != nil {
			fmt.Println("Error !! : ", s)
		}
	}()*/
	if ns_owner != "" {
		logger.V(1).Info("user name is" + ns_owner)
		grafanadashboard.CreateUserDashboard(context.TODO(), namespace.GetName(), ns_owner)
	}

	return ctrl.Result{}, nil
}
func (r *NamespaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.Namespace{}).
		/*WithEventFilter(
			predicate.Funcs{
				// Only reconciling if the status.status change
				UpdateFunc: func(e event.UpdateEvent) bool {
					oldNsStatus := e.ObjectOld.(*v1.Namespace).DeepCopy().Status.Phase
					newNsStatus := e.ObjectNew.(*v1.Namespace).DeepCopy().Status.Phase

					oldNsLabels := e.ObjectOld.(*v1.Namespace).DeepCopy().Labels
					newNsLabels := e.ObjectNew.(*v1.Namespace).DeepCopy().Labels

					if !reflect.DeepEqual(oldNsStatus, newNsStatus) || !reflect.DeepEqual(oldNsLabels, newNsLabels) {
						return true
					} else {
						return false
					}
				},
			},
		).*/
		Complete(r)
}
