package kieapp

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/RHsyseng/operator-utils/pkg/logs"
	"github.com/RHsyseng/operator-utils/pkg/olm"
	"github.com/RHsyseng/operator-utils/pkg/resource/compare"
	"github.com/RHsyseng/operator-utils/pkg/resource/read"
	"github.com/RHsyseng/operator-utils/pkg/resource/write"
	"github.com/RHsyseng/operator-utils/pkg/utils/kubernetes"
	api "github.com/kiegroup/kie-cloud-operator/pkg/apis/app/v2"
	"github.com/kiegroup/kie-cloud-operator/pkg/controller/kieapp/constants"
	"github.com/kiegroup/kie-cloud-operator/pkg/controller/kieapp/defaults"
	"github.com/kiegroup/kie-cloud-operator/pkg/controller/kieapp/shared"
	"github.com/kiegroup/kie-cloud-operator/pkg/controller/kieapp/status"
	oappsv1 "github.com/openshift/api/apps/v1"
	buildv1 "github.com/openshift/api/build/v1"
	consolev1 "github.com/openshift/api/console/v1"
	oimagev1 "github.com/openshift/api/image/v1"
	routev1 "github.com/openshift/api/route/v1"
	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	"golang.org/x/mod/semver"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var log = logs.GetLogger("kieapp.controller")

// Reconciler reconciles a KieApp object
type Reconciler struct {
	Service    kubernetes.PlatformService
	OcpVersion string
}

// Reconcile reads that state of the cluster for a KieApp object and makes changes based on the state read
// and what is in the KieApp.Spec
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (reconciler *Reconciler) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	// The next several lines only execute if the operator is running in a pod, via deployment.
	// Otherwise, embedded configs are used and no console is deployed.
	if opName, depNameSpace, useEmbedded := defaults.UseEmbeddedFiles(reconciler.Service); !useEmbedded {
		myDep := &appsv1.Deployment{}
		err := reconciler.Service.Get(ctx, types.NamespacedName{Namespace: depNameSpace, Name: opName}, myDep)
		if err == nil {
			if shouldDeployConsole() {
				deployConsole(reconciler, myDep)
			}
			reconciler.CreateConfigMaps(myDep)
		} else {
			log.Error("Can't properly create ConfigMaps. ", err)
		}
		if err = reconciler.createConsoleYAMLSamples(); err != nil {
			log.Error(err)
		}
	}

	// Fetch the KieApp instance
	instance := &api.KieApp{}
	err := reconciler.Service.Get(ctx, request.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			log.Infof("No Custom Resource found named %s. Checking for dependent objects to delete.", request.Name)
			instance.ObjectMeta = metav1.ObjectMeta{
				Name:      request.Name,
				Namespace: request.Namespace,
			}
			deployed, err := reconciler.getDeployedResources(instance)
			if err != nil {
				return reconcile.Result{}, err
			}
			_, err = reconciler.reconcileResources(instance, nil, deployed)
			return reconcile.Result{}, err
		}
		// Error reading the object - requeue the request.
		reconciler.setFailedStatus(instance, api.UnknownReason, err)
		return reconcile.Result{}, err
	}

	//Obtain in-memory representation of basic environment being requested:
	env, err := defaults.GetEnvironment(instance, reconciler.Service)
	if err != nil {
		reconciler.setFailedStatus(instance, api.ConfigurationErrorReason, err)
		return reconcile.Result{}, err
	}

	//Verify the external references exist
	err = reconciler.verifyExternalReferences(instance)
	if err != nil {
		reconciler.setFailedStatus(instance, api.MissingDependenciesReason, err)
		return reconcile.Result{}, err
	}

	//Get requested routes based on environment template:
	requestedRoutes := getRequestedRoutes(env, instance)
	//Then check if all these routes are already created:
	reader := read.New(reconciler.Service).WithNamespace(instance.Namespace).WithOwnerObject(instance)
	deployedRoutes, err := reader.List(&routev1.RouteList{})
	if err != nil {
		return reconcile.Result{}, err
	}
	delta := compare.DefaultComparator().CompareArrays(deployedRoutes, requestedRoutes)

	//If not all routes are created, then create the missing ones. Other changes can be applied later.
	if len(delta.Added) > 0 {
		log.Debugf("Will create %d routes that were not found", len(delta.Added))
		writer := write.New(reconciler.Service).WithOwnerController(instance, reconciler.Service.GetScheme())
		added, err := writer.AddResources(delta.Added)
		if err != nil {
			return reconcile.Result{}, err
		} else if added {
			//Requeue after a little while to load route and its hostname
			return reconcile.Result{Requeue: true, RequeueAfter: time.Duration(500) * time.Millisecond}, err
		}
	}

	caConfigMap := &corev1.ConfigMap{}
	if defaults.IsOcpCA(instance) {
		if err := reconciler.Service.Get(ctx, types.NamespacedName{Name: instance.Name + "-kieapp-ca-bundle", Namespace: instance.Namespace}, caConfigMap); err != nil {
			if !errors.IsNotFound(err) {
				return reconcile.Result{Requeue: true, RequeueAfter: time.Duration(500) * time.Millisecond}, err
			}
		}
	}

	//With route hostnames now available, set remaining environment configuration:
	env, err = reconciler.setEnvironmentProperties(instance, env, deployedRoutes, caConfigMap)
	if err != nil {
		// requeue if secret request throws an error
		// we shouldn't reconcile the deployment with an incorrect or missing keystore secret
		return reconcile.Result{Requeue: true, RequeueAfter: time.Duration(500) * time.Millisecond}, err
	}
	//Create a list of objects that should be deployed
	requestedResources := reconciler.getKubernetesResources(instance, env)
	for index := range requestedResources {
		if isNamespaced(requestedResources[index]) {
			requestedResources[index].SetNamespace(instance.Namespace)
		}
	}

	//Obtain a list of objects that are actually deployed
	deployed, err := reconciler.getDeployedResources(instance)
	if err != nil {
		reconciler.setFailedStatus(instance, api.UnknownReason, err)
		return reconcile.Result{}, err
	}
	setDeploymentStatus(instance, deployed[reflect.TypeOf(oappsv1.DeploymentConfig{})])

	hasUpdates, err := reconciler.reconcileResources(instance, requestedResources, deployed)
	if err != nil {
		return reconcile.Result{}, err
	}

	// Check the KieServer ConfigMaps for necessary changes
	reconciler.checkKieServerConfigMap(instance, env)

	// Fetch the cached KieApp instance
	cachedInstance := &api.KieApp{}
	err = reconciler.Service.GetCached(ctx, request.NamespacedName, cachedInstance)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		reconciler.setFailedStatus(instance, api.UnknownReason, err)
		return reconcile.Result{}, err
	}

	// Update CR Status if needed
	return reconciler.checkStatus(ctx, instance, cachedInstance, hasUpdates)
}

