// Package watchers provides a means of watching changes in different
// Kubernetes resources.
package watchers

import (
	"fmt"
	"log"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	corev1informer "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/informers/internalinterfaces"
	networkingv1informer "k8s.io/client-go/informers/networking/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	routev1 "github.com/openshift/api/route/v1"
	openshiftroute "github.com/openshift/client-go/route/clientset/versioned"
	routev1client "github.com/openshift/client-go/route/clientset/versioned/typed/route/v1"
	routev1interfaces "github.com/openshift/client-go/route/informers/externalversions/internalinterfaces"
	routev1informer "github.com/openshift/client-go/route/informers/externalversions/route/v1"

	internalclient "github.com/skupperproject/skupper/internal/kube/client"
	"github.com/skupperproject/skupper/internal/kube/resource"
	skupperv2alpha1 "github.com/skupperproject/skupper/pkg/apis/skupper/v2alpha1"
	skupperclient "github.com/skupperproject/skupper/pkg/generated/client/clientset/versioned"
	skupperv2alpha1interfaces "github.com/skupperproject/skupper/pkg/generated/client/informers/externalversions/internalinterfaces"
	skupperv2alpha1informer "github.com/skupperproject/skupper/pkg/generated/client/informers/externalversions/skupper/v2alpha1"
)

// ResourceChange is the form in which events are added to the
// EventProcessor's work queue. Each ResourceChange event has a key
// that identifies the resource along with an implementation of the
// ResourceChangeHandler interface that will be used when processing
// the event.
type ResourceChange struct {
	Handler ResourceChangeHandler
	Key     string
}

// The ResourceChangeHandler interface allows the event processing
// loop to handle events from different resource types in a general
// way.
type ResourceChangeHandler interface {
	// The Handle method is used to process the event.
	Handle(event ResourceChange) error
	// The Describe method is used to log information about the
	// event when an error is returned by the Handle method.
	Describe(event ResourceChange) string
}

// The Watcher interface allows the EventProcessor to interact with
// different informers on startup.
type Watcher interface {
	HasSynced() func() bool
	Start(stopCh <-chan struct{})
}

// A EventProcessor provides a way to handle events from multiple
// different informers on the same go routine. It does this using a
// single work queue into which the events are added as instances of the
// ResourceChange struct.
type EventProcessor struct {
	errorKey        string
	client          kubernetes.Interface
	routeClient     openshiftroute.Interface
	dynamicClient   dynamic.Interface
	discoveryClient discovery.DiscoveryInterface
	skupperClient   skupperclient.Interface
	queue           workqueue.RateLimitingInterface
	resync          time.Duration
	watchers        []Watcher
}

// Creates a properly initialised EventProcessor instance.
func NewEventProcessor(name string, clients internalclient.Clients) *EventProcessor {
	return &EventProcessor{
		errorKey:        name + "Error",
		client:          clients.GetKubeClient(),
		routeClient:     clients.GetRouteInterface(),
		discoveryClient: clients.GetDiscoveryClient(),
		dynamicClient:   clients.GetDynamicClient(),
		skupperClient:   clients.GetSkupperClient(),
		queue:           workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), name),
		resync:          time.Minute * 5,
	}
}

func (c *EventProcessor) GetKubeClient() kubernetes.Interface {
	return c.client
}

func (c *EventProcessor) GetDynamicClient() dynamic.Interface {
	return c.dynamicClient
}

func (c *EventProcessor) GetDiscoveryClient() discovery.DiscoveryInterface {
	return c.discoveryClient
}

func (c *EventProcessor) HasContourHttpProxy() bool {
	return resource.IsResourceAvailable(c.discoveryClient, resource.ContourHttpProxyResource())
}

func (c *EventProcessor) HasGateway() bool {
	return resource.IsResourceAvailable(c.discoveryClient, resource.GatewayResource())
}

func (c *EventProcessor) HasTlsRoute() bool {
	return resource.IsResourceAvailable(c.discoveryClient, resource.TlsRouteResource())
}

func (c *EventProcessor) GetRouteInterface() openshiftroute.Interface {
	return c.routeClient
}

func (c *EventProcessor) GetRouteClient() routev1client.RouteV1Interface {
	if c.routeClient == nil {
		return nil
	}
	return c.routeClient.RouteV1()
}

func (c *EventProcessor) GetSkupperClient() skupperclient.Interface {
	return c.skupperClient
}

// Starts the event processing loop in a new go routine.
func (c *EventProcessor) Start(stopCh <-chan struct{}) {
	go wait.Until(c.run, time.Second, stopCh)
}

func (c *EventProcessor) run() {
	for c.process() {
	}
}

// This is a convenience function for tests that use the EventProcessor,
// which may wish to process events more granularly.
func (c *EventProcessor) TestProcess() bool {
	return c.process()
}

// This is a convenience function for tests that use the EventProcessor.
func (c *EventProcessor) TestProcessAll() {
	for c.queue.Len() > 0 {
		c.process()
	}
}

// The process method is the heart of the event processing loop.
func (c *EventProcessor) process() bool {
	obj, shutdown := c.queue.Get()

	if shutdown {
		return false
	}

	retry := false
	defer c.queue.Done(obj)
	if evt, ok := obj.(ResourceChange); ok {
		err := evt.Handler.Handle(evt)
		if err != nil {
			retry = true
			log.Printf("[%s] Error while handling %s: %s", c.errorKey, evt.Handler.Describe(evt), err)
		}
	} else {
		log.Printf("Invalid object on event queue for %q: %#v", c.errorKey, obj)
	}

	if retry && c.queue.NumRequeues(obj) < 5 {
		c.queue.AddRateLimited(obj)
		return true
	}
	c.queue.Forget(obj)

	return true
}

// Stops event processing.
func (c *EventProcessor) Stop() {
	c.queue.ShutDown()
}

// Creates an event handler that will take handle events from an
// informer by constructing an appropriate ResourceChange instance and
// adding it to the EventProcessor's work queue.
func (c *EventProcessor) newEventHandler(handler ResourceChangeHandler) *cache.ResourceEventHandlerFuncs {
	evt := ResourceChange{
		Handler: handler,
	}
	return &cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err != nil {
				utilruntime.HandleError(err)
			} else {
				evt.Key = key
				c.queue.Add(evt)
			}
		},
		UpdateFunc: func(old, new interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(new)
			if err != nil {
				utilruntime.HandleError(err)
			} else {
				evt.Key = key
				c.queue.Add(evt)
			}
		},
		DeleteFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err != nil {
				utilruntime.HandleError(err)
			} else {
				evt.Key = key
				c.queue.Add(evt)
			}
		},
	}
}

