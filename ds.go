package main

import (
	"fmt"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"sync"
	"time"
)

type EventType string

const (
	Added    EventType = "ADDED"
	Modified EventType = "MODIFIED"
	Deleted  EventType = "DELETED"
)

type Event struct {
	Type      EventType
	Object    interface{}
	OldObject interface{}
}

type PodController struct {
	mu            sync.Mutex
	Namespace     string
	DaemonsetName string
	Targets       *[]string
	queue         workqueue.TypedRateLimitingInterface[Event]
	podLiser      corelisters.PodLister
	podSynced     cache.InformerSynced
}

func (c *PodController) handleErr(err error, event Event) {
	if err == nil {
		// Forget about the #AddRateLimited history of the key on every successful synchronization.
		// This ensures that future processing of updates for this key is not delayed because of
		// an outdated error history.
		c.queue.Forget(event)
		return
	}

	// This controller retries 5 times if something goes wrong. After that, it stops trying.
	if c.queue.NumRequeues(event) < 5 {
		klog.Infof("Error syncing pod %v: %v", event, err)

		// Re-enqueue the key rate limited. Based on the rate limiter on the
		// queue and the re-enqueue history, the key will be processed later again.
		c.queue.AddRateLimited(event)
		return
	}

	c.queue.Forget(event)
	// Report to an external entity that, even after several retries, we could not successfully process this key
	runtime.HandleError(err)
	klog.Infof("Dropping pod %q out of the queue: %v", event, err)
}

func (c *PodController) processNextItem() bool {
	// Wait until there is a new item in the working queue
	event, quit := c.queue.Get()
	if quit {
		return false
	}
	// Tell the queue that we are done with processing this key. This unblocks the key for other workers
	// This allows safe parallel processing because two pods with the same key are never processed in
	// parallel.
	defer c.queue.Done(event)

	// Invoke the method containing the business logic
	err := c.processEvent(event)
	// Handle the error if something went wrong during the execution of the business logic
	c.handleErr(err, event)
	return true
}

func (c *PodController) runWorker() {
	for c.processNextItem() {
	}
}

func getPodTarget(pod *v1.Pod) (string, error) {
	var ip string
	for i := range pod.Status.PodIPs {
		ip = pod.Status.PodIPs[i].IP
		if len(ip) > 0 {
			break
		}
	}
	if len(ip) == 0 {
		ip = pod.Status.PodIP
	}

	if len(ip) == 0 {
		return "", fmt.Errorf("no pod ip yet")
	}

	c := pod.Spec.Containers[0]
	if len(c.Ports) > 0 {
		return fmt.Sprintf("http://%s:%d", ip, c.Ports[0].ContainerPort), nil
	} else {
		return fmt.Sprintf("http://%s:%d", ip, 8080), nil
	}
}

func (p *PodController) processEvent(event interface{}) error {
	switch e := event.(Event); e.Type {
	case Added:
		obj := e.Object
		if pod, ok := obj.(*v1.Pod); ok {
			klog.Info(pod.Name, " Added")
			if IsPodReady(pod) {
				if t, err := getPodTarget(pod); err == nil {
					p.mu.Lock()
					defer p.mu.Unlock()

					exist := false
					for _, target := range *p.Targets {
						if target == t {
							exist = true
							break
						}
					}
					if !exist {
						*p.Targets = append(*p.Targets, t)
						klog.Info(*p.Targets)
					}
				} else {
					klog.Info(pod.Name, " Added, error ", err)
				}
			}
		}
	case Modified:
		newPod, ok1 := e.Object.(*v1.Pod)
		oldPod, ok2 := e.OldObject.(*v1.Pod)
		if !ok1 || !ok2 {
			return nil
		}
		// If the pod's deletion timestamp is set, remove endpoint from ready address.
		if newPod.DeletionTimestamp != oldPod.DeletionTimestamp {
			klog.Info(newPod.Name, " DeletionTimestamp set")
			if t, err := getPodTarget(newPod); err == nil {
				p.mu.Lock()
				defer p.mu.Unlock()

				for i, target := range *p.Targets {
					if target == t {
						*p.Targets = append((*p.Targets)[:i], (*p.Targets)[i+1:]...)
						break
					}
				}
				klog.Info(*p.Targets)
				return nil
			} else {
				klog.Info(newPod.Name, " Modified, error ", err)
			}
		}

		if IsPodReady(oldPod) != IsPodReady(newPod) {
			if IsPodReady(newPod) {
				klog.Info(newPod.Name, " become Ready")
				if t, err := getPodTarget(newPod); err == nil {
					p.mu.Lock()
					defer p.mu.Unlock()

					exist := false
					for _, target := range *p.Targets {
						if target == t {
							exist = true
							break
						}
					}
					if !exist {
						*p.Targets = append(*p.Targets, t)
						klog.Info(*p.Targets)
					}
				} else {
					klog.Info(newPod.Name, " Modified, error ", err)
				}
			} else {
				klog.Info(newPod.Name, " become Unready")
				if t, err := getPodTarget(newPod); err == nil {
					p.mu.Lock()
					defer p.mu.Unlock()

					for i, target := range *p.Targets {
						if target == t {
							*p.Targets = append((*p.Targets)[:i], (*p.Targets)[i+1:]...)
							break
						}
					}
					klog.Info(*p.Targets)
				} else {
					klog.Info(newPod.Name, " Modified, error ", err)
				}
			}
		}
	case Deleted:
		obj := e.Object
		pod, ok := obj.(*v1.Pod)

		// When a delete is dropped, the relist will notice a pod in the store not
		// in the list, leading to the insertion of a tombstone object which contains
		// the deleted key/value. Note that this value might be stale. If the Pod
		// changed labels the new deployment will not be woken up till the periodic resync.
		if !ok {
			tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
			if !ok {
				runtime.HandleError(fmt.Errorf("couldn't get object from tombstone %#v", obj))
				return nil
			}
			pod, ok = tombstone.Obj.(*v1.Pod)
			if !ok {
				runtime.HandleError(fmt.Errorf("tombstone contained object that is not a pod %#v", obj))
				return nil
			}
		}

		klog.Info(pod.Name, " Deleted")
		if t, err := getPodTarget(pod); err == nil {
			p.mu.Lock()
			defer p.mu.Unlock()

			found := false
			for i, target := range *p.Targets {
				if target == t {
					*p.Targets = append((*p.Targets)[:i], (*p.Targets)[i+1:]...)
					found = true
					break
				}
			}
			if !found {
				klog.Info(pod.Name, " Deleted, but not found in targets")
			}
			klog.Info(*p.Targets)
		} else {
			klog.Info(pod.Name, " Deleted, error ", err)
		}
	}
	return nil
}

