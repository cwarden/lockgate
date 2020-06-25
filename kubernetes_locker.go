package lockgate

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/werf/lockgate/pkg/util"

	default_errors "errors"

	"k8s.io/apimachinery/pkg/api/errors"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/google/uuid"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

const (
	lockLeaseTTLSeconds                 = 10
	lockPollRetryPeriodSeconds          = 1
	optimisticLockingRetryPeriodSeconds = 1
	lockLeaseRenewPeriodSeconds         = 3
)

var (
	ErrLockAlreadyLeased        = default_errors.New("lock already leased")
	ErrNoExistingLockLeaseFound = default_errors.New("no existing lock lease found")
)

type KubernetesLocker struct {
	KubernetesInterface dynamic.Interface
	GVR                 schema.GroupVersionResource
	ResourceName        string
	Namespace           string

	mux sync.Mutex

	leaseRenewWorkers map[string]*LeaseRenewWorkerDescriptor
}

type LockLeaseRecord struct {
	LockHandle
	ExpireAtTimestamp  int64
	SharedHoldersCount int64
	IsShared           bool
}

type LeaseRenewWorkerDescriptor struct {
	DoneChan           chan struct{}
	SharedLeaseCounter int64
}

func NewKubernetesLocker(kubernetesInterface dynamic.Interface, gvr schema.GroupVersionResource, resourceName string, namespace string) *KubernetesLocker {
	return &KubernetesLocker{
		KubernetesInterface: kubernetesInterface,
		GVR:                 gvr,
		ResourceName:        resourceName,
		Namespace:           namespace,
		leaseRenewWorkers:   make(map[string]*LeaseRenewWorkerDescriptor),
	}
}

func (locker *KubernetesLocker) Acquire(lockName string, opts AcquireOptions) (bool, LockHandle, error) {
	debug("(acquire lock %q) opts=%#v", lockName, opts)
	return locker.acquire(lockName, opts, true)
}

func (locker *KubernetesLocker) Release(lockHandle LockHandle) error {
	debug("(release lock %q) %#v", lockHandle.LockName, lockHandle)
	return locker.release(lockHandle)
}

func (locker *KubernetesLocker) getResource() (*unstructured.Unstructured, error) {
	var err error
	var obj *unstructured.Unstructured

	if locker.Namespace == "" {
		obj, err = locker.KubernetesInterface.Resource(locker.GVR).Get(locker.ResourceName, metav1.GetOptions{})
	} else {
		obj, err = locker.KubernetesInterface.Resource(locker.GVR).Namespace(locker.Namespace).Get(locker.ResourceName, metav1.GetOptions{})
	}

	if err != nil {
		return obj, fmt.Errorf("cannot get %s by name %q: %s", locker.GVR.String(), locker.ResourceName, err)
	}
	return obj, err
}

func (locker *KubernetesLocker) updateResource(obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	var err error
	var newObj *unstructured.Unstructured

	if locker.Namespace == "" {
		newObj, err = locker.KubernetesInterface.Resource(locker.GVR).Update(obj, metav1.UpdateOptions{})
	} else {
		newObj, err = locker.KubernetesInterface.Resource(locker.GVR).Namespace(locker.Namespace).Update(obj, metav1.UpdateOptions{})
	}

	if errors.IsAlreadyExists(err) || err == nil {
		return newObj, err
	} else {
		return newObj, fmt.Errorf("cannot update %s by name %s: %s", locker.GVR.String(), locker.ResourceName, err)
	}
}

func (locker *KubernetesLocker) newLockLeaseRecord(lockName string, isShared bool) *LockLeaseRecord {
	return &LockLeaseRecord{
		LockHandle:         LockHandle{UUID: uuid.New().String(), LockName: lockName},
		ExpireAtTimestamp:  time.Now().Unix() + lockLeaseTTLSeconds,
		SharedHoldersCount: 1,
		IsShared:           isShared,
	}
}

func (locker *KubernetesLocker) objectLockLeaseAnnotationName(lockName string) string {
	return fmt.Sprintf("lockgate.io/%s", util.Sha3_224Hash(lockName))
}

func (locker *KubernetesLocker) extractObjectLockLease(obj *unstructured.Unstructured, lockName string) (*LockLeaseRecord, error) {
	annots := obj.GetAnnotations()
	if data, hasKey := annots[locker.objectLockLeaseAnnotationName(lockName)]; hasKey {
		var lease *LockLeaseRecord
		if err := json.Unmarshal([]byte(data), &lease); err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: invalid %s annotation value in %s/%s: %s", locker.objectLockLeaseAnnotationName(lockName), obj.GetKind(), locker.ResourceName, err)
			return nil, nil
		}
		return lease, nil
	}
	return nil, nil
}