func (reconciler *Reconciler) checkStatus(ctx context.Context, instance, cachedInstance *api.KieApp, hasUpdates bool) (reconcile.Result, error) {
	var requeue bool
	if hasUpdates {
		requeue = status.SetProvisioning(instance)
	} else {
		requeue = status.SetDeployed(instance)
	}
	return reconciler.updateStatus(ctx, instance, cachedInstance, requeue)
}

func (reconciler *Reconciler) updateStatus(ctx context.Context, instance, cachedInstance *api.KieApp, requeue bool) (reconcile.Result, error) {
	if reconciler.hasStatusChanges(instance, cachedInstance) {
		if instance.ResourceVersion == cachedInstance.ResourceVersion {
			if err := reconciler.Service.Status().Update(ctx, instance); err != nil {
				return reconcile.Result{}, err
			}
		} else {
			return reconcile.Result{Requeue: true}, nil
		}
	}
	return reconcile.Result{Requeue: requeue}, nil
}

func (reconciler *Reconciler) reconcileResources(ownerController metav1.Object, requestedResources []client.Object, deployed map[reflect.Type][]client.Object) (bool, error) {
	writer := write.New(reconciler.Service).WithOwnerController(ownerController, reconciler.Service.GetScheme())
	//Compare what's deployed with what should be deployed
	requested := compare.NewMapBuilder().Add(requestedResources...).ResourceMap()
	comparator := getComparator()
	deltas := comparator.Compare(deployed, requested)
	var hasUpdates bool
	for resourceType, delta := range deltas {
		if !delta.HasChanges() {
			continue
		}
		log.Debugf("Will create %d, update %d, and delete %d instances of %v", len(delta.Added), len(delta.Updated), len(delta.Removed), resourceType)
		added, err := writer.AddResources(delta.Added)
		if err != nil {
			return false, err
		}
		updated, err := writer.UpdateResources(deployed[resourceType], delta.Updated)
		if err != nil {
			return false, err
		}
		removed, err := writer.RemoveResources(delta.Removed)
		if err != nil {
			return false, err
		}
		hasUpdates = hasUpdates || added || updated || removed
	}
	return hasUpdates, nil
}

func isNamespaced(resource client.Object) bool {
	if reflect.TypeOf(resource) == reflect.TypeOf(&consolev1.ConsoleLink{}) {
		return false
	}
	return true
}

func getComparator() compare.MapComparator {
	resourceComparator := compare.DefaultComparator()
	dcType := reflect.TypeOf(oappsv1.DeploymentConfig{})
	defaultDCComparator := resourceComparator.GetComparator(dcType)
	resourceComparator.SetComparator(dcType, func(deployed client.Object, requested client.Object) bool {
		dc1 := deployed.(*oappsv1.DeploymentConfig)
		dc2 := requested.(*oappsv1.DeploymentConfig)
		for i := range dc1.Spec.Triggers {
			if len(dc2.Spec.Triggers) <= i {
				return false
			}
			trigger1 := dc1.Spec.Triggers[i]
			trigger2 := dc2.Spec.Triggers[i]
			if trigger2.ImageChangeParams != nil && trigger1.ImageChangeParams != nil {
				if trigger2.ImageChangeParams.From.Namespace == "" {
					//This value is generated based on image stream being found in current or openshift project:
					trigger1.ImageChangeParams.From.Namespace = ""
				}
			}
		}
		return defaultDCComparator(deployed, requested)
	})

	bcType := reflect.TypeOf(buildv1.BuildConfig{})
	defaultBCComparator := resourceComparator.GetComparator(bcType)
	resourceComparator.SetComparator(bcType, func(deployed client.Object, requested client.Object) bool {
		bc1 := deployed.(*buildv1.BuildConfig)
		bc2 := requested.(*buildv1.BuildConfig).DeepCopy()
		if bc1.Spec.Strategy.SourceStrategy != nil {
			//This value is generated based on image stream being found in current or openshift project:
			bc1.Spec.Strategy.SourceStrategy.From.Namespace = bc2.Spec.Strategy.SourceStrategy.From.Namespace
		}
		if len(bc1.Spec.Triggers) > 0 && len(bc2.Spec.Triggers) == 0 {
			//Triggers are generated based on provided github repo
			bc1.Spec.Triggers = bc2.Spec.Triggers
		}
		return defaultBCComparator(deployed, requested)
	})

	configMapType := reflect.TypeOf(corev1.ConfigMap{})
	resourceComparator.SetComparator(configMapType, func(deployed client.Object, requested client.Object) bool {
		configMap1 := deployed.(*corev1.ConfigMap)
		configMap2 := requested.(*corev1.ConfigMap)
		var pairs [][2]interface{}
		pairs = append(pairs, [2]interface{}{configMap1.Name, configMap2.Name})
		pairs = append(pairs, [2]interface{}{configMap1.Namespace, configMap2.Namespace})
		pairs = append(pairs, [2]interface{}{configMap1.Labels, configMap2.Labels})
		pairs = append(pairs, [2]interface{}{configMap1.Annotations, configMap2.Annotations})
		if configMap1.Labels["config.openshift.io/inject-trusted-cabundle"] != "true" {
			pairs = append(pairs, [2]interface{}{configMap1.Data, configMap2.Data})
			pairs = append(pairs, [2]interface{}{configMap1.BinaryData, configMap2.BinaryData})
		}
		equal := compare.EqualPairs(pairs)
		if !equal {
			log.Infof("Resources are not equal -- deployed %+v -- requested %+v", deployed, requested)
		}
		return equal
	})

	return compare.MapComparator{Comparator: resourceComparator}
}