func (c *EventProcessor) addWatcher(watcher Watcher) {
	c.watchers = append(c.watchers, watcher)
}

// Starts all the configured informers.
func (c *EventProcessor) StartWatchers(stopCh <-chan struct{}) {
	for _, watcher := range c.watchers {
		watcher.Start(stopCh)
	}
}

// Wait for all the configured informers to sync.
func (c *EventProcessor) WaitForCacheSync(stopCh <-chan struct{}) bool {
	return cache.WaitForCacheSync(stopCh, c.haveWatchersSynced()...)
}

func (c *EventProcessor) haveWatchersSynced() []cache.InformerSynced {
	var combined []cache.InformerSynced
	for _, watcher := range c.watchers {
		combined = append(combined, watcher.HasSynced())
	}
	return combined
}

// Watches for ConfigMap related events matching options and invokes the handler function accordingly.
func (c *EventProcessor) WatchConfigMaps(options internalinterfaces.TweakListOptionsFunc, namespace string, handler ConfigMapHandler) *ConfigMapWatcher {
	watcher := &ConfigMapWatcher{
		handler: handler,
		informer: corev1informer.NewFilteredConfigMapInformer(
			c.client,
			namespace,
			c.resync,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
			options),
		namespace: namespace,
	}

	watcher.informer.AddEventHandler(c.newEventHandler(watcher))
	c.addWatcher(watcher)
	return watcher
}

type ConfigMapHandler func(string, *corev1.ConfigMap) error

type ConfigMapWatcher struct {
	handler   ConfigMapHandler
	informer  cache.SharedIndexInformer
	namespace string
}

func (w *ConfigMapWatcher) HasSynced() func() bool {
	return w.informer.HasSynced
}

func (w *ConfigMapWatcher) Handle(event ResourceChange) error {
	obj, err := w.Get(event.Key)
	if err != nil {
		return err
	}
	return w.handler(event.Key, obj)
}

func (w *ConfigMapWatcher) Describe(event ResourceChange) string {
	return fmt.Sprintf("ConfigMap %s", event.Key)
}

func (w *ConfigMapWatcher) Start(stopCh <-chan struct{}) {
	go w.informer.Run(stopCh)
}

func (w *ConfigMapWatcher) Sync(stopCh <-chan struct{}) bool {
	return cache.WaitForCacheSync(stopCh, w.informer.HasSynced)
}

// Provides access to the latest ConfigMap resource with the specified
// key as seen by this watcher.
func (w *ConfigMapWatcher) Get(key string) (*corev1.ConfigMap, error) {
	entity, exists, err := w.informer.GetStore().GetByKey(key)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	return entity.(*corev1.ConfigMap), nil
}

// Provides a list of all current ConfigMap resources seen by this watcher.
func (w *ConfigMapWatcher) List() []*corev1.ConfigMap {
	list := w.informer.GetStore().List()
	results := []*corev1.ConfigMap{}
	for _, o := range list {
		results = append(results, o.(*corev1.ConfigMap))
	}
	return results
}

func (c *EventProcessor) WatchSecrets(options internalinterfaces.TweakListOptionsFunc, namespace string, handler SecretHandler) *SecretWatcher {
	watcher := &SecretWatcher{
		handler: handler,
		informer: corev1informer.NewFilteredSecretInformer(
			c.client,
			namespace,
			c.resync,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
			options),
		namespace: namespace,
	}

	watcher.informer.AddEventHandler(c.newEventHandler(watcher))
	c.addWatcher(watcher)
	return watcher
}

func (c *EventProcessor) WatchAllSecrets(namespace string, handler SecretHandler) *SecretWatcher {
	watcher := &SecretWatcher{
		handler: handler,
		informer: corev1informer.NewSecretInformer(
			c.client,
			namespace,
			c.resync,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
		),
		namespace: namespace,
	}

	watcher.informer.AddEventHandler(c.newEventHandler(watcher))
	c.addWatcher(watcher)
	return watcher
}

type SecretHandler func(string, *corev1.Secret) error

type SecretWatcher struct {
	handler   SecretHandler
	informer  cache.SharedIndexInformer
	namespace string
}

func (w *SecretWatcher) HasSynced() func() bool {
	return w.informer.HasSynced
}

func (w *SecretWatcher) Handle(event ResourceChange) error {
	obj, err := w.Get(event.Key)
	if err != nil {
		return err
	}
	return w.handler(event.Key, obj)
}

func (w *SecretWatcher) Describe(event ResourceChange) string {
	return fmt.Sprintf("Secret %s", event.Key)
}

func (w *SecretWatcher) Start(stopCh <-chan struct{}) {
	go w.informer.Run(stopCh)
}

func (w *SecretWatcher) Sync(stopCh <-chan struct{}) bool {
	return cache.WaitForCacheSync(stopCh, w.informer.HasSynced)
}

func (w *SecretWatcher) Get(key string) (*corev1.Secret, error) {
	entity, exists, err := w.informer.GetStore().GetByKey(key)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	return entity.(*corev1.Secret), nil
}

func (w *SecretWatcher) List() []*corev1.Secret {
	list := w.informer.GetStore().List()
	results := []*corev1.Secret{}
	for _, o := range list {
		results = append(results, o.(*corev1.Secret))
	}
	return results
}

type ServiceHandler func(string, *corev1.Service) error

func (c *EventProcessor) WatchServices(options internalinterfaces.TweakListOptionsFunc, namespace string, handler ServiceHandler) *ServiceWatcher {
	watcher := &ServiceWatcher{
		client:  c.client,
		handler: handler,
		informer: corev1informer.NewFilteredServiceInformer(
			c.client,
			namespace,
			c.resync,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
			options,
		),
		namespace: namespace,
	}

	watcher.informer.AddEventHandler(c.newEventHandler(watcher))
	c.addWatcher(watcher)
	return watcher
}

type ServiceWatcher struct {
	client    kubernetes.Interface
	handler   ServiceHandler
	informer  cache.SharedIndexInformer
	namespace string
}

