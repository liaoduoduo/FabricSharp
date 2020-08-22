/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package validation

import (
	"math"
	"strings"

	"github.com/hyperledger/fabric-protos-go/ledger/rwset/kvrwset"
	"github.com/hyperledger/fabric/core/ledger/internal/version"
	"github.com/hyperledger/fabric/core/ledger/kvledger/txmgmt/privacyenabledstate"
	"github.com/hyperledger/fabric/core/ledger/kvledger/txmgmt/rwsetutil"
	"github.com/hyperledger/fabric/core/ledger/kvledger/txmgmt/statedb"
	"github.com/hyperledger/fabric/core/ledger/kvledger/txmgmt/statemetadata"
	"github.com/hyperledger/fabric/orderer/common/localconfig"
)

func prepareTxOps(rwset *rwsetutil.TxRwSet, txht *version.Height,
	precedingUpdates *publicAndHashUpdates, db *privacyenabledstate.DB) (txOps, error) {
	txops := txOps{}
	txops.applyTxRwset(rwset)
	//logger.Debugf("prepareTxOps() txops after applying raw rwset=%#v", spew.Sdump(txops))
	for ck, keyop := range txops {
		// check if the final state of the key, value and metadata, is already present in the transaction, then skip
		// otherwise we need to retrieve latest state and merge in the current value or metadata update
		if keyop.isDelete() || keyop.isUpsertAndMetadataUpdate() {
			continue
		}

		// check if only value is updated in the current transaction then merge the metadata from last committed state
		if keyop.isOnlyUpsert() {
			latestMetadata, err := retrieveLatestMetadata(ck.ns, ck.coll, ck.key, precedingUpdates, db)
			if err != nil {
				return nil, err
			}
			keyop.metadata = latestMetadata
			continue
		}

		// only metadata is updated in the current transaction. Merge the value from the last committed state
		// If the key does not exist in the last state, make this key as noop in current transaction
		latestVal, err := retrieveLatestState(ck.ns, ck.coll, ck.key, precedingUpdates, db)
		if err != nil {
			return nil, err
		}
		if latestVal != nil {
			keyop.value = latestVal.Value
		} else {
			delete(txops, ck)
		}
	}
	//logger.Debugf("prepareTxOps() txops after final processing=%#v", spew.Sdump(txops))
	return txops, nil
}

// applyTxRwset records the upsertion/deletion of a kv and updatation/deletion
// of associated metadata present in a txrwset
func (txops txOps) applyTxRwset(rwset *rwsetutil.TxRwSet) error {
	for _, nsRWSet := range rwset.NsRwSets {
		ns := nsRWSet.NameSpace
		if localconfig.LineageSupported() {
			// By default, MaxUint64 implies no snapshot read is used
			var depSnapshot uint64 = math.MaxUint64
			for _, kvRead := range nsRWSet.KvRwSet.Reads {
				if kvRead.Version != nil {
					logger.Infof("Ns: %s, Read Key: %s, Read Version Blk Num: %d", ns, kvRead.GetKey(), kvRead.Version.BlockNum)
				} else {
					logger.Infof("Ns: %s, Read Key: %s, Read Version Blk Num: nil", ns, kvRead.GetKey())
				}
				if localconfig.IsOCC() {
					// Under OCC, the version of each read key is the snapshot
					if kvRead.Version == nil {
						depSnapshot = math.MaxUint64
					} else {
						depSnapshot = kvRead.Version.BlockNum
					}
				}
			}
			// }

			deps := map[string][]string{}
			for _, kvWrite := range nsRWSet.KvRwSet.Writes {
				if !strings.HasSuffix(kvWrite.Key, "_prov") {
					continue
				}
				// record with key XX_prov captures the dependency of XX, in the format of YY_ZZ_.
				// Need to ignore the whitespace after splitting with "_"
				key := strings.Split(kvWrite.Key, "_")[0]
				depKeys := []string{}
				for _, dk := range strings.Split(string(kvWrite.Value), "_") {
					if dk != "" {
						depKeys = append(depKeys, dk)
					}
				}
				deps[key] = depKeys
			}
			logger.Infof("Preprocess Txn Deps: [%v], length: %d", deps, len(deps))
			for _, kvWrite := range nsRWSet.KvRwSet.Writes {
				if strings.HasSuffix(kvWrite.Key, "_prov") {
					continue
				}
				keyDeps := []string{}
				if d, ok := deps[kvWrite.Key]; ok {
					keyDeps = d
				}
				txops.applyKVWriteWithDep(ns, "", kvWrite, keyDeps, depSnapshot)
			}

		} else {
			for _, kvWrite := range nsRWSet.KvRwSet.Writes {
				txops.applyKVWrite(ns, "", kvWrite)
			}

		}

		for _, kvMetadataWrite := range nsRWSet.KvRwSet.MetadataWrites {
			txops.applyMetadata(ns, "", kvMetadataWrite)
		}

		// apply collection level kvwrite and kvMetadataWrite
		for _, collHashRWset := range nsRWSet.CollHashedRwSets {
			coll := collHashRWset.CollectionName
			for _, hashedWrite := range collHashRWset.HashedRwSet.HashedWrites {
				txops.applyKVWrite(ns, coll,
					&kvrwset.KVWrite{
						Key:      string(hashedWrite.KeyHash),
						Value:    hashedWrite.ValueHash,
						IsDelete: hashedWrite.IsDelete,
					},
				)
			}

			for _, metadataWrite := range collHashRWset.HashedRwSet.MetadataWrites {
				txops.applyMetadata(ns, coll,
					&kvrwset.KVMetadataWrite{
						Key:     string(metadataWrite.KeyHash),
						Entries: metadataWrite.Entries,
					},
				)
			}
		}
	}
	return nil
}