func setDeploymentStatus(instance *api.KieApp, resources []client.Object) {
	var dcs []oappsv1.DeploymentConfig
	for index := range resources {
		dc := resources[index].(*oappsv1.DeploymentConfig)
		dcs = append(dcs, *dc)
	}
	instance.Status.Deployments = olm.GetDeploymentConfigStatus(dcs)
}

func (reconciler *Reconciler) verifyExternalReferences(cr *api.KieApp) error {
	var err error

	if cr.Status.Applied.Auth != nil && cr.Status.Applied.Auth.RoleMapper != nil {
		err = reconciler.verifyExternalReference(cr.GetNamespace(), cr.Status.Applied.Auth.RoleMapper.From)
	}
	if cr.Status.Applied.Objects.Console != nil {
		if err == nil && cr.Status.Applied.Objects.Console.GitHooks != nil {
			err = reconciler.verifyExternalReference(cr.GetNamespace(), cr.Status.Applied.Objects.Console.GitHooks.From)
		}
	}
	return err
}

func (reconciler *Reconciler) verifyExternalReference(namespace string, ref *api.ObjRef) error {
	if ref == nil {
		return nil
	}
	name := types.NamespacedName{
		Name:      ref.Name,
		Namespace: namespace,
	}
	allowedObjects := []client.Object{&corev1.ConfigMap{}, &corev1.Secret{}, &corev1.PersistentVolumeClaim{}}
	for _, obj := range allowedObjects {
		expected := reflect.Indirect(reflect.ValueOf(obj)).Type().Name()
		if ref.Kind == expected {
			log.Debugf("Get external reference %s", ref)
			return reconciler.Service.Get(context.TODO(), name, obj)
		}
	}
	return fmt.Errorf("unsupported Kind: %s", ref.Kind)
}

func getRequestedRoutes(env api.Environment, instance *api.KieApp) []client.Object {
	//Derive routes that should be created:
	objects := filterOmittedObjects(getCustomObjects(env))
	var requestedRoutes []client.Object
	for i := range objects {
		for j := range objects[i].Routes {
			route := &objects[i].Routes[j]
			route.SetGroupVersionKind(routev1.GroupVersion.WithKind("Route"))
			route.SetNamespace(instance.GetNamespace())
			requestedRoutes = append(requestedRoutes, route)
		}
	}
	return requestedRoutes
}

func (reconciler *Reconciler) hasStatusChanges(instance, cachedInstance *api.KieApp) bool {
	if !reflect.DeepEqual(instance.Status, cachedInstance.Status) {
		return true
	}
	if len(instance.Status.Applied.Objects.Servers) > 0 {
		if len(instance.Status.Applied.Objects.Servers) != len(cachedInstance.Status.Applied.Objects.Servers) {
			return true
		}
		for i := range instance.Status.Applied.Objects.Servers {
			if !reflect.DeepEqual(instance.Status.Applied.Objects.Servers[i], cachedInstance.Status.Applied.Objects.Servers[i]) {
				return true
			}
		}
	}
	return false
}

func (reconciler *Reconciler) setFailedStatus(instance *api.KieApp, reason api.ReasonType, err error) {
	status.SetFailed(instance, reason, err)
	if updateError := reconciler.Service.Status().Update(context.TODO(), instance); updateError != nil {
		log.Warn("Unable to update object after receiving failed status. ", err)
	}
}

// Check ImageStream
func (reconciler *Reconciler) checkImageStreamTag(name, namespace string) bool {
	log := log.With("kind", "ImageStreamTag", "name", name, "namespace", namespace)
	result := strings.Split(name, ":")
	if len(result) == 1 {
		result = append(result, "latest")
	}
	tagName := fmt.Sprintf("%s:%s", result[0], result[1])
	_, err := reconciler.Service.ImageStreamTags(namespace).Get(context.TODO(), tagName, metav1.GetOptions{})
	if err != nil {
		log.Debug("Object does not exist")
		return false
	}
	return true
}