func (locker *KubernetesLocker) setObjectLockLease(obj *unstructured.Unstructured, lease *LockLeaseRecord) {
	if data, err := json.Marshal(lease); err != nil {
		panic(fmt.Sprintf("json marshal %#v failed: %s", lease, err))
	} else {
		annots := obj.GetAnnotations()
		if annots == nil {
			annots = make(map[string]string)
		}
		annots[locker.objectLockLeaseAnnotationName(lease.LockName)] = string(data)
		obj.SetAnnotations(annots)
	}
}

func (locker *KubernetesLocker) unsetObjectLockLease(obj *unstructured.Unstructured, lockName string) {
	annots := obj.GetAnnotations()
	if annots != nil {
		delete(annots, locker.objectLockLeaseAnnotationName(lockName))
		if len(annots) == 0 {
			obj.SetAnnotations(nil)
		} else {
			obj.SetAnnotations(annots)
		}
	}
}

func (locker *KubernetesLocker) acquire(lockName string, opts AcquireOptions, shouldCallOnWait bool) (bool, LockHandle, error) {
RETRY_ACQUIRE:

	if obj, err := locker.getResource(); err != nil {
		return false, LockHandle{}, err
	} else {
		debug("(acquire lock %q) getResource -> %s/%s %#v", lockName, obj.GetKind(), obj.GetName(), obj.GetAnnotations())

		if oldLease, err := locker.extractObjectLockLease(obj, lockName); err != nil {
			return false, LockHandle{}, err
		} else if oldLease != nil {
			debug("(acquire lock %q) oldLease -> %#v", lockName, oldLease)

			if time.Now().After(time.Unix(oldLease.ExpireAtTimestamp, 0)) {
				debug("(acquire lock %q) old lease expired, take over with the new lease!", lockName)

				newLease := locker.newLockLeaseRecord(lockName, opts.Shared)
				debug("(acquire lock %q) new lease: %#v", lockName, newLease)

				locker.setObjectLockLease(obj, newLease)
				if _, err := locker.updateResource(obj); isOptimisticLockingError(err) {
					debug("(acquire lock %q) update %s/%s optimistic locking error! Will retry acquire ...", lockName, obj.GetKind(), obj.GetName())
					time.Sleep(optimisticLockingRetryPeriodSeconds * time.Second)
					goto RETRY_ACQUIRE
				} else if err != nil {
					return false, LockHandle{}, err
				}
				locker.runLeaseRenewWorker(newLease.LockHandle, opts)
				return true, newLease.LockHandle, nil
			}

			if opts.Shared && oldLease.IsShared {
				oldLease.SharedHoldersCount++
				oldLease.ExpireAtTimestamp = time.Now().Unix() + lockLeaseTTLSeconds
				debug("(acquire lock %q) incremented shared holders counter for existing lease: %#v", lockName, oldLease)
				locker.setObjectLockLease(obj, oldLease)
				if _, err := locker.updateResource(obj); isOptimisticLockingError(err) {
					debug("(acquire lock %q) update %s/%s optimistic locking error! Will retry acquire ...", lockName, obj.GetKind(), obj.GetName())
					time.Sleep(optimisticLockingRetryPeriodSeconds * time.Second)
					goto RETRY_ACQUIRE
				} else if err != nil {
					return false, LockHandle{}, err
				}
				// always run lease worker even if our process already holds this lock
				locker.runLeaseRenewWorker(oldLease.LockHandle, opts)
				return true, oldLease.LockHandle, nil
			}

			if opts.NonBlocking {
				debug("(acquire lock %q) non blocking acquire done: lock not taken!", lockName)
				return false, LockHandle{}, nil
			}

			debug("(acquire lock %q) poll lock: will retry in %d seconds", lockName, lockPollRetryPeriodSeconds)
			debug("(acquire lock %q) ---", lockName)

			if opts.OnWaitFunc != nil && shouldCallOnWait {
				var acquireLocked bool
				var acquireHandle LockHandle
				var acquireErr error

				if err := opts.OnWaitFunc(lockName, func() error {
					time.Sleep(lockPollRetryPeriodSeconds * time.Second)
					acquireLocked, acquireHandle, acquireErr = locker.acquire(lockName, opts, false)
					return acquireErr
				}); err != nil {
					return acquireLocked, acquireHandle, err
				}

				return acquireLocked, acquireHandle, acquireErr
			} else {
				time.Sleep(lockPollRetryPeriodSeconds * time.Second)
				goto RETRY_ACQUIRE
			}
		}

		newLease := locker.newLockLeaseRecord(lockName, opts.Shared)
		debug("(acquire lock %q) new lease: %#v", lockName, newLease)

		locker.setObjectLockLease(obj, newLease)
		if _, err := locker.updateResource(obj); isOptimisticLockingError(err) {
			debug("(acquire lock %q) update %s/%s optimistic locking error! Will retry acquire...", lockName, obj.GetKind(), obj.GetName())
			time.Sleep(optimisticLockingRetryPeriodSeconds * time.Second)
			goto RETRY_ACQUIRE
		} else if err != nil {
			return false, LockHandle{}, err
		}
		locker.runLeaseRenewWorker(newLease.LockHandle, opts)
		return true, newLease.LockHandle, nil
	}
}

