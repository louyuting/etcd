// Copyright 2015 The etcd Authors
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

package backend

import (
	"bytes"
	"math"
	"sync"
	"sync/atomic"
	"time"

	bolt "go.etcd.io/bbolt"
	"go.uber.org/zap"
)

type BatchTx interface {
	ReadTx
	UnsafeCreateBucket(name []byte)
	UnsafePut(bucketName []byte, key []byte, value []byte)
	UnsafeSeqPut(bucketName []byte, key []byte, value []byte)
	UnsafeDelete(bucketName []byte, key []byte)
	// Commit commits a previous tx and begins a new writable one.
	Commit()
	// CommitAndStop commits the previous tx and does not create a new one.
	CommitAndStop()
}

// batchTx是写事务的封装
// 这里的访问都需要提前获取排他锁
type batchTx struct {
	// 这里的锁是保证 batchTx 的排他，也就是单线程
	// 所以对下面属性的访问是都是在锁的保护下。
	sync.Mutex
	tx      *bolt.Tx
	backend *backend

	pending int
}

func (t *batchTx) Lock() {
	t.Mutex.Lock()
}

func (t *batchTx) Unlock() {
	if t.pending >= t.backend.batchLimit {
		t.commit(false)
	}
	t.Mutex.Unlock()
}

// BatchTx interface embeds ReadTx interface. But RLock() and RUnlock() do not
// have appropriate semantics in BatchTx interface. Therefore should not be called.
// TODO: might want to decouple ReadTx and BatchTx

func (t *batchTx) RLock() {
	panic("unexpected RLock")
}

func (t *batchTx) RUnlock() {
	panic("unexpected RUnlock")
}

func (t *batchTx) UnsafeCreateBucket(name []byte) {
	_, err := t.tx.CreateBucket(name)
	if err != nil && err != bolt.ErrBucketExists {
		t.backend.lg.Fatal(
			"failed to create a bucket",
			zap.String("bucket-name", string(name)),
			zap.Error(err),
		)
	}
	t.pending++
}

// UnsafePut must be called holding the lock on the tx.
func (t *batchTx) UnsafePut(bucketName []byte, key []byte, value []byte) {
	t.unsafePut(bucketName, key, value, false)
}

// UnsafeSeqPut must be called holding the lock on the tx.
func (t *batchTx) UnsafeSeqPut(bucketName []byte, key []byte, value []byte) {
	t.unsafePut(bucketName, key, value, true)
}

func (t *batchTx) unsafePut(bucketName []byte, key []byte, value []byte, seq bool) {
	bucket := t.tx.Bucket(bucketName)
	if bucket == nil {
		t.backend.lg.Fatal(
			"failed to find a bucket",
			zap.String("bucket-name", string(bucketName)),
		)
	}
	if seq {
		// it is useful to increase fill percent when the workloads are mostly append-only.
		// this can delay the page split and reduce space usage.
		bucket.FillPercent = 0.9
	}
	if err := bucket.Put(key, value); err != nil {
		t.backend.lg.Fatal(
			"failed to write to a bucket",
			zap.String("bucket-name", string(bucketName)),
			zap.Error(err),
		)
	}
	t.pending++
}

// UnsafeRange must be called holding the lock on the tx.
func (t *batchTx) UnsafeRange(bucketName, key, endKey []byte, limit int64) ([][]byte, [][]byte) {
	bucket := t.tx.Bucket(bucketName)
	if bucket == nil {
		t.backend.lg.Fatal(
			"failed to find a bucket",
			zap.String("bucket-name", string(bucketName)),
		)
	}
	return unsafeRange(bucket.Cursor(), key, endKey, limit)
}

func unsafeRange(c *bolt.Cursor, key, endKey []byte, limit int64) (keys [][]byte, vs [][]byte) {
	if limit <= 0 {
		limit = math.MaxInt64
	}
	var isMatch func(b []byte) bool
	if len(endKey) > 0 {
		isMatch = func(b []byte) bool { return bytes.Compare(b, endKey) < 0 }
	} else {
		isMatch = func(b []byte) bool { return bytes.Equal(b, key) }
		limit = 1
	}

	// 根据key去定位持久化存储
	for ck, cv := c.Seek(key); ck != nil && isMatch(ck); ck, cv = c.Next() {
		vs = append(vs, cv)
		keys = append(keys, ck)
		if limit == int64(len(keys)) {
			break
		}
	}
	return keys, vs
}

// UnsafeDelete must be called holding the lock on the tx.
func (t *batchTx) UnsafeDelete(bucketName []byte, key []byte) {
	bucket := t.tx.Bucket(bucketName)
	if bucket == nil {
		t.backend.lg.Fatal(
			"failed to find a bucket",
			zap.String("bucket-name", string(bucketName)),
		)
	}
	err := bucket.Delete(key)
	if err != nil {
		t.backend.lg.Fatal(
			"failed to delete a key",
			zap.String("bucket-name", string(bucketName)),
			zap.Error(err),
		)
	}
	t.pending++
}

