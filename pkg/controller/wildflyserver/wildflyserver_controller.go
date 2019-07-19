package wildflyserver

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	wildflyv1alpha1 "github.com/wildfly/wildfly-operator/pkg/apis/wildfly/v1alpha1"
	wildflyutil "github.com/wildfly/wildfly-operator/pkg/controller/util"

	routev1 "github.com/openshift/api/route/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var log = logf.Log.WithName("controller_wildflyserver")

const (
	httpApplicationPort           int32 = 8080
	httpManagementPort            int32 = 9990
	recoveryPort                  int32 = 4712
	standaloneServerDataDirPath         = "/wildfly/standalone/data"
	wflyMgmtTxnRecoveryUserName         = "transaction.recovery.scaledown"
	labelWildflyOperatorInService       = "wildfly.operator.in.service"
)

var (
	mgmtOpReload = map[string]interface{}{
		"address":   []string{},
		"operation": "reload",
	}
	mgmtOpTxnEnableRecoveryListener = map[string]interface{}{
		"address": []string{
			"subsystem", "transactions",
		},
		"operation": "write-attribute",
		"name":      "recovery-listener",
		"value":     "true",
	}
	mgmtOpTxnProbe = map[string]interface{}{
		"address": []string{
			"subsystem", "transactions", "log-store", "log-store",
		},
		"operation": "probe",
	}
	mgmtOpTxnRead = map[string]interface{}{
		"address": []string{
			"subsystem", "transactions", "log-store", "log-store",
		},
		"operation":       "read-children-resources",
		"child-type":      "transactions",
		"recursive":       "true",
		"include-runtime": "true",
	}
)

