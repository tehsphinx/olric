// Copyright 2018-2019 Burak Sezer
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

package olric

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/buraksezer/olric/config"

	"github.com/buraksezer/olric/internal/discovery"

	"github.com/buraksezer/olric/internal/protocol"
	"github.com/buraksezer/olric/internal/storage"
	"github.com/vmihailenco/msgpack"
)

var (
	routingMtx   sync.RWMutex
	rebalanceMtx sync.Mutex
)

type dmapbox struct {
	PartID    uint64
	Backup    bool
	Name      string
	Payload   []byte
	AccessLog map[uint64]int64
}

func (db *Olric) moveDMap(part *partition, name string, dm *dmap, owner discovery.Member) error {
	dm.Lock()
	defer dm.Unlock()

	payload, err := dm.storage.Export()
	if err != nil {
		return err
	}
	data := &dmapbox{
		PartID:  part.id,
		Backup:  part.backup,
		Name:    name,
		Payload: payload,
	}
	// cache structure will be regenerated by mergeDMap. Just pack the accessLog.
	if dm.cache != nil && dm.cache.accessLog != nil {
		data.AccessLog = dm.cache.accessLog
	}
	value, err := msgpack.Marshal(data)
	if err != nil {
		return err
	}

	req := &protocol.Message{
		Value: value,
	}
	_, err = db.requestTo(owner.String(), protocol.OpMoveDMap, req)
	if err != nil {
		return err
	}

	// Delete moved dmap instance. the gc will free the allocated memory.
	part.m.Delete(name)
	return nil
}

func (db *Olric) selectVersionForMerge(dm *dmap, hkey uint64, vdata *storage.VData) (*storage.VData, error) {
	current, err := dm.storage.Get(hkey)
	if err == storage.ErrKeyNotFound {
		return vdata, nil
	}
	if err != nil {
		return nil, err
	}
	versions := []*version{{Data: current}, {Data: vdata}}
	versions = db.sortVersions(versions)
	return versions[0].Data, nil
}

func (db *Olric) mergeDMaps(part *partition, data *dmapbox) error {
	str, err := storage.Import(data.Payload)
	if err != nil {
		return err
	}

	tmp, exist := part.m.Load(data.Name)
	if !exist {
		// create a new DMap if it doesn't exist.
		tmp, err = db.createDMap(part, data.Name, str)
		if err != nil {
			return err
		}
	}

	// Acquire DMap's lock. No one should work on it.
	dm := tmp.(*dmap)
	dm.Lock()
	defer dm.Unlock()

	// Merge accessLog.
	if dm.cache != nil && dm.cache.accessLog != nil {
		dm.cache.Lock()
		for hkey, t := range data.AccessLog {
			if _, ok := dm.cache.accessLog[hkey]; !ok {
				dm.cache.accessLog[hkey] = t
			}
		}
		dm.cache.Unlock()
	}

	// We do not need the following loop if the DMap is created here.
	if !exist {
		return nil
	}

	var mergeErr error
	str.Range(func(hkey uint64, vdata *storage.VData) bool {
		winner, err := db.selectVersionForMerge(dm, hkey, vdata)
		if err != nil {
			mergeErr = err
			return false
		}
		mergeErr = dm.storage.Put(hkey, winner)
		if mergeErr == storage.ErrFragmented {
			db.wg.Add(1)
			go db.compactTables(dm)
			mergeErr = nil
		}
		if mergeErr != nil {
			return false
		}
		return true
	})
	return mergeErr
}

func (db *Olric) rebalancePrimaryPartitions() {
	rsign := atomic.LoadUint64(&routingSignature)
	for partID := uint64(0); partID < db.config.PartitionCount; partID++ {
		if !db.isAlive() {
			// The server is gone.
			break
		}

		if rsign != atomic.LoadUint64(&routingSignature) {
			// Routing table is updated. Just quit. Another rebalancer goroutine will work on the
			// new table immediately.
			break
		}

		part := db.partitions[partID]
		if part.length() == 0 {
			// Empty partition. Skip it.
			continue
		}

		owner := part.owner()
		if hostCmp(owner, db.this) {
			// Already belongs to me.
			continue
		}
		// This is a previous owner. Move the keys.
		part.m.Range(func(name, dm interface{}) bool {
			db.log.V(2).Printf("[INFO] Moving DMap: %s (backup: %v) on PartID: %d to %s",
				name, part.backup, partID, owner)
			err := db.moveDMap(part, name.(string), dm.(*dmap), owner)
			if err != nil {
				db.log.V(3).Printf("[ERROR] Failed to move DMap: %s on PartID: %d to %s: %v",
					name, partID, owner, err)
			}
			// if this returns true, the iteration continues
			return rsign == atomic.LoadUint64(&routingSignature)
		})
	}
}