// Create local ImageStreamTag
func (reconciler *Reconciler) createLocalImageTag(tagRefName, imageURL string, cr *api.KieApp) error {
	result := strings.Split(tagRefName, ":")
	if len(result) == 1 {
		result = append(result, "latest")
	}
	product := defaults.GetProduct(cr)
	tagName := fmt.Sprintf("%s:%s", result[0], result[1])
	imageName := tagName
	major, _, _ := defaults.GetMajorMinorMicro(cr.Status.Applied.Version)
	regContext := fmt.Sprintf("%s-%s", product, major)
	if _, _, imageContext := defaults.GetImage(imageURL); imageContext != "" {
		regContext = imageContext
	}

	// default registry settings
	registry := &api.KieAppRegistry{
		Insecure: logs.GetBoolEnv("INSECURE"),
	}
	if cr.Status.Applied.ImageRegistry != nil {
		registry = cr.Status.Applied.ImageRegistry
	}
	if registry.Registry == "" {
		registry.Registry = logs.GetEnv("REGISTRY", constants.ImageRegistry)
	}
	registryAddress := registry.Registry
	if strings.Contains(result[0], "datagrid") {
		registryAddress = constants.ImageRegistry
		regContext = "jboss-datagrid-7"
	} else if strings.Contains(result[0], "amq-broker-7") {
		registryAddress = constants.ImageRegistry
		regContext = "amq-broker-7"
		if strings.Contains(result[0], "scaledown") {
			regContext = "amq-broker-7-tech-preview"
		}
	} else if result[0] == "postgresql" || result[0] == "mysql" {
		registryAddress = constants.ImageRegistry
		regContext = "rhscl"
		pattern := regexp.MustCompile("[0-9]+")
		imageName = fmt.Sprintf("%s-%s-rhel7:%s", result[0], strings.Join(pattern.FindAllString(result[1], -1), ""), "latest")
	}
	registryURL := fmt.Sprintf("%s/%s/%s", registryAddress, regContext, imageName)

	isnew := &oimagev1.ImageStreamTag{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tagName,
			Namespace: cr.Namespace,
		},
		Tag: &oimagev1.TagReference{
			Name: result[1],
			From: &corev1.ObjectReference{
				Kind: "DockerImage",
				Name: registryURL,
			},
			ReferencePolicy: oimagev1.TagReferencePolicy{
				Type: oimagev1.LocalTagReferencePolicy,
			},
		},
	}
	isnew.SetGroupVersionKind(oimagev1.GroupVersion.WithKind("ImageStreamTag"))
	if registry.Insecure {
		isnew.Tag.ImportPolicy = oimagev1.TagImportPolicy{
			Insecure: true,
		}
	}
	/*
	   https://issues.redhat.com/browse/RHPAM-4167
	   If we are using ImageTags we can set scheduled at true for ImportPolicy
	*/
	if cr.Spec.UseImageTags && cr.Spec.ScheduledImportPolicy {
		isnew.Tag.ImportPolicy.Scheduled = true
	}
	log := log.With("kind", isnew.GetObjectKind().GroupVersionKind().Kind, "name", isnew.Name, "from", isnew.Tag.From.Name, "namespace", isnew.Namespace)
	log.Info("Creating")
	_, err := reconciler.Service.ImageStreamTags(isnew.Namespace).Create(context.TODO(), isnew, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		log.Error("Issue creating object. ", err)
		return err
	}
	return nil
}

//loadRoutes attempts to load as many of the specified routes as it can find
func (reconciler *Reconciler) loadRoutes(requestedRoutes []client.Object) (map[types.NamespacedName]routev1.Route, error) {
	deployedRoutes := make(map[types.NamespacedName]routev1.Route)
	for _, requested := range requestedRoutes {
		namespacedName := shared.GetNamespacedName(requested)
		deployed := routev1.Route{}
		err := reconciler.Service.Get(context.TODO(), namespacedName, &deployed)
		if err == nil {
			deployedRoutes[namespacedName] = deployed
		} else if !errors.IsNotFound(err) {
			return nil, err
		}
	}
	return deployedRoutes, nil
}

func (reconciler *Reconciler) setEnvironmentProperties(cr *api.KieApp, env api.Environment, routes []client.Object, caConfigMap *corev1.ConfigMap) (api.Environment, error) {
	if defaults.IsOcpCA(cr) {
		secret, err := reconciler.generateTruststoreSecret(
			cr.Status.Applied.CommonConfig.ApplicationName+constants.TruststoreSecret,
			cr,
			caConfigMap,
		)
		if err != nil {
			return api.Environment{}, err
		}
		env.Others[0].Secrets = append(env.Others[0].Secrets, secret)
	}

	// console keystore generation
	if !env.Console.Omit {
		consoleCN := reconciler.setConsoleHost(cr, env, routes)
		defaults.ConfigureHostname(&env.Console, cr, consoleCN)
		if cr.Status.Applied.Objects.Console.KeystoreSecret == "" && !cr.Status.Applied.CommonConfig.DisableSsl {
			secret, err := reconciler.generateKeystoreSecret(
				fmt.Sprintf(constants.KeystoreSecret, strings.Join([]string{cr.Status.Applied.CommonConfig.ApplicationName, "businesscentral"}, "-")),
				consoleCN,
				cr,
			)
			if err != nil {
				return api.Environment{}, err
			}
			env.Console.Secrets = append(env.Console.Secrets, secret)
		}
	}

	// dashbuilder keystore generation
	if cr.Status.Applied.Objects.Dashbuilder != nil && !cr.Status.Applied.CommonConfig.DisableSsl {
		consoleCN := reconciler.setConsoleHost(cr, env, routes)
		defaults.ConfigureHostname(&env.Dashbuilder, cr, consoleCN)
		if cr.Status.Applied.Objects.Dashbuilder.KeystoreSecret == "" {
			secret, err := reconciler.generateKeystoreSecret(
				fmt.Sprintf(constants.KeystoreSecret, strings.Join([]string{cr.Status.Applied.CommonConfig.ApplicationName, "dashbuilder"}, "-")),
				consoleCN,
				cr,
			)
			if err != nil {
				return api.Environment{}, err
			}
			env.Dashbuilder.Secrets = append(env.Dashbuilder.Secrets, secret)
		}
	}

	// server(s) keystore generation
	for i, server := range env.Servers {
		if server.Omit {
			break
		}
		serverCN := ""
		for _, rt := range server.Routes {
			if checkTLS(rt.Spec.TLS) {
				// use host of first tls route in env template
				serverCN = reconciler.GetRouteHost(rt, routes)
				break
			}
		}
		if serverCN == "" {
			serverCN = cr.Status.Applied.CommonConfig.ApplicationName
		}
		defaults.ConfigureHostname(&server, cr, serverCN)
		serverSet, kieDeploymentName := defaults.GetServerSet(cr, i)
		if serverSet.KeystoreSecret == "" && !cr.Status.Applied.CommonConfig.DisableSsl {
			secret, err := reconciler.generateKeystoreSecret(
				fmt.Sprintf(constants.KeystoreSecret, kieDeploymentName),
				serverCN,
				cr,
			)
			if err != nil {
				return api.Environment{}, err
			}
			server.Secrets = append(server.Secrets, secret)
		}
		env.Servers[i] = server
	}

	// smartrouter keystore generation
	if !env.SmartRouter.Omit {
		smartCN := ""
		for _, rt := range env.SmartRouter.Routes {
			if checkTLS(rt.Spec.TLS) {
				// use host of first tls route in env template
				smartCN = reconciler.GetRouteHost(rt, routes)
				break
			}
		}
		if smartCN == "" {
			smartCN = cr.Status.Applied.CommonConfig.ApplicationName
		}

		defaults.ConfigureHostname(&env.SmartRouter, cr, smartCN)
		if (cr.Status.Applied.Objects.SmartRouter == nil || cr.Status.Applied.Objects.SmartRouter.KeystoreSecret == "") && !cr.Status.Applied.CommonConfig.DisableSsl {
			secret, err := reconciler.generateKeystoreSecret(
				fmt.Sprintf(constants.KeystoreSecret, strings.Join([]string{cr.Status.Applied.CommonConfig.ApplicationName, "smartrouter"}, "-")),
				smartCN,
				cr,
			)
			if err != nil {
				return api.Environment{}, err
			}
			env.SmartRouter.Secrets = append(env.SmartRouter.Secrets, secret)
		}
	}
	return defaults.ConsolidateObjects(env, cr), nil
}

