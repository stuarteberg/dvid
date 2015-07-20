// +build basholeveldb

package local

import (
	"bytes"
	"fmt"

	"github.com/janelia-flyem/dvid/dvid"
	"github.com/janelia-flyem/dvid/storage"

	levigo "github.com/janelia-flyem/go/basholeveldb"
	humanize "github.com/janelia-flyem/go/go-humanize"
)

// These constants were guided by Basho documentation and their tuning of leveldb:
//   https://github.com/basho/leveldb/blob/develop/README
// See video on "Optimizing LevelDB for Performance and Scale" here:
//   http://www.youtube.com/watch?v=vo88IdglU_8
const (
	Version = "Basho Leveldb"

	Driver = "github.com/janelia-flyem/go/levigo"

	// Default size of LRU cache that caches frequently used uncompressed blocks.
	DefaultCacheSize = 536870912

	// Default # bits for Bloom Filter.  The filter reduces the number of unnecessary
	// disk reads needed for Get() calls by a large factor.
	DefaultBloomBits = 16

	// Number of open files that can be used by the datastore.  You may need to
	// increase this if your datastore has a large working set (budget one open
	// file per 2MB of working set).  You might need to do "ulimit -n 1200" or
	// some other number to make sure you can handle the default.
	DefaultMaxOpenFiles = 1024

	// Approximate size of user data packed per block.  Note that the
	// block size specified here corresponds to uncompressed data.  The
	// actual size of the unit read from disk may be smaller if
	// compression is enabled.  This parameter can be changed dynamically.
	DefaultBlockSize = 64 * dvid.Kilo

	// Amount of data to build up in memory (backed by an unsorted log
	// on disk) before converting to a sorted on-disk file.  Increasing
	// this value will automatically increase the size of the datastore
	// compared to the actual stored data.
	//
	// Larger values increase performance, especially during bulk loads.
	// Up to two write buffers may be held in memory at the same time,
	// so you may wish to adjust this parameter to control memory usage.
	// Also, a larger write buffer will result in a longer recovery time
	// the next time the database is opened.
	DefaultWriteBufferSize = 62914560

	// Write Options

	// If Sync=true, the write will be flushed from the operating system
	// buffer cache is considered complete.  If set, writes will be slower.
	//
	// If Sync=false, and the machine crashes, some recent
	// writes may be lost.  Note that if it is just the process that
	// crashes (i.e., the machine does not reboot), no writes will be
	// lost even if Sync=false.
	//
	// In other words, a DB write with sync==false has similar
	// crash semantics as the "write()" system call.  A DB write
	// with sync==true has similar crash semantics to a "write()"
	// system call followed by "fsync()".
	DefaultSync = false

	// Read Options

	// If true, all data read from underlying storage will be verified
	// against coresponding checksums, thereby making reads slower.
	DefaultVerifyChecksums = false

	// If true, iteration caching will be disabled.  This might be of
	// use during bulk scans.
	DefaultDontFillCache = false
)

type Ranges []levigo.Range

type Sizes []uint64

// --- The Leveldb Implementation must satisfy a Engine interface ----

type LevelDB struct {
	// Directory of datastore
	directory string

	// Config at time of Open()
	config dvid.Config

	options *leveldbOptions
	ldb     *levigo.DB
}