func (w *ServiceWatcher) Handle(event ResourceChange) error {
	obj, err := w.Get(event.Key)
	if err != nil {
		return err
	}
	return w.handler(event.Key, obj)
}

func (w *ServiceWatcher) Describe(event ResourceChange) string {
	return fmt.Sprintf("Service %s", event.Key)
}

func (w *ServiceWatcher) Start(stopCh <-chan struct{}) {
	go w.informer.Run(stopCh)
}

func (w *ServiceWatcher) Sync(stopCh <-chan struct{}) bool {
	return cache.WaitForCacheSync(stopCh, w.informer.HasSynced)
}

func (w *ServiceWatcher) HasSynced() func() bool {
	return w.informer.HasSynced
}

func (w *ServiceWatcher) Get(key string) (*corev1.Service, error) {
	entity, exists, err := w.informer.GetStore().GetByKey(key)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	return entity.(*corev1.Service), nil
}

func (w *ServiceWatcher) List() []*corev1.Service {
	list := w.informer.GetStore().List()
	results := []*corev1.Service{}
	for _, o := range list {
		results = append(results, o.(*corev1.Service))
	}
	return results
}

type PodHandler func(string, *corev1.Pod) error

func (c *EventProcessor) WatchPods(selector string, namespace string, handler PodHandler) *PodWatcher {
	options := func(options *metav1.ListOptions) {
		options.LabelSelector = selector
	}
	watcher := &PodWatcher{
		handler: handler,
		informer: corev1informer.NewFilteredPodInformer(
			c.client,
			namespace,
			c.resync,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
			options,
		),
		namespace: namespace,
	}

	watcher.informer.AddEventHandler(c.newEventHandler(watcher))
	c.addWatcher(watcher)
	return watcher
}

type PodWatcher struct {
	handler   PodHandler
	informer  cache.SharedIndexInformer
	namespace string
}

func (w *PodWatcher) HasSynced() func() bool {
	return w.informer.HasSynced
}

func (w *PodWatcher) Handle(event ResourceChange) error {
	obj, err := w.Get(event.Key)
	if err != nil {
		return err
	}
	return w.handler(event.Key, obj)
}

func (w *PodWatcher) Describe(event ResourceChange) string {
	return fmt.Sprintf("Pod %s", event.Key)
}

func (w *PodWatcher) Start(stopCh <-chan struct{}) {
	go w.informer.Run(stopCh)
}

func (w *PodWatcher) Sync(stopCh <-chan struct{}) bool {
	return cache.WaitForCacheSync(stopCh, w.informer.HasSynced)
}

func (w *PodWatcher) Get(key string) (*corev1.Pod, error) {
	entity, exists, err := w.informer.GetStore().GetByKey(key)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	return entity.(*corev1.Pod), nil
}

func (w *PodWatcher) List() []*corev1.Pod {
	list := w.informer.GetStore().List()
	pods := []*corev1.Pod{}
	for _, p := range list {
		pods = append(pods, p.(*corev1.Pod))
	}
	return pods
}

func (c *EventProcessor) WatchContourHttpProxies(options dynamicinformer.TweakListOptionsFunc, namespace string, handler DynamicHandler) *DynamicWatcher {
	if !c.HasContourHttpProxy() {
		log.Println("Cannot watch HttpProxies; resource not installed")
		return nil
	}
	return c.WatchDynamic(resource.ContourHttpProxyResource(), options, namespace, handler)
}

func (c *EventProcessor) WatchGateways(options dynamicinformer.TweakListOptionsFunc, namespace string, handler DynamicHandler) *DynamicWatcher {
	if !c.HasGateway() {
		log.Println("Cannot watch Gateways; resource not installed")
		return nil
	}
	return c.WatchDynamic(resource.GatewayResource(), options, namespace, handler)
}

func (c *EventProcessor) WatchTlsRoutes(options dynamicinformer.TweakListOptionsFunc, namespace string, handler DynamicHandler) *DynamicWatcher {
	if !c.HasTlsRoute() {
		log.Println("Cannot watch TLSRoutes; resource not installed")
		return nil
	}
	return c.WatchDynamic(resource.TlsRouteResource(), options, namespace, handler)
}

func (c *EventProcessor) WatchDynamic(resource schema.GroupVersionResource, options dynamicinformer.TweakListOptionsFunc, namespace string, handler DynamicHandler) *DynamicWatcher {
	watcher := &DynamicWatcher{
		handler: handler,
		informer: dynamicinformer.NewFilteredDynamicInformer(
			c.dynamicClient,
			resource,
			namespace,
			c.resync,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
			options).Informer(),
		namespace: namespace,
		resource:  resource,
	}

	watcher.informer.AddEventHandler(c.newEventHandler(watcher))
	c.addWatcher(watcher)
	return watcher
}

type DynamicHandler func(string, *unstructured.Unstructured) error

type DynamicWatcher struct {
	handler   DynamicHandler
	informer  cache.SharedIndexInformer
	namespace string
	resource  schema.GroupVersionResource
}

func (w *DynamicWatcher) Resource() schema.GroupVersionResource {
	return w.resource
}

func (w *DynamicWatcher) HasSynced() func() bool {
	return w.informer.HasSynced
}

func (w *DynamicWatcher) Handle(event ResourceChange) error {
	obj, err := w.Get(event.Key)
	if err != nil {
		return err
	}
	return w.handler(event.Key, obj)
}

func (w *DynamicWatcher) Describe(event ResourceChange) string {
	return fmt.Sprintf("Dynamic %s", event.Key)
}

func (w *DynamicWatcher) Start(stopCh <-chan struct{}) {
	go w.informer.Run(stopCh)
}

func (w *DynamicWatcher) Sync(stopCh <-chan struct{}) bool {
	return cache.WaitForCacheSync(stopCh, w.informer.HasSynced)
}

func (w *DynamicWatcher) Get(key string) (*unstructured.Unstructured, error) {
	entity, exists, err := w.informer.GetStore().GetByKey(key)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	return entity.(*unstructured.Unstructured), nil
}

func (w *DynamicWatcher) List() []*unstructured.Unstructured {
	list := w.informer.GetStore().List()
	results := []*unstructured.Unstructured{}
	for _, o := range list {
		results = append(results, o.(*unstructured.Unstructured))
	}
	return results
}

