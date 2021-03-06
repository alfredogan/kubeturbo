package integration

import (
	"context"
	"fmt"
	"reflect"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	kubeclientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	restclient "k8s.io/client-go/rest"

	. "github.com/onsi/ginkgo"

	"github.com/turbonomic/kubeturbo/pkg/action"
	"github.com/turbonomic/kubeturbo/pkg/action/executor"
	"github.com/turbonomic/kubeturbo/pkg/cluster"
	"github.com/turbonomic/kubeturbo/test/integration/framework"
	"github.com/turbonomic/turbo-go-sdk/pkg/proto"
)

var _ = Describe("Action Execution when quota exists ", func() {
	f := framework.NewTestFramework("action-executor")
	var kubeConfig *restclient.Config
	var namespace string
	var actionHandler *action.ActionHandler
	var kubeClient *kubeclientset.Clientset

	// This quota yaml maps to exactly one pod for all resources
	// A pod move will thus have to evaluate and update all the resorces
	// in the quota and thus verifying each resource update.
	quotaYaml := `
apiVersion: v1
kind: ResourceQuota
metadata:
  generateName: test-quota-
spec:
  hard:
    requests.cpu: 50m
    requests.memory: 100Mi
    limits.cpu: 100m
    limits.memory: 200Mi
    pods: "1"
`

	BeforeEach(func() {
		f.BeforeEach()
		// The following setup is shared across tests here
		if kubeConfig == nil {

			kubeConfig := f.GetKubeConfig()
			kubeClient = f.GetKubeClient("action-executor")

			dynamicClient, err := dynamic.NewForConfig(kubeConfig)
			if err != nil {
				framework.Failf("Failed to generate dynamic client for kubernetes test cluster: %v", err)
			}

			cluster.NewClusterScraper(kubeClient, dynamicClient)
			actionHandlerConfig := action.NewActionHandlerConfig("", nil, nil,
				cluster.NewClusterScraper(kubeClient, dynamicClient), []string{"*"}, nil, false)

			actionHandler = action.NewActionHandler(actionHandlerConfig)

			namespace = f.TestNamespaceName()
		}
	})

	Describe("executing action move pod", func() {
		It("should result in new pod on target node", func() {
			quota := createQuota(kubeClient, namespace, quotaYaml)
			defer deleteQuota(kubeClient, quota)

			dep, err := createDeployResource(kubeClient, depSingleContainerWithResources(namespace, "", 1, false, true, false))
			framework.ExpectNoError(err, "Error creating test resources")
			defer deleteDeploy(kubeClient, dep)

			pod, err := getDeploymentsPod(kubeClient, dep.Name, namespace, "")
			framework.ExpectNoError(err, "Error getting deployments pod")
			// This should not happen. We should ideally get a pod.
			if pod == nil {
				framework.Failf("Failed to find a pod for deployment: %s", dep.Name)
			}

			targetNodeName := getTargetSENodeName(f, pod)
			if targetNodeName == "" {
				framework.Failf("Failed to find a pod for deployment: %s", dep.Name)
			}

			_, err = actionHandler.ExecuteAction(newActionExecutionDTO(proto.ActionItemDTO_MOVE,
				newTargetSEFromPod(pod), newHostSEFromNodeName(targetNodeName)), nil, &mockProgressTrack{})
			framework.ExpectNoError(err, "Move action failed")

			pod = validateMovedPod(kubeClient, dep.Name, "deployment", namespace, targetNodeName)
			validateGCAnnotationRemoved(kubeClient, pod)
			validateQuotaReverted(kubeClient, quota)
		})
	})

	Describe("executing action move pod with volume attached", func() {
		It("should result in new pod on target node", func() {
			// TODO: The storageclass can be taken as a configurable parameter from commandline
			// This works against a kind cluster. Ensure to update the storageclass name to the right name when
			// running against a different cluster.
			quota := createQuota(kubeClient, namespace, quotaYaml)
			defer deleteQuota(kubeClient, quota)

			pvc, err := createVolumeClaim(kubeClient, namespace, "standard")
			dep, err := createDeployResource(kubeClient, depSingleContainerWithResources(namespace, pvc.Name, 1, true, true, false))
			framework.ExpectNoError(err, "Error creating test resources")
			defer deleteDeploy(kubeClient, dep)

			pod, err := getDeploymentsPod(kubeClient, dep.Name, namespace, "")
			framework.ExpectNoError(err, "Error getting deployments pod")
			// This should not happen. We should ideally get a pod.
			if pod == nil {
				framework.Failf("Failed to find a pod for deployment: %s", dep.Name)
			}

			targetNodeName := getTargetSENodeName(f, pod)
			if targetNodeName == "" {
				framework.Failf("Failed to find a pod for deployment: %s", dep.Name)
			}

			_, err = actionHandler.ExecuteAction(newActionExecutionDTO(proto.ActionItemDTO_MOVE,
				newTargetSEFromPod(pod), newHostSEFromNodeName(targetNodeName)), nil, &mockProgressTrack{})
			framework.ExpectNoError(err, "Move action failed")

			validateMovedPod(kubeClient, dep.Name, "deployment", namespace, targetNodeName)
			validateGCAnnotationRemoved(kubeClient, pod)
			validateQuotaReverted(kubeClient, quota)
		})
	})

	// TODO: this particular Describe is currently used as the teardown for this
	// whole test (not the suite).
	// This will work only if run sequentially. Find a better way to do this.
	Describe("test teardowon", func() {
		It(fmt.Sprintf("Deleting framework namespace: %s", namespace), func() {
			f.AfterEach()
		})
	})
})