func (locker *KubernetesLocker) release(lockHandle LockHandle) error {
	if err := locker.stopLeaseRenewWorker(lockHandle); err != nil {
		return err
	}

	return locker.changeLease(lockHandle, func(obj *unstructured.Unstructured, currentLease *LockLeaseRecord) error {
		currentLease.SharedHoldersCount--

		if currentLease.SharedHoldersCount == 0 {
			locker.unsetObjectLockLease(obj, lockHandle.LockName)
		} else {
			locker.setObjectLockLease(obj, currentLease)
		}

		return nil
	})
}

func (locker *KubernetesLocker) stopLeaseRenewWorker(lockHandle LockHandle) error {
	debug("(stopLeaseRenewWorker %q %q) before lock", lockHandle.LockName, lockHandle.UUID)
	locker.mux.Lock()
	isLocked := true
	unlockFunc := func() {
		debug("(stopLeaseRenewWorker %q %q) unlock", lockHandle.LockName, lockHandle.UUID)
		if isLocked {
			locker.mux.Unlock()
			isLocked = false
		}
	}
	defer unlockFunc()
	debug("(stopLeaseRenewWorker %q %q) after lock", lockHandle.LockName, lockHandle.UUID)

	if desc, hasKey := locker.leaseRenewWorkers[lockHandle.UUID]; !hasKey {
		return fmt.Errorf("unknown id %q for lock %q", lockHandle.UUID, lockHandle.LockName)
	} else {
		desc.SharedLeaseCounter--
		if desc.SharedLeaseCounter == 0 {
			delete(locker.leaseRenewWorkers, lockHandle.UUID)
			unlockFunc()

			debug("(stopLeaseRenewWorker %q %q) before DoneChan signal", lockHandle.LockName, lockHandle.UUID)
			desc.DoneChan <- struct{}{}
			debug("(stopLeaseRenewWorker %q %q) after DoneChan signal, before DoneChan close", lockHandle.LockName, lockHandle.UUID)
			close(desc.DoneChan)
			debug("(stopLeaseRenewWorker %q %q) after DoneChan close", lockHandle.LockName, lockHandle.UUID)
		}
	}

	return nil
}

func (locker *KubernetesLocker) isLeaseRenewWorkerActive(lockHandle LockHandle) bool {
	debug("(isLeaseRenewWorkerActive %q %q) before lock", lockHandle.LockName, lockHandle.UUID)
	locker.mux.Lock()
	defer func() {
		debug("(isLeaseRenewWorkerActive%q %q) unlock", lockHandle.LockName, lockHandle.UUID)
		locker.mux.Unlock()
	}()
	debug("(isLeaseRenewWorkerActive %q %q) after lock", lockHandle.LockName, lockHandle.UUID)

	_, hasKey := locker.leaseRenewWorkers[lockHandle.UUID]
	return hasKey
}

func (locker *KubernetesLocker) runLeaseRenewWorker(lockHandle LockHandle, opts AcquireOptions) {
	debug("(runLeaseRenewWorker %q %q) before lock", lockHandle.LockName, lockHandle.UUID)
	locker.mux.Lock()
	defer func() {
		debug("(runLeaseRenewWorker %q %q) unlock", lockHandle.LockName, lockHandle.UUID)
		locker.mux.Unlock()
	}()
	debug("(runLeaseRenewWorker %q %q) before lock", lockHandle.LockName, lockHandle.UUID)

	if desc, hasKey := locker.leaseRenewWorkers[lockHandle.UUID]; !hasKey {
		desc := &LeaseRenewWorkerDescriptor{
			DoneChan:           make(chan struct{}, 0),
			SharedLeaseCounter: 1,
		}
		locker.leaseRenewWorkers[lockHandle.UUID] = desc
		go locker.leaseRenewWorker(lockHandle, opts, desc.DoneChan)
	} else {
		desc.SharedLeaseCounter++
	}
}