type Callback func(context string) error

type CallbackHandler struct {
	callback Callback
	context  string
}

func (c *CallbackHandler) Handle(event ResourceChange) error {
	return c.callback(c.context)
}

func (c *CallbackHandler) Describe(event ResourceChange) string {
	return fmt.Sprintf("Callback %v(%s)", c.callback, c.context)
}

// Allows triggering of a callback on the event processing
// thread. This method call be called on any thread/goroutine.
func (c *EventProcessor) CallbackAfter(delay time.Duration, callback Callback, context string) {
	evt := ResourceChange{
		Handler: &CallbackHandler{
			callback: callback,
			context:  context,
		},
	}
	c.queue.AddAfter(evt, delay)
}

func (c *EventProcessor) WatchNamespaces(options internalinterfaces.TweakListOptionsFunc, handler NamespaceHandler) *NamespaceWatcher {
	watcher := &NamespaceWatcher{
		handler: handler,
		informer: corev1informer.NewFilteredNamespaceInformer(
			c.client,
			c.resync,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
			options),
	}

	watcher.informer.AddEventHandler(c.newEventHandler(watcher))
	c.addWatcher(watcher)
	return watcher
}

type NamespaceHandler func(string, *corev1.Namespace) error

type NamespaceWatcher struct {
	handler  NamespaceHandler
	informer cache.SharedIndexInformer
}

func (w *NamespaceWatcher) HasSynced() func() bool {
	return w.informer.HasSynced
}

func (w *NamespaceWatcher) Handle(event ResourceChange) error {
	obj, err := w.Get(event.Key)
	if err != nil {
		return err
	}
	return w.handler(event.Key, obj)
}

func (w *NamespaceWatcher) Describe(event ResourceChange) string {
	return fmt.Sprintf("Namespace %s", event.Key)
}

func (w *NamespaceWatcher) Start(stopCh <-chan struct{}) {
	go w.informer.Run(stopCh)
}

func (w *NamespaceWatcher) Sync(stopCh <-chan struct{}) bool {
	return cache.WaitForCacheSync(stopCh, w.informer.HasSynced)
}

func (w *NamespaceWatcher) Get(key string) (*corev1.Namespace, error) {
	entity, exists, err := w.informer.GetStore().GetByKey(key)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	return entity.(*corev1.Namespace), nil
}

func (w *NamespaceWatcher) List() []*corev1.Namespace {
	list := w.informer.GetStore().List()
	results := []*corev1.Namespace{}
	for _, o := range list {
		results = append(results, o.(*corev1.Namespace))
	}
	return results
}

func (c *EventProcessor) WatchNodes(handler NodeHandler) *NodeWatcher {
	watcher := &NodeWatcher{
		handler: handler,
		informer: corev1informer.NewNodeInformer(
			c.client,
			c.resync,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}),
	}

	watcher.informer.AddEventHandler(c.newEventHandler(watcher))
	c.addWatcher(watcher)
	return watcher
}

type NodeHandler func(string, *corev1.Node) error

type NodeWatcher struct {
	handler  NodeHandler
	informer cache.SharedIndexInformer
}

func (w *NodeWatcher) HasSynced() func() bool {
	return w.informer.HasSynced
}

func (w *NodeWatcher) Handle(event ResourceChange) error {
	obj, err := w.Get(event.Key)
	if err != nil {
		return err
	}
	return w.handler(event.Key, obj)
}

func (w *NodeWatcher) Describe(event ResourceChange) string {
	return fmt.Sprintf("Node %s", event.Key)
}

func (w *NodeWatcher) Start(stopCh <-chan struct{}) {
	go w.informer.Run(stopCh)
}

func (w *NodeWatcher) Sync(stopCh <-chan struct{}) bool {
	return cache.WaitForCacheSync(stopCh, w.informer.HasSynced)
}

func (w *NodeWatcher) Get(key string) (*corev1.Node, error) {
	entity, exists, err := w.informer.GetStore().GetByKey(key)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	return entity.(*corev1.Node), nil
}

func (w *NodeWatcher) List() []*corev1.Node {
	list := w.informer.GetStore().List()
	results := []*corev1.Node{}
	for _, o := range list {
		results = append(results, o.(*corev1.Node))
	}
	return results
}

func (c *EventProcessor) WatchSites(namespace string, handler SiteHandler) *SiteWatcher {
	watcher := &SiteWatcher{
		handler: handler,
		informer: skupperv2alpha1informer.NewSiteInformer(
			c.skupperClient,
			namespace,
			time.Second*30,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}),
		namespace: namespace,
	}
	watcher.informer.AddEventHandler(c.newEventHandler(watcher))
	c.addWatcher(watcher)
	return watcher
}

type SiteHandler func(string, *skupperv2alpha1.Site) error

type SiteWatcher struct {
	handler   SiteHandler
	informer  cache.SharedIndexInformer
	namespace string
}

func (w *SiteWatcher) Handle(event ResourceChange) error {
	obj, err := w.Get(event.Key)
	if err != nil {
		return err
	}
	return w.handler(event.Key, obj)
}

func (w *SiteWatcher) HasSynced() func() bool {
	return w.informer.HasSynced
}

func (w *SiteWatcher) Describe(event ResourceChange) string {
	return fmt.Sprintf("Site %s", event.Key)
}

func (w *SiteWatcher) Start(stopCh <-chan struct{}) {
	go w.informer.Run(stopCh)
}

func (w *SiteWatcher) Sync(stopCh <-chan struct{}) bool {
	return cache.WaitForCacheSync(stopCh, w.informer.HasSynced)
}

func (w *SiteWatcher) Get(key string) (*skupperv2alpha1.Site, error) {
	entity, exists, err := w.informer.GetStore().GetByKey(key)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	return entity.(*skupperv2alpha1.Site), nil
}

func (w *SiteWatcher) List() []*skupperv2alpha1.Site {
	list := w.informer.GetStore().List()
	results := []*skupperv2alpha1.Site{}
	for _, o := range list {
		results = append(results, o.(*skupperv2alpha1.Site))
	}
	return results
}

