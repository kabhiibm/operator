/*
Copyright 2019 The Tekton Authors

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

package tektonpipeline

import (
	"context"
	"fmt"

	mf "github.com/manifestival/manifestival"
	"github.com/tektoncd/operator/pkg/apis/operator/v1alpha1"
	tektonpipelinereconciler "github.com/tektoncd/operator/pkg/client/injection/reconciler/operator/v1alpha1/tektonpipeline"
	"github.com/tektoncd/operator/pkg/reconciler/common"
	"github.com/tektoncd/operator/pkg/reconciler/kubernetes/tektoninstallerset/client"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"knative.dev/pkg/logging"
	pkgreconciler "knative.dev/pkg/reconciler"
)

const (
	resourceKind = v1alpha1.KindTektonPipeline

	proxyLabel = "operator.tekton.dev/disable-proxy=true"
)

// Reconciler implements controller.Reconciler for TektonPipeline resources.
type Reconciler struct {
	// installer Set client to do CRUD operations for components
	installerSetClient *client.InstallerSetClient
	//manifest has the source manifest of Tekton Pipeline for a
	// particular version
	manifest mf.Manifest
	// Platform-specific behavior to affect the transform
	extension common.Extension
	// kube client to interact with core k8s resources
	kubeClientSet kubernetes.Interface
	// version of pipelines which we are installing
	pipelineVersion string
}

// Check that our Reconciler implements controller.Reconciler
var _ tektonpipelinereconciler.Interface = (*Reconciler)(nil)

// ReconcileKind compares the actual state with the desired, and attempts to
// converge the two.
func (r *Reconciler) ReconcileKind(ctx context.Context, tp *v1alpha1.TektonPipeline) pkgreconciler.Event {
	logger := logging.FromContext(ctx).With("name", tp.GetName())
	tp.Status.InitializeConditions()
	tp.Status.SetVersion(r.pipelineVersion)

	if tp.GetName() != v1alpha1.PipelineResourceName {
		msg := fmt.Sprintf("Resource ignored, Expected Name: %s, Got Name: %s",
			v1alpha1.PipelineResourceName,
			tp.GetName(),
		)
		logger.Error(msg)
		tp.Status.MarkNotReady(msg)
		return nil
	}

	// Pass the object through defaulting
	tp.SetDefaults(ctx)

	if err := r.targetNamespaceCheck(ctx, tp); err != nil {
		return err
	}

	if err := r.extension.PreReconcile(ctx, tp); err != nil {
		tp.Status.MarkPreReconcilerFailed(fmt.Sprintf("PreReconciliation failed: %s", err.Error()))
		return err
	}

	// Mark PreReconcile Complete
	tp.Status.MarkPreReconcilerComplete()

	if err := r.installerSetClient.MainSet(ctx, tp); err != nil {
		logger.Errorf("failed for main set: %v", err)
		return err
	}

	if err := r.extension.PostReconcile(ctx, tp); err != nil {
		tp.Status.MarkPostReconcilerFailed(fmt.Sprintf("PostReconciliation failed: %s", err.Error()))
		return err
	}

	// Mark PostReconcile Complete
	tp.Status.MarkPostReconcilerComplete()

	return nil
}

func (r *Reconciler) targetNamespaceCheck(ctx context.Context, tp *v1alpha1.TektonPipeline) error {
	labels := r.manifest.Filter(mf.ByKind("Namespace")).Resources()[0].GetLabels()

	ns, err := r.kubeClientSet.CoreV1().Namespaces().Get(ctx, tp.GetSpec().GetTargetNamespace(), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			if err := common.CreateTargetNamespace(ctx, labels, tp, r.kubeClientSet); err != nil {
				return err
			}
			return nil
		}
		return err
	}
	for key, value := range labels {
		ns.Labels[key] = value
	}
	_, err = r.kubeClientSet.CoreV1().Namespaces().Update(ctx, ns, metav1.UpdateOptions{})
	return err
}