func GetOptions(create bool, config dvid.Config) (*leveldbOptions, error) {
	opt := &leveldbOptions{
		Options:      levigo.NewOptions(),
		ReadOptions:  levigo.NewReadOptions(),
		WriteOptions: levigo.NewWriteOptions(),
		env:          levigo.NewDefaultEnv(),
	}
	opt.WriteOptions.SetSync(DefaultSync) // Huge performance penalty to set sync to true

	// Set flags based on create parameter
	opt.SetCreateIfMissing(create)
	opt.SetErrorIfExists(create)

	// Create associated data structures with default values
	bloomBits, found, err := config.GetInt("BloomFilterBitsPerKey")
	if err != nil {
		return nil, err
	}
	if !found {
		bloomBits = DefaultBloomBits
	}
	if create {
		opt.SetBloomFilterBitsPerKey(bloomBits)
	}

	cacheSize, found, err := config.GetInt("CacheSize")
	if err != nil {
		return nil, err
	}
	if !found {
		cacheSize = DefaultCacheSize
	} else {
		cacheSize *= dvid.Mega
	}
	if create {
		dvid.Infof("leveldb cache size: %s\n",
			humanize.Bytes(uint64(cacheSize)))
		opt.SetLRUCacheSize(cacheSize)
	}

	writeBufferSize, found, err := config.GetInt("WriteBufferSize")
	if err != nil {
		return nil, err
	}
	if !found {
		writeBufferSize = DefaultWriteBufferSize
	} else {
		writeBufferSize *= dvid.Mega
	}
	if create {
		dvid.Infof("leveldb write buffer size: %s\n",
			humanize.Bytes(uint64(writeBufferSize)))
		opt.SetWriteBufferSize(writeBufferSize)
	}

	maxOpenFiles, found, err := config.GetInt("MaxOpenFiles")
	if err != nil {
		return nil, err
	}
	if !found {
		maxOpenFiles = DefaultMaxOpenFiles
	}
	if create {
		opt.SetMaxOpenFiles(maxOpenFiles)
	}

	blockSize, found, err := config.GetInt("BlockSize")
	if err != nil {
		return nil, err
	}
	if !found {
		blockSize = DefaultBlockSize
	}
	if create {
		opt.SetBlockSize(blockSize)
	}

	opt.SetInfoLog(nil)
	opt.SetParanoidChecks(false)
	//opt.SetBlockRestartInterval(8)

	// Don't bother with compression on leveldb side because it will be
	// selectively applied on DVID side.  We may return and then transmit
	// Snappy-compressed data without ever decompressing on server-side.
	opt.SetCompression(levigo.NoCompression) // (levigo.SnappyCompression)

	return opt, nil
}

// NewKeyValueStore returns a leveldb backend.  If create is true, the leveldb
// will be created at the path if it doesn't already exist.
func NewKeyValueStore(path string, create bool, config dvid.Config) (storage.Engine, error) {
	dvid.StartCgo()
	defer dvid.StopCgo()

	opt, err := GetOptions(create, config)
	if err != nil {
		return nil, err
	}

	leveldb := &LevelDB{
		directory: path,
		config:    config,
		options:   opt,
	}

	ldb, err := levigo.Open(path, opt.Options)
	if err != nil {
		return nil, err
	}
	leveldb.ldb = ldb

	return leveldb, nil
}

// RepairStore tries to repair a damaged leveldb
func RepairStore(path string, config dvid.Config) error {
	dvid.StartCgo()
	defer dvid.StopCgo()

	opt, err := GetOptions(false, config)
	if err != nil {
		return err
	}

	err = levigo.RepairDatabase(path, opt.Options)
	if err != nil {
		return err
	}
	return nil
}

// ---- Engine interface ----

func (db *LevelDB) String() string {
	return "basho-tuned leveldb + levigo driver"
}

func (db *LevelDB) GetConfig() dvid.Config {
	return db.config
}

// Close closes the leveldb and then the I/O abstraction for leveldb.
func (db *LevelDB) Close() {
	if db != nil {
		if db.ldb != nil {
			db.ldb.Close()
		}
		if db.options.Options != nil {
			db.options.Options.Close()
		}
		if db.options.ReadOptions != nil {
			db.options.ReadOptions.Close()
		}
		if db.options.WriteOptions != nil {
			db.options.WriteOptions.Close()
		}
		if db.options.filter != nil {
			db.options.filter.Close()
		}
		if db.options.cache != nil {
			db.options.cache.Close()
		}
		if db.options.env != nil {
			db.options.env.Close()
		}
	}
}

// ---- OrderedKeyValueGetter interface ------

// Get returns a value given a key.
func (db *LevelDB) Get(ctx storage.Context, tk storage.TKey) ([]byte, error) {
	if ctx == nil {
		return nil, fmt.Errorf("Received nil context in Get()")
	}
	if ctx.Versioned() {
		vctx, ok := ctx.(storage.VersionedCtx)
		if !ok {
			return nil, fmt.Errorf("Bad Get(): context is versioned but doesn't fulfill interface: %v", ctx)
		}

		// Get all versions of this key and return the most recent
		// log.Printf("  basholeveldb versioned get of key %v\n", k)
		values, err := db.getSingleKeyVersions(vctx, tk)
		// log.Printf("            got back %v\n", values)
		if err != nil {
			return nil, err
		}
		kv, err := vctx.VersionedKeyValue(values)
		// log.Printf("  after deversioning: %v\n", kv)
		if kv != nil {
			return kv.V, err
		}
		return nil, err
	} else {
		key := ctx.ConstructKey(tk)
		ro := db.options.ReadOptions
		// log.Printf("  basholeveldb unversioned get of key %v\n", key)
		dvid.StartCgo()
		v, err := db.ldb.Get(ro, key)
		dvid.StopCgo()
		storage.StoreValueBytesRead <- len(v)
		return v, err
	}
}