func (locker *KubernetesLocker) leaseRenewWorker(lockHandle LockHandle, opts AcquireOptions, doneChan chan struct{}) {
	ticker := time.NewTicker(lockLeaseRenewPeriodSeconds * time.Second)
	defer ticker.Stop()

	var lastRenewAt time.Time

	for {
		select {
		case <-ticker.C:
			debug("(leaseRenewWorker %q %q) tick!", lockHandle.LockName, lockHandle.UUID)

			// Throttle lease renew procedure, do not renew lease more than once in lockLeaseRenewPeriodSeconds seconds period
			if time.Now().Sub(lastRenewAt) < lockLeaseRenewPeriodSeconds*time.Second {
				debug("(leaseRenewWorker %q %q) skip, last renew was at %s", lockHandle.LockName, lockHandle.UUID, lastRenewAt.String())
				continue
			}

			if !locker.isLeaseRenewWorkerActive(lockHandle) {
				debug("(leaseRenewWorker %q %q) already stopped, ignore check")
				continue
			}

			debug("(leaseRenewWorker %q %q) do renew lease", lockHandle.LockName, lockHandle.UUID)
			if err := locker.renewLease(lockHandle); err == ErrLockAlreadyLeased || err == ErrNoExistingLockLeaseFound {
				fmt.Fprintf(os.Stderr, "ERROR: lost lease %s for lock %q\n", lockHandle.UUID, lockHandle.LockName)
				if err := opts.OnLostLeaseFunc(lockHandle); err != nil {
					fmt.Fprintf(os.Stderr, "ERROR: %s\n", err)
				}
				return
			} else if err != nil {
				fmt.Fprintf(os.Stderr, "ERROR: cannot extend lease %s for lock %q: %s\n", lockHandle.UUID, lockHandle.LockName, err)
			}

			lastRenewAt = time.Now()

		case <-doneChan:
			debug("(leaseRenewWorker %q %q) stopped!", lockHandle.LockName, lockHandle.UUID)
			return
		}
	}
}

func (locker *KubernetesLocker) renewLease(lockHandle LockHandle) error {
	return locker.changeLease(lockHandle, func(obj *unstructured.Unstructured, lease *LockLeaseRecord) error {
		lease.ExpireAtTimestamp = time.Now().Unix() + lockLeaseTTLSeconds
		locker.setObjectLockLease(obj, lease)
		return nil
	})
}

func (locker *KubernetesLocker) changeLease(lockHandle LockHandle, changeFunc func(obj *unstructured.Unstructured, currentLease *LockLeaseRecord) error) error {
RETRY_CHANGE:

	if obj, err := locker.getResource(); err != nil {
		return err
	} else {
		debug("(change lock %q lease) getResource -> %s/%s %#v", lockHandle.LockName, obj.GetKind(), obj.GetName(), obj.GetAnnotations())

		if currentLease, err := locker.extractObjectLockLease(obj, lockHandle.LockName); err != nil {
			return err
		} else if currentLease == nil {
			return ErrNoExistingLockLeaseFound
		} else if currentLease.UUID != lockHandle.UUID {
			return ErrLockAlreadyLeased
		} else {
			if err := changeFunc(obj, currentLease); err != nil {
				return err
			}
		}

		if _, err := locker.updateResource(obj); isOptimisticLockingError(err) {
			debug("(change lock %q lease) update %s/%s optimistic locking error! Will retry change...", lockHandle.LockName, obj.GetKind(), obj.GetName())
			time.Sleep(optimisticLockingRetryPeriodSeconds * time.Second)
			goto RETRY_CHANGE
		} else if err != nil {
			return err
		}

		return nil
	}
}

func debug(format string, args ...interface{}) {
	if os.Getenv("LOCKGATE_DEBUG") == "1" {
		fmt.Printf("LOCKGATE_DEBUG: %s\n", fmt.Sprintf(format, args...))
	}
}

func isOptimisticLockingError(err error) bool {
	if err != nil {
		return strings.HasSuffix(err.Error(), "the object has been modified; please apply your changes to the latest version and try again")
	}
	return false
}