// Add creates a new WildFlyServer Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	return add(mgr, newReconciler(mgr))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) reconcile.Reconciler {
	return &ReconcileWildFlyServer{client: mgr.GetClient(), scheme: mgr.GetScheme()}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("wildflyserver-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource WildFlyServer
	err = c.Watch(&source.Kind{Type: &wildflyv1alpha1.WildFlyServer{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	// Watch for changes to secondary resources and requeue the owner WildFlyServer
	enqueueRequestForOwner := handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &wildflyv1alpha1.WildFlyServer{},
	}
	for _, obj := range [3]runtime.Object{&appsv1.StatefulSet{}, &corev1.Service{}, &routev1.Route{}} {
		if err = c.Watch(&source.Kind{Type: obj}, &enqueueRequestForOwner); err != nil {
			return err
		}
	}
	return nil
}

var _ reconcile.Reconciler = &ReconcileWildFlyServer{}

// ReconcileWildFlyServer reconciles a WildFlyServer object
type ReconcileWildFlyServer struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client client.Client
	scheme *runtime.Scheme
}

// Reconcile reads that state of the cluster for a WildFlyServer object and makes changes based on the state read
// and what is in the WildFlyServer.Spec
// TODO(user): Modify this Reconcile function to implement your Controller logic.  This example creates
// a Pod as an example
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileWildFlyServer) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling WildFlyServer")

	// Fetch the WildFlyServer instance
	wildflyServer := &wildflyv1alpha1.WildFlyServer{}
	err := r.client.Get(context.TODO(), request.NamespacedName, wildflyServer)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	// TODO: the changed secret needs to be promoted to the existing pods (or they need to be rescheduled)
	// Define secret jboss cli connection for txn recovery processing
	foundTxnRecoverySecret := &v1.Secret{}
	err = r.client.Get(context.TODO(), types.NamespacedName{Name: getTxnRecoverySecretName(wildflyServer), Namespace: wildflyServer.Namespace}, foundTxnRecoverySecret)
	if err != nil && errors.IsNotFound(err) {
		txnRecoverySecret := r.txnRecoveryJBossCliPasswordSecret(wildflyServer)
		err = r.client.Create(context.TODO(), txnRecoverySecret)
		if err != nil {
			reqLogger.Error(err, "Failed to create Secret necessary for txn recovery.", "Secret.Namespace", txnRecoverySecret.Namespace, "Secret.Name", txnRecoverySecret.Name)
			return reconcile.Result{}, err
		}
		// Secret created successfully - return and requeue
		return reconcile.Result{Requeue: true}, nil
	} else if err != nil {
		reqLogger.Error(err, "Failed to get Secret.", "txn recovery secret name", getTxnRecoverySecretName(wildflyServer))
		return reconcile.Result{}, err
	}

	// Check if the statefulSet already exists, if not create a new one
	foundStatefulSet := &appsv1.StatefulSet{}
	err = r.client.Get(context.TODO(), types.NamespacedName{Name: wildflyServer.Name, Namespace: wildflyServer.Namespace}, foundStatefulSet)
	if err != nil && errors.IsNotFound(err) {
		// Define a new statefulSet
		statefulSet := r.statefulSetForWildFly(wildflyServer, foundTxnRecoverySecret)
		reqLogger.Info("Creating a new StatefulSet.", "StatefulSet.Namespace", statefulSet.Namespace, "StatefulSet.Name", statefulSet.Name)
		err = r.client.Create(context.TODO(), statefulSet)
		if err != nil {
			reqLogger.Error(err, "Failed to create new StatefulSet.", "StatefulSet.Namespace", statefulSet.Namespace, "StatefulSet.Name", statefulSet.Name)
			return reconcile.Result{}, err
		}
		// StatefulSet created successfully - return and requeue
		return reconcile.Result{Requeue: true}, nil
	} else if err != nil {
		reqLogger.Error(err, "Failed to get StatefulSet.")
		return reconcile.Result{}, err
	}

	// check WildFlyServerSpec if it's about to be scaled down
	podList, err := r.getPodsForWildFly(wildflyServer)
	if err != nil {
		reqLogger.Error(err, "Failed to list pods.", "WildFlyServer.Namespace", wildflyServer.Namespace, "WildFlyServer.Name", wildflyServer.Name)
		return reconcile.Result{}, err
	}
	// TODO: do not enter the scaledown handling if the pod was already cleaned from transactions, use the pod status to check
	wildflyServerSpecSize := wildflyServer.Spec.Size
	numberOfPodsToScaleDown := len(podList.Items) - int(wildflyServerSpecSize)
	scaleDownPodsState := make(map[string]string) // pod name - state
	scaleDownErrors := []error{}
	for scaleDownIndex := 1; scaleDownIndex <= numberOfPodsToScaleDown; scaleDownIndex++ {
		// scaledown scenario, need to handle transction recovery
		scaleDownPod := podList.Items[len(podList.Items)-scaleDownIndex]
		scaleDownPodName := scaleDownPod.ObjectMeta.Name
		scaleDownPodIP := scaleDownPod.Status.PodIP
		if strings.Contains(scaleDownPodIP, ":") {
			scaleDownPodIP = "[" + scaleDownPodIP + "]" // for IPv6
		}
		reqLogger.Info("Found ip for the pod ", "podname", scaleDownPodName, "podip", scaleDownPodIP) // TODO: debug?

		// setting-up the pod status
		wildflyServerSpecPodStatus := getWildflyServerPodStatusByName(wildflyServer, scaleDownPodName)
		if wildflyServerSpecPodStatus != nil {
			scaleDownPodsState[scaleDownPodName] = wildflyServerSpecPodStatus.State
		} else {
			scaleDownPodsState[scaleDownPodName] = wildflyv1alpha1.PodStateScalingDownDirty
		}

		if wildflyServerSpecPodStatus.State != wildflyv1alpha1.PodStateScalingDownClean {
			// removing the pod from the Service handling
			scaleDownPod.ObjectMeta.Labels[labelWildflyOperatorInService] = "under-scale-down-processing"
			if err = r.client.Update(context.TODO(), &scaleDownPod); err != nil {
				reqLogger.Error(err, "Failed to update pod labels", "Pod name", scaleDownPod.ObjectMeta.Name, " with labels ", scaleDownPod.ObjectMeta.Labels)
				scaleDownErrors = append(scaleDownErrors, err)
				break
			}

			reqLogger.Info(">>>>> Going for scaledown with pod ", "pod name", scaleDownPod.ObjectMeta.Name) // TODO: debug?

			managementURL := fmt.Sprintf("http://%v:%v/management", scaleDownPodIP, httpManagementPort)
			username := string(foundTxnRecoverySecret.Data["username"])
			password := string(foundTxnRecoverySecret.Data["password"])
			reqLogger.Info("Connection to host digest auth", "url", managementURL, "username", username, "password", password) // TODO: debug/todelete?

			// TXN ENABLE RECOVERY LISTENER
			opEnableRecovery, err := fromJSONToReader(mgmtOpTxnEnableRecoveryListener)
			if err != nil {
				reqLogger.Error(err, "Fail to parse JSON management command",
					"command", mgmtOpTxnEnableRecoveryListener, "for pod", scaleDownPodName, "IP address", scaleDownPodIP)
				return reconcile.Result{}, err
			}
			res, err := httpDigestPostWithJSON(managementURL, username, password, opEnableRecovery)
			if err != nil {
				reqLogger.Error(err, "Failed to process management operation", "HTTP response", res)
				return reconcile.Result{}, err
			}
			defer res.Body.Close()
			jsonBody, err := decodeJSONBody(res)
			if err != nil {
				reqLogger.Error(err, "Cannot decode JSON body", "HTTP response", res)
				return reconcile.Result{}, err
			}
			if !isMgmtOutcomeSuccesful(res, jsonBody) {
				reqLogger.Info("Failed to enable transaction recovery listener. Scaledown processing can take longer", "HTTP response", res)
			}
			// enabling the recovery listner requires reload if not already set
			isReloadRequires := getJSONDataByIndex(jsonBody["response-headers"], "operation-requires-reload")
			if isReloadRequires == "true" {
				opEnableRecovery, err := fromJSONToReader(mgmtOpReload)
				if err != nil {
					reqLogger.Error(err, "Failed to parse JSON management command",
						"command", mgmtOpReload, "for pod", scaleDownPodName, "IP address", scaleDownPodIP)
					return reconcile.Result{}, err
				}
				res, err = httpDigestPostWithJSON(managementURL, username, password, opEnableRecovery)
				if err == nil {
					defer res.Body.Close()
				}
				return reconcile.Result{Requeue: true}, nil
			}

			// TXN PROBE
			// pod is still available for remote command execution as it's running
			// _, err = wildflyutil.ExecRemote(scaleDownPod, "/opt/jboss/wildfly/bin/jboss-cli.sh -c --command='/subsystem=transactions/log-store=log-store:probe()'")
			opProbe, err := fromJSONToReader(mgmtOpTxnProbe)
			if err != nil {
				reqLogger.Error(err, "Failed to parse JSON management command",
					"command", mgmtOpTxnProbe, "for pod", scaleDownPodName, "IP address", scaleDownPodIP)
				return reconcile.Result{}, err
			}
			res, err = httpDigestPostWithJSON(managementURL, username, password, opProbe)
			if err == nil {
				defer res.Body.Close()
			}

			// TXN READ TXNs
			isForScan := false
			opTxnRead, err := fromJSONToReader(mgmtOpTxnRead)
			if err != nil {
				reqLogger.Error(err, "Failed to parse JSON management command",
					"command", mgmtOpTxnRead, "for pod", scaleDownPodName, "IP address", scaleDownPodIP)
				return reconcile.Result{}, err
			}
			res, err = httpDigestPostWithJSON(managementURL, username, password, opTxnRead)
			if err != nil {
				reqLogger.Error(err, "Failed to process management operation", "HTTP response", res)
				return reconcile.Result{}, err
			}
			defer res.Body.Close()
			jsonBody, err = decodeJSONBody(res)
			if err != nil {
				reqLogger.Error(err, "Cannot decode JSON body", "HTTP response", res)
				return reconcile.Result{}, err
			}
			if !isMgmtOutcomeSuccesful(res, jsonBody) {
				err = fmt.Errorf("Cannot get list of the in-doubt transactions at pod %v for transaction scaledown", scaleDownPodName)
				reqLogger.Error(err, "Failure on transaction scaledown", "HTTP response", res)
				return reconcile.Result{}, err
			}
			transactions := jsonBody["result"]
			txnMap, isMap := transactions.(map[string]interface{})
			if isMap && len(txnMap) > 0 {
				reqLogger.Info("Recovery scan to be invoked as the transaction log storage is not empty.", "Transaction list", txnMap)
				isForScan = true
			}
			commandResult, err := wildflyutil.ExecRemote(scaleDownPod, "ls \""+standaloneServerDataDirPath+"/ejb-xa-recovery/\" 2> /dev/null || true")
			if err != nil {
				reqLogger.Error(err, "Cannot query filesystem to check existing remote transactions", "pod name", scaleDownPodName)
				return reconcile.Result{}, err
			}
			if commandResult != "" {
				reqLogger.Info("Recovery scan to be invoked because of the folder data/ejb-xa-recovery/ is not empty.", "Output listing", commandResult)
				isForScan = true
			}
			if isForScan {
				// java -cp /opt/jboss/wildfly/modules/system/layers/base/org/jboss/jts/main/narayana-jts-idlj-*.Final.jar com.arjuna.ats.arjuna.tools.RecoveryMonitor -host quickstart-0 -port 4712 -timeout 18000
				socketConnect(scaleDownPodIP, recoveryPort, "SCAN")
				scaleDownPodsState[scaleDownPodName] = wildflyv1alpha1.PodStateScalingDownDirty
			} else {
				// no data in object store this pod is clean to go
				scaleDownPodsState[scaleDownPodName] = wildflyv1alpha1.PodStateScalingDownClean
			}
		}
	}
	if len(scaleDownErrors) > 0 {
		return reconcile.Result{}, scaleDownErrors[0]
	}
	if containsValue(&scaleDownPodsState, wildflyv1alpha1.PodStateScalingDownDirty) {
		return reconcile.Result{Requeue: true}, nil // success but we need to requeue
	}

	// check if the stateful set is up to date with the WildFlyServerSpec
	if checkUpdate(&wildflyServer.Spec, foundStatefulSet) {
		err = r.client.Update(context.TODO(), foundStatefulSet)
		if err != nil {
			reqLogger.Error(err, "Failed to update StatefulSet.", "StatefulSet.Namespace", foundStatefulSet.Namespace, "StatefulSet.Name", foundStatefulSet.Name)
			return reconcile.Result{}, err
		}

		// Spec updated - return and requeue
		return reconcile.Result{Requeue: true}, nil
	}

	// Check if the loadbalancer already exists, if not create a new one
	foundLoadBalancer := &corev1.Service{}
	loadBalancerName := loadBalancerServiceName(wildflyServer)
	err = r.client.Get(context.TODO(), types.NamespacedName{Name: loadBalancerName, Namespace: wildflyServer.Namespace}, foundLoadBalancer)
	if err != nil && errors.IsNotFound(err) {
		// Define a new loadbalancer
		loadBalancer := r.loadBalancerForWildFly(wildflyServer)
		reqLogger.Info("Creating a new LoadBalancer.", "LoadBalancer.Namespace", loadBalancer.Namespace, "LoadBalancer.Name", loadBalancer.Name)
		err = r.client.Create(context.TODO(), loadBalancer)
		if err != nil {
			reqLogger.Error(err, "Failed to create new LoadBalancer.", "LoadBalancer.Namespace", loadBalancer.Namespace, "LoadBalancer.Name", loadBalancer.Name)
			return reconcile.Result{}, err
		}
		// loadbalancer created successfully - return and requeue
		return reconcile.Result{Requeue: true}, nil
	} else if err != nil {
		reqLogger.Error(err, "Failed to get LoadBalancer.")
		return reconcile.Result{}, err
	}

	// Check if the HTTP route must be created.
	foundRoute := &routev1.Route{}
	if !wildflyServer.Spec.DisableHTTPRoute {
		err = r.client.Get(context.TODO(), types.NamespacedName{Name: wildflyServer.Name, Namespace: wildflyServer.Namespace}, foundRoute)
		if err != nil && errors.IsNotFound(err) {
			// Define a new Route
			route := r.routeForWildFly(wildflyServer)
			reqLogger.Info("Creating a new Route.", "Route.Namespace", route.Namespace, "Route.Name", route.Name)
			err = r.client.Create(context.TODO(), route)
			if err != nil {
				reqLogger.Error(err, "Failed to create new Route.", "Route.Namespace", route.Namespace, "Route.Name", route.Name)
				return reconcile.Result{}, err
			}
			// Route created successfully - return and requeue
			return reconcile.Result{Requeue: true}, nil
		} else if err != nil && errorIsNoMatchesForKind(err, "Route", "route.openshift.io/v1") {
			// if the operator runs on k8s, Route resource does not exist and the route creation must be skipped.
			reqLogger.Info("Routes are not supported, skip creation of the HTTP route")
			wildflyServer.Spec.DisableHTTPRoute = true
			if err = r.client.Update(context.TODO(), wildflyServer); err != nil {
				reqLogger.Error(err, "Failed to update WildFlyServerSpec to disable HTTP Route.", "WildFlyServer.Namespace", wildflyServer.Namespace, "WildFlyServer.Name", wildflyServer.Name)
				return reconcile.Result{}, err
			}
			return reconcile.Result{Requeue: true}, nil
		} else if err != nil {
			reqLogger.Error(err, "Failed to get Route.")
			return reconcile.Result{}, err
		}
	}

	// Requeue until the pod list matches the spec's size
	if len(podList.Items) != int(wildflyServerSpecSize) {
		reqLogger.Info("Number of pods does not match the desired size", "PodList.Size", len(podList.Items), "Size", wildflyServerSpecSize)
		return reconcile.Result{Requeue: true}, nil
	}

	// Update WildFly Server host status
	update := false
	if !wildflyServer.Spec.DisableHTTPRoute {
		hosts := make([]string, len(foundRoute.Status.Ingress))
		for i, ingress := range foundRoute.Status.Ingress {
			hosts[i] = ingress.Host
		}
		if !reflect.DeepEqual(hosts, wildflyServer.Status.Hosts) {
			update = true
			wildflyServer.Status.Hosts = hosts
			reqLogger.Info("Updating hosts", "WildFlyServer", wildflyServer)
		}
	}

	// TODO: here is wrong
	// Update Wildfly Server scale down processing info
	if wildflyServer.Status.ScalingdownPods != int32(numberOfPodsToScaleDown) {
		update = true
		wildflyServer.Status.ScalingdownPods = int32(numberOfPodsToScaleDown)
	}

	// Update WildFly Server pod status
	requeue, podsStatus := getPodStatus(podList.Items)
	if !reflect.DeepEqual(podsStatus, wildflyServer.Status.Pods) {
		update = true
		wildflyServer.Status.Pods = podsStatus
	}

	if update {
		if err := r.client.Status().Update(context.Background(), wildflyServer); err != nil {
			reqLogger.Error(err, "Failed to update pods in WildFlyServer status.")
			return reconcile.Result{}, err
		}
	}
	if requeue {
		return reconcile.Result{Requeue: true}, nil
	}

	return reconcile.Result{}, nil
}