// getSingleKeyVersions returns all versions of a key.  These key-value pairs will be sorted
// in ascending key order and could include a tombstone key.
func (db *LevelDB) getSingleKeyVersions(vctx storage.VersionedCtx, k []byte) ([]*storage.KeyValue, error) {
	dvid.StartCgo()
	ro := levigo.NewReadOptions()
	it := db.ldb.NewIterator(ro)
	defer func() {
		it.Close()
		dvid.StopCgo()
	}()

	values := []*storage.KeyValue{}
	kStart, err := vctx.MinVersionKey(k)
	if err != nil {
		return nil, err
	}
	kEnd, err := vctx.MaxVersionKey(k)
	if err != nil {
		return nil, err
	}

	it.Seek(kStart)
	for {
		if it.Valid() {
			itKey := it.Key()
			storage.StoreKeyBytesRead <- len(itKey)
			if bytes.Compare(itKey, kEnd) > 0 {
				// log.Printf("key past %v\n", kEnd)
				return values, nil
			}
			itValue := it.Value()
			// log.Printf("got value of length %d\n", len(itValue))
			storage.StoreValueBytesRead <- len(itValue)
			values = append(values, &storage.KeyValue{itKey, itValue})
			it.Next()
		} else {
			err = it.GetError()
			// log.Printf("iteration done, err = %v\n", err)
			if err == nil {
				return values, nil
			}
			return nil, err
		}
	}
}

type errorableKV struct {
	*storage.KeyValue
	error
}

func sendKV(vctx storage.VersionedCtx, values []*storage.KeyValue, ch chan errorableKV) {
	// fmt.Printf("sendKV: values %v\n", values)
	if len(values) != 0 {
		kv, err := vctx.VersionedKeyValue(values)
		if err != nil {
			ch <- errorableKV{nil, err}
			return
		}
		if kv != nil {
			// fmt.Printf("Sending kv: %v\n", kv)
			ch <- errorableKV{kv, nil}
		}
	}
}

// versionedRange sends a range of key-value pairs for a particular version down a channel.
func (db *LevelDB) versionedRange(vctx storage.VersionedCtx, kStart, kEnd storage.TKey, ch chan errorableKV, keysOnly bool) {
	dvid.StartCgo()
	ro := levigo.NewReadOptions()
	it := db.ldb.NewIterator(ro)
	defer func() {
		it.Close()
		dvid.StopCgo()
	}()

	minKey, err := vctx.MinVersionKey(kStart)
	if err != nil {
		ch <- errorableKV{nil, err}
		return
	}
	maxKey, err := vctx.MaxVersionKey(kEnd)
	if err != nil {
		ch <- errorableKV{nil, err}
		return
	}

	values := []*storage.KeyValue{}
	maxVersionKey, err := vctx.MaxVersionKey(kStart)
	if err != nil {
		ch <- errorableKV{nil, err}
		return
	}
	// log.Printf("         minKey %v\n", minKey)
	// log.Printf("         maxKey %v\n", maxKey)
	// log.Printf("  maxVersionKey %v\n", maxVersionKey)

	it.Seek(minKey)
	var itValue []byte
	for {
		if it.Valid() {
			if !keysOnly {
				itValue = it.Value()
				storage.StoreValueBytesRead <- len(itValue)
			}
			itKey := it.Key()
			// log.Printf("   +++valid key %v\n", itKey)
			storage.StoreKeyBytesRead <- len(itKey)

			// Did we pass all versions for last key read?
			if bytes.Compare(itKey, maxVersionKey) > 0 {
				indexBytes, err := vctx.TKeyFromKey(itKey)
				if err != nil {
					ch <- errorableKV{nil, err}
					return
				}
				maxVersionKey, err = vctx.MaxVersionKey(indexBytes)
				if err != nil {
					ch <- errorableKV{nil, err}
					return
				}
				// log.Printf("->maxVersionKey %v (transmitting %d values)\n", maxVersionKey, len(values))
				sendKV(vctx, values, ch)
				values = []*storage.KeyValue{}
			}
			// Did we pass the final key?
			if bytes.Compare(itKey, maxKey) > 0 {
				if len(values) > 0 {
					sendKV(vctx, values, ch)
				}
				ch <- errorableKV{nil, nil}
				return
			}
			// log.Printf("Appending value with key %v\n", itKey)
			values = append(values, &storage.KeyValue{itKey, itValue})
			it.Next()
		} else {
			if err = it.GetError(); err != nil {
				ch <- errorableKV{nil, err}
			} else {
				sendKV(vctx, values, ch)
				ch <- errorableKV{nil, nil}
			}
			return
		}
	}
}