var codecs = scheme.Codecs

func decodeYaml(bytes []byte) (runtime.Object, error) {
	decode := codecs.UniversalDeserializer().Decode
	obj, _, err := decode(bytes, nil, nil)
	if err != nil {
		return nil, err
	}

	return obj, nil
}

func createQuota(client *kubeclientset.Clientset, namespace, quotaYaml string) *corev1.ResourceQuota {
	obj, err := decodeYaml([]byte(quotaYaml))
	if err != nil {
		framework.Failf("Failed to decode yaml, %s: %v.", quotaYaml, err)
	}
	quota, isQuota := obj.(*corev1.ResourceQuota)
	if !isQuota {
		framework.Failf("Incorrect object type after decoding %s.", quotaYaml)
	}
	createdQuota, err := client.CoreV1().ResourceQuotas(namespace).Create(context.TODO(), quota, metav1.CreateOptions{})
	if err != nil {
		framework.Failf("Error creating quota %s. %v", quotaYaml, err)
	}

	return createdQuota
}

func deleteQuota(client *kubeclientset.Clientset, quota *corev1.ResourceQuota) {
	err := client.CoreV1().ResourceQuotas(quota.Namespace).Delete(context.TODO(), quota.Name, metav1.DeleteOptions{})
	if err != nil {
		framework.Failf("Error deleting quota %s/%s. %v", quota.Namespace, quota.Name, err)
	}
}

func deleteDeploy(client *kubeclientset.Clientset, dep *appsv1.Deployment) {
	err := client.AppsV1().Deployments(dep.Namespace).Delete(context.TODO(), dep.Name, metav1.DeleteOptions{})
	if err != nil {
		framework.Failf("Error deleting deployment %s/%s. %v", dep.Namespace, dep.Name, err)
	}
}

func validateGCAnnotationRemoved(client kubeclientset.Interface, pod *corev1.Pod) {
	_, gcAnnotationFound := pod.Annotations[executor.QuotaAnnotationKey]
	if gcAnnotationFound {
		framework.Failf("The GC annotation was not removed on pod %s/%s.", pod.Name, pod.Namespace)
	}
}

func validateQuotaReverted(client kubeclientset.Interface, quota *corev1.ResourceQuota) {
	usedQuota, err := client.CoreV1().ResourceQuotas(quota.Namespace).Get(context.TODO(), quota.Name, metav1.GetOptions{})
	if err != nil {
		framework.Failf("Error getting quota %s/%s again. %v", quota.Namespace, quota.Name, err)
	}
	if !reflect.DeepEqual(quota.Spec.Hard, usedQuota.Spec.Hard) {
		framework.Failf("Quota does not seem to be reverted to its original value after action original: %v new: %v", quota, usedQuota)
	}
}