// check if the statefulset resource is up to date with the WildFlyServerSpec
func checkUpdate(spec *wildflyv1alpha1.WildFlyServerSpec, statefuleSet *appsv1.StatefulSet) bool {
	var update bool
	// Ensure the application image is up to date
	applicationImage := spec.ApplicationImage
	if statefuleSet.Spec.Template.Spec.Containers[0].Image != applicationImage {
		log.Info("Updating application image to "+applicationImage, "StatefulSet.Namespace", statefuleSet.Namespace, "StatefulSet.Name", statefuleSet.Name)
		statefuleSet.Spec.Template.Spec.Containers[0].Image = applicationImage
		update = true
	}
	// Ensure the statefulset replicas is up to date
	size := spec.Size
	if *statefuleSet.Spec.Replicas != size {
		log.Info("Updating replica size to "+strconv.Itoa(int(size)), "StatefulSet.Namespace", statefuleSet.Namespace, "StatefulSet.Name", statefuleSet.Name)
		statefuleSet.Spec.Replicas = &size
		update = true
	}
	// Ensure the env variables are up to date
	for _, env := range spec.Env {
		if !matches(&statefuleSet.Spec.Template.Spec.Containers[0], env) {
			log.Info("Updated statefulset env", "StatefulSet.Namespace", statefuleSet.Namespace, "StatefulSet.Name", statefuleSet.Name, "Env", env)
			update = true
		}
	}
	// Ensure the envFrom variables are up to date
	envFrom := spec.EnvFrom
	if !reflect.DeepEqual(statefuleSet.Spec.Template.Spec.Containers[0].EnvFrom, envFrom) {
		log.Info("Updating envFrom", "StatefulSet.Namespace", statefuleSet.Namespace, "StatefulSet.Name", statefuleSet.Name)
		statefuleSet.Spec.Template.Spec.Containers[0].EnvFrom = envFrom
		update = true
	}

	return update
}