// unversionedRange sends a range of key-value pairs down a channel.
func (db *LevelDB) unversionedRange(ctx storage.Context, kStart, kEnd storage.TKey, ch chan errorableKV, keysOnly bool) {
	dvid.StartCgo()
	ro := levigo.NewReadOptions()
	it := db.ldb.NewIterator(ro)
	defer func() {
		it.Close()
		dvid.StopCgo()
	}()

	// Apply context if applicable
	keyBeg := ctx.ConstructKey(kStart)
	keyEnd := ctx.ConstructKey(kEnd)

	// fmt.Printf("unversionedRange():\n")
	// fmt.Printf("    index beg: %v\n", kStart)
	// fmt.Printf("    index end: %v\n", kEnd)
	// fmt.Printf("    key start: %v\n", keyBeg)
	// fmt.Printf("      key end: %v\n", keyEnd)

	var itValue []byte
	it.Seek(keyBeg)
	for {
		if it.Valid() {
			// fmt.Printf("unversioned found key %v, %d bytes value\n", it.Key(), len(it.Value()))
			if !keysOnly {
				itValue = it.Value()
				storage.StoreValueBytesRead <- len(itValue)
			}
			itKey := it.Key()
			storage.StoreKeyBytesRead <- len(itKey)
			// Did we pass the final key?
			if bytes.Compare(itKey, keyEnd) > 0 {
				break
			}
			ch <- errorableKV{&storage.KeyValue{itKey, itValue}, nil}
			it.Next()
		} else {
			break
		}
	}
	if err := it.GetError(); err != nil {
		ch <- errorableKV{nil, err}
	} else {
		ch <- errorableKV{nil, nil}
	}
	return
}

// KeysInRange returns a range of present keys spanning (kStart, kEnd).  Values
// associated with the keys are not read.   If the keys are versioned, only keys
// in the ancestor path of the current context's version will be returned.
func (db *LevelDB) KeysInRange(ctx storage.Context, kStart, kEnd storage.TKey) ([]storage.TKey, error) {
	if ctx == nil {
		return nil, fmt.Errorf("Received nil context in KeysInRange()")
	}
	ch := make(chan errorableKV)

	// Run the range query on a potentially versioned key in a goroutine.
	go func() {
		if !ctx.Versioned() {
			db.unversionedRange(ctx, kStart, kEnd, ch, true)
		} else {
			db.versionedRange(ctx.(storage.VersionedCtx), kStart, kEnd, ch, true)
		}
	}()

	// Consume the keys.
	values := []storage.TKey{}
	for {
		result := <-ch
		if result.KeyValue == nil {
			return values, nil
		}
		if result.error != nil {
			return nil, result.error
		}
		tk, err := ctx.TKeyFromKey(result.KeyValue.K)
		if err != nil {
			return nil, err
		}
		values = append(values, tk)
	}
}

// SendKeysInRange sends a range of keys spanning (kStart, kEnd).  Values
// associated with the keys are not read.   If the keys are versioned, only keys
// in the ancestor path of the current context's version will be returned.
// End of range is marked by a nil key.
func (db *LevelDB) SendKeysInRange(ctx storage.Context, kStart, kEnd storage.TKey, kch storage.KeyChan) error {
	if ctx == nil {
		return fmt.Errorf("Received nil context in SendKeysInRange()")
	}
	ch := make(chan errorableKV)

	// Run the range query on a potentially versioned key in a goroutine.
	go func() {
		if !ctx.Versioned() {
			db.unversionedRange(ctx, kStart, kEnd, ch, true)
		} else {
			db.versionedRange(ctx.(storage.VersionedCtx), kStart, kEnd, ch, true)
		}
	}()

	// Consume the keys.
	for {
		result := <-ch
		if result.KeyValue == nil {
			kch <- nil
			return nil
		}
		if result.error != nil {
			kch <- nil
			return result.error
		}
		kch <- result.KeyValue.K
	}
}