func (c *EventProcessor) WatchListeners(namespace string, handler ListenerHandler) *ListenerWatcher {
	watcher := &ListenerWatcher{
		handler: handler,
		informer: skupperv2alpha1informer.NewListenerInformer(
			c.skupperClient,
			namespace,
			time.Second*30,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}),
		namespace: namespace,
	}
	watcher.informer.AddEventHandler(c.newEventHandler(watcher))
	c.addWatcher(watcher)
	return watcher
}

type ListenerHandler func(string, *skupperv2alpha1.Listener) error

type ListenerWatcher struct {
	handler   ListenerHandler
	informer  cache.SharedIndexInformer
	namespace string
}

func (w *ListenerWatcher) Handle(event ResourceChange) error {
	obj, err := w.Get(event.Key)
	if err != nil {
		return err
	}
	return w.handler(event.Key, obj)
}

func (w *ListenerWatcher) HasSynced() func() bool {
	return w.informer.HasSynced
}

func (w *ListenerWatcher) Describe(event ResourceChange) string {
	return fmt.Sprintf("Listener %s", event.Key)
}

func (w *ListenerWatcher) Start(stopCh <-chan struct{}) {
	go w.informer.Run(stopCh)
}

func (w *ListenerWatcher) Sync(stopCh <-chan struct{}) bool {
	return cache.WaitForCacheSync(stopCh, w.informer.HasSynced)
}

func (w *ListenerWatcher) Get(key string) (*skupperv2alpha1.Listener, error) {
	entity, exists, err := w.informer.GetStore().GetByKey(key)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	return entity.(*skupperv2alpha1.Listener), nil
}

func (w *ListenerWatcher) List() []*skupperv2alpha1.Listener {
	list := w.informer.GetStore().List()
	results := []*skupperv2alpha1.Listener{}
	for _, o := range list {
		results = append(results, o.(*skupperv2alpha1.Listener))
	}
	return results
}

func (c *EventProcessor) WatchConnectors(namespace string, handler ConnectorHandler) *ConnectorWatcher {
	watcher := &ConnectorWatcher{
		handler: handler,
		informer: skupperv2alpha1informer.NewConnectorInformer(
			c.skupperClient,
			namespace,
			time.Second*30,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}),
		namespace: namespace,
	}
	watcher.informer.AddEventHandler(c.newEventHandler(watcher))
	c.addWatcher(watcher)
	return watcher
}

type ConnectorHandler func(string, *skupperv2alpha1.Connector) error

type ConnectorWatcher struct {
	handler   ConnectorHandler
	informer  cache.SharedIndexInformer
	namespace string
}

func (w *ConnectorWatcher) Handle(event ResourceChange) error {
	obj, err := w.Get(event.Key)
	if err != nil {
		return err
	}
	return w.handler(event.Key, obj)
}

func (w *ConnectorWatcher) HasSynced() func() bool {
	return w.informer.HasSynced
}

func (w *ConnectorWatcher) Describe(event ResourceChange) string {
	return fmt.Sprintf("Connector %s", event.Key)
}

func (w *ConnectorWatcher) Start(stopCh <-chan struct{}) {
	go w.informer.Run(stopCh)
}

func (w *ConnectorWatcher) Sync(stopCh <-chan struct{}) bool {
	return cache.WaitForCacheSync(stopCh, w.informer.HasSynced)
}

func (w *ConnectorWatcher) Get(key string) (*skupperv2alpha1.Connector, error) {
	entity, exists, err := w.informer.GetStore().GetByKey(key)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	return entity.(*skupperv2alpha1.Connector), nil
}

func (w *ConnectorWatcher) List() []*skupperv2alpha1.Connector {
	list := w.informer.GetStore().List()
	results := []*skupperv2alpha1.Connector{}
	for _, o := range list {
		results = append(results, o.(*skupperv2alpha1.Connector))
	}
	return results
}

func (c *EventProcessor) WatchLinks(namespace string, handler LinkHandler) *LinkWatcher {
	watcher := &LinkWatcher{
		handler: handler,
		informer: skupperv2alpha1informer.NewLinkInformer(
			c.skupperClient,
			namespace,
			time.Second*30,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}),
		namespace: namespace,
	}
	watcher.informer.AddEventHandler(c.newEventHandler(watcher))
	c.addWatcher(watcher)
	return watcher
}

type LinkHandler func(string, *skupperv2alpha1.Link) error

type LinkWatcher struct {
	handler   LinkHandler
	informer  cache.SharedIndexInformer
	namespace string
}

func (w *LinkWatcher) Handle(event ResourceChange) error {
	obj, err := w.Get(event.Key)
	if err != nil {
		return err
	}
	return w.handler(event.Key, obj)
}

func (w *LinkWatcher) HasSynced() func() bool {
	return w.informer.HasSynced
}

func (w *LinkWatcher) Describe(event ResourceChange) string {
	return fmt.Sprintf("Link %s", event.Key)
}

func (w *LinkWatcher) Start(stopCh <-chan struct{}) {
	go w.informer.Run(stopCh)
}

func (w *LinkWatcher) Sync(stopCh <-chan struct{}) bool {
	return cache.WaitForCacheSync(stopCh, w.informer.HasSynced)
}

func (w *LinkWatcher) Get(key string) (*skupperv2alpha1.Link, error) {
	entity, exists, err := w.informer.GetStore().GetByKey(key)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	return entity.(*skupperv2alpha1.Link), nil
}

func (w *LinkWatcher) List() []*skupperv2alpha1.Link {
	list := w.informer.GetStore().List()
	results := []*skupperv2alpha1.Link{}
	for _, o := range list {
		results = append(results, o.(*skupperv2alpha1.Link))
	}
	return results
}

func (c *EventProcessor) WatchAccessTokens(namespace string, handler AccessTokenHandler) *AccessTokenWatcher {
	watcher := &AccessTokenWatcher{
		handler: handler,
		informer: skupperv2alpha1informer.NewAccessTokenInformer(
			c.skupperClient,
			namespace,
			time.Second*30,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}),
		namespace: namespace,
	}
	watcher.informer.AddEventHandler(c.newEventHandler(watcher))
	c.addWatcher(watcher)
	return watcher
}

type AccessTokenHandler func(string, *skupperv2alpha1.AccessToken) error

type AccessTokenWatcher struct {
	handler   AccessTokenHandler
	informer  cache.SharedIndexInformer
	namespace string
}