// listing pods which belongs to the WildFly server
//   the pods are differentiated based on the selectors
func (r *ReconcileWildFlyServer) getPodsForWildFly(w *wildflyv1alpha1.WildFlyServer) (*corev1.PodList, error) {
	podList := &corev1.PodList{}
	labelsForWildfly := labelsForWildFly(w)
	delete(labelsForWildfly, labelWildflyOperatorInService)
	labelSelector := labels.SelectorFromSet(labelsForWildfly)
	listOps := &client.ListOptions{
		Namespace:     w.Namespace,
		LabelSelector: labelSelector,
	}
	err := r.client.List(context.TODO(), listOps, podList)
	return podList, err
}

// matches checks if the envVar from the WildFlyServerSpec matches the same env var from the container.
// If it does not match, it updates the container EnvVar with the fields from the WildFlyServerSpec and return false.
func matches(container *v1.Container, envVar corev1.EnvVar) bool {
	for index, e := range container.Env {
		if envVar.Name == e.Name {
			if !reflect.DeepEqual(envVar, e) {
				container.Env[index] = envVar
				return false
			}
			return true
		}
	}
	//append new spec env to container's env var
	container.Env = append(container.Env, envVar)
	return false
}

// statefulSetForWildFly returns a wildfly StatefulSet object
func (r *ReconcileWildFlyServer) statefulSetForWildFly(w *wildflyv1alpha1.WildFlyServer, txnRecoverySecret *corev1.Secret) *appsv1.StatefulSet {
	ls := labelsForWildFly(w)
	delete(ls, labelWildflyOperatorInService)
	replicas := w.Spec.Size
	applicationImage := w.Spec.ApplicationImage
	volumeName := w.Name + "-volume"

	mgmtUser := string(txnRecoverySecret.Data["username"])
	mgmtPassword := generateWflyMgmtHashedPassword(txnRecoverySecret)

	statefulSet := &appsv1.StatefulSet{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "StatefulSet",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      w.Name,
			Namespace: w.Namespace,
			Labels:    ls,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &replicas,
			ServiceName: loadBalancerServiceName(w),
			Selector: &metav1.LabelSelector{
				MatchLabels: ls,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: ls,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  w.Name,
						Image: applicationImage,
						Ports: []corev1.ContainerPort{
							{
								ContainerPort: httpApplicationPort,
								Name:          "http",
							},
							{
								ContainerPort: httpManagementPort,
								Name:          "admin",
							},
						},
						LivenessProbe: createLivenessProbe(),
						// Readiness Probe is options
						ReadinessProbe: createReadinessProbe(),
						Lifecycle: &corev1.Lifecycle{
							PostStart: &corev1.Handler{
								Exec: &corev1.ExecAction{
									Command: []string{
										"/bin/sh",
										"-c",
										fmt.Sprintf("echo '%s=%s' >> \"%s/../configuration/mgmt-users.properties\"", mgmtUser, mgmtPassword, standaloneServerDataDirPath),
									},
								},
							},
						},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      volumeName,
							MountPath: standaloneServerDataDirPath,
						}},
						// TODO the KUBERNETES_NAMESPACE and KUBERNETES_LABELS env should only be set if
						// the application uses clustering and KUBE_PING.
						Env: []corev1.EnvVar{
							{
								Name: "KUBERNETES_NAMESPACE",
								ValueFrom: &corev1.EnvVarSource{
									FieldRef: &corev1.ObjectFieldSelector{
										FieldPath: "metadata.namespace",
									},
								},
							},
							{
								Name:  "KUBERNETES_LABELS",
								Value: labels.SelectorFromSet(ls).String(),
							},
						},
					}},
					ServiceAccountName: w.Spec.ServiceAccountName,
				},
			},
		},
	}

	if len(w.Spec.EnvFrom) > 0 {
		statefulSet.Spec.Template.Spec.Containers[0].EnvFrom = append(statefulSet.Spec.Template.Spec.Containers[0].EnvFrom, w.Spec.EnvFrom...)
	}

	if len(w.Spec.Env) > 0 {
		statefulSet.Spec.Template.Spec.Containers[0].Env = append(statefulSet.Spec.Template.Spec.Containers[0].Env, w.Spec.Env...)
	}

	storageSpec := w.Spec.Storage

	if storageSpec == nil {
		statefulSet.Spec.Template.Spec.Volumes = append(statefulSet.Spec.Template.Spec.Volumes, corev1.Volume{
			Name: volumeName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		})
	} else if storageSpec.EmptyDir != nil {
		emptyDir := storageSpec.EmptyDir
		statefulSet.Spec.Template.Spec.Volumes = append(statefulSet.Spec.Template.Spec.Volumes, corev1.Volume{
			Name: volumeName,
			VolumeSource: v1.VolumeSource{
				EmptyDir: emptyDir,
			},
		})
	} else {
		pvcTemplate := storageSpec.VolumeClaimTemplate
		if pvcTemplate.Name == "" {
			pvcTemplate.Name = volumeName
		}
		pvcTemplate.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
		pvcTemplate.Spec.Resources = storageSpec.VolumeClaimTemplate.Spec.Resources
		pvcTemplate.Spec.Selector = storageSpec.VolumeClaimTemplate.Spec.Selector
		statefulSet.Spec.VolumeClaimTemplates = append(statefulSet.Spec.VolumeClaimTemplates, pvcTemplate)
	}

	standaloneConfigMap := w.Spec.StandaloneConfigMap
	if standaloneConfigMap != nil {
		configMapName := standaloneConfigMap.Name
		configMapKey := standaloneConfigMap.Key
		if configMapKey == "" {
			configMapKey = "standalone.xml"
		}
		log.Info("Reading standalone configuration from configmap", "StandaloneConfigMap.Name", configMapName, "StandaloneConfigMap.Key", configMapKey)

		statefulSet.Spec.Template.Spec.Volumes = append(statefulSet.Spec.Template.Spec.Volumes, corev1.Volume{
			Name: "standalone-config-volume",
			VolumeSource: v1.VolumeSource{
				ConfigMap: &v1.ConfigMapVolumeSource{
					LocalObjectReference: v1.LocalObjectReference{
						Name: configMapName,
					},
					Items: []v1.KeyToPath{
						{
							Key:  configMapKey,
							Path: "standalone.xml",
						},
					},
				},
			},
		})
		statefulSet.Spec.Template.Spec.Containers[0].VolumeMounts = append(statefulSet.Spec.Template.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{
			Name:      "standalone-config-volume",
			MountPath: "/wildfly/standalone/configuration/standalone.xml",
			SubPath:   "standalone.xml",
		})
	}

	// Set WildFlyServer instance as the owner and controller
	controllerutil.SetControllerReference(w, statefulSet, r.scheme)
	return statefulSet
}

