package reconciler

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/yard-turkey/lib-bucket-provisioner/pkg/apis/objectbucket.io/v1alpha1"
	"github.com/yard-turkey/lib-bucket-provisioner/pkg/provisioner/api"
	pErr "github.com/yard-turkey/lib-bucket-provisioner/pkg/provisioner/api/errors"
)

// Options encapsulates configurable fields in the reconciler.  Defaults are set if not defined.
type Options struct {
	RetryInterval time.Duration
	RetryTimeout  time.Duration
}

// ObjectBucketClaimReconciler implements a set of methods for processing OBC events and
type ObjectBucketClaimReconciler struct {
	*internalClient

	provisionerName string
	provisioner     api.Provisioner

	retryInterval time.Duration
	retryTimeout  time.Duration
}

var _ reconcile.Reconciler = &ObjectBucketClaimReconciler{}

// NewObjectBucketClaimReconciler constructs an OBC reconciler to be injected into the controller by NewProvisioner().
// This reconciler is the core logic for OBC controller.
// client.Client should be generated by the manager after its scheme has been updated.
// scheme is the manager's updated scheme.
// name is the name of the provisioner
// provisioner is the implemented Provisioner interface defined by the consumer of the library
// options are configurable settings to tweak retry logic within the Reconcile call stack.
func NewObjectBucketClaimReconciler(client client.Client, scheme *runtime.Scheme, name string, provisioner api.Provisioner, options Options) *ObjectBucketClaimReconciler {

	log.Info("constructing new reconciler", "provisioner", name)

	if options.RetryInterval < defaultRetryBaseInterval {
		options.RetryInterval = defaultRetryBaseInterval
	}
	logD.Info("retry loop setting", "RetryBaseInterval", options.RetryInterval.Seconds())
	if options.RetryTimeout < defaultRetryTimeout {
		options.RetryTimeout = defaultRetryTimeout
	}
	logD.Info("retry loop setting", "RetryTimeout", options.RetryTimeout.Seconds())

	return &ObjectBucketClaimReconciler{
		internalClient: &internalClient{
			ctx:    context.Background(),
			Client: client,
			scheme: scheme,
		},
		provisionerName: strings.ToLower(name),
		provisioner:     provisioner,
		retryInterval:   options.RetryInterval,
		retryTimeout:    options.RetryTimeout,
	}
}

// Reconcile implements the Reconciler interface.  This function contains the business logic of the
// OBC controller.  Currently, the process strictly serves as a POC for an OBC controller and is
// extremely fragile.
func (r *ObjectBucketClaimReconciler) Reconcile(request reconcile.Request) (reconcile.Result, error) {

	setLoggersWithRequest(request)

	logD.Info("new Reconcile iteration")

	var done = reconcile.Result{Requeue: false}

	obc, err := claimForKey(request.NamespacedName, r.internalClient)

	/**************************
	 Delete or Revoke Bucket
	***************************/
	if err != nil {
		// the OBC was deleted or some other error
		log.Info("error getting claim")
		if errors.IsNotFound(err) {
			log.Info("looks like the OBC was deleted, proceeding with cleanup")
			err := r.handleDeleteClaim(request.NamespacedName)
			if err != nil {
				log.Error(err, "error cleaning up ObjectBucket: %v")
			}
			return done, err
		}
		return done, fmt.Errorf("error getting claim for request key %q", request)
	}

	/*******************************************************
	 Provision New Bucket or Grant Access to Existing Bucket
	********************************************************/
	if !shouldProvision(obc) {
		log.Info("skipping provision")
		return done, nil
	}
	class, err := storageClassForClaim(obc, r.internalClient)
	if err != nil {
		return done, err
	}
	if !r.supportedProvisioner(class.Provisioner) {
		log.Info("unsupported provisioner", "got", class.Provisioner)
		return done, nil
	}
	greenfield := scForNewBkt(class)

	// By now, we should know that the OBC matches our provisioner, lacks an OB, and thus requires provisioning
	err = r.handleProvisionClaim(request.NamespacedName, obc, class, greenfield)

	// If handleReconcile() errors, the request will be re-queued.  In the distant future, we will likely want some ignorable error types in order to skip re-queuing
	return done, err
}