// UnsafeForEach must be called holding the lock on the tx.
func (t *batchTx) UnsafeForEach(bucketName []byte, visitor func(k, v []byte) error) error {
	return unsafeForEach(t.tx, bucketName, visitor)
}

func unsafeForEach(tx *bolt.Tx, bucket []byte, visitor func(k, v []byte) error) error {
	if b := tx.Bucket(bucket); b != nil {
		return b.ForEach(visitor)
	}
	return nil
}

// Commit commits a previous tx and begins a new writable one.
func (t *batchTx) Commit() {
	t.Lock()
	t.commit(false)
	t.Unlock()
}

// CommitAndStop commits the previous tx and does not create a new one.
func (t *batchTx) CommitAndStop() {
	t.Lock()
	t.commit(true)
	t.Unlock()
}

func (t *batchTx) safePending() int {
	t.Mutex.Lock()
	defer t.Mutex.Unlock()
	return t.pending
}

func (t *batchTx) commit(stop bool) {
	// commit the last tx
	if t.tx != nil {
		// 没有pending的事务，所以不需要提交
		if t.pending == 0 && !stop {
			return
		}

		start := time.Now()

		// gofail: var beforeCommit struct{}
		err := t.tx.Commit()
		// gofail: var afterCommit struct{}

		rebalanceSec.Observe(t.tx.Stats().RebalanceTime.Seconds())
		spillSec.Observe(t.tx.Stats().SpillTime.Seconds())
		writeSec.Observe(t.tx.Stats().WriteTime.Seconds())
		commitSec.Observe(time.Since(start).Seconds())
		atomic.AddInt64(&t.backend.commits, 1)

		t.pending = 0
		if err != nil {
			t.backend.lg.Fatal("failed to commit tx", zap.Error(err))
		}
	}
	if !stop {
		// 重新创建写事务
		t.tx = t.backend.begin(true)
	}
}

// 批量提交写事务
type batchTxBuffered struct {
	batchTx
	buf txWriteBuffer
}

func newBatchTxBuffered(backend *backend) *batchTxBuffered {
	tx := &batchTxBuffered{
		batchTx: batchTx{backend: backend},
		buf: txWriteBuffer{
			txBuffer: txBuffer{make(map[string]*bucketBuffer)},
			seq:      true,
		},
	}
	// 这里会开启一个新的事务
	tx.Commit()
	return tx
}

// 一个写事务结束时候，会调用这个函数
func (t *batchTxBuffered) Unlock() {
	// 表示当前 batch 已经有写事务结束但是还没有提交
	if t.pending != 0 {
		// 获取读事务的写锁，然后把写事务的KV更新同步到读事务的缓存Buffer中
		t.backend.readTx.Lock() // blocks txReadBuffer for writing.
		t.buf.writeback(&t.backend.readTx.buf)
		t.backend.readTx.Unlock()
		// 如果当前的写事务的更新操作的次数已经超过了betch的上限，就将当前pending的
		// 所有写操作一次提交到BoltDB数据库
		if t.pending >= t.backend.batchLimit {
			t.commit(false)
		}
	}
	t.batchTx.Unlock()
}

// batch 提交写事务
func (t *batchTxBuffered) Commit() {
	t.Lock()
	t.commit(false)
	t.Unlock()
}

func (t *batchTxBuffered) CommitAndStop() {
	t.Lock()
	t.commit(true)
	t.Unlock()
}

func (t *batchTxBuffered) commit(stop bool) {
	// all read txs must be closed to acquire boltdb commit rwlock
	// 这里是个读写锁，会等待所有读锁关闭，然后竞争到独占的写锁
	// 写事务的提交需要等所有读事务的锁释放
	t.backend.readTx.Lock()
	t.unsafeCommit(stop)
	// 释放读事务的写锁
	t.backend.readTx.Unlock()
}

func (t *batchTxBuffered) unsafeCommit(stop bool) {
	// 处理读事务
	if t.backend.readTx.tx != nil {
		// wait all store read transactions using the current boltdb tx to finish,
		// then close the boltdb txs
		// rollback 读事务
		go func(tx *bolt.Tx, wg *sync.WaitGroup) {
			wg.Wait()
			if err := tx.Rollback(); err != nil {
				t.backend.lg.Fatal("failed to rollback tx", zap.Error(err))
			}
		}(t.backend.readTx.tx, t.backend.readTx.txWg)
		// 每次写事务的batch提交都需要清空 读事务的buffer
		t.backend.readTx.reset()
	}

	// batch提交写事务
	t.batchTx.commit(stop)

	if !stop {
		// 重新创建读事务
		t.backend.readTx.tx = t.backend.begin(false)
	}
}

func (t *batchTxBuffered) UnsafePut(bucketName []byte, key []byte, value []byte) {
	t.batchTx.UnsafePut(bucketName, key, value)
	t.buf.put(bucketName, key, value)
}

func (t *batchTxBuffered) UnsafeSeqPut(bucketName []byte, key []byte, value []byte) {
	t.batchTx.UnsafeSeqPut(bucketName, key, value)
	t.buf.putSeq(bucketName, key, value)
}