// loadBalancerForWildFly returns a loadBalancer service
func (r *ReconcileWildFlyServer) loadBalancerForWildFly(w *wildflyv1alpha1.WildFlyServer) *corev1.Service {
	labels := labelsForWildFly(w)
	loadBalancer := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      loadBalancerServiceName(w),
			Namespace: w.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: labels,
			// SessionAffinity: sessionAffinity,
			ClusterIP: corev1.ClusterIPNone,
			Ports: []corev1.ServicePort{
				{
					Name: "http",
					Port: httpApplicationPort,
				},
			},
		},
	}
	// Set WildFlyServer instance as the owner and controller
	controllerutil.SetControllerReference(w, loadBalancer, r.scheme)
	return loadBalancer
}

func (r *ReconcileWildFlyServer) routeForWildFly(w *wildflyv1alpha1.WildFlyServer) *routev1.Route {
	weight := int32(100)

	route := &routev1.Route{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "route.openshift.io/v1",
			Kind:       "Route",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      w.Name,
			Namespace: w.Namespace,
			Labels:    labelsForWildFly(w),
		},
		Spec: routev1.RouteSpec{
			To: routev1.RouteTargetReference{
				Kind:   "Service",
				Name:   loadBalancerServiceName(w),
				Weight: &weight,
			},
			Port: &routev1.RoutePort{
				TargetPort: intstr.FromString("http"),
			},
		},
	}
	// Set WildFlyServer instance as the owner and controller
	controllerutil.SetControllerReference(w, route, r.scheme)

	return route
}

