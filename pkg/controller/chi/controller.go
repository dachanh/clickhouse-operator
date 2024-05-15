// Copyright 2019 Altinity Ltd and/or its affiliates. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package chi

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/altinity/clickhouse-operator/pkg/metrics/clickhouse"
	"github.com/altinity/clickhouse-operator/pkg/model/chi/tags"
	"time"

	"github.com/sanity-io/litter"
	"gopkg.in/d4l3k/messagediff.v1"

	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	apiExtensions "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilRuntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeInformers "k8s.io/client-go/informers"
	kube "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedCore "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"

	"github.com/altinity/queue"

	log "github.com/altinity/clickhouse-operator/pkg/announcer"
	api "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	"github.com/altinity/clickhouse-operator/pkg/apis/metrics"
	"github.com/altinity/clickhouse-operator/pkg/chop"
	chopClientSet "github.com/altinity/clickhouse-operator/pkg/client/clientset/versioned"
	chopClientSetScheme "github.com/altinity/clickhouse-operator/pkg/client/clientset/versioned/scheme"
	chopInformers "github.com/altinity/clickhouse-operator/pkg/client/informers/externalversions"
	"github.com/altinity/clickhouse-operator/pkg/controller"
	model "github.com/altinity/clickhouse-operator/pkg/model/chi"
	"github.com/altinity/clickhouse-operator/pkg/util"
)

// NewController creates instance of Controller
func NewController(
	chopClient chopClientSet.Interface,
	extClient apiExtensions.Interface,
	kubeClient kube.Interface,
	chopInformerFactory chopInformers.SharedInformerFactory,
	kubeInformerFactory kubeInformers.SharedInformerFactory,
) *Controller {

	// Initializations
	_ = chopClientSetScheme.AddToScheme(scheme.Scheme)

	// Setup events
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(log.Info)
	eventBroadcaster.StartRecordingToSink(
		&typedCore.EventSinkImpl{
			Interface: kubeClient.CoreV1().Events(""),
		},
	)
	recorder := eventBroadcaster.NewRecorder(
		scheme.Scheme,
		core.EventSource{
			Component: componentName,
		},
	)

	// Create Controller instance
	controller := &Controller{
		kubeClient:              kubeClient,
		extClient:               extClient,
		chopClient:              chopClient,
		chiLister:               chopInformerFactory.Clickhouse().V1().ClickHouseInstallations().Lister(),
		chiListerSynced:         chopInformerFactory.Clickhouse().V1().ClickHouseInstallations().Informer().HasSynced,
		chitLister:              chopInformerFactory.Clickhouse().V1().ClickHouseInstallationTemplates().Lister(),
		chitListerSynced:        chopInformerFactory.Clickhouse().V1().ClickHouseInstallationTemplates().Informer().HasSynced,
		serviceLister:           kubeInformerFactory.Core().V1().Services().Lister(),
		serviceListerSynced:     kubeInformerFactory.Core().V1().Services().Informer().HasSynced,
		endpointsLister:         kubeInformerFactory.Core().V1().Endpoints().Lister(),
		endpointsListerSynced:   kubeInformerFactory.Core().V1().Endpoints().Informer().HasSynced,
		configMapLister:         kubeInformerFactory.Core().V1().ConfigMaps().Lister(),
		configMapListerSynced:   kubeInformerFactory.Core().V1().ConfigMaps().Informer().HasSynced,
		statefulSetLister:       kubeInformerFactory.Apps().V1().StatefulSets().Lister(),
		statefulSetListerSynced: kubeInformerFactory.Apps().V1().StatefulSets().Informer().HasSynced,
		podLister:               kubeInformerFactory.Core().V1().Pods().Lister(),
		podListerSynced:         kubeInformerFactory.Core().V1().Pods().Informer().HasSynced,
		recorder:                recorder,
	}
	controller.initQueues()
	controller.addEventHandlers(chopInformerFactory, kubeInformerFactory)

	return controller
}

// initQueues
func (c *Controller) initQueues() {
	queuesNum := chop.Config().Reconcile.Runtime.ReconcileCHIsThreadsNumber + api.DefaultReconcileSystemThreadsNumber
	for i := 0; i < queuesNum; i++ {
		c.queues = append(
			c.queues,
			queue.New(),
			//workqueue.NewNamedRateLimitingQueue(
			//	workqueue.DefaultControllerRateLimiter(),
			//	fmt.Sprintf("chi%d", i),
			//),
		)
	}
}