// handleProvision is an extraction of the core provisioning process in order to defer clean up
// on a provisioning failure
func (r *ObjectBucketClaimReconciler) handleProvisionClaim(key client.ObjectKey, obc *v1alpha1.ObjectBucketClaim, class *storagev1.StorageClass, isDynamicProvisioning bool) error {

	var (
		ob        *v1alpha1.ObjectBucket
		secret    *corev1.Secret
		configMap *corev1.ConfigMap
		err       error
	)

	obc, err = claimForKey(key, r.internalClient)
	if err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("OBC was lost before we could provision: %v", err)
		}
		return err
	}

	// Following getting the claim, if any provisioning task fails, clean up provisioned artifacts.
	// It is assumed that if the get claim fails, no resources were generated to begin with.
	defer func() {
		if err != nil {
			log.Error(err, "cleaning up reconcile artifacts")
			if !pErr.IsBucketExists(err) && ob != nil && isDynamicProvisioning {
				log.Info("deleting bucket", "name", ob.Spec.Endpoint.BucketName)
				if err := r.provisioner.Delete(ob); err != nil {
					log.Error(err, "error deleting bucket")
				}
			}
			r.deleteResources(ob, configMap, secret)
		}
	}()

	bucketName := class.Parameters[v1alpha1.StorageClassBucket]
	if isDynamicProvisioning {
		bucketName, err = composeBucketName(obc)
		if err != nil {
			return fmt.Errorf("error composing bucket name: %v", err)
		}
	}
	if len(bucketName) == 0 {
		return fmt.Errorf("bucket name missing")
	}

	if !shouldProvision(obc) {
		return nil
	}

	options := &api.BucketOptions{
		ReclaimPolicy:     class.ReclaimPolicy,
		BucketName:        bucketName,
		ObjectBucketClaim: obc.DeepCopy(),
		Parameters:        class.Parameters,
	}

	verb := "provisioning"
	if !isDynamicProvisioning {
		verb = "granting access to"
	}
	logD.Info(verb, "bucket", options.BucketName)

	if isDynamicProvisioning {
		ob, err = r.provisioner.Provision(options)
	} else {
		ob, err = r.provisioner.Grant(options)
	}
	if err != nil {
		return fmt.Errorf("error %s bucket: %v", verb, err)
	} else if ob == (&v1alpha1.ObjectBucket{}) {
		return fmt.Errorf("provisioner returned nil/empty object bucket")
	}

	setObjectBucketName(ob, key)
	ob.Spec.StorageClassName = obc.Spec.StorageClassName
	ob.Spec.ClaimRef, err = claimRefForKey(key, r.internalClient)
	ob.SetFinalizers([]string{finalizer})

	if ob, err = createObjectBucket(ob, r.internalClient, r.retryInterval, r.retryTimeout); err != nil {
		return err
	}

	if secret, err = createSecret(obc, ob.Spec.Authentication, r.Client, r.retryInterval, r.retryTimeout); err != nil {
		return err
	}

	if configMap, err = createConfigMap(obc, ob.Spec.Endpoint, r.Client, r.retryInterval, r.retryTimeout); err != nil {
		return err
	}

	obc.Spec.ObjectBucketName = ob.Name
	obc.Spec.BucketName = bucketName
	if err = updateClaim(obc, r.internalClient); err != nil {
		return err
	}
	log.Info("provisioning succeeded")
	return nil
}

func (r *ObjectBucketClaimReconciler) handleDeleteClaim(key client.ObjectKey) error {

	// TODO each delete should retry a few times to mitigate intermittent errors

	cm, err := configMapForClaimKey(key, r.internalClient)
	if err == nil {
		err = deleteConfigMap(cm, r.internalClient)
		if err != nil {
			return err
		}
	} else {
		log.Error(err, "could not get configMap")
	}

	secret, err := secretForClaimKey(key, r.internalClient)
	if err == nil {
		err = deleteSecret(secret, r.internalClient)
		if err != nil {
			return err
		}
	} else {
		log.Error(err, "could not get secret")
	}

	ob, err := r.objectBucketForClaimKey(key)
	if err != nil {
		if errors.IsNotFound(err) {
			log.Error(err, "objectBucket not found, assuming it was already deleted")
			return nil
		}
		return fmt.Errorf("error getting objectBucket for key: %v", err)
	} else if ob == nil {
		log.Error(nil, "got nil objectBucket, assuming deletion complete")
		return nil
	}

	class, err := storageClassForOB(ob, r.internalClient)
	if err != nil || class == nil {
		return fmt.Errorf("error getting storageclass from OB %q", ob.Name)
	}
	newBkt := scForNewBkt(class)

	// decide whether Delete or Revoke is called
	if newBkt {
		if err = r.provisioner.Delete(ob); err != nil {
			// Do not proceed to deleting the ObjectBucket if the deprovisioning fails for bookkeeping purposes
			return fmt.Errorf("provisioner error deleting bucket %v", err)
		}
	} else {
		if err = r.provisioner.Revoke(ob); err != nil {
			return fmt.Errorf("provisioner error revoking access to bucket %v", err)
		}
	}

	if err = deleteObjectBucket(ob, r.internalClient); err != nil {
		if errors.IsNotFound(err) {
			log.Error(err, "ObjectBucket vanished during deprovisioning, assuming deletion complete")
		} else {
			return fmt.Errorf("error deleting objectBucket %v", ob.Name)
		}
	}
	return nil
}

func (r *ObjectBucketClaimReconciler) supportedProvisioner(provisioner string) bool {
	return provisioner == r.provisionerName
}

func (r *ObjectBucketClaimReconciler) objectBucketForClaimKey(key client.ObjectKey) (*v1alpha1.ObjectBucket, error) {
	logD.Info("getting objectBucket for key", "key", key)
	ob := &v1alpha1.ObjectBucket{}
	obKey := client.ObjectKey{
		Name: fmt.Sprintf(objectBucketNameFormat, key.Namespace, key.Name),
	}
	err := r.Client.Get(r.ctx, obKey, ob)
	if err != nil {
		return nil, fmt.Errorf("error listing object buckets: %v", err)
	}
	return ob, nil
}

func (r *ObjectBucketClaimReconciler) updateObjectBucketClaimPhase(obc *v1alpha1.ObjectBucketClaim, phase v1alpha1.ObjectBucketClaimStatusPhase) (*v1alpha1.ObjectBucketClaim, error) {
	obc.Status.Phase = phase
	err := r.Client.Update(r.ctx, obc)
	if err != nil {
		return nil, fmt.Errorf("error updating phase: %v", err)
	}
	return obc, nil
}

func (r *ObjectBucketClaimReconciler) deleteResources(ob *v1alpha1.ObjectBucket, cm *corev1.ConfigMap, s *corev1.Secret) {
	if err := deleteObjectBucket(ob, r.internalClient); err != nil {
		log.Error(err, "error deleting objectBucket")
	}
	if err := deleteSecret(s, r.internalClient); err != nil {
		log.Error(err, "error deleting secret")
	}
	if err := deleteConfigMap(cm, r.internalClient); err != nil {
		log.Error(err, "error deleting configMap")
	}
}