func (w *AccessTokenWatcher) Handle(event ResourceChange) error {
	obj, err := w.Get(event.Key)
	if err != nil {
		return err
	}
	return w.handler(event.Key, obj)
}

func (w *AccessTokenWatcher) HasSynced() func() bool {
	return w.informer.HasSynced
}

func (w *AccessTokenWatcher) Describe(event ResourceChange) string {
	return fmt.Sprintf("AccessToken %s", event.Key)
}

func (w *AccessTokenWatcher) Start(stopCh <-chan struct{}) {
	go w.informer.Run(stopCh)
}

func (w *AccessTokenWatcher) Sync(stopCh <-chan struct{}) bool {
	return cache.WaitForCacheSync(stopCh, w.informer.HasSynced)
}

func (w *AccessTokenWatcher) Get(key string) (*skupperv2alpha1.AccessToken, error) {
	entity, exists, err := w.informer.GetStore().GetByKey(key)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	return entity.(*skupperv2alpha1.AccessToken), nil
}

func (w *AccessTokenWatcher) List() []*skupperv2alpha1.AccessToken {
	list := w.informer.GetStore().List()
	results := []*skupperv2alpha1.AccessToken{}
	for _, o := range list {
		results = append(results, o.(*skupperv2alpha1.AccessToken))
	}
	return results
}

func (c *EventProcessor) WatchAccessGrants(namespace string, handler AccessGrantHandler) *AccessGrantWatcher {
	watcher := &AccessGrantWatcher{
		handler: handler,
		informer: skupperv2alpha1informer.NewAccessGrantInformer(
			c.skupperClient,
			namespace,
			time.Second*30,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}),
		namespace: namespace,
	}
	watcher.informer.AddEventHandler(c.newEventHandler(watcher))
	c.addWatcher(watcher)
	return watcher
}

type AccessGrantHandler func(string, *skupperv2alpha1.AccessGrant) error

type AccessGrantWatcher struct {
	handler   AccessGrantHandler
	informer  cache.SharedIndexInformer
	namespace string
}

func (w *AccessGrantWatcher) Handle(event ResourceChange) error {
	obj, err := w.Get(event.Key)
	if err != nil {
		return err
	}
	return w.handler(event.Key, obj)
}

func (w *AccessGrantWatcher) HasSynced() func() bool {
	return w.informer.HasSynced
}

func (w *AccessGrantWatcher) Describe(event ResourceChange) string {
	return fmt.Sprintf("AccessGrant %s", event.Key)
}

func (w *AccessGrantWatcher) Start(stopCh <-chan struct{}) {
	go w.informer.Run(stopCh)
}

func (w *AccessGrantWatcher) Sync(stopCh <-chan struct{}) bool {
	return cache.WaitForCacheSync(stopCh, w.informer.HasSynced)
}

func (w *AccessGrantWatcher) Get(key string) (*skupperv2alpha1.AccessGrant, error) {
	entity, exists, err := w.informer.GetStore().GetByKey(key)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	return entity.(*skupperv2alpha1.AccessGrant), nil
}

func (w *AccessGrantWatcher) List() []*skupperv2alpha1.AccessGrant {
	list := w.informer.GetStore().List()
	results := []*skupperv2alpha1.AccessGrant{}
	for _, o := range list {
		results = append(results, o.(*skupperv2alpha1.AccessGrant))
	}
	return results
}

func (c *EventProcessor) WatchSecuredAccesses(namespace string, handler SecuredAccessHandler) *SecuredAccessWatcher {
	watcher := &SecuredAccessWatcher{
		handler: handler,
		informer: skupperv2alpha1informer.NewSecuredAccessInformer(
			c.skupperClient,
			namespace,
			time.Second*30,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}),
		namespace: namespace,
	}
	watcher.informer.AddEventHandler(c.newEventHandler(watcher))
	c.addWatcher(watcher)
	return watcher
}

func (c *EventProcessor) WatchSecuredAccessesWithOptions(options skupperv2alpha1interfaces.TweakListOptionsFunc, namespace string, handler SecuredAccessHandler) *SecuredAccessWatcher {
	watcher := &SecuredAccessWatcher{
		handler: handler,
		informer: skupperv2alpha1informer.NewFilteredSecuredAccessInformer(
			c.skupperClient,
			namespace,
			time.Second*30,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
			options),
		namespace: namespace,
	}
	watcher.informer.AddEventHandler(c.newEventHandler(watcher))
	c.addWatcher(watcher)
	return watcher
}

type SecuredAccessHandler func(string, *skupperv2alpha1.SecuredAccess) error

type SecuredAccessWatcher struct {
	handler   SecuredAccessHandler
	informer  cache.SharedIndexInformer
	namespace string
}

func (w *SecuredAccessWatcher) Handle(event ResourceChange) error {
	obj, err := w.Get(event.Key)
	if err != nil {
		return err
	}
	return w.handler(event.Key, obj)
}

func (w *SecuredAccessWatcher) HasSynced() func() bool {
	return w.informer.HasSynced
}

func (w *SecuredAccessWatcher) Describe(event ResourceChange) string {
	return fmt.Sprintf("SecuredAccess %s", event.Key)
}

func (w *SecuredAccessWatcher) Start(stopCh <-chan struct{}) {
	go w.informer.Run(stopCh)
}

func (w *SecuredAccessWatcher) Sync(stopCh <-chan struct{}) bool {
	return cache.WaitForCacheSync(stopCh, w.informer.HasSynced)
}

func (w *SecuredAccessWatcher) Get(key string) (*skupperv2alpha1.SecuredAccess, error) {
	entity, exists, err := w.informer.GetStore().GetByKey(key)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	return entity.(*skupperv2alpha1.SecuredAccess), nil
}

func (w *SecuredAccessWatcher) List() []*skupperv2alpha1.SecuredAccess {
	list := w.informer.GetStore().List()
	results := []*skupperv2alpha1.SecuredAccess{}
	for _, o := range list {
		results = append(results, o.(*skupperv2alpha1.SecuredAccess))
	}
	return results
}

func (c *EventProcessor) WatchIngresses(options internalinterfaces.TweakListOptionsFunc, namespace string, handler IngressHandler) *IngressWatcher {
	watcher := &IngressWatcher{
		handler: handler,
		informer: networkingv1informer.NewFilteredIngressInformer(
			c.client,
			namespace,
			c.resync,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
			options),
		namespace: namespace,
	}

	watcher.informer.AddEventHandler(c.newEventHandler(watcher))
	c.addWatcher(watcher)
	return watcher
}