// newSecretForCR returns an empty secret for holding the secrets merge
func (r *ReconcileWildFlyServer) txnRecoveryJBossCliPasswordSecret(w *wildflyv1alpha1.WildFlyServer) *corev1.Secret {
	password := generateToken(16)
	secret := &corev1.Secret{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Secret",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      getTxnRecoverySecretName(w),
			Namespace: w.Namespace,
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"username": wflyMgmtTxnRecoveryUserName,
			"password": password,
		},
	}
	// Set WildFlyServer instance as the owner and controller
	controllerutil.SetControllerReference(w, secret, r.scheme)
	return secret
}

func getWildflyServerPodStatusByName(w *wildflyv1alpha1.WildFlyServer, podName string) *wildflyv1alpha1.PodStatus {
	i := sort.Search(len(w.Status.Pods), func(i int) bool { return podName == w.Status.Pods[i].Name })
	if i < 0 || i >= len(w.Status.Pods) || podName != w.Status.Pods[i].Name {
		return nil
	}
	return &w.Status.Pods[i]
}

func containsValue(m *map[string]string, v string) bool {
	for _, x := range *m {
		if x == v {
			return true
		}
	}
	return false
}

func getTxnRecoverySecretName(w *wildflyv1alpha1.WildFlyServer) string {
	return w.Name + "-" + wflyMgmtTxnRecoveryUserName
}