// IsPodReady returns true if Pods Ready condition is true
// copied from k8s.io/kubernetes/pkg/api/v1/pod
func IsPodReady(pod *v1.Pod) bool {
	return isPodReadyConditionTrue(pod.Status)
}

// IsPodTerminal returns true if a pod is terminal, all containers are stopped and cannot ever regress.
// copied from k8s.io/kubernetes/pkg/api/v1/pod
func isPodTerminal(pod *v1.Pod) bool {
	return isPodPhaseTerminal(pod.Status.Phase)
}

// IsPodPhaseTerminal returns true if the pod's phase is terminal.
// copied from k8s.io/kubernetes/pkg/api/v1/pod
func isPodPhaseTerminal(phase v1.PodPhase) bool {
	return phase == v1.PodFailed || phase == v1.PodSucceeded
}

// IsPodReadyConditionTrue returns true if a pod is ready; false otherwise.
// copied from k8s.io/kubernetes/pkg/api/v1/pod
func isPodReadyConditionTrue(status v1.PodStatus) bool {
	condition := getPodReadyCondition(&status)
	return condition != nil && condition.Status == v1.ConditionTrue
}

// getPodReadyCondition extracts the pod ready condition from the given status and returns that.
// Returns nil if the condition is not present.
// copied from k8s.io/kubernetes/pkg/api/v1/pod
func getPodReadyCondition(status *v1.PodStatus) *v1.PodCondition {
	if status == nil || status.Conditions == nil {
		return nil
	}

	for i := range status.Conditions {
		if status.Conditions[i].Type == v1.PodReady {
			return &status.Conditions[i]
		}
	}
	return nil
}

func (p *PodController) addPod(obj interface{}) {
	p.queue.Add(Event{Added, obj, nil})
}

func (p *PodController) updatePod(old, cur interface{}) {
	p.queue.Add(Event{Modified, cur, old})
}

func (p *PodController) deletePod(obj interface{}) {
	p.queue.Add(Event{Deleted, obj, nil})
}

func (p *PodController) getAllPods() {
	p.mu.Lock()
	defer p.mu.Unlock()

	klog.Info("enter getAllPods")

	*p.Targets = (*p.Targets)[:0]
	allPods, err := p.podLiser.Pods(p.Namespace).List(labels.Everything())

	if err != nil {
		klog.Info("fail to list pods ", err)
		return
	}
	for _, pod := range allPods {
		for _, owner := range pod.OwnerReferences {
			if owner.Kind == "DaemonSet" && owner.Name == p.DaemonsetName {
				if IsPodReady(pod) {
					if t, err := getPodTarget(pod); err == nil {
						*p.Targets = append(*p.Targets, t)
					} else {
						klog.Info(pod.Name, " without ip")
					}
				} else {
					klog.Info(pod.Name, " Pod not ready")
				}
			}
		}
	}
	klog.Info(*p.Targets)
}

func watchPodForDs(namespace string, dsname string, targets *[]string, mu sync.Mutex) {
	KubeConfig, _ := rest.InClusterConfig()
	client := kubernetes.NewForConfigOrDie(KubeConfig)

	sharedInformerFactory := informers.NewSharedInformerFactoryWithOptions(client, time.Minute, informers.WithNamespace(namespace))
	podInformer := sharedInformerFactory.Core().V1().Pods()

	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[Event]())

	controller := PodController{
		mu:            mu,
		Namespace:     namespace,
		DaemonsetName: dsname,
		Targets:       targets,
		queue:         queue,
		podLiser:      podInformer.Lister(),
	}

	podInformer.Informer().AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: func(obj interface{}) bool {
			switch pod := obj.(type) {
			case *v1.Pod:
				if pod.Namespace == controller.Namespace {
					for _, owner := range pod.OwnerReferences {
						if owner.Kind == "DaemonSet" && owner.Name == controller.DaemonsetName {
							return true
						}
					}
				}
			default:
				runtime.HandleError(fmt.Errorf("unable to handle object %T", obj))
				return false
			}
			return false
		},
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc:    controller.addPod,
			UpdateFunc: controller.updatePod,
			DeleteFunc: controller.deletePod,
		},
	})

	stopCh := make(chan struct{})

	sharedInformerFactory.Start(stopCh)
	sharedInformerFactory.WaitForCacheSync(stopCh)

	if !cache.WaitForNamedCacheSync("watch-ds-pod",
		stopCh,
		podInformer.Informer().HasSynced,
	) {
		return
	}

	controller.getAllPods()

	go wait.Until(controller.runWorker, time.Second, stopCh)

	go func() {
		for {
			select {
			case <-stopCh:
				runtime.HandleCrash()
				queue.ShutDown()
				klog.Info("Stopping Pod controller")
				return
			}
		}
	}()
}