func (db *Olric) rebalanceBackupPartitions() {
	rsign := atomic.LoadUint64(&routingSignature)
	for partID := uint64(0); partID < db.config.PartitionCount; partID++ {
		if !db.isAlive() {
			// The server is gone.
			break
		}

		part := db.backups[partID]
		if part.length() == 0 {
			// Empty partition. Skip it.
			continue
		}
		owners := part.loadOwners()
		if len(owners) == db.config.ReplicaCount-1 {
			// everything is ok
			continue
		}

		var ids []uint64
		offset := len(owners) - 1 - (db.config.ReplicaCount - 1)
		for i := len(owners) - 1; i > offset; i-- {
			owner := owners[i]
			if hostCmp(db.this, owner) {
				continue
			}
			ids = append(ids, owner.ID)
		}

		for _, id := range ids {
			if !db.isAlive() {
				// The server is gone.
				break
			}

			if rsign != atomic.LoadUint64(&routingSignature) {
				// Routing table is updated. Just quit. Another rebalancer goroutine will work on the
				// new table immediately.
				break
			}

			owner, err := db.discovery.FindMemberByID(id)
			if err != nil {
				db.log.V(2).Printf("[ERROR] Failed to get host by id: %d: %v", id, err)
				continue
			}

			part.m.Range(func(name, dm interface{}) bool {
				db.log.V(2).Printf("[INFO] Moving DMap: %s (backup: %v) on PartID: %d to %s",
					name, part.backup, partID, owner)
				err := db.moveDMap(part, name.(string), dm.(*dmap), owner)
				if err != nil {
					db.log.V(3).Printf("[ERROR] Failed to move backup DMap: %s on PartID: %d to %s: %v",
						name, partID, owner, err)
				}
				// if this returns true, the iteration continues
				return rsign == atomic.LoadUint64(&routingSignature)
			})
		}
	}
}

func (db *Olric) rebalancer() {
	rebalanceMtx.Lock()
	defer rebalanceMtx.Unlock()

	if err := db.checkOperationStatus(); err != nil {
		db.log.V(1).Printf("[WARN] Rebalancer awaits for bootstrapping")
		return
	}
	db.rebalancePrimaryPartitions()
	if db.config.ReplicaCount > config.MinimumReplicaCount {
		db.rebalanceBackupPartitions()
	}
}

func (db *Olric) checkOwnership(part *partition) bool {
	owners := part.loadOwners()
	for _, owner := range owners {
		if hostCmp(owner, db.this) {
			return true
		}
	}
	return false
}

func (db *Olric) moveDMapOperation(req *protocol.Message) *protocol.Message {
	err := db.checkOperationStatus()
	if err != nil {
		return db.prepareResponse(req, err)
	}

	box := &dmapbox{}
	err = msgpack.Unmarshal(req.Value, box)
	if err != nil {
		db.log.V(2).Printf("[ERROR] Failed to unmarshal dmap: %v", err)
		return req.Error(protocol.StatusInternalServerError, err)
	}

	var part *partition
	if box.Backup {
		part = db.backups[box.PartID]
	} else {
		part = db.partitions[box.PartID]
	}
	// Check ownership before merging. This is useful to prevent data corruption in network partitioning case.
	if !db.checkOwnership(part) {
		db.log.V(2).Printf("[ERROR] Received DMap: %s on PartID: %d (backup: %v) doesn't belong to me",
			box.Name, box.PartID, box.Backup)

		return req.Error(protocol.StatusBadRequest,
			fmt.Sprintf("partID: %d (backup: %v) doesn't belong to %s", box.PartID, box.Backup, db.this))
	}

	db.log.V(2).Printf("[INFO] Received DMap (backup:%v): %s on PartID: %d",
		box.Backup, box.Name, box.PartID)

	err = db.mergeDMaps(part, box)
	if err != nil {
		db.log.V(2).Printf("[ERROR] Failed to merge dmap: %v", err)
		return req.Error(protocol.StatusInternalServerError, err)
	}
	return req.Success()
}