func generateWflyMgmtHashedPassword(s *corev1.Secret) string {
	user := string(s.Data["username"])
	password := string(s.Data["password"])
	data := []byte(user + ":ManagementRealm:" + password)
	return fmt.Sprintf("%x", md5.Sum(data))
}

func generateToken(lenght int) string {
	b := make([]byte, lenght)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func fromJSONToReader(jsonData map[string]interface{}) (io.Reader, error) {
	jsonStreamBytes, err := json.Marshal(jsonData)
	if err != nil {
		return nil, fmt.Errorf("Fail to marshal JSON message %v", jsonData)
	}
	return bytes.NewBuffer(jsonStreamBytes), nil
}

func httpDigestPostWithJSON(hostname, username, password string, httpJSONData io.Reader) (*http.Response, error) {
	httpClient := &http.Client{Timeout: time.Second * 10}
	req, err := http.NewRequest("POST", hostname, httpJSONData)
	if err != nil {
		return nil, fmt.Errorf("Fail to create HTTP request for hostname %s", hostname)
	}
	req.Header.Set("Content-Type", "application/json")

	digestAuth := &wildflyutil.DigestHeaders{}
	digestAuth, err = digestAuth.Auth(username, password, hostname)
	if err != nil {
		return nil, fmt.Errorf("Fail to authenticate to %s for http WildFly management for username %s, error %v",
			hostname, username, err)
	}

	digestAuth.ApplyAuth(req)

	res, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Fail to invoke http WildFly managment to %s for username %s, error %v",
			hostname, username, err)
	}
	return res, nil
}

