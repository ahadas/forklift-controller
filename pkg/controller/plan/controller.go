/*
Copyright 2019 Red Hat Inc.

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

package plan

import (
	"context"
	"errors"
	libcnd "github.com/konveyor/controller/pkg/condition"
	liberr "github.com/konveyor/controller/pkg/error"
	"github.com/konveyor/controller/pkg/logging"
	libref "github.com/konveyor/controller/pkg/ref"
	api "github.com/konveyor/forklift-controller/pkg/apis/forklift/v1alpha1"
	"github.com/konveyor/forklift-controller/pkg/apis/forklift/v1alpha1/snapshot"
	plancontext "github.com/konveyor/forklift-controller/pkg/controller/plan/context"
	"github.com/konveyor/forklift-controller/pkg/controller/provider/web"
	"github.com/konveyor/forklift-controller/pkg/settings"
	k8serr "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"path"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
	"sort"
	"time"
)

const (
	// Controller name.
	Name = "plan"
	// Fast re-queue delay.
	FastReQ = time.Millisecond * 500
	// Slow re-queue delay.
	SlowReQ = time.Second * 3
)

//
// Package logger.
var log = logging.WithName(Name)

//
// Application settings.
var Settings = &settings.Settings

//
// Creates a new Plan Controller and adds it to the Manager.
func Add(mgr manager.Manager) error {
	reconciler := &Reconciler{
		EventRecorder: mgr.GetEventRecorderFor(Name),
		Client:        mgr.GetClient(),
		scheme:        mgr.GetScheme(),
	}
	cnt, err := controller.New(
		Name,
		mgr,
		controller.Options{
			Reconciler: reconciler,
		})
	if err != nil {
		log.Trace(err)
		return err
	}
	// Primary CR.
	err = cnt.Watch(
		&source.Kind{Type: &api.Plan{}},
		&handler.EnqueueRequestForObject{},
		&PlanPredicate{})
	if err != nil {
		log.Trace(err)
		return err
	}
	// References.
	err = cnt.Watch(
		&source.Kind{
			Type: &api.Provider{},
		},
		libref.Handler(),
		&ProviderPredicate{})
	if err != nil {
		log.Trace(err)
		return err
	}
	err = cnt.Watch(
		&source.Kind{
			Type: &api.Migration{},
		},
		&handler.EnqueueRequestsFromMapFunc{
			ToRequests: handler.ToRequestsFunc(RequestForMigration),
		},
		&MigrationPredicate{})
	if err != nil {
		log.Trace(err)
		return err
	}

	return nil
}

var _ reconcile.Reconciler = &Reconciler{}

//
// Reconciles a Plan object.
type Reconciler struct {
	record.EventRecorder
	client.Client
	scheme *runtime.Scheme
}

//
// Reconcile a Plan CR.
func (r *Reconciler) Reconcile(request reconcile.Request) (result reconcile.Result, err error) {
	fastReQ := reconcile.Result{RequeueAfter: FastReQ}
	slowReQ := reconcile.Result{RequeueAfter: SlowReQ}
	noReQ := reconcile.Result{}
	result = noReQ

	// Reset the logger.
	log.Reset()
	log.SetValues("plan", request)
	log.Info("Reconcile", "plan", request)

	defer func() {
		if err != nil {
			log.Trace(err)
			err = nil
		}
	}()

	// Fetch the CR.
	plan := &api.Plan{}
	err = r.Get(context.TODO(), request.NamespacedName, plan)
	if err != nil {
		if k8serr.IsNotFound(err) {
			err = nil
		}
		return
	}
	defer func() {
		log.Info("Conditions.", "all", plan.Status.Conditions)
	}()

	// Postpone as needed.
	postpone, err := r.postpone()
	if err != nil {
		log.Trace(err)
		return slowReQ, err
	}
	if postpone {
		log.Info("Postponed")
		return slowReQ, nil
	}

	// Begin staging conditions.
	plan.Status.BeginStagingConditions()

	// Validations.
	err = r.validate(plan)
	if err != nil {
		if errors.As(err, &web.ProviderNotReadyError{}) {
			result = slowReQ
			err = nil
		} else {
			result = fastReQ
		}
		return
	}

	// Ready condition.
	if !plan.Status.HasBlockerCondition() {
		plan.Status.SetCondition(libcnd.Condition{
			Type:     libcnd.Ready,
			Status:   True,
			Category: Required,
			Message:  "The migration plan is ready.",
		})
	}

	// End staging conditions.
	plan.Status.EndStagingConditions()

	// Record events.
	plan.Status.RecordEvents(plan, r)

	// Apply changes.
	plan.Status.ObservedGeneration = plan.Generation
	err = r.Status().Update(context.TODO(), plan)
	if err != nil {
		result = fastReQ
		return
	}

	//
	// Execute.
	// The plan is updated as needed to reflect status.
	reQ, err := r.execute(plan)
	if err != nil {
		result = fastReQ
		return
	}

	// Done
	if reQ > 0 {
		result = reconcile.Result{RequeueAfter: reQ}
	} else {
		result = noReQ
	}

	return
}

//
// Execute the plan.
func (r *Reconciler) execute(plan *api.Plan) (reQ time.Duration, err error) {
	if plan.Status.HasBlockerCondition() {
		return
	}
	list, err := r.pendingMigrations(plan)
	if err != nil {
		err = liberr.Wrap(err)
		return
	}
	var migration *api.Migration
	for _, migration = range list {
		if !migration.Status.MarkedStarted() {
			plan.Status.Migration.MarkReset()
			plan.Status.DeleteCondition(Succeeded, Failed)
		}
		break
	}
	if migration == nil {
		return
	}
	plan.Status.Migration.Active = migration.UID
	sn := snapshot.New(migration)
	if !sn.Contains("plan.UID") {
		sn.Set("plan.UID", plan.UID)
		sn.Set(api.SourceSnapshot, plan.Referenced.Provider.Source)
		sn.Set(api.DestinationSnapshot, plan.Referenced.Provider.Destination)
		sn.Set(api.MapSnapshot, plan.Spec.Map)
		err = r.Update(context.TODO(), migration)
		if err != nil {
			err = liberr.Wrap(err)
			return
		}
	}
	ctx, err := plancontext.New(r, plan, migration)
	if err != nil {
		err = liberr.Wrap(err)
		return
	}
	runner := Migration{Context: ctx}
	reQ, err = runner.Run()
	if err != nil {
		err = liberr.Wrap(err)
		return
	}
	err = r.Status().Update(context.TODO(), plan)
	if err != nil {
		err = liberr.Wrap(err)
	}
	if len(list) > 1 && reQ == 0 {
		reQ = FastReQ
	}

	return
}

//
// Sorted list of pending migrations.
func (r *Reconciler) pendingMigrations(plan *api.Plan) (list []*api.Migration, err error) {
	all := &api.MigrationList{}
	err = r.List(context.TODO(), all)
	if err != nil {
		err = liberr.Wrap(err)
		return
	}
	list = []*api.Migration{}
	for i := range all.Items {
		migration := &all.Items[i]
		if !migration.Match(plan) {
			continue
		}
		if migration.Status.MarkedCompleted() {
			continue
		}
		list = append(list, migration)
	}
	sort.Slice(
		list,
		func(i, j int) bool {
			mA := list[i].ObjectMeta
			mB := list[j].ObjectMeta
			tA := mA.CreationTimestamp
			tB := mB.CreationTimestamp
			if !tA.Equal(&tB) {
				return tA.Before(&tB)
			}
			nA := path.Join(mA.Namespace, mA.Name)
			nB := path.Join(mB.Namespace, mB.Name)
			return nA < nB
		})

	return
}

//
// Postpone reconciliation.
// Ensure that dependencies (CRs) have been reconciled.
func (r *Reconciler) postpone() (postpone bool, err error) {
	providerList := &api.ProviderList{}
	err = r.List(context.TODO(), providerList)
	if err != nil {
		err = liberr.Wrap(err)
		return
	}
	for _, provider := range providerList.Items {
		if provider.Status.ObservedGeneration < provider.Generation {
			postpone = true
			return
		}
	}
	hostList := &api.HostList{}
	err = r.List(context.TODO(), hostList)
	if err != nil {
		err = liberr.Wrap(err)
		return
	}
	for _, host := range hostList.Items {
		if host.Status.ObservedGeneration < host.Generation {
			postpone = true
			return
		}
	}

	return
}