func (c *Controller) addEventHandlersCHI(
	chopInformerFactory chopInformers.SharedInformerFactory,
) {
	chopInformerFactory.Clickhouse().V1().ClickHouseInstallations().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			chi := obj.(*api.ClickHouseInstallation)
			if !chop.Config().IsWatchedNamespace(chi.Namespace) {
				return
			}
			log.V(3).M(chi).Info("chiInformer.AddFunc")
			c.enqueueObject(NewReconcileCHI(reconcileAdd, nil, chi))
		},
		UpdateFunc: func(old, new interface{}) {
			oldChi := old.(*api.ClickHouseInstallation)
			newChi := new.(*api.ClickHouseInstallation)
			if !chop.Config().IsWatchedNamespace(newChi.Namespace) {
				return
			}
			log.V(3).M(newChi).Info("chiInformer.UpdateFunc")
			c.enqueueObject(NewReconcileCHI(reconcileUpdate, oldChi, newChi))
		},
		DeleteFunc: func(obj interface{}) {
			chi := obj.(*api.ClickHouseInstallation)
			if !chop.Config().IsWatchedNamespace(chi.Namespace) {
				return
			}
			log.V(3).M(chi).Info("chiInformer.DeleteFunc")
			c.enqueueObject(NewReconcileCHI(reconcileDelete, chi, nil))
		},
	})
}

func (c *Controller) addEventHandlersCHIT(
	chopInformerFactory chopInformers.SharedInformerFactory,
) {
	chopInformerFactory.Clickhouse().V1().ClickHouseInstallationTemplates().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			chit := obj.(*api.ClickHouseInstallationTemplate)
			if !chop.Config().IsWatchedNamespace(chit.Namespace) {
				return
			}
			log.V(3).M(chit).Info("chitInformer.AddFunc")
			c.enqueueObject(NewReconcileCHIT(reconcileAdd, nil, chit))
		},
		UpdateFunc: func(old, new interface{}) {
			oldChit := old.(*api.ClickHouseInstallationTemplate)
			newChit := new.(*api.ClickHouseInstallationTemplate)
			if !chop.Config().IsWatchedNamespace(newChit.Namespace) {
				return
			}
			log.V(3).M(newChit).Info("chitInformer.UpdateFunc")
			c.enqueueObject(NewReconcileCHIT(reconcileUpdate, oldChit, newChit))
		},
		DeleteFunc: func(obj interface{}) {
			chit := obj.(*api.ClickHouseInstallationTemplate)
			if !chop.Config().IsWatchedNamespace(chit.Namespace) {
				return
			}
			log.V(3).M(chit).Info("chitInformer.DeleteFunc")
			c.enqueueObject(NewReconcileCHIT(reconcileDelete, chit, nil))
		},
	})
}

func (c *Controller) addEventHandlersChopConfig(
	chopInformerFactory chopInformers.SharedInformerFactory,
) {
	chopInformerFactory.Clickhouse().V1().ClickHouseOperatorConfigurations().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			chopConfig := obj.(*api.ClickHouseOperatorConfiguration)
			if !chop.Config().IsWatchedNamespace(chopConfig.Namespace) {
				return
			}
			log.V(3).M(chopConfig).Info("chopInformer.AddFunc")
			c.enqueueObject(NewReconcileChopConfig(reconcileAdd, nil, chopConfig))
		},
		UpdateFunc: func(old, new interface{}) {
			newChopConfig := new.(*api.ClickHouseOperatorConfiguration)
			oldChopConfig := old.(*api.ClickHouseOperatorConfiguration)
			if !chop.Config().IsWatchedNamespace(newChopConfig.Namespace) {
				return
			}
			log.V(3).M(newChopConfig).Info("chopInformer.UpdateFunc")
			c.enqueueObject(NewReconcileChopConfig(reconcileUpdate, oldChopConfig, newChopConfig))
		},
		DeleteFunc: func(obj interface{}) {
			chopConfig := obj.(*api.ClickHouseOperatorConfiguration)
			if !chop.Config().IsWatchedNamespace(chopConfig.Namespace) {
				return
			}
			log.V(3).M(chopConfig).Info("chopInformer.DeleteFunc")
			c.enqueueObject(NewReconcileChopConfig(reconcileDelete, chopConfig, nil))
		},
	})
}