func (reconciler *Reconciler) setConsoleHost(cr *api.KieApp, env api.Environment, routes []client.Object) (consoleCN string) {

	if cr.Status.Applied.Environment == api.RhpamStandaloneDashbuilder {
		for _, rt := range env.Dashbuilder.Routes {
			if checkTLS(rt.Spec.TLS) {
				// use host of first tls route in env template
				consoleCN = reconciler.GetRouteHost(rt, routes)
				cr.Status.ConsoleHost = fmt.Sprintf("https://%s", consoleCN)
				log.Debug("Set dashbuilder console host to: ", cr.Status.ConsoleHost)
				break
			}
		}
	} else {
		for _, rt := range env.Console.Routes {
			if checkTLS(rt.Spec.TLS) {
				// use host of first tls route in env template
				consoleCN = reconciler.GetRouteHost(rt, routes)
				cr.Status.ConsoleHost = fmt.Sprintf("https://%s", consoleCN)
				log.Debug("Set console host to: ", cr.Status.ConsoleHost)
				break
			}
		}
	}

	if consoleCN == "" {
		consoleCN = cr.Status.Applied.CommonConfig.ApplicationName
		cr.Status.ConsoleHost = fmt.Sprintf("http://%s", consoleCN)
	}
	return consoleCN
}

func (reconciler *Reconciler) getKubernetesResources(cr *api.KieApp, env api.Environment) []client.Object {
	var resources []client.Object
	objects := filterOmittedObjects(getCustomObjects(env))
	objects = reconciler.imageStreams(cr, objects)
	for _, obj := range objects {
		resources = append(resources, reconciler.getCustomObjectResources(obj, cr)...)
	}
	consoleLink := reconciler.getConsoleLinkResource(cr)
	if consoleLink != nil {
		resources = append(resources, consoleLink)
	}
	return resources
}

func (reconciler *Reconciler) imageStreams(cr *api.KieApp, objects []api.CustomObject) []api.CustomObject {
	for i, object := range objects {
		// DC logic
		if len(object.BuildConfigs) == 0 {
			for index := range object.DeploymentConfigs {
				for ti, trigger := range object.DeploymentConfigs[index].Spec.Triggers {
					if trigger.Type == oappsv1.DeploymentTriggerOnImageChange {
						for _, containerName := range trigger.ImageChangeParams.ContainerNames {
							for _, container := range object.DeploymentConfigs[index].Spec.Template.Spec.Containers {
								if container.Name == containerName {
									objects[i].DeploymentConfigs[index].Spec.Triggers[ti].ImageChangeParams.From.Namespace, _ = reconciler.ensureImageStream(
										trigger.ImageChangeParams.From.Name,
										trigger.ImageChangeParams.From.Namespace,
										container.Image,
										cr,
									)
								}
							}
						}
					}
				}
			}
		}
		// BC logic
		for index := range object.BuildConfigs {
			if object.BuildConfigs[index].Spec.Strategy.Type == buildv1.SourceBuildStrategyType {
				objects[i].BuildConfigs[index].Spec.Strategy.SourceStrategy.From.Namespace, _ = reconciler.ensureImageStream(
					object.BuildConfigs[index].Spec.Strategy.SourceStrategy.From.Name,
					object.BuildConfigs[index].Spec.Strategy.SourceStrategy.From.Namespace,
					"",
					cr,
				)
			}
		}
	}
	return objects
}

func getCustomObjects(env api.Environment) []api.CustomObject {
	var objects []api.CustomObject
	objects = append(objects, env.Console)
	objects = append(objects, env.Dashbuilder)
	objects = append(objects, env.Servers...)
	objects = append(objects, env.SmartRouter)
	objects = append(objects, env.ProcessMigration)
	objects = append(objects, env.Databases...)
	objects = append(objects, env.Others...)
	return objects
}

func filterOmittedObjects(objects []api.CustomObject) []api.CustomObject {
	var objs []api.CustomObject
	for index := range objects {
		if !objects[index].Omit {
			objs = append(objs, objects[index])
		}
	}
	return objs
}