// applyKVWrite records upsertion/deletion of a kvwrite
func (txops txOps) applyKVWrite(ns, coll string, kvWrite *kvrwset.KVWrite) {
	if kvWrite.IsDelete {
		txops.delete(compositeKey{ns, coll, kvWrite.Key})
	} else {
		txops.upsert(compositeKey{ns, coll, kvWrite.Key}, kvWrite.Value)
	}
}

func (txops txOps) applyKVWriteWithDep(ns, coll string, kvWrite *kvrwset.KVWrite, deps []string, depSnapshot uint64) {
	if kvWrite.IsDelete {
		txops.deleteWithDep(compositeKey{ns, coll, kvWrite.Key}, deps, depSnapshot)
	} else {
		txops.upsertWithDep(compositeKey{ns, coll, kvWrite.Key}, kvWrite.Value, deps, depSnapshot)
	}
}

// applyMetadata records updatation/deletion of a metadataWrite
func (txops txOps) applyMetadata(ns, coll string, metadataWrite *kvrwset.KVMetadataWrite) error {
	if metadataWrite.Entries == nil {
		txops.metadataDelete(compositeKey{ns, coll, metadataWrite.Key})
	} else {
		metadataBytes, err := statemetadata.Serialize(metadataWrite.Entries)
		if err != nil {
			return err
		}
		txops.metadataUpdate(compositeKey{ns, coll, metadataWrite.Key}, metadataBytes)
	}
	return nil
}

// retrieveLatestState returns the value of the key from the precedingUpdates (if the key was operated upon by a previous tran in the block).
// If the key not present in the precedingUpdates, then this function, pulls the latest value from statedb
// TODO FAB-11328, pulling from state for (especially for couchdb) will pay significant performance penalty so a bulkload would be helpful.
// Further, all the keys that gets written will be required to pull from statedb by vscc for endorsement policy check (in the case of key level
// endorsement) and hence, the bulkload should be combined
func retrieveLatestState(ns, coll, key string,
	precedingUpdates *publicAndHashUpdates, db *privacyenabledstate.DB) (*statedb.VersionedValue, error) {
	var vv *statedb.VersionedValue
	var err error
	if coll == "" {
		vv := precedingUpdates.publicUpdates.Get(ns, key)
		if vv == nil {
			vv, err = db.GetState(ns, key)
		}
		return vv, err
	}

	vv = precedingUpdates.hashUpdates.Get(ns, coll, key)
	if vv == nil {
		vv, err = db.GetValueHash(ns, coll, []byte(key))
	}
	return vv, err
}

func retrieveLatestMetadata(ns, coll, key string,
	precedingUpdates *publicAndHashUpdates, db *privacyenabledstate.DB) ([]byte, error) {
	if coll == "" {
		vv := precedingUpdates.publicUpdates.Get(ns, key)
		if vv != nil {
			return vv.Metadata, nil
		}
		return db.GetStateMetadata(ns, key)
	}
	vv := precedingUpdates.hashUpdates.Get(ns, coll, key)
	if vv != nil {
		return vv.Metadata, nil
	}
	return db.GetPrivateDataMetadataByHash(ns, coll, []byte(key))
}

type keyOpsFlag uint8

const (
	upsertVal keyOpsFlag = 1 << iota
	metadataUpdate
	metadataDelete
	keyDelete
)

type compositeKey struct {
	ns, coll, key string
}

type txOps map[compositeKey]*keyOps

type keyOps struct {
	flag        keyOpsFlag
	value       []byte
	metadata    []byte
	deps        []string
	depSnapshot uint64
}

////////////////// txOps functions

func (txops txOps) upsert(k compositeKey, val []byte) {
	keyops := txops.getOrCreateKeyEntry(k)
	keyops.flag += upsertVal
	keyops.value = val
}

func (txops txOps) delete(k compositeKey) {
	keyops := txops.getOrCreateKeyEntry(k)
	keyops.flag += keyDelete
}

func (txops txOps) upsertWithDep(k compositeKey, val []byte, deps []string, depSnapshot uint64) {
	keyops := txops.getOrCreateKeyEntry(k)
	keyops.flag += upsertVal
	keyops.value = val
	keyops.deps = deps
	keyops.depSnapshot = depSnapshot
}

func (txops txOps) deleteWithDep(k compositeKey, deps []string, depSnapshot uint64) {
	keyops := txops.getOrCreateKeyEntry(k)
	keyops.flag += keyDelete
	keyops.deps = deps
	keyops.depSnapshot = depSnapshot
}

func (txops txOps) metadataUpdate(k compositeKey, metadata []byte) {
	keyops := txops.getOrCreateKeyEntry(k)
	keyops.flag += metadataUpdate
	keyops.metadata = metadata
}

func (txops txOps) metadataDelete(k compositeKey) {
	keyops := txops.getOrCreateKeyEntry(k)
	keyops.flag += metadataDelete
}

func (txops txOps) getOrCreateKeyEntry(k compositeKey) *keyOps {
	keyops, ok := txops[k]
	if !ok {
		keyops = &keyOps{}
		txops[k] = keyops
	}
	return keyops
}

////////////////// keyOps functions

func (keyops keyOps) isDelete() bool {
	return keyops.flag&(keyDelete) == keyDelete
}

func (keyops keyOps) isUpsertAndMetadataUpdate() bool {
	if keyops.flag&upsertVal == upsertVal {
		return keyops.flag&metadataUpdate == metadataUpdate ||
			keyops.flag&metadataDelete == metadataDelete
	}
	return false
}

func (keyops keyOps) isOnlyUpsert() bool {
	return keyops.flag|upsertVal == upsertVal
}