func (c *Controller) addEventHandlersService(
	kubeInformerFactory kubeInformers.SharedInformerFactory,
) {
	kubeInformerFactory.Core().V1().Services().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			service := obj.(*core.Service)
			if !c.isTrackedObject(service.GetObjectMeta()) {
				return
			}
			log.V(3).M(service).Info("serviceInformer.AddFunc")
		},
		UpdateFunc: func(old, new interface{}) {
			oldService := old.(*core.Service)
			if !c.isTrackedObject(&oldService.ObjectMeta) {
				return
			}
			log.V(3).M(oldService).Info("serviceInformer.UpdateFunc")
		},
		DeleteFunc: func(obj interface{}) {
			service := obj.(*core.Service)
			if !c.isTrackedObject(&service.ObjectMeta) {
				return
			}
			log.V(3).M(service).Info("serviceInformer.DeleteFunc")
		},
	})
}

func normalizeEndpoints(e *core.Endpoints) *core.Endpoints {
	if e == nil {
		e = &core.Endpoints{}
	}
	if len(e.Subsets) == 0 {
		e.Subsets = []core.EndpointSubset{
			{},
		}
	}
	if len(e.Subsets[0].Addresses) == 0 {
		e.Subsets[0].Addresses = []core.EndpointAddress{
			{},
		}
	}
	e.Subsets[0].Addresses[0].TargetRef = nil
	return e
}

func checkIP(path *messagediff.Path, iValue interface{}) bool {
	for _, pathNode := range *path {
		// .String() function adds "." in front of the pathNode
		// So it would be ".IP" for pathNode "IPs"
		s := pathNode.String()
		if s == ".IP" {
			if typed, ok := iValue.(string); ok {
				if typed != "" {
					// Have IP address assigned|modified
					return true
				}
			}
		}
	}
	return false
}

func updated(old, new *core.Endpoints) bool {
	oldSubsets := normalizeEndpoints(old).Subsets
	newSubsets := normalizeEndpoints(new).Subsets

	diff, equal := messagediff.DeepDiff(oldSubsets[0].Addresses, newSubsets[0].Addresses)
	if equal {
		log.V(3).M(old).Info("endpointsInformer.UpdateFunc: no changes found")
		// No need to react
		return false
	}

	assigned := false
	for path, iValue := range diff.Added {
		log.V(3).M(old).Info("endpointsInformer.UpdateFunc: added %v", path)
		if address, ok := iValue.(core.EndpointAddress); ok {
			if address.IP != "" {
				assigned = true
			}
		}
	}
	for path := range diff.Removed {
		log.V(3).M(old).Info("endpointsInformer.UpdateFunc: removed %v", path)
	}
	for path, iValue := range diff.Modified {
		log.V(3).M(old).Info("endpointsInformer.UpdateFunc: modified %v", path)
		assigned = assigned || checkIP(path, iValue)
	}

	if assigned {
		log.V(2).M(old).Info("endpointsInformer.UpdateFunc: IP ASSIGNED: %s", litter.Sdump(new.Subsets))
		return true
	}

	return false
}

func (c *Controller) addEventHandlersEndpoint(
	kubeInformerFactory kubeInformers.SharedInformerFactory,
) {
	kubeInformerFactory.Core().V1().Endpoints().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			endpoints := obj.(*core.Endpoints)
			if !c.isTrackedObject(&endpoints.ObjectMeta) {
				return
			}
			log.V(3).M(endpoints).Info("endpointsInformer.AddFunc")
		},
		UpdateFunc: func(old, new interface{}) {
			oldEndpoints := old.(*core.Endpoints)
			newEndpoints := new.(*core.Endpoints)
			if !c.isTrackedObject(&oldEndpoints.ObjectMeta) {
				return
			}
			log.V(3).M(newEndpoints).Info("endpointsInformer.UpdateFunc")
			if updated(oldEndpoints, newEndpoints) {
				c.enqueueObject(NewReconcileEndpoints(reconcileUpdate, oldEndpoints, newEndpoints))
				c.enqueueObject(NewDropDns(&newEndpoints.ObjectMeta))
			}
		},
		DeleteFunc: func(obj interface{}) {
			endpoints := obj.(*core.Endpoints)
			if !c.isTrackedObject(&endpoints.ObjectMeta) {
				return
			}
			log.V(3).M(endpoints).Info("endpointsInformer.DeleteFunc")
		},
	})
}

