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
	"reflect"

	"github.com/go-logr/logr"
	"github.com/grafana-operator/grafana-operator/v4/controllers/grafana"
	"github.com/grafana-operator/grafana-operator/v4/controllers/grafanadashboard"
	"github.com/grafana-operator/grafana-operator/v4/controllers/model"
	v1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

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
	Scheme  *runtime.Scheme
}

// +kubebuilder:rbac:groups=*,resources=*,verbs=*

func (r *NamespaceReconciler) Reconcile(_ context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = context.Background()

	// your logic here
	log.Log.Info("Reconciling Namespace")
	namespace := &v1.Namespace{}

	if err := r.Get(context.TODO(), req.NamespacedName, namespace); err != nil {
		if errors.IsNotFound(err) {
			log.Log.Info("Namespace resource not found. Ignoring since object must be deleted.")
			return ctrl.Result{}, nil
		}
		log.Log.Error(err, "Failed to get Namespace")
		return ctrl.Result{}, err
	}
	s := namespace.Annotations["owner"]
	if s == "" {
		s = namespace.Annotations["creator"]
	}

	model.GrafanaKey = grafana.GetGrafanaKey()
	//	log.Log.Info(s)
	/*defer func() {
		s := recover()
		if s != nil {
			fmt.Println("Error !! : ", s)
		}
	}()*/
	if s != "" {
		log.Log.Info("user name is" + s)
		grafanadashboard.CreateUserDashboard(context.TODO(), namespace.GetName(), s)
	}

	return ctrl.Result{}, nil
}
func (r *NamespaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.Namespace{}).
		WithEventFilter(
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
		).
		Complete(r)
}