// GetRange returns a range of values spanning (kStart, kEnd) keys.  These key-value
// pairs will be sorted in ascending key order.  If the keys are versioned, all key-value
// pairs for the particular version will be returned.
func (db *LevelDB) GetRange(ctx storage.Context, kStart, kEnd storage.TKey) ([]*storage.TKeyValue, error) {
	if ctx == nil {
		return nil, fmt.Errorf("Received nil context in GetRange()")
	}
	ch := make(chan errorableKV)

	// Run the range query on a potentially versioned key in a goroutine.
	go func() {
		if ctx == nil || !ctx.Versioned() {
			db.unversionedRange(ctx, kStart, kEnd, ch, false)
		} else {
			db.versionedRange(ctx.(storage.VersionedCtx), kStart, kEnd, ch, false)
		}
	}()

	// Consume the key-value pairs.
	values := []*storage.TKeyValue{}
	for {
		result := <-ch
		if result.KeyValue == nil {
			return values, nil
		}
		if result.error != nil {
			return nil, result.error
		}
		tk, err := ctx.TKeyFromKey(result.KeyValue.K)
		if err != nil {
			return nil, err
		}
		tkv := storage.TKeyValue{tk, result.KeyValue.V}
		values = append(values, &tkv)
	}
}

// ProcessRange sends a range of key-value pairs to chunk handlers.  If the keys are versioned,
// only key-value pairs for kStart's version will be transmitted.
func (db *LevelDB) ProcessRange(ctx storage.Context, kStart, kEnd storage.TKey, op *storage.ChunkOp, f storage.ChunkFunc) error {
	if ctx == nil {
		return fmt.Errorf("Received nil context in ProcessRange()")
	}
	ch := make(chan errorableKV)

	// Run the range query on a potentially versioned key in a goroutine.
	go func() {
		if ctx == nil || !ctx.Versioned() {
			db.unversionedRange(ctx, kStart, kEnd, ch, false)
		} else {
			db.versionedRange(ctx.(storage.VersionedCtx), kStart, kEnd, ch, false)
		}
	}()

	// Consume the key-value pairs.
	for {
		result := <-ch
		if result.KeyValue == nil {
			return nil
		}
		if result.error != nil {
			return result.error
		}
		if op.Wg != nil {
			op.Wg.Add(1)
		}
		tk, err := ctx.TKeyFromKey(result.KeyValue.K)
		if err != nil {
			return err
		}
		tkv := storage.TKeyValue{tk, result.KeyValue.V}
		chunk := &storage.Chunk{op, &tkv}
		if err := f(chunk); err != nil {
			return err
		}
	}
}

// SendRange sends a range of full keys.  This is to be used for low-level data
// retrieval like DVID-to-DVID communication and should not be used by data type
// implementations if possible.  A nil is sent down the channel when the
// range is complete.
func (db *LevelDB) SendRange(kStart, kEnd storage.Key, keysOnly bool, out chan *storage.KeyValue) error {
	dvid.StartCgo()
	ro := levigo.NewReadOptions()
	it := db.ldb.NewIterator(ro)
	defer func() {
		it.Close()
		dvid.StopCgo()
	}()

	var itValue []byte
	it.Seek(kStart)
	for {
		if it.Valid() {
			if !keysOnly {
				itValue = it.Value()
				storage.StoreValueBytesRead <- len(itValue)
			}
			itKey := it.Key()
			storage.StoreKeyBytesRead <- len(itKey)
			// Did we pass the final key?
			if bytes.Compare(itKey, kEnd) > 0 {
				break
			}
			kv := storage.KeyValue{itKey, itValue}
			out <- &kv
			it.Next()
		} else {
			break
		}
	}
	out <- nil
	if err := it.GetError(); err != nil {
		return err
	}
	return nil
}

// ---- KeyValueSetter interface ------

// Put writes a value with given key.
func (db *LevelDB) Put(ctx storage.Context, tk storage.TKey, v []byte) error {
	if ctx == nil {
		return fmt.Errorf("Received nil context in Put()")
	}
	wo := db.options.WriteOptions

	var err error
	key := ctx.ConstructKey(tk)
	if !ctx.Versioned() {
		dvid.StartCgo()
		err = db.ldb.Put(wo, key, v)
		dvid.StopCgo()
	} else {
		vctx, ok := ctx.(storage.VersionedCtx)
		if !ok {
			return fmt.Errorf("Non-versioned context that says it's versioned received in Put(): %v", ctx)
		}
		tombstoneKey := vctx.TombstoneKey(tk)
		batch := db.NewBatch(vctx).(*goBatch)
		batch.WriteBatch.Delete(tombstoneKey)
		batch.WriteBatch.Put(key, v)
		if err = batch.Commit(); err != nil {
			err = fmt.Errorf("Error on PUT: %v\n", err)
		}
	}

	storage.StoreKeyBytesWritten <- len(key)
	storage.StoreValueBytesWritten <- len(v)
	return err
}