func (c *Controller) addEventHandlersConfigMap(
	kubeInformerFactory kubeInformers.SharedInformerFactory,
) {
	kubeInformerFactory.Core().V1().ConfigMaps().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			configMap := obj.(*core.ConfigMap)
			if !c.isTrackedObject(&configMap.ObjectMeta) {
				return
			}
			log.V(3).M(configMap).Info("configMapInformer.AddFunc")
		},
		UpdateFunc: func(old, new interface{}) {
			configMap := old.(*core.ConfigMap)
			if !c.isTrackedObject(&configMap.ObjectMeta) {
				return
			}
			log.V(3).M(configMap).Info("configMapInformer.UpdateFunc")
		},
		DeleteFunc: func(obj interface{}) {
			configMap := obj.(*core.ConfigMap)
			if !c.isTrackedObject(&configMap.ObjectMeta) {
				return
			}
			log.V(3).M(configMap).Info("configMapInformer.DeleteFunc")
		},
	})
}

func (c *Controller) addEventHandlersStatefulSet(
	kubeInformerFactory kubeInformers.SharedInformerFactory,
) {
	kubeInformerFactory.Apps().V1().StatefulSets().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			statefulSet := obj.(*apps.StatefulSet)
			if !c.isTrackedObject(&statefulSet.ObjectMeta) {
				return
			}
			log.V(3).M(statefulSet).Info("statefulSetInformer.AddFunc")
			//controller.handleObject(obj)
		},
		UpdateFunc: func(old, new interface{}) {
			statefulSet := old.(*apps.StatefulSet)
			if !c.isTrackedObject(&statefulSet.ObjectMeta) {
				return
			}
			log.V(3).M(statefulSet).Info("statefulSetInformer.UpdateFunc")
		},
		DeleteFunc: func(obj interface{}) {
			statefulSet := obj.(*apps.StatefulSet)
			if !c.isTrackedObject(&statefulSet.ObjectMeta) {
				return
			}
			log.V(3).M(statefulSet).Info("statefulSetInformer.DeleteFunc")
			//controller.handleObject(obj)
		},
	})
}

func (c *Controller) addEventHandlersPod(
	kubeInformerFactory kubeInformers.SharedInformerFactory,
) {
	kubeInformerFactory.Core().V1().Pods().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod := obj.(*core.Pod)
			if !c.isTrackedObject(&pod.ObjectMeta) {
				return
			}
			log.V(3).M(pod).Info("podInformer.AddFunc")
			c.enqueueObject(NewReconcilePod(reconcileAdd, nil, pod))
		},
		UpdateFunc: func(old, new interface{}) {
			oldPod := old.(*core.Pod)
			newPod := new.(*core.Pod)
			if !c.isTrackedObject(&newPod.ObjectMeta) {
				return
			}
			log.V(3).M(newPod).Info("podInformer.UpdateFunc")
			c.enqueueObject(NewReconcilePod(reconcileUpdate, oldPod, newPod))
		},
		DeleteFunc: func(obj interface{}) {
			pod := obj.(*core.Pod)
			if !c.isTrackedObject(&pod.ObjectMeta) {
				return
			}
			log.V(3).M(pod).Info("podInformer.DeleteFunc")
			c.enqueueObject(NewReconcilePod(reconcileDelete, pod, nil))
		},
	})
}

// addEventHandlers
func (c *Controller) addEventHandlers(
	chopInformerFactory chopInformers.SharedInformerFactory,
	kubeInformerFactory kubeInformers.SharedInformerFactory,
) {
	c.addEventHandlersCHI(chopInformerFactory)
	c.addEventHandlersCHIT(chopInformerFactory)
	c.addEventHandlersChopConfig(chopInformerFactory)
	c.addEventHandlersService(kubeInformerFactory)
	c.addEventHandlersEndpoint(kubeInformerFactory)
	c.addEventHandlersConfigMap(kubeInformerFactory)
	c.addEventHandlersStatefulSet(kubeInformerFactory)
	c.addEventHandlersPod(kubeInformerFactory)
}

// isTrackedObject checks whether operator is interested in changes of this object
func (c *Controller) isTrackedObject(meta meta.Object) bool {
	return chop.Config().IsWatchedNamespace(meta.GetNamespace()) && tags.IsCHOPGeneratedObject(meta)
}

