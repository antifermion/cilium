// Copyright 2018 Authors of Cilium
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cache

import (
	"fmt"
	"path"
	"sync"

	"github.com/cilium/cilium/pkg/identity"
	"github.com/cilium/cilium/pkg/idpool"
	"github.com/cilium/cilium/pkg/kvstore"
	"github.com/cilium/cilium/pkg/kvstore/allocator"
	"github.com/cilium/cilium/pkg/labels"
	"github.com/cilium/cilium/pkg/lock"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/option"

	"github.com/sirupsen/logrus"
)

// globalIdentity is the structure used to store an identity in the kvstore
type globalIdentity struct {
	labels.Labels
}

// GetKey() encodes a globalIdentity as string
func (gi globalIdentity) GetKey() string {
	return kvstore.Encode(gi.SortedList())
}

// PutKey() decides a globalIdentity from its string representation
func (gi globalIdentity) PutKey(v string) (allocator.AllocatorKey, error) {
	b, err := kvstore.Decode(v)
	if err != nil {
		return nil, err
	}

	return globalIdentity{labels.NewLabelsFromSortedList(string(b))}, nil
}

var (
	// IdentityAllocator is an allocator for security identities from the
	// kvstore.
	IdentityAllocator *allocator.Allocator

	// IdentitiesPath is the path to where identities are stored in the key-value
	// store.
	IdentitiesPath = path.Join(kvstore.BaseKeyPrefix, "state", "identities", "v1")

	// setupMutex synchronizes InitIdentityAllocator() and Close()
	setupMutex lock.Mutex

	watcher identityWatcher
)

// IdentityAllocatorOwner is the interface the owner of an identity allocator
// must implement
type IdentityAllocatorOwner interface {
	// TriggerPolicyUpdates will be called whenever a policy recalculation
	// must be triggered
	TriggerPolicyUpdates(force bool, reason string) *sync.WaitGroup

	// GetSuffix must return the node specific suffix to use
	GetNodeSuffix() string
}

// InitIdentityAllocator creates the the identity allocator. Only the first
// invocation of this function will have an effect.
func InitIdentityAllocator(owner IdentityAllocatorOwner) {
	setupMutex.Lock()
	defer setupMutex.Unlock()

	if IdentityAllocator != nil {
		log.Panic("InitIdentityAllocator() in succession without calling Close()")
	}

	identity.InitWellKnownIdentities()

	log.Info("Initializing identity allocator")

	minID := idpool.ID(identity.MinimalNumericIdentity)
	maxID := idpool.ID(^uint16(0))
	events := make(allocator.AllocatorEventChan, 1024)

	// It is important to start listening for events before calling
	// NewAllocator() as it will emit events while filling the
	// initial cache
	watcher.watch(owner, events)

	a, err := allocator.NewAllocator(IdentitiesPath, globalIdentity{},
		allocator.WithMax(maxID), allocator.WithMin(minID),
		allocator.WithSuffix(owner.GetNodeSuffix()),
		allocator.WithEvents(events),
		allocator.WithMasterKeyProtection(),
		allocator.WithPrefixMask(idpool.ID(option.Config.ClusterID<<option.ClusterIDShift)))
	if err != nil {
		log.WithError(err).Fatal("Unable to initialize identity allocator")
	}

	IdentityAllocator = a
}

// Close closes the identity allocator and allows to call
// InitIdentityAllocator() again
func Close() {
	setupMutex.Lock()
	defer setupMutex.Unlock()

	if IdentityAllocator == nil {
		log.Panic("Close() called without calling InitIdentityAllocator() first")
	}

	IdentityAllocator.Delete()
	watcher.stop()
	IdentityAllocator = nil
}

// WaitForInitialIdentities waits for the initial set of security identities to
// have been received and populated into the allocator cache
func WaitForInitialIdentities() {
	IdentityAllocator.WaitForInitialSync()
}

// IdentityAllocationIsLocal returns true if a call to AllocateIdentity with
// the given labels would not require accessing the KV store to allocate the
// identity.
// Currently, this function returns true only if the labels are those of a
// reserved identity, i.e. if the slice contains a single reserved
// "reserved:*" label.
func IdentityAllocationIsLocal(lbls labels.Labels) bool {
	// If there is only one label with the "reserved" source and a well-known
	// key, the well-known identity for it can be allocated locally.
	return LookupReservedIdentityByLabels(lbls) != nil
}

// AllocateIdentity allocates an identity described by the specified labels. If
// an identity for the specified set of labels already exist, the identity is
// re-used and reference counting is performed, otherwise a new identity is
// allocated via the kvstore.
func AllocateIdentity(lbls labels.Labels) (*identity.Identity, bool, error) {
	log.WithFields(logrus.Fields{
		logfields.IdentityLabels: lbls.String(),
	}).Debug("Resolving identity")

	// If there is only one label with the "reserved" source and a well-known
	// key, use the well-known identity for that key.
	if reservedIdentity := LookupReservedIdentityByLabels(lbls); reservedIdentity != nil {
		log.WithFields(logrus.Fields{
			logfields.Identity:       reservedIdentity.ID,
			logfields.IdentityLabels: lbls.String(),
			"isNew":                  false,
		}).Debug("Resolved reserved identity")
		return reservedIdentity, false, nil
	}

	if IdentityAllocator == nil {
		return nil, false, fmt.Errorf("allocator not initialized")
	}

	id, isNew, err := IdentityAllocator.Allocate(globalIdentity{lbls})
	if err != nil {
		return nil, false, err
	}

	log.WithFields(logrus.Fields{
		logfields.Identity:       id,
		logfields.IdentityLabels: lbls.String(),
		"isNew":                  isNew,
	}).Debug("Resolved identity")

	return identity.NewIdentity(identity.NumericIdentity(id), lbls), isNew, nil
}

// Release is the reverse operation of AllocateIdentity() and releases the
// identity again. This function may result in kvstore operations.
// After the last user has released the ID, the returned lastUse value is true.
func Release(id *identity.Identity) error {
	// Ignore reserved identities.
	if identity.LookupReservedIdentity(id.ID) != nil {
		return nil
	}

	if IdentityAllocator == nil {
		return fmt.Errorf("allocator not initialized")
	}

	return IdentityAllocator.Release(globalIdentity{id.Labels})
}

// ReleaseSlice attempts to release a set of identities. It is a helper
// function that may be useful for cleaning up multiple identities in paths
// where several identities may be allocated and another error means that they
// should all be released.
func ReleaseSlice(identities []*identity.Identity) error {
	var err error
	for _, id := range identities {
		if id == nil {
			continue
		}
		if err2 := Release(id); err2 != nil {
			log.WithError(err2).WithFields(logrus.Fields{
				logfields.Identity: id,
			}).Error("Failed to release identity")
			err = err2
		}
	}
	return err
}

// WatchRemoteIdentities starts watching for identities in another kvstore and
// syncs all identities to the local identity cache.
func WatchRemoteIdentities(backend kvstore.BackendOperations) *allocator.RemoteCache {
	return IdentityAllocator.WatchRemoteKVStore(backend, IdentitiesPath)
}