// Delete removes a value with given key.
func (db *LevelDB) Delete(ctx storage.Context, tk storage.TKey) error {
	if ctx == nil {
		return fmt.Errorf("Received nil context in Delete()")
	}
	wo := db.options.WriteOptions

	var err error
	key := ctx.ConstructKey(tk)
	if !ctx.Versioned() {
		dvid.StartCgo()
		err = db.ldb.Delete(wo, key)
		dvid.StopCgo()
	} else {
		vctx, ok := ctx.(storage.VersionedCtx)
		if !ok {
			return fmt.Errorf("Non-versioned context that says it's versioned received in Delete(): %v", ctx)
		}
		tombstoneKey := vctx.TombstoneKey(tk)
		batch := db.NewBatch(vctx).(*goBatch)
		batch.WriteBatch.Delete(key)
		batch.WriteBatch.Put(tombstoneKey, dvid.EmptyValue())
		if err = batch.Commit(); err != nil {
			err = fmt.Errorf("Error on Delete: %v\n", err)
		}
	}

	return err
}

// ---- OrderedKeyValueSetter interface ------

// PutRange puts type key-value pairs that have been sorted in sequential key order.
// Current implementation in levigo driver simply does a batch write.
func (db *LevelDB) PutRange(ctx storage.Context, kvs []storage.TKeyValue) error {
	if ctx == nil {
		return fmt.Errorf("Received nil context in PutRange()")
	}
	batch := db.NewBatch(ctx).(*goBatch)
	for _, kv := range kvs {
		batch.Put(kv.K, kv.V)
	}
	if err := batch.Commit(); err != nil {
		return err
	}
	return nil
}

// DeleteRange removes all key-value pairs with keys in the given range.
func (db *LevelDB) DeleteRange(ctx storage.Context, kStart, kEnd storage.TKey) error {
	if ctx == nil {
		return fmt.Errorf("Received nil context in DeleteRange()")
	}

	// For leveldb, we just iterate over keys in range and delete each one using batch.
	const BATCH_SIZE = 10000
	batch := db.NewBatch(ctx).(*goBatch)

	ch := make(chan errorableKV)

	// Run the keys-only range query in a goroutine.
	go func() {
		if ctx == nil || !ctx.Versioned() {
			db.unversionedRange(ctx, kStart, kEnd, ch, true)
		} else {
			db.versionedRange(ctx.(storage.VersionedCtx), kStart, kEnd, ch, true)
		}
	}()

	// Consume the key-value pairs.
	numKV := 0
	for {
		result := <-ch
		if result.KeyValue == nil {
			break
		}
		if result.error != nil {
			return result.error
		}

		// The key coming down channel is not index but full key, so no need to construct key using context.
		// If versioned, write a tombstone using current version id since we don't want to delete locked ancestors.
		// If unversioned, just delete.
		tk, err := ctx.TKeyFromKey(result.KeyValue.K)
		if err != nil {
			return err
		}
		batch.Delete(tk)

		if (numKV+1)%BATCH_SIZE == 0 {
			if err := batch.Commit(); err != nil {
				return fmt.Errorf("Error on batch DELETE at key-value pair %d: %v\n", numKV, err)
			}
			batch = db.NewBatch(ctx).(*goBatch)
		}
		numKV++
	}
	if numKV%BATCH_SIZE != 0 {
		if err := batch.Commit(); err != nil {
			return fmt.Errorf("Error on last batch DELETE: %v\n", err)
		}
	}
	dvid.Debugf("Deleted %d key-value pairs via delete range for %s.\n", numKV, ctx)
	return nil
}