// Run syncs caches, starts workers
func (c *Controller) Run(ctx context.Context) {
	defer utilRuntime.HandleCrash()
	defer func() {
		for i := range c.queues {
			//c.queues[i].ShutDown()
			c.queues[i].Close()
		}
	}()

	log.V(1).Info("Starting ClickHouseInstallation controller")
	if !waitForCacheSync(
		ctx,
		"ClickHouseInstallation",
		c.chiListerSynced,
		c.statefulSetListerSynced,
		c.configMapListerSynced,
		c.serviceListerSynced,
	) {
		// Unable to sync
		return
	}

	// Label controller runtime objects with proper labels
	max := 10
	for cnt := 0; cnt < max; cnt++ {
		switch err := c.labelMyObjectsTree(ctx); err {
		case nil:
			cnt = max
		case ErrOperatorPodNotSpecified:
			log.V(1).F().Error("Since operator pod is not specified, will not perform labeling")
			cnt = max
		default:
			log.V(1).F().Error("ERROR label objects, will retry. Err: %v", err)
			util.WaitContextDoneOrTimeout(ctx, 5*time.Second)
		}
	}

	//
	// Start threads
	//
	workersNum := len(c.queues)
	log.V(1).F().Info("ClickHouseInstallation controller: starting workers number: %d", workersNum)
	for i := 0; i < workersNum; i++ {
		log.V(1).F().Info("ClickHouseInstallation controller: starting worker %d out of %d", i+1, workersNum)
		sys := false
		if i < api.DefaultReconcileSystemThreadsNumber {
			sys = true
		}
		worker := c.newWorker(c.queues[i], sys)
		go wait.Until(worker.run, runWorkerPeriod, ctx.Done())
	}
	defer log.V(1).F().Info("ClickHouseInstallation controller: shutting down workers")

	log.V(1).F().Info("ClickHouseInstallation controller: workers started")
	<-ctx.Done()
}

func prepareCHIAdd(command *ReconcileCHI) bool {
	newjs, _ := json.Marshal(command.new)
	newchi := api.ClickHouseInstallation{
		TypeMeta: meta.TypeMeta{
			APIVersion: api.SchemeGroupVersion.String(),
			Kind:       api.ClickHouseInstallationCRDResourceKind,
		},
	}
	_ = json.Unmarshal(newjs, &newchi)
	command.new = &newchi
	logCommand(command)
	return true
}

func prepareCHIUpdate(command *ReconcileCHI) bool {
	actionPlan := model.NewActionPlan(command.old, command.new)
	if !actionPlan.HasActionsToDo() {
		return false
	}
	oldjson, _ := json.MarshalIndent(command.old, "", "  ")
	newjson, _ := json.MarshalIndent(command.new, "", "  ")
	log.V(2).Info("AP enqueue---------------------------------------------:\n%s\n", actionPlan)
	log.V(3).Info("old enqueue--------------------------------------------:\n%s\n", string(oldjson))
	log.V(3).Info("new enqueue--------------------------------------------:\n%s\n", string(newjson))

	oldjs, _ := json.Marshal(command.old)
	newjs, _ := json.Marshal(command.new)
	oldchi := api.ClickHouseInstallation{}
	newchi := api.ClickHouseInstallation{
		TypeMeta: meta.TypeMeta{
			APIVersion: api.SchemeGroupVersion.String(),
			Kind:       api.ClickHouseInstallationCRDResourceKind,
		},
	}
	_ = json.Unmarshal(oldjs, &oldchi)
	_ = json.Unmarshal(newjs, &newchi)
	command.old = &oldchi
	command.new = &newchi
	logCommand(command)
	return true
}

func logCommand(command *ReconcileCHI) {
	namespace := "uns"
	name := "un"
	switch {
	case command.new != nil:
		namespace = command.new.Namespace
		name = command.new.Name
	case command.old != nil:
		namespace = command.old.Namespace
		name = command.old.Name
	}
	log.V(1).Info("ENQUEUE new ReconcileCHI cmd=%s for %s/%s", command.cmd, namespace, name)
}

