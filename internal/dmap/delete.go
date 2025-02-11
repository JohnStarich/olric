// Copyright 2018-2021 Burak Sezer
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

package dmap

import (
	"github.com/buraksezer/olric/internal/cluster/partitions"
	"github.com/buraksezer/olric/internal/discovery"
	"github.com/buraksezer/olric/internal/protocol"
	"github.com/buraksezer/olric/pkg/storage"
	"golang.org/x/sync/errgroup"
)

func (dm *DMap) deleteBackupFromFragment(key string, kind partitions.Kind) error {
	hkey := partitions.HKey(dm.name, key)
	f, err := dm.getFragment(hkey, kind)
	if err == errFragmentNotFound {
		// key doesn't exist
		return nil
	}
	if err != nil {
		return err
	}
	f.Lock()
	defer f.Unlock()

	err = f.storage.Delete(hkey)
	if err == storage.ErrFragmented {
		dm.s.wg.Add(1)
		go dm.s.callCompactionOnStorage(f)
		err = nil
	}
	return err
}

func (dm *DMap) deleteFromPreviousOwners(key string, owners []discovery.Member) error {
	// Traverse in reverse order. Except from the latest host, this one.
	for i := len(owners) - 2; i >= 0; i-- {
		owner := owners[i]
		req := protocol.NewDMapMessage(protocol.OpDeletePrev)
		req.SetDMap(dm.name)
		req.SetKey(key)
		_, err := dm.s.requestTo(owner.String(), req)
		if err != nil {
			return err
		}
	}
	return nil
}

func (dm *DMap) deleteBackupOnCluster(hkey uint64, key string) error {
	owners := dm.s.backup.PartitionOwnersByHKey(hkey)
	var g errgroup.Group
	for _, owner := range owners {
		mem := owner
		g.Go(func() error {
			// TODO: Add retry with backoff
			req := protocol.NewDMapMessage(protocol.OpDeleteBackup)
			req.SetDMap(dm.name)
			req.SetKey(key)
			_, err := dm.s.requestTo(mem.String(), req)
			if err != nil {
				dm.s.log.V(3).Printf("[ERROR] Failed to delete backup key/value on %s: %s", dm.name, err)
			}
			return err
		})
	}
	return g.Wait()
}

// deleteOnCluster is not a thread-safe function
func (dm *DMap) deleteOnCluster(hkey uint64, key string, f *fragment) error {
	owners := dm.s.primary.PartitionOwnersByHKey(hkey)
	if len(owners) == 0 {
		panic("partition owners list cannot be empty")
	}

	err := dm.deleteFromPreviousOwners(key, owners)
	if err != nil {
		return err
	}

	if dm.s.config.ReplicaCount != 0 {
		err := dm.deleteBackupOnCluster(hkey, key)
		if err != nil {
			return err
		}
	}

	err = f.storage.Delete(hkey)
	if err == storage.ErrFragmented {
		dm.s.wg.Add(1)
		go dm.s.callCompactionOnStorage(f)
		err = nil
	}

	// Delete it from access log if everything is ok.
	// If we delete the hkey when err is not nil, LRU/MaxIdleDuration may not work properly.
	if err == nil {
		dm.deleteAccessLog(hkey, f)
	}
	return err
}

func (dm *DMap) deleteKey(key string) error {
	hkey := partitions.HKey(dm.name, key)
	member := dm.s.primary.PartitionByHKey(hkey).Owner()
	if !member.CompareByName(dm.s.rt.This()) {
		req := protocol.NewDMapMessage(protocol.OpDelete)
		req.SetDMap(dm.name)
		req.SetKey(key)
		_, err := dm.s.requestTo(member.String(), req)
		return err
	}

	// notice that "delete" operation is run on the cluster.
	f, err := dm.getOrCreateFragment(hkey, partitions.PRIMARY)
	if err != nil {
		return err
	}
	f.Lock()
	defer f.Unlock()
	return dm.deleteOnCluster(hkey, key, f)
}

// Delete deletes the value for the given key. Delete will not return error if key doesn't exist. It's thread-safe.
// It is safe to modify the contents of the argument after Delete returns.
func (dm *DMap) Delete(key string) error {
	return dm.deleteKey(key)
}
