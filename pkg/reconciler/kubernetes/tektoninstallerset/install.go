/*
Copyright 2021 The Tekton Authors

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

package tektoninstallerset

import (
	"context"
	"fmt"
	"strings"

	mf "github.com/manifestival/manifestival"
	"github.com/tektoncd/operator/pkg/apis/operator/v1alpha1"
	"github.com/tektoncd/operator/pkg/reconciler/shared/hash"
	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/ptr"
)

const (
	replicasForHash = 999
)

type installer struct {
	manifest        *mf.Manifest
	mfClient        mf.Client
	logger          *zap.SugaredLogger
	crds            []unstructured.Unstructured
	clusterScoped   []unstructured.Unstructured
	namespaceScoped []unstructured.Unstructured
	deployment      []unstructured.Unstructured
}

func NewInstaller(manifest *mf.Manifest, mfClient mf.Client, logger *zap.SugaredLogger) *installer {
	installer := &installer{
		manifest:        manifest,
		mfClient:        mfClient,
		logger:          logger,
		crds:            []unstructured.Unstructured{},
		clusterScoped:   []unstructured.Unstructured{},
		namespaceScoped: []unstructured.Unstructured{},
		deployment:      []unstructured.Unstructured{},
	}

	// we filter out resource as some resources are dependent on others
	// for eg. namespace should be created before configmap
	// non k8s core resources like openshift resources will be classified as
	// namespace scoped
	for _, res := range manifest.Resources() {
		if strings.ToLower(res.GetKind()) == "customresourcedefinition" {
			installer.crds = append(installer.crds, res)
			continue
		} else if res.GetKind() == "Deployment" {
			installer.deployment = append(installer.deployment, res)
			continue
		}
		if isClusterScoped(res.GetKind()) && strings.ToLower(res.GetKind()) != "clusterrolebinding" {
			installer.clusterScoped = append(installer.clusterScoped, res)
			continue
		}
		installer.namespaceScoped = append(installer.namespaceScoped, res)
	}
	return installer
}

// https://github.com/manifestival/manifestival/blob/af1baacf01ec54390c3cbd46ee561d52b2b4ab14/transform.go#L107
func isClusterScoped(kind string) bool {
	switch strings.ToLower(kind) {
	case "componentstatus",
		"namespace",
		"node",
		"persistentvolume",
		"mutatingwebhookconfiguration",
		"validatingwebhookconfiguration",
		"customresourcedefinition",
		"apiservice",
		"meshpolicy",
		"tokenreview",
		"selfsubjectaccessreview",
		"selfsubjectrulesreview",
		"subjectaccessreview",
		"certificatesigningrequest",
		"podsecuritypolicy",
		"clusterrolebinding",
		"clusterrole",
		"priorityclass",
		"storageclass",
		"volumeattachment":
		return true
	}
	return false
}

func (i *installer) ensureResources(resources []unstructured.Unstructured) error {
	for _, r := range resources {
		expectedHash, err := hash.Compute(r.Object)
		if err != nil {
			return err
		}

		i.logger.Infof("fetching resource %s: %s/%s", r.GetKind(), r.GetNamespace(), r.GetName())

		res, err := i.mfClient.Get(&r)
		if err != nil {
			if apierrs.IsNotFound(err) {
				i.logger.Infof("resource not found, creating %s: %s/%s", r.GetKind(), r.GetNamespace(), r.GetName())
				// add hash on the resource of expected manifest and create
				anno := r.GetAnnotations()
				if anno == nil {
					anno = map[string]string{}
				}
				anno[v1alpha1.LastAppliedHashKey] = expectedHash
				r.SetAnnotations(anno)
				err = i.mfClient.Create(&r)
				if err != nil {
					return err
				}
				continue
			}
			return err
		}

		i.logger.Infof("found resource %s: %s/%s, checking for update!", r.GetKind(), r.GetNamespace(), r.GetName())

		// if resource exist then check if expected hash is different from the one
		// on the resource
		hashOnResource := res.GetAnnotations()[v1alpha1.LastAppliedHashKey]

		if expectedHash == hashOnResource {
			continue
		}

		i.logger.Infof("updating resource %s: %s/%s", r.GetKind(), r.GetNamespace(), r.GetName())

		anno := r.GetAnnotations()
		if anno == nil {
			anno = map[string]string{}
		}
		anno[v1alpha1.LastAppliedHashKey] = expectedHash
		r.SetAnnotations(anno)

		installManifests, err := mf.ManifestFrom(mf.Slice([]unstructured.Unstructured{r}), mf.UseClient(i.mfClient))
		if err != nil {
			return err
		}
		if err := installManifests.Apply(); err != nil {
			return err
		}
	}
	return nil
}

func (i *installer) EnsureCRDs() error {
	return i.ensureResources(i.crds)
}

func (i *installer) EnsureClusterScopedResources() error {
	return i.ensureResources(i.clusterScoped)
}

func (i *installer) EnsureNamespaceScopedResources() error {
	return i.ensureResources(i.namespaceScoped)
}

func (i *installer) EnsureDeploymentResources() error {
	reconcileAgain := false
	for _, d := range i.deployment {
		if err := i.ensureDeployment(&d); err != nil {
			// if error is RECONCILER_AGAIN_ERR, then
			// continue with rest of the Deployments in the manifest list
			if err == v1alpha1.RECONCILE_AGAIN_ERR {
				reconcileAgain = true
				continue
			}
			return err
		}
	}
	// if atleast 1 instance in the loop returned RECONCILE_AGAIN_ERR, then
	// return RECONCILE_AGAIN_ERR
	if reconcileAgain {
		return v1alpha1.RECONCILE_AGAIN_ERR
	}
	return nil
}

func computeDeploymentHash(d appsv1.Deployment) (string, error) {
	// set replicas to a constant value and then calculate hash so
	// that later if user updates replicas, we can exclude that change.
	// setting the replicas to same const and checking the hash
	// so that we can allow only replica change revert any other change
	// done to the deployment spec
	d.Spec.Replicas = ptr.Int32(replicasForHash)

	return hash.Compute(d.Spec)
}

func (i *installer) createDeployment(expected *unstructured.Unstructured) error {

	dep := &appsv1.Deployment{}
	err := runtime.DefaultUnstructuredConverter.FromUnstructured(expected.Object, dep)
	if err != nil {
		return err
	}

	hash, err := computeDeploymentHash(*dep)
	if err != nil {
		return fmt.Errorf("failed to compute hash of deployment: %v", err)
	}

	if len(dep.Annotations) == 0 {
		dep.Annotations = map[string]string{}
	}
	dep.Annotations[v1alpha1.LastAppliedHashKey] = hash

	unstrObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(dep)
	if err != nil {
		return err
	}
	expected.SetUnstructuredContent(unstrObj)

	return i.mfClient.Create(expected)
}

func (i *installer) updateDeployment(existing *unstructured.Unstructured, existingDeployment, expectedDeployment *appsv1.Deployment) error {
	i.logger.Infof("updating resource %s: %s/%s", existing.GetKind(), existing.GetNamespace(), existing.GetName())

	// save on cluster replicas in a var and assign it back to deployment
	onClusterReplicas := existingDeployment.Spec.Replicas

	existingDeployment.Spec = expectedDeployment.Spec
	existingDeployment.Spec.Replicas = onClusterReplicas

	// compute new hash of spec and add as annotation
	newHash, err := computeDeploymentHash(*existingDeployment)
	if err != nil {
		return fmt.Errorf("failed to compute new hash of existing deployment: %v", err)
	}

	if len(existingDeployment.Annotations) == 0 {
		existingDeployment.Annotations = map[string]string{}
	}

	existingDeployment.Annotations[v1alpha1.LastAppliedHashKey] = newHash

	unstrObj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(existingDeployment)
	if err != nil {
		return err
	}
	existing.SetUnstructuredContent(unstrObj)

	err = i.mfClient.Update(existing)
	if err != nil {
		return v1alpha1.RECONCILE_AGAIN_ERR
	}
	return err
}

func (i *installer) ensureDeployment(expected *unstructured.Unstructured) error {
	i.logger.Infof("fetching resource %s: %s/%s", expected.GetKind(), expected.GetNamespace(), expected.GetName())

	// check if deployment already exist
	existing, err := i.mfClient.Get(expected)
	if err != nil {
		// If deployment doesn't exist, then create new
		if apierrs.IsNotFound(err) {
			i.logger.Infof("resource not found, creating %s: %s/%s", expected.GetKind(), expected.GetNamespace(), expected.GetName())
			if err := i.createDeployment(expected); err == nil {
				return err
			}
			return nil
		}
		return err
	}

	i.logger.Infof("found resource %s: %s/%s, checking for update!", existing.GetKind(), existing.GetNamespace(), existing.GetName())

	// if already exist then check if spec is changed
	existingDeployment := &appsv1.Deployment{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(existing.Object, existingDeployment); err != nil {
		return err
	}

	expectedDeployment := &appsv1.Deployment{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(expected.Object, expectedDeployment); err != nil {
		return err
	}

	// compare existing deployment spec hash with the one saved in annotation
	// if annotation doesn't exist then update the deployment

	existingDepSpecHash, err := computeDeploymentHash(*existingDeployment)
	if err != nil {
		return fmt.Errorf("failed to compute hash of existing deployment: %v", err)
	}

	hashFromAnnotation, hashExist := existingDeployment.Annotations[v1alpha1.LastAppliedHashKey]

	// if hash doesn't exist then update the deployment with hash
	if !hashExist {
		return i.updateDeployment(existing, existingDeployment, expectedDeployment)
	}

	// if both hashes are same, that means deployment on cluster is the same as when it
	// was created (there may be change in replica which we allow)
	if existingDepSpecHash == hashFromAnnotation {

		// there might be a case where deployment in installerSet spec might have changed
		// compare the expected deployment spec hash with the hash in annotation
		expectedDepSpecHash, err := computeDeploymentHash(*expectedDeployment)
		if err != nil {
			return fmt.Errorf("failed to compute hash of expected deployment: %v", err)
		}

		if expectedDepSpecHash != hashFromAnnotation {
			return i.updateDeployment(existing, existingDeployment, expectedDeployment)
		}

		return nil
	}

	// hash is changed so revert back to original deployment
	// keeping the replicas change if exist

	return i.updateDeployment(existing, existingDeployment, expectedDeployment)
}

func (i *installer) IsWebhookReady() error {
	for _, u := range i.deployment {
		if !strings.Contains(u.GetName(), "webhook") {
			continue
		}
		err := i.isDeploymentReady(&u)
		if err != nil {
			return err
		}
	}
	return nil
}

func (i *installer) IsControllerReady() error {
	for _, u := range i.deployment {
		if !strings.Contains(u.GetName(), "controller") {
			continue
		}
		err := i.isDeploymentReady(&u)
		if err != nil {
			return err
		}
	}
	return nil
}

func (i *installer) AllDeploymentsReady() error {
	for _, u := range i.deployment {
		if strings.Contains(u.GetName(), "controller") ||
			strings.Contains(u.GetName(), "webhook") {
			continue
		}
		err := i.isDeploymentReady(&u)
		if err != nil {
			return err
		}
	}
	return nil
}

func (i *installer) IsJobCompleted(ctx context.Context, labels map[string]string, installSetName string) error {
	for _, u := range i.manifest.Filter(mf.ByKind("Job")).Resources() {
		resource, err := i.mfClient.Get(&u)
		if err != nil {
			return err
		}
		job := &batchv1.Job{}
		if err := scheme.Scheme.Convert(resource, job, nil); err != nil {
			return err
		}

		logger := logging.FromContext(ctx)
		if !isJobCompleted(job) {
			logger.Info("job not ready in installerset, name: %s, created-by: %s, in namespace: %s", installSetName, labels[v1alpha1.CreatedByKey], job.GetNamespace())
			return fmt.Errorf("Job not successful")
		}
	}

	return nil
}

func (i *installer) isDeploymentReady(d *unstructured.Unstructured) error {
	resource, err := i.mfClient.Get(d)
	if err != nil {
		return err
	}

	deployment := &appsv1.Deployment{}
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(resource.Object, deployment)
	if err != nil {
		return err
	}

	if !isDeploymentAvailable(deployment) {
		return fmt.Errorf("%s deployment not ready", deployment.GetName())
	}

	return nil
}

func isDeploymentAvailable(d *appsv1.Deployment) bool {
	for _, c := range d.Status.Conditions {
		if c.Type == appsv1.DeploymentAvailable && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func isJobCompleted(d *batchv1.Job) bool {
	for _, c := range d.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