// enqueueObject adds ClickHouseInstallation object to the work queue
func (c *Controller) enqueueObject(obj queue.PriorityQueueItem) {
	handle := []byte(obj.Handle().(string))
	index := 0
	enqueue := false
	switch command := obj.(type) {
	case *ReconcileCHI:
		variants := len(c.queues) - api.DefaultReconcileSystemThreadsNumber
		index = api.DefaultReconcileSystemThreadsNumber + util.HashIntoIntTopped(handle, variants)
		switch command.cmd {
		case reconcileAdd:
			enqueue = prepareCHIAdd(command)
		case reconcileUpdate:
			enqueue = prepareCHIUpdate(command)
		}
	case
		*ReconcileCHIT,
		*ReconcileChopConfig,
		*ReconcileEndpoints,
		*ReconcilePod,
		*DropDns:
		variants := api.DefaultReconcileSystemThreadsNumber
		index = util.HashIntoIntTopped(handle, variants)
		enqueue = true
	}
	if enqueue {
		//c.queues[index].AddRateLimited(obj)
		c.queues[index].Insert(obj)
	}
}

// updateWatch
func (c *Controller) updateWatch(chi *api.ClickHouseInstallation) {
	watched := metrics.NewWatchedCHI(chi)
	go c.updateWatchAsync(watched)
}

// updateWatchAsync
func (c *Controller) updateWatchAsync(chi *metrics.WatchedCHI) {
	if err := clickhouse.InformMetricsExporterAboutWatchedCHI(chi); err != nil {
		log.V(1).F().Info("FAIL update watch (%s/%s): %q", chi.Namespace, chi.Name, err)
	} else {
		log.V(1).Info("OK update watch (%s/%s): %s", chi.Namespace, chi.Name, chi)
	}
}

// deleteWatch
func (c *Controller) deleteWatch(chi *api.ClickHouseInstallation) {
	watched := metrics.NewWatchedCHI(chi)
	go c.deleteWatchAsync(watched)
}

// deleteWatchAsync
func (c *Controller) deleteWatchAsync(chi *metrics.WatchedCHI) {
	if err := clickhouse.InformMetricsExporterToDeleteWatchedCHI(chi); err != nil {
		log.V(1).F().Info("FAIL delete watch (%s/%s): %q", chi.Namespace, chi.Name, err)
	} else {
		log.V(1).Info("OK delete watch (%s/%s)", chi.Namespace, chi.Name)
	}
}

// addChopConfig
func (c *Controller) addChopConfig(chopConfig *api.ClickHouseOperatorConfiguration) error {
	if chop.Get().ConfigManager.IsConfigListed(chopConfig) {
		log.V(1).M(chopConfig).F().Info("already known config - do nothing")
	} else {
		log.V(1).M(chopConfig).F().Info("new, previously unknown config, need to apply")
		// TODO
		// NEED REFACTORING
		// os.Exit(0)
	}

	return nil
}

// updateChopConfig
func (c *Controller) updateChopConfig(old, new *api.ClickHouseOperatorConfiguration) error {
	if old.GetObjectMeta().GetResourceVersion() == new.GetObjectMeta().GetResourceVersion() {
		log.V(2).M(old).F().Info("ResourceVersion did not change: %s", old.GetObjectMeta().GetResourceVersion())
		// No need to react
		return nil
	}

	log.V(2).M(new).F().Info("ResourceVersion change: %s to %s", old.GetObjectMeta().GetResourceVersion(), new.GetObjectMeta().GetResourceVersion())
	// TODO
	// NEED REFACTORING
	//os.Exit(0)

	return nil
}

// deleteChit deletes CHIT
func (c *Controller) deleteChopConfig(chopConfig *api.ClickHouseOperatorConfiguration) error {
	log.V(2).M(chopConfig).F().P()
	// TODO
	// NEED REFACTORING
	//os.Exit(0)

	return nil
}

type patchFinalizers struct {
	Op    string   `json:"op"`
	Path  string   `json:"path"`
	Value []string `json:"value"`
}