// DeleteAll deletes all key-value associated with a context (data instance and version).
func (db *LevelDB) DeleteAll(ctx storage.Context, allVersions bool) error {
	if ctx == nil {
		return fmt.Errorf("Received nil context in Delete()")
	}
	dvid.StartCgo()

	// Don't have to worry about tombstones.  Delete all keys from all versions for this instance id.
	minTKey := storage.MinTKey(storage.TKeyMinClass)
	maxTKey := storage.MaxTKey(storage.TKeyMaxClass)
	vctx, ok := ctx.(storage.VersionedCtx)
	if !ok {
		return fmt.Errorf("Non-versioned context passed to DELETE ALL VERSIONS in basholeveldb driver: %v", ctx)
	}
	minKey, err := vctx.MinVersionKey(minTKey)
	if err != nil {
		return err
	}
	maxKey, err := vctx.MaxVersionKey(maxTKey)
	if err != nil {
		return err
	}

	const BATCH_SIZE = 10000
	batch := db.NewBatch(ctx).(*goBatch)

	ro := levigo.NewReadOptions()
	it := db.ldb.NewIterator(ro)
	defer func() {
		it.Close()
		dvid.StopCgo()
	}()

	numKV := 0
	it.Seek(minKey)
	deleteVersion := ctx.VersionID()
	for {
		if it.Valid() {
			itKey := it.Key()
			storage.StoreKeyBytesRead <- len(itKey)
			// Did we pass the final key?
			if bytes.Compare(itKey, maxKey) > 0 {
				break
			}
			if !allVersions {
				_, v, _, err := storage.DataKeyToLocalIDs(itKey)
				if err != nil {
					return fmt.Errorf("Error on DELETE ALL for version %d: %v", ctx.VersionID(), err)
				}
				if v != deleteVersion {
					it.Next()
					continue
				}
			}
			batch.WriteBatch.Delete(itKey)
			if (numKV+1)%BATCH_SIZE == 0 {
				if err := batch.Commit(); err != nil {
					return fmt.Errorf("Error on DELETE ALL at key-value pair %d: %v", numKV, err)
				}
				batch = db.NewBatch(ctx).(*goBatch)
			}
			numKV++

			it.Next()
		} else {
			break
		}
	}
	if numKV%BATCH_SIZE != 0 {
		if err := batch.Commit(); err != nil {
			return fmt.Errorf("Error on last batch DELETE: %v\n", err)
		}
	}
	dvid.Debugf("Deleted %d key-value pairs via DELETE ALL for %s.\n", numKV, ctx)
	return nil
}

// --- Batcher interface ----

type goBatch struct {
	ctx  storage.Context
	vctx storage.VersionedCtx
	*levigo.WriteBatch
	wo  *levigo.WriteOptions
	ldb *levigo.DB
}

// NewBatch returns an implementation that allows batch writes
func (db *LevelDB) NewBatch(ctx storage.Context) storage.Batch {
	if ctx == nil {
		dvid.Criticalf("Received nil context in NewBatch()")
		return nil
	}
	dvid.StartCgo()
	defer dvid.StopCgo()

	var vctx storage.VersionedCtx
	var ok bool
	vctx, ok = ctx.(storage.VersionedCtx)
	if !ok {
		vctx = nil
	}
	return &goBatch{ctx, vctx, levigo.NewWriteBatch(), db.options.WriteOptions, db.ldb}
}

// --- Batch interface ---

func (batch *goBatch) Delete(tk storage.TKey) {
	if batch == nil || batch.ctx == nil {
		dvid.Criticalf("Received nil batch or nil batch context in Delete()\n")
		return
	}
	dvid.StartCgo()
	defer dvid.StopCgo()

	key := batch.ctx.ConstructKey(tk)
	if batch.vctx != nil {
		tombstone := batch.vctx.TombstoneKey(tk) // This will now have current version
		batch.WriteBatch.Put(tombstone, dvid.EmptyValue())
	}
	batch.WriteBatch.Delete(key)
}

func (batch *goBatch) Put(tk storage.TKey, v []byte) {
	if batch == nil || batch.ctx == nil {
		dvid.Criticalf("Received nil batch or nil batch context in Delete()\n")
		return
	}
	dvid.StartCgo()
	defer dvid.StopCgo()

	key := batch.ctx.ConstructKey(tk)
	if batch.vctx != nil {
		tombstone := batch.vctx.TombstoneKey(tk) // This will now have current version
		batch.WriteBatch.Delete(tombstone)
	}
	batch.WriteBatch.Put(key, v)

	storage.StoreKeyBytesWritten <- len(key)
	storage.StoreValueBytesWritten <- len(v)
	batch.WriteBatch.Put(key, v)
}

func (batch *goBatch) Commit() error {
	dvid.StartCgo()
	defer dvid.StopCgo()

	err := batch.ldb.Write(batch.wo, batch.WriteBatch)
	batch.WriteBatch.Close()
	return err
}