type IngressHandler func(string, *networkingv1.Ingress) error

type IngressWatcher struct {
	handler   IngressHandler
	informer  cache.SharedIndexInformer
	namespace string
}

func (w *IngressWatcher) HasSynced() func() bool {
	return w.informer.HasSynced
}

func (w *IngressWatcher) Handle(event ResourceChange) error {
	obj, err := w.Get(event.Key)
	if err != nil {
		return err
	}
	return w.handler(event.Key, obj)
}

func (w *IngressWatcher) Describe(event ResourceChange) string {
	return fmt.Sprintf("Ingress %s", event.Key)
}

func (w *IngressWatcher) Start(stopCh <-chan struct{}) {
	go w.informer.Run(stopCh)
}

func (w *IngressWatcher) Sync(stopCh <-chan struct{}) bool {
	return cache.WaitForCacheSync(stopCh, w.informer.HasSynced)
}

func (w *IngressWatcher) Get(key string) (*networkingv1.Ingress, error) {
	entity, exists, err := w.informer.GetStore().GetByKey(key)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	return entity.(*networkingv1.Ingress), nil
}

func (w *IngressWatcher) List() []*networkingv1.Ingress {
	list := w.informer.GetStore().List()
	results := []*networkingv1.Ingress{}
	for _, o := range list {
		results = append(results, o.(*networkingv1.Ingress))
	}
	return results
}

func (c *EventProcessor) WatchRoutes(options routev1interfaces.TweakListOptionsFunc, namespace string, handler RouteHandler) *RouteWatcher {
	if c.routeClient == nil {
		return nil
	}
	watcher := &RouteWatcher{
		handler: handler,
		informer: routev1informer.NewFilteredRouteInformer(
			c.routeClient,
			namespace,
			c.resync,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
			options),
		namespace: namespace,
	}

	watcher.informer.AddEventHandler(c.newEventHandler(watcher))
	c.addWatcher(watcher)
	return watcher
}

type RouteHandler func(string, *routev1.Route) error

type RouteWatcher struct {
	handler   RouteHandler
	informer  cache.SharedIndexInformer
	namespace string
}

func (w *RouteWatcher) HasSynced() func() bool {
	return w.informer.HasSynced
}

func (w *RouteWatcher) Handle(event ResourceChange) error {
	obj, err := w.Get(event.Key)
	if err != nil {
		return err
	}
	return w.handler(event.Key, obj)
}

func (w *RouteWatcher) Describe(event ResourceChange) string {
	return fmt.Sprintf("Route %s", event.Key)
}

func (w *RouteWatcher) Start(stopCh <-chan struct{}) {
	go w.informer.Run(stopCh)
}

func (w *RouteWatcher) Sync(stopCh <-chan struct{}) bool {
	return cache.WaitForCacheSync(stopCh, w.informer.HasSynced)
}

func (w *RouteWatcher) Get(key string) (*routev1.Route, error) {
	entity, exists, err := w.informer.GetStore().GetByKey(key)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	return entity.(*routev1.Route), nil
}

func (w *RouteWatcher) List() []*routev1.Route {
	list := w.informer.GetStore().List()
	results := []*routev1.Route{}
	for _, o := range list {
		results = append(results, o.(*routev1.Route))
	}
	return results
}

func (c *EventProcessor) WatchCertificates(namespace string, handler CertificateHandler) *CertificateWatcher {
	watcher := &CertificateWatcher{
		handler: handler,
		informer: skupperv2alpha1informer.NewCertificateInformer(
			c.skupperClient,
			namespace,
			time.Second*30,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}),
		namespace: namespace,
	}
	watcher.informer.AddEventHandler(c.newEventHandler(watcher))
	c.addWatcher(watcher)
	return watcher
}

type CertificateHandler func(string, *skupperv2alpha1.Certificate) error

type CertificateWatcher struct {
	handler   CertificateHandler
	informer  cache.SharedIndexInformer
	namespace string
}

func (w *CertificateWatcher) Handle(event ResourceChange) error {
	obj, err := w.Get(event.Key)
	if err != nil {
		return err
	}
	return w.handler(event.Key, obj)
}

func (w *CertificateWatcher) HasSynced() func() bool {
	return w.informer.HasSynced
}

func (w *CertificateWatcher) Describe(event ResourceChange) string {
	return fmt.Sprintf("Certificate %s", event.Key)
}

func (w *CertificateWatcher) Start(stopCh <-chan struct{}) {
	go w.informer.Run(stopCh)
}

func (w *CertificateWatcher) Sync(stopCh <-chan struct{}) bool {
	return cache.WaitForCacheSync(stopCh, w.informer.HasSynced)
}

func (w *CertificateWatcher) Get(key string) (*skupperv2alpha1.Certificate, error) {
	entity, exists, err := w.informer.GetStore().GetByKey(key)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	return entity.(*skupperv2alpha1.Certificate), nil
}

func (w *CertificateWatcher) List() []*skupperv2alpha1.Certificate {
	list := w.informer.GetStore().List()
	results := []*skupperv2alpha1.Certificate{}
	for _, o := range list {
		results = append(results, o.(*skupperv2alpha1.Certificate))
	}
	return results
}

func (c *EventProcessor) WatchRouterAccesses(namespace string, handler RouterAccessHandler) *RouterAccessWatcher {
	watcher := &RouterAccessWatcher{
		handler: handler,
		informer: skupperv2alpha1informer.NewRouterAccessInformer(
			c.skupperClient,
			namespace,
			time.Second*30,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}),
		namespace: namespace,
	}
	watcher.informer.AddEventHandler(c.newEventHandler(watcher))
	c.addWatcher(watcher)
	return watcher
}

type RouterAccessHandler func(string, *skupperv2alpha1.RouterAccess) error

type RouterAccessWatcher struct {
	handler   RouterAccessHandler
	informer  cache.SharedIndexInformer
	namespace string
}

func (w *RouterAccessWatcher) Handle(event ResourceChange) error {
	obj, err := w.Get(event.Key)
	if err != nil {
		return err
	}
	return w.handler(event.Key, obj)
}