// patchCHIFinalizers patch ClickHouseInstallation finalizers
func (c *Controller) patchCHIFinalizers(ctx context.Context, chi *api.ClickHouseInstallation) error {
	if util.IsContextDone(ctx) {
		log.V(2).Info("task is done")
		return nil
	}

	// TODO fix this with verbosity update
	// Start Debug object
	//js, err := json.MarshalIndent(chi, "", "  ")
	//if err != nil {
	//	log.V(1).M(chi).F().Error("%q", err)
	//}
	//log.V(3).M(chi).F().Info("\n%s\n", js)
	// End Debug object

	payload, _ := json.Marshal([]patchFinalizers{{
		Op:    "replace",
		Path:  "/metadata/finalizers",
		Value: chi.GetObjectMeta().GetFinalizers(),
	}})

	_new, err := c.chopClient.ClickhouseV1().ClickHouseInstallations(chi.Namespace).Patch(ctx, chi.Name, types.JSONPatchType, payload, controller.NewPatchOptions())
	if err != nil {
		// Error update
		log.V(1).M(chi).F().Error("%q", err)
		return err
	}

	if chi.GetObjectMeta().GetResourceVersion() != _new.GetObjectMeta().GetResourceVersion() {
		// Updated
		log.V(2).M(chi).F().Info("ResourceVersion change: %s to %s", chi.GetObjectMeta().GetResourceVersion(), _new.GetObjectMeta().GetResourceVersion())
		chi.GetObjectMeta().SetResourceVersion(_new.GetObjectMeta().GetResourceVersion())
		return nil
	}

	// ResourceVersion not changed - no update performed?

	return nil
}

// UpdateCHIStatusOptions defines how to update CHI status
type UpdateCHIStatusOptions struct {
	api.CopyCHIStatusOptions
	TolerateAbsence bool
}

// updateCHIObjectStatus updates ClickHouseInstallation object's Status
func (c *Controller) updateCHIObjectStatus(ctx context.Context, chi *api.ClickHouseInstallation, opts UpdateCHIStatusOptions) (err error) {
	if util.IsContextDone(ctx) {
		log.V(2).Info("task is done")
		return nil
	}

	for retry, attempt := true, 1; retry; attempt++ {
		if attempt >= 5 {
			retry = false
		}

		err = c.doUpdateCHIObjectStatus(ctx, chi, opts)
		if err == nil {
			return nil
		}

		if retry {
			log.V(2).M(chi).F().Warning("got error, will retry. err: %q", err)
			time.Sleep(1 * time.Second)
		} else {
			log.V(1).M(chi).F().Error("got error, all retries are exhausted. err: %q", err)
		}
	}
	return
}

// doUpdateCHIObjectStatus updates ClickHouseInstallation object's Status
func (c *Controller) doUpdateCHIObjectStatus(ctx context.Context, chi *api.ClickHouseInstallation, opts UpdateCHIStatusOptions) error {
	if util.IsContextDone(ctx) {
		log.V(2).Info("task is done")
		return nil
	}

	namespace, name := util.NamespaceName(chi.GetObjectMeta())
	log.V(3).M(chi).F().Info("Update CHI status")

	podIPs := c.getPodsIPs(chi)

	cur, err := c.chopClient.ClickhouseV1().ClickHouseInstallations(namespace).Get(ctx, name, controller.NewGetOptions())
	if err != nil {
		if opts.TolerateAbsence {
			return nil
		}
		log.V(1).M(chi).F().Error("%q", err)
		return err
	}
	if cur == nil {
		if opts.TolerateAbsence {
			return nil
		}
		log.V(1).M(chi).F().Error("NULL returned")
		return fmt.Errorf("ERROR GetCR (%s/%s): NULL returned", namespace, name)
	}

	// Update status of a real object.
	cur.EnsureStatus().CopyFrom(chi.Status, opts.CopyCHIStatusOptions)
	cur.EnsureStatus().SetPodIPs(podIPs)

	_new, err := c.chopClient.ClickhouseV1().ClickHouseInstallations(chi.Namespace).UpdateStatus(ctx, cur, controller.NewUpdateOptions())
	if err != nil {
		// Error update
		log.V(2).M(chi).F().Info("Got error upon update, may retry. err: %q", err)
		return err
	}

	// Propagate updated ResourceVersion into chi
	if chi.GetObjectMeta().GetResourceVersion() != _new.GetObjectMeta().GetResourceVersion() {
		log.V(3).M(chi).F().Info("ResourceVersion change: %s to %s", chi.GetObjectMeta().GetResourceVersion(), _new.GetObjectMeta().GetResourceVersion())
		chi.GetObjectMeta().SetResourceVersion(_new.GetObjectMeta().GetResourceVersion())
		return nil
	}

	// ResourceVersion not changed - no update performed?

	return nil
}