/** Clear and Close were removed due to how other key-value stores implement batches.
    It's easier to implement cross-database handling of a simple write/delete batch
    that commits then closes rather than something that clears.

func (batch *goBatch) Clear() {
	dvid.StartCgo()
	defer dvid.StopCgo()
	batch.WriteBatch.Clear()
}

func (batch *goBatch) Close() {
	dvid.StartCgo()
	defer dvid.StopCgo()
	batch.WriteBatch.Close()
}
**/

// --- Options ----

type leveldbOptions struct {
	*levigo.Options
	*levigo.ReadOptions  // Standard settings on NewReadOptions()
	*levigo.WriteOptions // Standard settings on NewWriteOptions()

	// Keep leveldb settings for quick recall and checks on set
	nLRUCacheBytes  int
	bloomBitsPerKey int
	writeBufferSize int
	maxOpenFiles    int
	blockSize       int

	// Keep pointers for associated data structures for close
	cache  *levigo.Cache
	filter *levigo.FilterPolicy
	env    *levigo.Env
}

// Amount of data to build up in memory (backed by an unsorted log
// on disk) before converting to a sorted on-disk file.
//
// Larger values increase performance, especially during bulk loads.
// Up to two write buffers may be held in memory at the same time,
// so you may wish to adjust this parameter to control memory usage.
// Also, a larger write buffer will result in a longer recovery time
// the next time the database is opened.
func (opts *leveldbOptions) SetWriteBufferSize(nBytes int) {
	if nBytes != opts.writeBufferSize {
		dvid.Debugf("Write buffer set to %d bytes.\n", nBytes)
		opts.Options.SetWriteBufferSize(nBytes)
		opts.writeBufferSize = nBytes
	}
}

func (opts *leveldbOptions) GetWriteBufferSize() (nBytes int) {
	nBytes = opts.writeBufferSize
	return
}

// Number of open files that can be used by the DB.  You may need to
// increase this if your database has a large working set (budget
// one open file per 2MB of working set).
// See: http://leveldb.googlecode.com/svn/trunk/doc/impl.html
func (opts *leveldbOptions) SetMaxOpenFiles(nFiles int) {
	if nFiles != opts.maxOpenFiles {
		opts.Options.SetMaxOpenFiles(nFiles)
		opts.maxOpenFiles = nFiles
	}
}

func (opts *leveldbOptions) GetMaxOpenFiles() (nFiles int) {
	nFiles = opts.maxOpenFiles
	return
}

// Approximate size of user data packed per block.  Note that the
// block size specified here corresponds to uncompressed data.  The
// actual size of the unit read from disk may be smaller if
// compression is enabled.  This parameter can be changed dynamically.
func (opts *leveldbOptions) SetBlockSize(nBytes int) {
	if nBytes != opts.blockSize {
		dvid.Debugf("Block size set to %d bytes.\n", nBytes)
		opts.Options.SetBlockSize(nBytes)
		opts.blockSize = nBytes
	}
}

func (opts *leveldbOptions) GetBlockSize() (nBytes int) {
	nBytes = opts.blockSize
	return
}

// SetCache sets the size of the LRU cache that caches frequently used
// uncompressed blocks.
func (opts *leveldbOptions) SetLRUCacheSize(nBytes int) {
	if nBytes != opts.nLRUCacheBytes {
		if opts.cache != nil {
			opts.cache.Close()
		}
		dvid.Debugf("LRU cache size set to %d bytes.\n", nBytes)
		opts.cache = levigo.NewLRUCache(nBytes)
		opts.nLRUCacheBytes = nBytes
		opts.Options.SetCache(opts.cache)
	}
}

func (opts *leveldbOptions) GetLRUCacheSize() (nBytes int) {
	nBytes = opts.nLRUCacheBytes
	return
}

// SetBloomFilter sets the bits per key for a bloom filter.  This filter
// will reduce the number of unnecessary disk reads needed for Get() calls
// by a large factor.
func (opts *leveldbOptions) SetBloomFilterBitsPerKey(bitsPerKey int) {
	if bitsPerKey != opts.bloomBitsPerKey {
		if opts.filter != nil {
			opts.filter.Close()
		}
		opts.filter = levigo.NewBloomFilter(DefaultBloomBits)
		opts.bloomBitsPerKey = bitsPerKey
		opts.Options.SetFilterPolicy(opts.filter)
	}
}

func (opts *leveldbOptions) GetBloomFilterBitsPerKey() (bitsPerKey int) {
	bitsPerKey = opts.bloomBitsPerKey
	return
}
