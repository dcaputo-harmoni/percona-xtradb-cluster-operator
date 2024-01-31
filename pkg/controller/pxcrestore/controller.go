package pxcrestore

import (
	"context"
	"fmt"
	"time"

	"github.com/pkg/errors"
	batchv1 "k8s.io/api/batch/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8sretry "k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/percona/percona-xtradb-cluster-operator/clientcmd"
	api "github.com/percona/percona-xtradb-cluster-operator/pkg/apis/pxc/v1"
	"github.com/percona/percona-xtradb-cluster-operator/pkg/pxc/backup"
	"github.com/percona/percona-xtradb-cluster-operator/pkg/pxc/backup/storage"
	"github.com/percona/percona-xtradb-cluster-operator/version"
)

// Add creates a new PerconaXtraDBClusterRestore Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	r, err := newReconciler(mgr)
	if err != nil {
		return err
	}
	return add(mgr, r)
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) (reconcile.Reconciler, error) {
	sv, err := version.Server()
	if err != nil {
		return nil, fmt.Errorf("get version: %v", err)
	}

	cli, err := clientcmd.NewClient()
	if err != nil {
		return nil, errors.Wrap(err, "create clientcmd")
	}

	return &ReconcilePerconaXtraDBClusterRestore{
		client:               mgr.GetClient(),
		clientcmd:            cli,
		scheme:               mgr.GetScheme(),
		serverVersion:        sv,
		newStorageClientFunc: storage.NewClient,
	}, nil
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	return builder.ControllerManagedBy(mgr).
		Named("pxcrestore-controller").
		Watches(&api.PerconaXtraDBClusterRestore{}, &handler.EnqueueRequestForObject{}).
		Complete(r)
}

var _ reconcile.Reconciler = &ReconcilePerconaXtraDBClusterRestore{}

// ReconcilePerconaXtraDBClusterRestore reconciles a PerconaXtraDBClusterRestore object
type ReconcilePerconaXtraDBClusterRestore struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client    client.Client
	clientcmd *clientcmd.Client
	scheme    *runtime.Scheme

	serverVersion *version.ServerVersion

	newStorageClientFunc storage.NewClientFunc
}