func decodeJSONBody(res *http.Response) (map[string]interface{}, error) {
	var jsonBody map[string]interface{}
	err := json.NewDecoder(res.Body).Decode(&jsonBody)
	if err != nil {
		return nil, fmt.Errorf("Fail parse HTTP body to JSON, error: %v", err)
	}
	return jsonBody, nil
}

// WARN: closing json body
func isMgmtOutcomeSuccesful(res *http.Response, jsonBody map[string]interface{}) bool {
	if res.StatusCode != http.StatusOK {
		return false
	}
	if jsonBody["outcome"] == "success" {
		return true
	}
	return false
}

func getJSONDataByIndex(json interface{}, indexes ...string) string {
	jsonInProgress := json
	for _, index := range indexes {
		switch vv := jsonInProgress.(type) {
		case map[string]interface{}:
			jsonInProgress = vv[index]
		default:
			return ""
		}
	}
	switch vv := jsonInProgress.(type) {
	case string:
		return vv
	case int:
		return strconv.Itoa(vv)
	case bool:
		return strconv.FormatBool(vv)
	default:
		return ""
	}
}

func socketConnect(hostname string, port int32, command string) (string, error) {
	// connect to socket
	toConnectTo := fmt.Sprintf("%v:%v", hostname, port)
	conn, _ := net.Dial("tcp", toConnectTo)
	// send to socket
	fmt.Fprintf(conn, command+"\n")
	// blocking operation, listen for reply
	message, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("Error to get response for command %s sending to %s:%v, error: %v",
			command, hostname, port, err)
	}
	return message, nil
}

// getPodStatus returns the pod names of the array of pods passed in
func getPodStatus(pods []corev1.Pod) (bool, []wildflyv1alpha1.PodStatus) {
	var requeue = false
	var podStatus []wildflyv1alpha1.PodStatus
	for _, pod := range pods {
		podStatus = append(podStatus, wildflyv1alpha1.PodStatus{
			Name:  pod.Name,
			PodIP: pod.Status.PodIP,
		})
		if pod.Status.PodIP == "" {
			requeue = true
		}
	}
	return requeue, podStatus
}

// createLivenessProbe create a Exec probe if the SERVER_LIVENESS_SCRIPT env var is present.
// Otherwise, it creates a HTTPGet probe that checks the /health endpoint on the admin port.
//
// If defined, the SERVER_LIVENESS_SCRIPT env var must be the path of a shell script that
// complies to the Kuberenetes probes requirements.
func createLivenessProbe() *corev1.Probe {
	livenessProbeScript, defined := os.LookupEnv("SERVER_LIVENESS_SCRIPT")
	if defined {
		return &corev1.Probe{
			Handler: corev1.Handler{
				Exec: &v1.ExecAction{
					Command: []string{"/bin/bash", "-c", livenessProbeScript},
				},
			},
			InitialDelaySeconds: 60,
		}
	}
	return &corev1.Probe{
		Handler: corev1.Handler{
			HTTPGet: &v1.HTTPGetAction{
				Path: "/health",
				Port: intstr.FromString("admin"),
			},
		},
		InitialDelaySeconds: 60,
	}
}

// createReadinessProbe create a Exec probe if the SERVER_READINESS_SCRIPT env var is present.
// Otherwise, it returns nil (i.e. no readiness probe is configured).
//
// If defined, the SERVER_READINESS_SCRIPT env var must be the path of a shell script that
// complies to the Kuberenetes probes requirements.
func createReadinessProbe() *corev1.Probe {
	readinessProbeScript, defined := os.LookupEnv("SERVER_READINESS_SCRIPT")
	if defined {
		return &corev1.Probe{
			Handler: corev1.Handler{
				Exec: &v1.ExecAction{
					Command: []string{"/bin/bash", "-c", readinessProbeScript},
				},
			},
		}
	}
	return nil
}

func labelsForWildFly(w *wildflyv1alpha1.WildFlyServer) map[string]string {
	labels := make(map[string]string)
	labels["app.kubernetes.io/name"] = w.Name
	labels["app.kubernetes.io/managed-by"] = os.Getenv("LABEL_APP_MANAGED_BY")
	labels["app.openshift.io/runtime"] = os.Getenv("LABEL_APP_RUNTIME")
	labels[labelWildflyOperatorInService] = "active"
	if w.Labels != nil {
		for labelKey, labelValue := range w.Labels {
			labels[labelKey] = labelValue
		}
	}
	return labels
}

func loadBalancerServiceName(w *wildflyv1alpha1.WildFlyServer) string {
	return w.Name + "-loadbalancer"
}

func errorIsNoMatchesForKind(err error, kind string, version string) bool {
	return strings.HasPrefix(err.Error(), fmt.Sprintf("no matches for kind \"%s\" in version \"%s\"", kind, version))
}