func (reconciler *Reconciler) generateKeystoreSecret(secretName, keystoreCN string, cr *api.KieApp) (secret corev1.Secret, err error) {
	existingSecret := corev1.Secret{}
	err = reconciler.Service.Get(context.TODO(), types.NamespacedName{Name: secretName, Namespace: cr.Namespace}, &existingSecret)
	if err != nil && !errors.IsNotFound(err) {
		return secret, err
	}
	keyStorePassword := []byte(cr.Status.Applied.CommonConfig.KeyStorePassword)
	if ok, _ := shared.IsValidKeyStoreSecret(existingSecret, keystoreCN, keyStorePassword); ok {
		secret = existingSecret
	} else {
		keystoreByte, err := shared.GenerateKeystore(keystoreCN, keyStorePassword)
		if err != nil {
			return secret, err
		}
		secret = corev1.Secret{
			Type: corev1.SecretTypeOpaque,
			ObjectMeta: metav1.ObjectMeta{
				Name: secretName,
				Labels: map[string]string{
					"app":         cr.Status.Applied.CommonConfig.ApplicationName,
					"application": cr.Status.Applied.CommonConfig.ApplicationName,
				},
			},
			Data: map[string][]byte{
				constants.KeystoreName: keystoreByte,
			},
		}
		secret.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
	}

	return secret, nil
}

func (reconciler *Reconciler) generateTruststoreSecret(secretName string, cr *api.KieApp, caConfigMap *corev1.ConfigMap) (secret corev1.Secret, err error) {
	// add truststore to secret if ca bundle exists in configmap
	if len(caConfigMap.Data) > 0 && caConfigMap.Data[constants.CaBundleKey] != "" {
		existingSecret := corev1.Secret{}
		err = reconciler.Service.Get(context.TODO(), types.NamespacedName{Name: secretName, Namespace: cr.Namespace}, &existingSecret)
		if err != nil && !errors.IsNotFound(err) {
			return secret, err
		}
		caBundle := []byte(caConfigMap.Data[constants.CaBundleKey])
		if ok, _ := shared.IsValidTruststoreSecret(existingSecret, caBundle); ok {
			secret = existingSecret
		} else {
			truststoreByte, err := shared.GenerateTruststore(caBundle)
			if err != nil {
				return secret, err
			}
			secret = corev1.Secret{
				Type: corev1.SecretTypeOpaque,
				ObjectMeta: metav1.ObjectMeta{
					Name: secretName,
					Labels: map[string]string{
						"app":         cr.Status.Applied.CommonConfig.ApplicationName,
						"application": cr.Status.Applied.CommonConfig.ApplicationName,
					},
				},
				Data: map[string][]byte{
					constants.TruststoreName: truststoreByte,
				},
			}
			secret.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
		}
	}

	return secret, nil
}

// getCustomObjectResources returns all kubernetes resources that need to be created for the given CustomObject
func (reconciler *Reconciler) getCustomObjectResources(object api.CustomObject, cr *api.KieApp) []client.Object {
	var allObjects []client.Object
	if object.Omit {
		return allObjects
	}
	for index := range object.PersistentVolumeClaims {
		object.PersistentVolumeClaims[index].SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("PersistentVolumeClaim"))
		allObjects = append(allObjects, &object.PersistentVolumeClaims[index])
	}
	for index := range object.ServiceAccounts {
		object.ServiceAccounts[index].SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("ServiceAccount"))
		allObjects = append(allObjects, &object.ServiceAccounts[index])
	}
	for index := range object.Secrets {
		object.Secrets[index].SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
		allObjects = append(allObjects, &object.Secrets[index])
	}
	for index := range object.Roles {
		object.Roles[index].SetGroupVersionKind(rbacv1.SchemeGroupVersion.WithKind("Role"))
		allObjects = append(allObjects, &object.Roles[index])
	}
	for index := range object.RoleBindings {
		object.RoleBindings[index].SetGroupVersionKind(rbacv1.SchemeGroupVersion.WithKind("RoleBinding"))
		allObjects = append(allObjects, &object.RoleBindings[index])
	}
	for index := range object.DeploymentConfigs {
		object.DeploymentConfigs[index].SetGroupVersionKind(oappsv1.GroupVersion.WithKind("DeploymentConfig"))
		allObjects = append(allObjects, &object.DeploymentConfigs[index])
	}
	for index := range object.Services {
		object.Services[index].SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Service"))
		allObjects = append(allObjects, &object.Services[index])
	}
	for index := range object.StatefulSets {
		object.StatefulSets[index].SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind("StatefulSet"))
		allObjects = append(allObjects, &object.StatefulSets[index])
	}
	for index := range object.Routes {
		object.Routes[index].SetGroupVersionKind(routev1.GroupVersion.WithKind("Route"))
		allObjects = append(allObjects, &object.Routes[index])
	}
	for index := range object.ImageStreams {
		object.ImageStreams[index].SetGroupVersionKind(oimagev1.GroupVersion.WithKind("ImageStream"))
		allObjects = append(allObjects, &object.ImageStreams[index])
	}
	for index := range object.BuildConfigs {
		object.BuildConfigs[index].SetGroupVersionKind(buildv1.GroupVersion.WithKind("BuildConfig"))
		allObjects = append(allObjects, &object.BuildConfigs[index])
	}
	for index := range object.ConfigMaps {
		object.ConfigMaps[index].SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("ConfigMap"))
		allObjects = append(allObjects, &object.ConfigMaps[index])
	}
	return allObjects
}

func (reconciler *Reconciler) ensureImageStream(name, namespace, imageURL string, cr *api.KieApp) (string, error) {
	if cr.Status.Applied.ImageRegistry != nil {
		if reconciler.checkImageStreamTag(name, cr.Namespace) {
			return cr.Namespace, nil
		}
		log.Warnf("ImageStreamTag %s/%s doesn't exist.", namespace, name)
		err := reconciler.createLocalImageTag(name, imageURL, cr)
		if err != nil {
			log.Error(err)
			return namespace, err
		}
		return cr.Namespace, nil
	}

	if reconciler.checkImageStreamTag(name, namespace) {
		return namespace, nil
	} else if reconciler.checkImageStreamTag(name, cr.Namespace) {
		return cr.Namespace, nil
	} else {
		log.Warnf("ImageStreamTag %s/%s doesn't exist.", namespace, name)
		err := reconciler.createLocalImageTag(name, imageURL, cr)
		if err != nil {
			log.Error(err)
			return namespace, err
		}
	}
	return cr.Namespace, nil
}