func (c *Controller) poll(ctx context.Context, chi *api.ClickHouseInstallation, f func(c *api.ClickHouseInstallation, e error) bool) {
	if util.IsContextDone(ctx) {
		log.V(2).Info("task is done")
		return
	}

	namespace, name := util.NamespaceName(chi.GetObjectMeta())

	for {
		cur, err := c.chopClient.ClickhouseV1().ClickHouseInstallations(namespace).Get(ctx, name, controller.NewGetOptions())
		if f(cur, err) {
			// Continue polling
			if util.IsContextDone(ctx) {
				log.V(2).Info("task is done")
				return
			}
			time.Sleep(15 * time.Second)
		} else {
			// Stop polling
			return
		}
	}
}

// installFinalizer
func (c *Controller) installFinalizer(ctx context.Context, chi *api.ClickHouseInstallation) error {
	if util.IsContextDone(ctx) {
		log.V(2).Info("task is done")
		return nil
	}

	log.V(2).M(chi).S().P()
	defer log.V(2).M(chi).E().P()

	cur, err := c.chopClient.ClickhouseV1().ClickHouseInstallations(chi.Namespace).Get(ctx, chi.Name, controller.NewGetOptions())
	if err != nil {
		return err
	}
	if cur == nil {
		return fmt.Errorf("ERROR GetCR (%s/%s): NULL returned", chi.Namespace, chi.Name)
	}

	if util.InArray(FinalizerName, cur.GetObjectMeta().GetFinalizers()) {
		// Already installed
		return nil
	}
	log.V(3).M(chi).F().Info("no finalizer found, need to install one")

	cur.GetObjectMeta().SetFinalizers(append(cur.GetObjectMeta().GetFinalizers(), FinalizerName))
	return c.patchCHIFinalizers(ctx, cur)
}

// uninstallFinalizer
func (c *Controller) uninstallFinalizer(ctx context.Context, chi *api.ClickHouseInstallation) error {
	if util.IsContextDone(ctx) {
		log.V(2).Info("task is done")
		return nil
	}

	log.V(2).M(chi).S().P()
	defer log.V(2).M(chi).E().P()

	cur, err := c.chopClient.ClickhouseV1().ClickHouseInstallations(chi.Namespace).Get(ctx, chi.Name, controller.NewGetOptions())
	if err != nil {
		return err
	}
	if cur == nil {
		return fmt.Errorf("ERROR GetCR (%s/%s): NULL returned", chi.Namespace, chi.Name)
	}

	cur.GetObjectMeta().SetFinalizers(util.RemoveFromArray(FinalizerName, cur.GetObjectMeta().GetFinalizers()))

	return c.patchCHIFinalizers(ctx, cur)
}

// handleObject enqueues CHI which is owner of `obj` into reconcile loop
func (c *Controller) handleObject(obj interface{}) {
	// TODO review
	object, ok := obj.(meta.Object)
	if !ok {
		ts, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilRuntime.HandleError(fmt.Errorf(messageUnableToDecode))
			return
		}
		object, ok = ts.Obj.(meta.Object)
		if !ok {
			utilRuntime.HandleError(fmt.Errorf(messageUnableToDecode))
			return
		}
	}

	// object is an instance of meta.Object

	// Checking that we control current StatefulSet Object
	ownerRef := meta.GetControllerOf(object)
	if ownerRef == nil {
		// No owner
		return
	}

	// Ensure owner is of a proper kind
	if ownerRef.Kind != api.ClickHouseInstallationCRDResourceKind {
		return
	}

	log.V(1).Info("Processing object: %s", object.GetName())

	// Get owner - it is expected to be CHI
	// TODO chi, err := c.chi.ClickHouseInstallations(object.GetNamespace()).Get(ownerRef.Name)

	// TODO
	//if err != nil {
	//	log.V(1).Infof("ignoring orphaned object '%s' of ClickHouseInstallation '%s'", object.GetSelfLink(), ownerRef.Name)
	//	return
	//}

	// Add CHI object into reconcile loop
	// TODO c.enqueueObject(chi.Namespace, chi.Name, chi)
}

// waitForCacheSync is a logger-wrapper over cache.WaitForCacheSync() and it waits for caches to populate
func waitForCacheSync(ctx context.Context, name string, cacheSyncs ...cache.InformerSynced) bool {
	log.V(1).F().Info("Syncing caches for %s controller", name)
	if !cache.WaitForCacheSync(ctx.Done(), cacheSyncs...) {
		utilRuntime.HandleError(fmt.Errorf(messageUnableToSync, name))
		return false
	}
	log.V(1).F().Info("Caches are synced for %s controller", name)
	return true
}