// Reconcile reads that state of the cluster for a PerconaXtraDBClusterRestore object and makes changes based on the state read
// and what is in the PerconaXtraDBClusterRestore.Spec
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcilePerconaXtraDBClusterRestore) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	log := logf.FromContext(ctx)

	rr := reconcile.Result{
		RequeueAfter: time.Second * 5,
	}

	cr := &api.PerconaXtraDBClusterRestore{}
	err := r.client.Get(ctx, request.NamespacedName, cr)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			return rr, nil
		}
		// Error reading the object - requeue the request.
		return rr, err
	}

	switch cr.Status.State {
	case api.RestoreSucceeded, api.RestoreFailed:
		return reconcile.Result{}, nil
	}

	statusState := cr.Status.State
	statusMsg := ""

	defer func() {
		if err := setStatus(ctx, r.client, cr, statusState, statusMsg); err != nil {
			log.Error(err, "failed to set status")
		}
	}()

	otherRestore, err := isOtherRestoreInProgress(ctx, r.client, cr)
	if err != nil {
		return rr, errors.Wrap(err, "failed to check if other restore is in progress")
	}
	if otherRestore != nil {
		err = errors.Errorf("unable to continue, concurent restore job %s running now", otherRestore.Name)
		statusState = api.RestoreFailed
		statusMsg = err.Error()
		return rr, err
	}

	if err := cr.CheckNsetDefaults(); err != nil {
		statusState = api.RestoreFailed
		statusMsg = err.Error()
		return rr, err
	}

	cluster := new(api.PerconaXtraDBCluster)
	if err := r.client.Get(ctx, types.NamespacedName{Name: cr.Spec.PXCCluster, Namespace: cr.Namespace}, cluster); err != nil {
		if k8serrors.IsNotFound(err) {
			statusState = api.RestoreFailed
			statusMsg = err.Error()
		}
		return rr, errors.Wrapf(err, "get cluster %s", cr.Spec.PXCCluster)
	}

	if err := cluster.CheckNSetDefaults(r.serverVersion, log); err != nil {
		statusState = api.RestoreFailed
		statusMsg = err.Error()
		return rr, errors.Wrap(err, "wrong PXC options")
	}

	err = backup.CheckPITRErrors(ctx, r.client, r.clientcmd, cluster)
	if err != nil {
		statusState = api.RestoreFailed
		statusMsg = err.Error()
		return rr, err
	}

	bcp, err := getBackup(ctx, r.client, cr)
	if err != nil {
		statusState = api.RestoreFailed
		statusMsg = err.Error()
		return rr, errors.Wrap(err, "get backup")
	}

	switch statusState {
	case api.RestoreNew:
		annotations := cr.GetAnnotations()
		_, unsafePITR := annotations[api.AnnotationUnsafePITR]
		cond := meta.FindStatusCondition(bcp.Status.Conditions, api.BackupConditionPITRReady)
		if cond != nil && cond.Status == metav1.ConditionFalse && !unsafePITR {
			statusState = api.RestoreFailed
			statusMsg = fmt.Sprintf("Backup doesn't guarantee consistent recovery with PITR. Annotate PerconaXtraDBClusterRestore with %s to force it.", api.AnnotationUnsafePITR)
			return rr, nil
		}
		err = r.validate(ctx, cr, bcp, cluster)
		if err != nil {
			if errors.Is(err, errWaitValidate) {
				return rr, nil
			}
			err = errors.Wrap(err, "failed to validate restore job")
			return rr, err
		}
		cr.Status.PXCSize = cluster.Spec.PXC.Size
		cr.Status.AllowUnsafeConfig = cluster.Spec.AllowUnsafeConfig
		log.Info("stopping cluster", "cluster", cr.Spec.PXCCluster)
		statusState = api.RestoreStopCluster
	case api.RestoreStopCluster:
		err = stopCluster(ctx, r.client, cluster.DeepCopy())
		if err != nil {
			switch err {
			case errWaitingPods, errWaitingPVC:
				log.Info("waiting for cluster to stop", "cluster", cr.Spec.PXCCluster, "msg", err.Error())
				return rr, nil
			}
			return rr, errors.Wrapf(err, "stop cluster %s", cluster.Name)
		}

		log.Info("starting restore", "cluster", cr.Spec.PXCCluster, "backup", cr.Spec.BackupName)
		err = r.restore(ctx, cr, bcp, cluster)
		if err != nil {
			if errors.Is(err, errWaitInit) {
				return rr, nil
			}
			err = errors.Wrap(err, "run restore")
			return rr, err
		}
		statusState = api.RestoreRestore
	case api.RestoreRestore:
		restorer, err := r.getRestorer(cr, bcp, cluster)
		if err != nil {
			return rr, errors.Wrap(err, "failed to get restorer")
		}
		restorerJob, err := restorer.Job()
		if err != nil {
			return rr, errors.Wrap(err, "failed to create restore job")
		}
		job := new(batchv1.Job)
		if err := r.client.Get(ctx, types.NamespacedName{
			Name:      restorerJob.Name,
			Namespace: restorerJob.Namespace,
		}, job); err != nil {
			return rr, errors.Wrap(err, "failed to get restore job")
		}

		finished, err := isJobFinished(job)
		if err != nil {
			statusState = api.RestoreFailed
			statusMsg = err.Error()
			return rr, err
		}
		if !finished {
			log.Info("Waiting for restore job to finish", "job", job.Name)
			return rr, nil
		}

		if cr.Spec.PITR != nil {
			if cluster.Spec.Pause {
				err = k8sretry.RetryOnConflict(k8sretry.DefaultRetry, func() error {
					current := new(api.PerconaXtraDBCluster)
					err := r.client.Get(ctx, types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace}, current)
					if err != nil {
						return errors.Wrap(err, "get cluster")
					}
					current.Spec.Pause = false
					current.Spec.PXC.Size = 1
					current.Spec.AllowUnsafeConfig = true
					return r.client.Update(ctx, current)
				})
				if err != nil {
					return rr, errors.Wrap(err, "update cluster")
				}
				return rr, nil
			} else {
				if cluster.Status.ObservedGeneration == cluster.Generation && cluster.Status.PXC.Status == api.AppStateReady {
					log.Info("Waiting for cluster to start", "cluster", cluster.Name)
					return rr, nil
				}
			}

			log.Info("point-in-time recovering", "cluster", cr.Spec.PXCCluster)
			err = r.pitr(ctx, cr, bcp, cluster)
			if err != nil {
				if errors.Is(err, errWaitInit) {
					return rr, nil
				}
				return rr, errors.Wrap(err, "run pitr")
			}
			statusState = api.RestorePITR
			return rr, nil
		}

		log.Info("starting cluster", "cluster", cr.Spec.PXCCluster)
		statusState = api.RestoreStartCluster
	case api.RestorePITR:
		restorer, err := r.getRestorer(cr, bcp, cluster)
		if err != nil {
			return rr, errors.Wrap(err, "failed to get restorer")
		}
		restorerJob, err := restorer.PITRJob()
		if err != nil {
			return rr, errors.Wrap(err, "failed to create restore job")
		}
		job := new(batchv1.Job)
		if err := r.client.Get(ctx, types.NamespacedName{
			Name:      restorerJob.Name,
			Namespace: restorerJob.Namespace,
		}, job); err != nil {
			return rr, errors.Wrap(err, "failed to get restore job")
		}

		finished, err := isJobFinished(job)
		if err != nil {
			statusState = api.RestoreFailed
			statusMsg = err.Error()
			return rr, err
		}
		if !finished {
			log.Info("Waiting for restore job to finish", "job", job.Name)
			return rr, nil
		}

		log.Info("starting cluster", "cluster", cr.Spec.PXCCluster)
		statusState = api.RestoreStartCluster
	case api.RestoreStartCluster:
		if cluster.Spec.Pause || (cr.Status.PXCSize != 0 && cluster.Spec.PXC.Size != cr.Status.PXCSize) {
			err = k8sretry.RetryOnConflict(k8sretry.DefaultRetry, func() error {
				current := new(api.PerconaXtraDBCluster)
				err := r.client.Get(ctx, types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace}, current)
				if err != nil {
					return errors.Wrap(err, "get cluster")
				}
				current.Spec.Pause = false
				if cr.Status.PXCSize != 0 {
					current.Spec.PXC.Size = cr.Status.PXCSize
					current.Spec.AllowUnsafeConfig = cr.Status.AllowUnsafeConfig
				}
				return r.client.Update(ctx, current)
			})
			if err != nil {
				return rr, errors.Wrap(err, "update cluster")
			}
		} else {
			if cluster.Status.ObservedGeneration == cluster.Generation && cluster.Status.PXC.Status == api.AppStateReady {
				restorer, err := r.getRestorer(cr, bcp, cluster)
				if err != nil {
					return rr, errors.Wrap(err, "failed to get restorer")
				}
				if err := restorer.Finalize(ctx); err != nil {
					return rr, errors.Wrap(err, "failed to finalize restore")
				}

				statusState = api.RestoreSucceeded
				return rr, nil
			}
		}

		log.Info("Waiting for cluster to start", "cluster", cluster.Name)
	}

	return rr, nil
}