// createObj creates an object based on the error passed in from a `client.Get`
func (reconciler *Reconciler) createObj(object client.Object, err error) (reconcile.Result, error) {
	log := log.With("kind", object.GetObjectKind().GroupVersionKind().Kind, "name", object.GetName(), "namespace", object.GetNamespace())

	if err != nil && errors.IsNotFound(err) {
		// Define a new Object
		log.Info("Creating")
		err = reconciler.Service.Create(context.TODO(), object)
		if err != nil {
			log.Warn("Failed to create object. ", err)
			return reconcile.Result{}, err
		}
		// Object created successfully - return and requeue
		return reconcile.Result{RequeueAfter: time.Duration(200) * time.Millisecond}, nil
	} else if err != nil {
		log.Error("Failed to get object. ", err)
		return reconcile.Result{}, err
	}
	log.Debug("Skip reconcile - object already exists")
	return reconcile.Result{}, nil
}

// UpdateObj reconciles the given object
func (reconciler *Reconciler) UpdateObj(obj api.OpenShiftObject) (reconcile.Result, error) {
	log := log.With("kind", obj.GetObjectKind().GroupVersionKind().Kind, "name", obj.GetName(), "namespace", obj.GetNamespace())
	log.Info("Updating")
	err := reconciler.Service.Update(context.TODO(), obj)
	if err != nil {
		log.Warn("Failed to update object. ", err)
		return reconcile.Result{}, err
	}
	// Object updated - return and requeue
	return reconcile.Result{Requeue: true}, nil
}

func checkTLS(tls *routev1.TLSConfig) bool {
	if tls != nil {
		return true
	}
	return false
}

// GetRouteHost returns the Hostname of the route provided
func (reconciler *Reconciler) GetRouteHost(route routev1.Route, routes []client.Object) string {
	for index := range routes {
		candidate := routes[index].(*routev1.Route)
		if candidate.Name == route.Name && candidate.Namespace == route.Namespace {
			return candidate.Spec.Host
		}
	}
	return ""
}

// CreateConfigMaps generates & creates necessary ConfigMaps from embedded files
func (reconciler *Reconciler) CreateConfigMaps(myDep *appsv1.Deployment) {
	configMaps := defaults.ConfigMapsFromFile(myDep, myDep.Namespace, reconciler.Service.GetScheme())
	for _, configMap := range configMaps {
		var testDir bool
		result := strings.Split(configMap.Name, "-")
		if len(result) > 1 {
			if result[1] == "testdata" {
				testDir = true
			}
		}
		// don't create configmaps for test directories
		if !testDir {
			// if configmap already exists, compare to new
			if existingCM, exists := reconciler.createConfigMap(&configMap); exists {
				// if new configmap and existing have different data
				if !reflect.DeepEqual(configMap.Data, existingCM.Data) || !reflect.DeepEqual(configMap.BinaryData, existingCM.BinaryData) {
					log.Infof("Differences detected in %s ConfigMap.", configMap.Name)
					existingCM.Name = strings.Join([]string{configMap.Name, "bak"}, "-")
					for annotation, ver := range configMap.Annotations {
						if annotation == api.SchemeGroupVersion.Group {
							existingCM.Name = strings.Join([]string{configMap.Name, ver, "bak"}, "-")
						}
					}
					existingCM.ResourceVersion = ""
					existingCM.OwnerReferences = nil
					// create a backup configmap of existing
					// if backup configmap already exists, overwrite w/ new backup
					if existingBackupCM, exists := reconciler.createConfigMap(existingCM); exists {
						// if backup configmap and existing backup have different data
						if !reflect.DeepEqual(existingCM.Data, existingBackupCM.Data) || !reflect.DeepEqual(existingCM.BinaryData, existingBackupCM.BinaryData) {
							existingBackupCM.Data = existingCM.Data
						_:
							reconciler.UpdateObj(existingBackupCM)
						}
					}
				}
			}
		}
	}
}

// createConfigMap creates an individual ConfigMap, will return the existing ConfigMap object should one exist
func (reconciler *Reconciler) createConfigMap(obj api.OpenShiftObject) (*corev1.ConfigMap, bool) {
	emptyObj := &corev1.ConfigMap{}
	err := reconciler.Service.Get(context.TODO(), types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}, emptyObj)
	if errors.IsNotFound(err) {
		// attempt creation of configmap if doesn't exist
	_:
		reconciler.createObj(obj, err)
		return &corev1.ConfigMap{}, false
	} else if err != nil {
		log.Error(err)
		return &corev1.ConfigMap{}, false
	}
	return emptyObj, true
}

// checkKieServerConfigMap checks ConfigMaps owned by Kie Servers
func (reconciler *Reconciler) checkKieServerConfigMap(instance *api.KieApp, env api.Environment) {
	listOpts := []client.ListOption{
		client.InNamespace(instance.Namespace),
	}
	cmList := &corev1.ConfigMapList{}
	if err := reconciler.Service.List(context.TODO(), cmList, listOpts...); err != nil {
		log.Warn("Failed to list ConfigMaps. ", err)
	} else {
		serverDcList := make(map[string]int32)
		for _, server := range env.Servers {
			for _, sDc := range server.DeploymentConfigs {
				serverDcList[sDc.Name] = sDc.Spec.Replicas
			}
		}
		// sort through ConfigMap list, focus on ones owned by kie servers whose replicas setting is zero
		for _, cm := range cmList.Items {
			for _, ownerRef := range cm.OwnerReferences {
				if serverDcList[ownerRef.Name] == 0 && ownerRef.Kind == "DeploymentConfig" && cm.Labels[constants.KieServerCMLabel] != "" && cm.Labels[constants.KieServerCMLabel] != "DETACHED" {
					dcObj := &oappsv1.DeploymentConfig{}
					if err := reconciler.Service.Get(context.TODO(), types.NamespacedName{Name: ownerRef.Name, Namespace: cm.Namespace}, dcObj); err != nil {
						log.Error(err)
					}
					// if server DC replicas equal zero, execute DELETE against console
					if dcObj.Status.AvailableReplicas == 0 {
						cmObj := &corev1.ConfigMap{}
						if err := reconciler.Service.Get(context.TODO(), types.NamespacedName{Name: ownerRef.Name, Namespace: cm.Namespace}, cmObj); err != nil {
							log.Error(err)
						}
						cmObj.Labels[constants.KieServerCMLabel] = "DETACHED"
						log.Infof("%s replicas set to zero so relabeling associated ConfigMap as DETACHED", cm.Name)
						if _, err = reconciler.UpdateObj(cmObj); err != nil {
							log.Error(err)
						}
					}
				}
			}
		}
	}
}