func (w *RouterAccessWatcher) HasSynced() func() bool {
	return w.informer.HasSynced
}

func (w *RouterAccessWatcher) Describe(event ResourceChange) string {
	return fmt.Sprintf("RouterAccess %s", event.Key)
}

func (w *RouterAccessWatcher) Start(stopCh <-chan struct{}) {
	go w.informer.Run(stopCh)
}

func (w *RouterAccessWatcher) Sync(stopCh <-chan struct{}) bool {
	return cache.WaitForCacheSync(stopCh, w.informer.HasSynced)
}

func (w *RouterAccessWatcher) Get(key string) (*skupperv2alpha1.RouterAccess, error) {
	entity, exists, err := w.informer.GetStore().GetByKey(key)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	return entity.(*skupperv2alpha1.RouterAccess), nil
}

func (w *RouterAccessWatcher) List() []*skupperv2alpha1.RouterAccess {
	list := w.informer.GetStore().List()
	results := []*skupperv2alpha1.RouterAccess{}
	for _, o := range list {
		results = append(results, o.(*skupperv2alpha1.RouterAccess))
	}
	return results
}

func ByName(name string) internalinterfaces.TweakListOptionsFunc {
	return func(options *metav1.ListOptions) {
		options.FieldSelector = "metadata.name=" + name
	}
}

func SkupperResourceByName(name string) skupperv2alpha1interfaces.TweakListOptionsFunc {
	return func(options *metav1.ListOptions) {
		options.FieldSelector = "metadata.name=" + name
	}
}

func (c *EventProcessor) WatchAttachedConnectorBindings(namespace string, handler AttachedConnectorBindingHandler) *AttachedConnectorBindingWatcher {
	watcher := &AttachedConnectorBindingWatcher{
		handler: handler,
		informer: skupperv2alpha1informer.NewAttachedConnectorBindingInformer(
			c.skupperClient,
			namespace,
			time.Second*30,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}),
		namespace: namespace,
	}
	watcher.informer.AddEventHandler(c.newEventHandler(watcher))
	c.addWatcher(watcher)
	return watcher
}

type AttachedConnectorBindingHandler func(string, *skupperv2alpha1.AttachedConnectorBinding) error

type AttachedConnectorBindingWatcher struct {
	handler   AttachedConnectorBindingHandler
	informer  cache.SharedIndexInformer
	namespace string
}

func (w *AttachedConnectorBindingWatcher) Handle(event ResourceChange) error {
	obj, err := w.Get(event.Key)
	if err != nil {
		return err
	}
	return w.handler(event.Key, obj)
}

func (w *AttachedConnectorBindingWatcher) HasSynced() func() bool {
	return w.informer.HasSynced
}

func (w *AttachedConnectorBindingWatcher) Describe(event ResourceChange) string {
	return fmt.Sprintf("AttachedConnectorBinding %s", event.Key)
}

func (w *AttachedConnectorBindingWatcher) Start(stopCh <-chan struct{}) {
	go w.informer.Run(stopCh)
}

func (w *AttachedConnectorBindingWatcher) Sync(stopCh <-chan struct{}) bool {
	return cache.WaitForCacheSync(stopCh, w.informer.HasSynced)
}

func (w *AttachedConnectorBindingWatcher) Get(key string) (*skupperv2alpha1.AttachedConnectorBinding, error) {
	entity, exists, err := w.informer.GetStore().GetByKey(key)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	return entity.(*skupperv2alpha1.AttachedConnectorBinding), nil
}

func (w *AttachedConnectorBindingWatcher) List() []*skupperv2alpha1.AttachedConnectorBinding {
	list := w.informer.GetStore().List()
	results := []*skupperv2alpha1.AttachedConnectorBinding{}
	for _, o := range list {
		results = append(results, o.(*skupperv2alpha1.AttachedConnectorBinding))
	}
	return results
}

func (c *EventProcessor) WatchAttachedConnectors(namespace string, handler AttachedConnectorHandler) *AttachedConnectorWatcher {
	watcher := &AttachedConnectorWatcher{
		handler: handler,
		informer: skupperv2alpha1informer.NewAttachedConnectorInformer(
			c.skupperClient,
			namespace,
			time.Second*30,
			cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}),
		namespace: namespace,
	}
	watcher.informer.AddEventHandler(c.newEventHandler(watcher))
	c.addWatcher(watcher)
	return watcher
}

type AttachedConnectorHandler func(string, *skupperv2alpha1.AttachedConnector) error

type AttachedConnectorWatcher struct {
	handler   AttachedConnectorHandler
	informer  cache.SharedIndexInformer
	namespace string
}

func (w *AttachedConnectorWatcher) Handle(event ResourceChange) error {
	obj, err := w.Get(event.Key)
	if err != nil {
		return err
	}
	return w.handler(event.Key, obj)
}

func (w *AttachedConnectorWatcher) HasSynced() func() bool {
	return w.informer.HasSynced
}

func (w *AttachedConnectorWatcher) Describe(event ResourceChange) string {
	return fmt.Sprintf("AttachedConnector %s", event.Key)
}

func (w *AttachedConnectorWatcher) Start(stopCh <-chan struct{}) {
	go w.informer.Run(stopCh)
}

func (w *AttachedConnectorWatcher) Sync(stopCh <-chan struct{}) bool {
	return cache.WaitForCacheSync(stopCh, w.informer.HasSynced)
}

func (w *AttachedConnectorWatcher) Get(key string) (*skupperv2alpha1.AttachedConnector, error) {
	entity, exists, err := w.informer.GetStore().GetByKey(key)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	return entity.(*skupperv2alpha1.AttachedConnector), nil
}

func (w *AttachedConnectorWatcher) List() []*skupperv2alpha1.AttachedConnector {
	list := w.informer.GetStore().List()
	results := []*skupperv2alpha1.AttachedConnector{}
	for _, o := range list {
		results = append(results, o.(*skupperv2alpha1.AttachedConnector))
	}
	return results
}

func FilterByNamespace[V any](match func(string) bool, handler func(string, V) error) func(string, V) error {
	if match == nil {
		return handler
	}
	return func(key string, value V) error {
		namespace, _, _ := cache.SplitMetaNamespaceKey(key)
		if match(namespace) {
			return handler(key, value)
		}
		return nil
	}
}