func (reconciler *Reconciler) getDeployedResources(instance *api.KieApp) (map[reflect.Type][]client.Object, error) {
	log := log.With("kind", instance.Kind, "name", instance.Name, "namespace", instance.Namespace)

	reader := read.New(reconciler.Service).WithNamespace(instance.Namespace).WithOwnerObject(instance)
	resourceMap, err := reader.ListAll(
		&oappsv1.DeploymentConfigList{},
		&corev1.PersistentVolumeClaimList{},
		&corev1.ServiceAccountList{},
		&rbacv1.RoleList{},
		&rbacv1.RoleBindingList{},
		&corev1.ServiceList{},
		&appsv1.StatefulSetList{},
		&routev1.RouteList{},
		&oimagev1.ImageStreamList{},
		&buildv1.BuildConfigList{},
		&corev1.ConfigMapList{},
	)
	if err != nil {
		log.Warn("Failed to list deployed objects. ", err)
		return nil, err
	}

	//secretList := &corev1.SecretList{}
	//err = reconciler.Service.List(context.TODO(), listOps, secretList) //TODO: can't list secrets due to bug:
	// https://github.com/kubernetes-sigs/controller-runtime/issues/362
	// multiple group-version-kinds associated with type *api.SecretList, refusing to guess at one
	// Will work around by loading known secrets instead

	var secrets []client.Object
	dcs := resourceMap[reflect.TypeOf(oappsv1.DeploymentConfig{})]
	for _, res := range dcs {
		dc := res.(*oappsv1.DeploymentConfig)
		for _, volume := range dc.Spec.Template.Spec.Volumes {
			if volume.Secret != nil {
				name := volume.Secret.SecretName
				secret := &corev1.Secret{}
				err := reconciler.Service.Get(context.TODO(), types.NamespacedName{Name: name, Namespace: instance.GetNamespace()}, secret)
				if err != nil && !errors.IsNotFound(err) {
					log.Warn("Failed to load Secret", err)
					return nil, err
				}
				for _, ownerRef := range secret.GetOwnerReferences() {
					if ownerRef.UID == instance.UID {
						secrets = append(secrets, secret)
						break
					}
				}
			}
		}
	}
	resourceMap[reflect.TypeOf(corev1.Secret{})] = secrets

	if semver.Compare(reconciler.OcpVersion, "v4.2") >= 0 || reconciler.OcpVersion == "" {
		consoleLink := &consolev1.ConsoleLink{}
		err = reconciler.Service.Get(context.TODO(), types.NamespacedName{Name: getConsoleLinkName(instance)}, consoleLink)
		if err != nil {
			if errors.IsNotFound(err) {
				return resourceMap, nil
			}
			log.Warn("Failed to load ConsoleLink", err)
			return resourceMap, err
		}
		resourceMap[reflect.TypeOf(*consoleLink)] = []client.Object{consoleLink}
	}

	return resourceMap, nil
}

func (reconciler *Reconciler) getCSV(operator *appsv1.Deployment) *operatorsv1alpha1.ClusterServiceVersion {
	csv := &operatorsv1alpha1.ClusterServiceVersion{}
	for _, ref := range operator.GetOwnerReferences() {
		if ref.Kind == "ClusterServiceVersion" {
			err := reconciler.Service.Get(context.TODO(), types.NamespacedName{Namespace: operator.Namespace, Name: ref.Name}, csv)
			if err != nil {
				if errors.IsNotFound(err) {
					log.Debug("CSV not found. ", err)
				}
				log.Error("Failed to get CSV. ", err)
			}
		}
	}
	return csv
}

func (reconciler *Reconciler) getConsoleLinkResource(cr *api.KieApp) client.Object {
	if cr.GetDeletionTimestamp() != nil || cr.Status.ConsoleHost == "" || !strings.HasPrefix(cr.Status.ConsoleHost, "https://") ||
		(reconciler.OcpVersion != "" && semver.Compare(reconciler.OcpVersion, "v4.2") < 0) {
		return nil
	}
	consoleLink := &consolev1.ConsoleLink{
		ObjectMeta: metav1.ObjectMeta{
			Name: getConsoleLinkName(cr),
			Labels: map[string]string{
				"rhpam-namespace": cr.Namespace,
				"rhpam-app":       cr.Name,
			},
		},
		Spec: consolev1.ConsoleLinkSpec{
			Link: consolev1.Link{
				Href: cr.Status.ConsoleHost,
				Text: getConsoleLinkFriendlyName(cr),
			},
			Location: consolev1.NamespaceDashboard,
			NamespaceDashboard: &consolev1.NamespaceDashboardSpec{
				Namespaces: []string{cr.Namespace},
			},
		},
	}
	return consoleLink
}

func getConsoleLinkName(cr *api.KieApp) string {
	return fmt.Sprintf("%s-link-%s", cr.Namespace, cr.Name)
}

func getConsoleLinkFriendlyName(cr *api.KieApp) string {
	return fmt.Sprintf("%s: %s", cr.Name, constants.EnvironmentConstants[cr.Status.Applied.Environment].App.FriendlyName)
}
